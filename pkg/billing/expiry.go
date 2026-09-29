// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"log"
	"time"
)

// CreditExpirer periodically forfeits the unspent part of expired credit
// grants (Service.ExpireCredits). Balance reads are already exact without
// it; this keeps the customer- and operator-visible ledger showing the
// forfeiture. Follows the UsageCollector pattern: a ticker, an immediate
// first run, and a select on stop/ctx.
type CreditExpirer struct {
	billingService *Service
	interval       time.Duration
	stopChan       chan struct{}
}

// NewCreditExpirer creates the job.
func NewCreditExpirer(billingService *Service) *CreditExpirer {
	return &CreditExpirer{
		billingService: billingService,
		interval:       1 * time.Hour,
		stopChan:       make(chan struct{}),
	}
}

// Start begins periodic forfeiture.
func (e *CreditExpirer) Start(ctx context.Context) {
	log.Println("Starting credit expirer...")

	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()

	e.run(ctx)
	for {
		select {
		case <-ticker.C:
			e.run(ctx)
		case <-e.stopChan:
			log.Println("Stopping credit expirer...")
			return
		case <-ctx.Done():
			log.Println("Credit expirer stopped (context cancelled)")
			return
		}
	}
}

// Stop stops the job.
func (e *CreditExpirer) Stop() {
	close(e.stopChan)
}

func (e *CreditExpirer) run(ctx context.Context) {
	n, err := e.billingService.ExpireCredits(ctx)
	if err != nil {
		log.Printf("WARN: credit expiry error: %v", err)
		return
	}
	if n > 0 {
		log.Printf("Credit expirer: forfeited the unspent part of %d expired credit lot(s)", n)
	}
}
