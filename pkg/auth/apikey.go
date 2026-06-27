package auth

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// GenerateAPIKey creates a new API key with format: tpk_XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX
func GenerateAPIKey() (key string, hash string, prefix string, err error) {
	// Generate 32 random bytes
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", "", err
	}

	// Encode to base64 (URL-safe)
	encoded := base64.URLEncoding.EncodeToString(b)
	// Remove padding
	encoded = strings.TrimRight(encoded, "=")

	// Add prefix
	key = fmt.Sprintf("tpk_%s", encoded)
	prefix = key[:12] // tpk_12345678

	// Hash for storage
	hashBytes, err := bcrypt.GenerateFromPassword([]byte(key), bcrypt.DefaultCost)
	if err != nil {
		return "", "", "", err
	}
	hash = string(hashBytes)

	return key, hash, prefix, nil
}

// VerifyAPIKey checks if a key matches the stored hash
func VerifyAPIKey(key, hash string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(key))
	return err == nil
}
