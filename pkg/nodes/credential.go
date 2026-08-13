// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package nodes

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// Token/credential formats:
//
//	enrollment token:  tne_<43 base64url chars>   (tne = teepin node enroll)
//	node credential:   tnc_<43 base64url chars>   (tnc = teepin node cred)
//
// Both are high-entropy (32 random bytes). They are hashed with SHA-256 for
// storage — NOT bcrypt: the credential is verified on every agent
// connection and the input is already random, so a slow password KDF adds
// cost without adding security. The prefix is stored alongside the hash so a
// lookup is one indexed read, never a table scan.
const (
	enrollTokenPrefix = "tne_"
	credentialPrefix  = "tnc_"
	// prefixLen is how many leading characters we index on. Long enough to
	// be effectively unique, short enough to be a cheap index key.
	prefixLen = 12
)

// generateSecret returns a new random secret with the given 4-char prefix,
// its SHA-256 hash (hex), and the indexed lookup prefix.
func generateSecret(kind string) (secret, hash, prefix string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", "", fmt.Errorf("generate random secret: %w", err)
	}
	encoded := strings.TrimRight(base64.URLEncoding.EncodeToString(b), "=")
	secret = kind + encoded
	hash = hashSecret(secret)
	prefix = secret[:prefixLen]
	return secret, hash, prefix, nil
}

// hashSecret is the one place the storage hash is computed, so mint and
// verify can never disagree on the algorithm.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// secretPrefix returns the indexed lookup prefix for a presented secret, or
// "" if it is too short to be one of ours (rejected before any DB call).
func secretPrefix(secret string) string {
	if len(secret) < prefixLen {
		return ""
	}
	return secret[:prefixLen]
}

// secretMatches compares a presented secret against a stored hash in
// constant time, so a mismatch leaks no timing information about how much of
// the hash matched.
func secretMatches(secret, storedHash string) bool {
	got := hashSecret(secret)
	return subtle.ConstantTimeCompare([]byte(got), []byte(storedHash)) == 1
}
