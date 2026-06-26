// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/teepin/teepin-core/pkg/gpu"
	"github.com/teepin/teepin-core/pkg/models"
)

// Server represents the API server
type Server struct {
	k8sClient    *kubernetes.Clientset
	gpuAllocator *gpu.Allocator
}

// NewServer creates a new API server
func NewServer(k8sClient *kubernetes.Clientset, gpuAllocator *gpu.Allocator) *Server {
	return &Server{
		k8sClient:    k8sClient,
		gpuAllocator: gpuAllocator,
	}
}

// ListInstanceTypes returns available instance types
func (s *Server) ListInstanceTypes(c *gin.Context) {
	profiles := s.gpuAllocator.GetAvailableProfiles()

	instanceTypes := make([]models.InstanceType, 0, len(profiles))
	for _, profile := range profiles {
		instanceTypes = append(instanceTypes, models.InstanceType{
			Name:         fmt.Sprintf("gpu.h100.%s", profile.Name),
			GPUVRAM:      fmt.Sprintf("%dGB", profile.MemoryGB),
			GPUMemoryGB:  profile.MemoryGB,
			CPUUnits:     8,  // Default
			Memory:       "32GB",  // Default
			PricePerHour: profile.Price,
			Description:  fmt.Sprintf("H100 GPU with %dGB VRAM (MIG %s)", profile.MemoryGB, profile.Name),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"instance_types": instanceTypes,
	})
}

// CreateInstance creates a new compute instance
func (s *Server) CreateInstance(c *gin.Context) {
	var req models.CreateInstanceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Generate instance ID
	instanceID := fmt.Sprintf("inst-%s", uuid.New().String()[:8])

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
	pod, err := s.createPod(c.Request.Context(), instanceID, &req, allocation)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to create pod: %v", err)})
		return
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
	}

	// In production, endpoint would be LoadBalancer IP
	instance.Endpoint = fmt.Sprintf("https://%s.teepin.cloud", instanceID)

	c.JSON(http.StatusCreated, instance)
}

// ListInstances lists all instances
func (s *Server) ListInstances(c *gin.Context) {
	// In MVP, list all pods in default namespace
	pods, err := s.k8sClient.CoreV1().Pods("default").List(c.Request.Context(), metav1.ListOptions{
		LabelSelector: "app.teepin.cloud/managed=true",
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

// GetInstance gets details of a specific instance
func (s *Server) GetInstance(c *gin.Context) {
	instanceID := c.Param("id")

	// Find pod by ID
	pods, err := s.k8sClient.CoreV1().Pods("default").List(c.Request.Context(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app.teepin.cloud/instance-id=%s", instanceID),
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

// DeleteInstance deletes an instance
func (s *Server) DeleteInstance(c *gin.Context) {
	instanceID := c.Param("id")

	// Delete pod
	err := s.k8sClient.CoreV1().Pods("default").DeleteCollection(c.Request.Context(), metav1.DeleteOptions{}, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app.teepin.cloud/instance-id=%s", instanceID),
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "instance deleted",
		"id":      instanceID,
	})
}

// GetInstanceLogs gets logs from an instance
func (s *Server) GetInstanceLogs(c *gin.Context) {
	instanceID := c.Param("id")

	// Find pod
	pods, err := s.k8sClient.CoreV1().Pods("default").List(c.Request.Context(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app.teepin.cloud/instance-id=%s", instanceID),
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if len(pods.Items) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "instance not found"})
		return
	}

	// TODO: Stream logs
	c.JSON(http.StatusOK, gin.H{
		"message": "Logs endpoint - to be implemented with streaming",
		"pod":     pods.Items[0].Name,
	})
}

// GetInstanceMetrics gets metrics from an instance
func (s *Server) GetInstanceMetrics(c *gin.Context) {
	instanceID := c.Param("id")

	// TODO: Query Prometheus for metrics
	c.JSON(http.StatusOK, gin.H{
		"instance_id": instanceID,
		"metrics": gin.H{
			"gpu_utilization": 85,
			"gpu_memory_used": "18.5GB",
			"cpu_utilization": 45,
			"memory_used":     "24GB",
		},
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

func (s *Server) createPod(ctx context.Context, instanceID string, req *models.CreateInstanceRequest, allocation *gpu.Allocation) (*corev1.Pod, error) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", req.Name, uuid.New().String()[:5]),
			Namespace: "default",
			Labels: map[string]string{
				"app.teepin.cloud/managed":     "true",
				"app.teepin.cloud/instance-id": instanceID,
				"app.teepin.cloud/name":        req.Name,
			},
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
		// In production, this would be the actual MIG device
		// For local dev, we just label the node selector
		pod.Spec.NodeSelector = map[string]string{
			"gpu-type": "h100-simulated",
		}

		// Simulated GPU resource request
		pod.Spec.Containers[0].Resources.Limits["nvidia.com/gpu"] = resource.MustParse("1")
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
	instanceID := pod.Labels["app.teepin.cloud/instance-id"]
	name := pod.Labels["app.teepin.cloud/name"]

	return models.Instance{
		ID:         instanceID,
		Name:       name,
		Status:     string(pod.Status.Phase),
		CreatedAt:  pod.CreationTimestamp.Time,
		UpdatedAt:  pod.CreationTimestamp.Time,
		InternalIP: pod.Status.PodIP,
	}
}

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
