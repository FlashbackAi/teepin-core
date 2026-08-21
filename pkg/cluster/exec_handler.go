// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	agentpb "github.com/FlashbackAi/teepin-core/pkg/agentpb"
)

// Interactive exec limits. Struct fields on ExecHandler below (not
// package consts like the Stage 3 tunnel's) for maxSessionDuration/
// idleTimeout/attachTimeout specifically, so tests can shrink them
// instead of sleeping for real minutes; the rest are fine as consts.
const (
	// execMaxConcurrentPerSession is far lower than the HTTP tunnel's 32:
	// a proxied request holds its slot for at most proxyRequestTimeout
	// (60s), an exec session holds it for minutes to hours.
	execMaxConcurrentPerSession = 4
	// execMaxConcurrentPerProject stops one customer taking every slot on
	// a shared datacenter node. Tracked here (live sessions), not at
	// ticket-issue time in pkg/api — a ticket can be issued and never
	// redeemed, so counting issued tickets would overcount; this counts
	// what is actually attached.
	execMaxConcurrentPerProject = 2

	execDefaultMaxSessionDuration = 60 * time.Minute
	execDefaultIdleTimeout        = 15 * time.Minute
	// execDefaultAttachTimeout is not optional: an agent that predates
	// this feature falls through handleCommand's default case and
	// replies nothing usable for ExecStart — without this deadline the
	// browser would hang until execDefaultIdleTimeout.
	execDefaultAttachTimeout = 10 * time.Second

	execPingInterval = 30 * time.Second
	execPongWait     = 70 * time.Second
	execWriteWait    = 10 * time.Second
	execReadLimit    = 64 * 1024
)

// Private WebSocket close codes (4000-4999 is the reserved-for-use range)
// for every way an exec session can end without a clean exit. The
// browser WebSocket API exposes only a close code + reason string — it
// cannot see an HTTP status, so every failure here is reported this way
// rather than as a pre-upgrade HTTP response.
const (
	wsCloseAuthFailed      = 4401
	wsCloseNotFound        = 4404
	wsCloseNodeOffline     = 4503
	wsCloseNodeUnreachable = 4502
	wsCloseTooMany         = 4429
	wsCloseAttachTimeout   = 4504
	wsCloseIdle            = 4408
	wsCloseNoShell         = 4501
)

// execClientFrame is a text-frame message from the browser/CLI. Binary
// frames (no JSON envelope) carry raw stdin bytes instead, so terminal
// input never pays a base64 encoding cost. The FIRST frame on every
// connection must be type=auth — see ExecHandler.ServeInstance.
type execClientFrame struct {
	Type   string `json:"type"`
	ID     string `json:"id,omitempty"`     // type=auth
	Secret string `json:"secret,omitempty"` // type=auth
	Rows   uint16 `json:"rows,omitempty"`   // type=auth (initial size) or type=resize
	Cols   uint16 `json:"cols,omitempty"`   // type=auth (initial size) or type=resize
}

// execServerFrame is a text-frame message to the browser/CLI. Binary
// frames carry raw stdout bytes.
type execServerFrame struct {
	Type     string `json:"type"` // ready | exit | error
	Code     string `json:"code,omitempty"`
	Message  string `json:"message,omitempty"`
	ExitCode int32  `json:"exit_code,omitempty"`
}

// ExecHandler serves the interactive-exec WebSocket endpoint, mirroring
// ProxyHandler's shape (registry + target + a per-provider concurrency
// cap) but for a long-lived bidirectional session instead of one
// request/response.
type ExecHandler struct {
	registry *Registry
	target   ProxyTarget // reuse as-is; the port it also returns is unused here
	tickets  *TicketStore
	upgrader websocket.Upgrader

	mu              sync.Mutex
	inFlight        map[string]int // per provider ID
	inFlightProject map[string]int // per project ID (ticket.ProjectID.String())

	maxSessionDuration time.Duration
	idleTimeout        time.Duration
	attachTimeout      time.Duration

	// onSessionEnd records the audit-trail row's end state (compute.
	// exec_sessions, migration 022) once per session. A plain callback
	// rather than an interface+adapter pair (like ProxyTarget's) because
	// there is exactly one call site and one implementation — main.go
	// wires it directly to compute.Store.EndExecSession as a closure.
	// Nil is valid (no database / standalone mode): the session simply
	// isn't recorded.
	onSessionEnd func(ticketID, podName string, exitCode *int, closeReason string)
}

// NewExecHandler builds a handler. checkOrigin should reuse the same
// allowlist corsMiddleware parses (cmd/api-server) — defense in depth,
// since the issuing POST is already CORS-protected and needs a JWT.
func NewExecHandler(registry *Registry, target ProxyTarget, tickets *TicketStore, checkOrigin func(*http.Request) bool) *ExecHandler {
	return &ExecHandler{
		registry: registry,
		target:   target,
		tickets:  tickets,
		upgrader: websocket.Upgrader{
			CheckOrigin:       checkOrigin,
			EnableCompression: false,
		},
		inFlight:           make(map[string]int),
		inFlightProject:    make(map[string]int),
		maxSessionDuration: execDefaultMaxSessionDuration,
		idleTimeout:        execDefaultIdleTimeout,
		attachTimeout:      execDefaultAttachTimeout,
	}
}

// WithSessionRecorder enables audit persistence. Returns the same
// *ExecHandler for chaining, matching this codebase's established
// optional-dependency idiom (Server.WithNodePlacer etc.).
func (h *ExecHandler) WithSessionRecorder(f func(ticketID, podName string, exitCode *int, closeReason string)) *ExecHandler {
	h.onSessionEnd = f
	return h
}

// acquire checks and reserves BOTH caps atomically — if the project cap
// is full, the provider slot must not be taken either, and vice versa.
func (h *ExecHandler) acquire(providerID, projectID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.inFlight[providerID] >= execMaxConcurrentPerSession {
		return false
	}
	if h.inFlightProject[projectID] >= execMaxConcurrentPerProject {
		return false
	}
	h.inFlight[providerID]++
	h.inFlightProject[projectID]++
	return true
}

func (h *ExecHandler) release(providerID, projectID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.inFlight[providerID]--
	if h.inFlight[providerID] <= 0 {
		delete(h.inFlight, providerID)
	}
	h.inFlightProject[projectID]--
	if h.inFlightProject[projectID] <= 0 {
		delete(h.inFlightProject, projectID)
	}
}

// ServeInstance upgrades to a WebSocket and bridges it to one interactive
// exec session on instanceID's agent. instanceID is the bare TEEPIN
// instance ID — the caller (routing in cmd/api-server) has already
// resolved it from the URL path.
//
// Every failure is a post-upgrade close code + JSON error frame, never a
// pre-upgrade HTTP status: the browser WebSocket API cannot see the
// latter at all, so there is no point trying to fail before the upgrade
// beyond bounding how long an unauthenticated socket stays open (the 5s
// auth deadline below).
func (h *ExecHandler) ServeInstance(w http.ResponseWriter, r *http.Request, instanceID string) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // the upgrader already wrote an HTTP error response
	}
	defer conn.Close()
	conn.SetReadLimit(execReadLimit)

	// --- Auth: the ticket is the FIRST frame, not a query param (see the
	// Stage 3 plan doc for interactive exec — this keeps it out of ALB
	// access logs and any proxy on the path). ---
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, authMsg, err := conn.ReadMessage()
	if err != nil {
		execCloseWithError(conn, wsCloseAuthFailed, "expired", "no ticket presented in time")
		return
	}
	var auth execClientFrame
	if jsonErr := json.Unmarshal(authMsg, &auth); jsonErr != nil || auth.Type != "auth" {
		execCloseWithError(conn, wsCloseAuthFailed, "expired", "malformed auth frame")
		return
	}
	ticket, err := h.tickets.Redeem(auth.ID, auth.Secret)
	if err != nil || ticket.InstanceID != instanceID {
		execCloseWithError(conn, wsCloseAuthFailed, "expired", "the session ticket is invalid, expired, or already used")
		return
	}

	// Audit state, updated as the session progresses and recorded exactly
	// once when this function returns — every exit path below (offline,
	// concurrency-capped, agent-rejected, clean exit, abandoned) is
	// covered by this one deferred call rather than needing its own.
	var (
		recordedPodName     string
		recordedExitCode    *int
		recordedCloseReason string
	)
	if h.onSessionEnd != nil {
		defer func() {
			h.onSessionEnd(ticket.ID, recordedPodName, recordedExitCode, recordedCloseReason)
		}()
	}
	// closeWithError/closeClean wrap the free functions of the same
	// shape below, additionally recording the close reason (and, for a
	// clean exit, the exit code) into the audit row set up above. Every
	// termination point from here on uses these instead of calling
	// execCloseWithError/execCloseClean directly.
	closeWithError := func(code int, jsonCode, message string) {
		recordedCloseReason = jsonCode
		execCloseWithError(conn, code, jsonCode, message)
	}
	closeClean := func(exitCode int32) {
		recordedCloseReason = "exit"
		ec := int(exitCode)
		recordedExitCode = &ec
		execCloseClean(conn, exitCode)
	}

	providerID, _, ok := h.target.ResolveProvider(r.Context(), instanceID)
	if !ok {
		closeWithError(wsCloseNotFound, "not_found", "instance not found")
		return
	}
	session, ok := h.registry.ByProvider(providerID)
	if !ok {
		closeWithError(wsCloseNodeOffline, "node_offline", "the node hosting this instance is offline")
		return
	}
	projectKey := ticket.ProjectID.String()
	if !h.acquire(providerID, projectKey) {
		closeWithError(wsCloseTooMany, "too_many", "too many terminal sessions are already running for this node or project")
		return
	}
	defer h.release(providerID, projectKey)

	ctx, cancel := context.WithTimeout(r.Context(), h.maxSessionDuration)
	defer cancel()

	requestID := "exec-" + instanceID + "-" + uuid.New().String()[:8]
	events := session.openExecStream(requestID)
	defer session.closeExecStream(requestID)

	rows, cols := auth.Rows, auth.Cols
	if rows == 0 || cols == 0 {
		rows, cols = 24, 80 // sane default if the client omitted its size
	}
	if err := session.send(&agentpb.ControlMessage{
		RequestId: requestID,
		Payload: &agentpb.ControlMessage_ExecStart{ExecStart: &agentpb.ExecStart{
			InstanceId: instanceID,
			Command:    ticket.Command,
			Tty:        true, // v1 is TTY-only
			Rows:       uint32(rows),
			Cols:       uint32(cols),
			Container:  ticket.Container,
		}},
	}); err != nil {
		closeWithError(wsCloseNodeOffline, "node_offline", "the node hosting this instance is offline")
		return
	}

	// Reader goroutine — the ONLY goroutine calling conn.ReadMessage,
	// satisfying gorilla's one-reader-at-a-time contract. It never writes
	// to the socket (the main loop below owns every write); it signals
	// termination purely by cancelling ctx.
	_ = conn.SetReadDeadline(time.Now().Add(execPongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(execPongWait))
	})
	go func() {
		defer cancel()
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			switch mt {
			case websocket.BinaryMessage:
				if len(data) == 0 {
					continue
				}
				_ = session.send(&agentpb.ControlMessage{
					RequestId: requestID,
					Payload: &agentpb.ControlMessage_ExecInput{ExecInput: &agentpb.ExecInput{
						Stdin: data,
					}},
				})
			case websocket.TextMessage:
				var frame execClientFrame
				if json.Unmarshal(data, &frame) != nil {
					continue
				}
				if frame.Type == "resize" && frame.Rows > 0 && frame.Cols > 0 {
					_ = session.send(&agentpb.ControlMessage{
						RequestId: requestID,
						Payload: &agentpb.ControlMessage_ExecInput{ExecInput: &agentpb.ExecInput{
							Rows: uint32(frame.Rows),
							Cols: uint32(frame.Cols),
						}},
					})
				}
			}
		}
	}()
	// Best-effort: tell the agent to stop the shell the moment this
	// function returns, by whatever path (clean exit, error, or the
	// reader goroutine noticing the socket died). If the session is
	// already over this lands on a closed stream and does nothing.
	defer func() {
		_ = session.send(&agentpb.ControlMessage{
			RequestId: requestID,
			Payload: &agentpb.ControlMessage_ExecInput{ExecInput: &agentpb.ExecInput{
				Cancel: true,
			}},
		})
	}()

	pingTicker := time.NewTicker(execPingInterval)
	defer pingTicker.Stop()
	idleTimer := time.NewTimer(h.idleTimeout)
	defer idleTimer.Stop()
	attachTimer := time.NewTimer(h.attachTimeout)
	defer attachTimer.Stop()
	attached := false

	for {
		select {
		case <-ctx.Done():
			// Ambiguous by nature: the reader goroutine cancels ctx on
			// ANY read error, including a client-initiated close (the
			// common case) — but also on the maxSessionDuration cap
			// elapsing. Report it as a plain close rather than guessing
			// at a fabricated exit code either way.
			if attached {
				recordedCloseReason = "closed"
				_ = conn.SetWriteDeadline(time.Now().Add(execWriteWait))
				_ = conn.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, "session ended"),
					time.Now().Add(execWriteWait))
			} else {
				closeWithError(wsCloseNodeOffline, "node_offline", "session ended")
			}
			return

		case <-attachTimer.C:
			if !attached {
				closeWithError(wsCloseAttachTimeout, "attach_timeout",
					"this node did not respond in time — it may not support terminal sessions yet")
				return
			}

		case <-idleTimer.C:
			closeWithError(wsCloseIdle, "idle", "closed after 15 minutes with no activity")
			return

		case <-pingTicker.C:
			_ = conn.SetWriteDeadline(time.Now().Add(execWriteWait))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}

		case ev, open := <-events:
			if !open {
				if attached {
					closeWithError(wsCloseNodeUnreachable, "node_unreachable",
						"the node hosting this instance became unreachable")
				} else {
					closeWithError(wsCloseNodeOffline, "node_offline",
						"the node hosting this instance is offline")
				}
				return
			}
			idleTimer.Reset(h.idleTimeout)

			switch {
			case ev.open != nil:
				attached = true
				attachTimer.Stop()
				recordedPodName = ev.open.PodName
				_ = conn.SetWriteDeadline(time.Now().Add(execWriteWait))
				_ = conn.WriteJSON(execServerFrame{Type: "ready"})

			case ev.output != nil:
				_ = conn.SetWriteDeadline(time.Now().Add(execWriteWait))
				if err := conn.WriteMessage(websocket.BinaryMessage, ev.output.Data); err != nil {
					return
				}

			case ev.end != nil:
				if ev.end.ErrorCode != agentpb.ErrorCode_ERROR_CODE_UNSPECIFIED {
					code, jsonCode := execEndCloseCode(ev.end.ErrorCode)
					closeWithError(code, jsonCode, ev.end.ErrorMessage)
					return
				}
				closeClean(ev.end.ExitCode)
				return
			}
		}
	}
}

// execEndCloseCode maps an agent-reported ExecEnd failure to a close
// code + JSON reason. EXEC_UNSUPPORTED (no shell in the image) is the
// only one expected in ordinary operation; the rest are a defensive
// fallback for a genuinely unexpected agent-side error.
func execEndCloseCode(ec agentpb.ErrorCode) (code int, jsonCode string) {
	switch ec {
	case agentpb.ErrorCode_ERROR_CODE_EXEC_UNSUPPORTED:
		return wsCloseNoShell, "no_shell"
	case agentpb.ErrorCode_ERROR_CODE_NOT_FOUND:
		return wsCloseNotFound, "not_found"
	default:
		return wsCloseNodeUnreachable, "agent_error"
	}
}

func execCloseWithError(conn *websocket.Conn, code int, jsonCode, message string) {
	_ = conn.SetWriteDeadline(time.Now().Add(execWriteWait))
	_ = conn.WriteJSON(execServerFrame{Type: "error", Code: jsonCode, Message: message})
	_ = conn.SetWriteDeadline(time.Now().Add(execWriteWait))
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, message), time.Now().Add(execWriteWait))
}

func execCloseClean(conn *websocket.Conn, exitCode int32) {
	_ = conn.SetWriteDeadline(time.Now().Add(execWriteWait))
	_ = conn.WriteJSON(execServerFrame{Type: "exit", ExitCode: exitCode})
	_ = conn.SetWriteDeadline(time.Now().Add(execWriteWait))
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1000, "session ended"), time.Now().Add(execWriteWait))
}
