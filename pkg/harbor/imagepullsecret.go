// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package harbor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// DockerConfigJSON represents the structure of .dockerconfigjson
type DockerConfigJSON struct {
	Auths map[string]DockerAuth `json:"auths"`
}

// DockerAuth represents authentication for a Docker registry
type DockerAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Auth     string `json:"auth"`
}

// CreateImagePullSecret creates a Kubernetes secret for Docker registry authentication
func CreateImagePullSecret(ctx context.Context, k8sClient kubernetes.Interface, namespace, secretName, registryURL, username, password string) error {
	// Create base64 encoded auth string
	authString := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))

	// Create Docker config JSON
	dockerConfig := DockerConfigJSON{
		Auths: map[string]DockerAuth{
			registryURL: {
				Username: username,
				Password: password,
				Auth:     authString,
			},
		},
	}

	// Marshal to JSON
	dockerConfigJSON, err := json.Marshal(dockerConfig)
	if err != nil {
		return fmt.Errorf("failed to marshal docker config: %w", err)
	}

	// Create Kubernetes secret
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
			Labels: map[string]string{
				"teepin.io/managed": "true",
				"teepin.io/type":    "registry-credentials",
			},
		},
		Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: dockerConfigJSON,
		},
	}

	// Create the secret
	_, err = k8sClient.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create image pull secret: %w", err)
	}

	return nil
}

// UpdateImagePullSecret updates an existing ImagePullSecret
func UpdateImagePullSecret(ctx context.Context, k8sClient kubernetes.Interface, namespace, secretName, registryURL, username, password string) error {
	// Create base64 encoded auth string
	authString := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))

	// Create Docker config JSON
	dockerConfig := DockerConfigJSON{
		Auths: map[string]DockerAuth{
			registryURL: {
				Username: username,
				Password: password,
				Auth:     authString,
			},
		},
	}

	// Marshal to JSON
	dockerConfigJSON, err := json.Marshal(dockerConfig)
	if err != nil {
		return fmt.Errorf("failed to marshal docker config: %w", err)
	}

	// Get existing secret
	secret, err := k8sClient.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get secret: %w", err)
	}

	// Update secret data
	secret.Data = map[string][]byte{
		corev1.DockerConfigJsonKey: dockerConfigJSON,
	}

	// Update the secret
	_, err = k8sClient.CoreV1().Secrets(namespace).Update(ctx, secret, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update image pull secret: %w", err)
	}

	return nil
}

// DeleteImagePullSecret deletes an ImagePullSecret
func DeleteImagePullSecret(ctx context.Context, k8sClient kubernetes.Interface, namespace, secretName string) error {
	err := k8sClient.CoreV1().Secrets(namespace).Delete(ctx, secretName, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("failed to delete image pull secret: %w", err)
	}
	return nil
}

// GetImagePullSecret retrieves an ImagePullSecret
func GetImagePullSecret(ctx context.Context, k8sClient kubernetes.Interface, namespace, secretName string) (*corev1.Secret, error) {
	secret, err := k8sClient.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get image pull secret: %w", err)
	}
	return secret, nil
}

// AttachImagePullSecretToServiceAccount adds ImagePullSecret reference to a ServiceAccount
func AttachImagePullSecretToServiceAccount(ctx context.Context, k8sClient kubernetes.Interface, namespace, serviceAccountName, secretName string) error {
	// Get service account
	sa, err := k8sClient.CoreV1().ServiceAccounts(namespace).Get(ctx, serviceAccountName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get service account: %w", err)
	}

	// Check if secret already attached
	for _, secret := range sa.ImagePullSecrets {
		if secret.Name == secretName {
			return nil // Already attached
		}
	}

	// Add secret to ImagePullSecrets
	sa.ImagePullSecrets = append(sa.ImagePullSecrets, corev1.LocalObjectReference{
		Name: secretName,
	})

	// Update service account
	_, err = k8sClient.CoreV1().ServiceAccounts(namespace).Update(ctx, sa, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update service account: %w", err)
	}

	return nil
}
