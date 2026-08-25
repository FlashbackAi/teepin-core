// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/cluster"
)

func TestSanitizeEventLine_PassesThroughAllowlistedFields(t *testing.T) {
	sanitized, ok := sanitizeEventLine([]byte(`{"type":"action","tool":"TerminalTool","summary":"Running tests","ts":1234.5}`))
	if !ok {
		t.Fatal("expected a well-formed action line to sanitize successfully")
	}
	var got map[string]any
	if err := json.Unmarshal(sanitized, &got); err != nil {
		t.Fatalf("sanitized output is not valid JSON: %v", err)
	}
	for _, field := range []string{"type", "tool", "summary", "ts"} {
		if _, ok := got[field]; !ok {
			t.Errorf("allowlisted field %q was dropped", field)
		}
	}
}

// The single most important test in this file: a field NOT on the
// allowlist — standing in for a hypothetical future "model" or
// "provider" key — must never survive sanitisation, even if it somehow
// appeared in the agent's own output. This is the structural enforcement
// of "hide the model" (KUMBHA-DESIGN.md), verified at the one point that
// actually reaches the browser.
func TestSanitizeEventLine_StripsUnknownFieldsIncludingHypotheticalModelField(t *testing.T) {
	sanitized, ok := sanitizeEventLine([]byte(`{"type":"action","tool":"TerminalTool","summary":"hi","ts":1,"model":"qwen3-coder-30b","provider":"vllm","extra":"anything"}`))
	if !ok {
		t.Fatal("expected sanitisation to succeed")
	}
	var got map[string]any
	json.Unmarshal(sanitized, &got)
	for _, forbidden := range []string{"model", "provider", "extra"} {
		if _, present := got[forbidden]; present {
			t.Errorf("field %q survived sanitisation — the allowlist is not being enforced", forbidden)
		}
	}
	if len(got) != 4 {
		t.Errorf("got %d fields, want exactly the 4 allowlisted ones: %+v", len(got), got)
	}
}

func TestSanitizeEventLine_RejectsBlankLines(t *testing.T) {
	if _, ok := sanitizeEventLine([]byte("   ")); ok {
		t.Error("a blank line should not sanitize successfully")
	}
	if _, ok := sanitizeEventLine([]byte("")); ok {
		t.Error("an empty line should not sanitize successfully")
	}
}

func TestSanitizeEventLine_RejectsMalformedJSON(t *testing.T) {
	if _, ok := sanitizeEventLine([]byte("not json at all")); ok {
		t.Error("malformed JSON should not sanitize successfully")
	}
}

func TestSanitizeEventLine_RejectsLinesWithNoTypeField(t *testing.T) {
	// A stray line of output that happens to be valid JSON but isn't one
	// of run.py's own emit() calls (e.g. a library logging a JSON blob to
	// stdout by accident) must not be forwarded as if it were an event.
	if _, ok := sanitizeEventLine([]byte(`{"tool":"TerminalTool","summary":"no type field"}`)); ok {
		t.Error("a line with no type field should not sanitize successfully")
	}
}

func TestLogLineWriter_SplitsAcrossMultipleWriteCalls(t *testing.T) {
	events := make(chan json.RawMessage, 10)
	w := &logLineWriter{events: events}

	// A line split across two Write calls, as real stdout streaming would
	// deliver it — the buffering must reassemble it correctly.
	w.Write([]byte(`{"type":"action","summary":"hel`))
	w.Write([]byte("lo\"}\n"))

	select {
	case ev := <-events:
		var got map[string]any
		json.Unmarshal(ev, &got)
		if got["summary"] != "hello" {
			t.Errorf("summary = %v, want %q — the split write was not reassembled", got["summary"], "hello")
		}
	default:
		t.Fatal("no event was produced from the split write")
	}
}

func TestLogLineWriter_HandlesMultipleLinesInOneWrite(t *testing.T) {
	events := make(chan json.RawMessage, 10)
	w := &logLineWriter{events: events}

	w.Write([]byte("{\"type\":\"action\",\"summary\":\"one\"}\n{\"type\":\"action\",\"summary\":\"two\"}\n"))

	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
}

func TestLogLineWriter_DropsRatherThanBlocksWhenChannelFull(t *testing.T) {
	events := make(chan json.RawMessage, 1)
	w := &logLineWriter{events: events}

	// Fill the channel, then write a second line — Write must return
	// promptly (not block), which is the whole point of the select/default.
	w.Write([]byte("{\"type\":\"action\",\"summary\":\"first\"}\n"))
	done := make(chan struct{})
	go func() {
		w.Write([]byte("{\"type\":\"action\",\"summary\":\"second\"}\n"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Write blocked instead of dropping when the events channel was full")
	}
}

func TestEventTicketStore_IssueThenRedeemSucceedsExactlyOnce(t *testing.T) {
	store := NewEventTicketStore()
	sessionID, projectID, accountID := uuid.New(), uuid.New(), uuid.New()

	id, secret, err := store.Issue(EventTicket{
		SessionID: sessionID, ProjectID: projectID, AccountID: accountID, AgentInstanceID: "kumbha-agent-abc",
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	ticket, err := store.Redeem(id, secret)
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if ticket.SessionID != sessionID || ticket.AgentInstanceID != "kumbha-agent-abc" {
		t.Errorf("redeemed ticket = %+v, want it to match what was issued", ticket)
	}

	if _, err := store.Redeem(id, secret); !errors.Is(err, ErrEventTicketNotFound) {
		t.Errorf("second redeem: got %v, want ErrEventTicketNotFound (single-use)", err)
	}
}

func TestEventTicketStore_RedeemRejectsWrongSecret(t *testing.T) {
	store := NewEventTicketStore()
	id, _, err := store.Issue(EventTicket{SessionID: uuid.New()})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := store.Redeem(id, "wrong-secret"); !errors.Is(err, ErrEventTicketNotFound) {
		t.Errorf("got %v, want ErrEventTicketNotFound for a wrong secret", err)
	}
}

func TestEventTicketStore_RedeemRejectsExpiredTicket(t *testing.T) {
	store := NewEventTicketStore()
	id, secret, err := store.Issue(EventTicket{SessionID: uuid.New()})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	// Reach into the store to simulate time passing, rather than sleeping
	// for real in a test.
	store.mu.Lock()
	t2 := store.tickets[secret]
	t2.ExpiresAt = time.Now().Add(-time.Second)
	store.tickets[secret] = t2
	store.mu.Unlock()

	if _, err := store.Redeem(id, secret); !errors.Is(err, ErrEventTicketNotFound) {
		t.Errorf("got %v, want ErrEventTicketNotFound for an expired ticket", err)
	}
}

// scriptedStreamCall is one entry in streamRetryCluster's scripted
// StreamLogs behaviour: optionally write some bytes (simulating real
// output), then return err.
type scriptedStreamCall struct {
	write []byte
	err   error
}

// streamRetryCluster is a minimal cluster.Client for
// streamLogsWithRetry tests: StreamLogs plays back a scripted sequence of
// calls. GetInstanceStatus reports instanceStat/statusErr until the
// script is exhausted (streamCalls >= len(calls)), then reports the pod
// as "terminated" — this is what lets a test script naturally terminate
// the retry loop after its last scripted call instead of retrying
// forever, without needing extra unused script entries.
type streamRetryCluster struct {
	calls        []scriptedStreamCall
	streamCalls  int
	instanceStat *cluster.InstanceStatus
	statusErr    error
}

func (f *streamRetryCluster) StreamLogs(_ context.Context, _ cluster.Scope, _ string, _ cluster.LogOptions, w io.Writer) error {
	i := f.streamCalls
	f.streamCalls++
	if i >= len(f.calls) {
		// Ran past the script — block-free "never returns real data,
		// never errors" would hang the test, so fail loudly instead.
		return errors.New("streamRetryCluster: StreamLogs called more times than scripted")
	}
	call := f.calls[i]
	if len(call.write) > 0 {
		w.Write(call.write)
	}
	return call.err
}

func (f *streamRetryCluster) GetInstanceStatus(context.Context, cluster.Scope, string) (*cluster.InstanceStatus, error) {
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	if f.streamCalls >= len(f.calls) {
		return &cluster.InstanceStatus{Status: "terminated"}, nil
	}
	return f.instanceStat, nil
}

func (f *streamRetryCluster) CreateInstance(context.Context, cluster.InstanceSpec) (*cluster.InstanceResult, error) {
	return nil, errors.New("not implemented")
}
func (f *streamRetryCluster) DeleteInstance(context.Context, cluster.Scope, string) error {
	return errors.New("not implemented")
}
func (f *streamRetryCluster) ListInstanceStatuses(context.Context, cluster.Scope) ([]cluster.InstanceStatus, error) {
	return nil, nil
}
func (f *streamRetryCluster) Inventory(context.Context) ([]cluster.NodeInventory, error) {
	return nil, nil
}
func (f *streamRetryCluster) Healthy(context.Context) bool { return true }
func (f *streamRetryCluster) ResolveInstanceAddress(context.Context, string, int32) (string, error) {
	return "", cluster.ErrNotFound
}

func newTestEventsHandler(fc *streamRetryCluster) *EventsHandler {
	h := NewEventsHandler(fc, NewEventTicketStore(), nil)
	// Fast retry interval so these tests run in milliseconds, not minutes.
	h.streamRetryInterval = time.Millisecond
	h.streamRetryBudget = 50 * time.Millisecond
	return h
}

// The cold-start case, unchanged by the fix: StreamLogs fails outright
// (no bytes ever written) every time until the pod becomes reachable —
// must keep retrying rather than giving up on the first failure.
func TestStreamLogsWithRetry_ColdStartRetriesUntilPodReachable(t *testing.T) {
	fc := &streamRetryCluster{
		calls: []scriptedStreamCall{
			{err: errors.New("container is waiting to start")},
			{err: errors.New("container is waiting to start")},
			{write: []byte("{\"type\":\"action\",\"summary\":\"hi\"}\n"), err: nil},
		},
		instanceStat: &cluster.InstanceStatus{Status: "running"},
	}
	h := newTestEventsHandler(fc)
	events := make(chan json.RawMessage, 10)

	err := h.streamLogsWithRetry(context.Background(), cluster.Scope{}, "kumbha-agent-x", events)
	if err != nil {
		t.Fatalf("streamLogsWithRetry: %v", err)
	}
	if fc.streamCalls != 3 {
		t.Errorf("StreamLogs called %d times, want 3 (2 cold-start failures + 1 success)", fc.streamCalls)
	}
}

// The core of this fix: once real output has been seen, a StreamLogs
// return must NOT be treated as terminal while the pod is still actually
// running — it must retry, not give up (found live 2026-08-24: a WS
// closed ~9s into a session while the agent kept working for minutes
// afterward, and nothing ever reconnected the event relay).
func TestStreamLogsWithRetry_RetriesAfterDataSeenIfPodStillRunning(t *testing.T) {
	fc := &streamRetryCluster{
		calls: []scriptedStreamCall{
			{write: []byte("{\"type\":\"action\",\"summary\":\"first\"}\n"), err: nil}, // stream drops after real data
			{write: []byte("{\"type\":\"action\",\"summary\":\"second\"}\n"), err: nil},
		},
		instanceStat: &cluster.InstanceStatus{Status: "running"},
	}
	h := newTestEventsHandler(fc)
	events := make(chan json.RawMessage, 10)

	err := h.streamLogsWithRetry(context.Background(), cluster.Scope{}, "kumbha-agent-x", events)
	if err != nil {
		t.Fatalf("streamLogsWithRetry: %v", err)
	}
	if fc.streamCalls != 2 {
		t.Errorf("StreamLogs called %d times, want 2 — the first drop should have been retried since the pod was still running", fc.streamCalls)
	}
}

// The other half of the same fix: once the pod is confirmed gone, do NOT
// retry — this is the legitimate "the agent finished" / "it really
// failed" case, and must return promptly so the browser gets its
// terminal "closed"/"error" frame.
func TestStreamLogsWithRetry_StopsWhenPodConfirmedGone(t *testing.T) {
	fc := &streamRetryCluster{
		calls: []scriptedStreamCall{
			{write: []byte("{\"type\":\"action\",\"summary\":\"first\"}\n"), err: nil},
		},
		instanceStat: &cluster.InstanceStatus{Status: "terminated"},
	}
	h := newTestEventsHandler(fc)
	events := make(chan json.RawMessage, 10)

	err := h.streamLogsWithRetry(context.Background(), cluster.Scope{}, "kumbha-agent-x", events)
	if err != nil {
		t.Fatalf("streamLogsWithRetry: %v", err)
	}
	if fc.streamCalls != 1 {
		t.Errorf("StreamLogs called %d times, want 1 — a terminated pod must not be retried", fc.streamCalls)
	}
}

// If the pod's status can no longer even be read (e.g. it was deleted
// outright), that must also be treated as terminal, not retried forever.
func TestStreamLogsWithRetry_StopsWhenStatusUnreadable(t *testing.T) {
	fc := &streamRetryCluster{
		calls: []scriptedStreamCall{
			{write: []byte("{\"type\":\"action\",\"summary\":\"first\"}\n"), err: nil},
		},
		statusErr: cluster.ErrNotFound,
	}
	h := newTestEventsHandler(fc)
	events := make(chan json.RawMessage, 10)

	err := h.streamLogsWithRetry(context.Background(), cluster.Scope{}, "kumbha-agent-x", events)
	if err != nil {
		t.Fatalf("streamLogsWithRetry: %v", err)
	}
	if fc.streamCalls != 1 {
		t.Errorf("StreamLogs called %d times, want 1 — an unreadable status must not be retried", fc.streamCalls)
	}
}
