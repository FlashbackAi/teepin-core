// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/FlashbackAi/teepin-core/pkg/models"
)

var listCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls", "get"},
	Short:   "List all instances",
	Long: `List all compute instances.

Examples:
  # List all instances
  teepin list

  # JSON output
  teepin list -o json

  # Aliases
  teepin ls
  teepin get instances
`,
	Run: runList,
}

func init() {
	rootCmd.AddCommand(listCmd)
}

type ListInstancesResponse struct {
	Instances []models.Instance `json:"instances"`
	Count     int               `json:"count"`
}

func runList(cmd *cobra.Command, args []string) {
	config := loadConfig()

	// Make API request
	apiURL := config.APIURL + "/v1/compute/instances"
	resp, err := http.Get(apiURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Error connecting to API: %v\n", err)
		fmt.Fprintf(os.Stderr, "   Make sure the API server is running at: %s\n", config.APIURL)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "❌ Failed to list instances\n")
		fmt.Fprintf(os.Stderr, "   Status: %d\n", resp.StatusCode)
		fmt.Fprintf(os.Stderr, "   Response: %s\n", string(body))
		os.Exit(1)
	}

	var response ListInstancesResponse
	if err := json.Unmarshal(body, &response); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Error parsing response: %v\n", err)
		os.Exit(1)
	}

	// Handle different output formats
	switch output {
	case "json":
		fmt.Println(string(body))
	case "yaml":
		// TODO: YAML output
		fmt.Println(string(body))
	default:
		// Table output
		if len(response.Instances) == 0 {
			fmt.Println("No instances found.")
			fmt.Println()
			fmt.Println("To deploy your first instance:")
			fmt.Println("  teepin deploy --image pytorch/pytorch:latest --gpu-vram 25GB")
			return
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tSTATUS\tGPU VRAM\tCREATED")
		fmt.Fprintln(w, "--\t----\t------\t--------\t-------")

		for _, instance := range response.Instances {
			vram := instance.GPUVRAM
			if vram == "" {
				vram = "-"
			}

			// Format created time
			created := instance.CreatedAt.Format("2006-01-02 15:04")

			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				instance.ID,
				instance.Name,
				instance.Status,
				vram,
				created,
			)
		}

		w.Flush()
		fmt.Println()
		fmt.Printf("Total: %d instance(s)\n", response.Count)
	}
}
