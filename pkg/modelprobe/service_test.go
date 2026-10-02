// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package modelprobe

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/FlashbackAi/teepin-core/pkg/inference"
	"github.com/FlashbackAi/teepin-core/pkg/modelcatalog"
)

type memCatalog struct{ models map[string]modelcatalog.Model }

func (c memCatalog) GetModel(_ context.Context, route string) (*modelcatalog.Model, error) {
	m, ok := c.models[route]
	if !ok {
		return nil, modelcatalog.ErrNotFound
	}
	return &m, nil
}

func (c memCatalog) ListModels(context.Context) ([]modelcatalog.Model, error) {
	var out []modelcatalog.Model
	for _, m := range c.models {
		out = append(out, m)
	}
	return out, nil
}

type fixedProviders struct{ p inference.Provider }

func (f fixedProviders) ProviderFor(context.Context, modelcatalog.Model) (inference.Provider, error) {
	return f.p, nil
}
func (f fixedProviders) BuildProvider(modelcatalog.Model, string) (inference.Provider, error) {
	return f.p, nil
}

type memStore struct {
	mu sync.Mutex
	m  map[string]*Report
}

func (s *memStore) Get(_ context.Context, route string) (*Report, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.m[route]; ok {
		return r, nil
	}
	return nil, ErrNoReport
}

func (s *memStore) All(context.Context) (map[string]*Report, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]*Report{}
	for k, v := range s.m {
		out[k] = v
	}
	return out, nil
}

func (s *memStore) Put(_ context.Context, r *Report) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]*Report{}
	}
	s.m[r.ModelRoute] = MergeReports(s.m[r.ModelRoute], r)
	return nil
}

func (s *memStore) Delete(_ context.Context, route string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, route)
	return nil
}

// described is a fake model that also reports metadata.
type described struct{ *fakeModel }

func (d described) DiscoverMetadata(context.Context) (inference.Metadata, error) {
	return inference.Metadata{ContextWindow: 131072, MaxOutputTokens: 8192, Source: "test"}, nil
}

func newService(p inference.Provider, routes ...string) (*Service, *memStore) {
	cat := memCatalog{models: map[string]modelcatalog.Model{}}
	for _, r := range routes {
		cat.models[r] = modelcatalog.Model{ModelRoute: r, Enabled: true}
	}
	st := &memStore{}
	svc := NewService(cat, fixedProviders{p}, st)
	svc.newRunner = func(p inference.Provider, route string) *Runner { return runner(p) }
	return svc, st
}

func TestService_CheckStoresMetadataAndEveryCapability(t *testing.T) {
	svc, _ := newService(described{&fakeModel{}}, "teepin/test")
	rep, err := svc.Check(context.Background(), "teepin/test")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Metadata == nil || rep.Metadata.ContextWindow != 131072 || rep.Metadata.Source != "test" {
		t.Errorf("metadata = %+v", rep.Metadata)
	}
	for _, c := range AllCapabilities {
		if rep.Check(c).Status != StatusPassed {
			t.Errorf("%s = %s (%s)", c, rep.Check(c).Status, rep.Check(c).Detail)
		}
	}
}

func TestService_ManyToolsIsSkippedWhenPlainToolsFail(t *testing.T) {
	f := &fakeModel{noTools: true}
	svc, _ := newService(f, "teepin/test")
	rep, err := svc.Check(context.Background(), "teepin/test")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Check(CapTools).Status != StatusFailed {
		t.Errorf("tools = %s", rep.Check(CapTools).Status)
	}
	if rep.Check(CapToolsMany).Status != StatusUntested {
		t.Errorf("the many-tools test must not run for a model that cannot call a tool: %s", rep.Check(CapToolsMany).Status)
	}
}

func TestService_ProviderWithoutMetadataSaysSo(t *testing.T) {
	svc, _ := newService(&fakeModel{}, "teepin/test")
	rep, _ := svc.Check(context.Background(), "teepin/test", CapVision)
	if len(rep.Checks) != 1 || rep.Checks[0].Capability != CapVision {
		t.Errorf("only the requested capability should run: %+v", rep.Checks)
	}
	if rep.Metadata != nil {
		t.Errorf("a partial re-check does not re-read metadata: %+v", rep.Metadata)
	}
}

func TestService_OnlyOneCheckPerModelAtATime(t *testing.T) {
	svc, _ := newService(&fakeModel{}, "teepin/test")
	if !svc.begin("teepin/test") {
		t.Fatal("first begin refused")
	}
	if _, err := svc.Check(context.Background(), "teepin/test"); !errors.Is(err, ErrCheckRunning) {
		t.Errorf("err = %v, want ErrCheckRunning", err)
	}
	svc.end("teepin/test")
}

func TestService_CheckMissingSkipsModelsAlreadyChecked(t *testing.T) {
	f := &fakeModel{}
	svc, st := newService(f, "teepin/a", "teepin/b")
	_ = st.Put(context.Background(), &Report{ModelRoute: "teepin/a", Checks: []Check{{Capability: CapTools, Status: StatusPassed}}})
	before := f.calls
	svc.CheckMissing(context.Background())
	if _, err := st.Get(context.Background(), "teepin/b"); err != nil {
		t.Errorf("the unchecked model was not checked: %v", err)
	}
	if rep, _ := st.Get(context.Background(), "teepin/a"); rep.Check(CapVision).Status != StatusUntested {
		t.Errorf("an already-checked model must be left alone: %+v", rep.Checks)
	}
	if f.calls == before {
		t.Error("nothing was probed")
	}
}

func TestService_AnOutageDuringACheckDoesNotEraseEarlierEvidence(t *testing.T) {
	f := &fakeModel{}
	svc, st := newService(f, "teepin/test")
	if _, err := svc.Check(context.Background(), "teepin/test", CapTools); err != nil {
		t.Fatal(err)
	}
	f.outage = errors.New("503")
	rep, err := svc.Check(context.Background(), "teepin/test", CapTools)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Check(CapTools).Status != StatusPassed {
		t.Errorf("a backend outage replaced a verified result with %s", rep.Check(CapTools).Status)
	}
	_ = st
}
