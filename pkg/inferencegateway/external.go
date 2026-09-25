// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package inferencegateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/FlashbackAi/teepin-core/pkg/inference"
	"github.com/FlashbackAi/teepin-core/pkg/modelcatalog"
)

// External (third-party API) models: built straight from their catalog row
// plus their API key, so registering, editing, or rotating the key of one
// in Control Center takes effect on the next request — no redeploy, no
// boot-time wiring in main.go.

// externalFactory builds a Provider for an external catalog model. A field
// on Gateway rather than a direct call, so tests can substitute a fake.
type externalFactory func(m modelcatalog.Model, apiKey string) (inference.Provider, error)

func newExternalProvider(m modelcatalog.Model, apiKey string) (inference.Provider, error) {
	switch m.Provider {
	case modelcatalog.ProviderAnthropic:
		return inference.NewAnthropic(inference.AnthropicConfig{
			Model:           m.ProviderModel,
			APIKey:          apiKey,
			ContextWindow:   m.ContextWindow,
			MaxOutputTokens: m.MaxOutputTokens,
		}), nil
	case modelcatalog.ProviderOpenAICompatible:
		return inference.NewVLLM(inference.VLLMConfig{
			BaseURL:       m.BaseURL,
			Model:         m.ProviderModel,
			APIKey:        apiKey,
			ContextWindow: m.ContextWindow,
			SupportsTools: m.SupportsTools,
		}), nil
	case modelcatalog.ProviderTinfoilConfidential:
		return inference.NewTinfoilConfidential(inference.TinfoilConfidentialConfig{
			Enclave:       m.BaseURL,
			Model:         m.ProviderModel,
			APIKey:        apiKey,
			ContextWindow: m.ContextWindow,
		})
	default:
		return nil, fmt.Errorf("model %q is not served by an external provider", m.ModelRoute)
	}
}

// SetSecrets enables API keys for external models: a model with an
// APIKeyRef reads its key from secrets, under environment's prefix (see
// SecretName). Without it, external models are built with no key — the
// Anthropic SDK then falls back to its own ANTHROPIC_API_KEY resolution.
func (g *Gateway) SetSecrets(secrets SecretsClient, environment string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.secrets = secrets
	g.environment = environment
}

// externalProviderFor returns a cached Provider for an external model,
// rebuilding it when its config or API key changed since the last build.
func (g *Gateway) externalProviderFor(ctx context.Context, m modelcatalog.Model) (inference.Provider, error) {
	g.mu.Lock()
	secrets, environment := g.secrets, g.environment
	g.mu.Unlock()

	apiKey := ""
	if m.APIKeyRef != "" && secrets != nil {
		v, ok, err := secrets.Get(ctx, SecretName(environment, m.APIKeyRef))
		if err != nil {
			return nil, fmt.Errorf("%w: reading API key for %q: %v", inference.ErrProviderUnavailable, m.ModelRoute, err)
		}
		if ok {
			apiKey = v
		}
	}

	hash := externalHash(m, apiKey)
	g.mu.Lock()
	if cached, ok := g.externals[m.ModelRoute]; ok && cached.configHash == hash {
		g.mu.Unlock()
		return cached.provider, nil
	}
	g.mu.Unlock()

	p, err := g.newExternal(m, apiKey)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.externals[m.ModelRoute] = cachedProvider{configHash: hash, provider: p}
	g.mu.Unlock()
	return p, nil
}

// externalHash covers every field that changes how the Provider behaves or
// authenticates. The key is hashed so it never appears in the clear here.
func externalHash(m modelcatalog.Model, apiKey string) string {
	keySum := sha256.Sum256([]byte(apiKey))
	return fmt.Sprintf("%s|%s|%s|%d|%d|%t|%s",
		m.Provider, m.ProviderModel, m.BaseURL, m.ContextWindow, m.MaxOutputTokens,
		m.SupportsTools, hex.EncodeToString(keySum[:]))
}

// ModelState is whether a model can actually be served right now.
type ModelState string

const (
	// StateServing: a self-hosted model with at least one live mount, or an
	// external model whose last health check passed.
	StateServing ModelState = "serving"
	// StateNoBackend: a self-hosted model with nothing mounted right now.
	StateNoBackend ModelState = "no_backend"
	// StateUnhealthy: an external model whose last health check failed.
	StateUnhealthy ModelState = "unhealthy"
	// StateUnknown: not checked yet, or the provider has no health check —
	// never reported as unhealthy, since silence is not a failure.
	StateUnknown ModelState = "unknown"
)

// ModelStatus is a model's servability, for the operator's catalog view.
type ModelStatus struct {
	State     ModelState `json:"state"`
	Detail    string     `json:"detail,omitempty"`
	CheckedAt *time.Time `json:"checked_at,omitempty"`
}

// Status reports whether m can be served right now. Self-hosted models are
// checked live against node_services; external models report their last
// cached health check (see StartHealthChecks), so listing the catalog
// never waits on a third-party API.
func (g *Gateway) Status(ctx context.Context, m modelcatalog.Model) ModelStatus {
	if m.Provider.IsExternal() {
		g.mu.Lock()
		st, ok := g.health[m.ModelRoute]
		g.mu.Unlock()
		if !ok {
			return ModelStatus{State: StateUnknown}
		}
		return st
	}
	candidates, err := g.candidatesFor(ctx, m.ModelRoute)
	if err != nil {
		return ModelStatus{State: StateUnknown, Detail: err.Error()}
	}
	if len(candidates) == 0 {
		return ModelStatus{State: StateNoBackend, Detail: "not mounted on any node"}
	}
	return ModelStatus{State: StateServing, Detail: fmt.Sprintf("%d mounted backend(s)", len(candidates))}
}

const healthCheckTimeout = 10 * time.Second

// CheckExternalHealth runs one round of health checks over every enabled
// external model EXCEPT confidential-inference ones, sequentially — the
// count is small. Checks are metadata calls (inference.HealthChecker),
// never completions, so they cost no tokens. Confidential models
// (ProviderTinfoilConfidential) are deliberately excluded here and checked
// instead by CheckConfidentialAttestation on its own, independent, faster
// cadence (see StartConfidentialAttestationChecks) — this general interval
// is meant to be tunable for noise/cost reasons on ordinary vendor APIs
// without that also slowing down how quickly a broken attestation guarantee
// gets caught.
func (g *Gateway) CheckExternalHealth(ctx context.Context) {
	models, err := g.catalog.ListModels(ctx)
	if err != nil {
		log.Printf("WARN: model health check could not list the catalog: %v", err)
		return
	}
	for _, m := range models {
		if !m.Enabled || !m.Provider.IsExternal() || m.Provider == modelcatalog.ProviderTinfoilConfidential {
			continue
		}
		g.setHealth(m.ModelRoute, g.checkOne(ctx, m))
	}
}

// CheckConfidentialAttestation runs one round of attestation checks over
// every enabled confidential-inference model. Split from CheckExternalHealth
// so its cadence (see StartConfidentialAttestationChecks) is never coupled
// to whatever interval an operator sets for ordinary external-model health
// checks — this one reads an already-held, locally-cached verification
// state (see TinfoilConfidentialProvider.CheckHealth's own doc comment), so
// it costs nothing to run often, and a confidentiality guarantee silently
// breaking deserves to be caught fast regardless of how noisy or expensive
// checking some other vendor's API is judged to be.
func (g *Gateway) CheckConfidentialAttestation(ctx context.Context) {
	models, err := g.catalog.ListModels(ctx)
	if err != nil {
		log.Printf("WARN: confidential attestation check could not list the catalog: %v", err)
		return
	}
	for _, m := range models {
		if !m.Enabled || m.Provider != modelcatalog.ProviderTinfoilConfidential {
			continue
		}
		g.setHealth(m.ModelRoute, g.checkOne(ctx, m))
	}
}

func (g *Gateway) checkOne(ctx context.Context, m modelcatalog.Model) ModelStatus {
	now := time.Now()
	p, err := g.externalProviderFor(ctx, m)
	if err != nil {
		g.logAttestationFailureIfConfidential(m, err)
		return ModelStatus{State: StateUnhealthy, Detail: err.Error(), CheckedAt: &now}
	}
	checker, ok := p.(inference.HealthChecker)
	if !ok {
		return ModelStatus{State: StateUnknown, CheckedAt: &now}
	}
	checkCtx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()
	if err := checker.CheckHealth(checkCtx); err != nil {
		g.logAttestationFailureIfConfidential(m, err)
		return ModelStatus{State: StateUnhealthy, Detail: err.Error(), CheckedAt: &now}
	}
	return ModelStatus{State: StateServing, CheckedAt: &now}
}

// logAttestationFailureIfConfidential emits a distinct, loudly-grep-able
// ERROR line when the model that just failed its periodic health check is
// confidential-inference (ProviderTinfoilConfidential — currently the only
// Provider that promises hardware attestation at all) — as opposed to every
// other external-model health failure, which stays unlogged here exactly as
// before (a plain vLLM/Anthropic hiccup is ordinary and already visible via
// the admin catalog's cached status). A confidentiality guarantee silently
// failing is not ordinary: this is the signal an operator's log-based
// alerting (Loki/CloudWatch) should watch for, since a customer relying on
// this model's attestation would otherwise have no way to know it just
// stopped holding. Filters on the catalog's own Provider field, not a type
// assertion on the constructed provider, so this fires even when
// construction itself failed (externalProviderFor's error case above) — an
// unreachable enclave is at least as serious as one whose CheckHealth
// merely reports unverified.
func (g *Gateway) logAttestationFailureIfConfidential(m modelcatalog.Model, checkErr error) {
	if m.Provider != modelcatalog.ProviderTinfoilConfidential {
		return
	}
	log.Printf("ERROR: confidential inference attestation check failed for %q (provider=%s): %v — this model may no longer be serving verified confidential inference",
		m.ModelRoute, m.Provider, checkErr)
}

// ErrAttestationNotSupported means m's provider does not implement
// inference.AttestationReporter — not a failure to alert on, just "this
// model has nothing to prove" (a plain vLLM or Anthropic-backed model).
// Distinct from a resolution/attestation error, which means the model DOES
// claim to be attested but that claim currently can't be substantiated.
var ErrAttestationNotSupported = errors.New("this model does not provide an attestation proof")

// Attestation returns the live attestation proof for an external model that
// implements inference.AttestationReporter (currently only
// ProviderTinfoilConfidential) — the customer-facing "verify it yourself"
// surface. Mirrors Status/checkOne's own resolve-then-type-assert shape.
func (g *Gateway) Attestation(ctx context.Context, m modelcatalog.Model) (any, error) {
	if !m.Provider.IsExternal() {
		return nil, ErrAttestationNotSupported
	}
	p, err := g.externalProviderFor(ctx, m)
	if err != nil {
		return nil, err
	}
	reporter, ok := p.(inference.AttestationReporter)
	if !ok {
		return nil, ErrAttestationNotSupported
	}
	return reporter.Attestation()
}

func (g *Gateway) setHealth(route string, st ModelStatus) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.health[route] = st
}

// StartHealthChecks checks external models (excluding confidential-
// inference ones — see CheckExternalHealth) immediately, then every
// interval until ctx is cancelled. Run it with `go`.
func (g *Gateway) StartHealthChecks(ctx context.Context, interval time.Duration) {
	g.CheckExternalHealth(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			g.CheckExternalHealth(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// StartConfidentialAttestationChecks checks confidential-inference models'
// attestation immediately, then every interval until ctx is cancelled — a
// separate loop from StartHealthChecks, deliberately, so this interval
// stays fast regardless of whatever an operator sets the general external-
// model health-check interval to (see CheckConfidentialAttestation's own
// doc comment for why a fast interval here costs nothing). Run it with `go`.
func (g *Gateway) StartConfidentialAttestationChecks(ctx context.Context, interval time.Duration) {
	g.CheckConfidentialAttestation(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			g.CheckConfidentialAttestation(ctx)
		case <-ctx.Done():
			return
		}
	}
}
