// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package auth

import (
	"strings"
	"testing"
)

func TestValidateAlias(t *testing.T) {
	valid := []string{"acme", "acme-corp", "flashback-tech", "a1b2c3", "x" + strings.Repeat("y", 61)}
	for _, a := range valid {
		if err := ValidateAlias(a); err != nil {
			t.Errorf("ValidateAlias(%q) = %v, want nil", a, err)
		}
	}

	invalid := map[string]string{
		"":                      "empty",
		"ab":                    "too short",
		"-acme":                 "leading hyphen",
		"acme-":                 "trailing hyphen",
		"ACME":                  "uppercase",
		"acme corp":             "space",
		"acme_corp":             "underscore",
		"acme.corp":             "dot",
		strings.Repeat("x", 64): "too long",
	}
	for a, why := range invalid {
		if err := ValidateAlias(a); err == nil {
			t.Errorf("ValidateAlias(%q) = nil, want error (%s)", a, why)
		}
	}
}

// Reserved aliases would either collide with platform hostnames
// (api.teepin.com, console.teepin.com) or let a customer impersonate
// TEEPIN itself.
func TestValidateAlias_RejectsReserved(t *testing.T) {
	for _, a := range []string{"api", "console", "admin", "teepin", "support", "billing"} {
		if err := ValidateAlias(a); err == nil {
			t.Errorf("ValidateAlias(%q) = nil, want error: reserved", a)
		}
	}
}

func TestValidateUsername(t *testing.T) {
	for _, u := range []string{"alice", "bob.smith", "carol-jones", "dev_1"} {
		if err := ValidateUsername(u); err != nil {
			t.Errorf("ValidateUsername(%q) = %v, want nil", u, err)
		}
	}
	for _, u := range []string{"", "a", "Alice", "alice@acme.com", "-alice"} {
		if err := ValidateUsername(u); err == nil {
			t.Errorf("ValidateUsername(%q) = nil, want error", u)
		}
	}
}

func TestGenerateAccountNumber(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		n, err := generateAccountNumber()
		if err != nil {
			t.Fatalf("generateAccountNumber: %v", err)
		}
		if len(n) != 10 {
			t.Fatalf("account number %q has length %d, want 10", n, len(n))
		}
		if n[0] == '0' {
			t.Errorf("account number %q starts with 0 — would render inconsistently", n)
		}
		for _, r := range n {
			if r < '0' || r > '9' {
				t.Fatalf("account number %q contains a non-digit", n)
			}
		}
		seen[n] = true
	}
	// Random, not sequential: a sequential number would disclose how
	// many customers exist. 200 draws from 9e9 should never repeat.
	if len(seen) != 200 {
		t.Errorf("generated %d distinct numbers from 200 draws — not random", len(seen))
	}
}

func TestAccountFormattedNumber(t *testing.T) {
	a := &Account{AccountNumber: "4815162342"}
	if got, want := a.FormattedNumber(), "4815-1623-42"; got != want {
		t.Errorf("FormattedNumber() = %q, want %q", got, want)
	}

	// Malformed input must not panic — display code runs on every page.
	short := &Account{AccountNumber: "123"}
	if got := short.FormattedNumber(); got != "123" {
		t.Errorf("FormattedNumber() on short input = %q, want %q", got, "123")
	}
}

func TestDeriveAlias(t *testing.T) {
	cases := map[string]string{
		"Flashback Tech":  "flashback-tech",
		"ACME Corp.":      "acme-corp",
		"  spaced  out  ": "spaced-out",
		"Bo":              "bo-acct", // padded to the 3-char minimum
	}
	for in, want := range cases {
		if got := deriveAlias(in); got != want {
			t.Errorf("deriveAlias(%q) = %q, want %q", in, got, want)
		}
	}

	// Whatever it derives must itself be a valid alias.
	for in := range cases {
		if err := ValidateAlias(deriveAlias(in)); err != nil {
			t.Errorf("deriveAlias(%q) produced an invalid alias: %v", in, err)
		}
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"production":   "production",
		"My Project":   "my-project",
		"ACME/prod v2": "acme-prod-v2", // slashes would break URLs
		"  trim  ":     "trim",
		"!!!":          "", // caller rejects empty slugs
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

// User roles gate what a login may do. Billing authority is
// deliberately owner-only: admins run infrastructure, they do not hold
// spending authority.
func TestUserRolePermissions(t *testing.T) {
	cases := []struct {
		role                        string
		manageUsers, billing, write bool
	}{
		{RoleOwner, true, true, true},
		{RoleAdmin, true, false, true},
		{RoleMember, false, false, true},
		{RoleViewer, false, false, false},
	}

	for _, tc := range cases {
		u := &User{Role: tc.role}
		if got := u.CanManageUsers(); got != tc.manageUsers {
			t.Errorf("%s.CanManageUsers() = %v, want %v", tc.role, got, tc.manageUsers)
		}
		if got := u.CanManageBilling(); got != tc.billing {
			t.Errorf("%s.CanManageBilling() = %v, want %v", tc.role, got, tc.billing)
		}
		if got := u.CanWrite(); got != tc.write {
			t.Errorf("%s.CanWrite() = %v, want %v", tc.role, got, tc.write)
		}
	}
}
