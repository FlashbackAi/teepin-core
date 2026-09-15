// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build windows

package main

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// relationProcessorCore is LOGICAL_PROCESSOR_RELATIONSHIP's
// RelationProcessorCore value (0) — selects per-core entries from
// GetLogicalProcessorInformationEx rather than per-package/per-cache ones.
const relationProcessorCore = 0

// systemLogicalProcessorInformationExHeader mirrors the fixed-size header
// of Win32's SYSTEM_LOGICAL_PROCESSOR_INFORMATION_EX: Relationship selects
// which union variant follows, and Size is how far to advance to the NEXT
// entry in the buffer (entries are variable-length, never sizeof(this
// struct) — advancing by anything else corrupts the walk).
type systemLogicalProcessorInformationExHeader struct {
	Relationship uint32
	Size         uint32
}

// processorRelationshipEfficiencyClassOffset is PROCESSOR_RELATIONSHIP's
// EfficiencyClass field offset within a SYSTEM_LOGICAL_PROCESSOR_INFORMATION_EX
// entry: an 8-byte header (Relationship, Size) is immediately followed by
// PROCESSOR_RELATIONSHIP{ Flags BYTE; EfficiencyClass BYTE; ... }, putting
// EfficiencyClass at offset 8+1.
const processorRelationshipEfficiencyClassOffset = 9

var (
	kernel32                             = windows.NewLazySystemDLL("kernel32.dll")
	procGetLogicalProcessorInformationEx = kernel32.NewProc("GetLogicalProcessorInformationEx")
	procGlobalMemoryStatusEx             = kernel32.NewProc("GlobalMemoryStatusEx")
)

// memoryStatusEx mirrors Win32's MEMORYSTATUSEX. golang.org/x/sys/windows
// (the version this repo pins) does not wrap GlobalMemoryStatusEx itself,
// so this is called directly via kernel32, same approach as
// GetLogicalProcessorInformationEx above. Length must be set to
// sizeof(memoryStatusEx) before the call — the documented Win32 contract.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// probeHost detects P-core/E-core counts via
// GetLogicalProcessorInformationEx(RelationProcessorCore), reading each
// core's EfficiencyClass byte — the same hybrid-CPU signal Windows 11's own
// scheduler (Intel Thread Director / AMD equivalent) uses. A homogeneous
// CPU reports EfficiencyClass 0 for every core, indistinguishable from "no
// hybrid info at all" — collapsed to (0, 0), "no split detected", rather
// than claiming every core is an E-core.
func probeHost() (probeResult, error) {
	pCores, eCores, err := detectCoreSplit()
	if err != nil {
		// Detection failing must not sink the whole probe — memory is still
		// useful on its own, and (0, 0) is the correct "unknown" signal on
		// the caller side, same convention as everywhere else P/E detection
		// can fail in this feature.
		pCores, eCores = 0, 0
	}

	memGB, memErr := totalMemoryGB()
	if memErr != nil {
		memGB = 0
	}

	if err != nil && memErr != nil {
		return probeResult{}, fmt.Errorf("both core-split and memory detection failed: cores=%v mem=%v", err, memErr)
	}
	return probeResult{PCores: pCores, ECores: eCores, TotalMemoryGB: memGB}, nil
}

func detectCoreSplit() (pCores, eCores int, err error) {
	var size uint32
	// First call with a nil buffer to learn the required size — the
	// documented Win32 pattern for GetLogicalProcessorInformationEx.
	r, _, callErr := procGetLogicalProcessorInformationEx.Call(
		uintptr(relationProcessorCore),
		0,
		uintptr(unsafe.Pointer(&size)),
	)
	if r != 0 {
		return 0, 0, fmt.Errorf("size probe unexpectedly succeeded")
	}
	if callErr != windows.ERROR_INSUFFICIENT_BUFFER {
		return 0, 0, fmt.Errorf("size probe failed: %w", callErr)
	}
	if size == 0 {
		return 0, 0, fmt.Errorf("reported zero buffer size")
	}

	buf := make([]byte, size)
	r, _, callErr = procGetLogicalProcessorInformationEx.Call(
		uintptr(relationProcessorCore),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	if r == 0 {
		return 0, 0, fmt.Errorf("call failed: %w", callErr)
	}

	for offset := uint32(0); offset < size; {
		if offset+8 > size {
			break // truncated entry; stop rather than read out of bounds
		}
		entry := (*systemLogicalProcessorInformationExHeader)(unsafe.Pointer(&buf[offset]))
		if entry.Size == 0 || offset+entry.Size > size {
			break // malformed; avoid an infinite or out-of-bounds walk
		}
		if entry.Relationship == relationProcessorCore && offset+processorRelationshipEfficiencyClassOffset < size {
			if buf[offset+processorRelationshipEfficiencyClassOffset] > 0 {
				pCores++
			} else {
				eCores++
			}
		}
		offset += entry.Size
	}

	// EfficiencyClass 0 on every core is indistinguishable from "this CPU
	// reports no hybrid info at all" (the common, homogeneous case) — treat
	// it as "no split detected" rather than claiming every core is an
	// E-core, which would hand placement/pricing a fabricated signal.
	if pCores == 0 {
		eCores = 0
	}
	return pCores, eCores, nil
}

func totalMemoryGB() (int, error) {
	var status memoryStatusEx
	status.Length = uint32(unsafe.Sizeof(status))
	r, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&status)))
	if r == 0 {
		return 0, fmt.Errorf("GlobalMemoryStatusEx failed: %w", err)
	}
	const gib = 1024 * 1024 * 1024
	return int((status.TotalPhys + gib/2) / gib), nil
}
