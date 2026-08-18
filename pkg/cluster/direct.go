// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

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
}

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
	pod, err := c.buildPod(spec)
	if err != nil {
		return nil, err
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

// managedSelector matches every TEEPIN-managed instance in a scope.
func managedSelector(scope Scope) string {
	return appendScope(fmt.Sprintf("%s=true", labelManaged), scope)
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

	for _, pod := range pods.Items {
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
	return err == nil
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
			Containers: []corev1.Container{
				{
					Name: "app",
					// nil leaves the image's own ENTRYPOINT/CMD in place.
					// Silently dropping these once left base images
					// crash-looping with no explanation.
					Command: spec.Command,
					Args:    spec.Args,
					Image:   spec.Image,
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

	return st
}

func boolPtr(b bool) *bool { return &b }
