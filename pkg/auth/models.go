package auth

import (
	"time"

	"github.com/google/uuid"
)

type User struct {
	ID        uuid.UUID `json:"id"`
	AccountID uuid.UUID `json:"account_id"`
	Email     string    `json:"email"`
	// Username is set for sub-users, who sign in with
	// alias + username + password. Account owners sign in with their
	// globally-unique email and leave this empty.
	Username      string     `json:"username,omitempty"`
	Role          string     `json:"role"` // owner | admin | member | viewer
	Status        string     `json:"status,omitempty"`
	PasswordHash  string     `json:"-"` // Never expose
	FullName      string     `json:"full_name,omitempty"`
	EmailVerified bool       `json:"email_verified"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	DeletedAt     *time.Time `json:"deleted_at,omitempty"`
}

// CanManageUsers reports whether the role may invite, edit or remove
// sub-users.
func (u *User) CanManageUsers() bool {
	return u.Role == RoleOwner || u.Role == RoleAdmin
}

// CanManageBilling reports whether the role may change payment details.
// Deliberately owner-only: admins run infrastructure, they do not hold
// spending authority.
func (u *User) CanManageBilling() bool {
	return u.Role == RoleOwner
}

// CanWrite reports whether the role may create or delete resources.
func (u *User) CanWrite() bool {
	return u.Role == RoleOwner || u.Role == RoleAdmin || u.Role == RoleMember
}

type Project struct {
	ID uuid.UUID `json:"id"`
	// AccountID is the tenant that owns this project. Every query
	// touching a project must filter on it.
	AccountID uuid.UUID `json:"account_id"`
	// OwnerID records which user created the project; it confers no
	// exclusive rights, since the ACCOUNT owns the project.
	OwnerID     uuid.UUID `json:"owner_id"`
	Name        string    `json:"name"`
	Slug        string    `json:"slug"`
	Description string    `json:"description,omitempty"`
	// Environment is "dev", "staging", "prod", or empty when the customer
	// has not declared one. The console shows it as a badge next to the
	// project name everywhere, so that a destructive action in production
	// looks different from the same action in a scratch project.
	Environment string     `json:"environment,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	DeletedAt   *time.Time `json:"deleted_at,omitempty"`
}

// ValidEnvironments are the only accepted values, matching the CHECK
// constraint in migration 008.
var ValidEnvironments = []string{"dev", "staging", "prod"}

// IsValidEnvironment reports whether e may be stored. Empty is valid and
// means "not declared".
func IsValidEnvironment(e string) bool {
	if e == "" {
		return true
	}
	for _, valid := range ValidEnvironments {
		if e == valid {
			return true
		}
	}
	return false
}

type APIKey struct {
	ID uuid.UUID `json:"id"`
	// AccountID is resolved from the key's project at validation time,
	// not stored on the key — one source of truth for which tenant a
	// key belongs to.
	AccountID  uuid.UUID  `json:"account_id"`
	ProjectID  uuid.UUID  `json:"project_id"`
	UserID     uuid.UUID  `json:"user_id"`
	Name       string     `json:"name"`
	KeyHash    string     `json:"-"`          // Never expose
	KeyPrefix  string     `json:"key_prefix"` // tpk_12345678
	Scopes     []string   `json:"scopes"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

type Session struct {
	ID               uuid.UUID  `json:"id"`
	UserID           uuid.UUID  `json:"user_id"`
	RefreshTokenHash string     `json:"-"` // Never expose
	UserAgent        string     `json:"user_agent,omitempty"`
	IPAddress        string     `json:"ip_address,omitempty"`
	ExpiresAt        time.Time  `json:"expires_at"`
	CreatedAt        time.Time  `json:"created_at"`
	RevokedAt        *time.Time `json:"revoked_at,omitempty"`
}
