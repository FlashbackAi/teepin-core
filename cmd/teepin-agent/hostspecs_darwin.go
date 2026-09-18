// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build darwin

package main

import "golang.org/x/sys/unix"

// nativeMemoryGB reads physical memory via sysctl — the agent normally
// runs inside a Linux guest and reads /proc/meminfo, but in native mode
// (TEEPIN_RUNTIME=native, see nativeruntime) it runs directly on macOS,
// where that file does not exist and detection would silently report 0.
func nativeMemoryGB() int {
	bytes, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0
	}
	const gib = 1024 * 1024 * 1024
	return int((bytes + gib/2) / gib)
}

// nativePECores uses the same sysctl names as cmd/teepin-hostprobe:
// hw.perflevel0 is the performance cluster, hw.perflevel1 the efficiency
// cluster. Missing perflevel0 (an Intel Mac) means "no split detected".
func nativePECores() (pCores, eCores int) {
	p, err := unix.SysctlUint32("hw.perflevel0.physicalcpu")
	if err != nil {
		return 0, 0
	}
	e, err := unix.SysctlUint32("hw.perflevel1.physicalcpu")
	if err != nil {
		return int(p), 0
	}
	return int(p), int(e)
}
