// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"fmt"
	"strings"
)

// The service catalog turns the platform's internal usage identifiers
// ("cpu.home", "kumbha/teepin/fast:input", "object_storage_gb_month") into the
// names a customer reads on an invoice ("CPU compute", "Kumbha", "Object
// storage"). It is the single place that mapping lives: the invoice, the
// billing summary and any future statement all classify through it, so a new
// service is one added rule here instead of a hunt through several files.
//
// A rule is matched on the resource type's prefix; the first match wins, so
// order specific rules before general ones.

// Presentation describes how one usage resource is presented to a customer.
type Presentation struct {
	// Service is the invoice section: "GPU compute", "Inference", ...
	Service string
	// Title is the line's own description within that section.
	Title string
	// Rate is how a unit price should be quoted: "per 1M tokens",
	// "per hour", "per GB-month". Empty when there is no meaningful rate.
	Rate string
	// Scale is the divisor applied to the unit price for display: token
	// prices are quoted per million, so a stored per-token price of 0.0000002
	// shows as 0.20 per 1M tokens. 1 for everything else.
	Scale float64
}

type catalogRule struct {
	prefix string
	build  func(resourceType, rest, unit string) Presentation
}

// serviceRules is ordered: first matching prefix wins.
var serviceRules = []catalogRule{
	{"gpu.", func(_, rest, _ string) Presentation {
		return Presentation{Service: "GPU compute", Title: "GPU " + humanize(rest), Rate: "per hour", Scale: 1}
	}},
	{"cpu.", func(_, rest, _ string) Presentation {
		title := "CPU " + humanize(rest)
		if rest == "home" {
			title = "CPU compute (on-demand node)"
		}
		return Presentation{Service: "CPU compute", Title: title, Rate: "per hour", Scale: 1}
	}},
	{"kumbha/", func(_, rest, _ string) Presentation {
		route, direction := splitDirection(rest)
		return Presentation{Service: "Kumbha", Title: tokenTitle(route, direction), Rate: "per 1M tokens", Scale: 1e6}
	}},
	{"inference/", func(_, rest, _ string) Presentation {
		route, direction := splitDirection(rest)
		return Presentation{Service: "Inference", Title: tokenTitle(route, direction), Rate: "per 1M tokens", Scale: 1e6}
	}},
	{"object_storage_gb_month", func(_, _, _ string) Presentation {
		return Presentation{Service: "Object storage", Title: "Storage", Rate: "per GB-month", Scale: 1}
	}},
	{"object_storage_gb_egress", func(_, _, _ string) Presentation {
		return Presentation{Service: "Object storage", Title: "Data transfer out", Rate: "per GB", Scale: 1}
	}},
	{"storage", func(_, _, _ string) Presentation {
		return Presentation{Service: "Block storage", Title: "Persistent volume", Rate: "per GB-month", Scale: 1}
	}},
	{"network", func(_, _, _ string) Presentation {
		return Presentation{Service: "Networking", Title: "Network", Rate: "per GB", Scale: 1}
	}},
}

// Classify maps a usage resource type to its customer-facing presentation.
// It never returns an empty Title or Service: an unrecognised or blank type
// still renders as something a customer can read ("Other usage"), never as a
// raw identifier or an empty row.
func Classify(resourceType string) Presentation {
	rt := strings.TrimSpace(resourceType)
	if rt == "" {
		return Presentation{Service: "Other charges", Title: "Other usage", Scale: 1}
	}
	for _, r := range serviceRules {
		if strings.HasPrefix(rt, r.prefix) {
			return r.build(rt, strings.TrimPrefix(rt, r.prefix), "")
		}
	}
	return Presentation{Service: "Other charges", Title: humanize(rt), Scale: 1}
}

// splitDirection separates "teepin/fast:input" into ("teepin/fast", "input").
// The older inference rows ("input_tokens") have no route; the direction is
// then the whole remainder.
func splitDirection(rest string) (route, direction string) {
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		return rest[:i], rest[i+1:]
	}
	switch rest {
	case "input_tokens":
		return "", "input"
	case "output_tokens":
		return "", "output"
	}
	return rest, ""
}

func tokenTitle(route, direction string) string {
	what := "tokens"
	switch direction {
	case "input":
		what = "input tokens"
	case "output":
		what = "output tokens"
	}
	if route == "" {
		return strings.ToUpper(what[:1]) + what[1:]
	}
	return route + " — " + what
}

// humanize turns "h100.mig-2g" into "H100 MIG 2g": dots and dashes become
// spaces and short all-caps-looking tokens are upper-cased. Deliberately
// simple — real names belong in a rule above; this only keeps an unknown type
// readable.
func humanize(s string) string {
	s = strings.NewReplacer(".", " ", "-", " ", "_", " ", "/", " / ").Replace(s)
	fields := strings.Fields(s)
	for i, f := range fields {
		switch strings.ToLower(f) {
		case "gpu", "cpu", "mig", "gb", "vram", "h100", "a100", "api":
			fields[i] = strings.ToUpper(f)
		default:
			fields[i] = strings.ToUpper(f[:1]) + f[1:]
		}
	}
	return strings.Join(fields, " ")
}

// FormatQuantity renders a usage quantity the way a customer expects to read
// it: tokens abbreviated (45.0M tokens), hours and GB to sensible precision.
func FormatQuantity(q float64, unit string) string {
	if q == 0 {
		return "-"
	}
	switch strings.ToLower(unit) {
	case "tokens":
		switch {
		case q >= 1e9:
			return fmt.Sprintf("%.2fB tokens", q/1e9)
		case q >= 1e6:
			return fmt.Sprintf("%.2fM tokens", q/1e6)
		case q >= 1e4:
			return fmt.Sprintf("%.1fK tokens", q/1e3)
		default:
			return fmt.Sprintf("%.0f tokens", q)
		}
	case "hours", "hour":
		return fmt.Sprintf("%.2f hours", q)
	case "gb":
		return fmt.Sprintf("%.2f GB", q)
	case "":
		return trimZeros(fmt.Sprintf("%.4f", q))
	default:
		return trimZeros(fmt.Sprintf("%.4f", q)) + " " + unit
	}
}

func trimZeros(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// FormatUnitPrice quotes a stored unit price in the line's natural rate
// ("$0.20 per 1M tokens"). Empty when the line has no meaningful rate.
func FormatUnitPrice(unitPrice float64, line Presentation, currencyPrefix string) string {
	if unitPrice <= 0 || line.Rate == "" {
		return ""
	}
	scale := line.Scale
	if scale == 0 {
		scale = 1
	}
	p := unitPrice * scale
	return currencyPrefix + priceText(p) + " " + line.Rate
}

// priceText prints a price with the precision it needs: at least two decimals,
// up to four when the price has them ("0.20", "0.175", "0.0032"), never
// trailing zeros beyond that.
func priceText(p float64) string {
	s := fmt.Sprintf("%.4f", p)
	for strings.HasSuffix(s, "0") && len(s)-strings.Index(s, ".") > 3 {
		s = s[:len(s)-1]
	}
	return s
}
