// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	agentpb "github.com/FlashbackAi/teepin-core/pkg/agentpb"
)

// Stage 3 tunnel limits — deliberately conservative for a pilot. A large
// response or a slow customer connection must never be allowed to starve
// the shared control channel for every other request/report on the same
// provider session; these bounds are what keep that true. See the
// send/sendProxy split below for the specific hazard they guard against.
const (
	// proxyRequestTimeout bounds one proxied HTTP request end-to-end.
	// Deliberately much shorter than pendingTimeout (2 minutes, tuned for
	// image pulls): a customer's browser is waiting synchronously, and a
	// request that cannot complete in a minute is not going to complete.
	proxyRequestTimeout = 60 * time.Second

	// proxyIdleTimeout resets a request if no ProxyData arrives for this
	// long — catches a peer that went silent without closing cleanly.
	proxyIdleTimeout = 30 * time.Second

	// proxyMaxResponseBytes caps how much response body one request may
	// stream back before it is aborted (reset). Prevents one large
	// download from monopolising a provider's shared stream indefinitely.
	proxyMaxResponseBytes = 32 * 1024 * 1024

	// proxyMaxConcurrentPerSession caps how many proxied requests one
	// provider session serves at once. Beyond this, new requests are
	// rejected (503) rather than queued — bounds memory and stops one
	// instance's traffic from starving another instance on the same node.
	proxyMaxConcurrentPerSession = 32
)

// ProxyTarget resolves a customer-facing hostname to the provider session
// that should serve it. Implemented by a thin adapter over pkg/compute's
// store (instance ID -> provider_id, see Stage 3 defect 8) plus the
// cluster Registry (provider_id -> live session) — see Stage 3 plan B2.
type ProxyTarget interface {
	// ResolveProvider returns the provider ID that owns instanceID and the
	// container port to route to (persisted at create time — see
	// compute.instances.container_port, Stage 3 defect found while wiring
	// this), or false if the instance does not exist (or is not one this
	// cluster mode can proxy to — the direct/single-node path has no
	// sessions at all). Never distinguishes "does not exist" from
	// "belongs to another tenant": the edge is unauthenticated by design
	// (these are public URLs), so existence must not leak beyond what
	// proxying already reveals.
	ResolveProvider(ctx context.Context, instanceID string) (providerID string, port int32, ok bool)
}

// ProxyHandler serves customer HTTP traffic for *.{domain} by tunnelling it
// over the target instance's agent session — the only network path that
// reaches a home node behind NAT, and (once the DNS cutover in Stage 3 plan
// B1 lands) the datacenter path too. See pkg/cluster/registry.go's proxy
// stream machinery, mirrored on the LogChunk pattern.
type ProxyHandler struct {
	registry *Registry
	target   ProxyTarget

	// inFlight counts concurrent proxied requests PER PROVIDER, enforcing
	// proxyMaxConcurrentPerSession. Keyed by provider ID rather than a
	// single global counter: one busy node must not throttle every other
	// node's traffic.
	mu       sync.Mutex
	inFlight map[string]int
}

// NewProxyHandler builds a handler over the given registry and instance
// resolver.
func NewProxyHandler(registry *Registry, target ProxyTarget) *ProxyHandler {
	return &ProxyHandler{
		registry: registry,
		target:   target,
		inFlight: make(map[string]int),
	}
}

func (h *ProxyHandler) acquire(providerID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.inFlight[providerID] >= proxyMaxConcurrentPerSession {
		return false
	}
	h.inFlight[providerID]++
	return true
}

func (h *ProxyHandler) release(providerID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.inFlight[providerID]--
	if h.inFlight[providerID] <= 0 {
		delete(h.inFlight, providerID)
	}
}

// ServeInstance proxies one HTTP request/response to instanceID's agent
// session. instanceID is the bare TEEPIN instance ID (e.g. "inst-6fea56ce")
// — the caller (the host-routing middleware, Stage 3 plan B1) has already
// stripped the domain suffix from the request Host.
//
// Three offline cases, deliberately distinct (Stage 3 plan B6):
//  1. No session at request time: 503, no round trip.
//  2. Session dies before headers are sent: 502.
//  3. Session dies after headers are sent: connection closes without a
//     terminating chunk (a truncated response, which HTTP clients
//     correctly treat as an error) — the status code cannot change once
//     written, so this is the only honest option.
func (h *ProxyHandler) ServeInstance(w http.ResponseWriter, r *http.Request, instanceID string) {
	providerID, port, ok := h.target.ResolveProvider(r.Context(), instanceID)
	if !ok {
		http.Error(w, "this instance is not currently reachable", http.StatusNotFound)
		return
	}

	session, ok := h.registry.ByProvider(providerID)
	if !ok {
		http.Error(w, "the node hosting this instance is offline", http.StatusServiceUnavailable)
		return
	}

	if !h.acquire(providerID) {
		http.Error(w, "this node is handling too many requests right now; retry shortly", http.StatusServiceUnavailable)
		return
	}
	defer h.release(providerID)

	ctx, cancel := context.WithTimeout(r.Context(), proxyRequestTimeout)
	defer cancel()

	requestID := "proxy-" + instanceID + "-" + uuid.New().String()[:8]
	events := session.openProxyStream(requestID)
	defer session.closeProxyStream(requestID)

	if err := session.send(&agentpb.ControlMessage{
		RequestId: requestID,
		Payload: &agentpb.ControlMessage_ProxyRequest{
			ProxyRequest: &agentpb.ProxyRequest{
				InstanceId: instanceID,
				Method:     r.Method,
				Path:       r.URL.RequestURI(),
				Headers:    headersToProto(r.Header),
				HasBody:    r.ContentLength != 0, // 0 or -1 (unknown, chunked) both count as "there may be a body"
				Port:       port,
			},
		},
	}); err != nil {
		http.Error(w, "the node hosting this instance is offline", http.StatusServiceUnavailable)
		return
	}

	// Pump the request body as ProxyData chunks. Best-effort: a send
	// failure here means the connection is already gone, and the response
	// read loop below will discover that on its own.
	if r.Body != nil {
		go pumpRequestBody(ctx, session, requestID, r.Body)
	} else {
		_ = session.send(&agentpb.ControlMessage{
			RequestId: requestID,
			Payload:   &agentpb.ControlMessage_ProxyData{ProxyData: &agentpb.ProxyData{Eof: true}},
		})
	}

	h.relayResponse(ctx, w, events)
}

// pumpRequestBody streams a request body to the agent as ProxyData chunks,
// terminated by one Eof chunk. Runs in its own goroutine so a slow client
// upload does not block ServeInstance from starting to read the response —
// in principle a server could begin responding before the request body is
// fully sent (HTTP allows this; most simple proxied APIs will not, but
// nothing here assumes otherwise).
func pumpRequestBody(ctx context.Context, session *AgentSession, requestID string, body io.ReadCloser) {
	defer body.Close()
	buf := make([]byte, 32*1024)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		n, err := body.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if sendErr := session.send(&agentpb.ControlMessage{
				RequestId: requestID,
				Payload:   &agentpb.ControlMessage_ProxyData{ProxyData: &agentpb.ProxyData{Data: chunk}},
			}); sendErr != nil {
				return
			}
		}
		if err != nil {
			_ = session.send(&agentpb.ControlMessage{
				RequestId: requestID,
				Payload:   &agentpb.ControlMessage_ProxyData{ProxyData: &agentpb.ProxyData{Eof: true}},
			})
			return
		}
	}
}

// relayResponse reads ProxyResponse (once) then ProxyData chunks from the
// agent and writes them to w, flushing per chunk so streaming responses
// (SSE, chunked transfer) actually stream rather than buffering until EOF.
func (h *ProxyHandler) relayResponse(ctx context.Context, w http.ResponseWriter, events chan proxyEvent) {
	flusher, _ := w.(http.Flusher)
	headersSent := false
	var written int64

	idle := time.NewTimer(proxyIdleTimeout)
	defer idle.Stop()

	for {
		select {
		case <-ctx.Done():
			// Either proxyRequestTimeout elapsed or the customer's own
			// connection went away (r.Context() is the parent). Nothing
			// more to do: if headers were already sent, closing here
			// truncates the response, which is the correct signal.
			if !headersSent {
				http.Error(w, "the instance did not respond in time", http.StatusGatewayTimeout)
			}
			return

		case <-idle.C:
			if !headersSent {
				http.Error(w, "the instance stopped responding", http.StatusBadGateway)
			}
			return

		case ev, open := <-events:
			if !open {
				// Channel closed: AgentSession.Close() ran — the node
				// disconnected mid-request (Stage 3 plan B6, case 2/3).
				if !headersSent {
					http.Error(w, "the node hosting this instance became unreachable", http.StatusBadGateway)
				}
				return
			}
			idle.Reset(proxyIdleTimeout)

			if ev.response != nil {
				if ev.response.Error != "" {
					// The agent could not reach the instance at all (pod
					// gone, connection refused) — a clean, customer-facing
					// reason rather than a raw Kubernetes error.
					http.Error(w, fmt.Sprintf("this instance could not be reached: %s", ev.response.Error), http.StatusBadGateway)
					return
				}
				applyProtoHeaders(w.Header(), ev.response.Headers)
				status := int(ev.response.StatusCode)
				if status == 0 {
					status = http.StatusOK
				}
				w.WriteHeader(status)
				headersSent = true
				continue
			}

			if ev.data == nil {
				continue
			}
			if ev.data.Reset_ {
				// Agent-side abort (its own limit, or its target vanished
				// mid-stream). Headers may already be sent; either way,
				// stop writing and let the connection close without a
				// terminating chunk.
				return
			}
			if len(ev.data.Data) > 0 {
				written += int64(len(ev.data.Data))
				if written > proxyMaxResponseBytes {
					// Over the cap: stop relaying. The customer sees a
					// truncated response, which is honest — better than
					// buffering unboundedly or pretending completion.
					return
				}
				if !headersSent {
					w.WriteHeader(http.StatusOK)
					headersSent = true
				}
				_, _ = w.Write(ev.data.Data)
				if flusher != nil {
					flusher.Flush()
				}
			}
			if ev.data.Eof {
				return
			}
		}
	}
}

func headersToProto(h http.Header) []*agentpb.Header {
	out := make([]*agentpb.Header, 0, len(h))
	for name, values := range h {
		out = append(out, &agentpb.Header{Name: name, Values: values})
	}
	return out
}

func applyProtoHeaders(dst http.Header, headers []*agentpb.Header) {
	for _, h := range headers {
		for _, v := range h.Values {
			dst.Add(h.Name, v)
		}
	}
}
