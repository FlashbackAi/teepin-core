// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"context"
	"fmt"

	"github.com/FlashbackAi/teepin-core/pkg/inference"
)

// Kumbha has no model registry of its own. Which models the build agent may
// use, and in what order, is a property of each model in the platform's one
// catalog (pkg/modelcatalog: kumbha_enabled / kumbha_priority), and every
// completion is served through Teepin Inference's gateway — the same path a
// customer's own API call takes. ModelBackend is that seam.

// Model is one model the build agent may use.
type Model struct {
	// Route is the model's catalog model_route — what the backend is asked
	// for. Never shown to a customer.
	Route string
	// Engine identifies the serving backend in usage records' provider
	// column, the one place backend identity is recorded (for the admin
	// margin view).
	Engine string
}

// ModelBackend lists Kumbha's models and serves completions against them.
// Implemented in production by an adapter over modelcatalog and
// inferencegateway (cmd/api-server/adapters.go), which keeps this package
// free of both.
type ModelBackend interface {
	// KumbhaModels returns the models backing ONE alias, in the order they
	// should be tried. Before the alias parameter existed (migration 055),
	// every alias silently resolved to the same list — see
	// pkg/modelcatalog.Service.ListKumbhaModels's own doc comment.
	KumbhaModels(ctx context.Context, alias string) ([]Model, error)
	// Complete serves req against the catalog model named by req.Model.
	Complete(ctx context.Context, accountID string, req inference.Request) (*inference.Response, error)
}

// DefaultModelAlias is the model name the agent image addresses Kumbha by
// absent an explicit customer choice.
//
// Kumbha's aliases are not models: whichever alias a request names, it is
// served by that alias's own catalog models in priority order (an
// operator-controlled list — pkg/modelcatalog's kumbha_alias column, not
// something a customer's choice bypasses). "teepin/deep" is still accepted
// so an agent image or harness configured with the old frontier-route name
// keeps working. "teepin/confidential" is the one alias with a real,
// customer-visible meaning today (hardware-attested inference, chosen
// explicitly in the Kumbha composer) rather than an internal fast/deep
// distinction the catalog alone decides.
const DefaultModelAlias = "teepin/fast"

// ModelAliases is every alias a session may be created with, and the only
// values TEEPIN_ROUTE (the agent pod env var) is ever set to. Exported so
// the API layer can validate a customer-supplied choice against the exact
// same set this package resolves, rather than duplicating the list.
var ModelAliases = []string{DefaultModelAlias, "teepin/deep", "teepin/confidential"}

func isModelAlias(name string) bool {
	for _, a := range ModelAliases {
		if name == a {
			return true
		}
	}
	return false
}

// StaticModel is one entry of a StaticModels backend.
type StaticModel struct {
	Route    string
	Engine   string
	Provider inference.Provider
}

// StaticModels is a fixed, in-memory ModelBackend: its models are tried in
// slice order, each served by its own Provider. For tests and for running
// Kumbha without a model catalog.
type StaticModels []StaticModel

// KumbhaModels ignores alias: StaticModels is a fixed test/no-catalog
// fallback with no per-alias tagging of its own, so it serves every alias
// identically — the pre-migration-055 behavior, acceptable here since this
// backend exists for tests and running without a real catalog, not for
// exercising alias-specific routing.
func (s StaticModels) KumbhaModels(_ context.Context, _ string) ([]Model, error) {
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
