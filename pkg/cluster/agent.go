// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	agentpb "github.com/FlashbackAi/teepin-core/pkg/agentpb"
)

// AgentClient reaches GPU capacity through agents dialling in over gRPC.
//
// This is what lets the control plane run on AWS: it holds no Kubernetes
// credentials and opens no connection to the GPU datacenter. The agent
// connects outbound and this side pushes commands down the stream it
// holds open.
//
// Instance status is served from a cache the agents keep current, not by
// asking on every read. A customer polling their instance list must not
// produce a round trip across the internet per request, and the agent
// reports transitions as they happen anyway.
type AgentClient struct {
	registry *Registry

	mu sync.RWMutex
	// statuses is the last known state of every instance, keyed by
	// instance ID. Updated by unsolicited InstanceStatus messages.
	statuses map[string]InstanceStatus
	// providers remembers which session's provider created each
	// instance, so a later StreamLogs/DeleteInstance routes back to the
	// SAME agent rather than registry.Any() — an arbitrary session in a
	// multi-provider deployment. Found live 2026-08-22 while researching
	// the logs pipeline: CreateInstance already routes by ByProvider(),
	// but StreamLogs and DeleteInstance both still used Any(), a latent
	// bug from before a second provider (a home node) ever connected.
	// Set at create time; cleared in ForgetInstance.
	providers map[string]string

	// startedAt is when THIS AgentClient — one per control-plane process —
	// was constructed. See statusCacheGracePeriod's own doc comment: it
	// bounds how long an empty statuses cache is treated as "not
	// populated yet" rather than "genuinely nothing running".
	startedAt time.Time
}

var _ Client = (*AgentClient)(nil)

// statusCacheGracePeriod bounds how long after this control-plane process
// starts an empty statuses cache is treated as untrustworthy rather than
// a genuine answer — see ListInstanceStatuses' own doc comment for the
// race this closes. Set comfortably past pkg/agentrunner's statusInterval
// (15s: the home node's OWN periodic status-report cadence, which has no
// immediate first fire on connect) plus real gRPC reconnect time, the
// same "comfortably past X" margin reasoning used for screenshotTokenTTL
// vs captureTimeoutDefault elsewhere in this codebase.
const statusCacheGracePeriod = 45 * time.Second

func NewAgentClient(registry *Registry) *AgentClient {
	return &AgentClient{
		registry:  registry,
		statuses:  make(map[string]InstanceStatus),
		providers: make(map[string]string),
		startedAt: time.Now(),
	}
}

// sessionForInstance resolves the session that should handle a command
// for an already-existing instance: the SAME provider that created it,
// when known, falling back to Any() for the single-provider datacenter
// path (or an instance created before this routing existed).
func (c *AgentClient) sessionForInstance(instanceID string) (*AgentSession, bool) {
	c.mu.RLock()
	providerID := c.providers[instanceID]
	c.mu.RUnlock()

	if providerID != "" {
		if session, ok := c.registry.ByProvider(providerID); ok {
			return session, true
		}
		// The owning provider is known but not currently connected — do
		// NOT fall back to Any() here. Routing a home instance's logs to
		// a different provider's session would return "not found" from
		// an agent that never had this pod, a more confusing failure
		// than a clean "unavailable".
		return nil, false
	}
	return c.registry.Any()
}

// RecordStatus stores a status reported by an agent. Called by the gRPC
// server when an InstanceStatus arrives.
func (c *AgentClient) RecordStatus(status InstanceStatus) {
	if status.InstanceID == "" {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if existing, ok := c.statuses[status.InstanceID]; ok {
		// Terminated is final. A late-arriving "running" for an instance
		// already reported gone would otherwise resurrect it in the cache
		// and restart billing for a workload that no longer exists.
		if existing.Status == "terminated" && status.Status != "terminated" {
			return
		}

		// A report with no endpoint fields does not mean "the endpoint is
		// now gone" — it means THIS report did not carry them. The only
		// wire source of empty endpoint fields today is an agent build
		// that predates them entirely (or a home node's periodic sweep,
		// which never reports endpoint state at all — only CreateInstance
		// synthesizes it, see Stage 3 plan B8). Blindly overwriting here
		// would let a stale-but-still-connected agent silently erase a
		// correctly-set endpoint on its very next heartbeat: the
		// reconciler compares this cache against the database and would
		// then persist the erasure via UpdateEndpoint, exactly reproducing
		// the datacenter-side defect 1 bug this cache exists to prevent —
		// just triggered by a partial report instead of a missing one. The
		// only real way to lose an endpoint is deletion, which goes
		// through ForgetInstance, not a status update.
		if status.EndpointURL == "" && status.DNSName == "" && status.PublicIP == "" &&
			!status.TLSEnabled && !status.TLSReady {
			status.EndpointURL = existing.EndpointURL
			status.DNSName = existing.DNSName
			status.PublicIP = existing.PublicIP
			status.TLSEnabled = existing.TLSEnabled
			status.TLSReady = existing.TLSReady
		}

		// Hidden is set ONCE, at CreateInstance's own seed, from the
		// creating spec's labels (see that call site's own doc comment) —
		// it is a fixed fact about which POD this is, not runtime state a
		// later report could legitimately change. The wire message this
		// method is fed from (agentpb.InstanceStatus, via grpcserver.go's
		// handleMessage) has no Hidden field at all, so status.Hidden is
		// always the Go zero value (false) here — blindly overwriting
		// would un-hide a Kaniko build or Kumbha agent pod the moment its
		// FIRST status report arrived after creation. Found live
		// 2026-08-31: fixing reportStatuses to finally include these pods
		// (AllTenantsIncludingHidden, needed so their status ever reaches
		// this cache at all) immediately regressed this — every push
		// reset Hidden to false, and both pod types started appearing in
		// the customer's own Compute list within seconds of being
		// created.
		status.Hidden = existing.Hidden
	}

	c.statuses[status.InstanceID] = status
}

// ForgetInstance drops an instance from the cache after deletion.
func (c *AgentClient) ForgetInstance(instanceID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.statuses, instanceID)
	delete(c.providers, instanceID)
}

// scopeAllows reports whether a cached status is visible to a scope.
//
// The cluster enforces tenancy through label selectors; the cache has no
// labels, so the scope is checked against the tenancy recorded when the
// instance was created. Instances with no recorded tenancy are visible
// only to unrestricted scopes — failing closed, because an instance
// whose owner is unknown must not be shown to a guess.
func scopeAllows(scope Scope, status InstanceStatus) bool {
	if !scope.IsRestricted() {
		return true
	}
	if scope.ProjectID != "" && status.ProjectID != scope.ProjectID {
		return false
	}
	if scope.AccountID != "" && status.AccountID != scope.AccountID {
		return false
	}
	return true
}

func (c *AgentClient) CreateInstance(ctx context.Context, spec InstanceSpec) (*InstanceResult, error) {
	return c.createOrReplace(ctx, spec, false)
}

// UpdateInstance replaces an existing instance's pod in place — see the
// Client interface's own doc comment. On the wire this is the same
// CreateInstanceCommand as an ordinary create, with replace_existing set:
// the agent (pkg/agentrunner) routes that flag past its usual "already
// exists -> report success without recreating" idempotency check and into
// DirectClient.UpdateInstance instead, which deletes only the pod and
// recreates it under the same instance ID.
func (c *AgentClient) UpdateInstance(ctx context.Context, _ Scope, spec InstanceSpec) (*InstanceResult, error) {
	return c.createOrReplace(ctx, spec, true)
}

func (c *AgentClient) createOrReplace(ctx context.Context, spec InstanceSpec, replaceExisting bool) (*InstanceResult, error) {
	// Route to the OWNING provider when placement resolved one (home-class,
	// multi-provider). Falls back to Any() only for the single-provider
	// datacenter path where no provider was resolved. Sending a create for
	// provider B's node to provider A — the old Any()-always behaviour —
	// would run the workload on the wrong hardware the moment two providers
	// are connected.
	var session *AgentSession
	var ok bool
	if spec.ProviderID != "" {
		session, ok = c.registry.ByProvider(spec.ProviderID)
	} else {
		session, ok = c.registry.Any()
	}
	if !ok {
		return nil, ErrClusterUnavailable
	}

	ports := make([]*agentpb.PortMapping, 0, len(spec.Ports))
	for _, p := range spec.Ports {
		ports = append(ports, &agentpb.PortMapping{
			Container: int32(p.Container),
			Protocol:  p.Protocol,
		})
	}

	cmd := &agentpb.CreateInstanceCommand{
		InstanceId:                      spec.InstanceID,
		AccountId:                       spec.AccountID,
		ProjectId:                       spec.ProjectID,
		Image:                           spec.Image,
		Command:                         spec.Command,
		Args:                            spec.Args,
		CpuUnits:                        int32(spec.CPUUnits),
		MemoryGb:                        int32(spec.MemoryGB),
		GpuResource:                     spec.GPUResource,
		GpuQuantity:                     int32(spec.GPUQuantity),
		GpuVramGb:                       int32(spec.GPUVRAMGB),
		NodeName:                        spec.NodeName,
		Env:                             spec.Env,
		Labels:                          spec.Labels,
		Ports:                           ports,
		EndpointDomain:                  spec.EndpointDomain,
		EnableTls:                       spec.EnableTLS,
		TlsIssuer:                       spec.TLSIssuer,
		ImagePullSecret:                 spec.ImagePullSecret,
		StorageGb:                       int32(spec.StorageGB),
		EphemeralStorageGb:              int32(spec.EphemeralStorageGB),
		AlwaysPullImage:                 spec.AlwaysPullImage,
		NeverRestart:                    spec.NeverRestart,
		AllowFilesystemOwnershipChanges: spec.AllowFilesystemOwnershipChanges,
		ReplaceExisting:                 replaceExisting,
	}

	// The instance ID is the idempotency key: a command redelivered after
	// a reconnect must not produce a second pod. (This does not apply to a
	// replace: replaceExisting routes the agent past that exact check —
	// see the proto field's own doc comment.)
	result, err := session.dispatch(ctx, &agentpb.ControlMessage{
		RequestId: spec.InstanceID,
		Payload:   &agentpb.ControlMessage_CreateInstance{CreateInstance: cmd},
	})
	if err != nil {
		return nil, err
	}
	if !result.Success {
		return nil, errorFromResult(result)
	}

	// Remember which session actually created this instance — session's
	// own ProviderID, not spec.ProviderID, since the Any() fallback path
	// (datacenter, no ProviderID resolved by placement) still lands on a
	// real session with a real provider identity. This is what lets
	// StreamLogs/DeleteInstance route back to the SAME agent later
	// instead of an arbitrary one.
	c.mu.Lock()
	c.providers[spec.InstanceID] = session.ProviderID
	c.mu.Unlock()

	endpointURL, dnsName, tlsEnabled, tlsReady := result.EndpointUrl, result.DnsName, result.TlsEnabled, result.TlsReady

	// Home nodes never provision an Ingress of their own (see
	// cmd/teepin-agent/main.go's homeClusterClient — networking is always
	// nil there, on purpose: nothing can reach a home node directly from
	// the internet regardless of what a local Ingress claimed). The only
	// way in is the Stage 3 tunnel through this same agent session, so the
	// control plane is the endpoint's provisioner for home instances,
	// exactly as the Stage 3 plan's EndpointProvisioner seam anticipates —
	// it synthesizes the URL here rather than expecting one from
	// CommandResult, which for a home agent is always empty.
	//
	// TLS is reported ready immediately: the wildcard ACM cert on
	// *.teepin.com (Stage 3 plan B7) is already issued and covers every
	// instance hostname the moment DNS resolves — there is no per-instance
	// cert-manager wait to track here, unlike the datacenter path.
	if spec.NodeClass == "home" && len(spec.Ports) > 0 {
		domain := spec.EndpointDomain
		if domain != "" {
			dnsName = spec.InstanceID + "." + domain
			endpointURL = "https://" + dnsName
			tlsEnabled = true
			tlsReady = true
		}
	}

	// Seed the cache so an immediate read-after-create does not report
	// the instance missing while waiting for the first status push.
	c.RecordStatus(InstanceStatus{
		InstanceID:  spec.InstanceID,
		AccountID:   spec.AccountID,
		ProjectID:   spec.ProjectID,
		Status:      "pending",
		PodName:     result.PodName,
		ObservedAt:  time.Now().UTC(),
		EndpointURL: endpointURL,
		DNSName:     dnsName,
		PublicIP:    result.PublicIp,
		TLSEnabled:  tlsEnabled,
		TLSReady:    tlsReady,
		// See InstanceStatus.Hidden's own doc comment: this seed is the
		// ONE place that needs it, since the home node's own sweep
		// correctly never reports a hidden pod at all (managedSelector
		// excludes it before a status is even constructed) — nothing
		// downstream will ever overwrite this with a wire update that
		// forgets to set it.
		Hidden: spec.Labels[labelKumbhaAgent] == "true",
	})

	return &InstanceResult{
		PodName:     result.PodName,
		EndpointURL: endpointURL,
		PublicIP:    result.PublicIp,
		DNSName:     dnsName,
		TLSEnabled:  tlsEnabled,
		TLSReady:    tlsReady,
	}, nil
}

func (c *AgentClient) DeleteInstance(ctx context.Context, scope Scope, instanceID string) error {
	// Tenancy is checked here, against the cache, because the agent
	// executes what it is told: it has no view of which customer owns
	// what. An out-of-scope instance is treated as absent, and delete is
	// idempotent, so this returns success without touching it.
	if !c.visible(scope, instanceID) {
		return nil
	}

	session, ok := c.sessionForInstance(instanceID)
	if !ok {
		return ErrClusterUnavailable
	}

	result, err := session.dispatch(ctx, &agentpb.ControlMessage{
		RequestId: "delete-" + instanceID,
		Payload: &agentpb.ControlMessage_DeleteInstance{
			DeleteInstance: &agentpb.DeleteInstanceCommand{InstanceId: instanceID},
		},
	})
	if err != nil {
		return err
	}
	if !result.Success && result.ErrorCode != agentpb.ErrorCode_ERROR_CODE_NOT_FOUND {
		return errorFromResult(result)
	}

	c.ForgetInstance(instanceID)
	return nil
}

func (c *AgentClient) GetInstanceStatus(_ context.Context, scope Scope, instanceID string) (*InstanceStatus, error) {
	c.mu.RLock()
	status, ok := c.statuses[instanceID]
	c.mu.RUnlock()

	if !ok || !scopeAllows(scope, status) {
		return nil, ErrNotFound
	}

	// No agent connected means the cache is stale and cannot be trusted
	// for anything a customer acts on.
	if c.registry.Count() == 0 {
		return nil, ErrClusterUnavailable
	}

	return &status, nil
}

// hiddenInstanceIDPrefixes are Kumbha's own naming conventions for
// internal-tooling pods (pkg/kumbha's agent/screenshot pods, pkg/build's
// Kaniko pods) — duplicated here as plain strings, not imported, since
// those packages already import pkg/cluster and importing back would
// cycle. Checked as a FALLBACK alongside InstanceStatus.Hidden in
// ListInstanceStatuses below, not a replacement for it: Hidden (set at
// CreateInstance's own seed from the real spec.Labels) is the precise,
// authoritative signal for a pod created after this existed. This prefix
// check exists because Hidden is CACHED, MUTABLE state that a status
// update can corrupt (see RecordStatus's own doc comment) or that a
// cache-clearing event (a control-plane restart) loses entirely for an
// already-running pod — found live 2026-08-31: a 5-day-old agent pod's
// cache entry, corrupted by an update from before RecordStatus preserved
// Hidden, kept showing in a customer's Compute list even after that fix
// shipped, because the fix only stops FUTURE corruption, it cannot heal
// an already-wrong cached value. A prefix check has no state to
// corrupt — it is recomputed fresh on every read from the one thing that
// is always present and immutable: the instance's own ID.
var hiddenInstanceIDPrefixes = []string{"kumbha-agent-", "kaniko-build-", "kumbha-shot-"}

func isHiddenInstanceID(instanceID string) bool {
	for _, prefix := range hiddenInstanceIDPrefixes {
		if strings.HasPrefix(instanceID, prefix) {
			return true
		}
	}
	return false
}

func (c *AgentClient) ListInstanceStatuses(_ context.Context, scope Scope) ([]InstanceStatus, error) {
	// CRITICAL: with no agent connected this must fail rather than return
	// an empty list. The reconciler marks instances terminated when they
	// are absent from this result, so an empty list during a disconnect
	// would stop billing for every instance on the platform.
	if c.registry.Count() == 0 {
		return nil, ErrClusterUnavailable
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	// A second, narrower way the guard above can be defeated: a home node
	// can be freshly (re)connected — registry.Count() > 0 — while THIS
	// control-plane process's own cache is still completely empty,
	// because it was just constructed (a control-plane restart/redeploy)
	// and pkg/agentrunner's statusLoop only reports on a plain ticker with
	// no immediate first fire — it waits a full statusInterval (15s)
	// after connecting before its first report ever reaches here. The
	// reconciler's own "reconcile immediately on startup" has no such
	// wait, so every control-plane restart races this exact window: read
	// naively, a clean empty statuses list looks identical to "every
	// instance vanished simultaneously", which the reconciler cannot
	// distinguish from a truly empty answer on its own (both look like
	// zero live statuses against however many the database expects) — so
	// the distinction has to be made HERE, the one place that actually
	// knows whether this is early-post-startup or not. Found live
	// 2026-08-31: exactly this mass-terminated a real, healthy,
	// currently-serving instance (inst-5ed29952) more than once during a
	// single evening of routine control-plane redeploys, each one
	// silently breaking a working deployment for a reason that had
	// nothing to do with anything actually wrong with it. Bounded by
	// statusCacheGracePeriod so a deployment with genuinely zero
	// instances, ever, does not return this error forever — only during
	// the narrow window right after this process started.
	if len(c.statuses) == 0 && time.Since(c.startedAt) < statusCacheGracePeriod {
		return nil, ErrClusterUnavailable
	}

	out := make([]InstanceStatus, 0, len(c.statuses))
	for _, status := range c.statuses {
		if status.Hidden || isHiddenInstanceID(status.InstanceID) {
			continue
		}
		if scopeAllows(scope, status) {
			out = append(out, status)
		}
	}
	return out, nil
}

func (c *AgentClient) StreamLogs(ctx context.Context, scope Scope, instanceID string, opts LogOptions, w io.Writer) error {
	if !c.visible(scope, instanceID) {
		return ErrNotFound
	}

	session, ok := c.sessionForInstance(instanceID)
	if !ok {
		return ErrClusterUnavailable
	}

	requestID := "logs-" + instanceID + "-" + uuid.New().String()[:8]
	chunks := session.openLogStream(requestID)
	defer session.closeLogStream(requestID)

	if err := session.send(&agentpb.ControlMessage{
		RequestId: requestID,
		Payload: &agentpb.ControlMessage_FetchLogs{
			FetchLogs: &agentpb.FetchLogsCommand{
				InstanceId: instanceID,
				TailLines:  int32(opts.TailLines),
				Follow:     opts.Follow,
			},
		},
	}); err != nil {
		return fmt.Errorf("%w: %v", ErrClusterUnavailable, err)
	}

	// Follow streams until the caller goes away; a bounded fetch ends at
	// EOF or the timeout below.
	deadline := time.NewTimer(pendingTimeout)
	defer deadline.Stop()

	for {
		select {
		case chunk, open := <-chunks:
			if !open {
				// Channel closed: the agent disconnected mid-stream.
				return ErrClusterUnavailable
			}
			if len(chunk.Data) > 0 {
				if _, err := w.Write(chunk.Data); err != nil {
					return err
				}
			}
			if chunk.Eof {
				return nil
			}
			if !opts.Follow {
				// Each chunk extends the window: a slow but progressing
				// log fetch should not be cut off by a fixed deadline.
				deadline.Reset(pendingTimeout)
			}

		case <-ctx.Done():
			// Customer disconnected. Not an error.
			return nil

		case <-deadline.C:
			if opts.Follow {
				// A follow stream that is simply quiet is healthy; keep
				// waiting until the caller's context ends.
				deadline.Reset(pendingTimeout)
				continue
			}
			return fmt.Errorf("%w: no log data within %s", ErrClusterUnavailable, pendingTimeout)
		}
	}
}

func (c *AgentClient) Inventory(context.Context) ([]NodeInventory, error) {
	sessions := c.registry.Sessions()
	if len(sessions) == 0 {
		return nil, ErrClusterUnavailable
	}

	var all []NodeInventory
	for _, session := range sessions {
		nodes, observedAt := session.Inventory()
		if observedAt.IsZero() {
			continue
		}
		// Stale inventory is worse than none: placing against capacity
		// reported minutes ago produces allocations the agent then
		// rejects, and the customer sees a failure rather than a
		// slightly slower success elsewhere.
		if time.Since(observedAt) > 2*time.Minute {
			continue
		}
		all = append(all, nodes...)
	}

	return all, nil
}

func (c *AgentClient) Healthy(context.Context) bool {
	return c.registry.Count() > 0
}

// InstanceMetrics is not meaningful on AgentClient: unlike Inventory
// (which the allocator pulls live, per placement decision), instance
// utilization reaches the control plane as a PUSH — the agent sends
// InstanceMetricsReport, handleMessage's write-through persists it
// straight to compute.instance_metrics — the same shape node metrics
// already use (see grpcserver.go's reportInventorySeen). Nothing calls
// this method today; implemented only to satisfy the Client interface.
func (c *AgentClient) InstanceMetrics(context.Context) ([]InstanceMetric, error) {
	return nil, nil
}

// ResolveInstanceAddress is not meaningful on AgentClient: the control
// plane holds no Kubernetes credentials in agent mode and never resolves a
// pod address itself — that is exactly what the Stage 3 tunnel
// (pkg/cluster.ProxyHandler, dispatching over the same Registry this
// client uses) exists to do instead, on the AGENT side of the connection
// (see DirectClient.ResolveInstanceAddress). Implemented only to satisfy
// the Client interface.
func (c *AgentClient) ResolveInstanceAddress(context.Context, string, int32) (string, error) {
	return "", fmt.Errorf("%w: address resolution happens agent-side, not on the control plane", ErrClusterUnavailable)
}

// visible reports whether an instance is in scope, failing closed when
// it is unknown to the cache.
func (c *AgentClient) visible(scope Scope, instanceID string) bool {
	if !scope.IsRestricted() {
		return true
	}

	c.mu.RLock()
	status, ok := c.statuses[instanceID]
	c.mu.RUnlock()

	return ok && scopeAllows(scope, status)
}

// errorFromResult maps an agent's error code to a sentinel the control
// plane branches on.
//
// The codes exist so this does not become string matching: the control
// plane reallocates on RESOURCE_EXHAUSTED and surfaces IMAGE_PULL to the
// customer, and those must not depend on an agent's error wording.
func errorFromResult(result *agentpb.CommandResult) error {
	message := result.ErrorMessage
	if message == "" {
		message = "agent reported failure"
	}

	switch result.ErrorCode {
	case agentpb.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED:
		return fmt.Errorf("%w: %s", ErrResourceExhausted, message)
	case agentpb.ErrorCode_ERROR_CODE_NOT_FOUND:
		return ErrNotFound
	case agentpb.ErrorCode_ERROR_CODE_IMAGE_PULL:
		return fmt.Errorf("%w: %s", ErrImagePull, message)
	case agentpb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT:
		return errors.New(message)
	default:
		return errors.New(message)
	}
}
