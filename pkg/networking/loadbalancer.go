// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package networking

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// createLoadBalancerService creates a Kubernetes LoadBalancer Service for an instance
func (s *Service) createLoadBalancerService(ctx context.Context, instanceID uuid.UUID, port int32) (string, error) {
	serviceName := s.generateServiceName(instanceID)
	podSelector := fmt.Sprintf("inst-%s", instanceID.String()[:8])

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: s.namespace,
			Labels: map[string]string{
				"teepin.io/instance-id": instanceID.String(),
				"teepin.io/managed":     "true",
				"teepin.io/type":        "loadbalancer",
			},
			Annotations: map[string]string{
				// ExternalDNS annotations for automatic DNS record creation
				"external-dns.alpha.kubernetes.io/hostname": s.generateDNSName(instanceID),
				"external-dns.alpha.kubernetes.io/ttl":      "300",
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeLoadBalancer,
			Selector: map[string]string{
				"teepin.io/instance": podSelector,
			},
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Protocol:   corev1.ProtocolTCP,
					Port:       80,
					TargetPort: intstr.FromInt(int(port)),
				},
			},
			SessionAffinity: corev1.ServiceAffinityNone,
		},
	}

	_, err := s.k8sClient.CoreV1().Services(s.namespace).Create(ctx, service, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to create Service: %w", err)
	}

	return serviceName, nil
}

// deleteLoadBalancerService deletes a LoadBalancer Service
func (s *Service) deleteLoadBalancerService(ctx context.Context, serviceName string) error {
	err := s.k8sClient.CoreV1().Services(s.namespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("failed to delete Service: %w", err)
	}
	return nil
}

// getLoadBalancerIP retrieves the external IP assigned to a LoadBalancer Service
func (s *Service) getLoadBalancerIP(ctx context.Context, serviceName string) (string, error) {
	// Wait up to 30 seconds for IP assignment
	timeout := time.After(30 * time.Second)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timeout:
			return "", fmt.Errorf("timeout waiting for LoadBalancer IP assignment")
		case <-ticker.C:
			service, err := s.k8sClient.CoreV1().Services(s.namespace).Get(ctx, serviceName, metav1.GetOptions{})
			if err != nil {
				return "", fmt.Errorf("failed to get Service: %w", err)
			}

			// Check for assigned IP
			if len(service.Status.LoadBalancer.Ingress) > 0 {
				ingress := service.Status.LoadBalancer.Ingress[0]
				if ingress.IP != "" {
					return ingress.IP, nil
				}
				if ingress.Hostname != "" {
					// Some cloud providers return hostname instead of IP
					return ingress.Hostname, nil
				}
			}
		}
	}
}

// getServicePort retrieves the target port from a Service
func (s *Service) getServicePort(ctx context.Context, serviceName string) (int32, error) {
	service, err := s.k8sClient.CoreV1().Services(s.namespace).Get(ctx, serviceName, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("failed to get Service: %w", err)
	}

	if len(service.Spec.Ports) > 0 {
		targetPort := service.Spec.Ports[0].TargetPort
		if targetPort.Type == intstr.Int {
			return targetPort.IntVal, nil
		}
	}

	return 0, fmt.Errorf("no port found in Service")
}
