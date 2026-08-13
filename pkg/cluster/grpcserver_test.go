// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"context"
	"errors"
	"testing"
)

// fakeNodeAuth resolves exactly one credential to one identity.
type fakeNodeAuth struct {
	credential string
	identity   *NodeIdentity
}

func (f *fakeNodeAuth) AuthenticateNode(_ context.Context, cred string) (*NodeIdentity, error) {
	if cred == f.credential {
		return f.identity, nil
	}
	return nil, errors.New("invalid")
}

// The shared datacenter token authenticates and yields NO node identity —
// the existing path, unchanged, regardless of whether a node authenticator
// is present.
func TestResolveCredential_SharedTokenUnchanged(t *testing.T) {
	for _, withNodeAuth := range []bool{false, true} {
		s := NewAgentServer(nil, nil, "shared-secret")
		if withNodeAuth {
			s = s.WithNodeAuthenticator(&fakeNodeAuth{credential: "tnc_x", identity: &NodeIdentity{}})
		}
		id, err := s.resolveCredential(context.Background(), "shared-secret")
		if err != nil {
			t.Fatalf("withNodeAuth=%v: shared token rejected: %v", withNodeAuth, err)
		}
		if id != nil {
			t.Errorf("withNodeAuth=%v: shared token yielded an identity; want nil (datacenter path)", withNodeAuth)
		}
	}
}

// A per-node credential resolves to its identity — including the class, which
// the caller then trusts over the RegisterRequest.
func TestResolveCredential_PerNodeIdentity(t *testing.T) {
	want := &NodeIdentity{NodeName: "mac-mini", ProviderID: "home-sreek", Class: "home"}
	s := NewAgentServer(nil, nil, "shared-secret").
		WithNodeAuthenticator(&fakeNodeAuth{credential: "tnc_valid", identity: want})

	id, err := s.resolveCredential(context.Background(), "tnc_valid")
	if err != nil {
		t.Fatalf("valid per-node credential rejected: %v", err)
	}
	if id == nil || id.Class != "home" || id.NodeName != "mac-mini" {
		t.Fatalf("resolved identity = %+v, want %+v", id, want)
	}
}

// A credential that is neither the shared token nor a known node is rejected.
func TestResolveCredential_UnknownRejected(t *testing.T) {
	s := NewAgentServer(nil, nil, "shared-secret").
		WithNodeAuthenticator(&fakeNodeAuth{credential: "tnc_valid", identity: &NodeIdentity{}})

	if _, err := s.resolveCredential(context.Background(), "tnc_bogus"); err == nil {
		t.Fatal("unknown credential accepted")
	}
}

// With no shared token and no node authenticator, everything is refused —
// the channel never runs open.
func TestResolveCredential_NoAuthConfiguredRefuses(t *testing.T) {
	s := NewAgentServer(nil, nil, "")
	if _, err := s.resolveCredential(context.Background(), "anything"); err == nil {
		t.Fatal("credential accepted with no auth configured")
	}
}
