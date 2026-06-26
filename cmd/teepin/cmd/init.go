// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

type Config struct {
	APIURL        string `yaml:"api_url"`
	DefaultRegion string `yaml:"default_region"`
	OutputFormat  string `yaml:"output_format"`
}

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize TEEPIN configuration",
	Long: `Initialize TEEPIN configuration on your local machine.

This creates:
  • Config directory: ~/.teepin/
  • Default config: ~/.teepin/config.yaml

After initialization, run 'teepin login' to authenticate.
`,
	Run: runInit,
}

func init() {
	rootCmd.AddCommand(initCmd)
}

func runInit(cmd *cobra.Command, args []string) {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Error: %v\n", err)
		os.Exit(1)
	}

	teepinDir := filepath.Join(home, ".teepin")
	configFile := filepath.Join(teepinDir, "config.yaml")

	// Create .teepin directory
	if err := os.MkdirAll(teepinDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to create directory: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✓ Created config directory: %s\n", teepinDir)

	// Create default config
	config := Config{
		APIURL:        "http://localhost:8080", // TODO: Change to https://api.teepin.cloud in production
		DefaultRegion: "us-west-1",
		OutputFormat:  "table",
	}

	data, err := yaml.Marshal(&config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to marshal config: %v\n", err)
		os.Exit(1)
	}

	if err := os.WriteFile(configFile, data, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to write config: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✓ Generated default config: %s\n", configFile)

	fmt.Println("✓ Ready to authenticate")
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Println("  1. Run 'teepin login' to authenticate")
	fmt.Println("  2. Run 'teepin types' to see available instance types")
	fmt.Println("  3. Run 'teepin deploy --help' to deploy your first instance")
}
