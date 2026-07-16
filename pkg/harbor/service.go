// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package harbor

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"k8s.io/client-go/kubernetes"
)

// Service handles Harbor operations for TEEPIN
type Service struct {
	client     *Client
	k8sClient  kubernetes.Interface
	db         *sql.DB
	encryptKey []byte // 32 bytes for AES-256
}

// RegistryAccess contains registry access details for a project
type RegistryAccess struct {
	ProjectID         uuid.UUID `json:"project_id"`
	RegistryURL       string    `json:"registry_url"`
	HarborProjectName string    `json:"harbor_project_name"`
	Username          string    `json:"username"`
	Password          string    `json:"password"` // Only returned on creation
	ImagePrefix       string    `json:"image_prefix"`
	DockerLoginCmd    string    `json:"docker_login_command"`
	CreatedAt         time.Time `json:"created_at"`
}

// NewService creates a new Harbor service
func NewService(harborClient *Client, k8sClient kubernetes.Interface, db *sql.DB, encryptionKey string) *Service {
	// Use first 32 bytes of encryption key for AES-256
	key := []byte(encryptionKey)
	if len(key) > 32 {
		key = key[:32]
	} else if len(key) < 32 {
		// Pad with zeros if too short (not recommended in production)
		padded := make([]byte, 32)
		copy(padded, key)
		key = padded
	}

	return &Service{
		client:     harborClient,
		k8sClient:  k8sClient,
		db:         db,
		encryptKey: key,
	}
}

// ProvisionProjectRegistry creates Harbor project, robot account, and ImagePullSecret
func (s *Service) ProvisionProjectRegistry(ctx context.Context, projectID uuid.UUID, projectName string) (*RegistryAccess, error) {
	// Check if already provisioned
	existing, err := s.GetRegistryCredentials(ctx, projectID)
	if err == nil {
		return existing, nil // Already provisioned
	}

	// Generate Harbor project name (sanitized)
	harborProjectName := s.generateHarborProjectName(projectName, projectID)

	// 1. Create Harbor project
	project, err := s.client.CreateProject(ctx, harborProjectName, false) // private by default
	if err != nil {
		return nil, fmt.Errorf("failed to create Harbor project: %w", err)
	}

	// 2. Create robot account for the project
	robotName := fmt.Sprintf("%s-robot", harborProjectName)
	robot, err := s.client.CreateRobotAccount(ctx, harborProjectName, robotName, []string{"push", "pull", "delete"})
	if err != nil {
		// Cleanup: delete project if robot creation fails
		_ = s.client.DeleteProject(ctx, harborProjectName)
		return nil, fmt.Errorf("failed to create robot account: %w", err)
	}

	// 3. Create Kubernetes ImagePullSecret
	secretName := fmt.Sprintf("%s-registry", strings.ToLower(projectName))
	err = CreateImagePullSecret(
		ctx,
		s.k8sClient,
		"teepin", // namespace
		secretName,
		project.RegistryURL,
		robot.Name,
		robot.Token,
	)
	if err != nil {
		// Cleanup: delete robot and project
		_ = s.client.DeleteRobotAccount(ctx, robot.ID)
		_ = s.client.DeleteProject(ctx, harborProjectName)
		return nil, fmt.Errorf("failed to create image pull secret: %w", err)
	}

	// 4. Encrypt and store credentials in database
	encryptedToken, err := s.encrypt(robot.Token)
	if err != nil {
		return nil, fmt.Errorf("failed to encrypt token: %w", err)
	}

	query := `
		INSERT INTO auth.registry_credentials
		(project_id, harbor_project_name, robot_account_id, robot_account_name, docker_config_json)
		VALUES ($1, $2, $3, $4, $5)
	`

	_, err = s.db.ExecContext(ctx, query,
		projectID,
		harborProjectName,
		fmt.Sprintf("%d", robot.ID),
		robot.Name,
		encryptedToken,
	)
	if err != nil {
		// Cleanup
		_ = DeleteImagePullSecret(ctx, s.k8sClient, "teepin", secretName)
		_ = s.client.DeleteRobotAccount(ctx, robot.ID)
		_ = s.client.DeleteProject(ctx, harborProjectName)
		return nil, fmt.Errorf("failed to store credentials: %w", err)
	}

	// Return access details
	return &RegistryAccess{
		ProjectID:         projectID,
		RegistryURL:       project.RegistryURL,
		HarborProjectName: harborProjectName,
		Username:          robot.Name,
		Password:          robot.Token, // Only returned on creation
		ImagePrefix:       fmt.Sprintf("%s/%s", project.RegistryURL, harborProjectName),
		DockerLoginCmd:    fmt.Sprintf("docker login %s -u '%s' -p '<token>'", project.RegistryURL, robot.Name),
		CreatedAt:         time.Now(),
	}, nil
}

// GetRegistryCredentials retrieves registry credentials for a project
func (s *Service) GetRegistryCredentials(ctx context.Context, projectID uuid.UUID) (*RegistryAccess, error) {
	query := `
		SELECT harbor_project_name, robot_account_name, docker_config_json, created_at
		FROM auth.registry_credentials
		WHERE project_id = $1 AND revoked_at IS NULL
	`

	var harborProjectName, robotName, encryptedToken string
	var createdAt time.Time

	err := s.db.QueryRowContext(ctx, query, projectID).Scan(
		&harborProjectName,
		&robotName,
		&encryptedToken,
		&createdAt,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("registry not provisioned for project")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get credentials: %w", err)
	}

	// Get Harbor project details
	project, err := s.client.GetProject(ctx, harborProjectName)
	if err != nil {
		return nil, fmt.Errorf("failed to get Harbor project: %w", err)
	}

	return &RegistryAccess{
		ProjectID:         projectID,
		RegistryURL:       project.RegistryURL,
		HarborProjectName: harborProjectName,
		Username:          robotName,
		Password:          "", // Don't return password for security
		ImagePrefix:       fmt.Sprintf("%s/%s", project.RegistryURL, harborProjectName),
		DockerLoginCmd:    "Use API key to retrieve credentials",
		CreatedAt:         createdAt,
	}, nil
}

// RevokeRegistryAccess deletes robot account, ImagePullSecret, and marks as revoked
func (s *Service) RevokeRegistryAccess(ctx context.Context, projectID uuid.UUID) error {
	// Get credentials
	query := `
		SELECT harbor_project_name, robot_account_id
		FROM auth.registry_credentials
		WHERE project_id = $1 AND revoked_at IS NULL
	`

	var harborProjectName, robotAccountID string
	err := s.db.QueryRowContext(ctx, query, projectID).Scan(&harborProjectName, &robotAccountID)
	if err == sql.ErrNoRows {
		return fmt.Errorf("registry not found")
	}
	if err != nil {
		return fmt.Errorf("failed to get credentials: %w", err)
	}

	// Delete robot account from Harbor
	var robotID int64
	fmt.Sscanf(robotAccountID, "%d", &robotID)
	_ = s.client.DeleteRobotAccount(ctx, robotID) // Best effort

	// Delete ImagePullSecret from Kubernetes
	secretName := fmt.Sprintf("%s-registry", strings.ToLower(harborProjectName))
	_ = DeleteImagePullSecret(ctx, s.k8sClient, "teepin", secretName) // Best effort

	// Mark as revoked in database
	updateQuery := `
		UPDATE auth.registry_credentials
		SET revoked_at = NOW()
		WHERE project_id = $1
	`

	_, err = s.db.ExecContext(ctx, updateQuery, projectID)
	if err != nil {
		return fmt.Errorf("failed to mark as revoked: %w", err)
	}

	return nil
}

// generateHarborProjectName creates a sanitized Harbor project name
func (s *Service) generateHarborProjectName(projectName string, projectID uuid.UUID) string {
	// Harbor project names must be lowercase alphanumeric with hyphens
	sanitized := strings.ToLower(projectName)
	sanitized = strings.ReplaceAll(sanitized, " ", "-")
	sanitized = strings.ReplaceAll(sanitized, "_", "-")

	// Add project ID suffix to ensure uniqueness
	shortID := projectID.String()[:8]
	return fmt.Sprintf("teepin-%s-%s", sanitized, shortID)
}

// encrypt encrypts data using AES-256-GCM
func (s *Service) encrypt(plaintext string) (string, error) {
	block, err := aes.NewCipher(s.encryptKey)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}

	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// decrypt decrypts data using AES-256-GCM
func (s *Service) decrypt(ciphertext string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", err
	}

	block, err := aes.NewCipher(s.encryptKey)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertextBytes := data[:nonceSize], data[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertextBytes, nil)
	if err != nil {
		return "", err
	}

	return string(plaintext), nil
}
