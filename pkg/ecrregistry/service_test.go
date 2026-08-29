// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package ecrregistry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/google/uuid"
)

// fakeECRAPI is a minimal ecrAPI double — records calls and returns
// scripted responses, no real AWS access.
type fakeECRAPI struct {
	describeOut *ecr.DescribeRepositoriesOutput
	describeErr error
	createOut   *ecr.CreateRepositoryOutput
	createErr   error
	tokenOut    *ecr.GetAuthorizationTokenOutput
	tokenErr    error

	createCalls []string // repository names CreateRepository was called with
}

func (f *fakeECRAPI) GetAuthorizationToken(context.Context, *ecr.GetAuthorizationTokenInput, ...func(*ecr.Options)) (*ecr.GetAuthorizationTokenOutput, error) {
	return f.tokenOut, f.tokenErr
}

func (f *fakeECRAPI) DescribeRepositories(context.Context, *ecr.DescribeRepositoriesInput, ...func(*ecr.Options)) (*ecr.DescribeRepositoriesOutput, error) {
	return f.describeOut, f.describeErr
}

func (f *fakeECRAPI) CreateRepository(_ context.Context, params *ecr.CreateRepositoryInput, _ ...func(*ecr.Options)) (*ecr.CreateRepositoryOutput, error) {
	f.createCalls = append(f.createCalls, aws.ToString(params.RepositoryName))
	return f.createOut, f.createErr
}

func TestImagePrefix_ReusesExistingRepository(t *testing.T) {
	api := &fakeECRAPI{
		describeOut: &ecr.DescribeRepositoriesOutput{
			Repositories: []types.Repository{{RepositoryUri: aws.String("123.dkr.ecr.us-east-1.amazonaws.com/teepin/kumbha-builds-dev")}},
		},
	}
	s := &Service{client: api, RepositoryName: "teepin/kumbha-builds-dev"}

	uri, err := s.ImagePrefix(context.Background(), uuid.New(), "unused")
	if err != nil {
		t.Fatalf("ImagePrefix: %v", err)
	}
	if uri != "123.dkr.ecr.us-east-1.amazonaws.com/teepin/kumbha-builds-dev" {
		t.Errorf("uri = %q, want the existing repository's URI", uri)
	}
	if len(api.createCalls) != 0 {
		t.Errorf("CreateRepository was called for an already-existing repository: %v", api.createCalls)
	}
}

func TestImagePrefix_CreatesRepositoryWhenNotFound(t *testing.T) {
	api := &fakeECRAPI{
		describeErr: &types.RepositoryNotFoundException{},
		createOut: &ecr.CreateRepositoryOutput{
			Repository: &types.Repository{RepositoryUri: aws.String("123.dkr.ecr.us-east-1.amazonaws.com/teepin/kumbha-builds-dev")},
		},
	}
	s := &Service{client: api, RepositoryName: "teepin/kumbha-builds-dev"}

	uri, err := s.ImagePrefix(context.Background(), uuid.New(), "unused")
	if err != nil {
		t.Fatalf("ImagePrefix: %v", err)
	}
	if uri != "123.dkr.ecr.us-east-1.amazonaws.com/teepin/kumbha-builds-dev" {
		t.Errorf("uri = %q, want the newly created repository's URI", uri)
	}
	if len(api.createCalls) != 1 || api.createCalls[0] != "teepin/kumbha-builds-dev" {
		t.Errorf("CreateRepository calls = %v, want exactly one for teepin/kumbha-builds-dev", api.createCalls)
	}
}

func TestImagePrefix_CreateRepositoryUsesMutableTags(t *testing.T) {
	// pkg/build.Request.Tag's own contract: repeated builds within one
	// session overwrite the same tag rather than accumulating. An
	// IMMUTABLE repository would reject that second push outright.
	api := &fakeECRAPI{
		describeErr: &types.RepositoryNotFoundException{},
	}
	var captured *ecr.CreateRepositoryInput
	// Wrap CreateRepository to capture the full input, not just the name.
	capturing := &capturingECRAPI{fakeECRAPI: api, onCreate: func(in *ecr.CreateRepositoryInput) { captured = in }}
	s := &Service{client: capturing, RepositoryName: "teepin/kumbha-builds-dev"}

	if _, err := s.ImagePrefix(context.Background(), uuid.New(), "unused"); err != nil {
		t.Fatalf("ImagePrefix: %v", err)
	}
	if captured == nil {
		t.Fatal("CreateRepository was never called")
	}
	if captured.ImageTagMutability != types.ImageTagMutabilityMutable {
		t.Errorf("ImageTagMutability = %v, want MUTABLE", captured.ImageTagMutability)
	}
}

// capturingECRAPI wraps fakeECRAPI to capture the CreateRepositoryInput
// passed in, since fakeECRAPI itself only records the repository name.
type capturingECRAPI struct {
	*fakeECRAPI
	onCreate func(*ecr.CreateRepositoryInput)
}

func (c *capturingECRAPI) CreateRepository(ctx context.Context, params *ecr.CreateRepositoryInput, optFns ...func(*ecr.Options)) (*ecr.CreateRepositoryOutput, error) {
	c.onCreate(params)
	if c.fakeECRAPI.createOut != nil {
		return c.fakeECRAPI.createOut, c.fakeECRAPI.createErr
	}
	return &ecr.CreateRepositoryOutput{
		Repository: &types.Repository{RepositoryUri: aws.String("123.dkr.ecr.us-east-1.amazonaws.com/teepin/kumbha-builds-dev")},
	}, nil
}

func TestImagePrefix_PropagatesAmbiguousDescribeError(t *testing.T) {
	api := &fakeECRAPI{describeErr: errors.New("network blip")}
	s := &Service{client: api, RepositoryName: "teepin/kumbha-builds-dev"}

	_, err := s.ImagePrefix(context.Background(), uuid.New(), "unused")
	if err == nil {
		t.Fatal("got nil error for an ambiguous describe failure, want it propagated")
	}
	if len(api.createCalls) != 0 {
		t.Error("CreateRepository was called despite an ambiguous (not-not-found) describe error")
	}
}

func TestDockerConfigJSONForBuild_DecodesTokenIntoUsernamePassword(t *testing.T) {
	rawToken := base64.StdEncoding.EncodeToString([]byte("AWS:supersecrettoken"))
	api := &fakeECRAPI{
		tokenOut: &ecr.GetAuthorizationTokenOutput{
			AuthorizationData: []types.AuthorizationData{{
				AuthorizationToken: aws.String(rawToken),
				ProxyEndpoint:      aws.String("https://123.dkr.ecr.us-east-1.amazonaws.com"),
				ExpiresAt:          aws.Time(time.Now().Add(12 * time.Hour)),
			}},
		},
	}
	s := &Service{client: api, RepositoryName: "teepin/kumbha-builds-dev"}

	raw, err := s.DockerConfigJSONForBuild(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("DockerConfigJSONForBuild: %v", err)
	}

	var cfg dockerConfigJSON
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	auth, ok := cfg.Auths["123.dkr.ecr.us-east-1.amazonaws.com"]
	if !ok {
		t.Fatalf("auths map missing the registry host, got %+v", cfg.Auths)
	}
	if auth.Username != "AWS" || auth.Password != "supersecrettoken" {
		t.Errorf("got username=%q password=%q, want AWS/supersecrettoken", auth.Username, auth.Password)
	}
	// The credential must never appear as plain, un-base64'd text
	// anywhere in the raw JSON beyond the intentional Password field —
	// i.e. this is exactly one occurrence, not leaked a second time.
	if strings.Count(raw, "supersecrettoken") != 1 {
		t.Errorf("expected the raw token to appear exactly once in the encoded config, got %d times", strings.Count(raw, "supersecrettoken"))
	}
}

// TestImageAuth_DecodesTokenIntoUsernamePassword covers the same token
// decode as TestDockerConfigJSONForBuild_DecodesTokenIntoUsernamePassword,
// via the plain-(username,password) accessor pkg/imageinfo's port-detection
// uses instead of a full .dockerconfigjson.
func TestImageAuth_DecodesTokenIntoUsernamePassword(t *testing.T) {
	rawToken := base64.StdEncoding.EncodeToString([]byte("AWS:supersecrettoken"))
	api := &fakeECRAPI{
		tokenOut: &ecr.GetAuthorizationTokenOutput{
			AuthorizationData: []types.AuthorizationData{{
				AuthorizationToken: aws.String(rawToken),
				ProxyEndpoint:      aws.String("https://123.dkr.ecr.us-east-1.amazonaws.com"),
				ExpiresAt:          aws.Time(time.Now().Add(12 * time.Hour)),
			}},
		},
	}
	s := &Service{client: api, RepositoryName: "teepin/kumbha-builds-dev"}

	username, password, err := s.ImageAuth(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("ImageAuth: %v", err)
	}
	if username != "AWS" || password != "supersecrettoken" {
		t.Errorf("got username=%q password=%q, want AWS/supersecrettoken", username, password)
	}
}

func TestImageAuth_NoAuthorizationDataIsError(t *testing.T) {
	api := &fakeECRAPI{tokenOut: &ecr.GetAuthorizationTokenOutput{}}
	s := &Service{client: api, RepositoryName: "teepin/kumbha-builds-dev"}

	if _, _, err := s.ImageAuth(context.Background(), uuid.New()); err == nil {
		t.Error("got nil error for an empty AuthorizationData response, want an error")
	}
}

func TestDockerConfigJSONForBuild_NoAuthorizationDataIsError(t *testing.T) {
	api := &fakeECRAPI{tokenOut: &ecr.GetAuthorizationTokenOutput{}}
	s := &Service{client: api, RepositoryName: "teepin/kumbha-builds-dev"}

	if _, err := s.DockerConfigJSONForBuild(context.Background(), uuid.New()); err == nil {
		t.Error("got nil error for an empty AuthorizationData response, want an error")
	}
}

func TestDockerConfigJSONForBuild_PropagatesAPIError(t *testing.T) {
	api := &fakeECRAPI{tokenErr: errors.New("access denied")}
	s := &Service{client: api, RepositoryName: "teepin/kumbha-builds-dev"}

	if _, err := s.DockerConfigJSONForBuild(context.Background(), uuid.New()); err == nil {
		t.Error("got nil error for a failed GetAuthorizationToken call, want it propagated")
	}
}

func TestNewService_RejectsEmptyRepositoryName(t *testing.T) {
	if _, err := NewService(context.Background(), ""); err == nil {
		t.Error("got nil error for an empty repository name, want one")
	}
}
