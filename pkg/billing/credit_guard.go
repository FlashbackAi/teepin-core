// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ErrInsufficientCredit means an account cannot pay for what it asked for.
// Callers map it to 402 Payment Required.
var ErrInsufficientCredit = errors.New("insufficient credit")

const (
	// balanceCacheTTL bounds how stale a pre-flight balance may be. Every
	// metered request checks the balance first, and the balance function
	// sums the account's whole ledger, so checking straight from the
	// database on each call would not scale. Five seconds keeps the check
	// cheap while a settled spend or top-up still shows up almost
	// immediately (both adjust or drop the cached entry directly; the TTL
	// only matters across control-plane replicas).
	balanceCacheTTL = 5 * time.Second

	// MinLaunchRunway is how long an instance must be affordable for at
	// launch. Launching something the account could pay for only briefly
	// would be stopped by the credit enforcer within minutes; refusing up
	// front is kinder than starting it just to kill it.
	MinLaunchRunway = 15 * time.Minute
)

type cachedBalance struct {
	value float64
	at    time.Time
}

// balanceCache holds recent balances for pre-flight checks. Zero value is
// ready to use.
type balanceCache struct {
	mu      sync.Mutex
	entries map[uuid.UUID]cachedBalance
	now     func() time.Time
}

func (c *balanceCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *balanceCache) get(id uuid.UUID) (float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[id]
	if !ok || c.clock().Sub(e.at) > balanceCacheTTL {
		return 0, false
	}
	return e.value, true
}

func (c *balanceCache) put(id uuid.UUID, v float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[uuid.UUID]cachedBalance)
	}
	c.entries[id] = cachedBalance{value: v, at: c.clock()}
}

// adjust moves a cached balance by delta so a settled spend is visible to
// the very next pre-flight without a database read. A missing entry stays
// missing (the next check reads the truth).
func (c *balanceCache) adjust(id uuid.UUID, delta float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[id]; ok {
		e.value += delta
		c.entries[id] = e
	}
}

func (c *balanceCache) forget(id uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, id)
}

// CreditAvailable returns the account's spendable credit for a pre-flight
// check. Slightly cached (see balanceCacheTTL); use CreditBalance for the
// exact figure.
func (s *Service) CreditAvailable(ctx context.Context, accountID uuid.UUID) (float64, error) {
	if v, ok := s.balances.get(accountID); ok {
		return v, nil
	}
	v, err := s.CreditBalance(ctx, accountID)
	if err != nil {
		return 0, err
	}
	s.balances.put(accountID, v)
	return v, nil
}

// CanAfford reports whether the account can pay worstCase, returning the
// balance it judged on. A non-positive worstCase (an unpriced request) is
// always affordable and reads nothing. Anything else requires a positive
// balance that covers it, so an account at $0 is refused even for a tiny
// request. A lookup failure returns the error: callers fail closed.
func (s *Service) CanAfford(ctx context.Context, accountID uuid.UUID, worstCase float64) (bool, float64, error) {
	if worstCase <= 0 || math.IsNaN(worstCase) {
		return true, 0, nil
	}
	balance, err := s.CreditAvailable(ctx, accountID)
	if err != nil {
		return false, 0, err
	}
	return balance > 0 && balance >= worstCase, balance, nil
}

// HasRunway reports whether the account can pay for hourlyCost of usage for
// MinLaunchRunway. Used before launching something that keeps costing.
func (s *Service) HasRunway(ctx context.Context, accountID uuid.UUID, hourlyCost float64) (bool, float64, error) {
	return s.CanAfford(ctx, accountID, hourlyCost*MinLaunchRunway.Hours())
}
