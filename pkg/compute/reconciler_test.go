// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package compute

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func managedPod(instanceID string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instanceID + "-pod",
			Namespace: "default",
			Labels: map[string]string{
				labelManaged:    "true",
				labelInstanceID: instanceID,
			},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func expectListActive(mock sqlmock.Sqlmock, id, status string) {
	mock.ExpectQuery(`SELECT .+ FROM compute\.instances WHERE terminated_at IS NULL`).
		WillReturnRows(instanceRows().AddRow(
			id, uuid.New(), uuid.New(), "app", "nginx:latest",
			"gpu.h100.2g.20gb", status, 20, 8, 32, "",
			id+"-pod", "default", time.Now(), time.Now(), nil, nil,
		))
}

func TestReconcile_UpdatesStatusFromPodPhase(t *testing.T) {
	store, mock := newMockStore(t)
	expectListActive(mock, "inst-aaaa1111", StatusPending)

	// Pod is now Running → status must be updated.
	mock.ExpectExec(`UPDATE compute\.instances`).
		WithArgs(StatusRunning, "inst-aaaa1111").
		WillReturnResult(sqlmock.NewResult(0, 1))

	k8s := fake.NewSimpleClientset(managedPod("inst-aaaa1111", corev1.PodRunning))
	r := NewReconciler(store, k8s, "default")

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestReconcile_TerminatesInstanceWithoutPod(t *testing.T) {
	store, mock := newMockStore(t)
	expectListActive(mock, "inst-bbbb2222", StatusRunning)

	// No pod in the cluster → billing must stop.
	mock.ExpectExec(`UPDATE compute\.instances`).
		WithArgs(StatusTerminated, "inst-bbbb2222").
		WillReturnResult(sqlmock.NewResult(0, 1))

	k8s := fake.NewSimpleClientset() // empty cluster
	r := NewReconciler(store, k8s, "default")

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestReconcile_CompletedPodTerminatesBilling(t *testing.T) {
	store, mock := newMockStore(t)
	expectListActive(mock, "inst-cccc3333", StatusRunning)

	// Succeeded pod → MarkTerminated (stamps terminated_at), not a
	// plain status update.
	mock.ExpectExec(`UPDATE compute\.instances`).
		WithArgs(StatusTerminated, "inst-cccc3333").
		WillReturnResult(sqlmock.NewResult(0, 1))

	k8s := fake.NewSimpleClientset(managedPod("inst-cccc3333", corev1.PodSucceeded))
	r := NewReconciler(store, k8s, "default")

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
	// No UPDATE expected: pod phase matches stored status.

	k8s := fake.NewSimpleClientset(managedPod("inst-dddd4444", corev1.PodRunning))
	r := NewReconciler(store, k8s, "default")

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestPodPhaseToStatus(t *testing.T) {
	cases := map[corev1.PodPhase]string{
		corev1.PodRunning:   StatusRunning,
		corev1.PodPending:   StatusPending,
		corev1.PodFailed:    StatusFailed,
		corev1.PodSucceeded: StatusTerminated,
	}
	for phase, want := range cases {
		if got := podPhaseToStatus(phase); got != want {
			t.Errorf("podPhaseToStatus(%s) = %q, want %q", phase, got, want)
		}
	}
}
