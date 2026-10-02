// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package modelprobe

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Store keeps each model's latest report in inference.model_probe_reports.
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// ErrNoReport means the model has never been checked.
var ErrNoReport = errors.New("no capability report for this model")

// Get returns the model's latest report.
func (s *Store) Get(ctx context.Context, route string) (*Report, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT report FROM inference.model_probe_reports WHERE model_route = $1
	`, route).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, ErrNoReport
	}
	if err != nil {
		return nil, fmt.Errorf("failed to load capability report: %w", err)
	}
	var r Report
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("failed to decode capability report: %w", err)
	}
	return &r, nil
}

// All returns every model's latest report, keyed by route.
func (s *Store) All(ctx context.Context) (map[string]*Report, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT model_route, report FROM inference.model_probe_reports`)
	if err != nil {
		return nil, fmt.Errorf("failed to list capability reports: %w", err)
	}
	defer rows.Close()
	out := map[string]*Report{}
	for rows.Next() {
		var route string
		var raw []byte
		if err := rows.Scan(&route, &raw); err != nil {
			return nil, fmt.Errorf("failed to scan capability report: %w", err)
		}
		var r Report
		if err := json.Unmarshal(raw, &r); err != nil {
			continue // one unreadable row must not hide the rest
		}
		out[route] = &r
	}
	return out, rows.Err()
}

// Put saves a report. A check that could not run (StatusError) or was not run
// keeps the model's earlier conclusive result for that capability: an outage
// must not erase evidence, or flip a model from "verified" to "unknown".
func (s *Store) Put(ctx context.Context, r *Report) error {
	prev, err := s.Get(ctx, r.ModelRoute)
	if err != nil && !errors.Is(err, ErrNoReport) {
		return err
	}
	merged := MergeReports(prev, r)
	raw, err := json.Marshal(merged)
	if err != nil {
		return fmt.Errorf("failed to encode capability report: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO inference.model_probe_reports (model_route, ran_at, report)
		VALUES ($1, $2, $3::jsonb)
		ON CONFLICT (model_route) DO UPDATE SET ran_at = EXCLUDED.ran_at, report = EXCLUDED.report
	`, merged.ModelRoute, merged.RanAt, raw); err != nil {
		return fmt.Errorf("failed to save capability report: %w", err)
	}
	return nil
}

// Delete forgets a model's report (a changed backend makes old evidence stale).
func (s *Store) Delete(ctx context.Context, route string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM inference.model_probe_reports WHERE model_route = $1`, route); err != nil {
		return fmt.Errorf("failed to delete capability report: %w", err)
	}
	return nil
}

// MergeReports combines a new report with the previous one: a capability
// that was not concluded this time (error or untested) keeps its earlier
// check, and metadata that was not read keeps the earlier reading.
func MergeReports(prev, next *Report) *Report {
	if prev == nil {
		return next
	}
	out := *next
	out.Checks = nil
	for _, c := range AllCapabilities {
		n := next.Check(c)
		if (n.Status == StatusError || n.Status == StatusUntested) && prev.Check(c).Status != StatusUntested {
			p := prev.Check(c)
			if n.Status == StatusError {
				p.Detail = p.Detail + " (latest re-check could not run: " + n.Detail + ")"
			}
			out.Checks = append(out.Checks, p)
			continue
		}
		if n.Status != StatusUntested {
			out.Checks = append(out.Checks, n)
		}
	}
	if (out.Metadata == nil || out.Metadata.Error != "") && prev.Metadata != nil && prev.Metadata.Error == "" {
		out.Metadata = prev.Metadata
	}
	return &out
}
