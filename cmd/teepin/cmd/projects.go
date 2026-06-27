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
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var projectsCmd = &cobra.Command{
	Use:   "projects",
	Short: "Manage TEEPIN projects",
	Long:  `List and manage your TEEPIN projects (workspaces)`,
}

var projectsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all projects",
	Long: `List all projects in your account.

Examples:
  teepin projects list
  teepin projects list -o json
`,
	Run: runProjectsList,
}

var projectsCreateCmd = &cobra.Command{
	Use:   "create NAME",
	Short: "Create a new project",
	Long: `Create a new project (workspace).

Examples:
  teepin projects create "Production"
  teepin projects create "Development" --description "Dev environment"
`,
	Args: cobra.ExactArgs(1),
	Run:  runProjectsCreate,
}

var projectDescription string

func init() {
	rootCmd.AddCommand(projectsCmd)
	projectsCmd.AddCommand(projectsListCmd)
	projectsCmd.AddCommand(projectsCreateCmd)

	projectsCreateCmd.Flags().StringVar(&projectDescription, "description", "", "Project description")
}

func runProjectsList(cmd *cobra.Command, args []string) {
	apiKey, err := loadAPIKey()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Not authenticated. Please run: teepin auth login\n")
		os.Exit(1)
	}

	apiURL := getAPIURL()
	req, _ := http.NewRequest("GET", apiURL+"/v1/projects", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to list projects: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		var errResp map[string]interface{}
		json.Unmarshal(body, &errResp)
		fmt.Fprintf(os.Stderr, "❌ Failed to list projects: %v\n", errResp["error"])
		os.Exit(1)
	}

	var result map[string]interface{}
	json.Unmarshal(body, &result)

	if output == "json" {
		fmt.Println(string(body))
		return
	}

	// Table output
	projects := result["projects"].([]interface{})
	if len(projects) == 0 {
		fmt.Println("No projects found. Create one with: teepin projects create <name>")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tSLUG\tCREATED")
	fmt.Fprintln(w, "--\t----\t----\t-------")

	for _, p := range projects {
		project := p.(map[string]interface{})
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			project["id"],
			project["name"],
			project["slug"],
			project["created_at"],
		)
	}
	w.Flush()
}

func runProjectsCreate(cmd *cobra.Command, args []string) {
	name := args[0]

	apiKey, err := loadAPIKey()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Not authenticated. Please run: teepin auth login\n")
		os.Exit(1)
	}

	projectID, err := createProjectWithKey(apiKey, name, projectDescription)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to create project: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("✅ Project created successfully!")
	fmt.Printf("   ID: %s\n", projectID)
	fmt.Printf("   Name: %s\n", name)
}

func createProjectWithKey(apiKey, name, description string) (string, error) {
	apiURL := getAPIURL()
	reqBody := map[string]string{
		"name":        name,
		"description": description,
	}

	jsonData, _ := json.Marshal(reqBody)
	req, _ := http.NewRequest("POST", apiURL+"/v1/projects", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Body = io.NopCloser(bytes.NewBuffer(jsonData))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusCreated {
		var errResp map[string]interface{}
		json.Unmarshal(body, &errResp)
		return "", fmt.Errorf("%v", errResp["error"])
	}

	var project map[string]interface{}
	json.Unmarshal(body, &project)

	return project["id"].(string), nil
}

func loadAPIKey() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	credFile := home + "/.teepin/credentials"
	data, err := os.ReadFile(credFile)
	if err != nil {
		return "", err
	}

	// Simple parsing of "api_key: xxx" format
	var apiKey string
	fmt.Sscanf(string(data), "api_key: %s", &apiKey)

	if apiKey == "" {
		return "", fmt.Errorf("no API key found")
	}

	return apiKey, nil
}
