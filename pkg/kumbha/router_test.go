// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"context"
	"errors"
	"testing"

	"github.com/FlashbackAi/teepin-core/pkg/inference"
)

// fakeProvider is shared by router_test.go and gateway_test.go. usage/err
// are zero-valued by default, which reproduces the router tests' original
// behaviour (a bare completion with no usage, never an error).
type fakeProvider struct {
	name  string
	usage inference.Usage
	err   error
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Complete(context.Context, inference.Request) (*inference.Response, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &inference.Response{Model: f.name, Usage: f.usage}, nil
}
func (f *fakeProvider) Stream(context.Context, inference.Request, func(inference.Chunk) error) error {
	return nil
}
func (f *fakeProvider) Capabilities() inference.Capabilities { return inference.Capabilities{} }

func TestRouter_Resolve_KnownRouteReturnsItsProvider(t *testing.T) {
	fast := &fakeProvider{name: "vllm"}
	r := NewRouter(map[string]Route{
		"teepin/fast": {Provider: fast, ProviderName: "vllm"},
	})

	route, err := r.Resolve("teepin/fast")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if route.Provider != fast || route.ProviderName != "vllm" {
		t.Errorf("got %+v, want the registered fast route", route)
	}
}

func TestRouter_Resolve_UnknownModelIsErrUnknownModel(t *testing.T) {
	r := NewRouter(map[string]Route{
		"teepin/fast": {Provider: &fakeProvider{name: "vllm"}, ProviderName: "vllm"},
	})

	_, err := r.Resolve("teepin/deep")
	if !errors.Is(err, inference.ErrUnknownModel) {
		t.Errorf("got %v, want inference.ErrUnknownModel", err)
	}
}

func TestRouter_Resolve_NilProviderInMapIsTreatedAsUnconfigured(t *testing.T) {
	// A route entry can exist with a nil Provider when main.go registers
	// "teepin/deep" but no Anthropic key was configured — Resolve must
	// refuse cleanly rather than dispatching to a nil Provider and
	// panicking.
	r := NewRouter(map[string]Route{
		"teepin/deep": {Provider: nil, ProviderName: "anthropic"},
	})

	_, err := r.Resolve("teepin/deep")
	if !errors.Is(err, inference.ErrUnknownModel) {
		t.Errorf("got %v, want inference.ErrUnknownModel for an unconfigured route", err)
	}
}
