// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build !windows && !darwin

package main

import "fmt"

// probeHost on any platform other than Windows/macOS: this binary is only
// ever meant to run on a home node's HOST OS before its Linux guest exists
// (see main.go's own doc comment) — a native Linux home node needs no
// guest and no probe at all, so this stub exists only to keep `go build
// ./...` clean on Linux CI, never to actually be invoked there.
func probeHost() (probeResult, error) {
	return probeResult{}, fmt.Errorf("teepin-hostprobe is only implemented for windows and darwin (this platform runs teepin-agent natively and needs no host probe)")
}
