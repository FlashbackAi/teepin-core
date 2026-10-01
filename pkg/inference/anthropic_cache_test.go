// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package inference

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

const cacheTestReply = `{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],
	"model":"claude-opus-5","stop_reason":"end_turn",
	"usage":{"input_tokens":50,"output_tokens":7,"cache_read_input_tokens":16000,"cache_creation_input_tokens":300}}`

// capture runs one completion and returns the request body Anthropic received.
func capture(t *testing.T, req Request) (map[string]any, *Response) {
	t.Helper()
	var got map[string]any
	p, _ := newTestAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(cacheTestReply))
	})
	resp, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	return got, resp
}

// breakpoints lists where the request carries cache_control, in request order.
func breakpoints(body map[string]any) []string {
	var out []string
	has := func(v any) bool { m, ok := v.(map[string]any); return ok && m["cache_control"] != nil }
	for i, v := range asList(body["tools"]) {
		if has(v) {
			out = append(out, "tool"+itoa(i))
		}
	}
	for i, v := range asList(body["system"]) {
		if has(v) {
			out = append(out, "system"+itoa(i))
		}
	}
	for mi, m := range asList(body["messages"]) {
		for bi, b := range asList(m.(map[string]any)["content"]) {
			if has(b) {
				out = append(out, "msg"+itoa(mi)+"."+itoa(bi))
			}
		}
	}
	return out
}

func asList(v any) []any { l, _ := v.([]any); return l }
func itoa(i int) string  { b, _ := json.Marshal(i); return string(b) }

func agentRequest(turns int) Request {
	msgs := []string{`{"role":"system","content":"You are a builder."}`, `{"role":"user","content":"build me an app"}`}
	for i := 0; i < turns; i++ {
		id := "call_" + itoa(i)
		msgs = append(msgs,
			`{"role":"assistant","content":null,"tool_calls":[{"id":"`+id+`","type":"function","function":{"name":"run","arguments":"{}"}}]}`,
			`{"role":"tool","tool_call_id":"`+id+`","content":"done"}`)
	}
	return Request{
		Messages: rawMessages(msgs...),
		Extra: map[string]json.RawMessage{"tools": json.RawMessage(
			`[{"type":"function","function":{"name":"run","parameters":{"type":"object"}}},{"type":"function","function":{"name":"look","parameters":{"type":"object"}}}]`)},
	}
}

// The stable prefixes of an agent's request are marked, never more than the
// four the API allows, so a later call reads them back instead of paying again.
func TestAnthropic_Cache_MarksToolsSystemAndTheConversationTail(t *testing.T) {
	got, _ := capture(t, agentRequest(3))
	marks := breakpoints(got)
	joined := strings.Join(marks, ",")
	if len(marks) > 4 {
		t.Fatalf("%d breakpoints (%s); the API allows 4", len(marks), joined)
	}
	for _, want := range []string{"tool1", "system0"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no breakpoint at %s (have %s)", want, joined)
		}
	}
	// The last tool only: marking every tool would burn the allowance.
	if strings.Contains(joined, "tool0") {
		t.Errorf("breakpoint on the first tool: %s", joined)
	}
	msgs := asList(got["messages"])
	last := len(msgs) - 1
	if !strings.Contains(joined, "msg"+itoa(last)+".") {
		t.Errorf("the final message is not marked (have %s)", joined)
	}
}

func TestAnthropic_Cache_ShortConversationsAndBareRequestsStillValid(t *testing.T) {
	for name, req := range map[string]Request{
		"no tools, no system": {Messages: rawMessages(`{"role":"user","content":"hi"}`)},
		"tools only":          {Messages: rawMessages(`{"role":"user","content":"hi"}`), Extra: agentRequest(0).Extra},
	} {
		got, _ := capture(t, req)
		if n := len(breakpoints(got)); n > 4 {
			t.Errorf("%s: %d breakpoints", name, n)
		}
	}
}

// Marking for the cache must not change what the model is asked.
func TestAnthropic_Cache_DoesNotChangeTheRequestContent(t *testing.T) {
	got, _ := capture(t, agentRequest(2))
	text, _ := json.Marshal(got)
	for _, want := range []string{"build me an app", "You are a builder.", `"name":"look"`, "call_1"} {
		if !strings.Contains(string(text), want) {
			t.Errorf("request lost %q", want)
		}
	}
}

// With caching Anthropic's input_tokens is only the freshly processed part. The
// customer is billed on all input, so the cached parts are added back; the
// split is kept for the hit-rate picture.
func TestAnthropic_Cache_UsageCountsAllInputAndKeepsTheSplit(t *testing.T) {
	_, resp := capture(t, agentRequest(1))
	if resp.Usage.InputTokens != 50+16000+300 {
		t.Errorf("InputTokens = %d, want 16350 (fresh + cache read + cache write)", resp.Usage.InputTokens)
	}
	if resp.Usage.CachedInputTokens != 16000 || resp.Usage.CacheWriteTokens != 300 || resp.Usage.OutputTokens != 7 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	var body struct {
		Usage struct {
			PromptTokens        int `json:"prompt_tokens"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Usage.PromptTokens != 16350 || body.Usage.PromptTokensDetails.CachedTokens != 16000 {
		t.Errorf("response usage = %+v; a harness reads cache hits from prompt_tokens_details", body.Usage)
	}
}

// Tool definitions count toward the window: leaving them out let requests
// through that the backend then refused.
func TestEstimateTokens_CountsToolDefinitions(t *testing.T) {
	bare := Request{Messages: rawMessages(`{"role":"user","content":"hi"}`)}
	withTools := bare
	withTools.Extra = map[string]json.RawMessage{"tools": json.RawMessage(`"` + strings.Repeat("x", 40000) + `"`)}
	if got, base := EstimateTokens(withTools), EstimateTokens(bare); got < base+9000 {
		t.Errorf("estimate with 40KB of tools = %d, without = %d; tools are not counted", got, base)
	}
}

// Agent harnesses shorten their history and retry only when the refusal reads as
// a context-length error.
func TestFitsContext_UsesTheWordingHarnessesRecogniseAsAnOverflow(t *testing.T) {
	err := FitsContext(Request{Messages: rawMessages(`{"role":"user","content":"` + strings.Repeat("a", 4000) + `"}`)}, Capabilities{ContextWindow: 100})
	if !errors.Is(err, ErrContextTooLarge) || !strings.Contains(strings.ToLower(err.Error()), "context length exceeded") {
		t.Fatalf("err = %v", err)
	}
}

// What a call cost Teepin counts the cache discount: fresh input at the full
// price, cache reads at a tenth, cache writes at a quarter more.
func TestVendorInputCost_CountsTheCacheDiscount(t *testing.T) {
	// $3 per million. 1,000 fresh + 16,000 read + 300 written (InputTokens is the total).
	u := Usage{InputTokens: 17300, CachedInputTokens: 16000, CacheWriteTokens: 300}
	want := (1000 + 16000*0.10 + 300*1.25) / 1e6 * 3
	if got := VendorInputCost(u, 3); got < want-1e-12 || got > want+1e-12 {
		t.Errorf("VendorInputCost = %v, want %v", got, want)
	}
	// Without caching it is plain tokens times price.
	if got := VendorInputCost(Usage{InputTokens: 2_000_000}, 3); got < 6-1e-9 || got > 6+1e-9 {
		t.Errorf("uncached cost = %v, want 6", got)
	}
	// A backend over-reporting cached tokens never makes the cost negative.
	if got := VendorInputCost(Usage{InputTokens: 100, CachedInputTokens: 500}, 3); got < 0 {
		t.Errorf("cost = %v, must not be negative", got)
	}
}
