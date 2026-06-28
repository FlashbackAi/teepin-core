// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/ulule/limiter/v3"
	limiterRedis "github.com/ulule/limiter/v3/drivers/store/redis"
)

// Limiter manages rate limiting for API requests
type Limiter struct {
	config      *Config
	redisClient *redis.Client
	store       limiter.Store

	// Cached limiters for each tier (to avoid recreating)
	tierLimiters map[string]*limiter.Limiter

	// Cached limiters for each endpoint
	endpointLimiters map[string]*limiter.Limiter
}

// NewLimiter creates a new rate limiter instance
func NewLimiter(config *Config) (*Limiter, error) {
	if !config.Enabled {
		return &Limiter{
			config: config,
		}, nil
	}

	// Parse Redis URL and create client
	opts, err := redis.ParseURL(config.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("invalid redis URL: %w", err)
	}

	// Add password if configured
	if config.RedisPassword != "" {
		opts.Password = config.RedisPassword
	}

	// Create Redis client
	redisClient := redis.NewClient(opts)

	// Test connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := redisClient.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	// Create limiter store
	store, err := limiterRedis.NewStoreWithOptions(redisClient, limiter.StoreOptions{
		Prefix:   "teepin:ratelimit",
		MaxRetry: 3,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create limiter store: %w", err)
	}

	l := &Limiter{
		config:           config,
		redisClient:      redisClient,
		store:            store,
		tierLimiters:     make(map[string]*limiter.Limiter),
		endpointLimiters: make(map[string]*limiter.Limiter),
	}

	// Pre-create limiters for each tier
	for tierName, tierConfig := range config.Tiers {
		rate := limiter.Rate{
			Period: time.Second,
			Limit:  tierConfig.RequestsPerSecond,
		}
		l.tierLimiters[tierName] = limiter.New(store, rate)
	}

	// Pre-create limiters for each endpoint
	for endpoint, epConfig := range config.Endpoints {
		if epConfig.Disabled {
			continue
		}
		rate := limiter.Rate{
			Period: epConfig.Period,
			Limit:  epConfig.Rate,
		}
		l.endpointLimiters[endpoint] = limiter.New(store, rate)
	}

	return l, nil
}

// CheckLimit checks if a request should be rate limited
// Returns the limiter context and whether the request is allowed
func (l *Limiter) CheckLimit(ctx context.Context, key, tier, endpoint string) (limiter.Context, error) {
	// If rate limiting is disabled, always allow
	if !l.config.Enabled {
		return limiter.Context{
			Limit:     -1,
			Remaining: -1,
			Reset:     0,
			Reached:   false,
		}, nil
	}

	// Check endpoint-specific limit first (more restrictive)
	if l.config.IsEndpointRateLimited(endpoint) {
		endpointLimiter := l.endpointLimiters[endpoint]
		if endpointLimiter != nil {
			// Use endpoint-specific key (e.g., "user123:POST /v1/compute/instances")
			endpointKey := fmt.Sprintf("%s:%s", key, endpoint)
			limiterCtx, err := endpointLimiter.Get(ctx, endpointKey)
			if err != nil {
				return limiter.Context{}, fmt.Errorf("failed to check endpoint rate limit: %w", err)
			}

			// If endpoint limit reached, deny immediately
			if limiterCtx.Reached {
				return limiterCtx, nil
			}
		}
	}

	// Check tier-based global limit
	tierLimiter := l.tierLimiters[tier]
	if tierLimiter == nil {
		// Fallback to authenticated tier
		tierLimiter = l.tierLimiters["authenticated"]
	}

	limiterCtx, err := tierLimiter.Get(ctx, key)
	if err != nil {
		return limiter.Context{}, fmt.Errorf("failed to check tier rate limit: %w", err)
	}

	return limiterCtx, nil
}

// Allow checks if a request is allowed under rate limits
func (l *Limiter) Allow(ctx context.Context, key, tier, endpoint string) (bool, limiter.Context, error) {
	limiterCtx, err := l.CheckLimit(ctx, key, tier, endpoint)
	if err != nil {
		// In case of error, fail open (allow request but log error)
		// This prevents Redis outage from taking down the API
		return true, limiter.Context{}, err
	}

	allowed := !limiterCtx.Reached

	// Dry run mode: log violations but don't block
	if l.config.DryRunMode && limiterCtx.Reached {
		// In production, this would log to monitoring system
		fmt.Printf("[RATE_LIMIT_DRY_RUN] Would block: key=%s tier=%s endpoint=%s limit=%d\n",
			key, tier, endpoint, limiterCtx.Limit)
		return true, limiterCtx, nil
	}

	return allowed, limiterCtx, nil
}

// Reset resets the rate limit for a specific key (admin function)
func (l *Limiter) Reset(ctx context.Context, key string) error {
	if !l.config.Enabled {
		return nil
	}

	// Reset all tier limiters for this key
	for _, tierLimiter := range l.tierLimiters {
		if _, err := tierLimiter.Reset(ctx, key); err != nil {
			return fmt.Errorf("failed to reset tier limiter: %w", err)
		}
	}

	// Reset all endpoint limiters for this key
	for endpoint, endpointLimiter := range l.endpointLimiters {
		endpointKey := fmt.Sprintf("%s:%s", key, endpoint)
		if _, err := endpointLimiter.Reset(ctx, endpointKey); err != nil {
			return fmt.Errorf("failed to reset endpoint limiter: %w", err)
		}
	}

	return nil
}

// GetCurrentUsage returns current usage stats for a key
func (l *Limiter) GetCurrentUsage(ctx context.Context, key, tier string) (*UsageStats, error) {
	if !l.config.Enabled {
		return &UsageStats{
			Tier:      tier,
			Unlimited: true,
		}, nil
	}

	tierLimiter := l.tierLimiters[tier]
	if tierLimiter == nil {
		tierLimiter = l.tierLimiters["authenticated"]
	}

	limiterCtx, err := tierLimiter.Peek(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("failed to get usage stats: %w", err)
	}

	return &UsageStats{
		Tier:      tier,
		Limit:     limiterCtx.Limit,
		Remaining: limiterCtx.Remaining,
		Reset:     time.Unix(limiterCtx.Reset, 0),
		Used:      limiterCtx.Limit - limiterCtx.Remaining,
		Unlimited: false,
	}, nil
}

// Close closes the Redis connection
func (l *Limiter) Close() error {
	if l.redisClient != nil {
		return l.redisClient.Close()
	}
	return nil
}

// UsageStats contains rate limit usage information
type UsageStats struct {
	Tier      string    `json:"tier"`
	Limit     int64     `json:"limit"`
	Remaining int64     `json:"remaining"`
	Reset     time.Time `json:"reset"`
	Used      int64     `json:"used"`
	Unlimited bool      `json:"unlimited"`
}
