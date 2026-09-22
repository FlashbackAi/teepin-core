// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// TaxInput is what a tax decision is made on.
type TaxInput struct {
	Country  string // bill-to ISO 3166-1 alpha-2
	TaxID    string // the customer registration number, if they gave one
	Subtotal float64
	Currency string
}

// TaxPolicy decides which taxes apply to an invoice. Pluggable on purpose: the
// rules are a legal and registration question (Teepin may charge a tax only in
// a jurisdiction where it is registered), so they are configuration the
// operator turns on deliberately, never code that guesses. The default policy
// charges no tax.
type TaxPolicy interface {
	Calculate(in TaxInput) []TaxLine
}

// NoTax charges nothing. It is the default.
type NoTax struct{}

// Calculate implements TaxPolicy.
func (NoTax) Calculate(TaxInput) []TaxLine { return nil }

// TaxRule is one configured tax.
type TaxRule struct {
	// Country the rule applies to (ISO alpha-2). "*" applies everywhere.
	Country string `json:"country"`
	Name    string `json:"name"`
	// Rate as a fraction: 0.18 for 18%.
	Rate float64 `json:"rate"`
	// Code printed beside the charge where the jurisdiction requires one
	// (for example a GST service accounting code). Optional.
	Code string `json:"code,omitempty"`
	// ReverseChargeWithTaxID: when the customer supplied a tax ID, the
	// customer accounts for the tax and it is shown but not added.
	ReverseChargeWithTaxID bool `json:"reverse_charge_with_tax_id,omitempty"`
}

// StaticTaxPolicy applies a fixed list of rules.
type StaticTaxPolicy struct{ Rules []TaxRule }

// ParseTaxRules reads rules from JSON (the TEEPIN_TAX_RULES setting). An empty
// string means no rules.
func ParseTaxRules(raw string) (*StaticTaxPolicy, error) {
	if strings.TrimSpace(raw) == "" {
		return &StaticTaxPolicy{}, nil
	}
	var rules []TaxRule
	if err := json.Unmarshal([]byte(raw), &rules); err != nil {
		return nil, fmt.Errorf("invalid tax rules: %w", err)
	}
	for i, r := range rules {
		if strings.TrimSpace(r.Country) == "" || strings.TrimSpace(r.Name) == "" {
			return nil, fmt.Errorf("tax rule %d: country and name are required", i)
		}
		if r.Rate < 0 || r.Rate > 1 {
			return nil, fmt.Errorf("tax rule %d (%s): rate must be a fraction between 0 and 1", i, r.Name)
		}
	}
	return &StaticTaxPolicy{Rules: rules}, nil
}

// Calculate applies every rule that matches the customer country.
func (p *StaticTaxPolicy) Calculate(in TaxInput) []TaxLine {
	var lines []TaxLine
	country := strings.ToUpper(strings.TrimSpace(in.Country))
	for _, r := range p.Rules {
		if r.Country != "*" && !strings.EqualFold(r.Country, country) {
			continue
		}
		lines = append(lines, TaxLine{
			Name:          r.Name,
			Jurisdiction:  strings.ToUpper(r.Country),
			Rate:          r.Rate,
			Amount:        roundCents(in.Subtotal * r.Rate),
			Code:          r.Code,
			ReverseCharge: r.ReverseChargeWithTaxID && strings.TrimSpace(in.TaxID) != "",
		})
	}
	return lines
}

// ChargedTax sums the tax actually added to the total: reverse-charge lines
// are informational and excluded.
func ChargedTax(lines []TaxLine) float64 {
	var sum float64
	for _, l := range lines {
		if !l.ReverseCharge {
			sum += l.Amount
		}
	}
	return roundCents(sum)
}

func roundCents(v float64) float64 { return math.Round(v*100) / 100 }
