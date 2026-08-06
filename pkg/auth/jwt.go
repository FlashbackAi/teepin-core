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
	jwt.RegisteredClaims
}

// GenerateJWT creates an access token (15 minutes) and refresh token
// (7 days) for a user, carrying their account and role.
func GenerateJWT(user *User, accountAlias, secret string) (accessToken, refreshToken string, err error) {
	newClaims := func(ttl time.Duration) Claims {
		return Claims{
			UserID:       user.ID,
			Email:        user.Email,
			AccountID:    user.AccountID,
			AccountAlias: accountAlias,
			Role:         user.Role,
			RegisteredClaims: jwt.RegisteredClaims{
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
				IssuedAt:  jwt.NewNumericDate(time.Now()),
				Issuer:    "teepin-api",
				Subject:   user.ID.String(),
			},
		}
	}

	accessToken, err = jwt.NewWithClaims(jwt.SigningMethodHS256, newClaims(15*time.Minute)).
		SignedString([]byte(secret))
	if err != nil {
		return "", "", err
	}

	refreshToken, err = jwt.NewWithClaims(jwt.SigningMethodHS256, newClaims(7*24*time.Hour)).
		SignedString([]byte(secret))
	if err != nil {
		return "", "", err
	}

	return accessToken, refreshToken, nil
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
