// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package networking

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// createIngress creates a Kubernetes Ingress with TLS for an instance.
// useTLS/tlsIssuer come from the caller's resolved EndpointOptions, not
// directly from s.useTLS/s.tlsIssuer — see EndpointOptions on why this
// call can override the Service's configured defaults.
func (s *Service) createIngress(ctx context.Context, instanceID uuid.UUID, dnsName, serviceName string, port int32, useTLS bool, tlsIssuer string) (string, error) {
	ingressName := s.generateIngressName(instanceID)
	pathTypePrefix := networkingv1.PathTypePrefix

	ingress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name: ingressName,
			// Must live alongside the Service it references — an Ingress
			// cannot route to a Service in another namespace.
			Namespace: s.instanceNamespace,
			Labels: map[string]string{
				"teepin.io/instance-id": instanceID.String(),
				"teepin.io/managed":     "true",
				"teepin.io/type":        "ingress",
			},
			Annotations: map[string]string{
				// Nginx Ingress Controller annotations
				"kubernetes.io/ingress.class":                    "nginx",
				"nginx.ingress.kubernetes.io/ssl-redirect":       "true",
				"nginx.ingress.kubernetes.io/force-ssl-redirect": "true",
			},
		},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{
				{
					Host: dnsName,
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{
							Paths: []networkingv1.HTTPIngressPath{
								{
									Path:     "/",
									PathType: &pathTypePrefix,
									Backend: networkingv1.IngressBackend{
										Service: &networkingv1.IngressServiceBackend{
											Name: serviceName,
											// The customer's actual port: hardcoding 80
											// routed to a port most instances never
											// listen on.
											Port: networkingv1.ServiceBackendPort{
												Number: port,
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	// Add TLS configuration if enabled
	if useTLS {
		ingress.Annotations["cert-manager.io/cluster-issuer"] = tlsIssuer
		ingress.Spec.TLS = []networkingv1.IngressTLS{
			{
				Hosts:      []string{dnsName},
				SecretName: fmt.Sprintf("%s-tls", ingressName),
			},
		}
	}

	_, err := s.k8sClient.NetworkingV1().Ingresses(s.instanceNamespace).Create(ctx, ingress, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to create Ingress: %w", err)
	}

	return ingressName, nil
}

// deleteIngress deletes an Ingress
func (s *Service) deleteIngress(ctx context.Context, ingressName string) error {
	err := s.k8sClient.NetworkingV1().Ingresses(s.instanceNamespace).Delete(ctx, ingressName, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("failed to delete Ingress: %w", err)
	}
	return nil
}

// isTLSReady checks if the TLS certificate has been provisioned by cert-manager
func (s *Service) isTLSReady(ctx context.Context, ingressName string, useTLS bool) (bool, error) {
	if !useTLS {
		return false, nil
	}

	ingress, err := s.k8sClient.NetworkingV1().Ingresses(s.instanceNamespace).Get(ctx, ingressName, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to get Ingress: %w", err)
	}

	// Check if TLS secret exists
	if len(ingress.Spec.TLS) == 0 {
		return false, nil
	}

	secretName := ingress.Spec.TLS[0].SecretName

	// Try to get the TLS secret
	_, err = s.k8sClient.CoreV1().Secrets(s.instanceNamespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to get TLS secret: %w", err)
	}

	// Secret exists, TLS is ready
	return true, nil
}
