// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/billing"
	"github.com/FlashbackAi/teepin-core/pkg/cluster"
	"github.com/FlashbackAi/teepin-core/pkg/inference"
)

// ProvisionGate answers whether an account may create resources right
// now — the same "no validated payment method, no resources" check
// compute provisioning already enforces (pkg/api.ProvisionGate).
// Duplicated as an interface here, rather than importing pkg/api, because
// pkg/api imports pkg/kumbha for the HTTP handlers — importing back would
// be a cycle. Implemented by billing.Service, same concrete type either
// side of the boundary uses.
type ProvisionGate interface {
	AccountCanProvision(ctx context.Context, accountID uuid.UUID) (bool, string, error)
}

// PricingProvider supplies the live Kumbha Gateway rates. Read fresh on
// every call by the concrete implementation (billing.Service) — same
// contract as every other rate in the platform: a price change must apply
// to the very next request, never a cached quote.
type PricingProvider interface {
	LLMPriceInputPerMillion(ctx context.Context) float64
	LLMPriceOutputPerMillion(ctx context.Context) float64
}

// ModelPricingProvider supplies per-route customer-facing rates —
// implemented by modelcatalog.Service. Optional: a Gateway without one
// (WithModelPricing never called) always falls back to PricingProvider's
// single flat rate, same as before this existed. Set on one, this is
// preferred over the flat rate for any route that has a catalog entry,
// because different backends behind Kumbha cost wildly different real
// amounts per token — see cost()'s own doc comment.
type ModelPricingProvider interface {
	ModelPricing(ctx context.Context, modelRoute string) (input, output float64, ok bool)
}

// ModelVendorCostProvider supplies what a route costs Teepin per million tokens
// (nil for either when unrecorded or when Teepin runs the model itself).
// Implemented by modelcatalog.Service; optional, and used only to record the
// margin on each usage line, never to price anything for the customer.
type ModelVendorCostProvider interface {
	ModelVendorCost(ctx context.Context, modelRoute string) (input, output *float64)
}

// CreditChecker answers whether an account can pay for a request before it
// is served. Implemented by billing.Service. Optional on the Gateway.
type CreditChecker interface {
	CanAfford(ctx context.Context, accountID uuid.UUID, worstCase float64) (bool, float64, error)
}

// completionOutputReserveTokens is how much output a completion's pre-flight
// assumes when the request does not bound it tighter. The difference on an
// unusually long answer is absorbed by the balance floor (ConsumeCredit
// never draws more than the balance).
const completionOutputReserveTokens = 2048

const (
	// completionSettleTimeout bounds recording a served completion's usage.
	completionSettleTimeout = 15 * time.Second
	// settleAttempts is how many times each recording step is tried.
	settleAttempts = 3
)

// settleRetryDelay is the pause between recording attempts. A variable so
// tests need not wait.
var settleRetryDelay = 200 * time.Millisecond

// UsageRecorder is the subset of billing.Service the Gateway needs at
// session close: write the ledger rows and draw down credit against them.
// An interface so kumbha's tests do not require a full *billing.Service.
type UsageRecorder interface {
	RecordUsage(ctx context.Context, record *billing.UsageRecord) error
	ConsumeCredit(ctx context.Context, accountID, usageRecordID uuid.UUID, cost float64) (float64, error)
}

var (
	// ErrGateUnavailable means the payment-status check itself failed —
	// fails CLOSED (refuses the session) rather than opening an unmetered
	// hole on a database blip, same posture as compute's payment gate.
	ErrGateUnavailable = errors.New("unable to verify billing status")
	// ErrPaymentRequired means the gate explicitly refused — no validated
	// payment method, or a non-active account.
	ErrPaymentRequired = errors.New("payment method required")
	// ErrAgentNotConfigured means LaunchAgent was called on a Gateway
	// built without WithAgent — e.g. before the Kumbha agent image exists
	// (see the plan's M3). Distinct from a runtime launch failure so a
	// caller can tell "not available on this deployment" from "briefly
	// failed, maybe retry".
	ErrAgentNotConfigured = errors.New("kumbha agent is not configured on this deployment")
	// ErrAgentNotRunning means StopAgent was called on a session with no
	// live agent pod — nothing to interrupt. Distinct from
	// ErrAgentNotConfigured (no agent capability at all) so the HTTP layer
	// can return a clear "nothing running" 409 rather than a confusing
	// "not available on this deployment".
	ErrAgentNotRunning = errors.New("no agent is currently running for this session")
	// ErrAgentRouteUnavailable means LaunchAgent's pre-flight check found no
	// model enabled for Kumbha in the catalog — the launch is refused before
	// a pod is ever created, rather than producing a pod that is guaranteed
	// to fail its first completion silently. The HTTP layer responds with a
	// generic message: which models back Kumbha is operator-only.
	ErrAgentRouteUnavailable = errors.New("no model is currently available for the build agent")

	// errNoKumbhaModels means the catalog has no model enabled for Kumbha.
	errNoKumbhaModels = fmt.Errorf("%w: no model is enabled for Kumbha", inference.ErrProviderUnavailable)

	// ErrModelUnavailable means the chosen model cannot serve right now (its
	// backend is not running or failed its health check). A build started on
	// it would fail at the first step, so it is refused up front instead.
	ErrModelUnavailable = fmt.Errorf("%w: the selected model is not available right now", inference.ErrProviderUnavailable)
)

// Gateway is the Kumbha Gateway's business logic — the request lifecycle
// in KUMBHA-DESIGN.md, minus the HTTP transport (pkg/api/kumbha_handlers.go
// owns stages 1-2 and 10-11; this is stages 3-9).
type Gateway struct {
	// zeroPriceWarned rate-limits the warning about a model billed at $0, so a
	// busy build does not repeat it on every completion.
	zeroMu          sync.Mutex
	zeroPriceWarned map[string]time.Time

	// contextSeen remembers, per session, how large the last request to the model
	// was, so the console can show how full the builder's context is. A display
	// aid, not a record: it lives in memory, so a restart forgets it and the next
	// completion fills it in again.
	contextMu   sync.Mutex
	contextSeen map[uuid.UUID]contextReading

	// agentLaunched, if set, is told about every agent launch (see
	// WithAgentLaunchHook in recorder.go).
	agentLaunched func(sess *Session, launchSeq int)

	store   *Store
	models  ModelBackend
	gate    ProvisionGate
	pricing PricingProvider
	usage   UsageRecorder

	// cluster/mintToken/agentConfig back LaunchAgent (agent.go) — all
	// nil/zero until WithAgent is called, which is expected to stay true
	// until the Kumbha agent image exists.
	cluster     cluster.Client
	mintToken   TokenMinter
	agentConfig AgentConfig

	// modelPricing is OPTIONAL — nil means cost() always uses pricing's
	// flat platform-wide rate. Set via WithModelPricing once a
	// modelcatalog.Service exists.
	modelPricing ModelPricingProvider

	// vendorCost is OPTIONAL: when set, each completion's usage lines carry what
	// they cost Teepin (cost_basis), so margin can be read off the ledger.
	vendorCost ModelVendorCostProvider

	// credit is OPTIONAL — nil means completions are not checked against the
	// account's credit balance (tests, standalone mode). Set via
	// WithCreditGuard.
	credit CreditChecker

	// nodeCapacity is OPTIONAL — nil means LaunchAgent/CaptureScreenshot
	// fall back to cluster.Client's own placement (registry.Any() for the
	// tunnel-based home path, a random pick among every connected node
	// with zero regard for free CPU/memory). Set via WithNodeCapacity.
	nodeCapacity NodeCapacityLister
}

// NewGateway builds a Gateway. models may be nil for a deployment that only
// manages sessions (every completion and agent launch then reports no model
// available).
func NewGateway(store *Store, models ModelBackend, gate ProvisionGate, pricing PricingProvider, usage UsageRecorder) *Gateway {
	return &Gateway{store: store, models: models, gate: gate, pricing: pricing, usage: usage}
}

// WithVendorCost records what each completion cost Teepin, from the model
// catalog's vendor costs, on its usage lines.
func (g *Gateway) WithVendorCost(p ModelVendorCostProvider) *Gateway {
	g.vendorCost = p
	return g
}

// WithModelPricing enables per-model pricing in cost(), preferred over the
// flat platform-wide rate for any model registered in the catalog. Returns
// the same *Gateway for chaining, matching WithAgent's own shape.
func (g *Gateway) WithModelPricing(p ModelPricingProvider) *Gateway {
	g.modelPricing = p
	return g
}

// WithCreditGuard makes Complete refuse (billing.ErrInsufficientCredit) a
// completion the account's credit cannot pay for. A session budget only caps
// what one build may spend; it says nothing about whether the account has
// the money, so without this a build could keep spending at a zero balance.
func (g *Gateway) WithCreditGuard(c CreditChecker) *Gateway {
	g.credit = c
	return g
}

// WithNodeCapacity enables capacity-aware placement for the agent's own
// pods (LaunchAgent, CaptureScreenshot) — picking a home node that
// actually has room, instead of cluster.Registry.Any()'s blind random
// pick among every connected node regardless of free CPU/memory. Returns
// the same *Gateway for chaining, matching every other With* builder here.
func (g *Gateway) WithNodeCapacity(lister NodeCapacityLister) *Gateway {
	g.nodeCapacity = lister
	return g
}

// CreateSession pre-authorises a new build session, gated by the same
// payment check compute provisioning already enforces: an account that
// cannot create an instance cannot start a Kumbha session either.
//
// modelRoute is the exact catalog model_route a customer picked from the
// Kumbha model picker; "" defers to whichever kumbha-enabled model has the
// lowest kumbha_priority (an operator-set "recommended default," not a
// failover order — see resolveModel's own doc comment). The resolved
// route is bound to the session for its whole lifetime; every completion
// within it uses exactly this model, never a different one Kumbha
// silently substitutes.
func (g *Gateway) CreateSession(ctx context.Context, accountID, projectID uuid.UUID, budget float64, label, modelRoute string) (*Session, error) {
	resolved, err := g.resolveModel(ctx, modelRoute)
	if err != nil {
		return nil, err
	}
	if g.gate != nil {
		allowed, reason, err := g.gate.AccountCanProvision(ctx, accountID)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrGateUnavailable, err)
		}
		if !allowed {
			return nil, fmt.Errorf("%w: %s", ErrPaymentRequired, reason)
		}
	}
	return g.store.Create(ctx, accountID, projectID, budget, label, resolved.Route)
}

// resolveModel validates a customer's chosen model route against the
// currently kumbha-enabled set, or — if none was given — picks the
// lowest-kumbha_priority one as the default. Never returns a DIFFERENT
// model than what was actually requested: a caller naming an unknown or
// no-longer-enabled route gets a clear error, not a silent substitution.
func (g *Gateway) resolveModel(ctx context.Context, requestedRoute string) (Model, error) {
	models, err := g.kumbhaModels(ctx)
	if err != nil {
		return Model{}, err
	}
	if requestedRoute == "" {
		// The lowest-priority model that can serve right now (the list is in
		// kumbha_priority order), never one that is down.
		for _, m := range models {
			if m.Unavailable == "" {
				return m, nil
			}
		}
		return Model{}, ErrModelUnavailable
	}
	for _, m := range models {
		if m.Route == requestedRoute {
			if m.Unavailable != "" {
				return Model{}, fmt.Errorf("%w: %s (%s)", ErrModelUnavailable, m.DisplayName, m.Unavailable)
			}
			return m, nil
		}
	}
	return Model{}, fmt.Errorf("%w: %q", inference.ErrUnknownModel, requestedRoute)
}

// GetSession loads a session scoped to its owning account.
func (g *Gateway) GetSession(ctx context.Context, id, accountID uuid.UUID) (*Session, error) {
	return g.store.Get(ctx, id, accountID)
}

// IncreaseBudget raises an open session's pre-authorised spend cap — the
// console's "raise budget" control (build/[id]/budget-meter.tsx),
// replacing the old up-front budget picker on the composer: a customer
// cannot sensibly judge a build's cost before it has started, so every
// session now starts at a fixed default and asks for more only once
// there is real spend to judge it against. Same payment-gate check
// CreateSession applies — more authorised spend is the same kind of
// commitment a new session's own budget is.
//
// Loads the session first (rather than leaving the "was it too low"
// distinction to a SQL WHERE clause's RowsAffected) so the caller gets a
// specific, actionable error — ErrBudgetNotIncreased vs ErrSessionNotFound
// vs ErrSessionClosed — instead of one ambiguous failure for all three.
func (g *Gateway) IncreaseBudget(ctx context.Context, id, accountID uuid.UUID, newBudget float64) error {
	if g.gate != nil {
		allowed, reason, err := g.gate.AccountCanProvision(ctx, accountID)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrGateUnavailable, err)
		}
		if !allowed {
			return fmt.Errorf("%w: %s", ErrPaymentRequired, reason)
		}
	}

	sess, err := g.store.Get(ctx, id, accountID)
	if err != nil {
		return err
	}
	if sess.Status != "open" {
		return ErrSessionClosed
	}
	if newBudget <= sess.Budget {
		return ErrBudgetNotIncreased
	}

	return g.store.IncreaseBudget(ctx, id, accountID, newBudget)
}

// ListSessions returns a project's Kumbha build history, most recent
// first.
func (g *Gateway) ListSessions(ctx context.Context, accountID, projectID uuid.UUID) ([]*Session, error) {
	return g.store.ListByProject(ctx, accountID, projectID)
}

// DeleteSessions removes the given sessions from the account's build
// history — a customer explicitly requesting a delete (through a
// confirming UI, see build/page.tsx's BulkDeleteDialog) is authorization
// to stop it too if it is still building, not just to clean up ones that
// already finished. There is no separate "stop, then delete" step:
// found live 2026-08-26 that requiring one is worse UX for no real safety
// gain, since the confirmation dialog already IS the "are you sure" gate
// — the same one-step pattern GitHub Actions/Vercel/Render use for
// cancelling a running job.
//
// For each requested id still marked "open" in the DB, this closes it
// first via CloseSession — settling any unbilled usage and tearing down
// the agent pod, exactly as an explicit customer Close already does —
// before Store.Delete removes the row. This also fixes a related bug
// found the same day: the stored status column alone is not trustworthy
// evidence of a live pod (nothing closes a session whose pod died on its
// own — crashed, evicted, or exited after its own idle timeout — without
// ever reaching CloseSession), so without this step an account's history
// could get stuck showing "Building" indefinitely and be entirely
// undeletable.
func (g *Gateway) DeleteSessions(ctx context.Context, accountID uuid.UUID, ids []uuid.UUID) ([]uuid.UUID, error) {
	for _, id := range ids {
		sess, err := g.store.Get(ctx, id, accountID)
		if err != nil || sess.Status != "open" {
			continue // not found, wrong account, or already closed — Store.Delete handles it
		}
		if _, err := g.CloseSession(ctx, id, accountID, "closed"); err != nil {
			// Best-effort, same posture as CloseSession's own settlement
			// errors: log for an operator, but still let Store.Delete
			// below decide this row's fate on its now-updated status.
			log.Printf("WARN: closing Kumbha session %s before delete had settlement errors: %v", id, err)
		}
	}
	return g.store.Delete(ctx, accountID, ids)
}

// SaveWorkspaceVersion appends a new workspace version for a session and
// moves the current-version pointer to it. createdBy distinguishes an
// agent's automatic save (after a file_editor call) from a customer's
// explicit edit-and-save in the console IDE — both go through this one
// path, since both are just "a new version," differing only in who made
// it. Returns the new version number.
func (g *Gateway) SaveWorkspaceVersion(ctx context.Context, sessionID uuid.UUID, files []WorkspaceFile, skipped []SkippedFile, createdBy CreatedBy) (int, error) {
	return g.store.SaveVersion(ctx, sessionID, files, skipped, createdBy)
}

// CurrentWorkspace returns whatever version is currently live for a
// session, scoped to the owning account — what the file browser and ZIP
// download show by default.
func (g *Gateway) CurrentWorkspace(ctx context.Context, sessionID, accountID uuid.UUID) (*Snapshot, error) {
	return g.store.GetCurrentVersion(ctx, sessionID, accountID)
}

// WorkspaceVersion returns one specific version, scoped to the owning
// account — used to view or download an older version before deciding
// whether to roll back to it.
func (g *Gateway) WorkspaceVersion(ctx context.Context, sessionID, accountID uuid.UUID, version int) (*Snapshot, error) {
	return g.store.GetVersion(ctx, sessionID, accountID, version)
}

// WorkspaceHistory returns every checkpointed version's metadata (no file
// content), newest first, scoped to the owning account — the console's
// version history list.
func (g *Gateway) WorkspaceHistory(ctx context.Context, sessionID, accountID uuid.UUID) ([]VersionInfo, error) {
	return g.store.ListVersions(ctx, sessionID, accountID)
}

// CheckpointWorkspace marks the session's current draft workspace version
// as a permanent, customer-visible checkpoint — called once, right after
// a Kumbha deploy actually succeeds (see DeployKumbhaSession). See
// Store.CheckpointCurrentVersion for the full reasoning.
func (g *Gateway) CheckpointWorkspace(ctx context.Context, sessionID uuid.UUID) error {
	return g.store.CheckpointCurrentVersion(ctx, sessionID)
}

// PollMessages returns and marks-delivered every undelivered follow-up
// message for a session — called by the agent pod's own poll loop
// (run.py's wait_for_next_instruction), authenticated with its
// session-scoped credential, not a customer JWT.
func (g *Gateway) PollMessages(ctx context.Context, sessionID uuid.UUID) ([]Message, error) {
	return g.store.PollMessages(ctx, sessionID)
}

// SetAppInstanceID records the compute instance a session's Deploy most
// recently created — see migration 026 and Session.AppInstanceID's own
// doc comment.
func (g *Gateway) SetAppInstanceID(ctx context.Context, sessionID uuid.UUID, instanceID string) error {
	return g.store.SetAppInstanceID(ctx, sessionID, instanceID)
}

// AcquireDeployLock/ReleaseDeployLock expose Store's own methods of the
// same name — see AcquireDeployLock's own doc comment for the incident
// this closes (two overlapping deploy calls for one session racing each
// other's outcome).
func (g *Gateway) AcquireDeployLock(ctx context.Context, sessionID uuid.UUID, staleAfter time.Duration) (bool, error) {
	return g.store.AcquireDeployLock(ctx, sessionID, staleAfter)
}

func (g *Gateway) ReleaseDeployLock(ctx context.Context, sessionID uuid.UUID) error {
	return g.store.ReleaseDeployLock(ctx, sessionID)
}

// GetGithubRepo/SetGithubRepo expose Store's own methods of the same
// name — see their doc comments (pkg/kumbha/session.go) for why this is
// deliberately not a Session field.
func (g *Gateway) GetGithubRepo(ctx context.Context, sessionID uuid.UUID) (string, error) {
	return g.store.GetGithubRepo(ctx, sessionID)
}

func (g *Gateway) SetGithubRepo(ctx context.Context, sessionID uuid.UUID, repo string) error {
	return g.store.SetGithubRepo(ctx, sessionID, repo)
}

// SaveScreenshot records the deployed app's most recently captured
// screenshot — called only by the screenshot pod's own upload endpoint,
// see Store.SaveScreenshot.
func (g *Gateway) SaveScreenshot(ctx context.Context, sessionID uuid.UUID, png []byte) error {
	return g.store.SaveScreenshot(ctx, sessionID, png)
}

// SetLastDeployStatus records the outcome of a session's most recent
// build/deploy attempt — see Store.SetLastDeployStatus.
func (g *Gateway) SetLastDeployStatus(ctx context.Context, sessionID uuid.UUID, errMsg string) error {
	return g.store.SetLastDeployStatus(ctx, sessionID, errMsg)
}

// Screenshot returns a session's most recently captured screenshot,
// scoped to the owning account — see Store.GetScreenshot.
func (g *Gateway) Screenshot(ctx context.Context, sessionID, accountID uuid.UUID) ([]byte, time.Time, error) {
	return g.store.GetScreenshot(ctx, sessionID, accountID)
}

// RollbackWorkspace moves the current-version pointer to an existing,
// older (or newer) version — the undo for a customer edit or an agent
// step that broke something. Does not delete or overwrite anything: the
// version being rolled back FROM is still there, so a rollback can itself
// be undone by rolling forward again.
func (g *Gateway) RollbackWorkspace(ctx context.Context, sessionID, accountID uuid.UUID, version int) error {
	return g.store.SetCurrentVersion(ctx, sessionID, accountID, version)
}

// ApproveDeploy flips a session's pre-deploy cost-approval gate — see
// KUMBHA-DESIGN.md's "Pre-deploy cost approval" section. Called by the
// console when the customer approves the itemised Deployment Plan; the
// teepin-mcp-server's provisioning verbs (create_instance, deploy,
// attach_domain) check this via GetSession before making any real API
// call, so this is a hard backend gate a prompt-injected agent cannot
// talk its way past, not a UI-only confirmation.
func (g *Gateway) ApproveDeploy(ctx context.Context, id, accountID uuid.UUID) error {
	return g.store.SetDeployApproved(ctx, id, accountID)
}

// CompletionResult is what the HTTP handler needs to build its response —
// the provider's response plus the figures for the X-Teepin-Cost /
// X-Teepin-Session-Spent headers the design doc specifies.
type CompletionResult struct {
	Response *inference.Response
	Cost     float64
	Spent    float64
	Budget   float64
}

// kumbhaModels returns the catalog models backing alias, in order, or
// errNoKumbhaModels when there are none.
func (g *Gateway) kumbhaModels(ctx context.Context) ([]Model, error) {
	if g.models == nil {
		return nil, errNoKumbhaModels
	}
	models, err := g.models.KumbhaModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: listing Kumbha's models: %v", inference.ErrProviderUnavailable, err)
	}
	if len(models) == 0 {
		return nil, errNoKumbhaModels
	}
	return models, nil
}

// Complete runs stages 3-9 of the request lifecycle: check budget,
// dispatch, capture tokens, compute cost, accrue.
//
// req.Model must be the EXACT route sess was created with (sess.ModelRoute)
// — there is no failover to a different model here. That is deliberate:
// the earlier alias-bucket design could fail over from a customer's
// deliberately-chosen model (e.g. a confidential one) to a DIFFERENT model
// with different guarantees, silently. A failure here is surfaced to the
// caller as a real error on the model they actually picked, not routed
// around.
//
// A pre-flight budget check happens here (sess.Spent >= sess.Budget) so an
// already-exhausted session is refused before spending a round trip on a
// provider call; the authoritative check is still Accrue's atomic
// compare-under-lock afterward, since the pre-flight value can be stale by
// the time dispatch completes under concurrent requests.
func (g *Gateway) Complete(ctx context.Context, sess *Session, req inference.Request) (*CompletionResult, error) {
	if sess.Status != "open" {
		return nil, ErrSessionClosed
	}
	if sess.Spent >= sess.Budget {
		return nil, ErrBudgetExhausted
	}
	if req.Model != sess.ModelRoute {
		return nil, fmt.Errorf("%w: session is bound to %q, not %q", inference.ErrUnknownModel, sess.ModelRoute, req.Model)
	}
	return g.serve(ctx, sess, sess.ModelRoute, "", true, req)
}

// serve runs one completion on route and bills it to the session: the credit
// check, the dispatch, the cost, the session's spend and the usage lines. It is
// the whole of Complete after the route check, so the builder's own calls and the
// image reader's (a different model, billed to the same build) are metered the
// same way. engine, when given, names the serving backend for the usage record;
// empty looks it up from the builder list. countsAsContext says whether the
// request's size is the builder's context reading.
func (g *Gateway) serve(ctx context.Context, sess *Session, route, engine string, countsAsContext bool, req inference.Request) (*CompletionResult, error) {
	// A Gateway built with models=nil (see NewGateway's own doc comment: "a
	// deployment that only manages sessions") can never actually reach here
	// in production — CreateSession's resolveModel already refuses to bind
	// a session to any model without a real backend to list from — but
	// guard it explicitly anyway rather than let g.models.Complete below
	// panic on a nil interface.
	if g.models == nil {
		return nil, errNoKumbhaModels
	}

	// Refuse before spending tokens on an account that cannot pay for them.
	// Fails closed: an unreadable balance refuses the completion (the agent
	// harness retries), it does not open an unmetered hole.
	if g.credit != nil {
		inRate, outRate := g.rates(ctx, route)
		out := req.MaxTokens
		if out <= 0 || out > completionOutputReserveTokens {
			out = completionOutputReserveTokens
		}
		worst := float64(inference.EstimateTokens(req))/1e6*inRate + float64(out)/1e6*outRate
		ok, _, err := g.credit.CanAfford(ctx, sess.AccountID, worst)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrGateUnavailable, err)
		}
		if !ok {
			return nil, billing.ErrInsufficientCredit
		}
	}

	// Engine is looked up only for the audit/margin column, from the SAME
	// kumbha-enabled list the picker uses — a model an operator disabled
	// for Kumbha mid-session (rare) falls back to an empty engine string
	// rather than blocking an otherwise-servable completion; the
	// underlying gateway call below doesn't consult kumbha_enabled at all.
	if engine == "" {
		if models, err := g.kumbhaModels(ctx); err == nil {
			for _, m := range models {
				if m.Route == route {
					engine = m.Engine
					break
				}
			}
		}
	}

	start := time.Now()
	resp, err := g.models.Complete(ctx, sess.AccountID.String(), req)
	if err != nil {
		return nil, err
	}
	end := time.Now()

	cost := g.cost(ctx, route, resp.Usage)
	if cost == 0 && resp.Usage.InputTokens+resp.Usage.OutputTokens > 0 {
		g.warnZeroPrice(route, resp.Usage)
	}

	// From here the model has answered and its tokens are spent, so nothing
	// below may turn the response into an error. The harness retries a failed
	// completion (up to 5 times), and each retry runs the model and bills
	// again - a bookkeeping hiccup used to charge the same work repeatedly
	// while the customer received nothing. Bookkeeping therefore retries
	// itself and, if it still cannot finish, is logged for reconciliation
	// and the answer is returned anyway.
	//
	// Runs on a detached context so a client hanging up cannot cancel the
	// writes that bill for tokens already generated.
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), completionSettleTimeout)
	defer cancel()

	newSpent, err := g.store.Accrue(bctx, sess.ID, sess.AccountID, cost,
		req.Model, engine, resp.Usage.InputTokens, resp.Usage.OutputTokens)
	if errors.Is(err, ErrBudgetExhausted) {
		// This answer pushed the session past its budget. The model has already
		// answered, so the tokens are owed: record them anyway (the budget stops
		// the NEXT completion, up front) rather than refuse a response that was
		// paid for and would only be retried.
		newSpent, err = g.store.AccrueServed(bctx, sess.ID, sess.AccountID, cost,
			req.Model, engine, resp.Usage.InputTokens, resp.Usage.OutputTokens)
	}
	if err != nil {
		// Whatever the reason (a recording fault, or the session closed or went
		// away while the model was working), the account still pays for tokens
		// that were generated, and the answer is still delivered.
		log.Printf("ERROR: kumbha session %s: completion served but its spend was not recorded on the session (cost %.6f): %v", sess.ID, cost, err)
		newSpent = sess.Spent + cost
	}

	// Settled immediately, per completion - not batched until the session
	// closes. Billing is metered per-token already; a customer's ability to
	// keep chatting (and thus keep building) must not depend on an explicit
	// "close this session" action, and batching to Close left an abandoned
	// session's spend unrecorded indefinitely. See CloseSession's own doc
	// comment: settlement no longer happens there at all, so a session's
	// `spent` counter and its invoice-visible cost never diverge.
	inRate, outRate := g.rates(bctx, route)
	var inBasis, outBasis float64
	if g.vendorCost != nil {
		// What this completion cost Teepin, not the customer: input counts the
		// prompt-cache discount. Zero when the model is self-hosted or its vendor
		// cost is not recorded (unattributed, never a made-up figure).
		if vin, vout := g.vendorCost.ModelVendorCost(bctx, route); vin != nil || vout != nil {
			if vin != nil {
				inBasis = inference.VendorInputCost(resp.Usage, *vin)
			}
			if vout != nil {
				outBasis = float64(resp.Usage.OutputTokens) / 1e6 * *vout
			}
		}
	}
	if resp.Usage.InputTokens > 0 {
		if err := g.settleLine(bctx, sess, req.Model+":input", engine,
			float64(resp.Usage.InputTokens), float64(resp.Usage.InputTokens)/1e6*inRate, inBasis, start, end); err != nil {
			log.Printf("ERROR: kumbha session %s: completion served but its input usage was not billed: %v", sess.ID, err)
		}
	}
	if resp.Usage.OutputTokens > 0 {
		if err := g.settleLine(bctx, sess, req.Model+":output", engine,
			float64(resp.Usage.OutputTokens), float64(resp.Usage.OutputTokens)/1e6*outRate, outBasis, start, end); err != nil {
			log.Printf("ERROR: kumbha session %s: completion served but its output usage was not billed: %v", sess.ID, err)
		}
	}

	if countsAsContext {
		g.noteContext(sess.ID, resp.Usage.InputTokens)
	}

	return &CompletionResult{Response: resp, Cost: cost, Spent: newSpent, Budget: sess.Budget}, nil
}

type contextReading struct {
	tokens int
	at     time.Time
}

// contextReadingTTL is how long an unrefreshed reading is kept; a build idle
// for longer than this no longer has a meaningful "current" context size.
const contextReadingTTL = 6 * time.Hour

// noteContext records the size of the request just sent for a session.
func (g *Gateway) noteContext(id uuid.UUID, tokens int) {
	if tokens <= 0 {
		return
	}
	g.contextMu.Lock()
	defer g.contextMu.Unlock()
	if g.contextSeen == nil {
		g.contextSeen = make(map[uuid.UUID]contextReading)
	}
	now := time.Now()
	if len(g.contextSeen) > 1024 { // bound the map: drop what has gone stale
		for k, v := range g.contextSeen {
			if now.Sub(v.at) > contextReadingTTL {
				delete(g.contextSeen, k)
			}
		}
	}
	g.contextSeen[id] = contextReading{tokens: tokens, at: now}
}

// ContextUsage returns how many tokens the session's last request to the model
// held and the model's window (0 when the catalog does not know it). ok is false
// when nothing recent is known, which a caller should show as "unknown", not 0.
func (g *Gateway) ContextUsage(ctx context.Context, sess *Session) (tokens, window int, ok bool) {
	g.contextMu.Lock()
	r, seen := g.contextSeen[sess.ID]
	g.contextMu.Unlock()
	if !seen || time.Since(r.at) > contextReadingTTL {
		return 0, 0, false
	}
	window, _ = g.routeLimits(ctx, sess.ModelRoute)
	return r.tokens, window, true
}

// zeroPriceWarnEvery is how often a model billed at $0 is reported.
const zeroPriceWarnEvery = time.Hour

// warnZeroPrice reports, at most once an hour per model, that a completion was
// served and priced at $0, so nothing was drawn from the customer's credit.
// A price of $0 is a valid choice for a deliberately free model, but it is also
// the default for every new catalog entry, and a model left at the default is
// otherwise given away without a trace.
func (g *Gateway) warnZeroPrice(route string, usage inference.Usage) {
	g.zeroMu.Lock()
	defer g.zeroMu.Unlock()
	now := time.Now()
	if last, ok := g.zeroPriceWarned[route]; ok && now.Sub(last) < zeroPriceWarnEvery {
		return
	}
	if g.zeroPriceWarned == nil {
		g.zeroPriceWarned = make(map[string]time.Time)
	}
	g.zeroPriceWarned[route] = now
	log.Printf("WARN: kumbha: model %q served %d tokens but is priced at $0, so no credit was consumed; set its price in the model catalog if this is not intended",
		route, usage.InputTokens+usage.OutputTokens)
}

// rates returns the per-million-token input/output rates to charge for a
// completion served by the catalog model modelRoute. Prefers that model's
// own customer rate from the catalog — different models behind Kumbha cost
// wildly different real amounts per token, so pricing them all off one
// flat platform-wide rate silently mis-bills the moment a second,
// differently-priced model goes live (found live 2026-09-22: build-session
// traffic moved to a paid Anthropic model while still priced off the same
// flat rate as the self-hosted one). Falls back to the flat rate for a
// model with no catalog entry, rather than charging nothing for it. Factored out of cost() so
// the settlement lines in Complete (the actual invoice-visible credit
// consumption) are always computed from the exact same rates as the
// session's own spent/budget figure — those diverging would mean a
// customer's account credit gets debited at a different rate than what
// their build session shows.
func (g *Gateway) rates(ctx context.Context, modelRoute string) (input, output float64) {
	if g.modelPricing != nil {
		if in, out, ok := g.modelPricing.ModelPricing(ctx, modelRoute); ok {
			return in, out
		}
	}
	return g.pricing.LLMPriceInputPerMillion(ctx), g.pricing.LLMPriceOutputPerMillion(ctx)
}

// cost prices a completion's usage at rates()'s per-route rate.
func (g *Gateway) cost(ctx context.Context, modelRoute string, usage inference.Usage) float64 {
	inRate, outRate := g.rates(ctx, modelRoute)
	return float64(usage.InputTokens)/1e6*inRate + float64(usage.OutputTokens)/1e6*outRate
}

// CloseSession marks a session closed and tears down its agent pod.
//
// No longer settles any billing — Complete() settles every completion's
// cost into usage_records IMMEDIATELY as it happens (see its own doc
// comment on why: a customer's ability to keep chatting must not depend
// on an explicit close, and batching settlement to Close left an
// abandoned-but-never-closed session's spend sitting unrecorded
// indefinitely). Settling AGAIN here from RouteUsage's cumulative totals
// would double-charge the account for everything Complete() already
// settled. This is now purely internal bookkeeping — there is no
// customer-facing "Close session" action any more; the only caller is
// DeleteSessions' own stop-then-delete step, closing a still-open session
// immediately before removing its row.
func (g *Gateway) CloseSession(ctx context.Context, id, accountID uuid.UUID, reason string) (*Session, error) {
	sess, err := g.store.Close(ctx, id, accountID, reason)
	if err != nil {
		return nil, err
	}

	// The agent pod is Kumbha's own workload (see LaunchAgent's doc
	// comment) — nothing else deletes it, so a session that never tears
	// its pod down here leaks it permanently. The customer never sees or
	// manages it either way.
	if sess.AgentInstanceID != "" && g.cluster != nil {
		if err := g.cluster.DeleteInstance(ctx, cluster.ProjectScope(sess.ProjectID.String()), sess.AgentInstanceID); err != nil {
			return sess, fmt.Errorf("failed to tear down agent pod %s: %w", sess.AgentInstanceID, err)
		}
	}

	return sess, nil
}

// StopAgent interrupts a session's currently-running agent pod immediately
// — the "Stop" action (replaces the old "Close session" button, which
// bundled an unrelated permanent chat-block into what was really just
// "I want this run to stop"). Deliberately a hard kill, not a graceful
// mid-turn pause: openhands-sdk exposes no confirmed public API for
// cleanly cancelling an in-flight conversation step (see
// deploy/kumbha-agent/run.py's own flagged uncertainty about this SDK's
// exact surface elsewhere), and guessing at one here would repeat exactly
// the class of mistake this codebase has been careful to avoid throughout
// Kumbha's build-out. Whatever the agent had written to the workspace
// (PVC) up to the moment of the kill is kept — DeliverMessage's own
// relaunch path already handles resuming from there with a fresh agent
// turn, the same as when a pod exits on its own idle timeout. The session
// itself is left "open": nothing about Stop should block a later message.
func (g *Gateway) StopAgent(ctx context.Context, sess *Session) error {
	if g.cluster == nil {
		return ErrAgentNotConfigured
	}
	// No agent pod ever recorded on this session is exactly as "nothing
	// running to interrupt" as one that was recorded but has since
	// exited — both are ErrAgentNotRunning, not ErrAgentNotConfigured
	// (reserved for "this platform has no agent capability at all").
	// isAgentRunning already treats sess.AgentInstanceID == "" as false
	// with no error, so this short-circuit just skips a redundant call.
	if sess.AgentInstanceID == "" {
		return ErrAgentNotRunning
	}
	running, err := g.isAgentRunning(ctx, sess)
	if err != nil {
		return fmt.Errorf("failed to check agent status: %w", err)
	}
	if !running {
		return ErrAgentNotRunning
	}
	return g.cluster.DeleteInstance(ctx, cluster.ProjectScope(sess.ProjectID.String()), sess.AgentInstanceID)
}

// settleLine writes one usage_records row for a session's (route,
// direction) and draws it down against the account's credits.
func (g *Gateway) settleLine(ctx context.Context, sess *Session, resourceType, provider string, quantity, totalCost, costBasis float64, start, end time.Time) error {
	record := &billing.UsageRecord{
		AccountID:    sess.AccountID,
		ProjectID:    sess.ProjectID,
		SubjectType:  "inference_session",
		SubjectID:    sess.ID.String(),
		ResourceType: "kumbha/" + resourceType,
		Quantity:     quantity,
		Unit:         "tokens",
		TotalCost:    totalCost,
		CostBasis:    costBasis,
		Provider:     provider,
		StartTime:    start,
		EndTime:      end,
	}
	if quantity > 0 {
		record.UnitPrice = totalCost / quantity * 1e6 // back into a per-million rate for display
	}
	// Each step retries on its own. RecordUsage creates the row only on
	// success (a failed attempt inserted nothing), so it is repeated until it
	// lands once; ConsumeCredit is idempotent per usage record, so repeating
	// it can never draw twice. Neither retry can double-charge.
	var err error
	for attempt := 0; attempt < settleAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(settleRetryDelay)
		}
		if err = g.usage.RecordUsage(ctx, record); err == nil {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("failed to record %s: %w", resourceType, err)
	}
	if totalCost > 0 {
		for attempt := 0; attempt < settleAttempts; attempt++ {
			if attempt > 0 {
				time.Sleep(settleRetryDelay)
			}
			if _, err = g.usage.ConsumeCredit(ctx, sess.AccountID, record.ID, totalCost); err == nil {
				break
			}
		}
		if err != nil {
			return fmt.Errorf("failed to consume credit for %s (usage record %s): %w", resourceType, record.ID, err)
		}
	}
	return nil
}
