package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

type Service struct {
	db        *sql.DB
	jwtSecret string
}

func NewService(db *sql.DB, jwtSecret string) *Service {
	return &Service{
		db:        db,
		jwtSecret: jwtSecret,
	}
}

// RegisterUser creates a new user account
func (s *Service) RegisterUser(ctx context.Context, email, password, fullName string) (*User, error) {
	// Hash password
	passwordHash, err := HashPassword(password)
	if err != nil {
		return nil, fmt.Errorf("failed to hash password: %w", err)
	}

	// Create user
	var user User
	query := `
		INSERT INTO auth.users (email, password_hash, full_name)
		VALUES ($1, $2, $3)
		RETURNING id, email, full_name, email_verified, created_at, updated_at
	`

	err = s.db.QueryRowContext(ctx, query, email, passwordHash, fullName).Scan(
		&user.ID, &user.Email, &user.FullName, &user.EmailVerified,
		&user.CreatedAt, &user.UpdatedAt,
	)

	if err != nil {
		if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "23505" {
			return nil, fmt.Errorf("email already exists")
		}
		return nil, fmt.Errorf("failed to create user: %w", err)
	}

	return &user, nil
}

// Login authenticates a user and returns JWT tokens
func (s *Service) Login(ctx context.Context, email, password string) (accessToken, refreshToken string, err error) {
	// Get user
	var user User
	query := `
		SELECT id, email, password_hash, full_name, email_verified, created_at, updated_at
		FROM auth.users
		WHERE email = $1 AND deleted_at IS NULL
	`

	err = s.db.QueryRowContext(ctx, query, email).Scan(
		&user.ID, &user.Email, &user.PasswordHash, &user.FullName,
		&user.EmailVerified, &user.CreatedAt, &user.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return "", "", fmt.Errorf("invalid credentials")
	}
	if err != nil {
		return "", "", fmt.Errorf("failed to get user: %w", err)
	}

	// Verify password
	if !VerifyPassword(user.PasswordHash, password) {
		return "", "", fmt.Errorf("invalid credentials")
	}

	// Generate JWT tokens
	accessToken, refreshToken, err = GenerateJWT(user.ID, user.Email, s.jwtSecret)
	if err != nil {
		return "", "", fmt.Errorf("failed to generate tokens: %w", err)
	}

	return accessToken, refreshToken, nil
}

// GetUserByID retrieves a user by ID
func (s *Service) GetUserByID(ctx context.Context, userID uuid.UUID) (*User, error) {
	var user User
	query := `
		SELECT id, email, full_name, email_verified, created_at, updated_at
		FROM auth.users
		WHERE id = $1 AND deleted_at IS NULL
	`

	err := s.db.QueryRowContext(ctx, query, userID).Scan(
		&user.ID, &user.Email, &user.FullName, &user.EmailVerified,
		&user.CreatedAt, &user.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("user not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get user: %w", err)
	}

	return &user, nil
}

// CreateProject creates a new project/workspace.
// Slugs are globally unique: the first project to claim a name gets
// the clean slug; later projects with the same name (from any owner)
// get a short random suffix — customers must never be blocked because
// someone else already named a project "production".
func (s *Service) CreateProject(ctx context.Context, ownerID uuid.UUID, name, description string) (*Project, error) {
	slug := strings.ToLower(strings.ReplaceAll(name, " ", "-"))

	project, err := s.insertProject(ctx, ownerID, name, slug, description)
	if isUniqueViolation(err) {
		suffixed := fmt.Sprintf("%s-%s", slug, uuid.New().String()[:6])
		project, err = s.insertProject(ctx, ownerID, name, suffixed, description)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to create project: %w", err)
	}

	return project, nil
}

func (s *Service) insertProject(ctx context.Context, ownerID uuid.UUID, name, slug, description string) (*Project, error) {
	var project Project
	query := `
		INSERT INTO auth.projects (owner_id, name, slug, description)
		VALUES ($1, $2, $3, $4)
		RETURNING id, owner_id, name, slug, description, created_at, updated_at
	`

	err := s.db.QueryRowContext(ctx, query, ownerID, name, slug, description).Scan(
		&project.ID, &project.OwnerID, &project.Name, &project.Slug,
		&project.Description, &project.CreatedAt, &project.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	return &project, nil
}

// isUniqueViolation reports whether err is a PostgreSQL unique
// constraint violation (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505"
}

// ListProjects lists all projects for a user
func (s *Service) ListProjects(ctx context.Context, ownerID uuid.UUID) ([]Project, error) {
	query := `
		SELECT id, owner_id, name, slug, description, created_at, updated_at
		FROM auth.projects
		WHERE owner_id = $1 AND deleted_at IS NULL
		ORDER BY created_at DESC
	`

	rows, err := s.db.QueryContext(ctx, query, ownerID)
	if err != nil {
		return nil, fmt.Errorf("failed to list projects: %w", err)
	}
	defer rows.Close()

	var projects []Project
	for rows.Next() {
		var project Project
		err := rows.Scan(
			&project.ID, &project.OwnerID, &project.Name, &project.Slug,
			&project.Description, &project.CreatedAt, &project.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan project: %w", err)
		}
		projects = append(projects, project)
	}

	return projects, nil
}

// GetProject retrieves a project by ID
func (s *Service) GetProject(ctx context.Context, projectID uuid.UUID) (*Project, error) {
	var project Project
	query := `
		SELECT id, owner_id, name, slug, description, created_at, updated_at
		FROM auth.projects
		WHERE id = $1 AND deleted_at IS NULL
	`

	err := s.db.QueryRowContext(ctx, query, projectID).Scan(
		&project.ID, &project.OwnerID, &project.Name, &project.Slug,
		&project.Description, &project.CreatedAt, &project.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("project not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get project: %w", err)
	}

	return &project, nil
}

// CreateAPIKey creates a new API key for a project
func (s *Service) CreateAPIKey(ctx context.Context, userID, projectID uuid.UUID, name string, scopes []string) (key string, apiKey *APIKey, err error) {
	// Generate API key
	key, hash, prefix, err := GenerateAPIKey()
	if err != nil {
		return "", nil, fmt.Errorf("failed to generate API key: %w", err)
	}

	// Store in database
	var createdKey APIKey
	query := `
		INSERT INTO auth.api_keys (project_id, user_id, name, key_hash, key_prefix, scopes)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, project_id, user_id, name, key_prefix, scopes, created_at
	`

	err = s.db.QueryRowContext(ctx, query, projectID, userID, name, hash, prefix, pq.Array(scopes)).Scan(
		&createdKey.ID, &createdKey.ProjectID, &createdKey.UserID, &createdKey.Name,
		&createdKey.KeyPrefix, pq.Array(&createdKey.Scopes), &createdKey.CreatedAt,
	)

	if err != nil {
		return "", nil, fmt.Errorf("failed to create API key: %w", err)
	}

	return key, &createdKey, nil
}

// ValidateAPIKey validates an API key and returns the key details
func (s *Service) ValidateAPIKey(ctx context.Context, key string) (*APIKey, error) {
	// Extract prefix
	if len(key) < 12 || !strings.HasPrefix(key, "tpk_") {
		return nil, fmt.Errorf("invalid API key format")
	}

	prefix := key[:12]

	// Get all keys with this prefix
	query := `
		SELECT id, project_id, user_id, name, key_hash, key_prefix, scopes, last_used_at, created_at
		FROM auth.api_keys
		WHERE key_prefix = $1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > NOW())
	`

	rows, err := s.db.QueryContext(ctx, query, prefix)
	if err != nil {
		return nil, fmt.Errorf("failed to query API key: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var apiKey APIKey
		err := rows.Scan(
			&apiKey.ID, &apiKey.ProjectID, &apiKey.UserID, &apiKey.Name, &apiKey.KeyHash,
			&apiKey.KeyPrefix, pq.Array(&apiKey.Scopes), &apiKey.LastUsedAt, &apiKey.CreatedAt,
		)
		if err != nil {
			continue
		}

		// Verify the key
		if VerifyAPIKey(key, apiKey.KeyHash) {
			// Update last used timestamp
			go s.updateAPIKeyLastUsed(apiKey.ID)
			return &apiKey, nil
		}
	}

	return nil, fmt.Errorf("invalid API key")
}

// updateAPIKeyLastUsed updates the last_used_at timestamp (async)
func (s *Service) updateAPIKeyLastUsed(keyID uuid.UUID) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := `UPDATE auth.api_keys SET last_used_at = NOW() WHERE id = $1`
	s.db.ExecContext(ctx, query, keyID)
}

// ListAPIKeys lists all API keys for a project
func (s *Service) ListAPIKeys(ctx context.Context, projectID uuid.UUID) ([]APIKey, error) {
	query := `
		SELECT id, project_id, user_id, name, key_prefix, scopes, last_used_at, created_at
		FROM auth.api_keys
		WHERE project_id = $1 AND revoked_at IS NULL
		ORDER BY created_at DESC
	`

	rows, err := s.db.QueryContext(ctx, query, projectID)
	if err != nil {
		return nil, fmt.Errorf("failed to list API keys: %w", err)
	}
	defer rows.Close()

	var apiKeys []APIKey
	for rows.Next() {
		var apiKey APIKey
		err := rows.Scan(
			&apiKey.ID, &apiKey.ProjectID, &apiKey.UserID, &apiKey.Name,
			&apiKey.KeyPrefix, pq.Array(&apiKey.Scopes), &apiKey.LastUsedAt, &apiKey.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan API key: %w", err)
		}
		apiKeys = append(apiKeys, apiKey)
	}

	return apiKeys, nil
}

// RevokeAPIKey revokes an API key
func (s *Service) RevokeAPIKey(ctx context.Context, keyID uuid.UUID) error {
	query := `UPDATE auth.api_keys SET revoked_at = NOW() WHERE id = $1`
	_, err := s.db.ExecContext(ctx, query, keyID)
	if err != nil {
		return fmt.Errorf("failed to revoke API key: %w", err)
	}
	return nil
}
