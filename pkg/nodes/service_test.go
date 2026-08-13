// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package nodes

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func newMock(t *testing.T) (*Service, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return NewService(db), mock, func() { db.Close() }
}

// A minted token stores only a hash, and the class is whatever the operator
// chose — proving class is fixed server-side at mint time.
func TestCreateEnrollmentToken_StoresHashAndClass(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	mock.ExpectQuery(`INSERT INTO compute\.node_enrollment_tokens`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), "datacenter", "rack-1", "op", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(uuid.New(), time.Now()))

	plaintext, tok, err := s.CreateEnrollmentToken(context.Background(), "rack-1", "datacenter", "op", time.Hour)
	if err != nil {
		t.Fatalf("CreateEnrollmentToken: %v", err)
	}
	if tok.Class != "datacenter" {
		t.Errorf("class = %q, want datacenter", tok.Class)
	}
	if plaintext[:4] != enrollTokenPrefix {
		t.Errorf("token %q lacks enroll prefix", plaintext)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestCreateEnrollmentToken_Validation(t *testing.T) {
	s := NewService(nil) // rejected before any query
	if _, _, err := s.CreateEnrollmentToken(context.Background(), "  ", "home", "op", time.Hour); err == nil {
		t.Error("blank label accepted")
	}
	if _, _, err := s.CreateEnrollmentToken(context.Background(), "x", "bogus", "op", time.Hour); err == nil {
		t.Error("invalid class accepted")
	}
}

// Enroll takes the class from the TOKEN row, not from the agent — the core
// class-integrity guarantee. Here the token says 'datacenter' and the node
// is created with that class regardless of anything in specs.
func TestEnroll_ClassComesFromToken(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	// We must present a token whose hash matches a stored row. Mint one
	// out-of-band by generating a secret and feeding back its hash.
	secret, hash, prefix, _ := generateSecret(enrollTokenPrefix)
	tokenID := uuid.New()
	nodeID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id, token_hash, class, expires_at, consumed_at\s+FROM compute\.node_enrollment_tokens\s+WHERE token_prefix = \$1\s+FOR UPDATE`).
		WithArgs(prefix).
		WillReturnRows(sqlmock.NewRows([]string{"id", "token_hash", "class", "expires_at", "consumed_at"}).
			AddRow(tokenID, hash, "datacenter", time.Now().Add(time.Hour), nil))
	mock.ExpectQuery(`INSERT INTO compute\.nodes`).
		WithArgs("node-a", "prov-a", "datacenter", sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), 0, false, sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at", "updated_at"}).
			AddRow(nodeID, time.Now(), time.Now()))
	mock.ExpectExec(`UPDATE compute\.node_enrollment_tokens\s+SET consumed_at = NOW\(\), node_id`).
		WithArgs(nodeID, tokenID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	cred, node, err := s.Enroll(context.Background(), secret, NodeSpecs{NodeName: "node-a", ProviderID: "prov-a"})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if node.Class != "datacenter" {
		t.Errorf("node class = %q, want datacenter (from token)", node.Class)
	}
	if cred[:4] != credentialPrefix {
		t.Errorf("credential %q lacks cred prefix", cred)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// A token already consumed is rejected — single-use.
func TestEnroll_ConsumedTokenRejected(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	secret, hash, prefix, _ := generateSecret(enrollTokenPrefix)
	consumed := time.Now().Add(-time.Minute)

	mock.ExpectBegin()
	mock.ExpectQuery(`FROM compute\.node_enrollment_tokens\s+WHERE token_prefix`).
		WithArgs(prefix).
		WillReturnRows(sqlmock.NewRows([]string{"id", "token_hash", "class", "expires_at", "consumed_at"}).
			AddRow(uuid.New(), hash, "home", time.Now().Add(time.Hour), consumed))
	mock.ExpectRollback()

	if _, _, err := s.Enroll(context.Background(), secret, NodeSpecs{NodeName: "n", ProviderID: "p"}); !errors.Is(err, ErrTokenConsumed) {
		t.Fatalf("err = %v, want ErrTokenConsumed", err)
	}
}

// An expired token is rejected even if never consumed.
func TestEnroll_ExpiredTokenRejected(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	secret, hash, prefix, _ := generateSecret(enrollTokenPrefix)

	mock.ExpectBegin()
	mock.ExpectQuery(`FROM compute\.node_enrollment_tokens\s+WHERE token_prefix`).
		WithArgs(prefix).
		WillReturnRows(sqlmock.NewRows([]string{"id", "token_hash", "class", "expires_at", "consumed_at"}).
			AddRow(uuid.New(), hash, "home", time.Now().Add(-time.Minute), nil))
	mock.ExpectRollback()

	if _, _, err := s.Enroll(context.Background(), secret, NodeSpecs{NodeName: "n", ProviderID: "p"}); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("err = %v, want ErrTokenExpired", err)
	}
}

// A token whose plaintext does not hash-match any stored row is rejected —
// no timing/format leak, and no accidental match on prefix collision.
func TestEnroll_WrongSecretRejected(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	// Present one secret, store a DIFFERENT secret's hash under same prefix.
	present, _, prefix, _ := generateSecret(enrollTokenPrefix)
	_, otherHash, _, _ := generateSecret(enrollTokenPrefix)

	mock.ExpectBegin()
	mock.ExpectQuery(`FROM compute\.node_enrollment_tokens\s+WHERE token_prefix`).
		WithArgs(prefix).
		WillReturnRows(sqlmock.NewRows([]string{"id", "token_hash", "class", "expires_at", "consumed_at"}).
			AddRow(uuid.New(), otherHash, "home", time.Now().Add(time.Hour), nil))
	mock.ExpectRollback()

	if _, _, err := s.Enroll(context.Background(), present, NodeSpecs{NodeName: "n", ProviderID: "p"}); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want ErrTokenInvalid", err)
	}
}

// A malformed credential is rejected before any DB call.
func TestAuthenticateNode_MalformedRejected(t *testing.T) {
	s := NewService(nil)
	if _, err := s.AuthenticateNode(context.Background(), "not-a-cred"); !errors.Is(err, ErrNodeInvalid) {
		t.Fatalf("err = %v, want ErrNodeInvalid", err)
	}
}

// A valid credential resolves to its node.
func TestAuthenticateNode_Valid(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	cred, hash, prefix, _ := generateSecret(credentialPrefix)
	id := uuid.New()

	mock.ExpectQuery(`FROM compute\.nodes\s+WHERE credential_prefix`).
		WithArgs(prefix).
		WillReturnRows(nodeAuthRow(id, hash, "online", nil))

	node, err := s.AuthenticateNode(context.Background(), cred)
	if err != nil {
		t.Fatalf("AuthenticateNode: %v", err)
	}
	if node.ID != id {
		t.Errorf("resolved wrong node")
	}
}

// A revoked credential is rejected even though the hash matches.
func TestAuthenticateNode_RevokedRejected(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	cred, hash, prefix, _ := generateSecret(credentialPrefix)
	revoked := time.Now()

	mock.ExpectQuery(`FROM compute\.nodes\s+WHERE credential_prefix`).
		WithArgs(prefix).
		WillReturnRows(nodeAuthRow(uuid.New(), hash, "online", &revoked))

	if _, err := s.AuthenticateNode(context.Background(), cred); !errors.Is(err, ErrNodeRevoked) {
		t.Fatalf("err = %v, want ErrNodeRevoked", err)
	}
}

// A disabled node is rejected.
func TestAuthenticateNode_DisabledRejected(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	cred, hash, prefix, _ := generateSecret(credentialPrefix)

	mock.ExpectQuery(`FROM compute\.nodes\s+WHERE credential_prefix`).
		WithArgs(prefix).
		WillReturnRows(nodeAuthRow(uuid.New(), hash, "disabled", nil))

	if _, err := s.AuthenticateNode(context.Background(), cred); !errors.Is(err, ErrNodeDisabled) {
		t.Fatalf("err = %v, want ErrNodeDisabled", err)
	}
}

// UpsertSeen issues a single INSERT ... ON CONFLICT — the write-through that
// gives every connected node a durable row without disturbing an enrolled
// node's class or credential.
func TestUpsertSeen(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	mock.ExpectExec(`(?s)INSERT INTO compute\.nodes.*ON CONFLICT \(node_name\) DO UPDATE`).
		WithArgs("gpu-node-1", "dc-provider", "datacenter", sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), 8, true,
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := s.UpsertSeen(context.Background(), "datacenter", NodeSpecs{
		NodeName: "gpu-node-1", ProviderID: "dc-provider", GPUCount: 8, MIGCapable: true,
	})
	if err != nil {
		t.Fatalf("UpsertSeen: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestMarkStaleOffline(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	mock.ExpectExec(`UPDATE compute\.nodes\s+SET status = 'offline'`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 2))

	n, err := s.MarkStaleOffline(context.Background(), 90*time.Second)
	if err != nil {
		t.Fatalf("MarkStaleOffline: %v", err)
	}
	if n != 2 {
		t.Errorf("transitioned %d, want 2", n)
	}
}

// nodeAuthRow builds the row shape AuthenticateNode scans.
func nodeAuthRow(id uuid.UUID, hash, status string, revoked *time.Time) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "node_name", "provider_id", "class", "region",
		"cpu_cores", "memory_gb", "gpu_model", "gpu_count", "mig_capable",
		"os", "arch", "agent_version", "status", "credential_hash", "revoked_at",
	}).AddRow(id, "node-x", "prov-x", "home", "", 4, 8, "", 0, false,
		"linux", "amd64", "0.1.0", status, hash, revoked)
}
