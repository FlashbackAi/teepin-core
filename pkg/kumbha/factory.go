// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/inference"
)

// cachedCandidateProvider pairs a built Provider with the config+secret
// hash it was built from, so ProviderFactory.Build can tell whether an
// already-constructed Provider is still current — mirrors
// inferencegateway.Gateway's own providerFor/configHash pattern exactly.
type cachedCandidateProvider struct {
	hash     string
	provider inference.Provider
}

// ProviderFactory builds inference.Provider instances from RouteCandidate
// rows, fetching each candidate's API key live from Secrets Manager
// (secrets may be nil, meaning "no secrets configured" — every candidate
// is then built with an empty key, same as an unauthenticated vLLM
// endpoint today) and caching the result per candidate id until its
// config or secret actually changes. This is what makes editing a
// candidate's base_url/model/priority — or rotating its key — from
// Control Center take effect on the very next request, with no redeploy
// and no process restart.
type ProviderFactory struct {
	secrets     SecretsClient
	environment string

	mu    sync.Mutex
	cache map[uuid.UUID]cachedCandidateProvider
}

func NewProviderFactory(secrets SecretsClient, environment string) *ProviderFactory {
	return &ProviderFactory{secrets: secrets, environment: environment, cache: map[uuid.UUID]cachedCandidateProvider{}}
}

// Build returns a Provider for the candidate, rebuilding it only if this is
// the first request since the candidate was created or its config/secret
// changed since the last build.
func (f *ProviderFactory) Build(ctx context.Context, c RouteCandidate) (inference.Provider, error) {
	secretValue := ""
	if c.HasSecret && f.secrets != nil {
		v, ok, err := f.secrets.Get(ctx, CandidateSecretName(f.environment, c.ID))
		if err != nil {
			return nil, fmt.Errorf("failed to read secret for candidate %s: %w", c.ID, err)
		}
		if ok {
			secretValue = v
		}
	}

	hash := candidateHash(c, secretValue)

	f.mu.Lock()
	if cached, ok := f.cache[c.ID]; ok && cached.hash == hash {
		f.mu.Unlock()
		return cached.provider, nil
	}
	f.mu.Unlock()

	provider, err := newCandidateProvider(c, secretValue)
	if err != nil {
		return nil, err
	}

	f.mu.Lock()
	f.cache[c.ID] = cachedCandidateProvider{hash: hash, provider: provider}
	f.mu.Unlock()
	return provider, nil
}

func newCandidateProvider(c RouteCandidate, apiKey string) (inference.Provider, error) {
	switch c.ProviderType {
	case "vllm":
		return inference.NewVLLM(inference.VLLMConfig{
			BaseURL:       c.BaseURL,
			Model:         c.Model,
			APIKey:        apiKey,
			ContextWindow: c.ContextWindow,
			SupportsTools: c.SupportsTools,
		}), nil
	case "anthropic":
		return inference.NewAnthropic(inference.AnthropicConfig{
			Model:           c.Model,
			APIKey:          apiKey,
			ContextWindow:   c.ContextWindow,
			MaxOutputTokens: c.MaxOutputTokens,
		}), nil
	default:
		return nil, fmt.Errorf("unknown provider_type %q for candidate %s", c.ProviderType, c.ID)
	}
}

// candidateHash includes every field that changes how the Provider
// behaves or authenticates, so any edit from Control Center — including a
// secret rotation the SecretsClient just picked up — invalidates the
// cache instead of silently keeping a stale client around. The secret
// value is hashed rather than included in the clear, purely so it never
// appears in a debugger/log of this string.
func candidateHash(c RouteCandidate, secretValue string) string {
	secretSum := sha256.Sum256([]byte(secretValue))
	return fmt.Sprintf("%s|%s|%s|%d|%t|%d|%s",
		c.ProviderType, c.BaseURL, c.Model, c.ContextWindow, c.SupportsTools,
		c.MaxOutputTokens, hex.EncodeToString(secretSum[:]))
}
