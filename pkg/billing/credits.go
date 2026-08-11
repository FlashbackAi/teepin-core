// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"database/sql"
	"fmt"
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

// CreditBalance returns an account's spendable credit: the sum of grants
// that have not expired, plus every consumption/expiry/revocation row
// (which are always negative and always count). A lapsed grant is
// excluded from the total but its offsetting rows, if ExpireCredits has
// written them, keep the arithmetic honest either way.
//
// The balance is derived from the append-only ledger, never stored — so
// it cannot drift from its own history.
func (s *Service) CreditBalance(ctx context.Context, accountID uuid.UUID) (float64, error) {
	var balance sql.NullFloat64
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amount), 0)
		FROM billing.credit_transactions
		WHERE account_id = $1
		  AND (kind != 'grant' OR expires_at IS NULL OR expires_at > NOW())
	`, accountID).Scan(&balance)
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
	return nil
}

// ConsumeCredit draws up to cost from an account's balance for one
// metered usage record, returning the amount actually applied (which may
// be zero, or a partial draw when the balance is smaller than the cost).
// The remainder — cost minus applied — is what still gets billed to the
// card later.
//
// Atomic and idempotent:
//   - the account row is locked FOR UPDATE so two concurrent collectors
//     cannot both spend the same balance;
//   - the consumption row is keyed to usageRecordID with a unique index,
//     so re-processing an interval inserts nothing and applies nothing.
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

	// Lock the account so the balance we read cannot be spent by another
	// collector between the read and our insert.
	if _, err := tx.ExecContext(ctx,
		`SELECT 1 FROM auth.accounts WHERE id = $1 FOR UPDATE`, accountID); err != nil {
		return 0, fmt.Errorf("failed to lock account: %w", err)
	}

	var balance sql.NullFloat64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amount), 0)
		FROM billing.credit_transactions
		WHERE account_id = $1
		  AND (kind != 'grant' OR expires_at IS NULL OR expires_at > NOW())
	`, accountID).Scan(&balance); err != nil {
		return 0, fmt.Errorf("failed to read balance: %w", err)
	}

	applied = cost
	if balance.Float64 < applied {
		applied = balance.Float64
	}
	if applied <= 0 {
		// No credit to apply; commit nothing and report zero. (Committing
		// an empty tx is fine and keeps the defer simple.)
		return 0, tx.Commit()
	}

	// A consumption row is negative.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO billing.credit_transactions
		(account_id, amount, kind, reason, usage_record_id)
		VALUES ($1, $2, 'consumption', 'usage', $3)
	`, accountID, -applied, usageRecordID); err != nil {
		return 0, fmt.Errorf("failed to record consumption: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("failed to commit consumption: %w", err)
	}
	return applied, nil
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
