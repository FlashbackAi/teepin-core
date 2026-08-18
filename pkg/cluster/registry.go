// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	agentpb "github.com/FlashbackAi/teepin-core/pkg/agentpb"
)

// pendingTimeout bounds how long a caller waits for an agent to answer.
//
// Generous, because the agent may be pulling a multi-gigabyte image
// before it can report success, and the alternative — failing a request
// that is actually progressing — leaves an instance running that the
// control plane believes never started.
const pendingTimeout = 2 * time.Minute

// AgentSession is one connected agent: a GPU provider holding a stream
// open to the control plane.
//
// The agent dials out, so this is always created by the server side
// accepting a Connect call. Sessions are addressed by provider ID; a
// reconnect replaces the previous session for that provider.
type AgentSession struct {
	ProviderID string
	Region     string
	Version    string
	// Class is "datacenter" (shared-token agent) or "home" (per-node
	// credential). Carried so the write-through persistence records the
	// right class without re-resolving the credential on every report.
	Class      string
	ConnectedA time.Time

	// send serialises writes to the stream. gRPC streams are not safe for
	// concurrent SendMsg, and the control plane dispatches from many
	// request goroutines at once.
	send func(*agentpb.ControlMessage) error

	mu sync.Mutex

	// pending maps request_id to the channel awaiting its CommandResult.
	pending map[string]chan *agentpb.CommandResult

	// logStreams maps request_id to the channel receiving LogChunks.
	// Separate from pending because a log request produces many messages
	// before its terminating result.
	logStreams map[string]chan *agentpb.LogChunk

	// proxyStreams maps request_id to the channel receiving one
	// ProxyResponse (headers, exactly once) followed by ProxyData chunks
	// (the response body) — Stage 3's HTTP tunnel for customer endpoints.
	// Same shape as logStreams deliberately: a stalled reader (the
	// customer's connection closed) must not block the shared stream
	// reader that serves every other request for this provider, which is
	// exactly what deliverLogChunk's timeout-drop already guards against.
	proxyStreams map[string]chan proxyEvent

	// inventory is the last report received. Read by the allocator on
	// every placement decision, so it is kept here rather than fetched
	// on demand — a round trip to the GPU datacenter per allocation
	// would add latency to every create.
	inventory   []NodeInventory
	inventoryAt time.Time

	closed bool
}

// NewAgentSession creates a session over a stream send function. class is
// "datacenter" unless the agent authenticated with a per-node credential.
func NewAgentSession(providerID, region, version, class string, send func(*agentpb.ControlMessage) error) *AgentSession {
	if class == "" {
		class = "datacenter"
	}
	return &AgentSession{
		ProviderID:   providerID,
		Region:       region,
		Version:      version,
		Class:        class,
		ConnectedA:   time.Now().UTC(),
		send:         send,
		pending:      make(map[string]chan *agentpb.CommandResult),
		logStreams:   make(map[string]chan *agentpb.LogChunk),
		proxyStreams: make(map[string]chan proxyEvent),
	}
}

// proxyEvent is one message on a proxy response stream: either the
// one-time ProxyResponse (headers) or a ProxyData chunk (body), never
// both — mirroring the order the agent actually sends them (response once,
// then zero or more data chunks).
type proxyEvent struct {
	response *agentpb.ProxyResponse
	data     *agentpb.ProxyData
}

// dispatch sends a command and waits for its result.
//
// Every command carries a request_id the agent echoes back. Without that
// correlation a single stream could not multiplex concurrent commands —
// two simultaneous creates would race for each other's answers.
func (s *AgentSession) dispatch(ctx context.Context, msg *agentpb.ControlMessage) (*agentpb.CommandResult, error) {
	requestID := msg.RequestId
	if requestID == "" {
		requestID = uuid.New().String()
		msg.RequestId = requestID
	}

	// Buffered: the reader goroutine must never block delivering a
	// result, even if this caller has already timed out and gone away.
	resultCh := make(chan *agentpb.CommandResult, 1)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrClusterUnavailable
	}
	s.pending[requestID] = resultCh
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.pending, requestID)
		s.mu.Unlock()
	}()

	if err := s.send(msg); err != nil {
		return nil, fmt.Errorf("%w: send failed: %v", ErrClusterUnavailable, err)
	}

	timeout := time.NewTimer(pendingTimeout)
	defer timeout.Stop()

	select {
	case result := <-resultCh:
		return result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timeout.C:
		// The command may still be executing on the agent. Commands are
		// idempotent by request_id precisely so that a retry after this
		// does not create a second instance.
		return nil, fmt.Errorf("%w: agent did not respond within %s", ErrClusterUnavailable, pendingTimeout)
	}
}

// deliverResult routes a CommandResult to whoever is waiting for it.
// Unknown request IDs are dropped: the caller timed out, or the result
// arrived after a reconnect.
func (s *AgentSession) deliverResult(requestID string, result *agentpb.CommandResult) {
	s.mu.Lock()
	ch, ok := s.pending[requestID]
	s.mu.Unlock()

	if ok {
		// Non-blocking: the channel is buffered for exactly one result,
		// and a duplicate must not wedge the reader goroutine.
		select {
		case ch <- result:
		default:
		}
	}
}

// deliverLogChunk routes a LogChunk to its stream.
func (s *AgentSession) deliverLogChunk(requestID string, chunk *agentpb.LogChunk) {
	s.mu.Lock()
	ch, ok := s.logStreams[requestID]
	s.mu.Unlock()

	if !ok {
		return
	}

	select {
	case ch <- chunk:
	case <-time.After(5 * time.Second):
		// A consumer that has stopped reading (customer closed the
		// connection) must not block the shared stream reader, which
		// serves every other command for this provider.
	}
}

// openLogStream registers a channel for a log request.
func (s *AgentSession) openLogStream(requestID string) chan *agentpb.LogChunk {
	ch := make(chan *agentpb.LogChunk, 64)

	s.mu.Lock()
	s.logStreams[requestID] = ch
	s.mu.Unlock()

	return ch
}

func (s *AgentSession) closeLogStream(requestID string) {
	s.mu.Lock()
	delete(s.logStreams, requestID)
	s.mu.Unlock()
}

// deliverProxyResponse routes the one-time response-headers message to its
// stream.
func (s *AgentSession) deliverProxyResponse(requestID string, resp *agentpb.ProxyResponse) {
	s.mu.Lock()
	ch, ok := s.proxyStreams[requestID]
	s.mu.Unlock()

	if !ok {
		return
	}

	select {
	case ch <- proxyEvent{response: resp}:
	case <-time.After(5 * time.Second):
		// Same reasoning as deliverLogChunk: a customer who closed their
		// connection must not block the shared stream reader.
	}
}

// deliverProxyData routes a response-body chunk to its stream.
func (s *AgentSession) deliverProxyData(requestID string, data *agentpb.ProxyData) {
	s.mu.Lock()
	ch, ok := s.proxyStreams[requestID]
	s.mu.Unlock()

	if !ok {
		return
	}

	select {
	case ch <- proxyEvent{data: data}:
	case <-time.After(5 * time.Second):
	}
}

// openProxyStream registers a channel for one proxied HTTP request.
func (s *AgentSession) openProxyStream(requestID string) chan proxyEvent {
	ch := make(chan proxyEvent, 64)

	s.mu.Lock()
	s.proxyStreams[requestID] = ch
	s.mu.Unlock()

	return ch
}

func (s *AgentSession) closeProxyStream(requestID string) {
	s.mu.Lock()
	delete(s.proxyStreams, requestID)
	s.mu.Unlock()
}

// setInventory stores the latest capacity report.
func (s *AgentSession) setInventory(nodes []NodeInventory) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inventory = nodes
	s.inventoryAt = time.Now().UTC()
}

// Inventory returns the last reported capacity and when it was observed.
func (s *AgentSession) Inventory() ([]NodeInventory, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inventory, s.inventoryAt
}

// Close marks the session dead and fails everything waiting on it.
//
// Waking blocked callers matters: without it, every in-flight request
// would sit until its two-minute timeout after an agent disconnects,
// holding HTTP connections open for no reason.
func (s *AgentSession) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return
	}
	s.closed = true

	failure := &agentpb.CommandResult{
		Success:      false,
		ErrorCode:    agentpb.ErrorCode_ERROR_CODE_CLUSTER_ERROR,
		ErrorMessage: "agent disconnected",
	}
	for id, ch := range s.pending {
		select {
		case ch <- failure:
		default:
		}
		delete(s.pending, id)
	}

	for id, ch := range s.logStreams {
		close(ch)
		delete(s.logStreams, id)
	}

	// Closing every proxy stream is what lets an in-flight customer HTTP
	// request fail immediately on disconnect (502, "node became
	// unreachable") instead of hanging until its own timeout — see Stage 3
	// plan B6.
	for id, ch := range s.proxyStreams {
		close(ch)
		delete(s.proxyStreams, id)
	}
}

// Registry tracks connected agents.
//
// One control plane serves many GPU providers, and agents connect and
// disconnect independently — a provider rebooting a node must not affect
// another provider's customers.
type Registry struct {
	mu       sync.RWMutex
	sessions map[string]*AgentSession
}

func NewRegistry() *Registry {
	return &Registry{sessions: make(map[string]*AgentSession)}
}

// Add registers a session, replacing any existing one for the provider.
//
// Replacement rather than rejection: an agent that lost its network
// without a clean shutdown leaves a stale session behind, and refusing
// the reconnect would strand that provider until the old session's
// keepalive expired.
func (r *Registry) Add(session *AgentSession) {
	r.mu.Lock()
	previous, exists := r.sessions[session.ProviderID]
	r.sessions[session.ProviderID] = session
	r.mu.Unlock()

	if exists {
		previous.Close()
	}
}

// Remove drops a session if it is still the current one for its
// provider. The identity check prevents a slow disconnect from evicting
// the session that already replaced it.
func (r *Registry) Remove(session *AgentSession) {
	r.mu.Lock()
	if current, ok := r.sessions[session.ProviderID]; ok && current == session {
		delete(r.sessions, session.ProviderID)
	}
	r.mu.Unlock()

	session.Close()
}

// Any returns a connected session, or false when none are.
//
// Used only when the control plane has NOT resolved a specific provider —
// the datacenter single-provider path. Multi-provider placement resolves a
// target and calls ByProvider instead; see AgentClient.CreateInstance.
func (r *Registry) Any() (*AgentSession, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, session := range r.sessions {
		return session, true
	}
	return nil, false
}

// ByProvider returns the session for a specific provider, or false if that
// provider is not currently connected.
//
// This is what makes multi-provider placement correct: a command allocated
// against provider B's node MUST go to B's session, not to whichever session
// a map iteration happened to yield first (the old Any() behaviour, which
// was a latent bug the moment a second provider connected).
func (r *Registry) ByProvider(providerID string) (*AgentSession, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	session, ok := r.sessions[providerID]
	return session, ok
}

// Get returns one provider's session.
func (r *Registry) Get(providerID string) (*AgentSession, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	session, ok := r.sessions[providerID]
	return session, ok
}

// Count returns how many agents are connected. Surfaced by the health
// endpoint so an operator can tell "no capacity" from "no agents".
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.sessions)
}

// Sessions returns every connected session.
func (r *Registry) Sessions() []*AgentSession {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]*AgentSession, 0, len(r.sessions))
	for _, session := range r.sessions {
		out = append(out, session)
	}
	return out
}
