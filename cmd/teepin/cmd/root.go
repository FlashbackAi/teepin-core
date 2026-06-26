// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

const version = "0.1.0"

var (
	cfgFile string
	output  string
)

var rootCmd = &cobra.Command{
	Use:   "teepin",
	Short: "TEEPIN - GPU-accelerated cloud infrastructure",
	Long: `TEEPIN CLI - Deploy GPU-accelerated applications with exact VRAM allocation

Examples:
  # Deploy an instance
  teepin deploy pytorch/pytorch:latest --gpu-vram 25GB

  # List instances
  teepin list

  # View logs
  teepin logs inst-a82e7f3

  # Get instance details
  teepin describe inst-a82e7f3

Documentation: https://docs.teepin.cloud
`,
}

// Execute runs the root command
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	cobra.OnInitialize(initConfig)

	// Global flags
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default: $HOME/.teepin/config.yaml)")
	rootCmd.PersistentFlags().StringVarP(&output, "output", "o", "table", "output format: table, json, yaml")
}

func initConfig() {
	// Config initialization will be implemented later
	if cfgFile != "" {
		// Use config file from flag
	} else {
		// Find home directory
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

		// Use default config path
		cfgFile = home + "/.teepin/config.yaml"
	}
}
