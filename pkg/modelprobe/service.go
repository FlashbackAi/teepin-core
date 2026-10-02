// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package modelprobe

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/FlashbackAi/teepin-core/pkg/inference"
	"github.com/FlashbackAi/teepin-core/pkg/modelcatalog"
)

// Catalog is the part of the model catalog the prober reads.
type Catalog interface {
	GetModel(ctx context.Context, route string) (*modelcatalog.Model, error)
	ListModels(ctx context.Context) ([]modelcatalog.Model, error)
}

// Providers builds the provider that real traffic uses for a model, so a probe
// exercises exactly the path a customer's request takes.
type Providers interface {
	// ProviderFor returns the provider for a saved model.
	ProviderFor(ctx context.Context, m modelcatalog.Model) (inference.Provider, error)
	// BuildProvider builds one for a model that is not saved yet, with the API
	// key an operator just typed.
	BuildProvider(m modelcatalog.Model, apiKey string) (inference.Provider, error)
}

// ReportStore keeps reports (Store implements it).
type ReportStore interface {
	Get(ctx context.Context, route string) (*Report, error)
	All(ctx context.Context) (map[string]*Report, error)
	Put(ctx context.Context, r *Report) error
	Delete(ctx context.Context, route string) error
}

// ErrCheckRunning means a check of this model is already in progress.
var ErrCheckRunning = errors.New("a capability check for this model is already running")

// checkTimeout bounds one whole check: metadata plus every capability.
const checkTimeout = 6 * time.Minute

// Service runs checks and keeps their results.
type Service struct {
	catalog   Catalog
	providers Providers
	store     ReportStore

	// newRunner is overridable so tests can drive the service with a fake.
	newRunner func(p inference.Provider, route string) *Runner

	mu      sync.Mutex
	running map[string]bool
}

func NewService(catalog Catalog, providers Providers, store ReportStore) *Service {
	return &Service{
		catalog: catalog, providers: providers, store: store,
		running: map[string]bool{},
		newRunner: func(p inference.Provider, route string) *Runner {
			return &Runner{Provider: p, Route: route}
		},
	}
}

func (s *Service) begin(route string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[route] {
		return false
	}
	s.running[route] = true
	return true
}

func (s *Service) end(route string) {
	s.mu.Lock()
	delete(s.running, route)
	s.mu.Unlock()
}

// Latest returns the model's stored report (ErrNoReport when never checked).
func (s *Service) Latest(ctx context.Context, route string) (*Report, error) {
	return s.store.Get(ctx, route)
}

// Check reads the model's metadata and tests every capability, stores the
// result and returns it. only limits it to some capabilities when non-empty.
func (s *Service) Check(ctx context.Context, route string, only ...Capability) (*Report, error) {
	if !s.begin(route) {
		return nil, ErrCheckRunning
	}
	defer s.end(route)

	m, err := s.catalog.GetModel(ctx, route)
	if err != nil {
		return nil, err
	}
	provider, err := s.providers.ProviderFor(ctx, *m)
	if err != nil {
		return nil, fmt.Errorf("cannot reach %q to check it: %w", route, err)
	}

	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	rep := &Report{ModelRoute: route, RanAt: time.Now().UTC()}
	if len(only) == 0 {
		rep.Metadata = metadataOf(ctx, provider)
	}
	runner := s.newRunner(provider, route)
	want := map[Capability]bool{}
	for _, c := range only {
		want[c] = true
	}
	for _, c := range AllCapabilities {
		if len(only) > 0 && !want[c] {
			continue
		}
		// The many-tools test only means something for a model whose plain tool
		// test passes; asking a model that cannot call tools to pick from
		// twenty-six wastes calls and proves nothing.
		if c == CapToolsMany && rep.Check(CapTools).Status != StatusPassed {
			continue
		}
		rep.Checks = append(rep.Checks, runner.RunCapability(ctx, c))
	}
	if err := s.store.Put(ctx, rep); err != nil {
		return nil, err
	}
	return s.store.Get(ctx, route)
}

// Discover reads metadata for a model that is not saved yet, with the API key
// just typed, so the registration form can be filled in from the backend.
func (s *Service) Discover(ctx context.Context, m modelcatalog.Model, apiKey string) (*MetadataReport, error) {
	provider, err := s.providers.BuildProvider(m, apiKey)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return metadataOf(ctx, provider), nil
}

func metadataOf(ctx context.Context, p inference.Provider) *MetadataReport {
	src, ok := p.(inference.MetadataSource)
	if !ok {
		return &MetadataReport{Error: "this kind of backend does not describe its models"}
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	md, err := src.DiscoverMetadata(cctx)
	if err != nil {
		return &MetadataReport{Error: err.Error()}
	}
	return &MetadataReport{
		ContextWindow: md.ContextWindow, MaxOutputTokens: md.MaxOutputTokens, Vision: md.Vision, Source: md.Source,
	}
}

// CheckMissing checks every enabled model that has never been checked, one
// after another, so models registered before checks existed get evidence too.
func (s *Service) CheckMissing(ctx context.Context) {
	models, err := s.catalog.ListModels(ctx)
	if err != nil {
		log.Printf("WARN: model capability backfill could not list the catalog: %v", err)
		return
	}
	for _, m := range models {
		if !m.Enabled {
			continue
		}
		if _, err := s.store.Get(ctx, m.ModelRoute); !errors.Is(err, ErrNoReport) {
			continue
		}
		if _, err := s.Check(ctx, m.ModelRoute); err != nil && !errors.Is(err, ErrCheckRunning) {
			log.Printf("WARN: capability check of %q failed to run: %v", m.ModelRoute, err)
			continue
		}
		log.Printf("model capability check of %q done", m.ModelRoute)
	}
}

// Invalidate drops a model's report, for when its backend changed.
func (s *Service) Invalidate(ctx context.Context, route string) error {
	return s.store.Delete(ctx, route)
}

// IsRunning reports whether a check of the model is in progress.
func (s *Service) IsRunning(route string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running[route]
}

// CheckAsync starts a check in the background and returns at once: a full
// check makes a dozen model calls and can take minutes, longer than a request
// should be held open. It returns false when one is already running.
func (s *Service) CheckAsync(route string) bool {
	if s.IsRunning(route) {
		return false
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), checkTimeout+time.Minute)
		defer cancel()
		if _, err := s.Check(ctx, route); err != nil && !errors.Is(err, ErrCheckRunning) {
			log.Printf("WARN: capability check of %q failed to run: %v", route, err)
		}
	}()
	return true
}

// Reports returns every stored report, keyed by route.
func (s *Service) Reports(ctx context.Context) (map[string]*Report, error) {
	return s.store.All(ctx)
}
