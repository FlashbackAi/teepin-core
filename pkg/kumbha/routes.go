// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

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

	mu     sync.RWMutex
	status map[string]RouteHealth
}

// NewRouteMonitor builds a monitor over the given configured routes (the
// same map passed to NewRouter). Every route starts HealthUnknown until the
// first check completes — callers wanting an immediate first reading should
// call CheckNow once at startup rather than waiting for the first tick.
func NewRouteMonitor(routes map[string]Route) *RouteMonitor {
	m := &RouteMonitor{routes: routes, status: make(map[string]RouteHealth, len(routes))}
	for name := range routes {
		m.status[name] = RouteHealth{Status: HealthUnknown}
	}
	return m
}

// Statuses returns a snapshot of every route's last health result.
func (m *RouteMonitor) Statuses() map[string]RouteHealth {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]RouteHealth, len(m.status))
	for k, v := range m.status {
		out[k] = v
	}
	return out
}

// CheckNow runs one round of health checks immediately, sequentially (the
// route count is small — two or three — so there is no value in the added
// complexity of running them concurrently). Each route gets its own bounded
// timeout so one hung backend cannot delay the others.
func (m *RouteMonitor) CheckNow(ctx context.Context) {
	for name, route := range m.routes {
		m.checkOne(ctx, name, route)
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

func (m *RouteMonitor) set(name string, h RouteHealth) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status[name] = h
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
