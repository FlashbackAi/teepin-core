// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package solana

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

const (
	tRecipient = "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM"
	tMint      = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	tPayer     = "7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU"
	tRef       = "3Mx9qkHkYfFz5eC7a9bX8zQ2m4rT6uW1vN8sD3gL5pRj"
	tPayerATA  = "5vL5HXmHw8Cw8uqyd6aQBb6AnJjrzLZ4Gv7yUEJwQ9kR"
	tRecipATA  = "HgYvAYX1Pm5bKQsJ1bdLhW3tEw8Ar1x5CzQ2n9VfS4Uj"
)

// txJSON builds a jsonParsed getTransaction result. accountKeys are objects, as
// the node returns them.
func txJSON(t *testing.T, err string, keys []string, pre, post string) *Transaction {
	t.Helper()
	ks := ""
	for i, k := range keys {
		if i > 0 {
			ks += ","
		}
		ks += fmt.Sprintf(`{"pubkey":%q,"signer":%t,"writable":true,"source":"transaction"}`, k, i == 0)
	}
	raw := fmt.Sprintf(`{
		"slot": 300000000, "blockTime": 1790000000,
		"meta": {"err": %s, "preTokenBalances": [%s], "postTokenBalances": [%s]},
		"transaction": {"signatures": ["5sigSIGsig"], "message": {"accountKeys": [%s]}}
	}`, err, pre, post, ks)
	var tx Transaction
	if e := json.Unmarshal([]byte(raw), &tx); e != nil {
		t.Fatalf("fixture does not decode: %v", e)
	}
	return &tx
}

func bal(index int, mint, owner string, amount int64) string {
	return fmt.Sprintf(`{"accountIndex":%d,"mint":%q,"owner":%q,"uiTokenAmount":{"amount":"%d","decimals":6,"uiAmountString":"x"}}`,
		index, mint, owner, amount)
}

func keys() []string { return []string{tPayer, tPayerATA, tRecipATA, tRef} }

func TestValidateUSDCPayment_AcceptsARealPayment(t *testing.T) {
	// The payer's token account drops by 20 USDC, ours rises by 20.
	pre := bal(1, tMint, tPayer, 100_000_000) + "," + bal(2, tMint, tRecipient, 5_000_000)
	post := bal(1, tMint, tPayer, 80_000_000) + "," + bal(2, tMint, tRecipient, 25_000_000)
	p, err := ValidateUSDCPayment(txJSON(t, "null", keys(), pre, post), tRecipient, tMint, tRef)
	if err != nil {
		t.Fatalf("a real payment was refused: %v", err)
	}
	if p.AmountMicro != 20_000_000 || p.Payer != tPayer || p.Signature != "5sigSIGsig" || p.Slot != 300000000 {
		t.Errorf("payment = %+v", p)
	}
}

// Our token account did not exist before: it is created by the payment.
func TestValidateUSDCPayment_NewTokenAccountStartsAtZero(t *testing.T) {
	post := bal(1, tMint, tPayer, 80_000_000) + "," + bal(2, tMint, tRecipient, 20_000_000)
	pre := bal(1, tMint, tPayer, 100_000_000)
	p, err := ValidateUSDCPayment(txJSON(t, "null", keys(), pre, post), tRecipient, tMint, tRef)
	if err != nil || p.AmountMicro != 20_000_000 {
		t.Fatalf("payment = %+v, err = %v; want 20 USDC into a brand-new account", p, err)
	}
}

func TestValidateUSDCPayment_Refusals(t *testing.T) {
	good := func() (string, string) {
		return bal(2, tMint, tRecipient, 0), bal(2, tMint, tRecipient, 20_000_000)
	}
	other := "9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin"

	type tc struct {
		name string
		tx   func() *Transaction
		want error
	}
	cases := []tc{
		{"failed on chain", func() *Transaction {
			pre, post := good()
			return txJSON(t, `{"InstructionError":[0,"Custom"]}`, keys(), pre, post)
		}, ErrTxFailed},
		{"no reference", func() *Transaction {
			pre, post := good()
			return txJSON(t, "null", []string{tPayer, tPayerATA, tRecipATA}, pre, post)
		}, ErrNoReference},
		{"someone else's reference", func() *Transaction {
			pre, post := good()
			return txJSON(t, "null", []string{tPayer, tPayerATA, tRecipATA, other}, pre, post)
		}, ErrNoReference},
		{"paid a different token", func() *Transaction {
			pre := bal(2, other, tRecipient, 0)
			post := bal(2, other, tRecipient, 20_000_000)
			return txJSON(t, "null", keys(), pre, post)
		}, ErrNoPayment},
		{"paid a different wallet", func() *Transaction {
			pre := bal(2, tMint, other, 0)
			post := bal(2, tMint, other, 20_000_000)
			return txJSON(t, "null", keys(), pre, post)
		}, ErrNoPayment},
		{"balance unchanged", func() *Transaction {
			b := bal(2, tMint, tRecipient, 7_000_000)
			return txJSON(t, "null", keys(), b, b)
		}, ErrNoPayment},
		{"balance fell (we paid out)", func() *Transaction {
			return txJSON(t, "null", keys(), bal(2, tMint, tRecipient, 20_000_000), bal(2, tMint, tRecipient, 5_000_000))
		}, ErrNoPayment},
		{"no balances at all", func() *Transaction {
			return txJSON(t, "null", keys(), "", "")
		}, ErrNoPayment},
		{"wrong decimals", func() *Transaction {
			bad := `{"accountIndex":2,"mint":"` + tMint + `","owner":"` + tRecipient + `","uiTokenAmount":{"amount":"20000000000","decimals":9}}`
			return txJSON(t, "null", keys(), "", bad)
		}, ErrWrongDecimals},
		{"absurd amount", func() *Transaction {
			bad := `{"accountIndex":2,"mint":"` + tMint + `","owner":"` + tRecipient + `","uiTokenAmount":{"amount":"99999999999999999999999","decimals":6}}`
			return txJSON(t, "null", keys(), "", bad)
		}, ErrMalformedTx},
	}
	for _, c := range cases {
		if _, err := ValidateUSDCPayment(c.tx(), tRecipient, tMint, tRef); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	if _, err := ValidateUSDCPayment(nil, tRecipient, tMint, tRef); !errors.Is(err, ErrMalformedTx) {
		t.Errorf("nil transaction: %v", err)
	}
	if _, err := ValidateUSDCPayment(&Transaction{}, tRecipient, tMint, tRef); !errors.Is(err, ErrMalformedTx) {
		t.Errorf("empty transaction: %v", err)
	}
}

// If the transaction also pays something back out of our token account, only
// the net gain counts.
func TestValidateUSDCPayment_CountsOnlyTheNetGain(t *testing.T) {
	pre := bal(2, tMint, tRecipient, 10_000_000) + "," + bal(4, tMint, tRecipient, 10_000_000)
	post := bal(2, tMint, tRecipient, 40_000_000) + "," + bal(4, tMint, tRecipient, 5_000_000)
	p, err := ValidateUSDCPayment(txJSON(t, "null", append(keys(), "x"), pre, post), tRecipient, tMint, tRef)
	if err != nil || p.AmountMicro != 25_000_000 {
		t.Fatalf("payment = %+v, err = %v; want a net gain of 25 USDC (+30 -5)", p, err)
	}
}

func TestAccountKeyDecodesObjectsAndPlainStrings(t *testing.T) {
	var tx Transaction
	raw := `{"transaction":{"signatures":["s"],"message":{"accountKeys":["` + tPayer + `",{"pubkey":"` + tRef + `","signer":false,"writable":false}]}},"meta":{"err":null}}`
	if err := json.Unmarshal([]byte(raw), &tx); err != nil {
		t.Fatal(err)
	}
	k := tx.Transaction.Message.AccountKeys
	if len(k) != 2 || k[0].Pubkey != tPayer || k[1].Pubkey != tRef {
		t.Errorf("keys = %+v", k)
	}
}
