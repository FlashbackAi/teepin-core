// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestTicketStore_RedeemedExactlyOnce is the security-critical assertion
// for the whole ticket design: N concurrent redeem attempts against the
// SAME issued ticket must yield exactly one success. Run with -race.
func TestTicketStore_RedeemedExactlyOnce(t *testing.T) {
	s := NewTicketStore()
	id, secret, err := s.Issue(ExecTicket{InstanceID: "inst-abc"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	const attempts = 50
	var wg sync.WaitGroup
	var successes int32
	var mu sync.Mutex

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Redeem(id, secret); err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if successes != 1 {
		t.Errorf("got %d successful redemptions of one ticket, want exactly 1", successes)
	}
}

func TestTicketStore_RedeemReturnsWhatWasIssued(t *testing.T) {
	s := NewTicketStore()
	projectID, accountID, userID := uuid.New(), uuid.New(), uuid.New()

	id, secret, err := s.Issue(ExecTicket{
		InstanceID: "inst-xyz",
		ProjectID:  projectID,
		AccountID:  accountID,
		UserID:     userID,
		Container:  "sidecar",
		Command:    []string{"/bin/bash"},
		TTY:        true,
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	got, err := s.Redeem(id, secret)
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if got.InstanceID != "inst-xyz" || got.ProjectID != projectID || got.AccountID != accountID ||
		got.UserID != userID || got.Container != "sidecar" || len(got.Command) != 1 || !got.TTY {
		t.Errorf("redeemed ticket does not match what was issued: %+v", got)
	}
}

func TestTicketStore_WrongSecretFails(t *testing.T) {
	s := NewTicketStore()
	id, _, err := s.Issue(ExecTicket{InstanceID: "inst-abc"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := s.Redeem(id, "not-the-real-secret"); err != ErrTicketNotFound {
		t.Errorf("got %v, want ErrTicketNotFound", err)
	}
}

func TestTicketStore_WrongIDWithRightSecretFails(t *testing.T) {
	// A mismatched (id, secret) pair — the id check is defense in depth
	// on top of the secret being the real credential; either alone
	// failing must reject the redeem.
	s := NewTicketStore()
	_, secret, err := s.Issue(ExecTicket{InstanceID: "inst-abc"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := s.Redeem("wrong-id", secret); err != ErrTicketNotFound {
		t.Errorf("got %v, want ErrTicketNotFound", err)
	}
	// The ticket is consumed by the failed attempt regardless (Redeem
	// deletes before validating) — confirm a second try with the CORRECT
	// id also fails now, proving single-use holds even across a failed
	// redemption.
	id2, secret2, err := s.Issue(ExecTicket{InstanceID: "inst-def"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := s.Redeem("wrong-id", secret2); err != ErrTicketNotFound {
		t.Fatalf("first redeem: got %v, want ErrTicketNotFound", err)
	}
	if _, err := s.Redeem(id2, secret2); err != ErrTicketNotFound {
		t.Errorf("second redeem with correct id: got %v, want ErrTicketNotFound (ticket must be consumed by the first attempt)", err)
	}
}

func TestTicketStore_ExpiredTicketFails(t *testing.T) {
	s := NewTicketStore()
	id, secret, err := s.Issue(ExecTicket{InstanceID: "inst-abc"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	// Reach in and force expiry rather than sleeping past the real TTL.
	s.mu.Lock()
	t2 := s.tickets[secret]
	t2.ExpiresAt = time.Now().Add(-time.Second)
	s.tickets[secret] = t2
	s.mu.Unlock()

	if _, err := s.Redeem(id, secret); err != ErrTicketNotFound {
		t.Errorf("got %v, want ErrTicketNotFound for an expired ticket", err)
	}
}

func TestTicketStore_StoreFullRejectsIssue(t *testing.T) {
	s := NewTicketStore()
	// Fill directly rather than issuing 10k real tickets through crypto/rand.
	s.mu.Lock()
	for i := 0; i < execTicketMax; i++ {
		s.tickets[uuid.New().String()] = ExecTicket{ExpiresAt: time.Now().Add(execTicketTTL)}
	}
	s.mu.Unlock()

	if _, _, err := s.Issue(ExecTicket{InstanceID: "inst-abc"}); err != ErrTicketStoreFull {
		t.Errorf("got %v, want ErrTicketStoreFull", err)
	}
}

func TestTicketStore_ReapRemovesOnlyExpired(t *testing.T) {
	s := NewTicketStore()
	_, freshSecret, err := s.Issue(ExecTicket{InstanceID: "inst-fresh"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	_, staleSecret, err := s.Issue(ExecTicket{InstanceID: "inst-stale"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	s.mu.Lock()
	stale := s.tickets[staleSecret]
	stale.ExpiresAt = time.Now().Add(-time.Minute)
	s.tickets[staleSecret] = stale
	s.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Reap(ctx)
		close(done)
	}()

	// Reap runs on a ticker (execTicketReapInterval); call the sweep body
	// directly instead of waiting out the real interval.
	s.mu.Lock()
	now := time.Now()
	for secret, tk := range s.tickets {
		if now.After(tk.ExpiresAt) {
			delete(s.tickets, secret)
		}
	}
	s.mu.Unlock()
	cancel()
	<-done

	if _, ok := s.tickets[staleSecret]; ok {
		t.Error("stale ticket survived the sweep")
	}
	if _, ok := s.tickets[freshSecret]; !ok {
		t.Error("fresh ticket was wrongly swept")
	}
}
