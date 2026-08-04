// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package networking

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// createInstanceService creates a ClusterIP Service fronting one
// instance's pod.
//
// Deliberately ClusterIP, not LoadBalancer: a per-instance
// LoadBalancer consumes one routable IP per customer, which a single
// bare-metal node cannot supply (and without MetalLB it never gets an
// address at all — it stays <pending> and instance creation stalls).
// Public reachability comes from the shared ingress-nginx LoadBalancer
// plus a wildcard DNS record, so a single node IP serves every
// instance, distinguished by hostname.
func (s *Service) createInstanceService(ctx context.Context, instanceID uuid.UUID, instanceName string, port int32) (string, error) {
	serviceName := s.generateServiceName(instanceID)

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: s.instanceNamespace,
			Labels: map[string]string{
				"teepin.io/instance-id": instanceID.String(),
				"teepin.io/managed":     "true",
				"teepin.io/type":        "instance-service",
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			// Must match the labels the API sets on instance pods
			// (pkg/api/server.go createPod). A mismatch here routes
			// nowhere and every customer request 503s.
			Selector: map[string]string{
				"app.teepin.cloud/instance-id": instanceName,
			},
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Protocol:   corev1.ProtocolTCP,
					Port:       port,
					TargetPort: intstr.FromInt(int(port)),
				},
			},
			SessionAffinity: corev1.ServiceAffinityNone,
		},
	}

	_, err := s.k8sClient.CoreV1().Services(s.instanceNamespace).Create(ctx, service, metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			return serviceName, nil
		}
		return "", fmt.Errorf("failed to create Service: %w", err)
	}

	return serviceName, nil
}

// deleteInstanceService deletes an instance's Service.
func (s *Service) deleteInstanceService(ctx context.Context, serviceName string) error {
	err := s.k8sClient.CoreV1().Services(s.instanceNamespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("failed to delete Service: %w", err)
	}
	return nil
}

// ingressPublicIP returns the address customers' DNS should point at:
// the shared ingress-nginx LoadBalancer, or the node IP when the
// controller runs as NodePort (the bare-metal default).
func (s *Service) ingressPublicIP(ctx context.Context) string {
	svc, err := s.k8sClient.CoreV1().Services("ingress-nginx").
		Get(ctx, "ingress-nginx-controller", metav1.GetOptions{})
	if err == nil && len(svc.Status.LoadBalancer.Ingress) > 0 {
		if ip := svc.Status.LoadBalancer.Ingress[0].IP; ip != "" {
			return ip
		}
		if h := svc.Status.LoadBalancer.Ingress[0].Hostname; h != "" {
			return h
		}
	}

	// NodePort / hostNetwork: fall back to a node's address.
	nodes, err := s.k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil || len(nodes.Items) == 0 {
		return ""
	}
	var internal string
	for _, addr := range nodes.Items[0].Status.Addresses {
		if addr.Type == corev1.NodeExternalIP && addr.Address != "" {
			return addr.Address
		}
		if addr.Type == corev1.NodeInternalIP && internal == "" {
			internal = addr.Address
		}
	}
	return internal
}

// getServicePort retrieves the port from an instance Service.
func (s *Service) getServicePort(ctx context.Context, serviceName string) (int32, error) {
	service, err := s.k8sClient.CoreV1().Services(s.instanceNamespace).Get(ctx, serviceName, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("failed to get Service: %w", err)
	}

	if len(service.Spec.Ports) > 0 {
		return service.Spec.Ports[0].Port, nil
	}

	return 0, fmt.Errorf("no port found in Service")
}
