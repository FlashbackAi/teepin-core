// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package nativeruntime

import (
	"io"
	"os"
	"strings"
)

// tailOf returns up to the last n lines of f (all of what fits in
// logTailLimitBytes when n <= 0) and leaves f's offset at end-of-file, so a
// caller that wants to follow can keep reading from exactly where the tail
// stopped. Only the last logTailLimitBytes are ever read: a model server
// can log for days, and a tail must not slurp a multi-gigabyte file.
func tailOf(f *os.File, n int) string {
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	start := info.Size() - logTailLimitBytes
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	text := string(data)
	if start > 0 {
		// Drop the (almost certainly partial) first line of a truncated read.
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
	}
	if n <= 0 {
		return text
	}
	lines := strings.SplitAfter(text, "\n")
	// SplitAfter yields a trailing "" when text ends in a newline.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "")
}

// lastLines is a one-line-per-entry summary of the log's last n lines,
// used as a failure reason. Best-effort: an unreadable log yields "".
func lastLines(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	tail := strings.TrimSpace(tailOf(f, n))
	tail = strings.Join(strings.Fields(strings.ReplaceAll(tail, "\n", " | ")), " ")
	const maxReason = 400
	if len(tail) > maxReason {
		tail = tail[len(tail)-maxReason:]
	}
	return tail
}
