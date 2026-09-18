// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package inferencegateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/inference"
	"github.com/FlashbackAi/teepin-core/pkg/modelcatalog"
	"github.com/FlashbackAi/teepin-core/pkg/nodeservices"
)

// ErrThrottled means the gateway itself declined to dispatch — a per-
// account or per-backend concurrency ceiling was already full — as
// distinct from inference.ErrProviderUnavailable, which means a backend
// was actually reached and failed. Both are retryable, but keeping them
// distinct lets an HTTP layer log/alert on them differently: one is "your
// own infra is saturated," the other is "the caller is sending too much."
var ErrThrottled = errors.New("too many concurrent requests for this account/model right now")

// Defaults used when a mounted backend's config or the gateway's own
// per-account policy doesn't specify one. Deliberately conservative —
// erring toward throttling early is cheap to loosen later, whereas a
// default that's too generous can quietly overload a shared home GPU.
const (
	defaultMaxConcurrency = 4
	defaultPerAccountCap  = 2
	defaultQueueWait      = 30 * time.Second
)

// providerFactory builds a Provider for a mounted backend's config.
// A field, not a hardcoded call, so tests can substitute a fake and so a
// future tunnel-backed engine can be added without changing Gateway's own
// dispatch logic.
type providerFactory func(cfg ModelServiceConfig) (inference.Provider, error)

// Gateway is Teepin Inference's router.
type Gateway struct {
	catalog     *modelcatalog.Service
	nodeSvcs    *nodeservices.Service
	newProvider providerFactory

	// QueueWait bounds how long a request waits for a backend concurrency
	// slot to free before it's throttled — the actual "queue" in
	// "queued rather than rejected outright." Exported so a caller (or a
	// test wanting this to run in milliseconds, not real seconds) can
	// override the default.
	QueueWait time.Duration

	mu sync.Mutex
	// frontier holds pre-wired third-party providers, keyed by catalog
	// model_route — registered at construction (main.go's job, not this
	// package's), never looked up via node_services at all: a proxied
	// model has no home-node routing question to answer.
	frontier map[string]inference.Provider
	// providers caches a constructed Provider per node_service id, keyed
	// further by a hash of its config so a config change (a re-mount with
	// a different base_url/model) invalidates the cache entry instead of
	// silently reusing a stale HTTP client.
	providers map[uuid.UUID]cachedProvider
	// backendSem is a buffered-channel semaphore per node_service,
	// capacity = that backend's MaxConcurrency — a send blocks (queues)
	// once full, a receive (on release) frees the next waiter immediately,
	// no polling.
	backendSem map[uuid.UUID]chan struct{}
	// inFlight mirrors backendSem's occupancy for least-loaded selection —
	// a channel's own length would work too, but selection needs to read
	// it without a goroutine actually holding the semaphore yet.
	inFlight map[uuid.UUID]int
	// accountInFlight counts in-flight-or-queued requests per
	// (account, model route), the fairness ceiling that stops one account
	// starving others against a shared backend.
	accountInFlight map[accountModelKey]int
}

type cachedProvider struct {
	configHash string
	provider   inference.Provider
}

type accountModelKey struct {
	accountID  string
	modelRoute string
}

// New constructs a Gateway. newProvider may be nil to use the real
// HTTP-backed factory (defaultProviderFactory); tests pass a fake.
func New(catalog *modelcatalog.Service, nodeSvcs *nodeservices.Service, newProvider providerFactory) *Gateway {
	if newProvider == nil {
		newProvider = defaultProviderFactory
	}
	return &Gateway{
		catalog:         catalog,
		nodeSvcs:        nodeSvcs,
		newProvider:     newProvider,
		QueueWait:       defaultQueueWait,
		frontier:        make(map[string]inference.Provider),
		providers:       make(map[uuid.UUID]cachedProvider),
		backendSem:      make(map[uuid.UUID]chan struct{}),
		inFlight:        make(map[uuid.UUID]int),
		accountInFlight: make(map[accountModelKey]int),
	}
}

// defaultProviderFactory builds a real HTTP-backed Provider. Only the two
// engines actually deployed today are wired — see ModelServiceConfig's own
// doc comment for why MLX/tunnel-backed engines aren't here yet.
func defaultProviderFactory(cfg ModelServiceConfig) (inference.Provider, error) {
	switch cfg.Engine {
	case "vllm", "vllm-omni":
		return inference.NewVLLM(inference.VLLMConfig{
			BaseURL: cfg.BaseURL,
			Model:   cfg.BackendModel,
			APIKey:  cfg.APIKey,
		}), nil
	default:
		return nil, fmt.Errorf("inferencegateway: no Provider factory for engine %q yet", cfg.Engine)
	}
}

// RegisterFrontierProvider wires a pre-configured third-party Provider
// (e.g. Anthropic) to a catalog model_route. Call sites own the vendor
// credential; this package never sees it.
func (g *Gateway) RegisterFrontierProvider(modelRoute string, p inference.Provider) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.frontier[modelRoute] = p
}

// Complete resolves req.Model, picks a backend, and dispatches — the
// stateless request/response path (see the roadmap's product-boundary
// decision: Teepin Inference never owns conversation state, only this one
// call). accountID identifies the caller for the per-account concurrency
// ceiling; a future HTTP layer resolves it from the API key before calling
// in.
func (g *Gateway) Complete(ctx context.Context, accountID string, req inference.Request) (*inference.Response, error) {
	provider, _, release, err := g.resolve(ctx, accountID, req.Model)
	if err != nil {
		return nil, err
	}
	defer release()
	return provider.Complete(ctx, req)
}

// Stream is Complete's streaming counterpart, same resolution/gating.
func (g *Gateway) Stream(ctx context.Context, accountID string, req inference.Request, onChunk func(inference.Chunk) error) error {
	provider, _, release, err := g.resolve(ctx, accountID, req.Model)
	if err != nil {
		return err
	}
	defer release()
	return provider.Stream(ctx, req, onChunk)
}

// resolve does everything short of actually calling the provider: catalog
// lookup, frontier-vs-self-hosted branch, least-loaded backend selection,
// and concurrency gating. Returns a release func that MUST be called
// exactly once (via defer) regardless of outcome, to free the slots it
// acquired.
func (g *Gateway) resolve(ctx context.Context, accountID, modelRoute string) (inference.Provider, *uuid.UUID, func(), error) {
	model, err := g.catalog.GetModel(ctx, modelRoute)
	if errors.Is(err, modelcatalog.ErrNotFound) || (err == nil && !model.Enabled) {
		return nil, nil, noop, fmt.Errorf("%w: %q", inference.ErrUnknownModel, modelRoute)
	}
	if err != nil {
		return nil, nil, noop, fmt.Errorf("resolve model %q: %w", modelRoute, err)
	}

	releaseAccount, err := g.acquireAccountSlot(accountID, modelRoute)
	if err != nil {
		return nil, nil, noop, err
	}

	if model.CostClass == modelcatalog.CostClassFrontier {
		g.mu.Lock()
		p, ok := g.frontier[modelRoute]
		g.mu.Unlock()
		if !ok {
			releaseAccount()
			return nil, nil, noop, fmt.Errorf("%w: %q has no registered frontier provider", inference.ErrProviderUnavailable, modelRoute)
		}
		return p, nil, releaseAccount, nil
	}

	candidates, err := g.candidatesFor(ctx, modelRoute)
	if err != nil {
		releaseAccount()
		return nil, nil, noop, err
	}
	if len(candidates) == 0 {
		releaseAccount()
		return nil, nil, noop, fmt.Errorf("%w: no backend currently mounted for %q", inference.ErrProviderUnavailable, modelRoute)
	}

	ns, cfg := g.pickLeastLoaded(candidates)
	releaseBackend, err := g.acquireBackendSlot(ctx, ns.ID, cfg.MaxConcurrency)
	if err != nil {
		releaseAccount()
		return nil, nil, noop, err
	}

	provider, err := g.providerFor(ns.ID, cfg)
	if err != nil {
		releaseBackend()
		releaseAccount()
		return nil, nil, noop, err
	}

	id := ns.ID
	return provider, &id, func() {
		releaseBackend()
		releaseAccount()
	}, nil
}

func noop() {}

// candidatesFor returns every node_services row actually serving
// modelRoute right now: desired AND observed both mounted. Queried live,
// same "no caching, an admin's mount/unmount takes effect immediately"
// rule already used for pricing elsewhere on this platform — deliberate,
// not an oversight, given today's fleet size.
func (g *Gateway) candidatesFor(ctx context.Context, modelRoute string) ([]candidate, error) {
	rows, err := g.nodeSvcs.ListByKind(ctx, nodeservices.KindInferenceModel)
	if err != nil {
		return nil, fmt.Errorf("list mounted model backends: %w", err)
	}
	var out []candidate
	for _, ns := range rows {
		if ns.DesiredState != nodeservices.DesiredMounted || ns.ObservedState != nodeservices.ObservedMounted {
			continue
		}
		cfg, err := ParseModelServiceConfig(ns.Config)
		if err != nil || cfg.ModelRoute != modelRoute {
			continue // malformed or a different model's mount — skip, never fail the whole request over one bad row
		}
		out = append(out, candidate{ns: ns, cfg: cfg})
	}
	return out, nil
}

type candidate struct {
	ns  nodeservices.NodeService
	cfg ModelServiceConfig
}

// pickLeastLoaded returns whichever candidate currently has the fewest
// in-flight requests — the right default for LLM traffic specifically,
// where request duration varies enormously by length, unlike round-robin
// which assumes roughly-equal-cost requests.
func (g *Gateway) pickLeastLoaded(candidates []candidate) (nodeservices.NodeService, ModelServiceConfig) {
	g.mu.Lock()
	defer g.mu.Unlock()

	best := candidates[0]
	bestLoad := g.inFlight[best.ns.ID]
	for _, c := range candidates[1:] {
		if load := g.inFlight[c.ns.ID]; load < bestLoad {
			best, bestLoad = c, load
		}
	}
	return best.ns, best.cfg
}

// acquireBackendSlot bounds concurrency against ONE mounted backend via a
// buffered-channel semaphore: an immediate send succeeds if a slot is
// free; otherwise the request queues (blocks) until either a slot frees,
// QueueWait elapses, or the caller's context is cancelled. No polling — a
// release always unblocks the next waiter, if any, right away.
func (g *Gateway) acquireBackendSlot(ctx context.Context, nsID uuid.UUID, maxConcurrency int) (func(), error) {
	sem := g.semFor(nsID, maxConcurrency)

	release := func() {
		<-sem
		g.mu.Lock()
		g.inFlight[nsID]--
		g.mu.Unlock()
	}

	select {
	case sem <- struct{}{}:
		g.mu.Lock()
		g.inFlight[nsID]++
		g.mu.Unlock()
		return release, nil
	default:
	}

	timer := time.NewTimer(g.QueueWait)
	defer timer.Stop()
	select {
	case sem <- struct{}{}:
		g.mu.Lock()
		g.inFlight[nsID]++
		g.mu.Unlock()
		return release, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, fmt.Errorf("%w: backend at capacity (%d concurrent)", ErrThrottled, maxConcurrency)
	}
}

// semFor returns this node_service's semaphore, creating it (or recreating
// it, if MaxConcurrency changed since the last mount) as needed.
func (g *Gateway) semFor(nsID uuid.UUID, maxConcurrency int) chan struct{} {
	if maxConcurrency <= 0 {
		maxConcurrency = defaultMaxConcurrency
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	sem, ok := g.backendSem[nsID]
	if !ok || cap(sem) != maxConcurrency {
		sem = make(chan struct{}, maxConcurrency)
		g.backendSem[nsID] = sem
	}
	return sem
}

// acquireAccountSlot is a fairness ceiling, not a capacity limit: it
// rejects immediately rather than queueing, so one account's burst can
// never pile up waiting goroutines at the expense of others. Deliberately
// simpler than a full weighted-fair-queue scheduler — this gets most of
// the benefit (no starvation) at a fraction of the complexity, appropriate
// for the platform's current scale.
func (g *Gateway) acquireAccountSlot(accountID, modelRoute string) (func(), error) {
	key := accountModelKey{accountID: accountID, modelRoute: modelRoute}

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.accountInFlight[key] >= defaultPerAccountCap {
		return nil, fmt.Errorf("%w: account already has %d requests in flight against %q",
			ErrThrottled, defaultPerAccountCap, modelRoute)
	}
	g.accountInFlight[key]++
	return func() {
		g.mu.Lock()
		g.accountInFlight[key]--
		g.mu.Unlock()
	}, nil
}

// providerFor returns a cached Provider for a node_service, rebuilding it
// if this is the first request since the row was mounted or its config
// changed (e.g. re-mounted with a different base_url) — config changes
// invalidate the cache instead of silently keeping a stale HTTP client.
func (g *Gateway) providerFor(nsID uuid.UUID, cfg ModelServiceConfig) (inference.Provider, error) {
	hash := configHash(cfg)

	g.mu.Lock()
	if cached, ok := g.providers[nsID]; ok && cached.configHash == hash {
		g.mu.Unlock()
		return cached.provider, nil
	}
	g.mu.Unlock()

	p, err := g.newProvider(cfg)
	if err != nil {
		return nil, err
	}

	g.mu.Lock()
	g.providers[nsID] = cachedProvider{configHash: hash, provider: p}
	g.mu.Unlock()
	return p, nil
}

func configHash(cfg ModelServiceConfig) string {
	return fmt.Sprintf("%s|%s|%s|%s|%d", cfg.Engine, cfg.BaseURL, cfg.BackendModel, cfg.APIKey, cfg.MaxConcurrency)
}
