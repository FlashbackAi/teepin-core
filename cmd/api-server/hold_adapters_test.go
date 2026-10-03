// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/cluster"
	"github.com/FlashbackAi/teepin-core/pkg/compute"
)

// holdCluster records what the adapter asks of the cluster. Embedding the
// interface leaves every other method nil: the adapter must call only these.
type holdCluster struct {
	cluster.Client
	stopped []string
	deleted []string
	routed  map[string]string
	stopErr error
	noStop  bool
}

func (c *holdCluster) StopInstance(_ context.Context, _ cluster.Scope, id string) error {
	c.stopped = append(c.stopped, id)
	return c.stopErr
}
func (c *holdCluster) DeleteInstance(_ context.Context, _ cluster.Scope, id string) error {
	c.deleted = append(c.deleted, id)
	return nil
}
func (c *holdCluster) RouteInstance(id, provider string) {
	if c.routed == nil {
		c.routed = map[string]string{}
	}
	c.routed[id] = provider
}

// plainCluster cannot stop an instance with its disk kept.
type plainCluster struct {
	cluster.Client
	deleted []string
}

func (c *plainCluster) DeleteInstance(_ context.Context, _ cluster.Scope, id string) error {
	c.deleted = append(c.deleted, id)
	return nil
}

func holdStore(t *testing.T) (*compute.Store, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return compute.NewStore(db), mock
}

func activeRows(ids ...string) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{
		"id", "account_id", "project_id", "user_id", "name", "image",
		"instance_type_id", "status", "gpu_vram_gb",
		"cpu_units", "memory_gb", "endpoint",
		"k8s_pod_name", "k8s_namespace",
		"provider_id", "node_name", "dns_name", "public_ip", "tls_enabled", "tls_ready", "container_port",
		"storage_gb",
		"created_at", "updated_at", "started_at", "terminated_at", "build_session_id",
	})
	for _, id := range ids {
		rows.AddRow(id, uuid.New(), uuid.New(), uuid.New(), "app", "img",
			"cpu.home", "running", 0, 2, 4, "",
			id+"-pod", "default", "provider-7", "node-a", "", "", false, false, 8080,
			50, time.Now(), time.Now(), nil, nil, nil)
	}
	return rows
}

func expectActive(mock sqlmock.Sqlmock, ids ...string) {
	mock.ExpectQuery(`FROM compute\.instances WHERE account_id = \$1 AND terminated_at IS NULL`).
		WillReturnRows(activeRows(ids...))
}

func TestHoldInstances_StopsWithDiskKeptAndRecordsIt(t *testing.T) {
	store, mock := holdStore(t)
	cl := &holdCluster{}
	expectActive(mock, "inst-a", "inst-b")
	mock.ExpectQuery(`SELECT launch_spec FROM compute\.instances`).
		WillReturnRows(sqlmock.NewRows([]string{"launch_spec"}).AddRow([]byte("sealed")))
	mock.ExpectExec(`UPDATE compute\.instances\s+SET status = \$2, stopped_at = NOW\(\)`).
		WithArgs("inst-a", compute.StatusStopped, compute.StatusRunning, compute.StatusPending).
		WillReturnResult(sqlmock.NewResult(0, 1))

	held, err := newComputeStopper(cl, store).HoldInstances(context.Background(), uuid.New(), []string{"inst-a"})
	if err != nil || len(held) != 1 || held[0] != "inst-a" {
		t.Fatalf("held=%v err=%v", held, err)
	}
	if len(cl.stopped) != 1 || len(cl.deleted) != 0 {
		t.Errorf("stopped=%v deleted=%v; the disk must be kept", cl.stopped, cl.deleted)
	}
	if cl.routed["inst-a"] != "provider-7" {
		t.Errorf("routing = %v, want the record's provider recorded for the later delete", cl.routed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// Without a stored launch spec the instance could never be started again, so
// it is left running and reported - not stopped, and above all not deleted.
func TestHoldInstances_NoStoredSpecLeavesItRunning(t *testing.T) {
	store, mock := holdStore(t)
	cl := &holdCluster{}
	expectActive(mock, "inst-a")
	mock.ExpectQuery(`SELECT launch_spec FROM compute\.instances`).
		WillReturnRows(sqlmock.NewRows([]string{"launch_spec"}).AddRow(nil))

	held, err := newComputeStopper(cl, store).HoldInstances(context.Background(), uuid.New(), []string{"inst-a"})
	if err == nil || len(held) != 0 {
		t.Fatalf("held=%v err=%v; want an error and nothing held", held, err)
	}
	if len(cl.stopped) != 0 || len(cl.deleted) != 0 {
		t.Errorf("the cluster was touched: stopped=%v deleted=%v", cl.stopped, cl.deleted)
	}
}

// A cluster that cannot keep a disk must never be asked to delete instead.
func TestHoldInstances_ClusterThatCannotKeepTheDiskIsNeverAskedToDelete(t *testing.T) {
	store, mock := holdStore(t)
	cl := &plainCluster{}
	expectActive(mock, "inst-a")

	held, err := newComputeStopper(cl, store).HoldInstances(context.Background(), uuid.New(), []string{"inst-a"})
	if err == nil || len(held) != 0 {
		t.Fatalf("held=%v err=%v", held, err)
	}
	if len(cl.deleted) != 0 {
		t.Errorf("a disk-backed instance was deleted: %v", cl.deleted)
	}
}

func TestHoldInstances_FailedStopIsNotRecordedAsStopped(t *testing.T) {
	store, mock := holdStore(t)
	cl := &holdCluster{stopErr: errors.New("old agent: unsupported command")}
	expectActive(mock, "inst-a")
	mock.ExpectQuery(`SELECT launch_spec FROM compute\.instances`).
		WillReturnRows(sqlmock.NewRows([]string{"launch_spec"}).AddRow([]byte("sealed")))

	held, err := newComputeStopper(cl, store).HoldInstances(context.Background(), uuid.New(), []string{"inst-a"})
	if err == nil || len(held) != 0 {
		t.Fatalf("held=%v err=%v", held, err)
	}
	// No MarkStopped UPDATE was expected: recording a stop that did not happen
	// would make the reconciler and billing believe the instance is gone.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// Only the instances asked about are touched.
func TestHoldInstances_IgnoresInstancesNotRequested(t *testing.T) {
	store, mock := holdStore(t)
	cl := &holdCluster{}
	expectActive(mock, "inst-a", "inst-other")

	held, _ := newComputeStopper(cl, store).HoldInstances(context.Background(), uuid.New(), []string{"inst-missing"})
	if len(held) != 0 || len(cl.stopped) != 0 {
		t.Errorf("held=%v stopped=%v", held, cl.stopped)
	}
}

func TestHeldDisks_PurgeDeletesEachStoppedInstanceAfterRouting(t *testing.T) {
	store, mock := holdStore(t)
	cl := &holdCluster{}
	mock.ExpectQuery(`FROM compute\.instances WHERE account_id = \$1 AND status = \$2 AND terminated_at IS NULL`).
		WillReturnRows(activeRows("inst-a", "inst-b"))
	mock.ExpectExec(`SET status = \$1, terminated_at = NOW\(\)`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`SET status = \$1, terminated_at = NOW\(\)`).WillReturnResult(sqlmock.NewResult(0, 1))

	if err := newHeldDisks(cl, store).PurgeAccount(context.Background(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	if len(cl.deleted) != 2 {
		t.Errorf("deleted = %v, want both stopped instances", cl.deleted)
	}
	if cl.routed["inst-a"] != "provider-7" || cl.routed["inst-b"] != "provider-7" {
		t.Errorf("routing = %v: the delete must reach the node that holds the disk", cl.routed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
