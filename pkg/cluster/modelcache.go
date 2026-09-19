// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"context"
	"errors"
	"fmt"
	"sync"

	agentpb "github.com/FlashbackAi/teepin-core/pkg/agentpb"
)

// CachedModel is one model downloaded onto a node's disk, as its agent
// reported it.
type CachedModel struct {
	RepoID    string `json:"repo_id"`
	SizeBytes int64  `json:"size_bytes"`
}

// ErrProviderOffline means no agent session is connected for the provider.
var ErrProviderOffline = errors.New("cluster: provider is offline")

// ErrModelNotCached means the agent reports the model is not in its cache.
var ErrModelNotCached = errors.New("cluster: model is not in the node's cache")

// modelCacheState is what one session last reported about its host's model
// cache. Kept separate from the session's other fields so this feature stays
// self-contained.
type modelCacheState struct {
	mu     sync.Mutex
	models []CachedModel
	known  bool
}

func (m *modelCacheState) set(inv *agentpb.GPUInventory) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.known = inv.CachedModelsKnown
	m.models = m.models[:0]
	for _, cm := range inv.CachedModels {
		m.models = append(m.models, CachedModel{RepoID: cm.RepoId, SizeBytes: cm.SizeBytes})
	}
}

func (m *modelCacheState) get() ([]CachedModel, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]CachedModel(nil), m.models...), m.known
}

// CachedModels returns the model cache last reported by the provider's agent.
// online is false when no session is connected. known is false when the agent
// does not report a model cache at all (an older agent, or a node running no
// native models), which is different from an empty cache.
func (r *Registry) CachedModels(providerID string) (models []CachedModel, known, online bool) {
	session, ok := r.ByProvider(providerID)
	if !ok {
		return nil, false, false
	}
	models, known = session.cache.get()
	return models, known, true
}

// DeleteCachedModel asks the provider's agent to remove one model from its
// cache. The agent validates the repo id itself.
func (r *Registry) DeleteCachedModel(ctx context.Context, providerID, repoID string) error {
	session, ok := r.ByProvider(providerID)
	if !ok {
		return ErrProviderOffline
	}
	result, err := session.dispatch(ctx, &agentpb.ControlMessage{
		Payload: &agentpb.ControlMessage_DeleteCachedModel{
			DeleteCachedModel: &agentpb.DeleteCachedModelCommand{RepoId: repoID},
		},
	})
	if err != nil {
		return err
	}
	if result.Success {
		return nil
	}
	if result.ErrorCode == agentpb.ErrorCode_ERROR_CODE_NOT_FOUND {
		return fmt.Errorf("%w: %s", ErrModelNotCached, repoID)
	}
	return errorFromResult(result)
}
