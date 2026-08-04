// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Package networking gives customer instances a public HTTPS endpoint.
//
// Architecture: one shared ingress-nginx LoadBalancer (or NodePort on
// bare metal) fronts every instance, and a WILDCARD DNS record
// (*.teepin.io -> node IP) resolves all instance hostnames to it.
// Per-instance routing is by hostname through an Ingress; each instance
// gets a ClusterIP Service, never its own LoadBalancer.
//
// The alternative — a LoadBalancer per instance — needs one routable IP
// per customer, which a single bare-metal node cannot provide; without
// MetalLB such a Service never gets an address and instance creation
// stalls waiting for one. The wildcard approach serves unlimited
// instances from a single IP and survives moving to new hardware with
// one DNS change.
package networking

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"k8s.io/client-go/kubernetes"
)

// Service handles networking operations for TEEPIN instances
type Service struct {
	k8sClient         kubernetes.Interface
	domain            string // Base domain for instances (e.g. "teepin.io")
	instanceNamespace string // Namespace customer instances run in
	useTLS            bool   // Whether to provision SSL certificates
	tlsIssuer         string // cert-manager ClusterIssuer name
}

// Config holds networking service configuration
type Config struct {
	Domain    string // Base domain (e.g. "teepin.io")
	Namespace string // Namespace customer instances run in (e.g. "default")
	UseTLS    bool   // Enable SSL certificate provisioning
	TLSIssuer string // cert-manager ClusterIssuer (e.g. "letsencrypt-prod")
}

// EndpointInfo contains networking details for an instance
type EndpointInfo struct {
	InstanceID   uuid.UUID
	ServiceName  string
	IngressName  string
	PublicIP     string
	DNSName      string
	HTTPURL      string
	HTTPSURL     string
	TLSEnabled   bool
	TLSReady     bool
	InternalPort int32
	ExternalPort int32
}

// NewService creates a new networking service
func NewService(k8sClient kubernetes.Interface, config Config) *Service {
	return &Service{
		k8sClient:         k8sClient,
		domain:            config.Domain,
		instanceNamespace: config.Namespace,
		useTLS:            config.UseTLS,
		tlsIssuer:         config.TLSIssuer,
	}
}

// ProvisionEndpoint gives an instance a public hostname: a ClusterIP
// Service selecting its pod, plus an Ingress routing
// <instance-name>.<domain> to it (with TLS when enabled).
//
// instanceName is the TEEPIN instance ID (e.g. "inst-6fea56ce") — it
// must match the app.teepin.cloud/instance-id label on the pod, and it
// forms the hostname the customer receives.
func (s *Service) ProvisionEndpoint(ctx context.Context, instanceID uuid.UUID, instanceName string, port int32) (*EndpointInfo, error) {
	dnsName := s.generateDNSNameFor(instanceName)

	serviceName, err := s.createInstanceService(ctx, instanceID, instanceName, port)
	if err != nil {
		return nil, fmt.Errorf("failed to create Service: %w", err)
	}

	ingressName, err := s.createIngress(ctx, instanceID, dnsName, serviceName, port)
	if err != nil {
		// Cleanup: delete service if ingress creation fails
		_ = s.deleteInstanceService(ctx, serviceName)
		return nil, fmt.Errorf("failed to create Ingress: %w", err)
	}

	// The shared ingress address — resolved immediately, with no
	// per-instance IP allocation to wait for.
	publicIP := s.ingressPublicIP(ctx)

	scheme := "http"
	if s.useTLS {
		scheme = "https"
	}

	return &EndpointInfo{
		InstanceID:   instanceID,
		ServiceName:  serviceName,
		IngressName:  ingressName,
		PublicIP:     publicIP,
		DNSName:      dnsName,
		HTTPURL:      fmt.Sprintf("http://%s", dnsName),
		HTTPSURL:     fmt.Sprintf("%s://%s", scheme, dnsName),
		TLSEnabled:   s.useTLS,
		TLSReady:     false, // cert-manager needs time to issue
		InternalPort: port,
		ExternalPort: 443,
	}, nil
}

// RevokeEndpoint deletes the Service and Ingress for an instance.
func (s *Service) RevokeEndpoint(ctx context.Context, instanceID uuid.UUID) error {
	serviceName := s.generateServiceName(instanceID)
	ingressName := s.generateIngressName(instanceID)

	if err := s.deleteIngress(ctx, ingressName); err != nil {
		return fmt.Errorf("failed to delete Ingress: %w", err)
	}

	if err := s.deleteInstanceService(ctx, serviceName); err != nil {
		return fmt.Errorf("failed to delete Service: %w", err)
	}

	return nil
}

// GetEndpointInfo retrieves current networking information for an instance.
func (s *Service) GetEndpointInfo(ctx context.Context, instanceID uuid.UUID, instanceName string) (*EndpointInfo, error) {
	serviceName := s.generateServiceName(instanceID)
	ingressName := s.generateIngressName(instanceID)
	dnsName := s.generateDNSNameFor(instanceName)

	port, err := s.getServicePort(ctx, serviceName)
	if err != nil {
		port = 0
	}

	tlsReady, err := s.isTLSReady(ctx, ingressName)
	if err != nil {
		tlsReady = false
	}

	scheme := "http"
	if s.useTLS {
		scheme = "https"
	}

	return &EndpointInfo{
		InstanceID:   instanceID,
		ServiceName:  serviceName,
		IngressName:  ingressName,
		PublicIP:     s.ingressPublicIP(ctx),
		DNSName:      dnsName,
		HTTPURL:      fmt.Sprintf("http://%s", dnsName),
		HTTPSURL:     fmt.Sprintf("%s://%s", scheme, dnsName),
		TLSEnabled:   s.useTLS,
		TLSReady:     tlsReady,
		InternalPort: port,
		ExternalPort: 443,
	}, nil
}

// generateServiceName creates a Kubernetes Service name for an instance
func (s *Service) generateServiceName(instanceID uuid.UUID) string {
	return fmt.Sprintf("inst-%s-svc", instanceID.String()[:8])
}

// generateIngressName creates a Kubernetes Ingress name for an instance
func (s *Service) generateIngressName(instanceID uuid.UUID) string {
	return fmt.Sprintf("inst-%s-ingress", instanceID.String()[:8])
}

// generateDNSNameFor builds the customer-facing hostname from the
// TEEPIN instance ID, e.g. "inst-6fea56ce.teepin.io". Covered by the
// wildcard record, so no DNS API call is needed.
func (s *Service) generateDNSNameFor(instanceName string) string {
	return fmt.Sprintf("%s.%s", instanceName, s.domain)
}
