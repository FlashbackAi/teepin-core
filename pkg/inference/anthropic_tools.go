// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package inference

import (
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
)

// OpenAI <-> Anthropic tool-calling translation for AnthropicProvider.
//
// Tool calling is the one part of the OpenAI chat shape that cannot pass
// through Anthropic untouched: tools are declared differently, a model's
// tool call comes back as a content block rather than a message field, and a
// tool's result is a content block inside a user turn rather than a message
// with its own "tool" role. Before this existed the adapter dropped all of
// it — tools were never sent, tool_use blocks were discarded, tool results
// were flattened to user text — and an agent harness saw a model that only
// ever answered in prose. Found live 2026-09-23: a Kumbha build agent on a
// Claude-backed route wrote pseudo tool calls as XML text (with guessed tool
// names and invented results), never ran a single command, and so never
// wrote a file.

// openAITool is an entry of an OpenAI request's "tools" array.
type openAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// openAIToolCall is one entry of an assistant message's "tool_calls" — the
// shape both read from request history and written into response bodies.
type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
		// Arguments is a JSON-encoded string, not an object — OpenAI's own
		// contract, which harnesses parse themselves.
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// toAnthropicTools translates the request's OpenAI "tools" array. A tool of
// any type other than "function" is rejected rather than skipped: silently
// dropping a tool the harness declared is exactly the failure this file
// exists to end.
func toAnthropicTools(extra map[string]json.RawMessage) ([]anthropic.ToolUnionParam, error) {
	raw, ok := extra["tools"]
	if !ok || isJSONNull(raw) {
		return nil, nil
	}
	var tools []openAITool
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, fmt.Errorf("invalid tools: %w", err)
	}

	out := make([]anthropic.ToolUnionParam, 0, len(tools))
	for _, t := range tools {
		if t.Type != "function" {
			return nil, fmt.Errorf("unsupported tool type %q", t.Type)
		}
		schema, err := toAnthropicInputSchema(t.Function.Parameters)
		if err != nil {
			return nil, fmt.Errorf("tool %q: %w", t.Function.Name, err)
		}
		tool := anthropic.ToolParam{Name: t.Function.Name, InputSchema: schema}
		if t.Function.Description != "" {
			tool.Description = anthropic.String(t.Function.Description)
		}
		out = append(out, anthropic.ToolUnionParam{OfTool: &tool})
	}
	return out, nil
}

// toAnthropicInputSchema maps an OpenAI function's JSON-schema "parameters"
// onto Anthropic's input_schema. properties/required have dedicated fields;
// every other keyword ($defs, additionalProperties, ...) rides along in
// ExtraFields so a schema that references its own definitions stays valid.
// "type" is dropped because Anthropic's input_schema is always "object".
func toAnthropicInputSchema(params json.RawMessage) (anthropic.ToolInputSchemaParam, error) {
	var schema anthropic.ToolInputSchemaParam
	if len(params) == 0 || isJSONNull(params) {
		return schema, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(params, &fields); err != nil {
		return schema, fmt.Errorf("invalid parameters schema: %w", err)
	}
	for key, value := range fields {
		switch key {
		case "type":
		case "properties":
			schema.Properties = value
		case "required":
			if err := json.Unmarshal(value, &schema.Required); err != nil {
				return schema, fmt.Errorf("invalid required list: %w", err)
			}
		default:
			if schema.ExtraFields == nil {
				schema.ExtraFields = make(map[string]any)
			}
			schema.ExtraFields[key] = value
		}
	}
	return schema, nil
}

// toAnthropicToolChoice translates OpenAI's "tool_choice" (plus
// "parallel_tool_calls": false, which Anthropic expresses on the choice
// itself). Absent both, it returns the zero value, which the SDK omits and
// Anthropic treats as "auto" — the same default OpenAI has.
func toAnthropicToolChoice(extra map[string]json.RawMessage) (anthropic.ToolChoiceUnionParam, error) {
	var choice anthropic.ToolChoiceUnionParam

	disableParallel := false
	if raw, ok := extra["parallel_tool_calls"]; ok && !isJSONNull(raw) {
		var parallel bool
		if err := json.Unmarshal(raw, &parallel); err != nil {
			return choice, fmt.Errorf("invalid parallel_tool_calls: %w", err)
		}
		disableParallel = !parallel
	}

	raw, ok := extra["tool_choice"]
	if !ok || isJSONNull(raw) {
		if disableParallel {
			choice.OfAuto = &anthropic.ToolChoiceAutoParam{DisableParallelToolUse: anthropic.Bool(true)}
		}
		return choice, nil
	}

	var mode string
	if err := json.Unmarshal(raw, &mode); err == nil {
		switch mode {
		case "auto":
			choice.OfAuto = &anthropic.ToolChoiceAutoParam{}
		case "required":
			choice.OfAny = &anthropic.ToolChoiceAnyParam{}
		case "none":
			return anthropic.ToolChoiceUnionParam{OfNone: &anthropic.ToolChoiceNoneParam{}}, nil
		default:
			return choice, fmt.Errorf("unsupported tool_choice %q", mode)
		}
	} else {
		var named struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if err := json.Unmarshal(raw, &named); err != nil || named.Function.Name == "" {
			return choice, fmt.Errorf("invalid tool_choice: %s", raw)
		}
		choice.OfTool = &anthropic.ToolChoiceToolParam{Name: named.Function.Name}
	}

	if disableParallel {
		switch {
		case choice.OfAuto != nil:
			choice.OfAuto.DisableParallelToolUse = anthropic.Bool(true)
		case choice.OfAny != nil:
			choice.OfAny.DisableParallelToolUse = anthropic.Bool(true)
		case choice.OfTool != nil:
			choice.OfTool.DisableParallelToolUse = anthropic.Bool(true)
		}
	}
	return choice, nil
}

// toolUseBlocks turns an assistant message's OpenAI tool_calls into
// Anthropic tool_use blocks. Arguments arrive as a JSON string; an empty
// one (a tool with no parameters) becomes {}, and one that is not valid
// JSON is rejected — forwarding it would only fail upstream, less clearly.
func toolUseBlocks(calls []openAIToolCall) ([]anthropic.ContentBlockParamUnion, error) {
	blocks := make([]anthropic.ContentBlockParamUnion, 0, len(calls))
	for _, call := range calls {
		args := json.RawMessage(call.Function.Arguments)
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		if !json.Valid(args) {
			return nil, fmt.Errorf("tool call %q has invalid JSON arguments", call.ID)
		}
		blocks = append(blocks, anthropic.NewToolUseBlock(call.ID, args, call.Function.Name))
	}
	return blocks, nil
}

// fromAnthropicToolUse turns a response's tool_use block back into an
// OpenAI tool call.
func fromAnthropicToolUse(block anthropic.ToolUseBlock) openAIToolCall {
	var call openAIToolCall
	call.ID = block.ID
	call.Type = "function"
	call.Function.Name = block.Name
	call.Function.Arguments = string(block.Input)
	if call.Function.Arguments == "" {
		call.Function.Arguments = "{}"
	}
	return call
}

func isJSONNull(raw json.RawMessage) bool {
	return string(raw) == "null"
}
