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
}

// loadComputeRates reads the current rates. Never cached across calls:
// an admin price change applies from the next read.
func (s *Service) loadComputeRates(ctx context.Context) computeRates {
	return computeRates{
		vramPerGBHour:  s.VRAMPricePerGBHour(ctx),
		cpuCoreHour:    s.CPUCoreRate(ctx),
		memoryGBHour:   s.MemoryGBRate(ctx),
		storageGBMonth: s.StorageGBMonthRate(ctx),
	}
}

// unitPrice is the per-hour price of the instance's compute, excluding
// storage (which is priced per GB-month and added separately in cost).
//
// A GPU instance is linear on allocated VRAM. A CPU-only instance (home
// compute) is linear on cores + memory; its rates default to 0, so it costs
// nothing until an operator sets a price.
func (r computeRates) unitPrice(inst billableInstance) float64 {
	if inst.GPUVRAMGB > 0 {
		return float64(inst.GPUVRAMGB) * r.vramPerGBHour
	}
	return float64(inst.CPUUnits)*r.cpuCoreHour + float64(inst.MemoryGB)*r.memoryGBHour
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

// ComputeHourlyPrice is what an instance of this size costs per hour right now,
// storage included: the very formula the usage collector charges and the credit
// enforcer projects, so a price shown to a customer cannot differ from the bill.
// Zero while the relevant rates have not been set (every price starts at $0).
// Read from the live rates on every call, like every other quote.
func (s *Service) ComputeHourlyPrice(ctx context.Context, gpuVRAMGB, cpuUnits, memoryGB, storageGB int) float64 {
	return s.ComputeHourlyPricer(ctx)(gpuVRAMGB, cpuUnits, memoryGB, storageGB)
}

// ComputeHourlyPricer reads the live rates ONCE and returns a function that
// prices any number of instances from them, for a caller pricing a whole list
// (one set of rate reads, not one per row). Same formula as ComputeHourlyPrice.
func (s *Service) ComputeHourlyPricer(ctx context.Context) func(gpuVRAMGB, cpuUnits, memoryGB, storageGB int) float64 {
	rates := s.loadComputeRates(ctx)
	return func(gpuVRAMGB, cpuUnits, memoryGB, storageGB int) float64 {
		return rates.hourly(billableInstance{
			GPUVRAMGB: gpuVRAMGB, CPUUnits: cpuUnits, MemoryGB: memoryGB, StorageGB: storageGB,
		})
	}
}
