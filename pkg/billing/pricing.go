// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/FlashbackAi/teepin-core/pkg/gpu"
)

// PricingInfo is the current platform pricing configuration.
type PricingInfo struct {
	VRAMPricePerGBHour float64 `json:"vram_price_per_gb_hour"`
	// CPU/memory rates for home-class (CPU) compute. Default 0 — CPU
	// instances bill nothing until an operator sets a rate, so nothing
	// running free today is retroactively charged.
	CPUPricePerCoreHour  float64 `json:"cpu_price_per_core_hour"`
	MemoryPricePerGBHour float64 `json:"memory_price_per_gb_hour"`
	// StoragePricePerGBMonth is a GB-MONTH rate, unlike every other rate
	// here (per-hour) — the collector converts it (see hoursPerMonth in
	// collector.go). Same "default 0, ships on, bills nothing until an
	// operator sets it" pattern as the CPU/memory rates.
	StoragePricePerGBMonth float64 `json:"storage_price_per_gb_month"`
	// LLM rates are per-MILLION tokens, priced separately for input and
	// output because the two cost very differently on every backend this
	// gateway routes to. Same "default 0, ships on, bills nothing until an
	// operator sets it" pattern as CPU/memory/storage.
	LLMPricePerMillionInput  float64    `json:"llm_price_per_million_input"`
	LLMPricePerMillionOutput float64    `json:"llm_price_per_million_output"`
	UpdatedBy                *string    `json:"updated_by,omitempty"`
	UpdatedAt                *time.Time `json:"updated_at,omitempty"`
}

// VRAMPricePerGBHour returns the live GPU VRAM rate from billing.pricing.
// It is read fresh on every call — allocations and billing collections
// must always use the current admin-configured price, never a cached or
// compiled-in one. Falls back to the compiled default when the platform
// runs without a database or the read fails (fail-open on the known
// launch rate rather than refusing service).
func (s *Service) VRAMPricePerGBHour(ctx context.Context) float64 {
	if s == nil || s.db == nil {
		return gpu.DefaultPricePerGBHour
	}

	var price float64
	err := s.db.QueryRowContext(ctx,
		`SELECT vram_price_per_gb_hour FROM billing.pricing WHERE id = 1`,
	).Scan(&price)
	if err != nil || price <= 0 {
		log.Printf("WARN: pricing read failed (using default $%.2f/GB-hr): %v", gpu.DefaultPricePerGBHour, err)
		return gpu.DefaultPricePerGBHour
	}
	return price
}

// CPUCoreRate / MemoryGBRate return the live CPU and memory rates for
// home-class metering. Read fresh each call, like the VRAM rate. Unlike VRAM
// there is NO compiled default: the rates are 0 unless an operator set them,
// and 0 is a legitimate "do not charge" value, so a read miss returns 0
// rather than inventing a price.
func (s *Service) CPUCoreRate(ctx context.Context) float64 {
	return s.rate(ctx, "cpu_price_per_core_hour")
}

func (s *Service) MemoryGBRate(ctx context.Context) float64 {
	return s.rate(ctx, "memory_price_per_gb_hour")
}

// StorageGBMonthRate returns the live per-GB-month persistent-storage
// rate. Same 0-default, no-compiled-fallback contract as CPUCoreRate.
func (s *Service) StorageGBMonthRate(ctx context.Context) float64 {
	return s.rate(ctx, "storage_price_per_gb_month")
}

// LLMPriceInputPerMillion / LLMPriceOutputPerMillion return the live Kumbha
// Gateway rates, per million tokens. Same 0-default, no-compiled-fallback
// contract as CPUCoreRate — inference bills nothing until an operator sets
// a rate, same as every other dimension added after launch.
func (s *Service) LLMPriceInputPerMillion(ctx context.Context) float64 {
	return s.rate(ctx, "llm_price_per_million_input")
}

func (s *Service) LLMPriceOutputPerMillion(ctx context.Context) float64 {
	return s.rate(ctx, "llm_price_per_million_output")
}

// rate reads one numeric pricing column, returning 0 on any error (the
// safe direction for a rate that defaults to "free").
func (s *Service) rate(ctx context.Context, column string) float64 {
	if s == nil || s.db == nil {
		return 0
	}
	var v float64
	// column is a compiled-in constant from the two callers above, never
	// user input, so interpolating it is safe.
	err := s.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT %s FROM billing.pricing WHERE id = 1`, column),
	).Scan(&v)
	if err != nil {
		return 0
	}
	return v
}

// GetPricing returns the full pricing configuration row for the admin API.
func (s *Service) GetPricing(ctx context.Context) (*PricingInfo, error) {
	info := &PricingInfo{}
	err := s.db.QueryRowContext(ctx,
		`SELECT vram_price_per_gb_hour, cpu_price_per_core_hour,
		        memory_price_per_gb_hour, storage_price_per_gb_month,
		        llm_price_per_million_input, llm_price_per_million_output,
		        updated_by, updated_at
		 FROM billing.pricing WHERE id = 1`,
	).Scan(&info.VRAMPricePerGBHour, &info.CPUPricePerCoreHour,
		&info.MemoryPricePerGBHour, &info.StoragePricePerGBMonth,
		&info.LLMPricePerMillionInput, &info.LLMPricePerMillionOutput,
		&info.UpdatedBy, &info.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("failed to read pricing: %w", err)
	}
	return info, nil
}

// SetLLMPricing updates the Kumbha Gateway's per-million-token rates. Zero
// is allowed (means "do not charge"), same as CPU/memory/storage — a
// separate endpoint from the GPU rate's PUT so that rate's "must be
// positive" contract stays untouched.
func (s *Service) SetLLMPricing(ctx context.Context, inputRate, outputRate float64, updatedBy string) error {
	if inputRate < 0 || outputRate < 0 {
		return fmt.Errorf("rates must be non-negative")
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE billing.pricing
		 SET llm_price_per_million_input = $1, llm_price_per_million_output = $2,
		     updated_by = $3, updated_at = NOW()
		 WHERE id = 1`,
		inputRate, outputRate, updatedBy)
	if err != nil {
		return fmt.Errorf("failed to update LLM pricing: %w", err)
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return fmt.Errorf("pricing row missing; set the GPU rate first")
	}
	log.Printf("LLM pricing updated to $%.4f/M input, $%.4f/M output tokens by %s", inputRate, outputRate, updatedBy)
	return nil
}

// SetCPUPricing updates the CPU and memory rates for home-class metering.
// Zero is allowed (it means "do not charge"), unlike the VRAM rate which must
// be positive. Takes effect on the next billing tick; metered usage keeps its
// rate.
func (s *Service) SetCPUPricing(ctx context.Context, cpuRate, memRate float64, updatedBy string) error {
	if cpuRate < 0 || memRate < 0 {
		return fmt.Errorf("rates must be non-negative")
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE billing.pricing
		 SET cpu_price_per_core_hour = $1, memory_price_per_gb_hour = $2,
		     updated_by = $3, updated_at = NOW()
		 WHERE id = 1`,
		cpuRate, memRate, updatedBy)
	if err != nil {
		return fmt.Errorf("failed to update CPU pricing: %w", err)
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return fmt.Errorf("pricing row missing; set the GPU rate first")
	}
	log.Printf("CPU pricing updated to $%.4f/core-hr, $%.4f/GB-hr by %s", cpuRate, memRate, updatedBy)
	return nil
}

// SetStoragePricing updates the persistent-storage GB-month rate. Zero is
// allowed (means "do not charge"), same as CPU/memory — a separate
// endpoint from the GPU rate's PUT so that rate's "must be positive"
// contract stays untouched.
func (s *Service) SetStoragePricing(ctx context.Context, gbMonthRate float64, updatedBy string) error {
	if gbMonthRate < 0 {
		return fmt.Errorf("rate must be non-negative")
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE billing.pricing
		 SET storage_price_per_gb_month = $1, updated_by = $2, updated_at = NOW()
		 WHERE id = 1`,
		gbMonthRate, updatedBy)
	if err != nil {
		return fmt.Errorf("failed to update storage pricing: %w", err)
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return fmt.Errorf("pricing row missing; set the GPU rate first")
	}
	log.Printf("Storage pricing updated to $%.4f/GB-month by %s", gbMonthRate, updatedBy)
	return nil
}

// SetVRAMPricePerGBHour updates the live GPU VRAM rate. Takes effect on
// the next allocation and the next billing collection tick; already
// written usage records keep the rate they were metered at.
func (s *Service) SetVRAMPricePerGBHour(ctx context.Context, price float64, updatedBy string) error {
	if price <= 0 {
		return fmt.Errorf("price must be positive, got %.4f", price)
	}

	result, err := s.db.ExecContext(ctx,
		`UPDATE billing.pricing
		 SET vram_price_per_gb_hour = $1, updated_by = $2, updated_at = NOW()
		 WHERE id = 1`,
		price, updatedBy)
	if err != nil {
		return fmt.Errorf("failed to update pricing: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to update pricing: %w", err)
	}
	if rows == 0 {
		// Row is seeded by migration 006; recreate it if it was removed.
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO billing.pricing (id, vram_price_per_gb_hour, updated_by) VALUES (1, $1, $2)`,
			price, updatedBy); err != nil {
			return fmt.Errorf("failed to insert pricing row: %w", err)
		}
	}

	log.Printf("GPU pricing updated to $%.4f/GB-hour by %s", price, updatedBy)
	return nil
}
