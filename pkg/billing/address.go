// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"encoding/json"
	"strings"
)

// FormatAddress turns a stored billing address into printable lines.
//
// accounts.billing_address is JSONB, and the account form may send either
// plain text (stored as a JSON string) or a structured object, so the value
// reaching an invoice can be a quoted JSON string, an object, or ordinary
// text. All three become newline-separated lines; anything unparseable is used
// as-is rather than dropped, because losing a customer address silently on a
// legal document is worse than printing it imperfectly.
func FormatAddress(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return ""
	}

	var text string
	if json.Unmarshal([]byte(raw), &text) == nil {
		return cleanLines(text)
	}

	var obj map[string]any
	if json.Unmarshal([]byte(raw), &obj) == nil {
		return cleanLines(joinAddressFields(obj))
	}
	return cleanLines(raw)
}

// joinAddressFields assembles the common address field names in reading
// order: street lines, then "city, state postal", then country.
func joinAddressFields(obj map[string]any) string {
	get := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := obj[k].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
	var lines []string
	for _, l := range []string{
		get("name", "attention"),
		get("line1", "address1", "address_line_1", "street", "street1"),
		get("line2", "address2", "address_line_2", "street2"),
	} {
		if l != "" {
			lines = append(lines, l)
		}
	}
	cityLine := strings.TrimSpace(strings.Join(nonEmpty(
		get("city", "town"),
		strings.TrimSpace(get("state", "region", "province")+" "+get("postal_code", "postcode", "zip", "pincode")),
	), ", "))
	if cityLine != "" {
		lines = append(lines, cityLine)
	}
	if c := get("country"); c != "" {
		lines = append(lines, c)
	}
	return strings.Join(lines, "\n")
}

func nonEmpty(parts ...string) []string {
	var out []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

func cleanLines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
