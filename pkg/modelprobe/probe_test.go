// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package modelprobe

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/FlashbackAi/teepin-core/pkg/inference"
)

// fakeModel answers the probes the way a capable model would, by reading the
// request itself: it decodes the generated image to name its colours and the
// generated sound to count its beeps. That also proves the fixtures contain
// what the questions claim.
type fakeModel struct {
	// behaviours to break
	noTools     bool // answers in prose instead of calling tools
	badArgs     bool // returns tool calls whose arguments are not JSON
	reject      error
	outage      error
	ignoresTool bool // never uses a tool result
	truncated   bool // runs out of output tokens before answering
	toolsBreak  bool // answers 502 only when the request carries tools
	calls       int
}

func (f *fakeModel) Name() string                         { return "fake" }
func (f *fakeModel) Capabilities() inference.Capabilities { return inference.Capabilities{} }
func (f *fakeModel) Stream(context.Context, inference.Request, func(inference.Chunk) error) error {
	return errors.New("not used")
}

type wireMsg struct {
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	ToolCalls json.RawMessage `json:"tool_calls"`
}

func (f *fakeModel) Complete(_ context.Context, req inference.Request) (*inference.Response, error) {
	f.calls++
	if f.outage != nil {
		return nil, fmt.Errorf("%w: %v", inference.ErrProviderUnavailable, f.outage)
	}
	if f.reject != nil {
		return nil, fmt.Errorf("%w: %v", inference.ErrProviderRejected, f.reject)
	}
	if f.truncated {
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
			"finish_reason": "length",
			"message":       map[string]any{"role": "assistant", "content": "Let me think about"},
		}}})
		return &inference.Response{Body: b}, nil
	}
	if _, has := req.Extra["tools"]; has && f.toolsBreak {
		return nil, fmt.Errorf("%w: upstream status 502", inference.ErrProviderUnavailable)
	}
	var msgs []wireMsg
	for _, raw := range req.Messages {
		var m wireMsg
		_ = json.Unmarshal(raw, &m)
		msgs = append(msgs, m)
	}
	last := msgs[len(msgs)-1]

	// A tool result is in the conversation: answer using it.
	if last.Role == "tool" {
		var res struct {
			Temp int `json:"temp_c"`
		}
		var content string
		_ = json.Unmarshal(last.Content, &content)
		_ = json.Unmarshal([]byte(content), &res)
		if f.ignoresTool {
			return textReply("It is sunny."), nil
		}
		return textReply(fmt.Sprintf("It is %d degrees and sunny.", res.Temp)), nil
	}

	// Multimodal content: a list of parts.
	var parts []map[string]any
	if json.Unmarshal(last.Content, &parts) == nil {
		return f.answerParts(parts), nil
	}

	var text string
	_ = json.Unmarshal(last.Content, &text)
	if strings.Contains(text, "Build a web page") {
		return textReply("I do not know how to do that."), nil // the build task needs fakeBuilder
	}
	if _, has := req.Extra["tools"]; has {
		if f.noTools {
			return textReply("I would use a tool, but here is my answer instead."), nil
		}
		var tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		_ = json.Unmarshal(req.Extra["tools"], &tools)
		switch {
		case strings.Contains(text, "weather"):
			city := regexp.MustCompile(`in ([A-Z][a-z]+)`).FindStringSubmatch(text)[1]
			return toolReply("get_weather", fmt.Sprintf(`{"city":%q}`, city), f.badArgs), nil
		default:
			m := regexp.MustCompile(`(\d+) plus (\d+)`).FindStringSubmatch(text)
			return toolReply("add_numbers", fmt.Sprintf(`{"a":%s,"b":%s}`, m[1], m[2]), f.badArgs), nil
		}
	}
	return textReply("ok"), nil
}

func (f *fakeModel) answerParts(parts []map[string]any) *inference.Response {
	for _, p := range parts {
		switch p["type"] {
		case "image_url":
			url := p["image_url"].(map[string]any)["url"].(string)
			l, r := nameColours(url)
			return textReply(fmt.Sprintf("left=%s, right=%s", l, r))
		case "input_audio":
			data := p["input_audio"].(map[string]any)["data"].(string)
			return textReply(fmt.Sprint(countBeeps(data)))
		}
	}
	return textReply("?")
}

func textReply(s string) *inference.Response {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": s}}}})
	return &inference.Response{Body: b}
}

func toolReply(name, args string, badArgs bool) *inference.Response {
	if badArgs {
		args = "{not json"
	}
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{
		"role": "assistant", "content": nil,
		"tool_calls": []any{map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": name, "arguments": args}}},
	}}}})
	return &inference.Response{Body: b}
}

// nameColours decodes the image and names the colour of its left and right half.
func nameColours(dataURL string) (string, string) {
	raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(dataURL, "data:image/png;base64,"))
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return "?", "?"
	}
	name := func(x, y int) string {
		r, g, b, _ := img.At(x, y).RGBA()
		best, bestD := "?", 1<<62
		for _, c := range colours {
			dr, dg, db := int(r>>8)-int(c.rgb.R), int(g>>8)-int(c.rgb.G), int(b>>8)-int(c.rgb.B)
			if d := dr*dr + dg*dg + db*db; d < bestD {
				best, bestD = c.name, d
			}
		}
		return best
	}
	bnd := img.Bounds()
	return name(bnd.Min.X+2, bnd.Min.Y+2), name(bnd.Max.X-3, bnd.Min.Y+2)
}

// countBeeps decodes the WAV and counts runs of sound separated by silence.
func countBeeps(b64 string) int {
	raw, _ := base64.StdEncoding.DecodeString(b64)
	if len(raw) < 44 {
		return -1
	}
	pcm := raw[44:]
	const window = 160 // 10 ms at 16 kHz
	beeps, inSound := 0, false
	for i := 0; i+window*2 <= len(pcm); i += window * 2 {
		var peak int
		for j := 0; j < window; j++ {
			v := int(int16(binary.LittleEndian.Uint16(pcm[i+j*2:])))
			if v < 0 {
				v = -v
			}
			if v > peak {
				peak = v
			}
		}
		loud := peak > 3000
		if loud && !inSound {
			beeps++
		}
		inSound = loud
	}
	return beeps
}

// singleStepCapabilities are the capabilities the plain fake model can answer;
// the build tasks need the scripted builder (agentloop_test.go).
var singleStepCapabilities = []Capability{CapTools, CapToolsMany, CapVision, CapAudio}

func runner(m inference.Provider) *Runner {
	return &Runner{Provider: m, Route: "teepin/test", CallTimeout: time.Second, Now: func() time.Time { return time.Unix(0, 0) }}
}

func TestFixtures_SayWhatTheQuestionsClaim(t *testing.T) {
	l, r := nameColours(twoToneImage(colours[2], colours[3]))
	if l != "green" || r != "yellow" {
		t.Errorf("image decoded as left=%s right=%s, want green/yellow", l, r)
	}
	for n := 2; n <= 4; n++ {
		if got := countBeeps(beepsWAV(n)); got != n {
			t.Errorf("sound with %d beeps decoded as %d", n, got)
		}
	}
	// A valid PNG decodes at the size claimed.
	raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(twoToneImage(colours[0], colours[1]), "data:image/png;base64,"))
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(raw)); err != nil || cfg.Width != 96 {
		t.Errorf("not a usable PNG: %v %+v", err, cfg)
	}
}

func TestRunner_CapableModelPassesEverything(t *testing.T) {
	r := runner(&fakeModel{})
	for _, c := range singleStepCapabilities {
		got := r.RunCapability(context.Background(), c)
		if got.Status != StatusPassed {
			t.Errorf("%s: %s (%s)", c, got.Status, got.Detail)
		}
	}
}

func TestRunner_TextOnlyModelFailsToolsWithAReason(t *testing.T) {
	got := runner(&fakeModel{noTools: true}).RunCapability(context.Background(), CapTools)
	if got.Status != StatusFailed || !strings.Contains(got.Detail, "step 1") || !strings.Contains(got.Detail, "instead of calling a tool") {
		t.Errorf("got %s: %q", got.Status, got.Detail)
	}
}

func TestRunner_MalformedArgumentsFail(t *testing.T) {
	got := runner(&fakeModel{badArgs: true}).RunCapability(context.Background(), CapTools)
	if got.Status != StatusFailed || !strings.Contains(got.Detail, "unreadable arguments") {
		t.Errorf("got %s: %q", got.Status, got.Detail)
	}
}

func TestRunner_IgnoringAToolResultFails(t *testing.T) {
	got := runner(&fakeModel{ignoresTool: true}).RunCapability(context.Background(), CapTools)
	if got.Status != StatusFailed || !strings.Contains(got.Detail, "step 3") {
		t.Errorf("got %s: %q", got.Status, got.Detail)
	}
}

func TestRunner_ARefusalCountsAgainstTheCapability(t *testing.T) {
	got := runner(&fakeModel{reject: errors.New("images are not supported")}).RunCapability(context.Background(), CapVision)
	if got.Status != StatusFailed || !strings.Contains(got.Detail, "images are not supported") {
		t.Errorf("got %s: %q", got.Status, got.Detail)
	}
}

func TestRunner_AnOutageIsNotEvidence(t *testing.T) {
	f := &fakeModel{outage: errors.New("503")}
	got := runner(f).RunCapability(context.Background(), CapTools)
	if got.Status != StatusError {
		t.Fatalf("a backend that was down must not read as a failed capability: %s (%s)", got.Status, got.Detail)
	}
}

func TestRunner_StopsOnceTheOutcomeIsSettled(t *testing.T) {
	f := &fakeModel{}
	runner(f).RunCapability(context.Background(), CapVision)
	if f.calls != 2 {
		t.Errorf("a passing capability needs only the 2 required attempts, made %d calls", f.calls)
	}
	f = &fakeModel{noTools: true}
	runner(f).RunCapability(context.Background(), CapTools)
	if f.calls != 2 {
		t.Errorf("a failing capability can stop once it cannot reach 2 passes (2 failures), made %d calls", f.calls)
	}
}

func TestEffective(t *testing.T) {
	passed := &Report{Checks: []Check{{Capability: CapTools, Status: StatusPassed}}}
	failed := &Report{Checks: []Check{{Capability: CapTools, Status: StatusFailed}}}
	errored := &Report{Checks: []Check{{Capability: CapTools, Status: StatusError}}}
	for _, tc := range []struct {
		name     string
		r        *Report
		declared bool
		want     bool
	}{
		{"verified beats an undeclared flag", passed, false, true},
		{"failing beats a declared flag", failed, true, false},
		{"no report, declared", nil, true, true},
		{"no report, not declared", nil, false, false},
		{"only an error, declared stands", errored, true, true},
		{"only an error, not declared", errored, false, false},
	} {
		if got, _ := Effective(tc.r, CapTools, tc.declared); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestMergeReports_AnOutageKeepsTheEarlierVerdict(t *testing.T) {
	prev := &Report{ModelRoute: "m", Checks: []Check{
		{Capability: CapTools, Status: StatusPassed, Detail: "3 of 3"},
		{Capability: CapVision, Status: StatusFailed, Detail: "wrong colours"},
	}, Metadata: &MetadataReport{ContextWindow: 32768, Source: "x"}}
	next := &Report{ModelRoute: "m", Checks: []Check{
		{Capability: CapTools, Status: StatusError, Detail: "503"},
		{Capability: CapVision, Status: StatusPassed, Detail: "now fixed"},
	}, Metadata: &MetadataReport{Error: "unreachable"}}
	got := MergeReports(prev, next)
	if got.Check(CapTools).Status != StatusPassed || !strings.Contains(got.Check(CapTools).Detail, "could not run") {
		t.Errorf("tools: %+v", got.Check(CapTools))
	}
	if got.Check(CapVision).Status != StatusPassed {
		t.Errorf("a fresh conclusive result must replace the old one: %+v", got.Check(CapVision))
	}
	if got.Metadata == nil || got.Metadata.ContextWindow != 32768 {
		t.Errorf("a failed metadata lookup must not erase the earlier reading: %+v", got.Metadata)
	}
}

// A reasoning model that is cut off while still thinking has not shown it cannot
// call a tool. Marking it failed would lock a capable model out of building.
func TestRunner_ACutOffReplyIsNotEvidenceAgainstTheCapability(t *testing.T) {
	for _, c := range []Capability{CapTools, CapVision, CapAudio} {
		got := runner(&fakeModel{truncated: true}).RunCapability(context.Background(), c)
		if got.Status != StatusError || !strings.Contains(got.Detail, "ran out of output tokens") {
			t.Errorf("%s: %s (%s), want error", c, got.Status, got.Detail)
		}
	}
}

// A router that answers plain requests but errors whenever tools are included
// does not support tools. Reading that as an outage would leave the question
// open forever; a real outage (everything fails) still stays inconclusive.
func TestRunner_ErrorsThatOnlyHappenWithToolsCountAsFailing(t *testing.T) {
	got := runner(&fakeModel{toolsBreak: true}).RunCapability(context.Background(), CapTools)
	if got.Status != StatusFailed || !strings.Contains(got.Detail, "plain request but errors when tools are included") {
		t.Errorf("got %s: %q", got.Status, got.Detail)
	}
	got = runner(&fakeModel{outage: errors.New("503")}).RunCapability(context.Background(), CapTools)
	if got.Status != StatusError {
		t.Errorf("a total outage must stay inconclusive: %s (%s)", got.Status, got.Detail)
	}
}
