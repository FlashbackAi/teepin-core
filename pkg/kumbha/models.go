// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"context"
	"fmt"

	"github.com/FlashbackAi/teepin-core/pkg/inference"
)

// Kumbha has no model registry of its own. Which models the build agent may
// use is a property of each model in the platform's one catalog
// (pkg/modelcatalog: kumbha_enabled), and every completion is served
// through Teepin Inference's gateway — the same path a customer's own API
// call takes. ModelBackend is that seam.
//
// A customer picks one of these models DIRECTLY, by its real route — there
// is no alias/tier bucket sitting in front of them. An earlier design
// (migration 055's kumbha_alias — "teepin/fast" / "teepin/deep" /
// "teepin/confidential") modeled "which bucket" as one mutually-exclusive
// category per model, which does not match reality: a model can be BOTH
// tool-capable and confidential, or neither, and forcing it into exactly
// one of three buckets either hid a real gap (a bucket with no
// tool-capable model in it, silently unusable by the build agent) or
// created a real security regression — a bucket with more than one
// backing model could fail over from a customer's deliberately-chosen
// confidential model to a DIFFERENT, non-confidential one, silently.
// Direct selection with per-model badges (Confidential, Self-hosted vs
// third-party — both computed from Provider, never stored separately)
// avoids both problems: a customer sees exactly what they are choosing,
// and a failure surfaces as an error on THAT model, never a silent switch
// to one with different guarantees.

// Model is one model the build agent may use, listed to a customer so they
// can choose it directly by name.
type Model struct {
	// Route is the model's catalog model_route — what a customer's choice,
	// and every completion request for that session, names directly.
	Route       string
	DisplayName string
	// Engine identifies the serving backend in usage records' provider
	// column, the one place backend identity is recorded (for the admin
	// margin view) — never shown to a customer.
	Engine string
	// Confidential mirrors modelcatalog.ProviderTinfoilConfidential — a
	// hardware-attested enclave, not a claim Teepin or a plain third party
	// makes about itself.
	Confidential bool
	// SelfHosted mirrors modelcatalog.ProviderNode — false means a
	// third-party API Teepin calls directly (a "vendor-hosted" badge).
	SelfHosted            bool
	SupportsTools         bool
	SupportsVision        bool
	SupportsAudio         bool
	InputPricePerMillion  float64
	OutputPricePerMillion float64
}

// ModelBackend lists Kumbha's models and serves completions against them.
// Implemented in production by an adapter over modelcatalog and
// inferencegateway (cmd/api-server/adapters.go), which keeps this package
// free of both.
type ModelBackend interface {
	// KumbhaModels returns every model the build agent may currently be
	// asked for — for a customer-facing picker. Order is catalog
	// kumbha_priority (display/default order), never a failover sequence:
	// a customer's choice is exact and is never silently substituted for a
	// different model.
	KumbhaModels(ctx context.Context) ([]Model, error)
	// Complete serves req against the catalog model named by req.Model.
	Complete(ctx context.Context, accountID string, req inference.Request) (*inference.Response, error)
}

// StaticModel is one entry of a StaticModels backend.
type StaticModel struct {
	Route    string
	Engine   string
	Provider inference.Provider
}

// StaticModels is a fixed, in-memory ModelBackend: its models are listed in
// slice order, each served by its own Provider. For tests and for running
// Kumbha without a model catalog.
type StaticModels []StaticModel

func (s StaticModels) KumbhaModels(context.Context) ([]Model, error) {
	out := make([]Model, 0, len(s))
	for _, m := range s {
		out = append(out, Model{Route: m.Route, Engine: m.Engine})
	}
	return out, nil
}

func (s StaticModels) Complete(ctx context.Context, _ string, req inference.Request) (*inference.Response, error) {
	for _, m := range s {
		if m.Route == req.Model && m.Provider != nil {
			return m.Provider.Complete(ctx, req)
		}
	}
	return nil, fmt.Errorf("%w: %q", inference.ErrUnknownModel, req.Model)
}
