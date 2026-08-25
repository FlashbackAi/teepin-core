// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Package ecrregistry provisions and authenticates access to a single,
// shared ECR repository for Kumbha build output — the ECR-backed
// implementation of pkg/build.RegistryProvider, chosen over standing up a
// self-hosted Harbor server on any deployment where Harbor is not already
// configured. Same reasoning ROADMAP.md's 2026-08-23 decision already
// applied to the Kumbha agent image itself: ECR is already live
// infrastructure (control-plane and kumbha-agent both already push/pull
// through it), so reusing it here is less new infrastructure to build and
// operate than a second registry server would be.
//
// Unlike Harbor's robot-account model (a long-lived, per-project
// credential created once and stored encrypted in the database — see
// pkg/harbor.Service.DockerConfigJSONForBuild), ECR authentication is
// minted fresh on every build: ecr:GetAuthorizationToken returns a token
// valid ~12 hours, scoped by the CALLER'S OWN IAM identity (the control
// plane's ECS task role) rather than anything project-specific. There is
// nothing here to provision, store, encrypt, or revoke per project — the
// tradeoff is that access is registry-wide (any push permission granted
// to the task role applies to every project's build alike), not isolated
// per project the way a Harbor Project/robot-account pair is. Judged
// acceptable because Kaniko's own build already runs in a pod scoped to
// one project (see pkg/build); nothing about a shared repository weakens
// an isolation boundary this platform relies on elsewhere.
package ecrregistry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/google/uuid"
)

// ecrAPI is the slice of *ecr.Client this package actually calls —
// extracted as an interface purely so tests can substitute a fake instead
// of making real AWS calls. The real *ecr.Client satisfies it
// structurally with no adapter needed.
type ecrAPI interface {
	GetAuthorizationToken(ctx context.Context, params *ecr.GetAuthorizationTokenInput, optFns ...func(*ecr.Options)) (*ecr.GetAuthorizationTokenOutput, error)
	DescribeRepositories(ctx context.Context, params *ecr.DescribeRepositoriesInput, optFns ...func(*ecr.Options)) (*ecr.DescribeRepositoriesOutput, error)
	CreateRepository(ctx context.Context, params *ecr.CreateRepositoryInput, optFns ...func(*ecr.Options)) (*ecr.CreateRepositoryOutput, error)
}

// Service implements pkg/build.RegistryProvider against ECR. One
// repository shared across every project's build images, distinguished
// by TAG (pkg/build's own Request.Tag, typically a short session id)
// rather than by repository — a per-project repository was considered
// and rejected: ECR repository creation needs its own IAM call and
// permission on every first build for a new project, multiplying what
// the task role needs against Harbor's single up-front grant, for
// isolation this platform does not otherwise rely on at the registry
// layer.
type Service struct {
	client ecrAPI
	// RepositoryName is the shared repository every project's build
	// images push into (e.g. "teepin/kumbha-builds-dev").
	RepositoryName string
}

// NewService builds an ECR-backed registry provider using the AWS SDK's
// default credential/region resolution — in production the ECS task role
// supplies temporary credentials automatically via the container
// credentials endpoint, the same "no static access keys anywhere"
// convention pkg/storage/s3 already establishes.
func NewService(ctx context.Context, repositoryName string) (*Service, error) {
	if repositoryName == "" {
		return nil, fmt.Errorf("ecrregistry: repository name is required")
	}
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("ecrregistry: load AWS config: %w", err)
	}
	return &Service{client: ecr.NewFromConfig(cfg), RepositoryName: repositoryName}, nil
}

// ImagePrefix ensures the shared build repository exists (idempotent —
// mirrors ProvisionProjectRegistry's own "already provisioned, reuse it"
// shape) and returns its pushable URI, e.g.
// "123456789012.dkr.ecr.us-east-1.amazonaws.com/teepin/kumbha-builds-dev".
// projectName is accepted only to satisfy pkg/build.RegistryProvider —
// Harbor uses it to name a per-project Project; ECR does not need it
// here, since every project shares one repository (see the Service doc
// comment on why). Tags stay MUTABLE: pkg/build's own Request.Tag doc
// comment states repeated builds within one session are meant to
// overwrite, not accumulate — an immutable repository would reject that
// second push outright.
func (s *Service) ImagePrefix(ctx context.Context, _ uuid.UUID, _ string) (string, error) {
	out, err := s.client.DescribeRepositories(ctx, &ecr.DescribeRepositoriesInput{
		RepositoryNames: []string{s.RepositoryName},
	})
	if err == nil && len(out.Repositories) > 0 {
		return aws.ToString(out.Repositories[0].RepositoryUri), nil
	}
	var notFound *types.RepositoryNotFoundException
	if err != nil && !errors.As(err, &notFound) {
		return "", fmt.Errorf("ecrregistry: describe repository: %w", err)
	}

	created, err := s.client.CreateRepository(ctx, &ecr.CreateRepositoryInput{
		RepositoryName:     aws.String(s.RepositoryName),
		ImageTagMutability: types.ImageTagMutabilityMutable,
		ImageScanningConfiguration: &types.ImageScanningConfiguration{
			ScanOnPush: true,
		},
	})
	if err != nil {
		return "", fmt.Errorf("ecrregistry: create repository: %w", err)
	}
	return aws.ToString(created.Repository.RepositoryUri), nil
}

// dockerConfigJSON/dockerAuth mirror the standard .dockerconfigjson
// format (not Harbor-specific — this is the OCI/Docker credential file
// shape any registry client reads) — defined locally rather than
// importing pkg/harbor's own copy, so this package stays independent of
// Harbor: a deployment may configure either one without the other.
type dockerConfigJSON struct {
	Auths map[string]dockerAuth `json:"auths"`
}

type dockerAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Auth     string `json:"auth"`
}

// DockerConfigJSONForBuild mints a fresh ECR authorization token (~12
// hours, scoped by the caller's own IAM role — nothing project-specific
// to look up) and returns it as a marshaled .dockerconfigjson string, the
// same shape pkg/harbor's own DockerConfigJSONForBuild returns and
// pkg/build's buildInstanceSpec writes into
// /kaniko/.docker/config.json via Env. projectID is accepted only to
// satisfy pkg/build.RegistryProvider — see the Service doc comment on why
// ECR access is not project-scoped the way Harbor's is.
func (s *Service) DockerConfigJSONForBuild(ctx context.Context, _ uuid.UUID) (string, error) {
	out, err := s.client.GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return "", fmt.Errorf("ecrregistry: get authorization token: %w", err)
	}
	if len(out.AuthorizationData) == 0 {
		return "", fmt.Errorf("ecrregistry: no authorization data returned")
	}
	data := out.AuthorizationData[0]

	// AuthorizationToken is base64("AWS:<password>") per ECR's own API
	// contract — decoded here so Username/Password are populated
	// individually, matching the shape Harbor's own robot-account
	// credentials already produce, rather than passing the pre-encoded
	// token through as an opaque blob a reader would need to already
	// know ECR's own encoding to make sense of.
	decoded, err := base64.StdEncoding.DecodeString(aws.ToString(data.AuthorizationToken))
	if err != nil {
		return "", fmt.Errorf("ecrregistry: decode authorization token: %w", err)
	}
	username, password, ok := strings.Cut(string(decoded), ":")
	if !ok {
		return "", fmt.Errorf("ecrregistry: unexpected authorization token format")
	}

	registry := strings.TrimPrefix(aws.ToString(data.ProxyEndpoint), "https://")
	authString := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	cfg := dockerConfigJSON{
		Auths: map[string]dockerAuth{
			registry: {Username: username, Password: password, Auth: authString},
		},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("ecrregistry: encode docker config: %w", err)
	}
	return string(raw), nil
}
