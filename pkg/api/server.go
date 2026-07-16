// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/FlashbackAi/teepin-core/pkg/compute"
	"github.com/FlashbackAi/teepin-core/pkg/gpu"
	"github.com/FlashbackAi/teepin-core/pkg/models"
	"github.com/FlashbackAi/teepin-core/pkg/networking"
)

// Server represents the API server
type Server struct {
	k8sClient         kubernetes.Interface
	gpuAllocator      *gpu.Allocator
	networkingService *networking.Service
	store             *compute.Store // nil in standalone mode (no database)
}

// NewServer creates a new API server. store may be nil when the
// platform runs without a database (local standalone mode); in that
// case instances are not persisted and not billed.
func NewServer(k8sClient kubernetes.Interface, gpuAllocator *gpu.Allocator, networkingService *networking.Service, store *compute.Store) *Server {
	return &Server{
		k8sClient:         k8sClient,
		gpuAllocator:      gpuAllocator,
		networkingService: networkingService,
		store:             store,
	}
}

// ListInstanceTypes returns available instance types derived from the
// cluster's live GPU inventory. Custom VRAM sizes are always available
// via the gpu_vram request field and are not enumerated here.
func (s *Server) ListInstanceTypes(c *gin.Context) {
	types, err := s.gpuAllocator.AvailableInstanceTypes(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": fmt.Sprintf("GPU discovery failed: %v", err)})
		return
	}

	instanceTypes := make([]models.InstanceType, 0, len(types))
	for _, t := range types {
		isolation := "shared GPU, exact VRAM accounting"
		if t.Isolation == gpu.AllocationMIG {
			isolation = "MIG hardware isolation"
		}
		instanceTypes = append(instanceTypes, models.InstanceType{
			Name:         t.Name,
			GPUVRAM:      fmt.Sprintf("%dGB", t.MemoryGB),
			GPUMemoryGB:  t.MemoryGB,
			CPUUnits:     8,      // Default
			Memory:       "32GB", // Default
			PricePerHour: t.PricePerHour,
			Description:  fmt.Sprintf("%s GPU with %dGB VRAM (%s)", strings.ToUpper(t.GPUModel), t.MemoryGB, isolation),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"instance_types": instanceTypes,
		"pricing":        fmt.Sprintf("$%.2f per GB-hour, exact allocation (custom sizes supported via gpu_vram)", gpu.PricePerGBHour),
	})
}

// CreateInstance creates a new compute instance
func (s *Server) CreateInstance(c *gin.Context) {
	var req models.CreateInstanceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Billing integrity: when persistence is enabled, every instance
	// must belong to a project so its usage can be metered. Refuse to
	// run unbilled workloads.
	projectID, ok := s.requireProjectScope(c)
	if !ok {
		return
	}
	userID, _ := auth.GetUserID(c)

	// Generate instance UUID
	instanceUUID := uuid.New()
	instanceID := fmt.Sprintf("inst-%s", instanceUUID.String()[:8])

	// Parse VRAM requirement
	var vramGB int
	var err error
	if req.GPUVRAM != "" {
		vramGB, err = gpu.ParseVRAM(req.GPUVRAM)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid gpu_vram: %v", err)})
			return
		}
	}

	// Allocate GPU if VRAM specified
	var allocation *gpu.Allocation
	if vramGB > 0 {
		allocation, err = s.gpuAllocator.AllocateByVRAM(c.Request.Context(), vramGB)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": fmt.Sprintf("GPU allocation failed: %v", err)})
			return
		}
	}

	// Create Kubernetes pod
	pod, err := s.createPod(c.Request.Context(), instanceID, instanceUUID, projectID, &req, allocation)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to create pod: %v", err)})
		return
	}

	// Provision networking endpoint (LoadBalancer + Ingress + TLS)
	var endpointInfo *networking.EndpointInfo
	if s.networkingService != nil && len(req.Ports) > 0 {
		// Use first exposed port for LoadBalancer
		exposedPort := int32(req.Ports[0].Container)

		endpointInfo, err = s.networkingService.ProvisionEndpoint(c.Request.Context(), instanceUUID, exposedPort)
		if err != nil {
			// Cleanup: delete pod if networking provisioning fails
			_ = s.k8sClient.CoreV1().Pods("default").Delete(c.Request.Context(), pod.Name, metav1.DeleteOptions{})
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to provision endpoint: %v", err)})
			return
		}
	}

	// Persist the instance — this is what billing meters. If it fails
	// the workload must not keep running unbilled: roll back.
	if s.store != nil {
		record := &compute.InstanceRecord{
			ID:           instanceID,
			ProjectID:    projectID,
			UserID:       userID,
			Name:         req.Name,
			Image:        req.Image,
			Status:       compute.StatusPending,
			CPUUnits:     req.CPUUnits,
			MemoryGB:     parseMemoryGB(req.Memory),
			K8sPodName:   pod.Name,
			K8sNamespace: pod.Namespace,
		}
		if allocation != nil {
			record.InstanceType = allocation.InstanceType
			record.GPUVRAMGB = allocation.AllocatedVRAM
		}
		if endpointInfo != nil {
			record.Endpoint = endpointInfo.HTTPSURL
		}

		if err := s.store.Create(c.Request.Context(), record); err != nil {
			if s.networkingService != nil && endpointInfo != nil {
				_ = s.networkingService.RevokeEndpoint(c.Request.Context(), instanceUUID)
			}
			_ = s.k8sClient.CoreV1().Pods(pod.Namespace).Delete(c.Request.Context(), pod.Name, metav1.DeleteOptions{})
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to persist instance: %v", err)})
			return
		}
	}

	// Build response
	instance := models.Instance{
		ID:        instanceID,
		Name:      req.Name,
		Image:     req.Image,
		Status:    string(pod.Status.Phase),
		CPUUnits:  req.CPUUnits,
		Memory:    req.Memory,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
		Labels:    req.Labels,
	}

	if allocation != nil {
		instance.GPUVRAM = fmt.Sprintf("%dGB", allocation.RequestedVRAM)
		instance.AllocatedVRAM = fmt.Sprintf("%dGB", allocation.AllocatedVRAM)
		instance.InstanceType = allocation.InstanceType
		instance.PricePerHour = gpu.GetPriceForVRAM(allocation.AllocatedVRAM)
		if allocation.AllocatedVRAM > allocation.RequestedVRAM {
			instance.AllocationNote = fmt.Sprintf(
				"requested %dGB; allocated %dGB — the smallest isolation unit that fits (billed for %dGB at $%.2f/hr). Exact custom sizes arrive with software VRAM partitioning.",
				allocation.RequestedVRAM, allocation.AllocatedVRAM,
				allocation.AllocatedVRAM, instance.PricePerHour)
		}
	}

	// Add endpoint information
	if endpointInfo != nil {
		instance.Endpoint = endpointInfo.HTTPSURL
		instance.PublicIP = endpointInfo.PublicIP
		instance.DNSName = endpointInfo.DNSName
		instance.TLSEnabled = endpointInfo.TLSEnabled
		instance.TLSReady = endpointInfo.TLSReady
	}

	c.JSON(http.StatusCreated, instance)
}

// ListInstances lists the caller's instances. With tenancy active the
// result is scoped to the caller's project — never other tenants'.
func (s *Server) ListInstances(c *gin.Context) {
	projectID, ok := s.requireProjectScope(c)
	if !ok {
		return
	}

	selector := "app.teepin.cloud/managed=true"
	if projectID != uuid.Nil {
		selector += fmt.Sprintf(",%s=%s", LabelProjectID, projectID)
	}

	pods, err := s.k8sClient.CoreV1().Pods("default").List(c.Request.Context(), metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	instances := make([]models.Instance, 0, len(pods.Items))
	for _, pod := range pods.Items {
		instance := podToInstance(&pod)
		instances = append(instances, instance)
	}

	c.JSON(http.StatusOK, gin.H{
		"instances": instances,
		"count":     len(instances),
	})
}

// GetInstance gets details of a specific instance. Another tenant's
// instance is indistinguishable from a nonexistent one (404).
func (s *Server) GetInstance(c *gin.Context) {
	instanceID := c.Param("id")

	projectID, ok := s.requireProjectScope(c)
	if !ok {
		return
	}

	pods, err := s.k8sClient.CoreV1().Pods("default").List(c.Request.Context(), metav1.ListOptions{
		LabelSelector: instanceSelector(instanceID, projectID),
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if len(pods.Items) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "instance not found"})
		return
	}

	instance := podToInstance(&pods.Items[0])
	c.JSON(http.StatusOK, instance)
}

// DeleteInstance deletes an instance. Scoped to the caller's project:
// another tenant's instance is a 404, and deleting a nonexistent
// instance is a 404 (not a silent success).
func (s *Server) DeleteInstance(c *gin.Context) {
	instanceID := c.Param("id")

	projectID, ok := s.requireProjectScope(c)
	if !ok {
		return
	}
	selector := instanceSelector(instanceID, projectID)

	pods, err := s.k8sClient.CoreV1().Pods("default").List(c.Request.Context(), metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if len(pods.Items) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "instance not found"})
		return
	}

	// Revoke networking endpoint (LoadBalancer + Ingress) using the
	// full instance UUID carried on the pod.
	if s.networkingService != nil {
		if fullUUID, exists := pods.Items[0].Labels["teepin.io/instance-uuid"]; exists {
			if instanceUUID, err := uuid.Parse(fullUUID); err == nil {
				if err := s.networkingService.RevokeEndpoint(c.Request.Context(), instanceUUID); err != nil {
					// Log error but continue with pod deletion
					c.Header("X-Warning", fmt.Sprintf("Failed to cleanup networking: %v", err))
				}
			}
		}
	}

	// Delete pod(s)
	err = s.k8sClient.CoreV1().Pods("default").DeleteCollection(c.Request.Context(), metav1.DeleteOptions{}, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Stop billing: stamp terminated_at. Idempotent; the reconciler
	// would also catch this within a minute if it failed here.
	if s.store != nil {
		if err := s.store.MarkTerminated(c.Request.Context(), instanceID); err != nil {
			c.Header("X-Warning", fmt.Sprintf("failed to finalize billing record: %v", err))
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "instance deleted",
		"id":      instanceID,
	})
}

// GetInstanceLogs gets logs from an instance
func (s *Server) GetInstanceLogs(c *gin.Context) {
	instanceID := c.Param("id")

	projectID, ok := s.requireProjectScope(c)
	if !ok {
		return
	}

	pods, err := s.k8sClient.CoreV1().Pods("default").List(c.Request.Context(), metav1.ListOptions{
		LabelSelector: instanceSelector(instanceID, projectID),
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if len(pods.Items) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "instance not found"})
		return
	}
	pod := &pods.Items[0]

	// Tail size: ?tail=N (default 100, capped to keep responses sane).
	tail := int64(100)
	if t, err := strconv.ParseInt(c.Query("tail"), 10, 64); err == nil && t > 0 && t <= 10000 {
		tail = t
	}

	opts := &corev1.PodLogOptions{
		TailLines:  &tail,
		Timestamps: c.Query("timestamps") == "true",
	}
	raw, err := s.k8sClient.CoreV1().Pods(pod.Namespace).
		GetLogs(pod.Name, opts).Do(c.Request.Context()).Raw()
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("failed to fetch logs: %v", err)})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"instance_id": instanceID,
		"tail":        tail,
		"logs":        string(raw),
	})
}

// GetInstanceMetrics gets metrics from an instance.
// Not implemented yet: returning made-up numbers to customers is worse
// than admitting the gap. Real metrics arrive with the Prometheus/DCGM
// integration milestone.
func (s *Server) GetInstanceMetrics(c *gin.Context) {
	instanceID := c.Param("id")

	projectID, ok := s.requireProjectScope(c)
	if !ok {
		return
	}

	pods, err := s.k8sClient.CoreV1().Pods("default").List(c.Request.Context(), metav1.ListOptions{
		LabelSelector: instanceSelector(instanceID, projectID),
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if len(pods.Items) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "instance not found"})
		return
	}

	c.JSON(http.StatusNotImplemented, gin.H{
		"error":       "instance metrics are not available yet (Prometheus/DCGM integration is planned before GA)",
		"instance_id": instanceID,
	})
}

// DeploySDL deploys from SDL template
func (s *Server) DeploySDL(c *gin.Context) {
	// TODO: Implement SDL parser
	c.JSON(http.StatusNotImplemented, gin.H{
		"message": "SDL deployment to be implemented in next iteration",
	})
}

// Helper functions

// LabelProjectID scopes a pod to the owning TEEPIN project; every
// tenant-facing read/delete filters on it.
const LabelProjectID = "teepin.io/project-id"

// annotationInstanceType records the hardware-derived instance type on
// the pod so reads can report it without a database lookup.
const annotationInstanceType = "teepin.io/instance-type"

// requireProjectScope returns the caller's project ID. When
// persistence is enabled every compute operation must be scoped to a
// project (billing + tenant isolation); unauthenticated calls get 401.
// In standalone mode (no database) there is no tenancy and uuid.Nil is
// returned with ok=true.
func (s *Server) requireProjectScope(c *gin.Context) (uuid.UUID, bool) {
	if s.store == nil {
		return uuid.Nil, true
	}
	projectID, ok := auth.GetProjectID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": "authentication with a project is required (use an API key: Authorization: Bearer tpk_...)",
		})
		return uuid.Nil, false
	}
	return projectID, true
}

// instanceSelector builds the label selector for one instance,
// restricted to the caller's project when tenancy is active.
func instanceSelector(instanceID string, projectID uuid.UUID) string {
	selector := fmt.Sprintf("app.teepin.cloud/instance-id=%s", instanceID)
	if projectID != uuid.Nil {
		selector += fmt.Sprintf(",%s=%s", LabelProjectID, projectID)
	}
	return selector
}

func (s *Server) createPod(ctx context.Context, instanceID string, instanceUUID, projectID uuid.UUID, req *models.CreateInstanceRequest, allocation *gpu.Allocation) (*corev1.Pod, error) {
	// Generate pod selector for networking
	podSelector := fmt.Sprintf("inst-%s", instanceID[5:]) // Remove "inst-" prefix

	labels := map[string]string{
		"app.teepin.cloud/managed":     "true",
		"app.teepin.cloud/instance-id": instanceID,
		"app.teepin.cloud/name":        req.Name,
		"teepin.io/instance":           podSelector,           // For LoadBalancer selector
		"teepin.io/instance-uuid":      instanceUUID.String(), // For cleanup
	}
	if projectID != uuid.Nil {
		labels[LabelProjectID] = projectID.String()
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", req.Name, uuid.New().String()[:5]),
			Namespace: "default",
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "app",
					Image: req.Image,
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse(fmt.Sprintf("%d", req.CPUUnits)),
							corev1.ResourceMemory: resource.MustParse(convertMemoryToK8sFormat(req.Memory)),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse(fmt.Sprintf("%d", req.CPUUnits)),
							corev1.ResourceMemory: resource.MustParse(convertMemoryToK8sFormat(req.Memory)),
						},
					},
				},
			},
		},
	}

	// Add GPU resources if allocated
	if allocation != nil {
		// VRAM annotations drive capacity accounting (inventory) and
		// billing reconciliation.
		pod.ObjectMeta.Annotations = map[string]string{
			gpu.AnnotationVRAMGB:      strconv.Itoa(allocation.AllocatedVRAM),
			gpu.AnnotationGPUResource: allocation.ResourceName,
			annotationInstanceType:    allocation.InstanceType,
		}

		// Pin to the node whose capacity the allocator accounted against.
		if allocation.NodeName != "" {
			pod.Spec.NodeSelector = map[string]string{
				"kubernetes.io/hostname": allocation.NodeName,
			}
		}

		// Request the allocated device: a MIG slice (e.g.
		// nvidia.com/mig-2g.20gb) or a shared GPU (nvidia.com/gpu).
		// Simulated allocations skip this — local Kind nodes expose no
		// GPU extended resources and the pod would be unschedulable.
		if !allocation.Simulated {
			pod.Spec.Containers[0].Resources.Limits[corev1.ResourceName(allocation.ResourceName)] =
				resource.MustParse(strconv.Itoa(allocation.Quantity))
		}
	}

	// Add environment variables
	if len(req.Env) > 0 {
		envVars := make([]corev1.EnvVar, 0, len(req.Env))
		for key, value := range req.Env {
			envVars = append(envVars, corev1.EnvVar{
				Name:  key,
				Value: value,
			})
		}
		pod.Spec.Containers[0].Env = envVars
	}

	// Add ports
	if len(req.Ports) > 0 {
		ports := make([]corev1.ContainerPort, 0, len(req.Ports))
		for _, port := range req.Ports {
			ports = append(ports, corev1.ContainerPort{
				ContainerPort: int32(port.Container),
				Protocol:      corev1.ProtocolTCP,
			})
		}
		pod.Spec.Containers[0].Ports = ports
	}

	// Create pod
	createdPod, err := s.k8sClient.CoreV1().Pods("default").Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}

	return createdPod, nil
}

func podToInstance(pod *corev1.Pod) models.Instance {
	instance := models.Instance{
		ID:         pod.Labels["app.teepin.cloud/instance-id"],
		Name:       pod.Labels["app.teepin.cloud/name"],
		Status:     string(pod.Status.Phase),
		CreatedAt:  pod.CreationTimestamp.Time,
		UpdatedAt:  pod.CreationTimestamp.Time,
		InternalIP: pod.Status.PodIP,
	}

	if len(pod.Spec.Containers) > 0 {
		instance.Image = pod.Spec.Containers[0].Image
	}

	// GPU details from the allocation annotations.
	if v, err := strconv.Atoi(pod.Annotations[gpu.AnnotationVRAMGB]); err == nil && v > 0 {
		instance.GPUVRAM = fmt.Sprintf("%dGB", v)
		instance.AllocatedVRAM = instance.GPUVRAM
		instance.PricePerHour = gpu.GetPriceForVRAM(v)
	}
	if t := pod.Annotations[annotationInstanceType]; t != "" {
		instance.InstanceType = t
	}

	return instance
}

// parseMemoryGB parses memory strings like "32GB" or "512MB" to whole
// GB (rounded up) for persistence. Unparseable input yields 0.
func parseMemoryGB(memory string) int {
	m := memoryRe.FindStringSubmatch(memory)
	if m == nil {
		return 0
	}
	value, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	if strings.EqualFold(m[2], "MB") || strings.EqualFold(m[2], "M") {
		value = (value + 1023) / 1024
	}
	return value
}

var memoryRe = regexp.MustCompile(`^(\d+)\s*([GgMm][Bb]?)$`)

// convertMemoryToK8sFormat converts memory strings like "16GB" to Kubernetes format "16Gi"
func convertMemoryToK8sFormat(memory string) string {
	// Parse common formats: 16GB, 32GB, etc.
	// Kubernetes expects Gi (gibibytes) not GB (gigabytes)
	var value string
	var unit string

	// Extract number and unit
	if len(memory) >= 3 {
		if memory[len(memory)-2:] == "GB" || memory[len(memory)-2:] == "gb" {
			value = memory[:len(memory)-2]
			unit = "Gi"
		} else if memory[len(memory)-2:] == "MB" || memory[len(memory)-2:] == "mb" {
			value = memory[:len(memory)-2]
			unit = "Mi"
		} else if memory[len(memory)-1:] == "G" || memory[len(memory)-1:] == "g" {
			value = memory[:len(memory)-1]
			unit = "Gi"
		} else if memory[len(memory)-1:] == "M" || memory[len(memory)-1:] == "m" {
			value = memory[:len(memory)-1]
			unit = "Mi"
		} else {
			// Already in Kubernetes format or unknown
			return memory
		}
	} else {
		return memory
	}

	return value + unit
}
