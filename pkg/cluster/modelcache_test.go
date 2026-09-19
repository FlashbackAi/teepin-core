// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"context"
	"errors"
	"testing"

	agentpb "github.com/FlashbackAi/teepin-core/pkg/agentpb"
)

func TestRegistryCachedModels(t *testing.T) {
	reg := NewRegistry()
	if _, _, online := reg.CachedModels("nobody"); online {
		t.Fatal("an unknown provider reported online")
	}

	session := NewAgentSession("prov", "r", "v", "", func(*agentpb.ControlMessage) error { return nil })
	reg.Add(session)

	if _, known, online := reg.CachedModels("prov"); !online || known {
		t.Fatalf("before any report: known=%v online=%v, want online but unknown", known, online)
	}

	session.cache.set(&agentpb.GPUInventory{
		CachedModelsKnown: true,
		CachedModels:      []*agentpb.CachedModel{{RepoId: "org/a", SizeBytes: 10}},
	})
	models, known, _ := reg.CachedModels("prov")
	if !known || len(models) != 1 || models[0].RepoID != "org/a" || models[0].SizeBytes != 10 {
		t.Errorf("got %+v known=%v", models, known)
	}

	// Reported-and-empty is distinct from never-reported.
	session.cache.set(&agentpb.GPUInventory{CachedModelsKnown: true})
	models, known, _ = reg.CachedModels("prov")
	if !known || len(models) != 0 {
		t.Errorf("empty report: got %+v known=%v", models, known)
	}
}

func TestRegistryDeleteCachedModel(t *testing.T) {
	reg := NewRegistry()
	if err := reg.DeleteCachedModel(context.Background(), "nobody", "a/b"); !errors.Is(err, ErrProviderOffline) {
		t.Fatalf("err = %v, want ErrProviderOffline", err)
	}

	sentCh := make(chan *agentpb.ControlMessage, 1)
	session := NewAgentSession("prov", "r", "v", "", func(m *agentpb.ControlMessage) error { sentCh <- m; return nil })
	reg.Add(session)

	// Plays the agent: reads the command and answers it.
	run := func(res *agentpb.CommandResult) (*agentpb.ControlMessage, error) {
		go func() { m := <-sentCh; sentCh <- m; session.deliverResult(m.RequestId, res) }()
		err := reg.DeleteCachedModel(context.Background(), "prov", "org/a")
		return <-sentCh, err
	}

	msg, err := run(&agentpb.CommandResult{Success: true})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if msg.GetDeleteCachedModel().GetRepoId() != "org/a" {
		t.Errorf("sent %+v", msg)
	}

	if _, err := run(&agentpb.CommandResult{ErrorCode: agentpb.ErrorCode_ERROR_CODE_NOT_FOUND, ErrorMessage: "gone"}); !errors.Is(err, ErrModelNotCached) {
		t.Errorf("NOT_FOUND mapped to %v, want ErrModelNotCached", err)
	}
	if _, err := run(&agentpb.CommandResult{ErrorCode: agentpb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, ErrorMessage: "bad repo"}); err == nil || errors.Is(err, ErrModelNotCached) {
		t.Errorf("INVALID_ARGUMENT mapped to %v, want a plain error", err)
	}
}
