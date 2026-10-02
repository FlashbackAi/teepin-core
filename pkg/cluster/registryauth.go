// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"context"
	"errors"
	"fmt"

	"github.com/FlashbackAi/teepin-core/pkg/agentpb"
)

// RegistryAuth is a credential for pulling images from a private registry.
// The control plane mints it (for ECR, from the task role) and sends it to each
// node, which stores it as the imagePullSecret pods already reference, so no one
// has to refresh a token by hand on a node. Password is a secret and is never
// logged.
type RegistryAuth struct {
	// SecretName is the imagePullSecret the pods reference.
	SecretName string
	Server     string
	Username   string
	Password   string
}

// Validate refuses a credential that could not be stored as a pull secret.
func (a RegistryAuth) Validate() error {
	switch {
	case a.SecretName == "":
		return errors.New("registry auth: secret name is required")
	case a.Server == "":
		return errors.New("registry auth: server is required")
	case a.Username == "" || a.Password == "":
		return errors.New("registry auth: username and password are required")
	}
	return nil
}

// RegistryAuthApplier is implemented by a cluster client that can store a
// registry credential as an imagePullSecret (DirectClient). The agent runner
// uses it when the control plane sends a RegistryAuthCommand.
type RegistryAuthApplier interface {
	ApplyRegistryAuth(ctx context.Context, auth RegistryAuth) error
}

// SetOnConnect registers fn to run (in its own goroutine) every time an agent
// connects, so the control plane can hand a freshly connected node its
// credentials at once (a node that slept past a token's expiry comes back with
// a stale one). Replaces any earlier hook.
func (r *Registry) SetOnConnect(fn func(providerID string)) {
	r.mu.Lock()
	r.onConnect = fn
	r.mu.Unlock()
}

// ProviderIDs lists the providers with a live agent session right now.
func (r *Registry) ProviderIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.sessions))
	for id := range r.sessions {
		out = append(out, id)
	}
	return out
}

// PushRegistryAuth sends one registry credential to a provider's agent and waits
// for it to be stored.
func (r *Registry) PushRegistryAuth(ctx context.Context, providerID string, auth RegistryAuth) error {
	if err := auth.Validate(); err != nil {
		return err
	}
	session, ok := r.ByProvider(providerID)
	if !ok {
		return ErrProviderOffline
	}
	result, err := session.dispatch(ctx, &agentpb.ControlMessage{
		Payload: &agentpb.ControlMessage_RegistryAuth{
			RegistryAuth: &agentpb.RegistryAuthCommand{
				SecretName: auth.SecretName,
				Server:     auth.Server,
				Username:   auth.Username,
				Password:   auth.Password,
			},
		},
	})
	if err != nil {
		return err
	}
	if result.Success {
		return nil
	}
	return fmt.Errorf("agent could not store the registry credential: %w", errorFromResult(result))
}
