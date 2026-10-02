// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package inference

import (
	"context"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
)

// Metadata is what a backend says about a model, read without generating a
// token. It is a hint to prefill the catalog and to compare against what an
// operator declared; it is never taken as proof that a capability works
// through the gateway (the capability probes in pkg/modelprobe do that).
type Metadata struct {
	// ContextWindow and MaxOutputTokens are 0 when the backend did not say.
	ContextWindow   int
	MaxOutputTokens int
	// Vision reports image input when the backend states it; nil when it says
	// nothing either way.
	Vision *bool
	// Source names where the figures came from, for the operator's screen.
	Source string
}

// MetadataSource is implemented by providers that can describe a model for
// free (a metadata call, not a completion).
type MetadataSource interface {
	DiscoverMetadata(ctx context.Context) (Metadata, error)
}

// DiscoverMetadata reads the served model's limits from the OpenAI-compatible
// /v1/models endpoint (vLLM reports max_model_len there).
func (p *VLLMProvider) DiscoverMetadata(ctx context.Context) (Metadata, error) {
	models, err := fetchModels(ctx, p.baseURL, p.apiKey)
	if err != nil {
		return Metadata{}, err
	}
	for _, m := range models {
		if m.ID == p.model {
			return Metadata{ContextWindow: m.MaxModelLen, Source: p.baseURL + "/v1/models"}, nil
		}
	}
	return Metadata{}, fmt.Errorf("model %q is not listed by %s/v1/models", p.model, p.baseURL)
}

// DiscoverMetadata reads the model's limits and image support from
// Anthropic's models API.
func (p *AnthropicProvider) DiscoverMetadata(ctx context.Context) (Metadata, error) {
	info, err := p.client.Models.Get(ctx, p.model, anthropic.ModelGetParams{})
	if err != nil {
		return Metadata{}, fmt.Errorf("anthropic model %q: %w", p.model, err)
	}
	vision := info.Capabilities.ImageInput.Supported
	return Metadata{
		ContextWindow:   int(info.MaxInputTokens),
		MaxOutputTokens: int(info.MaxTokens),
		Vision:          &vision,
		Source:          "Anthropic models API",
	}, nil
}

var (
	_ MetadataSource = (*VLLMProvider)(nil)
	_ MetadataSource = (*AnthropicProvider)(nil)
)
