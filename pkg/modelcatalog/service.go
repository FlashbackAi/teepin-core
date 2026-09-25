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

// defaultMaxOutputTokens is used when a registration leaves
// MaxOutputTokens unset — the same default the column itself carries.
const defaultMaxOutputTokens = 4096

// validate checks a registration and fills in defaults for the fields a
// caller may leave empty.
func (m *Model) validate() error {
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
	if m.Provider == "" {
		m.Provider = ProviderNode
	}
	switch m.Provider {
	case ProviderNode:
	case ProviderAnthropic:
		if m.ProviderModel == "" {
			return fmt.Errorf("provider_model is required for an anthropic model")
		}
	case ProviderOpenAICompatible:
		if m.ProviderModel == "" || m.BaseURL == "" {
			return fmt.Errorf("provider_model and base_url are required for an openai_compatible model")
		}
	case ProviderTinfoilConfidential:
		if m.ProviderModel == "" || m.BaseURL == "" {
			return fmt.Errorf("provider_model and base_url are required for a tinfoil_confidential model")
		}
	default:
		return fmt.Errorf("invalid provider %q", m.Provider)
	}
	if m.MaxOutputTokens <= 0 {
		m.MaxOutputTokens = defaultMaxOutputTokens
	}
	return nil
}

// RegisterModel creates or updates a catalog entry's capabilities and how
// it is served. Pricing, availability and the API key are intentionally
// NOT part of this call — SetPricing, SetVendorCost, SetAvailability and
// SetAPIKeyRef are separate, so editing a model's capabilities can never
// disturb a price, a Kumbha ordering, or a stored key an admin already set
// (the upsert below leaves those columns alone).
func (s *Service) RegisterModel(ctx context.Context, m Model) error {
	if err := m.validate(); err != nil {
		return err
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO inference.models
			(model_route, display_name, cost_class, engine, context_window,
			 supports_tools, supports_vision, supports_audio, enabled,
			 provider, provider_model, base_url, max_output_tokens, updated_by, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, NOW())
		ON CONFLICT (model_route) DO UPDATE SET
			display_name      = EXCLUDED.display_name,
			cost_class        = EXCLUDED.cost_class,
			engine            = EXCLUDED.engine,
			context_window    = EXCLUDED.context_window,
			supports_tools    = EXCLUDED.supports_tools,
			supports_vision   = EXCLUDED.supports_vision,
			supports_audio    = EXCLUDED.supports_audio,
			enabled           = EXCLUDED.enabled,
			provider          = EXCLUDED.provider,
			provider_model    = EXCLUDED.provider_model,
			base_url          = EXCLUDED.base_url,
			max_output_tokens = EXCLUDED.max_output_tokens,
			updated_by        = EXCLUDED.updated_by,
			updated_at        = NOW()
	`, m.ModelRoute, m.DisplayName, string(m.CostClass), m.Engine, m.ContextWindow,
		m.SupportsTools, m.SupportsVision, m.SupportsAudio, m.Enabled,
		string(m.Provider), m.ProviderModel, m.BaseURL, m.MaxOutputTokens, m.UpdatedBy)
	if err != nil {
		return fmt.Errorf("failed to register model %q: %w", m.ModelRoute, err)
	}
	log.Printf("Model catalog: registered %q (%s via %s, cost_class=%s)", m.ModelRoute, m.Engine, m.Provider, m.CostClass)
	return nil
}

// Availability is a partial update of where a model may be used; a nil
// field is left as it is.
type Availability struct {
	OfferedToCustomers *bool
	KumbhaEnabled      *bool
	KumbhaPriority     *int
}

// SetAvailability updates whether a model is offered to customers and
// whether (and in what order) the Kumbha build agent may use it.
func (s *Service) SetAvailability(ctx context.Context, modelRoute string, a Availability, updatedBy string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE inference.models
		SET offered_to_customers = COALESCE($1, offered_to_customers),
		    kumbha_enabled       = COALESCE($2, kumbha_enabled),
		    kumbha_priority      = COALESCE($3, kumbha_priority),
		    updated_by = $4, updated_at = NOW()
		WHERE model_route = $5
	`, a.OfferedToCustomers, a.KumbhaEnabled, a.KumbhaPriority, updatedBy, modelRoute)
	if err != nil {
		return fmt.Errorf("failed to set availability for %q: %w", modelRoute, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetAPIKeyRef records which secret holds the model's API key (see
// Model.APIKeyRef), or clears it with "". The caller writes the secret
// itself; the catalog only ever stores where it is.
func (s *Service) SetAPIKeyRef(ctx context.Context, modelRoute, ref, updatedBy string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE inference.models
		SET api_key_ref = NULLIF($1, ''), updated_by = $2, updated_at = NOW()
		WHERE model_route = $3
	`, ref, updatedBy, modelRoute)
	if err != nil {
		return fmt.Errorf("failed to set api key for %q: %w", modelRoute, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListKumbhaModels returns every enabled, Kumbha-enabled model right now, in
// the order the build composer's picker should default to (kumbha_priority,
// then route). Kumbha's gateway resolves a customer's directly-chosen route
// against this same list (see pkg/kumbha/gateway.go's resolveModel) — there
// is no alias/tier filter: a customer addresses one exact model, never a
// bucket that could silently resolve to a different one.
func (s *Service) ListKumbhaModels(ctx context.Context) ([]Model, error) {
	return s.queryModels(ctx,
		selectModelsSQL+` WHERE enabled AND kumbha_enabled ORDER BY kumbha_priority, model_route`)
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
	return s.queryModels(ctx, selectModelsSQL+` ORDER BY model_route`)
}

func (s *Service) queryModels(ctx context.Context, query string, args ...any) ([]Model, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
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
	       enabled, provider, provider_model, base_url, max_output_tokens,
	       COALESCE(api_key_ref, ''), offered_to_customers, kumbha_enabled, kumbha_priority,
	       updated_by, created_at, updated_at
	FROM inference.models`

// row is satisfied by both *sql.Row and *sql.Rows, so scanModel/scanModelRow
// share one Scan call shape.
type row interface {
	Scan(dest ...any) error
}

func scanModel(r row) (*Model, error) { return scanModelRow(r) }

func scanModelRow(r row) (*Model, error) {
	var m Model
	var costClass, provider string
	if err := r.Scan(
		&m.ModelRoute, &m.DisplayName, &costClass, &m.Engine, &m.ContextWindow,
		&m.SupportsTools, &m.SupportsVision, &m.SupportsAudio,
		&m.InputPricePerMillion, &m.OutputPricePerMillion,
		&m.VendorInputCostPerMillion, &m.VendorOutputCostPerMillion,
		&m.Enabled, &provider, &m.ProviderModel, &m.BaseURL, &m.MaxOutputTokens,
		&m.APIKeyRef, &m.OfferedToCustomers, &m.KumbhaEnabled, &m.KumbhaPriority,
		&m.UpdatedBy, &m.CreatedAt, &m.UpdatedAt,
	); err != nil {
		return nil, err
	}
	m.CostClass = CostClass(costClass)
	m.Provider = Provider(provider)
	m.HasAPIKey = m.APIKeyRef != ""
	return &m, nil
}
