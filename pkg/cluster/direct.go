// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	clientexec "k8s.io/client-go/util/exec"
	metricsclientset "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/gpu"
	"github.com/FlashbackAi/teepin-core/pkg/networking"
)

// WorkloadNamespace is the namespace holding customer workloads. Single
// namespace today; tenancy is enforced by label selectors and the
// account_id predicate in the database, not by namespace boundaries.
//
// Exported because it is not just an internal detail: it is the ONE
// correct value for any Service that needs to select these pods
// (pkg/networking). A namespace passed in independently (previously
// TEEPIN_NAMESPACE, defaulting to "teepin" while pods ran in "default")
// has no way to be right except by accident — a Service in the wrong
// namespace matches nothing and every customer request 503s (Stage 3
// defect 3). Making this a shared constant instead of two configured
// values removes the class of bug rather than fixing one instance of it.
const WorkloadNamespace = "default"

// workloadNamespace is a package-local alias so every existing call site
// in this file did not need touching when the constant was exported.
const workloadNamespace = WorkloadNamespace

// Label and annotation keys. Duplicated from pkg/api rather than
// imported: pkg/api depends on this package, and the reverse would be a
// cycle. These strings are part of the on-cluster contract — an agent
// built from a different revision must still recognise pods created by
// this one, so they change only with a migration.
const (
	labelManaged    = "app.teepin.cloud/managed"
	labelInstanceID = "app.teepin.cloud/instance-id"
	labelName       = "app.teepin.cloud/name"

	// Tenancy labels. These MUST match what pkg/api already writes onto
	// pods in production — a mismatch would make every scoped lookup
	// silently match nothing, turning every customer read into a 404
	// rather than an obvious failure.
	labelProjectID = "teepin.io/project-id"
	labelAccountID = "teepin.io/account-id"

	// MUST match pkg/kumbha's agentLabel exactly. Kumbha's own agent pod
	// carries the same account/project tenancy labels as any customer
	// instance (correctly, for billing/scoping) and is otherwise
	// indistinguishable from one at this layer — the "never appears in
	// the customer's Compute list" guarantee (pkg/kumbha/agent.go's own
	// doc comment) was never actually true: ListInstances is cluster-
	// authoritative (server.go's ListInstances queries the live cluster,
	// merging in compute.instances only for extra metadata), not
	// database-authoritative, and nothing excluded this label from that
	// query until it was excluded here. Found live 2026-08-23: a
	// customer's CPU compute list showed "kumbha-agent-<id>" with a
	// working Delete button.
	labelKumbhaAgent = "teepin.io/kumbha-agent"

	// Consumed by the networking Service selector.
	labelInstanceShort = "teepin.io/instance"
	labelInstanceUUID  = "teepin.io/instance-uuid"

	annotationInstanceType = "teepin.io/instance-type"
)

// EndpointProvisioner gives an instance a public address. The only
// implementation today is *networking.Service (local Ingress via
// cert-manager, the datacenter path); Stage 3's home-node tunnel adds a
// second one at the control-plane edge, which is the reason this is an
// interface rather than DirectClient holding *networking.Service directly
// as it did before.
//
// Deliberately defined here rather than in client.go: client.go's own
// Client interface is k8s-import-clean by design ("nothing above this
// interface may import k8s.io/client-go"), and networking.EndpointInfo
// transitively pulls in client-go through pkg/networking. direct.go is
// already the one file in this package allowed to depend on it.
type EndpointProvisioner interface {
	ProvisionEndpoint(ctx context.Context, instanceID uuid.UUID, instanceName string, port int32, opts networking.EndpointOptions) (*networking.EndpointInfo, error)
	RevokeEndpoint(ctx context.Context, instanceID uuid.UUID) error
	// GetEndpointInfo reads back an instance's live endpoint state — used
	// by the status sweep (statusWithEndpoint) to catch the TLS-ready
	// transition, which ProvisionEndpoint cannot report at create time
	// (cert-manager issues asynchronously). See Stage 3 plan A6.
	GetEndpointInfo(ctx context.Context, instanceID uuid.UUID, instanceName string, opts networking.EndpointOptions) (*networking.EndpointInfo, error)
}

// Compile-time check that the concrete networking.Service satisfies the
// interface DirectClient now depends on.
var _ EndpointProvisioner = (*networking.Service)(nil)

// DirectClient talks to Kubernetes with client-go, for when the control
// plane runs beside the GPUs.
//
// This is the pre-split behaviour preserved behind the interface. It
// stays after the agent ships: single-node and local development
// deployments have no reason to run a gRPC round trip to reach a cluster
// on the same machine, and it is the reference implementation the agent
// is checked against.
type DirectClient struct {
	k8s        kubernetes.Interface
	networking EndpointProvisioner
	inventory  *gpu.Inventory

	// RuntimeClass for GPU pods. Empty disables it, for clusters where
	// nvidia is already containerd's default runtime.
	gpuRuntimeClass string

	// rest is required for ExecAttach — remotecommand.NewSPDYExecutor
	// needs a *rest.Config, which kubernetes.Interface alone cannot
	// provide. Nil until WithRESTConfig is called, in which case
	// ExecAttach returns ErrExecUnsupported rather than a nil-deref.
	rest *rest.Config

	// podCIDR is the cluster's pod network range, needed to write a
	// NetworkPolicy that blocks pod-to-pod traffic without also blocking
	// the node/ingress/internet (see buildNetworkPolicy). Defaults to
	// k3s's own default — every home node runs k3s today — overridable
	// via WithPodCIDR for a cluster configured differently.
	podCIDR string

	// metricsClient reads metrics.k8s.io (metrics-server) for per-pod
	// CPU/memory usage. Built alongside rest in WithRESTConfig — same
	// gate as ExecAttach: nil until a REST config is supplied, in which
	// case InstanceMetrics returns no data rather than a nil-deref.
	metricsClient metricsclientset.Interface

	// lastInstanceIO holds the previous sweep's network byte counters per
	// instance, so InstanceMetrics can compute a wall-clock RATE the same
	// way host-level network metrics do (see pkg/agentrunner/hostmetrics.go)
	// — /stats/summary reports cumulative counters, not a rate. Keyed by
	// instance ID rather than pod name/UID: a replaced pod (redeploy) is
	// the SAME instance to a customer, and starting its rate over from a
	// fresh pod's zero baseline for one cycle is correct, whereas keeping
	// a stale UID-keyed entry around after the old pod is gone would leak
	// unboundedly. Entries for instances no longer seen in a sweep are
	// pruned that same sweep — see pruneInstanceIOCache.
	lastInstanceIO   map[string]instanceIOSample
	lastInstanceIOMu sync.Mutex
}

const defaultPodCIDR = "10.42.0.0/16"

// Compile-time check. Cheap, and it fails the build rather than a
// request when the interface and implementation drift apart.
var _ Client = (*DirectClient)(nil)

// NewDirectClient builds a cluster client backed by client-go.
// networkingService may be nil, in which case instances get no public
// endpoint.
//
// Pass a concrete *networking.Service (or nil) here, never a variable of
// interface type holding a nil *networking.Service — an interface value
// wrapping a typed nil is non-nil under `== nil`, which is exactly the
// footgun already documented for PricingProvider in cmd/api-server/main.go.
// A literal `nil` (as cmd/teepin-agent/main.go's home-node path passes)
// remains safe; only an already-boxed nil is the hazard.
func NewDirectClient(k8s kubernetes.Interface, networkingService EndpointProvisioner, inventory *gpu.Inventory, gpuRuntimeClass string) *DirectClient {
	return &DirectClient{
		k8s:             k8s,
		networking:      networkingService,
		inventory:       inventory,
		gpuRuntimeClass: gpuRuntimeClass,
	}
}

// WithRESTConfig enables ExecAttach. A chaining setter rather than a
// NewDirectClient parameter — matching WithEndpointConfig/WithNodePlacer
// elsewhere in this codebase — so the dozen existing construction sites
// (including every fake.Clientset-backed test, which has no rest config
// to give) compile unchanged. Returns the same *DirectClient.
func (c *DirectClient) WithRESTConfig(cfg *rest.Config) *DirectClient {
	c.rest = cfg
	// Best-effort: a failure here (malformed config) leaves metricsClient
	// nil, and InstanceMetrics degrades to "no data" rather than this
	// constructor-time call failing the whole agent startup over a
	// secondary, non-critical capability.
	if mc, err := metricsclientset.NewForConfig(cfg); err == nil {
		c.metricsClient = mc
	} else {
		log.Printf("WARN: metrics.k8s.io client unavailable, instance metrics disabled: %v", err)
	}
	return c
}

// WithPodCIDR overrides the default pod network range used to build each
// instance's NetworkPolicy. Same chaining-setter shape as WithRESTConfig.
func (c *DirectClient) WithPodCIDR(cidr string) *DirectClient {
	c.podCIDR = cidr
	return c
}

// effectivePodCIDR returns the configured pod CIDR, or the k3s default
// when none was set — so an existing NewDirectClient call site that never
// calls WithPodCIDR still gets a correct, working NetworkPolicy rather
// than one built against an empty CIDR (which would match nothing,
// silently making the "except" clause a no-op and blocking everything
// including DNS and the node itself).
func (c *DirectClient) effectivePodCIDR() string {
	if c.podCIDR != "" {
		return c.podCIDR
	}
	return defaultPodCIDR
}

// endpointOptionsFor derives the networking override from a placement
// decision. spec.EnableTLS is a plain bool (not a pointer), so there is no
// wire-level way to distinguish "the caller wants TLS off" from "the
// caller said nothing" — only the true case is treated as an override;
// false always falls through to the agent's own configured default. This
// keeps a spec with EndpointDomain/EnableTLS/TLSIssuer left at their zero
// values byte-for-byte equivalent to calling ProvisionEndpoint with no
// options at all, which is what every datacenter request did before these
// fields were wired (Stage 3 plan A9) — nothing regresses for a caller
// that never sets them.
func endpointOptionsFor(spec InstanceSpec) networking.EndpointOptions {
	opts := networking.EndpointOptions{
		Domain:    spec.EndpointDomain,
		TLSIssuer: spec.TLSIssuer,
	}
	if spec.EnableTLS {
		t := true
		opts.UseTLS = &t
	}
	return opts
}

// CreateInstance builds the pod and, when ports are requested, its
// endpoint.
func (c *DirectClient) CreateInstance(ctx context.Context, spec InstanceSpec) (*InstanceResult, error) {
	// Volume before pod: buildPod references the PVC by name in its
	// Volumes spec, so the claim must exist (or already have existed —
	// IsAlreadyExists is success, matching every other idempotent create
	// in this file) before the pod is created, or the pod would sit in
	// ContainerCreating waiting on a claim that never arrives.
	if spec.StorageGB > 0 {
		pvc, pvcErr := buildPVC(spec)
		if pvcErr != nil {
			return nil, pvcErr
		}
		if _, err := c.k8s.CoreV1().PersistentVolumeClaims(workloadNamespace).Create(ctx, pvc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("create pvc: %w", err)
		}
	}

	pod, err := c.buildPod(spec)
	if err != nil {
		return nil, err
	}

	// Isolation before the pod goes live: NetworkPolicy selects by the
	// instance label the pod will carry, so creating it first (or
	// concurrently — order between these two doesn't matter, only that
	// both exist before traffic flows) means there is never a window
	// where a freshly-started pod is reachable pod-to-pod before its
	// policy attaches.
	netpol := buildNetworkPolicy(spec, c.effectivePodCIDR())
	if _, err := c.k8s.NetworkingV1().NetworkPolicies(workloadNamespace).Create(ctx, netpol, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("create network policy: %w", err)
	}

	created, err := c.k8s.CoreV1().Pods(workloadNamespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		// A GPU resource that vanished between the inventory snapshot and
		// this call is a lost race, not a client error: the caller should
		// reallocate against fresh inventory rather than fail the customer's
		// request. Kubernetes reports it as an invalid resource quantity.
		if apierrors.IsInvalid(err) && strings.Contains(err.Error(), spec.GPUResource) {
			return nil, fmt.Errorf("%w: %s", ErrResourceExhausted, spec.GPUResource)
		}
		return nil, fmt.Errorf("create pod: %w", err)
	}

	result := &InstanceResult{PodName: created.Name}

	// Endpoint provisioning is best-effort and deliberately non-fatal:
	// a running instance the customer can reach by other means beats one
	// deleted because DNS was slow. The reconciler fills the endpoint in
	// later.
	if len(spec.Ports) > 0 && c.networking != nil {
		instanceUUID, parseErr := uuid.Parse(spec.InstanceID)
		if parseErr != nil {
			// Instance IDs are "inst-<hex>", not UUIDs. The networking
			// service keys off the database UUID, which the caller passes
			// in Labels when it wants an endpoint.
			if raw, ok := spec.Labels[labelInstanceUUID]; ok {
				instanceUUID, parseErr = uuid.Parse(raw)
			}
		}
		if parseErr == nil {
			endpoint, epErr := c.networking.ProvisionEndpoint(
				ctx, instanceUUID, spec.InstanceID, int32(spec.Ports[0].Container),
				endpointOptionsFor(spec))
			if epErr == nil && endpoint != nil {
				// Prefer HTTPS, but report the HTTP URL while cert-manager
				// is still issuing — an endpoint the customer can reach now
				// beats a blank field that fills in minutes later.
				result.EndpointURL = endpoint.HTTPSURL
				if !endpoint.TLSReady || result.EndpointURL == "" {
					result.EndpointURL = endpoint.HTTPURL
				}
				result.PublicIP = endpoint.PublicIP
				// Carried on the result (not just used locally) so a caller
				// with no Kubernetes access of its own — the control plane,
				// in production — can persist and serve the full picture
				// without ever calling networking.Service itself. See
				// Stage 3 plan defects 1/2.
				result.DNSName = endpoint.DNSName
				result.TLSEnabled = endpoint.TLSEnabled
				result.TLSReady = endpoint.TLSReady
			}
		}
	}

	return result, nil
}

// UpdateInstance replaces an existing instance's pod in place: it deletes
// only the pod (never the Service, Ingress, PVC or NetworkPolicy — see
// DeleteInstance, which tears down all of those, for contrast), then
// calls CreateInstance again with the same spec. CreateInstance already
// tolerates every one of those other resources still existing
// (PVC/NetworkPolicy creation is IsAlreadyExists-tolerant, and so is
// Service/Ingress creation inside networking.Service) and the fresh pod
// carries the SAME app.teepin.cloud/instance-id label the existing
// Service already selects on — so once the new pod is Ready, traffic
// reaches it automatically with no DNS/TLS/Ingress change at all.
//
// Returns ErrNotFound if the instance does not exist in scope, rather
// than silently falling through to CreateInstance and provisioning a
// fresh, orphaned instance under an ID nothing else points to.
func (c *DirectClient) UpdateInstance(ctx context.Context, scope Scope, spec InstanceSpec) (*InstanceResult, error) {
	pods, err := c.k8s.CoreV1().Pods(workloadNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: instanceSelector(scope, spec.InstanceID),
	})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return nil, ErrNotFound
	}

	for _, pod := range pods.Items {
		if delErr := c.k8s.CoreV1().Pods(workloadNamespace).Delete(
			ctx, pod.Name, metav1.DeleteOptions{}); delErr != nil && !apierrors.IsNotFound(delErr) {
			return nil, fmt.Errorf("delete pod %s: %w", pod.Name, delErr)
		}
	}

	return c.CreateInstance(ctx, spec)
}

// ResolveInstanceAddress returns instanceID's pod IP, for the Stage 3
// tunnel's agent-side proxy handler. Deliberately the pod IP, not a
// Service ClusterIP — a home node has no Service at all (networking is nil
// there; see homeClusterClient in cmd/teepin-agent), and the datacenter
// path's Service exists only for the public Ingress path, which is a
// different concern from "where does this agent send a proxied request
// locally". One lookup mechanism covers both node classes.
//
// AllTenants scope: the agent proxies whatever instance ID the control
// plane told it to (already resolved and authorized upstream, at the
// hostname->provider lookup in pkg/cluster's ProxyHandler) — the agent has
// no tenancy of its own to check.
func (c *DirectClient) ResolveInstanceAddress(ctx context.Context, instanceID string, port int32) (string, error) {
	pods, err := c.k8s.CoreV1().Pods(workloadNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: instanceSelector(AllTenants(), instanceID),
	})
	if err != nil {
		return "", fmt.Errorf("list pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return "", ErrNotFound
	}
	ip := pods.Items[0].Status.PodIP
	if ip == "" {
		// Pod exists but has no IP yet (still scheduling/starting) —
		// distinct from "does not exist", but the caller only has one
		// error to work with; ErrNotFound is the closer of the two (both
		// mean "cannot proxy to it right now").
		return "", ErrNotFound
	}
	return fmt.Sprintf("%s:%d", ip, port), nil
}

// instanceSelector builds the label selector for one instance within a
// scope.
//
// The scope predicate goes into the selector rather than being applied
// to the results: an out-of-scope instance is then invisible at the API
// level, so there is no window in which the wrong pod has been fetched
// and might be acted on.
func instanceSelector(scope Scope, instanceID string) string {
	selector := fmt.Sprintf("%s=%s", labelInstanceID, instanceID)
	return appendScope(selector, scope)
}

// managedSelector matches every TEEPIN-managed instance in a scope, for
// LISTING — excludes Kumbha's own agent pods (see labelKumbhaAgent) by
// default, unlike instanceSelector: a caller that already knows a pod's
// exact instance ID (CloseSession's teardown, the event relay's
// StreamLogs) must still find it. Only the customer-facing list must
// not — see Scope.IncludeHidden's own doc comment for the one caller
// (the home-node agent's own status-reporting sweep) that needs these
// pods included, not excluded.
func managedSelector(scope Scope) string {
	selector := fmt.Sprintf("%s=true", labelManaged)
	if !scope.IncludeHidden {
		selector += fmt.Sprintf(",%s!=true", labelKumbhaAgent)
	}
	return appendScope(selector, scope)
}

func appendScope(selector string, scope Scope) string {
	if scope.ProjectID != "" {
		selector += fmt.Sprintf(",%s=%s", labelProjectID, scope.ProjectID)
	}
	if scope.AccountID != "" {
		selector += fmt.Sprintf(",%s=%s", labelAccountID, scope.AccountID)
	}
	return selector
}

// DeleteInstance removes the pod and any endpoint. Missing resources are
// not an error: commands may be redelivered after an agent reconnects,
// and a delete that finds nothing has achieved its purpose.
func (c *DirectClient) DeleteInstance(ctx context.Context, scope Scope, instanceID string) error {
	pods, err := c.k8s.CoreV1().Pods(workloadNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: instanceSelector(scope, instanceID),
	})
	if err != nil {
		return fmt.Errorf("list pods: %w", err)
	}

	// A Kumbha agent pod's PVC is its customer's actual workspace — the
	// files it wrote, meant to survive the pod being killed and relaunched
	// (StopAgent's own doc comment: "Whatever the agent had written to the
	// workspace (PVC) up to the moment of the kill is kept"). But every
	// path that tears down that pod (StopAgent's hard-kill, LaunchAgent's
	// own launch-failed cleanup) goes through this SAME DeleteInstance a
	// customer's own compute-instance deletion uses — and that one
	// correctly DOES wipe storage, since the customer is done with it for
	// good. Found live 2026-08-31: a relaunched agent found its own
	// workspace completely empty — no app code, no Dockerfile, nothing
	// from a build that had deployed successfully hours earlier — because
	// this method deleted the PVC unconditionally, silently breaking the
	// promise StopAgent's own comment makes. Detected via the pod's own
	// label (the same teepin.io/kumbha-agent marker LaunchAgent stamps on
	// it and ListInstanceStatuses already filters on — an established
	// precedent for this exact kind of special-casing, not a new one) so
	// a customer's ordinary instance delete is entirely unaffected.
	isKumbhaAgent := false
	for _, pod := range pods.Items {
		if pod.Labels[labelKumbhaAgent] == "true" {
			isKumbhaAgent = true
		}
		if delErr := c.k8s.CoreV1().Pods(workloadNamespace).Delete(
			ctx, pod.Name, metav1.DeleteOptions{}); delErr != nil && !apierrors.IsNotFound(delErr) {
			return fmt.Errorf("delete pod %s: %w", pod.Name, delErr)
		}

		// Endpoint teardown keyed off the UUID label written at creation.
		if c.networking != nil {
			if raw, ok := pod.Labels[labelInstanceUUID]; ok {
				if instanceUUID, parseErr := uuid.Parse(raw); parseErr == nil {
					_ = c.networking.RevokeEndpoint(ctx, instanceUUID)
				}
			}
		}
	}

	// PVC and NetworkPolicy are named deterministically from instanceID
	// (see pvcName/networkPolicyName), so no lookup is needed — just
	// delete by name, same idempotent IsNotFound-is-success idiom as the
	// pod delete above. A PVC delete when StorageGB was never set simply
	// finds nothing, which is not an error.
	if !isKumbhaAgent {
		if err := c.k8s.CoreV1().PersistentVolumeClaims(workloadNamespace).Delete(
			ctx, pvcName(instanceID), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete pvc: %w", err)
		}
	}
	if err := c.k8s.NetworkingV1().NetworkPolicies(workloadNamespace).Delete(
		ctx, networkPolicyName(instanceID), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete network policy: %w", err)
	}

	return nil
}

// GetInstanceStatus reports one instance's live status.
func (c *DirectClient) GetInstanceStatus(ctx context.Context, scope Scope, instanceID string) (*InstanceStatus, error) {
	pods, err := c.k8s.CoreV1().Pods(workloadNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: instanceSelector(scope, instanceID),
	})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return nil, ErrNotFound
	}

	status := c.statusWithEndpoint(ctx, &pods.Items[0])
	return &status, nil
}

// ErrExecUnsupported means this client cannot attach an interactive
// session — no *rest.Config was ever supplied (WithRESTConfig), which is
// the case for every client until the agent's exec support ships.
var ErrExecUnsupported = errors.New("interactive exec is not supported on this node")

// Compile-time check that DirectClient satisfies ExecCapable.
var _ ExecCapable = (*DirectClient)(nil)

// ExecAttach implements ExecCapable. It resolves instance_id to a live
// pod exactly like GetInstanceStatus (list by instanceSelector,
// AllTenants — the agent has no tenancy view of its own and trusts the
// control plane's check, same as every other agent-side operation),
// then attaches via Kubernetes' pods/exec subresource.
//
// ioStreams.OnOpen fires once pod+container are resolved, before any
// shell is attempted — it reports "attached to this pod/container", not
// "a shell exists there". A distroless image still gets an OnOpen
// followed quickly by an EXEC_UNSUPPORTED end, which is correct: the
// customer WAS attached to their container, it just has no shell.
func (c *DirectClient) ExecAttach(ctx context.Context, req ExecRequest, ioStreams ExecIO) (ExecOutcome, error) {
	if c.rest == nil {
		return ExecOutcome{}, ErrExecUnsupported
	}

	pods, err := c.k8s.CoreV1().Pods(workloadNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: instanceSelector(AllTenants(), req.InstanceID),
	})
	if err != nil {
		return ExecOutcome{}, fmt.Errorf("list pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return ExecOutcome{}, ErrNotFound
	}
	pod := pods.Items[0]
	if pod.Status.Phase != corev1.PodRunning {
		return ExecOutcome{}, fmt.Errorf("instance is %s, not running", strings.ToLower(string(pod.Status.Phase)))
	}

	container := req.Container
	if container == "" {
		if len(pod.Spec.Containers) == 0 {
			return ExecOutcome{}, fmt.Errorf("pod has no containers")
		}
		container = pod.Spec.Containers[0].Name
	}
	running := false
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == container && cs.State.Running != nil {
			running = true
			break
		}
	}
	if !running {
		return ExecOutcome{}, fmt.Errorf("container %q is not running", container)
	}

	if ioStreams.OnOpen != nil {
		ioStreams.OnOpen(pod.Name, container)
	}

	probeShell := len(req.Command) == 0
	command := req.Command
	if probeShell {
		command = []string{"/bin/bash", "-i"}
	}

	outcome, err := c.streamExec(ctx, pod.Name, container, command, req, ioStreams)
	if err != nil && probeShell && isMissingShellError(err) {
		outcome, err = c.streamExec(ctx, pod.Name, container, []string{"/bin/sh", "-i"}, req, ioStreams)
		if err != nil && isMissingShellError(err) {
			return ExecOutcome{}, fmt.Errorf("%w: no shell found (tried /bin/bash, /bin/sh)", ErrExecUnsupported)
		}
	}
	return outcome, err
}

// streamExec makes one pods/exec attempt with a specific command. Split
// out of ExecAttach so the /bin/bash -> /bin/sh fallback is a plain
// retry rather than duplicated setup.
func (c *DirectClient) streamExec(ctx context.Context, podName, container string, command []string, req ExecRequest, ioStreams ExecIO) (ExecOutcome, error) {
	// The Kubernetes API server rejects a stderr stream alongside a tty
	// (400) — enforce this here rather than trusting the caller.
	stderr := ioStreams.Stderr
	if req.TTY {
		stderr = nil
	}

	execReq := c.k8s.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(workloadNamespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdin:     true,
			Stdout:    true,
			Stderr:    stderr != nil,
			TTY:       req.TTY,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(c.rest, "POST", execReq.URL())
	if err != nil {
		return ExecOutcome{}, fmt.Errorf("build exec executor: %w", err)
	}

	var sizeQ *terminalSizeQueue
	var streamSizeQueue remotecommand.TerminalSizeQueue
	if req.TTY {
		// Seeded with the initial size: client-go calls Next() immediately,
		// and an unseeded queue renders every full-screen program at 0x0.
		sizeQ = newTerminalSizeQueue(remotecommand.TerminalSize{Width: req.Cols, Height: req.Rows})
		streamSizeQueue = sizeQ
		if ioStreams.Resize != nil {
			resizeDone := make(chan struct{})
			defer close(resizeDone)
			go func() {
				for {
					select {
					case sz, ok := <-ioStreams.Resize:
						if !ok {
							return
						}
						sizeQ.push(remotecommand.TerminalSize{Width: sz.Cols, Height: sz.Rows})
					case <-resizeDone:
						return
					}
				}
			}()
		}
	}

	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:             ioStreams.Stdin,
		Stdout:            ioStreams.Stdout,
		Stderr:            stderr,
		Tty:               req.TTY,
		TerminalSizeQueue: streamSizeQueue,
	})
	if sizeQ != nil {
		sizeQ.close()
	}

	if err != nil {
		// A non-zero shell exit is a normal, successful exec SESSION
		// outcome, not a platform error — ExecEnd.exit_code is exactly
		// for this.
		if exitErr, ok := err.(clientexec.ExitError); ok {
			return ExecOutcome{ExitCode: exitErr.ExitStatus()}, nil
		}
		return ExecOutcome{}, err
	}
	return ExecOutcome{ExitCode: 0}, nil
}

// isMissingShellError reports whether err looks like "the requested
// executable does not exist in the image" rather than some other exec
// failure. Runtime-dependent (containerd and cri-o phrase this
// differently), so this matches on the two shapes actually observed:
// an ExitError with the shell's conventional 126/127 status, or a
// stream-setup error whose text names the failure.
func isMissingShellError(err error) bool {
	if err == nil {
		return false
	}
	if exitErr, ok := err.(clientexec.ExitError); ok {
		code := exitErr.ExitStatus()
		return code == 126 || code == 127
	}
	msg := err.Error()
	for _, needle := range []string{
		"executable file not found",
		"no such file or directory",
		"OCI runtime exec failed",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// terminalSizeQueue implements remotecommand.TerminalSizeQueue over a
// 1-slot, replace-not-block channel: a resize mid-drag must never block
// the demux goroutine feeding it, and only the latest pending size
// matters (last-write-wins). Single producer (the caller's demux
// goroutine via push), single consumer (client-go's own resize-watcher
// goroutine via Next) — no additional locking needed.
type terminalSizeQueue struct {
	ch chan remotecommand.TerminalSize
}

func newTerminalSizeQueue(initial remotecommand.TerminalSize) *terminalSizeQueue {
	q := &terminalSizeQueue{ch: make(chan remotecommand.TerminalSize, 1)}
	q.ch <- initial
	return q
}

func (q *terminalSizeQueue) Next() *remotecommand.TerminalSize {
	sz, ok := <-q.ch
	if !ok {
		return nil // ends client-go's resize-watcher goroutine
	}
	return &sz
}

func (q *terminalSizeQueue) push(sz remotecommand.TerminalSize) {
	select {
	case q.ch <- sz:
		return
	default:
	}
	// Full: drop the stale pending value, then push the new one. Best
	// effort — if a Next() call wins the race for the slot in between,
	// the new size is simply dropped and picked up by the next resize.
	select {
	case <-q.ch:
	default:
	}
	select {
	case q.ch <- sz:
	default:
	}
}

func (q *terminalSizeQueue) close() {
	close(q.ch)
}

// ListInstanceStatuses returns every TEEPIN-managed instance in the
// cluster. The reconciler compares this against the database to find
// instances that vanished — a node reboot or eviction leaves the database
// billing for a pod that no longer exists.
func (c *DirectClient) ListInstanceStatuses(ctx context.Context, scope Scope) ([]InstanceStatus, error) {
	pods, err := c.k8s.CoreV1().Pods(workloadNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: managedSelector(scope),
	})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}

	statuses := make([]InstanceStatus, 0, len(pods.Items))
	for i := range pods.Items {
		statuses = append(statuses, c.statusWithEndpoint(ctx, &pods.Items[i]))
	}
	return statuses, nil
}

// statusWithEndpoint reports podStatus plus, when the pod has a
// provisioned endpoint, its current DNS/TLS state — this is what lets a
// TLS-ready flip (cert-manager finishing issuance 30-90s after create)
// reach the control plane on the next sweep instead of never at all
// (Stage 3 plan A6). Best-effort: a pod with no UUID label (never had an
// endpoint) or a lookup failure just reports the bare pod status, same as
// before this method existed.
func (c *DirectClient) statusWithEndpoint(ctx context.Context, pod *corev1.Pod) InstanceStatus {
	status := podStatus(pod)
	if c.networking == nil {
		return status
	}
	raw, ok := pod.Labels[labelInstanceUUID]
	if !ok {
		return status
	}
	instanceUUID, err := uuid.Parse(raw)
	if err != nil {
		return status
	}
	// EndpointOptions{} here (not the create-time spec's options) is safe
	// ONLY because every instance created through this agent's DirectClient
	// shares one server-wide domain/TLS config today (server.go's
	// endpointDomain/enableTLS/tlsIssuer, set once via WithEndpointConfig) —
	// there is no per-instance divergence yet, so the sweep's resolved
	// defaults always match what create-time actually used. If a future
	// change makes EndpointDomain/EnableTLS genuinely vary per instance
	// (this DirectClient never runs Phase B's home path, which synthesizes
	// endpoints elsewhere — see AgentClient.CreateInstance for home nodes —
	// but a future per-project domain override would reach here too), this
	// call must instead resolve the SAME options create-time used, which
	// means persisting them (e.g. as pod annotations) rather than
	// recomputing from current agent config.
	endpoint, err := c.networking.GetEndpointInfo(ctx, instanceUUID, status.InstanceID, networking.EndpointOptions{})
	if err != nil || endpoint == nil {
		return status
	}
	status.EndpointURL = endpoint.HTTPSURL
	if !endpoint.TLSReady || status.EndpointURL == "" {
		status.EndpointURL = endpoint.HTTPURL
	}
	status.DNSName = endpoint.DNSName
	status.PublicIP = endpoint.PublicIP
	status.TLSEnabled = endpoint.TLSEnabled
	status.TLSReady = endpoint.TLSReady
	return status
}

// StreamLogs copies container logs to w.
func (c *DirectClient) StreamLogs(ctx context.Context, scope Scope, instanceID string, opts LogOptions, w io.Writer) error {
	pods, err := c.k8s.CoreV1().Pods(workloadNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: instanceSelector(scope, instanceID),
	})
	if err != nil {
		return fmt.Errorf("list pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return ErrNotFound
	}

	logOpts := &corev1.PodLogOptions{
		Follow:     opts.Follow,
		Timestamps: opts.Timestamps,
	}
	if opts.TailLines > 0 {
		tail := int64(opts.TailLines)
		logOpts.TailLines = &tail
	}

	stream, err := c.k8s.CoreV1().Pods(workloadNamespace).
		GetLogs(pods.Items[0].Name, logOpts).Stream(ctx)
	if err != nil {
		return fmt.Errorf("open log stream: %w", err)
	}
	defer stream.Close()

	// io.Copy rather than a scanner: logs are bytes, and a scanner would
	// impose a line-length limit on output we do not control.
	_, err = io.Copy(w, stream)
	if err != nil && ctx.Err() == nil {
		return fmt.Errorf("stream logs: %w", err)
	}
	return nil
}

// Inventory reports GPU capacity, translating the gpu package's view
// into the transport-neutral shape the interface promises.
func (c *DirectClient) Inventory(ctx context.Context) ([]NodeInventory, error) {
	if c.inventory == nil {
		return nil, nil
	}

	nodes, err := c.inventory.Snapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("gpu inventory: %w", err)
	}

	// Readiness is not part of the gpu package's view — it reports
	// capacity from labels and allocatable resources, which a cordoned or
	// NotReady node still advertises. Placing work there would schedule
	// against a node that cannot accept it, so readiness is resolved here
	// and defaults to false when it cannot be determined.
	ready := c.nodeReadiness(ctx)

	out := make([]NodeInventory, 0, len(nodes))
	for _, n := range nodes {
		ni := NodeInventory{
			NodeName:       n.NodeName,
			GPUProduct:     n.Product,
			GPUModel:       n.Model,
			MemoryGBPerGPU: n.MemoryGBPerGPU,
			GPUCount:       n.GPUCount,
			MIGCapable:     n.MIGCapable,
			SharedCapacity: n.SharedCapacity,
			SharedUsed:     n.SharedUsed,
			UsedVRAMGB:     n.UsedVRAMGB,
			// Simulated nodes have no counterpart in the node list (they
			// are synthesised for local development), so a lookup would
			// always report them unready and make the local environment
			// refuse every instance.
			Ready: n.Simulated || ready[n.NodeName],
		}
		for _, m := range n.MIGResources {
			ni.MIGResources = append(ni.MIGResources, MIGResource{
				ResourceName: m.ResourceName,
				Profile:      m.Profile,
				Slices:       m.Slices,
				MemoryGB:     m.MemoryGB,
				Capacity:     m.Capacity,
				Used:         m.Used,
			})
		}
		out = append(out, ni)
	}
	return out, nil
}

// nodeReadiness maps node name to schedulability: Ready condition true
// and not cordoned.
//
// Returns an empty map on error, which marks every node unready. That is
// the safe direction — refusing to place work when readiness is unknown
// beats scheduling onto a node that cannot run it.
func (c *DirectClient) nodeReadiness(ctx context.Context) map[string]bool {
	ready := map[string]bool{}

	if c.k8s == nil {
		return ready
	}

	nodes, err := c.k8s.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return ready
	}

	for i := range nodes.Items {
		node := &nodes.Items[i]
		if node.Spec.Unschedulable {
			ready[node.Name] = false
			continue
		}
		for _, cond := range node.Status.Conditions {
			if cond.Type == corev1.NodeReady {
				ready[node.Name] = cond.Status == corev1.ConditionTrue
				break
			}
		}
	}

	return ready
}

// Healthy reports whether the Kubernetes API is reachable.
//
// Deliberately a cheap call with a short timeout: this runs on the
// request path to decide whether to accept new instances, so it must not
// block a health check behind a slow cluster.
func (c *DirectClient) Healthy(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	_, err := c.k8s.CoreV1().Nodes().List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		// Reported to the control plane as a plain bool (GPUInventory's
		// cluster_ready field, see agentrunner.reportInventory) — WHY it
		// failed is otherwise invisible on both sides. This showed up as a
		// live "not schedulable" investigation (2026-08-19) that had
		// nothing but a bare false to go on. Called every inventoryInterval
		// (30s), so this logs at most once per interval, never a hot loop.
		log.Printf("cluster health check failed: %v", err)
		return false
	}
	return true
}

// buildPod translates a placement decision into a pod definition.
//
// Every non-obvious field here was a production failure at some point;
// the comments say which.
func (c *DirectClient) buildPod(spec InstanceSpec) (*corev1.Pod, error) {
	labels := map[string]string{
		labelManaged:    "true",
		labelInstanceID: spec.InstanceID,
		// Selected on by the networking Service. The pod carries it so
		// the Service created alongside can find it.
		labelInstanceShort: spec.InstanceID,
	}

	// Caller labels first, so the tenancy labels below cannot be
	// overridden by a customer-supplied label of the same name.
	for k, v := range spec.Labels {
		labels[k] = v
	}

	// Tenancy labels are the predicate every scoped read filters on.
	// Written last and unconditionally: a pod without them is invisible
	// to its owner and, worse, visible to an unscoped query.
	if spec.AccountID != "" {
		labels[labelAccountID] = spec.AccountID
	}
	if spec.ProjectID != "" {
		labels[labelProjectID] = spec.ProjectID
	}

	cpu, err := resource.ParseQuantity(strconv.Itoa(spec.CPUUnits))
	if err != nil {
		return nil, fmt.Errorf("invalid cpu units %d: %w", spec.CPUUnits, err)
	}
	mem, err := resource.ParseQuantity(fmt.Sprintf("%dGi", spec.MemoryGB))
	if err != nil {
		return nil, fmt.Errorf("invalid memory %dGB: %w", spec.MemoryGB, err)
	}

	// Ephemeral-storage limit: a safety guard rail, not a customer choice.
	// Found live 2026-08-21: buildPod set no storage limit at all, so a
	// workload's writable layer was backed directly by the host's full
	// disk (1007G on Srialla for a "2 vCPU / 4GB" instance) — any single
	// instance could exhaust a shared home node's disk and take down k3s,
	// the agent, and every other tenant on it. EphemeralStorageGB is
	// always > 0 by the time it reaches here (callers default it), but
	// zero is tolerated as "no limit set" rather than parsed into a
	// zero-quantity limit, which Kubernetes would treat as "must not use
	// any disk at all" and evict the pod immediately.
	var ephemeral resource.Quantity
	hasEphemeral := spec.EphemeralStorageGB > 0
	if hasEphemeral {
		ephemeral, err = resource.ParseQuantity(fmt.Sprintf("%dGi", spec.EphemeralStorageGB))
		if err != nil {
			return nil, fmt.Errorf("invalid ephemeral storage %dGB: %w", spec.EphemeralStorageGB, err)
		}
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", spec.InstanceID, uuid.New().String()[:5]),
			Namespace: workloadNamespace,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			// Tenant isolation: customer workloads must never carry
			// credentials for the Kubernetes API. Network policy alone
			// cannot guarantee the API is unreachable (kube-proxy DNATs the
			// service VIP to node addresses, and IPv4 ipBlock rules do not
			// cover IPv6 nodes) — without a token, reaching it gains
			// nothing.
			AutomountServiceAccountToken: boolPtr(false),
			// Pod-level hardening, added 2026-08-22 alongside the NetworkPolicy
			// this pod is also given (see the caller of buildPod). Deliberately
			// NOT RunAsNonRoot: the stock nginx image (and most base images)
			// run as root and bind port 80, and this platform does not control
			// what a customer's image does internally — enforcing non-root
			// would break the exact workload running in production. Privilege
			// escalation and every capability except binding a low port are
			// still denied regardless of what the image asks for.
			SecurityContext: &corev1.PodSecurityContext{
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			// A bare Pod defaults to RestartPolicy: Always when left
			// unset — correct for a customer's persistent compute
			// instance, catastrophic for a one-shot workload (see
			// InstanceSpec.NeverRestart's own doc comment for the live
			// incident this guards against: Kumbha's agent pod silently
			// restarting mid-build and re-running the whole thing from
			// scratch against the same prompt, on repeat).
			RestartPolicy: func() corev1.RestartPolicy {
				if spec.NeverRestart {
					return corev1.RestartPolicyNever
				}
				return corev1.RestartPolicyAlways
			}(),
			Containers: []corev1.Container{
				{
					Name: "app",
					// nil leaves the image's own ENTRYPOINT/CMD in place.
					// Silently dropping these once left base images
					// crash-looping with no explanation.
					Command: spec.Command,
					Args:    spec.Args,
					Image:   spec.Image,
					ImagePullPolicy: func() corev1.PullPolicy {
						if spec.AlwaysPullImage {
							return corev1.PullAlways
						}
						return "" // API server defaults this by tag, as before
					}(),
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: boolPtr(false),
						Capabilities: &corev1.Capabilities{
							Drop: []corev1.Capability{"ALL"},
							// NET_BIND_SERVICE re-added deliberately: without
							// it, an image that binds a port below 1024 as
							// root (nginx's default config does exactly this)
							// fails to start once every other capability is
							// dropped. The filesystem-ownership set is added
							// ONLY when the caller explicitly asked for it
							// (see InstanceSpec.AllowFilesystemOwnershipChanges'
							// own doc comment) — every other workload keeps
							// the fully locked-down set.
							Add: append([]corev1.Capability{"NET_BIND_SERVICE"}, filesystemOwnershipCapabilities(spec)...),
						},
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    cpu,
							corev1.ResourceMemory: mem,
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    cpu,
							corev1.ResourceMemory: mem,
						},
					},
				},
			},
		},
	}

	if hasEphemeral {
		pod.Spec.Containers[0].Resources.Requests[corev1.ResourceEphemeralStorage] = ephemeral
		pod.Spec.Containers[0].Resources.Limits[corev1.ResourceEphemeralStorage] = ephemeral
	}

	// Persistent volume: customer-chosen size, billed by GB-month (see
	// pkg/billing). Deleted with the instance — reattaching a volume to a
	// later instance is a possible future feature, not built. On a home
	// node this is k3s's local-path provisioner: the data lives on that
	// node's own disk and is unreachable while the node is offline, a
	// materially different durability story than datacenter network
	// storage. The console must say so; buildPod only wires the mount.
	if spec.StorageGB > 0 {
		const volumeName = "data"
		pod.Spec.Volumes = []corev1.Volume{
			{
				Name: volumeName,
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: pvcName(spec.InstanceID),
					},
				},
			},
		}
		pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{
			{Name: volumeName, MountPath: "/data"},
		}
	}

	if spec.ImagePullSecret != "" {
		pod.Spec.ImagePullSecrets = []corev1.LocalObjectReference{
			{Name: spec.ImagePullSecret},
		}
	}

	// VRAM annotations drive capacity accounting (inventory) and billing
	// reconciliation. They are keyed off allocated VRAM rather than the
	// extended resource name, because a simulated allocation on a
	// local-dev node has VRAM to account for but requests no device — and
	// dropping the annotations there would silently break local capacity
	// accounting.
	if spec.GPUVRAMGB > 0 {
		pod.ObjectMeta.Annotations = map[string]string{
			gpu.AnnotationVRAMGB: strconv.Itoa(spec.GPUVRAMGB),
		}
		if spec.GPUResource != "" {
			pod.ObjectMeta.Annotations[gpu.AnnotationGPUResource] = spec.GPUResource
		}
		if spec.InstanceType != "" {
			pod.ObjectMeta.Annotations[annotationInstanceType] = spec.InstanceType
		}

		// Pin to the node whose capacity the allocator accounted against.
		// Without this the scheduler may place the pod elsewhere and the
		// accounting describes a node the workload never landed on.
		if spec.NodeName != "" {
			pod.Spec.NodeSelector = map[string]string{
				"kubernetes.io/hostname": spec.NodeName,
			}
		}
	}

	// A real GPU device was requested (as opposed to a simulated
	// allocation, which accounts VRAM but requests no hardware).
	if spec.GPUResource != "" {
		// GPU pods MUST run under the NVIDIA container runtime. Without it
		// containerd starts the container with plain runc: Kubernetes still
		// accounts for the GPU resource, the device nodes and driver
		// libraries are never injected, and the customer gets a container
		// with no usable GPU — billed, but unusable. This was found only by
		// exec'ing into a running container.
		if c.gpuRuntimeClass != "" {
			rc := c.gpuRuntimeClass
			pod.Spec.RuntimeClassName = &rc
		}

		qty, qErr := resource.ParseQuantity(strconv.Itoa(spec.GPUQuantity))
		if qErr != nil {
			return nil, fmt.Errorf("invalid gpu quantity %d: %w", spec.GPUQuantity, qErr)
		}
		pod.Spec.Containers[0].Resources.Limits[corev1.ResourceName(spec.GPUResource)] = qty
	}

	if len(spec.Env) > 0 {
		envVars := make([]corev1.EnvVar, 0, len(spec.Env))
		for k, v := range spec.Env {
			envVars = append(envVars, corev1.EnvVar{Name: k, Value: v})
		}
		pod.Spec.Containers[0].Env = envVars
	}

	if len(spec.Ports) > 0 {
		ports := make([]corev1.ContainerPort, 0, len(spec.Ports))
		for _, p := range spec.Ports {
			proto := corev1.ProtocolTCP
			if strings.EqualFold(p.Protocol, "udp") {
				proto = corev1.ProtocolUDP
			}
			ports = append(ports, corev1.ContainerPort{
				ContainerPort: int32(p.Container),
				Protocol:      proto,
			})
		}
		pod.Spec.Containers[0].Ports = ports
	}

	return pod, nil
}

// pvcName and networkPolicyName are deterministic from the instance ID so
// create/delete never need to look anything up first — they just name the
// object and act.
func pvcName(instanceID string) string           { return PVCName(instanceID) }
func networkPolicyName(instanceID string) string { return "netpol-" + instanceID }

// PVCName is pvcName exported: callers outside this package that know an
// instance ID (e.g. pkg/build, mounting a Kumbha agent's workspace volume
// into a Kaniko pod) need to derive the same PVC name buildPod already
// mounts, without duplicating the "pvc-" convention a second time where
// it could silently drift from this one.
func PVCName(instanceID string) string { return "pvc-" + instanceID }

// buildPVC builds the PersistentVolumeClaim for an instance's /data mount
// (see buildPod). StorageClassName is left nil so it resolves to the
// cluster's default — k3s's local-path provisioner on a home node,
// whatever a datacenter cluster defines otherwise. Carries the same
// tenancy labels as the pod so a scoped list/delete of PVCs is possible
// later without a schema change.
func buildPVC(spec InstanceSpec) (*corev1.PersistentVolumeClaim, error) {
	size, err := resource.ParseQuantity(fmt.Sprintf("%dGi", spec.StorageGB))
	if err != nil {
		return nil, fmt.Errorf("invalid storage size %dGB: %w", spec.StorageGB, err)
	}
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName(spec.InstanceID),
			Namespace: workloadNamespace,
			Labels: map[string]string{
				labelManaged:       "true",
				labelInstanceShort: spec.InstanceID,
				labelInstanceID:    spec.InstanceID,
				labelAccountID:     spec.AccountID,
				labelProjectID:     spec.ProjectID,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: size},
			},
		},
	}, nil
}

// buildNetworkPolicy default-denies pod-to-pod traffic for one instance
// while leaving everything else open — the node (the agent proxies tunnel
// requests to the pod IP from a host process, outside the pod network),
// the ingress controller, and the public internet.
//
// NetworkPolicy is allow-list once a pod is selected by any policy, so
// every direction actually needed must be listed explicitly:
//   - Ingress: 0.0.0.0/0 except the pod CIDR — blocks pod-to-pod inbound,
//     permits the node and anything routing in from outside the cluster.
//   - Egress: CoreDNS first, by name (kube-dns's pod IP is itself inside
//     the pod CIDR — a naive "except pod CIDR" block breaks ALL name
//     resolution, the classic NetworkPolicy footgun), then 0.0.0.0/0
//     except the pod CIDR and the cloud metadata address.
//
// podCIDR is operator-configured (TEEPIN_POD_CIDR) because it varies by
// cluster — k3s defaults to 10.42.0.0/16, confirmed against a live
// instance's pod IP (10.42.0.64) this session; a datacenter cluster would
// supply its own.
func buildNetworkPolicy(spec InstanceSpec, podCIDR string) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	dnsPort := intstr.FromInt(53)

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      networkPolicyName(spec.InstanceID),
			Namespace: workloadNamespace,
			Labels: map[string]string{
				labelManaged:       "true",
				labelInstanceShort: spec.InstanceID,
				labelInstanceID:    spec.InstanceID,
				labelAccountID:     spec.AccountID,
				labelProjectID:     spec.ProjectID,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{labelInstanceShort: spec.InstanceID},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					From: []networkingv1.NetworkPolicyPeer{
						{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: []string{podCIDR}}},
					},
				},
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To: []networkingv1.NetworkPolicyPeer{
						{
							NamespaceSelector: &metav1.LabelSelector{},
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"k8s-app": "kube-dns"},
							},
						},
					},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &udp, Port: &dnsPort},
						{Protocol: &tcp, Port: &dnsPort},
					},
				},
				{
					To: []networkingv1.NetworkPolicyPeer{
						{IPBlock: &networkingv1.IPBlock{
							CIDR:   "0.0.0.0/0",
							Except: []string{podCIDR, "169.254.169.254/32"},
						}},
					},
				},
			},
		},
	}
}

// podStatus maps a pod to the interface's status view.
//
// The mapping is not the identity: Kubernetes phases describe pods,
// TEEPIN statuses describe what a customer is billed for. A pod stuck on
// ImagePullBackOff is Pending to Kubernetes but failed to us — nobody
// should be charged for a container that will never start.
func podStatus(pod *corev1.Pod) InstanceStatus {
	st := InstanceStatus{
		InstanceID: pod.Labels[labelInstanceID],
		PodName:    pod.Name,
		NodeName:   pod.Spec.NodeName,
		ObservedAt: time.Now().UTC(),
		// Reported so both implementations return the same shape, even
		// though this one has already enforced tenancy via the selector.
		AccountID: pod.Labels[labelAccountID],
		ProjectID: pod.Labels[labelProjectID],
	}

	switch pod.Status.Phase {
	case corev1.PodRunning:
		st.Status = "running"
	case corev1.PodSucceeded:
		st.Status = "terminated"
	case corev1.PodFailed:
		st.Status = "failed"
		st.Message = pod.Status.Reason
	default:
		st.Status = "pending"
	}

	// Surface the waiting reason, and treat unrecoverable image problems
	// as failure rather than leaving the instance pending forever.
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting == nil {
			continue
		}
		reason := cs.State.Waiting.Reason
		st.Message = cs.State.Waiting.Message
		switch reason {
		case "ImagePullBackOff", "ErrImagePull", "InvalidImageName":
			st.Status = "failed"
			if st.Message == "" {
				st.Message = reason
			}
		case "CrashLoopBackOff":
			st.Status = "failed"
			if st.Message == "" {
				st.Message = reason
			}
		}
	}

	// A container that actually started and then exited non-zero (by far
	// the common case for a build script failing) never hits the Waiting
	// branch above — it has State.Terminated, not State.Waiting, by the
	// time the pod is observed as PodFailed. pod.Status.Reason (set
	// above) is a pod-level field that Kubernetes leaves empty for a
	// plain container-exit failure — it's really for eviction/admission
	// failures — so without this, Message stayed "" for the single most
	// common build failure, and the caller (pkg/build.Service.Build)
	// fell back to the uninformative "build failed: failed". Found live
	// 2026-08-26 from a real customer-facing 422 with an empty log and
	// that exact message.
	if st.Status == "failed" && st.Message == "" {
		for _, cs := range pod.Status.ContainerStatuses {
			t := cs.State.Terminated
			if t == nil {
				continue
			}
			reason := t.Reason
			if reason == "" {
				reason = "Error"
			}
			if t.Message != "" {
				st.Message = fmt.Sprintf("exit code %d: %s (%s)", t.ExitCode, reason, t.Message)
			} else {
				st.Message = fmt.Sprintf("exit code %d: %s", t.ExitCode, reason)
			}
			break
		}
	}

	return st
}

func boolPtr(b bool) *bool { return &b }

// filesystemOwnershipCapabilities returns the extra capabilities a
// workload needs to manage file ownership AND its own process identity
// as root normally would — see InstanceSpec.AllowFilesystemOwnershipChanges'
// own doc comment for the two incidents this now covers. CHOWN/FOWNER
// let it set ownership on files it does not own; DAC_OVERRIDE/FSETID/
// SETFCAP cover permission bits and extended file capabilities a real
// base image layer can legitimately carry — all five are what Kaniko
// needs to unpack an image. SETGID/SETUID are a SEPARATE, later addition
// (found live 2026-08-26, one build after the first five alone): the
// standard "start as root, fork workers, drop each to a dedicated
// less-privileged user" pattern most daemon images use (nginx, postgres,
// and more) calls setgid()/setuid() to do that drop, and without these
// two nginx's own worker processes failed outright with "setgid(101):
// Operation not permitted" — a build unpacking a base image and a
// running daemon dropping its own privileges are different operations,
// so both capability groups are genuinely needed, not overlapping.
// Empty (no extra grant) unless the caller explicitly opted in.
func filesystemOwnershipCapabilities(spec InstanceSpec) []corev1.Capability {
	if !spec.AllowFilesystemOwnershipChanges {
		return nil
	}
	return []corev1.Capability{"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "SETFCAP", "SETGID", "SETUID"}
}
