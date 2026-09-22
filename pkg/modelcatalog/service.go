// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package modelcatalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
)

// ErrNotFound means the model_route does not exist in the catalog.
var ErrNotFound = errors.New("model not found in catalog")

// Service manages Teepin Inference's model catalog and pricing.
type Service struct {
	db *sql.DB
}

// NewService constructs the catalog service.
func NewService(db *sql.DB) *Service { return &Service{db: db} }

// RegisterModel creates or updates a catalog entry's capabilities and
// engine. Pricing is intentionally NOT part of this call — SetPricing and
// SetVendorCost are separate, mirroring billing.pricing's own convention of
// one endpoint per rate-pair so setting capabilities can never accidentally
// disturb a price an admin already configured (an upsert here preserves
// existing pricing columns rather than resetting them to 0).
func (s *Service) RegisterModel(ctx context.Context, m Model) error {
	if m.ModelRoute == "" {
		return fmt.Errorf("model_route is required")
	}
	if m.CostClass != CostClassOwn && m.CostClass != CostClassFrontier {
		return fmt.Errorf("invalid cost_class %q", m.CostClass)
	}
	if m.Engine == "" {
		return fmt.Errorf("engine is required")
	}
	if m.ContextWindow < 0 {
		return fmt.Errorf("context_window must be non-negative")
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO inference.models
			(model_route, display_name, cost_class, engine, context_window,
			 supports_tools, supports_vision, supports_audio, enabled, updated_by, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW())
		ON CONFLICT (model_route) DO UPDATE SET
			display_name    = EXCLUDED.display_name,
			cost_class      = EXCLUDED.cost_class,
			engine          = EXCLUDED.engine,
			context_window  = EXCLUDED.context_window,
			supports_tools  = EXCLUDED.supports_tools,
			supports_vision = EXCLUDED.supports_vision,
			supports_audio  = EXCLUDED.supports_audio,
			enabled         = EXCLUDED.enabled,
			updated_by      = EXCLUDED.updated_by,
			updated_at      = NOW()
	`, m.ModelRoute, m.DisplayName, string(m.CostClass), m.Engine, m.ContextWindow,
		m.SupportsTools, m.SupportsVision, m.SupportsAudio, m.Enabled, m.UpdatedBy)
	if err != nil {
		return fmt.Errorf("failed to register model %q: %w", m.ModelRoute, err)
	}
	log.Printf("Model catalog: registered %q (%s, cost_class=%s)", m.ModelRoute, m.Engine, m.CostClass)
	return nil
}

// SetPricing updates a model's customer-facing per-million-token rates.
// Zero is valid ("do not charge"), same contract as every other rate this
// platform exposes.
func (s *Service) SetPricing(ctx context.Context, modelRoute string, inputRate, outputRate float64, updatedBy string) error {
	if inputRate < 0 || outputRate < 0 {
		return fmt.Errorf("rates must be non-negative")
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE inference.models
		SET input_price_per_million = $1, output_price_per_million = $2,
		    updated_by = $3, updated_at = NOW()
		WHERE model_route = $4
	`, inputRate, outputRate, updatedBy, modelRoute)
	if err != nil {
		return fmt.Errorf("failed to set pricing for %q: %w", modelRoute, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	log.Printf("Model catalog: %q priced at $%.4f/M input, $%.4f/M output by %s", modelRoute, inputRate, outputRate, updatedBy)
	return nil
}

// SetVendorCost records what a frontier model actually costs Teepin per
// million tokens — separate from SetPricing because it is a cost, not a
// price, and feeds the admin margin view rather than customer billing.
// Passing nil for either value clears it back to "unknown" rather than a
// fabricated zero.
func (s *Service) SetVendorCost(ctx context.Context, modelRoute string, vendorInput, vendorOutput *float64, updatedBy string) error {
	if vendorInput != nil && *vendorInput < 0 {
		return fmt.Errorf("vendor input cost must be non-negative")
	}
	if vendorOutput != nil && *vendorOutput < 0 {
		return fmt.Errorf("vendor output cost must be non-negative")
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE inference.models
		SET vendor_input_cost_per_million = $1, vendor_output_cost_per_million = $2,
		    updated_by = $3, updated_at = NOW()
		WHERE model_route = $4
	`, vendorInput, vendorOutput, updatedBy, modelRoute)
	if err != nil {
		return fmt.Errorf("failed to set vendor cost for %q: %w", modelRoute, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetEnabled gates whether the router may consider this model. Retiring a
// model (enabled=false) keeps its pricing/audit history rather than
// deleting the row.
func (s *Service) SetEnabled(ctx context.Context, modelRoute string, enabled bool, updatedBy string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE inference.models
		SET enabled = $1, updated_by = $2, updated_at = NOW()
		WHERE model_route = $3
	`, enabled, updatedBy, modelRoute)
	if err != nil {
		return fmt.Errorf("failed to set enabled for %q: %w", modelRoute, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ModelPricing returns a route's customer-facing per-million-token rates,
// for a caller (Kumbha's own gateway) that wants to price a completion by
// route rather than read the whole catalog entry. ok is false when the
// route has no catalog entry at all — the caller should fall back to
// whatever flat default it has, rather than silently billing $0 for a real
// backend nobody has registered yet.
func (s *Service) ModelPricing(ctx context.Context, modelRoute string) (input, output float64, ok bool) {
	m, err := s.GetModel(ctx, modelRoute)
	if err != nil {
		return 0, 0, false
	}
	return m.InputPricePerMillion, m.OutputPricePerMillion, true
}

// GetModel returns one catalog entry.
func (s *Service) GetModel(ctx context.Context, modelRoute string) (*Model, error) {
	m, err := scanModel(s.db.QueryRowContext(ctx, selectModelsSQL+` WHERE model_route = $1`, modelRoute))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get model %q: %w", modelRoute, err)
	}
	return m, nil
}

// ListModels returns the full catalog, enabled and disabled alike — callers
// that only want routable models should filter on Enabled themselves (the
// admin catalog page wants to show disabled entries too).
func (s *Service) ListModels(ctx context.Context) ([]Model, error) {
	rows, err := s.db.QueryContext(ctx, selectModelsSQL+` ORDER BY model_route`)
	if err != nil {
		return nil, fmt.Errorf("failed to list models: %w", err)
	}
	defer rows.Close()

	// Initialized, not a nil slice: an empty result marshals to JSON `null`
	// otherwise, which crashes any caller that expects an array to filter
	// or read .length on (found live 2026-09-18 — the console's own
	// catalog page did exactly that against a genuinely empty catalog).
	out := []Model{}
	for rows.Next() {
		m, err := scanModelRow(rows)
		if err != nil {
			return nil, fmt.Errorf("failed to scan model: %w", err)
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// DeleteModel removes a catalog entry entirely. Prefer SetEnabled(false) for
// a model that has ever been priced or billed against — this is for
// cleaning up a mistaken registration, not retiring a real one.
func (s *Service) DeleteModel(ctx context.Context, modelRoute string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM inference.models WHERE model_route = $1`, modelRoute)
	if err != nil {
		return fmt.Errorf("failed to delete model %q: %w", modelRoute, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

const selectModelsSQL = `
	SELECT model_route, display_name, cost_class, engine, context_window,
	       supports_tools, supports_vision, supports_audio,
	       input_price_per_million, output_price_per_million,
	       vendor_input_cost_per_million, vendor_output_cost_per_million,
	       enabled, updated_by, created_at, updated_at
	FROM inference.models`

// row is satisfied by both *sql.Row and *sql.Rows, so scanModel/scanModelRow
// share one Scan call shape.
type row interface {
	Scan(dest ...any) error
}

func scanModel(r row) (*Model, error) { return scanModelRow(r) }

func scanModelRow(r row) (*Model, error) {
	var m Model
	var costClass string
	if err := r.Scan(
		&m.ModelRoute, &m.DisplayName, &costClass, &m.Engine, &m.ContextWindow,
		&m.SupportsTools, &m.SupportsVision, &m.SupportsAudio,
		&m.InputPricePerMillion, &m.OutputPricePerMillion,
		&m.VendorInputCostPerMillion, &m.VendorOutputCostPerMillion,
		&m.Enabled, &m.UpdatedBy, &m.CreatedAt, &m.UpdatedAt,
	); err != nil {
		return nil, err
	}
	m.CostClass = CostClass(costClass)
	return &m, nil
}
