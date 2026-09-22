// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import "testing"

func TestNoTaxIsTheDefault(t *testing.T) {
	if got := (NoTax{}).Calculate(TaxInput{Country: "IN", Subtotal: 100}); len(got) != 0 {
		t.Errorf("NoTax charged %v", got)
	}
	empty, err := ParseTaxRules("  ")
	if err != nil || len(empty.Calculate(TaxInput{Country: "IN", Subtotal: 100})) != 0 {
		t.Errorf("no configured rules must charge nothing: %v", err)
	}
}

func TestStaticTaxPolicy_AppliesOnlyMatchingCountry(t *testing.T) {
	p, err := ParseTaxRules(`[{"country":"IN","name":"IGST","rate":0.18,"code":"998315"}]`)
	if err != nil {
		t.Fatal(err)
	}
	got := p.Calculate(TaxInput{Country: "in", Subtotal: 100.55})
	if len(got) != 1 || got[0].Name != "IGST" || got[0].Amount != 18.10 || got[0].Code != "998315" || got[0].Jurisdiction != "IN" {
		t.Errorf("India rule = %+v, want IGST 18.10 (18%% of 100.55, rounded)", got)
	}
	if got := p.Calculate(TaxInput{Country: "US", Subtotal: 100}); len(got) != 0 {
		t.Errorf("a US customer was charged an India tax: %+v", got)
	}
	if got := p.Calculate(TaxInput{Country: "", Subtotal: 100}); len(got) != 0 {
		t.Errorf("an unknown country was taxed: %+v", got)
	}
}

func TestStaticTaxPolicy_ReverseChargeIsShownButNotAdded(t *testing.T) {
	p, _ := ParseTaxRules(`[{"country":"IN","name":"IGST","rate":0.18,"reverse_charge_with_tax_id":true}]`)

	b2b := p.Calculate(TaxInput{Country: "IN", TaxID: "29ABCDE1234F1Z5", Subtotal: 100})
	if len(b2b) != 1 || !b2b[0].ReverseCharge || ChargedTax(b2b) != 0 {
		t.Errorf("a registered customer should self-account: %+v charged=%v", b2b, ChargedTax(b2b))
	}
	b2c := p.Calculate(TaxInput{Country: "IN", Subtotal: 100})
	if len(b2c) != 1 || b2c[0].ReverseCharge || ChargedTax(b2c) != 18 {
		t.Errorf("an unregistered customer should be charged: %+v charged=%v", b2c, ChargedTax(b2c))
	}
}

func TestParseTaxRules_RejectsBadConfig(t *testing.T) {
	for _, raw := range []string{`not json`, `[{"name":"X","rate":0.1}]`, `[{"country":"IN","rate":0.1}]`, `[{"country":"IN","name":"X","rate":18}]`} {
		if _, err := ParseTaxRules(raw); err == nil {
			t.Errorf("accepted bad rules: %s", raw)
		}
	}
}
