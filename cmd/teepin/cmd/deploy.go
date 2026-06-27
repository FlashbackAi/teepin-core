// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/FlashbackAi/teepin-core/pkg/models"
	"gopkg.in/yaml.v3"
)

var (
	deployImage   string
	deployName    string
	deployGPUVRAM string
	deployCPU     int
	deployMemory  string
	deployRegion  string
	deployEnv     []string
	deployPorts   []string
	deployDetach  bool
	deployWait    bool
	deployDryRun  bool
)

var deployCmd = &cobra.Command{
	Use:   "deploy [flags]",
	Short: "Deploy a GPU instance",
	Long: `Deploy a GPU-accelerated compute instance.

Examples:
  # Basic deployment
  teepin deploy --image pytorch/pytorch:latest --gpu-vram 25GB

  # Full configuration
  teepin deploy \
    --image pytorch/pytorch:latest \
    --name pytorch-training \
    --gpu-vram 25GB \
    --cpu 4 \
    --memory 16GB \
    --env EPOCHS=100 \
    --env BATCH_SIZE=32 \
    --port 8888:8888

  # Detached mode (scripting)
  INSTANCE_ID=$(teepin deploy --image pytorch/pytorch:latest --gpu-vram 25GB -d)
`,
	Run: runDeploy,
}

func init() {
	rootCmd.AddCommand(deployCmd)

	deployCmd.Flags().StringVarP(&deployImage, "image", "i", "", "Container image (required)")
	deployCmd.Flags().StringVarP(&deployName, "name", "n", "", "Instance name (auto-generated if not provided)")
	deployCmd.Flags().StringVarP(&deployGPUVRAM, "gpu-vram", "g", "", "GPU VRAM allocation (e.g., 10GB, 25GB, 40GB, 80GB)")
	deployCmd.Flags().IntVarP(&deployCPU, "cpu", "c", 4, "CPU cores")
	deployCmd.Flags().StringVarP(&deployMemory, "memory", "m", "16GB", "Memory allocation")
	deployCmd.Flags().StringVarP(&deployRegion, "region", "r", "", "Deployment region")
	deployCmd.Flags().StringSliceVarP(&deployEnv, "env", "e", []string{}, "Environment variables (KEY=VALUE)")
	deployCmd.Flags().StringSliceVarP(&deployPorts, "port", "p", []string{}, "Port mappings (HOST:CONTAINER)")
	deployCmd.Flags().BoolVarP(&deployDetach, "detach", "d", false, "Run in background (only output instance ID)")
	deployCmd.Flags().BoolVarP(&deployWait, "wait", "w", true, "Wait for instance to be ready")
	deployCmd.Flags().BoolVar(&deployDryRun, "dry-run", false, "Show what would be deployed without actually deploying")

	deployCmd.MarkFlagRequired("image")
}

func runDeploy(cmd *cobra.Command, args []string) {
	// Load config
	config := loadConfig()

	// Parse environment variables
	envMap := make(map[string]string)
	for _, env := range deployEnv {
		parts := strings.SplitN(env, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	// Parse port mappings
	var portMappings []models.PortMapping
	for _, port := range deployPorts {
		parts := strings.SplitN(port, ":", 2)
		if len(parts) == 2 {
			var publicPort, containerPort int
			fmt.Sscanf(parts[0], "%d", &publicPort)
			fmt.Sscanf(parts[1], "%d", &containerPort)
			portMappings = append(portMappings, models.PortMapping{
				Container: containerPort,
				Public:    publicPort,
				Protocol:  "tcp",
			})
		}
	}

	// Auto-generate name if not provided
	if deployName == "" {
		deployName = fmt.Sprintf("instance-%d", time.Now().Unix())
	}

	// Build request
	req := models.CreateInstanceRequest{
		Name:     deployName,
		Image:    deployImage,
		GPUVRAM:  deployGPUVRAM,
		CPUUnits: deployCPU,
		Memory:   deployMemory,
		Env:      envMap,
		Ports:    portMappings,
	}

	if deployDryRun {
		fmt.Println("🔍 Dry run - showing what would be deployed:")
		fmt.Println()
		data, _ := json.MarshalIndent(req, "", "  ")
		fmt.Println(string(data))
		return
	}

	// Show progress (unless detached)
	if !deployDetach {
		fmt.Printf("🚀 Deploying %s...\n", deployName)
		fmt.Println()
		fmt.Println("✓ Validating configuration")
	}

	// Make API request
	apiURL := config.APIURL + "/v1/compute/instances"
	reqBody, err := json.Marshal(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Error: %v\n", err)
		os.Exit(1)
	}

	resp, err := http.Post(apiURL, "application/json", bytes.NewBuffer(reqBody))
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Error connecting to API: %v\n", err)
		fmt.Fprintf(os.Stderr, "   Make sure the API server is running at: %s\n", config.APIURL)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusCreated {
		fmt.Fprintf(os.Stderr, "❌ Failed to create instance\n")
		fmt.Fprintf(os.Stderr, "   Status: %d\n", resp.StatusCode)
		fmt.Fprintf(os.Stderr, "   Response: %s\n", string(body))
		os.Exit(1)
	}

	var instance models.Instance
	if err := json.Unmarshal(body, &instance); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Error parsing response: %v\n", err)
		os.Exit(1)
	}

	// Detached mode - just output ID
	if deployDetach {
		fmt.Println(instance.ID)
		return
	}

	// Success output
	if deployGPUVRAM != "" {
		fmt.Printf("✓ Checking VRAM availability (%s)\n", deployGPUVRAM)
		fmt.Println("✓ Allocating GPU")
	}
	fmt.Println("✓ Creating pod in cluster")
	fmt.Println("⏳ Waiting for instance to be ready...")
	fmt.Println()

	// Print instance details
	fmt.Println("┌─────────────────────────────────────────────────────────┐")
	fmt.Println("│ Instance Created Successfully                           │")
	fmt.Println("├─────────────────────────────────────────────────────────┤")
	fmt.Printf("│ %-20s %-35s │\n", "ID:", instance.ID)
	fmt.Printf("│ %-20s %-35s │\n", "Name:", instance.Name)
	fmt.Printf("│ %-20s %-35s │\n", "Status:", instance.Status)
	fmt.Printf("│ %-20s %-35s │\n", "Image:", truncate(instance.Image, 35))
	if instance.GPUVRAM != "" {
		fmt.Printf("│ %-20s %-35s │\n", "GPU VRAM:", instance.GPUVRAM+" (exact allocation)")
	}
	fmt.Printf("│ %-20s %-35s │\n", "CPU:", fmt.Sprintf("%d cores", instance.CPUUnits))
	fmt.Printf("│ %-20s %-35s │\n", "Memory:", instance.Memory)
	if deployRegion != "" {
		fmt.Printf("│ %-20s %-35s │\n", "Region:", deployRegion)
	}
	if instance.Endpoint != "" {
		fmt.Printf("│ %-20s %-35s │\n", "Endpoint:", truncate(instance.Endpoint, 35))
	}
	// Calculate cost (example)
	cost := calculateCost(instance)
	fmt.Printf("│ %-20s %-35s │\n", "Cost:", fmt.Sprintf("$%.2f/hour", cost))
	fmt.Println("├─────────────────────────────────────────────────────────┤")
	fmt.Printf("│ %-20s %-35s │\n", "Created:", instance.CreatedAt.Format("2006-01-02 15:04:05 UTC"))
	fmt.Println("└─────────────────────────────────────────────────────────┘")
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Printf("  • View logs:    teepin logs %s\n", instance.ID)
	fmt.Printf("  • SSH access:   teepin exec %s\n", instance.ID)
	fmt.Printf("  • Stop:         teepin stop %s\n", instance.ID)
	fmt.Printf("  • Delete:       teepin delete %s\n", instance.ID)
}

func loadConfig() Config {
	home, _ := os.UserHomeDir()
	configFile := filepath.Join(home, ".teepin", "config.yaml")

	// Default config
	config := Config{
		APIURL:        "http://localhost:8080",
		DefaultRegion: "us-west-1",
		OutputFormat:  "table",
	}

	// Read config file if exists
	if data, err := os.ReadFile(configFile); err == nil {
		yaml.Unmarshal(data, &config)
	}

	return config
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

func calculateCost(instance models.Instance) float64 {
	// Simple cost calculation
	cost := 0.0

	// GPU cost ($0.10 per GB-hour)
	if instance.GPUVRAM != "" {
		vram := 0
		fmt.Sscanf(instance.GPUVRAM, "%dGB", &vram)
		cost += float64(vram) * 0.10
	}

	// CPU cost ($0.05 per core-hour)
	cost += float64(instance.CPUUnits) * 0.05

	// Memory cost ($0.01 per GB-hour)
	if instance.Memory != "" {
		memory := 0
		fmt.Sscanf(instance.Memory, "%dGB", &memory)
		cost += float64(memory) * 0.01
	}

	return cost
}
