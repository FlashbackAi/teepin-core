// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package compute

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/cluster"
)

// stubCluster reports a fixed set of instance statuses, or a fixed
// error. Only the methods the reconciler uses do anything.
type stubCluster struct {
	statuses []cluster.InstanceStatus
	err      error

	// sawScope records what the reconciler asked for, so the test can
	// assert it queried across all tenants rather than one project.
	sawScope cluster.Scope
}

func (s *stubCluster) ListInstanceStatuses(_ context.Context, scope cluster.Scope) ([]cluster.InstanceStatus, error) {
	s.sawScope = scope
	if s.err != nil {
		return nil, s.err
	}
	return s.statuses, nil
}

func (s *stubCluster) CreateInstance(context.Context, cluster.InstanceSpec) (*cluster.InstanceResult, error) {
	return nil, s.err
}
func (s *stubCluster) DeleteInstance(context.Context, cluster.Scope, string) error { return s.err }
func (s *stubCluster) GetInstanceStatus(context.Context, cluster.Scope, string) (*cluster.InstanceStatus, error) {
	return nil, s.err
}
func (s *stubCluster) StreamLogs(context.Context, cluster.Scope, string, cluster.LogOptions, io.Writer) error {
	return s.err
}
func (s *stubCluster) Inventory(context.Context) ([]cluster.NodeInventory, error) {
	return nil, s.err
}
func (s *stubCluster) Healthy(context.Context) bool { return s.err == nil }

func liveInstance(id, status string) cluster.InstanceStatus {
	return cluster.InstanceStatus{
		InstanceID: id,
		Status:     status,
		ObservedAt: time.Now().UTC(),
	}
}

func expectListActive(mock sqlmock.Sqlmock, id, status string) {
	mock.ExpectQuery(`SELECT .+ FROM compute\.instances WHERE terminated_at IS NULL`).
		WillReturnRows(instanceRows().AddRow(
			id, uuid.New(), uuid.New(), uuid.New(), "app", "nginx:latest",
			"gpu.h100.2g.20gb", status, 20, 8, 32, "",
			id+"-pod", "default", time.Now(), time.Now(), nil, nil,
		))
}

func TestReconcile_UpdatesStatusFromClusterStatus(t *testing.T) {
	store, mock := newMockStore(t)
	expectListActive(mock, "inst-aaaa1111", StatusPending)

	// Cluster reports running → stored status must catch up.
	mock.ExpectExec(`UPDATE compute\.instances`).
		WithArgs(StatusRunning, "inst-aaaa1111").
		WillReturnResult(sqlmock.NewResult(0, 1))

	stub := &stubCluster{statuses: []cluster.InstanceStatus{
		liveInstance("inst-aaaa1111", StatusRunning),
	}}
	r := NewReconciler(store, stub)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestReconcile_QueriesAcrossAllTenants(t *testing.T) {
	store, mock := newMockStore(t)
	expectListActive(mock, "inst-aaaa1111", StatusRunning)

	stub := &stubCluster{statuses: []cluster.InstanceStatus{
		liveInstance("inst-aaaa1111", StatusRunning),
	}}
	r := NewReconciler(store, stub)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// A scoped query here would make every other tenant's instances look
	// absent, and the absence rule would terminate them all.
	if stub.sawScope.IsRestricted() {
		t.Errorf("reconciler must query AllTenants, got scope %+v", stub.sawScope)
	}
}

func TestReconcile_TerminatesInstanceMissingFromCluster(t *testing.T) {
	store, mock := newMockStore(t)
	expectListActive(mock, "inst-bbbb2222", StatusRunning)

	// Not in the cluster → billing must stop.
	mock.ExpectExec(`UPDATE compute\.instances`).
		WithArgs(StatusTerminated, "inst-bbbb2222").
		WillReturnResult(sqlmock.NewResult(0, 1))

	stub := &stubCluster{} // empty cluster
	r := NewReconciler(store, stub)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestReconcile_UnreachableClusterTerminatesNothing guards the worst
// failure this loop can have.
//
// The rule "absent from the cluster means terminated" is correct only
// when the cluster actually answered. If an unreachable cluster were
// read as an empty list, one network blip would mark every running
// instance on the platform terminated: billing would stop for every
// customer at once, and every customer would be told their workloads had
// vanished. Stale state is recoverable; this is not.
func TestReconcile_UnreachableClusterTerminatesNothing(t *testing.T) {
	store, mock := newMockStore(t)
	expectListActive(mock, "inst-cccc3333", StatusRunning)

	// Deliberately no ExpectExec: any write at all is a failure here,
	// and sqlmock fails the test on unexpected statements.

	stub := &stubCluster{err: cluster.ErrClusterUnavailable}
	r := NewReconciler(store, stub)

	// Not an error either: an unreachable cluster is an expected
	// transient condition, not something to alert on every minute.
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unreachable cluster should be handled quietly, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("no writes should occur when the cluster is unreachable: %v", err)
	}
}

func TestReconcile_CompletedInstanceTerminatesBilling(t *testing.T) {
	store, mock := newMockStore(t)
	expectListActive(mock, "inst-cccc3333", StatusRunning)

	// Terminated → MarkTerminated (stamps terminated_at), not a plain
	// status update, or billing would keep running.
	mock.ExpectExec(`UPDATE compute\.instances`).
		WithArgs(StatusTerminated, "inst-cccc3333").
		WillReturnResult(sqlmock.NewResult(0, 1))

	stub := &stubCluster{statuses: []cluster.InstanceStatus{
		liveInstance("inst-cccc3333", StatusTerminated),
	}}
	r := NewReconciler(store, stub)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestReconcile_NoChangeNoWrites(t *testing.T) {
	store, mock := newMockStore(t)
	expectListActive(mock, "inst-dddd4444", StatusRunning)
	// No UPDATE expected: cluster status matches stored status.

	stub := &stubCluster{statuses: []cluster.InstanceStatus{
		liveInstance("inst-dddd4444", StatusRunning),
	}}
	r := NewReconciler(store, stub)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
