// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package main

import (
	"log"
	"os"
	"strings"
)

// Teepin Build was called Kumbha, and its settings were TEEPIN_KUMBHA_*. They
// are now TEEPIN_BUILD_*. Until every environment's task definition carries the
// new names (one tofu apply), a deployment that still sets only the old name
// keeps working: the old value is used for the new name. A setting given under
// its new name always wins. Remove after the next apply of every environment
// (ROADMAP: Teepin Build rename).
var legacyBuildEnvRenames = map[string]string{
	"TEEPIN_KUMBHA_BUILD_IMAGE_PULL_SECRET": "TEEPIN_BUILD_APP_IMAGE_PULL_SECRET",
}

func adoptLegacyBuildEnv(environ []string, lookup func(string) (string, bool), set func(string, string) error) []string {
	var adopted []string
	for _, kv := range environ {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, "TEEPIN_KUMBHA_") {
			continue
		}
		target, renamed := legacyBuildEnvRenames[name]
		if !renamed {
			target = "TEEPIN_BUILD_" + strings.TrimPrefix(name, "TEEPIN_KUMBHA_")
		}
		if _, exists := lookup(target); exists {
			continue
		}
		if err := set(target, value); err == nil {
			adopted = append(adopted, name)
		}
	}
	return adopted
}

func init() {
	if adopted := adoptLegacyBuildEnv(os.Environ(), os.LookupEnv, os.Setenv); len(adopted) > 0 {
		// Names only, never values: some of these hold image pull secrets.
		log.Printf("WARN: settings still use the former TEEPIN_KUMBHA_ names and were adopted as TEEPIN_BUILD_: %s", strings.Join(adopted, ", "))
	}
}
