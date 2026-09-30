// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build migrationdrill

// Runs the monthly statement against a real Postgres. See
// credits_integration_test.go for how to run it.
package billing

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
)

func itProject(t *testing.T, db *sql.DB, account, owner uuid.UUID, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := db.Exec(`INSERT INTO auth.projects (id, owner_id, account_id, name, slug) VALUES ($1, $2, $3, $4, $5)`,
		id, owner, account, name, "p-"+id.String()[:8]); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return id
}

func itOwner(t *testing.T, db *sql.DB, account uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := db.QueryRow(`INSERT INTO auth.users (email, password_hash, account_id, role, status)
		VALUES ($1, 'x', $2, 'owner', 'active') RETURNING id`, "st-"+uuid.NewString()[:8]+"@example.test", account).Scan(&id); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	return id
}

// itUsage records a usage record and its consumption from credit at a moment.
func itUsage(t *testing.T, db *sql.DB, account, project uuid.UUID, resource, unit string, qty, cost float64, at time.Time) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := db.QueryRow(`
		INSERT INTO billing.usage_records
			(account_id, project_id, subject_type, subject_id, resource_type, quantity, unit, unit_price, total_cost, start_time, end_time)
		VALUES ($1, $2, 'instance', 'x', $3, $4, $5, 0, $6, $7, $7) RETURNING id`,
		account, project, resource, qty, unit, cost, at).Scan(&id); err != nil {
		t.Fatalf("seed usage: %v", err)
	}
	return id
}

// itLedger inserts a ledger row and returns its id. lot is the credit lot a
// consumption row was drawn from (real consumption rows always carry one).
func itLedger(t *testing.T, db *sql.DB, account uuid.UUID, kind string, amount float64, reason string, usage *uuid.UUID, at time.Time, lot ...uuid.UUID) uuid.UUID {
	t.Helper()
	var u, l any
	if usage != nil {
		u = *usage
	}
	if len(lot) > 0 {
		l = lot[0]
	}
	var id uuid.UUID
	if err := db.QueryRow(`INSERT INTO billing.credit_transactions (account_id, amount, kind, reason, usage_record_id, lot_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`, account, amount, kind, reason, u, l, at).Scan(&id); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}
	return id
}

func TestStatementIntegration_ReconcilesAndGroupsByProjectAndService(t *testing.T) {
	db := integrationDB(t)
	s := NewService(db)
	ctx := context.Background()
	rates(t, db) // also drops the usage_records foreign keys used by seeded data

	acct := itAccount(t, db)
	owner := itOwner(t, db, acct)
	web := itProject(t, db, acct, owner, "web")
	ml := itProject(t, db, acct, owner, "ml")

	now := time.Now().UTC()
	thisMonth := monthStartUTC(now)
	lastMonth := thisMonth.AddDate(0, -1, 0)
	inLast := lastMonth.Add(5 * 24 * time.Hour)
	inThis := thisMonth.Add(1 * time.Hour)

	// Last month: bought $100, used $30 -> closes at $70.
	lotLast := itLedger(t, db, acct, "purchase", 100, "Credit purchase", nil, inLast)
	u0 := itUsage(t, db, acct, web, "cpu.home", "hours", 10, 30, inLast)
	itLedger(t, db, acct, "consumption", -30, "usage", &u0, inLast.Add(time.Hour), lotLast)

	// This month: bought $50, granted $5, used $20 on GPU (project ml) and $10 on
	// storage (web), $3 expired.
	lotBuy := itLedger(t, db, acct, "purchase", 50, "Credit purchase", nil, inThis)
	lotGrant := itLedger(t, db, acct, "grant", 5, "goodwill", nil, inThis)
	// One GPU usage record of $20 drawn across two credit lots: two consumption
	// rows, but its 4 hours must be counted once.
	u1 := itUsage(t, db, acct, ml, "gpu.h100.mig-2g", "hours", 4, 20, inThis.Add(time.Hour))
	itLedger(t, db, acct, "consumption", -12, "usage", &u1, inThis.Add(time.Hour), lotGrant)
	itLedger(t, db, acct, "consumption", -8, "usage", &u1, inThis.Add(time.Hour), lotBuy)
	u2 := itUsage(t, db, acct, web, "object_storage_gb_month", "gb", 2, 10, inThis.Add(2*time.Hour))
	itLedger(t, db, acct, "consumption", -10, "usage", &u2, inThis.Add(2*time.Hour), lotBuy)
	itLedger(t, db, acct, "expiry", -3, "expired", nil, inThis.Add(3*time.Hour))

	// --- last month
	prev, err := s.Statement(ctx, acct, lastMonth.Format("2006-01"))
	if err != nil {
		t.Fatal(err)
	}
	if !near(prev.Opening, 0, 1e-6) || !near(prev.Purchased, 100, 1e-6) || !near(prev.Used, 30, 1e-6) || !near(prev.Closing, 70, 1e-6) {
		t.Fatalf("last month: %+v", prev)
	}

	// --- this month
	st, err := s.Statement(ctx, acct, thisMonth.Format("2006-01"))
	if err != nil {
		t.Fatal(err)
	}
	// The month opens with exactly what last month closed with.
	if !near(st.Opening, prev.Closing, 1e-6) {
		t.Errorf("opening %v != last month's closing %v", st.Opening, prev.Closing)
	}
	if !near(st.Purchased, 50, 1e-6) || !near(st.Granted, 5, 1e-6) || !near(st.Used, 30, 1e-6) || !near(st.Expired, 3, 1e-6) {
		t.Errorf("movements: %+v", st)
	}
	// closing = opening + added - used - expired = 70 + 55 - 30 - 3
	if !near(st.Closing, 92, 1e-6) {
		t.Errorf("closing = %v, want 92", st.Closing)
	}
	if len(st.Projects) != 2 || st.Projects[0].Name != "ml" || !near(st.Projects[0].Amount, 20, 1e-6) ||
		st.Projects[1].Name != "web" || !near(st.Projects[1].Amount, 10, 1e-6) {
		t.Fatalf("projects = %+v, want ml $20 then web $10", st.Projects)
	}
	gpu := st.Projects[0].Services[0]
	if gpu.Service != "GPU compute" || len(gpu.Resources) != 1 || !near(gpu.Resources[0].Quantity, 4, 1e-6) || !near(gpu.Resources[0].Amount, 20, 1e-6) {
		t.Errorf("gpu service = %+v, want one resource of 4 hours (not 8) costing $20", gpu)
	}
	if st.Projects[1].Services[0].Service != "Object storage" {
		t.Errorf("web service = %q", st.Projects[1].Services[0].Service)
	}

	// The CSV reconciles to the page: it starts from the opening balance and its
	// running balance ends at the closing balance.
	var lines []StatementLine
	if err := s.EachStatementLine(ctx, acct, thisMonth.Format("2006-01"), func(l StatementLine) error {
		lines = append(lines, l)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 6 {
		t.Fatalf("csv lines = %d, want 6 ledger rows", len(lines))
	}
	if !near(lines[len(lines)-1].Balance, st.Closing, 1e-6) {
		t.Errorf("last running balance %v != closing %v", lines[len(lines)-1].Balance, st.Closing)
	}
	for i := 1; i < len(lines); i++ {
		if lines[i].When.Before(lines[i-1].When) {
			t.Error("lines not in order")
		}
	}
	var sawGPU bool
	for _, l := range lines {
		if l.Kind == "consumption" && l.Project == "ml" && l.Service == "GPU compute" {
			sawGPU = true
		}
	}
	if !sawGPU {
		t.Error("consumption lines carry no project/service")
	}
}

// Another account's activity never appears.
func TestStatementIntegration_IsolatedPerAccount(t *testing.T) {
	db := integrationDB(t)
	s := NewService(db)
	ctx := context.Background()
	rates(t, db)
	a, b := itAccount(t, db), itAccount(t, db)
	at := monthStartUTC(time.Now()).Add(time.Hour)
	itLedger(t, db, a, "purchase", 40, "a", nil, at)
	itLedger(t, db, b, "purchase", 999, "b", nil, at)

	st, err := s.Statement(ctx, a, time.Now().UTC().Format("2006-01"))
	if err != nil {
		t.Fatal(err)
	}
	if !near(st.Purchased, 40, 1e-6) || !near(st.Closing, 40, 1e-6) {
		t.Errorf("account a sees %+v", st)
	}
}

func TestStatementIntegration_MonthsListStartsAtFirstActivity(t *testing.T) {
	db := integrationDB(t)
	s := NewService(db)
	rates(t, db)
	acct := itAccount(t, db)
	thisMonth := monthStartUTC(time.Now())
	itLedger(t, db, acct, "purchase", 10, "x", nil, thisMonth.AddDate(0, -2, 3))

	months, err := s.StatementMonths(context.Background(), acct)
	if err != nil {
		t.Fatal(err)
	}
	if len(months) != 3 || months[0] != thisMonth.Format("2006-01") || months[2] != thisMonth.AddDate(0, -2, 0).Format("2006-01") {
		t.Errorf("months = %v, want this month back to two months ago", months)
	}
}
