// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package inference

import (
	"encoding/json"
	"errors"
	"testing"
)

func rawMessages(strs ...string) []json.RawMessage {
	out := make([]json.RawMessage, len(strs))
	for i, s := range strs {
		out[i] = json.RawMessage(s)
	}
	return out
}

func TestFitsContext_UnconfiguredWindowNeverRejects(t *testing.T) {
	req := Request{Messages: rawMessages(`{"role":"user","content":"hi"}`), MaxTokens: 1_000_000}
	if err := FitsContext(req, Capabilities{ContextWindow: 0}); err != nil {
		t.Errorf("unconfigured window rejected a request: %v", err)
	}
}

func TestFitsContext_WithinWindowPasses(t *testing.T) {
	req := Request{
		Messages:  rawMessages(`{"role":"user","content":"hello there"}`),
		MaxTokens: 100,
	}
	if err := FitsContext(req, Capabilities{ContextWindow: 8192}); err != nil {
		t.Errorf("small request rejected against an 8k window: %v", err)
	}
}

func TestFitsContext_OverWindowRejectsBeforeDispatch(t *testing.T) {
	big := make([]byte, 100_000) // ~25k estimated tokens
	for i := range big {
		big[i] = 'a'
	}
	req := Request{
		Messages:  rawMessages(`{"role":"user","content":"` + string(big) + `"}`),
		MaxTokens: 4096,
	}
	err := FitsContext(req, Capabilities{ContextWindow: 4096})
	if !errors.Is(err, ErrContextTooLarge) {
		t.Errorf("got %v, want ErrContextTooLarge", err)
	}
}

func TestFitsContext_ReservesRoomForMaxTokens(t *testing.T) {
	// A prompt that alone fits, but not alongside the requested completion
	// length, must still be rejected — the window is shared between input
	// and output, and the fit check exists specifically to catch this
	// before a real request burns tokens discovering it.
	msg := make([]byte, 3000) // ~750 estimated tokens
	for i := range msg {
		msg[i] = 'a'
	}
	req := Request{
		Messages:  rawMessages(`{"role":"user","content":"` + string(msg) + `"}`),
		MaxTokens: 4000,
	}
	err := FitsContext(req, Capabilities{ContextWindow: 4096})
	if !errors.Is(err, ErrContextTooLarge) {
		t.Errorf("got %v, want ErrContextTooLarge (prompt + max_tokens exceeds window)", err)
	}
}

func TestEstimateTokens_ScalesWithContent(t *testing.T) {
	small := Request{Messages: rawMessages(`{"role":"user","content":"hi"}`)}
	large := Request{Messages: rawMessages(`{"role":"user","content":"` + string(make([]byte, 10_000)) + `"}`)}
	if EstimateTokens(large) <= EstimateTokens(small) {
		t.Error("a 10KB message did not estimate to more tokens than a 2-byte one")
	}
}

func TestEstimateTokens_NeverNegative(t *testing.T) {
	if got := EstimateTokens(Request{}); got < 0 {
		t.Errorf("EstimateTokens on an empty request = %d, want >= 0", got)
	}
}
