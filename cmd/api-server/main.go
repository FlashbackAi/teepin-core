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
	"strconv"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/FlashbackAi/teepin-core/pkg/api"
	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/FlashbackAi/teepin-core/pkg/billing"
	"github.com/FlashbackAi/teepin-core/pkg/database"
	"github.com/FlashbackAi/teepin-core/pkg/gpu"
)

const (
	version = "0.1.0"
)

func main() {
	log.Printf("Starting TEEPIN API Server v%s\n", version)

	// Initialize database client
	dbClient, err := initDatabaseClient()
	if err != nil {
		log.Printf("⚠️  Database client not available: %v", err)
		log.Println("⚠️  Authentication and persistence disabled")
	} else {
		log.Println("✅ Connected to PostgreSQL database")
	}

	// Initialize auth service
	jwtSecret := getEnv("JWT_SECRET", "change_me_in_production_super_secret_key_12345")
	var authService *auth.Service
	var authHandler *api.AuthHandler
	var authMiddleware *auth.Middleware

	if dbClient != nil {
		authService = auth.NewService(dbClient.DB(), jwtSecret)
		authHandler = api.NewAuthHandler(authService)
		authMiddleware = auth.NewMiddleware(authService, jwtSecret)
		log.Println("✅ Authentication system initialized")
	}

	// Initialize billing service
	var billingService *billing.Service
	var billingHandler *api.BillingHandler
	var usageCollector *billing.UsageCollector

	if dbClient != nil && authService != nil {
		billingService = billing.NewService(dbClient.DB())
		billingHandler = api.NewBillingHandler(billingService, authService)
		usageCollector = billing.NewUsageCollector(dbClient.DB(), billingService)
		log.Println("✅ Billing system initialized")

		// Start usage collector in background
		go usageCollector.Start(context.Background())
	}

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
	router := setupRouter(apiServer, authHandler, authMiddleware, billingHandler)

	// Create HTTP server
	port := getEnv("PORT", "8080")
	srv := &http.Server{
		Addr:    ":" + port,
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

	// Close database connection
	if dbClient != nil {
		dbClient.Close()
	}

	log.Println("Server exited")
}

func initDatabaseClient() (*database.Client, error) {
	// Get database config from environment
	host := getEnv("DB_HOST", "postgres.teepin.svc.cluster.local")
	portStr := getEnv("DB_PORT", "5432")
	user := getEnv("DB_USER", "teepin")
	password := getEnv("DB_PASSWORD", "teepin_local_password_change_in_prod")
	dbname := getEnv("DB_NAME", "teepin_db")
	sslmode := getEnv("DB_SSLMODE", "disable")

	port, err := strconv.Atoi(portStr)
	if err != nil {
		port = 5432
	}

	cfg := database.Config{
		Host:     host,
		Port:     port,
		User:     user,
		Password: password,
		DBName:   dbname,
		SSLMode:  sslmode,
	}

	client, err := database.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	return client, nil
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

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func setupRouter(apiServer *api.Server, authHandler *api.AuthHandler, authMiddleware *auth.Middleware, billingHandler *api.BillingHandler) *gin.Engine {
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
		// Authentication endpoints (public)
		if authHandler != nil {
			authRoutes := v1.Group("/auth")
			{
				authRoutes.POST("/register", authHandler.Register)
				authRoutes.POST("/login", authHandler.Login)
				authRoutes.GET("/me", authMiddleware.RequireAuth(), authHandler.GetCurrentUser)
			}

			// Project endpoints (require auth)
			projects := v1.Group("/projects")
			projects.Use(authMiddleware.RequireAuth())
			{
				projects.POST("", authHandler.CreateProject)
				projects.GET("", authHandler.ListProjects)
				projects.GET("/:id", authHandler.GetProject)
				projects.POST("/:id/api-keys", authHandler.CreateAPIKey)
				projects.GET("/:id/api-keys", authHandler.ListAPIKeys)
				projects.DELETE("/:id/api-keys/:key_id", authHandler.RevokeAPIKey)
			}
		}

		// Billing endpoints (require auth)
		if billingHandler != nil && authMiddleware != nil {
			billing := v1.Group("/billing")
			billing.Use(authMiddleware.RequireAuth())
			{
				billing.GET("/usage", billingHandler.GetUsageSummary)
				billing.GET("/usage/records", billingHandler.GetUsageRecords)
				billing.GET("/current-month", billingHandler.GetCurrentMonthUsage)
				billing.GET("/invoices", billingHandler.ListInvoices)
				billing.GET("/invoices/:id", billingHandler.GetInvoice)
				billing.POST("/invoices", billingHandler.CreateInvoice)
			}
		}

		// Compute endpoints (optional auth for now, will be required later)
		compute := v1.Group("/compute")
		if authMiddleware != nil {
			compute.Use(authMiddleware.OptionalAuth())
		}
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
		if authMiddleware != nil {
			deployments.Use(authMiddleware.OptionalAuth())
		}
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
