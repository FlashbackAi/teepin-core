// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// EnforcementMode says what the credit enforcer does about an account that
// is about to run out of credit.
type EnforcementMode string

const (
	// EnforceOff disables the enforcer entirely.
	EnforceOff EnforcementMode = "off"
	// EnforceDryRun evaluates every account and logs what it would stop,
	// without stopping anything. For rolling the enforcer out safely.
	EnforceDryRun EnforcementMode = "dry-run"
	// EnforceOn stops compute when credit runs out.
	EnforceOn EnforcementMode = "enforce"
)

// ParseEnforcementMode reads the TEEPIN_CREDIT_ENFORCEMENT setting. Empty
// means enforce: prepaid billing is meaningless if zero balance is not
// actually the end of paid compute. An unrecognised value is an error, not a
// silent fallback - a typo must not quietly turn enforcement off.
func ParseEnforcementMode(v string) (EnforcementMode, error) {
	switch m := EnforcementMode(strings.ToLower(strings.TrimSpace(v))); m {
	case "":
		return EnforceOn, nil
	case EnforceOff, EnforceDryRun, EnforceOn:
		return m, nil
	default:
		return "", fmt.Errorf("unknown credit enforcement mode %q (want enforce, dry-run or off)", v)
	}
}

const (
	// DefaultEnforcementInterval is how often accounts are evaluated. The
	// projection below covers the time between ticks, so this only bounds
	// how long a fully-exhausted account can keep running.
	DefaultEnforcementInterval = 1 * time.Minute

	// enforcementHorizon is how far ahead the enforcer looks. An account is
	// stopped when its projected credit would be gone within this window:
	// two ticks, so the balance cannot reach zero between one evaluation and
	// the next (which would let compute run for free).
	enforcementHorizon = 2 * DefaultEnforcementInterval

	// heldLogInterval rate-limits the repeated error for an instance that
	// cannot be stopped yet, so a stuck instance does not flood the log
	// every minute.
	heldLogInterval = 1 * time.Hour
)

// ComputeStopper ends running instances. It lives outside this package
// because stopping compute needs the cluster and node-agent clients, which
// billing must not import.
type ComputeStopper interface {
	// StopInstances ends the given instances of one account. It returns the
	// IDs it stopped; an error alongside a non-empty list means partial
	// success.
	StopInstances(ctx context.Context, accountID uuid.UUID, instanceIDs []string) ([]string, error)

	// HoldInstances stops instances that have a persistent disk WITHOUT
	// deleting the disk, so the customer can start them again after adding
	// credit (or lose them when the storage hold expires). Same return
	// contract as StopInstances. An instance that cannot be held safely (no
	// stored launch spec, or an agent that cannot keep the disk) is reported
	// as an error and left running - never deleted.
	HoldInstances(ctx context.Context, accountID uuid.UUID, instanceIDs []string) ([]string, error)
}

// tailSettler bills the unmetered final stretch of an account's compute.
type tailSettler interface {
	CollectAccount(ctx context.Context, accountID uuid.UUID) error
}

// CreditEnforcer keeps prepaid accounts from running paid compute on credit
// they do not have. Each tick it projects, per account, how much credit
// remains once the not-yet-metered time is charged, and stops the account's
// paid compute when that would run out within enforcementHorizon.
//
// Metering itself (UsageCollector) runs on a coarser schedule; the
// projection is what makes enforcement independent of it.
type CreditEnforcer struct {
	db       *sql.DB
	billing  *Service
	stopper  ComputeStopper
	settler  tailSettler
	mode     EnforcementMode
	interval time.Duration
	stopChan chan struct{}
	now      func() time.Time

	mu         sync.Mutex
	heldLogged map[string]time.Time
}

// NewCreditEnforcer creates the enforcer. settler may be nil (the tail is
// then billed by the next scheduled collection).
func NewCreditEnforcer(db *sql.DB, billing *Service, stopper ComputeStopper, settler tailSettler, mode EnforcementMode) *CreditEnforcer {
	return &CreditEnforcer{
		db:         db,
		billing:    billing,
		stopper:    stopper,
		settler:    settler,
		mode:       mode,
		interval:   DefaultEnforcementInterval,
		stopChan:   make(chan struct{}),
		now:        time.Now,
		heldLogged: make(map[string]time.Time),
	}
}

// Start runs the enforcement loop until Stop or ctx is cancelled.
func (e *CreditEnforcer) Start(ctx context.Context) {
	if e.mode == EnforceOff {
		log.Println("Credit enforcer disabled (TEEPIN_CREDIT_ENFORCEMENT=off)")
		return
	}
	log.Printf("Starting credit enforcer (mode=%s)...", e.mode)

	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()

	e.Tick(ctx)
	for {
		select {
		case <-ticker.C:
			e.Tick(ctx)
		case <-e.stopChan:
			log.Println("Stopping credit enforcer...")
			return
		case <-ctx.Done():
			log.Println("Credit enforcer stopped (context cancelled)")
			return
		}
	}
}

// Stop stops the loop.
func (e *CreditEnforcer) Stop() {
	close(e.stopChan)
}

// accountAssessment is the enforcer's view of one account at one moment.
type accountAssessment struct {
	// Burn is the account's current spend in credit per hour.
	Burn float64
	// Accrued is what has been consumed since each instance was last
	// metered but not yet drawn from the balance.
	Accrued float64
	// Remaining is the balance after Accrued: the credit that is really left.
	Remaining float64
	// Paid are the instances that cost something; free ones (rate 0) never
	// count toward running out and are never stopped.
	Paid []billableInstance
}

// ExhaustedWithin reports whether the remaining credit runs out inside d.
func (a accountAssessment) ExhaustedWithin(d time.Duration) bool {
	if a.Burn <= 0 {
		return false
	}
	return a.Remaining <= a.Burn*d.Hours()
}

// assess projects an account's credit. balance is the ledger balance;
// billedThrough maps instance ID to the end of its last metered interval.
func assess(balance float64, insts []billableInstance, billedThrough map[string]time.Time, rates computeRates, now time.Time) accountAssessment {
	a := accountAssessment{}
	for _, inst := range insts {
		hourly := rates.hourly(inst)
		if hourly <= 0 {
			continue
		}
		a.Paid = append(a.Paid, inst)
		a.Burn += hourly
		if since := now.Sub(billedThrough[inst.ID]); since > 0 {
			a.Accrued += hourly * since.Hours()
		}
	}
	a.Remaining = balance - a.Accrued
	return a
}

// Tick evaluates every account that has running compute once.
func (e *CreditEnforcer) Tick(ctx context.Context) {
	running, billedThrough, err := e.runningInstances(ctx)
	if err != nil {
		log.Printf("WARN: credit enforcer could not list running instances: %v", err)
		return
	}
	if len(running) == 0 {
		return
	}

	byAccount := make(map[uuid.UUID][]billableInstance)
	for _, inst := range running {
		byAccount[inst.AccountID] = append(byAccount[inst.AccountID], inst)
	}

	rates := e.billing.loadComputeRates(ctx)
	now := e.now()

	for accountID, insts := range byAccount {
		if ctx.Err() != nil {
			return
		}
		balance, err := e.billing.CreditBalance(ctx, accountID)
		if err != nil {
			// Fail toward keeping customer workloads: an unreadable
			// balance is not evidence of an empty one. It is retried next
			// tick.
			log.Printf("WARN: credit enforcer could not read balance for account %s: %v", accountID, err)
			continue
		}
		a := assess(balance, insts, billedThrough, rates, now)
		if !a.ExhaustedWithin(enforcementHorizon) {
			continue
		}
		e.exhausted(ctx, accountID, a)
	}
}

// exhausted handles an account whose credit is about to run out.
func (e *CreditEnforcer) exhausted(ctx context.Context, accountID uuid.UUID, a accountAssessment) {
	// Instances with a persistent disk are held (stopped, disk kept, startable
	// after a top-up); the rest are simply ended. Deleting a disk-backed
	// instance would destroy the customer's data, which the agreed behavior
	// at zero credit is to keep for 7 days.
	var stoppable, holdable []string
	for _, inst := range a.Paid {
		if inst.StorageGB > 0 {
			holdable = append(holdable, inst.ID)
		} else {
			stoppable = append(stoppable, inst.ID)
		}
	}
	if len(stoppable) == 0 && len(holdable) == 0 {
		return
	}

	if e.mode == EnforceDryRun {
		log.Printf("Credit enforcer (dry-run): would stop %d and hold %d instance(s) of account %s (remaining credit %.4f, burn %.4f/hour)",
			len(stoppable), len(holdable), accountID, a.Remaining, a.Burn)
		return
	}

	var done int
	if len(stoppable) > 0 {
		stopped, err := e.stopper.StopInstances(ctx, accountID, stoppable)
		done += len(stopped)
		if err != nil {
			log.Printf("ERROR: credit enforcer failed to stop instances of account %s (stopped %d of %d): %v",
				accountID, len(stopped), len(stoppable), err)
		}
	}
	if len(holdable) > 0 {
		held, err := e.stopper.HoldInstances(ctx, accountID, holdable)
		done += len(held)
		if err != nil {
			e.reportHeld(accountID, holdable, held, a, err)
		}
	}
	if done > 0 {
		log.Printf("Credit enforcer: ended %d instance(s) of account %s because credit ran out (remaining %.4f, burn %.4f/hour)",
			done, accountID, a.Remaining, a.Burn)
		// Charge the final stretch now so the ledger reflects it and the
		// balance the customer sees is settled.
		if e.settler != nil {
			if err := e.settler.CollectAccount(ctx, accountID); err != nil {
				log.Printf("WARN: credit enforcer could not settle final usage for account %s: %v", accountID, err)
			}
		}
	}
}

// reportHeld logs, at most once an hour per instance, that a disk-backed
// instance could not be held and so keeps running unpaid. Loud on purpose:
// it is revenue leaking, and the usual cause (an agent that predates the
// stop command, or a missing launch spec) needs an operator.
func (e *CreditEnforcer) reportHeld(accountID uuid.UUID, attempted, held []string, a accountAssessment, cause error) {
	done := make(map[string]bool, len(held))
	for _, id := range held {
		done[id] = true
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	for _, id := range attempted {
		if done[id] {
			continue
		}
		if last, ok := e.heldLogged[id]; ok && now.Sub(last) < heldLogInterval {
			continue
		}
		e.heldLogged[id] = now
		log.Printf("ERROR: account %s is out of credit (remaining %.4f) but instance %s has a persistent disk and could not be stopped with the disk kept; it keeps running unpaid: %v",
			accountID, a.Remaining, id, cause)
	}
}

// runningInstances lists every running instance with the end of its last
// metered interval.
func (e *CreditEnforcer) runningInstances(ctx context.Context) ([]billableInstance, map[string]time.Time, error) {
	rows, err := e.db.QueryContext(ctx, `
		SELECT i.id, i.account_id, i.project_id, COALESCE(i.instance_type_id, ''),
		       COALESCE(i.gpu_vram_gb, 0), COALESCE(i.cpu_units, 0),
		       COALESCE(i.memory_gb, 0), COALESCE(i.storage_gb, 0),
		       i.p_cores_used, i.e_cores_used,
		       i.created_at, i.terminated_at,
		       GREATEST(COALESCE(b.last_end, i.created_at), COALESCE(i.resumed_at, i.created_at))
		FROM compute.instances i
		LEFT JOIN LATERAL (
			SELECT MAX(end_time) AS last_end
			FROM billing.usage_records ur
			WHERE ur.instance_id = i.id
		) b ON true
		WHERE i.status = 'running' AND i.terminated_at IS NULL`)
	if err != nil {
		return nil, nil, fmt.Errorf("query failed: %w", err)
	}
	defer rows.Close()

	var out []billableInstance
	billedThrough := make(map[string]time.Time)
	for rows.Next() {
		var inst billableInstance
		var through time.Time
		if err := rows.Scan(&inst.ID, &inst.AccountID, &inst.ProjectID, &inst.InstanceType,
			&inst.GPUVRAMGB, &inst.CPUUnits, &inst.MemoryGB, &inst.StorageGB,
			&inst.PCoresUsed, &inst.ECoresUsed,
			&inst.CreatedAt, &inst.TerminatedAt, &through); err != nil {
			return nil, nil, fmt.Errorf("scan failed: %w", err)
		}
		out = append(out, inst)
		billedThrough[inst.ID] = through
	}
	return out, billedThrough, rows.Err()
}
