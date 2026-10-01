// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package solana

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Network is the Solana cluster payments are accepted on.
type Network string

const (
	Mainnet Network = "mainnet-beta"
	Devnet  Network = "devnet"
)

// USDCDecimals is the precision of USDC on Solana: 1 USDC = 1,000,000 base units.
const USDCDecimals = 6

// microPerUSD is how many USDC base units make one US dollar (USDC is pegged 1:1).
const microPerUSD int64 = 1_000_000

// microPerCent is how many base units make one US cent.
const microPerCent int64 = microPerUSD / 100

// usdcMints are Circle's published USDC mint addresses.
// Source: https://developers.circle.com/stablecoins/usdc-contract-addresses
var usdcMints = map[Network]string{
	Mainnet: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
	Devnet:  "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU",
}

// ParseNetwork validates a configured network name.
func ParseNetwork(s string) (Network, error) {
	switch n := Network(strings.ToLower(strings.TrimSpace(s))); n {
	case Mainnet, Devnet:
		return n, nil
	default:
		return "", fmt.Errorf("unknown Solana network %q (want %q or %q)", s, Mainnet, Devnet)
	}
}

// USDCMint returns the USDC mint for a network.
func USDCMint(n Network) (string, error) {
	m, ok := usdcMints[n]
	if !ok {
		return "", fmt.Errorf("no USDC mint known for network %q", n)
	}
	return m, nil
}

// NewReference returns a fresh Solana Pay reference: 32 random bytes in base58.
// It rides along in the payment as a read-only account, which is what lets us
// find that one payment on chain later. It is never a signer and no private key
// exists for it.
func NewReference() (string, error) {
	b := make([]byte, PublicKeyLen)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("could not generate a payment reference: %w", err)
	}
	return EncodeBase58(b), nil
}

// CentsToMicro converts US cents to USDC base units.
func CentsToMicro(cents int64) int64 { return cents * microPerCent }

// MicroToCents converts USDC base units to whole US cents, rounding DOWN. A
// payment is only ever credited in whole cents, never more than what arrived.
func MicroToCents(micro int64) int64 {
	if micro <= 0 {
		return 0
	}
	return micro / microPerCent
}

// FormatAmount writes base units as the plain decimal a Solana Pay URL needs:
// no exponent, a leading zero below one, no trailing zeros ("20", "20.5",
// "0.05", "20.123456").
func FormatAmount(micro int64) string {
	whole := micro / microPerUSD
	frac := micro % microPerUSD
	if frac < 0 {
		frac = -frac
	}
	if frac == 0 {
		return strconv.FormatInt(whole, 10)
	}
	f := fmt.Sprintf("%06d", frac)
	return fmt.Sprintf("%d.%s", whole, strings.TrimRight(f, "0"))
}

// TransferRequest is a Solana Pay "transfer request": a link or QR that tells a
// wallet to send USDC to us, tagged with a reference.
// Spec: https://docs.solanapay.com/spec
type TransferRequest struct {
	// Recipient is OUR wallet's public key (not a token account).
	Recipient string
	// Mint is the SPL token mint being paid in (USDC).
	Mint string
	// Reference is the unique per-payment key from NewReference.
	Reference string
	// AmountMicro is the amount in USDC base units.
	AmountMicro int64
	Label       string
	Message     string
	Memo        string
}

// ErrBadTransferRequest means the request cannot form a valid URL.
var ErrBadTransferRequest = errors.New("invalid Solana Pay transfer request")

// URL renders the request as a "solana:" link, validating every field the wallet
// will rely on so a bad request fails here rather than as a confused wallet.
func (r TransferRequest) URL() (string, error) {
	if _, err := ParsePublicKey(r.Recipient); err != nil {
		return "", fmt.Errorf("%w: recipient: %v", ErrBadTransferRequest, err)
	}
	if _, err := ParsePublicKey(r.Mint); err != nil {
		return "", fmt.Errorf("%w: token mint: %v", ErrBadTransferRequest, err)
	}
	if _, err := ParsePublicKey(r.Reference); err != nil {
		return "", fmt.Errorf("%w: reference: %v", ErrBadTransferRequest, err)
	}
	if r.AmountMicro <= 0 {
		return "", fmt.Errorf("%w: amount must be positive", ErrBadTransferRequest)
	}

	q := []string{
		"amount=" + FormatAmount(r.AmountMicro),
		"spl-token=" + r.Mint,
		"reference=" + r.Reference,
	}
	if r.Label != "" {
		q = append(q, "label="+escape(r.Label))
	}
	if r.Message != "" {
		q = append(q, "message="+escape(r.Message))
	}
	if r.Memo != "" {
		q = append(q, "memo="+escape(r.Memo))
	}
	return "solana:" + r.Recipient + "?" + strings.Join(q, "&"), nil
}

// escape URL-encodes a query value with %20 for spaces. The spec asks for
// URL-encoded text, and some wallets do not decode '+' as a space.
func escape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}
