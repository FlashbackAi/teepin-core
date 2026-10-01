// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/api"
	"github.com/FlashbackAi/teepin-core/pkg/billing"
	"github.com/FlashbackAi/teepin-core/pkg/build"
	"github.com/FlashbackAi/teepin-core/pkg/cluster"
	"github.com/FlashbackAi/teepin-core/pkg/compute"
	"github.com/FlashbackAi/teepin-core/pkg/email"
	"github.com/FlashbackAi/teepin-core/pkg/inference"
	"github.com/FlashbackAi/teepin-core/pkg/inferencegateway"
	"github.com/FlashbackAi/teepin-core/pkg/kumbha"
	"github.com/FlashbackAi/teepin-core/pkg/modelcatalog"
	"github.com/FlashbackAi/teepin-core/pkg/nodes"
	"github.com/FlashbackAi/teepin-core/pkg/payments"
)

// The two adapters below translate between the concrete *payments.Client
// and the neutral interfaces the billing and api packages declare. They
// exist so neither of those packages imports stripe-go — the translation
// happens once, here at the composition root.

// stripeGatewayAdapter makes *payments.Client satisfy
// billing.StripeGateway (which speaks billing.CardSummary, not
// payments.CardDetails).
type stripeGatewayAdapter struct {
	c *payments.Client
}

func newStripeGatewayAdapter(c *payments.Client) *stripeGatewayAdapter {
	return &stripeGatewayAdapter{c: c}
}

func (a *stripeGatewayAdapter) EnsureCustomer(existingID, email, name, accountNumber string) (string, error) {
	return a.c.EnsureCustomer(existingID, email, name, accountNumber)
}

func (a *stripeGatewayAdapter) CreateSetupIntent(customerID, currency string) (string, string, error) {
	return a.c.CreateSetupIntent(customerID, currency)
}

func (a *stripeGatewayAdapter) GetPaymentMethod(pmID string) (*billing.CardSummary, error) {
	d, err := a.c.GetPaymentMethod(pmID)
	if err != nil {
		return nil, err
	}
	return &billing.CardSummary{
		Brand:           d.Brand,
		Last4:           d.Last4,
		ExpMonth:        d.ExpMonth,
		ExpYear:         d.ExpYear,
		PaymentMethodID: d.PaymentMethodID,
	}, nil
}

func (a *stripeGatewayAdapter) DetachPaymentMethod(pmID string) error {
	return a.c.DetachPaymentMethod(pmID)
}

func (a *stripeGatewayAdapter) CreatePaymentIntent(customerID, pmID, currency string, amountCents int64, invoiceID, idempotencyKey string) (string, string, error) {
	return a.c.CreatePaymentIntent(customerID, pmID, currency, amountCents, invoiceID, idempotencyKey)
}

func (a *stripeGatewayAdapter) CreateAutoTopUpPaymentIntent(customerID, pmID string, amountCents int64, currency, topUpID, accountNumber, idempotencyKey string) (string, string, error) {
	return a.c.CreateAutoTopUpPaymentIntent(customerID, pmID, amountCents, currency, topUpID, accountNumber, idempotencyKey)
}

func (a *stripeGatewayAdapter) CreateTopUpPaymentIntent(customerID string, amountCents int64, currency, topUpID, accountNumber, idempotencyKey string) (string, string, error) {
	return a.c.CreateTopUpPaymentIntent(customerID, amountCents, currency, topUpID, accountNumber, idempotencyKey)
}

// stripeWebhookAdapter makes *payments.Client satisfy
// api.StripeWebhookVerifier, translating the verified event and card
// details into the api package's own types.
type stripeWebhookAdapter struct {
	c *payments.Client
}

func newStripeWebhookAdapter(c *payments.Client) *stripeWebhookAdapter {
	return &stripeWebhookAdapter{c: c}
}

func (a *stripeWebhookAdapter) VerifyWebhook(payload []byte, sigHeader string) (*api.WebhookEvent, error) {
	e, err := a.c.VerifyWebhook(payload, sigHeader)
	if err != nil {
		return nil, err
	}
	out := &api.WebhookEvent{
		Type:                e.Type,
		SetupIntentID:       e.SetupIntentID,
		PaymentMethodID:     e.PaymentMethodID,
		PaymentIntentID:     e.PaymentIntentID,
		InvoiceID:           e.InvoiceID,
		FailureReason:       e.FailureReason,
		IsTopUp:             e.Purpose == payments.TopUpPurpose,
		AmountReceivedCents: e.AmountReceivedCents,
		Currency:            e.Currency,
	}
	if e.Card != nil {
		out.Card = &api.CardDetails{
			Brand:           e.Card.Brand,
			Last4:           e.Card.Last4,
			ExpMonth:        e.Card.ExpMonth,
			ExpYear:         e.Card.ExpYear,
			PaymentMethodID: e.Card.PaymentMethodID,
		}
	}
	return out, nil
}

func (a *stripeWebhookAdapter) PaymentMethodSummary(paymentMethodID string) (string, error) {
	return a.c.PaymentMethodSummary(paymentMethodID)
}

func (a *stripeWebhookAdapter) GetCard(paymentMethodID string) (*api.CardDetails, error) {
	d, err := a.c.GetPaymentMethod(paymentMethodID)
	if err != nil {
		return nil, err
	}
	return &api.CardDetails{
		Brand:           d.Brand,
		Last4:           d.Last4,
		ExpMonth:        d.ExpMonth,
		ExpYear:         d.ExpYear,
		PaymentMethodID: d.PaymentMethodID,
	}, nil
}

// nodeAuthAdapter makes *nodes.Service satisfy cluster.NodeAuthenticator,
// translating a resolved *nodes.Node into the neutral cluster.NodeIdentity.
// It lives here so pkg/cluster never imports pkg/nodes (which would couple the
// k8s-free cluster boundary to the node store).
type nodeAuthAdapter struct {
	svc *nodes.Service
}

func newNodeAuthAdapter(svc *nodes.Service) *nodeAuthAdapter {
	return &nodeAuthAdapter{svc: svc}
}

func (a *nodeAuthAdapter) AuthenticateNode(ctx context.Context, credential string) (*cluster.NodeIdentity, error) {
	n, err := a.svc.AuthenticateNode(ctx, credential)
	if err != nil {
		return nil, err
	}
	return &cluster.NodeIdentity{
		NodeName:   n.NodeName,
		ProviderID: n.ProviderID,
		Class:      n.Class,
	}, nil
}

// nodeReporterAdapter makes *nodes.Service satisfy cluster.NodeReporter. It
// writes asynchronously with a background context so a slow DB never stalls
// the gRPC message pump, and best-effort: a failed persist is logged, not
// retried into the hot path (the next heartbeat will try again).
type nodeReporterAdapter struct {
	svc *nodes.Service
}

func newNodeReporterAdapter(svc *nodes.Service) *nodeReporterAdapter {
	return &nodeReporterAdapter{svc: svc}
}

func (a *nodeReporterAdapter) ReportSeen(seen cluster.NodeSeen) {
	go func() {
		if err := a.svc.UpsertSeen(context.Background(), seen.Class, nodes.NodeSpecs{
			NodeName:         seen.NodeName,
			ProviderID:       seen.ProviderID,
			Region:           seen.Region,
			CPUCores:         seen.CPUCores,
			MemoryGB:         seen.MemoryGB,
			OS:               seen.OS,
			Arch:             seen.Arch,
			PCores:           seen.PCores,
			ECores:           seen.ECores,
			GPUModel:         seen.GPUModel,
			GPUCount:         seen.GPUCount,
			MIGCapable:       seen.MIGCapable,
			AgentVersion:     seen.AgentVersion,
			K8sReady:         seen.K8sReady,
			CPUUsedPercent:   seen.CPUUsedPercent,
			MemoryUsedGB:     seen.MemoryUsedGB,
			GPUUsedVRAMGB:    seen.GPUUsedVRAMGB,
			NetworkRxMbps:    seen.NetworkRxMbps,
			NetworkTxMbps:    seen.NetworkTxMbps,
			StorageReadMbps:  seen.StorageReadMbps,
			StorageWriteMbps: seen.StorageWriteMbps,
		}); err != nil {
			log.Printf("WARN: node write-through failed for %s: %v", seen.NodeName, err)
		}
	}()
}

// instanceMetricsReporterAdapter makes *compute.Store satisfy
// cluster.InstanceMetricsReporter. Same posture as nodeReporterAdapter:
// writes asynchronously with a background context so a slow DB never
// stalls the gRPC message pump, best-effort (a failed persist is logged,
// not retried — the next sweep, ~30s later, will try again).
type instanceMetricsReporterAdapter struct {
	store *compute.Store
}

func newInstanceMetricsReporterAdapter(store *compute.Store) *instanceMetricsReporterAdapter {
	return &instanceMetricsReporterAdapter{store: store}
}

func (a *instanceMetricsReporterAdapter) ReportInstanceMetricsSeen(seen []cluster.InstanceMetricSeen) {
	go func() {
		writes := make([]compute.InstanceMetricWrite, len(seen))
		for i, s := range seen {
			writes[i] = compute.InstanceMetricWrite{
				InstanceID:     s.InstanceID,
				CPUUsedPercent: s.CPUUsedPercent,
				MemoryUsedGB:   s.MemoryUsedGB,
				NetworkRxMbps:  s.NetworkRxMbps,
				NetworkTxMbps:  s.NetworkTxMbps,
				StorageUsedGB:  s.StorageUsedGB,
			}
		}
		if err := a.store.RecordInstanceMetrics(context.Background(), writes); err != nil {
			log.Printf("WARN: instance metrics write-through failed for %d instance(s): %v", len(writes), err)
		}
	}()
}

// nodePlacerAdapter makes *nodes.Service satisfy api.NodePlacer, translating
// the nodes package's Placement/errors into the neutral shapes the api
// package expects — so api never imports pkg/nodes.
type nodePlacerAdapter struct {
	svc *nodes.Service
}

func newNodePlacerAdapter(svc *nodes.Service) *nodePlacerAdapter {
	return &nodePlacerAdapter{svc: svc}
}

func (a *nodePlacerAdapter) PlaceCPU(ctx context.Context, arch string, cpuUnits, memoryGB, pCores, eCores int) (string, string, string, *int, *int, error) {
	p, err := a.svc.PlaceCPU(ctx, nodes.PlacementReq{
		Arch: arch, CPUUnits: cpuUnits, MemoryGB: memoryGB, PCores: pCores, ECores: eCores,
	})
	if err != nil {
		return "", "", "", nil, nil, err
	}
	return p.NodeName, p.ProviderID, p.Arch, p.PCoresUsed, p.ECoresUsed, nil
}

func (a *nodePlacerAdapter) IsNoCapacity(err error) bool {
	return errors.Is(err, nodes.ErrNoHomeCapacity)
}

func (a *nodePlacerAdapter) IsArchUnavailable(err error) bool {
	return errors.Is(err, nodes.ErrArchUnavailable)
}

func (a *nodePlacerAdapter) IsInsufficientCapacity(err error) bool {
	return errors.Is(err, nodes.ErrInsufficientCapacity)
}

// hiddenWorkloadAdapter makes cluster.Client satisfy
// nodes.HiddenWorkloadCounter — see that interface's own doc comment for
// why this exists. Knows the fixed CPU/memory footprint of each of
// Teepin's three internal pod types (Kumbha agent, its screenshot-capture
// pod, Kaniko builds) via their exported name-prefix constants, so
// pkg/nodes never needs to import pkg/kumbha or pkg/build just to
// recognise one.
type hiddenWorkloadAdapter struct {
	cluster                        cluster.Client
	agentCPU, agentMemGB           int
	screenshotCPU, screenshotMemGB int
	buildCPU, buildMemGB           int
}

func newHiddenWorkloadAdapter(c cluster.Client, agentCPU, agentMemGB, buildCPU, buildMemGB int) *hiddenWorkloadAdapter {
	return &hiddenWorkloadAdapter{
		cluster:         c,
		agentCPU:        agentCPU,
		agentMemGB:      agentMemGB,
		screenshotCPU:   kumbha.ScreenshotCPUUnits,
		screenshotMemGB: kumbha.ScreenshotMemoryGB,
		buildCPU:        buildCPU,
		buildMemGB:      buildMemGB,
	}
}

func (a *hiddenWorkloadAdapter) HiddenUsageByNode(ctx context.Context) (map[string]nodes.HiddenUsage, error) {
	statuses, err := a.cluster.ListInstanceStatuses(ctx, cluster.AllTenantsIncludingHidden())
	if err != nil {
		return nil, err
	}

	out := map[string]nodes.HiddenUsage{}
	for _, st := range statuses {
		if !st.Hidden || st.NodeName == "" {
			continue
		}
		// A pod already terminated or failed holds nothing — only a
		// still-scheduled one (running, or pending while its image pulls)
		// occupies the node. Matches podStatus's own phase mapping in
		// pkg/cluster/direct.go: PodSucceeded/PodFailed both map here to
		// something other than "running"/"pending".
		if st.Status != compute.StatusRunning && st.Status != compute.StatusPending {
			continue
		}

		var cpu, mem int
		switch {
		case strings.HasPrefix(st.PodName, build.PodNamePrefix):
			cpu, mem = a.buildCPU, a.buildMemGB
		case strings.HasPrefix(st.PodName, kumbha.ScreenshotPodNamePrefix):
			cpu, mem = a.screenshotCPU, a.screenshotMemGB
		case strings.HasPrefix(st.PodName, kumbha.AgentPodNamePrefix):
			cpu, mem = a.agentCPU, a.agentMemGB
		default:
			// An unrecognised hidden pod (a future addition this adapter
			// was never updated for) — count nothing rather than guess a
			// size that could be wrong in either direction.
			continue
		}
		u := out[st.NodeName]
		u.CPUCores += cpu
		u.MemoryGB += mem
		out[st.NodeName] = u
	}
	return out, nil
}

// kumbhaModelBackend makes the model catalog plus Teepin Inference's gateway
// satisfy kumbha.ModelBackend: Kumbha's completions are served exactly like
// a customer's own API call, from whichever catalog models are enabled for
// Kumbha. Lives here so pkg/kumbha imports neither package.
type kumbhaModelBackend struct {
	catalog *modelcatalog.Service
	gateway *inferencegateway.Gateway
}

func newKumbhaModelBackend(catalog *modelcatalog.Service, gateway *inferencegateway.Gateway) *kumbhaModelBackend {
	return &kumbhaModelBackend{catalog: catalog, gateway: gateway}
}

func (b *kumbhaModelBackend) KumbhaModels(ctx context.Context) ([]kumbha.Model, error) {
	models, err := b.catalog.ListKumbhaModels(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]kumbha.Model, 0, len(models))
	for _, m := range models {
		unavailable := ""
		if b.gateway != nil {
			unavailable = b.gateway.Status(ctx, m).Unservable()
		}
		out = append(out, kumbha.Model{
			Unavailable:           unavailable,
			Route:                 m.ModelRoute,
			DisplayName:           m.DisplayName,
			Engine:                m.Engine,
			Confidential:          m.Provider == modelcatalog.ProviderTinfoilConfidential,
			SelfHosted:            !m.Provider.IsThirdParty(),
			SupportsTools:         m.SupportsTools,
			SupportsVision:        m.SupportsVision,
			SupportsAudio:         m.SupportsAudio,
			ContextWindow:         m.ContextWindow,
			MaxOutputTokens:       m.MaxOutputTokens,
			InputPricePerMillion:  m.InputPricePerMillion,
			OutputPricePerMillion: m.OutputPricePerMillion,
		})
	}
	return out, nil
}

func (b *kumbhaModelBackend) Complete(ctx context.Context, accountID string, req inference.Request) (*inference.Response, error) {
	resp, err := b.gateway.Complete(ctx, accountID, req)
	// The gateway declining to dispatch (a concurrency ceiling already
	// full) is, to Kumbha, one more reason this model is unavailable right
	// now — the signal to try its next model.
	if errors.Is(err, inferencegateway.ErrThrottled) {
		return nil, fmt.Errorf("%w: %v", inference.ErrProviderUnavailable, err)
	}
	return resp, err
}

// nodeCapacityAdapter makes *nodes.Service satisfy kumbha.NodeCapacityLister,
// translating nodes.NodeCapacity into the neutral kumbha.CapacityCandidate —
// so pkg/kumbha never imports pkg/nodes. Filters to home nodes currently
// online, the same predicate HomeCapacitySummary applies internally
// (capacity.go:268) — a datacenter node or an offline home node is never a
// candidate for Kumbha's own agent pod placement. Exists specifically to
// fix a live incident: LaunchAgent used to dispatch via
// cluster.Registry.Any() (a capacity-blind, randomized pick among connected
// sessions), which landed a build on a home node with zero free memory and
// left the pod Pending forever with no explanation — confirmed via
// `kubectl describe pod` showing "Insufficient memory" on 2026-09-22.
type nodeCapacityAdapter struct {
	svc *nodes.Service
}

func newNodeCapacityAdapter(svc *nodes.Service) *nodeCapacityAdapter {
	return &nodeCapacityAdapter{svc: svc}
}

func (a *nodeCapacityAdapter) ListNodeCapacity(ctx context.Context) ([]kumbha.CapacityCandidate, error) {
	all, err := a.svc.ListNodeCapacity(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]kumbha.CapacityCandidate, 0, len(all))
	for _, c := range all {
		if c.Class != nodes.ClassHome || c.Status != nodes.StatusOnline {
			continue
		}
		out = append(out, kumbha.CapacityCandidate{
			ProviderID: c.ProviderID,
			FreeCPU:    c.FreeCPU,
			FreeMemGB:  c.FreeMemGB,
		})
	}
	return out, nil
}

// instanceProxyTarget makes *compute.Store (+ a *cluster.Registry for the
// single-session fallback below) satisfy cluster.ProxyTarget — the Stage 3
// tunnel's hostname-to-session lookup (plan B2). Lives here, not in
// pkg/cluster, for the same reason every other adapter does: it needs
// pkg/compute's concrete store, and pkg/cluster must not import it.
type instanceProxyTarget struct {
	store    *compute.Store
	registry *cluster.Registry
}

func newInstanceProxyTarget(store *compute.Store, registry *cluster.Registry) *instanceProxyTarget {
	return &instanceProxyTarget{store: store, registry: registry}
}

// ResolveProvider looks up instanceID's owning provider and container port.
//
// Home-class instances always carry a ProviderID (Stage 2's dispatch fix —
// see AgentClient.CreateInstance — requires it to route the create itself,
// so it is never empty here). Datacenter instances predate per-instance
// ProviderID entirely: the single-agent datacenter path dispatches via
// registry.Any() and never had a reason to record which provider it landed
// on. Rather than leave datacenter instances unreachable through the
// unified edge (Stage 3 plan B1) until every instance record is backfilled,
// an empty ProviderID falls back to the registry's own session — correct
// as long as there is at most one non-home agent connected, exactly the
// assumption registry.Any() already makes everywhere else in this
// datacenter-single-agent pilot.
func (t *instanceProxyTarget) ResolveProvider(ctx context.Context, instanceID string) (string, int32, bool) {
	rec, err := t.store.Get(ctx, instanceID)
	if err != nil || rec == nil || rec.TerminatedAt != nil {
		return "", 0, false
	}

	port := int32(rec.ContainerPort)

	if rec.ProviderID != "" {
		return rec.ProviderID, port, true
	}

	// No provider recorded (pre-Stage-3 datacenter instance) — fall back to
	// the sole connected session, if there is exactly one to be unambiguous
	// about.
	if t.registry != nil && t.registry.Count() == 1 {
		if session, ok := t.registry.Any(); ok {
			return session.ProviderID, port, true
		}
	}

	return "", 0, false
}

// resourceSuspender implements billing.ResourceSuspender: it tears down
// an account's running instances at the cluster and marks each
// terminated in the store. Lives here because it needs both the cluster
// client and the instance store, which billing must not depend on.
type resourceSuspender struct {
	cluster cluster.Client
	store   *compute.Store
}

func newResourceSuspender(c cluster.Client, store *compute.Store) *resourceSuspender {
	return &resourceSuspender{cluster: c, store: store}
}

func (r *resourceSuspender) SuspendAccountResources(ctx context.Context, accountID uuid.UUID) (int, error) {
	instances, err := r.store.ListActiveByAccount(ctx, accountID)
	if err != nil {
		return 0, err
	}
	stopped := 0
	for _, inst := range instances {
		// Scope to the instance's own project — the same tenancy predicate
		// a normal delete uses, so a bug here cannot reach another tenant.
		scope := cluster.ProjectScope(inst.ProjectID.String())
		if err := r.cluster.DeleteInstance(ctx, scope, inst.ID); err != nil {
			// Log and continue: one stuck instance must not block
			// suspending the rest of the account.
			log.Printf("WARN: suspend: failed to delete instance %s: %v", inst.ID, err)
			continue
		}
		if err := r.store.MarkTerminated(ctx, inst.ID); err != nil {
			log.Printf("WARN: suspend: deleted instance %s but failed to mark terminated: %v", inst.ID, err)
			continue
		}
		stopped++
	}
	return stopped, nil
}

// computeStopper implements billing.ComputeStopper for the credit enforcer:
// it ends the named instances at the cluster and marks each terminated.
// Only instances that are still active for the given account are touched,
// so an id from another tenant (or one already gone) is ignored.
type computeStopper struct {
	cluster cluster.Client
	store   *compute.Store
}

func newComputeStopper(c cluster.Client, store *compute.Store) *computeStopper {
	return &computeStopper{cluster: c, store: store}
}

func (s *computeStopper) StopInstances(ctx context.Context, accountID uuid.UUID, instanceIDs []string) ([]string, error) {
	want := make(map[string]bool, len(instanceIDs))
	for _, id := range instanceIDs {
		want[id] = true
	}
	active, err := s.store.ListActiveByAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	var stopped []string
	var errs []error
	for _, inst := range active {
		if !want[inst.ID] {
			continue
		}
		scope := cluster.ProjectScope(inst.ProjectID.String())
		if err := s.cluster.DeleteInstance(ctx, scope, inst.ID); err != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", inst.ID, err))
			continue
		}
		if err := s.store.MarkTerminated(ctx, inst.ID); err != nil {
			errs = append(errs, fmt.Errorf("mark %s terminated: %w", inst.ID, err))
			continue
		}
		stopped = append(stopped, inst.ID)
	}
	return stopped, errors.Join(errs...)
}

// HoldInstances stops instances that have a persistent disk WITHOUT deleting
// the disk (billing.ComputeStopper). An instance is held only if it can be
// started again later - its launch spec must be stored - and the cluster can
// keep the disk; otherwise it is reported and left running, never deleted:
// its disk is the customer's data.
func (s *computeStopper) HoldInstances(ctx context.Context, accountID uuid.UUID, instanceIDs []string) ([]string, error) {
	want := make(map[string]bool, len(instanceIDs))
	for _, id := range instanceIDs {
		want[id] = true
	}
	active, err := s.store.ListActiveByAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	stopper, canStop := s.cluster.(cluster.InstanceStopper)
	var held []string
	var errs []error
	for _, inst := range active {
		if !want[inst.ID] {
			continue
		}
		if !canStop {
			errs = append(errs, fmt.Errorf("%s: this cluster client cannot stop an instance with its disk kept", inst.ID))
			continue
		}
		if _, err := s.store.LoadLaunchSpec(ctx, inst.ID); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", inst.ID, err))
			continue
		}
		if r, ok := s.cluster.(cluster.InstanceRouter); ok {
			r.RouteInstance(inst.ID, inst.ProviderID)
		}
		scope := cluster.ProjectScope(inst.ProjectID.String())
		if err := stopper.StopInstance(ctx, scope, inst.ID); err != nil {
			errs = append(errs, fmt.Errorf("%s: stop: %w", inst.ID, err))
			continue
		}
		if _, err := s.store.MarkStopped(ctx, inst.ID); err != nil {
			// The pod is gone but the record still says running; the
			// reconciler would mark it terminated and lose the disk's
			// record. Say so loudly - this needs a human.
			errs = append(errs, fmt.Errorf("%s: stopped but not recorded: %w", inst.ID, err))
			continue
		}
		held = append(held, inst.ID)
	}
	return held, errors.Join(errs...)
}

// heldDisks is the storage-hold view of stopped instances' disks
// (billing.HeldStorage): the disks are covered by the same 7-day hold as
// object storage, and deleted when it expires.
type heldDisks struct {
	cluster cluster.Client
	store   *compute.Store
}

func newHeldDisks(c cluster.Client, store *compute.Store) *heldDisks {
	return &heldDisks{cluster: c, store: store}
}

func (h *heldDisks) Name() string { return "instance disks" }

func (h *heldDisks) AccountsWithStorage(ctx context.Context) ([]uuid.UUID, error) {
	return h.store.AccountsWithStoppedInstances(ctx)
}

// PurgeAccount deletes every stopped instance of the account together with
// its disk. Each is routed to the node that holds its disk first: a stopped
// instance has no live status the client could route by.
func (h *heldDisks) PurgeAccount(ctx context.Context, accountID uuid.UUID) error {
	stopped, err := h.store.ListStoppedByAccount(ctx, accountID)
	if err != nil {
		return err
	}
	var errs []error
	for _, inst := range stopped {
		if r, ok := h.cluster.(cluster.InstanceRouter); ok {
			r.RouteInstance(inst.ID, inst.ProviderID)
		}
		if err := h.cluster.DeleteInstance(ctx, cluster.AllTenants(), inst.ID); err != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", inst.ID, err))
			continue
		}
		if err := h.store.MarkTerminated(ctx, inst.ID); err != nil {
			errs = append(errs, fmt.Errorf("mark %s terminated: %w", inst.ID, err))
		}
	}
	return errors.Join(errs...)
}

// newEmailSender picks how email leaves the platform: Amazon SES when
// TEEPIN_EMAIL_FROM names a verified sender, otherwise a log-only sender so
// the rest of the system runs unchanged in development.
func newEmailSender() email.Sender {
	from := os.Getenv("TEEPIN_EMAIL_FROM")
	if from == "" {
		log.Println("Email not configured (TEEPIN_EMAIL_FROM unset): notices are logged, not sent")
		return email.LogSender{}
	}
	cfg, err := awsconfig.LoadDefaultConfig(context.Background())
	if err != nil {
		log.Printf("WARN: email disabled: could not load AWS configuration: %v", err)
		return email.LogSender{}
	}
	sender, err := email.NewSESSender(sesv2.NewFromConfig(cfg), from)
	if err != nil {
		log.Printf("WARN: email disabled: %v", err)
		return email.LogSender{}
	}
	log.Println("Email enabled via Amazon SES")
	return sender
}
