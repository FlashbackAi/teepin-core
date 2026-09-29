// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build migrationdrill

// Drill for migration 060 (credit lots): seeds pre-lots ledgers of the
// shapes that matter, migrates up, checks the backfill and the balance
// function, migrates down, and up again. Run against a disposable Postgres:
//
//	TEEPIN_DRILL_DSN='postgres://postgres:test@localhost:55433/teepin?sslmode=disable' \
//	  go test -tags migrationdrill -run TestCreditLotsDrill ./migrations/ -v
package migrations

import (
	"database/sql"
	"math"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec failed: %v\nquery: %s", err, q)
	}
}

func queryFloat(t *testing.T, db *sql.DB, q string, args ...any) float64 {
	t.Helper()
	var v sql.NullFloat64
	if err := db.QueryRow(q, args...).Scan(&v); err != nil {
		t.Fatalf("query failed: %v\nquery: %s", err, q)
	}
	return v.Float64
}

func queryInt(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var v int
	if err := db.QueryRow(q, args...).Scan(&v); err != nil {
		t.Fatalf("query failed: %v\nquery: %s", err, q)
	}
	return v
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func seedAccount(t *testing.T, db *sql.DB, number, alias string) string {
	t.Helper()
	var id string
	if err := db.QueryRow(`
		INSERT INTO auth.accounts (account_number, alias, type, display_name)
		VALUES ($1, $2, 'organization', $2) RETURNING id
	`, number, alias).Scan(&id); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	return id
}

func seedGrant(t *testing.T, db *sql.DB, account string, amount float64, createdDaysAgo int, expiresInDays *int) string {
	t.Helper()
	var id string
	var expires any
	if expiresInDays != nil {
		expires = *expiresInDays
	}
	if err := db.QueryRow(`
		INSERT INTO billing.credit_transactions (account_id, amount, kind, reason, expires_at, created_at)
		VALUES ($1, $2, 'grant', 'drill', CASE WHEN $4::int IS NULL THEN NULL ELSE NOW() + ($4::int * INTERVAL '1 day') END,
		        NOW() - ($3::int * INTERVAL '1 day'))
		RETURNING id
	`, account, amount, createdDaysAgo, expires).Scan(&id); err != nil {
		t.Fatalf("seed grant: %v", err)
	}
	return id
}

func seedConsumption(t *testing.T, db *sql.DB, account string, amount float64, usageID string, minutesAfterStart int) {
	t.Helper()
	mustExec(t, db, `
		INSERT INTO billing.credit_transactions (account_id, amount, kind, reason, usage_record_id, created_at)
		VALUES ($1, $2, 'consumption', 'usage', $3::uuid, NOW() - INTERVAL '30 days' + ($4::int * INTERVAL '1 minute'))
	`, account, -amount, usageID, minutesAfterStart)
}

func intp(n int) *int { return &n }

func TestCreditLotsDrill(t *testing.T) {
	dsn := os.Getenv("TEEPIN_DRILL_DSN")
	if dsn == "" {
		t.Skip("set TEEPIN_DRILL_DSN to run the migration drill")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	// Start from the schema as it was before lots.
	if err := migrator(t, db).Migrate(58); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 058: %v", err)
	}

	// The seeded usage_record_ids are not real usage records (a real one
	// needs a project, user and instance behind it). In production the
	// backfill's split rows copy an id that already satisfies this foreign
	// key, so it holds by construction; it is dropped here only so the
	// drill can exercise the arithmetic. Reset the database between runs.
	mustExec(t, db, `ALTER TABLE billing.credit_transactions DROP CONSTRAINT IF EXISTS credit_transactions_usage_record_id_fkey`)

	// A: the dev-account shape. A $100 grant that has already expired, with
	// $18.75 of spending drawn from it. Before lots this read as -$18.75.
	a := seedAccount(t, db, "000000000001", "drill-a")
	seedGrant(t, db, a, 100, 40, intp(-10))
	for i, u := range []string{
		"aaaaaaaa-0000-0000-0000-000000000001",
		"aaaaaaaa-0000-0000-0000-000000000002",
		"aaaaaaaa-0000-0000-0000-000000000003",
	} {
		seedConsumption(t, db, a, 6.25, u, i)
	}

	// B: spending that spans two grants. G1 ($10, expires in 30 days) is
	// drawn first, then G2 ($20, never expires). u2 (-8) must split 4/4.
	b := seedAccount(t, db, "000000000002", "drill-b")
	g1 := seedGrant(t, db, b, 10, 40, intp(30))
	g2 := seedGrant(t, db, b, 20, 40, nil)
	seedConsumption(t, db, b, 6, "bbbbbbbb-0000-0000-0000-000000000001", 0)
	seedConsumption(t, db, b, 8, "bbbbbbbb-0000-0000-0000-000000000002", 1)
	seedConsumption(t, db, b, 1, "bbbbbbbb-0000-0000-0000-000000000003", 2)

	// C: an unexpired grant, untouched.
	c := seedAccount(t, db, "000000000003", "drill-c")
	seedGrant(t, db, c, 50, 5, intp(20))

	// D: a grant that expired without ever being used.
	d := seedAccount(t, db, "000000000004", "drill-d")
	seedGrant(t, db, d, 25, 40, intp(-1))

	check := func(label string) {
		t.Helper()
		bal := func(acct string) float64 {
			return queryFloat(t, db, `SELECT billing.credit_balance($1)`, acct)
		}
		sum := func(acct string) float64 {
			return queryFloat(t, db, `SELECT SUM(amount) FROM billing.credit_transactions WHERE account_id = $1`, acct)
		}

		// A: forfeits only the unspent $81.25; balance exactly zero.
		if got := bal(a); !near(got, 0) {
			t.Errorf("%s: A balance = %v, want 0", label, got)
		}
		if got := sum(a); !near(got, 0) {
			t.Errorf("%s: A ledger sum = %v, want 0 (expiry row must offset the unspent grant)", label, got)
		}
		if got := queryFloat(t, db, `SELECT -amount FROM billing.credit_transactions WHERE account_id = $1 AND kind = 'expiry'`, a); !near(got, 81.25) {
			t.Errorf("%s: A forfeited %v, want 81.25", label, got)
		}

		// B: G1 fully drawn, G2 has 15 left; nothing forfeited.
		if got := bal(b); !near(got, 15) {
			t.Errorf("%s: B balance = %v, want 15", label, got)
		}
		lotRemaining := func(lot string) float64 {
			return queryFloat(t, db, `
				SELECT l.amount + COALESCE((SELECT SUM(c.amount) FROM billing.credit_transactions c WHERE c.lot_id = l.id), 0)
				FROM billing.credit_transactions l WHERE l.id = $1`, lot)
		}
		if got := lotRemaining(g1); !near(got, 0) {
			t.Errorf("%s: G1 remaining = %v, want 0", label, got)
		}
		if got := lotRemaining(g2); !near(got, 15) {
			t.Errorf("%s: G2 remaining = %v, want 15", label, got)
		}
		if got := queryFloat(t, db, `SELECT -SUM(amount) FROM billing.credit_transactions WHERE usage_record_id = 'bbbbbbbb-0000-0000-0000-000000000002'`); !near(got, 8) {
			t.Errorf("%s: u2 total = %v, want 8 (split must not change the amount)", label, got)
		}
		if got := queryInt(t, db, `SELECT COUNT(*) FROM billing.credit_transactions WHERE usage_record_id = 'bbbbbbbb-0000-0000-0000-000000000002'`); got != 2 {
			t.Errorf("%s: u2 rows = %d, want 2 (one per lot)", label, got)
		}
		if got := queryInt(t, db, `SELECT COUNT(*) FROM billing.credit_transactions WHERE account_id = $1 AND kind = 'expiry'`, b); got != 0 {
			t.Errorf("%s: B has %d expiry rows, want 0", label, got)
		}

		// C: untouched.
		if got := bal(c); !near(got, 50) {
			t.Errorf("%s: C balance = %v, want 50", label, got)
		}
		if got := queryInt(t, db, `SELECT COUNT(*) FROM billing.credit_transactions WHERE account_id = $1`, c); got != 1 {
			t.Errorf("%s: C rows = %d, want 1", label, got)
		}

		// D: a never-used expired grant is forfeited whole.
		if got := bal(d); !near(got, 0) {
			t.Errorf("%s: D balance = %v, want 0", label, got)
		}
		if got := queryFloat(t, db, `SELECT -amount FROM billing.credit_transactions WHERE account_id = $1 AND kind = 'expiry'`, d); !near(got, 25) {
			t.Errorf("%s: D forfeited %v, want 25", label, got)
		}
	}

	// Up: backfill and forfeit.
	if err := migrator(t, db).Migrate(60); err != nil {
		t.Fatalf("migrate to 060: %v", err)
	}
	check("after up")

	// The function must also exclude a lot that expired but has not been
	// forfeited by the sweeper yet.
	e := seedAccount(t, db, "000000000005", "drill-e")
	mustExec(t, db, `
		INSERT INTO billing.credit_transactions (account_id, amount, kind, reason, expires_at)
		VALUES ($1, 40, 'grant', 'drill', NOW() - INTERVAL '1 hour')`, e)
	if got := queryFloat(t, db, `SELECT billing.credit_balance($1)`, e); !near(got, 0) {
		t.Errorf("unforfeited expired grant counted in balance: %v, want 0", got)
	}

	// A lot can be forfeited only once.
	if _, err := db.Exec(`
		INSERT INTO billing.credit_transactions (account_id, amount, kind, reason, lot_id)
		SELECT account_id, -1, 'expiry', 'dup', lot_id FROM billing.credit_transactions
		WHERE kind = 'expiry' AND account_id = $1`, a); err == nil {
		t.Error("a second expiry row for the same lot was accepted")
	}

	// Down: rows merge back, expiry rows go, old index returns.
	mustExec(t, db, `DELETE FROM billing.credit_transactions WHERE account_id = $1`, e)
	mustExec(t, db, `DELETE FROM auth.accounts WHERE id = $1`, e)
	if err := migrator(t, db).Migrate(58); err != nil {
		t.Fatalf("migrate down to 058: %v", err)
	}
	if got := queryInt(t, db, `SELECT COUNT(*) FROM billing.credit_transactions WHERE usage_record_id = 'bbbbbbbb-0000-0000-0000-000000000002'`); got != 1 {
		t.Errorf("after down: u2 rows = %d, want 1 (merged)", got)
	}
	if got := queryFloat(t, db, `SELECT -amount FROM billing.credit_transactions WHERE usage_record_id = 'bbbbbbbb-0000-0000-0000-000000000002'`); !near(got, 8) {
		t.Errorf("after down: u2 amount = %v, want 8", got)
	}
	if got := queryInt(t, db, `SELECT COUNT(*) FROM billing.credit_transactions WHERE kind = 'expiry'`); got != 0 {
		t.Errorf("after down: %d expiry rows remain", got)
	}

	// Up again: idempotent from the restored state.
	if err := migrator(t, db).Migrate(60); err != nil {
		t.Fatalf("re-up to 060: %v", err)
	}
	check("after re-up")
}
