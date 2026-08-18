// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package statuspage

import (
	"context"
	"io"
	"testing"

	"github.com/FlashbackAi/teepin-core/pkg/cluster"
)

// stubCluster returns fixed inventory, or an error.
type stubCluster struct {
	nodes []cluster.NodeInventory
	err   error
}

func (s stubCluster) Inventory(context.Context) ([]cluster.NodeInventory, error) {
	return s.nodes, s.err
}
func (stubCluster) CreateInstance(context.Context, cluster.InstanceSpec) (*cluster.InstanceResult, error) {
	return nil, nil
}
func (stubCluster) DeleteInstance(context.Context, cluster.Scope, string) error { return nil }
func (stubCluster) GetInstanceStatus(context.Context, cluster.Scope, string) (*cluster.InstanceStatus, error) {
	return nil, nil
}
func (stubCluster) ListInstanceStatuses(context.Context, cluster.Scope) ([]cluster.InstanceStatus, error) {
	return nil, nil
}
func (stubCluster) StreamLogs(context.Context, cluster.Scope, string, cluster.LogOptions, io.Writer) error {
	return nil
}
func (stubCluster) Healthy(context.Context) bool { return true }
func (stubCluster) ResolveInstanceAddress(context.Context, string, int32) (string, error) {
	return "", nil
}

func reporterWith(nodes []cluster.NodeInventory, err error) *Reporter {
	return New(Config{APIKey: "k", PageID: "p", MetricID: "m"},
		stubCluster{nodes: nodes, err: err})
}

func TestAvailablePercent_MIGSlices(t *testing.T) {
	// The real A100: seven 1g.10gb slices, one in use.
	r := reporterWith([]cluster.NodeInventory{{
		NodeName: "gpu-1",
		Ready:    true,
		MIGResources: []cluster.MIGResource{
			{ResourceName: "nvidia.com/mig-1g.10gb", Capacity: 7, Used: 1},
		},
	}}, nil)

	got, ok := r.availablePercent(context.Background())
	if !ok {
		t.Fatal("expected a data point")
	}
	if want := 6.0 / 7.0 * 100; got != want {
		t.Errorf("available = %.2f%%, want %.2f%%", got, want)
	}
}

func TestAvailablePercent_IgnoresUnreadyNodes(t *testing.T) {
	r := reporterWith([]cluster.NodeInventory{
		{
			NodeName: "healthy",
			Ready:    true,
			MIGResources: []cluster.MIGResource{
				{Capacity: 4, Used: 0},
			},
		},
		{
			// Cordoned or NotReady: its capacity is not obtainable, so
			// counting it would advertise availability that does not
			// exist and send customers into failed allocations.
			NodeName: "draining",
			Ready:    false,
			MIGResources: []cluster.MIGResource{
				{Capacity: 100, Used: 0},
			},
		},
	}, nil)

	got, ok := r.availablePercent(context.Background())
	if !ok {
		t.Fatal("expected a data point")
	}
	if got != 100 {
		t.Errorf("available = %.2f%%, want 100%% (only the ready node counts)", got)
	}
}

// TestAvailablePercent_UnreachableReportsNothing is the important one.
//
// When inventory cannot be read, the reporter must publish NO data
// point. Publishing 0% would tell every customer on a public status page
// that the platform is full, when in reality it may be entirely healthy
// and merely unreachable from the control plane for a few seconds. A gap
// in the graph says "we could not measure"; a zero says something false.
func TestAvailablePercent_UnreachableReportsNothing(t *testing.T) {
	r := reporterWith(nil, cluster.ErrClusterUnavailable)

	_, ok := r.availablePercent(context.Background())
	if ok {
		t.Error("a data point was produced from unreachable inventory; 0% would read as 'platform full'")
	}
}

func TestAvailablePercent_NoCapacityReportsNothing(t *testing.T) {
	// An empty but successful inventory is not a measurement of
	// availability either.
	r := reporterWith([]cluster.NodeInventory{}, nil)

	if _, ok := r.availablePercent(context.Background()); ok {
		t.Error("expected no data point when no capacity is reported at all")
	}
}

func TestAvailablePercent_FullIsZeroNotNegative(t *testing.T) {
	// Used may briefly exceed capacity while the device plugin catches
	// up. A negative percentage on a public page looks like a bug.
	r := reporterWith([]cluster.NodeInventory{{
		Ready: true,
		MIGResources: []cluster.MIGResource{
			{Capacity: 4, Used: 5},
		},
	}}, nil)

	got, ok := r.availablePercent(context.Background())
	if !ok {
		t.Fatal("expected a data point")
	}
	if got != 0 {
		t.Errorf("available = %.2f%%, want 0%%", got)
	}
}

func TestEnabled_RequiresAllThreeSettings(t *testing.T) {
	// Partial configuration must not half-start: a reporter with a page
	// but no key would log a failure every minute forever.
	cases := map[string]Config{
		"no key":    {PageID: "p", MetricID: "m"},
		"no page":   {APIKey: "k", MetricID: "m"},
		"no metric": {APIKey: "k", PageID: "p"},
	}
	for name, cfg := range cases {
		if New(cfg, stubCluster{}).Enabled() {
			t.Errorf("%s: reporter should be disabled", name)
		}
	}

	full := Config{APIKey: "k", PageID: "p", MetricID: "m"}
	if !New(full, stubCluster{}).Enabled() {
		t.Error("fully configured reporter should be enabled")
	}
}
