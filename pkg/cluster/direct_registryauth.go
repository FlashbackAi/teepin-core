// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ RegistryAuthApplier = (*DirectClient)(nil)

// dockerConfigJSON is the content of a kubernetes.io/dockerconfigjson secret.
func dockerConfigJSON(auth RegistryAuth) ([]byte, error) {
	type entry struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Auth     string `json:"auth"`
	}
	return json.Marshal(map[string]map[string]entry{
		"auths": {
			auth.Server: {
				Username: auth.Username,
				Password: auth.Password,
				Auth:     base64.StdEncoding.EncodeToString([]byte(auth.Username + ":" + auth.Password)),
			},
		},
	})
}

// ApplyRegistryAuth stores a registry credential as the imagePullSecret pods in
// the workload namespace reference, creating it or replacing what is there. The
// kubelet reads the secret at each pull, so the next pull (including the retry
// of one that was failing on an expired token) uses the fresh value.
func (c *DirectClient) ApplyRegistryAuth(ctx context.Context, auth RegistryAuth) error {
	if err := auth.Validate(); err != nil {
		return err
	}
	content, err := dockerConfigJSON(auth)
	if err != nil {
		return fmt.Errorf("encode registry credential: %w", err)
	}
	secrets := c.k8s.CoreV1().Secrets(workloadNamespace)
	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:   auth.SecretName,
			Labels: map[string]string{"app.kubernetes.io/managed-by": "teepin-control-plane"},
		},
		Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{corev1.DockerConfigJsonKey: content},
	}
	existing, err := secrets.Get(ctx, auth.SecretName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if _, err := secrets.Create(ctx, desired, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create pull secret: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("read pull secret: %w", err)
	}
	// Keep what is already on it (its owner, other labels); replace the
	// credential and make sure the type is right.
	desired.ObjectMeta = existing.ObjectMeta
	if desired.Labels == nil {
		desired.Labels = map[string]string{}
	}
	desired.Labels["app.kubernetes.io/managed-by"] = "teepin-control-plane"
	if existing.Type != corev1.SecretTypeDockerConfigJson {
		// A secret's type cannot be changed in place.
		if err := secrets.Delete(ctx, auth.SecretName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("replace pull secret of the wrong type: %w", err)
		}
		desired.ObjectMeta = metav1.ObjectMeta{Name: auth.SecretName, Labels: desired.Labels}
		if _, err := secrets.Create(ctx, desired, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create pull secret: %w", err)
		}
		return nil
	}
	if _, err := secrets.Update(ctx, desired, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update pull secret: %w", err)
	}
	return nil
}
