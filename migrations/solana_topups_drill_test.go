// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build migrationdrill

// Drill for migration 067 (USDC top-ups on Solana): existing Stripe top-ups
// must survive, the new provider/status values and uniqueness rules must hold,
// and the down migration must refuse to run once a Solana payment exists.
//
//	TEEPIN_DRILL_DSN='postgres://postgres:test@localhost:55433/teepin?sslmode=disable' \
//	  go test -tags migrationdrill -run TestSolanaTopUpsDrill ./migrations/ -v
package migrations

import (
	"database/sql"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

func TestSolanaTopUpsDrill(t *testing.T) {
	dsn := os.Getenv("TEEPIN_DRILL_DSN")
	if dsn == "" {
		t.Skip("set TEEPIN_DRILL_DSN to run the migration drill")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := migrator(t, db).Migrate(66); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 066: %v", err)
	}
	acct := seedAccount(t, db, "000000000077", "drill-solana")
	mustExec(t, db, `
		INSERT INTO billing.credit_topups (account_id, amount, provider, status, stripe_payment_intent_id)
		VALUES ($1, 25, 'stripe', 'succeeded', 'pi_drill_067')`, acct)

	// Before 067 a Solana row is not allowed.
	if _, err := db.Exec(`
		INSERT INTO billing.credit_topups (account_id, amount, provider) VALUES ($1, 25, 'solana')`, acct); err == nil {
		t.Fatal("a solana top-up was accepted before migration 067")
	}

	if err := migrator(t, db).Migrate(67); err != nil {
		t.Fatalf("migrate to 067: %v", err)
	}
	if got := queryInt(t, db, `SELECT COUNT(*) FROM billing.credit_topups WHERE provider = 'stripe' AND status = 'succeeded'`); got != 1 {
		t.Errorf("existing stripe top-ups = %d, want 1", got)
	}

	// A Solana top-up needs its reference.
	if _, err := db.Exec(`
		INSERT INTO billing.credit_topups (account_id, amount, provider) VALUES ($1, 25, 'solana')`, acct); err == nil {
		t.Error("a solana top-up without a reference was accepted")
	}
	mustExec(t, db, `INSERT INTO billing.credit_topups (account_id, amount, provider, solana_reference) VALUES ($1, 25, 'solana', 'refA')`, acct)
	mustExec(t, db, `INSERT INTO billing.credit_topups (account_id, amount, provider, solana_reference) VALUES ($1, 30, 'solana', 'refB')`, acct)
	if _, err := db.Exec(`INSERT INTO billing.credit_topups (account_id, amount, provider, solana_reference) VALUES ($1, 25, 'solana', 'refA')`, acct); err == nil {
		t.Error("a duplicate reference was accepted")
	}

	// One transaction can settle only one top-up.
	mustExec(t, db, `UPDATE billing.credit_topups SET solana_signature = 'sigX' WHERE solana_reference = 'refA'`)
	if _, err := db.Exec(`UPDATE billing.credit_topups SET solana_signature = 'sigX' WHERE solana_reference = 'refB'`); err == nil {
		t.Error("one transaction signature was accepted for two top-ups")
	}

	// The review status exists; unknown ones and unknown providers do not.
	mustExec(t, db, `UPDATE billing.credit_topups SET status = 'review' WHERE solana_reference = 'refB'`)
	if _, err := db.Exec(`UPDATE billing.credit_topups SET status = 'bogus' WHERE solana_reference = 'refB'`); err == nil {
		t.Error("an unknown status was accepted")
	}
	if _, err := db.Exec(`INSERT INTO billing.credit_topups (account_id, amount, provider) VALUES ($1, 25, 'paypal')`, acct); err == nil {
		t.Error("an unknown provider was accepted")
	}

	// Rolling back must refuse while a Solana payment exists.
	if err := migrator(t, db).Migrate(66); err == nil {
		t.Fatal("migration 067 rolled back while solana top-ups existed")
	}
	if got := queryInt(t, db, `SELECT COUNT(*) FROM billing.credit_topups WHERE provider = 'solana'`); got != 2 {
		t.Errorf("solana top-ups after the refused rollback = %d, want 2 (nothing deleted)", got)
	}
	// golang-migrate marks the target version dirty when a step fails; nothing
	// was applied (the guard raised before any change), so restore the version.
	if _, err := db.Exec(`UPDATE schema_migrations SET version = 67, dirty = false`); err != nil {
		t.Fatalf("clear dirty flag: %v", err)
	}

	// With no Solana payments, it rolls back cleanly and goes up again.
	mustExec(t, db, `DELETE FROM billing.credit_topups WHERE provider = 'solana'`)
	if err := migrator(t, db).Migrate(66); err != nil {
		t.Fatalf("migrate down to 066: %v", err)
	}
	if columnExists(t, db, "billing", "credit_topups", "solana_reference") {
		t.Error("solana_reference still present after the rollback")
	}
	if _, err := db.Exec(`INSERT INTO billing.credit_topups (account_id, amount, provider, status) VALUES ($1, 25, 'stripe', 'review')`, acct); err == nil {
		t.Error("the review status survived the rollback")
	}
	if err := migrator(t, db).Migrate(67); err != nil {
		t.Fatalf("migrate up to 067 again: %v", err)
	}
	if got := queryInt(t, db, `SELECT COUNT(*) FROM billing.credit_topups WHERE provider = 'stripe'`); got != 1 {
		t.Errorf("stripe top-ups after the round trip = %d, want 1", got)
	}
}
