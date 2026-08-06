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
	"github.com/FlashbackAi/teepin-core/pkg/cluster"
	"github.com/FlashbackAi/teepin-core/pkg/compute"
	"github.com/FlashbackAi/teepin-core/pkg/gpu"
	"github.com/FlashbackAi/teepin-core/pkg/models"
	"github.com/FlashbackAi/teepin-core/pkg/networking"
)

// Server represents the API server
// PricingProvider supplies the live platform GPU rate. Implemented by
// billing.Service; nil means no database and the compiled-in default
// rate applies.
type PricingProvider interface {
	VRAMPricePerGBHour(ctx context.Context) float64
}

type Server struct {
	// cluster is the only route to GPU capacity. The API server holds no
	// Kubernetes client of its own: that is what lets the control plane
	// run on AWS while the GPUs sit in a datacenter elsewhere. See
	// pkg/cluster for the two implementations.
	cluster cluster.Client

	gpuAllocator      *gpu.Allocator
	networkingService *networking.Service
	store             *compute.Store  // nil in standalone mode (no database)
	pricing           PricingProvider // nil in standalone mode (no database)
}

// NewServer creates a new API server. store and pricing may be nil when
// the platform runs without a database (local standalone mode); in that
// case instances are not persisted, not billed, and priced at the
// compiled-in default rate.
func NewServer(clusterClient cluster.Client, gpuAllocator *gpu.Allocator, networkingService *networking.Service, store *compute.Store, pricing PricingProvider) *Server {
	return &Server{
		cluster:           clusterClient,
		gpuAllocator:      gpuAllocator,
		networkingService: networkingService,
		store:             store,
		pricing:           pricing,
	}
}

// scopeFor builds the cluster tenancy predicate for a request.
//
// Every compute read and write passes through here. In standalone mode
// (no database) there is no tenancy and the scope is unrestricted;
// otherwise it is pinned to the caller's project, which requireScope has
// already established is present.
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

// CreateInstance creates a new compute instance
func (s *Server) CreateInstance(c *gin.Context) {
	var req models.CreateInstanceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
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

	// Generate instance UUID
	instanceUUID := uuid.New()
	instanceID := fmt.Sprintf("inst-%s", instanceUUID.String()[:8])

	// Parse VRAM requirement
	var vramGB int
	var err error
	if req.GPUVRAM != "" {
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

	// The endpoint is provisioned by the cluster layer alongside the pod:
	// it must happen next to the cluster, not from the control plane,
	// which has no Kubernetes access once the agent split completes.
	var endpointInfo *networking.EndpointInfo
	if s.networkingService != nil && len(req.Ports) > 0 {
		// Read back what the cluster layer provisioned so the response
		// carries DNS and TLS state. Best-effort: a missing endpoint
		// record must not fail an instance that is already running.
		if info, epErr := s.networkingService.GetEndpointInfo(
			c.Request.Context(), instanceUUID, instanceID); epErr == nil {
			endpointInfo = info
		}
	}

	// Persist the instance — this is what billing meters. If it fails
	// the workload must not keep running unbilled: roll back.
	if s.store != nil {
		record := &compute.InstanceRecord{
			ID:           instanceID,
			AccountID:    accountID,
			ProjectID:    projectID,
			UserID:       userID,
			Name:         req.Name,
			Image:        req.Image,
			Status:       compute.StatusPending,
			CPUUnits:     req.CPUUnits,
			MemoryGB:     parseMemoryGB(req.Memory),
			K8sPodName:   result.PodName,
			K8sNamespace: "default",
		}
		if allocation != nil {
			record.InstanceType = allocation.InstanceType
			record.GPUVRAMGB = allocation.AllocatedVRAM
		}
		if endpointInfo != nil {
			record.Endpoint = endpointInfo.HTTPSURL
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

	// Add endpoint information
	if endpointInfo != nil {
		instance.Endpoint = endpointInfo.HTTPSURL
		instance.PublicIP = endpointInfo.PublicIP
		instance.DNSName = endpointInfo.DNSName
		instance.TLSEnabled = endpointInfo.TLSEnabled
		instance.TLSReady = endpointInfo.TLSReady
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
				"error": "GPU capacity is temporarily unreachable; instance state cannot be read right now",
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
		instances = append(instances, statusToInstance(st, records[st.InstanceID], rate))
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
				"error": "GPU capacity is temporarily unreachable; instance state cannot be read right now",
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

	c.JSON(http.StatusOK, statusToInstance(*st, record, s.vramRate(c.Request.Context())))
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
				"error": "GPU capacity is temporarily unreachable; the instance cannot be deleted right now",
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
				"error": "GPU capacity is temporarily unreachable; logs cannot be fetched right now",
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

	// Tokens issued before accounts existed carry no account claim;
	// the project label still scopes them correctly, so accept them
	// rather than locking out in-flight sessions.
	accountID, _ = auth.GetAccountID(c)

	return projectID, accountID, true
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
		spec.Ports = append(spec.Ports, cluster.PortMapping{
			Container: port.Container,
			Protocol:  "tcp",
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
func statusToInstance(st cluster.InstanceStatus, record *compute.InstanceRecord, vramRate float64) models.Instance {
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
	instance.Endpoint = record.Endpoint

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
