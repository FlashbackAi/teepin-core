// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
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

// AgentServer implements the gRPC service agents dial into.
//
// It owns no cluster logic: it authenticates the agent, registers a
// session, and pumps messages between the stream and the AgentClient.
// All decisions live above it.
type AgentServer struct {
	agentpb.UnimplementedClusterAgentServer

	registry *Registry
	client   *AgentClient

	// token authenticates agents. A shared secret rather than mTLS for
	// now: it is one string to rotate, and the stream already runs over
	// TLS terminated at the ALB. mTLS is the right answer once there are
	// multiple providers with different trust levels.
	token string
}

func NewAgentServer(registry *Registry, client *AgentClient, token string) *AgentServer {
	return &AgentServer{registry: registry, client: client, token: token}
}

// Connect handles one agent's lifetime.
//
// The agent dials out and sends Register first; everything after that is
// asynchronous in both directions on the same stream.
func (s *AgentServer) Connect(stream agentpb.ClusterAgent_ConnectServer) error {
	if err := s.authenticate(stream); err != nil {
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
	if register.ProviderId == "" {
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

	session := NewAgentSession(register.ProviderId, register.Region, register.AgentVersion, send)

	s.registry.Add(session)
	defer s.registry.Remove(session)

	log.Printf("Agent connected: provider=%s region=%s version=%s cluster=%s",
		register.ProviderId, register.Region, register.AgentVersion, register.ClusterVersion)
	defer log.Printf("Agent disconnected: provider=%s", register.ProviderId)

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
			InstanceID: st.InstanceId,
			Status:     st.Status,
			PodName:    st.PodName,
			NodeName:   st.NodeName,
			Message:    st.Message,
			AccountID:  st.AccountId,
			ProjectID:  st.ProjectId,
			ObservedAt: st.ObservedAt.AsTime(),
		})

	case *agentpb.AgentMessage_Inventory:
		session.setInventory(inventoryFromProto(payload.Inventory))

	case *agentpb.AgentMessage_Pong:
		// Liveness only; the stream staying open is the real signal.

	case *agentpb.AgentMessage_Register:
		// Re-registration on an established stream. Harmless, ignored.

	default:
		log.Printf("Agent sent unknown message type from provider=%s", session.ProviderID)
	}
}

// authenticate checks the shared secret from stream metadata.
func (s *AgentServer) authenticate(stream agentpb.ClusterAgent_ConnectServer) error {
	if s.token == "" {
		// Refuse rather than run open. An unauthenticated agent channel
		// would let anyone who can reach the port place workloads on
		// customer GPUs and read customer logs.
		return status.Error(codes.Unauthenticated,
			"agent authentication is not configured on this control plane")
	}

	md, ok := metadata.FromIncomingContext(stream.Context())
	if !ok {
		return status.Error(codes.Unauthenticated, "missing metadata")
	}

	values := md.Get(AgentTokenMetadataKey)
	if len(values) == 0 {
		return status.Error(codes.Unauthenticated, "missing agent token")
	}

	// Constant-time: a timing side channel on a long-lived shared secret
	// is worth avoiding even though the attack is impractical here.
	if subtle.ConstantTimeCompare([]byte(values[0]), []byte(s.token)) != 1 {
		return status.Error(codes.Unauthenticated, "invalid agent token")
	}

	return nil
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
