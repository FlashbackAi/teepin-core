// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"context"
	"errors"
	"testing"

	"github.com/FlashbackAi/teepin-core/pkg/inference"
)

// fakeProvider is a canned inference.Provider: it returns err when set,
// otherwise a response carrying usage.
type fakeProvider struct {
	name  string
	err   error
	usage inference.Usage
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

func TestStaticModels_ListsInOrderAndServesByRoute(t *testing.T) {
	models := StaticModels{
		{Route: "a", Engine: "vllm", Provider: &fakeProvider{name: "first"}},
		{Route: "b", Engine: "anthropic", Provider: &fakeProvider{name: "second"}},
	}

	listed, err := models.KumbhaModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0] != (Model{Route: "a", Engine: "vllm"}) || listed[1].Route != "b" {
		t.Errorf("KumbhaModels = %+v, want a then b", listed)
	}

	resp, err := models.Complete(context.Background(), "acct", inference.Request{Model: "b"})
	if err != nil || resp.Model != "second" {
		t.Errorf("Complete(b) = %v, %v; want the second provider's response", resp, err)
	}
	if _, err := models.Complete(context.Background(), "acct", inference.Request{Model: "missing"}); !errors.Is(err, inference.ErrUnknownModel) {
		t.Errorf("Complete(missing) = %v, want ErrUnknownModel", err)
	}
}

func TestIsModelAlias(t *testing.T) {
	for name, want := range map[string]bool{
		"teepin/fast":                true,
		"teepin/deep":                true,
		"anthropic/claude-haiku-4-5": false,
		"":                           false,
	} {
		if got := isModelAlias(name); got != want {
			t.Errorf("isModelAlias(%q) = %v, want %v", name, got, want)
		}
	}
}
