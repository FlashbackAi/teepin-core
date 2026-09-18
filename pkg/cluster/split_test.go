// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"context"
	"errors"
	"io"
	"testing"
)

// recClient records which methods were called and holds a set of known
// instance IDs, mimicking one runtime.
type recClient struct {
	name    string
	known   map[string]bool
	created []string
	deleted []string
	healthy bool
	listErr error
	list    []InstanceStatus
}

func newRec(name string, known ...string) *recClient {
	r := &recClient{name: name, known: map[string]bool{}}
	for _, k := range known {
		r.known[k] = true
	}
	return r
}

func (r *recClient) CreateInstance(_ context.Context, spec InstanceSpec) (*InstanceResult, error) {
	r.created = append(r.created, spec.InstanceID)
	r.known[spec.InstanceID] = true
	return &InstanceResult{PodName: r.name}, nil
}
func (r *recClient) UpdateInstance(context.Context, Scope, InstanceSpec) (*InstanceResult, error) {
	return &InstanceResult{PodName: r.name}, nil
}
func (r *recClient) DeleteInstance(_ context.Context, _ Scope, id string) error {
	r.deleted = append(r.deleted, id)
	delete(r.known, id)
	return nil
}
func (r *recClient) GetInstanceStatus(_ context.Context, _ Scope, id string) (*InstanceStatus, error) {
	if r.known[id] {
		return &InstanceStatus{InstanceID: id, Status: "running", PodName: r.name}, nil
	}
	return nil, ErrNotFound
}
func (r *recClient) ListInstanceStatuses(context.Context, Scope) ([]InstanceStatus, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.list, nil
}
func (r *recClient) StreamLogs(_ context.Context, _ Scope, id string, _ LogOptions, w io.Writer) error {
	if !r.known[id] {
		return ErrNotFound
	}
	_, _ = w.Write([]byte(r.name))
	return nil
}
func (r *recClient) Inventory(context.Context) ([]NodeInventory, error)        { return nil, nil }
func (r *recClient) InstanceMetrics(context.Context) ([]InstanceMetric, error) { return nil, nil }
func (r *recClient) Healthy(context.Context) bool                              { return r.healthy }
func (r *recClient) ResolveInstanceAddress(_ context.Context, id string, _ int32) (string, error) {
	if r.known[id] {
		return r.name + ":1", nil
	}
	return "", ErrNotFound
}

func TestSplitClient_CreateRoutesBySpec(t *testing.T) {
	cont, nat := newRec("k8s"), newRec("native")
	s := NewSplitClient(cont, nat)
	ctx := context.Background()

	_, _ = s.CreateInstance(ctx, InstanceSpec{InstanceID: "model", Command: []string{"mlx_lm.server"}})
	_, _ = s.CreateInstance(ctx, InstanceSpec{InstanceID: "cust", Image: "nginx"})
	// An image AND a command is a container with an overridden entrypoint.
	_, _ = s.CreateInstance(ctx, InstanceSpec{InstanceID: "cust2", Image: "busybox", Command: []string{"sleep"}})

	if len(nat.created) != 1 || nat.created[0] != "model" {
		t.Errorf("native created = %v, want [model]", nat.created)
	}
	if len(cont.created) != 2 {
		t.Errorf("container created = %v, want [cust cust2]", cont.created)
	}
}

// Deleting a container instance must reach the container runtime even though
// the native runtime's own delete is idempotent and would "succeed" on any ID.
func TestSplitClient_DeleteReachesTheRuntimeThatHoldsTheInstance(t *testing.T) {
	cont, nat := newRec("k8s", "cust"), newRec("native", "model")
	s := NewSplitClient(cont, nat)
	ctx := context.Background()

	if err := s.DeleteInstance(ctx, AllTenants(), "cust"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteInstance(ctx, AllTenants(), "model"); err != nil {
		t.Fatal(err)
	}
	if len(cont.deleted) != 1 || cont.deleted[0] != "cust" {
		t.Errorf("container deleted = %v", cont.deleted)
	}
	if len(nat.deleted) != 1 || nat.deleted[0] != "model" {
		t.Errorf("native deleted = %v", nat.deleted)
	}
}

func TestSplitClient_ReadsFindEitherRuntime(t *testing.T) {
	s := NewSplitClient(newRec("k8s", "cust"), newRec("native", "model"))
	ctx := context.Background()

	for _, id := range []string{"cust", "model"} {
		if _, err := s.GetInstanceStatus(ctx, AllTenants(), id); err != nil {
			t.Errorf("status(%s): %v", id, err)
		}
		if _, err := s.ResolveInstanceAddress(ctx, id, 80); err != nil {
			t.Errorf("resolve(%s): %v", id, err)
		}
		var b nopBuf
		if err := s.StreamLogs(ctx, AllTenants(), id, LogOptions{}, &b); err != nil {
			t.Errorf("logs(%s): %v", id, err)
		}
	}
	if _, err := s.GetInstanceStatus(ctx, AllTenants(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id err = %v, want ErrNotFound", err)
	}
}

type nopBuf struct{}

func (nopBuf) Write(p []byte) (int, error) { return len(p), nil }

func TestSplitClient_ListMergesAndToleratesOneFailure(t *testing.T) {
	cont, nat := newRec("k8s"), newRec("native")
	cont.list = []InstanceStatus{{InstanceID: "cust"}}
	nat.list = []InstanceStatus{{InstanceID: "model"}}
	s := NewSplitClient(cont, nat)

	got, err := s.ListInstanceStatuses(context.Background(), AllTenants())
	if err != nil || len(got) != 2 {
		t.Fatalf("merged list = %v, %v", got, err)
	}

	nat.listErr = errors.New("boom")
	got, err = s.ListInstanceStatuses(context.Background(), AllTenants())
	if err != nil || len(got) != 1 {
		t.Fatalf("one runtime failing hid the other: %v, %v", got, err)
	}

	cont.listErr = errors.New("also boom")
	if _, err = s.ListInstanceStatuses(context.Background(), AllTenants()); err == nil {
		t.Fatal("both failing must be an error")
	}
}

func TestSplitClient_HealthyIsTheContainerRuntimes(t *testing.T) {
	cont, nat := newRec("k8s"), newRec("native")
	nat.healthy = true
	if NewSplitClient(cont, nat).Healthy(context.Background()) {
		t.Error("reported healthy from the native runtime; must follow containers")
	}
	cont.healthy = true
	if !NewSplitClient(cont, nat).Healthy(context.Background()) {
		t.Error("container runtime healthy but split reported unhealthy")
	}
}
