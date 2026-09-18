// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build !darwin

package main

// On every platform except macOS the agent runs in (or as) Linux, where
// /proc and /sys already answer these — see enroll.go. These stubs exist
// only so the darwin-specific detection has a same-named counterpart.
func nativeMemoryGB() int { return 0 }

func nativePECores() (pCores, eCores int) { return 0, 0 }
