// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package modelprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/FlashbackAi/teepin-core/pkg/inference"
)

// fakeBuilder plays a model doing the build task, one scripted step per turn,
// in native or text mode, with chosen mistakes.
type fakeBuilder struct {
	text bool
	// mistakes
	skipCheck    bool // finishes without ever looking at the file
	wrongHeading bool
	neverFinish  bool // says it is done in prose instead of calling finish
	loops        bool // keeps listing the directory forever
	badFirstCall bool // first call has unreadable arguments, then recovers
	sawTools     bool // a request carried native tool definitions
	sawProtocol  bool // a request carried the text protocol prompt
	calls        int
}

func (f *fakeBuilder) Name() string                         { return "fakeBuilder" }
func (f *fakeBuilder) Capabilities() inference.Capabilities { return inference.Capabilities{} }
func (f *fakeBuilder) Stream(context.Context, inference.Request, func(inference.Chunk) error) error {
	return errors.New("not used")
}

var headingRe = regexp.MustCompile(`says "([^"]+)"`)

func (f *fakeBuilder) Complete(_ context.Context, req inference.Request) (*inference.Response, error) {
	f.calls++
	if _, ok := req.Extra["tools"]; ok {
		f.sawTools = true
	}
	var heading string
	results := 0
	for _, raw := range req.Messages {
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(raw, &m)
		var text string
		_ = json.Unmarshal(m.Content, &text)
		if m.Role == "system" && strings.Contains(text, "BEGIN FUNCTION") {
			f.sawProtocol = true
		}
		if mm := headingRe.FindStringSubmatch(text); mm != nil {
			heading = mm[1]
		}
		if m.Role == "tool" || (m.Role == "user" && strings.HasPrefix(text, "EXECUTION RESULT of [")) {
			results++
		}
	}
	if f.wrongHeading {
		heading = "Wrong"
	}
	page := fmt.Sprintf("<html><body><h1>%s</h1><button id=\"go\">Go</button></body></html>", heading)

	type step struct {
		name string
		args map[string]any
	}
	var next *step
	switch {
	case f.loops:
		next = &step{"terminal", map[string]any{"command": "ls"}}
	case f.badFirstCall && results == 0:
		return f.reply("file_editor", map[string]any{}, "{not json")
	default:
		idx := results
		if f.badFirstCall {
			idx = results - 1
		}
		switch idx {
		case 0:
			next = &step{"file_editor", map[string]any{"command": "create", "path": "/workspace/index.html", "file_text": page}}
		case 1:
			if f.skipCheck {
				next = &step{"finish", map[string]any{"message": "done"}}
			} else {
				next = &step{"terminal", map[string]any{"command": "cat /workspace/index.html"}}
			}
		case 2:
			if f.neverFinish {
				return textReply("All done, the page is built."), nil
			}
			next = &step{"finish", map[string]any{"message": "Built the page."}}
		default:
			return textReply("Finished."), nil
		}
	}
	args, _ := json.Marshal(next.args)
	return f.reply(next.name, next.args, string(args))
}

func (f *fakeBuilder) reply(name string, args map[string]any, rawArgs string) (*inference.Response, error) {
	if f.text {
		var b strings.Builder
		b.WriteString("I will do the next step.\n\n<function=" + name + ">\n")
		for k, v := range args {
			fmt.Fprintf(&b, "<parameter=%s>\n%v\n</parameter>\n", k, v)
		}
		b.WriteString("</function>")
		if rawArgs == "{not json" {
			return textReply("<function=file_editor>\n<parameter=command>\n</function>"), nil // missing pieces
		}
		return textReply(b.String()), nil
	}
	return toolReply(name, rawArgs, false), nil
}

func (f *fakeBuilder) run(text bool) Check {
	f.text = text
	c := CapBuild
	if text {
		c = CapBuildText
	}
	return runner(f).RunCapability(context.Background(), c)
}

func TestBuildLoop_CompetentModelPassesInBothModes(t *testing.T) {
	for _, text := range []bool{false, true} {
		f := &fakeBuilder{}
		got := f.run(text)
		if got.Status != StatusPassed {
			t.Errorf("text=%v: %s (%s)", text, got.Status, got.Detail)
		}
		if text && (f.sawTools || !f.sawProtocol) {
			t.Errorf("text mode must describe the tools in the prompt and send no tool definitions: tools=%v protocol=%v", f.sawTools, f.sawProtocol)
		}
		if !text && (!f.sawTools || f.sawProtocol) {
			t.Errorf("native mode must send tool definitions and no text protocol: tools=%v protocol=%v", f.sawTools, f.sawProtocol)
		}
	}
}

func TestBuildLoop_EachKindOfMistakeFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    fakeBuilder
		want string
	}{
		{"never looks at the file", fakeBuilder{skipCheck: true}, "never looked at the file"},
		{"builds the wrong page", fakeBuilder{wrongHeading: true}, "no <h1> reading"},
		{"says it is done without finishing", fakeBuilder{neverFinish: true}, "never finished"},
		{"loops without making progress", fakeBuilder{loops: true}, "did not finish within"},
	} {
		for _, text := range []bool{false, true} {
			f := tc.f
			got := f.run(text)
			if got.Status != StatusFailed || !strings.Contains(got.Detail, tc.want) {
				t.Errorf("%s (text=%v): %s %q, want failure containing %q", tc.name, text, got.Status, got.Detail, tc.want)
			}
		}
	}
}

func TestBuildLoop_RecoversFromABadCall(t *testing.T) {
	// The first call's arguments are unreadable; the workspace says so and the
	// model carries on. A model that recovers has not failed.
	f := &fakeBuilder{badFirstCall: true}
	if got := f.run(false); got.Status != StatusPassed {
		t.Errorf("native: %s (%s)", got.Status, got.Detail)
	}
}

func TestBuildLoop_ABackendOutageIsNotAFailure(t *testing.T) {
	got := runner(&fakeModel{outage: errors.New("503")}).RunCapability(context.Background(), CapBuild)
	if got.Status != StatusError {
		t.Errorf("%s (%s)", got.Status, got.Detail)
	}
}

func TestWorkspace_BehavesLikeTheRealTools(t *testing.T) {
	w := newWorkspace()
	if out := w.run("file_editor", map[string]any{"command": "view", "path": "/workspace/index.html"}); !strings.HasPrefix(out, "Error") {
		t.Errorf("viewing a missing file: %q", out)
	}
	w.run("file_editor", map[string]any{"command": "create", "path": "/workspace/index.html", "file_text": "a\nb"})
	if out := w.run("file_editor", map[string]any{"command": "create", "path": "/workspace/index.html", "file_text": "x"}); !strings.Contains(out, "already exists") {
		t.Errorf("creating over an existing file: %q", out)
	}
	if out := w.run("file_editor", map[string]any{"command": "str_replace", "path": "/workspace/index.html", "old_str": "zzz", "new_str": "q"}); !strings.HasPrefix(out, "Error") {
		t.Errorf("replacing text that is not there: %q", out)
	}
	if out := w.run("terminal", map[string]any{"command": "rm -rf /"}); !strings.Contains(out, "not available") {
		t.Errorf("an unknown command: %q", out)
	}
	if out := w.run("terminal", map[string]any{"command": "cat /workspace/index.html"}); out != "a\nb" {
		t.Errorf("cat: %q", out)
	}
	if !w.viewed {
		t.Error("looking at the file after creating it must be recorded")
	}
}

func TestParseTextCalls(t *testing.T) {
	calls := parseTextCalls("Let me create the file.\n\n<function=file_editor>\n<parameter=command>create</parameter>\n<parameter=path>/workspace/a.html</parameter>\n<parameter=file_text>\n<h1>Hi</h1>\nsecond line\n</parameter>\n</function>")
	if len(calls) != 1 || calls[0].name != "file_editor" {
		t.Fatalf("calls = %+v", calls)
	}
	var args map[string]string
	_ = json.Unmarshal([]byte(calls[0].args), &args)
	if args["command"] != "create" || args["path"] != "/workspace/a.html" || args["file_text"] != "<h1>Hi</h1>\nsecond line" {
		t.Errorf("args = %v", args)
	}
	if got := parseTextCalls("Just an answer, no call."); len(got) != 0 {
		t.Errorf("prose parsed as a call: %+v", got)
	}
}

func TestBuildMode(t *testing.T) {
	st := func(c Capability, s Status) Check { return Check{Capability: c, Status: s} }
	rep := func(cs ...Check) *Report { return &Report{Checks: cs} }
	for _, tc := range []struct {
		name     string
		rep      *Report
		declared bool
		want     ToolMode
	}{
		{"native tools and a passing build task: native", rep(st(CapTools, StatusPassed), st(CapBuild, StatusPassed)), false, ToolModeNative},
		{"native tools passed, build task not run yet: native", rep(st(CapTools, StatusPassed)), false, ToolModeNative},
		{"tools passed but the build task failed, text works: text", rep(st(CapTools, StatusPassed), st(CapBuild, StatusFailed), st(CapBuildText, StatusPassed)), true, ToolModeText},
		{"tools passed but the build task failed, text fails too: cannot build", rep(st(CapTools, StatusPassed), st(CapBuild, StatusFailed), st(CapBuildText, StatusFailed)), true, ""},
		{"native tools failed, text works: text", rep(st(CapTools, StatusFailed), st(CapBuildText, StatusPassed)), true, ToolModeText},
		{"native tools failed, no text result: cannot build despite the declaration", rep(st(CapTools, StatusFailed)), true, ""},
		{"never checked, declared tools: native (registered before checks existed)", nil, true, ToolModeNative},
		{"never checked, not declared: cannot build", nil, false, ""},
		{"tools check could not run, declared: native", rep(st(CapTools, StatusError)), true, ToolModeNative},
		{"tools check could not run, not declared: cannot build", rep(st(CapTools, StatusError)), false, ""},
		{"native works but loses lists of objects, text works: text", rep(st(CapTools, StatusPassed), st(CapToolsLists, StatusFailed), st(CapBuild, StatusPassed), st(CapBuildText, StatusPassed)), false, ToolModeText},
		{"native loses lists of objects, text fails: native, never locked out", rep(st(CapTools, StatusPassed), st(CapToolsLists, StatusFailed), st(CapBuild, StatusPassed), st(CapBuildText, StatusFailed)), false, ToolModeNative},
		{"native loses lists of objects, text not checked: native", rep(st(CapTools, StatusPassed), st(CapToolsLists, StatusFailed)), false, ToolModeNative},
		{"lists check passed: native even if text passed too", rep(st(CapTools, StatusPassed), st(CapToolsLists, StatusPassed), st(CapBuildText, StatusPassed)), false, ToolModeNative},
	} {
		got, ok := BuildMode(tc.rep, tc.declared)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("%s: got %q ok=%v, want %q", tc.name, got, ok, tc.want)
		}
	}
}

// comboModel answers the single-step probes like fakeModel and the build task
// like fakeBuilder, so the whole service can be driven end to end.
type comboModel struct {
	probes  *fakeModel
	builder *fakeBuilder
	text    bool
}

func (c *comboModel) Name() string                         { return "combo" }
func (c *comboModel) Capabilities() inference.Capabilities { return inference.Capabilities{} }
func (c *comboModel) Stream(context.Context, inference.Request, func(inference.Chunk) error) error {
	return errors.New("not used")
}
func (c *comboModel) Complete(ctx context.Context, req inference.Request) (*inference.Response, error) {
	for _, raw := range req.Messages {
		if strings.Contains(string(raw), "Build a web page") {
			_, hasTools := req.Extra["tools"]
			c.builder.text = !hasTools
			if hasTools && c.probes.noTools {
				return textReply("I would rather just talk."), nil // native tool calls do not work
			}
			return c.builder.Complete(ctx, req)
		}
	}
	return c.probes.Complete(ctx, req)
}

func TestService_RunsTheTextBuildTaskOnlyWhenNativeToolsDoNotWork(t *testing.T) {
	// Native tools work: the native build task runs, the text one does not.
	svc, _ := newService(&comboModel{probes: &fakeModel{}, builder: &fakeBuilder{}}, "teepin/test")
	rep, err := svc.Check(context.Background(), "teepin/test")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Check(CapBuild).Status != StatusPassed {
		t.Errorf("build = %s (%s)", rep.Check(CapBuild).Status, rep.Check(CapBuild).Detail)
	}
	if rep.Check(CapBuildText).Status != StatusUntested {
		t.Errorf("the text fallback must not run when native works: %s", rep.Check(CapBuildText).Status)
	}
	if mode, ok := BuildMode(rep, false); !ok || mode != ToolModeNative {
		t.Errorf("mode = %q ok = %v", mode, ok)
	}

	// Native tools do not work: the native build task is skipped, the text one
	// runs, and the model is usable in text mode.
	svc, _ = newService(&comboModel{probes: &fakeModel{noTools: true}, builder: &fakeBuilder{}}, "teepin/test")
	rep, err = svc.Check(context.Background(), "teepin/test")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Check(CapTools).Status != StatusFailed || rep.Check(CapBuild).Status != StatusUntested {
		t.Errorf("tools = %s, build = %s", rep.Check(CapTools).Status, rep.Check(CapBuild).Status)
	}
	if rep.Check(CapBuildText).Status != StatusPassed {
		t.Errorf("build_text = %s (%s)", rep.Check(CapBuildText).Status, rep.Check(CapBuildText).Detail)
	}
	if mode, ok := BuildMode(rep, true); !ok || mode != ToolModeText {
		t.Errorf("mode = %q ok = %v, want text", mode, ok)
	}
}

// A model like GLM-5.3: native tool calls pass, but lists of objects arrive
// empty. The text build task runs, and when it passes the model builds in text.
func TestService_LostListsOfObjectsRunTheTextBuildTask(t *testing.T) {
	svc, _ := newService(&comboModel{probes: &fakeModel{dropsLists: true}, builder: &fakeBuilder{}}, "teepin/test")
	rep, err := svc.Check(context.Background(), "teepin/test")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Check(CapTools).Status != StatusPassed || rep.Check(CapToolsLists).Status != StatusFailed {
		t.Fatalf("tools = %s, tools_lists = %s (%s)", rep.Check(CapTools).Status, rep.Check(CapToolsLists).Status, rep.Check(CapToolsLists).Detail)
	}
	if rep.Check(CapBuildText).Status != StatusPassed {
		t.Errorf("build_text = %s (%s)", rep.Check(CapBuildText).Status, rep.Check(CapBuildText).Detail)
	}
	if mode, ok := BuildMode(rep, false); !ok || mode != ToolModeText {
		t.Errorf("mode = %q ok = %v, want text", mode, ok)
	}
}
