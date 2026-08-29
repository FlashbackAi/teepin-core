// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Package imageinfo reads a container image's declared ports (its EXPOSE
// directive, OCI Config.ExposedPorts) so the create-instance form can
// default the "Port" field instead of forcing every customer to already
// know what their image listens on.
package imageinfo

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// resolveTimeout bounds one manifest fetch. This runs synchronously
// inside an HTTP handler a customer's browser is waiting on, so it must
// fail fast rather than hang on a slow or unreachable registry.
const resolveTimeout = 5 * time.Second

// allowedRegistries is the SSRF guard for this feature.
//
// name.ParseReference accepts an arbitrary hostname, and remote.Image
// then makes an outbound HTTP request to whatever host that resolves to.
// Without this allowlist, an authenticated customer could put an internal
// or link-local address (e.g. the AWS instance metadata endpoint) in the
// "image" field of a create-instance request and have the control plane
// issue a request to it on their behalf — a real SSRF vector, not a
// theoretical one, given this runs on ECS with an IAM task role attached.
//
// A registry not on this list is not an error: resolution simply returns
// no ports, and the customer falls back to typing the port manually —
// exactly today's behaviour before this feature existed. Nothing about
// instance CREATION is restricted to these registries; only the optional
// port-auto-detect convenience is.
var allowedRegistries = map[string]bool{
	"index.docker.io":      true, // name.DefaultRegistry — what "nginx" parses to
	"registry-1.docker.io": true, // Docker Hub's actual pull endpoint
	"ghcr.io":              true,
	"quay.io":              true,
	"gcr.io":               true,
	"public.ecr.aws":       true,
	"registry.k8s.io":      true,
}

// PortInfo is one port an image's manifest declares it listens on.
type PortInfo struct {
	Port     int
	Protocol string // "tcp" or "udp"
}

// ResolvePorts returns the ports imageRef's manifest declares via EXPOSE,
// sorted for a deterministic response.
//
// An empty slice with a nil error is the normal outcome for the common
// cases: an image on a registry outside allowedRegistries, an image with
// no ExposedPorts in its config (many document the port only in a
// README), or a private image this call has no credentials for. The
// caller's job is to treat "no ports found" as "let the customer type
// one" — never as a failure blocking instance creation, since this is a
// convenience default, not a requirement.
//
// A non-nil error means the reference itself could not be parsed at all
// (a genuinely malformed image string), which the caller may want to
// surface differently.
func ResolvePorts(ctx context.Context, imageRef string) ([]PortInfo, error) {
	return resolvePorts(ctx, imageRef, allowedRegistries)
}

// resolvePorts is ResolvePorts with an injectable allowlist, so tests can
// point at a local fake registry without weakening the real one:
// production always calls this through ResolvePorts with the real
// allowedRegistries, never with a caller-supplied list.
func resolvePorts(ctx context.Context, imageRef string, allowed map[string]bool) ([]PortInfo, error) {
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return nil, fmt.Errorf("invalid image reference: %w", err)
	}
	if !allowed[ref.Context().RegistryStr()] {
		return nil, nil
	}
	return resolveConfigPorts(ctx, ref, nil)
}

// ResolvePortsWithAuth reads imageRef's manifest exactly like
// ResolvePorts, but authenticates the pull with the given credential and
// does NOT apply allowedRegistries. Only for a caller passing an image
// reference IT constructed itself — never customer-supplied input: the
// SSRF guard ResolvePorts enforces (see allowedRegistries' own doc
// comment) exists specifically because a customer can put an arbitrary
// host in a create-instance request; DeployKumbhaSession's imageRef, by
// contrast, is built entirely server-side (the Kumbha build registry's
// own resolved prefix + a session tag), so there is nothing here for a
// customer to redirect.
//
// Used to auto-populate a deploy's ports from the image TEEPIN ITSELF
// just built and pushed — found live 2026-08-26: a customer correctly
// pointed out that requiring the agent to separately remember and pass
// `ports` on every deploy, when the built image's own manifest already
// declares it (via the Dockerfile's EXPOSE, inherited through FROM same
// as `docker build` itself resolves it), is not something the customer
// or the model should have to get right by hand.
func ResolvePortsWithAuth(ctx context.Context, imageRef, username, password string) ([]PortInfo, error) {
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return nil, fmt.Errorf("invalid image reference: %w", err)
	}
	return resolveConfigPorts(ctx, ref, &authn.Basic{Username: username, Password: password})
}

// resolveConfigPorts is the shared manifest fetch + ExposedPorts parse
// behind both ResolvePorts and ResolvePortsWithAuth. auth nil means an
// anonymous pull (the public-registry path); non-nil authenticates the
// request.
func resolveConfigPorts(ctx context.Context, ref name.Reference, auth authn.Authenticator) ([]PortInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()

	opts := []remote.Option{remote.WithContext(ctx)}
	if auth != nil {
		opts = append(opts, remote.WithAuth(auth))
	}

	img, err := remote.Image(ref, opts...)
	if err != nil {
		// Registry unreachable, image not found, or private without
		// credentials — all normal, expected outcomes for an arbitrary
		// customer-typed image string, not a caller-visible error.
		return nil, nil
	}

	cfg, err := img.ConfigFile()
	if err != nil {
		return nil, nil
	}

	ports := make([]PortInfo, 0, len(cfg.Config.ExposedPorts))
	for spec := range cfg.Config.ExposedPorts {
		port, protocol, ok := parsePortSpec(spec)
		if !ok {
			continue
		}
		ports = append(ports, PortInfo{Port: port, Protocol: protocol})
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i].Port < ports[j].Port })
	return ports, nil
}

// parsePortSpec parses one OCI ExposedPorts key, e.g. "80/tcp" or "8080"
// (protocol defaults to tcp when omitted, matching the OCI image spec).
func parsePortSpec(spec string) (port int, protocol string, ok bool) {
	protocol = "tcp"
	portStr := spec
	if idx := strings.IndexByte(spec, '/'); idx != -1 {
		portStr = spec[:idx]
		protocol = strings.ToLower(spec[idx+1:])
	}
	p, err := strconv.Atoi(portStr)
	if err != nil || p < 1 || p > 65535 {
		return 0, "", false
	}
	return p, protocol, true
}
