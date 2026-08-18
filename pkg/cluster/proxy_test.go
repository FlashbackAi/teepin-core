// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	agentpb "github.com/FlashbackAi/teepin-core/pkg/agentpb"
)

// fakeProxyTarget resolves every instanceID to a fixed provider/port, or
// reports not-found when told to.
type fakeProxyTarget struct {
	providerID string
	port       int32
	found      bool
}

func (f *fakeProxyTarget) ResolveProvider(context.Context, string) (string, int32, bool) {
	return f.providerID, f.port, f.found
}

// simulateAgentReply plays the agent side of one proxied request: it reads
// the ProxyRequest the handler sent, then feeds back a ProxyResponse
// followed by ProxyData chunks, exactly as pkg/agentrunner's
// handleProxyRequest would. Runs in its own goroutine so the handler's
// synchronous ServeInstance can proceed concurrently.
func simulateAgentReply(session *AgentSession, status int32, headers map[string]string, body []byte, requestIDCh <-chan string) {
	requestID := <-requestIDCh

	var hdrs []*agentpb.Header
	for k, v := range headers {
		hdrs = append(hdrs, &agentpb.Header{Name: k, Values: []string{v}})
	}

	session.deliverProxyResponse(requestID, &agentpb.ProxyResponse{
		StatusCode: status,
		Headers:    hdrs,
	})
	if len(body) > 0 {
		session.deliverProxyData(requestID, &agentpb.ProxyData{Data: body})
	}
	session.deliverProxyData(requestID, &agentpb.ProxyData{Eof: true})
}

// newCapturingSession builds a session whose send() captures the
// request_id of every ProxyRequest sent, published on the returned
// channel (buffered 1 — one request per test).
func newCapturingSession(providerID string) (*AgentSession, chan string) {
	ids := make(chan string, 1)
	var session *AgentSession
	session = NewAgentSession(providerID, "us-east", "test", "", func(msg *agentpb.ControlMessage) error {
		if pr := msg.GetProxyRequest(); pr != nil {
			ids <- msg.RequestId
		}
		return nil
	})
	return session, ids
}

func TestProxyHandler_ServeInstance_FullRoundTrip(t *testing.T) {
	session, ids := newCapturingSession("provider-1")
	registry := NewRegistry()
	registry.Add(session)

	handler := NewProxyHandler(registry, &fakeProxyTarget{providerID: "provider-1", port: 8080, found: true})

	go simulateAgentReply(session, 200, map[string]string{"Content-Type": "text/plain"}, []byte("hello from instance"), ids)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeInstance(rec, req, "inst-abc12345")

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "hello from instance" {
		t.Errorf("body = %q, want %q", rec.Body.String(), "hello from instance")
	}
	if rec.Header().Get("Content-Type") != "text/plain" {
		t.Errorf("Content-Type = %q, want text/plain", rec.Header().Get("Content-Type"))
	}
}

// TestProxyHandler_NoSessionIs503 covers Stage 3 plan B6 case 1: no
// session at request time must fail fast (503), with no round trip at
// all — there is nothing to wait on.
func TestProxyHandler_NoSessionIs503(t *testing.T) {
	registry := NewRegistry() // empty: no sessions
	handler := NewProxyHandler(registry, &fakeProxyTarget{providerID: "provider-1", port: 80, found: true})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		handler.ServeInstance(rec, req, "inst-missing01")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeInstance did not return promptly with no session connected")
	}

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

// TestProxyHandler_UnresolvableInstanceIs404 covers the "instance does not
// exist, or belongs to another tenant" case — indistinguishable on
// purpose (see ProxyTarget's doc comment: the edge must not leak
// existence across tenants).
func TestProxyHandler_UnresolvableInstanceIs404(t *testing.T) {
	registry := NewRegistry()
	handler := NewProxyHandler(registry, &fakeProxyTarget{found: false})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeInstance(rec, req, "inst-doesnotexist")

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestProxyHandler_SessionDiesBeforeHeadersIs502 covers Stage 3 plan B6
// case 2: the agent session closes before ProxyResponse arrives. The
// customer must see a clean 502, not a hang until proxyRequestTimeout.
func TestProxyHandler_SessionDiesBeforeHeadersIs502(t *testing.T) {
	session, ids := newCapturingSession("provider-1")
	registry := NewRegistry()
	registry.Add(session)
	handler := NewProxyHandler(registry, &fakeProxyTarget{providerID: "provider-1", port: 80, found: true})

	go func() {
		<-ids // wait for the request to be sent, then simulate a disconnect
		session.Close()
	}()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		handler.ServeInstance(rec, req, "inst-diesearly1")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ServeInstance did not return promptly after the session closed mid-request")
	}

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

// TestProxyHandler_ConcurrencyCapReturns503 covers B5's per-provider
// concurrency gate: beyond proxyMaxConcurrentPerSession in-flight
// requests to the SAME provider, a new request is rejected immediately
// rather than queued — bounding memory and stopping one instance's
// traffic from starving another instance on the same node.
func TestProxyHandler_ConcurrencyCapReturns503(t *testing.T) {
	registry := NewRegistry()
	// A session whose send() never replies — every dispatched request
	// blocks, so this test can hold exactly proxyMaxConcurrentPerSession
	// slots open deterministically.
	session := NewAgentSession("provider-1", "us-east", "test", "", func(*agentpb.ControlMessage) error { return nil })
	registry.Add(session)

	handler := NewProxyHandler(registry, &fakeProxyTarget{providerID: "provider-1", port: 80, found: true})

	// Fill every slot by acquiring directly — exercising acquire/release
	// in isolation is more deterministic than racing proxyMaxConcurrentPerSession
	// real goroutines through ServeInstance to fill the gate first.
	for i := 0; i < proxyMaxConcurrentPerSession; i++ {
		if !handler.acquire("provider-1") {
			t.Fatalf("acquire failed before reaching the cap, at slot %d", i)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		handler.ServeInstance(rec, req, "inst-capped0001")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeInstance did not return promptly when the concurrency cap was full")
	}

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (concurrency cap)", rec.Code)
	}
}

// TestAgentSession_ProxyStream_UnknownRequestIDDropsSilently: a
// ProxyResponse/ProxyData arriving for a request_id with no registered
// stream (already closed, or a stray/duplicate delivery) must not panic
// or block — it is simply dropped.
func TestAgentSession_ProxyStream_UnknownRequestIDDropsSilently(t *testing.T) {
	session := NewAgentSession("provider-1", "us-east", "test", "", func(*agentpb.ControlMessage) error { return nil })

	done := make(chan struct{})
	go func() {
		session.deliverProxyResponse("req-never-opened", &agentpb.ProxyResponse{StatusCode: 200})
		session.deliverProxyData("req-never-opened", &agentpb.ProxyData{Data: []byte("x")})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("delivering to an unknown proxy request_id blocked or hung")
	}
}

// TestAgentSession_Close_ClosesProxyStreams confirms Close() tears down
// every open proxy stream (not just logStreams/pending), which is what
// lets ServeInstance's relayResponse loop observe the channel closing and
// return a 502 instead of hanging until proxyRequestTimeout.
func TestAgentSession_Close_ClosesProxyStreams(t *testing.T) {
	session := NewAgentSession("provider-1", "us-east", "test", "", func(*agentpb.ControlMessage) error { return nil })

	events := session.openProxyStream("req-1")
	session.Close()

	select {
	case _, open := <-events:
		if open {
			t.Error("expected the proxy stream channel to be closed, got an open event instead")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("proxy stream channel was not closed by Close()")
	}
}
