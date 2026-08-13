// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Package cluster defines the boundary between the TEEPIN control plane
// and GPU capacity.
//
// The control plane decides WHAT should exist — placement, tenancy,
// billing — and calls this interface to make it so. Two implementations
// exist:
//
//   - direct: an in-cluster Kubernetes client (the control plane runs
//     beside the GPUs). This is the pre-split behaviour, kept so the
//     migration can proceed in steps and roll back.
//   - agent:  a gRPC client that forwards to an agent dialling in from
//     the GPU datacenter. The control plane holds no Kubernetes
//     credentials at all.
//
// Nothing above this interface may import k8s.io/client-go. That
// constraint is what makes the control plane deployable to AWS.
package cluster

import (
	"context"
	"errors"
	"io"
	"time"
)

// Sentinel errors. Callers must branch on these rather than string
// matching: the control plane reacts differently to "capacity gone"
// (reallocate) than to "bad image" (tell the customer).
var (
	// ErrResourceExhausted means the GPU resource disappeared between
	// the inventory report and execution — the caller should reallocate
	// against fresh inventory rather than fail the request.
	ErrResourceExhausted = errors.New("gpu resource no longer available")

	// ErrNotFound means the instance does not exist in the cluster.
	// Deleting a missing instance is success, not this error.
	ErrNotFound = errors.New("instance not found in cluster")

	// ErrImagePull means the image is unreachable or credentials are
	// missing. Customer-visible and not retryable by us.
	ErrImagePull = errors.New("failed to pull container image")

	// ErrClusterUnavailable means no agent is connected for the target
	// provider, or the cluster is unreachable. Existing instances are
	// unaffected; only new work is blocked.
	ErrClusterUnavailable = errors.New("no cluster connection available")
)

// InstanceSpec is a fully-resolved placement decision. The GPU resource
// and node have already been chosen by the control-plane allocator; the
// cluster layer executes rather than re-decides.
type InstanceSpec struct {
	InstanceID string
	AccountID  string
	ProjectID  string

	Image   string
	Command []string
	Args    []string
	Env     map[string]string
	Labels  map[string]string

	CPUUnits int
	MemoryGB int

	// GPUResource empty means either CPU-only, or a simulated allocation
	// on a local-dev node: those account VRAM but request no device,
	// because no device plugin advertises the resource and the pod would
	// never be schedulable.
	GPUResource string // "nvidia.com/mig-1g.10gb"
	GPUQuantity int
	GPUVRAMGB   int    // billed VRAM, annotated for capacity accounting
	NodeName    string // pin to the node whose capacity was accounted

	// ProviderID names the agent session this instance must be dispatched
	// to. Empty = the single-provider datacenter path (dispatch to Any()).
	// Set for a home-class placement so the create reaches the SAME session
	// whose node was chosen — never an arbitrary one.
	ProviderID string
	// NodeClass is "home" for a consumer-node placement, "" for the
	// datacenter/GPU path. Carried through for the agent's own logging and
	// for future class-specific handling.
	NodeClass string

	// InstanceType is the hardware-derived name ("gpu.h100.mig-2g"),
	// annotated onto the pod so reads can report it without a database
	// lookup.
	InstanceType string

	// Public endpoint; empty Ports means no Service or Ingress.
	Ports           []PortMapping
	EndpointDomain  string
	EnableTLS       bool
	TLSIssuer       string
	ImagePullSecret string
}

type PortMapping struct {
	Container int
	Protocol  string
}

// InstanceResult describes what was actually created.
type InstanceResult struct {
	PodName     string
	EndpointURL string
	PublicIP    string
}

// InstanceStatus is the cluster's view of an instance. The reconciler
// moves the database towards this; the cluster is authoritative because
// it observes reality.
type InstanceStatus struct {
	InstanceID string
	Status     string // pending | running | failed | terminated
	PodName    string
	NodeName   string
	Message    string
	ObservedAt time.Time

	// Tenancy, carried so the agent-backed client can apply Scope to
	// statuses it holds in memory. The direct client filters with label
	// selectors and does not need these, but an in-memory cache has no
	// labels to filter on — without them a scoped read could not tell
	// whose instance it was looking at.
	AccountID string
	ProjectID string
}

// NodeInventory is a point-in-time report of one node's GPU capacity.
// It is NOT a reservation: the allocator may still lose a race, which
// surfaces as ErrResourceExhausted.
type NodeInventory struct {
	NodeName       string
	GPUProduct     string
	GPUModel       string
	MemoryGBPerGPU int
	GPUCount       int
	MIGCapable     bool
	SharedCapacity int
	SharedUsed     int
	UsedVRAMGB     int
	MIGResources   []MIGResource
	Ready          bool
}

type MIGResource struct {
	ResourceName string
	Profile      string
	Slices       int
	MemoryGB     int
	Capacity     int
	Used         int
}

// LogOptions controls log retrieval.
type LogOptions struct {
	TailLines int
	Follow    bool

	// Timestamps prefixes each line with its RFC3339 timestamp.
	Timestamps bool
}

// Scope is the tenancy predicate applied to every instance lookup.
//
// It is a required argument rather than an optional filter, and that is
// deliberate. The handlers this replaces scoped reads by appending a
// project label to the selector; anything that forgot to would let any
// authenticated caller read, log or delete another customer's instance
// by guessing its ID. Making Scope part of every signature means
// forgetting it is a compile error rather than a silent tenancy leak.
//
// The zero Scope matches everything and must only be used by internal
// callers that legitimately see all tenants — the reconciler and the
// billing collector. Use AllTenants to say so out loud.
type Scope struct {
	// ProjectID restricts to one project. Nil means unrestricted.
	ProjectID string

	// AccountID restricts to one account. Nil means unrestricted.
	AccountID string
}

// AllTenants is the explicit unrestricted scope, for the reconciler and
// billing collector. Named so that an unscoped query is a visible
// decision at the call site rather than an empty struct literal.
func AllTenants() Scope { return Scope{} }

// ProjectScope restricts to a single project — the normal case for any
// customer-facing request.
func ProjectScope(projectID string) Scope {
	return Scope{ProjectID: projectID}
}

// IsRestricted reports whether this scope filters anything. Used by
// implementations to decide whether a tenancy predicate is needed.
func (s Scope) IsRestricted() bool {
	return s.ProjectID != "" || s.AccountID != ""
}

// Client is the control plane's view of GPU capacity.
//
// Implementations must be safe for concurrent use: the API server calls
// this from many request goroutines simultaneously.
type Client interface {
	// CreateInstance realises a placement decision. Returns
	// ErrResourceExhausted if the chosen GPU resource is gone, so the
	// caller can reallocate.
	CreateInstance(ctx context.Context, spec InstanceSpec) (*InstanceResult, error)

	// DeleteInstance removes an instance and its endpoint. Deleting a
	// missing instance returns nil — idempotent, because commands may be
	// redelivered after a reconnect.
	//
	// An instance outside the scope is treated as missing: a caller must
	// not be able to delete another tenant's workload, nor learn that it
	// exists.
	DeleteInstance(ctx context.Context, scope Scope, instanceID string) error

	// GetInstanceStatus returns one instance's live status. Returns
	// ErrNotFound when the instance is outside the scope — indistinguishable
	// from absent, so existence does not leak across tenants.
	GetInstanceStatus(ctx context.Context, scope Scope, instanceID string) (*InstanceStatus, error)

	// ListInstanceStatuses returns TEEPIN-managed instances within the
	// scope. The reconciler passes AllTenants: it must see instances that
	// vanished (node reboot, eviction) across every customer to stop
	// billing them.
	ListInstanceStatuses(ctx context.Context, scope Scope) ([]InstanceStatus, error)

	// StreamLogs writes logs to w. With Follow it blocks until ctx is
	// cancelled or the instance exits. Logs frequently contain secrets and
	// customer data, so the scope is enforced exactly as for reads.
	StreamLogs(ctx context.Context, scope Scope, instanceID string, opts LogOptions, w io.Writer) error

	// Inventory returns current GPU capacity, which the allocator
	// reasons over.
	Inventory(ctx context.Context) ([]NodeInventory, error)

	// Healthy reports whether cluster operations can currently succeed.
	// False means new instances are refused while account, billing and
	// usage endpoints continue to serve — the point of the split.
	Healthy(ctx context.Context) bool
}
