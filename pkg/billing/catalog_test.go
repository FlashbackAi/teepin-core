// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		resource string
		service  string
		title    string
	}{
		{"cpu.home", "CPU compute", "CPU compute (on-demand node)"},
		{"cpu.small", "CPU compute", "CPU Small"},
		{"gpu.h100.mig-2g", "GPU compute", "GPU H100 MIG 2g"},
		{"kumbha/teepin/fast:input", "Kumbha", "teepin/fast — input tokens"},
		{"kumbha/teepin/fast:output", "Kumbha", "teepin/fast — output tokens"},
		{"inference/teepin/qwen3-30b-a3b:input", "Inference", "teepin/qwen3-30b-a3b — input tokens"},
		{"inference/teepin/qwen3-30b-a3b:output", "Inference", "teepin/qwen3-30b-a3b — output tokens"},
		// Rows written before inference usage carried the model in the type.
		{"inference/input_tokens", "Inference", "Input tokens"},
		{"inference/output_tokens", "Inference", "Output tokens"},
		{"object_storage_gb_month", "Object storage", "Storage"},
		{"object_storage_gb_egress", "Object storage", "Data transfer out"},
		{"storage_gb", "Block storage", "Persistent volume"},
		{"", "Other charges", "Other usage"},
		{"   ", "Other charges", "Other usage"},
		{"something.new-thing", "Other charges", "Something New Thing"},
	}
	for _, c := range cases {
		got := Classify(c.resource)
		if got.Service != c.service || got.Title != c.title {
			t.Errorf("Classify(%q) = {%q, %q}, want {%q, %q}", c.resource, got.Service, got.Title, c.service, c.title)
		}
	}
}

// A customer must never see an empty row or a raw identifier, whatever the
// resource type looks like.
func TestClassify_NeverEmpty(t *testing.T) {
	for _, rt := range []string{"", " ", "x", "a/b/c", "::", "kumbha/", "inference/", "gpu.", "🙂"} {
		got := Classify(rt)
		if got.Service == "" || got.Title == "" {
			t.Errorf("Classify(%q) produced an empty field: %+v", rt, got)
		}
	}
}

func TestFormatQuantity(t *testing.T) {
	cases := []struct {
		q    float64
		unit string
		want string
	}{
		{45010939, "tokens", "45.01M tokens"},
		{2072459, "tokens", "2.07M tokens"},
		{935, "tokens", "935 tokens"},
		{25000, "tokens", "25.0K tokens"},
		{3.2e9, "tokens", "3.20B tokens"},
		{176.077, "hours", "176.08 hours"},
		{1.5, "GB", "1.50 GB"},
		{0, "hours", "-"},
		{12.5, "", "12.5"},
		{3, "items", "3 items"},
	}
	for _, c := range cases {
		if got := FormatQuantity(c.q, c.unit); got != c.want {
			t.Errorf("FormatQuantity(%v, %q) = %q, want %q", c.q, c.unit, got, c.want)
		}
	}
}

func TestFormatUnitPrice(t *testing.T) {
	tokens := Classify("inference/teepin/x:input")
	if got := FormatUnitPrice(0.0000002, tokens, "$"); got != "$0.20 per 1M tokens" {
		t.Errorf("per-token price quoted as %q, want $0.20 per 1M tokens", got)
	}
	hours := Classify("cpu.home")
	if got := FormatUnitPrice(0.0032, hours, "$"); got != "$0.0032 per hour" {
		t.Errorf("sub-cent hourly price quoted as %q", got)
	}
	if got := FormatUnitPrice(0, hours, "$"); got != "" {
		t.Errorf("a zero price must not be quoted, got %q", got)
	}
	if got := FormatUnitPrice(5, Classify(""), "$"); got != "" {
		t.Errorf("a line with no rate must not be quoted, got %q", got)
	}
}

// Token prices are quoted per million and must keep their real precision: a
// $0.175 rate printed as $0.17 would misstate what the customer is charged.
func TestFormatUnitPrice_KeepsRealPrecision(t *testing.T) {
	tokens := Classify("kumbha/teepin/fast:input")
	cases := []struct {
		price float64
		want  string
	}{
		{0.000000175, "$0.175 per 1M tokens"},
		{0.00000081, "$0.81 per 1M tokens"},
		{0.0000002, "$0.20 per 1M tokens"},
		{0.000003, "$3.00 per 1M tokens"},
	}
	for _, c := range cases {
		if got := FormatUnitPrice(c.price, tokens, "$"); got != c.want {
			t.Errorf("FormatUnitPrice(%v) = %q, want %q", c.price, got, c.want)
		}
	}
}
