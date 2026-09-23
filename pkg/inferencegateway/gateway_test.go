// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package inferencegateway

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/inference"
	"github.com/FlashbackAi/teepin-core/pkg/modelcatalog"
	"github.com/FlashbackAi/teepin-core/pkg/nodeservices"
)

// fakeProvider lets tests control exactly what "calling a backend" does,
// without any real HTTP.
type fakeProvider struct {
	name       string
	onComplete func(ctx context.Context, req inference.Request) (*inference.Response, error)
}

func (f *fakeProvider) Name() string                         { return f.name }
func (f *fakeProvider) Capabilities() inference.Capabilities { return inference.Capabilities{} }
func (f *fakeProvider) Complete(ctx context.Context, req inference.Request) (*inference.Response, error) {
	return f.onComplete(ctx, req)
}
func (f *fakeProvider) Stream(ctx context.Context, req inference.Request, onChunk func(inference.Chunk) error) error {
	return errors.New("fakeProvider.Stream not implemented")
}

var _ inference.Provider = (*fakeProvider)(nil)

func newTestServices(t *testing.T) (*modelcatalog.Service, *nodeservices.Service, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return modelcatalog.NewService(db), nodeservices.NewService(db), mock, func() { db.Close() }
}

func expectGetModel(mock sqlmock.Sqlmock, route string, m modelcatalog.Model) {
	mock.ExpectQuery(`SELECT model_route, display_name, cost_class`).
		WithArgs(route).
		WillReturnRows(sqlmock.NewRows([]string{
			"model_route", "display_name", "cost_class", "engine", "context_window",
			"supports_tools", "supports_vision", "supports_audio",
			"input_price_per_million", "output_price_per_million",
			"vendor_input_cost_per_million", "vendor_output_cost_per_million",
			"enabled", "provider", "provider_model", "base_url", "max_output_tokens",
			"api_key_ref", "offered_to_customers", "kumbha_enabled", "kumbha_priority",
			"updated_by", "created_at", "updated_at",
		}).AddRow(
			m.ModelRoute, m.DisplayName, string(m.CostClass), m.Engine, m.ContextWindow,
			m.SupportsTools, m.SupportsVision, m.SupportsAudio,
			m.InputPricePerMillion, m.OutputPricePerMillion,
			m.VendorInputCostPerMillion, m.VendorOutputCostPerMillion,
			m.Enabled, providerOrNode(m.Provider), m.ProviderModel, m.BaseURL, 4096,
			m.APIKeyRef, m.OfferedToCustomers, m.KumbhaEnabled, m.KumbhaPriority,
			m.UpdatedBy, time.Now(), time.Now(),
		))
}

func providerOrNode(p modelcatalog.Provider) string {
	if p == "" {
		return string(modelcatalog.ProviderNode)
	}
	return string(p)
}

func expectGetModelNotFound(mock sqlmock.Sqlmock, route string) {
	mock.ExpectQuery(`SELECT model_route, display_name, cost_class`).
		WithArgs(route).
		WillReturnError(sql.ErrNoRows)
}

func expectListByKindEmpty(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT id, node_id, kind, config`).
		WithArgs("inference_model").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "node_id", "kind", "config", "desired_state", "observed_state",
			"observed_error", "observed_endpoint", "observed_at", "created_by", "created_at", "updated_at",
		}))
}

func expectListByKindRows(mock sqlmock.Sqlmock, rows []mountedRow) {
	res := sqlmock.NewRows([]string{
		"id", "node_id", "kind", "config", "desired_state", "observed_state",
		"observed_error", "observed_endpoint", "observed_at", "created_by", "created_at", "updated_at",
	})
	for _, r := range rows {
		res = res.AddRow(r.id, uuid.New(), "inference_model", []byte(r.config), "mounted", "mounted",
			nil, r.endpoint, time.Now(), "op", time.Now(), time.Now())
	}
	mock.ExpectQuery(`SELECT id, node_id, kind, config`).
		WithArgs("inference_model").
		WillReturnRows(res)
}

type mountedRow struct {
	id       uuid.UUID
	config   string
	endpoint string
}

func TestComplete_UnknownModelRejected(t *testing.T) {
	catalog, nodeSvcs, mock, done := newTestServices(t)
	defer done()
	expectGetModelNotFound(mock, "no/such-model")

	g := New(catalog, nodeSvcs, nil)
	_, err := g.Complete(context.Background(), "acct1", inference.Request{Model: "no/such-model"})
	if !errors.Is(err, inference.ErrUnknownModel) {
		t.Fatalf("got %v, want ErrUnknownModel", err)
	}
}

func TestComplete_DisabledModelRejected(t *testing.T) {
	catalog, nodeSvcs, mock, done := newTestServices(t)
	defer done()
	expectGetModel(mock, "teepin/retired", modelcatalog.Model{
		ModelRoute: "teepin/retired", CostClass: modelcatalog.CostClassOwn, Engine: "vllm", Enabled: false,
	})

	g := New(catalog, nodeSvcs, nil)
	_, err := g.Complete(context.Background(), "acct1", inference.Request{Model: "teepin/retired"})
	if !errors.Is(err, inference.ErrUnknownModel) {
		t.Fatalf("got %v, want ErrUnknownModel — a disabled model must route the same as an unknown one", err)
	}
}

// fakeSecrets serves fixed secret values, recording which names were read.
type fakeSecrets struct {
	values map[string]string
	err    error
	read   []string
}

func (f *fakeSecrets) Get(_ context.Context, id string) (string, bool, error) {
	f.read = append(f.read, id)
	if f.err != nil {
		return "", false, f.err
	}
	v, ok := f.values[id]
	return v, ok, nil
}
func (f *fakeSecrets) Put(context.Context, string, string) error { return nil }
func (f *fakeSecrets) Delete(context.Context, string) error      { return nil }

var haiku = modelcatalog.Model{
	ModelRoute: "anthropic/claude-haiku-4-5", CostClass: modelcatalog.CostClassFrontier, Engine: "anthropic",
	Enabled: true, Provider: modelcatalog.ProviderAnthropic, ProviderModel: "claude-haiku-4-5-20251001",
	APIKeyRef: "inference-model-key-1",
}

// An external model is built from its catalog row and served directly —
// it never touches node_services (sqlmock would flag an unexpected
// ListByKind query), and its API key is read from Secrets Manager under
// the environment prefix.
func TestComplete_ExternalModelServedFromCatalogWithItsKey(t *testing.T) {
	catalog, nodeSvcs, mock, done := newTestServices(t)
	defer done()
	expectGetModel(mock, haiku.ModelRoute, haiku)

	g := New(catalog, nodeSvcs, nil)
	secrets := &fakeSecrets{values: map[string]string{"teepin/dev/inference-model-key-1": "sk-test"}}
	g.SetSecrets(secrets, "dev")
	var gotKey string
	var gotModel modelcatalog.Model
	g.newExternal = func(m modelcatalog.Model, apiKey string) (inference.Provider, error) {
		gotModel, gotKey = m, apiKey
		return &fakeProvider{name: "anthropic", onComplete: func(context.Context, inference.Request) (*inference.Response, error) {
			return &inference.Response{Model: "claude-haiku-4-5-20251001"}, nil
		}}, nil
	}

	resp, err := g.Complete(context.Background(), "acct1", inference.Request{Model: haiku.ModelRoute})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("resp.Model = %q", resp.Model)
	}
	if gotKey != "sk-test" || gotModel.ProviderModel != "claude-haiku-4-5-20251001" {
		t.Errorf("factory got key %q, model %q", gotKey, gotModel.ProviderModel)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet (or unexpected) DB calls: %v", err)
	}
}

// A key that cannot be read is a retryable outage for that model, not a
// silent keyless call.
func TestComplete_ExternalKeyReadFailureIsUnavailable(t *testing.T) {
	catalog, nodeSvcs, mock, done := newTestServices(t)
	defer done()
	expectGetModel(mock, haiku.ModelRoute, haiku)

	g := New(catalog, nodeSvcs, nil)
	g.SetSecrets(&fakeSecrets{err: errors.New("throttled")}, "dev")
	g.newExternal = func(modelcatalog.Model, string) (inference.Provider, error) {
		t.Error("provider built despite the key read failing")
		return nil, errors.New("unreachable")
	}

	_, err := g.Complete(context.Background(), "acct1", inference.Request{Model: haiku.ModelRoute})
	if !errors.Is(err, inference.ErrProviderUnavailable) {
		t.Fatalf("got %v, want ErrProviderUnavailable", err)
	}
}

// A rotated key must take effect on the next request: the cached provider
// is rebuilt only when the model config or key actually changed.
func TestExternalProviderFor_RebuildsOnlyWhenKeyChanges(t *testing.T) {
	g := New(nil, nil, nil)
	secrets := &fakeSecrets{values: map[string]string{"teepin/dev/inference-model-key-1": "v1"}}
	g.SetSecrets(secrets, "dev")
	builds := 0
	g.newExternal = func(modelcatalog.Model, string) (inference.Provider, error) {
		builds++
		return &fakeProvider{name: "anthropic"}, nil
	}

	for i := 0; i < 2; i++ {
		if _, err := g.externalProviderFor(context.Background(), haiku); err != nil {
			t.Fatal(err)
		}
	}
	if builds != 1 {
		t.Fatalf("built %d times for an unchanged model, want 1", builds)
	}
	secrets.values["teepin/dev/inference-model-key-1"] = "v2"
	if _, err := g.externalProviderFor(context.Background(), haiku); err != nil {
		t.Fatal(err)
	}
	if builds != 2 {
		t.Errorf("built %d times after a key rotation, want 2", builds)
	}
}

func TestStatus_SelfHostedWithNothingMountedIsNoBackend(t *testing.T) {
	catalog, nodeSvcs, mock, done := newTestServices(t)
	defer done()
	expectListByKindEmpty(mock)

	g := New(catalog, nodeSvcs, nil)
	st := g.Status(context.Background(), modelcatalog.Model{ModelRoute: "teepin/qwen3-30b-a3b", Provider: modelcatalog.ProviderNode})
	if st.State != StateNoBackend {
		t.Errorf("State = %q, want %q", st.State, StateNoBackend)
	}
}

func TestStatus_ExternalBeforeAnyCheckIsUnknown(t *testing.T) {
	g := New(nil, nil, nil)
	if st := g.Status(context.Background(), haiku); st.State != StateUnknown {
		t.Errorf("State = %q, want %q — an unchecked model must not look unhealthy", st.State, StateUnknown)
	}
}

func TestComplete_NoBackendMounted(t *testing.T) {
	catalog, nodeSvcs, mock, done := newTestServices(t)
	defer done()
	expectGetModel(mock, "teepin/qwen3-omni-7b", modelcatalog.Model{
		ModelRoute: "teepin/qwen3-omni-7b", CostClass: modelcatalog.CostClassOwn, Engine: "vllm-omni", Enabled: true,
	})
	expectListByKindEmpty(mock)

	g := New(catalog, nodeSvcs, nil)
	_, err := g.Complete(context.Background(), "acct1", inference.Request{Model: "teepin/qwen3-omni-7b"})
	if !errors.Is(err, inference.ErrProviderUnavailable) {
		t.Fatalf("got %v, want ErrProviderUnavailable", err)
	}
}

// TestComplete_MountedWithNoEndpointYetIsSkipped proves a row that's
// desired+observed mounted but has no ObservedEndpoint (a state that
// should not exist per ReportObserved's own contract, but is worth
// defending against rather than trusting blindly) is treated as not a
// real candidate, not dereferenced into a panic.
func TestComplete_MountedWithNoEndpointYetIsSkipped(t *testing.T) {
	catalog, nodeSvcs, mock, done := newTestServices(t)
	defer done()

	expectGetModel(mock, "teepin/x", modelcatalog.Model{ModelRoute: "teepin/x", CostClass: modelcatalog.CostClassOwn, Engine: "vllm", Enabled: true})
	mock.ExpectQuery(`SELECT id, node_id, kind, config`).
		WithArgs("inference_model").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "node_id", "kind", "config", "desired_state", "observed_state",
			"observed_error", "observed_endpoint", "observed_at", "created_by", "created_at", "updated_at",
		}).AddRow(
			uuid.New(), uuid.New(), "inference_model", []byte(`{"model_route":"teepin/x","engine":"vllm"}`),
			"mounted", "mounted", nil, nil, time.Now(), "op", time.Now(), time.Now(),
		))

	g := New(catalog, nodeSvcs, nil)
	_, err := g.Complete(context.Background(), "acct1", inference.Request{Model: "teepin/x"})
	if !errors.Is(err, inference.ErrProviderUnavailable) {
		t.Fatalf("got %v, want ErrProviderUnavailable", err)
	}
}

// TestComplete_RoutesToLeastLoadedBackend proves that when a model is
// mounted on two backends, a request lands on whichever one currently has
// fewer in-flight requests, not round-robin — the reasoning being LLM
// request durations vary too much for round-robin to stay balanced.
func TestComplete_RoutesToLeastLoadedBackend(t *testing.T) {
	catalog, nodeSvcs, mock, done := newTestServices(t)
	defer done()

	idA, idB := uuid.New(), uuid.New()
	rows := []mountedRow{
		{id: idA, config: `{"model_route":"teepin/x","engine":"vllm"}`, endpoint: "http://a"},
		{id: idB, config: `{"model_route":"teepin/x","engine":"vllm"}`, endpoint: "http://b"},
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	providerA := &fakeProvider{name: "A", onComplete: func(ctx context.Context, req inference.Request) (*inference.Response, error) {
		entered <- struct{}{}
		<-release
		return &inference.Response{Model: "A"}, nil
	}}
	providerB := &fakeProvider{name: "B", onComplete: func(ctx context.Context, req inference.Request) (*inference.Response, error) {
		return &inference.Response{Model: "B"}, nil
	}}

	g := New(catalog, nodeSvcs, func(cfg ModelServiceConfig, endpoint string) (inference.Provider, error) {
		if endpoint == "http://a" {
			return providerA, nil
		}
		return providerB, nil
	})

	expectGetModel(mock, "teepin/x", modelcatalog.Model{ModelRoute: "teepin/x", CostClass: modelcatalog.CostClassOwn, Engine: "vllm", Enabled: true})
	expectListByKindRows(mock, rows)
	expectGetModel(mock, "teepin/x", modelcatalog.Model{ModelRoute: "teepin/x", CostClass: modelcatalog.CostClassOwn, Engine: "vllm", Enabled: true})
	expectListByKindRows(mock, rows)

	done1 := make(chan struct{})
	go func() {
		defer close(done1)
		_, _ = g.Complete(context.Background(), "acct1", inference.Request{Model: "teepin/x"})
	}()
	<-entered // A's slot is now held — B has strictly fewer in-flight requests

	resp, err := g.Complete(context.Background(), "acct1", inference.Request{Model: "teepin/x"})
	if err != nil {
		t.Fatalf("second Complete: %v", err)
	}
	if resp.Model != "B" {
		t.Errorf("second request routed to %q, want B (the less-loaded backend)", resp.Model)
	}

	release <- struct{}{}
	<-done1
}

func TestAcquireBackendSlot_ThrottlesAtCapacity(t *testing.T) {
	catalog, nodeSvcs, _, done := newTestServices(t)
	defer done()

	g := New(catalog, nodeSvcs, nil)
	g.QueueWait = 20 * time.Millisecond // fast test, not the real 30s default
	nsID := uuid.New()

	release, err := g.acquireBackendSlot(context.Background(), nsID, 1)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	_, err = g.acquireBackendSlot(context.Background(), nsID, 1)
	if !errors.Is(err, ErrThrottled) {
		t.Fatalf("second acquire (capacity 1, slot held) got %v, want ErrThrottled", err)
	}

	release()
	release2, err := g.acquireBackendSlot(context.Background(), nsID, 1)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	release2()
}

func TestAcquireAccountSlot_RejectsAtCap(t *testing.T) {
	catalog, nodeSvcs, _, done := newTestServices(t)
	defer done()

	g := New(catalog, nodeSvcs, nil)

	var releases []func()
	for i := 0; i < defaultPerAccountCap; i++ {
		r, err := g.acquireAccountSlot("acct1", "teepin/x")
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		releases = append(releases, r)
	}

	if _, err := g.acquireAccountSlot("acct1", "teepin/x"); !errors.Is(err, ErrThrottled) {
		t.Fatalf("got %v, want ErrThrottled once the per-account cap is reached", err)
	}

	// A different account against the same model is unaffected.
	if _, err := g.acquireAccountSlot("acct2", "teepin/x"); err != nil {
		t.Errorf("a different account was throttled by acct1's cap: %v", err)
	}

	for _, r := range releases {
		r()
	}
}

func TestProviderFor_CachesUntilConfigChanges(t *testing.T) {
	catalog, nodeSvcs, _, done := newTestServices(t)
	defer done()

	calls := 0
	g := New(catalog, nodeSvcs, func(cfg ModelServiceConfig, endpoint string) (inference.Provider, error) {
		calls++
		return &fakeProvider{name: endpoint}, nil
	})

	nsID := uuid.New()
	cfg := ModelServiceConfig{Engine: "vllm", BackendModel: "m"}
	endpoint := "http://a"

	if _, err := g.providerFor(nsID, cfg, endpoint); err != nil {
		t.Fatalf("providerFor: %v", err)
	}
	if _, err := g.providerFor(nsID, cfg, endpoint); err != nil {
		t.Fatalf("providerFor (cached): %v", err)
	}
	if calls != 1 {
		t.Errorf("factory called %d times for an unchanged config, want 1", calls)
	}

	// The endpoint changing (a pod recreated after a node reboot, say)
	// must invalidate the cache even though cfg itself is unchanged —
	// otherwise the gateway would keep dispatching to a dead address.
	endpoint = "http://b"
	if _, err := g.providerFor(nsID, cfg, endpoint); err != nil {
		t.Fatalf("providerFor (changed endpoint): %v", err)
	}
	if calls != 2 {
		t.Errorf("factory called %d times after the endpoint changed, want 2 (cache should invalidate)", calls)
	}
}

func TestDefaultProviderFactory_UnsupportedEngineErrors(t *testing.T) {
	if _, err := defaultProviderFactory(ModelServiceConfig{Engine: "llamacpp"}, "http://a"); err == nil {
		t.Error("unknown engine accepted")
	}
}
