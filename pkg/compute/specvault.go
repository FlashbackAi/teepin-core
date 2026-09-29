// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package compute

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/FlashbackAi/teepin-core/pkg/cluster"
)

// SpecVault seals an instance's launch spec for storage. The spec carries
// the customer's environment variables, which routinely include secrets, so
// it is never written in the clear: AES-256-GCM under a key derived from the
// platform encryption key with a purpose label (the same raw key is used to
// encrypt registry credentials; deriving per purpose keeps the two uses
// independent).
type SpecVault struct {
	aead cipher.AEAD
}

// NewSpecVault builds a vault from the platform encryption key. An empty key
// is an error: without one, specs must not be stored at all.
func NewSpecVault(encryptionKey string) (*SpecVault, error) {
	if encryptionKey == "" {
		return nil, errors.New("an encryption key is required to store launch specs")
	}
	sum := sha256.Sum256([]byte("teepin/instance-launch-spec/v1\x00" + encryptionKey))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &SpecVault{aead: aead}, nil
}

// Seal encrypts spec. The instance ID is bound as additional data, so a
// sealed spec cannot be replayed onto a different instance's row.
func (v *SpecVault) Seal(spec cluster.InstanceSpec) ([]byte, error) {
	plain, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("failed to encode launch spec: %w", err)
	}
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return v.aead.Seal(nonce, nonce, plain, []byte(spec.InstanceID)), nil
}

// Open decrypts a spec sealed for instanceID. A wrong key, corrupted data or
// a spec belonging to another instance all fail.
func (v *SpecVault) Open(instanceID string, sealed []byte) (cluster.InstanceSpec, error) {
	var spec cluster.InstanceSpec
	n := v.aead.NonceSize()
	if len(sealed) < n {
		return spec, errors.New("launch spec is corrupted")
	}
	plain, err := v.aead.Open(nil, sealed[:n], sealed[n:], []byte(instanceID))
	if err != nil {
		return spec, fmt.Errorf("launch spec cannot be opened: %w", err)
	}
	if err := json.Unmarshal(plain, &spec); err != nil {
		return spec, fmt.Errorf("launch spec is malformed: %w", err)
	}
	if spec.InstanceID != instanceID {
		return spec, errors.New("launch spec belongs to a different instance")
	}
	return spec, nil
}
