// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	agentpb "github.com/FlashbackAi/teepin-core/pkg/agentpb"
)

// Limits for control-plane-originated tunnel calls (model inference). These
// are deliberately different from the customer-facing proxy limits above:
// an LLM call can spend minutes in prefill before the first byte and then
// stream for minutes more, where a browser request that takes a minute is
// already broken.
const (
	// TunnelResponseHeaderTimeout bounds the wait for the backend to start
	// answering (prefill of a long prompt on a slow machine).
	TunnelResponseHeaderTimeout = 10 * time.Minute
	// TunnelIdleTimeout resets a streaming response if no chunk arrives
	// for this long once it has started.
	TunnelIdleTimeout = 2 * time.Minute
	// tunnelAgentTimeoutSeconds is the local-call bound requested from the
	// agent; the agent clamps it to its own ceiling.
	tunnelAgentTimeoutSeconds = int32(15 * 60)
)

// ErrTunnelOffline means the provider's agent session is not connected.
var ErrTunnelOffline = errors.New("cluster: provider agent is offline")

// TunnelTransport is an http.RoundTripper that carries each request over one
// provider's agent session to a single instance port, reusing the same
// ProxyRequest/ProxyData protocol as the customer-facing ProxyHandler. It
// lets pkg/inference talk to a model server on a NAT'd machine with an
// ordinary http.Client. The request URL's host is ignored; only its path
// and query are sent.
type TunnelTransport struct {
	registry   *Registry
	providerID string
	instanceID string
	port       int32
}

// NewTunnelTransport binds a transport to one instance on one provider.
func NewTunnelTransport(registry *Registry, providerID, instanceID string, port int32) *TunnelTransport {
	return &TunnelTransport{registry: registry, providerID: providerID, instanceID: instanceID, port: port}
}

// RoundTrip implements http.RoundTripper.
func (t *TunnelTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	session, ok := t.registry.ByProvider(t.providerID)
	if !ok {
		return nil, fmt.Errorf("%w (%s)", ErrTunnelOffline, t.providerID)
	}

	ctx, cancel := context.WithCancel(req.Context())
	requestID := "tunnel-" + t.instanceID + "-" + uuid.New().String()[:8]
	events := session.openProxyStream(requestID)
	cleanup := func() {
		session.closeProxyStream(requestID)
		cancel()
	}

	hasBody := req.Body != nil && req.Body != http.NoBody

	// Go keeps the length in req.ContentLength, not the header map; carry it
	// explicitly so the agent can send a sized (not chunked) request to
	// servers that require Content-Length.
	headers := headersToProto(req.Header)
	if hasBody && req.ContentLength > 0 {
		headers = append(headers, &agentpb.Header{Name: "Content-Length", Values: []string{strconv.FormatInt(req.ContentLength, 10)}})
	}
	if err := session.send(&agentpb.ControlMessage{
		RequestId: requestID,
		Payload: &agentpb.ControlMessage_ProxyRequest{
			ProxyRequest: &agentpb.ProxyRequest{
				InstanceId:     t.instanceID,
				Method:         req.Method,
				Path:           req.URL.RequestURI(),
				Headers:        headers,
				HasBody:        hasBody,
				Port:           t.port,
				TimeoutSeconds: tunnelAgentTimeoutSeconds,
			},
		},
	}); err != nil {
		cleanup()
		return nil, fmt.Errorf("%w (%s): %v", ErrTunnelOffline, t.providerID, err)
	}

	if hasBody {
		go pumpRequestBody(ctx, session, requestID, req.Body)
	} else {
		_ = session.send(&agentpb.ControlMessage{
			RequestId: requestID,
			Payload:   &agentpb.ControlMessage_ProxyData{ProxyData: &agentpb.ProxyData{Eof: true}},
		})
	}

	// Wait for the response head.
	headTimer := time.NewTimer(TunnelResponseHeaderTimeout)
	defer headTimer.Stop()
	for {
		select {
		case <-ctx.Done():
			cleanup()
			return nil, ctx.Err()
		case <-headTimer.C:
			cleanup()
			return nil, fmt.Errorf("tunnel: no response from %s within %s", t.instanceID, TunnelResponseHeaderTimeout)
		case ev, open := <-events:
			if !open {
				cleanup()
				return nil, fmt.Errorf("%w: connection lost before a response", ErrTunnelOffline)
			}
			if ev.response == nil {
				continue // body data cannot legitimately precede the head; ignore
			}
			if ev.response.Error != "" {
				cleanup()
				return nil, fmt.Errorf("tunnel: instance could not be reached: %s", ev.response.Error)
			}
			status := int(ev.response.StatusCode)
			if status == 0 {
				status = http.StatusOK
			}
			header := http.Header{}
			applyProtoHeaders(header, ev.response.Headers)
			return &http.Response{
				Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
				StatusCode: status,
				Proto:      "HTTP/1.1",
				ProtoMajor: 1,
				ProtoMinor: 1,
				Header:     header,
				Body:       &tunnelBody{ctx: ctx, events: events, cleanup: cleanup},
				Request:    req,
			}, nil
		}
	}
}

// tunnelBody streams ProxyData chunks as an io.ReadCloser.
type tunnelBody struct {
	ctx     context.Context
	events  chan proxyEvent
	cleanup func()
	buf     []byte
	done    bool
	err     error
}

func (b *tunnelBody) Read(p []byte) (int, error) {
	for len(b.buf) == 0 {
		if b.err != nil {
			return 0, b.err
		}
		if b.done {
			return 0, io.EOF
		}
		idle := time.NewTimer(TunnelIdleTimeout)
		select {
		case <-b.ctx.Done():
			idle.Stop()
			b.err = b.ctx.Err()
		case <-idle.C:
			b.err = errors.New("tunnel: response stalled")
		case ev, open := <-b.events:
			idle.Stop()
			switch {
			case !open:
				b.err = io.ErrUnexpectedEOF
			case ev.data == nil:
				// a stray head; ignore
			case ev.data.Reset_:
				b.err = io.ErrUnexpectedEOF
			default:
				b.buf = ev.data.Data
				if ev.data.Eof {
					b.done = true
				}
			}
		}
	}
	n := copy(p, b.buf)
	b.buf = b.buf[n:]
	return n, nil
}

func (b *tunnelBody) Close() error {
	b.cleanup()
	return nil
}
