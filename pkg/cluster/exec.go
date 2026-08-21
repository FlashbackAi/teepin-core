// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"context"
	"io"
)

// ExecCapable is implemented by cluster clients that can attach an
// interactive session to a running instance's container. A type
// assertion rather than a Client method: Client is documented as "the
// control plane's view of GPU capacity" (client.go), and CPUOnly /
// Unavailable genuinely cannot exec into anything — a stub method that
// always errors is worse than the method's absence, and repeats the wart
// already visible on ResolveInstanceAddress.
type ExecCapable interface {
	ExecAttach(ctx context.Context, req ExecRequest, io ExecIO) (ExecOutcome, error)
}

// ExecRequest names what to attach to. Container empty means "the pod's
// first/only container" — resolved against the live pod spec, not a
// hardcoded name, since a customer may pick a specific container.
type ExecRequest struct {
	InstanceID string
	Container  string
	Command    []string // empty = probe for a shell (see isMissingShellError)
	TTY        bool
	Rows, Cols uint16
}

// ExecIO carries the byte streams for one session. Stderr MUST be nil
// when TTY is true — the Kubernetes API server rejects a stderr stream
// alongside a tty, and ExecAttach enforces this rather than trusting the
// caller. Resize is nil when TTY is false.
//
// OnOpen fires exactly once, synchronously, the moment the pod/container
// is resolved and BEFORE any stdin/stdout streaming begins — this is the
// only way the caller can send an ExecOpen ahead of output, since
// ExecAttach itself blocks for the whole session and only returns at the
// end. A quiet shell prints nothing, so without this signal there is no
// way to distinguish "attached and idle" from "still connecting".
type ExecIO struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Resize <-chan TerminalSize
	OnOpen func(podName, container string)
}

// TerminalSize is a customer-driven resize event.
type TerminalSize struct {
	Rows, Cols uint16
}

// ExecOutcome is ExecAttach's final result, once the session has ended.
type ExecOutcome struct {
	ExitCode int
}
