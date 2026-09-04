// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"log"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	agentpb "github.com/FlashbackAi/teepin-core/pkg/agentpb"
)

// NodeIdentity is the provable identity of an agent that authenticated with
// a PER-NODE credential (as opposed to the shared datacenter token). Its
// fields are authoritative over anything the agent asserts in RegisterRequest
// — in particular Class, which an agent must never be able to self-elevate.
type NodeIdentity struct {
	NodeName   string
	ProviderID string
	Class      string
}

// NodeAuthenticator resolves a per-node credential to a NodeIdentity. It is
// an interface so pkg/cluster does not import pkg/nodes (which would create a
// cycle: nodes has no reason to depend on cluster, and cluster must stay
// free of anything that imports k8s). main injects the concrete resolver.
//
// A nil NodeAuthenticator means only the shared datacenter token is accepted
// — the home-compute feature is off, and behaviour is exactly as before.
type NodeAuthenticator interface {
	// AuthenticateNode returns the identity for a valid credential, or an
	// error for an invalid/revoked/disabled one.
	AuthenticateNode(ctx context.Context, credential string) (*NodeIdentity, error)
}

// NodeSeen is a write-through record that a node is connected and reporting.
type NodeSeen struct {
	NodeName     string
	ProviderID   string
	Class        string
	Region       string
	CPUCores     int
	MemoryGB     int
	GPUModel     string
	GPUCount     int
	MIGCapable   bool
	AgentVersion string
	// K8sReady is the reporting session's own Client.Healthy(ctx) at report
	// time — distinct from "connected" (the session existing at all). A
	// node whose agent is up but whose local Kubernetes (k3s, on a home
	// node) is unreachable reports false here, so placement can tell the
	// two states apart instead of treating both as "online".
	K8sReady bool
	// CPUUsedPercent/MemoryUsedGB are the reporting HOST's own current
	// utilization (not any one workload's) — see GPUInventory's own proto
	// comment. Unlike CPUCores/MemoryGB (static capacity, set once at
	// enroll and never refreshed — see UpsertSeen's own doc comment on
	// why that gap exists for those two), these ARE refreshed on every
	// report, since that is the whole point of a utilization reading.
	// Added 2026-09-04 as the foundation for node telemetry (stats/
	// graphs/status page/marketing globe — ROADMAP.md's 2026-09-03 entry).
	CPUUsedPercent float64
	MemoryUsedGB   float64
	// GPUUsedVRAMGB is the CURRENT VRAM in use on a GPU node row — only
	// ever set on the per-GPU-node branch of reportInventorySeen (a
	// CPU-only home node's single ReportSeen call leaves it at zero,
	// which compute.nodes.gpu_count already disambiguates from "GPU
	// present but idle" for anything reading the history back). Sourced
	// from GPUNode.used_vram_gb — a field the allocator already consumes
	// live; this is that SAME already-reported number, finally also
	// persisted with history instead of only ever living in memory.
	// Added 2026-09-04, closing a gap in the SAME day's own telemetry
	// foundation work — found live while auditing it for solidity.
	GPUUsedVRAMGB int
	// NetworkRxMbps/NetworkTxMbps/StorageReadMbps/StorageWriteMbps are the
	// same session-level, host-wide, current-not-capacity shape as
	// CPUUsedPercent/MemoryUsedGB above — throughput RATES in MB/s, not
	// cumulative counters, for the same reason those two are refreshed
	// on every report. Added the same day the customer explicitly asked
	// "does this cover network/storage too?" after CPU/mem/VRAM shipped.
	NetworkRxMbps    float64
	NetworkTxMbps    float64
	StorageReadMbps  float64
	StorageWriteMbps float64
}

// NodeReporter persists node liveness/specs from the gRPC session — the
// write-through that gives every connected node a durable record so a
// control-plane restart no longer forgets its hardware. Interface, for the
// same no-import-cycle reason as NodeAuthenticator. Nil disables persistence
// (the pre-home-compute behaviour). It is deliberately NOT consulted for
// placement: the allocator still reads live session inventory.
type NodeReporter interface {
	ReportSeen(seen NodeSeen)
}

// AgentServer implements the gRPC service agents dial into.
//
// It owns no cluster logic: it authenticates the agent, registers a
// session, and pumps messages between the stream and the AgentClient.
// All decisions live above it.
type AgentServer struct {
	agentpb.UnimplementedClusterAgentServer

	registry *Registry
	client   *AgentClient

	// token authenticates DATACENTER agents. A shared secret rather than
	// mTLS for now: it is one string to rotate, and the stream already runs
	// over TLS terminated at the ALB.
	token string

	// nodeAuth authenticates HOME (and any per-node) agents by their own
	// credential. Nil when the home-compute feature is off, in which case
	// only the shared token is accepted.
	nodeAuth NodeAuthenticator

	// nodeReporter persists node liveness on each inventory report. Nil
	// disables persistence (pre-home-compute behaviour).
	nodeReporter NodeReporter
}

func NewAgentServer(registry *Registry, client *AgentClient, token string) *AgentServer {
	return &AgentServer{registry: registry, client: client, token: token}
}

// WithNodeAuthenticator enables per-node credential authentication. Returns
// the same server for chaining, so existing call sites compile unchanged.
func (s *AgentServer) WithNodeAuthenticator(a NodeAuthenticator) *AgentServer {
	s.nodeAuth = a
	return s
}

// WithNodeReporter enables write-through node persistence on inventory
// reports. Returns the same server for chaining.
func (s *AgentServer) WithNodeReporter(r NodeReporter) *AgentServer {
	s.nodeReporter = r
	return s
}

// Connect handles one agent's lifetime.
//
// The agent dials out and sends Register first; everything after that is
// asynchronous in both directions on the same stream.
func (s *AgentServer) Connect(stream agentpb.ClusterAgent_ConnectServer) error {
	identity, err := s.authenticate(stream)
	if err != nil {
		return err
	}

	// First message must be Register: the control plane cannot route
	// anything until it knows which provider is speaking.
	first, err := stream.Recv()
	if err != nil {
		return status.Error(codes.Unavailable, "stream closed before registration")
	}

	register := first.GetRegister()
	if register == nil {
		return status.Error(codes.InvalidArgument, "first message must be RegisterRequest")
	}

	// Provider identity. For a per-node credential, it comes from the
	// CREDENTIAL, not the RegisterRequest — a node cannot claim to be a
	// different provider or a different class than it enrolled as. Only the
	// shared-token (datacenter) path trusts the self-asserted provider_id.
	providerID := register.ProviderId
	if identity != nil {
		providerID = identity.ProviderID
	}
	if providerID == "" {
		return status.Error(codes.InvalidArgument, "provider_id is required")
	}

	// gRPC forbids concurrent SendMsg on one stream, and commands are
	// dispatched from many request goroutines, so every write goes
	// through this mutex.
	var sendMu sync.Mutex
	send := func(msg *agentpb.ControlMessage) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(msg)
	}

	class := "datacenter"
	if identity != nil {
		class = identity.Class
	}

	session := NewAgentSession(providerID, register.Region, register.AgentVersion, class, send)

	s.registry.Add(session)
	defer s.registry.Remove(session)

	log.Printf("Agent connected: provider=%s class=%s region=%s version=%s cluster=%s",
		providerID, class, register.Region, register.AgentVersion, register.ClusterVersion)
	defer log.Printf("Agent disconnected: provider=%s", providerID)

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			// Disconnects are routine — nodes reboot, networks blip. The
			// agent reconnects with backoff; this is not an alert.
			log.Printf("Agent stream ended: provider=%s: %v", register.ProviderId, err)
			return nil
		}

		s.handleMessage(session, msg)
	}
}

// handleMessage routes one agent message.
func (s *AgentServer) handleMessage(session *AgentSession, msg *agentpb.AgentMessage) {
	switch payload := msg.Payload.(type) {
	case *agentpb.AgentMessage_Result:
		session.deliverResult(msg.RequestId, payload.Result)

	case *agentpb.AgentMessage_LogChunk:
		session.deliverLogChunk(msg.RequestId, payload.LogChunk)

	case *agentpb.AgentMessage_InstanceStatus:
		st := payload.InstanceStatus
		s.client.RecordStatus(InstanceStatus{
			InstanceID:  st.InstanceId,
			Status:      st.Status,
			PodName:     st.PodName,
			NodeName:    st.NodeName,
			Message:     st.Message,
			AccountID:   st.AccountId,
			ProjectID:   st.ProjectId,
			ObservedAt:  st.ObservedAt.AsTime(),
			EndpointURL: st.EndpointUrl,
			DNSName:     st.DnsName,
			PublicIP:    st.PublicIp,
			TLSEnabled:  st.TlsEnabled,
			TLSReady:    st.TlsReady,
		})

	case *agentpb.AgentMessage_Inventory:
		inv := inventoryFromProto(payload.Inventory)
		session.setInventory(inv)
		// Write-through persistence: record each reported node as seen. Async
		// and best-effort — a slow or failing DB must never stall the message
		// pump or drop the live inventory the allocator depends on. This does
		// not feed placement; it is the durable identity/liveness record.
		if s.nodeReporter != nil {
			s.reportInventorySeen(session, inv, payload.Inventory.ClusterReady,
				float64(payload.Inventory.CpuUsedPercent), payload.Inventory.MemoryUsedGb,
				payload.Inventory.NetworkRxMbps, payload.Inventory.NetworkTxMbps,
				payload.Inventory.StorageReadMbps, payload.Inventory.StorageWriteMbps)
		}

	case *agentpb.AgentMessage_Pong:
		// Liveness only; the stream staying open is the real signal.

	case *agentpb.AgentMessage_Register:
		// Re-registration on an established stream. Harmless, ignored.

	case *agentpb.AgentMessage_ProxyResponse:
		session.deliverProxyResponse(msg.RequestId, payload.ProxyResponse)

	case *agentpb.AgentMessage_ProxyData:
		session.deliverProxyData(msg.RequestId, payload.ProxyData)

	case *agentpb.AgentMessage_ExecOpen:
		session.deliverExecOpen(msg.RequestId, payload.ExecOpen)

	case *agentpb.AgentMessage_ExecOutput:
		session.deliverExecOutput(msg.RequestId, payload.ExecOutput)

	case *agentpb.AgentMessage_ExecEnd:
		session.deliverExecEnd(msg.RequestId, payload.ExecEnd)

	default:
		log.Printf("Agent sent unknown message type from provider=%s", session.ProviderID)
	}
}

// reportInventorySeen persists a "seen" record for each node the session
// reports. A datacenter session reports one or more GPU nodes; a home
// session that carries no GPU inventory is recorded once under its own
// identity so it still shows online. Reporting is delegated to the (async)
// reporter, so this never blocks the message pump.
//
// k8sReady is the SESSION-level cluster.Client.Healthy(ctx) result carried
// on the inventory report (GPUInventory.cluster_ready) — the same value
// applies to every node in this report, since they all share one agent
// process and one cluster client.
func (s *AgentServer) reportInventorySeen(session *AgentSession, inv []NodeInventory, k8sReady bool, cpuUsedPercent, memoryUsedGB, netRxMbps, netTxMbps, storageReadMbps, storageWriteMbps float64) {
	if len(inv) == 0 {
		// CPU-only / home node: no GPU inventory to enumerate. Record the
		// node under the session's own identity (provider id doubles as node
		// name for a single-node home provider).
		s.nodeReporter.ReportSeen(NodeSeen{
			NodeName:         session.ProviderID,
			ProviderID:       session.ProviderID,
			Class:            session.Class,
			Region:           session.Region,
			AgentVersion:     session.Version,
			K8sReady:         k8sReady,
			CPUUsedPercent:   cpuUsedPercent,
			MemoryUsedGB:     memoryUsedGB,
			NetworkRxMbps:    netRxMbps,
			NetworkTxMbps:    netTxMbps,
			StorageReadMbps:  storageReadMbps,
			StorageWriteMbps: storageWriteMbps,
		})
		return
	}
	for _, n := range inv {
		s.nodeReporter.ReportSeen(NodeSeen{
			NodeName:     n.NodeName,
			ProviderID:   session.ProviderID,
			Class:        session.Class,
			Region:       session.Region,
			GPUModel:     n.GPUModel,
			GPUCount:     n.GPUCount,
			MIGCapable:   n.MIGCapable,
			MemoryGB:     n.MemoryGBPerGPU * n.GPUCount,
			AgentVersion: session.Version,
			K8sReady:     k8sReady,
			// Session-level utilization, same reading attached to every
			// GPU node this session reports (one host, one utilization
			// figure) — matches how ClusterReady is already handled above.
			CPUUsedPercent:   cpuUsedPercent,
			MemoryUsedGB:     memoryUsedGB,
			NetworkRxMbps:    netRxMbps,
			NetworkTxMbps:    netTxMbps,
			StorageReadMbps:  storageReadMbps,
			StorageWriteMbps: storageWriteMbps,
			// Per-GPU, unlike the two above: n.UsedVRAMGB is THIS specific
			// device's own current usage, already reported live for the
			// allocator's own use (pkg/cluster/direct.go,
			// pkg/cluster/snapshotter.go) — this is that same number,
			// finally also threaded to persisted history.
			GPUUsedVRAMGB: n.UsedVRAMGB,
		})
	}
}

// authenticate resolves the caller's credential from stream metadata.
//
// It accepts EITHER the shared datacenter token OR a per-node credential:
//   - shared token match  → returns (nil, nil): a datacenter agent, identity
//     taken from its RegisterRequest as before.
//   - per-node credential → returns (*NodeIdentity, nil): identity is proven
//     by the credential and overrides the RegisterRequest.
//
// The shared token is tried first so the existing datacenter path is
// unchanged and never reaches the (possibly nil) node authenticator.
func (s *AgentServer) authenticate(stream agentpb.ClusterAgent_ConnectServer) (*NodeIdentity, error) {
	if s.token == "" && s.nodeAuth == nil {
		// Refuse rather than run open. An unauthenticated agent channel
		// would let anyone who can reach the port place workloads on
		// customer GPUs and read customer logs.
		return nil, status.Error(codes.Unauthenticated,
			"agent authentication is not configured on this control plane")
	}

	md, ok := metadata.FromIncomingContext(stream.Context())
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "missing metadata")
	}

	values := md.Get(AgentTokenMetadataKey)
	if len(values) == 0 {
		return nil, status.Error(codes.Unauthenticated, "missing agent token")
	}

	return s.resolveCredential(stream.Context(), values[0])
}

// resolveCredential is the credential-decision core, split out from metadata
// extraction so it can be unit-tested without a gRPC stream.
//
//	shared token match → (nil, nil)          — datacenter agent, unchanged
//	per-node match     → (*NodeIdentity, nil) — identity proven by credential
//	neither            → Unauthenticated error
//
// The shared token is checked first, so the datacenter path never depends on
// (and never reaches) the node authenticator.
func (s *AgentServer) resolveCredential(ctx context.Context, presented string) (*NodeIdentity, error) {
	if s.token != "" && subtle.ConstantTimeCompare([]byte(presented), []byte(s.token)) == 1 {
		return nil, nil
	}

	if s.nodeAuth != nil {
		identity, err := s.nodeAuth.AuthenticateNode(ctx, presented)
		if err == nil && identity != nil {
			return identity, nil
		}
	}

	return nil, status.Error(codes.Unauthenticated, "invalid agent token")
}

// AgentTokenMetadataKey carries the shared secret. Lowercase because
// gRPC normalises metadata keys and a mismatch here fails at runtime
// rather than compile time.
const AgentTokenMetadataKey = "x-teepin-agent-token"

func inventoryFromProto(inv *agentpb.GPUInventory) []NodeInventory {
	if inv == nil {
		return nil
	}

	out := make([]NodeInventory, 0, len(inv.Nodes))
	for _, node := range inv.Nodes {
		ni := NodeInventory{
			NodeName:       node.NodeName,
			GPUProduct:     node.GpuProduct,
			GPUModel:       node.GpuModel,
			MemoryGBPerGPU: int(node.MemoryGbPerGpu),
			GPUCount:       int(node.GpuCount),
			MIGCapable:     node.MigCapable,
			SharedCapacity: int(node.SharedCapacity),
			SharedUsed:     int(node.SharedUsed),
			UsedVRAMGB:     int(node.UsedVramGb),
			Ready:          node.Ready,
		}
		for _, m := range node.MigResources {
			ni.MIGResources = append(ni.MIGResources, MIGResource{
				ResourceName: m.ResourceName,
				Profile:      m.Profile,
				Slices:       int(m.Slices),
				MemoryGB:     int(m.MemoryGb),
				Capacity:     int(m.Capacity),
				Used:         int(m.Used),
			})
		}
		out = append(out, ni)
	}
	return out
}

// HealthService answers the ALB's gRPC health checks.
//
// The load balancer health-checks the gRPC port, and a target group
// pointing at a port with no health service marks every task unhealthy —
// which is exactly the failure that took the Fargate deployment down
// before this existed.
type HealthService struct {
	registry *Registry
}

func NewHealthService(registry *Registry) *HealthService {
	return &HealthService{registry: registry}
}

// Check reports serving whenever the gRPC server is up.
//
// Deliberately NOT conditional on an agent being connected: this answers
// "can an agent connect to me?", and reporting unhealthy because no
// agent has connected yet would remove the very endpoint agents dial
// into — a deadlock where nothing can ever reconnect.
func (h *HealthService) Check(string) (bool, string) {
	count := h.registry.Count()
	return true, fmt.Sprintf("serving, %d agent(s) connected", count)
}
