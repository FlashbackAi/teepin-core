// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package networking

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"k8s.io/client-go/kubernetes"
)

// Service handles networking operations for TEEPIN instances
type Service struct {
	k8sClient  kubernetes.Interface
	domain     string // Base domain for instances (e.g., "teepin.cloud")
	namespace  string // Kubernetes namespace for instances
	useTLS     bool   // Whether to provision SSL certificates
	tlsIssuer  string // cert-manager ClusterIssuer name
}

// Config holds networking service configuration
type Config struct {
	Domain    string // Base domain (e.g., "teepin.cloud")
	Namespace string // Kubernetes namespace (e.g., "teepin")
	UseTLS    bool   // Enable SSL certificate provisioning
	TLSIssuer string // cert-manager ClusterIssuer (e.g., "letsencrypt-prod")
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
		k8sClient:  k8sClient,
		domain:     config.Domain,
		namespace:  config.Namespace,
		useTLS:     config.UseTLS,
		tlsIssuer:  config.TLSIssuer,
	}
}

// ProvisionEndpoint creates a LoadBalancer Service and Ingress for an instance
func (s *Service) ProvisionEndpoint(ctx context.Context, instanceID uuid.UUID, port int32) (*EndpointInfo, error) {
	// Generate DNS name for instance
	dnsName := s.generateDNSName(instanceID)

	// Create LoadBalancer Service
	serviceName, err := s.createLoadBalancerService(ctx, instanceID, port)
	if err != nil {
		return nil, fmt.Errorf("failed to create LoadBalancer service: %w", err)
	}

	// Create Ingress with TLS
	ingressName, err := s.createIngress(ctx, instanceID, dnsName, serviceName, port)
	if err != nil {
		// Cleanup: delete service if ingress creation fails
		_ = s.deleteLoadBalancerService(ctx, serviceName)
		return nil, fmt.Errorf("failed to create Ingress: %w", err)
	}

	// Get LoadBalancer IP (may take a few seconds to provision)
	publicIP, err := s.getLoadBalancerIP(ctx, serviceName)
	if err != nil {
		// Note: This is not a fatal error - IP may not be assigned yet
		publicIP = "<pending>"
	}

	return &EndpointInfo{
		InstanceID:   instanceID,
		ServiceName:  serviceName,
		IngressName:  ingressName,
		PublicIP:     publicIP,
		DNSName:      dnsName,
		HTTPURL:      fmt.Sprintf("http://%s", dnsName),
		HTTPSURL:     fmt.Sprintf("https://%s", dnsName),
		TLSEnabled:   s.useTLS,
		TLSReady:     false, // cert-manager needs time to provision
		InternalPort: port,
		ExternalPort: 443, // Ingress terminates TLS on 443
	}, nil
}

// RevokeEndpoint deletes the LoadBalancer Service and Ingress for an instance
func (s *Service) RevokeEndpoint(ctx context.Context, instanceID uuid.UUID) error {
	serviceName := s.generateServiceName(instanceID)
	ingressName := s.generateIngressName(instanceID)

	// Delete Ingress
	if err := s.deleteIngress(ctx, ingressName); err != nil {
		return fmt.Errorf("failed to delete Ingress: %w", err)
	}

	// Delete LoadBalancer Service
	if err := s.deleteLoadBalancerService(ctx, serviceName); err != nil {
		return fmt.Errorf("failed to delete LoadBalancer service: %w", err)
	}

	return nil
}

// GetEndpointInfo retrieves current networking information for an instance
func (s *Service) GetEndpointInfo(ctx context.Context, instanceID uuid.UUID) (*EndpointInfo, error) {
	serviceName := s.generateServiceName(instanceID)
	ingressName := s.generateIngressName(instanceID)
	dnsName := s.generateDNSName(instanceID)

	// Get LoadBalancer IP
	publicIP, err := s.getLoadBalancerIP(ctx, serviceName)
	if err != nil {
		publicIP = "<not available>"
	}

	// Get service port
	port, err := s.getServicePort(ctx, serviceName)
	if err != nil {
		port = 0
	}

	// Check TLS certificate status
	tlsReady, err := s.isTLSReady(ctx, ingressName)
	if err != nil {
		tlsReady = false
	}

	return &EndpointInfo{
		InstanceID:   instanceID,
		ServiceName:  serviceName,
		IngressName:  ingressName,
		PublicIP:     publicIP,
		DNSName:      dnsName,
		HTTPURL:      fmt.Sprintf("http://%s", dnsName),
		HTTPSURL:     fmt.Sprintf("https://%s", dnsName),
		TLSEnabled:   s.useTLS,
		TLSReady:     tlsReady,
		InternalPort: port,
		ExternalPort: 443,
	}, nil
}

// generateServiceName creates a Kubernetes Service name for an instance
func (s *Service) generateServiceName(instanceID uuid.UUID) string {
	return fmt.Sprintf("inst-%s-lb", instanceID.String()[:8])
}

// generateIngressName creates a Kubernetes Ingress name for an instance
func (s *Service) generateIngressName(instanceID uuid.UUID) string {
	return fmt.Sprintf("inst-%s-ingress", instanceID.String()[:8])
}

// generateDNSName creates a DNS name for an instance
func (s *Service) generateDNSName(instanceID uuid.UUID) string {
	return fmt.Sprintf("inst-%s.%s", instanceID.String()[:8], s.domain)
}
