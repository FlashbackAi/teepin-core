// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Package agentrunner executes control-plane commands against a local
// Kubernetes cluster.
//
// It is the agent's brain, separated from cmd/teepin-agent so it can be
// tested without a network: the command-handling logic is where the
// interesting failures live, not in the dialling.
package agentrunner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	agentpb "github.com/FlashbackAi/teepin-core/pkg/agentpb"
	"github.com/FlashbackAi/teepin-core/pkg/cluster"
	"github.com/FlashbackAi/teepin-core/pkg/gpu"
)

// inventoryInterval is how often capacity is reported.
//
// 30s trades a little staleness for far fewer messages. The control
// plane also treats inventory as advisory — it may lose an allocation
// race regardless, which surfaces as ErrResourceExhausted and a retry —
// so reporting faster would buy accuracy the design does not depend on.
const inventoryInterval = 30 * time.Second

// statusInterval is how often instance statuses are swept.
//
// Kubernetes watches would be more responsive, but a poll is
// dramatically simpler to reason about across reconnects, and one minute
// is well inside the billing granularity that consumes it.
const statusInterval = 15 * time.Second

// Config configures a Runner.
type Config struct {
	ProviderID string
	Region     string
	Version    string

	// Cluster executes the actual work. In production this is a
	// DirectClient over the local Kubernetes API.
	Cluster cluster.Client

	// Inventory reports GPU capacity. May be nil on CPU-only providers.
	Inventory *gpu.Inventory
}

// Runner owns one control-plane connection.
type Runner struct {
	cfg Config

	// sendMu serialises stream writes. Commands are handled in their own
	// goroutines and all reply on the same stream; gRPC forbids
	// concurrent SendMsg.
	sendMu sync.Mutex

	// lastReported deduplicates status pushes: without it, every sweep
	// would resend the unchanged state of every instance, which at any
	// scale is a lot of traffic saying nothing.
	lastReported map[string]string
	statusMu     sync.Mutex
}

func New(cfg Config) *Runner {
	return &Runner{
		cfg:          cfg,
		lastReported: make(map[string]string),
	}
}

// stream is the subset of the generated bidi stream the runner needs.
// Narrowed to an interface so tests can drive it without gRPC.
type stream interface {
	Send(*agentpb.AgentMessage) error
	Recv() (*agentpb.ControlMessage, error)
}

// Run registers, then serves commands until the stream ends.
func (r *Runner) Run(ctx context.Context, s stream) error {
	if err := r.send(s, &agentpb.AgentMessage{
		Payload: &agentpb.AgentMessage_Register{
			Register: &agentpb.RegisterRequest{
				ProviderId:   r.cfg.ProviderID,
				AgentVersion: r.cfg.Version,
				Region:       r.cfg.Region,
			},
		},
	}); err != nil {
		return fmt.Errorf("register: %w", err)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Inventory immediately on connect: the control plane treats every
	// reconnect as a fresh source of truth and has no capacity data
	// until this arrives.
	r.reportInventory(ctx, s)
	r.reportStatuses(ctx, s)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); r.inventoryLoop(ctx, s) }()
	go func() { defer wg.Done(); r.statusLoop(ctx, s) }()
	defer wg.Wait()

	for {
		msg, err := s.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		// Each command runs in its own goroutine: a slow image pull must
		// not stall the delete or log request behind it.
		go r.handleCommand(ctx, s, msg)
	}
}

func (r *Runner) handleCommand(ctx context.Context, s stream, msg *agentpb.ControlMessage) {
	switch payload := msg.Payload.(type) {
	case *agentpb.ControlMessage_CreateInstance:
		r.handleCreate(ctx, s, msg.RequestId, payload.CreateInstance)

	case *agentpb.ControlMessage_DeleteInstance:
		r.handleDelete(ctx, s, msg.RequestId, payload.DeleteInstance)

	case *agentpb.ControlMessage_FetchLogs:
		r.handleLogs(ctx, s, msg.RequestId, payload.FetchLogs)

	case *agentpb.ControlMessage_Ping:
		_ = r.send(s, &agentpb.AgentMessage{
			RequestId: msg.RequestId,
			Payload: &agentpb.AgentMessage_Pong{Pong: &agentpb.Pong{
				SentAt:     payload.Ping.SentAt,
				ReceivedAt: timestamppb.Now(),
			}},
		})

	default:
		r.replyError(s, msg.RequestId, agentpb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT,
			"unsupported command")
	}
}

func (r *Runner) handleCreate(ctx context.Context, s stream, requestID string, cmd *agentpb.CreateInstanceCommand) {
	ports := make([]cluster.PortMapping, 0, len(cmd.Ports))
	for _, p := range cmd.Ports {
		ports = append(ports, cluster.PortMapping{
			Container: int(p.Container),
			Protocol:  p.Protocol,
		})
	}

	spec := cluster.InstanceSpec{
		InstanceID:      cmd.InstanceId,
		AccountID:       cmd.AccountId,
		ProjectID:       cmd.ProjectId,
		Image:           cmd.Image,
		Command:         cmd.Command,
		Args:            cmd.Args,
		Env:             cmd.Env,
		Labels:          cmd.Labels,
		CPUUnits:        int(cmd.CpuUnits),
		MemoryGB:        int(cmd.MemoryGb),
		GPUResource:     cmd.GpuResource,
		GPUQuantity:     int(cmd.GpuQuantity),
		GPUVRAMGB:       int(cmd.GpuVramGb),
		NodeName:        cmd.NodeName,
		Ports:           ports,
		EndpointDomain:  cmd.EndpointDomain,
		EnableTLS:       cmd.EnableTls,
		TLSIssuer:       cmd.TlsIssuer,
		ImagePullSecret: cmd.ImagePullSecret,
	}

	// Idempotency: a command redelivered after a reconnect must not
	// create a second pod. The control plane uses the instance ID as the
	// request ID precisely so this check is possible.
	if existing, err := r.cfg.Cluster.GetInstanceStatus(
		ctx, cluster.AllTenants(), cmd.InstanceId); err == nil && existing != nil {
		log.Printf("Instance %s already exists - treating redelivered command as success", cmd.InstanceId)
		_ = r.send(s, &agentpb.AgentMessage{
			RequestId: requestID,
			Payload: &agentpb.AgentMessage_Result{Result: &agentpb.CommandResult{
				Success: true,
				PodName: existing.PodName,
			}},
		})
		return
	}

	result, err := r.cfg.Cluster.CreateInstance(ctx, spec)
	if err != nil {
		log.Printf("Create %s failed: %v", cmd.InstanceId, err)
		r.replyError(s, requestID, errorCodeFor(err), err.Error())
		return
	}

	log.Printf("Created instance %s (pod %s)", cmd.InstanceId, result.PodName)

	_ = r.send(s, &agentpb.AgentMessage{
		RequestId: requestID,
		Payload: &agentpb.AgentMessage_Result{Result: &agentpb.CommandResult{
			Success:     true,
			PodName:     result.PodName,
			EndpointUrl: result.EndpointURL,
			PublicIp:    result.PublicIP,
		}},
	})

	// Report immediately so the control plane's cache reflects the new
	// instance without waiting for the next sweep.
	r.reportStatuses(ctx, s)
}

func (r *Runner) handleDelete(ctx context.Context, s stream, requestID string, cmd *agentpb.DeleteInstanceCommand) {
	// AllTenants: the control plane has already checked tenancy. The
	// agent executes what it is told and has no view of customers.
	if err := r.cfg.Cluster.DeleteInstance(ctx, cluster.AllTenants(), cmd.InstanceId); err != nil {
		log.Printf("Delete %s failed: %v", cmd.InstanceId, err)
		r.replyError(s, requestID, errorCodeFor(err), err.Error())
		return
	}

	log.Printf("Deleted instance %s", cmd.InstanceId)

	r.statusMu.Lock()
	delete(r.lastReported, cmd.InstanceId)
	r.statusMu.Unlock()

	_ = r.send(s, &agentpb.AgentMessage{
		RequestId: requestID,
		Payload:   &agentpb.AgentMessage_Result{Result: &agentpb.CommandResult{Success: true}},
	})
}

func (r *Runner) handleLogs(ctx context.Context, s stream, requestID string, cmd *agentpb.FetchLogsCommand) {
	writer := &chunkWriter{
		runner:     r,
		stream:     s,
		requestID:  requestID,
		instanceID: cmd.InstanceId,
	}

	err := r.cfg.Cluster.StreamLogs(ctx, cluster.AllTenants(), cmd.InstanceId,
		cluster.LogOptions{
			TailLines: int(cmd.TailLines),
			Follow:    cmd.Follow,
		}, writer)

	if err != nil && !errors.Is(err, context.Canceled) {
		r.replyError(s, requestID, errorCodeFor(err), err.Error())
		return
	}

	// EOF terminates the stream on the control-plane side; without it
	// the reader waits for its timeout on every completed log fetch.
	_ = r.send(s, &agentpb.AgentMessage{
		RequestId: requestID,
		Payload: &agentpb.AgentMessage_LogChunk{LogChunk: &agentpb.LogChunk{
			InstanceId: cmd.InstanceId,
			Eof:        true,
		}},
	})
}

// chunkWriter turns io.Writer calls from the log stream into LogChunk
// messages.
type chunkWriter struct {
	runner     *Runner
	stream     stream
	requestID  string
	instanceID string
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	// Copy: p is only valid for the duration of the call, and the
	// message may outlive it in the send path.
	data := make([]byte, len(p))
	copy(data, p)

	err := w.runner.send(w.stream, &agentpb.AgentMessage{
		RequestId: w.requestID,
		Payload: &agentpb.AgentMessage_LogChunk{LogChunk: &agentpb.LogChunk{
			InstanceId: w.instanceID,
			Data:       data,
		}},
	})
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func (r *Runner) inventoryLoop(ctx context.Context, s stream) {
	ticker := time.NewTicker(inventoryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			r.reportInventory(ctx, s)
		case <-ctx.Done():
			return
		}
	}
}

func (r *Runner) statusLoop(ctx context.Context, s stream) {
	ticker := time.NewTicker(statusInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			r.reportStatuses(ctx, s)
		case <-ctx.Done():
			return
		}
	}
}

func (r *Runner) reportInventory(ctx context.Context, s stream) {
	nodes, err := r.cfg.Cluster.Inventory(ctx)
	if err != nil {
		log.Printf("Inventory failed: %v", err)
		return
	}

	pbNodes := make([]*agentpb.GPUNode, 0, len(nodes))
	for _, n := range nodes {
		node := &agentpb.GPUNode{
			NodeName:       n.NodeName,
			GpuProduct:     n.GPUProduct,
			GpuModel:       n.GPUModel,
			MemoryGbPerGpu: int32(n.MemoryGBPerGPU),
			GpuCount:       int32(n.GPUCount),
			MigCapable:     n.MIGCapable,
			SharedCapacity: int32(n.SharedCapacity),
			SharedUsed:     int32(n.SharedUsed),
			UsedVramGb:     int32(n.UsedVRAMGB),
			Ready:          n.Ready,
		}
		for _, m := range n.MIGResources {
			node.MigResources = append(node.MigResources, &agentpb.MIGResource{
				ResourceName: m.ResourceName,
				Profile:      m.Profile,
				Slices:       int32(m.Slices),
				MemoryGb:     int32(m.MemoryGB),
				Capacity:     int32(m.Capacity),
				Used:         int32(m.Used),
			})
		}
		pbNodes = append(pbNodes, node)
	}

	_ = r.send(s, &agentpb.AgentMessage{
		Payload: &agentpb.AgentMessage_Inventory{Inventory: &agentpb.GPUInventory{
			Nodes:      pbNodes,
			ObservedAt: timestamppb.Now(),
		}},
	})
}

// reportStatuses pushes changed instance statuses.
func (r *Runner) reportStatuses(ctx context.Context, s stream) {
	statuses, err := r.cfg.Cluster.ListInstanceStatuses(ctx, cluster.AllTenants())
	if err != nil {
		log.Printf("Status sweep failed: %v", err)
		return
	}

	r.statusMu.Lock()
	defer r.statusMu.Unlock()

	seen := make(map[string]bool, len(statuses))

	for _, st := range statuses {
		seen[st.InstanceID] = true

		// Only report transitions. Resending unchanged state every sweep
		// is pure noise on a shared stream.
		if previous, ok := r.lastReported[st.InstanceID]; ok && previous == st.Status {
			continue
		}
		r.lastReported[st.InstanceID] = st.Status

		_ = r.send(s, &agentpb.AgentMessage{
			Payload: &agentpb.AgentMessage_InstanceStatus{
				InstanceStatus: &agentpb.InstanceStatus{
					InstanceId: st.InstanceID,
					Status:     st.Status,
					PodName:    st.PodName,
					NodeName:   st.NodeName,
					Message:    st.Message,
					AccountId:  st.AccountID,
					ProjectId:  st.ProjectID,
					ObservedAt: timestamppb.New(st.ObservedAt),
				},
			},
		})
	}

	// An instance that was here last sweep and is gone now must be
	// reported terminated. Without this the control plane keeps billing
	// a workload that an eviction or node reboot destroyed — the pod is
	// gone and nothing else would ever say so.
	for instanceID := range r.lastReported {
		if seen[instanceID] {
			continue
		}
		delete(r.lastReported, instanceID)

		_ = r.send(s, &agentpb.AgentMessage{
			Payload: &agentpb.AgentMessage_InstanceStatus{
				InstanceStatus: &agentpb.InstanceStatus{
					InstanceId: instanceID,
					Status:     "terminated",
					Message:    "instance no longer present in cluster",
					ObservedAt: timestamppb.Now(),
				},
			},
		})
	}
}

func (r *Runner) send(s stream, msg *agentpb.AgentMessage) error {
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	return s.Send(msg)
}

func (r *Runner) replyError(s stream, requestID string, code agentpb.ErrorCode, message string) {
	_ = r.send(s, &agentpb.AgentMessage{
		RequestId: requestID,
		Payload: &agentpb.AgentMessage_Result{Result: &agentpb.CommandResult{
			Success:      false,
			ErrorCode:    code,
			ErrorMessage: message,
		}},
	})
}

// errorCodeFor maps a cluster error to the wire code.
//
// The control plane branches on these: RESOURCE_EXHAUSTED triggers
// reallocation, IMAGE_PULL is shown to the customer. Losing the
// distinction here would turn a recoverable race into a customer-visible
// failure.
func errorCodeFor(err error) agentpb.ErrorCode {
	switch {
	case errors.Is(err, cluster.ErrResourceExhausted):
		return agentpb.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED
	case errors.Is(err, cluster.ErrNotFound):
		return agentpb.ErrorCode_ERROR_CODE_NOT_FOUND
	case errors.Is(err, cluster.ErrImagePull):
		return agentpb.ErrorCode_ERROR_CODE_IMAGE_PULL
	default:
		return agentpb.ErrorCode_ERROR_CODE_CLUSTER_ERROR
	}
}
