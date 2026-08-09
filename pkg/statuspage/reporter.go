// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Package statuspage publishes platform metrics to an Atlassian
// Statuspage.
//
// It reports GPU capacity AVAILABLE, not used. On a status page the
// reader is usually already worried, and "capacity" alone is ambiguous —
// 15% could mean 15% free or 15% consumed. Reporting availability means
// the graph falling always means "worse", which is the direction people
// intuitively read on a status page.
package statuspage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/FlashbackAi/teepin-core/pkg/cluster"
)

// reportInterval is how often a data point is pushed.
//
// Statuspage requires at least one point every 5 minutes or the graph
// shows gaps; 60s gives resolution to see a burst of allocations without
// generating meaningful load.
const reportInterval = time.Minute

// Config configures the reporter. Absent PageID or MetricID disables it.
type Config struct {
	APIKey   string
	PageID   string
	MetricID string

	// BaseURL is overridable for tests.
	BaseURL string
}

// Reporter pushes GPU availability to Statuspage.
type Reporter struct {
	cfg     Config
	cluster cluster.Client
	client  *http.Client
}

func New(cfg Config, clusterClient cluster.Client) *Reporter {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.statuspage.io/v1"
	}
	return &Reporter{
		cfg:     cfg,
		cluster: clusterClient,
		// A status reporter must never become the reason a request
		// hangs, so its own calls are tightly bounded.
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// Enabled reports whether the reporter is configured.
func (r *Reporter) Enabled() bool {
	return r.cfg.APIKey != "" && r.cfg.PageID != "" && r.cfg.MetricID != ""
}

// Start pushes a data point every minute until the context ends.
func (r *Reporter) Start(ctx context.Context) {
	if !r.Enabled() {
		return
	}

	log.Println("Statuspage reporter started (GPU capacity available)")

	ticker := time.NewTicker(reportInterval)
	defer ticker.Stop()

	r.reportOnce(ctx)

	for {
		select {
		case <-ticker.C:
			r.reportOnce(ctx)
		case <-ctx.Done():
			log.Println("Statuspage reporter stopped")
			return
		}
	}
}

func (r *Reporter) reportOnce(ctx context.Context) {
	available, ok := r.availablePercent(ctx)
	if !ok {
		// Deliberately no data point. A gap in the graph is honest —
		// "we could not measure" — whereas publishing 0% would tell
		// every customer the platform is full when it may be fine and
		// merely unreachable from here.
		return
	}

	if err := r.submit(ctx, available); err != nil {
		log.Printf("Statuspage submit failed: %v", err)
	}
}

// availablePercent computes free GPU capacity across every ready node.
//
// Counts MIG slices and whole shared GPUs as equivalent allocation
// units, because that is what a customer is actually asking about: not
// "how many gigabytes exist" but "can I get a GPU right now".
func (r *Reporter) availablePercent(ctx context.Context) (float64, bool) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	nodes, err := r.cluster.Inventory(ctx)
	if err != nil {
		log.Printf("Statuspage: inventory unavailable (%v) - skipping data point", err)
		return 0, false
	}

	var total, used int
	for _, node := range nodes {
		if !node.Ready {
			continue
		}
		for _, mig := range node.MIGResources {
			total += mig.Capacity
			used += mig.Used
		}
		total += node.SharedCapacity
		used += node.SharedUsed
	}

	if total == 0 {
		// No capacity is being reported at all. That is a real condition
		// but not a measurement of availability, and publishing 0% would
		// read as "platform full" rather than "no data".
		return 0, false
	}

	free := total - used
	if free < 0 {
		free = 0
	}
	return float64(free) / float64(total) * 100, true
}

func (r *Reporter) submit(ctx context.Context, value float64) error {
	payload := map[string]any{
		"data": map[string]any{
			"timestamp": time.Now().Unix(),
			"value":     value,
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/pages/%s/metrics/%s/data",
		r.cfg.BaseURL, r.cfg.PageID, r.cfg.MetricID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "OAuth "+r.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("statuspage returned %s", resp.Status)
	}
	return nil
}
