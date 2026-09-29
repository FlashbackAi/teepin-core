// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import "context"

// computeRates are the platform's per-unit compute prices at one moment. It
// is the single definition of "what does an instance cost", used both to
// charge for elapsed time (UsageCollector) and to project the cost of time
// not yet charged (CreditEnforcer), so the two cannot disagree about a
// price.
type computeRates struct {
	vramPerGBHour  float64
	cpuCoreHour    float64
	memoryGBHour   float64
	storageGBMonth float64
	pCoreHour      float64
	eCoreHour      float64
}

// loadComputeRates reads the current rates. Never cached across calls:
// an admin price change applies from the next read.
func (s *Service) loadComputeRates(ctx context.Context) computeRates {
	return computeRates{
		vramPerGBHour:  s.VRAMPricePerGBHour(ctx),
		cpuCoreHour:    s.CPUCoreRate(ctx),
		memoryGBHour:   s.MemoryGBRate(ctx),
		storageGBMonth: s.StorageGBMonthRate(ctx),
		pCoreHour:      s.PCoreRate(ctx),
		eCoreHour:      s.ECoreRate(ctx),
	}
}

// unitPrice is the per-hour price of the instance's compute, excluding
// storage (which is priced per GB-month and added separately in cost).
//
// A GPU instance is linear on allocated VRAM. A CPU-only instance (home
// compute) is linear on cores + memory; its rates default to 0, so it costs
// nothing until an operator sets a price. A CPU instance placed with a
// DETECTED P/E split (PCoresUsed/ECoresUsed both non-nil — see
// billableInstance) prices via the separate P-core/E-core rates instead of
// the single undifferentiated CPU rate; an instance with no detected split
// is billed exactly as before that feature.
func (r computeRates) unitPrice(inst billableInstance) float64 {
	switch {
	case inst.GPUVRAMGB > 0:
		return float64(inst.GPUVRAMGB) * r.vramPerGBHour
	case inst.PCoresUsed != nil && inst.ECoresUsed != nil:
		return float64(*inst.PCoresUsed)*r.pCoreHour + float64(*inst.ECoresUsed)*r.eCoreHour + float64(inst.MemoryGB)*r.memoryGBHour
	default:
		return float64(inst.CPUUnits)*r.cpuCoreHour + float64(inst.MemoryGB)*r.memoryGBHour
	}
}

// storagePerHour is the storage component of an instance's hourly cost.
// Storage is priced per GB-MONTH; hoursPerMonth (730 = 365*24/12, the
// standard average used for this conversion) is a deliberate constant
// rather than a calendar-specific calculation, which would drift the same
// instance's hourly rate depending on which actual month elapsed.
func (r computeRates) storagePerHour(inst billableInstance) float64 {
	if inst.StorageGB > 0 && r.storageGBMonth > 0 {
		return float64(inst.StorageGB) * r.storageGBMonth / hoursPerMonth
	}
	return 0
}

// hourly is the total cost per hour of running the instance.
func (r computeRates) hourly(inst billableInstance) float64 {
	return r.unitPrice(inst) + r.storagePerHour(inst)
}

// cost prices hours of running the instance. unitPrice is the per-hour
// compute rate (recorded on the usage record for transparency); total also
// includes storage.
func (r computeRates) cost(inst billableInstance, hours float64) (unitPrice, total float64) {
	unitPrice = r.unitPrice(inst)
	return unitPrice, unitPrice*hours + r.storagePerHour(inst)*hours
}
