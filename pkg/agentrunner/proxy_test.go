// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package agentrunner

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	agentpb "github.com/FlashbackAi/teepin-core/pkg/agentpb"
	"github.com/FlashbackAi/teepin-core/pkg/cluster"
)

// addressCluster resolves ResolveInstanceAddress to a fixed address (an
// httptest.Server's host:port, stripped of its scheme) — standing in for
// the pod IP a real DirectClient would return, with zero Kubernetes
// involved. err, when set, makes resolution fail (an instance that is not
// running here).
type addressCluster struct {
	nullCluster
	addr string
	err  error
}

func (a addressCluster) ResolveInstanceAddress(context.Context, string, int32) (string, error) {
	if a.err != nil {
		return "", a.err
	}
	return a.addr, nil
}

// snapshot returns a copy of every message sent so far, for a test to
// inspect without racing further sends.
func (s *stubStream) snapshot() []*agentpb.AgentMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*agentpb.AgentMessage, len(s.sent))
	copy(out, s.sent)
	return out
}

func stripScheme(url string) string {
	return strings.TrimPrefix(strings.TrimPrefix(url, "http://"), "https://")
}

// waitForProxyBodyRegistered polls until handleProxyRequest has registered
// its body channel for requestID (or the deadline passes). Needed because
// registration happens inside handleProxyRequest, asynchronously relative
// to a test's own deliverProxyBody calls — the real control plane has the
// same ordering constraint (it only starts pumping a request body after
// its own ProxyRequest send completes).
func waitForProxyBodyRegistered(r *Runner, requestID string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r.proxyBodiesMu.Lock()
		_, ok := r.proxyBodies[requestID]
		r.proxyBodiesMu.Unlock()
		if ok {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// TestHandleProxyRequest_FullRoundTrip is the highest-value test in Phase
// B per the Stage 3 plan: it drives handleProxyRequest against a REAL
// httptest.Server standing in for the customer's pod — zero Kubernetes,
// zero network beyond localhost — and asserts the full ProxyResponse +
// ProxyData sequence a real agent would emit.
func TestHandleProxyRequest_FullRoundTrip(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/hello" {
			t.Errorf("backend saw path %q, want /hello", r.URL.Path)
		}
		w.Header().Set("X-Custom", "instance-value")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello from the pod"))
	}))
	defer backend.Close()

	r := New(Config{
		ProviderID: "test-provider",
		Region:     "us-east",
		Version:    "test",
		Cluster:    addressCluster{addr: stripScheme(backend.URL)},
	})
	s := newStubStream()

	r.handleProxyRequest(context.Background(), s, "req-1", &agentpb.ProxyRequest{
		InstanceId: "inst-abc12345",
		Method:     http.MethodGet,
		Path:       "/hello",
		Port:       80,
	})

	sent := s.snapshot()
	if len(sent) < 2 {
		t.Fatalf("expected at least a ProxyResponse and a terminating ProxyData, got %d messages", len(sent))
	}

	resp := sent[0].GetProxyResponse()
	if resp == nil {
		t.Fatalf("first message was not a ProxyResponse: %+v", sent[0])
	}
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}
	if resp.Error != "" {
		t.Errorf("Error = %q, want empty", resp.Error)
	}
	found := false
	for _, h := range resp.Headers {
		if h.Name == "X-Custom" && len(h.Values) == 1 && h.Values[0] == "instance-value" {
			found = true
		}
	}
	if !found {
		t.Errorf("response headers missing X-Custom: %v", resp.Headers)
	}

	var body []byte
	var sawEOF bool
	for _, msg := range sent[1:] {
		data := msg.GetProxyData()
		if data == nil {
			continue
		}
		body = append(body, data.Data...)
		if data.Eof {
			sawEOF = true
		}
		if data.Reset_ {
			t.Error("unexpected Reset_ on a clean response")
		}
	}
	if string(body) != "hello from the pod" {
		t.Errorf("body = %q, want %q", string(body), "hello from the pod")
	}
	if !sawEOF {
		t.Error("no terminating Eof ProxyData chunk was sent")
	}
}

// TestHandleProxyRequest_RequestBodyForwarded covers the request-body
// direction (plan verification step "request-body direction works"): a
// POST body delivered via ProxyData chunks must reach the backend intact.
func TestHandleProxyRequest_RequestBodyForwarded(t *testing.T) {
	received := make(chan string, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		received <- string(b)
		w.WriteHeader(http.StatusCreated)
	}))
	defer backend.Close()

	r := New(Config{
		ProviderID: "test-provider",
		Region:     "us-east",
		Version:    "test",
		Cluster:    addressCluster{addr: stripScheme(backend.URL)},
	})
	s := newStubStream()

	done := make(chan struct{})
	go func() {
		r.handleProxyRequest(context.Background(), s, "req-body-1", &agentpb.ProxyRequest{
			InstanceId: "inst-body0001",
			Method:     http.MethodPost,
			Path:       "/upload",
			HasBody:    true,
			Port:       80,
		})
		close(done)
	}()

	// handleProxyRequest registers its body channel asynchronously (after
	// resolving the address), so deliverProxyBody must not race ahead of
	// that registration — exactly like the real control plane, which only
	// starts pumping the request body once it knows the ProxyRequest was
	// sent. Poll briefly for registration, mirroring that ordering.
	if !waitForProxyBodyRegistered(r, "req-body-1", 2*time.Second) {
		t.Fatal("handleProxyRequest never registered its proxy body channel")
	}

	// Deliver the body as ProxyData chunks, exactly like the control
	// plane's pumpRequestBody would, then signal EOF.
	r.deliverProxyBody("req-body-1", &agentpb.ProxyData{Data: []byte("chunk-one-")})
	r.deliverProxyBody("req-body-1", &agentpb.ProxyData{Data: []byte("chunk-two")})
	r.deliverProxyBody("req-body-1", &agentpb.ProxyData{Eof: true})

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handleProxyRequest did not complete")
	}

	select {
	case body := <-received:
		if body != "chunk-one-chunk-two" {
			t.Errorf("backend received body %q, want %q", body, "chunk-one-chunk-two")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("backend never received a request")
	}

	sent := s.snapshot()
	if len(sent) == 0 || sent[0].GetProxyResponse() == nil || sent[0].GetProxyResponse().StatusCode != 201 {
		t.Errorf("expected a 201 ProxyResponse, got %+v", sent)
	}
}

// TestHandleProxyRequest_UnreachableInstanceReportsError covers the
// resolve-failure path: an instance not running here (deleted, or a
// resolve error) must produce a ProxyResponse.Error, not a hang or panic —
// this is what pkg/cluster's relayResponse turns into a customer-facing
// 502 (see handleProxyRequest's own doc comment).
func TestHandleProxyRequest_UnreachableInstanceReportsError(t *testing.T) {
	r := New(Config{
		ProviderID: "test-provider",
		Region:     "us-east",
		Version:    "test",
		Cluster:    addressCluster{err: cluster.ErrNotFound},
	})
	s := newStubStream()

	r.handleProxyRequest(context.Background(), s, "req-gone-1", &agentpb.ProxyRequest{
		InstanceId: "inst-gone00001",
		Method:     http.MethodGet,
		Path:       "/",
		Port:       80,
	})

	sent := s.snapshot()
	if len(sent) != 1 {
		t.Fatalf("expected exactly one message (the error response), got %d", len(sent))
	}
	resp := sent[0].GetProxyResponse()
	if resp == nil || resp.Error == "" {
		t.Fatalf("expected a ProxyResponse with a non-empty Error, got %+v", sent[0])
	}
}

// TestHandleProxyRequest_ConnectionRefusedReportsError covers the case
// where the address resolves but nothing is listening (the pod is
// starting, or crashed) — a normal, expected outcome for a customer's own
// workload, not an agent fault, and must still produce a clean error
// response rather than hanging until the caller's own timeout.
func TestHandleProxyRequest_ConnectionRefusedReportsError(t *testing.T) {
	r := New(Config{
		ProviderID: "test-provider",
		Region:     "us-east",
		Version:    "test",
		// Port 1 on localhost: nothing listens there, and it never will —
		// deterministic "connection refused" with no external dependency.
		Cluster: addressCluster{addr: "127.0.0.1:1"},
	})
	s := newStubStream()

	done := make(chan struct{})
	go func() {
		r.handleProxyRequest(context.Background(), s, "req-refused1", &agentpb.ProxyRequest{
			InstanceId: "inst-refused001",
			Method:     http.MethodGet,
			Path:       "/",
			Port:       1,
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleProxyRequest did not return promptly on connection refused")
	}

	sent := s.snapshot()
	if len(sent) != 1 {
		t.Fatalf("expected exactly one message (the error response), got %d", len(sent))
	}
	resp := sent[0].GetProxyResponse()
	if resp == nil || resp.Error == "" {
		t.Fatalf("expected a ProxyResponse with a non-empty Error, got %+v", sent[0])
	}
}

// TestHandleProxyRequest_UsesRequestPort confirms the port threaded
// through ProxyRequest.Port actually reaches ResolveInstanceAddress — the
// exact wiring fixed in this pass (previously a stub always passing 0).
func TestHandleProxyRequest_UsesRequestPort(t *testing.T) {
	var gotPort int32 = -1
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	cl := recordingPortCluster{addr: stripScheme(backend.URL), gotPort: &gotPort}
	r := New(Config{
		ProviderID: "test-provider",
		Region:     "us-east",
		Version:    "test",
		Cluster:    cl,
	})
	s := newStubStream()

	r.handleProxyRequest(context.Background(), s, "req-port-1", &agentpb.ProxyRequest{
		InstanceId: "inst-port00001",
		Method:     http.MethodGet,
		Path:       "/",
		Port:       8080,
	})

	if gotPort != 8080 {
		t.Errorf("ResolveInstanceAddress received port %d, want 8080 (from ProxyRequest.Port)", gotPort)
	}
}

type recordingPortCluster struct {
	nullCluster
	addr    string
	gotPort *int32
}

func (c recordingPortCluster) ResolveInstanceAddress(_ context.Context, _ string, port int32) (string, error) {
	*c.gotPort = port
	return c.addr, nil
}
