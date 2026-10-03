// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
)

// AlertLevel is how close an account is to running out of credit. Higher is
// worse; the warning ladder only ever moves up until credit is added.
type AlertLevel int

const (
	LevelNone AlertLevel = iota
	// LevelLow: about three days of credit left, or $20, whichever is first.
	LevelLow
	// LevelDay: about 24 hours left.
	LevelDay
	// LevelSixHours: about 6 hours left.
	LevelSixHours
	// LevelOutOfCredit: no credit. Compute is stopped or about to be, and
	// stored data is held.
	LevelOutOfCredit
)

func (l AlertLevel) String() string {
	switch l {
	case LevelLow:
		return "low"
	case LevelDay:
		return "day"
	case LevelSixHours:
		return "six_hours"
	case LevelOutOfCredit:
		return "out_of_credit"
	default:
		return "none"
	}
}

const (
	// lowBalanceFloor is the balance at or under which an account that is
	// spending gets the first warning, even with a long runway.
	lowBalanceFloor = 20.0

	runwayLow      = 72 * time.Hour
	runwayDay      = 24 * time.Hour
	runwaySixHours = 6 * time.Hour
)

// RunwayReport is an account's credit position: what is left, how fast it is
// being spent, and how long that lasts.
type RunwayReport struct {
	Balance float64
	// BurnPerHour is the current spend in credit per hour: running compute,
	// stopped disks and object storage at their present rates, or the last
	// 24 hours' actual consumption if that is higher (which is what counts
	// metered use such as inference, whose future volume cannot be known).
	BurnPerHour float64
	// Impacted is true when the account already has stopped instances or
	// stored data on hold - things a top-up would bring back.
	Impacted bool
	Level    AlertLevel
	// AutoRecharge is true when the customer's automatic recharge is on and
	// healthy, so running low is handled and Level below out-of-credit is
	// reported as none.
	AutoRecharge bool
}

// RunwayHours is how long the balance lasts at the current spend, and false
// when nothing is being spent.
func (r RunwayReport) RunwayHours() (float64, bool) {
	if r.BurnPerHour <= 0 {
		return 0, false
	}
	return math.Max(r.Balance, 0) / r.BurnPerHour, true
}

// levelFor decides the warning level. An account that is not spending has no
// runway to run out of, so it is never warned - unless it already has things
// on hold, in which case being at zero is the news.
func levelFor(balance, burn float64, impacted bool) AlertLevel {
	if balance <= 0 {
		if burn > 0 || impacted {
			return LevelOutOfCredit
		}
		return LevelNone
	}
	if burn <= 0 {
		return LevelNone
	}
	hours := balance / burn
	switch {
	case hours <= runwaySixHours.Hours():
		return LevelSixHours
	case hours <= runwayDay.Hours():
		return LevelDay
	case hours <= runwayLow.Hours() || balance <= lowBalanceFloor:
		return LevelLow
	default:
		return LevelNone
	}
}

// Runway reports the account's credit position.
func (s *Service) Runway(ctx context.Context, accountID uuid.UUID) (RunwayReport, error) {
	balance, err := s.CreditBalance(ctx, accountID)
	if err != nil {
		return RunwayReport{}, err
	}
	rates := s.loadComputeRates(ctx)

	compute, err := s.runningComputeBurn(ctx, accountID, rates)
	if err != nil {
		return RunwayReport{}, err
	}
	stoppedGB, stoppedCount, err := s.stoppedDiskTotals(ctx, accountID)
	if err != nil {
		return RunwayReport{}, err
	}
	bucketBytes, err := s.bucketBytes(ctx, accountID)
	if err != nil {
		return RunwayReport{}, err
	}
	trailing, err := s.trailingConsumption(ctx, accountID, 24*time.Hour)
	if err != nil {
		return RunwayReport{}, err
	}

	storage := float64(stoppedGB) * rates.storageGBMonth / hoursPerMonth
	objects := float64(bucketBytes) / (1 << 30) * s.ObjectStorageGBMonthRate(ctx) / gbMonthHoursForRunway
	known := compute + storage + objects
	burn := math.Max(known, trailing/24)

	hold, err := s.ActiveStorageHold(ctx, accountID)
	if err != nil {
		return RunwayReport{}, err
	}
	impacted := stoppedCount > 0 || hold != nil

	level := levelFor(balance, burn, impacted)
	// With a healthy automatic recharge the account is about to be topped up,
	// so "running low" warnings would only alarm the customer. Out-of-credit
	// still shows: it means the recharge did not save the account.
	covered, err := s.autoRechargeCovers(ctx, accountID)
	if err != nil {
		return RunwayReport{}, err
	}
	if covered && level < LevelOutOfCredit {
		level = LevelNone
	}

	return RunwayReport{
		Balance:      balance,
		BurnPerHour:  burn,
		Impacted:     impacted,
		Level:        level,
		AutoRecharge: covered,
	}, nil
}

// gbMonthHoursForRunway is the GB-month to GB-hour conversion the object
// storage meter uses (730).
const gbMonthHoursForRunway = 730.0

// runningComputeBurn is the hourly cost of the account's running instances
// at present rates.
func (s *Service) runningComputeBurn(ctx context.Context, accountID uuid.UUID, rates computeRates) (float64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT COALESCE(gpu_vram_gb, 0), COALESCE(cpu_units, 0), COALESCE(memory_gb, 0),
		       COALESCE(storage_gb, 0)
		FROM compute.instances
		WHERE account_id = $1 AND status = 'running' AND terminated_at IS NULL`, accountID)
	if err != nil {
		return 0, fmt.Errorf("failed to read running instances: %w", err)
	}
	defer rows.Close()
	var total float64
	for rows.Next() {
		var inst billableInstance
		if err := rows.Scan(&inst.GPUVRAMGB, &inst.CPUUnits, &inst.MemoryGB, &inst.StorageGB); err != nil {
			return 0, err
		}
		total += rates.hourly(inst)
	}
	return total, rows.Err()
}

// stoppedDiskTotals returns the total GB and count of the account's stopped
// instances.
func (s *Service) stoppedDiskTotals(ctx context.Context, accountID uuid.UUID) (gb int, count int, err error) {
	err = s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(COALESCE(storage_gb, 0)), 0), COUNT(*)
		FROM compute.instances
		WHERE account_id = $1 AND status = 'stopped' AND terminated_at IS NULL`, accountID).Scan(&gb, &count)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to read stopped instances: %w", err)
	}
	return gb, count, nil
}

// bucketBytes is the total size of the account's live object-storage buckets.
func (s *Service) bucketBytes(ctx context.Context, accountID uuid.UUID) (int64, error) {
	var n sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT SUM(total_bytes) FROM storage.buckets WHERE account_id = $1 AND deleted_at IS NULL`, accountID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("failed to read bucket sizes: %w", err)
	}
	return n.Int64, nil
}

// trailingConsumption is how much credit the account consumed over the window.
func (s *Service) trailingConsumption(ctx context.Context, accountID uuid.UUID, window time.Duration) (float64, error) {
	var v sql.NullFloat64
	err := s.db.QueryRowContext(ctx, `
		SELECT -SUM(amount) FROM billing.credit_transactions
		WHERE account_id = $1 AND kind = 'consumption' AND created_at > NOW() - make_interval(secs => $2)`,
		accountID, window.Seconds()).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("failed to read recent consumption: %w", err)
	}
	return math.Max(v.Float64, 0), nil
}
