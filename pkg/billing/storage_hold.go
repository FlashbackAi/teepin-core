// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
)

// DefaultStorageHoldPeriod is how long an out-of-credit account's stored
// data is kept before deletion. Decided 2026-09-29: 7 days, unbilled, and
// completely inaccessible while held.
const DefaultStorageHoldPeriod = 7 * 24 * time.Hour

// DefaultStorageHoldInterval is how often holds are started, released and
// purged.
const DefaultStorageHoldInterval = 1 * time.Minute

// StorageHold is one account's countdown to storage deletion.
type StorageHold struct {
	ID          uuid.UUID
	AccountID   uuid.UUID
	HeldSince   time.Time
	DeleteAfter time.Time
}

// StartStorageHold begins a hold for the account unless one is already
// active. Reports whether a new hold was created.
func (s *Service) StartStorageHold(ctx context.Context, accountID uuid.UUID, period time.Duration) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO billing.storage_holds (account_id, delete_after)
		VALUES ($1, NOW() + make_interval(secs => $2))
		ON CONFLICT (account_id) WHERE released_at IS NULL AND purged_at IS NULL DO NOTHING
	`, accountID, period.Seconds())
	if err != nil {
		return false, fmt.Errorf("failed to start storage hold: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ReleaseStorageHold ends the account's active hold (credit was added, or
// nothing is left to hold). Reports whether a hold was released.
func (s *Service) ReleaseStorageHold(ctx context.Context, accountID uuid.UUID) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE billing.storage_holds SET released_at = NOW()
		WHERE account_id = $1 AND released_at IS NULL AND purged_at IS NULL
	`, accountID)
	if err != nil {
		return false, fmt.Errorf("failed to release storage hold: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ActiveStorageHold returns the account's active hold, or nil.
func (s *Service) ActiveStorageHold(ctx context.Context, accountID uuid.UUID) (*StorageHold, error) {
	var h StorageHold
	err := s.db.QueryRowContext(ctx, `
		SELECT id, account_id, held_since, delete_after FROM billing.storage_holds
		WHERE account_id = $1 AND released_at IS NULL AND purged_at IS NULL
	`, accountID).Scan(&h.ID, &h.AccountID, &h.HeldSince, &h.DeleteAfter)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read storage hold: %w", err)
	}
	return &h, nil
}

// ActiveStorageHolds lists every active hold, for the sweeper.
func (s *Service) ActiveStorageHolds(ctx context.Context) ([]StorageHold, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, account_id, held_since, delete_after FROM billing.storage_holds
		WHERE released_at IS NULL AND purged_at IS NULL
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to list storage holds: %w", err)
	}
	defer rows.Close()
	var out []StorageHold
	for rows.Next() {
		var h StorageHold
		if err := rows.Scan(&h.ID, &h.AccountID, &h.HeldSince, &h.DeleteAfter); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// MarkStorageHoldPurged records that the held data was deleted.
func (s *Service) MarkStorageHoldPurged(ctx context.Context, holdID uuid.UUID) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE billing.storage_holds SET purged_at = NOW(), last_error = NULL WHERE id = $1
	`, holdID)
	return err
}

// RecordStorageHoldError keeps the latest purge failure on the hold so an
// operator can see why data is overdue for deletion.
func (s *Service) RecordStorageHoldError(ctx context.Context, holdID uuid.UUID, msg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE billing.storage_holds SET last_error = $2 WHERE id = $1`, holdID, msg)
	return err
}

// StorageBillingFloor tells a storage meter what it may charge for. held is
// true while a hold is active: nothing may be billed. floor is the moment
// the most recent hold ended; usage before it must not be billed, or
// releasing a hold would back-charge the whole held period.
func (s *Service) StorageBillingFloor(ctx context.Context, accountID uuid.UUID) (held bool, floor time.Time, err error) {
	var active bool
	var released sql.NullTime
	err = s.db.QueryRowContext(ctx, `
		SELECT COALESCE(BOOL_OR(released_at IS NULL AND purged_at IS NULL), FALSE),
		       MAX(released_at)
		FROM billing.storage_holds WHERE account_id = $1
	`, accountID).Scan(&active, &released)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("failed to read storage billing floor: %w", err)
	}
	return active, released.Time, nil
}

// HeldStorage is one kind of stored data that is held at zero credit
// (object storage, and later instance disks).
type HeldStorage interface {
	// Name identifies the kind in logs.
	Name() string
	// AccountsWithStorage lists accounts that currently hold any.
	AccountsWithStorage(ctx context.Context) ([]uuid.UUID, error)
	// PurgeAccount permanently deletes the account's stored data of this kind.
	PurgeAccount(ctx context.Context, accountID uuid.UUID) error
}

// StorageHoldSweeper runs the hold lifecycle: an account with stored data
// and no credit gets a hold; a hold ends when credit returns; when a hold
// runs out the data is deleted.
//
// Access while held is enforced elsewhere, by the balance itself (see the
// storage handlers) - not by this sweeper - so adding credit restores
// access at once rather than at the next sweep.
type StorageHoldSweeper struct {
	billing  *Service
	stores   []HeldStorage
	mode     EnforcementMode
	period   time.Duration
	interval time.Duration
	stopChan chan struct{}
}

// NewStorageHoldSweeper builds the sweeper. period <= 0 uses the default.
func NewStorageHoldSweeper(billing *Service, mode EnforcementMode, period time.Duration, stores ...HeldStorage) *StorageHoldSweeper {
	if period <= 0 {
		period = DefaultStorageHoldPeriod
	}
	return &StorageHoldSweeper{
		billing: billing, stores: stores, mode: mode, period: period,
		interval: DefaultStorageHoldInterval, stopChan: make(chan struct{}),
	}
}

// Start runs the sweeper until Stop or ctx is cancelled.
func (w *StorageHoldSweeper) Start(ctx context.Context) {
	if w.mode == EnforceOff {
		log.Println("Storage hold sweeper disabled (TEEPIN_STORAGE_HOLD=off)")
		return
	}
	log.Printf("Starting storage hold sweeper (mode=%s, hold=%s)...", w.mode, w.period)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	w.Sweep(ctx)
	for {
		select {
		case <-ticker.C:
			w.Sweep(ctx)
		case <-w.stopChan:
			return
		case <-ctx.Done():
			return
		}
	}
}

// Stop stops the sweeper.
func (w *StorageHoldSweeper) Stop() { close(w.stopChan) }

// Sweep does one pass.
func (w *StorageHoldSweeper) Sweep(ctx context.Context) {
	// Accounts that have anything stored.
	stored := make(map[uuid.UUID]bool)
	for _, st := range w.stores {
		ids, err := st.AccountsWithStorage(ctx)
		if err != nil {
			// Without a complete picture, holds could be released or
			// started wrongly. Skip the whole pass.
			log.Printf("WARN: storage hold sweeper: cannot list %s accounts: %v", st.Name(), err)
			return
		}
		for _, id := range ids {
			stored[id] = true
		}
	}

	holds, err := w.billing.ActiveStorageHolds(ctx)
	if err != nil {
		log.Printf("WARN: storage hold sweeper: %v", err)
		return
	}
	held := make(map[uuid.UUID]StorageHold, len(holds))
	for _, h := range holds {
		held[h.AccountID] = h
	}

	// Start holds for accounts with data and no credit.
	for id := range stored {
		if _, ok := held[id]; ok {
			continue
		}
		bal, err := w.billing.CreditBalance(ctx, id)
		if err != nil {
			log.Printf("WARN: storage hold sweeper: balance for %s: %v", id, err)
			continue
		}
		if bal > 0 {
			continue
		}
		if w.mode == EnforceDryRun {
			log.Printf("Storage hold (dry-run): would hold stored data of account %s for %s", id, w.period)
			continue
		}
		if started, err := w.billing.StartStorageHold(ctx, id, w.period); err != nil {
			log.Printf("WARN: storage hold sweeper: start hold for %s: %v", id, err)
		} else if started {
			log.Printf("Storage hold: account %s is out of credit; stored data is inaccessible and will be deleted in %s unless credit is added", id, w.period)
		}
	}

	// Release or purge active holds.
	now := time.Now()
	for _, h := range holds {
		bal, err := w.billing.CreditBalance(ctx, h.AccountID)
		if err != nil {
			log.Printf("WARN: storage hold sweeper: balance for %s: %v", h.AccountID, err)
			continue
		}
		switch {
		case bal > 0 || !stored[h.AccountID]:
			// Credit came back, or the customer deleted everything.
			if w.mode == EnforceDryRun {
				continue
			}
			if ok, err := w.billing.ReleaseStorageHold(ctx, h.AccountID); err != nil {
				log.Printf("WARN: storage hold sweeper: release %s: %v", h.AccountID, err)
			} else if ok {
				log.Printf("Storage hold: released for account %s", h.AccountID)
			}
		case !now.Before(h.DeleteAfter):
			w.purge(ctx, h)
		}
	}
}

func (w *StorageHoldSweeper) purge(ctx context.Context, h StorageHold) {
	if w.mode == EnforceDryRun {
		log.Printf("Storage hold (dry-run): would DELETE all stored data of account %s (hold expired %s)", h.AccountID, h.DeleteAfter.Format(time.RFC3339))
		return
	}
	// Last look at the balance immediately before the irreversible step: a
	// top-up that landed since the pass began must win.
	if bal, err := w.billing.CreditBalance(ctx, h.AccountID); err != nil || bal > 0 {
		return
	}
	var errs []error
	for _, st := range w.stores {
		if err := st.PurgeAccount(ctx, h.AccountID); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", st.Name(), err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		log.Printf("ERROR: storage hold: could not delete stored data of account %s (will retry): %v", h.AccountID, err)
		_ = w.billing.RecordStorageHoldError(ctx, h.ID, err.Error())
		return
	}
	if err := w.billing.MarkStorageHoldPurged(ctx, h.ID); err != nil {
		log.Printf("ERROR: storage hold: deleted data of account %s but could not record it: %v", h.AccountID, err)
		return
	}
	log.Printf("Storage hold: deleted stored data of account %s after the hold expired", h.AccountID)
}
