// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package main

import (
	"context"
	"errors"
	"log"

	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/api"
	"github.com/FlashbackAi/teepin-core/pkg/billing"
	"github.com/FlashbackAi/teepin-core/pkg/cluster"
	"github.com/FlashbackAi/teepin-core/pkg/compute"
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
		Type:            e.Type,
		SetupIntentID:   e.SetupIntentID,
		PaymentMethodID: e.PaymentMethodID,
		PaymentIntentID: e.PaymentIntentID,
		InvoiceID:       e.InvoiceID,
		FailureReason:   e.FailureReason,
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
			NodeName:     seen.NodeName,
			ProviderID:   seen.ProviderID,
			Region:       seen.Region,
			CPUCores:     seen.CPUCores,
			MemoryGB:     seen.MemoryGB,
			GPUModel:     seen.GPUModel,
			GPUCount:     seen.GPUCount,
			MIGCapable:   seen.MIGCapable,
			AgentVersion: seen.AgentVersion,
			K8sReady:     seen.K8sReady,
		}); err != nil {
			log.Printf("WARN: node write-through failed for %s: %v", seen.NodeName, err)
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

func (a *nodePlacerAdapter) PlaceCPU(ctx context.Context, arch string, cpuUnits, memoryGB int) (string, string, string, error) {
	p, err := a.svc.PlaceCPU(ctx, nodes.PlacementReq{Arch: arch, CPUUnits: cpuUnits, MemoryGB: memoryGB})
	if err != nil {
		return "", "", "", err
	}
	return p.NodeName, p.ProviderID, p.Arch, nil
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
