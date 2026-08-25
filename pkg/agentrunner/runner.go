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
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
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

// sendTimeout bounds a single write to the control plane.
//
// This is the liveness check that matters. gRPC's transport keepalive
// does NOT reliably detect a half-open connection here: grpc.NewClient
// manages the channel lazily, and once a stream is established the
// client will happily block in Recv() forever against a peer that has
// gone away. A Fargate task draining during a deploy produces exactly
// that — the control plane calls GracefulStop, the ALB drops the
// connection, and the agent notices nothing.
//
// Observed in production 2026-08-07: an agent held a dead connection for
// ten minutes with no reconnect attempt, while the control plane logged
// "cluster unreachable" every 30 seconds. Because writes happen at least
// every statusInterval, a blocked or failed write is the fastest honest
// signal that the connection is gone.
const sendTimeout = 20 * time.Second

// heartbeatInterval is how often the agent writes purely to prove the
// connection is alive. Combined with sendTimeout this bounds detection
// of a dead control plane at roughly 30 seconds, against the ten minutes
// observed before it existed.
const heartbeatInterval = 10 * time.Second

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

	// fail reports a dead connection to Run, which returns so the caller
	// reconnects. Buffered and sent non-blockingly: many goroutines may
	// notice the same failure, and only the first needs to be recorded.
	fail chan error

	// lastReported deduplicates status pushes: without it, every sweep
	// would resend the unchanged state of every instance, which at any
	// scale is a lot of traffic saying nothing.
	lastReported map[string]string
	statusMu     sync.Mutex

	// proxyBodies routes ControlMessage_ProxyData (request body chunks) to
	// the handleProxyRequest goroutine handling the same request_id.
	// Necessary because handleCommand dispatches every incoming
	// ControlMessage to its own goroutine (so a slow create does not stall
	// a delete behind it) — a ProxyRequest and its follow-up ProxyData
	// chunks arrive as SEPARATE messages, each independently dispatched,
	// so without this a body chunk would have no way to reach the
	// goroutine actually waiting for it.
	proxyBodies   map[string]chan *agentpb.ProxyData
	proxyBodiesMu sync.Mutex

	// execInputs routes ControlMessage_ExecInput (stdin bytes, resize,
	// cancel) to the handleExecStart goroutine handling the same
	// request_id — same reason proxyBodies exists: ExecStart and its
	// follow-up ExecInput messages arrive as separate, independently
	// dispatched ControlMessages.
	execInputs   map[string]chan *agentpb.ExecInput
	execInputsMu sync.Mutex
}

func New(cfg Config) *Runner {
	return &Runner{
		cfg:          cfg,
		lastReported: make(map[string]string),
		fail:         make(chan error, 1),
		proxyBodies:  make(map[string]chan *agentpb.ProxyData),
		execInputs:   make(map[string]chan *agentpb.ExecInput),
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
	// Forget what was reported on any previous connection.
	//
	// The control plane holds instance statuses in memory, so a restarted
	// task or a new connection starts from nothing. Meanwhile the status
	// sweep only sends CHANGES — so without this reset, an instance whose
	// state has not changed since the last connection is never re-sent,
	// and the control plane never learns it exists.
	//
	// The effect is severe: the instance runs, is billed, and is
	// completely invisible to its owner. Observed 2026-08-07, where two
	// running pods each appeared under only one of two API keys.
	// Re-registration must mean "here is everything", which is what the
	// proto's comment about treating every reconnect as a fresh source of
	// truth already promised.
	r.statusMu.Lock()
	r.lastReported = make(map[string]string)
	r.statusMu.Unlock()

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

	// cancel MUST run before wg.Wait below, or the deferred wait blocks
	// forever: the loop goroutines only exit on ctx.Done, so returning
	// from Run for any other reason (EOF, send failure) would hang here
	// and leak every goroutine from every past connection. Defers run in
	// reverse order, so this is declared AFTER the WaitGroup defer.
	ctx, cancel := context.WithCancel(ctx)

	// Inventory immediately on connect: the control plane treats every
	// reconnect as a fresh source of truth and has no capacity data
	// until this arrives.
	r.reportInventory(ctx, s)
	r.reportStatuses(ctx, s)

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); r.inventoryLoop(ctx, s) }()
	go func() { defer wg.Done(); r.statusLoop(ctx, s) }()
	go func() { defer wg.Done(); r.heartbeatLoop(ctx, s) }()

	// Reverse defer order: wait runs last, cancel first. Both are needed
	// — cancel alone would let goroutines outlive the connection and
	// write to a dead stream; wait alone would deadlock.
	defer wg.Wait()
	defer cancel()

	// Recv runs in its own goroutine so the loop below can also wake on a
	// send failure. Blocking directly on Recv is what let a dead
	// connection go unnoticed for ten minutes: the control plane had gone
	// away, but nothing was arriving to error on.
	received := make(chan *agentpb.ControlMessage)
	recvErr := make(chan error, 1)
	go func() {
		for {
			msg, err := s.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			select {
			case received <- msg:
			case <-ctx.Done():
				return
			}
		}
	}()

	for {
		select {
		case msg := <-received:
			// Each command runs in its own goroutine: a slow image pull
			// must not stall the delete or log request behind it.
			go r.handleCommand(ctx, s, msg)

		case err := <-recvErr:
			if err == io.EOF {
				return nil
			}
			return err

		case err := <-r.fail:
			// A write failed or hung: the connection is dead even though
			// Recv has not noticed. Returning triggers the caller's
			// reconnect with backoff.
			return fmt.Errorf("connection failed: %w", err)

		case <-ctx.Done():
			return ctx.Err()
		}
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

	case *agentpb.ControlMessage_ProxyRequest:
		r.handleProxyRequest(ctx, s, msg.RequestId, payload.ProxyRequest)

	case *agentpb.ControlMessage_ProxyData:
		r.deliverProxyBody(msg.RequestId, payload.ProxyData)

	case *agentpb.ControlMessage_ExecStart:
		r.handleExecStart(ctx, s, msg.RequestId, payload.ExecStart)

	case *agentpb.ControlMessage_ExecInput:
		r.deliverExecInput(msg.RequestId, payload.ExecInput)

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
		InstanceID:         cmd.InstanceId,
		AccountID:          cmd.AccountId,
		ProjectID:          cmd.ProjectId,
		Image:              cmd.Image,
		Command:            cmd.Command,
		Args:               cmd.Args,
		Env:                cmd.Env,
		Labels:             cmd.Labels,
		CPUUnits:           int(cmd.CpuUnits),
		MemoryGB:           int(cmd.MemoryGb),
		GPUResource:        cmd.GpuResource,
		GPUQuantity:        int(cmd.GpuQuantity),
		GPUVRAMGB:          int(cmd.GpuVramGb),
		NodeName:           cmd.NodeName,
		Ports:              ports,
		EndpointDomain:     cmd.EndpointDomain,
		EnableTLS:          cmd.EnableTls,
		TLSIssuer:          cmd.TlsIssuer,
		ImagePullSecret:    cmd.ImagePullSecret,
		StorageGB:          int(cmd.StorageGb),
		EphemeralStorageGB: int(cmd.EphemeralStorageGb),
		AlwaysPullImage:    cmd.AlwaysPullImage,
		NeverRestart:       cmd.NeverRestart,
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
			DnsName:     result.DNSName,
			TlsEnabled:  result.TLSEnabled,
			TlsReady:    result.TLSReady,
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

// Stage 3 tunnel constants — the agent's half of the limits described in
// pkg/cluster/proxy.go. Kept independent (not shared constants) because
// they bound different things: the control plane's limits are about one
// SHARED stream serving many providers; these are about one agent's own
// resource use talking to its local pod.
const (
	// proxyRequestBodyLimit caps how much of a request body this agent
	// will forward to a local pod — matches proxyMaxResponseBytes on the
	// control-plane side, applied to the opposite direction.
	proxyRequestBodyLimit = 32 * 1024 * 1024
	// proxyLocalTimeout bounds the agent's own HTTP call to the pod. Sits
	// safely inside the control plane's proxyRequestTimeout (60s) so the
	// agent is always the one to give up first and report a clean error,
	// rather than the edge timing out first with no explanation.
	proxyLocalTimeout = 45 * time.Second
)

// deliverProxyBody routes a request-body chunk to the handleProxyRequest
// goroutine handling the same request_id. See Runner.proxyBodies for why
// this indirection exists — ProxyRequest and ProxyData arrive as separate
// ControlMessages, each independently dispatched to its own goroutine.
func (r *Runner) deliverProxyBody(requestID string, data *agentpb.ProxyData) {
	r.proxyBodiesMu.Lock()
	ch, ok := r.proxyBodies[requestID]
	r.proxyBodiesMu.Unlock()

	if !ok {
		return
	}
	select {
	case ch <- data:
	case <-time.After(5 * time.Second):
		// Mirrors deliverLogChunk's control-plane-side guard: a request
		// whose handler has already given up must not block the shared
		// stream reader.
	}
}

// handleProxyRequest serves one Stage 3 tunnel request: resolve the
// target instance to a local address, replay the HTTP request against it,
// and stream the response back as ProxyResponse + ProxyData.
//
// Every failure path here reports a clean ProxyResponse.Error rather than
// letting the request simply time out on the control-plane side — an
// instance that is gone, unschedulable, or refusing connections is a
// normal, expected outcome for a customer's own workload, not an agent
// fault, and deserves a real error message at the edge (see
// pkg/cluster/proxy.go's relayResponse, which turns Error into a 502 with
// this text).
func (r *Runner) handleProxyRequest(ctx context.Context, s stream, requestID string, req *agentpb.ProxyRequest) {
	ctx, cancel := context.WithTimeout(ctx, proxyLocalTimeout)
	defer cancel()

	addr, err := r.cfg.Cluster.ResolveInstanceAddress(ctx, req.InstanceId, req.Port)
	if err != nil {
		_ = r.sendProxy(s, &agentpb.AgentMessage{
			RequestId: requestID,
			Payload: &agentpb.AgentMessage_ProxyResponse{ProxyResponse: &agentpb.ProxyResponse{
				Error: fmt.Sprintf("instance not reachable: %v", err),
			}},
		})
		return
	}

	var body io.Reader
	if req.HasBody {
		bodyCh := make(chan *agentpb.ProxyData, 64)
		r.proxyBodiesMu.Lock()
		r.proxyBodies[requestID] = bodyCh
		r.proxyBodiesMu.Unlock()
		defer func() {
			r.proxyBodiesMu.Lock()
			delete(r.proxyBodies, requestID)
			r.proxyBodiesMu.Unlock()
		}()
		body = newProxyBodyReader(ctx, bodyCh, proxyRequestBodyLimit)
	}

	url := fmt.Sprintf("http://%s%s", addr, req.Path)
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, url, body)
	if err != nil {
		_ = r.sendProxy(s, &agentpb.AgentMessage{
			RequestId: requestID,
			Payload: &agentpb.AgentMessage_ProxyResponse{ProxyResponse: &agentpb.ProxyResponse{
				Error: fmt.Sprintf("could not build request: %v", err),
			}},
		})
		return
	}
	for _, h := range req.Headers {
		for _, v := range h.Values {
			httpReq.Header.Add(h.Name, v)
		}
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		_ = r.sendProxy(s, &agentpb.AgentMessage{
			RequestId: requestID,
			Payload: &agentpb.AgentMessage_ProxyResponse{ProxyResponse: &agentpb.ProxyResponse{
				Error: fmt.Sprintf("connection failed: %v", err),
			}},
		})
		return
	}
	defer resp.Body.Close()

	if err := r.sendProxy(s, &agentpb.AgentMessage{
		RequestId: requestID,
		Payload: &agentpb.AgentMessage_ProxyResponse{ProxyResponse: &agentpb.ProxyResponse{
			StatusCode: int32(resp.StatusCode),
			Headers:    headersFromHTTP(resp.Header),
		}},
	}); err != nil {
		// The stream to the control plane is gone (proxySendTimeout, or
		// the connection dropped) — nothing more to send.
		return
	}

	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if sendErr := r.sendProxy(s, &agentpb.AgentMessage{
				RequestId: requestID,
				Payload:   &agentpb.AgentMessage_ProxyData{ProxyData: &agentpb.ProxyData{Data: chunk}},
			}); sendErr != nil {
				return
			}
		}
		if readErr != nil {
			eof := readErr == io.EOF
			_ = r.sendProxy(s, &agentpb.AgentMessage{
				RequestId: requestID,
				Payload: &agentpb.AgentMessage_ProxyData{ProxyData: &agentpb.ProxyData{
					Eof:    eof,
					Reset_: !eof, // a mid-body read error is an abort, not a clean end
				}},
			})
			return
		}
	}
}

// proxyBodyReader turns a channel of ProxyData chunks into an io.Reader,
// so the request body can be handed to http.NewRequestWithContext exactly
// like any other io.Reader — net/http drives the read loop, this just
// bridges the channel.
type proxyBodyReader struct {
	ctx     context.Context
	ch      <-chan *agentpb.ProxyData
	limit   int
	read    int
	pending []byte
	done    bool
}

func newProxyBodyReader(ctx context.Context, ch <-chan *agentpb.ProxyData, limit int) *proxyBodyReader {
	return &proxyBodyReader{ctx: ctx, ch: ch, limit: limit}
}

func (b *proxyBodyReader) Read(p []byte) (int, error) {
	for len(b.pending) == 0 {
		if b.done {
			return 0, io.EOF
		}
		select {
		case <-b.ctx.Done():
			return 0, b.ctx.Err()
		case chunk, open := <-b.ch:
			if !open {
				b.done = true
				return 0, io.EOF
			}
			if chunk.Reset_ {
				b.done = true
				return 0, fmt.Errorf("request body aborted by peer")
			}
			b.read += len(chunk.Data)
			if b.read > b.limit {
				b.done = true
				return 0, fmt.Errorf("request body exceeds %d bytes", b.limit)
			}
			b.pending = chunk.Data
			if chunk.Eof {
				b.done = true
			}
		}
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}

// ---------------------------------------------------------------------
// Interactive exec — "connect to instance terminal" from the console or
// the teepin CLI. Same request_id-correlation discipline as the Stage 3
// tunnel above, reusing cluster.ExecCapable's type assertion (see
// pkg/cluster/exec.go) so CPUOnly/Unavailable need no stub methods.
// ---------------------------------------------------------------------

// execAgentMaxDuration bounds one exec session on the agent side.
// Deliberately LONGER than the control plane's own session cap (60m,
// pkg/cluster's ExecHandler) — the agent holds the real resource (a
// live shell in a customer's pod), so a vanished or misbehaving control
// plane must not leave it running forever, but the CONTROL PLANE should
// be the one to produce the clean "session ended" message under normal
// operation, not have the agent cut it off first.
const execAgentMaxDuration = 65 * time.Minute

// deliverExecInput routes one ExecInput (stdin bytes, resize, stdin-close,
// or cancel) to the handleExecStart goroutine handling the same
// request_id. See Runner.execInputs for why this indirection exists.
func (r *Runner) deliverExecInput(requestID string, input *agentpb.ExecInput) {
	r.execInputsMu.Lock()
	ch, ok := r.execInputs[requestID]
	r.execInputsMu.Unlock()

	if !ok {
		return
	}
	select {
	case ch <- input:
	case <-time.After(5 * time.Second):
		// Mirrors deliverProxyBody's guard: a session whose handler has
		// already given up must not block the shared stream reader.
	}
}

// handleExecStart opens one interactive exec session: resolve the
// cluster client's exec capability, attach to the target pod/container,
// and pump stdin in / stdout out over the gRPC stream until the session
// ends.
func (r *Runner) handleExecStart(ctx context.Context, s stream, requestID string, req *agentpb.ExecStart) {
	// Register the input channel as the LITERAL FIRST statement, before
	// any resolution work below — a customer can type before ExecAttach
	// has even resolved the pod, and without this, handleProxyRequest's
	// same late-registration shape (a real, known gap there, papered
	// over only by a test-only poll helper) would silently drop those
	// keystrokes here instead of just delaying an HTTP body chunk.
	inputCh := make(chan *agentpb.ExecInput, 64)
	r.execInputsMu.Lock()
	r.execInputs[requestID] = inputCh
	r.execInputsMu.Unlock()
	defer func() {
		r.execInputsMu.Lock()
		delete(r.execInputs, requestID)
		r.execInputsMu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(ctx, execAgentMaxDuration)
	defer cancel()

	execer, ok := r.cfg.Cluster.(cluster.ExecCapable)
	if !ok {
		_ = r.sendExec(s, &agentpb.AgentMessage{
			RequestId: requestID,
			Payload: &agentpb.AgentMessage_ExecEnd{ExecEnd: &agentpb.ExecEnd{
				ErrorCode:    agentpb.ErrorCode_ERROR_CODE_EXEC_UNSUPPORTED,
				ErrorMessage: "this node cannot run interactive sessions",
			}},
		})
		return
	}

	stdinReader, stdinWriter := io.Pipe()
	resizeCh := make(chan cluster.TerminalSize, 1)

	// Demux goroutine: handleCommand dispatches ExecStart and every
	// follow-up ExecInput as independent messages (same reason
	// proxyBodies exists for the HTTP tunnel), so routing them onto the
	// single ExecIO the executor expects needs its own goroutine here.
	go func() {
		defer stdinWriter.Close()
		for {
			select {
			case in, open := <-inputCh:
				if !open {
					return
				}
				if len(in.Stdin) > 0 {
					if _, err := stdinWriter.Write(in.Stdin); err != nil {
						return
					}
				}
				if in.StdinClose {
					return
				}
				if in.Rows > 0 && in.Cols > 0 {
					sz := cluster.TerminalSize{Rows: uint16(in.Rows), Cols: uint16(in.Cols)}
					select {
					case resizeCh <- sz:
					default:
						// Non-blocking replace: last-write-wins, matching
						// terminalSizeQueue's own semantics on the other
						// side of this channel.
						select {
						case <-resizeCh:
						default:
						}
						select {
						case resizeCh <- sz:
						default:
						}
					}
				}
				if in.Cancel {
					cancel()
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	out := newExecOutputSender(ctx, r, s, requestID, agentpb.Stream_STREAM_STDOUT)
	defer out.flush()

	outcome, err := execer.ExecAttach(ctx, cluster.ExecRequest{
		InstanceID: req.InstanceId,
		Container:  req.Container,
		Command:    req.Command,
		TTY:        req.Tty,
		Rows:       uint16(req.Rows),
		Cols:       uint16(req.Cols),
	}, cluster.ExecIO{
		Stdin:  stdinReader,
		Stdout: out,
		Stderr: nil, // v1 is TTY-only; the API server rejects stderr alongside tty anyway
		Resize: resizeCh,
		OnOpen: func(podName, container string) {
			_ = r.sendExec(s, &agentpb.AgentMessage{
				RequestId: requestID,
				Payload: &agentpb.AgentMessage_ExecOpen{ExecOpen: &agentpb.ExecOpen{
					PodName:   podName,
					Container: container,
				}},
			})
		},
	})

	out.flush()

	if err != nil {
		code := agentpb.ErrorCode_ERROR_CODE_CLUSTER_ERROR
		switch {
		case errors.Is(err, cluster.ErrExecUnsupported):
			code = agentpb.ErrorCode_ERROR_CODE_EXEC_UNSUPPORTED
		case errors.Is(err, cluster.ErrNotFound):
			code = agentpb.ErrorCode_ERROR_CODE_NOT_FOUND
		}
		_ = r.sendExec(s, &agentpb.AgentMessage{
			RequestId: requestID,
			Payload: &agentpb.AgentMessage_ExecEnd{ExecEnd: &agentpb.ExecEnd{
				ErrorCode:    code,
				ErrorMessage: err.Error(),
			}},
		})
		return
	}

	_ = r.sendExec(s, &agentpb.AgentMessage{
		RequestId: requestID,
		Payload: &agentpb.AgentMessage_ExecEnd{ExecEnd: &agentpb.ExecEnd{
			ExitCode: int32(outcome.ExitCode),
		}},
	})
}

// Exec output tuning. Coalescing cuts message count 10-100x for chatty
// output (a build log, `cat` on a large file) at latency no human
// notices — SSH implementations do the same. The rate limit is the real
// protection for a residential uplink: WaitN blocks INSIDE Write, which
// blocks client-go's own SPDY read loop, which fills the pty buffer,
// which blocks the customer's process on write() — genuine end-to-end
// backpressure. There is deliberately no buffer here beyond the bounded
// coalescing window: turning that into a bytes.Buffer "to smooth things
// out" would convert real backpressure into unbounded memory growth,
// which is the single most damaging change anyone could make to this
// path (see the Stage 3 plan's head-of-line-blocking note — every write
// from an agent, including heartbeats, shares one sendMu).
const (
	execCoalesceWindow        = 10 * time.Millisecond
	execCoalesceMax           = 32 * 1024
	execOutputRateBytesPerSec = 2 * 1024 * 1024
	execOutputRateBurst       = 64 * 1024
)

// execOutputSender is the io.Writer handed to ExecIO.Stdout/Stderr. One
// per session, one per stream (stdout is a separate instance from
// stderr when both are in use).
type execOutputSender struct {
	ctx       context.Context
	r         *Runner
	s         stream
	requestID string
	kind      agentpb.Stream
	limiter   *rate.Limiter

	mu    sync.Mutex
	buf   []byte
	timer *time.Timer
}

func newExecOutputSender(ctx context.Context, r *Runner, s stream, requestID string, kind agentpb.Stream) *execOutputSender {
	return &execOutputSender{
		ctx:       ctx,
		r:         r,
		s:         s,
		requestID: requestID,
		kind:      kind,
		limiter:   rate.NewLimiter(rate.Limit(execOutputRateBytesPerSec), execOutputRateBurst),
	}
}

func (o *execOutputSender) Write(p []byte) (int, error) {
	if err := o.limiter.WaitN(o.ctx, len(p)); err != nil {
		return 0, err
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	o.buf = append(o.buf, p...)
	if o.timer == nil {
		o.timer = time.AfterFunc(execCoalesceWindow, o.flush)
	}
	if len(o.buf) >= execCoalesceMax {
		o.flushLocked()
	}
	return len(p), nil
}

func (o *execOutputSender) flush() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.flushLocked()
}

// flushLocked must be called with mu held.
func (o *execOutputSender) flushLocked() {
	if o.timer != nil {
		o.timer.Stop()
		o.timer = nil
	}
	if len(o.buf) == 0 {
		return
	}
	data := o.buf
	o.buf = nil
	_ = o.r.sendExec(o.s, &agentpb.AgentMessage{
		RequestId: o.requestID,
		Payload: &agentpb.AgentMessage_ExecOutput{ExecOutput: &agentpb.ExecOutput{
			Data:   data,
			Stream: o.kind,
		}},
	})
}

func headersFromHTTP(h http.Header) []*agentpb.Header {
	out := make([]*agentpb.Header, 0, len(h))
	for name, values := range h {
		out = append(out, &agentpb.Header{Name: name, Values: values})
	}
	return out
}

// heartbeatLoop writes to the control plane on a fixed interval.
//
// The other loops are not sufficient as a liveness signal: status sweeps
// only send on a CHANGE, so a steady-state agent with nothing happening
// can go minutes without writing anything. Inventory does write every
// 30s, but tying connection detection to it means a change in inventory
// cadence silently changes failure detection. An explicit heartbeat
// keeps the two concerns separate.
//
// The Pong message type is reused rather than adding a new one: the
// control plane already ignores unsolicited Pongs, so this needs no
// server-side change and no proto version bump.
func (r *Runner) heartbeatLoop(ctx context.Context, s stream) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// A failure here reports through send() and ends the
			// connection, which is the entire point of the heartbeat.
			_ = r.send(s, &agentpb.AgentMessage{
				Payload: &agentpb.AgentMessage_Pong{Pong: &agentpb.Pong{
					SentAt: timestamppb.Now(),
				}},
			})
		case <-ctx.Done():
			return
		}
	}
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
			// Whether THIS session's own cluster can schedule work right now
			// — checked fresh on every report so a home node's k3s crashing
			// after connect is reflected within one inventoryInterval, not
			// frozen at whatever it was when the agent started.
			ClusterReady: r.cfg.Cluster.Healthy(ctx),
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
		//
		// The dedup key is composite (status PLUS endpoint fields), not
		// status alone: the TLS-ready flip (cert-manager finishing
		// issuance 30-90s after create) happens with the pod's status
		// unchanged the whole time ("running" before and after). A
		// status-only key would suppress that transition forever — it
		// would never be seen as "changed" — and the control plane would
		// never learn a certificate became ready (Stage 3 plan A6).
		key := fmt.Sprintf("%s|%s|%s|%s|%t|%t",
			st.Status, st.EndpointURL, st.DNSName, st.PublicIP, st.TLSEnabled, st.TLSReady)
		if previous, ok := r.lastReported[st.InstanceID]; ok && previous == key {
			continue
		}
		r.lastReported[st.InstanceID] = key

		_ = r.send(s, &agentpb.AgentMessage{
			Payload: &agentpb.AgentMessage_InstanceStatus{
				InstanceStatus: &agentpb.InstanceStatus{
					InstanceId:  st.InstanceID,
					Status:      st.Status,
					PodName:     st.PodName,
					NodeName:    st.NodeName,
					Message:     st.Message,
					AccountId:   st.AccountID,
					ProjectId:   st.ProjectID,
					ObservedAt:  timestamppb.New(st.ObservedAt),
					EndpointUrl: st.EndpointURL,
					DnsName:     st.DNSName,
					PublicIp:    st.PublicIP,
					TlsEnabled:  st.TLSEnabled,
					TlsReady:    st.TLSReady,
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

// send writes to the control plane, treating a failure or a hang as a
// dead connection.
//
// The timeout is the important part. gRPC's SendMsg can block
// indefinitely when the peer has gone away without closing (a drained
// Fargate task behind an ALB), and the agent would otherwise sit there
// believing it is connected while the control plane reports no capacity.
func (r *Runner) send(s stream, msg *agentpb.AgentMessage) error {
	r.sendMu.Lock()
	defer r.sendMu.Unlock()

	done := make(chan error, 1)
	go func() { done <- s.Send(msg) }()

	select {
	case err := <-done:
		if err != nil {
			r.reportFailure(err)
		}
		return err

	case <-time.After(sendTimeout):
		// The goroutine above is left running: it holds sendMu's caller
		// contract only for this call, and it will exit when the stream
		// is finally torn down. Leaking it briefly is preferable to
		// blocking the agent forever.
		err := fmt.Errorf("send timed out after %s: connection presumed dead", sendTimeout)
		r.reportFailure(err)
		return err
	}
}

// proxySendTimeout bounds one write of a proxied response chunk.
// Deliberately the SAME order of magnitude as sendTimeout, not longer —
// see sendProxy for why a longer timeout would be the wrong fix here.
const proxySendTimeout = 20 * time.Second

// execSendTimeout bounds one write of exec output or an ExecOpen/ExecEnd
// message. Tighter than proxySendTimeout: an exec session is long-lived
// (minutes to hours, not one 60s request), so every write from an agent —
// heartbeats, inventory, HTTP proxy chunks, now exec output — serializes
// behind the same sendMu for that much longer. A write that cannot finish
// in 5 seconds means the shared connection is congested; killing one
// shell beats stalling every heartbeat and status report for 20 seconds.
const execSendTimeout = 5 * time.Second

// sendWithTimeout is the shared body of sendProxy and sendExec: a write
// that shares sendMu (gRPC forbids concurrent SendMsg regardless of which
// logical purpose is writing) but, unlike send, does NOT call
// reportFailure on timeout. A customer's slow HTTP connection or shell
// session is not evidence the control-plane connection is dead; send's
// reportFailure path exists for exactly the opposite case (the CONTROL
// channel itself hanging, e.g. a drained Fargate task behind the ALB with
// no FIN). Routing these writes through send would let one slow customer
// request or session tear down the entire agent connection — killing
// status reporting, inventory, and every other in-flight request for
// this provider — which is the single biggest risk flagged in the
// Stage 3 plan (B5). A stuck send here instead just fails that one call;
// the caller treats it as "this request's stream is gone" and aborts
// only that request.
func (r *Runner) sendWithTimeout(s stream, msg *agentpb.AgentMessage, timeout time.Duration) error {
	r.sendMu.Lock()
	defer r.sendMu.Unlock()

	done := make(chan error, 1)
	go func() { done <- s.Send(msg) }()

	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		// Leaked goroutine is bounded the same way send's is: it exits
		// once the underlying Send eventually returns or the stream is
		// torn down elsewhere.
		return fmt.Errorf("stream send timed out after %s", timeout)
	}
}

func (r *Runner) sendProxy(s stream, msg *agentpb.AgentMessage) error {
	return r.sendWithTimeout(s, msg, proxySendTimeout)
}

func (r *Runner) sendExec(s stream, msg *agentpb.AgentMessage) error {
	return r.sendWithTimeout(s, msg, execSendTimeout)
}

// reportFailure records the first error that indicates a dead
// connection, waking Run so the caller reconnects.
func (r *Runner) reportFailure(err error) {
	select {
	case r.fail <- err:
	default:
		// Already reporting a failure; the first one is the useful one.
	}
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
