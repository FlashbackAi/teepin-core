package auth

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

type Claims struct {
	UserID uuid.UUID `json:"user_id"`
	Email  string    `json:"email"`
	// AccountID is the tenant boundary. Every authorisation check reads
	// it from the token, so a request can never reach another account's
	// resources even if it guesses a valid project or instance ID.
	AccountID    uuid.UUID `json:"account_id,omitempty"`
	AccountAlias string    `json:"account_alias,omitempty"`
	Role         string    `json:"role,omitempty"`
	ProjectID    uuid.UUID `json:"project_id,omitempty"`
	// SessionID is set ONLY on a Kumbha agent credential (see
	// MintSessionToken) — never on a human login token. Its presence is
	// what makes Middleware.authenticate check the session's own
	// open/closed status on every request, so the credential is revoked
	// the instant the session ends without needing a token blocklist.
	SessionID uuid.UUID `json:"session_id,omitempty"`
	// TokenType distinguishes a login access token from its refresh
	// token — added 2026-08-23 after finding both were structurally
	// identical (same claims, only ExpiresAt differed), so a 7-day
	// refresh token worked as a bearer credential on every ordinary API
	// call, silently defeating the whole point of a 15-minute access
	// token. Middleware.authenticate rejects "refresh" outright; only
	// the /v1/auth/refresh endpoint accepts one. Empty/absent (older
	// already-issued tokens, and Kumbha's own MintSessionToken credential)
	// is treated as "access" — those are distinguished by SessionID
	// instead and this field does not apply to them.
	TokenType string `json:"token_type,omitempty"`
	jwt.RegisteredClaims
}

// GenerateJWT creates an access token (15 minutes) and refresh token
// (7 days) for a user, carrying their account and role.
func GenerateJWT(user *User, accountAlias, secret string) (accessToken, refreshToken string, err error) {
	newClaims := func(ttl time.Duration, tokenType string) Claims {
		return Claims{
			UserID:       user.ID,
			Email:        user.Email,
			AccountID:    user.AccountID,
			AccountAlias: accountAlias,
			Role:         user.Role,
			TokenType:    tokenType,
			RegisteredClaims: jwt.RegisteredClaims{
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
				IssuedAt:  jwt.NewNumericDate(time.Now()),
				Issuer:    "teepin-api",
				Subject:   user.ID.String(),
				// ID (jti): RegisteredClaims' NumericDate fields truncate
				// to whole seconds, so two tokens minted for the same
				// user within the same second would otherwise be
				// byte-identical (HMAC signing is deterministic) — found
				// via a refresh-rotation test that could mint both the
				// original and the "rotated" token in the same second.
				// A random ID guarantees every mint is distinct
				// regardless of timing, independent of anything else.
				ID: uuid.NewString(),
			},
		}
	}

	accessToken, err = jwt.NewWithClaims(jwt.SigningMethodHS256, newClaims(15*time.Minute, "access")).
		SignedString([]byte(secret))
	if err != nil {
		return "", "", err
	}

	refreshToken, err = jwt.NewWithClaims(jwt.SigningMethodHS256, newClaims(7*24*time.Hour, "refresh")).
		SignedString([]byte(secret))
	if err != nil {
		return "", "", err
	}

	return accessToken, refreshToken, nil
}

// MintSessionToken creates a short-lived credential scoped to one project
// AND one Kumbha build session, for the agent workload processing that
// session to call teepin.* APIs with (KUMBHA-DESIGN.md's Topology
// section). Deliberately not GenerateJWT's login token shape: there is no
// human user behind it (UserID stays uuid.Nil, same convention exec
// tickets already use for a userless credential), it carries the
// SessionID claim that ties its validity to the session's own lifecycle,
// and it is never refreshable — a new one is minted per session, and it
// simply stops working when the session closes (see
// Middleware.authenticate's session check) or ttl elapses, whichever is
// first.
//
// Scoping this narrowly is the security-critical property: the agent
// processes customer-supplied text (a prompt-injection surface), so a
// credential valid for any project or with no expiry would be one
// prompt-injection away from being the platform's worst vulnerability.
func MintSessionToken(accountID, projectID, sessionID uuid.UUID, ttl time.Duration, secret string) (string, error) {
	claims := Claims{
		AccountID: accountID,
		ProjectID: projectID,
		SessionID: sessionID,
		Role:      RoleMember,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "teepin-kumbha",
			Subject:   sessionID.String(),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
}

// VerifyJWT validates a JWT token and returns the claims
func VerifyJWT(tokenString, secret string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return []byte(secret), nil
	})

	if err != nil {
		return nil, err
	}

	if claims, ok := token.Claims.(*Claims); ok && token.Valid {
		return claims, nil
	}

	return nil, errors.New("invalid token")
}
