// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package objectstore

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSigner_RoundTrip(t *testing.T) {
	signer, err := NewSigner([]byte("test-signing-key"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	want := SignedDownload{
		AccountID:   uuid.New(),
		ProjectID:   uuid.New(),
		Bucket:      "photos",
		Key:         "vacation/day1/a.jpg",
		ExpiresAt:   time.Now().Add(10 * time.Minute).Truncate(time.Second),
		Disposition: DispositionInline,
	}
	token := signer.Mint(want)

	got, err := signer.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.AccountID != want.AccountID || got.ProjectID != want.ProjectID ||
		got.Bucket != want.Bucket || got.Key != want.Key || !got.ExpiresAt.Equal(want.ExpiresAt) ||
		got.Disposition != want.Disposition {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", got, want)
	}
}

// TestSigner_KeyContainingDelimiter proves an object key containing the
// internal field separator ("|") still round-trips correctly — Key is
// deliberately encoded last and taken as the unsplit remainder for
// exactly this reason (see encodePayload's own comment).
func TestSigner_KeyContainingDelimiter(t *testing.T) {
	signer, _ := NewSigner([]byte("k"))
	want := SignedDownload{
		AccountID: uuid.New(), ProjectID: uuid.New(), Bucket: "photos",
		Key:         "weird|key|with|pipes.jpg",
		ExpiresAt:   time.Now().Add(time.Minute).Truncate(time.Second),
		Disposition: DispositionAttachment,
	}
	token := signer.Mint(want)
	got, err := signer.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Key != want.Key {
		t.Fatalf("key with delimiters corrupted: got %q, want %q", got.Key, want.Key)
	}
}

// TestSigner_DispositionRoundTrips proves the encoded disposition is what
// the redemption handler later uses to choose Content-Disposition
// (inline for Preview, attachment for Download) — see the token's own
// doc comment on why this travels IN the signed payload rather than as
// an editable query parameter.
func TestSigner_DispositionRoundTrips(t *testing.T) {
	signer, _ := NewSigner([]byte("k"))
	for _, disposition := range []string{DispositionInline, DispositionAttachment} {
		token := signer.Mint(SignedDownload{
			AccountID: uuid.New(), ProjectID: uuid.New(), Bucket: "photos", Key: "a.jpg",
			ExpiresAt: time.Now().Add(time.Minute), Disposition: disposition,
		})
		got, err := signer.Verify(token)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if got.Disposition != disposition {
			t.Fatalf("Disposition = %q, want %q", got.Disposition, disposition)
		}
	}
}

func TestSigner_TamperedTokenRejected(t *testing.T) {
	signer, _ := NewSigner([]byte("k"))
	token := signer.Mint(SignedDownload{
		AccountID: uuid.New(), ProjectID: uuid.New(), Bucket: "photos", Key: "a.jpg",
		ExpiresAt: time.Now().Add(time.Minute), Disposition: DispositionInline,
	})

	// Flip the last character of the signature half.
	tampered := token[:len(token)-1] + "0"
	if tampered == token {
		tampered = token[:len(token)-1] + "1"
	}
	if _, err := signer.Verify(tampered); err != ErrInvalidSignedURL {
		t.Fatalf("expected ErrInvalidSignedURL for a tampered token, got %v", err)
	}
}

// TestSigner_WrongKeyRejected proves a token minted by one signing key
// (e.g. before a key rotation) is rejected by another — rotating
// TEEPIN_OBJECTSTORE_URL_SIGNING_KEY is the documented way to invalidate
// every outstanding link at once.
func TestSigner_WrongKeyRejected(t *testing.T) {
	signerA, _ := NewSigner([]byte("key-a"))
	signerB, _ := NewSigner([]byte("key-b"))

	token := signerA.Mint(SignedDownload{
		AccountID: uuid.New(), ProjectID: uuid.New(), Bucket: "photos", Key: "a.jpg",
		ExpiresAt: time.Now().Add(time.Minute), Disposition: DispositionInline,
	})
	if _, err := signerB.Verify(token); err != ErrInvalidSignedURL {
		t.Fatalf("expected ErrInvalidSignedURL across a key rotation, got %v", err)
	}
}

func TestSigner_ExpiredTokenRejected(t *testing.T) {
	signer, _ := NewSigner([]byte("k"))
	token := signer.Mint(SignedDownload{
		AccountID: uuid.New(), ProjectID: uuid.New(), Bucket: "photos", Key: "a.jpg",
		ExpiresAt:   time.Now().Add(-time.Minute), // already expired
		Disposition: DispositionInline,
	})
	if _, err := signer.Verify(token); err != ErrInvalidSignedURL {
		t.Fatalf("expected ErrInvalidSignedURL for an expired token, got %v", err)
	}
}

func TestSigner_MalformedTokenRejected(t *testing.T) {
	signer, _ := NewSigner([]byte("k"))
	for _, bad := range []string{"", "no-dot-in-here", "onlyonepart.", ".onlymac", "!!!.###"} {
		if _, err := signer.Verify(bad); err != ErrInvalidSignedURL {
			t.Errorf("Verify(%q): expected ErrInvalidSignedURL, got %v", bad, err)
		}
	}
}

func TestNewSigner_RejectsEmptyKey(t *testing.T) {
	if _, err := NewSigner(nil); err == nil {
		t.Fatal("expected an error for an empty signing key")
	}
	if _, err := NewSigner([]byte{}); err == nil {
		t.Fatal("expected an error for an empty signing key")
	}
}

// TestSigner_TokenIsURLSafe guards against a future change to the
// encoding accidentally introducing characters ("/", "+") that would need
// escaping in the :token path segment.
func TestSigner_TokenIsURLSafe(t *testing.T) {
	signer, _ := NewSigner([]byte("k"))
	token := signer.Mint(SignedDownload{
		AccountID: uuid.New(), ProjectID: uuid.New(), Bucket: "photos", Key: "a.jpg",
		ExpiresAt: time.Now().Add(time.Minute), Disposition: DispositionInline,
	})
	if strings.ContainsAny(token, "/+") {
		t.Fatalf("token contains characters unsafe in a URL path segment: %q", token)
	}
}
