// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
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
