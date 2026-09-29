// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Prepaid credit top-ups.
//
// Teepin is prepaid: a customer buys credit, and usage draws it down to
// zero. A top-up is created when the customer starts paying and only turns
// into spendable credit when the payment provider confirms the money
// settled (the webhook) — never on the browser's word, and never before an
// asynchronous method such as an ACH debit has actually cleared.

const (
	// MinTopUp and MaxTopUp bound a single purchase, in USD. The minimum
	// keeps card fees proportionate; the maximum limits the damage of a
	// stolen card on a new account and can be raised once there is
	// payment history to justify it.
	MinTopUp = 20.0
	MaxTopUp = 1000.0

	// topUpCurrency is the only currency purchases are made in today.
	topUpCurrency = "USD"

	// creditPurchaseDescription is the receipt's single line.
	creditPurchaseDescription = "Teepin prepaid credit"
)

// ErrTopUpAmount is returned for an amount outside the allowed range or
// not in whole cents.
var ErrTopUpAmount = fmt.Errorf("top-up amount must be between $%.0f and $%.0f, in whole cents", MinTopUp, MaxTopUp)

// ErrTopUpNotFound is returned when a top-up does not exist or belongs to
// another account.
var ErrTopUpNotFound = errors.New("top-up not found")

// ErrAccountClosed is returned when a closed account tries to buy credit.
var ErrAccountClosed = errors.New("this account is closed")

// TopUp is one credit purchase attempt, from creation to settlement.
type TopUp struct {
	ID                   uuid.UUID  `json:"id"`
	AccountID            uuid.UUID  `json:"account_id"`
	Amount               float64    `json:"amount"`
	Currency             string     `json:"currency"`
	Provider             string     `json:"provider"`
	Status               string     `json:"status"` // pending, processing, succeeded, failed
	PaymentMethodSummary string     `json:"payment_method_summary,omitempty"`
	FailureReason        string     `json:"failure_reason,omitempty"`
	ReceiptInvoiceID     *uuid.UUID `json:"receipt_invoice_id,omitempty"`
	SucceededAt          *time.Time `json:"succeeded_at,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
}

// TopUpIntent is what the browser needs to collect a top-up payment.
type TopUpIntent struct {
	TopUpID      uuid.UUID `json:"topup_id"`
	ClientSecret string    `json:"client_secret"`
	Amount       float64   `json:"amount"`
	Currency     string    `json:"currency"`
}

// topUpAmountCents validates a requested amount and converts it to cents.
// Amounts must be whole cents: a float that is not (e.g. 20.005) is
// rejected rather than silently rounded, because the customer would be
// charged something other than what they asked for.
func topUpAmountCents(amount float64) (int64, error) {
	if math.IsNaN(amount) || math.IsInf(amount, 0) {
		return 0, ErrTopUpAmount
	}
	cents := math.Round(amount * 100)
	if math.Abs(amount*100-cents) > 1e-6 {
		return 0, ErrTopUpAmount
	}
	if cents < MinTopUp*100 || cents > MaxTopUp*100 {
		return 0, ErrTopUpAmount
	}
	return int64(cents), nil
}

// CreateTopUp starts a credit purchase: it records a pending top-up and
// opens a Stripe PaymentIntent for it. No credit is added here — that
// happens in SettleTopUpByPaymentIntent once Stripe confirms payment.
func (s *Service) CreateTopUp(ctx context.Context, accountID uuid.UUID, amount float64) (*TopUpIntent, error) {
	cents, err := topUpAmountCents(amount)
	if err != nil {
		return nil, err
	}
	if s.stripe == nil {
		return nil, ErrPaymentsNotConfigured
	}

	cust, err := s.ensureStripeCustomer(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if cust.Status == "closed" {
		return nil, ErrAccountClosed
	}

	amount = float64(cents) / 100
	var topUpID uuid.UUID
	if err := s.db.QueryRowContext(ctx, `
		INSERT INTO billing.credit_topups (account_id, amount, currency, provider, status)
		VALUES ($1, $2, $3, 'stripe', 'pending')
		RETURNING id
	`, accountID, amount, topUpCurrency).Scan(&topUpID); err != nil {
		return nil, fmt.Errorf("failed to record top-up: %w", err)
	}

	piID, secret, err := s.stripe.CreateTopUpPaymentIntent(cust.CustomerID, cents,
		strings.ToLower(topUpCurrency), topUpID.String(), cust.AccountNumber, "topup-"+topUpID.String())
	if err != nil {
		// Record why, so the attempt is not left looking like it is still
		// waiting on the customer. Best-effort: the create error is what
		// the caller needs.
		if _, uerr := s.db.ExecContext(ctx, `
			UPDATE billing.credit_topups
			SET status = 'failed', failure_reason = $2, updated_at = NOW()
			WHERE id = $1
		`, topUpID, err.Error()); uerr != nil {
			log.Printf("WARN: top-up %s: payment intent failed (%v) and recording the failure also failed: %v", topUpID, err, uerr)
		}
		return nil, err
	}

	if _, err := s.db.ExecContext(ctx, `
		UPDATE billing.credit_topups
		SET stripe_payment_intent_id = $2, updated_at = NOW()
		WHERE id = $1
	`, topUpID, piID); err != nil {
		// Without the intent id the webhook could never find this top-up,
		// so a customer could pay and receive no credit. Fail the request
		// before the browser is given anything to pay with.
		return nil, fmt.Errorf("failed to record payment intent for top-up: %w", err)
	}

	return &TopUpIntent{TopUpID: topUpID, ClientSecret: secret, Amount: amount, Currency: topUpCurrency}, nil
}

// MarkTopUpProcessing records that a top-up's payment was submitted and is
// awaiting settlement (an ACH debit takes days). No credit is added.
// Idempotent, and a no-op for a top-up that already settled.
func (s *Service) MarkTopUpProcessing(ctx context.Context, piID string) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE billing.credit_topups
		SET status = 'processing', updated_at = NOW()
		WHERE stripe_payment_intent_id = $1 AND status IN ('pending', 'failed')
	`, piID); err != nil {
		return fmt.Errorf("failed to mark top-up processing: %w", err)
	}
	return nil
}

// FailTopUpByPaymentIntent records a failed payment attempt. A top-up that
// already succeeded is never downgraded. A failed PaymentIntent can still
// be retried by the customer with another method, which then settles the
// same top-up.
func (s *Service) FailTopUpByPaymentIntent(ctx context.Context, piID, reason string) error {
	if strings.TrimSpace(reason) == "" {
		reason = "the payment did not go through"
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE billing.credit_topups
		SET status = 'failed', failure_reason = $2, updated_at = NOW()
		WHERE stripe_payment_intent_id = $1 AND status != 'succeeded'
	`, piID, reason); err != nil {
		return fmt.Errorf("failed to record top-up failure: %w", err)
	}
	return nil
}

// SettleTopUpByPaymentIntent turns a confirmed payment into spendable
// credit. In one transaction it adds a 'purchase' row to the credit ledger,
// issues a paid receipt, and marks the top-up succeeded; the receipt PDF is
// rendered afterwards, best-effort.
//
// Safe to replay: a top-up that already succeeded is a no-op, and the
// ledger's unique index on topup_id means its credit can never be added
// twice. The top-up is matched by the PaymentIntent id we minted. What
// Stripe says it received must equal what the top-up recorded; a mismatch
// adds no credit and is logged as an error for an operator.
func (s *Service) SettleTopUpByPaymentIntent(ctx context.Context, piID string, receivedCents int64, currency, methodSummary string) error {
	// The bill-to snapshot is read before the transaction so its query does
	// not hold the top-up row lock; it only needs the account id.
	var accountID uuid.UUID
	err := s.db.QueryRowContext(ctx,
		`SELECT account_id FROM billing.credit_topups WHERE stripe_payment_intent_id = $1`,
		piID).Scan(&accountID)
	if err == sql.ErrNoRows {
		return ErrTopUpNotFound
	}
	if err != nil {
		return fmt.Errorf("failed to find top-up: %w", err)
	}
	bill, err := s.resolveBillToByAccount(ctx, accountID)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	var (
		topUpID       uuid.UUID
		amount        float64
		topUpCurrency string
		status        string
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT id, amount, currency, status
		FROM billing.credit_topups
		WHERE stripe_payment_intent_id = $1
		FOR UPDATE
	`, piID).Scan(&topUpID, &amount, &topUpCurrency, &status); err != nil {
		return fmt.Errorf("failed to lock top-up: %w", err)
	}
	if status == "succeeded" {
		return nil
	}

	expectedCents := int64(math.Round(amount * 100))
	if receivedCents != expectedCents || !strings.EqualFold(currency, topUpCurrency) {
		reason := fmt.Sprintf("amount mismatch: expected %d %s cents, received %d %s cents",
			expectedCents, topUpCurrency, receivedCents, strings.ToUpper(currency))
		log.Printf("ERROR: top-up %s (payment intent %s) not credited: %s", topUpID, piID, reason)
		if _, err := tx.ExecContext(ctx, `
			UPDATE billing.credit_topups
			SET status = 'failed', failure_reason = $2, updated_at = NOW()
			WHERE id = $1
		`, topUpID, reason); err != nil {
			return fmt.Errorf("failed to record top-up mismatch: %w", err)
		}
		return tx.Commit()
	}

	receiptID, invoiceNumber, err := s.insertCreditPurchaseReceipt(ctx, tx, bill, amount, topUpCurrency, methodSummary)
	if err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO billing.credit_transactions (account_id, amount, kind, reason, topup_id)
		VALUES ($1, $2, 'purchase', $3, $4)
	`, accountID, amount, "Credit purchase ("+invoiceNumber+")", topUpID); err != nil {
		return fmt.Errorf("failed to record credit purchase: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE billing.credit_topups
		SET status = 'succeeded', succeeded_at = NOW(), failure_reason = NULL,
		    payment_method_summary = $2, receipt_invoice_id = $3, updated_at = NOW()
		WHERE id = $1
	`, topUpID, nullIfEmpty(methodSummary), receiptID); err != nil {
		return fmt.Errorf("failed to mark top-up succeeded: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit top-up settlement: %w", err)
	}
	// The purchase is spendable now; do not let a pre-flight keep judging on
	// the balance from before it.
	s.balances.forget(accountID)

	// The credit and the receipt record are committed; the PDF is a derived
	// document and must not undo them if rendering or storage fails.
	if s.renderInvoicePDF == nil || s.pdfStore == nil {
		return nil // PDF storage not wired (local dev)
	}
	receipt, err := s.GetInvoice(ctx, receiptID)
	if err != nil {
		log.Printf("WARN: top-up %s credited but loading receipt %s for its PDF failed: %v", topUpID, invoiceNumber, err)
		return nil
	}
	s.generateAndStorePDF(ctx, receipt)
	return nil
}

// insertCreditPurchaseReceipt writes a paid receipt for a credit purchase
// inside the settlement transaction, so the credit and its receipt exist
// together or not at all. Tax is zero: purchases are made from the US
// entity, which charges no sales tax today (the same default as NoTax).
func (s *Service) insertCreditPurchaseReceipt(ctx context.Context, tx *sql.Tx, bill *billTo, amount float64, currency, methodSummary string) (uuid.UUID, string, error) {
	invoiceNumber, err := s.nextReceiptNumber(ctx, tx)
	if err != nil {
		return uuid.Nil, "", err
	}

	terms := "Paid in full"
	if methodSummary != "" {
		terms = "Paid in full by " + methodSummary
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)

	var receiptID uuid.UUID
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO billing.invoices
		(account_id, invoice_number, period_start, period_end,
		 subtotal, tax, total, status, source, currency, paid_at,
		 bill_to_name, bill_to_email, bill_to_address, bill_to_tax_id,
		 bill_to_account_number, bill_to_country, tax_details, payment_terms)
		VALUES ($1,$2,$3,$3,$4,0,$4,'paid','credit_purchase',$5,NOW(),
		        $6,$7,$8,$9,$10,$11,'[]'::jsonb,$12)
		RETURNING id
	`, bill.AccountID, invoiceNumber, today, amount, currency,
		nullIfEmpty(bill.Name), nullIfEmpty(bill.Email), nullIfEmpty(bill.Address),
		nullIfEmpty(bill.TaxID), nullIfEmpty(bill.AccountNumber), nullIfEmpty(bill.Country),
		terms,
	).Scan(&receiptID); err != nil {
		return uuid.Nil, "", fmt.Errorf("failed to create receipt: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO billing.invoice_line_items
		(invoice_id, description, quantity, unit_price, amount, position, service)
		VALUES ($1, $2, 1, $3, $3, 0, 'Prepaid credit')
	`, receiptID, creditPurchaseDescription, amount); err != nil {
		return uuid.Nil, "", fmt.Errorf("failed to create receipt line: %w", err)
	}
	return receiptID, invoiceNumber, nil
}

// topUpColumns is the shared select list for TopUp scans.
const topUpColumns = `id, account_id, amount, currency, provider, status,
	COALESCE(payment_method_summary, ''), COALESCE(failure_reason, ''),
	receipt_invoice_id, succeeded_at, created_at`

func scanTopUp(scan func(dest ...any) error) (TopUp, error) {
	var t TopUp
	err := scan(&t.ID, &t.AccountID, &t.Amount, &t.Currency, &t.Provider, &t.Status,
		&t.PaymentMethodSummary, &t.FailureReason, &t.ReceiptInvoiceID, &t.SucceededAt, &t.CreatedAt)
	return t, err
}

// GetTopUp returns one of the account's top-ups. Scoped to the account:
// another account's top-up is reported as not found, never as forbidden.
func (s *Service) GetTopUp(ctx context.Context, accountID, topUpID uuid.UUID) (*TopUp, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+topUpColumns+`
		FROM billing.credit_topups
		WHERE id = $1 AND account_id = $2
	`, topUpID, accountID)
	t, err := scanTopUp(row.Scan)
	if err == sql.ErrNoRows {
		return nil, ErrTopUpNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get top-up: %w", err)
	}
	return &t, nil
}

// ListTopUps returns the account's top-up history, newest first. Pending
// top-ups are left out: they are checkouts the customer opened but never
// paid, and listing them would read as payments stuck in flight.
func (s *Service) ListTopUps(ctx context.Context, accountID uuid.UUID) ([]TopUp, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+topUpColumns+`
		FROM billing.credit_topups
		WHERE account_id = $1 AND status != 'pending'
		ORDER BY created_at DESC
		LIMIT 100
	`, accountID)
	if err != nil {
		return nil, fmt.Errorf("failed to list top-ups: %w", err)
	}
	defer rows.Close()

	out := []TopUp{}
	for rows.Next() {
		t, err := scanTopUp(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("failed to scan top-up: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
