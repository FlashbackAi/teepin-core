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

// ErrPricingRequired means the change would make a model available (enabled,
// offered to customers, or usable by Teepin Build) while it has no price. A model
// left at the default $0 is served for free without a trace, so the catalog
// refuses to switch one on until both its input and output prices are set.
var ErrPricingRequired = errors.New("set the model's input and output price (both above zero) before making it available")

// Priced reports whether the model has a customer price on both sides.
func (m *Model) Priced() bool {
	return m.InputPricePerMillion > 0 && m.OutputPricePerMillion > 0
}

// pricedSQL is the catalog's own definition of "priced", for use in a WHERE
// clause so the rule is enforced in the same statement as the change.
const pricedSQL = `(input_price_per_million > 0 AND output_price_per_million > 0)`

// whyNotUpdated explains an UPDATE that touched no row: the model does not exist,
// or it exists and the pricing rule refused the change.
func (s *Service) whyNotUpdated(ctx context.Context, modelRoute string) error {
	if _, err := s.GetModel(ctx, modelRoute); err != nil {
		return err // ErrNotFound, or a read failure
	}
	return ErrPricingRequired
}

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
// NOT part of this call - SetPricing, SetVendorCost, SetAvailability and
// SetAPIKeyRef are separate, so editing a model's capabilities can never
// disturb a price, a Kumbha ordering, or a stored key an admin already set
// (the upsert below leaves those columns alone).
//
// A model registered as enabled must already be priced; use
// RegisterModelWithPricing to set the price in the same call.
func (s *Service) RegisterModel(ctx context.Context, m Model) error {
	return s.RegisterModelWithPricing(ctx, m, nil, nil)
}

// RegisterModelWithPricing is RegisterModel plus an optional price. A nil price
// leaves whatever is stored untouched (a new model starts at 0); a given one is
// written with the rest. Registering a model as enabled is refused unless it ends
// up with both prices above zero, whether from this call or already stored.
func (s *Service) RegisterModelWithPricing(ctx context.Context, m Model, inputPrice, outputPrice *float64) error {
	if err := m.validate(); err != nil {
		return err
	}
	if (inputPrice != nil && *inputPrice < 0) || (outputPrice != nil && *outputPrice < 0) {
		return fmt.Errorf("rates must be non-negative")
	}
	bothGiven := inputPrice != nil && outputPrice != nil && *inputPrice > 0 && *outputPrice > 0
	if m.Enabled && !bothGiven {
		// Not fully priced by this call, so what is already stored decides.
		effective := Model{}
		if existing, err := s.GetModel(ctx, m.ModelRoute); err == nil {
			effective = *existing
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		if inputPrice != nil {
			effective.InputPricePerMillion = *inputPrice
		}
		if outputPrice != nil {
			effective.OutputPricePerMillion = *outputPrice
		}
		if !effective.Priced() {
			return ErrPricingRequired
		}
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO inference.models
			(model_route, display_name, cost_class, engine, context_window,
			 supports_tools, supports_vision, supports_audio, enabled,
			 provider, provider_model, base_url, max_output_tokens, updated_by,
			 input_price_per_million, output_price_per_million, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
		        COALESCE($15::numeric, 0), COALESCE($16::numeric, 0), NOW())
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
			input_price_per_million  = COALESCE($15::numeric, inference.models.input_price_per_million),
			output_price_per_million = COALESCE($16::numeric, inference.models.output_price_per_million),
			updated_by        = EXCLUDED.updated_by,
			updated_at        = NOW()
	`, m.ModelRoute, m.DisplayName, string(m.CostClass), m.Engine, m.ContextWindow,
		m.SupportsTools, m.SupportsVision, m.SupportsAudio, m.Enabled,
		string(m.Provider), m.ProviderModel, m.BaseURL, m.MaxOutputTokens, m.UpdatedBy,
		inputPrice, outputPrice)
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
	KumbhaImageReader  *bool
}

// SetAvailability updates whether a model is offered to customers and
// whether (and in what order) the Kumbha build agent may use it.
//
// Turning either availability ON requires the model to be priced; the check is
// part of the UPDATE itself, so there is no window between checking and changing.
func (s *Service) SetAvailability(ctx context.Context, modelRoute string, a Availability, updatedBy string) error {
	switchingOn := (a.OfferedToCustomers != nil && *a.OfferedToCustomers) ||
		(a.KumbhaEnabled != nil && *a.KumbhaEnabled) ||
		(a.KumbhaImageReader != nil && *a.KumbhaImageReader)
	res, err := s.db.ExecContext(ctx, `
		UPDATE inference.models
		SET offered_to_customers = COALESCE($1, offered_to_customers),
		    kumbha_enabled       = COALESCE($2, kumbha_enabled),
		    kumbha_priority      = COALESCE($3, kumbha_priority),
		    kumbha_image_reader  = COALESCE($4, kumbha_image_reader),
		    updated_by = $5, updated_at = NOW()
		WHERE model_route = $6 AND (NOT $7::boolean OR `+pricedSQL+`)
	`, a.OfferedToCustomers, a.KumbhaEnabled, a.KumbhaPriority, a.KumbhaImageReader, updatedBy, modelRoute, switchingOn)
	if err != nil {
		return fmt.Errorf("failed to set availability for %q: %w", modelRoute, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return s.whyNotUpdated(ctx, modelRoute)
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

// ListImageReaders returns every enabled model set as an image reader, in
// kumbha_priority order. The caller uses the first one that can serve.
func (s *Service) ListImageReaders(ctx context.Context) ([]Model, error) {
	return s.queryModels(ctx,
		selectModelsSQL+` WHERE enabled AND kumbha_image_reader ORDER BY kumbha_priority, model_route`)
}

// SetPricing updates a model's customer-facing per-million-token rates.
// Zero is allowed while a model is switched off (it is simply not priced yet),
// but never on an enabled one: it would be served for free. To clear the price of
// a live model, disable it first. (Enabled is what makes a model live: the
// customer and Teepin Build flags only choose where an enabled model is offered.)
func (s *Service) SetPricing(ctx context.Context, modelRoute string, inputRate, outputRate float64, updatedBy string) error {
	if inputRate < 0 || outputRate < 0 {
		return fmt.Errorf("rates must be non-negative")
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE inference.models
		SET input_price_per_million = $1, output_price_per_million = $2,
		    updated_by = $3, updated_at = NOW()
		WHERE model_route = $4
		  AND (($1::numeric > 0 AND $2::numeric > 0) OR NOT enabled)
	`, inputRate, outputRate, updatedBy, modelRoute)
	if err != nil {
		return fmt.Errorf("failed to set pricing for %q: %w", modelRoute, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return s.whyNotUpdated(ctx, modelRoute)
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
//
// Enabling requires the model to be priced (see ErrPricingRequired); disabling
// never does.
func (s *Service) SetEnabled(ctx context.Context, modelRoute string, enabled bool, updatedBy string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE inference.models
		SET enabled = $1, updated_by = $2, updated_at = NOW()
		WHERE model_route = $3 AND (NOT $1::boolean OR `+pricedSQL+`)
	`, enabled, updatedBy, modelRoute)
	if err != nil {
		return fmt.Errorf("failed to set enabled for %q: %w", modelRoute, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return s.whyNotUpdated(ctx, modelRoute)
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

// ModelVendorCost returns what a route costs Teepin per million tokens, nil for
// whichever is unrecorded (and for both on a model Teepin runs itself). Lets
// Kumbha's gateway record the margin on each usage line.
func (s *Service) ModelVendorCost(ctx context.Context, modelRoute string) (input, output *float64) {
	m, err := s.GetModel(ctx, modelRoute)
	if err != nil {
		return nil, nil
	}
	return m.VendorInputCostPerMillion, m.VendorOutputCostPerMillion
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
	       kumbha_image_reader, updated_by, created_at, updated_at
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
		&m.KumbhaImageReader, &m.UpdatedBy, &m.CreatedAt, &m.UpdatedAt,
	); err != nil {
		return nil, err
	}
	m.CostClass = CostClass(costClass)
	m.Provider = Provider(provider)
	m.HasAPIKey = m.APIKeyRef != ""
	return &m, nil
}
