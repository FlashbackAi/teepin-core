// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package modelprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/FlashbackAi/teepin-core/pkg/inference"
)

// reply is the part of an OpenAI-shaped completion the probes read.
type reply struct {
	content   string
	toolCalls []toolCall
	// truncated: the model hit its output limit (finish_reason "length").
	truncated bool
}

type toolCall struct {
	id   string
	name string
	args string
}

// inconclusive marks a failure that says nothing about the capability: the
// backend was unreachable, so the test could not run.
type inconclusive struct{ err error }

func (e inconclusive) Error() string { return e.err.Error() }
func (e inconclusive) Unwrap() error { return e.err }

// ask sends one request and parses the reply. A backend outage is returned as
// inconclusive; a refusal (4xx) is an ordinary error, which counts as the
// capability not working.
func (r *Runner) ask(ctx context.Context, messages []any, extra map[string]any, maxTokens int) (reply, error) {
	req := inference.Request{Model: r.Route, MaxTokens: maxTokens}
	for _, m := range messages {
		raw, err := json.Marshal(m)
		if err != nil {
			return reply{}, err
		}
		req.Messages = append(req.Messages, raw)
	}
	if len(extra) > 0 {
		req.Extra = map[string]json.RawMessage{}
		for k, v := range extra {
			raw, err := json.Marshal(v)
			if err != nil {
				return reply{}, err
			}
			req.Extra[k] = raw
		}
	}
	cctx, cancel := context.WithTimeout(ctx, r.CallTimeout)
	defer cancel()
	resp, err := r.Provider.Complete(cctx, req)
	if err != nil {
		if errors.Is(err, inference.ErrProviderRejected) || errors.Is(err, inference.ErrContextTooLarge) {
			return reply{}, err
		}
		return reply{}, inconclusive{err}
	}
	rep, err := parseReply(resp.Body)
	if err != nil {
		return reply{}, err
	}
	// A model that spent its whole allowance thinking (reasoning models do) and
	// never reached its answer has not shown it cannot do the thing; judging it
	// on a cut-off reply would wrongly mark a capable model as failing.
	if rep.truncated && len(rep.toolCalls) == 0 {
		return reply{}, inconclusive{errors.New("the model ran out of output tokens before answering")}
	}
	return rep, nil
}

func parseReply(body json.RawMessage) (reply, error) {
	var parsed struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content   json.RawMessage `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return reply{}, fmt.Errorf("the reply was not a chat completion: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return reply{}, errors.New("the reply had no choices")
	}
	msg := parsed.Choices[0].Message
	var out reply
	var text string
	if len(msg.Content) > 0 && string(msg.Content) != "null" {
		if err := json.Unmarshal(msg.Content, &text); err != nil {
			// Content as a list of parts: join the text ones.
			var parts []struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(msg.Content, &parts) == nil {
				for _, p := range parts {
					text += p.Text
				}
			}
		}
	}
	out.content = text
	out.truncated = parsed.Choices[0].FinishReason == "length"
	for _, tc := range msg.ToolCalls {
		args := string(tc.Function.Arguments)
		// Arguments normally arrive as a JSON string holding JSON; some
		// backends send the object itself.
		var asString string
		if json.Unmarshal(tc.Function.Arguments, &asString) == nil {
			args = asString
		}
		out.toolCalls = append(out.toolCalls, toolCall{id: tc.ID, name: tc.Function.Name, args: args})
	}
	return out, nil
}

// probeTokens is the output allowance for every probe request. Generous on
// purpose: a reasoning model may think for a long while before it calls a tool,
// and cutting it off would look like an inability.
const probeTokens = 2048

// --- tools ---

func tool(name, desc string, props map[string]any, required ...string) map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": desc,
			"parameters": map[string]any{
				"type":       "object",
				"properties": props,
				"required":   required,
			},
		},
	}
}

var (
	weatherTool = tool("get_weather", "Get the current weather for a city.",
		map[string]any{"city": map[string]any{"type": "string", "description": "City name"}}, "city")
	addTool = tool("add_numbers", "Add two numbers and return the sum.",
		map[string]any{"a": map[string]any{"type": "number"}, "b": map[string]any{"type": "number"}}, "a", "b")
	searchTool = tool("search_docs", "Search the documentation for a phrase.",
		map[string]any{"query": map[string]any{"type": "string"}}, "query")
)

func userMsg(text string) map[string]any { return map[string]any{"role": "user", "content": text} }

// manyTools is the three real tools buried among filler ones, as many as the
// builder sends, each with a realistic description and arguments.
func manyTools() []any {
	names := []string{"terminal", "file_editor", "task_tracker", "glob", "grep", "browser_navigate", "browser_click",
		"browser_type", "browser_get_state", "browser_get_content", "browser_scroll", "browser_go_back",
		"browser_list_tabs", "browser_switch_tab", "browser_close_tab", "present_deployment_plan", "ask_user",
		"request_secret", "deploy", "create_instance", "attach_domain", "think", "task"}
	var out []any
	for _, n := range names {
		out = append(out, tool(n, "Builder tool "+n+": performs the "+strings.ReplaceAll(n, "_", " ")+" action and returns its result as text.",
			map[string]any{
				"target":  map[string]any{"type": "string", "description": "What to act on"},
				"options": map[string]any{"type": "string", "description": "Optional settings"},
			}, "target"))
	}
	return append(out, weatherTool, addTool, searchTool)
}

func trim(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

func firstCall(rep reply, want string) (toolCall, error) {
	if len(rep.toolCalls) == 0 {
		return toolCall{}, fmt.Errorf("answered in text instead of calling a tool (%q)", trim(rep.content))
	}
	tc := rep.toolCalls[0]
	if tc.name != want {
		return toolCall{}, fmt.Errorf("called %q, expected %q", tc.name, want)
	}
	return tc, nil
}

func checkAdd(tc toolCall, a, b float64) error {
	var args struct {
		A *float64 `json:"a"`
		B *float64 `json:"b"`
	}
	if err := json.Unmarshal([]byte(tc.args), &args); err != nil {
		return fmt.Errorf("the arguments were not valid JSON (%q)", trim(tc.args))
	}
	if args.A == nil || args.B == nil {
		return fmt.Errorf("the arguments were missing a number (%q)", trim(tc.args))
	}
	if !((*args.A == a && *args.B == b) || (*args.A == b && *args.B == a)) {
		return fmt.Errorf("wrong numbers in the arguments (%q)", trim(tc.args))
	}
	return nil
}

// toolsAttempt runs the three steps of the plain tool test once: make a call,
// pick the right tool among several, and use a tool's result.
func (r *Runner) toolsAttempt(ctx context.Context, n int) error {
	cities := []string{"Paris", "Tokyo", "Nairobi", "Lima"}
	city := cities[n%len(cities)]

	// 1. A well-formed call with the right argument.
	rep, err := r.ask(ctx, []any{userMsg("What is the weather in " + city + " right now? Use the tool.")},
		map[string]any{"tools": []any{weatherTool}, "tool_choice": "auto"}, probeTokens)
	if err != nil {
		return fmt.Errorf("step 1 (make a tool call): %w", err)
	}
	tc, err := firstCall(rep, "get_weather")
	if err != nil {
		return fmt.Errorf("step 1 (make a tool call): %w", err)
	}
	var wargs struct {
		City string `json:"city"`
	}
	if json.Unmarshal([]byte(tc.args), &wargs) != nil || !strings.Contains(strings.ToLower(wargs.City), strings.ToLower(city)) {
		return fmt.Errorf("step 1 (make a tool call): wrong or unreadable arguments (%q)", trim(tc.args))
	}

	// 2. The right tool among several, with numeric arguments.
	a, b := float64(17+n), float64(25+2*n)
	rep, err = r.ask(ctx, []any{userMsg(fmt.Sprintf("What is %v plus %v? Use a tool to work it out.", a, b))},
		map[string]any{"tools": []any{weatherTool, addTool, searchTool}, "tool_choice": "auto"}, probeTokens)
	if err != nil {
		return fmt.Errorf("step 2 (choose among tools): %w", err)
	}
	if tc, err = firstCall(rep, "add_numbers"); err != nil {
		return fmt.Errorf("step 2 (choose among tools): %w", err)
	}
	if err := checkAdd(tc, a, b); err != nil {
		return fmt.Errorf("step 2 (choose among tools): %w", err)
	}

	// 3. A tool's result is read and used in the next answer.
	temp := 20 + n
	msgs := []any{
		userMsg("What is the weather in " + city + "? Use the tool, then tell me the temperature."),
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
			"id": "call_1", "type": "function",
			"function": map[string]any{"name": "get_weather", "arguments": fmt.Sprintf(`{"city":%q}`, city)},
		}}},
		map[string]any{"role": "tool", "tool_call_id": "call_1", "content": fmt.Sprintf(`{"temp_c":%d,"sky":"sunny"}`, temp)},
	}
	rep, err = r.ask(ctx, msgs, map[string]any{"tools": []any{weatherTool}, "tool_choice": "auto"}, probeTokens)
	if err != nil {
		return fmt.Errorf("step 3 (use a tool result): %w", err)
	}
	if !strings.Contains(rep.content, fmt.Sprint(temp)) {
		return fmt.Errorf("step 3 (use a tool result): the answer did not use the result (%q)", trim(rep.content))
	}
	return nil
}

// toolsManyAttempt: pick add_numbers out of the builder-sized tool list.
func (r *Runner) toolsManyAttempt(ctx context.Context, n int) error {
	a, b := float64(31+n), float64(12+n)
	rep, err := r.ask(ctx, []any{userMsg(fmt.Sprintf("What is %v plus %v? Use the add_numbers tool.", a, b))},
		map[string]any{"tools": manyTools(), "tool_choice": "auto"}, probeTokens)
	if err != nil {
		return err
	}
	tc, err := firstCall(rep, "add_numbers")
	if err != nil {
		return err
	}
	return checkAdd(tc, a, b)
}

// --- vision and audio ---

func (r *Runner) visionAttempt(ctx context.Context, n int) error {
	left, right := colours[(2*n)%len(colours)], colours[(2*n+1)%len(colours)]
	msg := map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "This image is split down the middle into a left half and a right half, each one solid colour. Answer in this form and nothing else: left=<colour>, right=<colour>."},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": twoToneImage(left, right)}},
	}}
	rep, err := r.ask(ctx, []any{msg}, nil, probeTokens)
	if err != nil {
		return err
	}
	text := strings.ToLower(rep.content)
	li, ri := strings.Index(text, left.name), strings.Index(text, right.name)
	if li < 0 || ri < 0 || li > ri {
		return fmt.Errorf("expected left=%s, right=%s; got %q", left.name, right.name, trim(rep.content))
	}
	return nil
}

var numberWords = []string{"zero", "one", "two", "three", "four", "five"}

func (r *Runner) audioAttempt(ctx context.Context, n int) error {
	beeps := 2 + n%3
	msg := map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "How many separate beeps do you hear in this clip? Answer with just the number."},
		map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": beepsWAV(beeps), "format": "wav"}},
	}}
	rep, err := r.ask(ctx, []any{msg}, nil, probeTokens)
	if err != nil {
		return err
	}
	text := strings.ToLower(rep.content)
	if !strings.Contains(text, fmt.Sprint(beeps)) && !strings.Contains(text, numberWords[beeps]) {
		return fmt.Errorf("expected %d beeps; got %q", beeps, trim(rep.content))
	}
	return nil
}

// Runner tests one model through one provider.
type Runner struct {
	Provider inference.Provider
	// Route is the catalog route the requests name.
	Route string
	// Attempts is how many times each capability is tried; a capability
	// passes when at least Required attempts do the thing. Defaults 3 and 2.
	Attempts, Required int
	// CallTimeout bounds one model call. Default 90 seconds.
	CallTimeout time.Duration
	// Now is overridable for tests.
	Now func() time.Time
}

func (r *Runner) defaults() {
	if r.Attempts <= 0 {
		r.Attempts = 3
	}
	if r.Required <= 0 || r.Required > r.Attempts {
		r.Required = (r.Attempts + 2) / 2
	}
	if r.CallTimeout <= 0 {
		r.CallTimeout = 90 * time.Second
	}
	if r.Now == nil {
		r.Now = time.Now
	}
}

// baselineWorks reports whether the model answers a plain request with no
// tools, vision or audio: the control for "is the backend up at all".
func (r *Runner) baselineWorks(ctx context.Context) bool {
	rep, err := r.ask(ctx, []any{userMsg("Reply with the single word OK.")}, nil, probeTokens)
	if err != nil {
		var inc inconclusive
		return !errors.As(err, &inc) && rep.content != ""
	}
	return rep.content != "" || rep.truncated
}

// RunCapability tests one capability and reports the outcome.
func (r *Runner) RunCapability(ctx context.Context, c Capability) Check {
	r.defaults()
	var attempt func(context.Context, int) error
	switch c {
	case CapTools:
		attempt = r.toolsAttempt
	case CapToolsMany:
		attempt = r.toolsManyAttempt
	case CapVision:
		attempt = r.visionAttempt
	case CapAudio:
		attempt = r.audioAttempt
	case CapBuild:
		attempt = func(ctx context.Context, n int) error { return r.buildAttempt(ctx, n, false) }
	case CapBuildText:
		attempt = func(ctx context.Context, n int) error { return r.buildAttempt(ctx, n, true) }
	default:
		return Check{Capability: c, Status: StatusUntested, CheckedAt: r.Now()}
	}

	out := Check{Capability: c, CheckedAt: r.Now()}
	var lastFailure, lastOutage string
	outages := 0
	for i := 0; i < r.Attempts; i++ {
		if ctx.Err() != nil {
			break
		}
		out.Attempts++
		err := attempt(ctx, i)
		var inc inconclusive
		switch {
		case err == nil:
			out.Passed++
		case errors.As(err, &inc):
			outages++
			lastOutage = err.Error()
		default:
			lastFailure = err.Error()
		}
		// Stop early once the outcome is settled either way.
		if out.Passed >= r.Required || out.Attempts-outages-out.Passed > r.Attempts-r.Required {
			break
		}
	}

	switch {
	case out.Passed >= r.Required:
		out.Status = StatusPassed
		out.Detail = fmt.Sprintf("%d of %d attempts did it", out.Passed, out.Attempts)
	case outages > 0 && out.Passed+outages >= r.Required:
		// Not enough conclusive attempts to call it either way, unless the
		// errors are specific to tools: a backend that answers a plain request
		// but errors every time tools are included does not support them (a
		// router that chokes on the tools field answers 502). Telling that from
		// an outage takes one plain request.
		if (c == CapTools || c == CapToolsMany) && out.Passed == 0 && r.baselineWorks(ctx) {
			out.Status = StatusFailed
			out.Detail = "the backend answers a plain request but errors when tools are included: " + trim(lastOutage)
		} else {
			out.Status = StatusError
			out.Detail = "could not finish the check: " + trim(lastOutage)
		}
	default:
		out.Status = StatusFailed
		out.Detail = trim(lastFailure)
	}
	return out
}
