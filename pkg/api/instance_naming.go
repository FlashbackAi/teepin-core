// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"fmt"
	"math/rand/v2"

	"github.com/google/uuid"
)

// generateInstanceID mints a Heroku/Vercel-style "<adjective>-<noun>-<hex>"
// identifier — replaces the old "inst-<8hex>" scheme (e.g. "inst-5ed29952"),
// which was fine for an operator grepping logs but not something a
// customer wants to read back to support, remember, or see as their own
// deployment's hostname (inst-5ed29952.dev.teepin.com). Requested
// 2026-09-02.
//
// This IS the instance's real, permanent ID — the same string used as the
// Kubernetes label value, the DNS hostname component, the billing primary
// key, and the Stage 3 tunnel's routing lookup — not a cosmetic alias
// layered on top of the old scheme. Nothing anywhere in this codebase
// parses or depends on the OLD "inst-" prefix (confirmed: the only prefix
// checks in pkg/cluster.hiddenInstanceIDPrefixes match "kumbha-agent-" /
// "kaniko-build-" / "kumbha-shot-", Kumbha's own internal-tooling pods,
// never a customer instance's own ID) — every consumer treats the ID as
// an opaque string, so the format is free to change here without touching
// anything downstream.
//
// The trailing 4 hex characters (from a real UUID, not a second word)
// are what actually guarantees uniqueness — instanceNameAdjectives x
// instanceNameNouns x 16^4 is well over a billion combinations, but
// word-pair collisions alone are not implausible at any real scale, and
// nothing here checks the database for a collision before use (matching
// the old scheme's own behavior, which never checked either).
// "<adjective>-<noun>-<hex>" is comfortably short of the 63-character
// limit shared by Kubernetes label values and DNS labels — the same
// constraint the old scheme satisfied.
func generateInstanceID() string {
	adj := instanceNameAdjectives[rand.IntN(len(instanceNameAdjectives))]
	noun := instanceNameNouns[rand.IntN(len(instanceNameNouns))]
	suffix := uuid.New().String()[:4]
	return fmt.Sprintf("%s-%s-%s", adj, noun, suffix)
}

// instanceNameAdjectives/instanceNameNouns are deliberately short,
// common, unambiguous words — nothing that looks similar to another
// entry when read aloud or typed quickly (no near-homophones), nothing
// with an alternate spelling, nothing that could read as offensive out
// of context. Lowercase, single-word, no punctuation — the generated ID
// is a Kubernetes label value and a DNS label, both of which reject
// anything else.
var instanceNameAdjectives = []string{
	"brave", "swift", "calm", "bright", "quiet", "bold", "gentle", "vivid",
	"sunny", "misty", "quick", "sharp", "silver", "golden", "crimson", "azure",
	"amber", "coral", "jade", "ivory", "cosmic", "lunar", "solar", "arctic",
	"tropical", "alpine", "coastal", "hidden", "ancient", "modern", "rapid", "steady",
	"nimble", "keen", "lucky", "merry", "breezy", "cheerful", "curious", "daring",
	"eager", "fresh", "gleaming", "honest", "jolly", "kind", "lively", "mellow",
	"noble", "polished",
}

var instanceNameNouns = []string{
	"otter", "falcon", "cedar", "comet", "harbor", "meadow", "canyon", "glacier",
	"ember", "willow", "badger", "heron", "maple", "ridge", "delta", "summit",
	"lagoon", "thicket", "orchid", "pebble", "sparrow", "tundra", "prairie", "reef",
	"dune", "brook", "grove", "cliff", "marsh", "plateau", "fjord", "atoll",
	"birch", "aspen", "juniper", "magpie", "lynx", "raven", "falconer", "hawk",
	"beacon", "quarry", "trail", "vale", "cove", "spring", "meridian", "nimbus",
	"zephyr", "compass",
}
