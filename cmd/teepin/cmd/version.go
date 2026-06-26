// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Show version information",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("teepin version %s\n", version)
		fmt.Printf("API version: v1\n")
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
