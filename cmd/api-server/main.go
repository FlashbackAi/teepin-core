// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	agentpb "github.com/FlashbackAi/teepin-core/pkg/agentpb"
	"github.com/FlashbackAi/teepin-core/pkg/api"
	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/FlashbackAi/teepin-core/pkg/billing"
	billingpdf "github.com/FlashbackAi/teepin-core/pkg/billing/pdf"
	"github.com/FlashbackAi/teepin-core/pkg/cluster"
	"github.com/FlashbackAi/teepin-core/pkg/compute"
	"github.com/FlashbackAi/teepin-core/pkg/database"
	"github.com/FlashbackAi/teepin-core/pkg/gpu"
	"github.com/FlashbackAi/teepin-core/pkg/harbor"
	"github.com/FlashbackAi/teepin-core/pkg/networking"
	"github.com/FlashbackAi/teepin-core/pkg/nodes"
	"github.com/FlashbackAi/teepin-core/pkg/payments"
	"github.com/FlashbackAi/teepin-core/pkg/ratelimit"
	"github.com/FlashbackAi/teepin-core/pkg/statuspage"
	s3storage "github.com/FlashbackAi/teepin-core/pkg/storage/s3"
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

		// Apply embedded schema migrations (idempotent). Disable with
		// TEEPIN_AUTO_MIGRATE=false when migrations are managed
		// externally.
		if getEnv("TEEPIN_AUTO_MIGRATE", "true") == "true" {
			if err := database.Migrate(dbClient.DB()); err != nil {
				log.Fatalf("❌ Database migration failed: %v", err)
			}
		}
	}

	// Initialize auth service.
	// Security: never run authentication on a default secret — when the
	// database (and therefore auth) is enabled, JWT_SECRET must be set.
	jwtSecret := os.Getenv("JWT_SECRET")
	if dbClient != nil && jwtSecret == "" {
		log.Fatal("❌ JWT_SECRET must be set when the database is enabled — refusing to start with a default secret")
	}
	var authService *auth.Service
	var authHandler *api.AuthHandler
	var accountHandler *api.AccountHandler
	var authMiddleware *auth.Middleware

	if dbClient != nil {
		authService = auth.NewService(dbClient.DB(), jwtSecret)
		authHandler = api.NewAuthHandler(authService)
		accountHandler = api.NewAccountHandler(authService)
		authMiddleware = auth.NewMiddleware(authService, jwtSecret)
		log.Println("✅ Authentication system initialized")
	}

	// Initialize billing service
	var billingService *billing.Service
	var billingHandler *api.BillingHandler
	var usageCollector *billing.UsageCollector
	var stripeClient *payments.Client

	if dbClient != nil && authService != nil {
		billingService = billing.NewService(dbClient.DB())

		// Enable invoice-PDF generation + storage when a bucket is
		// configured. Absent (local dev with no AWS), invoices still
		// issue — just without a stored document. Credentials come from
		// the AWS default chain (the ECS task role in production); no
		// static keys are read here.
		if bucket := os.Getenv("INVOICES_BUCKET"); bucket != "" {
			s3Client, err := s3storage.NewClient(context.Background(), bucket)
			if err != nil {
				// Non-fatal: the platform runs without invoice PDFs
				// rather than refusing to start. Downloads report "not
				// yet available" until storage is reachable.
				log.Printf("WARN: invoice PDF storage disabled: %v", err)
			} else {
				billingService.WithPDFStorage(billingpdf.Render, s3Client)
				log.Printf("Invoice PDF storage enabled (bucket %s)", bucket)
			}
		} else {
			log.Println("Invoice PDF storage not configured (INVOICES_BUCKET unset); invoices will issue without a stored document")
		}

		// Enable Stripe payment methods when keys are configured. Absent
		// (local dev), the provisioning gate still works off the DB (no
		// verified card ⇒ blocked), CreateSetupIntent errors cleanly, and
		// the webhook endpoint returns 503.
		if stripeKey := os.Getenv("STRIPE_SECRET_KEY"); stripeKey != "" {
			stripeClient = payments.NewClient(stripeKey, os.Getenv("STRIPE_WEBHOOK_SECRET"))
			billingService.WithStripe(newStripeGatewayAdapter(stripeClient))
			log.Println("Stripe payments enabled")
		} else {
			log.Println("Stripe payments not configured (STRIPE_SECRET_KEY unset); provisioning requires a verified card, which cannot be added without Stripe")
		}

		billingHandler = api.NewBillingHandler(billingService, authService)
		usageCollector = billing.NewUsageCollector(dbClient.DB(), billingService)
		log.Println("✅ Billing system initialized")

		// Start usage collector in background
		go usageCollector.Start(context.Background())

		// Monthly billing cycle: on the 1st, auto-generate + issue a usage
		// invoice per account for the previous calendar month. Idempotent
		// and skips accounts with no usage.
		go billing.NewBillingCycle(dbClient.DB(), billingService).Start(context.Background())
		log.Println("Monthly billing cycle started")

		// Charge collector: charges issued usage invoices off-session against
		// the account's verified card, for the net amount after credits.
		// Only started when Stripe is configured — without a gateway there
		// is nothing to charge with, and starting it would only record
		// pointless "not configured" attempts.
		if stripeClient != nil {
			go billing.NewChargeCollector(dbClient.DB(), billingService).Start(context.Background())
			log.Println("Charge collector started")
		} else {
			log.Println("Charge collector not started (Stripe not configured); issued invoices will not be charged")
		}
	}

	// Home-compute pilot: consumer-grade nodes as CPU capacity. Behind a
	// flag (default off) so the feature can be archived cleanly — flag off
	// plus migration 016 reverted leaves the platform exactly as before.
	// When off, nodeService/nodeHandler stay nil: no enrollment endpoint,
	// no nodes routes, and the gRPC server keeps accepting only the shared
	// datacenter token.
	var nodeService *nodes.Service
	var nodeHandler *api.NodeHandler
	if dbClient != nil && getEnv("HOME_COMPUTE_ENABLED", "false") == "true" {
		nodeService = nodes.NewService(dbClient.DB())
		// billingService supplies the live CPU/memory rates so home-capacity
		// tier prices match what metering charges. It may be nil (no billing);
		// the handler treats that as zero rates.
		nodeHandler = api.NewNodeHandler(nodeService, billingService)
		log.Println("Home compute enabled: node enrollment and per-node credentials active")

		// Flip nodes with no recent heartbeat to offline. A node heartbeats
		// on each inventory report (~30s); three missed reports is a
		// generous staleness threshold that tolerates a blip without
		// flapping.
		go func() {
			const staleAfter = 2 * time.Minute
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for range ticker.C {
				if n, err := nodeService.MarkStaleOffline(context.Background(), staleAfter); err != nil {
					log.Printf("WARN: node stale sweep failed: %v", err)
				} else if n > 0 {
					log.Printf("Node stale sweep: %d node(s) marked offline", n)
				}
			}
		}()
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

	// Initialize Harbor service (container registry integration)
	var harborClient *harbor.Client
	var harborService *harbor.Service
	var registryHandler *api.RegistryHandler

	if dbClient != nil && authService != nil && k8sClient != nil {
		// Security: no default registry credentials and no key reuse.
		// Harbor integration only activates with an explicit admin
		// password and a dedicated credential-encryption key.
		harborPassword := os.Getenv("HARBOR_ADMIN_PASSWORD")
		encryptionKey := os.Getenv("ENCRYPTION_KEY")

		switch {
		case harborPassword == "":
			log.Println("⚠️  HARBOR_ADMIN_PASSWORD not set — container registry features disabled (defaults are not accepted)")
		case encryptionKey == "":
			log.Println("⚠️  ENCRYPTION_KEY not set — container registry features disabled (credential encryption requires a dedicated key)")
		default:
			harborConfig := harbor.Config{
				BaseURL:  getEnv("HARBOR_URL", "https://registry.teepin.io"),
				Username: getEnv("HARBOR_ADMIN_USERNAME", "admin"),
				Password: harborPassword,
			}

			harborClient, err = harbor.NewClient(harborConfig)
			if err != nil {
				log.Printf("⚠️  Harbor client initialization failed: %v", err)
				log.Println("⚠️  Container registry features disabled")
			} else {
				harborService = harbor.NewService(harborClient, k8sClient, dbClient.DB(), encryptionKey)
				registryHandler = api.NewRegistryHandler(harborService, authService)
				log.Println("✅ Harbor container registry integration initialized")
			}
		}
	}

	// Initialize networking service (LoadBalancer, DNS, SSL).
	// TEEPIN_DISABLE_ENDPOINTS=true skips public endpoint provisioning
	// entirely — for single-node tests without MetalLB/DNS, where a
	// LoadBalancer Service would hang Pending and fail instance
	// creation. Instances are then reached via kubectl port-forward.
	var networkingService *networking.Service

	if getEnv("TEEPIN_DISABLE_ENDPOINTS", "false") == "true" {
		log.Println("⚠️  Public endpoint provisioning DISABLED (TEEPIN_DISABLE_ENDPOINTS=true) — access instances via port-forward")
	} else if k8sClient != nil {
		networkingConfig := networking.Config{
			Domain: getEnv("TEEPIN_DOMAIN", "teepin.com"),
			// Namespace is NOT independently configured — it must always
			// match where DirectClient actually creates pods
			// (cluster.WorkloadNamespace), or the Service this provisions
			// selects nothing (Stage 3 defect 3: this used to default to
			// "teepin" while pods ran in "default").
			Namespace: cluster.WorkloadNamespace,
			UseTLS:    getEnv("ENABLE_TLS", "true") == "true",
			TLSIssuer: getEnv("TLS_ISSUER", "letsencrypt-prod"),
		}
		networkingService = networking.NewService(k8sClient, networkingConfig)
		log.Println("✅ Networking stack initialized (LoadBalancer, DNS, SSL)")
	}

	// Initialize GPU inventory and allocator.
	// Capacity is discovered at runtime from NVIDIA GPU Operator node
	// labels, so any NVIDIA GPU works (MIG-capable or not). Set
	// TEEPIN_GPU_SIMULATION=true for local development without GPUs.
	gpuSimulated := getEnv("TEEPIN_GPU_SIMULATION", "false") == "true"
	var gpuInventory *gpu.Inventory
	if k8sClient != nil {
		gpuInventory = gpu.NewInventory(k8sClient, gpuSimulated)
	} else {
		gpuInventory = gpu.NewInventory(nil, gpuSimulated)
	}
	// The allocator is constructed below, once the cluster client exists:
	// its inventory source depends on how capacity is reached. Building
	// it here against the local Kubernetes client is what made agent mode
	// fail with "no kubernetes client available for GPU discovery" — the
	// control plane on AWS has none, and the capacity report arrives over
	// the agent stream instead.
	if gpuSimulated {
		log.Println("✅ GPU allocator initialized (SIMULATION mode — no real GPU hardware)")
	} else {
		log.Println("✅ GPU allocator initialized (hardware discovery via GPU Operator labels)")
	}

	// Initialize rate limiting
	var rateLimitMiddleware *ratelimit.Middleware
	rateLimitConfig := initRateLimiting()
	if rateLimitConfig != nil && rateLimitConfig.Enabled {
		limiter, err := ratelimit.NewLimiter(rateLimitConfig)
		if err != nil {
			log.Printf("⚠️  Failed to initialize rate limiter: %v", err)
			log.Println("⚠️  Rate limiting disabled")
		} else {
			rateLimitMiddleware = ratelimit.NewMiddleware(limiter, rateLimitConfig)
			log.Println("✅ Rate limiting initialized (Redis 7.2)")
		}
	} else {
		log.Println("⚠️  Rate limiting disabled (enable in config)")
	}

	// Initialize instance persistence — the billing source of truth.
	// The usage collector meters compute.instances, so instances must
	// be persisted there or they are never billed.
	var instanceStore *compute.Store
	if dbClient != nil {
		instanceStore = compute.NewStore(dbClient.DB())
		log.Println("✅ Instance persistence initialized")
	} else {
		log.Println("⚠️  Instance persistence disabled (no database) — instances will NOT be billed")
	}

	// Reconciler keeps DB state in sync with the cluster: pod phase
	// changes update status, vanished pods stop billing.
	// Started below, once the cluster client exists — the reconciler
	// reads cluster state through the same seam as the API.
	startReconciler := instanceStore != nil

	// Cluster client: the API server's only route to GPU capacity.
	//
	// TEEPIN_CLUSTER_MODE selects how capacity is reached:
	//
	//   direct — an in-cluster Kubernetes client. The control plane runs
	//            beside the GPUs. This is the single-node deployment and
	//            local development.
	//   agent  — capacity is reached over gRPC via an agent dialling in
	//            from the GPU datacenter, and this process holds no
	//            Kubernetes credentials at all. Required when the control
	//            plane runs on AWS.
	//
	// Defaults to direct: an operator who has not thought about this is
	// running beside their GPUs, and silently starting with no route to
	// capacity would be worse than the explicit failure below.
	clusterMode := getEnv("TEEPIN_CLUSTER_MODE", "direct")

	var (
		clusterClient cluster.Client
		agentServer   *cluster.AgentServer // non-nil only in agent mode
		agentRegistry *cluster.Registry
	)
	switch clusterMode {
	case "direct":
		if k8sClient == nil {
			log.Println("WARN: TEEPIN_CLUSTER_MODE=direct but no Kubernetes client is available - compute endpoints will report no capacity")
			clusterClient = cluster.NewUnavailable("no kubernetes client")
		} else {
			clusterClient = cluster.NewDirectClient(
				k8sClient,
				networkingService,
				gpuInventory,
				getEnv("TEEPIN_GPU_RUNTIME_CLASS", "nvidia"),
			)
			log.Println("Cluster mode: direct (in-cluster Kubernetes client)")
		}

	case "agent":
		agentToken := os.Getenv("TEEPIN_AGENT_TOKEN")
		if agentToken == "" {
			// Refuse rather than run an open agent channel: anyone able to
			// reach the port could otherwise place workloads on customer
			// GPUs and read customer logs.
			log.Fatal("TEEPIN_AGENT_TOKEN is required in agent mode")
		}

		agentRegistry = cluster.NewRegistry()
		agentClient := cluster.NewAgentClient(agentRegistry)
		agentServer = cluster.NewAgentServer(agentRegistry, agentClient, agentToken)

		// When home compute is enabled, the same gRPC channel also accepts
		// agents presenting a PER-NODE credential (home nodes). The shared
		// token still authenticates the datacenter agent unchanged; a
		// per-node credential resolves to a specific node whose class the
		// agent cannot self-assert.
		if nodeService != nil {
			agentServer = agentServer.
				WithNodeAuthenticator(newNodeAuthAdapter(nodeService)).
				WithNodeReporter(newNodeReporterAdapter(nodeService))
			log.Println("Agent channel: per-node credentials accepted, node persistence on (home compute)")
		}

		clusterClient = agentClient

		log.Println("Cluster mode: agent (gRPC control channel, no Kubernetes credentials held)")

	default:
		log.Fatalf("Invalid TEEPIN_CLUSTER_MODE %q (want \"direct\" or \"agent\")", clusterMode)
	}

	// GPU allocator, over whichever inventory source matches the
	// deployment. Placement policy is identical in both modes; only the
	// source of the capacity report differs.
	gpuAllocator := gpu.NewAllocator(cluster.NewSnapshotter(clusterClient))

	// Public status page metric. Silently inert unless all three
	// STATUSPAGE_* variables are set, so no deployment needs to know
	// about it.
	statusReporter := statuspage.New(statuspage.Config{
		APIKey:   os.Getenv("STATUSPAGE_API_KEY"),
		PageID:   os.Getenv("STATUSPAGE_PAGE_ID"),
		MetricID: os.Getenv("STATUSPAGE_GPU_METRIC_ID"),
	}, clusterClient)
	if statusReporter.Enabled() {
		go statusReporter.Start(context.Background())
	}

	// Reconciler keeps DB state in sync with the cluster: status changes
	// update the record, vanished instances stop billing. It runs
	// wherever the control plane runs, reading through the same cluster
	// seam as the API.
	if startReconciler {
		reconciler := compute.NewReconciler(instanceStore, clusterClient)
		go reconciler.Start(context.Background())
		log.Println("Instance reconciler started")
	}

	// Suspension sweeper: suspends accounts whose 24h payment grace period
	// has elapsed and tears down their resources. Inert until something
	// sets accounts.payment_failed_at (a card removed at Stripe today; a
	// failed charge later). Needs the DB, and — to actually stop
	// workloads — the cluster and instance store.
	if billingService != nil && instanceStore != nil {
		suspender := newResourceSuspender(clusterClient, instanceStore)
		sweeper := billing.NewSuspensionSweeper(dbClient.DB(), suspender)
		go sweeper.Start(context.Background())
		log.Println("Suspension sweeper started")
	}

	// Initialize API server with networking integration. billingService
	// doubles as the live pricing provider: rates come from the
	// billing.pricing table and are re-read before every allocation.
	// billingService is the pricing provider AND the provisioning gate —
	// both are methods on the one service. When it is nil (no database),
	// pass GENUINE nil interfaces, not a typed-nil pointer wrapped in an
	// interface: the latter is non-nil under `== nil` and would make the
	// standalone path call methods on a nil *billing.Service. The explicit
	// branch keeps the standalone (default pricing, no gate) path safe.
	var pricingProvider api.PricingProvider
	var provisionGate api.ProvisionGate
	if billingService != nil {
		pricingProvider = billingService
		provisionGate = billingService
	}
	apiServer := api.NewServer(clusterClient, gpuAllocator, instanceStore, pricingProvider, provisionGate).
		// Stamped onto every instance's placement so the customer-visible
		// hostname/TLS policy is a control-plane fact, read once here,
		// rather than independently configured per agent (TEEPIN_DOMAIN
		// previously existed on both this process and every agent process
		// — two sources of truth for the same URL).
		//
		// ENABLE_TLS default here is deliberately "false", NOT the "true"
		// the direct-mode networkingConfig above defaults to: this value
		// OVERRIDES whatever the agent's own TLS config says (see
		// cluster.endpointOptionsFor — true always overrides, false always
		// defers to the agent). Defaulting it true here would force TLS on
		// everywhere the instant this deployed, including before the
		// wildcard DNS record exists and HTTP-01 can resolve (Stage 3 plan
		// A7) — cert-manager would fail indefinitely and every endpoint
		// would stay HTTP-only anyway, for a worse, harder-to-diagnose
		// reason. Set ENABLE_TLS=true only after A7 verifies reachability.
		WithEndpointConfig(
			getEnv("TEEPIN_DOMAIN", "teepin.com"),
			getEnv("ENABLE_TLS", "false") == "true",
			getEnv("TLS_ISSUER", "letsencrypt-prod"),
		)
	// Enable home-class placement when home compute is on. Absent this, a
	// node_class:"home" request is refused cleanly (the placer is nil).
	if nodeService != nil {
		apiServer = apiServer.WithNodePlacer(newNodePlacerAdapter(nodeService))
	}

	// Admin API (pricing management): only enabled with an explicit
	// operator token — never on by default.
	var adminHandler *api.AdminHandler
	if billingService != nil {
		if adminToken := os.Getenv("ADMIN_API_TOKEN"); adminToken != "" {
			adminHandler = api.NewAdminHandler(billingService, authService, adminToken)
			log.Println("Admin API enabled (/v1/admin)")
		} else {
			log.Println("WARN: ADMIN_API_TOKEN not set — admin API (pricing management) disabled")
		}
	}

	// Stripe webhook handler — only when Stripe is configured. Registered
	// as an UNAUTHENTICATED route (Stripe calls it); its authentication is
	// the mandatory signature check inside the handler.
	var webhookHandler *api.WebhookHandler
	if stripeClient != nil && billingService != nil {
		webhookHandler = api.NewWebhookHandler(newStripeWebhookAdapter(stripeClient), billingService)
	}

	// Stage 3 tunnel edge (plan B1+B2): proxies *.{instanceDomain} traffic
	// to whichever agent session owns the target instance, over the same
	// gRPC channel that session already holds open. Needs both the
	// instance store (hostname -> provider/port, plan B2) and the agent
	// registry (provider -> live session) — only buildable in "agent"
	// cluster mode, where agentRegistry is non-nil. In "direct" mode there
	// is no agent session to tunnel through at all; the in-cluster
	// Ingress path (Phase A) is that deployment's only endpoint mechanism,
	// unchanged.
	var proxyHandler *cluster.ProxyHandler
	if agentRegistry != nil && instanceStore != nil {
		proxyHandler = cluster.NewProxyHandler(agentRegistry, newInstanceProxyTarget(instanceStore, agentRegistry))
		log.Println("Stage 3 tunnel edge enabled (instance traffic proxied over agent sessions)")
	}

	// Setup router
	router := setupRouter(apiServer, authHandler, accountHandler, authMiddleware, billingHandler, registryHandler, adminHandler, webhookHandler, nodeHandler, rateLimitMiddleware, proxyHandler, getEnv("TEEPIN_DOMAIN", "teepin.com"))

	// Create HTTP server
	port := getEnv("PORT", "8080")
	srv := &http.Server{
		Addr:    ":" + port,
		Handler: router,
	}

	// Start server in goroutine
	go func() {
		log.Printf("API server listening on %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Failed to start server: %v", err)
		}
	}()

	// gRPC control channel for agents, on its own port so the load
	// balancer can route HTTP and gRPC separately.
	var grpcServer *grpc.Server
	if agentServer != nil {
		grpcPort := getEnv("GRPC_PORT", "9090")

		listener, err := net.Listen("tcp", ":"+grpcPort)
		if err != nil {
			log.Fatalf("Failed to listen on gRPC port %s: %v", grpcPort, err)
		}

		grpcServer = grpc.NewServer(
			// Agents hold streams open indefinitely. Without an enforcement
			// policy gRPC rejects their keepalives as too aggressive and
			// tears down connections that are perfectly healthy.
			grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
				MinTime:             10 * time.Second,
				PermitWithoutStream: true,
			}),
			grpc.KeepaliveParams(keepalive.ServerParameters{
				Time:    30 * time.Second,
				Timeout: 10 * time.Second,
			}),
		)

		agentpb.RegisterClusterAgentServer(grpcServer, agentServer)

		// The standard gRPC health service, which the ALB target group
		// checks. A gRPC port with no health service marks every task
		// unhealthy and ECS destroys them - that failure cost a
		// deployment before this existed.
		healthServer := health.NewServer()
		healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
		healthpb.RegisterHealthServer(grpcServer, healthServer)

		// Reflection lets grpcurl introspect the service when debugging a
		// connection problem from outside the cluster.
		reflection.Register(grpcServer)

		go func() {
			log.Printf("Agent gRPC channel listening on :%s", grpcPort)
			if err := grpcServer.Serve(listener); err != nil {
				log.Printf("gRPC server stopped: %v", err)
			}
		}()
	}

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

	// GracefulStop lets agents observe a clean disconnect and reconnect
	// promptly, rather than waiting for a keepalive to expire against a
	// socket that is already gone.
	if grpcServer != nil {
		grpcServer.GracefulStop()
		log.Println("Agent gRPC channel stopped")
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

func initRateLimiting() *ratelimit.Config {
	// Try to load from config file
	configPath := getEnv("RATELIMIT_CONFIG", "config/ratelimit.yaml")
	config, err := ratelimit.LoadConfig(configPath)
	if err != nil {
		log.Printf("⚠️  Failed to load rate limit config from %s: %v", configPath, err)
		log.Println("⚠️  Using default rate limit configuration")
		config = ratelimit.DefaultConfig()
	}

	// Override Redis URL from environment if set
	if redisURL := os.Getenv("REDIS_URL"); redisURL != "" {
		config.RedisURL = redisURL
	}

	// Override Redis password from environment if set
	if redisPassword := os.Getenv("REDIS_PASSWORD"); redisPassword != "" {
		config.RedisPassword = redisPassword
	}

	// Allow disabling via environment variable
	if getEnv("RATE_LIMIT_ENABLED", "true") == "false" {
		config.Enabled = false
	}

	return config
}

func setupRouter(apiServer *api.Server, authHandler *api.AuthHandler, accountHandler *api.AccountHandler, authMiddleware *auth.Middleware, billingHandler *api.BillingHandler, registryHandler *api.RegistryHandler, adminHandler *api.AdminHandler, webhookHandler *api.WebhookHandler, nodeHandler *api.NodeHandler, rateLimitMiddleware *ratelimit.Middleware, proxyHandler *cluster.ProxyHandler, instanceDomain string) *gin.Engine {
	// Set Gin to release mode in production
	if os.Getenv("GIN_MODE") == "" {
		gin.SetMode(gin.ReleaseMode)
	}

	// gin.New() rather than gin.Default(): Default installs a Logger that
	// records every request including health checks. Behind an ALB with
	// two target groups that is ~12 lines every 15 seconds saying
	// nothing, which buries real events (a startup line becomes
	// unfindable within minutes) and is billed by CloudWatch per ingested
	// byte.
	router := gin.New()

	// Middleware (order matters!)
	router.Use(gin.LoggerWithConfig(gin.LoggerConfig{
		SkipPaths: []string{"/health", "/ready"},
	}))
	router.Use(gin.Recovery())
	router.Use(corsMiddleware())
	router.Use(requestIDMiddleware())

	// Stage 3 tunnel edge: *.{instanceDomain} traffic (excluding api./
	// console., the control plane's own hostnames) is a customer's
	// instance, not an API call — it bypasses auth and rate limiting
	// entirely and is handed straight to the proxy. Mounted first, before
	// anything that assumes an authenticated API request, exactly like the
	// unauthenticated Stripe-webhook and node-enroll routes below (see plan
	// B1: "bypasses the normal auth middleware — this is public customer
	// traffic, not authenticated API traffic").
	if proxyHandler != nil && instanceDomain != "" {
		router.Use(tunnelMiddleware(proxyHandler, instanceDomain))
	}

	// Rate limiting middleware (applies to all routes)
	// Note: Applied AFTER auth middleware so we can use user tier
	if rateLimitMiddleware != nil {
		router.Use(rateLimitMiddleware.Handler())
	}

	// Health checks
	router.GET("/health", healthHandler)
	router.GET("/version", versionHandler)

	// API v1
	v1 := router.Group("/v1")
	{
		// Stripe webhooks — UNAUTHENTICATED at the router because Stripe
		// calls it. Its authentication is the mandatory signature check
		// inside the handler, so it must sit outside every auth group.
		if webhookHandler != nil {
			v1.POST("/webhooks/stripe", webhookHandler.HandleStripe)
		}

		// Node enrollment — UNAUTHENTICATED at the router because the
		// enrolling agent has no credential yet; the one-time token in the
		// body is the authentication. Only mounted when home compute is on.
		if nodeHandler != nil {
			v1.POST("/nodes/enroll", nodeHandler.Enroll)
		}

		// Authentication endpoints (public)
		if authHandler != nil {
			authRoutes := v1.Group("/auth")
			{
				// Account owners sign in by email; sub-users by
				// account alias + username, since the same email may
				// exist in several accounts.
				authRoutes.POST("/login", authHandler.Login)
				authRoutes.GET("/me", authMiddleware.RequireAuth(), authHandler.GetCurrentUser)
				if accountHandler != nil {
					authRoutes.POST("/login/sub-user", accountHandler.LoginSubUser)
				}
			}

			// Account endpoints. Registration creates an account plus
			// its owner user — every user belongs to an account,
			// because the account is what gets billed.
			if accountHandler != nil {
				v1.POST("/accounts", accountHandler.Register)

				accounts := v1.Group("/accounts/current")
				accounts.Use(authMiddleware.RequireAuth())
				{
					accounts.GET("", accountHandler.GetCurrent)
					accounts.PATCH("", accountHandler.UpdateCurrent)
					accounts.POST("/convert-to-organization", accountHandler.ConvertToOrganization)
					accounts.GET("/users", accountHandler.ListUsers)
					accounts.POST("/users", accountHandler.CreateUser)
					accounts.PATCH("/users/:user_id", accountHandler.UpdateUser)
					accounts.DELETE("/users/:user_id", accountHandler.DeleteUser)

					// Payment methods belong to the account (the billing
					// entity), so they live under /accounts/current and use
					// the JWT session — not a project API key.
					if billingHandler != nil {
						accounts.POST("/payment-methods/setup-intent", billingHandler.CreateSetupIntent)
						accounts.GET("/payment-methods", billingHandler.ListPaymentMethods)
						accounts.DELETE("/payment-methods/:id", billingHandler.RemovePaymentMethod)
						accounts.POST("/payment-methods/:id/default", billingHandler.SetDefaultPaymentMethod)
					}
				}
			}

			// Project endpoints (require auth)
			projects := v1.Group("/projects")
			projects.Use(authMiddleware.RequireAuth())
			{
				projects.POST("", authHandler.CreateProject)
				projects.GET("", authHandler.ListProjects)
				projects.GET("/:id", authHandler.GetProject)
				projects.PATCH("/:id", authHandler.UpdateProject)
				projects.DELETE("/:id", authHandler.DeleteProject)
				projects.POST("/:id/api-keys", authHandler.CreateAPIKey)
				projects.GET("/:id/api-keys", authHandler.ListAPIKeys)
				projects.DELETE("/:id/api-keys/:key_id", authHandler.RevokeAPIKey)

				// Registry endpoints (if Harbor integration enabled)
				if registryHandler != nil {
					projects.POST("/:id/registry", registryHandler.ProvisionRegistry)
					projects.GET("/:id/registry", registryHandler.GetRegistryCredentials)
					projects.DELETE("/:id/registry", registryHandler.RevokeRegistry)
					projects.GET("/:id/registry/login-command", registryHandler.GetDockerLoginCommand)
				}
			}
		}

		// Billing endpoints (require auth)
		if billingHandler != nil && authMiddleware != nil {
			billing := v1.Group("/billing")
			billing.Use(authMiddleware.RequireAuth())
			{
				billing.GET("/summary", billingHandler.GetAccountSummary)
				billing.GET("/usage", billingHandler.GetUsageSummary)
				billing.GET("/usage/records", billingHandler.GetUsageRecords)
				billing.GET("/current-month", billingHandler.GetCurrentMonthUsage)
				billing.GET("/invoices", billingHandler.ListInvoices)
				billing.GET("/invoices/:id", billingHandler.GetInvoice)
				billing.GET("/invoices/:id/pdf", billingHandler.DownloadInvoicePDF)
				billing.POST("/invoices", billingHandler.CreateInvoice)
				billing.GET("/credits", billingHandler.GetCreditBalance)
			}
		}

		// Compute endpoints require auth — requireScope (server.go) rejects
		// an unscoped caller anyway, so OptionalAuth bought nothing but a
		// worse error message (401 from requireScope vs. this group).
		compute := v1.Group("/compute")
		if authMiddleware != nil {
			compute.Use(authMiddleware.RequireAuth())
		}
		{
			compute.GET("/instance-types", apiServer.ListInstanceTypes)
			compute.GET("/image-ports", apiServer.ImagePorts)
			compute.POST("/instances", apiServer.CreateInstance)
			compute.GET("/instances", apiServer.ListInstances)
			compute.GET("/instances/:id", apiServer.GetInstance)
			compute.DELETE("/instances/:id", apiServer.DeleteInstance)
			compute.GET("/instances/:id/logs", apiServer.GetInstanceLogs)
			compute.GET("/instances/:id/metrics", apiServer.GetInstanceMetrics)

			// Home capacity — which CPU tiers fit right now. Only mounted
			// when home compute is enabled; the create dialog uses it to
			// enable/disable tiers.
			if nodeHandler != nil {
				compute.GET("/home-capacity", nodeHandler.HomeCapacity)
			}
		}

		// Admin endpoints (operator token, separate from customer auth)
		if adminHandler != nil {
			admin := v1.Group("/admin")
			admin.Use(adminHandler.RequireAdminToken())
			{
				admin.GET("/pricing", adminHandler.GetPricing)
				admin.PUT("/pricing", adminHandler.UpdatePricing)
				admin.PUT("/pricing/cpu", adminHandler.UpdateCPUPricing)

				// Manual invoicing for the operator control centre.
				admin.GET("/accounts", adminHandler.ListAccounts)
				admin.GET("/accounts/:account_id/projects", adminHandler.ListAccountProjects)
				admin.POST("/accounts/:account_id/usage-invoices", adminHandler.GenerateAccountUsageInvoice)
				admin.POST("/accounts/:account_id/credits", adminHandler.GrantCredit)
				admin.GET("/accounts/:account_id/credits", adminHandler.GetAccountCredits)
				admin.POST("/invoices", adminHandler.CreateManualInvoice)
				admin.GET("/invoices/:id", adminHandler.GetInvoice)
				admin.GET("/invoices/:id/charge-state", adminHandler.GetInvoiceChargeState)
				admin.POST("/invoices/:id/issue", adminHandler.IssueInvoice)
				admin.POST("/invoices/:id/charge", adminHandler.ChargeInvoice)
				admin.POST("/invoices/:id/void", adminHandler.VoidInvoice)
				admin.GET("/projects/:project_id/invoices", adminHandler.ListProjectInvoices)
				admin.GET("/accounts/:account_id/invoices", adminHandler.ListAccountInvoices)

				// Node management (home compute). Same operator-token guard
				// as the rest of /v1/admin. Only mounted when enabled.
				if nodeHandler != nil {
					admin.GET("/nodes", nodeHandler.ListNodes)
					admin.POST("/nodes/enrollment-tokens", nodeHandler.CreateEnrollmentToken)
					admin.PUT("/nodes/:id/reservation", nodeHandler.SetReservation)
					admin.PATCH("/nodes/:id", nodeHandler.RenameNode)
					admin.DELETE("/nodes/:id", nodeHandler.DeleteNode)
					admin.POST("/nodes/:id/disable", nodeHandler.DisableNode)
				}
			}
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
		"status":    "healthy",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

func versionHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"version":     version,
		"api_version": "v1",
	})
}

// corsMiddleware allows browser clients (the console) to call the API
// cross-origin.
//
// Origins are an explicit allowlist, never "*": a wildcard origin
// combined with Allow-Credentials is rejected outright by every
// browser, so the previous "*" + credentials:true combination meant no
// authenticated cross-origin request could ever succeed. The allowlist
// also stops arbitrary sites driving the API with a user's cookies.
//
// Configure with TEEPIN_CORS_ORIGINS (comma-separated). Defaults cover
// the production console and local development.
func corsMiddleware() gin.HandlerFunc {
	raw := getEnv("TEEPIN_CORS_ORIGINS",
		"https://console.teepin.com,http://localhost:3000")

	allowed := map[string]bool{}
	for _, o := range strings.Split(raw, ",") {
		if o = strings.TrimSpace(o); o != "" {
			allowed[o] = true
		}
	}
	log.Printf("CORS allowed origins: %s", raw)

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")

		if origin != "" && allowed[origin] {
			c.Writer.Header().Set("Access-Control-Allow-Origin", origin)
			c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
			// Responses vary by Origin — without this, a shared cache
			// could serve one origin's headers to another.
			c.Writer.Header().Add("Vary", "Origin")
			c.Writer.Header().Set("Access-Control-Allow-Headers",
				"Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, Accept, Origin, Cache-Control, X-Requested-With, "+auth.ProjectHeader)
			c.Writer.Header().Set("Access-Control-Allow-Methods",
				"GET, POST, PUT, PATCH, DELETE, OPTIONS")
			c.Writer.Header().Set("Access-Control-Max-Age", "86400")
		}

		// Preflight: answer here regardless — a disallowed origin simply
		// receives no CORS headers and the browser blocks it.
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
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

// tunnelMiddleware routes customer instance traffic (Stage 3 plan B1).
//
// A request's Host decides everything: "inst-6fea56ce.teepin.com" is a
// customer's instance and goes to the tunnel; "api.teepin.com",
// "console.teepin.com", and every other host (including no dot at all —
// health checks from inside the VPC, bare IPs) fall through to the normal
// v1 routes unchanged. The reserved-label check must come before the
// wildcard match: without it, a customer who names an instance "api" or
// "console" (instance IDs are server-generated "inst-xxxxxxxx" today, but
// nothing here should depend on that not changing) could shadow the
// control plane's own endpoints.
func tunnelMiddleware(handler *cluster.ProxyHandler, domain string) gin.HandlerFunc {
	suffix := "." + domain
	reserved := map[string]bool{"api": true, "console": true}

	return func(c *gin.Context) {
		host := c.Request.Host
		if idx := strings.IndexByte(host, ':'); idx != -1 {
			host = host[:idx]
		}

		if !strings.HasSuffix(host, suffix) {
			c.Next()
			return
		}
		label := strings.TrimSuffix(host, suffix)
		if label == "" || strings.Contains(label, ".") || reserved[label] {
			c.Next()
			return
		}

		handler.ServeInstance(c.Writer, c.Request, label)
		c.Abort()
	}
}
