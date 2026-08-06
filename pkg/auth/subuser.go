// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package auth

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// usernameRe matches a sub-user username: lowercase alphanumeric with
// dots, hyphens and underscores. Unique within an account, not globally.
var usernameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,63}$`)

// ValidateUsername reports whether a sub-user username is usable.
func ValidateUsername(username string) error {
	if len(username) < 2 {
		return fmt.Errorf("username must be at least 2 characters")
	}
	if !usernameRe.MatchString(username) {
		return fmt.Errorf("username may contain only lowercase letters, numbers, dots, hyphens and underscores")
	}
	return nil
}

// CreateSubUserRequest describes a new user inside an existing account.
type CreateSubUserRequest struct {
	Username string
	Email    string
	Password string
	FullName string
	Role     string
}

// CreateSubUser adds a user to an account.
//
// Sub-users sign in with account alias + username + password: their
// email is NOT a globally unique identifier, because the same person may
// hold logins in several accounts.
func (s *Service) CreateSubUser(ctx context.Context, accountID uuid.UUID, req CreateSubUserRequest) (*User, error) {
	username := strings.ToLower(strings.TrimSpace(req.Username))
	if err := ValidateUsername(username); err != nil {
		return nil, err
	}

	switch req.Role {
	case RoleAdmin, RoleMember, RoleViewer:
		// fine
	case RoleOwner:
		// Ownership transfer is a separate, deliberate operation —
		// creating a second owner would break the one-owner invariant.
		return nil, fmt.Errorf("cannot create a second owner; transfer ownership instead")
	default:
		return nil, fmt.Errorf("role must be one of %q, %q, %q", RoleAdmin, RoleMember, RoleViewer)
	}

	passwordHash, err := HashPassword(req.Password)
	if err != nil {
		return nil, fmt.Errorf("failed to hash password: %w", err)
	}

	var u User
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO auth.users (account_id, email, username, password_hash, full_name, role)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, account_id, email, COALESCE(username,''), role, status,
		          full_name, email_verified, created_at, updated_at
	`, accountID, req.Email, username, passwordHash, req.FullName, req.Role).Scan(
		&u.ID, &u.AccountID, &u.Email, &u.Username, &u.Role, &u.Status,
		&u.FullName, &u.EmailVerified, &u.CreatedAt, &u.UpdatedAt,
	)
	if err != nil {
		if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "23505" {
			return nil, fmt.Errorf("username %q already exists in this account", username)
		}
		return nil, fmt.Errorf("failed to create user: %w", err)
	}

	return &u, nil
}

// ListAccountUsers returns every user in an account.
func (s *Service) ListAccountUsers(ctx context.Context, accountID uuid.UUID) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, account_id, email, COALESCE(username,''), role, status,
		       COALESCE(full_name,''), email_verified, created_at, updated_at
		FROM auth.users
		WHERE account_id = $1 AND deleted_at IS NULL
		ORDER BY CASE role
		           WHEN 'owner' THEN 0 WHEN 'admin' THEN 1
		           WHEN 'member' THEN 2 ELSE 3 END,
		         created_at
	`, accountID)
	if err != nil {
		return nil, fmt.Errorf("failed to list users: %w", err)
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.AccountID, &u.Email, &u.Username, &u.Role,
			&u.Status, &u.FullName, &u.EmailVerified, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan user: %w", err)
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// UpdateSubUserRole changes a sub-user's role.
func (s *Service) UpdateSubUserRole(ctx context.Context, accountID, userID uuid.UUID, role string) error {
	switch role {
	case RoleAdmin, RoleMember, RoleViewer:
	default:
		return fmt.Errorf("role must be one of %q, %q, %q", RoleAdmin, RoleMember, RoleViewer)
	}

	// The account_id predicate is the tenancy check: it makes editing a
	// user in another account a no-op rather than a privilege escalation.
	res, err := s.db.ExecContext(ctx, `
		UPDATE auth.users SET role = $3
		WHERE id = $2 AND account_id = $1 AND role <> 'owner'
	`, accountID, userID, role)
	if err != nil {
		return fmt.Errorf("failed to update role: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("user not found, or is the account owner")
	}
	return nil
}

// SetSubUserStatus enables or disables a sub-user. Disabling is
// preferred over deletion: it revokes access immediately while
// preserving the audit trail of what that user did.
func (s *Service) SetSubUserStatus(ctx context.Context, accountID, userID uuid.UUID, status string) error {
	if status != "active" && status != "disabled" {
		return fmt.Errorf("status must be 'active' or 'disabled'")
	}

	res, err := s.db.ExecContext(ctx, `
		UPDATE auth.users SET status = $3
		WHERE id = $2 AND account_id = $1 AND role <> 'owner'
	`, accountID, userID, status)
	if err != nil {
		return fmt.Errorf("failed to update status: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("user not found, or is the account owner")
	}
	return nil
}

// DeleteSubUser removes a sub-user from an account. The owner cannot be
// deleted — an account without an owner has no billing authority.
func (s *Service) DeleteSubUser(ctx context.Context, accountID, userID uuid.UUID) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE auth.users SET deleted_at = NOW(), status = 'disabled'
		WHERE id = $2 AND account_id = $1 AND role <> 'owner' AND deleted_at IS NULL
	`, accountID, userID)
	if err != nil {
		return fmt.Errorf("failed to delete user: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("user not found, or is the account owner")
	}
	return nil
}

// LoginSubUser authenticates alias + username + password.
func (s *Service) LoginSubUser(ctx context.Context, alias, username, password string) (accessToken, refreshToken string, err error) {
	account, err := s.GetAccountByAlias(ctx, alias)
	if err != nil {
		// Same message for unknown alias and wrong password: revealing
		// which aliases exist invites enumeration.
		return "", "", fmt.Errorf("invalid credentials")
	}
	if account.Status != "active" {
		return "", "", fmt.Errorf("account is %s", account.Status)
	}

	var u User
	err = s.db.QueryRowContext(ctx, `
		SELECT id, account_id, email, COALESCE(username,''), role, status,
		       password_hash, COALESCE(full_name,''), email_verified
		FROM auth.users
		WHERE account_id = $1 AND username = $2 AND deleted_at IS NULL
	`, account.ID, strings.ToLower(username)).Scan(
		&u.ID, &u.AccountID, &u.Email, &u.Username, &u.Role, &u.Status,
		&u.PasswordHash, &u.FullName, &u.EmailVerified,
	)
	if err == sql.ErrNoRows {
		return "", "", fmt.Errorf("invalid credentials")
	}
	if err != nil {
		return "", "", fmt.Errorf("failed to look up user: %w", err)
	}
	if u.Status == "disabled" {
		return "", "", fmt.Errorf("this login has been disabled")
	}
	if !VerifyPassword(u.PasswordHash, password) {
		return "", "", fmt.Errorf("invalid credentials")
	}

	return s.issueTokens(ctx, &u)
}
