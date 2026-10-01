// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package solana

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

const (
	testWallet = "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM"
	testMint   = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
)

func TestFormatAmount(t *testing.T) {
	cases := map[int64]string{
		0:                 "0",
		1:                 "0.000001",
		50_000:            "0.05",
		500_000:           "0.5",
		1_000_000:         "1",
		20_000_000:        "20",
		20_500_000:        "20.5",
		20_550_000:        "20.55",
		20_123_456:        "20.123456",
		1_000_000_000_000: "1000000",
	}
	for micro, want := range cases {
		if got := FormatAmount(micro); got != want {
			t.Errorf("FormatAmount(%d) = %q, want %q", micro, got, want)
		}
		if strings.ContainsAny(FormatAmount(micro), "eE") {
			t.Errorf("FormatAmount(%d) used scientific notation", micro)
		}
	}
}

func TestCentsAndMicroConversions(t *testing.T) {
	if CentsToMicro(2000) != 20_000_000 {
		t.Error("$20.00 should be 20,000,000 base units")
	}
	cases := map[int64]int64{
		-5:         0,
		0:          0,
		9_999:      0, // under a cent: nothing credited
		10_000:     1,
		20_000_000: 2000,
		// Dust beyond a whole cent is never credited.
		20_009_999: 2000,
		20_010_000: 2001,
	}
	for micro, want := range cases {
		if got := MicroToCents(micro); got != want {
			t.Errorf("MicroToCents(%d) = %d, want %d", micro, got, want)
		}
	}
}

func TestTransferRequestURL(t *testing.T) {
	ref, err := NewReference()
	if err != nil {
		t.Fatal(err)
	}
	got, err := TransferRequest{
		Recipient: testWallet, Mint: testMint, Reference: ref,
		AmountMicro: 25_500_000, Label: "Teepin", Message: "Credit top-up", Memo: "topup 1&2",
	}.URL()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "solana:"+testWallet+"?") {
		t.Fatalf("url = %q", got)
	}
	// The link must parse back to exactly the fields we put in.
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Opaque // solana:<recipient>?query
	_ = q
	query := got[strings.Index(got, "?")+1:]
	vals, err := url.ParseQuery(query)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"amount": "25.5", "spl-token": testMint, "reference": ref,
		"label": "Teepin", "message": "Credit top-up", "memo": "topup 1&2",
	}
	for k, v := range want {
		if vals.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, vals.Get(k), v)
		}
	}
	if strings.Contains(got, "+") {
		t.Errorf("a space was encoded as '+': %q", got)
	}
	if !strings.Contains(got, "message=Credit%20top-up") {
		t.Errorf("spaces must be %%20: %q", got)
	}
	if strings.Contains(got, "&1&") || strings.Contains(got, "memo=topup 1&2") {
		t.Errorf("an ampersand in the memo leaked into the query: %q", got)
	}
}

func TestTransferRequestRejectsBadFields(t *testing.T) {
	ref, _ := NewReference()
	ok := TransferRequest{Recipient: testWallet, Mint: testMint, Reference: ref, AmountMicro: 1}
	if _, err := ok.URL(); err != nil {
		t.Fatalf("valid request refused: %v", err)
	}
	for name, mutate := range map[string]func(*TransferRequest){
		"bad recipient":   func(r *TransferRequest) { r.Recipient = "nope" },
		"bad mint":        func(r *TransferRequest) { r.Mint = "" },
		"bad reference":   func(r *TransferRequest) { r.Reference = "0OIl" },
		"zero amount":     func(r *TransferRequest) { r.AmountMicro = 0 },
		"negative amount": func(r *TransferRequest) { r.AmountMicro = -1 },
	} {
		r := ok
		mutate(&r)
		if _, err := r.URL(); !errors.Is(err, ErrBadTransferRequest) {
			t.Errorf("%s: err = %v, want ErrBadTransferRequest", name, err)
		}
	}
}

func TestNewReferenceIsUniqueAndValid(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		r, err := NewReference()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParsePublicKey(r); err != nil {
			t.Fatalf("reference %q is not a valid 32-byte key: %v", r, err)
		}
		if seen[r] {
			t.Fatalf("duplicate reference %q", r)
		}
		seen[r] = true
	}
}

func TestParseNetworkAndMints(t *testing.T) {
	for _, n := range []Network{Mainnet, Devnet} {
		got, err := ParseNetwork(string(n))
		if err != nil || got != n {
			t.Errorf("ParseNetwork(%q) = %q, %v", n, got, err)
		}
		mint, err := USDCMint(n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParsePublicKey(mint); err != nil {
			t.Errorf("%s USDC mint %q is not a valid key: %v", n, mint, err)
		}
	}
	if m, _ := USDCMint(Mainnet); m == mustMint(t, Devnet) {
		t.Error("mainnet and devnet must not share a mint")
	}
	if _, err := ParseNetwork("testnet"); err == nil {
		t.Error("an unknown network was accepted")
	}
}

func mustMint(t *testing.T, n Network) string {
	t.Helper()
	m, err := USDCMint(n)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
