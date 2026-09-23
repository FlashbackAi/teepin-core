// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/cluster"
)

// AgentConfig is the fixed platform policy every session's agent pod runs
// with — the image and resource sizing. Not customer-selectable: the
// agent's own compute is billed separately from whatever app it
// eventually deploys (see CloseSession's settlement), so there is nothing
// for a customer to size here, only an operator.
type AgentConfig struct {
	// Image is the Kumbha agent image (OpenHands + the teepin-mcp-server
	// binary — see the plan's M3). Required for LaunchAgent to do
	// anything; WithAgent's caller (main.go) is expected to leave the
	// whole capability unconfigured until this exists, the same way
	// execTickets/nodePlacer are conditionally wired.
	Image              string
	CPUUnits           int
	MemoryGB           int
	StorageGB          int
	EphemeralStorageGB int
	// SessionTokenTTL bounds the agent's own credential lifetime,
	// independent of the session's budget or idle timeout — belt and
	// braces alongside the session-open check every request already makes
	// (see auth.Middleware's session-scoped credential path).
	SessionTokenTTL time.Duration
	// APIBaseURL is the control-plane host the agent pod calls back to —
	// both the LLM's own base_url (run.py appends "/v1/kumbha") and the
	// teepin-mcp-server's target for every teepin.* API call. An in-cluster
	// service DNS name in production (e.g.
	// "http://teepin-api.default.svc.cluster.local:8080"), never a
	// customer-facing URL.
	APIBaseURL string
	// ImagePullSecret is the name of a Kubernetes docker-registry Secret,
	// already present in cluster.WorkloadNamespace on whichever cluster
	// LaunchAgent's pods land on, granting pull access to Image. Empty
	// means Image must be publicly pullable — the same "ships on, does
	// nothing until configured" contract as the rest of AgentConfig; there
	// is deliberately no provisioning path for this secret here (unlike a
	// customer's own registry via pkg/harbor.Service.ProvisionProjectRegistry,
	// which is keyed to a real auth.projects row) — Kumbha's own agent
	// image is a platform resource, not a per-customer one, so the Harbor
	// project/robot account/secret behind this name is operator-provisioned
	// once, out of band, directly against whichever cluster runs the pods.
	ImagePullSecret string
	// VisionCapable tells the agent, via a plain-text note prepended to
	// its initial prompt (see run.py's main()), whether it is safe to set
	// include_screenshot=True on the official browser_get_state tool
	// (openhands.tools.browser_use.BrowserToolSet) — that tool has no
	// operator-side gate of its own; the MODEL decides per call whether
	// to request an image. Operator-set, not auto-detected: sending an
	// image content block to a route whose model cannot handle
	// multimodal input is a real risk (a malformed/rejected request
	// breaks that whole turn, not just the screenshot itself — bounded
	// and recoverable, since the SDK already retries failed completions,
	// but still worth avoiding by default), so this defaults to false
	// until an operator has actually confirmed the hosted model supports
	// vision.
	VisionCapable bool
}

// TokenMinter mints the agent's own short-lived, session-scoped
// credential. A function type rather than an interface bound to
// auth.MintSessionToken directly: pkg/kumbha has no reason to import
// pkg/auth (or know what a JWT is) just to launch a pod, and this keeps
// the signing secret entirely inside main.go's composition root.
type TokenMinter func(accountID, projectID, sessionID uuid.UUID, ttl time.Duration) (string, error)

// agentLabel marks a pod as Kumbha's own workload — used only for
// operator visibility (e.g. filtering in kubectl); it plays no role in
// keeping the pod out of the customer's Compute list, which is achieved
// structurally by never inserting a compute.instances row for it (see
// LaunchAgent's doc comment).
const agentLabel = "teepin.io/kumbha-agent"

// AgentPodNamePrefix identifies a Kumbha agent pod by its instance ID —
// used by pkg/nodes' hidden-workload capacity accounting (see
// nodes.HiddenWorkloadCounter) to recognise one of these pods in a live
// status list without pkg/nodes needing to know anything else about how
// this package names things. Exported so this string is the only place
// it is ever duplicated. Matches LaunchAgent's own podID construction
// below.
const AgentPodNamePrefix = "kumbha-agent-"

// WithAgent enables LaunchAgent. Returns the same *Gateway for chaining,
// so existing NewGateway call sites compile unchanged — without this,
// LaunchAgent returns ErrAgentNotConfigured rather than launching
// anything, the same fail-closed shape as every other optional capability
// in this codebase (s.execTickets == nil, s.nodePlacer == nil, ...).
func (g *Gateway) WithAgent(client cluster.Client, mintToken TokenMinter, cfg AgentConfig) *Gateway {
	g.cluster = client
	g.mintToken = mintToken
	g.agentConfig = cfg
	return g
}

// LaunchAgent provisions the Kumbha agent pod for a session and records
// its identifier on the session row.
//
// The pod is created by calling cluster.Client.CreateInstance DIRECTLY —
// the same interface method the public POST /v1/compute/instances handler
// calls, reusing 100% of its pod-building machinery (SecurityContext,
// NetworkPolicy, PVC, ephemeral-storage cap) — but NOT through that public
// handler, and the pod is never inserted into compute.instances. This is
// deliberate: the agent's own workload is Kumbha's, not a resource the
// customer manages, so a mystery "kumbha-agent-xyz" entry never appears in
// their Compute list. What the customer DOES eventually manage is the
// finished app the agent creates via the real API once deploy is
// approved — that one lands in compute.instances exactly like any
// customer-created instance always has.
// internalScratchPathInstruction tells the agent where to keep its own
// working notes/memory instead of the workspace root, which IS the
// customer's actual deployed site and ZIP download. Prepended to every
// launch prompt — first build and every resume alike — since it is a
// standing behavioral instruction, not part of any one customer's
// specific request. This is the only lever teepin-core actually has over
// the agent's own behavior: the agent image's own system prompt is built
// and baked in elsewhere, a separate build this package has no access
// to. Deferred 2026-09-01 ("why is the agent writing its own AGENTS.md
// in the app codebase instead of keeping it hidden in the backend"),
// addressed 2026-09-02 — paired with a hard server-side filter
// (SaveKumbhaWorkspace's own doc comment) that strips anything under
// this path from what actually gets persisted, so a prompt the agent
// ignores still cannot leak internal notes into the customer's ZIP or
// version history.
const internalScratchPathInstruction = "Keep any of your own internal working notes, memory, or scratch " +
	"files in a \"" + InternalScratchDir + "/\" directory at the workspace root — never at the workspace " +
	"root itself, and never inside any directory your Dockerfile copies into the built image. Nothing under " +
	"\"" + InternalScratchDir + "/\" is ever shown to the customer, deployed, or included in their downloads " +
	"— it exists purely for your own use across relaunches of this same session.\n\n"

// checkModelsAvailable reports whether the catalog has at least one model
// enabled for Kumbha — run once up front by LaunchAgent so a build fails
// immediately with a clear error when there is nothing to serve it,
// instead of launching a pod that is guaranteed to fail its very first
// completion silently. Found live 2026-09-22: a build submitted while
// Kumbha's only backend was disabled launched its agent pod successfully,
// then never produced any output at all, with nothing in the console
// explaining why — the session just sat at "Idle" forever.
//
// Only an empty list refuses the launch. A failure to read the list fails
// open: a database blip should not block every build launch, and the
// agent's own first completion will surface a real outage anyway.
func (g *Gateway) checkModelsAvailable(ctx context.Context) error {
	_, err := g.kumbhaModels(ctx)
	if errors.Is(err, errNoKumbhaModels) {
		return err
	}
	if err != nil {
		log.Printf("WARN: could not list Kumbha's models before launch, allowing it: %v", err)
	}
	return nil
}

// CapacityCandidate is one node's identity plus free room — the minimum
// pickNodeWithCapacity needs, deliberately NOT pkg/nodes.NodeCapacity
// directly (that package's own HiddenWorkloadCounter doc comment already
// establishes the precedent: a small local interface/type here instead of
// importing pkg/nodes, the same reasoning NodePlacer/ProjectPolicy use
// elsewhere in this codebase — pkg/kumbha has no other reason to know
// pkg/nodes exists).
type CapacityCandidate struct {
	// ProviderID is what cluster.Registry keys live agent sessions by and
	// what InstanceSpec.ProviderID/cluster.ByProvider expect — the exact
	// identifier that turns "this node has room" into "dispatch here".
	ProviderID string
	FreeCPU    int
	FreeMemGB  int
}

// NodeCapacityLister is the subset of nodes.Service LaunchAgent/
// CaptureScreenshot need to place the agent's own pods on a node that
// actually has room, instead of cluster.Registry.Any()'s blind random pick
// among every connected home node with zero regard for free CPU/memory.
// Implemented by *nodes.Service via a small adapter in cmd/api-server
// (mirrors modelcatalog.Service's own ModelPricing method shape: the
// concrete service grows one small method rather than this package
// depending on the whole nodes package). Optional on Gateway — nil (no
// WithNodeCapacity call) keeps today's Any()-based behaviour unchanged.
type NodeCapacityLister interface {
	ListNodeCapacity(ctx context.Context) ([]CapacityCandidate, error)
}

// ErrNoCapacity means no currently-connected node has enough free CPU/
// memory for the agent's own pod — returned instead of dispatching blind
// and letting Kubernetes discover the shortfall itself, which is how a
// build previously sat silently Pending for however long an operator took
// to notice (found live 2026-09-22: a build landed on a home node with
// zero free memory purely by the luck of Go's randomized map iteration in
// registry.Any(), a node genuinely unable to run it, while two other
// connected nodes sat mostly idle at the same moment).
var ErrNoCapacity = errors.New("no connected node currently has room for the build agent")

// pickNodeWithCapacity returns the ProviderID of a connected node with at
// least cpuUnits/memoryGB free, preferring the one with the MOST free CPU
// (a simple least-loaded strategy — spreads load across nodes rather than
// packing them one at a time, and needs no more information than
// ListNodeCapacity already returns). Returns ("", nil) — not an error —
// when g.nodeCapacity is not configured at all, so callers fall back to
// cluster.Client's own placement exactly as before this existed.
func (g *Gateway) pickNodeWithCapacity(ctx context.Context, cpuUnits, memoryGB int) (string, error) {
	if g.nodeCapacity == nil {
		return "", nil
	}
	candidates, err := g.nodeCapacity.ListNodeCapacity(ctx)
	if err != nil {
		// Fails open to cluster.Client's own placement rather than
		// blocking every build launch over a capacity-lookup blip — the
		// worst case reverts to today's pre-existing random-pick
		// behaviour, not a new failure mode.
		log.Printf("WARN: could not list node capacity before launch, falling back to unweighted placement: %v", err)
		return "", nil
	}
	best := ""
	bestFreeCPU := -1
	for _, c := range candidates {
		if c.FreeCPU < cpuUnits || c.FreeMemGB < memoryGB {
			continue
		}
		if c.FreeCPU > bestFreeCPU {
			best = c.ProviderID
			bestFreeCPU = c.FreeCPU
		}
	}
	if best == "" {
		return "", ErrNoCapacity
	}
	return best, nil
}

func (g *Gateway) LaunchAgent(ctx context.Context, sess *Session, prompt string) error {
	if g.cluster == nil || g.mintToken == nil {
		return ErrAgentNotConfigured
	}

	if err := g.checkModelsAvailable(ctx); err != nil {
		// Logged for the operator; the HTTP layer answers
		// ErrAgentRouteUnavailable with a generic message.
		log.Printf("WARN: kumbha agent launch refused: %v", err)
		return ErrAgentRouteUnavailable
	}

	token, err := g.mintToken(sess.AccountID, sess.ProjectID, sess.ID, g.agentConfig.SessionTokenTTL)
	if err != nil {
		return fmt.Errorf("failed to mint agent credential: %w", err)
	}

	// Picks a node that actually has room instead of leaving it to
	// cluster.Client's own Any() (a blind random pick among every
	// connected node) — see pickNodeWithCapacity's own doc comment for
	// the live incident this fixes. Empty providerID (nodeCapacity not
	// configured) is passed through unchanged to InstanceSpec, which is
	// exactly today's pre-existing behaviour.
	providerID, err := g.pickNodeWithCapacity(ctx, g.agentConfig.CPUUnits, g.agentConfig.MemoryGB)
	if err != nil {
		return err
	}

	// Short and deterministic from the session id, matching the existing
	// "inst-<short-uuid>" convention compute instances already use, so an
	// operator recognises the shape immediately in kubectl/logs even
	// though this pod is never customer-visible.
	podID := AgentPodNamePrefix + sess.ID.String()[:8]

	spec := cluster.InstanceSpec{
		InstanceID: podID,
		AccountID:  sess.AccountID.String(),
		ProjectID:  sess.ProjectID.String(),
		ProviderID: providerID,
		Image:      g.agentConfig.Image,
		Env: map[string]string{
			// The agent's own credential and everything it needs to start
			// working — never a platform-wide token, never long-lived (see
			// TokenMinter's doc comment and MintSessionToken).
			"TEEPIN_SESSION_TOKEN":  token,
			"TEEPIN_SESSION_ID":     sess.ID.String(),
			"TEEPIN_PROMPT":         internalScratchPathInstruction + prompt,
			"TEEPIN_API_BASE_URL":   g.agentConfig.APIBaseURL,
			"TEEPIN_VISION_CAPABLE": strconv.FormatBool(g.agentConfig.VisionCapable),
			// run.py's own working directory defaults to "/workspace" — the
			// pod's ephemeral, non-persistent root filesystem — while the
			// PVC this spec mounts (below, via StorageGB) lands at /data.
			// Without this, EVERY file the agent ever writes is gone the
			// instant its pod dies, regardless of NeverRestart or how
			// deliberately DeliverMessage's relaunch preserves the PVC:
			// there was never anything on it to preserve. Found live
			// 2026-09-23, the true root cause behind this session's whole
			// run of "workspace empty on resume" reports — a relaunched
			// agent's own `ls /workspace` came back genuinely empty while
			// the console still showed a saved version with real files,
			// proving the two were never the same storage.
			"TEEPIN_WORKSPACE": "/data",
		},
		Labels:             map[string]string{agentLabel: "true"},
		CPUUnits:           g.agentConfig.CPUUnits,
		MemoryGB:           g.agentConfig.MemoryGB,
		StorageGB:          g.agentConfig.StorageGB,
		EphemeralStorageGB: g.agentConfig.EphemeralStorageGB,
		ImagePullSecret:    g.agentConfig.ImagePullSecret,
		// The agent image's tag is not a genuinely immutable identifier
		// today (see the proto field's own doc comment) — force a fresh
		// pull every launch rather than risk a node silently reusing a
		// stale cached image under the same tag.
		AlwaysPullImage: true,
		// A bare Pod's default RestartPolicy (Always) is correct for a
		// customer's persistent compute instance and wrong for this one:
		// the agent is a one-shot process that is SUPPOSED to exit when
		// it finishes (or after wait_for_deploy_approval's bounded wait) —
		// left at the default, a Kubernetes restart would silently
		// re-run the entire build from scratch against the same original
		// prompt, on repeat, burning the customer's session budget on
		// duplicate work (found live 2026-08-24: a build session's
		// activity feed showed re-generated, differently-worded text for
		// actions already completed — the tell that this had already
		// happened before this fix).
		NeverRestart: true,
	}

	if _, err := g.cluster.CreateInstance(ctx, spec); err != nil {
		return fmt.Errorf("failed to launch agent: %w", err)
	}

	if err := g.store.SetAgentInstanceID(ctx, sess.ID, podID); err != nil {
		// The pod is running but the session does not know its own agent's
		// id — clean up immediately rather than leak a pod nothing can
		// find to delete later (CloseSession only tears down what
		// sess.AgentInstanceID names).
		_ = g.cluster.DeleteInstance(ctx, cluster.ProjectScope(sess.ProjectID.String()), podID)
		return fmt.Errorf("agent launched but could not be recorded on the session: %w", err)
	}
	sess.AgentInstanceID = podID
	return nil
}

// MintWorkspaceFetchToken mints a short-lived credential authorising
// exactly a GET of this session's current workspace archive, plus the URL
// to fetch it from. Used by the build pipeline's Kaniko pod: an
// initContainer fetches the archive over HTTP using this token and unpacks
// it as the build context, instead of mounting the agent's own live PVC —
// so a customer's IDE edit or a rollback (both of which only ever change
// what pkg/kumbha/workspace.go considers "current", never the agent pod's
// own working copy) is what actually gets built, not whatever the agent's
// pod happens to still have on disk.
//
// Reuses the exact same TokenMinter and APIBaseURL WithAgent already
// configured — same trust boundary and TTL policy as the agent's own
// credential, not a second mechanism to keep in sync. Returns
// ErrAgentNotConfigured if WithAgent was never called: without it there is
// no APIBaseURL to build a reachable URL from, so there is nothing this
// method could correctly return.
func (g *Gateway) MintWorkspaceFetchToken(sess *Session, ttl time.Duration) (token, archiveURL string, err error) {
	if g.mintToken == nil || g.agentConfig.APIBaseURL == "" {
		return "", "", ErrAgentNotConfigured
	}
	token, err = g.mintToken(sess.AccountID, sess.ProjectID, sess.ID, ttl)
	if err != nil {
		return "", "", fmt.Errorf("failed to mint workspace fetch token: %w", err)
	}
	archiveURL = strings.TrimRight(g.agentConfig.APIBaseURL, "/") +
		"/v1/kumbha/sessions/" + sess.ID.String() + "/workspace/archive"
	return token, archiveURL, nil
}

// ScreenshotCPUUnits/ScreenshotMemoryGB size the capture pod — small and
// fixed, not operator-configurable. Deliberately much smaller than a real
// agent session's own AgentConfig sizing: this launches the SAME image
// LaunchAgent does (see CaptureScreenshot below), just with a different
// Command and a request sized for "run one headless-Chromium capture and
// exit" rather than "run an autonomous coding session." Exported so
// nodes.HiddenWorkloadCounter can account for a running capture pod's
// footprint without pkg/nodes hardcoding a second copy of this size.
const (
	ScreenshotCPUUnits = 1
	ScreenshotMemoryGB = 1
)

// ScreenshotPodNamePrefix identifies a screenshot-capture pod by its
// instance ID — see AgentPodNamePrefix's own doc comment; same reasoning,
// a third hidden pod type nodes.HiddenWorkloadCounter must also
// recognise. Matches CaptureScreenshot's own podID construction below.
const ScreenshotPodNamePrefix = "kumbha-shot-"

// CaptureTimeoutDefault bounds how long CaptureScreenshot waits for the
// pod to finish before giving up and cleaning it up — a hung headless
// browser (a page that never reaches load, a broken navigation) must not
// leak a pod forever.
//
// Was 45s, far too short: the capture pod runs the SAME image the Kumbha
// agent pod does (see this method's own doc comment) with
// AlwaysPullImage: true, forcing a fresh pull every single launch — a
// multi-GB image containing Chromium, Python, and the whole OpenHands
// SDK. Found live 2026-08-31: `kubectl describe pod` on a real capture
// pod showed a "Pulling image" event still in progress, with no
// "Pulled"/"Started" event ever following, when the control plane's own
// 45s budget expired and began tearing it down — the pod was killed
// mid-pull, every single time, on a home node's ordinary internet
// connection. 4 minutes leaves a full minute of margin under
// screenshotTokenTTL below (which was ALREADY sized for a multi-minute
// capture — its own doc comment says "comfortably past
// CaptureTimeoutDefault" — this constant was simply never raised to
// match that intent).
//
// Exported (was lowercase) because pkg/api's triggerScreenshotCapture
// wraps this call in its own outer context.WithTimeout, on its own
// SEPARATE hardcoded value — and a nested context.WithTimeout always
// resolves to the EARLIER of the two deadlines, so that outer value
// silently capped this one no matter how high it was raised. Found in
// the same incident: the outer timeout was hardcoded to 90s, itself
// already shorter than the observed pull time (over 157s). Two
// independent copies of "how long can a capture take" that are supposed
// to agree but do not is exactly the shape of bug this was — exporting
// this constant so pkg/api derives its own bound FROM it, rather than
// keeping a second number in sync by hand, is what actually closes it.
const CaptureTimeoutDefault = 4 * time.Minute

// screenshotTokenTTL bounds the capture pod's own upload credential —
// comfortably past CaptureTimeoutDefault, so the server's own timeout
// governs a stuck capture rather than the token itself expiring mid-
// upload and turning into a confusing 401 instead of a clear timeout.
const screenshotTokenTTL = 5 * time.Minute

// screenshotBinaryPath is where deploy/kumbha-agent/Dockerfile installs
// the kumbha-screenshot binary — see that Dockerfile's own comment on why
// this rides along on the agent image rather than getting a separate one.
const screenshotBinaryPath = "/usr/local/bin/kumbha-screenshot"

// CaptureScreenshot launches a short-lived headless-browser pod that
// screenshots targetURL and uploads the result to this session's own
// screenshot storage (POST /v1/kumbha/sessions/:id/screenshot,
// Gateway.SaveScreenshot) — what backs the console's Preview tab thumbnail.
//
// Deliberately reuses the SAME image AgentConfig.Image names, rather than
// a separate screenshot-specific image: deploy/kumbha-agent/Dockerfile
// already installs Chromium for the agent's own browser tool and now also
// builds the small kumbha-screenshot binary alongside teepin-mcp-server,
// so there is no second image/registry/deploy pipeline to provision or
// keep pull-secrets in sync with — this is just the SAME pod-launch
// mechanism LaunchAgent already uses, with Command overriding run.py's
// entrypoint. Reuses the exact same TokenMinter and APIBaseURL WithAgent
// already configured, same trust boundary as MintWorkspaceFetchToken.
//
// Synchronous: waits for the pod to finish (or CaptureTimeoutDefault to
// elapse) and deletes it before returning, success or failure — the pod's
// own result is entirely the SIDE EFFECT of it calling the upload
// endpoint, not this method's return value, so there is nothing
// customer-visible left behind to inspect after the fact, same posture as
// pkg/build.Service.Build. Callers are expected to run this in a
// background goroutine after a deploy's own HTTP response is already
// sent — a slow or hung page render must never add latency to the deploy
// itself — and to treat a returned error as best-effort/log-only, the
// same posture as CheckpointWorkspace/SetAppInstanceID: a missing
// thumbnail is a cosmetic gap, not a failed deployment.
func (g *Gateway) CaptureScreenshot(ctx context.Context, sess *Session, targetURL string) error {
	if g.cluster == nil || g.mintToken == nil || g.agentConfig.APIBaseURL == "" || g.agentConfig.Image == "" {
		return ErrAgentNotConfigured
	}

	token, err := g.mintToken(sess.AccountID, sess.ProjectID, sess.ID, screenshotTokenTTL)
	if err != nil {
		return fmt.Errorf("failed to mint screenshot upload token: %w", err)
	}
	uploadURL := strings.TrimRight(g.agentConfig.APIBaseURL, "/") +
		"/v1/kumbha/sessions/" + sess.ID.String() + "/screenshot"

	ctx, cancel := context.WithTimeout(ctx, CaptureTimeoutDefault)
	defer cancel()

	podID := "kumbha-shot-" + sess.ID.String()[:8]
	scope := cluster.ProjectScope(sess.ProjectID.String())

	// Same capacity-aware placement as LaunchAgent's own pod — see
	// pickNodeWithCapacity's doc comment. This pod is small
	// (ScreenshotCPUUnits/ScreenshotMemoryGB), so it's a much rarer miss
	// than the main agent pod, but there's no reason to leave it on the
	// blind-random path just because it's usually fine.
	providerID, err := g.pickNodeWithCapacity(ctx, ScreenshotCPUUnits, ScreenshotMemoryGB)
	if err != nil {
		return err
	}

	spec := cluster.InstanceSpec{
		InstanceID: podID,
		AccountID:  sess.AccountID.String(),
		ProjectID:  sess.ProjectID.String(),
		ProviderID: providerID,
		Image:      g.agentConfig.Image,
		Command:    []string{screenshotBinaryPath},
		Env: map[string]string{
			"TEEPIN_TARGET_URL": targetURL,
			"TEEPIN_UPLOAD_URL": uploadURL,
			"TEEPIN_TOKEN":      token,
		},
		Labels:          map[string]string{agentLabel: "true"},
		CPUUnits:        ScreenshotCPUUnits,
		MemoryGB:        ScreenshotMemoryGB,
		NeverRestart:    true,
		ImagePullSecret: g.agentConfig.ImagePullSecret,
		// Same reasoning as LaunchAgent's own AlwaysPullImage: the image
		// tag is not guaranteed immutable in every deployment, and a stale
		// cached binary silently drifting from what was actually pushed is
		// a worse failure mode than one extra pull.
		AlwaysPullImage: true,
	}
	if _, err := g.cluster.CreateInstance(ctx, spec); err != nil {
		return fmt.Errorf("failed to launch screenshot capture pod: %w", err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = g.cluster.DeleteInstance(cleanupCtx, scope, podID)
	}()

	return g.waitForCaptureCompletion(ctx, scope, podID)
}

// waitForCaptureCompletion polls the capture pod until it reaches a
// terminal state, mirroring pkg/build.Service.waitForCompletion's own
// poll-every-2s shape — kept as a separate, smaller copy rather than a
// shared helper: the two packages' cluster.Client wiring is independent
// (pkg/build takes its own directly; Gateway's is optional and
// agent-launch-flavoured), and duplicating ~15 lines is cheaper than
// introducing a cross-package dependency for it.
func (g *Gateway) waitForCaptureCompletion(ctx context.Context, scope cluster.Scope, podID string) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("screenshot capture timed out or was cancelled: %w", ctx.Err())
		case <-ticker.C:
			status, err := g.cluster.GetInstanceStatus(ctx, scope, podID)
			if err != nil {
				if errors.Is(err, cluster.ErrNotFound) {
					continue // creation may not have propagated to this read yet
				}
				return fmt.Errorf("failed to check screenshot capture status: %w", err)
			}
			if status.Status == "terminated" {
				return nil
			}
			if status.Status == "failed" {
				msg := status.Message
				if msg == "" {
					msg = status.Status
				}
				return fmt.Errorf("screenshot capture failed: %s", msg)
			}
		}
	}
}

// isAgentRunning reports whether a session's agent pod is currently
// pending or running — "will eventually reach its poll loop", not
// "idle right now". A pod mid-completion still counts: it will pick up
// a queued message once its current turn ends. False (with no error) for
// both "no agent has ever been launched" and "the pod has exited" — the
// caller (DeliverMessage) treats both the same way, by launching one.
func (g *Gateway) isAgentRunning(ctx context.Context, sess *Session) (bool, error) {
	if g.cluster == nil || sess.AgentInstanceID == "" {
		return false, nil
	}
	status, err := g.cluster.GetInstanceStatus(ctx, cluster.ProjectScope(sess.ProjectID.String()), sess.AgentInstanceID)
	if err != nil {
		if errors.Is(err, cluster.ErrNotFound) {
			return false, nil
		}
		// An ambiguous failure (cluster unreachable, etc.) is NOT the same
		// as "confirmed gone" — propagate rather than guess and risk
		// launching a duplicate agent pod alongside one that is actually
		// still fine.
		return false, fmt.Errorf("failed to check agent status: %w", err)
	}
	return status.Status == "pending" || status.Status == "running", nil
}

// IsAgentRunning is isAgentRunning, exported for GetKumbhaSession — the
// console's build detail page polls it for a LIVE "is the agent still
// actually working" read, rather than the cheap agent_instance_id != ""
// proxy every other session-returning endpoint uses (see
// kumbhaSessionResponse's own comment on why that split exists).
func (g *Gateway) IsAgentRunning(ctx context.Context, sess *Session) (bool, error) {
	return g.isAgentRunning(ctx, sess)
}

// DeliverMessage is the whole of "chat + resume": queue a follow-up for
// the agent's own poll loop to pick up if it's still alive (run.py's
// wait_for_next_instruction, resuming the SAME conversation with full
// history intact — nothing about a queued message launches a new agent by
// itself), or relaunch the agent with the message as a fresh prompt if the
// previous pod already exited.
//
// The relaunch path is an honest, bounded degradation, not real
// conversation persistence: a relaunched agent has NO memory of what it
// discussed before (no persistence_dir/conversation_id wiring exists
// yet — a real follow-up item, not silently glossed over here) — but the
// WORKSPACE it built is still there (versioned, on disk via the PVC), and
// the relaunch prompt says so explicitly, so the agent inspects what
// exists rather than starting over or contradicting itself.
//
// Returns relaunched=true when a new agent pod was started rather than an
// existing one resuming — the caller (SendMessage's HTTP handler) surfaces
// this so the console can explain a longer-than-usual wait before the
// activity feed picks back up.
func (g *Gateway) DeliverMessage(ctx context.Context, sess *Session, content string) (relaunched bool, err error) {
	running, err := g.isAgentRunning(ctx, sess)
	if err != nil {
		return false, err
	}
	if running {
		if _, err := g.store.SendMessage(ctx, sess.ID, content); err != nil {
			return false, err
		}
		return false, nil
	}

	if g.mintToken == nil {
		return false, ErrAgentNotConfigured
	}
	resumePrompt := "The customer sent a new message after your previous run ended: \"" + content + "\"\n\n" +
		"Your workspace already contains what you built before this message — inspect its current " +
		"contents before making any change, and do not discard or rewrite existing work unless the " +
		"customer explicitly asks you to."
	if err := g.LaunchAgent(ctx, sess, resumePrompt); err != nil {
		return false, err
	}
	return true, nil
}
