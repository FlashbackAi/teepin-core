// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build !darwin

package main

// On every platform except macOS the agent runs in (or as) Linux, where
// /proc already answers this — see enroll.go. This stub exists only so the
// darwin-specific detection has a same-named counterpart.
func nativeMemoryGB() int { return 0 }
