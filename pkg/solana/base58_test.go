// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package solana

import (
	"bytes"
	"crypto/rand"
	"errors"
	"testing"
)

// Known vectors from the base58 reference test set, so a bug in the encoder
// cannot hide behind a decoder with the same bug.
func TestBase58KnownVectors(t *testing.T) {
	cases := []struct {
		hex string
		b58 string
	}{
		{"", ""},
		{"61", "2g"},
		{"626262", "a3gV"},
		{"636363", "aPEr"},
		{"00000000000000000000", "1111111111"},
		{"0000287fb4cd", "11233QC4"},
		{"00eb15231dfceb60925886b67d065299925915aeb172c06647", "1NS17iag9jJgTHD1VXjvLCEnZuQ3rJDE9L"},
	}
	for _, c := range cases {
		raw := mustHex(t, c.hex)
		if got := EncodeBase58(raw); got != c.b58 {
			t.Errorf("Encode(%s) = %q, want %q", c.hex, got, c.b58)
		}
		back, err := DecodeBase58(c.b58)
		if err != nil || !bytes.Equal(back, raw) {
			t.Errorf("Decode(%q) = %x, %v; want %s", c.b58, back, err, c.hex)
		}
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b := make([]byte, len(s)/2)
	for i := 0; i < len(b); i++ {
		var v byte
		for _, c := range s[2*i : 2*i+2] {
			v <<= 4
			switch {
			case c >= '0' && c <= '9':
				v |= byte(c - '0')
			case c >= 'a' && c <= 'f':
				v |= byte(c-'a') + 10
			default:
				t.Fatalf("bad hex %q", s)
			}
		}
		b[i] = v
	}
	return b
}

func TestBase58RoundTripsRandomKeys(t *testing.T) {
	for i := 0; i < 500; i++ {
		b := make([]byte, PublicKeyLen)
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
		if i%7 == 0 {
			b[0], b[1] = 0, 0 // exercise leading zeros
		}
		got, err := DecodeBase58(EncodeBase58(b))
		if err != nil || !bytes.Equal(got, b) {
			t.Fatalf("round trip failed for %x: %x, %v", b, got, err)
		}
	}
}

func TestDecodeBase58RejectsForeignCharacters(t *testing.T) {
	for _, s := range []string{"0", "O", "I", "l", "abc def", "abc+", "é"} {
		if _, err := DecodeBase58(s); !errors.Is(err, ErrInvalidBase58) {
			t.Errorf("Decode(%q) err = %v, want ErrInvalidBase58", s, err)
		}
	}
}

// The real USDC mint addresses, from Circle's published list, must parse as
// 32-byte keys: a guard against a mistyped constant.
func TestParsePublicKey(t *testing.T) {
	for _, ok := range []string{
		"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", // USDC, mainnet
		"4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU", // USDC, devnet
		"11111111111111111111111111111111",             // system program
	} {
		got, err := ParsePublicKey(ok)
		if err != nil || got != ok {
			t.Errorf("ParsePublicKey(%q) = %q, %v", ok, got, err)
		}
	}
	for _, bad := range []string{
		"", "short", "EPjFWdd5AufqSSqeM2qN1xzybapC8G4w", // truncated
		"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1vEPjFWdd", // too long
		"0PjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",        // '0' is not base58
	} {
		if _, err := ParsePublicKey(bad); !errors.Is(err, ErrInvalidPublicKey) {
			t.Errorf("ParsePublicKey(%q) err = %v, want ErrInvalidPublicKey", bad, err)
		}
	}
}
