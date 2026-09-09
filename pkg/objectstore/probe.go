// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package objectstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/google/uuid"
)

// ProbeResult is one health-probe reading — a real put/get/delete cycle
// run against a reserved, non-customer key. It never touches any bucket
// or object a customer owns; this is the ONLY thing this package ever
// writes under systemPrefix.
type ProbeResult struct {
	Backend   string
	Probe     string // "put" | "get" | "delete"
	OK        bool
	LatencyMS int
	Error     string
	CheckedAt time.Time
}

// HealthStatus is the derived, human-facing read of a backend's recent
// probe history — what the storage service tab's monitoring panel
// actually renders.
type HealthStatus struct {
	Backend   string         `json:"backend"`
	Status    string         `json:"status"` // healthy | degraded | down | unknown
	CheckedAt time.Time      `json:"checked_at"`
	LatencyMS map[string]int `json:"latency_ms"` // most recent successful latency per probe op
	LastError string         `json:"last_error,omitempty"`
	Recent    []ProbeResult  `json:"recent"`
}

const (
	HealthStatusHealthy  = "healthy"
	HealthStatusDegraded = "degraded"
	HealthStatusDown     = "down"
	HealthStatusUnknown  = "unknown" // no probe data yet
)

// probeCycleSize is put+get+delete — one full probe cycle.
const probeCycleSize = 3

// DeriveHealthStatus classifies a backend from its recent probe results
// (newest first, as Store.RecentProbeResults returns them):
//
//   - unknown: no probe data exists yet
//   - down: every op in the MOST RECENT cycle failed — Shelby (or
//     whatever backend is active) is failing right now
//   - degraded: some op failed within the last 3 cycles, but the most
//     recent cycle wasn't a total failure — absorbs a single flaky probe
//     without immediately crying wolf, while still surfacing a real
//     intermittent problem rather than hiding it
//   - healthy: every op in the last 3 cycles succeeded
func DeriveHealthStatus(backend string, recent []ProbeResult) HealthStatus {
	status := HealthStatus{Backend: backend, Status: HealthStatusUnknown, LatencyMS: map[string]int{}}
	if len(recent) == 0 {
		return status
	}
	status.CheckedAt = recent[0].CheckedAt

	const displayLimit = 20
	if len(recent) < displayLimit {
		status.Recent = recent
	} else {
		status.Recent = recent[:displayLimit]
	}

	for _, r := range recent {
		if r.OK {
			if _, seen := status.LatencyMS[r.Probe]; !seen {
				status.LatencyMS[r.Probe] = r.LatencyMS
			}
		} else if status.LastError == "" {
			status.LastError = r.Error
		}
	}

	mostRecentCycle := recent
	if len(mostRecentCycle) > probeCycleSize {
		mostRecentCycle = mostRecentCycle[:probeCycleSize]
	}
	if allFailed(mostRecentCycle) {
		status.Status = HealthStatusDown
		return status
	}

	window := recent
	if len(window) > 3*probeCycleSize {
		window = window[:3*probeCycleSize]
	}
	if anyFailed(window) {
		status.Status = HealthStatusDegraded
		return status
	}

	status.Status = HealthStatusHealthy
	return status
}

func allFailed(rs []ProbeResult) bool {
	if len(rs) == 0 {
		return false
	}
	for _, r := range rs {
		if r.OK {
			return false
		}
	}
	return true
}

func anyFailed(rs []ProbeResult) bool {
	for _, r := range rs {
		if !r.OK {
			return true
		}
	}
	return false
}

// DefaultProbeInterval is the production-safe default cadence — frequent
// enough to catch an outage within a few cycles, infrequent enough not to
// add meaningful load to a small shared backend like Shelby's sandbox.
// main.go's wiring can override this (TEEPIN_OBJECTSTORE_PROBE_INTERVAL_SECONDS)
// for a tighter loop while actively watching a live test.
const DefaultProbeInterval = 5 * time.Minute

// Prober periodically exercises a Backend with a real put/get/delete
// cycle against a reserved key, recording every result — the mechanism
// that turns "is Shelby actually up right now" from a guess into
// something the storage service tab shows directly, since Shelby has no
// SLA and has been confirmed (shelby-eval/) to fail silently under load.
type Prober struct {
	backend  Backend
	store    *Store
	interval time.Duration
}

// NewProber builds a Prober for one backend. interval <= 0 falls back to
// DefaultProbeInterval.
func NewProber(backend Backend, store *Store, interval time.Duration) *Prober {
	if interval <= 0 {
		interval = DefaultProbeInterval
	}
	return &Prober{backend: backend, store: store, interval: interval}
}

// Start runs probe cycles until ctx is cancelled, blocking the calling
// goroutine — callers run this via `go prober.Start(ctx)`. The first
// cycle runs immediately rather than waiting out the first interval, so
// a health reading exists as soon as the service starts rather than
// minutes later.
func (p *Prober) Start(ctx context.Context) {
	p.runCycle(ctx)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			p.runCycle(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (p *Prober) runCycle(ctx context.Context) {
	key := systemPrefix + "health/" + p.backend.Name() + "/" + uuid.New().String()
	payload := make([]byte, 256)
	if _, err := rand.Read(payload); err != nil {
		log.Printf("WARN: objectstore probe: failed to generate payload: %v", err)
		return
	}

	results := make([]ProbeResult, 0, probeCycleSize)

	start := time.Now()
	_, putErr := p.backend.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), PutOptions{ContentType: "application/octet-stream"})
	results = append(results, newProbeResult(p.backend.Name(), "put", start, putErr))

	// A failed Put leaves nothing to Get or Delete — recording just the
	// one failed op is the honest reading; there is no object to chase.
	if putErr == nil {
		start = time.Now()
		body, _, getErr := p.backend.Get(ctx, key, nil)
		if getErr == nil {
			got, readErr := io.ReadAll(body)
			body.Close()
			switch {
			case readErr != nil:
				getErr = fmt.Errorf("read probe body: %w", readErr)
			case !bytes.Equal(got, payload):
				getErr = fmt.Errorf("probe content mismatch: wrote %d bytes, read back %d", len(payload), len(got))
			}
		}
		results = append(results, newProbeResult(p.backend.Name(), "get", start, getErr))

		start = time.Now()
		delErr := p.backend.Delete(ctx, key)
		results = append(results, newProbeResult(p.backend.Name(), "delete", start, delErr))
	}

	for _, r := range results {
		if err := p.store.InsertProbeResult(ctx, r); err != nil {
			log.Printf("WARN: objectstore probe: failed to record %s result for backend %s: %v", r.Probe, p.backend.Name(), err)
		}
	}
}

func newProbeResult(backend, probe string, start time.Time, err error) ProbeResult {
	r := ProbeResult{
		Backend:   backend,
		Probe:     probe,
		OK:        err == nil,
		LatencyMS: int(time.Since(start).Milliseconds()),
		CheckedAt: time.Now(),
	}
	if err != nil {
		r.Error = err.Error()
	}
	return r
}
