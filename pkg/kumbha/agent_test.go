// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/cluster"
)

// fakeCluster is a minimal cluster.Client for LaunchAgent/CloseSession
// tests — records what was created/deleted, nothing more.
type fakeCluster struct {
	createErr error
	deleteErr error

	created []cluster.InstanceSpec
	deleted []string
}

func (f *fakeCluster) CreateInstance(_ context.Context, spec cluster.InstanceSpec) (*cluster.InstanceResult, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.created = append(f.created, spec)
	return &cluster.InstanceResult{PodName: spec.InstanceID}, nil
}

func (f *fakeCluster) DeleteInstance(_ context.Context, _ cluster.Scope, instanceID string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, instanceID)
	return nil
}

func (f *fakeCluster) GetInstanceStatus(context.Context, cluster.Scope, string) (*cluster.InstanceStatus, error) {
	return nil, cluster.ErrNotFound
}
func (f *fakeCluster) ListInstanceStatuses(context.Context, cluster.Scope) ([]cluster.InstanceStatus, error) {
	return nil, nil
}
func (f *fakeCluster) StreamLogs(context.Context, cluster.Scope, string, cluster.LogOptions, io.Writer) error {
	return nil
}
func (f *fakeCluster) Inventory(context.Context) ([]cluster.NodeInventory, error) { return nil, nil }
func (f *fakeCluster) Healthy(context.Context) bool                               { return true }
func (f *fakeCluster) ResolveInstanceAddress(context.Context, string, int32) (string, error) {
	return "", cluster.ErrNotFound
}

func fakeMintToken(_, _, _ uuid.UUID, _ time.Duration) (string, error) {
	return "fake-agent-token", nil
}

func TestGateway_LaunchAgent_NotConfiguredWithoutWithAgent(t *testing.T) {
	store, _ := newMockStore(t)
	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{})

	sess := &Session{ID: uuid.New(), AccountID: uuid.New(), ProjectID: uuid.New()}
	err := gw.LaunchAgent(context.Background(), sess, "build me a booking app")
	if !errors.Is(err, ErrAgentNotConfigured) {
		t.Errorf("got %v, want ErrAgentNotConfigured", err)
	}
}

func TestGateway_LaunchAgent_Success(t *testing.T) {
	store, mock := newMockStore(t)
	fc := &fakeCluster{}
	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{}).
		WithAgent(fc, fakeMintToken, AgentConfig{Image: "kumbha-agent:latest", CPUUnits: 2, MemoryGB: 4})

	sessID, accountID, projectID := uuid.New(), uuid.New(), uuid.New()
	sess := &Session{ID: sessID, AccountID: accountID, ProjectID: projectID}

	mock.ExpectExec(`UPDATE billing\.inference_sessions SET agent_instance_id`).
		WithArgs(sessID, "kumbha-agent-"+sessID.String()[:8]).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := gw.LaunchAgent(context.Background(), sess, "build me a booking app"); err != nil {
		t.Fatalf("LaunchAgent: %v", err)
	}

	if len(fc.created) != 1 {
		t.Fatalf("got %d CreateInstance calls, want 1", len(fc.created))
	}
	spec := fc.created[0]
	if spec.Env["TEEPIN_SESSION_TOKEN"] != "fake-agent-token" {
		t.Errorf("agent pod did not receive its session token: %+v", spec.Env)
	}
	if spec.Env["TEEPIN_PROMPT"] != "build me a booking app" {
		t.Errorf("agent pod did not receive the prompt: %+v", spec.Env)
	}
	if spec.AccountID != accountID.String() || spec.ProjectID != projectID.String() {
		t.Errorf("agent pod not scoped to the session's account/project: %+v", spec)
	}
	if sess.AgentInstanceID == "" {
		t.Error("session's AgentInstanceID was not updated in memory")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestGateway_LaunchAgent_RecordingFailureCleansUpThePod(t *testing.T) {
	store, mock := newMockStore(t)
	fc := &fakeCluster{}
	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{}).
		WithAgent(fc, fakeMintToken, AgentConfig{Image: "kumbha-agent:latest"})

	sessID := uuid.New()
	sess := &Session{ID: sessID, AccountID: uuid.New(), ProjectID: uuid.New()}

	mock.ExpectExec(`UPDATE billing\.inference_sessions SET agent_instance_id`).
		WillReturnError(errors.New("db write failed"))

	err := gw.LaunchAgent(context.Background(), sess, "prompt")
	if err == nil {
		t.Fatal("expected an error when recording the agent instance id fails")
	}
	if len(fc.created) != 1 {
		t.Fatalf("pod was not created before the recording failure: %d creates", len(fc.created))
	}
	if len(fc.deleted) != 1 || fc.deleted[0] != fc.created[0].InstanceID {
		t.Errorf("orphaned pod was not cleaned up: created %v, deleted %v", fc.created, fc.deleted)
	}
}

func TestGateway_CloseSession_TearsDownAgentPod(t *testing.T) {
	store, mock := newMockStore(t)
	sessID, accountID, projectID := uuid.New(), uuid.New(), uuid.New()
	startedAt := time.Now()
	agentPodID := "kumbha-agent-" + sessID.String()[:8]

	mock.ExpectQuery(`UPDATE billing\.inference_sessions`).
		WithArgs(sessID, accountID, "closed").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "account_id", "project_id", "budget", "spent", "status", "label",
			"agent_instance_id", "deploy_approved", "started_at", "ended_at",
		}).AddRow(sessID, accountID, projectID, 5.0, 0.0, "closed", nil, agentPodID, false, startedAt, startedAt))

	mock.ExpectQuery(`SELECT route, provider, input_tokens, output_tokens`).
		WithArgs(sessID).
		WillReturnRows(sqlmock.NewRows([]string{"route", "provider", "input_tokens", "output_tokens"}))

	fc := &fakeCluster{}
	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{}).
		WithAgent(fc, fakeMintToken, AgentConfig{})

	if _, err := gw.CloseSession(context.Background(), sessID, accountID, "closed"); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if len(fc.deleted) != 1 || fc.deleted[0] != agentPodID {
		t.Errorf("agent pod was not torn down: deleted = %v, want [%s]", fc.deleted, agentPodID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestGateway_CloseSession_NoAgentPodMeansNoDeleteCall(t *testing.T) {
	store, mock := newMockStore(t)
	sessID, accountID, projectID := uuid.New(), uuid.New(), uuid.New()
	startedAt := time.Now()

	mock.ExpectQuery(`UPDATE billing\.inference_sessions`).
		WithArgs(sessID, accountID, "closed").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "account_id", "project_id", "budget", "spent", "status", "label",
			"agent_instance_id", "deploy_approved", "started_at", "ended_at",
		}).AddRow(sessID, accountID, projectID, 5.0, 0.0, "closed", nil, nil, false, startedAt, startedAt))

	mock.ExpectQuery(`SELECT route, provider, input_tokens, output_tokens`).
		WithArgs(sessID).
		WillReturnRows(sqlmock.NewRows([]string{"route", "provider", "input_tokens", "output_tokens"}))

	fc := &fakeCluster{}
	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{}).
		WithAgent(fc, fakeMintToken, AgentConfig{})

	if _, err := gw.CloseSession(context.Background(), sessID, accountID, "closed"); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if len(fc.deleted) != 0 {
		t.Errorf("DeleteInstance was called with no agent pod on the session: %v", fc.deleted)
	}
}
