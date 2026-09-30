// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package envname

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	good := []string{"A", "AMADEUS_CLIENT_ID", "STRIPE_SECRET_KEY", "DB_URL2", "X_1"}
	for _, n := range good {
		if err := Validate(n); err != nil {
			t.Errorf("%q should be accepted: %v", n, err)
		}
	}
	bad := []string{
		"", "lower", "Mixed_Case", "1STARTS_WITH_DIGIT", "_LEADING", "HAS SPACE", "HAS-DASH", "WITH.DOT", "EMOJI_é",
		"TEEPIN_SESSION_TOKEN", "KUBERNETES_SERVICE_HOST", "LD_PRELOAD", "LD_LIBRARY_PATH", "DYLD_INSERT_LIBRARIES",
		"PATH", "HOME", "NODE_OPTIONS", "PYTHONPATH", strings.Repeat("A", MaxLength+1),
	}
	for _, n := range bad {
		if err := Validate(n); err == nil {
			t.Errorf("%q must be rejected", n)
		}
	}
}
