// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package inferencegateway

import (
	"net/http"
	"testing"
)

func TestTunnelEndpoint_RoundTrip(t *testing.T) {
	ep := TunnelEndpoint("provider-mac", "infsvc-abc", 8000)
	prov, inst, port, ok := ParseTunnelEndpoint(ep)
	if !ok || prov != "provider-mac" || inst != "infsvc-abc" || port != 8000 {
		t.Fatalf("round trip = (%q,%q,%d,%v) from %q", prov, inst, port, ok, ep)
	}
}

func TestParseTunnelEndpoint_RejectsNonTunnel(t *testing.T) {
	for _, ep := range []string{"http://10.0.0.1:8000", "tunnel://", "tunnel://p/i", "tunnel://p/:80", "tunnel://p/i:x", ""} {
		if _, _, _, ok := ParseTunnelEndpoint(ep); ok {
			t.Errorf("%q parsed as a tunnel endpoint", ep)
		}
	}
}

func TestDefaultProviderFactory_TunnelNotBuiltYet(t *testing.T) {
	_, err := defaultProviderFactory(ModelServiceConfig{Engine: "mlx"}, TunnelEndpoint("p", "i", 8000))
	if err == nil {
		t.Fatal("tunnel endpoint accepted; the transport is not built yet")
	}
}

func TestBackendModelName(t *testing.T) {
	cases := []struct {
		cfg  ModelServiceConfig
		want string
	}{
		{ModelServiceConfig{BackendModel: "custom", ModelSource: "https://huggingface.co/a/b"}, "custom"},
		{ModelServiceConfig{ModelSource: "https://huggingface.co/DreamFoundries/K2-Horizon/tree/main"}, "DreamFoundries/K2-Horizon"},
		{ModelServiceConfig{ModelSource: "https://example.com/m.tar.gz"}, ""},
		{ModelServiceConfig{ModelSource: "https://huggingface.co/"}, ""},
	}
	for _, c := range cases {
		if got := backendModelName(c.cfg); got != c.want {
			t.Errorf("backendModelName(%+v) = %q, want %q", c.cfg, got, c.want)
		}
	}
}

func TestGatewayDefaultFactory_TunnelUsesDialer(t *testing.T) {
	g := New(nil, nil, nil)
	cfg := ModelServiceConfig{Engine: "mlx", ModelSource: "https://huggingface.co/a/b"}
	ep := TunnelEndpoint("prov", "infsvc-1", 8000)

	if _, err := g.defaultFactory(cfg, ep); err == nil {
		t.Fatal("tunnel endpoint accepted with no dialer configured")
	}

	var gotProv, gotInst string
	var gotPort int
	g.SetTunnelDialer(func(p, i string, port int) http.RoundTripper {
		gotProv, gotInst, gotPort = p, i, port
		return http.DefaultTransport
	})
	if _, err := g.defaultFactory(cfg, ep); err != nil {
		t.Fatalf("defaultFactory with dialer: %v", err)
	}
	if gotProv != "prov" || gotInst != "infsvc-1" || gotPort != 8000 {
		t.Errorf("dialer got (%q,%q,%d)", gotProv, gotInst, gotPort)
	}
}
