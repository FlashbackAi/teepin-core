// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/inference"
)

// RouteStore persists per-route enable/disable, live-settable from Control
// Center instead of only at process boot via env vars/Terraform (which
// choose WHICH backends are wired at all — that part stays a redeploy;
// this is the operator's on/off switch over routes that already exist).
type RouteStore struct {
	db *sql.DB
}

// NewRouteStore wires the store.
func NewRouteStore(db *sql.DB) *RouteStore {
	return &RouteStore{db: db}
}

// IsEnabled reports whether route is enabled. A route with no row is
// enabled — the same "ships on, an explicit row is what turns it off"
// default every other toggle in this schema uses (e.g. modelcatalog.Model),
// so a route that has never been touched from Control Center behaves
// exactly as it did before this existed.
func (s *RouteStore) IsEnabled(ctx context.Context, route string) (bool, error) {
	var enabled bool
	err := s.db.QueryRowContext(ctx,
		`SELECT enabled FROM billing.kumbha_routes WHERE route_name = $1`, route,
	).Scan(&enabled)
	if err == sql.ErrNoRows {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("check route %q enabled: %w", route, err)
	}
	return enabled, nil
}

// Enabled bulk-fetches every route's enabled state in one query, for the
// admin listing endpoint — avoids one round trip per configured route.
// A route absent from the returned map is enabled (see IsEnabled).
func (s *RouteStore) Enabled(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT route_name, enabled FROM billing.kumbha_routes`)
	if err != nil {
		return nil, fmt.Errorf("list route settings: %w", err)
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var name string
		var enabled bool
		if err := rows.Scan(&name, &enabled); err != nil {
			return nil, fmt.Errorf("scan route setting: %w", err)
		}
		out[name] = enabled
	}
	return out, rows.Err()
}

// SetEnabled turns a route on or off. route need not already have a row —
// this upserts. Never validates route against the Router's configured set:
// the store has no reference to it, and a stale/typo'd route name here is
// simply inert (nothing ever consults it) rather than a hard failure, so
// Control Center's own listing (which DOES cross-reference the two) is
// what catches a real mismatch, not this write path.
func (s *RouteStore) SetEnabled(ctx context.Context, route string, enabled bool, updatedBy string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO billing.kumbha_routes (route_name, enabled, updated_by, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (route_name) DO UPDATE
		SET enabled = EXCLUDED.enabled, updated_by = EXCLUDED.updated_by, updated_at = now()
	`, route, enabled, nullIfEmpty(updatedBy))
	if err != nil {
		return fmt.Errorf("set route %q enabled=%v: %w", route, enabled, err)
	}
	return nil
}

// HealthStatus is a route's last-observed connectivity state.
type HealthStatus string

const (
	// HealthUnknown means no check has completed yet (just started), or
	// the route's Provider does not implement inference.HealthChecker —
	// deliberately NOT "unhealthy": a Provider with no check has given no
	// negative signal, and treating silence as failure would be inventing
	// one (see inference.HealthChecker's own doc comment).
	HealthUnknown   HealthStatus = "unknown"
	HealthHealthy   HealthStatus = "healthy"
	HealthUnhealthy HealthStatus = "unhealthy"
)

// RouteHealth is one route's last health-check result.
type RouteHealth struct {
	Status    HealthStatus
	Error     string
	CheckedAt time.Time
}

// RouteMonitor periodically checks every configured route's backend
// connectivity and caches the result in memory — read by the admin API on
// every request, so a slow/hung backend never makes loading Control Center
// itself slow. Costs no tokens (inference.HealthChecker calls are metadata
// lookups, not completions); routes whose Provider implements no health
// check simply stay HealthUnknown forever, which is the honest answer.
type RouteMonitor struct {
	routes map[string]Route // name -> Route, snapshotted at construction

	// candidates/builder are OPTIONAL and travel together — nil means only
	// the static routes map above is checked (today's behaviour before
	// this existed). Set via WithCandidates once a CandidateStore exists;
	// unlike routes, the candidate list is re-read on every check rather
	// than snapshotted, since it is meant to change live from Control
	// Center.
	candidates CandidateLister
	builder    CandidateBuilder

	mu              sync.RWMutex
	status          map[string]RouteHealth
	candidateStatus map[uuid.UUID]RouteHealth
}

// CandidateLister is what RouteMonitor needs to enumerate every route's
// candidates for health checking — implemented by *CandidateStore. A
// separate small interface from CandidateSource (which only lists one
// route at a time) since a health sweep needs every route at once.
type CandidateLister interface {
	ListAll(ctx context.Context) (map[string][]RouteCandidate, error)
}

// NewRouteMonitor builds a monitor over the given configured routes (the
// same map passed to NewRouter). Every route starts HealthUnknown until the
// first check completes — callers wanting an immediate first reading should
// call CheckNow once at startup rather than waiting for the first tick.
func NewRouteMonitor(routes map[string]Route) *RouteMonitor {
	m := &RouteMonitor{routes: routes, status: make(map[string]RouteHealth, len(routes)), candidateStatus: map[uuid.UUID]RouteHealth{}}
	for name := range routes {
		m.status[name] = RouteHealth{Status: HealthUnknown}
	}
	return m
}

// WithCandidates enables per-candidate health checking alongside the
// static routes map — every enabled candidate, across every route, gets
// its own cached health reading keyed by candidate id (CandidateStatuses).
// Returns the same *RouteMonitor for chaining.
func (m *RouteMonitor) WithCandidates(candidates CandidateLister, builder CandidateBuilder) *RouteMonitor {
	m.candidates = candidates
	m.builder = builder
	return m
}

// Statuses returns a snapshot of every static route's last health result.
func (m *RouteMonitor) Statuses() map[string]RouteHealth {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]RouteHealth, len(m.status))
	for k, v := range m.status {
		out[k] = v
	}
	return out
}

// CandidateStatuses returns a snapshot of every candidate's last health
// result, keyed by candidate id.
func (m *RouteMonitor) CandidateStatuses() map[uuid.UUID]RouteHealth {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[uuid.UUID]RouteHealth, len(m.candidateStatus))
	for k, v := range m.candidateStatus {
		out[k] = v
	}
	return out
}

// CheckNow runs one round of health checks immediately, sequentially (the
// route/candidate count is small, so there is no value in the added
// complexity of running them concurrently). Each check gets its own
// bounded timeout so one hung backend cannot delay the others.
func (m *RouteMonitor) CheckNow(ctx context.Context) {
	for name, route := range m.routes {
		m.checkOne(ctx, name, route)
	}
	if m.candidates != nil {
		m.checkCandidates(ctx)
	}
}

const routeHealthCheckTimeout = 10 * time.Second

func (m *RouteMonitor) checkOne(ctx context.Context, name string, route Route) {
	checker, ok := route.Provider.(inference.HealthChecker)
	if !ok {
		m.set(name, RouteHealth{Status: HealthUnknown, CheckedAt: time.Now()})
		return
	}
	checkCtx, cancel := context.WithTimeout(ctx, routeHealthCheckTimeout)
	defer cancel()

	if err := checker.CheckHealth(checkCtx); err != nil {
		m.set(name, RouteHealth{Status: HealthUnhealthy, Error: err.Error(), CheckedAt: time.Now()})
		return
	}
	m.set(name, RouteHealth{Status: HealthHealthy, CheckedAt: time.Now()})
}

// checkCandidates re-lists every route's candidates fresh on each call
// (rather than a snapshot taken once) since Control Center can add, edit,
// or delete one at any time — a stale candidate list here would mean a
// newly-added or newly-disabled candidate's health never updates. A list
// failure leaves the cached statuses untouched rather than wiping them, the
// same posture checkOne implicitly has (a hung backend just keeps its last
// reading until the next successful tick).
func (m *RouteMonitor) checkCandidates(ctx context.Context) {
	all, err := m.candidates.ListAll(ctx)
	if err != nil {
		return
	}
	for _, rows := range all {
		for _, c := range rows {
			if !c.Enabled {
				m.setCandidate(c.ID, RouteHealth{Status: HealthUnknown, CheckedAt: time.Now()})
				continue
			}
			m.checkOneCandidate(ctx, c)
		}
	}
}

func (m *RouteMonitor) checkOneCandidate(ctx context.Context, c RouteCandidate) {
	provider, err := m.builder.Build(ctx, c)
	if err != nil {
		m.setCandidate(c.ID, RouteHealth{Status: HealthUnhealthy, Error: err.Error(), CheckedAt: time.Now()})
		return
	}
	checker, ok := provider.(inference.HealthChecker)
	if !ok {
		m.setCandidate(c.ID, RouteHealth{Status: HealthUnknown, CheckedAt: time.Now()})
		return
	}
	checkCtx, cancel := context.WithTimeout(ctx, routeHealthCheckTimeout)
	defer cancel()
	if err := checker.CheckHealth(checkCtx); err != nil {
		m.setCandidate(c.ID, RouteHealth{Status: HealthUnhealthy, Error: err.Error(), CheckedAt: time.Now()})
		return
	}
	m.setCandidate(c.ID, RouteHealth{Status: HealthHealthy, CheckedAt: time.Now()})
}

func (m *RouteMonitor) set(name string, h RouteHealth) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status[name] = h
}

func (m *RouteMonitor) setCandidate(id uuid.UUID, h RouteHealth) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.candidateStatus[id] = h
}

// Start runs health checks on a fixed interval until ctx is cancelled —
// callers run this via `go monitor.Start(ctx, interval)`, mirroring
// billing.UsageCollector's own ticker-loop shape.
func (m *RouteMonitor) Start(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.CheckNow(ctx)
		case <-ctx.Done():
			return
		}
	}
}
