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
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/FlashbackAi/teepin-core/pkg/solana"
)

// USDC on Solana top-ups.
//
// How a payment flows:
//
//  1. The customer asks to add $X. We record a pending top-up with a fresh
//     random reference and hand back a Solana Pay link/QR for that amount.
//  2. Their wallet sends USDC to OUR wallet with the reference attached.
//  3. The watcher finds transactions that mention the reference, reads each
//     one back from a FINALIZED block, and verifies from the chain's own
//     balance changes that our wallet really received USDC (pkg/solana).
//  4. Only then is credit added, through the same settlement as a card
//     purchase: a ledger row plus a numbered receipt, in one transaction.
//
// Teepin holds no keys and signs nothing: it only watches an address.

const (
	// MaxCryptoTopUp bounds one USDC top-up, in USD. Higher than a card's limit:
	// there is no chargeback on a finalized on-chain payment.
	MaxCryptoTopUp = 5000.0

	// maxPendingCrypto caps unpaid USDC top-ups per account, so an account
	// cannot create work for the watcher without bound.
	maxPendingCrypto = 5

	// cryptoWatchWindow is how long an unpaid top-up is watched. A customer may
	// leave a payment link open for a while; one that never pays is closed.
	cryptoWatchWindow = 7 * 24 * time.Hour

	// DefaultCryptoWatchInterval is how often the watcher looks for payments.
	DefaultCryptoWatchInterval = 10 * time.Second
)

var (
	// ErrCryptoNotConfigured means USDC payments are not set up on this deployment.
	ErrCryptoNotConfigured = errors.New("USDC payments are not available")
	// ErrCryptoAmount means the amount is outside the allowed range or not whole cents.
	ErrCryptoAmount = fmt.Errorf("USDC top-up amount must be between $%.0f and $%.0f, in whole cents", MinTopUp, MaxCryptoTopUp)
	// ErrTooManyPending means the account already has too many unpaid USDC top-ups.
	ErrTooManyPending = errors.New("too many unpaid USDC top-ups; pay or wait for one to expire")
	// ErrSignatureUsed means a transaction already paid a different top-up.
	ErrSignatureUsed = errors.New("transaction already settled another top-up")
)

// SolanaConfig says where USDC payments go.
type SolanaConfig struct {
	Network solana.Network
	// Recipient is Teepin's wallet (a public key); the only thing the platform
	// ever needs to know about it.
	Recipient string
	// Mint is the USDC mint for the network.
	Mint string
}

type solanaPay struct {
	cfg SolanaConfig
	rpc solana.RPC
}

// WithSolana enables USDC top-ups. The recipient and mint are checked now, so a
// mistyped address stops the server starting instead of misdirecting payments.
func (s *Service) WithSolana(cfg SolanaConfig, rpc solana.RPC) (*Service, error) {
	recipient, err := solana.ParsePublicKey(cfg.Recipient)
	if err != nil {
		return s, fmt.Errorf("solana recipient: %w", err)
	}
	mint, err := solana.ParsePublicKey(cfg.Mint)
	if err != nil {
		return s, fmt.Errorf("solana usdc mint: %w", err)
	}
	if rpc == nil {
		return s, errors.New("solana rpc client is required")
	}
	s.solana = &solanaPay{cfg: SolanaConfig{Network: cfg.Network, Recipient: recipient, Mint: mint}, rpc: rpc}
	return s, nil
}

// CryptoEnabled reports whether USDC top-ups are available.
func (s *Service) CryptoEnabled() bool { return s.solana != nil }

// CryptoTopUpIntent is what the browser needs to show a payment request.
type CryptoTopUpIntent struct {
	TopUpID   uuid.UUID `json:"topup_id"`
	Amount    float64   `json:"amount"`
	Currency  string    `json:"currency"`
	Network   string    `json:"network"`
	Recipient string    `json:"recipient"`
	Mint      string    `json:"mint"`
	Reference string    `json:"reference"`
	// PayURL is the Solana Pay link; the QR code encodes exactly this.
	PayURL string `json:"pay_url"`
}

func cryptoAmountCents(amount float64) (int64, error) {
	if math.IsNaN(amount) || math.IsInf(amount, 0) {
		return 0, ErrCryptoAmount
	}
	cents := math.Round(amount * 100)
	if math.Abs(amount*100-cents) > 1e-6 {
		return 0, ErrCryptoAmount
	}
	if cents < MinTopUp*100 || cents > MaxCryptoTopUp*100 {
		return 0, ErrCryptoAmount
	}
	return int64(cents), nil
}

// payURL builds the Solana Pay request for a top-up.
func (s *Service) payURL(topUpID uuid.UUID, cents int64, reference string) (string, error) {
	return solana.TransferRequest{
		Recipient:   s.solana.cfg.Recipient,
		Mint:        s.solana.cfg.Mint,
		Reference:   reference,
		AmountMicro: solana.CentsToMicro(cents),
		Label:       "Teepin",
		Message:     "Teepin credit top-up",
		Memo:        "teepin-topup-" + topUpID.String(),
	}.URL()
}

// CreateCryptoTopUp starts a USDC purchase: it records a pending top-up with a
// fresh reference and returns the payment request. No credit is added here;
// that happens in SettleCryptoTopUp once the payment is finalized on chain.
func (s *Service) CreateCryptoTopUp(ctx context.Context, accountID uuid.UUID, amount float64) (*CryptoTopUpIntent, error) {
	// The amount is checked first so a customer is told what to fix even when
	// the method is unavailable, as for card top-ups.
	cents, err := cryptoAmountCents(amount)
	if err != nil {
		return nil, err
	}
	if s.solana == nil {
		return nil, ErrCryptoNotConfigured
	}

	var status string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM auth.accounts WHERE id = $1`, accountID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("account not found")
		}
		return nil, fmt.Errorf("failed to load account: %w", err)
	}
	if status == "closed" {
		return nil, ErrAccountClosed
	}

	var pending int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM billing.credit_topups
		WHERE account_id = $1 AND provider = 'solana' AND status = 'pending'
		  AND created_at > NOW() - make_interval(secs => $2)`,
		accountID, cryptoWatchWindow.Seconds()).Scan(&pending); err != nil {
		return nil, fmt.Errorf("failed to count pending top-ups: %w", err)
	}
	if pending >= maxPendingCrypto {
		return nil, ErrTooManyPending
	}

	reference, err := solana.NewReference()
	if err != nil {
		return nil, err
	}
	amountUSD := float64(cents) / 100
	var topUpID uuid.UUID
	if err := s.db.QueryRowContext(ctx, `
		INSERT INTO billing.credit_topups (account_id, amount, currency, provider, status, solana_reference)
		VALUES ($1, $2, $3, 'solana', 'pending', $4)
		RETURNING id`, accountID, amountUSD, topUpCurrency, reference).Scan(&topUpID); err != nil {
		return nil, fmt.Errorf("failed to record top-up: %w", err)
	}

	url, err := s.payURL(topUpID, cents, reference)
	if err != nil {
		// Cannot happen with a validated recipient/mint; if it does, do not
		// leave a pending row nobody can pay.
		_, _ = s.db.ExecContext(ctx, `UPDATE billing.credit_topups SET status = 'failed', failure_reason = $2 WHERE id = $1`, topUpID, err.Error())
		return nil, err
	}
	return &CryptoTopUpIntent{
		TopUpID: topUpID, Amount: amountUSD, Currency: topUpCurrency,
		Network: string(s.solana.cfg.Network), Recipient: s.solana.cfg.Recipient,
		Mint: s.solana.cfg.Mint, Reference: reference, PayURL: url,
	}, nil
}

// isUniqueViolation reports a Postgres unique-constraint violation.
func isUniqueViolation(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505"
}

func shortSignature(sig string) string {
	if len(sig) <= 14 {
		return sig
	}
	return sig[:6] + "..." + sig[len(sig)-6:]
}

// SettleCryptoTopUp turns a verified, finalized USDC payment into spendable
// credit. In one transaction it records the transaction signature, adds a
// 'purchase' row to the ledger and issues the numbered receipt.
//
// Safe to replay: a top-up that is no longer pending is left alone. A
// transaction can pay only ONE top-up (the signature is unique): a Solana Pay
// link may list several references, and without that a single payment could be
// presented as payment for many top-ups.
//
// What is credited is what actually arrived, in whole cents, rounded down. A
// payment outside the allowed range is not credited automatically: it is marked
// for review (the money is in the wallet; a person decides).
func (s *Service) SettleCryptoTopUp(ctx context.Context, topUpID uuid.UUID, p *solana.Payment) error {
	if s.solana == nil {
		return ErrCryptoNotConfigured
	}
	var accountID uuid.UUID
	err := s.db.QueryRowContext(ctx,
		`SELECT account_id FROM billing.credit_topups WHERE id = $1 AND provider = 'solana'`, topUpID).Scan(&accountID)
	if errors.Is(err, sql.ErrNoRows) {
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

	var status string
	if err := tx.QueryRowContext(ctx,
		`SELECT status FROM billing.credit_topups WHERE id = $1 FOR UPDATE`, topUpID).Scan(&status); err != nil {
		return fmt.Errorf("failed to lock top-up: %w", err)
	}
	if status != "pending" {
		return nil
	}

	cents := solana.MicroToCents(p.AmountMicro)
	if cents < int64(MinTopUp*100) || cents > int64(MaxCryptoTopUp*100) {
		reason := fmt.Sprintf("received %s USDC, outside the allowed range of $%.0f to $%.0f; needs manual review (transaction %s)",
			solana.FormatAmount(p.AmountMicro), MinTopUp, MaxCryptoTopUp, p.Signature)
		if _, err := tx.ExecContext(ctx, `
			UPDATE billing.credit_topups
			SET status = 'review', failure_reason = $2, solana_signature = $3, payer_address = $4,
			    received_micro = $5, updated_at = NOW()
			WHERE id = $1`, topUpID, reason, p.Signature, nullIfEmpty(p.Payer), p.AmountMicro); err != nil {
			if isUniqueViolation(err) {
				return ErrSignatureUsed
			}
			return fmt.Errorf("failed to mark top-up for review: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit review: %w", err)
		}
		log.Printf("ERROR: USDC top-up %s needs review: %s", topUpID, reason)
		return nil
	}
	amount := float64(cents) / 100

	// Claim the signature first: if another top-up already used it, stop here.
	if _, err := tx.ExecContext(ctx, `
		UPDATE billing.credit_topups
		SET solana_signature = $2, payer_address = $3, received_micro = $4, amount = $5, updated_at = NOW()
		WHERE id = $1`, topUpID, p.Signature, nullIfEmpty(p.Payer), p.AmountMicro, amount); err != nil {
		if isUniqueViolation(err) {
			return ErrSignatureUsed
		}
		return fmt.Errorf("failed to record the payment: %w", err)
	}

	methodSummary := "USDC on Solana (tx " + shortSignature(p.Signature) + ")"
	receiptID, invoiceNumber, err := s.recordPurchase(ctx, tx, bill, accountID, topUpID, amount, topUpCurrency, methodSummary)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE billing.credit_topups
		SET status = 'succeeded', succeeded_at = NOW(), failure_reason = NULL,
		    payment_method_summary = $2, receipt_invoice_id = $3, updated_at = NOW()
		WHERE id = $1`, topUpID, methodSummary, receiptID); err != nil {
		return fmt.Errorf("failed to mark top-up succeeded: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit top-up settlement: %w", err)
	}
	s.afterSettled(ctx, TopUpReceiptNotice{
		AccountID: accountID, ReceiptID: receiptID, ReceiptNumber: invoiceNumber,
		Amount: amount, Currency: topUpCurrency, PaymentMethod: methodSummary,
	}, topUpID)
	return nil
}

// CryptoWatcher looks for payments to pending USDC top-ups.
type CryptoWatcher struct {
	svc      *Service
	interval time.Duration
	stopChan chan struct{}
}

// NewCryptoWatcher builds the watcher.
func NewCryptoWatcher(svc *Service) *CryptoWatcher {
	return &CryptoWatcher{svc: svc, interval: DefaultCryptoWatchInterval, stopChan: make(chan struct{})}
}

// Start runs the watcher until Stop or ctx is cancelled.
func (w *CryptoWatcher) Start(ctx context.Context) {
	log.Printf("Starting USDC payment watcher (%s)...", w.svc.solana.cfg.Network)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	w.Sweep(ctx)
	for {
		select {
		case <-ticker.C:
			w.Sweep(ctx)
		case <-w.stopChan:
			return
		case <-ctx.Done():
			return
		}
	}
}

// Stop stops the watcher.
func (w *CryptoWatcher) Stop() { close(w.stopChan) }

type watchedTopUp struct {
	id        uuid.UUID
	reference string
}

// Sweep checks every pending USDC top-up that is due. A new top-up is checked
// every few seconds (the customer is waiting), an older one less often, and
// one past the window is closed.
func (w *CryptoWatcher) Sweep(ctx context.Context) {
	if _, err := w.svc.db.ExecContext(ctx, `
		UPDATE billing.credit_topups
		SET status = 'failed', failure_reason = 'no payment was received within 7 days', updated_at = NOW()
		WHERE provider = 'solana' AND status = 'pending'
		  AND created_at <= NOW() - make_interval(secs => $1)`, cryptoWatchWindow.Seconds()); err != nil {
		log.Printf("WARN: usdc watcher: closing stale top-ups: %v", err)
	}

	// Claiming (last_checked_at) and selecting in one statement, with SKIP
	// LOCKED, lets several replicas share the work without checking the same
	// top-up twice.
	rows, err := w.svc.db.QueryContext(ctx, `
		UPDATE billing.credit_topups t SET last_checked_at = NOW()
		WHERE t.id IN (
			SELECT id FROM billing.credit_topups
			WHERE provider = 'solana' AND status = 'pending'
			  AND created_at > NOW() - make_interval(secs => $1)
			  AND (last_checked_at IS NULL OR last_checked_at <= NOW() - CASE
			         WHEN created_at > NOW() - INTERVAL '30 minutes' THEN INTERVAL '10 seconds'
			         WHEN created_at > NOW() - INTERVAL '6 hours'    THEN INTERVAL '1 minute'
			         ELSE INTERVAL '10 minutes' END)
			ORDER BY created_at
			LIMIT 100
			FOR UPDATE SKIP LOCKED)
		RETURNING t.id, t.solana_reference`, cryptoWatchWindow.Seconds())
	if err != nil {
		log.Printf("WARN: usdc watcher: %v", err)
		return
	}
	var due []watchedTopUp
	for rows.Next() {
		var d watchedTopUp
		if err := rows.Scan(&d.id, &d.reference); err == nil {
			due = append(due, d)
		}
	}
	rows.Close()

	for _, d := range due {
		if ctx.Err() != nil {
			return
		}
		w.check(ctx, d)
	}
}

// check looks for a payment to one top-up and settles it if found.
func (w *CryptoWatcher) check(ctx context.Context, d watchedTopUp) {
	cfg := w.svc.solana.cfg
	sigs, err := w.svc.solana.rpc.SignaturesForAddress(ctx, d.reference, 20)
	if err != nil {
		log.Printf("WARN: usdc watcher: looking up top-up %s: %v", d.id, err)
		return
	}
	// Oldest first: the earliest valid payment is the one that counts.
	for i := len(sigs) - 1; i >= 0; i-- {
		if sigs[i].Failed() {
			continue
		}
		tx, err := w.svc.solana.rpc.Transaction(ctx, sigs[i].Signature)
		if err != nil {
			log.Printf("WARN: usdc watcher: reading transaction for top-up %s: %v", d.id, err)
			continue
		}
		if tx == nil {
			continue // not available as finalized yet; it will be seen again
		}
		pay, err := solana.ValidateUSDCPayment(tx, cfg.Recipient, cfg.Mint, d.reference)
		if err != nil {
			if !errors.Is(err, solana.ErrNoPayment) && !errors.Is(err, solana.ErrNoReference) && !errors.Is(err, solana.ErrTxFailed) {
				log.Printf("WARN: usdc watcher: top-up %s: transaction %s refused: %v", d.id, shortSignature(sigs[i].Signature), err)
			}
			continue
		}
		err = w.svc.SettleCryptoTopUp(ctx, d.id, pay)
		switch {
		case err == nil:
			log.Printf("USDC top-up %s settled (%s USDC, tx %s)", d.id, solana.FormatAmount(pay.AmountMicro), shortSignature(pay.Signature))
			return
		case errors.Is(err, ErrSignatureUsed):
			log.Printf("WARN: usdc watcher: transaction %s already paid another top-up; ignoring it for %s", shortSignature(pay.Signature), d.id)
		default:
			log.Printf("ERROR: usdc watcher: settling top-up %s: %v", d.id, err)
			return
		}
	}
}
