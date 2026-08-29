// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package imageinfo

import (
	"context"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// newTestRegistry starts an in-memory OCI registry (the same test double
// go-containerregistry's own suite uses) and returns its host:port, so
// tests exercise the real remote-fetch code path rather than mocking it
// away.
func newTestRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test registry URL: %v", err)
	}
	return u.Host
}

// pushImage builds a minimal image with the given ExposedPorts and pushes
// it to the test registry under repo.
func pushImage(t *testing.T, registryHost, repo string, exposedPorts map[string]struct{}) {
	t.Helper()
	base, err := random.Image(64, 1)
	if err != nil {
		t.Fatalf("build base image: %v", err)
	}
	img, err := mutate.Config(base, v1.Config{ExposedPorts: exposedPorts})
	if err != nil {
		t.Fatalf("set image config: %v", err)
	}
	ref, err := name.ParseReference(registryHost + "/" + repo)
	if err != nil {
		t.Fatalf("parse push reference: %v", err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("push test image: %v", err)
	}
}

func TestResolvePorts_ReturnsDeclaredPortsSorted(t *testing.T) {
	host := newTestRegistry(t)
	pushImage(t, host, "myapp:v1", map[string]struct{}{
		"443/tcp": {},
		"80/tcp":  {},
		"53/udp":  {},
	})

	ports, err := resolvePorts(context.Background(), host+"/myapp:v1", map[string]bool{host: true})
	if err != nil {
		t.Fatalf("resolvePorts: %v", err)
	}
	if len(ports) != 3 {
		t.Fatalf("got %d ports, want 3: %+v", len(ports), ports)
	}
	// Sorted by port number.
	want := []PortInfo{{53, "udp"}, {80, "tcp"}, {443, "tcp"}}
	for i, p := range ports {
		if p != want[i] {
			t.Errorf("ports[%d] = %+v, want %+v", i, p, want[i])
		}
	}
}

func TestResolvePorts_NoExposedPortsIsEmptyNotError(t *testing.T) {
	host := newTestRegistry(t)
	pushImage(t, host, "noexpose:v1", nil)

	ports, err := resolvePorts(context.Background(), host+"/noexpose:v1", map[string]bool{host: true})
	if err != nil {
		t.Fatalf("resolvePorts: %v", err)
	}
	if len(ports) != 0 {
		t.Errorf("got %d ports, want 0 for an image with no ExposedPorts: %+v", len(ports), ports)
	}
}

func TestResolvePorts_RegistryNotAllowlistedReturnsEmpty(t *testing.T) {
	host := newTestRegistry(t)
	pushImage(t, host, "myapp:v1", map[string]struct{}{"80/tcp": {}})

	// Same real, reachable registry — but NOT in the allowlist passed in.
	// Must return quietly, not an error, and (per the next test) without
	// ever attempting the network call.
	ports, err := resolvePorts(context.Background(), host+"/myapp:v1", map[string]bool{"other-registry.example.com": true})
	if err != nil {
		t.Fatalf("resolvePorts: %v", err)
	}
	if ports != nil {
		t.Errorf("got %+v, want nil for a non-allowlisted registry", ports)
	}
}

// TestResolvePorts_SSRFGuard is the regression test for the actual
// security property allowedRegistries exists to provide: an image
// reference pointing at an arbitrary, non-allowlisted host must never
// cause an outbound network attempt at all — not "attempt and fail", not
// "attempt and time out" — the registry check must short-circuit BEFORE
// any dial happens. Proven here by pointing at a host that would hang if
// actually dialed (a non-routable TEST-NET-1 address, RFC 5737) and
// asserting the call returns near-instantly, well under resolveTimeout.
func TestResolvePorts_SSRFGuard(t *testing.T) {
	start := time.Now()
	ports, err := ResolvePorts(context.Background(), "192.0.2.1:5000/internal-service:latest")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("resolvePorts: %v", err)
	}
	if ports != nil {
		t.Errorf("got %+v, want nil for a non-allowlisted, unroutable host", ports)
	}
	if elapsed > 1*time.Second {
		t.Errorf("took %v to reject a non-allowlisted host — the allowlist check must short-circuit BEFORE any network attempt, not after a timeout (this would indicate the SSRF guard is not actually preventing the outbound request)", elapsed)
	}
}

func TestResolvePorts_InvalidReferenceIsError(t *testing.T) {
	_, err := ResolvePorts(context.Background(), "not a valid image ref !!!")
	if err == nil {
		t.Error("expected an error for a malformed image reference")
	}
}

func TestResolvePorts_MissingImageOnAllowlistedRegistryReturnsEmpty(t *testing.T) {
	// A registry that IS on the allowlist but the specific image was
	// never pushed there (404) must degrade to "no ports found", not
	// fail the caller's create-instance flow — this is a convenience
	// default, never a hard dependency.
	host := newTestRegistry(t)

	ports, err := resolvePorts(context.Background(), host+"/never-pushed:latest", map[string]bool{host: true})
	if err != nil {
		t.Fatalf("resolvePorts should not error on a missing image, got: %v", err)
	}
	if ports != nil {
		t.Errorf("got %+v, want nil", ports)
	}
}

// TestResolvePortsWithAuth_BypassesAllowlist is the regression test for
// the actual point of this function: DeployKumbhaSession's own image
// reference (TEEPIN's private build registry, e.g. ECR) is never on
// allowedRegistries — that allowlist exists for arbitrary customer-typed
// image strings, a threat model that does not apply to a reference the
// control plane constructed itself. Same real, reachable test registry
// TestResolvePorts_RegistryNotAllowlistedReturnsEmpty proves gets
// REJECTED by the plain ResolvePorts path — here it must succeed.
func TestResolvePortsWithAuth_BypassesAllowlist(t *testing.T) {
	host := newTestRegistry(t)
	pushImage(t, host, "myapp:v1", map[string]struct{}{"80/tcp": {}})

	ports, err := ResolvePortsWithAuth(context.Background(), host+"/myapp:v1", "user", "pass")
	if err != nil {
		t.Fatalf("ResolvePortsWithAuth: %v", err)
	}
	if len(ports) != 1 || ports[0] != (PortInfo{80, "tcp"}) {
		t.Errorf("got %+v, want [{80 tcp}]", ports)
	}
}

func TestResolvePortsWithAuth_InvalidReferenceIsError(t *testing.T) {
	_, err := ResolvePortsWithAuth(context.Background(), "not a valid image ref !!!", "user", "pass")
	if err == nil {
		t.Error("expected an error for a malformed image reference")
	}
}

func TestParsePortSpec(t *testing.T) {
	cases := []struct {
		spec         string
		wantPort     int
		wantProtocol string
		wantOK       bool
	}{
		{"80/tcp", 80, "tcp", true},
		{"53/udp", 53, "udp", true},
		{"8080", 8080, "tcp", true}, // protocol omitted -> defaults to tcp
		{"80/TCP", 80, "tcp", true}, // protocol lowercased
		{"not-a-number/tcp", 0, "", false},
		{"0/tcp", 0, "", false},     // out of range
		{"70000/tcp", 0, "", false}, // out of range
		{"-5/tcp", 0, "", false},    // out of range
	}
	for _, c := range cases {
		port, protocol, ok := parsePortSpec(c.spec)
		if ok != c.wantOK {
			t.Errorf("parsePortSpec(%q) ok = %v, want %v", c.spec, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if port != c.wantPort || protocol != c.wantProtocol {
			t.Errorf("parsePortSpec(%q) = (%d, %q), want (%d, %q)", c.spec, port, protocol, c.wantPort, c.wantProtocol)
		}
	}
}
