// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Package envname validates the names of environment variables a customer
// supplies to a deployed app as secrets. It is its own tiny package so the
// agent's tool server (cmd/build-mcp-server) and the control plane apply the
// exact same rule without the tool server importing the control plane.
package envname

import (
	"fmt"
	"regexp"
	"strings"
)

// MaxLength is the longest accepted name.
const MaxLength = 64

var nameRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// reservedPrefixes are names the platform or the container runtime own. A
// customer secret must never be able to override them.
var reservedPrefixes = []string{"TEEPIN_", "KUBERNETES_", "LD_", "DYLD_"}

// reservedNames are exact names that change how a process starts or where it
// looks for code, so setting one from a secret form would be a way to alter
// the container's behaviour rather than configure the app.
var reservedNames = map[string]bool{
	"PATH": true, "HOME": true, "PWD": true, "SHELL": true, "USER": true,
	"NODE_OPTIONS": true, "PYTHONPATH": true, "PYTHONSTARTUP": true,
}

// Validate reports why name is not an acceptable secret variable name, or
// nil. Names are upper-case letters, digits and underscores, starting with a
// letter — the portable subset every runtime accepts.
func Validate(name string) error {
	if name == "" {
		return fmt.Errorf("a name is required")
	}
	if len(name) > MaxLength {
		return fmt.Errorf("the name is longer than %d characters", MaxLength)
	}
	if !nameRE.MatchString(name) {
		return fmt.Errorf("%q is not a valid name: use upper-case letters, digits and underscores, starting with a letter (for example AMADEUS_CLIENT_ID)", name)
	}
	if reservedNames[name] {
		return fmt.Errorf("%q is reserved and cannot be set as a secret", name)
	}
	for _, p := range reservedPrefixes {
		if strings.HasPrefix(name, p) {
			return fmt.Errorf("names starting with %q are reserved for the platform", p)
		}
	}
	return nil
}
