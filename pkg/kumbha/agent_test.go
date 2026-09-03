// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/FlashbackAi/teepin-core/pkg/cluster"
)

// fakeCluster is a minimal cluster.Client for LaunchAgent/CloseSession
// tests — records what was created/deleted, nothing more.
type fakeCluster struct {
	createErr error
	deleteErr error
	// statusResult/statusErr let DeliverMessage/isAgentRunning tests
	// control what GetInstanceStatus reports; unset (both nil) preserves
	// the original default every other test in this file already relies
	// on — "no such pod".
	statusResult *cluster.InstanceStatus
	statusErr    error

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

func (f *fakeCluster) UpdateInstance(_ context.Context, _ cluster.Scope, spec cluster.InstanceSpec) (*cluster.InstanceResult, error) {
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
	if f.statusResult != nil || f.statusErr != nil {
		return f.statusResult, f.statusErr
	}
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

func TestGateway_MintWorkspaceFetchToken_NotConfiguredWithoutWithAgent(t *testing.T) {
	store, _ := newMockStore(t)
	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{})

	sess := &Session{ID: uuid.New(), AccountID: uuid.New(), ProjectID: uuid.New()}
	_, _, err := gw.MintWorkspaceFetchToken(sess, time.Minute)
	if !errors.Is(err, ErrAgentNotConfigured) {
		t.Errorf("got %v, want ErrAgentNotConfigured", err)
	}
}

// The build pod's fetch-workspace initContainer needs a URL it can reach
// from wherever build pods run — the SAME APIBaseURL already configured
// for the agent pod itself (WithAgent), not a separate setting to keep in
// sync.
func TestGateway_MintWorkspaceFetchToken_ReturnsTokenAndArchiveURL(t *testing.T) {
	store, _ := newMockStore(t)
	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{}).
		WithAgent(&fakeCluster{}, fakeMintToken, AgentConfig{
			Image:      "kumbha-agent:latest",
			APIBaseURL: "http://teepin-api.default.svc.cluster.local:8080/",
		})

	sessID := uuid.New()
	sess := &Session{ID: sessID, AccountID: uuid.New(), ProjectID: uuid.New()}
	token, archiveURL, err := gw.MintWorkspaceFetchToken(sess, time.Minute)
	if err != nil {
		t.Fatalf("MintWorkspaceFetchToken: %v", err)
	}
	if token != "fake-agent-token" {
		t.Errorf("token = %q, want the value fakeMintToken returns", token)
	}
	want := "http://teepin-api.default.svc.cluster.local:8080/v1/kumbha/sessions/" + sessID.String() + "/workspace/archive"
	if archiveURL != want {
		t.Errorf("archiveURL = %q, want %q", archiveURL, want)
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
	if !strings.HasSuffix(spec.Env["TEEPIN_PROMPT"], "build me a booking app") {
		t.Errorf("agent pod did not receive the prompt: %+v", spec.Env)
	}
	if !strings.Contains(spec.Env["TEEPIN_PROMPT"], InternalScratchDir) {
		t.Error("agent pod's prompt did not carry the internal scratch-path instruction")
	}
	if spec.AccountID != accountID.String() || spec.ProjectID != projectID.String() {
		t.Errorf("agent pod not scoped to the session's account/project: %+v", spec)
	}
	// A restarted agent pod silently re-runs the entire build from
	// scratch against the same original prompt (found live 2026-08-24) —
	// this must never be left at Kubernetes' default RestartPolicy:
	// Always, unlike a normal customer compute instance.
	if !spec.NeverRestart {
		t.Error("agent pod spec did not set NeverRestart — a Kubernetes restart would silently re-run the whole build")
	}
	if sess.AgentInstanceID == "" {
		t.Error("session's AgentInstanceID was not updated in memory")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestGateway_LaunchAgent_PropagatesVisionCapableFlag covers AgentConfig.
// VisionCapable reaching the pod as TEEPIN_VISION_CAPABLE — what
// browser_tool.py's get_shared_browser() reads to decide whether
// browser_screenshot attaches an actual image or stays text-only. Both
// values are asserted (not just the non-default one) since the SAFE
// default (false) not silently flipping to true is the more
// safety-critical direction to catch a regression in.
func TestGateway_LaunchAgent_PropagatesVisionCapableFlag(t *testing.T) {
	for _, visionCapable := range []bool{false, true} {
		store, mock := newMockStore(t)
		fc := &fakeCluster{}
		gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{}).
			WithAgent(fc, fakeMintToken, AgentConfig{Image: "kumbha-agent:latest", VisionCapable: visionCapable})

		sessID := uuid.New()
		sess := &Session{ID: sessID, AccountID: uuid.New(), ProjectID: uuid.New()}

		mock.ExpectExec(`UPDATE billing\.inference_sessions SET agent_instance_id`).
			WithArgs(sessID, "kumbha-agent-"+sessID.String()[:8]).
			WillReturnResult(sqlmock.NewResult(0, 1))

		if err := gw.LaunchAgent(context.Background(), sess, "build me a booking app"); err != nil {
			t.Fatalf("LaunchAgent: %v", err)
		}

		want := strconv.FormatBool(visionCapable)
		if got := fc.created[0].Env["TEEPIN_VISION_CAPABLE"]; got != want {
			t.Errorf("VisionCapable=%v: TEEPIN_VISION_CAPABLE = %q, want %q", visionCapable, got, want)
		}
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
			"agent_instance_id", "app_instance_id", "deploy_approved", "started_at", "ended_at",
		}).AddRow(sessID, accountID, projectID, 5.0, 0.0, "closed", nil, agentPodID, nil, false, startedAt, startedAt))

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
			"agent_instance_id", "app_instance_id", "deploy_approved", "started_at", "ended_at",
		}).AddRow(sessID, accountID, projectID, 5.0, 0.0, "closed", nil, nil, nil, false, startedAt, startedAt))

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

// TestGateway_CloseSession_NeverSettlesAnything is the regression test for
// the actual point of removing settlement from CloseSession: Complete()
// already settled every completion's cost as it happened (see
// TestGateway_Complete_SuccessPricesAndAccrues), so CloseSession must
// NEVER touch usage.RecordUsage/ConsumeCredit at all — doing so again
// from RouteUsage's cumulative totals would double-charge the account for
// spend that was already settled per-completion.
func TestGateway_CloseSession_NeverSettlesAnything(t *testing.T) {
	store, mock := newMockStore(t)
	sessID, accountID, projectID := uuid.New(), uuid.New(), uuid.New()
	startedAt := time.Now()

	mock.ExpectQuery(`UPDATE billing\.inference_sessions`).
		WithArgs(sessID, accountID, "closed").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "account_id", "project_id", "budget", "spent", "status", "label",
			"agent_instance_id", "app_instance_id", "deploy_approved", "started_at", "ended_at",
		}).AddRow(sessID, accountID, projectID, 5.0, 3.5, "closed", nil, nil, nil, false, startedAt, startedAt))

	usage := &fakeUsageRecorder{}
	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{in: 2.0, out: 8.0}, usage)

	if _, err := gw.CloseSession(context.Background(), sessID, accountID, "closed"); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if len(usage.records) != 0 {
		t.Errorf("CloseSession wrote %d usage_records lines, want 0 — settlement is Complete()'s job now", len(usage.records))
	}
	if len(usage.consumed) != 0 {
		t.Errorf("CloseSession called ConsumeCredit %d times, want 0", len(usage.consumed))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// --- isAgentRunning / DeliverMessage ---

// TestGateway_IsAgentRunning_ExportedWrapperMatchesInternal confirms the
// exported IsAgentRunning (pkg/api.GetKumbhaSession's own live-status
// read) is not a second, drifted implementation — just isAgentRunning
// itself, thoroughly covered by the tests below via its private name.
func TestGateway_IsAgentRunning_ExportedWrapperMatchesInternal(t *testing.T) {
	store, _ := newMockStore(t)
	fc := &fakeCluster{statusResult: &cluster.InstanceStatus{Status: "running"}}
	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{}).
		WithAgent(fc, fakeMintToken, AgentConfig{})
	sess := &Session{ID: uuid.New(), ProjectID: uuid.New(), AgentInstanceID: "kumbha-agent-abc"}

	running, err := gw.IsAgentRunning(context.Background(), sess)
	if err != nil {
		t.Fatalf("IsAgentRunning: %v", err)
	}
	if !running {
		t.Error("got running=false, want true for a status of \"running\"")
	}
}

func TestIsAgentRunning_NoClusterConfiguredIsFalse(t *testing.T) {
	store, _ := newMockStore(t)
	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{})
	sess := &Session{ID: uuid.New(), ProjectID: uuid.New(), AgentInstanceID: "kumbha-agent-abc"}

	running, err := gw.isAgentRunning(context.Background(), sess)
	if err != nil {
		t.Fatalf("isAgentRunning: %v", err)
	}
	if running {
		t.Error("got running=true with no cluster configured, want false")
	}
}

func TestIsAgentRunning_NoAgentInstanceIDIsFalse(t *testing.T) {
	store, _ := newMockStore(t)
	fc := &fakeCluster{statusResult: &cluster.InstanceStatus{Status: "running"}}
	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{}).
		WithAgent(fc, fakeMintToken, AgentConfig{})
	sess := &Session{ID: uuid.New(), ProjectID: uuid.New()} // AgentInstanceID empty

	running, err := gw.isAgentRunning(context.Background(), sess)
	if err != nil {
		t.Fatalf("isAgentRunning: %v", err)
	}
	if running {
		t.Error("got running=true with no agent ever launched, want false")
	}
}

func TestIsAgentRunning_RunningOrPendingStatusIsTrue(t *testing.T) {
	for _, status := range []string{"running", "pending"} {
		store, _ := newMockStore(t)
		fc := &fakeCluster{statusResult: &cluster.InstanceStatus{Status: status}}
		gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{}).
			WithAgent(fc, fakeMintToken, AgentConfig{})
		sess := &Session{ID: uuid.New(), ProjectID: uuid.New(), AgentInstanceID: "kumbha-agent-abc"}

		running, err := gw.isAgentRunning(context.Background(), sess)
		if err != nil {
			t.Fatalf("isAgentRunning(%s): %v", status, err)
		}
		if !running {
			t.Errorf("status %q: got running=false, want true", status)
		}
	}
}

func TestIsAgentRunning_TerminatedOrFailedStatusIsFalse(t *testing.T) {
	for _, status := range []string{"terminated", "failed"} {
		store, _ := newMockStore(t)
		fc := &fakeCluster{statusResult: &cluster.InstanceStatus{Status: status}}
		gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{}).
			WithAgent(fc, fakeMintToken, AgentConfig{})
		sess := &Session{ID: uuid.New(), ProjectID: uuid.New(), AgentInstanceID: "kumbha-agent-abc"}

		running, err := gw.isAgentRunning(context.Background(), sess)
		if err != nil {
			t.Fatalf("isAgentRunning(%s): %v", status, err)
		}
		if running {
			t.Errorf("status %q: got running=true, want false", status)
		}
	}
}

func TestIsAgentRunning_NotFoundIsFalseWithNoError(t *testing.T) {
	store, _ := newMockStore(t)
	fc := &fakeCluster{statusErr: cluster.ErrNotFound}
	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{}).
		WithAgent(fc, fakeMintToken, AgentConfig{})
	sess := &Session{ID: uuid.New(), ProjectID: uuid.New(), AgentInstanceID: "kumbha-agent-abc"}

	running, err := gw.isAgentRunning(context.Background(), sess)
	if err != nil {
		t.Fatalf("isAgentRunning: %v", err)
	}
	if running {
		t.Error("got running=true for a pod cluster.ErrNotFound reported gone, want false")
	}
}

func TestIsAgentRunning_AmbiguousErrorPropagates(t *testing.T) {
	store, _ := newMockStore(t)
	boom := errors.New("cluster unreachable")
	fc := &fakeCluster{statusErr: boom}
	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{}).
		WithAgent(fc, fakeMintToken, AgentConfig{})
	sess := &Session{ID: uuid.New(), ProjectID: uuid.New(), AgentInstanceID: "kumbha-agent-abc"}

	_, err := gw.isAgentRunning(context.Background(), sess)
	if err == nil {
		t.Fatal("got nil error for an ambiguous cluster failure, want it propagated")
	}
}

func TestGateway_DeliverMessage_NotConfiguredWithoutWithAgent(t *testing.T) {
	store, _ := newMockStore(t)
	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{})
	sess := &Session{ID: uuid.New(), AccountID: uuid.New(), ProjectID: uuid.New()}

	_, err := gw.DeliverMessage(context.Background(), sess, "hello")
	if !errors.Is(err, ErrAgentNotConfigured) {
		t.Errorf("got %v, want ErrAgentNotConfigured", err)
	}
}

// The common case: the previous pod is still alive, so the message is
// just queued for its own poll loop to pick up — no new pod launched.
func TestGateway_DeliverMessage_QueuesWhenAgentRunning(t *testing.T) {
	store, mock := newMockStore(t)
	fc := &fakeCluster{statusResult: &cluster.InstanceStatus{Status: "running"}}
	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{}).
		WithAgent(fc, fakeMintToken, AgentConfig{})

	sessID := uuid.New()
	sess := &Session{ID: sessID, AccountID: uuid.New(), ProjectID: uuid.New(), AgentInstanceID: "kumbha-agent-abc"}

	mock.ExpectQuery(`INSERT INTO billing\.kumbha_messages`).
		WithArgs(sessID, "add a footer").
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(1), time.Now()))

	relaunched, err := gw.DeliverMessage(context.Background(), sess, "add a footer")
	if err != nil {
		t.Fatalf("DeliverMessage: %v", err)
	}
	if relaunched {
		t.Error("got relaunched=true while the agent pod is still running, want false")
	}
	if len(fc.created) != 0 {
		t.Errorf("CreateInstance was called while the agent pod is still running: %v", fc.created)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// The relaunch case: the previous pod is gone, so DeliverMessage starts a
// fresh one with the message folded into a resume-oriented prompt rather
// than queuing it (queuing would just strand it — nothing would ever
// poll for it).
func TestGateway_DeliverMessage_RelaunchesWhenAgentNotRunning(t *testing.T) {
	store, mock := newMockStore(t)
	fc := &fakeCluster{statusErr: cluster.ErrNotFound}
	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{}).
		WithAgent(fc, fakeMintToken, AgentConfig{Image: "kumbha-agent:latest"})

	sessID := uuid.New()
	sess := &Session{ID: sessID, AccountID: uuid.New(), ProjectID: uuid.New(), AgentInstanceID: "kumbha-agent-abc"}

	mock.ExpectExec(`UPDATE billing\.inference_sessions SET agent_instance_id`).
		WithArgs(sessID, "kumbha-agent-"+sessID.String()[:8]).
		WillReturnResult(sqlmock.NewResult(0, 1))

	relaunched, err := gw.DeliverMessage(context.Background(), sess, "add a footer")
	if err != nil {
		t.Fatalf("DeliverMessage: %v", err)
	}
	if !relaunched {
		t.Error("got relaunched=false with no live agent pod, want true")
	}
	if len(fc.created) != 1 {
		t.Fatalf("got %d CreateInstance calls, want 1", len(fc.created))
	}
	prompt := fc.created[0].Env["TEEPIN_PROMPT"]
	if !strings.Contains(prompt, "add a footer") {
		t.Errorf("relaunch prompt = %q, want it to include the customer's message", prompt)
	}
	if !strings.Contains(prompt, "workspace already contains") {
		t.Errorf("relaunch prompt = %q, want it to tell the agent its previous work still exists", prompt)
	}
}

// --- DeleteSessions ---

// TestGateway_DeleteSessions_StopsAnOpenSessionThenDeletesIt covers BOTH
// a genuinely still-building session (a customer's explicit delete is
// authorization to stop it, no separate "stop first" step — see
// DeleteSessions' own doc comment) and a "zombie" one whose pod already
// died without anyone calling Close: fakeCluster's default (no
// statusResult/statusErr set) reports the pod as not found, which is
// exactly the zombie case, and DeleteSessions no longer distinguishes
// the two — either way CloseSession runs before the delete.
func TestGateway_DeleteSessions_StopsAnOpenSessionThenDeletesIt(t *testing.T) {
	store, mock := newMockStore(t)
	sessID, accountID, projectID := uuid.New(), uuid.New(), uuid.New()
	startedAt := time.Now()
	agentPodID := "kumbha-agent-" + sessID.String()[:8]

	// Get: session is nominally "open" in the DB.
	mock.ExpectQuery(`SELECT id, account_id, project_id, budget, spent, status`).
		WithArgs(sessID, accountID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "account_id", "project_id", "budget", "spent", "status", "label",
			"agent_instance_id", "app_instance_id", "deploy_approved", "started_at", "ended_at",
			"last_deploy_failed", "last_deploy_error", "last_deploy_at",
		}).AddRow(sessID, accountID, projectID, 5.0, 0.0, "open", nil, agentPodID, nil, false, startedAt, nil, false, nil, nil))

	// CloseSession: Close + tear down the agent pod (no settlement any
	// more — see Gateway.CloseSession's own doc comment).
	mock.ExpectQuery(`UPDATE billing\.inference_sessions`).
		WithArgs(sessID, accountID, "closed").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "account_id", "project_id", "budget", "spent", "status", "label",
			"agent_instance_id", "app_instance_id", "deploy_approved", "started_at", "ended_at",
		}).AddRow(sessID, accountID, projectID, 5.0, 0.0, "closed", nil, agentPodID, nil, false, startedAt, startedAt))

	// Finally, the actual delete.
	mock.ExpectQuery(`DELETE FROM billing\.inference_sessions`).
		WithArgs(accountID, pq.Array([]uuid.UUID{sessID})).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(sessID))

	fc := &fakeCluster{}
	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{}).
		WithAgent(fc, fakeMintToken, AgentConfig{})

	deleted, err := gw.DeleteSessions(context.Background(), accountID, []uuid.UUID{sessID})
	if err != nil {
		t.Fatalf("DeleteSessions: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != sessID {
		t.Errorf("got %v, want %v deleted", deleted, sessID)
	}
	if len(fc.deleted) != 1 || fc.deleted[0] != agentPodID {
		t.Errorf("agent pod was not torn down: deleted = %v, want [%s]", fc.deleted, agentPodID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestGateway_DeleteSessions_AlreadyClosedSessionSkipsCloseCall(t *testing.T) {
	store, mock := newMockStore(t)
	sessID, accountID, projectID := uuid.New(), uuid.New(), uuid.New()
	startedAt := time.Now()

	mock.ExpectQuery(`SELECT id, account_id, project_id, budget, spent, status`).
		WithArgs(sessID, accountID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "account_id", "project_id", "budget", "spent", "status", "label",
			"agent_instance_id", "app_instance_id", "deploy_approved", "started_at", "ended_at",
			"last_deploy_failed", "last_deploy_error", "last_deploy_at",
		}).AddRow(sessID, accountID, projectID, 5.0, 0.0, "closed", nil, nil, nil, false, startedAt, startedAt, false, nil, nil))

	// No Close-related queries expected — already closed, nothing to stop.

	mock.ExpectQuery(`DELETE FROM billing\.inference_sessions`).
		WithArgs(accountID, pq.Array([]uuid.UUID{sessID})).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(sessID))

	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{})

	deleted, err := gw.DeleteSessions(context.Background(), accountID, []uuid.UUID{sessID})
	if err != nil {
		t.Fatalf("DeleteSessions: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != sessID {
		t.Errorf("got %v, want %v deleted", deleted, sessID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestGateway_CaptureScreenshot_NotConfiguredWithoutWithAgent(t *testing.T) {
	store, _ := newMockStore(t)
	// CaptureScreenshot reuses AgentConfig.Image (deploy/kumbha-agent's
	// own image) — there is no separate "screenshots configured"
	// capability to enable, so WithAgent alone is both necessary and
	// sufficient.
	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{})

	sess := &Session{ID: uuid.New(), AccountID: uuid.New(), ProjectID: uuid.New()}
	err := gw.CaptureScreenshot(context.Background(), sess, "https://inst-abc123.teepin.com")
	if !errors.Is(err, ErrAgentNotConfigured) {
		t.Errorf("got %v, want ErrAgentNotConfigured", err)
	}
}

// TestGateway_CaptureScreenshot_Success covers the whole happy path: the
// capture pod reuses the SAME image LaunchAgent does (no separate
// screenshot image/registry), overrides its entrypoint to the capture
// binary, is launched with the right target/upload/token env vars, hidden
// from the customer's Compute list (agentLabel), carries the agent
// image's own pull secret, and is torn down once it reports "terminated"
// — mirroring pkg/build.Service.Build's own launch-poll-cleanup shape.
func TestGateway_CaptureScreenshot_Success(t *testing.T) {
	store, _ := newMockStore(t)
	fc := &fakeCluster{statusResult: &cluster.InstanceStatus{Status: "terminated"}}
	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{}).
		WithAgent(fc, fakeMintToken, AgentConfig{
			Image:           "kumbha-agent:latest",
			APIBaseURL:      "http://teepin-api.default.svc.cluster.local:8080",
			ImagePullSecret: "teepin-kumbha-ecr",
		})

	sessID, accountID, projectID := uuid.New(), uuid.New(), uuid.New()
	sess := &Session{ID: sessID, AccountID: accountID, ProjectID: projectID}

	if err := gw.CaptureScreenshot(context.Background(), sess, "https://inst-abc123.teepin.com"); err != nil {
		t.Fatalf("CaptureScreenshot: %v", err)
	}

	if len(fc.created) != 1 {
		t.Fatalf("got %d CreateInstance calls, want 1", len(fc.created))
	}
	spec := fc.created[0]
	if spec.Image != "kumbha-agent:latest" {
		t.Errorf("Image = %q, want the SAME agent image reused, not a separate one", spec.Image)
	}
	if len(spec.Command) != 1 || spec.Command[0] != screenshotBinaryPath {
		t.Errorf("Command = %v, want an override to %q", spec.Command, screenshotBinaryPath)
	}
	if spec.Env["TEEPIN_TARGET_URL"] != "https://inst-abc123.teepin.com" {
		t.Errorf("capture pod did not receive the target URL: %+v", spec.Env)
	}
	wantUploadURL := "http://teepin-api.default.svc.cluster.local:8080/v1/kumbha/sessions/" + sessID.String() + "/screenshot"
	if spec.Env["TEEPIN_UPLOAD_URL"] != wantUploadURL {
		t.Errorf("upload URL = %q, want %q", spec.Env["TEEPIN_UPLOAD_URL"], wantUploadURL)
	}
	if spec.Env["TEEPIN_TOKEN"] != "fake-agent-token" {
		t.Errorf("capture pod did not receive its upload token: %+v", spec.Env)
	}
	if spec.Labels[agentLabel] != "true" {
		t.Error("capture pod is missing the label that hides it from the customer's Compute list")
	}
	if spec.ImagePullSecret != "teepin-kumbha-ecr" {
		t.Errorf("ImagePullSecret = %q, want the agent image's own secret reused", spec.ImagePullSecret)
	}
	if !spec.NeverRestart {
		t.Error("capture pod spec did not set NeverRestart")
	}
	if len(fc.deleted) != 1 || fc.deleted[0] != spec.InstanceID {
		t.Errorf("capture pod was not torn down: deleted = %v, want [%s]", fc.deleted, spec.InstanceID)
	}
}

// TestGateway_CaptureScreenshot_FailurePropagatesAndStillCleansUp proves
// a failed capture pod both returns an error (so the caller's WARN log
// says why) AND is still deleted — a failed thumbnail capture must never
// leak a pod, the same posture as a failed build.
func TestGateway_CaptureScreenshot_FailurePropagatesAndStillCleansUp(t *testing.T) {
	store, _ := newMockStore(t)
	fc := &fakeCluster{statusResult: &cluster.InstanceStatus{Status: "failed", Message: "chrome crashed"}}
	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{}).
		WithAgent(fc, fakeMintToken, AgentConfig{Image: "kumbha-agent:latest", APIBaseURL: "http://api.internal"})

	sess := &Session{ID: uuid.New(), AccountID: uuid.New(), ProjectID: uuid.New()}
	err := gw.CaptureScreenshot(context.Background(), sess, "https://inst-abc123.teepin.com")
	if err == nil {
		t.Fatal("got nil error for a failed capture pod, want one")
	}
	if len(fc.created) != 1 {
		t.Fatalf("got %d CreateInstance calls, want 1", len(fc.created))
	}
	if len(fc.deleted) != 1 || fc.deleted[0] != fc.created[0].InstanceID {
		t.Errorf("capture pod was not torn down after failing: deleted = %v", fc.deleted)
	}
}

// --- StopAgent ---

func TestGateway_StopAgent_NotConfiguredWithoutWithAgent(t *testing.T) {
	store, _ := newMockStore(t)
	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{})

	sess := &Session{ID: uuid.New(), AccountID: uuid.New(), ProjectID: uuid.New(), AgentInstanceID: "kumbha-agent-abcd1234"}
	if err := gw.StopAgent(context.Background(), sess); !errors.Is(err, ErrAgentNotConfigured) {
		t.Errorf("got %v, want ErrAgentNotConfigured", err)
	}
}

func TestGateway_StopAgent_NoAgentRunningIsErrAgentNotRunning(t *testing.T) {
	store, _ := newMockStore(t)
	// fakeCluster's default GetInstanceStatus (no statusResult/statusErr
	// set) reports cluster.ErrNotFound, which isAgentRunning maps to
	// (false, nil) — no pod to interrupt.
	fc := &fakeCluster{}
	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{}).
		WithAgent(fc, fakeMintToken, AgentConfig{})

	sess := &Session{ID: uuid.New(), AccountID: uuid.New(), ProjectID: uuid.New(), AgentInstanceID: "kumbha-agent-abcd1234"}
	if err := gw.StopAgent(context.Background(), sess); !errors.Is(err, ErrAgentNotRunning) {
		t.Errorf("got %v, want ErrAgentNotRunning", err)
	}
	if len(fc.deleted) != 0 {
		t.Errorf("DeleteInstance was called with no agent actually running: %v", fc.deleted)
	}
}

// TestGateway_StopAgent_KillsRunningPod is the whole point of Stop: a
// live agent pod gets torn down immediately (hard kill, not a graceful
// pause — see StopAgent's own doc comment on why), and the session is
// left otherwise untouched — no status change, nothing that would block
// a later message.
func TestGateway_StopAgent_KillsRunningPod(t *testing.T) {
	store, _ := newMockStore(t)
	agentPodID := "kumbha-agent-abcd1234"
	fc := &fakeCluster{statusResult: &cluster.InstanceStatus{Status: "running"}}
	gw := NewGateway(store, NewRouter(nil), nil, &fakePricing{}, &fakeUsageRecorder{}).
		WithAgent(fc, fakeMintToken, AgentConfig{})

	sess := &Session{ID: uuid.New(), AccountID: uuid.New(), ProjectID: uuid.New(), AgentInstanceID: agentPodID}
	if err := gw.StopAgent(context.Background(), sess); err != nil {
		t.Fatalf("StopAgent: %v", err)
	}
	if len(fc.deleted) != 1 || fc.deleted[0] != agentPodID {
		t.Errorf("agent pod was not torn down: deleted = %v, want [%s]", fc.deleted, agentPodID)
	}
}
