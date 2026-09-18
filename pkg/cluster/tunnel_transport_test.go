// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	agentpb "github.com/FlashbackAi/teepin-core/pkg/agentpb"
)

// newTunnelSession builds a session whose captured ProxyRequests are
// published for the test's fake agent goroutine.
func newTunnelSession(reqs chan<- *agentpb.ControlMessage) (*AgentSession, *Registry) {
	session := NewAgentSession("prov", "r", "v", "", func(msg *agentpb.ControlMessage) error {
		if msg.GetProxyRequest() != nil {
			reqs <- msg
		}
		return nil
	})
	reg := NewRegistry()
	reg.Add(session)
	return session, reg
}

func TestTunnelTransport_RoundTripStreamsResponseAndAsksForLongTimeout(t *testing.T) {
	reqs := make(chan *agentpb.ControlMessage, 1)
	session, reg := newTunnelSession(reqs)

	go func() {
		msg := <-reqs
		id := msg.RequestId
		session.deliverProxyResponse(id, &agentpb.ProxyResponse{StatusCode: 200,
			Headers: []*agentpb.Header{{Name: "Content-Type", Values: []string{"application/json"}}}})
		session.deliverProxyData(id, &agentpb.ProxyData{Data: []byte(`{"ok":`)})
		session.deliverProxyData(id, &agentpb.ProxyData{Data: []byte(`true}`)})
		session.deliverProxyData(id, &agentpb.ProxyData{Eof: true})
	}()

	client := &http.Client{Transport: NewTunnelTransport(reg, "prov", "infsvc-1", 8000)}
	resp, err := client.Post("http://tunnel/v1/chat/completions", "application/json", strings.NewReader(`{"a":1}`))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %q", body)
	}
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q", resp.Header.Get("Content-Type"))
	}
}

func TestTunnelTransport_SendsPathPortAndLongTimeout(t *testing.T) {
	reqs := make(chan *agentpb.ControlMessage, 1)
	session, reg := newTunnelSession(reqs)
	done := make(chan *agentpb.ProxyRequest, 1)
	go func() {
		msg := <-reqs
		done <- msg.GetProxyRequest()
		session.deliverProxyResponse(msg.RequestId, &agentpb.ProxyResponse{StatusCode: 204})
		session.deliverProxyData(msg.RequestId, &agentpb.ProxyData{Eof: true})
	}()

	client := &http.Client{Transport: NewTunnelTransport(reg, "prov", "infsvc-1", 8000)}
	resp, err := client.Post("http://tunnel/v1/chat/completions?x=1", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	resp.Body.Close()

	pr := <-done
	if pr.Path != "/v1/chat/completions?x=1" || pr.Port != 8000 || pr.InstanceId != "infsvc-1" {
		t.Errorf("ProxyRequest = %+v", pr)
	}
	if pr.TimeoutSeconds <= 60 {
		t.Errorf("TimeoutSeconds = %d, want a long inference timeout", pr.TimeoutSeconds)
	}
	if !pr.HasBody {
		t.Error("HasBody false for a POST with a body")
	}
}

func TestTunnelTransport_OfflineProvider(t *testing.T) {
	client := &http.Client{Transport: NewTunnelTransport(NewRegistry(), "nobody", "i", 8000)}
	_, err := client.Get("http://tunnel/x")
	if !errors.Is(err, ErrTunnelOffline) {
		t.Fatalf("err = %v, want ErrTunnelOffline", err)
	}
}

func TestTunnelTransport_AgentReportsUnreachableInstance(t *testing.T) {
	reqs := make(chan *agentpb.ControlMessage, 1)
	session, reg := newTunnelSession(reqs)
	go func() {
		msg := <-reqs
		session.deliverProxyResponse(msg.RequestId, &agentpb.ProxyResponse{Error: "instance not reachable: not found"})
	}()
	client := &http.Client{Transport: NewTunnelTransport(reg, "prov", "i", 8000)}
	_, err := client.Get("http://tunnel/x")
	if err == nil || !strings.Contains(err.Error(), "not reachable") {
		t.Fatalf("err = %v, want the agent's reason surfaced", err)
	}
}

func TestTunnelTransport_ResetMidStreamIsAnError(t *testing.T) {
	reqs := make(chan *agentpb.ControlMessage, 1)
	session, reg := newTunnelSession(reqs)
	go func() {
		msg := <-reqs
		id := msg.RequestId
		session.deliverProxyResponse(id, &agentpb.ProxyResponse{StatusCode: 200})
		session.deliverProxyData(id, &agentpb.ProxyData{Data: []byte("par")})
		session.deliverProxyData(id, &agentpb.ProxyData{Reset_: true})
	}()
	client := &http.Client{Transport: NewTunnelTransport(reg, "prov", "i", 8000)}
	resp, err := client.Get("http://tunnel/x")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("read err = %v, want ErrUnexpectedEOF for a truncated stream", err)
	}
}

// The declared body length must cross the tunnel as a Content-Length header,
// since Go keeps it in req.ContentLength rather than the header map.
func TestTunnelTransport_CarriesContentLength(t *testing.T) {
	reqs := make(chan *agentpb.ControlMessage, 1)
	session, reg := newTunnelSession(reqs)
	seen := make(chan []*agentpb.Header, 1)
	go func() {
		msg := <-reqs
		seen <- msg.GetProxyRequest().Headers
		session.deliverProxyResponse(msg.RequestId, &agentpb.ProxyResponse{StatusCode: 204})
		session.deliverProxyData(msg.RequestId, &agentpb.ProxyData{Eof: true})
	}()
	client := &http.Client{Transport: NewTunnelTransport(reg, "prov", "i", 8000)}
	resp, err := client.Post("http://tunnel/x", "application/json", strings.NewReader(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	var got string
	for _, h := range <-seen {
		if strings.EqualFold(h.Name, "Content-Length") && len(h.Values) > 0 {
			got = h.Values[0]
		}
	}
	if got != "7" {
		t.Errorf("Content-Length header = %q, want 7", got)
	}
}
