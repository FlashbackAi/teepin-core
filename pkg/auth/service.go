package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
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
// Login authenticates an ACCOUNT OWNER by email. Sub-users sign in with
// alias + username + password via LoginSubUser — their email is not a
// globally unique identifier.
func (s *Service) Login(ctx context.Context, email, password string) (accessToken, refreshToken string, err error) {
	var user User
	query := `
		SELECT id, account_id, email, COALESCE(username,''), role, status,
		       password_hash, COALESCE(full_name,''), email_verified, created_at, updated_at
		FROM auth.users
		WHERE email = $1 AND username IS NULL AND deleted_at IS NULL
	`

	err = s.db.QueryRowContext(ctx, query, email).Scan(
		&user.ID, &user.AccountID, &user.Email, &user.Username, &user.Role, &user.Status,
		&user.PasswordHash, &user.FullName, &user.EmailVerified,
		&user.CreatedAt, &user.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return "", "", fmt.Errorf("invalid credentials")
	}
	if err != nil {
		return "", "", fmt.Errorf("failed to get user: %w", err)
	}

	if !VerifyPassword(user.PasswordHash, password) {
		return "", "", fmt.Errorf("invalid credentials")
	}
	if user.Status == "disabled" {
		return "", "", fmt.Errorf("this login has been disabled")
	}

	return s.issueTokens(ctx, &user)
}

// issueTokens mints access and refresh tokens carrying the user's
// account context. Shared by owner and sub-user login so both paths
// produce identical claims.
func (s *Service) issueTokens(ctx context.Context, user *User) (accessToken, refreshToken string, err error) {
	var alias string
	// A missing alias must not block sign-in — the account ID in the
	// claims is what authorisation actually depends on.
	if err := s.db.QueryRowContext(ctx,
		`SELECT alias FROM auth.accounts WHERE id = $1`, user.AccountID).Scan(&alias); err != nil {
		alias = ""
	}

	accessToken, refreshToken, err = GenerateJWT(user, alias, s.jwtSecret)
	if err != nil {
		return "", "", fmt.Errorf("failed to generate tokens: %w", err)
	}
	return accessToken, refreshToken, nil
}

// Refresh exchanges a valid, non-expired refresh token for a brand new
// access/refresh pair — the piece that was missing entirely until
// 2026-08-23: a refresh token was minted and stored client-side at login,
// but nothing ever redeemed it, so an access token expiring after 15
// minutes meant a hard sign-out every 15 minutes instead of a silent
// refresh.
//
// Rotates the refresh token too (returns a new one, does not just re-mint
// an access token against the old refresh token indefinitely) — standard
// practice that bounds how long a leaked refresh token stays useful.
//
// Re-fetches the user from the database rather than trusting the embedded
// claims for another cycle: an account disabled since the refresh token
// was issued must stop minting new access tokens immediately, the same
// check Login itself makes.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (accessToken, newRefreshToken string, err error) {
	claims, err := VerifyJWT(refreshToken, s.jwtSecret)
	if err != nil {
		return "", "", fmt.Errorf("invalid or expired refresh token")
	}
	if claims.TokenType != "refresh" {
		return "", "", fmt.Errorf("not a refresh token")
	}

	var user User
	query := `
		SELECT id, account_id, email, COALESCE(username,''), role, status,
		       password_hash, COALESCE(full_name,''), email_verified, created_at, updated_at
		FROM auth.users
		WHERE id = $1 AND deleted_at IS NULL
	`
	if err := s.db.QueryRowContext(ctx, query, claims.UserID).Scan(
		&user.ID, &user.AccountID, &user.Email, &user.Username, &user.Role, &user.Status,
		&user.PasswordHash, &user.FullName, &user.EmailVerified,
		&user.CreatedAt, &user.UpdatedAt,
	); err != nil {
		return "", "", fmt.Errorf("user not found")
	}
	if user.Status == "disabled" {
		return "", "", fmt.Errorf("this login has been disabled")
	}

	return s.issueTokens(ctx, &user)
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
// CreateProject adds a project to an account. ownerID records who
// created it; the account owns it.
//
// Slugs are unique per account (migration 007), so two customers may
// each have a "production" project. A collision within one account
// falls back to a suffixed slug rather than failing the request.
func (s *Service) CreateProject(ctx context.Context, accountID, ownerID uuid.UUID, name, description string) (*Project, error) {
	slug := slugify(name)
	if slug == "" {
		return nil, fmt.Errorf("project name must contain at least one letter or number")
	}

	project, err := s.insertProject(ctx, accountID, ownerID, name, slug, description)
	if isUniqueViolation(err) {
		suffixed := fmt.Sprintf("%s-%s", slug, uuid.New().String()[:6])
		project, err = s.insertProject(ctx, accountID, ownerID, name, suffixed, description)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to create project: %w", err)
	}

	return project, nil
}

// slugify converts a project name into a URL-safe slug.
// Replacing only spaces (as this previously did) let names like
// "ACME/prod v2" produce slugs containing slashes and other characters
// that break URLs.
func slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = nonSlugChars.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 63 {
		s = strings.Trim(s[:63], "-")
	}
	return s
}

var nonSlugChars = regexp.MustCompile(`[^a-z0-9]+`)

func (s *Service) insertProject(ctx context.Context, accountID, ownerID uuid.UUID, name, slug, description string) (*Project, error) {
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO auth.projects (account_id, owner_id, name, slug, description)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+projectColumns+`
	`, accountID, ownerID, name, slug, description)

	return scanProject(row)
}

// isUniqueViolation reports whether err is a PostgreSQL unique
// constraint violation (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505"
}

// ListProjects lists all projects for a user
// projectColumns is the shared select list, so every project query
// returns the same shape and scanning cannot drift between them.
const projectColumns = `id, account_id, owner_id, name, slug,
	COALESCE(description,''), COALESCE(environment,''), created_at, updated_at`

func scanProject(row interface{ Scan(...any) error }) (*Project, error) {
	var p Project
	if err := row.Scan(&p.ID, &p.AccountID, &p.OwnerID, &p.Name, &p.Slug,
		&p.Description, &p.Environment, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

// ErrProjectNotFound is returned when a project does not exist or
// belongs to another account. Handlers map it to 404.
var ErrProjectNotFound = errors.New("project not found")

// ListProjects returns every project in an account.
//
// Scoped by ACCOUNT, not by owner: projects belong to the account, so
// every member of it sees them. Filtering by owner_id would hide a
// colleague's projects from their own teammates.
func (s *Service) ListProjects(ctx context.Context, accountID uuid.UUID) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+projectColumns+`
		FROM auth.projects
		WHERE account_id = $1 AND deleted_at IS NULL
		ORDER BY created_at DESC
	`, accountID)
	if err != nil {
		return nil, fmt.Errorf("failed to list projects: %w", err)
	}
	defer rows.Close()

	projects := []Project{}
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("failed to scan project: %w", err)
		}
		projects = append(projects, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read projects: %w", err)
	}

	return projects, nil
}

// GetProject retrieves a project by ID
// GetProject returns one project belonging to the given account.
//
// The account_id predicate is the tenancy check and is NOT optional:
// without it any authenticated caller who learned a project UUID could
// read another customer's project. A project in a different account is
// reported as not found — never as forbidden, which would confirm that
// it exists.
func (s *Service) GetProject(ctx context.Context, accountID, projectID uuid.UUID) (*Project, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+projectColumns+`
		FROM auth.projects
		WHERE id = $1 AND account_id = $2 AND deleted_at IS NULL
	`, projectID, accountID)

	p, err := scanProject(row)
	if err == sql.ErrNoRows {
		return nil, ErrProjectNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get project: %w", err)
	}
	return p, nil
}

// ProjectUpdate carries the fields a customer may change. A nil field
// means "leave unchanged", which is what distinguishes clearing a
// description from not touching it.
type ProjectUpdate struct {
	Name        *string
	Description *string
	Environment *string
}

// ErrProjectNameTaken is returned when a rename collides with another
// project in the same account. Handlers map it to 409.
var ErrProjectNameTaken = errors.New("a project with that name already exists")

// ErrInvalidEnvironment is returned for an environment outside the
// permitted set.
var ErrInvalidEnvironment = errors.New("environment must be dev, staging or prod")

// UpdateProject changes a project's mutable fields.
//
// Scoped by account_id for the same reason every other project query is:
// without it, learning a UUID would be enough to rename another
// customer's project.
//
// The slug deliberately does NOT follow a rename. It appears in API
// keys, registry paths and customer scripts, so silently changing it
// would break things the customer cannot see from this screen — the name
// is the label, the slug is the identifier.
func (s *Service) UpdateProject(ctx context.Context, accountID, projectID uuid.UUID, update ProjectUpdate) (*Project, error) {
	if update.Environment != nil && !IsValidEnvironment(*update.Environment) {
		return nil, ErrInvalidEnvironment
	}
	if update.Name != nil && strings.TrimSpace(*update.Name) == "" {
		return nil, fmt.Errorf("project name cannot be empty")
	}

	row := s.db.QueryRowContext(ctx, `
		UPDATE auth.projects
		SET name        = COALESCE($3, name),
		    description = COALESCE($4, description),
		    environment = COALESCE($5, environment),
		    updated_at  = NOW()
		WHERE id = $1 AND account_id = $2 AND deleted_at IS NULL
		RETURNING `+projectColumns,
		projectID, accountID, update.Name, update.Description, update.Environment)

	p, err := scanProject(row)
	if err == sql.ErrNoRows {
		return nil, ErrProjectNotFound
	}
	if err != nil {
		// The per-account unique index on name is the constraint a rename
		// can realistically violate; report it as a conflict the customer
		// can act on rather than a generic failure.
		if strings.Contains(err.Error(), "idx_projects_account_name") {
			return nil, ErrProjectNameTaken
		}
		return nil, fmt.Errorf("failed to update project: %w", err)
	}
	return p, nil
}

// ErrProjectHasInstances is returned when a project still holds live
// compute. Handlers map it to 409 and list the blockers.
var ErrProjectHasInstances = errors.New("project still has running instances")

// Note: DeleteProject wraps this with a count, so the customer-facing
// message reads "project still has running instances (1 instance)".

// DeleteProject soft-deletes a project.
//
// Refuses while instances are running, rather than cascading. Deleting a
// project is a naming decision; destroying running GPU workloads is not,
// and conflating them means one mistyped confirmation can end a training
// run. AWS refuses to delete a VPC with resources in it for the same
// reason. The caller is expected to surface the blocking instances so
// the customer can decide what to terminate.
//
// Soft delete because billing history references the project: a hard
// delete would either orphan usage records or destroy the evidence
// behind an invoice the customer may later query.
func (s *Service) DeleteProject(ctx context.Context, accountID, projectID uuid.UUID) error {
	// Confirms the project exists in THIS account before reporting
	// anything about its contents.
	if _, err := s.GetProject(ctx, accountID, projectID); err != nil {
		return err
	}

	var live int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM compute.instances
		WHERE project_id = $1 AND terminated_at IS NULL
	`, projectID).Scan(&live)
	if err != nil {
		return fmt.Errorf("failed to check for running instances: %w", err)
	}
	if live > 0 {
		// Phrased for the customer, who sees this verbatim. The sentinel
		// wraps it for callers that branch on the type.
		noun := "instances"
		if live == 1 {
			noun = "instance"
		}
		return fmt.Errorf("%w (%d %s)", ErrProjectHasInstances, live, noun)
	}

	// API keys are revoked with the project. Leaving them valid would
	// let a deleted project's credentials keep authenticating.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		UPDATE auth.api_keys SET revoked_at = NOW()
		WHERE project_id = $1 AND revoked_at IS NULL
	`, projectID); err != nil {
		return fmt.Errorf("failed to revoke project API keys: %w", err)
	}

	result, err := tx.ExecContext(ctx, `
		UPDATE auth.projects SET deleted_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND account_id = $2 AND deleted_at IS NULL
	`, projectID, accountID)
	if err != nil {
		return fmt.Errorf("failed to delete project: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrProjectNotFound
	}

	return tx.Commit()
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

	// The account is joined from the key's project rather than stored on
	// the key: one source of truth, so a key can never outlive or
	// disagree with the project it belongs to. Soft-deleted projects
	// yield no row, which revokes their keys implicitly.
	const query = `
		SELECT k.id, k.project_id, k.user_id, k.name, k.key_hash, k.key_prefix,
		       k.scopes, k.last_used_at, k.created_at, p.account_id
		FROM auth.api_keys k
		JOIN auth.projects p ON p.id = k.project_id AND p.deleted_at IS NULL
		WHERE k.key_prefix = $1
		  AND k.revoked_at IS NULL
		  AND (k.expires_at IS NULL OR k.expires_at > NOW())
	`

	rows, err := s.db.QueryContext(ctx, query, prefix)
	if err != nil {
		return nil, fmt.Errorf("failed to query API key: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var apiKey APIKey
		if err := rows.Scan(
			&apiKey.ID, &apiKey.ProjectID, &apiKey.UserID, &apiKey.Name, &apiKey.KeyHash,
			&apiKey.KeyPrefix, pq.Array(&apiKey.Scopes), &apiKey.LastUsedAt, &apiKey.CreatedAt,
			&apiKey.AccountID,
		); err != nil {
			// A malformed row must not mask a valid key sharing the
			// prefix; skip it and keep checking.
			continue
		}

		// Compare against every candidate with this prefix: the hash
		// comparison is constant-time, so this does not leak which
		// prefix matched.
		if VerifyAPIKey(key, apiKey.KeyHash) {
			go s.updateAPIKeyLastUsed(apiKey.ID)
			return &apiKey, nil
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read API keys: %w", err)
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
