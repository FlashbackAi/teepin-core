// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build migrationdrill

// Runs USDC top-ups on Solana against a real Postgres with a fake chain. See
// credits_integration_test.go for how to run it.
package billing

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strconv"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/solana"
)

// fakeChain is a stand-in for the Solana RPC. Transactions are keyed by
// signature; references map to the signatures that mention them.
type fakeChain struct {
	mu    sync.Mutex
	byRef map[string][]solana.SignatureInfo
	txs   map[string]*solana.Transaction
	calls int
}

func newFakeChain() *fakeChain {
	return &fakeChain{byRef: map[string][]solana.SignatureInfo{}, txs: map[string]*solana.Transaction{}}
}

func (c *fakeChain) SignaturesForAddress(_ context.Context, address string, _ int) ([]solana.SignatureInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.byRef[address], nil
}

func (c *fakeChain) Transaction(_ context.Context, sig string) (*solana.Transaction, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.txs[sig], nil
}

// pay puts a finalized payment of micro USDC to recipient on the chain, tagged
// with every reference given.
func (c *fakeChain) pay(recipient, mint, payer string, micro int64, refs ...string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	sig := "sig-" + uuid.NewString()
	keys := []solana.AccountKey{{Pubkey: payer, Signer: true}}
	for _, r := range refs {
		keys = append(keys, solana.AccountKey{Pubkey: r})
	}
	tx := &solana.Transaction{Slot: 100, Meta: &solana.TxMeta{
		PreTokenBalances: []solana.TokenBalance{
			{AccountIndex: 1, Mint: mint, Owner: recipient, UITokenAmount: solana.TokenAmount{Amount: "1000000", Decimals: 6}}},
		PostTokenBalances: []solana.TokenBalance{
			{AccountIndex: 1, Mint: mint, Owner: recipient, UITokenAmount: solana.TokenAmount{Amount: strconv.FormatInt(1000000+micro, 10), Decimals: 6}}},
	}}
	tx.Transaction.Signatures = []string{sig}
	tx.Transaction.Message.AccountKeys = keys
	c.txs[sig] = tx
	for _, r := range refs {
		c.byRef[r] = append([]solana.SignatureInfo{{Signature: sig, Slot: 100}}, c.byRef[r]...)
	}
	return sig
}

type cryptoHarness struct {
	t     *testing.T
	db    *sql.DB
	svc   *Service
	chain *fakeChain
	w     *CryptoWatcher
	acct  uuid.UUID
	cfg   SolanaConfig
	payer string
}

func newCryptoHarness(t *testing.T) *cryptoHarness {
	t.Helper()
	db := integrationDB(t)
	recipient, err := solana.NewReference() // any random 32-byte key is a valid address
	if err != nil {
		t.Fatal(err)
	}
	payer, _ := solana.NewReference()
	mint, err := solana.USDCMint(solana.Devnet)
	if err != nil {
		t.Fatal(err)
	}
	cfg := SolanaConfig{Network: solana.Devnet, Recipient: recipient, Mint: mint}
	chain := newFakeChain()
	svc, err := NewService(db).WithSolana(cfg, chain)
	if err != nil {
		t.Fatalf("WithSolana: %v", err)
	}
	return &cryptoHarness{t: t, db: db, svc: svc, chain: chain, w: NewCryptoWatcher(svc), acct: itAccount(t, db), cfg: cfg, payer: payer}
}

func (h *cryptoHarness) create(amount float64) *CryptoTopUpIntent {
	h.t.Helper()
	in, err := h.svc.CreateCryptoTopUp(context.Background(), h.acct, amount)
	if err != nil {
		h.t.Fatalf("CreateCryptoTopUp(%v): %v", amount, err)
	}
	return in
}

// sweep runs the watcher once, treating every pending top-up as due.
func (h *cryptoHarness) sweep() {
	h.t.Helper()
	if _, err := h.db.Exec(`UPDATE billing.credit_topups SET last_checked_at = NULL WHERE provider = 'solana'`); err != nil {
		h.t.Fatal(err)
	}
	h.w.Sweep(context.Background())
}

func (h *cryptoHarness) status(id uuid.UUID) string {
	h.t.Helper()
	var s string
	if err := h.db.QueryRow(`SELECT status FROM billing.credit_topups WHERE id = $1`, id).Scan(&s); err != nil {
		h.t.Fatal(err)
	}
	return s
}

func (h *cryptoHarness) purchases(id uuid.UUID) int {
	h.t.Helper()
	var n int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM billing.credit_transactions WHERE topup_id = $1`, id).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func usd(v float64) int64 { return int64(math.Round(v * 1e6)) }

// The whole path: a finalized payment to the right wallet with the right
// reference becomes exactly that much credit, once, with a receipt.
func TestCryptoTopUpIntegration_PaymentBecomesCreditOnce(t *testing.T) {
	h := newCryptoHarness(t)
	in := h.create(25)
	if in.PayURL == "" || in.Reference == "" || in.Recipient != h.cfg.Recipient {
		t.Fatalf("intent = %+v", in)
	}
	if got := h.status(in.TopUpID); got != "pending" {
		t.Fatalf("status before payment = %s", got)
	}
	if bal := itBalance(t, h.svc, h.acct); !itNear(bal, 0) {
		t.Fatalf("balance before payment = %v, want 0", bal)
	}

	h.sweep() // nothing on chain yet
	if got := h.status(in.TopUpID); got != "pending" {
		t.Fatalf("status with no payment = %s", got)
	}

	h.chain.pay(h.cfg.Recipient, h.cfg.Mint, h.payer, usd(25), in.Reference)
	h.sweep()
	if got := h.status(in.TopUpID); got != "succeeded" {
		t.Fatalf("status after payment = %s", got)
	}
	if bal := itBalance(t, h.svc, h.acct); !itNear(bal, 25) {
		t.Fatalf("balance = %v, want 25", bal)
	}

	var receipt sql.NullString
	var signature, payer sql.NullString
	var received sql.NullInt64
	if err := h.db.QueryRow(`SELECT receipt_invoice_id::text, solana_signature, payer_address, received_micro FROM billing.credit_topups WHERE id = $1`, in.TopUpID).
		Scan(&receipt, &signature, &payer, &received); err != nil {
		t.Fatal(err)
	}
	if !receipt.Valid || !signature.Valid || payer.String != h.payer || received.Int64 != usd(25) {
		t.Errorf("settled row: receipt=%v signature=%v payer=%v received=%v", receipt, signature, payer, received)
	}

	// Looking again changes nothing.
	h.sweep()
	h.sweep()
	if bal := itBalance(t, h.svc, h.acct); !itNear(bal, 25) {
		t.Fatalf("balance after repeated sweeps = %v, want 25", bal)
	}
	if n := h.purchases(in.TopUpID); n != 1 {
		t.Fatalf("ledger rows = %d, want 1", n)
	}
}

// What counts is what arrived, in whole cents, rounded down.
func TestCryptoTopUpIntegration_CreditsWhatArrivedRoundedDown(t *testing.T) {
	h := newCryptoHarness(t)
	in := h.create(25)
	h.chain.pay(h.cfg.Recipient, h.cfg.Mint, h.payer, 24_999_999, in.Reference) // one micro-dollar short of $25.00
	h.sweep()
	if bal := itBalance(t, h.svc, h.acct); !itNear(bal, 24.99) {
		t.Fatalf("balance = %v, want 24.99 (floored, never rounded up)", bal)
	}
	var amount float64
	if err := h.db.QueryRow(`SELECT amount FROM billing.credit_topups WHERE id = $1`, in.TopUpID).Scan(&amount); err != nil || !itNear(amount, 24.99) {
		t.Errorf("top-up amount = %v (err %v), want the credited 24.99", amount, err)
	}
}

// A customer who sends a different amount than the link asked for gets credit
// for what actually arrived, not what was requested.
func TestCryptoTopUpIntegration_UnderAndOverpaymentCreditTheReceivedAmount(t *testing.T) {
	h := newCryptoHarness(t)
	under, over := h.create(50), h.create(50)
	h.chain.pay(h.cfg.Recipient, h.cfg.Mint, h.payer, usd(30), under.Reference)
	h.chain.pay(h.cfg.Recipient, h.cfg.Mint, h.payer, usd(70), over.Reference)
	h.sweep()
	if bal := itBalance(t, h.svc, h.acct); !itNear(bal, 100) {
		t.Fatalf("balance = %v, want 100 (30 + 70 as received)", bal)
	}
}

// A payment outside the allowed range is not credited by machine: the money is
// in the wallet, so a person decides.
func TestCryptoTopUpIntegration_OutOfRangeGoesToReview(t *testing.T) {
	h := newCryptoHarness(t)
	small, large := h.create(25), h.create(25)
	h.chain.pay(h.cfg.Recipient, h.cfg.Mint, h.payer, usd(5), small.Reference)
	h.chain.pay(h.cfg.Recipient, h.cfg.Mint, h.payer, usd(6000), large.Reference)
	h.sweep()
	for _, in := range []*CryptoTopUpIntent{small, large} {
		if got := h.status(in.TopUpID); got != "review" {
			t.Errorf("status = %s, want review", got)
		}
		if n := h.purchases(in.TopUpID); n != 0 {
			t.Errorf("a payment held for review created %d ledger rows", n)
		}
	}
	if bal := itBalance(t, h.svc, h.acct); !itNear(bal, 0) {
		t.Fatalf("balance = %v, want 0", bal)
	}
	// Held payments are not re-examined on every sweep.
	h.sweep()
	if got := h.status(small.TopUpID); got != "review" {
		t.Errorf("status after another sweep = %s", got)
	}
}

// Payments that do not satisfy the checks never become credit.
func TestCryptoTopUpIntegration_RefusesWhatIsNotAPaymentToUs(t *testing.T) {
	h := newCryptoHarness(t)
	otherWallet, _ := solana.NewReference()
	otherMint, _ := solana.NewReference()

	wrongWallet, wrongMint, failed, notFinal := h.create(25), h.create(25), h.create(25), h.create(25)
	h.chain.pay(otherWallet, h.cfg.Mint, h.payer, usd(25), wrongWallet.Reference)
	h.chain.pay(h.cfg.Recipient, otherMint, h.payer, usd(25), wrongMint.Reference)

	h.chain.pay(h.cfg.Recipient, h.cfg.Mint, h.payer, usd(25), failed.Reference)
	h.chain.byRef[failed.Reference][0].Err = []byte(`{"InstructionError":[0,"Custom"]}`)

	notFinalSig := h.chain.pay(h.cfg.Recipient, h.cfg.Mint, h.payer, usd(25), notFinal.Reference)
	notFinalTx := h.chain.txs[notFinalSig]
	delete(h.chain.txs, notFinalSig) // listed, but not readable as finalized yet

	h.sweep()
	for name, in := range map[string]*CryptoTopUpIntent{"wrong wallet": wrongWallet, "wrong mint": wrongMint, "failed tx": failed, "not finalized": notFinal} {
		if got := h.status(in.TopUpID); got != "pending" {
			t.Errorf("%s: status = %s, want pending", name, got)
		}
	}
	if bal := itBalance(t, h.svc, h.acct); !itNear(bal, 0) {
		t.Fatalf("balance = %v, want 0", bal)
	}

	// Once the chain finalizes it, the very next look settles it.
	h.chain.mu.Lock()
	h.chain.txs[notFinalSig] = notFinalTx
	h.chain.mu.Unlock()
	h.sweep()
	if got := h.status(notFinal.TopUpID); got != "succeeded" {
		t.Errorf("status once finalized = %s, want succeeded", got)
	}
	if bal := itBalance(t, h.svc, h.acct); !itNear(bal, 25) {
		t.Errorf("balance once finalized = %v, want 25 (only that payment)", bal)
	}
}

// A transaction that does not carry the top-up's own reference never pays it,
// even when it pays our wallet the right amount.
func TestCryptoTopUpIntegration_PaymentWithoutTheReferenceIsIgnored(t *testing.T) {
	h := newCryptoHarness(t)
	in := h.create(25)
	other, _ := solana.NewReference()
	sig := h.chain.pay(h.cfg.Recipient, h.cfg.Mint, h.payer, usd(25), other)
	// Even if the index wrongly lists it under this reference, the transaction
	// itself must contain the reference.
	h.chain.byRef[in.Reference] = []solana.SignatureInfo{{Signature: sig}}
	h.sweep()
	if got := h.status(in.TopUpID); got != "pending" {
		t.Fatalf("status = %s, want pending", got)
	}
}

// One transaction can carry several references (a Solana Pay link may list
// many). It must pay only one top-up.
func TestCryptoTopUpIntegration_OneTransactionPaysOnlyOneTopUp(t *testing.T) {
	h := newCryptoHarness(t)
	a, b := h.create(25), h.create(25)
	h.chain.pay(h.cfg.Recipient, h.cfg.Mint, h.payer, usd(25), a.Reference, b.Reference)
	h.sweep()
	h.sweep()
	succeeded := 0
	for _, in := range []*CryptoTopUpIntent{a, b} {
		if h.status(in.TopUpID) == "succeeded" {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d top-ups settled from one transaction, want exactly 1", succeeded)
	}
	if bal := itBalance(t, h.svc, h.acct); !itNear(bal, 25) {
		t.Fatalf("balance = %v, want 25 (one payment, one credit)", bal)
	}
}

// Settlement attempted from several places at once credits once.
func TestCryptoTopUpIntegration_ConcurrentSettlementCreditsOnce(t *testing.T) {
	h := newCryptoHarness(t)
	in := h.create(25)
	sig := h.chain.pay(h.cfg.Recipient, h.cfg.Mint, h.payer, usd(25), in.Reference)
	pay, err := solana.ValidateUSDCPayment(h.chain.txs[sig], h.cfg.Recipient, h.cfg.Mint, in.Reference)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h.svc.SettleCryptoTopUp(context.Background(), in.TopUpID, pay); err != nil {
				t.Errorf("settle: %v", err)
			}
		}()
	}
	wg.Wait()
	if n := h.purchases(in.TopUpID); n != 1 {
		t.Fatalf("ledger rows = %d, want 1", n)
	}
	if bal := itBalance(t, h.svc, h.acct); !itNear(bal, 25) {
		t.Fatalf("balance = %v, want 25", bal)
	}
}

// Settling a top-up with a signature another top-up already used is refused,
// directly (not only via the watcher).
func TestCryptoTopUpIntegration_SettleRefusesAUsedSignature(t *testing.T) {
	h := newCryptoHarness(t)
	a, b := h.create(25), h.create(25)
	sig := h.chain.pay(h.cfg.Recipient, h.cfg.Mint, h.payer, usd(25), a.Reference, b.Reference)
	payA, _ := solana.ValidateUSDCPayment(h.chain.txs[sig], h.cfg.Recipient, h.cfg.Mint, a.Reference)
	payB, _ := solana.ValidateUSDCPayment(h.chain.txs[sig], h.cfg.Recipient, h.cfg.Mint, b.Reference)
	if err := h.svc.SettleCryptoTopUp(context.Background(), a.TopUpID, payA); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.SettleCryptoTopUp(context.Background(), b.TopUpID, payB); !errors.Is(err, ErrSignatureUsed) {
		t.Fatalf("err = %v, want ErrSignatureUsed", err)
	}
	if h.status(b.TopUpID) != "pending" || h.purchases(b.TopUpID) != 0 {
		t.Fatal("the refused settlement left a trace")
	}
}

func TestCryptoTopUpIntegration_AmountValidation(t *testing.T) {
	h := newCryptoHarness(t)
	for _, bad := range []float64{0, -5, 19.99, 25.005, 5000.01, math.NaN(), math.Inf(1)} {
		if _, err := h.svc.CreateCryptoTopUp(context.Background(), h.acct, bad); !errors.Is(err, ErrCryptoAmount) {
			t.Errorf("amount %v: err = %v, want ErrCryptoAmount", bad, err)
		}
	}
	for _, ok := range []float64{20, 25.5, 5000} {
		if _, err := h.svc.CreateCryptoTopUp(context.Background(), h.acct, ok); err != nil {
			t.Errorf("amount %v refused: %v", ok, err)
		}
	}
}

func TestCryptoTopUpIntegration_PendingCap(t *testing.T) {
	h := newCryptoHarness(t)
	for i := 0; i < maxPendingCrypto; i++ {
		h.create(20)
	}
	if _, err := h.svc.CreateCryptoTopUp(context.Background(), h.acct, 20); !errors.Is(err, ErrTooManyPending) {
		t.Fatalf("err = %v, want ErrTooManyPending", err)
	}
	// Another account is unaffected.
	if _, err := h.svc.CreateCryptoTopUp(context.Background(), itAccount(t, h.db), 20); err != nil {
		t.Fatalf("a different account was limited: %v", err)
	}
}

func TestCryptoTopUpIntegration_StaleTopUpsAreClosed(t *testing.T) {
	h := newCryptoHarness(t)
	in := h.create(25)
	if _, err := h.db.Exec(`UPDATE billing.credit_topups SET created_at = NOW() - INTERVAL '8 days' WHERE id = $1`, in.TopUpID); err != nil {
		t.Fatal(err)
	}
	// A payment that turns up after the window is not credited by machine.
	h.chain.pay(h.cfg.Recipient, h.cfg.Mint, h.payer, usd(25), in.Reference)
	h.sweep()
	if got := h.status(in.TopUpID); got != "failed" {
		t.Fatalf("status = %s, want failed", got)
	}
	if bal := itBalance(t, h.svc, h.acct); !itNear(bal, 0) {
		t.Fatalf("balance = %v, want 0", bal)
	}
}

// Each pending top-up is looked at on a schedule, not on every sweep.
func TestCryptoTopUpIntegration_WatcherPacesItsLookups(t *testing.T) {
	h := newCryptoHarness(t)
	h.create(25)
	h.w.Sweep(context.Background())
	first := h.chain.calls
	if first == 0 {
		t.Fatal("a new top-up was not checked")
	}
	h.w.Sweep(context.Background()) // immediately again: not due
	if h.chain.calls != first {
		t.Errorf("checked again straight away (%d -> %d calls)", first, h.chain.calls)
	}
}

func TestCryptoTopUpIntegration_NotConfigured(t *testing.T) {
	db := integrationDB(t)
	svc := NewService(db)
	if svc.CryptoEnabled() {
		t.Fatal("enabled without configuration")
	}
	if _, err := svc.CreateCryptoTopUp(context.Background(), itAccount(t, db), 25); !errors.Is(err, ErrCryptoNotConfigured) {
		t.Fatalf("err = %v, want ErrCryptoNotConfigured", err)
	}
}

func TestCryptoTopUpIntegration_RejectsBadConfiguration(t *testing.T) {
	db := integrationDB(t)
	good, _ := solana.NewReference()
	mint, _ := solana.USDCMint(solana.Devnet)
	if _, err := NewService(db).WithSolana(SolanaConfig{Recipient: "not-a-key", Mint: mint}, newFakeChain()); err == nil {
		t.Error("a malformed recipient was accepted")
	}
	if _, err := NewService(db).WithSolana(SolanaConfig{Recipient: good, Mint: "x"}, newFakeChain()); err == nil {
		t.Error("a malformed mint was accepted")
	}
	if _, err := NewService(db).WithSolana(SolanaConfig{Recipient: good, Mint: mint}, nil); err == nil {
		t.Error("a missing rpc client was accepted")
	}
}

// Other accounts' credit and top-ups are untouched.
func TestCryptoTopUpIntegration_CreditGoesToTheOwningAccountOnly(t *testing.T) {
	h := newCryptoHarness(t)
	other := itAccount(t, h.db)
	in := h.create(25)
	h.chain.pay(h.cfg.Recipient, h.cfg.Mint, h.payer, usd(25), in.Reference)
	h.sweep()
	if bal := itBalance(t, h.svc, other); !itNear(bal, 0) {
		t.Fatalf("another account's balance = %v, want 0", bal)
	}
	if bal := itBalance(t, h.svc, h.acct); !itNear(bal, 25) {
		t.Fatalf("owner's balance = %v, want 25", bal)
	}
}
