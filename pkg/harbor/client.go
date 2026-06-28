// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package harbor

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/mittwald/goharbor-client/v5/apiv2"
	modelv2 "github.com/mittwald/goharbor-client/v5/apiv2/model"
	harborconfig "github.com/mittwald/goharbor-client/v5/apiv2/pkg/config"
)

// Client wraps the Harbor API client
type Client struct {
	baseURL  string
	username string
	client   *apiv2.RESTClient
}

// Config holds Harbor client configuration
type Config struct {
	BaseURL  string
	Username string
	Password string
}

// Project represents a Harbor project
type Project struct {
	ID          int64
	Name        string
	Public      bool
	RegistryURL string
}

// RobotAccount represents a Harbor robot account
type RobotAccount struct {
	ID          int64
	Name        string
	ProjectName string
	Token       string
	Permissions []string
}

// NewClient creates a new Harbor API client
func NewClient(config Config) (*Client, error) {
	// Validate configuration
	if config.BaseURL == "" {
		return nil, fmt.Errorf("base URL is required")
	}
	if config.Username == "" {
		return nil, fmt.Errorf("username is required")
	}
	if config.Password == "" {
		return nil, fmt.Errorf("password is required")
	}

	// Parse and validate URL
	parsedURL, err := url.Parse(config.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid base URL: %w", err)
	}

	// Create Harbor client options
	opts := &harborconfig.Options{
		PageSize: 100,
		Page:     1,
		Timeout:  60 * time.Second,
		Sort:     "",
		Query:    "",
	}

	// Create Harbor client
	harborClient, err := apiv2.NewRESTClientForHost(
		parsedURL.String(),
		config.Username,
		config.Password,
		opts,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create Harbor client: %w", err)
	}

	return &Client{
		baseURL:  config.BaseURL,
		username: config.Username,
		client:   harborClient,
	}, nil
}

// CreateProject creates a new Harbor project
func (c *Client) CreateProject(ctx context.Context, projectName string, public bool) (*Project, error) {
	// Prepare project request
	req := &modelv2.ProjectReq{
		ProjectName: projectName,
		Public:      &public,
		Metadata: &modelv2.ProjectMetadata{
			AutoScan: &[]string{"true"}[0], // Enable auto-scan by default
		},
		StorageLimit: &[]int64{-1}[0], // -1 = unlimited
	}

	// Create project
	err := c.client.NewProject(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to create project: %w", err)
	}

	// Get created project details
	project, err := c.client.GetProject(ctx, projectName)
	if err != nil {
		return nil, fmt.Errorf("failed to get created project: %w", err)
	}

	return &Project{
		ID:          int64(project.ProjectID),
		Name:        project.Name,
		Public:      project.Metadata.Public == "true",
		RegistryURL: c.baseURL,
	}, nil
}

// CreateRobotAccount creates a robot account with scoped permissions for a project
func (c *Client) CreateRobotAccount(ctx context.Context, projectName, robotName string, permissions []string) (*RobotAccount, error) {
	// Get project to verify it exists
	project, err := c.client.GetProject(ctx, projectName)
	if err != nil {
		return nil, fmt.Errorf("project not found: %w", err)
	}

	// Prepare robot account permissions
	// For TEEPIN, we need: artifact push, pull, delete + tag create
	access := []*modelv2.Access{
		{
			Resource: fmt.Sprintf("/project/%d/repository", project.ProjectID),
			Action:   "push",
		},
		{
			Resource: fmt.Sprintf("/project/%d/repository", project.ProjectID),
			Action:   "pull",
		},
		{
			Resource: fmt.Sprintf("/project/%d/repository", project.ProjectID),
			Action:   "delete",
		},
	}

	// Prepare robot account request
	duration := int64(-1) // -1 = never expires
	robotReq := &modelv2.RobotCreate{
		Name:        robotName,
		Description: fmt.Sprintf("Robot account for TEEPIN project %s", projectName),
		Duration:    duration,
		Level:       "project",
		Permissions: []*modelv2.RobotPermission{
			{
				Kind:      "project",
				Namespace: projectName,
				Access:    access,
			},
		},
	}

	// Create robot account
	robot, err := c.client.NewRobotAccount(ctx, robotReq)
	if err != nil {
		return nil, fmt.Errorf("failed to create robot account: %w", err)
	}

	return &RobotAccount{
		ID:          robot.ID,
		Name:        robot.Name,
		ProjectName: projectName,
		Token:       robot.Secret,
		Permissions: permissions,
	}, nil
}

// GetRobotAccount retrieves a robot account by ID
func (c *Client) GetRobotAccount(ctx context.Context, robotID int64) (*RobotAccount, error) {
	robot, err := c.client.GetRobotAccountByID(ctx, robotID)
	if err != nil {
		return nil, fmt.Errorf("failed to get robot account: %w", err)
	}

	return &RobotAccount{
		ID:   robot.ID,
		Name: robot.Name,
	}, nil
}

// DeleteRobotAccount deletes a robot account
func (c *Client) DeleteRobotAccount(ctx context.Context, robotID int64) error {
	err := c.client.DeleteRobotAccountByID(ctx, robotID)
	if err != nil {
		return fmt.Errorf("failed to delete robot account: %w", err)
	}
	return nil
}

// DeleteProject deletes a Harbor project
func (c *Client) DeleteProject(ctx context.Context, projectName string) error {
	err := c.client.DeleteProject(ctx, projectName)
	if err != nil {
		return fmt.Errorf("failed to delete project: %w", err)
	}
	return nil
}

// GetProject retrieves project details
func (c *Client) GetProject(ctx context.Context, projectName string) (*Project, error) {
	project, err := c.client.GetProject(ctx, projectName)
	if err != nil {
		return nil, fmt.Errorf("failed to get project: %w", err)
	}

	return &Project{
		ID:          int64(project.ProjectID),
		Name:        project.Name,
		Public:      project.Metadata.Public == "true",
		RegistryURL: c.baseURL,
	}, nil
}

// ListProjects lists all Harbor projects
func (c *Client) ListProjects(ctx context.Context) ([]*Project, error) {
	projects, err := c.client.ListProjects(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("failed to list projects: %w", err)
	}

	result := make([]*Project, 0, len(projects))
	for _, p := range projects {
		result = append(result, &Project{
			ID:          int64(p.ProjectID),
			Name:        p.Name,
			Public:      p.Metadata.Public == "true",
			RegistryURL: c.baseURL,
		})
	}

	return result, nil
}

// HealthCheck verifies Harbor API connectivity by listing projects
func (c *Client) HealthCheck(ctx context.Context) error {
	_, err := c.client.ListProjects(ctx, "")
	if err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}
	return nil
}
