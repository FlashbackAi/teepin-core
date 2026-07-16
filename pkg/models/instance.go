// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package models

import "time"

// Instance represents a compute instance
type Instance struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	Image          string            `json:"image"`
	Status         string            `json:"status"`
	InstanceType   string            `json:"instance_type,omitempty"` // derived from hardware, e.g. "gpu.h100.2g.20gb"
	PricePerHour   float64           `json:"price_per_hour,omitempty"`
	GPUVRAM        string            `json:"gpu_vram,omitempty"`        // what was requested
	AllocatedVRAM  string            `json:"allocated_vram,omitempty"`  // what was reserved and billed
	AllocationNote string            `json:"allocation_note,omitempty"` // set when allocated > requested
	CPUUnits       int               `json:"cpu_units"`
	Memory         string            `json:"memory"`
	Endpoint       string            `json:"endpoint,omitempty"`    // HTTPS endpoint URL
	PublicIP       string            `json:"public_ip,omitempty"`   // LoadBalancer IP
	DNSName        string            `json:"dns_name,omitempty"`    // DNS hostname
	TLSEnabled     bool              `json:"tls_enabled,omitempty"` // SSL/TLS configured
	TLSReady       bool              `json:"tls_ready,omitempty"`   // SSL certificate provisioned
	InternalIP     string            `json:"internal_ip,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
	Labels         map[string]string `json:"labels,omitempty"`
}

// CreateInstanceRequest represents a request to create an instance
type CreateInstanceRequest struct {
	Name         string            `json:"name" binding:"required"`
	Image        string            `json:"image" binding:"required"`
	GPUVRAM      string            `json:"gpu_vram,omitempty"`      // e.g., "25GB"
	InstanceType string            `json:"instance_type,omitempty"` // Legacy: "gpu.h100.mig-2g"
	CPUUnits     int               `json:"cpu_units" binding:"required,min=1"`
	Memory       string            `json:"memory" binding:"required"` // e.g., "32GB"
	Ports        []PortMapping     `json:"ports,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
}

// PortMapping defines port exposure
type PortMapping struct {
	Container int    `json:"container" binding:"required"`
	Public    int    `json:"public,omitempty"`
	Protocol  string `json:"protocol,omitempty"` // tcp, udp
}

// InstanceType represents available instance configurations
type InstanceType struct {
	Name         string  `json:"name"`
	GPUVRAM      string  `json:"gpu_vram,omitempty"`
	GPUMemoryGB  int     `json:"gpu_memory_gb,omitempty"`
	CPUUnits     int     `json:"cpu_units"`
	Memory       string  `json:"memory"`
	PricePerHour float64 `json:"price_per_hour"`
	Description  string  `json:"description"`
}
