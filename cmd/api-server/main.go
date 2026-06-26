// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/teepin/teepin-core/pkg/api"
	"github.com/teepin/teepin-core/pkg/gpu"
)

const (
	version = "0.1.0"
)

func main() {
	log.Printf("Starting TEEPIN API Server v%s\n", version)

	// Initialize Kubernetes client (optional for standalone mode)
	k8sClient, err := initKubernetesClient()
	if err != nil {
		log.Printf("⚠️  Kubernetes client not available: %v", err)
		log.Println("⚠️  Running in STANDALONE mode (API endpoints work, instance creation disabled)")
		log.Println("⚠️  To enable full functionality, set up a Kubernetes cluster and ensure ~/.kube/config exists")
	} else {
		log.Println("✅ Connected to Kubernetes cluster")
	}

	// Initialize GPU allocator
	gpuAllocator := gpu.NewAllocator(k8sClient)
	log.Println("✅ GPU allocator initialized")

	// Initialize API server
	apiServer := api.NewServer(k8sClient, gpuAllocator)

	// Setup router
	router := setupRouter(apiServer)

	// Create HTTP server
	srv := &http.Server{
		Addr:    ":8080",
		Handler: router,
	}

	// Start server in goroutine
	go func() {
		log.Printf("🚀 API server listening on %s\n", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Failed to start server: %v", err)
		}
	}()

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")

	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}

	log.Println("Server exited")
}

func initKubernetesClient() (*kubernetes.Clientset, error) {
	var config *rest.Config
	var err error

	// Try in-cluster config first
	config, err = rest.InClusterConfig()
	if err != nil {
		// Fall back to kubeconfig
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			kubeconfig = os.Getenv("HOME") + "/.kube/config"
		}

		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("failed to load kubeconfig: %w", err)
		}
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	return clientset, nil
}

func setupRouter(apiServer *api.Server) *gin.Engine {
	// Set Gin to release mode in production
	if os.Getenv("GIN_MODE") == "" {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.Default()

	// Middleware
	router.Use(gin.Recovery())
	router.Use(corsMiddleware())
	router.Use(requestIDMiddleware())

	// Health checks
	router.GET("/health", healthHandler)
	router.GET("/version", versionHandler)

	// API v1
	v1 := router.Group("/v1")
	{
		// Compute endpoints
		compute := v1.Group("/compute")
		{
			compute.GET("/instance-types", apiServer.ListInstanceTypes)
			compute.POST("/instances", apiServer.CreateInstance)
			compute.GET("/instances", apiServer.ListInstances)
			compute.GET("/instances/:id", apiServer.GetInstance)
			compute.DELETE("/instances/:id", apiServer.DeleteInstance)
			compute.GET("/instances/:id/logs", apiServer.GetInstanceLogs)
			compute.GET("/instances/:id/metrics", apiServer.GetInstanceMetrics)
		}

		// SDL deployment endpoint
		deployments := v1.Group("/deployments")
		{
			deployments.POST("/sdl", apiServer.DeploySDL)
		}
	}

	return router
}

func healthHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status": "healthy",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

func versionHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"version": version,
		"api_version": "v1",
	})
}

func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, accept, origin, Cache-Control, X-Requested-With")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS, GET, PUT, DELETE, PATCH")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}

		c.Next()
	}
}

func requestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := c.GetHeader("X-Request-ID")
		if requestID == "" {
			requestID = fmt.Sprintf("%d", time.Now().UnixNano())
		}
		c.Set("request_id", requestID)
		c.Writer.Header().Set("X-Request-ID", requestID)
		c.Next()
	}
}
