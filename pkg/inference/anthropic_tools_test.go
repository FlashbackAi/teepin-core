// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package inference

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

const okAnthropicReply = `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-haiku-4-5","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`

// The live incident this file pins: the agent's tools never reached Claude,
// so it could only answer in prose. The declared tools must arrive in
// Anthropic's shape — including schema keywords beyond properties/required.
func TestAnthropic_Complete_SendsToolsInAnthropicShape(t *testing.T) {
	p, _ := newTestAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		var got struct {
			Tools []struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				InputSchema map[string]any `json:"input_schema"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		if len(got.Tools) != 1 {
			t.Fatalf("got %d tools upstream, want 1", len(got.Tools))
		}
		tool := got.Tools[0]
		if tool.Name != "terminal" || tool.Description != "Run a shell command" {
			t.Errorf("tool = %+v", tool)
		}
		if tool.InputSchema["type"] != "object" {
			t.Errorf("input_schema.type = %v, want object", tool.InputSchema["type"])
		}
		props, _ := tool.InputSchema["properties"].(map[string]any)
		if _, ok := props["command"]; !ok {
			t.Errorf("input_schema.properties = %v, want the command property", tool.InputSchema["properties"])
		}
		if req, _ := tool.InputSchema["required"].([]any); len(req) != 1 || req[0] != "command" {
			t.Errorf("input_schema.required = %v, want [command]", tool.InputSchema["required"])
		}
		if tool.InputSchema["additionalProperties"] != false {
			t.Errorf("additionalProperties = %v, want false carried through", tool.InputSchema["additionalProperties"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(okAnthropicReply))
	})

	_, err := p.Complete(context.Background(), Request{
		Messages: rawMessages(`{"role":"user","content":"list files"}`),
		Extra: map[string]json.RawMessage{
			"tools": json.RawMessage(`[{"type":"function","function":{"name":"terminal","description":"Run a shell command",` +
				`"parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"],"additionalProperties":false}}}]`),
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

func TestAnthropic_Complete_TranslatesToolChoice(t *testing.T) {
	cases := []struct {
		name       string
		toolChoice string
		parallel   string
		wantType   string
		wantName   string
		wantNoPar  bool
	}{
		{name: "required becomes any", toolChoice: `"required"`, wantType: "any"},
		{name: "named function becomes tool", toolChoice: `{"type":"function","function":{"name":"terminal"}}`, wantType: "tool", wantName: "terminal"},
		{name: "parallel off without a choice", parallel: `false`, wantType: "auto", wantNoPar: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := newTestAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
				var got struct {
					ToolChoice map[string]any `json:"tool_choice"`
				}
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if got.ToolChoice["type"] != tc.wantType {
					t.Errorf("tool_choice.type = %v, want %s", got.ToolChoice["type"], tc.wantType)
				}
				if tc.wantName != "" && got.ToolChoice["name"] != tc.wantName {
					t.Errorf("tool_choice.name = %v, want %s", got.ToolChoice["name"], tc.wantName)
				}
				if tc.wantNoPar && got.ToolChoice["disable_parallel_tool_use"] != true {
					t.Errorf("disable_parallel_tool_use = %v, want true", got.ToolChoice["disable_parallel_tool_use"])
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(okAnthropicReply))
			})
			extra := map[string]json.RawMessage{
				"tools": json.RawMessage(`[{"type":"function","function":{"name":"terminal","parameters":{"type":"object"}}}]`),
			}
			if tc.toolChoice != "" {
				extra["tool_choice"] = json.RawMessage(tc.toolChoice)
			}
			if tc.parallel != "" {
				extra["parallel_tool_calls"] = json.RawMessage(tc.parallel)
			}
			if _, err := p.Complete(context.Background(), Request{
				Messages: rawMessages(`{"role":"user","content":"hi"}`),
				Extra:    extra,
			}); err != nil {
				t.Fatalf("Complete: %v", err)
			}
		})
	}
}

// A multi-step agent turn replays its own tool calls and their results on
// every request. Both must reach Claude as real tool_use/tool_result blocks —
// with every result for one assistant turn inside a single user turn —
// rather than being flattened into prose.
func TestAnthropic_Complete_TranslatesToolCallHistory(t *testing.T) {
	p, _ := newTestAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		var got struct {
			Messages []struct {
				Role    string           `json:"role"`
				Content []map[string]any `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(got.Messages) != 3 {
			t.Fatalf("got %d turns, want 3 (user, assistant with tool_use, one merged user turn of tool_results)", len(got.Messages))
		}

		assistant := got.Messages[1]
		if assistant.Role != "assistant" || len(assistant.Content) != 3 {
			t.Fatalf("assistant turn = %+v, want text + 2 tool_use blocks", assistant)
		}
		if assistant.Content[0]["type"] != "text" || assistant.Content[0]["text"] != "Checking." {
			t.Errorf("assistant text block = %v", assistant.Content[0])
		}
		use := assistant.Content[1]
		if use["type"] != "tool_use" || use["id"] != "call_1" || use["name"] != "terminal" {
			t.Errorf("tool_use block = %v", use)
		}
		if input, _ := use["input"].(map[string]any); input["command"] != "ls" {
			t.Errorf("tool_use input = %v, want the parsed arguments object", use["input"])
		}
		if empty, _ := assistant.Content[2]["input"].(map[string]any); empty == nil {
			t.Errorf("a call with empty arguments must send input {}, got %v", assistant.Content[2]["input"])
		}

		results := got.Messages[2]
		if results.Role != "user" || len(results.Content) != 2 {
			t.Fatalf("results turn = %+v, want one user turn holding both tool_results", results)
		}
		first, second := results.Content[0], results.Content[1]
		if first["type"] != "tool_result" || first["tool_use_id"] != "call_1" {
			t.Errorf("first tool_result = %v", first)
		}
		if second["tool_use_id"] != "call_2" {
			t.Errorf("second tool_result = %v, want tool_use_id call_2", second)
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(okAnthropicReply))
	})

	_, err := p.Complete(context.Background(), Request{
		Messages: rawMessages(
			`{"role":"user","content":"what's here?"}`,
			`{"role":"assistant","content":"Checking.","tool_calls":[`+
				`{"id":"call_1","type":"function","function":{"name":"terminal","arguments":"{\"command\":\"ls\"}"}},`+
				`{"id":"call_2","type":"function","function":{"name":"task_tracker","arguments":""}}]}`,
			`{"role":"tool","tool_call_id":"call_1","content":"index.html"}`,
			`{"role":"tool","tool_call_id":"call_2","content":""}`,
		),
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

// Claude's tool_use must come back as OpenAI tool_calls — the field an
// OpenAI-speaking harness actually executes — with content null and
// finish_reason tool_calls.
func TestAnthropic_Complete_ReturnsToolUseAsToolCalls(t *testing.T) {
	p, _ := newTestAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant",` +
			`"content":[{"type":"tool_use","id":"toolu_1","name":"terminal","input":{"command":"ls -la"}}],` +
			`"model":"claude-haiku-4-5","stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":7}}`))
	})

	resp, err := p.Complete(context.Background(), Request{Messages: rawMessages(`{"role":"user","content":"list files"}`)})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	var body struct {
		Choices []struct {
			Message struct {
				Content   *string          `json:"content"`
				ToolCalls []openAIToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("resp.Body: %v", err)
	}
	msg := body.Choices[0].Message
	if msg.Content != nil {
		t.Errorf("content = %q, want null for a tool-only turn", *msg.Content)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("got %d tool_calls, want 1", len(msg.ToolCalls))
	}
	call := msg.ToolCalls[0]
	if call.ID != "toolu_1" || call.Type != "function" || call.Function.Name != "terminal" {
		t.Errorf("tool call = %+v", call)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || args["command"] != "ls -la" {
		t.Errorf("arguments = %q, want a JSON string of the input", call.Function.Arguments)
	}
	if body.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", body.Choices[0].FinishReason)
	}
}

// strict is forwarded only when the harness explicitly sets it — never
// invented — since Anthropic enforces additionalProperties:false at every
// schema level (confirmed live 2026-09-23) and a tool whose schema doesn't
// already meet that bar must not have it silently turned on.
func TestAnthropic_Complete_ForwardsStrictOnlyWhenHarnessSetsIt(t *testing.T) {
	cases := []struct {
		name  string
		extra string
		want  *bool
	}{
		{name: "unset", extra: `[{"type":"function","function":{"name":"t","parameters":{"type":"object"}}}]`, want: nil},
		{name: "explicit true", extra: `[{"type":"function","function":{"name":"t","strict":true,"parameters":{"type":"object"}}}]`, want: boolPtr(true)},
		{name: "explicit false", extra: `[{"type":"function","function":{"name":"t","strict":false,"parameters":{"type":"object"}}}]`, want: boolPtr(false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := newTestAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
				var got struct {
					Tools []struct {
						Strict *bool `json:"strict"`
					} `json:"tools"`
				}
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Fatalf("decode: %v", err)
				}
				gotStrict := got.Tools[0].Strict
				if (gotStrict == nil) != (tc.want == nil) || (gotStrict != nil && *gotStrict != *tc.want) {
					t.Errorf("upstream strict = %v, want %v", derefBool(gotStrict), derefBool(tc.want))
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(okAnthropicReply))
			})
			_, err := p.Complete(context.Background(), Request{
				Messages: rawMessages(`{"role":"user","content":"hi"}`),
				Extra:    map[string]json.RawMessage{"tools": json.RawMessage(tc.extra)},
			})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }
func derefBool(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}

// Dropping a declared tool silently is the exact failure this translation
// exists to end, so a tool type it cannot translate is refused up front.
func TestAnthropic_Complete_RejectsUntranslatableToolBeforeDispatch(t *testing.T) {
	p, _ := newTestAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("backend was called with a tool the adapter cannot translate")
	})
	_, err := p.Complete(context.Background(), Request{
		Messages: rawMessages(`{"role":"user","content":"hi"}`),
		Extra:    map[string]json.RawMessage{"tools": json.RawMessage(`[{"type":"web_search"}]`)},
	})
	if !errors.Is(err, ErrProviderRejected) {
		t.Errorf("got %v, want ErrProviderRejected", err)
	}
}
