// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/FlashbackAi/teepin-core/pkg/compute"
	"github.com/FlashbackAi/teepin-core/pkg/gpu"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// tenantPod builds a TEEPIN-managed pod owned by a project.
func tenantPod(instanceID string, projectID uuid.UUID, vramGB string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instanceID + "-pod",
			Namespace: "default",
			Labels: map[string]string{
				"app.teepin.cloud/managed":     "true",
				"app.teepin.cloud/instance-id": instanceID,
				"app.teepin.cloud/name":        "app-" + instanceID,
				"teepin.io/instance-uuid":      uuid.New().String(),
				LabelProjectID:                 projectID.String(),
			},
			Annotations: map[string]string{
				gpu.AnnotationVRAMGB:   vramGB,
				annotationInstanceType: "gpu.h100.custom-" + vramGB + "gb",
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// newTenantServer builds a Server with tenancy active (store present)
// over a fake cluster. The fake clientset does not filter List calls
// by label selector, so a reactor applies the selector the way the
// real API server would.
func newTenantServer(t *testing.T, objects ...runtime.Object) *Server {
	t.Helper()

	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	client := fake.NewSimpleClientset(objects...)
	client.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		listAction := action.(k8stesting.ListAction)
		restriction := listAction.GetListRestrictions().Labels

		pods, err := client.Tracker().List(
			corev1.SchemeGroupVersion.WithResource("pods"),
			corev1.SchemeGroupVersion.WithKind("Pod"), "default")
		if err != nil {
			return true, nil, err
		}

		filtered := &corev1.PodList{}
		for _, item := range pods.(*corev1.PodList).Items {
			if restriction == nil || restriction.Matches(labels.Set(item.Labels)) {
				filtered.Items = append(filtered.Items, item)
			}
		}
		return true, filtered, nil
	})

	return NewServer(client, nil, nil, compute.NewStore(db))
}

// doRequest performs a request against the handler with the caller's
// project injected the way auth middleware would.
func doRequest(handler gin.HandlerFunc, method, path string, params gin.Params, projectID uuid.UUID) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, path, nil)
	c.Params = params
	if projectID != uuid.Nil {
		c.Set(string(auth.ProjectIDKey), projectID)
	}
	handler(c)
	return w
}

func TestListInstances_ScopedToCallerProject(t *testing.T) {
	tenantA, tenantB := uuid.New(), uuid.New()
	server := newTenantServer(t,
		tenantPod("inst-aaaa1111", tenantA, "20"),
		tenantPod("inst-bbbb2222", tenantB, "25"),
	)

	w := doRequest(server.ListInstances, http.MethodGet, "/v1/compute/instances", nil, tenantA)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}

	var resp struct {
		Count     int `json:"count"`
		Instances []struct {
			ID string `json:"id"`
		} `json:"instances"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad response: %v", err)
	}
	if resp.Count != 1 || len(resp.Instances) != 1 || resp.Instances[0].ID != "inst-aaaa1111" {
		t.Errorf("tenant A must see only its own instance, got %s", w.Body.String())
	}
}

func TestListInstances_RequiresAuthWhenTenancyActive(t *testing.T) {
	server := newTenantServer(t)

	w := doRequest(server.ListInstances, http.MethodGet, "/v1/compute/instances", nil, uuid.Nil)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 without project context", w.Code)
	}
}

func TestGetInstance_CrossTenantIs404(t *testing.T) {
	tenantA, tenantB := uuid.New(), uuid.New()
	server := newTenantServer(t, tenantPod("inst-aaaa1111", tenantA, "20"))

	params := gin.Params{{Key: "id", Value: "inst-aaaa1111"}}

	// Owner sees it.
	if w := doRequest(server.GetInstance, http.MethodGet, "/", params, tenantA); w.Code != http.StatusOK {
		t.Errorf("owner GET status = %d, want 200", w.Code)
	}

	// Another tenant gets 404 — indistinguishable from nonexistent.
	if w := doRequest(server.GetInstance, http.MethodGet, "/", params, tenantB); w.Code != http.StatusNotFound {
		t.Errorf("cross-tenant GET status = %d, want 404", w.Code)
	}
}

func TestDeleteInstance_CrossTenantIs404(t *testing.T) {
	tenantA, tenantB := uuid.New(), uuid.New()
	server := newTenantServer(t, tenantPod("inst-aaaa1111", tenantA, "20"))

	params := gin.Params{{Key: "id", Value: "inst-aaaa1111"}}

	// Another tenant cannot delete it.
	if w := doRequest(server.DeleteInstance, http.MethodDelete, "/", params, tenantB); w.Code != http.StatusNotFound {
		t.Errorf("cross-tenant DELETE status = %d, want 404", w.Code)
	}
}

func TestDeleteInstance_NonexistentIs404(t *testing.T) {
	server := newTenantServer(t)

	params := gin.Params{{Key: "id", Value: "inst-nope0000"}}
	if w := doRequest(server.DeleteInstance, http.MethodDelete, "/", params, uuid.New()); w.Code != http.StatusNotFound {
		t.Errorf("DELETE nonexistent status = %d, want 404 (was silently 200 before)", w.Code)
	}
}

func TestPodToInstance_EnrichesFromAnnotations(t *testing.T) {
	pod := tenantPod("inst-aaaa1111", uuid.New(), "25")
	pod.Spec.Containers = []corev1.Container{{Image: "nginx:latest"}}

	inst := podToInstance(pod)

	if inst.GPUVRAM != "25GB" || inst.AllocatedVRAM != "25GB" {
		t.Errorf("VRAM = %q/%q, want 25GB/25GB", inst.GPUVRAM, inst.AllocatedVRAM)
	}
	if inst.PricePerHour != 2.50 {
		t.Errorf("PricePerHour = %.2f, want 2.50", inst.PricePerHour)
	}
	if inst.InstanceType != "gpu.h100.custom-25gb" {
		t.Errorf("InstanceType = %q, want gpu.h100.custom-25gb", inst.InstanceType)
	}
	if inst.Image != "nginx:latest" {
		t.Errorf("Image = %q, want nginx:latest", inst.Image)
	}
}

func TestParseMemoryGB(t *testing.T) {
	cases := map[string]int{
		"32GB":  32,
		"512MB": 1, // rounds up
		"4G":    4,
		"weird": 0,
		"":      0,
	}
	for in, want := range cases {
		if got := parseMemoryGB(in); got != want {
			t.Errorf("parseMemoryGB(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestRequireProjectScope_StandaloneModeAllowsAll(t *testing.T) {
	// Without a store (no database) there is no tenancy: requests
	// proceed unscoped, preserving the zero-dependency local dev flow.
	server := NewServer(fake.NewSimpleClientset(), nil, nil, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	projectID, ok := server.requireProjectScope(c)
	if !ok || projectID != uuid.Nil {
		t.Errorf("standalone mode must allow unscoped access, got ok=%v id=%s", ok, projectID)
	}
	if strings.Contains(w.Body.String(), "error") {
		t.Errorf("no error should be written in standalone mode")
	}
}
