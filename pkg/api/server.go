// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/FlashbackAi/teepin-core/pkg/build"
	"github.com/FlashbackAi/teepin-core/pkg/cluster"
	"github.com/FlashbackAi/teepin-core/pkg/compute"
	"github.com/FlashbackAi/teepin-core/pkg/gpu"
	"github.com/FlashbackAi/teepin-core/pkg/imageinfo"
	"github.com/FlashbackAi/teepin-core/pkg/kumbha"
	"github.com/FlashbackAi/teepin-core/pkg/models"
)

// Server represents the API server
// PricingProvider supplies the live platform GPU rate. Implemented by
// billing.Service; nil means no database and the compiled-in default
// rate applies.
type PricingProvider interface {
	VRAMPricePerGBHour(ctx context.Context) float64
}

// ProvisionGate answers whether an account may create resources right
// now — the "no validated payment method, no resources" check. Returns
// (false, customer-facing reason, nil) when blocked. Implemented by
// billing.Service; nil means no gate (standalone/no-database mode).
type ProvisionGate interface {
	AccountCanProvision(ctx context.Context, accountID uuid.UUID) (bool, string, error)
}

// GithubStore pushes each Kumbha checkpoint to a Teepin-owned GitHub repo
// — implemented by pkg/githubstore.Service. Defined here (the consumer),
// not there, same pattern as build.RegistryProvider: pkg/api depends on
// this narrow surface, never the concrete client, which is what lets a
// test inject a lightweight fake instead of a real GitHub-backed service
// pointed at a mock HTTP server.
type GithubStore interface {
	// ProvisionRepo creates (or finds, idempotently) sessionID's repo.
	ProvisionRepo(ctx context.Context, sessionID uuid.UUID) (string, error)
	// PushSnapshot commits files to sessionID's repo. Returns only error —
	// deliberately no repo name/URL, so pkg/api has nothing to
	// accidentally leak into a customer-facing response.
	PushSnapshot(ctx context.Context, sessionID uuid.UUID, files []kumbha.WorkspaceFile, message string) error
}

type Server struct {
	// cluster is the only route to GPU capacity. The API server holds no
	// Kubernetes client of its own: that is what lets the control plane
	// run on AWS while the GPUs sit in a datacenter elsewhere. See
	// pkg/cluster for the two implementations.
	cluster cluster.Client

	gpuAllocator *gpu.Allocator
	store        *compute.Store  // nil in standalone mode (no database)
	pricing      PricingProvider // nil in standalone mode (no database)
	gate         ProvisionGate   // nil in standalone mode (no billing gate)

	// nodePlacer resolves a home-class placement. Nil unless home compute is
	// enabled, in which case a node_class:"home" request is refused cleanly
	// (400) rather than panicking. Set via WithNodePlacer.
	nodePlacer NodePlacer

	// endpointDomain/enableTLS/tlsIssuer are stamped onto every placement's
	// InstanceSpec (see instanceSpec below) so the customer-visible hostname
	// and TLS policy are a control-plane fact, not independently configured
	// per agent (TEEPIN_DOMAIN previously existed on both the ECS task AND
	// every agent process — two sources of truth for the same URL, which
	// drift). Zero values leave a spec's EndpointDomain/EnableTLS/TLSIssuer
	// at their own zero values, which DirectClient/AgentClient treat as "use
	// the agent's own configured default" — see cluster.EndpointOptions.
	// Set via WithEndpointConfig.
	endpointDomain string
	enableTLS      bool
	tlsIssuer      string

	// ephemeralStorageGB is the safety cap stamped onto every instance's
	// InstanceSpec (not customer-selectable; see StorageGB for the billed,
	// customer-chosen volume). Zero here means "not configured", in which
	// case instanceSpec falls back to defaultEphemeralStorageGB rather
	// than leaving the pod's disk unbounded — found live 2026-08-22: a
	// pod with no limit at all was backed directly by the host's full
	// disk. Set via WithEphemeralStorageGB.
	ephemeralStorageGB int

	// execTickets issues short-lived, single-use credentials for
	// interactive exec's WebSocket attach step (pkg/cluster.ExecHandler
	// redeems them — see the ticket auth design in the interactive-exec
	// plan). Nil disables the feature cleanly: CreateExecSession returns
	// 404, matching how home-capacity behaves when home compute is off.
	execTickets *cluster.TicketStore

	// kumbha is the Kumbha Gateway's business logic (pkg/kumbha) — session
	// budgets, routing, metering. Nil disables the feature cleanly: every
	// Kumbha handler returns 404, matching execTickets' pattern above.
	kumbha *kumbha.Gateway

	// kumbhaEventTickets issues short-lived, single-use credentials for
	// the Kumbha event-relay WebSocket attach step (pkg/kumbha.EventsHandler
	// redeems them) — same ticket-auth shape as execTickets, for the same
	// reason (a WS handshake carries no Authorization header). Nil
	// disables the feature cleanly.
	kumbhaEventTickets *kumbha.EventTicketStore

	// kumbhaBuild runs Kaniko builds of a Kumbha session's workspace
	// (pkg/build) — what the "deploy" MCP verb calls once approved. Nil
	// disables the feature cleanly: BuildKumbhaSession returns 404, same
	// as every other optional Kumbha capability.
	kumbhaBuild *build.Service

	// kumbhaBuildImageRegistryPrefix/kumbhaBuildImagePullSecret together
	// let a home/datacenter node pull a Kumbha-built image back down
	// from ECR to actually run it — see instanceSpec's own use of these
	// and WithKumbhaBuildImagePullSecret's doc comment for why this is
	// NOT a customer-facing request field.
	kumbhaBuildImageRegistryPrefix string
	kumbhaBuildImagePullSecret     string

	// githubStore pushes each Kumbha session's checkpointed workspace to a
	// Teepin-owned repo (pkg/githubstore) — invisible to the customer, who
	// only ever gets the existing ZIP download. Nil disables the feature
	// cleanly: a checkpoint just skips the push, same best-effort posture
	// as CheckpointWorkspace/UpdateImage's own failure handling.
	githubStore GithubStore
}

// WithExecTickets enables interactive exec's REST half (ticket issuance).
// Returns the same *Server for chaining. The WebSocket attach half is a
// separate handler (cluster.ExecHandler) mounted directly in
// cmd/api-server/main.go, outside gin's JWT-auth group — the WS
// handshake carries no Authorization header.
func (s *Server) WithExecTickets(tickets *cluster.TicketStore) *Server {
	s.execTickets = tickets
	return s
}

// WithKumbha enables the Kumbha Gateway endpoints. Returns the same
// *Server for chaining, so existing NewServer call sites compile
// unchanged — a server built without this call keeps every Kumbha
// endpoint returning 404, same as execTickets when home compute is off.
func (s *Server) WithKumbha(gw *kumbha.Gateway) *Server {
	s.kumbha = gw
	return s
}

// WithKumbhaEventTickets enables the event-relay WebSocket's REST half
// (ticket issuance). Returns the same *Server for chaining. The WebSocket
// attach half is a separate handler (kumbha.EventsHandler) mounted
// directly in cmd/api-server/main.go, outside gin's JWT-auth group — same
// reasoning as WithExecTickets.
func (s *Server) WithKumbhaEventTickets(tickets *kumbha.EventTicketStore) *Server {
	s.kumbhaEventTickets = tickets
	return s
}

// WithKumbhaBuild enables the Kumbha "deploy" verb's build step (Kaniko).
// Returns the same *Server for chaining, so existing NewServer call sites
// compile unchanged — a server built without this call keeps
// BuildKumbhaSession returning 404.
func (s *Server) WithKumbhaBuild(b *build.Service) *Server {
	s.kumbhaBuild = b
	return s
}

// WithGithubStore enables pushing each Kumbha checkpoint to a Teepin-owned
// GitHub repo (pkg/githubstore). Returns the same *Server for chaining —
// a server built without this call simply never pushes, same posture as
// every other optional Kumbha capability.
func (s *Server) WithGithubStore(gs GithubStore) *Server {
	s.githubStore = gs
	return s
}

// WithKumbhaBuildImagePullSecret configures how a deployed Kumbha app
// instance pulls its own just-built image back down from ECR to
// actually run it — a DIFFERENT credential from the one Kaniko uses to
// PUSH it (pkg/ecrregistry's DockerConfigJSONForBuild, injected into the
// build pod directly, never a cluster Secret). Found live 2026-08-26: a
// real deploy built and pushed an image cleanly, then failed instance
// creation outright with "pull access denied ... no basic auth
// credentials" — nothing had ever wired a pull credential for the
// resulting instance at all.
//
// Deliberately NOT a models.CreateInstanceRequest field the customer
// could set directly: every workload pod lives in one shared namespace
// (cluster.WorkloadNamespace) across all tenants, so a customer-supplied
// secret NAME would let them reference (not read the contents of, but
// borrow the registry-pull use of) a secret they do not own, just by
// guessing or learning its name — a narrow but real tenant-isolation
// gap not worth opening for this. Instead, instanceSpec auto-attaches
// this secret ONLY when the image reference itself starts with
// registryPrefix — the one, stable, control-plane-owned ECR repository
// every Kumbha build pushes to (see ecrregistry.Service.ImagePrefix,
// which returns the SAME URI regardless of project) — so which secret
// gets used is a server-side policy keyed off the image string's own
// structure, never something a request body can influence. A pull
// secret name with no registry prefix (or vice versa) is a
// misconfiguration this treats as "feature disabled" rather than
// guessing: instanceSpec only attaches when BOTH are non-empty.
func (s *Server) WithKumbhaBuildImagePullSecret(registryPrefix, secretName string) *Server {
	s.kumbhaBuildImageRegistryPrefix = registryPrefix
	s.kumbhaBuildImagePullSecret = secretName
	return s
}

// WithEndpointConfig sets the domain/TLS policy stamped onto every
// instance's placement. Returns the same *Server for chaining, so existing
// NewServer call sites compile unchanged. Not calling this at all preserves
// today's behaviour exactly: every field defaults to the zero value, which
// means "let the agent's own configuration decide" (see EndpointDomain/
// EnableTLS/TLSIssuer's use in instanceSpec).
func (s *Server) WithEndpointConfig(domain string, enableTLS bool, tlsIssuer string) *Server {
	s.endpointDomain = domain
	s.enableTLS = enableTLS
	s.tlsIssuer = tlsIssuer
	return s
}

// defaultEphemeralStorageGB is used whenever WithEphemeralStorageGB is
// never called (every existing NewServer call site) or is called with 0.
const defaultEphemeralStorageGB = 10

// WithEphemeralStorageGB sets the per-instance disk-usage safety cap.
// Returns the same *Server for chaining, so existing NewServer call sites
// compile unchanged and keep working (via defaultEphemeralStorageGB)
// without needing to be updated.
func (s *Server) WithEphemeralStorageGB(gb int) *Server {
	s.ephemeralStorageGB = gb
	return s
}

// NodePlacer resolves a home CPU workload to a specific node + provider. An
// interface so the api package does not import pkg/nodes at construction;
// main injects the concrete nodes.Service through a thin adapter. Mirrors the
// ProvisionGate/PricingProvider pattern.
type NodePlacer interface {
	PlaceCPU(ctx context.Context, arch string, cpuUnits, memoryGB int) (nodeName, providerID, nodeArch string, err error)
	// Error classification so the handler can return the right status.
	IsNoCapacity(err error) bool
	IsArchUnavailable(err error) bool
	IsInsufficientCapacity(err error) bool
}

// WithNodePlacer enables home-class placement. Returns the same *Server for
// chaining, so existing NewServer call sites compile unchanged.
func (s *Server) WithNodePlacer(p NodePlacer) *Server {
	s.nodePlacer = p
	return s
}

// homeTarget is a resolved home placement, carried from the placement branch
// to the spec and the persisted instance record.
type homeTarget struct {
	nodeName   string
	providerID string
	arch       string
}

// NewServer creates a new API server. store, pricing and gate may be nil
// when the platform runs without a database (local standalone mode); in
// that case instances are not persisted, not billed, priced at the
// compiled-in default rate, and not gated on payment.
//
// Endpoint provisioning (Service/Ingress) is NOT wired here — it is owned
// entirely by the cluster layer (pkg/cluster, backed by pkg/networking on
// the agent side), which reports what it provisioned back over gRPC as
// part of CreateInstance's result. The API server previously held its own
// *networking.Service and called GetEndpointInfo directly, which only ever
// worked in the (unused in production) direct cluster mode — see Stage 3
// plan defects 1/2.
func NewServer(clusterClient cluster.Client, gpuAllocator *gpu.Allocator, store *compute.Store, pricing PricingProvider, gate ProvisionGate) *Server {
	return &Server{
		cluster:      clusterClient,
		gpuAllocator: gpuAllocator,
		store:        store,
		pricing:      pricing,
		gate:         gate,
	}
}

// scopeFor builds the cluster tenancy predicate for a request.
//
// Every compute read and write passes through here. In standalone mode
// (no database) there is no tenancy and the scope is unrestricted;
// otherwise it is pinned to the caller's project, which requireScope has
// already established is present.
// validatePorts checks a create/redeploy request's requested ports against
// this platform's constraints. Returns status == 0 and a nil body when
// every port is valid; otherwise the exact status/body the caller should
// write via c.JSON and return. Shared by CreateInstance and
// DeployKumbhaSession's redeploy path (see redeployKumbhaInstance) so a
// redeploy cannot bypass validation an initial create would have enforced
// merely by taking a different code path.
func validatePorts(ports []models.PortMapping) (status int, body gin.H) {
	// There is no per-instance public port in this architecture: every
	// instance is reached by hostname on 443 through the shared edge
	// (pkg/networking's ProvisionEndpoint hardcodes ExternalPort 443).
	// Silently accepting a port the platform cannot honour is worse than
	// rejecting it — a customer who set `public` and got a different port
	// (or none) would have no way to know their request was ignored.
	//
	// Protocol is validated here rather than left to silently fall back to
	// TCP downstream: a typo ("udp " with a trailing space, "UDP1") should
	// be a 400 the customer can fix, not a request that quietly runs as
	// something other than what was asked for.
	for _, port := range ports {
		if port.Public != 0 {
			return http.StatusBadRequest, gin.H{
				"error": "public port assignment is not supported; instances are reached by hostname on 443, not a chosen port",
			}
		}
		switch strings.ToLower(port.Protocol) {
		case "", "tcp", "udp":
		default:
			return http.StatusBadRequest, gin.H{
				"error": fmt.Sprintf("invalid port protocol %q: must be tcp or udp", port.Protocol),
			}
		}
		// The valid TCP/UDP port range. Below this, 0 is "no port" (should
		// never reach here as a real request) and negative values are a
		// client bug; above it, the value cannot be a port at all. This is
		// a sanity bound, not a privilege restriction — containers in this
		// platform are free to listen on any port including <1024 (e.g.
		// nginx's default of 80), since that is a container-namespace bind,
		// not a host one, and carries none of a bare process's root
		// requirement.
		if port.Container < 1 || port.Container > 65535 {
			return http.StatusBadRequest, gin.H{
				"error": fmt.Sprintf("invalid container port %d: must be between 1 and 65535", port.Container),
			}
		}
	}
	return 0, nil
}

// validateStorageGB checks a create/redeploy request's requested volume
// size. Same status==0-means-valid contract as validatePorts, and shared
// for the same reason.
func validateStorageGB(gb int) (status int, body gin.H) {
	// 1000GB is an operator-chosen sanity bound, not a technical limit —
	// there is no plan tier anywhere near it yet, and it exists purely to
	// reject a fat-fingered request (or an integer overflow upstream)
	// before it reaches PVC provisioning.
	if gb < 0 || gb > 1000 {
		return http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("invalid storage_gb %d: must be between 0 and 1000", gb),
		}
	}
	return 0, nil
}

func scopeFor(projectID uuid.UUID) cluster.Scope {
	if projectID == uuid.Nil {
		return cluster.AllTenants()
	}
	return cluster.ProjectScope(projectID.String())
}

// vramRate returns the current platform GPU rate ($/GB-hour). Read on
// every call — price changes made through the admin API must apply to
// the very next allocation, never a cached quote.
func (s *Server) vramRate(ctx context.Context) float64 {
	if s.pricing == nil {
		return gpu.DefaultPricePerGBHour
	}
	return s.pricing.VRAMPricePerGBHour(ctx)
}

// ListInstanceTypes returns available instance types derived from the
// cluster's live GPU inventory. Custom VRAM sizes are always available
// via the gpu_vram request field and are not enumerated here.
func (s *Server) ListInstanceTypes(c *gin.Context) {
	rate := s.vramRate(c.Request.Context())
	types, err := s.gpuAllocator.AvailableInstanceTypes(c.Request.Context(), rate)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": fmt.Sprintf("GPU discovery failed: %v", err)})
		return
	}

	instanceTypes := make([]models.InstanceType, 0, len(types))
	for _, t := range types {
		isolation := "shared GPU, exact VRAM accounting"
		if t.Isolation == gpu.AllocationMIG {
			isolation = "MIG hardware isolation"
		}
		instanceTypes = append(instanceTypes, models.InstanceType{
			Name:         t.Name,
			GPUVRAM:      fmt.Sprintf("%dGB", t.MemoryGB),
			GPUMemoryGB:  t.MemoryGB,
			CPUUnits:     8,      // Default
			Memory:       "32GB", // Default
			PricePerHour: t.PricePerHour,
			Description:  fmt.Sprintf("%s GPU with %dGB VRAM (%s)", strings.ToUpper(t.GPUModel), t.MemoryGB, isolation),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"instance_types": instanceTypes,
		"pricing":        fmt.Sprintf("$%.2f per GB-hour, exact allocation (custom sizes supported via gpu_vram)", rate),
	})
}

// imagePortsTimeout bounds the whole request, including gin's own
// overhead around ResolvePorts' own internal timeout — a defensive
// outer bound, not the primary one (that lives in pkg/imageinfo).
const imagePortsTimeout = 6 * time.Second

// ImagePorts looks up the ports a container image declares via EXPOSE, so
// the create-instance form can default the "Port" field instead of
// requiring every customer to already know what their image listens on.
//
// Never a hard dependency: an image whose registry isn't on the
// allowlist, that declares no ports, or that can't be reached at all
// returns an empty list with 200, exactly like "no ports found" — the
// customer can always type the port manually, same as before this
// endpoint existed. Nothing here can block instance creation.
func (s *Server) ImagePorts(c *gin.Context) {
	image := strings.TrimSpace(c.Query("image"))
	if image == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "image query parameter is required"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), imagePortsTimeout)
	defer cancel()

	ports, err := imageinfo.ResolvePorts(ctx, image)
	if err != nil {
		// The only error case is a malformed reference — everything else
		// (unreachable registry, missing image, no EXPOSE) already
		// degrades to an empty list inside ResolvePorts. A malformed
		// image string is also not fatal here: the customer's own
		// CreateInstance call will separately validate the image field
		// when they actually submit, so this just reports nothing found.
		c.JSON(http.StatusOK, gin.H{"ports": []models.ImagePort{}})
		return
	}

	out := make([]models.ImagePort, 0, len(ports))
	for _, p := range ports {
		out = append(out, models.ImagePort{Port: p.Port, Protocol: p.Protocol})
	}
	c.JSON(http.StatusOK, gin.H{"ports": out})
}

// CreateInstance creates a new compute instance
func (s *Server) CreateInstance(c *gin.Context) {
	var req models.CreateInstanceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if status, body := validatePorts(req.Ports); status != 0 {
		c.JSON(status, body)
		return
	}
	if status, body := validateStorageGB(req.StorageGB); status != 0 {
		c.JSON(status, body)
		return
	}

	// Billing integrity: when persistence is enabled, every instance
	// must belong to a project so its usage can be metered. Refuse to
	// run unbilled workloads.
	projectID, accountID, ok := s.requireScope(c)
	if !ok {
		return
	}
	userID, _ := auth.GetUserID(c)
	// Present only on a Kumbha session-scoped credential (auth.MintSessionToken)
	// — a human/API-key request never carries this claim, so it stays
	// uuid.Nil and record.KumbhaSessionID below is written as SQL NULL
	// (see nullUUID). This is what lets every instance a Kumbha agent
	// creates via create_instance be traced back to its session, closing
	// the gap that let two untracked instances come out of one build
	// (found live 2026-08-30/31 — see migration 032's own doc comment).
	kumbhaSessionID, _ := auth.GetSessionID(c)

	// Payment gate: no validated payment method (or a non-active account),
	// no resources. Checked here — after identity is resolved, before any
	// GPU is allocated or any side effect occurs — so a blocked account
	// consumes nothing. Skipped when no gate is wired (standalone mode).
	//
	// Fails CLOSED: a billing check that errors returns 503 and denies,
	// never hands out GPU capacity on a database blip.
	if s.gate != nil {
		allowed, reason, err := s.gate.AccountCanProvision(c.Request.Context(), accountID)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": "unable to verify billing status, please retry",
			})
			return
		}
		if !allowed {
			c.JSON(http.StatusPaymentRequired, gin.H{
				"error": reason,
				"code":  "payment_method_required",
			})
			return
		}
	}

	// The customer-facing instance ID is random; the endpoint UUID that
	// names its Service/Ingress/TLS resources is deterministically derived
	// FROM it (see endpointUUIDFor) rather than minted separately — that
	// is what lets a later Kumbha redeploy (DeployKumbhaSession) recompute
	// the exact same value the original create used, with nothing to
	// persist or look up, so its endpoint-provisioning call lands on the
	// SAME already-existing Service/Ingress instead of creating a second,
	// differently-named pair alongside them.
	instanceID := fmt.Sprintf("inst-%s", uuid.New().String()[:8])
	instanceUUID := endpointUUIDFor(instanceID)

	// Home-class placement — OPT-IN ONLY. This branch is entered solely
	// when the request explicitly asks for node_class:"home", so a normal
	// request can never land a paying customer's workload on a consumer
	// node, even by a bug: the home path is unreachable without the flag in
	// the request.
	var homePlacement *homeTarget
	if req.NodeClass == "home" {
		if s.nodePlacer == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "home compute is not enabled on this platform"})
			return
		}
		// Size the request from the validated fields (which carry the chosen
		// tier's cpu_units and memory). Placement refuses a node that cannot
		// fit this size.
		reqMemGB := parseMemoryGB(req.Memory)
		nodeName, providerID, nodeArch, err := s.nodePlacer.PlaceCPU(
			c.Request.Context(), req.Arch, req.CPUUnits, reqMemGB)
		if err != nil {
			switch {
			case s.nodePlacer.IsArchUnavailable(err):
				// A request problem (fixable), not transient capacity.
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			case s.nodePlacer.IsNoCapacity(err), s.nodePlacer.IsInsufficientCapacity(err):
				// Fail closed, like GPU exhaustion: 503 so clients retry when
				// capacity frees up.
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
			default:
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "home placement failed, try again"})
			}
			return
		}
		homePlacement = &homeTarget{nodeName: nodeName, providerID: providerID, arch: nodeArch}
	}

	// Parse VRAM requirement (GPU path — mutually exclusive with home).
	var vramGB int
	var err error
	if homePlacement == nil && req.GPUVRAM != "" {
		vramGB, err = gpu.ParseVRAM(req.GPUVRAM)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid gpu_vram: %v", err)})
			return
		}
	}

	// Allocate GPU if VRAM specified
	var allocation *gpu.Allocation
	if vramGB > 0 {
		allocation, err = s.gpuAllocator.AllocateByVRAM(c.Request.Context(), vramGB)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": fmt.Sprintf("GPU allocation failed: %v", err)})
			return
		}
	}

	// Hand the resolved placement to the cluster layer. Everything above
	// this point is a decision; everything below it is execution.
	spec := s.instanceSpec(instanceID, instanceUUID, projectID, accountID, &req, allocation)
	if homePlacement != nil {
		spec.NodeClass = "home"
		spec.NodeName = homePlacement.nodeName
		spec.ProviderID = homePlacement.providerID
	}

	scope := scopeFor(projectID)

	result, err := s.cluster.CreateInstance(c.Request.Context(), spec)
	if err != nil {
		// A GPU that vanished between allocation and execution is a lost
		// race, not the customer's fault: say so in terms they can act on,
		// and use 503 so clients retry rather than treating it as a bad
		// request.
		if errors.Is(err, cluster.ErrResourceExhausted) {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": "the allocated GPU was taken by another request; retry to be placed on fresh capacity",
			})
			return
		}
		if errors.Is(err, cluster.ErrClusterUnavailable) {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": "no GPU capacity is reachable right now; existing instances are unaffected",
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to create instance: %v", err)})
		return
	}

	// The endpoint is provisioned by the cluster layer alongside the pod,
	// and reported back on `result` — the control plane must never call
	// back into a networking client of its own to read it: in production
	// (TEEPIN_CLUSTER_MODE=agent) this process has no Kubernetes access at
	// all, so that call always silently found nothing (Stage 3 defects
	// 1/2). `result` already carries everything the agent provisioned,
	// over the same gRPC response that reported PodName.

	// Persist the instance — this is what billing meters. If it fails
	// the workload must not keep running unbilled: roll back.
	if s.store != nil {
		record := &compute.InstanceRecord{
			ID:              instanceID,
			AccountID:       accountID,
			ProjectID:       projectID,
			UserID:          userID,
			KumbhaSessionID: kumbhaSessionID,
			Name:            req.Name,
			Image:           req.Image,
			Status:          compute.StatusPending,
			CPUUnits:        req.CPUUnits,
			MemoryGB:        parseMemoryGB(req.Memory),
			K8sPodName:      result.PodName,
			K8sNamespace:    "default",
		}
		if allocation != nil {
			record.InstanceType = allocation.InstanceType
			record.GPUVRAMGB = allocation.AllocatedVRAM
		}
		if homePlacement != nil {
			// Records which home provider+node ran this — for delete/logs
			// routing and load-based placement of the next workload.
			record.ProviderID = homePlacement.providerID
			record.NodeName = homePlacement.nodeName
			record.InstanceType = "cpu.home"
		}
		record.Endpoint = result.EndpointURL
		record.DNSName = result.DNSName
		record.PublicIP = result.PublicIP
		record.TLSEnabled = result.TLSEnabled
		record.TLSReady = result.TLSReady
		// The Stage 3 tunnel needs to know which port to proxy to for an
		// already-running instance — persisted here rather than re-derived
		// later, since spec.Ports[0] (the source of truth at create time)
		// is not available on any subsequent read path.
		if len(req.Ports) > 0 {
			record.ContainerPort = req.Ports[0].Container
		}

		if err := s.store.Create(c.Request.Context(), record); err != nil {
			// Roll back rather than leave a workload running that billing
			// has no record of. DeleteInstance tears down the endpoint
			// too, so the networking cleanup is covered.
			if delErr := s.cluster.DeleteInstance(c.Request.Context(), scope, instanceID); delErr != nil {
				// Both failed: the instance is running and unbilled. Say so
				// loudly — this needs a human, and silence here is revenue
				// walking out of the door.
				log.Printf("ORPHANED INSTANCE %s: persistence failed (%v) and rollback failed (%v) - running unbilled, manual cleanup required",
					instanceID, err, delErr)
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to persist instance: %v", err)})
			return
		}

		// A Kumbha session that just provisioned REAL infrastructure needs
		// its current workspace draft checkpointed — the same thing
		// DeployKumbhaSession already does after its own create call, but
		// this one covers every OTHER way a session-scoped credential can
		// reach here (create_instance's MCP tool being the live example:
		// used as a workaround when `deploy` was erroring, it left a real,
		// working build's only saved draft un-checkpointed and therefore
		// invisible in the console's History — is_checkpoint-filtered,
		// see ListVersions — even though the file content itself was safe
		// in Postgres the whole time. Found live 2026-08-31. Best-effort:
		// the instance is real either way; only the console's history
		// view would be short one entry if this fails, not worth undoing
		// a successful create over.
		if s.kumbha != nil && kumbhaSessionID != uuid.Nil {
			if err := s.kumbha.CheckpointWorkspace(c.Request.Context(), kumbhaSessionID); err != nil {
				log.Printf("WARN: instance %s created for Kumbha session %s but failed to checkpoint its workspace: %v", instanceID, kumbhaSessionID, err)
			}
		}
	}

	// Build response
	instance := models.Instance{
		ID:    instanceID,
		Name:  req.Name,
		Image: req.Image,
		// A freshly created instance is always pending: the pod has been
		// accepted but no container has started. Previously this reported
		// the raw pod phase, which is "Pending" at this point anyway —
		// the difference is that it is now TEEPIN's vocabulary rather
		// than Kubernetes', consistently with every later read.
		Status:    compute.StatusPending,
		CPUUnits:  req.CPUUnits,
		Memory:    req.Memory,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
		Labels:    req.Labels,
	}

	if allocation != nil {
		instance.GPUVRAM = fmt.Sprintf("%dGB", allocation.RequestedVRAM)
		instance.AllocatedVRAM = fmt.Sprintf("%dGB", allocation.AllocatedVRAM)
		instance.InstanceType = allocation.InstanceType
		instance.PricePerHour = gpu.PriceForVRAM(allocation.AllocatedVRAM, s.vramRate(c.Request.Context()))
		if allocation.AllocatedVRAM > allocation.RequestedVRAM {
			instance.AllocationNote = fmt.Sprintf(
				"requested %dGB; allocated %dGB — the smallest isolation unit that fits (billed for %dGB at $%.2f/hr). Exact custom sizes arrive with software VRAM partitioning.",
				allocation.RequestedVRAM, allocation.AllocatedVRAM,
				allocation.AllocatedVRAM, instance.PricePerHour)
		}
	}

	// Endpoint information, straight from the gRPC result — see the note
	// above CreateInstance's persistence block.
	instance.Endpoint = result.EndpointURL
	instance.PublicIP = result.PublicIP
	instance.DNSName = result.DNSName
	instance.TLSEnabled = result.TLSEnabled
	instance.TLSReady = result.TLSReady
	if len(req.Ports) > 0 {
		instance.ContainerPort = req.Ports[0].Container
	}

	c.JSON(http.StatusCreated, instance)
}

// ListInstances lists the caller's instances. With tenancy active the
// result is scoped to the caller's project — never other tenants'.
func (s *Server) ListInstances(c *gin.Context) {
	projectID, accountID, ok := s.requireScope(c)
	if !ok {
		return
	}

	statuses, err := s.cluster.ListInstanceStatuses(c.Request.Context(), scopeFor(projectID))
	if err != nil {
		if errors.Is(err, cluster.ErrClusterUnavailable) {
			// The control plane is up; GPU capacity is not reachable. Say
			// which, so a customer does not read this as their instances
			// having been destroyed.
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": "Capacity is temporarily unreachable; instance state cannot be read right now",
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	records := s.recordsByID(c.Request.Context(), accountID, projectID)

	rate := s.vramRate(c.Request.Context())
	instances := make([]models.Instance, 0, len(statuses))
	for _, st := range statuses {
		instances = append(instances, statusToInstance(st, records[st.InstanceID], rate, s.endpointDomain))
	}

	c.JSON(http.StatusOK, gin.H{
		"instances": instances,
		"count":     len(instances),
	})
}

// recordsByID fetches the caller's stored instance records, keyed by
// instance ID.
//
// One query for the whole page, not one per instance: a customer with
// fifty instances would otherwise issue fifty round trips to Aurora on
// every list, which across the Atlantic is the difference between a fast
// page and a slow one.
//
// Returns an empty map in standalone mode or on error: the cluster view
// alone answers "what is running", and degrading to fewer fields beats
// failing the request outright.
func (s *Server) recordsByID(ctx context.Context, accountID, projectID uuid.UUID) map[string]*compute.InstanceRecord {
	out := map[string]*compute.InstanceRecord{}
	if s.store == nil || projectID == uuid.Nil {
		return out
	}

	records, err := s.store.ListByProject(ctx, accountID, projectID)
	if err != nil {
		return out
	}

	for i := range records {
		out[records[i].ID] = &records[i]
	}
	return out
}

// GetInstance gets details of a specific instance. Another tenant's
// instance is indistinguishable from a nonexistent one (404).
func (s *Server) GetInstance(c *gin.Context) {
	instanceID := c.Param("id")

	projectID, _, ok := s.requireScope(c)
	if !ok {
		return
	}

	st, err := s.cluster.GetInstanceStatus(c.Request.Context(), scopeFor(projectID), instanceID)
	if err != nil {
		// Another tenant's instance and a nonexistent one are the same
		// answer: existence must not leak.
		if errors.Is(err, cluster.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "instance not found"})
			return
		}
		if errors.Is(err, cluster.ErrClusterUnavailable) {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": "Capacity is temporarily unreachable; instance state cannot be read right now",
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var record *compute.InstanceRecord
	if s.store != nil {
		record, _ = s.store.Get(c.Request.Context(), instanceID)
	}

	c.JSON(http.StatusOK, statusToInstance(*st, record, s.vramRate(c.Request.Context()), s.endpointDomain))
}

// DeleteInstance deletes an instance. Scoped to the caller's project:
// another tenant's instance is a 404, and deleting a nonexistent
// instance is a 404 (not a silent success).
func (s *Server) DeleteInstance(c *gin.Context) {
	instanceID := c.Param("id")

	projectID, _, ok := s.requireScope(c)
	if !ok {
		return
	}
	scope := scopeFor(projectID)

	// Existence check before deleting, so that deleting something that
	// was never there is a 404 rather than a misleading success. The
	// cluster's own delete is idempotent (commands may be redelivered),
	// but a customer calling DELETE on a typo deserves to be told.
	if _, err := s.cluster.GetInstanceStatus(c.Request.Context(), scope, instanceID); err != nil {
		if errors.Is(err, cluster.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "instance not found"})
			return
		}
		if errors.Is(err, cluster.ErrClusterUnavailable) {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": "Capacity is temporarily unreachable; the instance cannot be deleted right now",
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Tears down the pod and its endpoint together — the endpoint
	// teardown lives next to the cluster because that is where the
	// Service and Ingress are.
	if err := s.cluster.DeleteInstance(c.Request.Context(), scope, instanceID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Stop billing: stamp terminated_at. Idempotent; the reconciler
	// would also catch this within a minute if it failed here.
	if s.store != nil {
		if err := s.store.MarkTerminated(c.Request.Context(), instanceID); err != nil {
			c.Header("X-Warning", fmt.Sprintf("failed to finalize billing record: %v", err))
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "instance deleted",
		"id":      instanceID,
	})
}

// GetInstanceLogs gets logs from an instance
func (s *Server) GetInstanceLogs(c *gin.Context) {
	instanceID := c.Param("id")

	projectID, _, ok := s.requireScope(c)
	if !ok {
		return
	}

	// Tail size: ?tail=N (default 100, capped to keep responses sane).
	tail := 100
	if t, err := strconv.Atoi(c.Query("tail")); err == nil && t > 0 && t <= 10000 {
		tail = t
	}

	// Buffered rather than streamed: this endpoint returns JSON, and the
	// cap above bounds the size. Live following belongs on a separate
	// streaming endpoint, which arrives with the agent work.
	var buf bytes.Buffer
	err := s.cluster.StreamLogs(c.Request.Context(), scopeFor(projectID), instanceID,
		cluster.LogOptions{
			TailLines:  tail,
			Timestamps: c.Query("timestamps") == "true",
		}, &buf)
	if err != nil {
		if errors.Is(err, cluster.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "instance not found"})
			return
		}
		if errors.Is(err, cluster.ErrClusterUnavailable) {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": "Capacity is temporarily unreachable; logs cannot be fetched right now",
			})
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("failed to fetch logs: %v", err)})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"instance_id": instanceID,
		"tail":        tail,
		"logs":        buf.String(),
	})
}

// GetInstanceMetrics gets metrics from an instance.
// Not implemented yet: returning made-up numbers to customers is worse
// than admitting the gap. Real metrics arrive with the Prometheus/DCGM
// integration milestone.
func (s *Server) GetInstanceMetrics(c *gin.Context) {
	instanceID := c.Param("id")

	projectID, _, ok := s.requireScope(c)
	if !ok {
		return
	}

	// Confirm the instance exists and belongs to the caller before
	// admitting the feature is missing: a 501 for an instance the caller
	// does not own would still confirm it exists.
	if _, err := s.cluster.GetInstanceStatus(
		c.Request.Context(), scopeFor(projectID), instanceID); err != nil {
		if errors.Is(err, cluster.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "instance not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusNotImplemented, gin.H{
		"error":       "instance metrics are not available yet (Prometheus/DCGM integration is planned before GA)",
		"instance_id": instanceID,
	})
}

// DeploySDL deploys from SDL template
func (s *Server) DeploySDL(c *gin.Context) {
	// TODO: Implement SDL parser
	c.JSON(http.StatusNotImplemented, gin.H{
		"message": "SDL deployment to be implemented in next iteration",
	})
}

// Helper functions

// LabelProjectID scopes a pod to the owning TEEPIN project; every
// tenant-facing read/delete filters on it.
const LabelProjectID = "teepin.io/project-id"

// annotationInstanceType records the hardware-derived instance type on
// the pod so reads can report it without a database lookup.
const annotationInstanceType = "teepin.io/instance-type"

// requireProjectScope returns the caller's project ID. When
// persistence is enabled every compute operation must be scoped to a
// project (billing + tenant isolation); unauthenticated calls get 401.
// In standalone mode (no database) there is no tenancy and uuid.Nil is
// returned with ok=true.
func (s *Server) requireProjectScope(c *gin.Context) (uuid.UUID, bool) {
	projectID, _, ok := s.requireScope(c)
	return projectID, ok
}

// requireScope resolves the project AND the owning account for a
// request.
//
// Pod queries filter on the project label, which already isolates
// tenants; the account is carried alongside so persisted rows and
// billing records record their tenant directly, and so future queries
// can authorise without a join.
//
// Returns (Nil, Nil, true) in standalone mode (no database), where
// there is no tenancy to enforce.
func (s *Server) requireScope(c *gin.Context) (projectID, accountID uuid.UUID, ok bool) {
	if s.store == nil {
		return uuid.Nil, uuid.Nil, true
	}

	projectID, hasProject := auth.GetProjectID(c)
	if !hasProject {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": "authentication with a project is required (use an API key: Authorization: Bearer tpk_...)",
		})
		return uuid.Nil, uuid.Nil, false
	}

	// account_id is REQUIRED, not best-effort. A billing gate keyed on the
	// account is silently defeated by a caller whose accountID resolves to
	// Nil, so a token with a project but no account claim must be rejected
	// rather than waved through. Every credential the compute API accepts
	// (project API keys, current JWTs) carries an account since migration
	// 007; one that does not is stale and must re-authenticate.
	accountID, hasAccount := auth.GetAccountID(c)
	if !hasAccount || accountID == uuid.Nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": "your session predates account-scoped billing; sign in again or issue a new API key",
		})
		return uuid.Nil, uuid.Nil, false
	}

	return projectID, accountID, true
}

// endpointUUIDNamespace is an arbitrary, fixed UUID used purely as the
// namespace argument to endpointUUIDFor's UUIDv5 derivation — its value
// carries no meaning beyond "a constant unique to this codebase," it just
// needs to never change once instances exist that depend on it.
var endpointUUIDNamespace = uuid.MustParse("6f6e8e2e-2f0a-4b8a-9b0f-2c6a2e7c9d1a")

// endpointUUIDFor deterministically derives the UUID that names an
// instance's Service/Ingress/TLS resources (networking.Service's
// generateServiceName/generateIngressName) from its stable, customer-
// facing instance ID — see the CreateInstance call site's own comment for
// why this must be a pure function of instanceID rather than a randomly
// minted value: a Kumbha redeploy (DeployKumbhaSession) swaps an
// instance's pod in place under the SAME instanceID and needs to hand
// UpdateInstance the identical endpoint UUID the original create used, or
// endpoint provisioning (IsAlreadyExists-tolerant only when the name
// actually matches) would create a second, orphaned Service/Ingress pair
// instead of reusing the live one.
func endpointUUIDFor(instanceID string) uuid.UUID {
	return uuid.NewSHA1(endpointUUIDNamespace, []byte(instanceID))
}

// instanceSpec translates a validated request plus a resolved GPU
// allocation into a cluster placement decision.
//
// This is the boundary: everything the control plane decides — tenancy,
// pricing, which GPU — is baked in here, and the cluster layer executes
// it without re-deciding anything. Nothing below this point knows what a
// customer or an invoice is.
func (s *Server) instanceSpec(instanceID string, instanceUUID, projectID, accountID uuid.UUID, req *models.CreateInstanceRequest, allocation *gpu.Allocation) cluster.InstanceSpec {
	labels := map[string]string{
		"app.teepin.cloud/name": req.Name,
		// The full UUID is how endpoint teardown finds the Service and
		// Ingress belonging to this instance; the short instance ID is
		// what the Service selector matches.
		"teepin.io/instance-uuid": instanceUUID.String(),
	}

	spec := cluster.InstanceSpec{
		InstanceID: instanceID,
		Image:      req.Image,
		Command:    req.Command,
		Args:       req.Args,
		Env:        req.Env,
		Labels:     labels,
		CPUUnits:   req.CPUUnits,
		MemoryGB:   parseMemoryGB(req.Memory),
		// Control-plane policy, not agent-local config — see
		// WithEndpointConfig. Zero values are indistinguishable from "not
		// set" downstream, which is deliberate: an operator who never calls
		// WithEndpointConfig gets exactly today's behaviour.
		EndpointDomain: s.endpointDomain,
		EnableTLS:      s.enableTLS,
		TLSIssuer:      s.tlsIssuer,
		StorageGB:      req.StorageGB,
	}

	if s.ephemeralStorageGB > 0 {
		spec.EphemeralStorageGB = s.ephemeralStorageGB
	} else {
		spec.EphemeralStorageGB = defaultEphemeralStorageGB
	}

	// Every customer compute instance gets this — see
	// InstanceSpec.AllowFilesystemOwnershipChanges' own doc comment.
	// Confirmed decision, 2026-08-26: found live that nginx's own
	// (completely standard) docker-entrypoint startup dance —
	// chown("/var/cache/nginx/client_temp", 101) to drop from root to
	// its own less-privileged user — fails outright without it, and the
	// same pattern (start as root, chown, then run as a dedicated user)
	// is standard across most official images (postgres, redis, mysql,
	// and more) — the "drop ALL capabilities" policy added 2026-08-22
	// was breaking basic usability for ordinary, non-malicious images,
	// not just an edge case. Deliberately NOT applied to the Kumbha
	// agent pod itself (LaunchAgent, agent.go) — that workload has no
	// legitimate need for it, and stays on the fully locked-down default.
	spec.AllowFilesystemOwnershipChanges = true

	// Auto-attach the Kumbha build registry's pull secret ONLY for an
	// image that actually came from there — see
	// WithKumbhaBuildImagePullSecret's own doc comment for why this is
	// keyed off the image string itself rather than a customer-settable
	// field.
	if s.kumbhaBuildImagePullSecret != "" && s.kumbhaBuildImageRegistryPrefix != "" &&
		strings.HasPrefix(req.Image, s.kumbhaBuildImageRegistryPrefix) {
		spec.ImagePullSecret = s.kumbhaBuildImagePullSecret
	}

	if projectID != uuid.Nil {
		spec.ProjectID = projectID.String()
	}
	if accountID != uuid.Nil {
		spec.AccountID = accountID.String()
	}

	if allocation != nil {
		// Simulated allocations (local development on nodes with no real
		// GPU) must not request an extended resource: no device plugin
		// advertises it, and the pod would sit unschedulable forever.
		if !allocation.Simulated {
			spec.GPUResource = allocation.ResourceName
			spec.GPUQuantity = allocation.Quantity
		}
		spec.GPUVRAMGB = allocation.AllocatedVRAM
		spec.NodeName = allocation.NodeName
		spec.InstanceType = allocation.InstanceType
	}

	for _, port := range req.Ports {
		// Protocol previously always hardcoded "tcp" here, discarding
		// whatever the customer sent — buildPod already honours Protocol
		// (setting corev1.ProtocolUDP when asked), so this was a real
		// customer-visible bug, not just an omission. Default preserved
		// for a request that leaves it unset.
		protocol := port.Protocol
		if protocol == "" {
			protocol = "tcp"
		}
		spec.Ports = append(spec.Ports, cluster.PortMapping{
			Container: port.Container,
			Protocol:  protocol,
		})
	}

	return spec
}

// statusToInstance builds the customer-facing view of an instance from
// the cluster's live status and, where available, the stored record.
//
// The two sources answer different questions and neither is sufficient
// alone. The cluster is authoritative about what is *running* — it
// observes reality, so a pod that died is visible here before any
// database row changes. The database is authoritative about what was
// *agreed*: image, GPU allocation and price are commercial facts that
// must not change because a pod was rescheduled.
//
// record may be nil in standalone mode (no database), in which case the
// commercial fields are simply absent rather than guessed.
//
// domain is s.endpointDomain, passed in because statusToInstance is a
// package-level function with no Server access of its own — needed for
// the endpoint-derivation fallback below.
func statusToInstance(st cluster.InstanceStatus, record *compute.InstanceRecord, vramRate float64, domain string) models.Instance {
	instance := models.Instance{
		ID:     st.InstanceID,
		Status: st.Status,
		// The status message carries why a pod is failing (ImagePullBackOff
		// and friends). Customers cannot fix what they cannot see.
		StatusMessage: st.Message,
		UpdatedAt:     st.ObservedAt,
	}

	if record == nil {
		return instance
	}

	instance.Name = record.Name
	instance.Image = record.Image
	instance.CreatedAt = record.CreatedAt
	instance.CPUUnits = record.CPUUnits
	instance.Memory = fmt.Sprintf("%dGB", record.MemoryGB)
	instance.InstanceType = record.InstanceType
	instance.PublicIP = record.PublicIP
	instance.ContainerPort = record.ContainerPort
	instance.StorageGB = record.StorageGB

	instance.Endpoint, instance.DNSName, instance.TLSEnabled, instance.TLSReady =
		resolveEndpoint(record, domain)

	if record.GPUVRAMGB > 0 {
		instance.GPUVRAM = fmt.Sprintf("%dGB", record.GPUVRAMGB)
		instance.AllocatedVRAM = instance.GPUVRAM
		// Priced from the live rate rather than a stored one: an admin
		// price change must apply to the next quote, and this field is a
		// current quote, not an invoice line.
		instance.PricePerHour = gpu.PriceForVRAM(record.GPUVRAMGB, vramRate)
	}

	return instance
}

// resolveEndpoint returns record's stored endpoint, DNS name, and TLS
// state — or, if none was stored, derives the same self-healing fallback
// statusToInstance always has: the tunnel (pkg/api's tunnelMiddleware)
// routes by hostname convention alone and never consults record.Endpoint,
// so an instance can be genuinely reachable while these columns are NULL
// — found live 2026-08-21 against a real running instance
// (inst-0f0bdb64), whose endpoint/dns_name/tls_ready all came back null
// from the API despite curl succeeding against its derived hostname. Only
// fires when the stored value is empty — never overrides a populated one,
// so a datacenter instance's real cert-manager-issued endpoint is
// untouched. Requires ContainerPort > 0: an instance with no exposed port
// genuinely has no endpoint, and deriving one would be a broken link, not
// a helpful fallback.
//
// Pulled out of statusToInstance so GetKumbhaSession's own live-status
// enrichment can resolve the SAME endpoint GetInstance would report,
// rather than trusting cluster.InstanceStatus.EndpointURL directly — that
// field is only ever populated by DirectClient (statusWithEndpoint);
// AgentClient's cached status (the actual topology this platform runs in
// today, home-node placement) never carries it, so a Kumbha deploy's own
// session read came back with a real, running app and an empty endpoint
// — found live 2026-08-29 against inst-55b4d443, whose
// /v1/compute/instances/:id response had a working
// https://inst-55b4d443.dev.teepin.com the session endpoint never saw.
func resolveEndpoint(record *compute.InstanceRecord, domain string) (endpoint, dnsName string, tlsEnabled, tlsReady bool) {
	if record == nil {
		return "", "", false, false
	}
	endpoint, dnsName, tlsEnabled, tlsReady = record.Endpoint, record.DNSName, record.TLSEnabled, record.TLSReady
	if endpoint == "" && record.ContainerPort > 0 && domain != "" {
		dnsName = record.ID + "." + domain
		endpoint = "https://" + dnsName
		tlsEnabled = true
		tlsReady = true
	}
	return endpoint, dnsName, tlsEnabled, tlsReady
}

// parseMemoryGB parses memory strings like "32GB" or "512MB" to whole
// GB (rounded up) for persistence. Unparseable input yields 0.
// boolPtr returns a pointer to b, for Kubernetes API fields that
// distinguish "false" from "unset".
func boolPtr(b bool) *bool { return &b }

// getEnv returns the environment variable or a default. Unlike a bare
// os.Getenv this distinguishes "unset" from "explicitly empty": an
// explicitly empty value is returned as empty, which callers use to
// disable optional behaviour.
func getEnv(key, defaultValue string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return defaultValue
}

func parseMemoryGB(memory string) int {
	m := memoryRe.FindStringSubmatch(memory)
	if m == nil {
		return 0
	}
	value, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	if strings.EqualFold(m[2], "MB") || strings.EqualFold(m[2], "M") {
		value = (value + 1023) / 1024
	}
	return value
}

var memoryRe = regexp.MustCompile(`^(\d+)\s*([GgMm][Bb]?)$`)

// convertMemoryToK8sFormat converts memory strings like "16GB" to Kubernetes format "16Gi"
func convertMemoryToK8sFormat(memory string) string {
	// Parse common formats: 16GB, 32GB, etc.
	// Kubernetes expects Gi (gibibytes) not GB (gigabytes)
	var value string
	var unit string

	// Extract number and unit
	if len(memory) >= 3 {
		if memory[len(memory)-2:] == "GB" || memory[len(memory)-2:] == "gb" {
			value = memory[:len(memory)-2]
			unit = "Gi"
		} else if memory[len(memory)-2:] == "MB" || memory[len(memory)-2:] == "mb" {
			value = memory[:len(memory)-2]
			unit = "Mi"
		} else if memory[len(memory)-1:] == "G" || memory[len(memory)-1:] == "g" {
			value = memory[:len(memory)-1]
			unit = "Gi"
		} else if memory[len(memory)-1:] == "M" || memory[len(memory)-1:] == "m" {
			value = memory[:len(memory)-1]
			unit = "Mi"
		} else {
			// Already in Kubernetes format or unknown
			return memory
		}
	} else {
		return memory
	}

	return value + unit
}
