// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package models

import "time"

// Instance represents a compute instance
type Instance struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Image  string `json:"image"`
	Status string `json:"status"`
	// StatusMessage explains a non-running status in terms the customer
	// can act on — "manifest unknown" for a bad image tag, the crash
	// reason for a container that will not start. Without it a failed
	// instance is just the word "failed", and the customer opens a
	// support ticket to learn what the cluster already knew.
	StatusMessage  string            `json:"status_message,omitempty"`
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
	Name         string `json:"name" binding:"required"`
	Image        string `json:"image" binding:"required"`
	GPUVRAM      string `json:"gpu_vram,omitempty"`      // e.g., "25GB"
	InstanceType string `json:"instance_type,omitempty"` // Legacy: "gpu.h100.mig-2g"
	CPUUnits     int    `json:"cpu_units" binding:"required,min=1"`
	Memory       string `json:"memory" binding:"required"` // e.g., "32GB"
	// NodeClass opts a workload onto a specific capacity class. Empty (the
	// default) is the datacenter/GPU path — unchanged. "home" places the
	// workload on a consumer-grade CPU node. A workload NEVER lands on a
	// home node unless it explicitly asks: the gate is opt-in.
	NodeClass string `json:"node_class,omitempty"`
	// Arch constrains placement to a CPU architecture ("amd64"/"arm64") for
	// a home workload — an amd64 image cannot run on an arm64 node. Empty
	// means no preference (a single-arch pilot).
	Arch string `json:"arch,omitempty"`
	// Command and Args override the image's ENTRYPOINT and CMD. Without
	// them an image whose default command exits (most base images,
	// including nvidia/cuda:*-base) crash-loops forever with no way for
	// the customer to keep it alive.
	Command []string          `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Ports   []PortMapping     `json:"ports,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
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
