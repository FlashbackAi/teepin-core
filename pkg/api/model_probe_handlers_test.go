// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/FlashbackAi/teepin-core/pkg/modelcatalog"
	"github.com/FlashbackAi/teepin-core/pkg/modelprobe"
)

// fakeProber records what the handlers ask of the capability checker.
type fakeProber struct {
	reports     map[string]*modelprobe.Report
	started     []string
	invalidated []string
	running     bool
	discovered  *modelcatalog.Model
}

func (f *fakeProber) CheckAsync(route string) bool { f.started = append(f.started, route); return true }
func (f *fakeProber) IsRunning(string) bool        { return f.running }
func (f *fakeProber) Latest(_ context.Context, route string) (*modelprobe.Report, error) {
	if r, ok := f.reports[route]; ok {
		return r, nil
	}
	return nil, modelprobe.ErrNoReport
}
func (f *fakeProber) Reports(context.Context) (map[string]*modelprobe.Report, error) {
	return f.reports, nil
}
func (f *fakeProber) Discover(_ context.Context, m modelcatalog.Model, _ string) (*modelprobe.MetadataReport, error) {
	f.discovered = &m
	return &modelprobe.MetadataReport{ContextWindow: 65536, Source: "test"}, nil
}
func (f *fakeProber) Invalidate(_ context.Context, route string) error {
	f.invalidated = append(f.invalidated, route)
	delete(f.reports, route)
	return nil
}

func TestBuildCapabilityView_EvidenceBeatsTheDeclaredFlags(t *testing.T) {
	m := modelcatalog.Model{ModelRoute: "teepin/x", SupportsTools: false, SupportsVision: true}
	rep := &modelprobe.Report{Checks: []modelprobe.Check{
		{Capability: modelprobe.CapTools, Status: modelprobe.StatusPassed},
		{Capability: modelprobe.CapVision, Status: modelprobe.StatusFailed},
	}}
	v := buildCapabilityView(m, rep, false)
	if !v.Effective["tools"].Allowed || v.Effective["tools"].Declared || v.Effective["tools"].Basis != "verified" {
		t.Errorf("tools: %+v (undeclared but passed its check: allowed)", v.Effective["tools"])
	}
	if v.Effective["vision"].Allowed || !v.Effective["vision"].Declared {
		t.Errorf("vision: %+v (declared but failed its check: not allowed)", v.Effective["vision"])
	}
	if v.Effective["audio"].Allowed {
		t.Errorf("audio: %+v (never declared, never tested: not allowed)", v.Effective["audio"])
	}
}

func TestAfterRegister_ChecksNewModelsAndChangedBackendsOnly(t *testing.T) {
	saved := modelcatalog.Model{ModelRoute: "teepin/x", Provider: modelcatalog.ProviderOpenAICompatible, ProviderModel: "m", BaseURL: "https://a"}
	checked := &modelprobe.Report{ModelRoute: "teepin/x"}

	cases := []struct {
		name        string
		prev        *modelcatalog.Model
		existing    *modelprobe.Report
		newKey      bool
		wantCheck   bool
		wantDropped bool
	}{
		{"a brand new model is checked", nil, nil, false, true, false},
		{"an edit that changes nothing about the backend is not", &saved, checked, false, false, false},
		{"an edit to a never-checked model checks it", &saved, nil, false, true, false},
		{"a new endpoint drops the old evidence and re-checks", &modelcatalog.Model{Provider: saved.Provider, ProviderModel: "m", BaseURL: "https://other"}, checked, false, true, true},
		{"a different model id re-checks", &modelcatalog.Model{Provider: saved.Provider, ProviderModel: "other", BaseURL: "https://a"}, checked, false, true, true},
		{"a new API key re-checks", &saved, checked, true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeProber{reports: map[string]*modelprobe.Report{}}
			if tc.existing != nil {
				f.reports["teepin/x"] = tc.existing
			}
			h := &ModelCatalogHandler{}
			h.WithProbes(f)
			h.afterRegister(context.Background(), tc.prev, saved, tc.newKey)
			if got := len(f.started) == 1; got != tc.wantCheck {
				t.Errorf("checked = %v, want %v", got, tc.wantCheck)
			}
			if got := len(f.invalidated) == 1; got != tc.wantDropped {
				t.Errorf("old evidence dropped = %v, want %v", got, tc.wantDropped)
			}
		})
	}
}

func TestAfterRegister_NoProberIsANoOp(t *testing.T) {
	h := &ModelCatalogHandler{}
	h.afterRegister(context.Background(), nil, modelcatalog.Model{ModelRoute: "x"}, true) // must not panic
}

func TestCheckModel_StartsACheckAndReturnsAtOnce(t *testing.T) {
	h, mock, done := newModelCatalogHandlerMock(t)
	defer done()
	f := &fakeProber{}
	h.WithProbes(f)
	expectModelRow(mock, "teepin/x", "anthropic", "")

	w := jsonRequest(h.CheckModel, http.MethodPost, "/v1/admin/inference/models/check?model_route=teepin/x", nil, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if len(f.started) != 1 || f.started[0] != "teepin/x" {
		t.Errorf("started = %v", f.started)
	}
}

func TestCheckModel_UnknownModelIs404AndNothingStarts(t *testing.T) {
	h, mock, done := newModelCatalogHandlerMock(t)
	defer done()
	f := &fakeProber{}
	h.WithProbes(f)
	mock.ExpectQuery(`SELECT model_route, display_name, cost_class`).WithArgs("teepin/nope").WillReturnError(sqlNoRows())
	w := jsonRequest(h.CheckModel, http.MethodPost, "/v1/admin/inference/models/check?model_route=teepin/nope", nil, nil)
	if w.Code != http.StatusNotFound || len(f.started) != 0 {
		t.Errorf("status = %d started = %v", w.Code, f.started)
	}
}

func TestModelCapabilities_ReturnsEvidenceAndWhatItMeans(t *testing.T) {
	h, mock, done := newModelCatalogHandlerMock(t)
	defer done()
	f := &fakeProber{running: true, reports: map[string]*modelprobe.Report{"teepin/x": {
		ModelRoute: "teepin/x",
		Checks:     []modelprobe.Check{{Capability: modelprobe.CapTools, Status: modelprobe.StatusFailed, Detail: "answered in text"}},
	}}}
	h.WithProbes(f)
	expectModelRow(mock, "teepin/x", "anthropic", "") // the row declares tools: true

	w := jsonRequest(h.ModelCapabilities, http.MethodGet, "/v1/admin/inference/models/capabilities?model_route=teepin/x", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	var got capabilityView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Running {
		t.Error("running flag not reported")
	}
	if e := got.Effective["tools"]; e.Allowed || !e.Declared {
		t.Errorf("a model that declared tools but failed its check must not be allowed: %+v", e)
	}
}

func TestDiscoverModel_ReadsMetadataWithoutSavingAnything(t *testing.T) {
	h, _, done := newModelCatalogHandlerMock(t)
	defer done()
	f := &fakeProber{}
	h.WithProbes(f)
	w := jsonRequest(h.DiscoverModel, http.MethodPost, "/v1/admin/inference/models/discover",
		[]byte(`{"provider":"openai_compatible","base_url":"https://x","provider_model":"m","api_key":"k"}`), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if f.discovered == nil || f.discovered.BaseURL != "https://x" || f.discovered.ProviderModel != "m" {
		t.Errorf("discovered = %+v", f.discovered)
	}
	bad := jsonRequest(h.DiscoverModel, http.MethodPost, "/v1/admin/inference/models/discover", []byte(`{"provider":"x"}`), nil)
	if bad.Code != http.StatusBadRequest {
		t.Errorf("a request without a model id: status %d, want 400", bad.Code)
	}
}

func TestProbeEndpoints_AreOffWithoutAProber(t *testing.T) {
	h, _, done := newModelCatalogHandlerMock(t)
	defer done()
	if w := jsonRequest(h.CheckModel, http.MethodPost, "/x?model_route=a", nil, nil); w.Code != http.StatusNotFound {
		t.Errorf("check: %d", w.Code)
	}
	if w := jsonRequest(h.ModelCapabilities, http.MethodGet, "/x?model_route=a", nil, nil); w.Code != http.StatusNotFound {
		t.Errorf("capabilities: %d", w.Code)
	}
	if w := jsonRequest(h.DiscoverModel, http.MethodPost, "/x", []byte(`{}`), nil); w.Code != http.StatusNotFound {
		t.Errorf("discover: %d", w.Code)
	}
}
