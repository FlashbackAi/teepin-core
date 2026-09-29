// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
)

// maxGrant caps a single credit grant. An operator mints spendable value
// with GrantCredit, and a typo'd extra zero must not hand out $50,000 of
// GPU time — a larger grant is a deliberate config change, not an
// accident. Compiled in rather than configurable so raising it is a
// reviewed code change.
const maxGrant = 5000.0

// GrantRequest describes an operator granting credit to an account.
type GrantRequest struct {
	AccountID uuid.UUID
	Amount    float64
	Reason    string
	GrantedBy string
	// ExpiresAt is optional; nil means the grant never expires.
	ExpiresAt *time.Time
}

// CreditBalance returns an account's spendable credit, computed by the
// billing.credit_balance database function (migration 060): every ledger
// row, minus whatever is left in lots that have expired but not yet been
// forfeited by the sweeper. Expiring a partly-spent grant therefore removes
// only its unspent part, so the balance cannot go negative from expiry.
//
// The balance is derived from the append-only ledger, never stored — so
// it cannot drift from its own history.
func (s *Service) CreditBalance(ctx context.Context, accountID uuid.UUID) (float64, error) {
	var balance sql.NullFloat64
	err := s.db.QueryRowContext(ctx, `SELECT billing.credit_balance($1)`, accountID).Scan(&balance)
	if err != nil {
		return 0, fmt.Errorf("failed to compute credit balance: %w", err)
	}
	return balance.Float64, nil
}

// GrantCredit records a positive grant. Guardrails: a non-empty reason
// (an unexplained credit is indistinguishable from fraud in an audit), a
// positive amount within maxGrant, and any expiry set in the future.
func (s *Service) GrantCredit(ctx context.Context, req GrantRequest) error {
	if strings.TrimSpace(req.Reason) == "" {
		return fmt.Errorf("a reason is required for a credit grant")
	}
	if req.Amount <= 0 {
		return fmt.Errorf("grant amount must be positive")
	}
	if req.Amount > maxGrant {
		return fmt.Errorf("grant amount %.2f exceeds the per-grant cap of %.2f", req.Amount, maxGrant)
	}
	if req.ExpiresAt != nil && !req.ExpiresAt.After(time.Now()) {
		return fmt.Errorf("expiry must be in the future")
	}

	var grantedBy interface{}
	if req.GrantedBy != "" {
		grantedBy = req.GrantedBy
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO billing.credit_transactions
		(account_id, amount, kind, reason, granted_by, expires_at)
		VALUES ($1, $2, 'grant', $3, $4, $5)
	`, req.AccountID, req.Amount, req.Reason, grantedBy, req.ExpiresAt)
	if err != nil {
		return fmt.Errorf("failed to record credit grant: %w", err)
	}
	s.balances.forget(req.AccountID)
	return nil
}

// ConsumeCredit draws up to cost from an account's credit for one metered
// usage record, returning the amount actually applied (which may be zero,
// or a partial draw when the balance is smaller than the cost). The
// balance therefore never goes negative. Teepin is prepaid, so the
// remainder — cost minus applied — is never collected; enforcement stops
// usage before the balance runs out so that remainder stays near zero, and
// it is derivable for audit as the usage record's total_cost minus its
// consumption rows.
//
// Spending is drawn from lots in a fixed order — grants that expire soonest
// first, then lots that never expire (grants and purchases), oldest first —
// and one consumption row is written per lot drawn, each naming its lot.
// Naming the lot is what lets an expiring grant forfeit only what is left
// of it.
//
// Atomic and idempotent:
//   - the account row is locked FOR UPDATE so two concurrent collectors
//     cannot both spend the same credit;
//   - a usage record that already has consumption rows draws nothing more,
//     and a unique index on (usage record, lot) backs that up.
func (s *Service) ConsumeCredit(ctx context.Context, accountID, usageRecordID uuid.UUID, cost float64) (applied float64, err error) {
	if cost <= 0 {
		return 0, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Idempotency: if this usage record was already drawn against, do
	// nothing and report zero newly applied. Checked inside the tx so it
	// races correctly with a concurrent attempt.
	var already int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM billing.credit_transactions WHERE usage_record_id = $1
	`, usageRecordID).Scan(&already); err != nil {
		return 0, fmt.Errorf("failed to check prior consumption: %w", err)
	}
	if already > 0 {
		return 0, nil
	}

	// Lock the account so the lots we read cannot be spent by another
	// collector between the read and our inserts.
	if _, err := tx.ExecContext(ctx,
		`SELECT 1 FROM auth.accounts WHERE id = $1 FOR UPDATE`, accountID); err != nil {
		return 0, fmt.Errorf("failed to lock account: %w", err)
	}

	// The account's balance is the ceiling: lots alone could overstate it
	// if a legacy consumption row could not be attributed to any lot.
	var balance sql.NullFloat64
	if err := tx.QueryRowContext(ctx, `SELECT billing.credit_balance($1)`, accountID).Scan(&balance); err != nil {
		return 0, fmt.Errorf("failed to read balance: %w", err)
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT l.id,
		       l.amount + COALESCE((SELECT SUM(c.amount)
		                              FROM billing.credit_transactions c
		                             WHERE c.lot_id = l.id), 0) AS remaining
		FROM billing.credit_transactions l
		WHERE l.account_id = $1
		  AND l.kind IN ('grant', 'purchase')
		  AND (l.expires_at IS NULL OR l.expires_at > NOW())
		ORDER BY (l.expires_at IS NULL), l.expires_at, l.created_at, l.id
	`, accountID)
	if err != nil {
		return 0, fmt.Errorf("failed to read credit lots: %w", err)
	}
	type lot struct {
		id        uuid.UUID
		remaining float64
	}
	var lots []lot
	for rows.Next() {
		var l lot
		if err := rows.Scan(&l.id, &l.remaining); err != nil {
			rows.Close()
			return 0, fmt.Errorf("failed to scan credit lot: %w", err)
		}
		lots = append(lots, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("failed to read credit lots: %w", err)
	}

	budget := math.Min(cost, balance.Float64)
	for _, l := range lots {
		if budget <= 0 {
			break
		}
		if l.remaining <= 0 {
			continue
		}
		take := math.Min(budget, l.remaining)
		// A consumption row is negative.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO billing.credit_transactions
			(account_id, amount, kind, reason, usage_record_id, lot_id)
			VALUES ($1, $2, 'consumption', 'usage', $3, $4)
		`, accountID, -take, usageRecordID, l.id); err != nil {
			return 0, fmt.Errorf("failed to record consumption: %w", err)
		}
		applied += take
		budget -= take
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("failed to commit consumption: %w", err)
	}
	if applied > 0 {
		s.balances.adjust(accountID, -applied)
	}
	return applied, nil
}

// ExpireCredits forfeits the unspent part of every expired grant that has
// not been forfeited yet, writing one 'expiry' row per lot dated to when it
// lapsed. Idempotent: a lot is forfeited at most once (unique index), and a
// fully-spent lot has nothing to forfeit. Balance reads are already exact
// without it (billing.credit_balance excludes expired lots' remainder); this
// keeps the visible ledger honest. Returns how many lots were forfeited.
func (s *Service) ExpireCredits(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO billing.credit_transactions (account_id, amount, kind, reason, lot_id, created_at)
		SELECT x.account_id, -x.remaining, 'expiry', 'Grant expired', x.id, x.expires_at
		FROM (
			SELECT l.id, l.account_id, l.expires_at,
			       l.amount + COALESCE((SELECT SUM(c.amount)
			                              FROM billing.credit_transactions c
			                             WHERE c.lot_id = l.id), 0) AS remaining
			FROM billing.credit_transactions l
			WHERE l.kind IN ('grant', 'purchase')
			  AND l.expires_at IS NOT NULL
			  AND l.expires_at <= NOW()
		) x
		WHERE x.remaining > 0
		ON CONFLICT (lot_id) WHERE kind = 'expiry' DO NOTHING
	`)
	if err != nil {
		return 0, fmt.Errorf("failed to forfeit expired credit: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ListCreditTransactions returns an account's ledger, newest first, for
// the control-centre credit view.
func (s *Service) ListCreditTransactions(ctx context.Context, accountID uuid.UUID) ([]CreditTransaction, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, account_id, amount, kind, reason, granted_by, expires_at,
		       usage_record_id, created_at
		FROM billing.credit_transactions
		WHERE account_id = $1
		ORDER BY created_at DESC
	`, accountID)
	if err != nil {
		return nil, fmt.Errorf("failed to list credit transactions: %w", err)
	}
	defer rows.Close()

	txns := []CreditTransaction{}
	for rows.Next() {
		var t CreditTransaction
		if err := rows.Scan(&t.ID, &t.AccountID, &t.Amount, &t.Kind, &t.Reason,
			&t.GrantedBy, &t.ExpiresAt, &t.UsageRecordID, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan credit transaction: %w", err)
		}
		txns = append(txns, t)
	}
	return txns, rows.Err()
}
