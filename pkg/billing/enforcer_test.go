// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

// gpuInst is a GPU instance of vramGB, so at a rate of R it costs vramGB*R
// per hour.
func gpuInst(id string, account uuid.UUID, vramGB, storageGB int) billableInstance {
	return billableInstance{ID: id, AccountID: account, GPUVRAMGB: vramGB, StorageGB: storageGB}
}

func TestParseEnforcementMode(t *testing.T) {
	cases := map[string]EnforcementMode{
		"":         EnforceOn,
		"enforce":  EnforceOn,
		" ENFORCE": EnforceOn,
		"dry-run":  EnforceDryRun,
		"off":      EnforceOff,
	}
	for in, want := range cases {
		got, err := ParseEnforcementMode(in)
		if err != nil || got != want {
			t.Errorf("ParseEnforcementMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseEnforcementMode("enforced"); err == nil {
		t.Error("a typo must be an error, not a silent fallback")
	}
}

func TestAssess_ProjectsUnmeteredTime(t *testing.T) {
	acct := uuid.New()
	now := time.Now()
	rates := computeRates{vramPerGBHour: 0.10}
	// 20GB at $0.10/GB-hour = $2.00/hour, metered up to 30 minutes ago.
	insts := []billableInstance{gpuInst("a", acct, 20, 0)}
	through := map[string]time.Time{"a": now.Add(-30 * time.Minute)}

	a := assess(5.00, insts, through, rates, now)

	if a.Burn != 2.00 {
		t.Errorf("burn = %v, want 2.00", a.Burn)
	}
	if got := a.Accrued; got < 0.99 || got > 1.01 {
		t.Errorf("accrued = %v, want about 1.00 (30 min at $2/hour)", got)
	}
	if got := a.Remaining; got < 3.99 || got > 4.01 {
		t.Errorf("remaining = %v, want about 4.00", got)
	}
}

func TestAssess_FreeInstancesNeverCount(t *testing.T) {
	acct := uuid.New()
	now := time.Now()
	// All rates zero: an unpriced (free) instance.
	a := assess(0, []billableInstance{{ID: "free", AccountID: acct, CPUUnits: 4, MemoryGB: 8}},
		map[string]time.Time{"free": now.Add(-time.Hour)}, computeRates{}, now)

	if a.Burn != 0 || len(a.Paid) != 0 {
		t.Fatalf("free instance counted as paid: %+v", a)
	}
	if a.ExhaustedWithin(enforcementHorizon) {
		t.Error("an account with only free compute must never be treated as exhausted, even at $0")
	}
}

func TestExhaustedWithin_Horizon(t *testing.T) {
	// $6/hour burn: the 2-minute horizon is $0.20.
	base := accountAssessment{Burn: 6.00}
	cases := []struct {
		remaining float64
		want      bool
	}{
		{5.00, false},
		{0.21, false},
		{0.20, true},
		{0.05, true},
		{0, true},
		{-1, true},
	}
	for _, c := range cases {
		a := base
		a.Remaining = c.remaining
		if got := a.ExhaustedWithin(enforcementHorizon); got != c.want {
			t.Errorf("remaining %.2f: exhausted=%v, want %v", c.remaining, got, c.want)
		}
	}
}

// fakeStopper records what the enforcer asks to stop.
type fakeStopper struct {
	calls       [][]string
	holds       [][]string
	failing     bool
	holdFailing bool
}

func (f *fakeStopper) HoldInstances(_ context.Context, _ uuid.UUID, ids []string) ([]string, error) {
	f.holds = append(f.holds, ids)
	if f.holdFailing {
		return nil, errors.New("agent cannot stop with the disk kept")
	}
	return ids, nil
}

func (f *fakeStopper) StopInstances(_ context.Context, _ uuid.UUID, ids []string) ([]string, error) {
	f.calls = append(f.calls, ids)
	if f.failing {
		return nil, errors.New("cluster unreachable")
	}
	return ids, nil
}

type fakeSettler struct{ calls []uuid.UUID }

func (f *fakeSettler) CollectAccount(_ context.Context, id uuid.UUID) error {
	f.calls = append(f.calls, id)
	return nil
}

func runningRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "account_id", "project_id", "instance_type_id", "gpu_vram_gb",
		"cpu_units", "memory_gb", "storage_gb", "p_cores_used", "e_cores_used",
		"created_at", "terminated_at", "billed_through",
	})
}

func newTestEnforcer(t *testing.T, mode EnforcementMode) (*CreditEnforcer, sqlmock.Sqlmock, *fakeStopper, *fakeSettler) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	stopper, settler := &fakeStopper{}, &fakeSettler{}
	return NewCreditEnforcer(db, NewService(db), stopper, settler, mode), mock, stopper, settler
}

// expectTick queues the reads one Tick performs for a single account:
// running instances, the six rate reads, then the balance.
func expectTick(mock sqlmock.Sqlmock, acct uuid.UUID, rows *sqlmock.Rows, balance float64) {
	mock.ExpectQuery(`FROM compute\.instances i`).WillReturnRows(rows)
	expectPricingRead(mock, 0.10)
	mock.ExpectQuery(`billing\.credit_balance`).WithArgs(acct).
		WillReturnRows(sqlmock.NewRows([]string{"b"}).AddRow(balance))
}

func TestTick_StopsDisklessInstancesWhenCreditGone(t *testing.T) {
	e, mock, stopper, settler := newTestEnforcer(t, EnforceOn)
	acct := uuid.New()
	now := time.Now()
	// 20GB = $2/hour, metered 30 minutes ago => $1.00 accrued. A $1.05
	// balance leaves $0.05, under the 2-minute horizon ($0.067).
	rows := runningRows().AddRow("i-1", acct, uuid.New(), "", 20, 0, 0, 0, nil, nil, now.Add(-time.Hour), nil, now.Add(-30*time.Minute))
	expectTick(mock, acct, rows, 1.05)

	e.Tick(context.Background())

	if len(stopper.calls) != 1 || len(stopper.calls[0]) != 1 || stopper.calls[0][0] != "i-1" {
		t.Fatalf("stopper calls = %v, want one call stopping i-1", stopper.calls)
	}
	if len(settler.calls) != 1 || settler.calls[0] != acct {
		t.Errorf("final stretch not settled immediately: %v", settler.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestTick_LeavesAffordableAccountsAlone(t *testing.T) {
	e, mock, stopper, _ := newTestEnforcer(t, EnforceOn)
	acct := uuid.New()
	now := time.Now()
	rows := runningRows().AddRow("i-1", acct, uuid.New(), "", 20, 0, 0, 0, nil, nil, now.Add(-time.Hour), nil, now.Add(-time.Minute))
	expectTick(mock, acct, rows, 50.00)

	e.Tick(context.Background())

	if len(stopper.calls) != 0 {
		t.Fatalf("stopped an account with plenty of credit: %v", stopper.calls)
	}
}

func TestTick_DryRunStopsNothing(t *testing.T) {
	e, mock, stopper, settler := newTestEnforcer(t, EnforceDryRun)
	acct := uuid.New()
	now := time.Now()
	rows := runningRows().AddRow("i-1", acct, uuid.New(), "", 20, 0, 0, 0, nil, nil, now.Add(-time.Hour), nil, now.Add(-30*time.Minute))
	expectTick(mock, acct, rows, 0)

	e.Tick(context.Background())

	if len(stopper.calls) != 0 || len(settler.calls) != 0 {
		t.Fatalf("dry-run acted: stop=%v settle=%v", stopper.calls, settler.calls)
	}
}

func TestTick_HoldsDiskBackedInstancesInsteadOfDeletingThem(t *testing.T) {
	e, mock, stopper, settler := newTestEnforcer(t, EnforceOn)
	acct := uuid.New()
	now := time.Now()
	// i-disk has a 50GB persistent disk; i-plain does not. Both are out of credit.
	rows := runningRows().
		AddRow("i-disk", acct, uuid.New(), "", 20, 0, 0, 50, nil, nil, now.Add(-time.Hour), nil, now.Add(-30*time.Minute)).
		AddRow("i-plain", acct, uuid.New(), "", 20, 0, 0, 0, nil, nil, now.Add(-time.Hour), nil, now.Add(-30*time.Minute))
	expectTick(mock, acct, rows, 0)

	e.Tick(context.Background())

	if len(stopper.calls) != 1 || len(stopper.calls[0]) != 1 || stopper.calls[0][0] != "i-plain" {
		t.Fatalf("stop calls = %v, want only i-plain (a delete of i-disk would destroy the customer's data)", stopper.calls)
	}
	if len(stopper.holds) != 1 || len(stopper.holds[0]) != 1 || stopper.holds[0][0] != "i-disk" {
		t.Fatalf("hold calls = %v, want i-disk held (disk kept)", stopper.holds)
	}
	if len(settler.calls) != 1 {
		t.Errorf("final stretch not settled: %v", settler.calls)
	}
}

// A disk-backed instance that cannot be held (old agent, no stored spec) is
// left running and reported - never deleted - and is not settled as stopped.
func TestTick_UnholdableInstanceKeepsRunningAndIsReported(t *testing.T) {
	e, mock, stopper, settler := newTestEnforcer(t, EnforceOn)
	stopper.holdFailing = true
	acct := uuid.New()
	now := time.Now()
	rows := runningRows().AddRow("i-disk", acct, uuid.New(), "", 20, 0, 0, 50, nil, nil, now.Add(-time.Hour), nil, now.Add(-30*time.Minute))
	expectTick(mock, acct, rows, 0)

	e.Tick(context.Background())

	if len(stopper.calls) != 0 {
		t.Fatalf("disk-backed instance was deleted: %v", stopper.calls)
	}
	if _, ok := e.heldLogged["i-disk"]; !ok {
		t.Error("the failure to hold was not reported")
	}
	if len(settler.calls) != 0 {
		t.Error("settled as if stopped though it is still running")
	}
}

func TestTick_UnreadableBalanceKeepsWorkloadsRunning(t *testing.T) {
	e, mock, stopper, _ := newTestEnforcer(t, EnforceOn)
	acct := uuid.New()
	now := time.Now()
	rows := runningRows().AddRow("i-1", acct, uuid.New(), "", 20, 0, 0, 0, nil, nil, now.Add(-time.Hour), nil, now.Add(-30*time.Minute))
	mock.ExpectQuery(`FROM compute\.instances i`).WillReturnRows(rows)
	expectPricingRead(mock, 0.10)
	mock.ExpectQuery(`billing\.credit_balance`).WithArgs(acct).WillReturnError(errors.New("db down"))

	e.Tick(context.Background())

	if len(stopper.calls) != 0 {
		t.Fatalf("an unreadable balance is not proof of an empty one; stopped %v", stopper.calls)
	}
}

func TestTick_FailedStopIsNotSettled(t *testing.T) {
	e, mock, stopper, settler := newTestEnforcer(t, EnforceOn)
	stopper.failing = true
	acct := uuid.New()
	now := time.Now()
	rows := runningRows().AddRow("i-1", acct, uuid.New(), "", 20, 0, 0, 0, nil, nil, now.Add(-time.Hour), nil, now.Add(-30*time.Minute))
	expectTick(mock, acct, rows, 0)

	e.Tick(context.Background())

	if len(stopper.calls) != 1 {
		t.Fatalf("expected one stop attempt, got %v", stopper.calls)
	}
	if len(settler.calls) != 0 {
		t.Error("settled usage for an instance that is still running")
	}
}

func TestReportHeld_RateLimited(t *testing.T) {
	e, _, _, _ := newTestEnforcer(t, EnforceOn)
	clock := time.Now()
	e.now = func() time.Time { return clock }
	inst := gpuInst("i-disk", uuid.New(), 20, 50)

	cause := errors.New("no agent")
	e.reportHeld(inst.AccountID, []string{"i-disk"}, nil, accountAssessment{}, cause)
	first := e.heldLogged["i-disk"]
	clock = clock.Add(10 * time.Minute)
	e.reportHeld(inst.AccountID, []string{"i-disk"}, nil, accountAssessment{}, cause)
	if !e.heldLogged["i-disk"].Equal(first) {
		t.Error("held error re-logged within the rate-limit window")
	}
	clock = clock.Add(2 * time.Hour)
	e.reportHeld(inst.AccountID, []string{"i-disk"}, nil, accountAssessment{}, cause)
	if e.heldLogged["i-disk"].Equal(first) {
		t.Error("held error never re-logged after the window")
	}
}
