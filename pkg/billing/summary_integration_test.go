// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build migrationdrill

// Runs the Bills summary against a real Postgres. See
// credits_integration_test.go for how to run it.
package billing

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
)

func itUsageFull(t *testing.T, db *sql.DB, account, project uuid.UUID, instance, resource, unit string, qty, cost float64, at time.Time) {
	t.Helper()
	var inst any
	if instance != "" {
		inst = instance
	}
	if _, err := db.Exec(`
		INSERT INTO billing.usage_records
			(account_id, project_id, instance_id, subject_type, subject_id, resource_type, quantity, unit, unit_price, total_cost, start_time, end_time)
		VALUES ($1, $2, $3, 'instance', 'x', $4, $5, $6, 0, $7, $8, $8)`,
		account, project, inst, resource, qty, unit, cost, at); err != nil {
		t.Fatalf("seed usage: %v", err)
	}
}

func serviceLine(t *testing.T, p ProjectLine, service string) ServiceLine {
	t.Helper()
	for _, l := range p.Services {
		if l.Service == service {
			return l
		}
	}
	t.Fatalf("no %q line in %+v", service, p.Services)
	return ServiceLine{}
}

// The Bills page names services through the same catalog as the statement and
// the invoice. Untyped compute and object storage used to fall into "Other".
func TestSummaryIntegration_UsesTheServiceCatalog(t *testing.T) {
	db := integrationDB(t)
	s := NewService(db)
	ctx := context.Background()
	rates(t, db)

	acct := itAccount(t, db)
	owner := itOwner(t, db, acct)
	proj := itProject(t, db, acct, owner, "apps")
	at := time.Now().UTC().Add(-time.Hour)
	a, b, c := uniq("sum-a"), uniq("sum-b"), uniq("sum-c")

	// Two home instances, one of which also has a record under another cpu.* type.
	itUsageFull(t, db, acct, proj, a, "cpu.home", "hours", 10, 1.00, at)
	itUsageFull(t, db, acct, proj, b, "cpu.home", "hours", 5, 0.50, at)
	itUsageFull(t, db, acct, proj, b, "cpu.small", "hours", 2, 0.20, at)
	// Compute whose instance type was never recorded (legacy Kumbha apps).
	itUsageFull(t, db, acct, proj, c, "", "hours", 100, 0.30, at)
	// A stopped disk, and object storage: neither is "Other".
	itUsageFull(t, db, acct, proj, a, "cpu.home (stopped, disk only)", "hours", 1, 0.05, at)
	itUsageFull(t, db, acct, proj, "", "object_storage_gb_month", "gb", 3, 0.40, at)
	itUsageFull(t, db, acct, proj, "", "object_storage_gb_egress", "gb", 1, 0.09, at)
	itUsageFull(t, db, acct, proj, "", "kumbha/teepin/fast:input", "tokens", 1000, 0.02, at)

	sum, err := s.GetAccountSummary(ctx, acct, at.Add(-time.Hour), at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Projects) != 1 {
		t.Fatalf("projects = %+v", sum.Projects)
	}
	p := sum.Projects[0]

	for _, l := range p.Services {
		if l.Service == "Other" || l.Service == "Other charges" {
			t.Errorf("a line fell into %q: %+v", l.Service, l)
		}
	}

	cpu := serviceLine(t, p, "CPU compute")
	if !near(cpu.Quantity, 18, 1e-6) || !near(cpu.Cost, 1.75, 1e-6) {
		t.Errorf("CPU compute = %+v, want 18 hours / $1.75 (home, small and the stopped disk together)", cpu)
	}
	// Instances a and b, counted once each even though b has two resource types.
	if cpu.Instances != 2 {
		t.Errorf("CPU compute instances = %d, want 2", cpu.Instances)
	}
	untyped := serviceLine(t, p, "Compute")
	if !near(untyped.Quantity, 100, 1e-6) || untyped.Instances != 1 {
		t.Errorf("untyped compute = %+v, want 100 hours on 1 instance", untyped)
	}
	// Storage and transfer share the unit "gb", and the Bills page groups by
	// service and unit, so they are one "Object storage" line.
	if obj := serviceLine(t, p, "Object storage"); !near(obj.Cost, 0.49, 1e-6) {
		t.Errorf("object storage = %+v, want $0.49", obj)
	}
	if !near(sum.TotalCost, 2.56, 1e-6) || !near(p.Cost, 2.56, 1e-6) {
		t.Errorf("total = %v / project = %v, want 2.56", sum.TotalCost, p.Cost)
	}
}
