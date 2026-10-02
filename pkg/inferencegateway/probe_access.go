// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package inferencegateway

import (
	"context"
	"fmt"

	"github.com/FlashbackAi/teepin-core/pkg/inference"
	"github.com/FlashbackAi/teepin-core/pkg/modelcatalog"
)

// ProviderFor returns the provider real traffic would use for a saved model,
// without taking a concurrency slot or touching any account: the capability
// prober (pkg/modelprobe) tests the same code path a customer's request takes,
// as the platform rather than as a customer.
func (g *Gateway) ProviderFor(ctx context.Context, m modelcatalog.Model) (inference.Provider, error) {
	if m.Provider.IsExternal() {
		return g.externalProviderFor(ctx, m)
	}
	candidates, err := g.candidatesFor(ctx, m.ModelRoute)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("%w: no backend currently mounted for %q", inference.ErrProviderUnavailable, m.ModelRoute)
	}
	ns, cfg := g.pickLeastLoaded(candidates)
	if ns.ObservedEndpoint == nil {
		return nil, fmt.Errorf("%w: %q has no reachable endpoint yet", inference.ErrProviderUnavailable, m.ModelRoute)
	}
	return g.providerFor(ns.ID, cfg, *ns.ObservedEndpoint)
}

// BuildProvider builds a provider for an external model that is not saved yet,
// using the API key just typed, so a registration form can ask the backend
// about the model before anything is stored.
func (g *Gateway) BuildProvider(m modelcatalog.Model, apiKey string) (inference.Provider, error) {
	if !m.Provider.IsExternal() {
		return nil, fmt.Errorf("only an external model can be described before it is saved")
	}
	return g.newExternal(m, apiKey)
}
