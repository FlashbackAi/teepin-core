// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package inferencegateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/google/uuid"
)

// SecretsClient is the narrow surface external models' API keys need from
// Secrets Manager: read the current value (live, not the one-time env-var
// snapshot ECS's own `valueFrom` gives every other secret in this
// platform), and write a new one from Control Center with no redeploy.
// An interface so tests supply a fake instead of making real AWS calls —
// same shape as pkg/database's own secretFetcher.
type SecretsClient interface {
	Get(ctx context.Context, secretID string) (value string, ok bool, err error)
	Put(ctx context.Context, secretID, value string) error
	Delete(ctx context.Context, secretID string) error
}

// SecretName is the full Secrets Manager name for a catalog model's API
// key reference (modelcatalog.Model.APIKeyRef, which omits the environment
// prefix so a database migration can record one without knowing which
// environment it runs in).
func SecretName(environment, ref string) string {
	return fmt.Sprintf("teepin/%s/%s", environment, ref)
}

// NewAPIKeyRef returns a fresh reference for a model's API key. The
// inference-model-key- prefix is what the IAM policy granting this process
// write access is scoped to — never the platform's other secrets (DB
// password, JWT signing key, Shelby/Stripe credentials); see iam.tf. Keys
// migrated from Kumbha's old route candidates keep their original
// kumbha-candidate- names, which that policy also covers.
func NewAPIKeyRef() string {
	return "inference-model-key-" + uuid.NewString()
}

// secretCacheTTL bounds how stale a model's API key can be after an
// operator rotates it from Control Center — short enough that "no
// redeploy needed" is true in practice, long enough that a busy model
// isn't making a live AWS call on every single completion.
const secretCacheTTL = 30 * time.Second

type secretCacheEntry struct {
	value   string
	ok      bool
	fetchAt time.Time
}

// AWSSecretsClient is the real Secrets Manager-backed implementation, with
// a short per-secret TTL cache so a busy model doesn't call GetSecretValue
// on every completion. Mirrors pkg/database's passwordCache almost exactly
// — same problem (a value that can change out from under a long-running
// process, where ECS's own secret injection only resolves once at
// container start), same fix (poll it directly, cache briefly).
type AWSSecretsClient struct {
	client *secretsmanager.Client

	mu    sync.Mutex
	cache map[string]secretCacheEntry
	now   func() time.Time
}

// NewAWSSecretsClient loads the default AWS config (the task role's own
// credentials on ECS) and returns a ready client. Returns an error only if
// AWS config itself can't be loaded — a missing/unreadable secret is a
// per-call error from Get/Put, not a construction-time failure.
func NewAWSSecretsClient(ctx context.Context) (*AWSSecretsClient, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config for model API key secrets: %w", err)
	}
	return &AWSSecretsClient{
		client: secretsmanager.NewFromConfig(cfg),
		cache:  map[string]secretCacheEntry{},
		now:    time.Now,
	}, nil
}

// Get returns secretID's current value, refreshing from Secrets Manager
// once the cached entry is older than secretCacheTTL. A refresh failure
// falls back to the last-known value (Secrets Manager being briefly
// unreachable must not itself break every in-flight completion) unless
// nothing has ever been successfully fetched, in which case the error is
// real and is returned.
func (c *AWSSecretsClient) Get(ctx context.Context, secretID string) (string, bool, error) {
	c.mu.Lock()
	entry, cached := c.cache[secretID]
	fresh := cached && c.now().Sub(entry.fetchAt) < secretCacheTTL
	c.mu.Unlock()
	if fresh {
		return entry.value, entry.ok, nil
	}

	out, err := c.client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: &secretID})
	if err != nil {
		var notFound *smtypes.ResourceNotFoundException
		if errors.As(err, &notFound) {
			c.mu.Lock()
			c.cache[secretID] = secretCacheEntry{ok: false, fetchAt: c.now()}
			c.mu.Unlock()
			return "", false, nil
		}
		if cached {
			// Keep serving the last-known-good value; try again next call.
			return entry.value, entry.ok, nil
		}
		return "", false, fmt.Errorf("failed to read secret %q: %w", secretID, err)
	}

	value := ""
	if out.SecretString != nil {
		value = *out.SecretString
	}
	c.mu.Lock()
	c.cache[secretID] = secretCacheEntry{value: value, ok: true, fetchAt: c.now()}
	c.mu.Unlock()
	return value, true, nil
}

// Put writes secretID's value, creating the secret first if it doesn't
// exist yet (a model getting its first key has no secret to update). The cache
// entry is refreshed immediately so a Get that follows in the same request
// (e.g. the admin handler round-tripping to confirm the write) sees the
// new value without waiting out the TTL.
func (c *AWSSecretsClient) Put(ctx context.Context, secretID, value string) error {
	_, err := c.client.PutSecretValue(ctx, &secretsmanager.PutSecretValueInput{
		SecretId:     &secretID,
		SecretString: &value,
	})
	if err != nil {
		var notFound *smtypes.ResourceNotFoundException
		if errors.As(err, &notFound) {
			_, createErr := c.client.CreateSecret(ctx, &secretsmanager.CreateSecretInput{
				Name:         &secretID,
				SecretString: &value,
			})
			if createErr != nil {
				return fmt.Errorf("failed to create secret %q: %w", secretID, createErr)
			}
		} else {
			return fmt.Errorf("failed to write secret %q: %w", secretID, err)
		}
	}

	c.mu.Lock()
	c.cache[secretID] = secretCacheEntry{value: value, ok: true, fetchAt: c.now()}
	c.mu.Unlock()
	return nil
}

// Delete removes a model's secret entirely, with Secrets Manager's own
// recovery window (same posture as every other secret this platform
// creates — see e.g. security.tf's recovery_window_in_days) rather than an
// immediate force-delete. Deleting an already-absent secret is treated as
// success, so this is safe to call unconditionally when a model is
// removed (never leaves the caller needing to check existence first).
func (c *AWSSecretsClient) Delete(ctx context.Context, secretID string) error {
	_, err := c.client.DeleteSecret(ctx, &secretsmanager.DeleteSecretInput{SecretId: &secretID})
	if err != nil {
		var notFound *smtypes.ResourceNotFoundException
		if !errors.As(err, &notFound) {
			return fmt.Errorf("failed to delete secret %q: %w", secretID, err)
		}
	}
	c.mu.Lock()
	delete(c.cache, secretID)
	c.mu.Unlock()
	return nil
}
