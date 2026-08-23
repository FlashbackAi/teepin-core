// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/FlashbackAi/teepin-core/pkg/cluster"
)

// EventTicket is a single-use, short-lived credential minted by the REST
// API (which knows the caller's identity via normal auth) and redeemed by
// the WebSocket attach endpoint (which cannot: a browser cannot set
// custom headers on a WS handshake). Mirrors cluster.ExecTicket's shape
// and reasoning exactly, duplicated here rather than shared — this is a
// read-only relay for a different resource (a session's agent pod, not
// an arbitrary instance's shell), and keeping it self-contained in
// pkg/kumbha means it can evolve independently of exec's bidirectional
// protocol without either one constraining the other.
type EventTicket struct {
	ID              string
	SessionID       uuid.UUID
	ProjectID       uuid.UUID
	AccountID       uuid.UUID
	AgentInstanceID string
	ExpiresAt       time.Time
}

var (
	// ErrEventTicketNotFound covers "never existed", "expired", and
	// "already used" identically — presented over an unauthenticated WS
	// handshake, so distinguishing cases would tell an attacker more than
	// they should learn (same reasoning as cluster.ErrTicketNotFound).
	ErrEventTicketNotFound  = errors.New("event ticket not found, expired, or already used")
	ErrEventTicketStoreFull = errors.New("too many outstanding event tickets")
)

const (
	eventTicketTTL          = 30 * time.Second
	eventTicketMax          = 10_000
	eventTicketReapInterval = 30 * time.Second
)

// EventTicketStore issues and redeems EventTickets. In-memory, under the
// same desired_count=1 constraint the exec TicketStore and agent Registry
// already live with — see their own doc comments.
type EventTicketStore struct {
	mu      sync.Mutex
	tickets map[string]EventTicket // keyed by secret, never by id
}

func NewEventTicketStore() *EventTicketStore {
	return &EventTicketStore{tickets: make(map[string]EventTicket)}
}

func (s *EventTicketStore) Issue(t EventTicket) (id, secret string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.tickets) >= eventTicketMax {
		return "", "", ErrEventTicketStoreFull
	}

	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return "", "", err
	}
	secret = base64.RawURLEncoding.EncodeToString(secretBytes)

	t.ID = uuid.New().String()
	t.ExpiresAt = time.Now().Add(eventTicketTTL)
	s.tickets[secret] = t

	return t.ID, secret, nil
}

func (s *EventTicketStore) Redeem(id, secret string) (EventTicket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tickets[secret]
	if !ok {
		return EventTicket{}, ErrEventTicketNotFound
	}
	delete(s.tickets, secret) // consumed regardless of what the checks below find

	if t.ID != id || time.Now().After(t.ExpiresAt) {
		return EventTicket{}, ErrEventTicketNotFound
	}
	return t, nil
}

func (s *EventTicketStore) Reap(ctx context.Context) {
	ticker := time.NewTicker(eventTicketReapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			now := time.Now()
			for secret, t := range s.tickets {
				if now.After(t.ExpiresAt) {
					delete(s.tickets, secret)
				}
			}
			s.mu.Unlock()
		}
	}
}

// eventFieldAllowlist is the ENTIRE set of fields that can ever reach the
// browser from an agent pod's stdout line. This is the structural
// enforcement point for "hide the model" (KUMBHA-DESIGN.md): run.py never
// writes a model/provider field to begin with, and even if it someday did
// by mistake, this allowlist strips it before the browser ever sees a
// byte — re-encoding from named fields, not a denylist that would need to
// be remembered to update.
var eventFieldAllowlist = []string{"type", "tool", "summary", "ts"}

// sanitizeEventLine parses one JSON line from the agent pod's stdout and
// re-encodes only the allowlisted fields. Returns ok=false for a blank or
// unparseable line (a partial write mid-flush, stray output from
// something other than the wrapper's own emit() calls) — dropped rather
// than forwarded, since forwarding malformed data to the console is worse
// than a gap in the activity feed.
func sanitizeEventLine(line []byte) (json.RawMessage, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil, false
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, false
	}
	out := make(map[string]json.RawMessage, len(eventFieldAllowlist))
	for _, key := range eventFieldAllowlist {
		if v, ok := raw[key]; ok {
			out[key] = v
		}
	}
	if _, ok := out["type"]; !ok {
		return nil, false // not one of our own emitted lines
	}
	sanitized, err := json.Marshal(out)
	if err != nil {
		return nil, false
	}
	return sanitized, true
}

// logLineWriter is the io.Writer cluster.Client.StreamLogs writes the
// agent pod's raw stdout into. It buffers across Write calls (stdout
// arrives in arbitrary chunks, not aligned to line boundaries), splits on
// '\n', sanitizes each complete line, and pushes it onto events for the
// WS handler's single writer goroutine to send — this type never touches
// the WebSocket connection itself, which is what keeps gorilla's
// one-writer-at-a-time contract intact alongside the ping ticker (see
// EventsHandler.ServeSession).
type logLineWriter struct {
	buf    []byte
	events chan<- json.RawMessage
}

func (w *logLineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := w.buf[:i]
		w.buf = w.buf[i+1:]
		if sanitized, ok := sanitizeEventLine(line); ok {
			select {
			case w.events <- sanitized:
			default:
				// The consumer (WS write loop) is behind — dropping here
				// rather than blocking is deliberate: StreamLogs must keep
				// draining the pod's log stream regardless of how fast the
				// browser is reading, the same way a slow web client
				// dropping frames beats stalling the whole pipeline.
			}
		}
	}
	return len(p), nil
}

const (
	eventsPingInterval = 30 * time.Second
	eventsWriteWait    = 10 * time.Second
	eventsChannelSize  = 256
)

var (
	wsCloseAuthFailed = 4401
	wsCloseNotFound   = 4404
)

// EventsHandler serves the read-only event-relay WebSocket — the
// console's live activity feed. Unlike interactive exec, there is no
// bidirectional protocol here: it tails cluster.Client.StreamLogs on the
// session's agent pod and forwards sanitized JSON lines, reusing the
// exact log pipeline already built for customer instances rather than a
// new pod-to-controlplane channel (KUMBHA-DESIGN.md's Topology decision).
type EventsHandler struct {
	cluster  cluster.Client
	tickets  *EventTicketStore
	upgrader websocket.Upgrader
}

func NewEventsHandler(client cluster.Client, tickets *EventTicketStore, checkOrigin func(*http.Request) bool) *EventsHandler {
	return &EventsHandler{
		cluster: client,
		tickets: tickets,
		upgrader: websocket.Upgrader{
			CheckOrigin:       checkOrigin,
			EnableCompression: false,
		},
	}
}

// wsAuthFrame is the first (and only) client->server frame this endpoint
// accepts — same ticket-presentation shape as exec's, for a consistent
// client-side pattern across both WS endpoints.
type wsAuthFrame struct {
	Type   string `json:"type"`
	ID     string `json:"id,omitempty"`
	Secret string `json:"secret,omitempty"`
}

type wsServerFrame struct {
	Type    string `json:"type"` // error | closed
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// ServeSession upgrades to a WebSocket and streams one session's agent
// activity to it. sessionID is the bare session ID — the caller (routing
// in cmd/api-server) has already resolved it from the URL path.
func (h *EventsHandler) ServeSession(w http.ResponseWriter, r *http.Request, sessionID string) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // the upgrader already wrote an HTTP error response
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, authMsg, err := conn.ReadMessage()
	if err != nil {
		closeWithError(conn, wsCloseAuthFailed, "expired", "no ticket presented in time")
		return
	}
	var auth wsAuthFrame
	if jsonErr := json.Unmarshal(authMsg, &auth); jsonErr != nil || auth.Type != "auth" {
		closeWithError(conn, wsCloseAuthFailed, "expired", "malformed auth frame")
		return
	}
	ticket, err := h.tickets.Redeem(auth.ID, auth.Secret)
	if err != nil || ticket.SessionID.String() != sessionID {
		closeWithError(conn, wsCloseAuthFailed, "expired", "the session ticket is invalid, expired, or already used")
		return
	}
	if ticket.AgentInstanceID == "" {
		closeWithError(conn, wsCloseNotFound, "not_found", "this session has no agent running yet")
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Reader goroutine — the ONLY goroutine calling conn.ReadMessage,
	// satisfying gorilla's one-reader-at-a-time contract. This is a
	// read-only relay, so any client frame is ignored; its only job is
	// detecting the client going away and answering pings, which is why
	// it exists at all.
	go func() {
		defer cancel()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	events := make(chan json.RawMessage, eventsChannelSize)
	streamErr := make(chan error, 1)
	go func() {
		scope := cluster.ProjectScope(ticket.ProjectID.String())
		err := h.cluster.StreamLogs(ctx, scope, ticket.AgentInstanceID,
			cluster.LogOptions{Follow: true}, &logLineWriter{events: events})
		streamErr <- err
		close(events)
	}()

	pingTicker := time.NewTicker(eventsPingInterval)
	defer pingTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-pingTicker.C:
			_ = conn.SetWriteDeadline(time.Now().Add(eventsWriteWait))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}

		case ev, open := <-events:
			if !open {
				// StreamLogs returned — the pod exited or the stream ended.
				// A nil error here is the normal "agent finished" case, not
				// a failure, so it gets a plain "closed" frame rather than
				// "error".
				if err := <-streamErr; err != nil {
					closeWithError(conn, wsCloseNotFound, "stream_ended", err.Error())
					return
				}
				_ = conn.SetWriteDeadline(time.Now().Add(eventsWriteWait))
				_ = conn.WriteJSON(wsServerFrame{Type: "closed"})
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(eventsWriteWait))
			if err := conn.WriteMessage(websocket.TextMessage, ev); err != nil {
				return
			}
		}
	}
}

func closeWithError(conn *websocket.Conn, code int, jsonCode, message string) {
	_ = conn.SetWriteDeadline(time.Now().Add(eventsWriteWait))
	_ = conn.WriteJSON(wsServerFrame{Type: "error", Code: jsonCode, Message: message})
	_ = conn.SetWriteDeadline(time.Now().Add(eventsWriteWait))
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, message), time.Now().Add(eventsWriteWait))
}
