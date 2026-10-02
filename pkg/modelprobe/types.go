// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Package modelprobe finds out what a model can actually do, instead of
// trusting a checkbox. It asks the backend what it knows about the model
// (free metadata: context window, output limit) and then tests each
// capability by really using it through the same provider code real traffic
// goes through: a tool call that must come back well formed, an image whose
// colours must be named, a sound whose beeps must be counted.
//
// The results are evidence kept per model. What a model is allowed to do (a
// builder needs working tool calls) follows the evidence where there is
// some, and the operator's declaration only where there is none.
package modelprobe

import (
	"time"
)

// Capability is one thing a model may be able to do.
type Capability string

const (
	// CapTools: the model returns well-formed native tool calls, picks the
	// right tool from several, and uses a tool's result in its next answer.
	CapTools Capability = "tools"
	// CapToolsMany: the same, with the builder's twenty-plus tool schemas in
	// the request. Informational: a model can pass the plain test and still
	// lose the thread with that many tools.
	CapToolsMany Capability = "tools_many"
	// CapToolsLists: a native tool call can carry a list of objects as an
	// argument. GLM-5.3's passed every other tool check while every list of
	// objects in its builds arrived empty ("resources": [], "options": []),
	// 2026-10-02. A failure moves building to the text protocol when that
	// works; otherwise native stays, since the builder's own tools avoid
	// lists of objects.
	CapToolsLists Capability = "tools_lists"
	// CapVision: the model reads an image.
	CapVision Capability = "vision"
	// CapAudio: the model hears a sound clip.
	CapAudio Capability = "audio"
)

// AllCapabilities is every capability the prober tests, in report order.
var AllCapabilities = []Capability{CapTools, CapToolsMany, CapToolsLists, CapBuild, CapBuildText, CapVision, CapAudio}

// Status is the outcome of testing one capability.
type Status string

const (
	// StatusPassed: enough attempts did the thing.
	StatusPassed Status = "passed"
	// StatusFailed: the backend answered and did not do the thing (or refused
	// the request outright). This is real evidence against the capability.
	StatusFailed Status = "failed"
	// StatusError: the test could not be run (backend down, key missing). No
	// evidence either way; it never replaces an earlier verdict.
	StatusError Status = "error"
	// StatusUntested: not tested yet.
	StatusUntested Status = "untested"
)

// Check is the result of testing one capability.
type Check struct {
	Capability Capability `json:"capability"`
	Status     Status     `json:"status"`
	// Attempts is how many times the test ran; Passed how many of those did
	// the thing. A capability passes when most attempts do, because a model's
	// answer is not deterministic.
	Attempts int `json:"attempts"`
	Passed   int `json:"passed"`
	// Detail says what was seen, short and safe to show an operator: which
	// step failed, or the backend's refusal.
	Detail    string    `json:"detail,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// MetadataReport is what the backend said about the model without being asked
// to generate anything.
type MetadataReport struct {
	ContextWindow   int    `json:"context_window,omitempty"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
	Vision          *bool  `json:"vision,omitempty"`
	Source          string `json:"source,omitempty"`
	// Error is set when the lookup was tried and failed.
	Error string `json:"error,omitempty"`
}

// Report is everything known about one model's capabilities.
type Report struct {
	ModelRoute string          `json:"model_route"`
	RanAt      time.Time       `json:"ran_at"`
	Metadata   *MetadataReport `json:"metadata,omitempty"`
	Checks     []Check         `json:"checks"`
}

// Check returns the check for c, or an untested one.
func (r *Report) Check(c Capability) Check {
	if r != nil {
		for _, ch := range r.Checks {
			if ch.Capability == c {
				return ch
			}
		}
	}
	return Check{Capability: c, Status: StatusUntested}
}

// Effective answers "may this model be treated as having capability c?".
// Evidence wins: a passed check means yes and a failed one means no, whatever
// was declared. With no conclusive evidence the operator's declaration stands,
// so a model registered before probing existed is not locked out, and one
// declared without a capability is not assumed to have it.
func Effective(r *Report, c Capability, declared bool) (ok bool, basis string) {
	switch r.Check(c).Status {
	case StatusPassed:
		return true, "verified"
	case StatusFailed:
		return false, "failed its check"
	default:
		if declared {
			return true, "declared, not yet verified"
		}
		return false, "not declared"
	}
}

// ToolMode is how the build agent talks to a model about tools.
type ToolMode string

const (
	// ToolModeNative sends tools as structured tool definitions and reads
	// structured calls back.
	ToolModeNative ToolMode = "native"
	// ToolModeText describes the tools in the prompt and reads function calls
	// out of the reply, for a model whose native tool calls do not work.
	ToolModeText ToolMode = "text"
)

// BuildMode decides how (and whether) a model can be used for building, from the
// evidence, falling back to the operator's declaration where there is none.
//
//   - native tool calls that pass both the single-step test and the build task:
//     native, unless they lose lists of objects and the text protocol passes
//     its build task, in which case text;
//   - otherwise the text protocol passing the build task: text;
//   - with no conclusive evidence, a declared-tools model keeps working natively
//     (a model registered before checks existed is not locked out);
//   - anything else cannot build.
func BuildMode(rep *Report, declaredTools bool) (mode ToolMode, ok bool) {
	tools, build, text := rep.Check(CapTools).Status, rep.Check(CapBuild).Status, rep.Check(CapBuildText).Status
	nativeBroken := tools == StatusFailed || build == StatusFailed
	switch {
	case tools == StatusPassed && !nativeBroken:
		// Lost lists of objects are a reason to prefer text only when text is
		// proven to work: native still builds, because the builder's tools take
		// plain fields, and a model must never be locked out on this alone.
		if rep.Check(CapToolsLists).Status == StatusFailed && text == StatusPassed {
			return ToolModeText, true
		}
		return ToolModeNative, true
	case text == StatusPassed:
		return ToolModeText, true
	case !nativeBroken && tools != StatusPassed && declaredTools:
		return ToolModeNative, true
	}
	return "", false
}

// NotBuildCapable is why a model that cannot call tools, natively or by text, is
// not offered for builds. Phrased for a customer.
const NotBuildCapable = "Can't use tools, which building needs"

// BuilderView is what the build agent may treat a model as able to do.
type BuilderView struct {
	// Tools: the model can be used for building at all, in some ToolMode.
	Tools         bool
	Mode          ToolMode
	Vision, Audio bool
	// Unavailable is why the model is not offered for building ("" when it is):
	// a reason already found (an outage) first, else that it cannot call tools.
	Unavailable string
}

// Builder is the single rule behind both the builder's model list and the
// picker's, so they cannot disagree. A model that cannot use tools in any mode
// is unavailable for building whatever else is true of it, because the builder
// acts only through tool calls.
func Builder(supportsTools, supportsVision, supportsAudio bool, rep *Report, unavailable string) BuilderView {
	mode, ok := BuildMode(rep, supportsTools)
	vision, _ := Effective(rep, CapVision, supportsVision)
	audio, _ := Effective(rep, CapAudio, supportsAudio)
	if unavailable == "" && !ok {
		unavailable = NotBuildCapable
	}
	return BuilderView{Tools: ok, Mode: mode, Vision: vision, Audio: audio, Unavailable: unavailable}
}
