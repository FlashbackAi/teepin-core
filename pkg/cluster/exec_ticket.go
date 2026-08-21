// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ExecTicket is a single-use, short-lived credential minted by the REST
// API (POST .../exec, which knows the caller's identity via the normal
// JWT/API-key auth) and redeemed by the WebSocket attach endpoint (which
// cannot: browsers cannot set custom headers on a WebSocket handshake,
// so a long-lived credential can never reach that endpoint directly).
// Bound to one instance and one resolved identity, valid for ~30s, and
// usable exactly once — see TicketStore.Redeem.
type ExecTicket struct {
	ID         string
	InstanceID string
	ProjectID  uuid.UUID
	AccountID  uuid.UUID
	UserID     uuid.UUID
	Container  string
	Command    []string
	TTY        bool
	ExpiresAt  time.Time
}

var (
	// ErrTicketNotFound covers "never existed", "expired", and "already
	// redeemed" identically on purpose — a ticket is presented over an
	// unauthenticated WebSocket handshake, so distinguishing these cases
	// in the response would tell an attacker more than they should learn.
	ErrTicketNotFound = errors.New("exec ticket not found, expired, or already used")
	// ErrTicketStoreFull bounds memory: a scripted loop hitting the issue
	// endpoint must not be able to grow the task's heap without limit.
	ErrTicketStoreFull = errors.New("too many outstanding exec tickets")
)

const (
	execTicketTTL          = 30 * time.Second
	execTicketMax          = 10_000
	execTicketReapInterval = 30 * time.Second
)

// TicketStore issues and redeems ExecTickets. In-memory — under the SAME
// desired_count=1 constraint the agent Registry already lives with (see
// its own doc comment): one ECS task holds all live session state today,
// and this is another piece of that state, not a new constraint.
type TicketStore struct {
	mu      sync.Mutex
	tickets map[string]ExecTicket // keyed by secret, never by id
}

func NewTicketStore() *TicketStore {
	return &TicketStore{tickets: make(map[string]ExecTicket)}
}

// Issue mints a ticket and returns its public id (safe to log/audit —
// see the exec_issue log line in pkg/api) and its secret. The secret is
// the actual credential: sent to the browser/CLI once in the POST
// response, presented back on the WS attach as the first frame, and
// never logged anywhere.
func (s *TicketStore) Issue(t ExecTicket) (id, secret string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.tickets) >= execTicketMax {
		return "", "", ErrTicketStoreFull
	}

	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return "", "", err
	}
	secret = base64.RawURLEncoding.EncodeToString(secretBytes)

	t.ID = uuid.New().String()
	t.ExpiresAt = time.Now().Add(execTicketTTL)
	s.tickets[secret] = t

	return t.ID, secret, nil
}

// Redeem consumes a ticket exactly once. The lookup and delete happen
// under the same lock, so N concurrent redeem attempts against the same
// secret can never yield more than one success — this atomicity is the
// entire single-use guarantee, and it is the property the tests for this
// file exist to pin down.
func (s *TicketStore) Redeem(id, secret string) (ExecTicket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tickets[secret]
	if !ok {
		return ExecTicket{}, ErrTicketNotFound
	}
	delete(s.tickets, secret) // consumed regardless of what the checks below find

	if t.ID != id || time.Now().After(t.ExpiresAt) {
		return ExecTicket{}, ErrTicketNotFound
	}
	return t, nil
}

// Reap sweeps expired, never-redeemed tickets. Hygiene against slow
// accumulation from abandoned attach flows — not the expiry guarantee
// itself, which Redeem already enforces independently on every call.
func (s *TicketStore) Reap(ctx context.Context) {
	ticker := time.NewTicker(execTicketReapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			now := time.Now()
			for secret, t := range s.tickets {
				if now.After(t.ExpiresAt) {
					delete(s.tickets, secret)
				}
			}
			s.mu.Unlock()
		}
	}
}
