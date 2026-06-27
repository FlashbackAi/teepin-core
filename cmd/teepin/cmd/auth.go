// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var authCmd = &cobra.Command{
	Use:   "auth",
	Short: "Authentication commands",
	Long:  `Manage authentication with TEEPIN (register, login)`,
}

var registerCmd = &cobra.Command{
	Use:   "register",
	Short: "Register a new TEEPIN account",
	Long: `Register a new user account with TEEPIN.

Examples:
  # Interactive registration
  teepin auth register

  # Register with flags
  teepin auth register --email user@example.com --name "John Doe"
`,
	Run: runRegister,
}

var authLoginCmd = &cobra.Command{
	Use:   "login",
	Short: "Login to TEEPIN and create API key",
	Long: `Login with email and password, create a project and API key.

This command will:
1. Login with your credentials
2. Create a project (if you don't have one)
3. Generate an API key
4. Save the API key locally

Examples:
  # Interactive login
  teepin auth login

  # Login with email
  teepin auth login --email user@example.com
`,
	Run: runAuthLogin,
}

var (
	email        string
	fullName     string
	stdinReader  *bufio.Reader
)

func init() {
	// Initialize stdin reader
	stdinReader = bufio.NewReader(os.Stdin)

	// Register commands
	rootCmd.AddCommand(authCmd)
	authCmd.AddCommand(registerCmd)
	authCmd.AddCommand(authLoginCmd)

	registerCmd.Flags().StringVar(&email, "email", "", "Email address")
	registerCmd.Flags().StringVar(&fullName, "name", "", "Full name")

	authLoginCmd.Flags().StringVar(&email, "email", "", "Email address")
}

// readLine reads a line from stdin
func readLine(prompt string) (string, error) {
	fmt.Print(prompt)
	line, err := stdinReader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("failed to read input: %w", err)
	}
	return strings.TrimSpace(line), nil
}

// readPassword reads a password from stdin, with terminal echo disabled if possible
func readPassword(prompt string) (string, error) {
	fmt.Print(prompt)

	// Check if stdin is a terminal
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		// Save terminal state
		oldState, err := term.MakeRaw(fd)
		if err == nil {
			// Successfully put terminal in raw mode
			defer term.Restore(fd, oldState)

			var password []byte
			var b [1]byte

			for {
				n, err := os.Stdin.Read(b[:])
				if err != nil {
					fmt.Println()
					return "", fmt.Errorf("failed to read password: %w", err)
				}
				if n == 0 {
					continue
				}

				// Handle different line endings
				if b[0] == '\n' || b[0] == '\r' {
					fmt.Println()
					break
				}

				// Handle backspace
				if b[0] == 127 || b[0] == 8 {
					if len(password) > 0 {
						password = password[:len(password)-1]
					}
					continue
				}

				// Ignore control characters
				if b[0] < 32 {
					continue
				}

				password = append(password, b[0])
			}

			return string(password), nil
		}
	}

	// Fallback: use buffered reader (password will be visible)
	line, err := stdinReader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("failed to read password: %w", err)
	}
	return strings.TrimSpace(line), nil
}

func runRegister(cmd *cobra.Command, args []string) {
	var err error

	// Get email if not provided
	if email == "" {
		email, err = readLine("Email: ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error: %v\n", err)
			os.Exit(1)
		}
	}

	if email == "" {
		fmt.Fprintf(os.Stderr, "❌ Error: Email is required\n")
		os.Exit(1)
	}

	// Get full name if not provided
	if fullName == "" {
		fullName, err = readLine("Full name: ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error: %v\n", err)
			os.Exit(1)
		}
	}

	// Get password
	password, err := readPassword("Password (min 8 characters): ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Error reading password: %v\n", err)
		os.Exit(1)
	}

	if len(password) < 8 {
		fmt.Fprintf(os.Stderr, "❌ Error: Password must be at least 8 characters\n")
		os.Exit(1)
	}

	// Confirm password
	confirm, err := readPassword("Confirm password: ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Error reading password: %v\n", err)
		os.Exit(1)
	}

	if password != confirm {
		fmt.Fprintf(os.Stderr, "❌ Error: Passwords do not match\n")
		os.Exit(1)
	}

	// Register user
	apiURL := getAPIURL()
	reqBody := map[string]string{
		"email":     email,
		"password":  password,
		"full_name": fullName,
	}

	jsonData, _ := json.Marshal(reqBody)
	resp, err := http.Post(apiURL+"/v1/auth/register", "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Registration failed: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusCreated {
		var errResp map[string]interface{}
		json.Unmarshal(body, &errResp)
		fmt.Fprintf(os.Stderr, "❌ Registration failed: %v\n", errResp["error"])
		os.Exit(1)
	}

	var user map[string]interface{}
	json.Unmarshal(body, &user)

	fmt.Println("✅ Registration successful!")
	fmt.Printf("✅ User ID: %s\n", user["id"])
	fmt.Printf("✅ Email: %s\n", user["email"])
	fmt.Println()
	fmt.Println("Next step: Login to create an API key")
	fmt.Println("  teepin auth login --email", email)
}

func runAuthLogin(cmd *cobra.Command, args []string) {
	var err error

	// Get email if not provided
	if email == "" {
		email, err = readLine("Email: ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error: %v\n", err)
			os.Exit(1)
		}
	}

	if email == "" {
		fmt.Fprintf(os.Stderr, "❌ Error: Email is required\n")
		os.Exit(1)
	}

	// Get password
	password, err := readPassword("Password: ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Error reading password: %v\n", err)
		os.Exit(1)
	}

	// Login
	apiURL := getAPIURL()
	reqBody := map[string]string{
		"email":    email,
		"password": password,
	}

	jsonData, _ := json.Marshal(reqBody)
	resp, err := http.Post(apiURL+"/v1/auth/login", "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Login failed: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		var errResp map[string]interface{}
		json.Unmarshal(body, &errResp)
		fmt.Fprintf(os.Stderr, "❌ Login failed: %v\n", errResp["error"])
		os.Exit(1)
	}

	var loginResp map[string]interface{}
	json.Unmarshal(body, &loginResp)

	accessToken := loginResp["access_token"].(string)

	fmt.Println("✅ Login successful!")
	fmt.Println()

	// List projects to see if user has any
	projects, err := listProjectsAPI(accessToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to list projects: %v\n", err)
		os.Exit(1)
	}

	var projectID string
	if len(projects) == 0 {
		// No projects yet - create one
		fmt.Println()
		fmt.Println("You don't have any projects yet. Let's create your first one!")
		fmt.Println("Projects help you organize your instances and resources.")
		fmt.Println()

		var projectName string
		projectName, err = readLine("Project name (e.g., Production, Development): ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error: %v\n", err)
			os.Exit(1)
		}

		if projectName == "" {
			// Use default if user just pressed enter
			username := strings.Split(email, "@")[0]
			projectName = fmt.Sprintf("%s's Project", username)
			fmt.Printf("Using default name: %s\n", projectName)
		}

		var projectDesc string
		projectDesc, err = readLine("Description (optional): ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error: %v\n", err)
			os.Exit(1)
		}

		fmt.Println()
		fmt.Println("Creating project...")
		projectID, err = createProjectAPI(accessToken, projectName, projectDesc)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Failed to create project: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✅ Project '%s' created!\n", projectName)
	} else {
		// User has existing projects
		fmt.Println()
		fmt.Println("Your projects:")
		for i, p := range projects {
			fmt.Printf("  %d. %s (%s)\n", i+1, p["name"], p["slug"])
		}
		fmt.Println()

		if len(projects) == 1 {
			// Only one project, use it automatically
			projectID = projects[0]["id"].(string)
			fmt.Printf("Using project: %s\n", projects[0]["name"])
		} else {
			// Multiple projects - let user choose
			choice, err := readLine(fmt.Sprintf("Select project (1-%d) or press Enter for first: ", len(projects)))
			if err != nil {
				fmt.Fprintf(os.Stderr, "❌ Error: %v\n", err)
				os.Exit(1)
			}

			selectedIdx := 0
			if choice != "" {
				fmt.Sscanf(choice, "%d", &selectedIdx)
				selectedIdx-- // Convert to 0-based index
				if selectedIdx < 0 || selectedIdx >= len(projects) {
					fmt.Fprintf(os.Stderr, "❌ Invalid selection\n")
					os.Exit(1)
				}
			}

			projectID = projects[selectedIdx]["id"].(string)
			fmt.Printf("Using project: %s\n", projects[selectedIdx]["name"])
		}
	}

	// Create API key
	fmt.Println()
	fmt.Println("Creating API key...")
	apiKeyValue, err := createAPIKeyAPI(accessToken, projectID, "CLI Key")
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to create API key: %v\n", err)
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
	content := fmt.Sprintf("api_key: %s\n", apiKeyValue)
	if err := os.WriteFile(credFile, []byte(content), 0600); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to save credentials: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("✅ API key created and saved!")
	fmt.Printf("✅ Credentials saved to %s\n", credFile)
	fmt.Println()
	fmt.Println("🚀 You're all set! Try deploying your first instance:")
	fmt.Println("  teepin deploy pytorch/pytorch:latest --gpu-vram 20GB")
}

// Helper functions
func getAPIURL() string {
	apiURL := os.Getenv("TEEPIN_API_URL")
	if apiURL == "" {
		apiURL = "http://localhost:8080"
	}
	return apiURL
}

func listProjectsAPI(token string) ([]map[string]interface{}, error) {
	apiURL := getAPIURL()
	req, _ := http.NewRequest("GET", apiURL+"/v1/projects", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to list projects: %s", body)
	}

	var result map[string]interface{}
	json.Unmarshal(body, &result)

	projects := []map[string]interface{}{}
	if projectList, ok := result["projects"].([]interface{}); ok {
		for _, p := range projectList {
			projects = append(projects, p.(map[string]interface{}))
		}
	}

	return projects, nil
}

func createProjectAPI(token, name, description string) (string, error) {
	apiURL := getAPIURL()
	reqBody := map[string]string{
		"name":        name,
		"description": description,
	}

	jsonData, _ := json.Marshal(reqBody)
	req, _ := http.NewRequest("POST", apiURL+"/v1/projects", bytes.NewBuffer(jsonData))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("failed to create project: %s", body)
	}

	var project map[string]interface{}
	json.Unmarshal(body, &project)

	return project["id"].(string), nil
}

func createAPIKeyAPI(token, projectID, name string) (string, error) {
	apiURL := getAPIURL()
	reqBody := map[string]interface{}{
		"name":   name,
		"scopes": []string{"instances:read", "instances:write"},
	}

	jsonData, _ := json.Marshal(reqBody)
	req, _ := http.NewRequest("POST", apiURL+"/v1/projects/"+projectID+"/api-keys", bytes.NewBuffer(jsonData))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("failed to create API key: %s", body)
	}

	var result map[string]interface{}
	json.Unmarshal(body, &result)

	return result["key"].(string), nil
}
