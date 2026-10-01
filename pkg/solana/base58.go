// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Package solana is the small slice of Solana that Teepin needs to accept USDC
// payments: Solana Pay transfer-request URLs, a JSON-RPC client for finding and
// reading transactions, and the checks that decide whether a transaction really
// paid us. It holds no keys and signs nothing: Teepin only ever receives.
package solana

import (
	"errors"
	"fmt"
	"math/big"
)

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// PublicKeyLen is the length in bytes of a Solana public key, and of a Solana
// Pay reference.
const PublicKeyLen = 32

var (
	// ErrInvalidBase58 means a string contains a character outside the alphabet.
	ErrInvalidBase58 = errors.New("invalid base58 string")
	// ErrInvalidPublicKey means a string is not a base58-encoded 32-byte key.
	ErrInvalidPublicKey = errors.New("not a valid Solana public key")
)

var (
	big58 = big.NewInt(58)
	// base58Index maps a character to its value, or -1.
	base58Index = func() [256]int {
		var idx [256]int
		for i := range idx {
			idx[i] = -1
		}
		for i := 0; i < len(base58Alphabet); i++ {
			idx[base58Alphabet[i]] = i
		}
		return idx
	}()
)

// EncodeBase58 encodes bytes in the Bitcoin base58 alphabet, which Solana uses
// for public keys and signatures. Leading zero bytes become leading '1's.
func EncodeBase58(b []byte) string {
	zeros := 0
	for zeros < len(b) && b[zeros] == 0 {
		zeros++
	}
	n := new(big.Int).SetBytes(b)
	mod := new(big.Int)
	var out []byte
	for n.Sign() > 0 {
		n.DivMod(n, big58, mod)
		out = append(out, base58Alphabet[mod.Int64()])
	}
	for i := 0; i < zeros; i++ {
		out = append(out, base58Alphabet[0])
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

// DecodeBase58 is the inverse of EncodeBase58.
func DecodeBase58(s string) ([]byte, error) {
	zeros := 0
	for zeros < len(s) && s[zeros] == base58Alphabet[0] {
		zeros++
	}
	n := new(big.Int)
	for i := zeros; i < len(s); i++ {
		v := base58Index[s[i]]
		if v < 0 {
			return nil, fmt.Errorf("%w: character %q at position %d", ErrInvalidBase58, s[i], i)
		}
		n.Mul(n, big58)
		n.Add(n, big.NewInt(int64(v)))
	}
	body := n.Bytes()
	out := make([]byte, zeros+len(body))
	copy(out[zeros:], body)
	return out, nil
}

// ParsePublicKey checks that s is a base58-encoded 32-byte key and returns it
// in canonical form. Used for every address that arrives from configuration or
// from an RPC response, so a typo or a truncated paste is caught at startup
// instead of on the first payment.
func ParsePublicKey(s string) (string, error) {
	b, err := DecodeBase58(s)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidPublicKey, err)
	}
	if len(b) != PublicKeyLen {
		return "", fmt.Errorf("%w: decodes to %d bytes, want %d", ErrInvalidPublicKey, len(b), PublicKeyLen)
	}
	return EncodeBase58(b), nil
}
