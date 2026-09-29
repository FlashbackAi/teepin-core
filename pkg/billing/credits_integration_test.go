// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build migrationdrill

// Runs the credit-lot code paths against a real Postgres, because sqlmock
// checks the shape of a query but not that the SQL actually works. Behind
// the same build tag as the migration drills so it never runs in the normal
// `go test ./...`:
//
//	TEEPIN_DRILL_DSN='postgres://postgres:test@localhost:55433/teepin?sslmode=disable' \
//	  go test -tags migrationdrill -run TestCreditLotsIntegration ./pkg/billing/ -v
package billing

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"sync"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	_ "github.com/lib/pq"

	"github.com/FlashbackAi/teepin-core/migrations"
)

func integrationDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEEPIN_DRILL_DSN")
	if dsn == "" {
		t.Skip("set TEEPIN_DRILL_DSN to run the credit-lot integration test")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("migration source: %v", err)
	}
	drv, err := migratepg.WithInstance(db, &migratepg.Config{})
	if err != nil {
		t.Fatalf("migration driver: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "postgres", drv)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate up: %v", err)
	}

	// Seeded usage_record_ids are not real usage records (a real one needs
	// a project, user and instance behind it); the constraint is dropped
	// for this disposable database only.
	if _, err := db.Exec(`ALTER TABLE billing.credit_transactions DROP CONSTRAINT IF EXISTS credit_transactions_usage_record_id_fkey`); err != nil {
		t.Fatalf("drop fk: %v", err)
	}
	return db
}

func itAccount(t *testing.T, db *sql.DB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	short := id.String()[:8]
	if _, err := db.Exec(`
		INSERT INTO auth.accounts (id, account_number, alias, type, display_name)
		VALUES ($1, $2, $3, 'organization', 'integration')
	`, id, "9"+short[:8], "it-"+short); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	return id
}

func itLot(t *testing.T, db *sql.DB, account uuid.UUID, kind string, amount float64, createdDaysAgo int, expiresInDays *int) uuid.UUID {
	t.Helper()
	var expires any
	if expiresInDays != nil {
		expires = *expiresInDays
	}
	var id uuid.UUID
	if err := db.QueryRow(`
		INSERT INTO billing.credit_transactions (account_id, amount, kind, reason, expires_at, created_at)
		VALUES ($1, $2, $3, 'integration',
		        CASE WHEN $5::int IS NULL THEN NULL ELSE NOW() + ($5::int * INTERVAL '1 day') END,
		        NOW() - ($4::int * INTERVAL '1 day'))
		RETURNING id
	`, account, amount, kind, createdDaysAgo, expires).Scan(&id); err != nil {
		t.Fatalf("seed lot: %v", err)
	}
	return id
}

func itDays(n int) *int { return &n }

func itNear(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func itBalance(t *testing.T, s *Service, account uuid.UUID) float64 {
	t.Helper()
	b, err := s.CreditBalance(context.Background(), account)
	if err != nil {
		t.Fatalf("CreditBalance: %v", err)
	}
	return b
}

func itConsume(t *testing.T, s *Service, account uuid.UUID, cost float64) (uuid.UUID, float64) {
	t.Helper()
	usage := uuid.New()
	applied, err := s.ConsumeCredit(context.Background(), account, usage, cost)
	if err != nil {
		t.Fatalf("ConsumeCredit: %v", err)
	}
	return usage, applied
}

func itRows(t *testing.T, db *sql.DB, usage uuid.UUID) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM billing.credit_transactions WHERE usage_record_id = $1`, usage).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

// Spending draws the soonest-expiring grant first, then never-expiring
// credit, splitting a charge across lots; a replay applies nothing; and the
// balance stops at exactly zero.
func TestCreditLotsIntegration_DrawOrderAndFloor(t *testing.T) {
	db := integrationDB(t)
	s := NewService(db)
	account := itAccount(t, db)

	// The never-expiring purchase is OLDER than the expiring grant, so the
	// draw order (expiring first) is distinguishable from oldest-first.
	itLot(t, db, account, "purchase", 20, 10, nil)
	itLot(t, db, account, "grant", 10, 5, itDays(30))

	u1, applied := itConsume(t, s, account, 6)
	if !itNear(applied, 6) || itRows(t, db, u1) != 1 {
		t.Fatalf("u1: applied %v in %d rows, want 6 in 1 (entirely from the expiring grant)", applied, itRows(t, db, u1))
	}
	u2, applied := itConsume(t, s, account, 8)
	if !itNear(applied, 8) || itRows(t, db, u2) != 2 {
		t.Fatalf("u2: applied %v in %d rows, want 8 in 2 (4 from the grant, 4 from the purchase)", applied, itRows(t, db, u2))
	}
	if got := itBalance(t, s, account); !itNear(got, 16) {
		t.Errorf("balance = %v, want 16", got)
	}

	// Replay of u2 applies nothing and adds no rows.
	applied, err := s.ConsumeCredit(context.Background(), account, u2, 8)
	if err != nil || applied != 0 || itRows(t, db, u2) != 2 {
		t.Errorf("replay: applied %v err %v rows %d, want 0/nil/2", applied, err, itRows(t, db, u2))
	}

	// A charge bigger than everything left takes exactly what is left.
	_, applied = itConsume(t, s, account, 100)
	if !itNear(applied, 16) {
		t.Errorf("oversized charge applied %v, want 16 (the whole balance)", applied)
	}
	if got := itBalance(t, s, account); !itNear(got, 0) {
		t.Errorf("balance after draining = %v, want 0", got)
	}
	_, applied = itConsume(t, s, account, 1)
	if applied != 0 || itBalance(t, s, account) < 0 {
		t.Errorf("spending at zero: applied %v balance %v, want 0 and never negative", applied, itBalance(t, s, account))
	}
}

// The incident this fix exists for: a partly-spent grant expires. The
// balance must land on zero (not negative), and the customer's next
// purchase must be worth its full face value.
func TestCreditLotsIntegration_ExpiredPartlySpentGrantThenPurchase(t *testing.T) {
	db := integrationDB(t)
	s := NewService(db)
	ctx := context.Background()
	account := itAccount(t, db)

	grant := itLot(t, db, account, "grant", 100, 40, itDays(-10)) // already expired
	for i := 0; i < 3; i++ {
		if _, err := db.Exec(`
			INSERT INTO billing.credit_transactions (account_id, amount, kind, reason, usage_record_id, lot_id, created_at)
			VALUES ($1, -6.25, 'consumption', 'usage', $2, $3, NOW() - INTERVAL '30 days')
		`, account, uuid.New(), grant); err != nil {
			t.Fatalf("seed consumption: %v", err)
		}
	}

	// Before the sweeper runs, the unspent $81.25 is already excluded.
	if got := itBalance(t, s, account); !itNear(got, 0) {
		t.Fatalf("balance with an unforfeited expired grant = %v, want 0", got)
	}
	if ok, _, err := s.AccountCanProvision(ctx, account); err != nil || ok {
		t.Errorf("gate open (%v, %v) for an account with no spendable credit", ok, err)
	}
	if _, applied := itConsume(t, s, account, 5); applied != 0 {
		t.Errorf("drew %v from an expired grant, want 0", applied)
	}

	// The sweeper forfeits exactly the unspent part, once.
	n, err := s.ExpireCredits(ctx)
	if err != nil || n != 1 {
		t.Fatalf("ExpireCredits = %d, %v; want 1, nil", n, err)
	}
	if n, err := s.ExpireCredits(ctx); err != nil || n != 0 {
		t.Errorf("second ExpireCredits = %d, %v; want 0, nil (idempotent)", n, err)
	}
	var forfeited float64
	if err := db.QueryRow(`SELECT -amount FROM billing.credit_transactions WHERE account_id = $1 AND kind = 'expiry'`, account).Scan(&forfeited); err != nil || !itNear(forfeited, 81.25) {
		t.Errorf("forfeited %v (err %v), want 81.25", forfeited, err)
	}
	if got := itBalance(t, s, account); !itNear(got, 0) {
		t.Errorf("balance after forfeiture = %v, want 0", got)
	}

	// A $20 purchase now shows as $20, not $1.25.
	itLot(t, db, account, "purchase", 20, 0, nil)
	if got := itBalance(t, s, account); !itNear(got, 20) {
		t.Errorf("balance after a $20 purchase = %v, want 20", got)
	}
	if ok, reason, err := s.AccountCanProvision(ctx, account); err != nil || !ok {
		t.Errorf("gate closed (%v, %q, %v) for an account with $20", ok, reason, err)
	}
}

// Many collectors spending at once must never take more than the balance:
// the account lock serialises them.
func TestCreditLotsIntegration_ConcurrentSpendingNeverOverdraws(t *testing.T) {
	db := integrationDB(t)
	db.SetMaxOpenConns(30)
	s := NewService(db)
	account := itAccount(t, db)
	itLot(t, db, account, "grant", 4, 1, itDays(30))
	itLot(t, db, account, "purchase", 6, 1, nil)

	const workers = 40
	var wg sync.WaitGroup
	var mu sync.Mutex
	total := 0.0
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			applied, err := s.ConsumeCredit(context.Background(), account, uuid.New(), 1)
			if err != nil {
				t.Errorf("ConsumeCredit: %v", err)
				return
			}
			mu.Lock()
			total += applied
			mu.Unlock()
		}()
	}
	wg.Wait()

	if !itNear(total, 10) {
		t.Errorf("concurrent workers applied %v in total, want exactly 10 (the balance)", total)
	}
	if got := itBalance(t, s, account); !itNear(got, 0) {
		t.Errorf("balance = %v, want 0", got)
	}
}

// Each document series is gapless on its own: they do not share a counter,
// a rolled-back allocation hands its number back, and concurrent issuers
// each get a distinct number with none skipped.
func TestDocumentNumbersIntegration(t *testing.T) {
	db := integrationDB(t)
	ctx := context.Background()

	alloc := func(series string, year int, commit bool) string {
		t.Helper()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		n, err := allocateDocumentNumber(ctx, tx, series, year)
		if err != nil {
			tx.Rollback()
			t.Fatalf("allocate: %v", err)
		}
		if commit {
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit: %v", err)
			}
		} else {
			tx.Rollback()
		}
		return n
	}

	// Independent series in the same year.
	if got := alloc(seriesInvoice, 2099, true); got != "INV-2099-000001" {
		t.Errorf("first invoice = %s", got)
	}
	if got := alloc(seriesInvoice, 2099, true); got != "INV-2099-000002" {
		t.Errorf("second invoice = %s", got)
	}
	if got := alloc(seriesReceipt, 2099, true); got != "RCT-2099-000001" {
		t.Errorf("first receipt = %s, want its own series (RCT-2099-000001)", got)
	}

	// A rollback returns the number.
	if got := alloc(seriesReceipt, 2099, false); got != "RCT-2099-000002" {
		t.Errorf("rolled-back receipt = %s", got)
	}
	if got := alloc(seriesReceipt, 2099, true); got != "RCT-2099-000002" {
		t.Errorf("receipt after a rollback = %s, want RCT-2099-000002 (number handed back)", got)
	}

	// Concurrent issuers: distinct, contiguous.
	db.SetMaxOpenConns(30)
	const workers = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[string]bool{}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := alloc(seriesReceipt, 2098, true)
			mu.Lock()
			seen[n] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(seen) != workers {
		t.Errorf("%d distinct numbers from %d concurrent issuers", len(seen), workers)
	}
	for i := 1; i <= workers; i++ {
		want := fmt.Sprintf("RCT-2098-%06d", i)
		if !seen[want] {
			t.Errorf("missing %s: the series has a gap", want)
		}
	}
}
