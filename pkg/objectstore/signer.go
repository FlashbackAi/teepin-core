// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package objectstore

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrInvalidSignedURL covers every way a redeemed token can fail: bad
// encoding, bad signature, or expired. Deliberately one error for all
// three — telling an attacker WHICH check failed would only help them
// forge a better token.
var ErrInvalidSignedURL = errors.New("objectstore: invalid or expired download link")

const (
	DefaultSignedURLTTL = 15 * time.Minute
	// MaxSignedURLTTL is short because an HMAC token cannot be revoked —
	// unlike a DB-backed ticket, there is no row to delete if a link
	// leaks. A caller wanting revocation should rotate
	// TEEPIN_OBJECTSTORE_URL_SIGNING_KEY, which invalidates every
	// outstanding token at once.
	MaxSignedURLTTL = 24 * time.Hour
)

// Signer mints and verifies short-lived, stateless download tokens —
// Teepin's own stand-in for a presigned URL, since Shelby rejects
// presigned URLs outright (confirmed: "Anonymous requests are not
// allowed ... AWS Signature Version 4 required", no query-string signing
// support at all). Stateless: no DB row, no reaper, works identically
// across any number of API replicas.
type Signer struct {
	key []byte
}

// NewSigner builds a Signer. An empty key is refused — a zero-length HMAC
// key would make every token trivially forgeable.
func NewSigner(key []byte) (*Signer, error) {
	if len(key) == 0 {
		return nil, errors.New("objectstore: a non-empty signing key is required")
	}
	return &Signer{key: key}, nil
}

// SignedDownload is what a mint call embeds in, and a redemption call
// recovers from, a token. Bucket/Key travel in the token itself — not
// looked up server-side by an opaque id — so redemption needs no state
// beyond the token and a fresh read of the current catalog.
type SignedDownload struct {
	AccountID uuid.UUID
	ProjectID uuid.UUID
	Bucket    string
	Key       string
	ExpiresAt time.Time
}

// Mint produces an opaque, URL-safe token. Callers should clamp TTL
// between a sane minimum and MaxSignedURLTTL before calling this —
// Signer itself does not enforce a ceiling, since the API handler is
// where a bad customer-supplied TTL should be rejected with a clear
// error, not silently clamped here.
func (s *Signer) Mint(d SignedDownload) string {
	payload := encodePayload(d)
	mac := s.sign(payload)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + hex.EncodeToString(mac)
}

// Verify checks the signature and expiry and returns the embedded
// download target. It does NOT check the object still exists or is still
// owned by that account/project — the caller (the redemption handler)
// must re-resolve against the live catalog, since a token can outlive a
// delete or a bucket rename.
func (s *Signer) Verify(token string) (SignedDownload, error) {
	encodedPayload, macHex, ok := strings.Cut(token, ".")
	if !ok {
		return SignedDownload{}, ErrInvalidSignedURL
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil {
		return SignedDownload{}, ErrInvalidSignedURL
	}
	mac, err := hex.DecodeString(macHex)
	if err != nil {
		return SignedDownload{}, ErrInvalidSignedURL
	}
	payload := string(payloadBytes)
	if !hmac.Equal(mac, s.sign(payload)) {
		return SignedDownload{}, ErrInvalidSignedURL
	}

	d, err := decodePayload(payload)
	if err != nil {
		return SignedDownload{}, ErrInvalidSignedURL
	}
	if time.Now().After(d.ExpiresAt) {
		return SignedDownload{}, ErrInvalidSignedURL
	}
	return d, nil
}

func (s *Signer) sign(payload string) []byte {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(payload))
	return mac.Sum(nil)
}

// encodePayload/decodePayload deliberately put Key LAST and split with a
// bounded count: an object key can legitimately contain "/" and, in
// principle, "|" — Bucket cannot (ValidBucketName's regex allows only
// lowercase alphanumerics, ".", "-"), so ordering the one field that
// might contain the delimiter last, and taking everything remaining
// after 4 splits as the key verbatim, means a "|" inside a key can never
// corrupt the other fields. Tamper-resistance itself comes from the MAC
// above, not from this choice — a mismatched split would simply fail to
// verify, never silently misattribute a field.
func encodePayload(d SignedDownload) string {
	return strings.Join([]string{
		d.AccountID.String(), d.ProjectID.String(), d.Bucket,
		strconv.FormatInt(d.ExpiresAt.Unix(), 10), d.Key,
	}, "|")
}

func decodePayload(payload string) (SignedDownload, error) {
	parts := strings.SplitN(payload, "|", 5)
	if len(parts) != 5 {
		return SignedDownload{}, ErrInvalidSignedURL
	}
	accountID, err := uuid.Parse(parts[0])
	if err != nil {
		return SignedDownload{}, ErrInvalidSignedURL
	}
	projectID, err := uuid.Parse(parts[1])
	if err != nil {
		return SignedDownload{}, ErrInvalidSignedURL
	}
	expiryUnix, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return SignedDownload{}, ErrInvalidSignedURL
	}
	return SignedDownload{
		AccountID: accountID,
		ProjectID: projectID,
		Bucket:    parts[2],
		ExpiresAt: time.Unix(expiryUnix, 0),
		Key:       parts[4],
	}, nil
}
