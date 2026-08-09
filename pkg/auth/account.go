// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Account types.
const (
	AccountTypePersonal     = "personal"
	AccountTypeOrganization = "organization"
)

// User roles within an account, most to least privileged.
const (
	RoleOwner  = "owner"  // billing authority; exactly one per account
	RoleAdmin  = "admin"  // full resource control, manages users
	RoleMember = "member" // resources in permitted projects
	RoleViewer = "viewer" // read-only
)

// Account is the tenant boundary: billing, payment methods and invoices
// attach here, and every resource belongs to exactly one account.
type Account struct {
	ID            uuid.UUID `json:"id"`
	AccountNumber string    `json:"account_number"` // "4815162342"
	Alias         string    `json:"alias"`          // "acme-corp"
	Type          string    `json:"type"`
	DisplayName   string    `json:"display_name"`

	// Organization-only; nil on personal accounts until converted.
	LegalName      *string `json:"legal_name,omitempty"`
	TaxID          *string `json:"tax_id,omitempty"`
	BillingEmail   *string `json:"billing_email,omitempty"`
	BillingAddress *string `json:"billing_address,omitempty"`
	Country        *string `json:"country,omitempty"`

	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// FormattedNumber renders the account number the way the console and
// support quote it: 4815-1623-42.
func (a *Account) FormattedNumber() string {
	n := a.AccountNumber
	if len(n) != 10 {
		return n
	}
	return fmt.Sprintf("%s-%s-%s", n[0:4], n[4:8], n[8:10])
}

// aliasRe matches a DNS-safe alias: lowercase alphanumeric and hyphens,
// not starting or ending with a hyphen. The alias appears in the
// sub-user sign-in URL (console.teepin.com/<alias>).
var aliasRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{1,61}[a-z0-9])?$`)

// reservedAliases cannot be claimed by customers: they would either
// collide with platform hostnames or impersonate TEEPIN itself.
var reservedAliases = map[string]bool{
	"api": true, "console": true, "www": true, "admin": true, "teepin": true,
	"support": true, "billing": true, "status": true, "docs": true, "help": true,
	"registry": true, "auth": true, "login": true, "signup": true, "app": true,
	"mail": true, "smtp": true, "ftp": true, "root": true, "system": true,
}

// ValidateAlias reports whether an alias is usable.
func ValidateAlias(alias string) error {
	if len(alias) < 3 {
		return fmt.Errorf("alias must be at least 3 characters")
	}
	if len(alias) > 63 {
		return fmt.Errorf("alias must be at most 63 characters")
	}
	if !aliasRe.MatchString(alias) {
		return fmt.Errorf("alias may contain only lowercase letters, numbers and hyphens, and cannot start or end with a hyphen")
	}
	if reservedAliases[alias] {
		return fmt.Errorf("alias %q is reserved", alias)
	}
	return nil
}

// generateAccountNumber returns a random 10-digit account number.
// Random rather than sequential: a sequential number would tell every
// customer how many accounts exist.
func generateAccountNumber() (string, error) {
	const digits = 10
	var sb strings.Builder
	// First digit 1-9 so the number never renders with a leading zero.
	first, err := rand.Int(rand.Reader, big.NewInt(9))
	if err != nil {
		return "", err
	}
	sb.WriteString(fmt.Sprintf("%d", first.Int64()+1))
	for i := 1; i < digits; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return "", err
		}
		sb.WriteString(fmt.Sprintf("%d", n.Int64()))
	}
	return sb.String(), nil
}

// CreateAccountRequest is the signup payload.
type CreateAccountRequest struct {
	Type        string // personal | organization
	DisplayName string // person's name, or company name
	Alias       string // optional for personal (derived when empty)
	Email       string
	Password    string
	FullName    string
}

// RegisterAccount creates an account and its owner user in one
// transaction. An account without an owner would be unreachable, and an
// owner without an account has nothing to bill — neither may exist
// alone, so a partial failure must roll back both.
func (s *Service) RegisterAccount(ctx context.Context, req CreateAccountRequest) (*Account, *User, error) {
	if req.Type != AccountTypePersonal && req.Type != AccountTypeOrganization {
		return nil, nil, fmt.Errorf("account type must be %q or %q", AccountTypePersonal, AccountTypeOrganization)
	}
	if strings.TrimSpace(req.DisplayName) == "" {
		return nil, nil, fmt.Errorf("display name is required")
	}

	alias := strings.ToLower(strings.TrimSpace(req.Alias))
	if alias == "" {
		alias = deriveAlias(req.DisplayName)
	}
	if err := ValidateAlias(alias); err != nil {
		return nil, nil, err
	}

	passwordHash, err := HashPassword(req.Password)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to hash password: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful commit

	// Account numbers are random, so a collision is possible though
	// vanishingly unlikely; retry a few times rather than failing.
	var account Account
	for attempt := 0; attempt < 5; attempt++ {
		number, err := generateAccountNumber()
		if err != nil {
			return nil, nil, fmt.Errorf("failed to generate account number: %w", err)
		}

		err = tx.QueryRowContext(ctx, `
			INSERT INTO auth.accounts (account_number, alias, type, display_name)
			VALUES ($1, $2, $3, $4)
			RETURNING id, account_number, alias, type, display_name, status, created_at, updated_at
		`, number, alias, req.Type, req.DisplayName).Scan(
			&account.ID, &account.AccountNumber, &account.Alias, &account.Type,
			&account.DisplayName, &account.Status, &account.CreatedAt, &account.UpdatedAt,
		)
		if err == nil {
			break
		}
		if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "23505" {
			if strings.Contains(pqErr.Constraint, "alias") {
				return nil, nil, fmt.Errorf("alias %q is already taken", alias)
			}
			continue // account_number collision — try another
		}
		return nil, nil, fmt.Errorf("failed to create account: %w", err)
	}
	if account.ID == uuid.Nil {
		return nil, nil, fmt.Errorf("failed to allocate an account number")
	}

	// The signup identity becomes the account owner. Owners sign in
	// with their globally-unique email, so username stays NULL.
	var user User
	err = tx.QueryRowContext(ctx, `
		INSERT INTO auth.users (account_id, email, password_hash, full_name, role)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, email, full_name, email_verified, created_at, updated_at
	`, account.ID, req.Email, passwordHash, req.FullName, RoleOwner).Scan(
		&user.ID, &user.Email, &user.FullName, &user.EmailVerified,
		&user.CreatedAt, &user.UpdatedAt,
	)
	if err != nil {
		if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "23505" {
			return nil, nil, fmt.Errorf("email already registered")
		}
		return nil, nil, fmt.Errorf("failed to create owner user: %w", err)
	}
	user.AccountID = account.ID
	user.Role = RoleOwner

	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("failed to commit: %w", err)
	}

	return &account, &user, nil
}

// deriveAlias turns a display name into a candidate alias.
// "Flashback Tech" → "flashback-tech".
func deriveAlias(displayName string) string {
	s := strings.ToLower(strings.TrimSpace(displayName))
	s = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 63 {
		s = strings.Trim(s[:63], "-")
	}
	// Pad names too short to be valid aliases ("Bo" → "bo-acct").
	if len(s) < 3 {
		s += "-acct"
	}
	return s
}

// GetAccount returns an account by ID.
func (s *Service) GetAccount(ctx context.Context, accountID uuid.UUID) (*Account, error) {
	var a Account
	err := s.db.QueryRowContext(ctx, `
		SELECT id, account_number, alias, type, display_name,
		       legal_name, tax_id, billing_email, billing_address::text, country,
		       status, created_at, updated_at
		FROM auth.accounts WHERE id = $1
	`, accountID).Scan(
		&a.ID, &a.AccountNumber, &a.Alias, &a.Type, &a.DisplayName,
		&a.LegalName, &a.TaxID, &a.BillingEmail, &a.BillingAddress, &a.Country,
		&a.Status, &a.CreatedAt, &a.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("account not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get account: %w", err)
	}
	return &a, nil
}

// GetAccountByAlias resolves an alias to an account — the first step of
// sub-user sign-in (alias + username + password).
// ListAllAccounts returns every account on the platform.
//
// OPERATOR ONLY. This deliberately has NO tenancy predicate, which is
// precisely why it must never be reachable from a customer-authenticated
// route — it exists for the admin control centre (choosing who to
// invoice) and is gated behind the operator token.
func (s *Service) ListAllAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, account_number, alias, type, display_name,
		       legal_name, tax_id, billing_email, country,
		       status, created_at, updated_at
		FROM auth.accounts
		WHERE status != 'closed'
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to list accounts: %w", err)
	}
	defer rows.Close()

	accounts := []Account{}
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.AccountNumber, &a.Alias, &a.Type,
			&a.DisplayName, &a.LegalName, &a.TaxID, &a.BillingEmail,
			&a.Country, &a.Status, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan account: %w", err)
		}
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

func (s *Service) GetAccountByAlias(ctx context.Context, alias string) (*Account, error) {
	var a Account
	err := s.db.QueryRowContext(ctx, `
		SELECT id, account_number, alias, type, display_name, status, created_at, updated_at
		FROM auth.accounts WHERE alias = $1
	`, strings.ToLower(alias)).Scan(
		&a.ID, &a.AccountNumber, &a.Alias, &a.Type, &a.DisplayName,
		&a.Status, &a.CreatedAt, &a.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("account not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get account: %w", err)
	}
	return &a, nil
}

// UpdateAccountRequest carries editable account fields. Nil leaves a
// field unchanged.
type UpdateAccountRequest struct {
	DisplayName    *string
	LegalName      *string
	TaxID          *string
	BillingEmail   *string
	BillingAddress *string
	Country        *string
}

// UpdateAccount edits account details.
func (s *Service) UpdateAccount(ctx context.Context, accountID uuid.UUID, req UpdateAccountRequest) (*Account, error) {
	_, err := s.db.ExecContext(ctx, `
		UPDATE auth.accounts SET
			display_name    = COALESCE($2, display_name),
			legal_name      = COALESCE($3, legal_name),
			tax_id          = COALESCE($4, tax_id),
			billing_email   = COALESCE($5, billing_email),
			billing_address = COALESCE($6::jsonb, billing_address),
			country         = COALESCE($7, country)
		WHERE id = $1
	`, accountID, req.DisplayName, req.LegalName, req.TaxID,
		req.BillingEmail, req.BillingAddress, req.Country)
	if err != nil {
		return nil, fmt.Errorf("failed to update account: %w", err)
	}
	return s.GetAccount(ctx, accountID)
}

// ConvertToOrganization turns a personal account into an organization.
// One-way by design: an organization with sub-users and invoices has no
// meaningful "personal" form to revert to. The account number and alias
// are preserved, so nothing customer-facing breaks.
func (s *Service) ConvertToOrganization(ctx context.Context, accountID uuid.UUID, legalName, taxID, country string) (*Account, error) {
	account, err := s.GetAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if account.Type == AccountTypeOrganization {
		return nil, fmt.Errorf("account is already an organization")
	}
	if strings.TrimSpace(legalName) == "" {
		return nil, fmt.Errorf("legal name is required to convert to an organization")
	}

	_, err = s.db.ExecContext(ctx, `
		UPDATE auth.accounts
		SET type = $2, legal_name = $3, tax_id = NULLIF($4,''), country = NULLIF($5,'')
		WHERE id = $1 AND type = $6
	`, accountID, AccountTypeOrganization, legalName, taxID, country, AccountTypePersonal)
	if err != nil {
		return nil, fmt.Errorf("failed to convert account: %w", err)
	}

	return s.GetAccount(ctx, accountID)
}
