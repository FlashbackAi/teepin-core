// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package compute

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/FlashbackAi/teepin-core/pkg/cluster"
)

func testSpec(id string) cluster.InstanceSpec {
	return cluster.InstanceSpec{
		InstanceID: id, Image: "myorg/app:v1", CPUUnits: 2, MemoryGB: 4, StorageGB: 50,
		Env:        map[string]string{"DATABASE_URL": "postgres://user:hunter2@db/app"},
		ProviderID: "provider-7", NodeName: "node-a",
		Ports: []cluster.PortMapping{{Container: 8080, Protocol: "tcp"}},
	}
}

func TestSpecVault_RoundTripKeepsEverythingAndNeverStoresPlaintext(t *testing.T) {
	v, err := NewSpecVault("a-long-platform-encryption-key")
	if err != nil {
		t.Fatal(err)
	}
	spec := testSpec("inst-vault001")
	sealed, err := v.Seal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if contains(sealed, "hunter2") || contains(sealed, "myorg/app") {
		t.Fatal("the sealed spec contains plaintext")
	}
	got, err := v.Open("inst-vault001", sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got.Env["DATABASE_URL"] != spec.Env["DATABASE_URL"] || got.ProviderID != "provider-7" ||
		got.NodeName != "node-a" || got.StorageGB != 50 || len(got.Ports) != 1 {
		t.Errorf("round trip lost data: %+v", got)
	}
}

func TestSpecVault_RejectsWrongKeyWrongInstanceAndTampering(t *testing.T) {
	v, _ := NewSpecVault("key-one-key-one-key-one")
	other, _ := NewSpecVault("key-two-key-two-key-two")
	sealed, _ := v.Seal(testSpec("inst-vault002"))

	if _, err := other.Open("inst-vault002", sealed); err == nil {
		t.Error("opened with the wrong key")
	}
	// A spec sealed for one instance must not open as another's: replaying
	// it onto a different row would launch someone else's workload.
	if _, err := v.Open("inst-vault003", sealed); err == nil {
		t.Error("opened for a different instance id")
	}
	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := v.Open("inst-vault002", tampered); err == nil {
		t.Error("opened tampered data")
	}
	if _, err := v.Open("inst-vault002", []byte("short")); err == nil {
		t.Error("opened truncated data")
	}
	if _, err := NewSpecVault(""); err == nil {
		t.Error("an empty key must be refused")
	}
}

func contains(b []byte, s string) bool {
	n := len(s)
	for i := 0; i+n <= len(b); i++ {
		if string(b[i:i+n]) == s {
			return true
		}
	}
	return false
}

func TestStore_MarkStopped_OnlyRunningOrPending(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectExec(`UPDATE compute\.instances\s+SET status = \$2, stopped_at = NOW\(\)`).
		WithArgs("inst-a", StatusStopped, StatusRunning, StatusPending).
		WillReturnResult(sqlmock.NewResult(0, 1))
	changed, err := store.MarkStopped(context.Background(), "inst-a")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}

	mock.ExpectExec(`UPDATE compute\.instances`).WillReturnResult(sqlmock.NewResult(0, 0))
	if changed, _ := store.MarkStopped(context.Background(), "inst-b"); changed {
		t.Error("reported a change for an instance that was not running")
	}
}

func TestStore_MarkStarted_RecordsResumeMoment(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectExec(`resumed_at = NOW\(\)`).
		WithArgs("inst-a", StatusPending, "pod-1", "https://e", "dns", "1.2.3.4", true, false, StatusStopped).
		WillReturnResult(sqlmock.NewResult(0, 1))
	changed, err := store.MarkStarted(context.Background(), "inst-a",
		StartResult{PodName: "pod-1", Endpoint: "https://e", DNSName: "dns", PublicIP: "1.2.3.4", TLSEnabled: true})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestStore_LoadLaunchSpec_MissingIsErrNoLaunchSpec(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectQuery(`SELECT launch_spec FROM compute\.instances`).
		WillReturnRows(sqlmock.NewRows([]string{"launch_spec"}).AddRow(nil))
	if _, err := store.LoadLaunchSpec(context.Background(), "inst-a"); !errors.Is(err, ErrNoLaunchSpec) {
		t.Errorf("err = %v, want ErrNoLaunchSpec", err)
	}
	mock.ExpectQuery(`SELECT launch_spec FROM compute\.instances`).
		WillReturnRows(sqlmock.NewRows([]string{"launch_spec"}))
	if _, err := store.LoadLaunchSpec(context.Background(), "inst-none"); !errors.Is(err, ErrNoLaunchSpec) {
		t.Errorf("unknown instance err = %v, want ErrNoLaunchSpec", err)
	}
}

// A stopped instance has no pod on purpose. The reconciler must not read that
// as a vanished workload and mark it terminated (which would lose its record
// and, with it, the way to delete or restart the disk).
func TestReconcile_LeavesStoppedInstanceAlone(t *testing.T) {
	store, mock := newMockStore(t)
	expectListActive(mock, "inst-held0001", StatusStopped)

	stub := &stubCluster{statuses: []cluster.InstanceStatus{liveInstance("inst-other", StatusRunning)}}
	r := NewReconciler(store, stub)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// No UPDATE was expected; sqlmock would have failed the call.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
