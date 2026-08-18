// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Command teepin-agent runs beside GPU capacity and executes work on
// behalf of the TEEPIN control plane.
//
// It dials OUT to the control plane and holds a gRPC stream open.
// Nothing inbound is exposed on the GPU node: the Kubernetes API server
// stays firewalled, and the control plane needs no route into the
// datacenter. This is the pattern used by GitHub Actions runners and
// Tailscale, and it is what allows the control plane to run on AWS while
// the GPUs sit anywhere.
//
// The agent holds the Kubernetes credentials. The control plane holds
// none.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	agentpb "github.com/FlashbackAi/teepin-core/pkg/agentpb"
	"github.com/FlashbackAi/teepin-core/pkg/agentrunner"
	"github.com/FlashbackAi/teepin-core/pkg/cluster"
	"github.com/FlashbackAi/teepin-core/pkg/gpu"
	"github.com/FlashbackAi/teepin-core/pkg/networking"
)

// Version is stamped at build time and reported at registration, so an
// operator can tell which agents are running old code.
var Version = "dev"

// newFlagSet builds a subcommand flag set that prints usage to stderr.
func newFlagSet(name string) *flag.FlagSet {
	return flag.NewFlagSet(name, flag.ContinueOnError)
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	// Subcommand dispatch. "run" is the default so existing datacenter
	// deployments (which invoke the binary with no argument) are unchanged.
	cmd := "run"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	switch cmd {
	case "enroll":
		if err := runEnroll(args); err != nil {
			log.Fatalf("enroll: %v", err)
		}
	case "run":
		runAgent()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (want: run, enroll)\n", cmd)
		os.Exit(2)
	}
}

// runAgent is the long-lived agent. It supports two identities:
//
//   - ENROLLED home node: a per-node credential saved by `enroll` exists.
//     It authenticates with that credential, needs no Kubernetes, and
//     reports an empty (CPU-only) inventory. This is the home-compute pilot.
//   - DATACENTER agent: no saved credential; the shared TEEPIN_AGENT_TOKEN
//     and a real Kubernetes client, exactly as before.
func runAgent() {
	log.Printf("TEEPIN cluster agent %s starting", Version)

	if cfg, err := loadNodeConfig(); err == nil {
		runEnrolledAgent(cfg)
		return
	}

	// Datacenter path — unchanged.
	controlPlane := mustEnv("TEEPIN_CONTROL_PLANE")
	token := mustEnv("TEEPIN_AGENT_TOKEN")
	providerID := getEnv("TEEPIN_PROVIDER_ID", "default")
	region := getEnv("TEEPIN_REGION", "us-east")

	k8sClient, err := newKubernetesClient()
	if err != nil {
		log.Fatalf("Kubernetes client: %v", err)
	}
	log.Println("Connected to Kubernetes")

	simulated := getEnv("TEEPIN_GPU_SIMULATED", "false") == "true"
	inventory := gpu.NewInventory(k8sClient, simulated)

	networkingService := networking.NewService(k8sClient, networking.Config{
		Domain: getEnv("TEEPIN_DOMAIN", "teepin.com"),
		// Namespace is NOT independently configured — it must always match
		// where DirectClient actually creates pods (cluster.WorkloadNamespace),
		// or the Service this provisions selects nothing (Stage 3 defect 3).
		Namespace: cluster.WorkloadNamespace,
		TLSIssuer: getEnv("TEEPIN_TLS_ISSUER", "letsencrypt-prod"),
	})

	// The agent executes through the same DirectClient the single-node
	// deployment uses. That is deliberate: one implementation of pod
	// construction, so a GPU pod created via an agent is byte-identical
	// to one created locally, and the tests covering it cover both paths.
	clusterClient := cluster.NewDirectClient(
		k8sClient,
		networkingService,
		inventory,
		getEnv("TEEPIN_GPU_RUNTIME_CLASS", "nvidia"),
	)

	runner := agentrunner.New(agentrunner.Config{
		ProviderID: providerID,
		Region:     region,
		Version:    Version,
		Cluster:    clusterClient,
		Inventory:  inventory,
	})

	ctx, cancel := shutdownContext()
	defer cancel()

	runForever(ctx, controlPlane, token, providerID, runner)
	log.Println("Agent stopped")
}

// runEnrolledAgent runs a home node using its saved per-node credential. It
// holds no Kubernetes credentials and reports an empty CPU-only inventory —
// enough for Stage 1 (the node shows online and survives a restart). Stage 2
// swaps the CPU-only client for a k3s-backed DirectClient to run workloads.
func runEnrolledAgent(cfg *nodeConfig) {
	log.Printf("Running as enrolled node %q (class=%s)", cfg.NodeName, cfg.Class)

	controlPlane := getEnv("TEEPIN_CONTROL_PLANE", "")
	if controlPlane == "" {
		log.Fatal("TEEPIN_CONTROL_PLANE is required (the gRPC control-plane address)")
	}

	runner := agentrunner.New(agentrunner.Config{
		ProviderID: cfg.NodeName,
		Region:     getEnv("TEEPIN_REGION", "home"),
		Version:    Version,
		Cluster:    homeClusterClient(),
		Inventory:  nil, // CPU-only: no GPU inventory
	})

	ctx, cancel := shutdownContext()
	defer cancel()

	// The credential travels in the SAME metadata slot as the shared token;
	// the control plane tries the shared token first, then resolves this as
	// a per-node credential.
	runForever(ctx, controlPlane, cfg.Credential, cfg.NodeName, runner)
	log.Println("Agent stopped")
}

// homeClusterClient returns the execution backend for a home node. It tries
// to reach a local Kubernetes (k3s writes its kubeconfig to
// /etc/rancher/k3s/k3s.yaml; the install script points KUBECONFIG at it) and,
// on success, runs workloads through the SAME DirectClient the datacenter
// uses — one pod-construction path, so a CPU pod on a home node is built
// identically to one in the datacenter.
//
// It passes nil networking (public endpoints are Stage 3) and nil GPU
// inventory (home nodes are CPU-only); DirectClient already guards both. If
// k3s is not reachable yet — a node enrolled but still being set up — it
// falls back to CPUOnly so the agent still connects and heartbeats (the node
// shows online), it simply cannot run workloads until k3s is up.
func homeClusterClient() cluster.Client {
	k8s, err := newKubernetesClient()
	if err != nil {
		log.Printf("WARN: no Kubernetes reachable (%v); running CPU-only without execution. "+
			"Install k3s and restart the agent to run workloads.", err)
		return cluster.NewCPUOnly()
	}
	log.Println("Kubernetes reachable: workloads will run via k3s")
	return cluster.NewDirectClient(
		k8s,
		nil, // networking: public endpoints are Stage 3
		nil, // inventory: CPU-only, no GPU
		getEnv("TEEPIN_GPU_RUNTIME_CLASS", ""), // no GPU runtime class on a home node
	)
}

// shutdownContext returns a context cancelled on SIGINT/SIGTERM. Closing the
// stream lets the control plane fail in-flight commands rather than waiting
// for their timeout.
func shutdownContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("Shutdown signal received")
		cancel()
	}()
	return ctx, cancel
}

// runForever maintains the control-plane connection, reconnecting with
// exponential backoff and jitter.
//
// The agent must survive the control plane restarting, an ALB failing
// over, and its own network dropping — none of which should require
// operator action. Jitter matters once there are many agents: without
// it, every agent reconnects at the same instant after a control-plane
// deploy and stampedes it.
func runForever(ctx context.Context, address, token, providerID string, runner *agentrunner.Runner) {
	const (
		minBackoff = 1 * time.Second
		maxBackoff = 60 * time.Second
	)

	backoff := minBackoff

	for ctx.Err() == nil {
		start := time.Now()

		if err := connectOnce(ctx, address, token, providerID, runner); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("Connection ended: %v", err)
		}

		// A connection that lasted a while indicates a healthy endpoint
		// that dropped for an unrelated reason, so reset rather than
		// carrying penalty backoff from an unrelated earlier failure.
		if time.Since(start) > time.Minute {
			backoff = minBackoff
		}

		// Full jitter: sleep a random duration in [0, backoff).
		// Deterministic backoff synchronises reconnect storms.
		sleep := time.Duration(rand.Int63n(int64(backoff)))
		log.Printf("Reconnecting in %s", sleep.Round(time.Millisecond))

		select {
		case <-time.After(sleep):
		case <-ctx.Done():
			return
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// connectOnce runs one connection to exhaustion.
func connectOnce(ctx context.Context, address, token, providerID string, runner *agentrunner.Runner) error {
	// TLS unless explicitly disabled for local development. The stream
	// carries customer log data and workload definitions.
	var transportCreds grpc.DialOption
	if getEnv("TEEPIN_AGENT_INSECURE", "false") == "true" {
		log.Println("WARN: TEEPIN_AGENT_INSECURE=true - the control channel is NOT encrypted")
		transportCreds = grpc.WithTransportCredentials(insecure.NewCredentials())
	} else {
		transportCreds = grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(nil, ""))
	}

	conn, err := grpc.NewClient(address,
		transportCreds,
		// Keepalives detect a dead peer that never sent a FIN — a NAT
		// timeout or a load balancer dropping an idle connection. Without
		// them the agent can sit believing it is connected while the
		// control plane has long forgotten it.
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return err
	}
	defer conn.Close()

	client := agentpb.NewClusterAgentClient(conn)

	// The token travels in metadata on the stream's own context.
	ctx = metadata.AppendToOutgoingContext(ctx, cluster.AgentTokenMetadataKey, token)

	stream, err := client.Connect(ctx)
	if err != nil {
		return err
	}

	log.Printf("Connected to control plane at %s as provider %s", address, providerID)
	return runner.Run(ctx, stream)
}

func newKubernetesClient() (kubernetes.Interface, error) {
	// In-cluster first: the agent normally runs as a pod beside the
	// workloads it manages.
	if config, err := rest.InClusterConfig(); err == nil {
		return kubernetes.NewForConfig(config)
	}

	kubeconfig := getEnv("KUBECONFIG", clientcmd.RecommendedHomeFile)
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(config)
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("%s is required", key)
	}
	return v
}

// getEnvInt is retained for tunables added later; unused values would
// otherwise be re-derived ad hoc at each call site.
func getEnvInt(key string, fallback int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
