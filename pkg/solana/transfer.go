// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package solana

import (
	"errors"
	"fmt"
	"strconv"
)

var (
	// ErrTxFailed means the transaction errored on chain, so nothing moved.
	ErrTxFailed = errors.New("transaction failed on chain")
	// ErrNoReference means the transaction does not mention the payment
	// reference, so it is not a payment to this request.
	ErrNoReference = errors.New("transaction does not carry the payment reference")
	// ErrNoPayment means the transaction did not increase our USDC balance.
	ErrNoPayment = errors.New("transaction did not pay the recipient any USDC")
	// ErrWrongDecimals means the token reports a precision other than USDC's,
	// which is never a real USDC payment.
	ErrWrongDecimals = errors.New("token decimals do not match USDC")
	// ErrMalformedTx means the response lacks what verification needs.
	ErrMalformedTx = errors.New("transaction response is incomplete")
)

// Payment is a verified payment of USDC to our wallet.
type Payment struct {
	Signature   string
	Slot        uint64
	BlockTime   *int64
	Payer       string
	AmountMicro int64
}

// ValidateUSDCPayment decides whether tx is a real payment of USDC to
// recipient for the request identified by reference. It never looks at what the
// payer or the wallet CLAIMED; it reads what the chain says happened:
//
//   - the transaction succeeded;
//   - it names our reference (the reference is random per request, so only the
//     payer of that request can have put it there);
//   - the recipient's balance of the USDC mint rose, and by how much. The amount
//     is the net change across the recipient's token accounts, so a transaction
//     that also pays something back out cannot inflate it.
//
// It does not decide whether the amount is acceptable, and it cannot tell
// whether the same transaction already paid another request: that is a
// uniqueness check on the signature, made where the payment is recorded.
func ValidateUSDCPayment(tx *Transaction, recipient, mint, reference string) (*Payment, error) {
	if tx == nil || tx.Meta == nil || len(tx.Transaction.Signatures) == 0 {
		return nil, ErrMalformedTx
	}
	if tx.Meta.Failed() {
		return nil, ErrTxFailed
	}

	var hasRef bool
	var payer string
	for i, k := range tx.Transaction.Message.AccountKeys {
		if k.Pubkey == reference {
			hasRef = true
		}
		if i == 0 {
			payer = k.Pubkey // the fee payer is always the first account
		}
	}
	if !hasRef {
		return nil, ErrNoReference
	}

	pre := make(map[int]uint64, len(tx.Meta.PreTokenBalances))
	for _, b := range tx.Meta.PreTokenBalances {
		if b.Mint != mint || b.Owner != recipient {
			continue
		}
		if b.UITokenAmount.Decimals != USDCDecimals {
			return nil, ErrWrongDecimals
		}
		v, err := parseAmount(b.UITokenAmount.Amount)
		if err != nil {
			return nil, err
		}
		pre[b.AccountIndex] += v
	}

	var delta int64
	var sawPost bool
	for _, b := range tx.Meta.PostTokenBalances {
		if b.Mint != mint || b.Owner != recipient {
			continue
		}
		if b.UITokenAmount.Decimals != USDCDecimals {
			return nil, ErrWrongDecimals
		}
		post, err := parseAmount(b.UITokenAmount.Amount)
		if err != nil {
			return nil, err
		}
		sawPost = true
		// An account with no pre-balance entry is new in this transaction: it
		// started at zero.
		delta += int64(post) - int64(pre[b.AccountIndex])
		delete(pre, b.AccountIndex)
	}
	// A recipient account that appears only BEFORE (closed in this transaction)
	// lost its balance.
	for _, v := range pre {
		delta -= int64(v)
	}
	if !sawPost || delta <= 0 {
		return nil, ErrNoPayment
	}

	return &Payment{
		Signature:   tx.Transaction.Signatures[0],
		Slot:        tx.Slot,
		BlockTime:   tx.BlockTime,
		Payer:       payer,
		AmountMicro: delta,
	}, nil
}

// parseAmount reads a token amount in base units. Anything that does not fit
// comfortably in an int64 is refused: no real USDC balance does, and a value
// that large is more likely a malformed or hostile response than a payment.
func parseAmount(s string) (uint64, error) {
	v, err := strconv.ParseUint(s, 10, 63)
	if err != nil {
		return 0, fmt.Errorf("%w: token amount %q", ErrMalformedTx, s)
	}
	return v, nil
}
