// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build migrationdrill

// Runs the stop-and-hold billing windows against a real Postgres. See
// credits_integration_test.go for how to run it.
package billing

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type usageRow struct {
	resource string
	hours    float64
	cost     float64
	start    time.Time
	end      time.Time
}

// uniq makes an instance id unique per test run, so reruns against the same
// disposable database do not collide.
var runSuffix = uuid.NewString()[:8]

func uniq(name string) string { return name + "-" + runSuffix }

func usageOf(t *testing.T, db *sql.DB, instanceID string) []usageRow {
	t.Helper()
	rows, err := db.Query(`SELECT resource_type, quantity, total_cost, start_time, end_time
		FROM billing.usage_records WHERE instance_id = $1 ORDER BY start_time`, instanceID)
	if err != nil {
		t.Fatalf("usage query: %v", err)
	}
	defer rows.Close()
	var out []usageRow
	for rows.Next() {
		var u usageRow
		if err := rows.Scan(&u.resource, &u.hours, &u.cost, &u.start, &u.end); err != nil {
			t.Fatal(err)
		}
		out = append(out, u)
	}
	return out
}

func near(a, b, tol float64) bool { d := a - b; return d < tol && d > -tol }

// setInstanceTimes rewrites an instance's lifecycle columns.
func setInstance(t *testing.T, db *sql.DB, id, status string, createdAgo, stoppedAgo, resumedAgo time.Duration) {
	t.Helper()
	q := `UPDATE compute.instances SET status = $2, created_at = NOW() - make_interval(secs => $3),
		stopped_at = CASE WHEN $4 >= 0 THEN NOW() - make_interval(secs => $4) END,
		resumed_at = CASE WHEN $5 >= 0 THEN NOW() - make_interval(secs => $5) END WHERE id = $1`
	ago := func(d time.Duration) float64 {
		if d < 0 {
			return -1
		}
		return d.Seconds()
	}
	if _, err := db.Exec(q, id, status, createdAgo.Seconds(), ago(stoppedAgo), ago(resumedAgo)); err != nil {
		t.Fatalf("set instance: %v", err)
	}
}

func rates(t *testing.T, db *sql.DB) {
	t.Helper()
	// Seeded instances have no real project or user behind them; drop the
	// usage records' foreign keys to those on this disposable database only.
	rows, err := db.Query(`SELECT conname FROM pg_constraint
		WHERE contype = 'f' AND conrelid = 'billing.usage_records'::regclass
		  AND (confrelid::regclass::text LIKE 'auth.%' OR confrelid::regclass::text LIKE 'compute.%')`)
	if err != nil {
		t.Fatalf("list fks: %v", err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	rows.Close()
	for _, n := range names {
		if _, err := db.Exec(`ALTER TABLE billing.usage_records DROP CONSTRAINT ` + n); err != nil {
			t.Fatalf("drop fk: %v", err)
		}
	}
	// $2/hour compute for 20GB, $0.10 per GB-hour of disk (73 per GB-month).
	if _, err := db.Exec(`UPDATE billing.pricing SET vram_price_per_gb_hour = 0.10, storage_price_per_gb_month = 73`); err != nil {
		t.Fatalf("rates: %v", err)
	}
}

// Stopping bills the running interval up to the stop and nothing for the time
// the instance was not running; the disk is then billed like any storage,
// except while a hold is active.
func TestStopHoldIntegration_BillingWindows(t *testing.T) {
	db := integrationDB(t)
	s := NewService(db)
	ctx := context.Background()
	rates(t, db)
	c := NewUsageCollector(db, s)

	// 1. Ran 30 minutes, stopped 30 minutes ago. The tail is billed at the
	// full rate ($2 compute + $5 for a 50GB disk = $7/hour), the stopped
	// half hour at the disk rate only ($5/hour).
	acct := itAccount(t, db)
	itLot(t, db, acct, "purchase", 100, 0, nil)
	itInstance(t, ctx, s, acct, uniq("it-stop-1"), 20, 50) // created 30m ago by the helper
	setInstance(t, db, uniq("it-stop-1"), "stopped", 60*time.Minute, 30*time.Minute, -1)
	if err := c.CollectAccount(ctx, acct); err != nil {
		t.Fatal(err)
	}
	u := usageOf(t, db, uniq("it-stop-1"))
	if len(u) != 2 {
		t.Fatalf("usage rows = %d (%+v), want the running tail and the stopped disk", len(u), u)
	}
	if !near(u[0].hours, 0.5, 0.01) || !near(u[0].cost, 3.5, 0.05) || strings.Contains(u[0].resource, "stopped") {
		t.Errorf("running tail = %+v, want 0.5h at $7/h = $3.50", u[0])
	}
	if !strings.Contains(u[1].resource, "stopped, disk only") || !near(u[1].hours, 0.5, 0.02) || !near(u[1].cost, 2.5, 0.06) {
		t.Errorf("stopped disk = %+v, want 0.5h at $5/h = $2.50 disk only", u[1])
	}
	if !near(u[1].start.Sub(u[0].end).Seconds(), 0, 2) {
		t.Errorf("gap between the running tail and the disk interval: %v -> %v", u[0].end, u[1].start)
	}

	// 2. While a storage hold is active nothing more is billed.
	if _, err := s.StartStorageHold(ctx, acct, DefaultStorageHoldPeriod); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE billing.usage_records SET end_time = end_time - INTERVAL '20 minutes' WHERE instance_id = $1`, uniq("it-stop-1")); err != nil {
		t.Fatal(err)
	}
	if err := c.CollectAccount(ctx, acct); err != nil {
		t.Fatal(err)
	}
	if n := len(usageOf(t, db, uniq("it-stop-1"))); n != 2 {
		t.Errorf("a held disk was billed: %d usage rows", n)
	}
}

// After a hold ends, disk billing resumes from the release moment; the held
// days are never back-charged.
func TestStopHoldIntegration_HeldPeriodIsNeverBackBilled(t *testing.T) {
	db := integrationDB(t)
	s := NewService(db)
	ctx := context.Background()
	rates(t, db)
	c := NewUsageCollector(db, s)

	acct := itAccount(t, db)
	itLot(t, db, acct, "purchase", 100, 0, nil)
	itInstance(t, ctx, s, acct, uniq("it-stop-2"), 20, 50)
	// Ran 10 minutes, stopped 30 minutes ago, hold lasted until 10 minutes ago.
	setInstance(t, db, uniq("it-stop-2"), "stopped", 40*time.Minute, 30*time.Minute, -1)
	if _, err := db.Exec(`INSERT INTO billing.storage_holds (account_id, held_since, delete_after, released_at)
		VALUES ($1, NOW() - INTERVAL '30 minutes', NOW() + INTERVAL '7 days', NOW() - INTERVAL '10 minutes')`, acct); err != nil {
		t.Fatal(err)
	}
	if err := c.CollectAccount(ctx, acct); err != nil {
		t.Fatal(err)
	}
	var disk *usageRow
	for _, r := range usageOf(t, db, uniq("it-stop-2")) {
		r := r
		if strings.Contains(r.resource, "stopped, disk only") {
			disk = &r
		}
	}
	if disk == nil {
		t.Fatal("no disk interval billed after the hold ended")
	}
	if !near(disk.hours, 10.0/60, 0.02) || !near(disk.cost, 5.0*10/60, 0.06) {
		t.Errorf("disk billed %+v; want only the last 10 minutes ($0.83), not the held 20", *disk)
	}
}

// A restarted instance is billed from the restart, not for the stopped gap
// since its last usage record.
func TestStopHoldIntegration_RestartDoesNotBillTheStoppedGap(t *testing.T) {
	db := integrationDB(t)
	s := NewService(db)
	ctx := context.Background()
	rates(t, db)
	c := NewUsageCollector(db, s)

	acct := itAccount(t, db)
	itLot(t, db, acct, "purchase", 100, 0, nil)
	itInstance(t, ctx, s, acct, uniq("it-stop-3"), 20, 50)
	// Created 3h ago, last billed 3h ago (the row was just created with no
	// usage), stopped and restarted; running again for the last 5 minutes.
	setInstance(t, db, uniq("it-stop-3"), "running", 3*time.Hour, -1, 5*time.Minute)
	if err := c.CollectAccount(ctx, acct); err != nil {
		t.Fatal(err)
	}
	u := usageOf(t, db, uniq("it-stop-3"))
	if len(u) != 1 {
		t.Fatalf("usage rows = %d (%+v), want one", len(u), u)
	}
	if !near(u[0].hours, 5.0/60, 0.01) || !near(u[0].cost, 7.0*5/60, 0.05) {
		t.Errorf("billed %+v; want the 5 minutes since restart ($0.58), not the 3-hour stopped gap", u[0])
	}
}

// The enforcer's projection also starts from the restart, so a restarted
// instance is not judged to have burned credit while it was stopped.
func TestStopHoldIntegration_EnforcerProjectionStartsAtRestart(t *testing.T) {
	db := integrationDB(t)
	s := NewService(db)
	ctx := context.Background()
	rates(t, db)

	acct := itAccount(t, db)
	itLot(t, db, acct, "purchase", 3, 0, nil) // $3: fine for 5 minutes at $7/h, not for 3 hours
	itInstance(t, ctx, s, acct, uniq("it-stop-4"), 20, 50)
	setInstance(t, db, uniq("it-stop-4"), "running", 3*time.Hour, -1, 5*time.Minute)

	stopper := &fakeStopper{}
	NewCreditEnforcer(db, s, stopper, nil, EnforceOn).Tick(ctx)
	// Other tests' accounts share this database; only this instance matters.
	for _, calls := range append(append([][]string{}, stopper.calls...), stopper.holds...) {
		for _, id := range calls {
			if id == uniq("it-stop-4") {
				t.Errorf("a freshly restarted instance was ended: stops=%v holds=%v", stopper.calls, stopper.holds)
			}
		}
	}
}
