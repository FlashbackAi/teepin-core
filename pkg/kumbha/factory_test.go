// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

type fakeSecretsClient struct {
	values map[string]string
	calls  int
}

func (f *fakeSecretsClient) Get(_ context.Context, secretID string) (string, bool, error) {
	f.calls++
	v, ok := f.values[secretID]
	return v, ok, nil
}

func (f *fakeSecretsClient) Put(_ context.Context, secretID, value string) error {
	if f.values == nil {
		f.values = map[string]string{}
	}
	f.values[secretID] = value
	return nil
}

func (f *fakeSecretsClient) Delete(_ context.Context, secretID string) error {
	delete(f.values, secretID)
	return nil
}

func TestProviderFactory_Build_CachesUntilConfigChanges(t *testing.T) {
	secrets := &fakeSecretsClient{}
	factory := NewProviderFactory(secrets, "dev")
	c := RouteCandidate{ID: uuid.New(), ProviderType: "vllm", BaseURL: "http://a", Model: "m", ContextWindow: 8000, SupportsTools: true}

	p1, err := factory.Build(context.Background(), c)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	p2, err := factory.Build(context.Background(), c)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if p1 != p2 {
		t.Error("Build returned a different Provider for an unchanged candidate — cache did not hit")
	}

	c.BaseURL = "http://b" // config changed
	p3, err := factory.Build(context.Background(), c)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if p3 == p1 {
		t.Error("Build kept the stale cached Provider after base_url changed")
	}
}

func TestProviderFactory_Build_SecretRotationInvalidatesCache(t *testing.T) {
	id := uuid.New()
	secretID := CandidateSecretName("dev", id)
	secrets := &fakeSecretsClient{values: map[string]string{secretID: "key-v1"}}
	factory := NewProviderFactory(secrets, "dev")
	c := RouteCandidate{ID: id, ProviderType: "vllm", BaseURL: "http://a", Model: "m", HasSecret: true}

	p1, err := factory.Build(context.Background(), c)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	secrets.values[secretID] = "key-v2" // rotated from Control Center
	p2, err := factory.Build(context.Background(), c)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if p1 == p2 {
		t.Error("Build kept the stale cached Provider after the secret rotated — a rotated key would silently never take effect")
	}
}

func TestProviderFactory_Build_NoSecretClientMeansEmptyKey(t *testing.T) {
	factory := NewProviderFactory(nil, "dev")
	c := RouteCandidate{ID: uuid.New(), ProviderType: "vllm", BaseURL: "http://a", Model: "m", HasSecret: true}

	if _, err := factory.Build(context.Background(), c); err != nil {
		t.Fatalf("Build should succeed with an empty key when no SecretsClient is configured, got: %v", err)
	}
}

func TestProviderFactory_Build_UnknownProviderType(t *testing.T) {
	factory := NewProviderFactory(nil, "dev")
	c := RouteCandidate{ID: uuid.New(), ProviderType: "openai", Model: "m"}

	if _, err := factory.Build(context.Background(), c); err == nil {
		t.Error("expected an error for an unrecognised provider_type, got nil")
	}
}
