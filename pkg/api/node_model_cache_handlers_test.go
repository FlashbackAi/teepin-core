// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/cluster"
	"github.com/FlashbackAi/teepin-core/pkg/nodes"
	"github.com/FlashbackAi/teepin-core/pkg/nodeservices"
)

type fakeCacheAgents struct {
	models  []cluster.CachedModel
	known   bool
	online  bool
	delErr  error
	deleted []string
}

func (f *fakeCacheAgents) CachedModels(string) ([]cluster.CachedModel, bool, bool) {
	return f.models, f.known, f.online
}
func (f *fakeCacheAgents) DeleteCachedModel(_ context.Context, _, repo string) error {
	if f.delErr != nil {
		return f.delErr
	}
	f.deleted = append(f.deleted, repo)
	return nil
}

type fakeNodes struct{ list []nodes.Node }

func (f fakeNodes) ListNodes(context.Context) ([]nodes.Node, error) { return f.list, nil }

type fakeMounts struct{ rows []nodeservices.NodeService }

func (f fakeMounts) ListForNode(context.Context, uuid.UUID) ([]nodeservices.NodeService, error) {
	return f.rows, nil
}

func mountRow(source string, desired nodeservices.DesiredState, observed nodeservices.ObservedState) nodeservices.NodeService {
	cfg, _ := json.Marshal(map[string]any{"model_route": "r", "engine": "mlx", "model_source": source})
	return nodeservices.NodeService{Kind: nodeservices.KindInferenceModel, Config: cfg, DesiredState: desired, ObservedState: observed}
}

func cacheRouter(h *NodeModelCacheHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/nodes/:id/cached-models", h.List)
	r.DELETE("/nodes/:id/cached-models", h.Delete)
	return r
}

func newCacheFixture() (*NodeModelCacheHandler, *fakeCacheAgents, *fakeMounts, nodes.Node) {
	node := nodes.Node{ID: uuid.New(), ProviderID: "prov-1", NodeName: "mac"}
	agents := &fakeCacheAgents{online: true, known: true, models: []cluster.CachedModel{
		{RepoID: "org/used", SizeBytes: 100},
		{RepoID: "org/spare", SizeBytes: 200},
	}}
	mounts := &fakeMounts{rows: []nodeservices.NodeService{
		mountRow("https://huggingface.co/org/used", nodeservices.DesiredMounted, nodeservices.ObservedMounted),
	}}
	return NewNodeModelCacheHandler(agents, fakeNodes{list: []nodes.Node{node}}, mounts), agents, mounts, node
}

func do(r *gin.Engine, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestModelCacheList_MarksInUse(t *testing.T) {
	h, _, _, node := newCacheFixture()
	rec := do(cacheRouter(h), http.MethodGet, "/nodes/"+node.ID.String()+"/cached-models")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out cachedModelsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !out.Known || !out.Online || len(out.Models) != 2 {
		t.Fatalf("response = %+v", out)
	}
	byRepo := map[string]bool{}
	for _, m := range out.Models {
		byRepo[m.RepoID] = m.InUse
	}
	if !byRepo["org/used"] || byRepo["org/spare"] {
		t.Errorf("in_use = %v, want used=true spare=false", byRepo)
	}
}

func TestModelCacheList_OfflineNodeIsAnEmptyOfflineResponse(t *testing.T) {
	h, agents, _, node := newCacheFixture()
	agents.online = false
	rec := do(cacheRouter(h), http.MethodGet, "/nodes/"+node.ID.String()+"/cached-models")
	var out cachedModelsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != 200 || out.Online || out.Models == nil || len(out.Models) != 0 {
		t.Errorf("code=%d response=%+v (models must be [] not null)", rec.Code, out)
	}
}

func TestModelCacheDelete(t *testing.T) {
	q := func(repo string) string { return "?repo=" + url.QueryEscape(repo) }

	t.Run("deletes an unused model", func(t *testing.T) {
		h, agents, _, node := newCacheFixture()
		rec := do(cacheRouter(h), http.MethodDelete, "/nodes/"+node.ID.String()+"/cached-models"+q("org/spare"))
		if rec.Code != 200 || len(agents.deleted) != 1 || agents.deleted[0] != "org/spare" {
			t.Errorf("code=%d deleted=%v", rec.Code, agents.deleted)
		}
	})

	t.Run("refuses a mounted model without touching the agent", func(t *testing.T) {
		h, agents, _, node := newCacheFixture()
		rec := do(cacheRouter(h), http.MethodDelete, "/nodes/"+node.ID.String()+"/cached-models"+q("org/used"))
		if rec.Code != http.StatusConflict || len(agents.deleted) != 0 {
			t.Errorf("code=%d deleted=%v, want 409 and no agent call", rec.Code, agents.deleted)
		}
	})

	t.Run("a model whose mount is fully unmounted can be deleted", func(t *testing.T) {
		h, agents, mounts, node := newCacheFixture()
		mounts.rows = []nodeservices.NodeService{
			mountRow("https://huggingface.co/org/used", nodeservices.DesiredUnmounted, nodeservices.ObservedUnmounted),
		}
		rec := do(cacheRouter(h), http.MethodDelete, "/nodes/"+node.ID.String()+"/cached-models"+q("org/used"))
		if rec.Code != 200 || len(agents.deleted) != 1 {
			t.Errorf("code=%d deleted=%v", rec.Code, agents.deleted)
		}
	})

	t.Run("a model still shutting down counts as in use", func(t *testing.T) {
		h, agents, mounts, node := newCacheFixture()
		mounts.rows = []nodeservices.NodeService{
			mountRow("https://huggingface.co/org/used", nodeservices.DesiredUnmounted, nodeservices.ObservedMounted),
		}
		rec := do(cacheRouter(h), http.MethodDelete, "/nodes/"+node.ID.String()+"/cached-models"+q("org/used"))
		if rec.Code != http.StatusConflict || len(agents.deleted) != 0 {
			t.Errorf("code=%d deleted=%v, want 409", rec.Code, agents.deleted)
		}
	})

	t.Run("error mapping", func(t *testing.T) {
		cases := []struct {
			err  error
			want int
		}{
			{cluster.ErrProviderOffline, http.StatusServiceUnavailable},
			{cluster.ErrModelNotCached, http.StatusNotFound},
			{errors.New("boom"), http.StatusBadGateway},
		}
		for _, c := range cases {
			h, agents, _, node := newCacheFixture()
			agents.delErr = c.err
			rec := do(cacheRouter(h), http.MethodDelete, "/nodes/"+node.ID.String()+"/cached-models"+q("org/spare"))
			if rec.Code != c.want {
				t.Errorf("%v -> %d, want %d", c.err, rec.Code, c.want)
			}
		}
	})

	t.Run("validation", func(t *testing.T) {
		h, _, _, node := newCacheFixture()
		if rec := do(cacheRouter(h), http.MethodDelete, "/nodes/"+node.ID.String()+"/cached-models"); rec.Code != 400 {
			t.Errorf("missing repo -> %d, want 400", rec.Code)
		}
		if rec := do(cacheRouter(h), http.MethodDelete, "/nodes/not-a-uuid/cached-models?repo=a/b"); rec.Code != 400 {
			t.Errorf("bad node id -> %d, want 400", rec.Code)
		}
		if rec := do(cacheRouter(h), http.MethodDelete, "/nodes/"+uuid.New().String()+"/cached-models?repo=a/b"); rec.Code != 404 {
			t.Errorf("unknown node -> %d, want 404", rec.Code)
		}
	})
}

// Hugging Face repo names are not case-sensitive in URLs, so a mount whose
// URL differs in case from the cached folder must still protect it.
func TestModelCacheDelete_InUseCheckIgnoresCase(t *testing.T) {
	h, agents, mounts, node := newCacheFixture()
	mounts.rows = []nodeservices.NodeService{
		mountRow("https://huggingface.co/ORG/USED", nodeservices.DesiredMounted, nodeservices.ObservedMounted),
	}
	rec := do(cacheRouter(h), http.MethodDelete, "/nodes/"+node.ID.String()+"/cached-models?repo=org%2Fused")
	if rec.Code != http.StatusConflict || len(agents.deleted) != 0 {
		t.Errorf("code=%d deleted=%v, want 409", rec.Code, agents.deleted)
	}
}
