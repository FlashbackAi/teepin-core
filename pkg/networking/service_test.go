// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package networking

import (
	"context"
	"testing"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func newTestService(useTLS bool) (*Service, *fake.Clientset) {
	client := fake.NewSimpleClientset()
	svc := NewService(client, Config{
		Domain:    "teepin.com",
		Namespace: "default",
		UseTLS:    useTLS,
		TLSIssuer: "letsencrypt-prod",
	})
	return svc, client
}

// TestProvisionEndpoint_ServiceSelectorMatchesPodLabel is the test the
// Stage 3 plan calls out by name: this alone would have caught defect 3
// (the namespace mismatch that made every Service select nothing in
// production). The label here — "app.teepin.cloud/instance-id" — must
// match exactly what pkg/cluster's buildPod writes onto the pod
// (labelInstanceID in direct.go); a typo or drift here again routes every
// customer request nowhere.
func TestProvisionEndpoint_ServiceSelectorMatchesPodLabel(t *testing.T) {
	svc, client := newTestService(false)
	instanceID := uuid.New()

	info, err := svc.ProvisionEndpoint(context.Background(), instanceID, "inst-abc12345", 8080, EndpointOptions{})
	if err != nil {
		t.Fatalf("ProvisionEndpoint: %v", err)
	}

	k8sSvc, err := client.CoreV1().Services("default").Get(context.Background(), info.ServiceName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get Service: %v", err)
	}

	want := "inst-abc12345"
	if got := k8sSvc.Spec.Selector["app.teepin.cloud/instance-id"]; got != want {
		t.Errorf("Service selector app.teepin.cloud/instance-id = %q, want %q (must match the label buildPod writes on the pod, or every request 503s)", got, want)
	}
}

// TestProvisionEndpoint_NamespaceMatchesConfig guards defect 3
// structurally: the Service and Ingress must land in the SAME namespace
// pods actually run in (cluster.WorkloadNamespace in production), because
// a Service cannot select pods in a different namespace.
func TestProvisionEndpoint_NamespaceMatchesConfig(t *testing.T) {
	svc, client := newTestService(false)
	instanceID := uuid.New()

	info, err := svc.ProvisionEndpoint(context.Background(), instanceID, "inst-abc12345", 8080, EndpointOptions{})
	if err != nil {
		t.Fatalf("ProvisionEndpoint: %v", err)
	}

	if _, err := client.CoreV1().Services("default").Get(context.Background(), info.ServiceName, metav1.GetOptions{}); err != nil {
		t.Errorf("Service not found in configured namespace %q: %v", "default", err)
	}
	if _, err := client.NetworkingV1().Ingresses("default").Get(context.Background(), info.IngressName, metav1.GetOptions{}); err != nil {
		t.Errorf("Ingress not found in configured namespace %q: %v", "default", err)
	}
}

// TestProvisionEndpoint_TLSAnnotationGatedOnUseTLS covers defect 4: the
// cert-manager annotation and spec.TLS block must appear ONLY when TLS is
// actually enabled for this call, not unconditionally and not from the
// Service's own possibly-stale default.
func TestProvisionEndpoint_TLSAnnotationGatedOnUseTLS(t *testing.T) {
	t.Run("TLS disabled: no annotation, no spec.TLS", func(t *testing.T) {
		svc, client := newTestService(false)
		instanceID := uuid.New()

		info, err := svc.ProvisionEndpoint(context.Background(), instanceID, "inst-notls001", 80, EndpointOptions{})
		if err != nil {
			t.Fatalf("ProvisionEndpoint: %v", err)
		}
		ing, err := client.NetworkingV1().Ingresses("default").Get(context.Background(), info.IngressName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get Ingress: %v", err)
		}
		if _, ok := ing.Annotations["cert-manager.io/cluster-issuer"]; ok {
			t.Error("cert-manager annotation present when UseTLS=false")
		}
		if len(ing.Spec.TLS) != 0 {
			t.Error("spec.TLS populated when UseTLS=false")
		}
		if info.TLSEnabled {
			t.Error("EndpointInfo.TLSEnabled=true when UseTLS=false")
		}
		if info.HTTPSURL[:4] != "http" || info.HTTPSURL[:5] == "https" {
			t.Errorf("HTTPSURL should use http:// scheme when TLS disabled, got %q", info.HTTPSURL)
		}
	})

	t.Run("TLS enabled: annotation and spec.TLS present", func(t *testing.T) {
		svc, client := newTestService(true)
		instanceID := uuid.New()

		info, err := svc.ProvisionEndpoint(context.Background(), instanceID, "inst-withtls01", 80, EndpointOptions{})
		if err != nil {
			t.Fatalf("ProvisionEndpoint: %v", err)
		}
		ing, err := client.NetworkingV1().Ingresses("default").Get(context.Background(), info.IngressName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get Ingress: %v", err)
		}
		if ing.Annotations["cert-manager.io/cluster-issuer"] != "letsencrypt-prod" {
			t.Errorf("cert-manager annotation = %q, want letsencrypt-prod", ing.Annotations["cert-manager.io/cluster-issuer"])
		}
		if len(ing.Spec.TLS) != 1 || ing.Spec.TLS[0].Hosts[0] != "inst-withtls01.teepin.com" {
			t.Errorf("spec.TLS not populated correctly: %+v", ing.Spec.TLS)
		}
		if !info.TLSEnabled {
			t.Error("EndpointInfo.TLSEnabled=false when UseTLS=true")
		}
		// cert-manager has not issued yet immediately after provisioning.
		if info.TLSReady {
			t.Error("TLSReady=true immediately after ProvisionEndpoint — cert-manager issues asynchronously")
		}
	})
}

// TestProvisionEndpoint_OptionsOverrideServiceDefaults covers the Stage 3
// A9 seam: a per-call EndpointOptions must override the Service's own
// configured domain/TLS, not just supplement it — this is what lets the
// control plane provision a home instance's endpoint with a different
// domain/TLS policy than a datacenter agent's own static config, from the
// exact same Service type.
func TestProvisionEndpoint_OptionsOverrideServiceDefaults(t *testing.T) {
	svc, client := newTestService(false) // service default: TLS off
	instanceID := uuid.New()
	trueVal := true

	info, err := svc.ProvisionEndpoint(context.Background(), instanceID, "inst-override01", 80, EndpointOptions{
		Domain:    "custom.example.com",
		UseTLS:    &trueVal,
		TLSIssuer: "letsencrypt-staging",
	})
	if err != nil {
		t.Fatalf("ProvisionEndpoint: %v", err)
	}
	if info.DNSName != "inst-override01.custom.example.com" {
		t.Errorf("DNSName = %q, want override domain applied", info.DNSName)
	}
	if !info.TLSEnabled {
		t.Error("TLSEnabled=false despite UseTLS override=true")
	}
	ing, err := client.NetworkingV1().Ingresses("default").Get(context.Background(), info.IngressName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get Ingress: %v", err)
	}
	if ing.Annotations["cert-manager.io/cluster-issuer"] != "letsencrypt-staging" {
		t.Errorf("issuer = %q, want override letsencrypt-staging", ing.Annotations["cert-manager.io/cluster-issuer"])
	}
}

// TestProvisionEndpoint_ZeroOptionsPreserveServiceDefaults confirms the
// A9 change is byte-for-byte revertable: a caller sending an empty
// EndpointOptions (every existing caller, before Stage 3) gets exactly
// the Service's own configured domain/TLS, unchanged.
func TestProvisionEndpoint_ZeroOptionsPreserveServiceDefaults(t *testing.T) {
	svc, _ := newTestService(true)
	instanceID := uuid.New()

	info, err := svc.ProvisionEndpoint(context.Background(), instanceID, "inst-zeroopt01", 80, EndpointOptions{})
	if err != nil {
		t.Fatalf("ProvisionEndpoint: %v", err)
	}
	if info.DNSName != "inst-zeroopt01.teepin.com" {
		t.Errorf("DNSName = %q, want the Service's configured domain", info.DNSName)
	}
	if !info.TLSEnabled {
		t.Error("TLSEnabled=false despite the Service's configured UseTLS=true")
	}
}

// TestRevokeEndpoint_IdempotentOnMissingResources covers a delete of an
// instance whose Service/Ingress are already gone (e.g. a retried delete
// after a partial failure) — must succeed quietly, not error.
func TestRevokeEndpoint_IdempotentOnMissingResources(t *testing.T) {
	svc, _ := newTestService(false)
	instanceID := uuid.New()

	if err := svc.RevokeEndpoint(context.Background(), instanceID); err != nil {
		t.Fatalf("RevokeEndpoint on a never-provisioned instance should be idempotent, got: %v", err)
	}
}

// TestRevokeEndpoint_DeletesServiceAndIngress is the straightforward
// round trip: provision, then revoke, then confirm both resources gone.
func TestRevokeEndpoint_DeletesServiceAndIngress(t *testing.T) {
	svc, client := newTestService(false)
	instanceID := uuid.New()

	info, err := svc.ProvisionEndpoint(context.Background(), instanceID, "inst-revoke001", 80, EndpointOptions{})
	if err != nil {
		t.Fatalf("ProvisionEndpoint: %v", err)
	}

	if err := svc.RevokeEndpoint(context.Background(), instanceID); err != nil {
		t.Fatalf("RevokeEndpoint: %v", err)
	}

	if _, err := client.CoreV1().Services("default").Get(context.Background(), info.ServiceName, metav1.GetOptions{}); err == nil {
		t.Error("Service still exists after RevokeEndpoint")
	}
	if _, err := client.NetworkingV1().Ingresses("default").Get(context.Background(), info.IngressName, metav1.GetOptions{}); err == nil {
		t.Error("Ingress still exists after RevokeEndpoint")
	}
}

// TestGetEndpointInfo_TLSReadyReflectsSecretPresence covers the A6
// reconcile's data source: TLSReady must flip only once the TLS secret
// cert-manager creates actually exists, not just because an Ingress with
// spec.TLS exists.
func TestGetEndpointInfo_TLSReadyReflectsSecretPresence(t *testing.T) {
	svc, client := newTestService(true)
	instanceID := uuid.New()

	info, err := svc.ProvisionEndpoint(context.Background(), instanceID, "inst-tlsready1", 80, EndpointOptions{})
	if err != nil {
		t.Fatalf("ProvisionEndpoint: %v", err)
	}

	before, err := svc.GetEndpointInfo(context.Background(), instanceID, "inst-tlsready1", EndpointOptions{})
	if err != nil {
		t.Fatalf("GetEndpointInfo (before secret): %v", err)
	}
	if before.TLSReady {
		t.Error("TLSReady=true before the TLS secret exists")
	}

	// Simulate cert-manager finishing issuance.
	secretName := info.IngressName + "-tls"
	_, err = client.CoreV1().Secrets("default").Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: "default"},
		Type:       corev1.SecretTypeTLS,
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create TLS secret: %v", err)
	}

	after, err := svc.GetEndpointInfo(context.Background(), instanceID, "inst-tlsready1", EndpointOptions{})
	if err != nil {
		t.Fatalf("GetEndpointInfo (after secret): %v", err)
	}
	if !after.TLSReady {
		t.Error("TLSReady=false after the TLS secret was created")
	}
}

// TestProvisionEndpoint_IngressRoutesToCustomerPort guards the "hardcoded
// port 80" class of bug the plan flags for A8: the Ingress backend must
// route to the customer's ACTUAL container port, not a fixed default.
func TestProvisionEndpoint_IngressRoutesToCustomerPort(t *testing.T) {
	svc, client := newTestService(false)
	instanceID := uuid.New()

	info, err := svc.ProvisionEndpoint(context.Background(), instanceID, "inst-port12345", 8080, EndpointOptions{})
	if err != nil {
		t.Fatalf("ProvisionEndpoint: %v", err)
	}

	ing, err := client.NetworkingV1().Ingresses("default").Get(context.Background(), info.IngressName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get Ingress: %v", err)
	}
	backend := ing.Spec.Rules[0].IngressRuleValue.HTTP.Paths[0].Backend
	if backend.Service.Port.Number != 8080 {
		t.Errorf("Ingress backend port = %d, want 8080 (the customer's container port)", backend.Service.Port.Number)
	}

	k8sSvc, err := client.CoreV1().Services("default").Get(context.Background(), info.ServiceName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get Service: %v", err)
	}
	if k8sSvc.Spec.Ports[0].Port != 8080 {
		t.Errorf("Service port = %d, want 8080", k8sSvc.Spec.Ports[0].Port)
	}
}

