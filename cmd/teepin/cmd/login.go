// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

var (
	apiKey string
)

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate with TEEPIN",
	Long: `Authenticate with TEEPIN using an API key.

Examples:
  # Interactive login (prompts for API key)
  teepin login

  # Login with API key
  teepin login --api-key YOUR_API_KEY
`,
	Run: runLogin,
}

func init() {
	rootCmd.AddCommand(loginCmd)
	loginCmd.Flags().StringVar(&apiKey, "api-key", "", "API key for authentication")
}

func runLogin(cmd *cobra.Command, args []string) {
	// If no API key provided, prompt for it
	if apiKey == "" {
		fmt.Print("Enter API key: ")
		fmt.Scanln(&apiKey)
	}

	if apiKey == "" {
		fmt.Fprintf(os.Stderr, "❌ Error: API key is required\n")
		os.Exit(1)
	}

	// Save API key to credentials file
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Error: %v\n", err)
		os.Exit(1)
	}

	teepinDir := filepath.Join(home, ".teepin")
	credFile := filepath.Join(teepinDir, "credentials")

	// Ensure directory exists
	if err := os.MkdirAll(teepinDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to create directory: %v\n", err)
		os.Exit(1)
	}

	// Write API key to file
	content := fmt.Sprintf("api_key: %s\n", apiKey)
	if err := os.WriteFile(credFile, []byte(content), 0600); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to save credentials: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("✓ Authentication successful")
	fmt.Printf("✓ API key saved to %s\n", credFile)
	fmt.Println()
	fmt.Println("You can now use TEEPIN CLI commands.")
	fmt.Println("Try: teepin list")
}
