// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build migrationdrill

// Runs the plan-approval and event-store SQL against a real Postgres (migration
// 068). The mocked tests check the shape of each query; this checks that the
// queries and the unique key actually behave. Behind the migration-drill tag:
//
//	TEEPIN_DRILL_DSN='postgres://postgres:test@localhost:55444/teepin?sslmode=disable' \
//	  go test -tags migrationdrill -run 'TestPlansIntegration|TestEventsIntegration' ./pkg/teepinbuild/ -v
package teepinbuild

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func res(name string, cpu, mem, storage int) PlanResource {
	return PlanResource{Name: name, CPUUnits: cpu, MemoryGB: mem, StorageGB: storage}
}

func TestPlansIntegration(t *testing.T) {
	db := secretsIntegrationDB(t)
	ctx := context.Background()
	store := NewStore(db)
	acct, sess := seedSession(t, db, uuid.NewString()[:8])
	otherAcct, otherSess := seedSession(t, db, uuid.NewString()[:8])

	// Nothing approved yet: the plan gate has nothing to say (the
	// deploy_approved flag, checked separately, refuses).
	if err := store.CheckApprovedFor(ctx, sess, 8, 8, 0); err != nil {
		t.Fatalf("no plan bound: CheckApprovedFor = %v, want nil", err)
	}

	small, err := store.RecordPlan(ctx, sess, acct, []PlanResource{res("app", 1, 2, 0)})
	if err != nil {
		t.Fatalf("record small plan: %v", err)
	}
	big, err := store.RecordPlan(ctx, sess, acct, []PlanResource{res("app", 4, 8, 10), res("worker", 2, 4, 0)})
	if err != nil {
		t.Fatalf("record big plan: %v", err)
	}

	// A plan belonging to another session cannot be approved here, even with
	// its exact id.
	foreign, err := store.RecordPlan(ctx, otherSess, otherAcct, []PlanResource{res("x", 64, 64, 0)})
	if err != nil {
		t.Fatalf("record foreign plan: %v", err)
	}
	if err := store.ApprovePlan(ctx, sess, acct, foreign); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("approving another session's plan: err = %v, want ErrPlanNotFound", err)
	}
	// ...and another account cannot approve this session's plan.
	if err := store.ApprovePlan(ctx, sess, otherAcct, small); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("approving as another account: err = %v, want ErrSessionNotFound", err)
	}

	// Approve the SMALL plan by id although a bigger one was presented later.
	if err := store.ApprovePlan(ctx, sess, acct, small); err != nil {
		t.Fatalf("approve small: %v", err)
	}
	got, err := store.Get(ctx, sess, acct)
	if err != nil || !got.DeployApproved {
		t.Fatalf("session after approve: %+v, err %v; want deploy_approved", got, err)
	}
	for _, tc := range []struct {
		name         string
		cpu, mem, st int
		wantErr      error
	}{
		{"exactly the approved size", 1, 2, 0, nil},
		{"smaller", 1, 1, 0, nil},
		{"more CPU", 2, 2, 0, ErrExceedsApprovedPlan},
		{"more memory", 1, 3, 0, ErrExceedsApprovedPlan},
		{"storage the plan does not include", 1, 2, 5, ErrExceedsApprovedPlan},
		{"the unapproved bigger plan's size", 4, 8, 10, ErrExceedsApprovedPlan},
	} {
		if err := store.CheckApprovedFor(ctx, sess, tc.cpu, tc.mem, tc.st); !errors.Is(err, tc.wantErr) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.wantErr)
		}
	}

	// Approving the bigger plan raises the limit; a resource fits if it fits
	// any ONE line.
	if err := store.ApprovePlan(ctx, sess, acct, big); err != nil {
		t.Fatalf("approve big: %v", err)
	}
	if err := store.CheckApprovedFor(ctx, sess, 4, 8, 10); err != nil {
		t.Errorf("approved big plan, app line: %v", err)
	}
	if err := store.CheckApprovedFor(ctx, sess, 2, 4, 0); err != nil {
		t.Errorf("approved big plan, worker line: %v", err)
	}
	if err := store.CheckApprovedFor(ctx, sess, 4, 8, 11); !errors.Is(err, ErrExceedsApprovedPlan) {
		t.Errorf("one figure over every line must be refused: %v", err)
	}

	// An older console sends no id: the newest plan is approved.
	acct2, sess2 := seedSession(t, db, uuid.NewString()[:8])
	if _, err := store.RecordPlan(ctx, sess2, acct2, []PlanResource{res("a", 1, 1, 0)}); err != nil {
		t.Fatal(err)
	}
	newest, err := store.RecordPlan(ctx, sess2, acct2, []PlanResource{res("b", 3, 3, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ApprovePlan(ctx, sess2, acct2, uuid.Nil); err != nil {
		t.Fatalf("approve newest: %v", err)
	}
	var bound uuid.UUID
	if err := db.QueryRow(`SELECT approved_plan_id FROM billing.inference_sessions WHERE id = $1`, sess2).Scan(&bound); err != nil || bound != newest {
		t.Errorf("approved_plan_id = %s (err %v), want the newest plan %s", bound, err, newest)
	}

	// An agent image from before plans were recorded: no plan exists, approval
	// still works and nothing is bound (legacy behaviour).
	acct3, sess3 := seedSession(t, db, uuid.NewString()[:8])
	if err := store.ApprovePlan(ctx, sess3, acct3, uuid.Nil); err != nil {
		t.Fatalf("legacy approve: %v", err)
	}
	if err := store.CheckApprovedFor(ctx, sess3, 64, 64, 64); err != nil {
		t.Errorf("legacy approval has no plan to enforce: %v", err)
	}

	// Content and cap validation, and a closed session.
	if _, err := store.RecordPlan(ctx, sess3, acct3, nil); !errors.Is(err, ErrInvalidPlan) {
		t.Errorf("empty plan: %v", err)
	}
	if _, err := store.RecordPlan(ctx, sess3, acct3, []PlanResource{res("z", 0, 0, 0)}); !errors.Is(err, ErrInvalidPlan) {
		t.Errorf("a resource that asks for nothing: %v", err)
	}
	if _, err := store.RecordPlan(ctx, sess3, acct3, []PlanResource{res("z", maxPlanUnits+1, 1, 0)}); !errors.Is(err, ErrInvalidPlan) {
		t.Errorf("absurd figure: %v", err)
	}
	if _, err := db.Exec(`UPDATE billing.inference_sessions SET status = 'closed' WHERE id = $1`, sess3); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordPlan(ctx, sess3, acct3, []PlanResource{res("z", 1, 1, 0)}); !errors.Is(err, ErrSessionClosed) {
		t.Errorf("closed session: %v", err)
	}
	if err := store.ApprovePlan(ctx, sess3, acct3, uuid.Nil); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("approving on a closed session: %v", err)
	}

	// Deleting the approved plan's row must not strand the session.
	if _, err := db.Exec(`DELETE FROM billing.build_plans WHERE id = $1`, big); err != nil {
		t.Fatalf("delete plan: %v", err)
	}
	if err := store.CheckApprovedFor(ctx, sess, 1, 1, 0); err != nil {
		t.Errorf("after the approved plan row is gone: %v", err)
	}
}

func TestEventsIntegration(t *testing.T) {
	db := secretsIntegrationDB(t)
	ctx := context.Background()
	store := NewStore(db)
	_, sess := seedSession(t, db, uuid.NewString()[:8])
	_, other := seedSession(t, db, uuid.NewString()[:8])

	// Launch counting.
	if n, err := store.AgentLaunchSeq(ctx, sess); err != nil || n != 0 {
		t.Fatalf("fresh session launch seq = %d, %v; want 0", n, err)
	}
	for want := 1; want <= 2; want++ {
		if n, err := store.BumpLaunchSeq(ctx, sess); err != nil || n != want {
			t.Fatalf("bump #%d = %d, %v", want, n, err)
		}
	}

	ev := func(s string) json.RawMessage { return json.RawMessage(`{"type":"action","summary":"` + s + `"}`) }

	// Launch 1: three lines, recorded twice (the pod's log was replayed): still
	// three rows.
	for pass := 0; pass < 2; pass++ {
		for i, s := range []string{"a", "b", "c"} {
			if err := store.RecordEvent(ctx, sess, 1, i+1, ev(s)); err != nil {
				t.Fatalf("record: %v", err)
			}
		}
	}
	// Launch 2 reuses the same line numbers: they are different events.
	if err := store.RecordEvent(ctx, sess, 2, 1, ev("d")); err != nil {
		t.Fatal(err)
	}
	// Another session's events never mix in.
	if err := store.RecordEvent(ctx, other, 1, 1, ev("foreign")); err != nil {
		t.Fatal(err)
	}
	// Past the per-launch cap: dropped, not an error.
	if err := store.RecordEvent(ctx, sess, 1, MaxEventLinesPerLaunch+1, ev("over")); err != nil {
		t.Fatal(err)
	}

	summaries := func(raws []json.RawMessage) string {
		out := ""
		for _, r := range raws {
			var m struct {
				Summary string `json:"summary"`
			}
			_ = json.Unmarshal(r, &m)
			out += m.Summary
		}
		return out
	}
	all, err := store.ListEvents(ctx, sess, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := summaries(all); got != "abcd" {
		t.Errorf("all events = %q, want abcd (in order, no duplicates, no foreign, no over-cap)", got)
	}
	first, _ := store.ListEvents(ctx, sess, 0, 1)
	if got := summaries(first); got != "abc" {
		t.Errorf("launch 1 = %q, want abc", got)
	}
	second, _ := store.ListEvents(ctx, sess, 2, 2)
	if got := summaries(second); got != "d" {
		t.Errorf("launch 2 = %q, want d", got)
	}
	if n, err := store.MaxRecordedLine(ctx, sess, 1); err != nil || n != 3 {
		t.Errorf("max recorded line launch 1 = %d, %v; want 3", n, err)
	}
	if n, _ := store.MaxRecordedLine(ctx, sess, 9); n != 0 {
		t.Errorf("max recorded line for an unknown launch = %d, want 0", n)
	}

	// Events go with the session.
	if _, err := db.Exec(`DELETE FROM billing.inference_sessions WHERE id = $1`, sess); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if left, _ := store.ListEvents(ctx, sess, 0, 9); len(left) != 0 {
		t.Errorf("%d events survived their session", len(left))
	}
}
