// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package modelprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// The builder-readiness check. The single-step tool tests prove a model can
// make a call; a build is dozens of them in a row, each depending on the last.
// This runs a small real task the way the builder does it, over several turns,
// with tools shaped like the builder's (a terminal and a file editor) against a
// pretend workspace held in memory: write a page, look at what was written,
// finish. A model passes when the page it built is right and it checked it.
//
// It runs in two modes. Native sends the tools as structured tool definitions
// (what a model with working tool calls gets). Text describes the tools in the
// prompt and reads function calls out of the reply, the protocol the agent
// harness falls back to for a model whose native tool calls do not work (the
// SDK's non-native mode; this follows its documented format, so it measures
// the same skill rather than the SDK's exact bytes).

const (
	// CapBuild: the model completes the build task using native tool calls.
	CapBuild Capability = "build"
	// CapBuildText: the model completes it through the text protocol.
	CapBuildText Capability = "build_text"
)

// maxLoopTurns bounds one run. The task takes three or four turns; twelve
// leaves room to recover from a mistake and still stops a model that loops.
const maxLoopTurns = 12

// builderTools are the tools the pretend builder offers.
func builderTools() []any {
	return []any{
		tool("terminal", "Run a shell command in the workspace and return its output.",
			map[string]any{"command": map[string]any{"type": "string", "description": "The command to run"}}, "command"),
		tool("file_editor", "View, create or edit a file. Commands: view, create, str_replace.",
			map[string]any{
				"command":   map[string]any{"type": "string", "enum": []string{"view", "create", "str_replace"}},
				"path":      map[string]any{"type": "string", "description": "Absolute path of the file"},
				"file_text": map[string]any{"type": "string", "description": "File content for create"},
				"old_str":   map[string]any{"type": "string", "description": "Text to replace, for str_replace"},
				"new_str":   map[string]any{"type": "string", "description": "Replacement text, for str_replace"},
			}, "command", "path"),
		tool("finish", "Finish the task with a short summary.",
			map[string]any{"message": map[string]any{"type": "string"}}, "message"),
	}
}

var headings = []string{"Teepin", "Orbit", "Maple", "Harbor"}

// workspace is the pretend filesystem and the record of what the model did.
type workspace struct {
	files    map[string]string
	created  bool
	viewed   bool // looked at the file after creating it
	finished bool
}

func newWorkspace() *workspace { return &workspace{files: map[string]string{}} }

// run executes one tool call and returns the text the model sees. Mistakes
// (a bad command, a file that exists) come back as errors the model can read and
// recover from, as they would from the real tools.
func (w *workspace) run(name string, args map[string]any) string {
	str := func(k string) string { s, _ := args[k].(string); return s }
	switch name {
	case "terminal":
		return w.terminal(strings.TrimSpace(str("command")))
	case "file_editor":
		path := str("path")
		switch str("command") {
		case "create":
			if path == "" {
				return "Error: path is required"
			}
			if _, exists := w.files[path]; exists {
				return fmt.Sprintf("Error: %s already exists. Use str_replace to change it.", path)
			}
			w.files[path] = str("file_text")
			if strings.HasSuffix(path, "index.html") {
				w.created = true
			}
			return "File created successfully at: " + path
		case "view":
			content, ok := w.files[path]
			if !ok {
				return fmt.Sprintf("Error: %s does not exist", path)
			}
			if w.created && strings.HasSuffix(path, "index.html") {
				w.viewed = true
			}
			var b strings.Builder
			for i, line := range strings.Split(content, "\n") {
				fmt.Fprintf(&b, "%6d\t%s\n", i+1, line)
			}
			return b.String()
		case "str_replace":
			content, ok := w.files[path]
			if !ok {
				return fmt.Sprintf("Error: %s does not exist", path)
			}
			old := str("old_str")
			if old == "" || strings.Count(content, old) != 1 {
				return "Error: old_str must appear exactly once in the file"
			}
			w.files[path] = strings.Replace(content, old, str("new_str"), 1)
			return "The file has been edited."
		default:
			return "Error: command must be one of view, create, str_replace"
		}
	case "finish":
		w.finished = true
		return "Task finished."
	default:
		return fmt.Sprintf("Error: there is no tool named %q", name)
	}
}

var catRe = regexp.MustCompile(`^cat\s+(\S+)$`)

func (w *workspace) terminal(cmd string) string {
	switch {
	case cmd == "ls" || cmd == "ls -la" || cmd == "ls -l" || strings.HasPrefix(cmd, "ls /workspace") || cmd == "pwd":
		if cmd == "pwd" {
			return "/workspace"
		}
		var names []string
		for p := range w.files {
			names = append(names, strings.TrimPrefix(p, "/workspace/"))
		}
		sort.Strings(names)
		return strings.Join(names, "\n")
	case catRe.MatchString(cmd):
		p := catRe.FindStringSubmatch(cmd)[1]
		if !strings.HasPrefix(p, "/") {
			p = "/workspace/" + p
		}
		content, ok := w.files[p]
		if !ok {
			return "cat: " + p + ": No such file or directory"
		}
		if w.created && strings.HasSuffix(p, "index.html") {
			w.viewed = true
		}
		return content
	case strings.HasPrefix(cmd, "mkdir"):
		return ""
	default:
		return "bash: " + strings.Fields(cmd + " ")[0] + ": command not available in this workspace"
	}
}

// verdict says whether the workspace holds a correct, checked result.
func (w *workspace) verdict(heading string) error {
	page, ok := w.files["/workspace/index.html"]
	if !ok {
		return errors.New("never created /workspace/index.html")
	}
	if !regexp.MustCompile(`(?is)<h1[^>]*>\s*` + regexp.QuoteMeta(heading) + `\s*</h1>`).MatchString(page) {
		return fmt.Errorf("the page has no <h1> reading %q", heading)
	}
	if !regexp.MustCompile(`(?i)<button[^>]*\bid\s*=\s*["']go["']`).MatchString(page) {
		return errors.New(`the page has no <button id="go">`)
	}
	if !w.viewed {
		return errors.New("never looked at the file after writing it")
	}
	if !w.finished {
		return errors.New("never finished the task")
	}
	return nil
}

func taskPrompt(heading string) string {
	return fmt.Sprintf("Build a web page. Create /workspace/index.html with an <h1> that says %q and a <button id=\"go\">Go</button>. "+
		"Then check the file with a tool, fix it if anything is wrong, and when it is right call finish with a one-line summary.", heading)
}

// buildAttempt runs the loop once in the given mode.
func (r *Runner) buildAttempt(ctx context.Context, n int, text bool) error {
	heading := headings[n%len(headings)]
	ws := newWorkspace()
	tools := builderTools()

	var msgs []any
	var extra map[string]any
	if text {
		msgs = append(msgs, map[string]any{"role": "system", "content": textProtocolPrompt(tools)})
	} else {
		extra = map[string]any{"tools": tools, "tool_choice": "auto"}
	}
	msgs = append(msgs, userMsg(taskPrompt(heading)))

	for turn := 0; turn < maxLoopTurns; turn++ {
		rep, err := r.ask(ctx, msgs, extra, probeTokens)
		if err != nil {
			return fmt.Errorf("turn %d: %w", turn+1, err)
		}
		calls := rep.toolCalls
		if text {
			calls = parseTextCalls(rep.content)
		}
		if len(calls) == 0 {
			// No call: the model has stopped. It passes only if the work is done.
			if verr := ws.verdict(heading); verr != nil {
				return fmt.Errorf("stopped after %d turns without finishing: %v", turn+1, verr)
			}
			return nil
		}

		// Only the first call is run in text mode (the protocol allows one at a
		// time); native calls are all run, in order, each answered.
		if text {
			calls = calls[:1]
		}
		if !text {
			var asCalls []any
			for i, c := range calls {
				calls[i].id = nonEmpty(c.id, fmt.Sprintf("call_%d_%d", turn, i))
				asCalls = append(asCalls, map[string]any{"id": calls[i].id, "type": "function",
					"function": map[string]any{"name": c.name, "arguments": c.args}})
			}
			msgs = append(msgs, map[string]any{"role": "assistant", "content": nil, "tool_calls": asCalls})
		} else {
			msgs = append(msgs, map[string]any{"role": "assistant", "content": rep.content})
		}
		for _, c := range calls {
			var args map[string]any
			var out string
			if err := json.Unmarshal([]byte(c.args), &args); err != nil {
				out = "Error: the arguments were not valid JSON"
			} else {
				out = ws.run(c.name, args)
			}
			if text {
				msgs = append(msgs, userMsg(fmt.Sprintf("EXECUTION RESULT of [%s]:\n%s", c.name, out)))
			} else {
				msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": c.id, "content": out})
			}
		}
		if ws.finished {
			return ws.verdict(heading)
		}
	}
	return fmt.Errorf("did not finish within %d turns", maxLoopTurns)
}

func nonEmpty(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}

// textProtocolPrompt describes the tools in words and fixes the reply format,
// following the agent harness's non-native function-calling convention.
func textProtocolPrompt(tools []any) string {
	var b strings.Builder
	b.WriteString("You have access to the following functions:\n\n")
	for i, t := range tools {
		fn := t.(map[string]any)["function"].(map[string]any)
		fmt.Fprintf(&b, "---- BEGIN FUNCTION #%d: %s ----\nDescription: %s\nParameters:\n", i+1, fn["name"], fn["description"])
		params := fn["parameters"].(map[string]any)
		required := map[string]bool{}
		for _, k := range params["required"].([]string) {
			required[k] = true
		}
		props := params["properties"].(map[string]any)
		var keys []string
		for k := range props {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for j, k := range keys {
			p := props[k].(map[string]any)
			req := "optional"
			if required[k] {
				req = "required"
			}
			fmt.Fprintf(&b, "  (%d) %s (%v, %s): %v\n", j+1, k, p["type"], req, p["description"])
		}
		fmt.Fprintf(&b, "---- END FUNCTION #%d ----\n\n", i+1)
	}
	b.WriteString("If you choose to call a function ONLY reply in the following format with NO suffix:\n\n" +
		"<function=example_function_name>\n<parameter=example_parameter_1>value_1</parameter>\n" +
		"<parameter=example_parameter_2>\nThis is the value for the second parameter\nthat can span\nmultiple lines\n</parameter>\n</function>\n\n" +
		"<IMPORTANT>\nReminder:\n- Function calls MUST follow the specified format, start with <function= and end with </function>\n" +
		"- Required parameters MUST be specified\n- Only call one function at a time\n" +
		"- You may provide optional reasoning for your function call in natural language BEFORE the function call, but NOT after\n" +
		"- If there is no function call available, answer the question like normal with your current knowledge and do not tell the user about function calls\n</IMPORTANT>")
	return b.String()
}

var (
	textCallRe  = regexp.MustCompile(`(?s)<function=([A-Za-z0-9_\-]+)>(.*?)</function>`)
	textParamRe = regexp.MustCompile(`(?s)<parameter=([A-Za-z0-9_\-]+)>\n?(.*?)\n?</parameter>`)
)

// parseTextCalls reads function calls out of a reply written in the text
// protocol, returning each as a call with JSON arguments.
func parseTextCalls(content string) []toolCall {
	var out []toolCall
	for _, m := range textCallRe.FindAllStringSubmatch(content, -1) {
		args := map[string]any{}
		for _, p := range textParamRe.FindAllStringSubmatch(m[2], -1) {
			args[p[1]] = p[2]
		}
		raw, _ := json.Marshal(args)
		out = append(out, toolCall{name: m[1], args: string(raw)})
	}
	return out
}
