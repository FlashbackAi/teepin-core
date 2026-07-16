// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package ratelimit

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config holds the complete rate limiting configuration
type Config struct {
	// Redis connection
	RedisURL      string `yaml:"redis_url" json:"redis_url"`
	RedisPassword string `yaml:"redis_password" json:"redis_password"`

	// Global defaults
	DefaultRPS   int64 `yaml:"default_rps" json:"default_rps"`
	DefaultBurst int64 `yaml:"default_burst" json:"default_burst"`

	// Tier-based limits
	Tiers map[string]*TierConfig `yaml:"tiers" json:"tiers"`

	// Per-endpoint overrides
	Endpoints map[string]*EndpointConfig `yaml:"endpoints" json:"endpoints"`

	// Feature flags
	Enabled       bool `yaml:"enabled" json:"enabled"`
	DryRunMode    bool `yaml:"dry_run_mode" json:"dry_run_mode"`     // Log violations but don't block
	ReturnHeaders bool `yaml:"return_headers" json:"return_headers"` // X-RateLimit-* headers
}

// TierConfig defines rate limits for a customer tier
type TierConfig struct {
	Name              string `yaml:"name" json:"name"`
	RequestsPerSecond int64  `yaml:"requests_per_second" json:"requests_per_second"`
	RequestsPerMinute int64  `yaml:"requests_per_minute" json:"requests_per_minute"`
	RequestsPerHour   int64  `yaml:"requests_per_hour" json:"requests_per_hour"`
	Burst             int64  `yaml:"burst" json:"burst"`
}

// EndpointConfig defines rate limits for specific API endpoints
type EndpointConfig struct {
	Rate     int64         `yaml:"rate" json:"rate"`         // requests per period
	Period   time.Duration `yaml:"period" json:"period"`     // time window
	Burst    int64         `yaml:"burst" json:"burst"`       // allowed burst
	Disabled bool          `yaml:"disabled" json:"disabled"` // disable rate limiting for this endpoint
}

// DefaultConfig returns the default rate limiting configuration
// Follows industry standards from AWS, GCP, Stripe research
func DefaultConfig() *Config {
	// Default to localhost for local development
	// Override with REDIS_URL environment variable for production
	redisURL := "redis://localhost:6379"

	return &Config{
		RedisURL:      redisURL,
		RedisPassword: "",
		DefaultRPS:    50,
		DefaultBurst:  100,
		Enabled:       true,
		DryRunMode:    false,
		ReturnHeaders: true,

		Tiers: map[string]*TierConfig{
			// Free tier - conservative limits to prevent abuse
			"free": {
				Name:              "Free",
				RequestsPerSecond: 10,
				RequestsPerMinute: 100,
				RequestsPerHour:   1000,
				Burst:             20,
			},

			// Authenticated tier - default for paying customers
			"authenticated": {
				Name:              "Authenticated",
				RequestsPerSecond: 50,
				RequestsPerMinute: 1000,
				RequestsPerHour:   10000,
				Burst:             100,
			},

			// Power users - approved via support ticket
			"power": {
				Name:              "Power User",
				RequestsPerSecond: 100,
				RequestsPerMinute: 2000,
				RequestsPerHour:   20000,
				Burst:             200,
			},

			// Enterprise - custom negotiated limits
			"enterprise": {
				Name:              "Enterprise",
				RequestsPerSecond: 200,
				RequestsPerMinute: 5000,
				RequestsPerHour:   50000,
				Burst:             500,
			},
		},

		Endpoints: map[string]*EndpointConfig{
			// Expensive operations - strict limits
			"POST /v1/compute/instances": {
				Rate:   10,
				Period: time.Minute,
				Burst:  5,
			},

			// Security-critical - prevent brute force
			"POST /v1/auth/login": {
				Rate:   5,
				Period: time.Minute,
				Burst:  3,
			},
			"POST /v1/auth/register": {
				Rate:   3,
				Period: time.Hour,
				Burst:  5,
			},

			// Moderate cost operations
			"DELETE /v1/compute/instances/:id": {
				Rate:   20,
				Period: time.Minute,
				Burst:  10,
			},
			"POST /v1/projects/:id/registry": {
				Rate:   5,
				Period: time.Minute,
				Burst:  3,
			},

			// Read operations - more permissive
			"GET /v1/compute/instances": {
				Rate:   60,
				Period: time.Minute,
				Burst:  20,
			},
			"GET /v1/compute/instances/:id": {
				Rate:   100,
				Period: time.Minute,
				Burst:  30,
			},
			"GET /v1/billing/usage": {
				Rate:   30,
				Period: time.Minute,
				Burst:  10,
			},

			// Health checks - no limit (monitoring must always work)
			"GET /health": {
				Disabled: true,
			},
			"GET /version": {
				Disabled: true,
			},
		},
	}
}

// LoadConfig loads rate limit configuration from YAML file
func LoadConfig(path string) (*Config, error) {
	// Start with defaults
	config := DefaultConfig()

	// If no config file specified, use defaults
	if path == "" {
		return config, nil
	}

	// Read file
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	// Parse YAML
	if err := yaml.Unmarshal(data, config); err != nil {
		return nil, fmt.Errorf("failed to parse config YAML: %w", err)
	}

	// Validate configuration
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return config, nil
}

// Validate checks if the configuration is valid
func (c *Config) Validate() error {
	// Check Redis URL
	if c.Enabled && c.RedisURL == "" {
		return fmt.Errorf("redis_url is required when rate limiting is enabled")
	}

	// Validate tiers
	for name, tier := range c.Tiers {
		if tier.RequestsPerSecond <= 0 {
			return fmt.Errorf("tier %s: requests_per_second must be > 0", name)
		}
		if tier.Burst <= 0 {
			return fmt.Errorf("tier %s: burst must be > 0", name)
		}
		// Ensure burst is not less than RPS (token bucket requirement)
		if tier.Burst < tier.RequestsPerSecond {
			return fmt.Errorf("tier %s: burst (%d) should be >= requests_per_second (%d)",
				name, tier.Burst, tier.RequestsPerSecond)
		}
	}

	// Validate endpoints
	for endpoint, config := range c.Endpoints {
		if config.Disabled {
			continue
		}
		if config.Rate <= 0 {
			return fmt.Errorf("endpoint %s: rate must be > 0", endpoint)
		}
		if config.Period <= 0 {
			return fmt.Errorf("endpoint %s: period must be > 0", endpoint)
		}
		if config.Burst <= 0 {
			return fmt.Errorf("endpoint %s: burst must be > 0", endpoint)
		}
	}

	return nil
}

// GetTierConfig returns the configuration for a specific tier
func (c *Config) GetTierConfig(tier string) *TierConfig {
	if t, ok := c.Tiers[tier]; ok {
		return t
	}
	// Default to authenticated tier if tier not found
	return c.Tiers["authenticated"]
}

// GetEndpointConfig returns the configuration for a specific endpoint
func (c *Config) GetEndpointConfig(endpoint string) *EndpointConfig {
	if ep, ok := c.Endpoints[endpoint]; ok {
		return ep
	}
	return nil
}

// IsEndpointRateLimited checks if rate limiting is enabled for an endpoint
func (c *Config) IsEndpointRateLimited(endpoint string) bool {
	if !c.Enabled {
		return false
	}

	epConfig := c.GetEndpointConfig(endpoint)
	if epConfig != nil && epConfig.Disabled {
		return false
	}

	return true
}
