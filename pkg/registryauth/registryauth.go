// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Package registryauth keeps every node's image-pull credential for a private
// registry (ECR) fresh from the control plane.
//
// An ECR token lasts 12 hours. Refreshing one by hand, or from a timer on each
// node, fails exactly when a laptop sleeps past the window (a monotonic timer
// stops counting while the machine is suspended) or a node is rebuilt: new pods
// then sit in ImagePullBackOff with nothing to say why. The control plane
// already holds the AWS identity that can mint the token, so it hands the
// credential to each connected node: at once when the node connects (a node
// waking from sleep reconnects) and again on a timer well inside the lifetime.
// The node stores it as the imagePullSecret its pods already reference.
package registryauth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecr"

	"github.com/FlashbackAi/teepin-core/pkg/cluster"
)

// Credential is a registry login with the time it stops working.
type Credential struct {
	Server    string
	Username  string
	Password  string
	ExpiresAt time.Time
}

// TokenSource mints a credential.
type TokenSource interface {
	Token(ctx context.Context) (Credential, error)
}

// Pusher delivers a credential to nodes (*cluster.Registry).
type Pusher interface {
	PushRegistryAuth(ctx context.Context, providerID string, auth cluster.RegistryAuth) error
	ProviderIDs() []string
}

// --- ECR ---

// ecrAPI is the slice of *ecr.Client used here, so tests can substitute a fake.
type ecrAPI interface {
	GetAuthorizationToken(ctx context.Context, params *ecr.GetAuthorizationTokenInput, optFns ...func(*ecr.Options)) (*ecr.GetAuthorizationTokenOutput, error)
}

// ECRSource mints ECR credentials from the control plane's own AWS identity
// (its ECS task role): the same grant the build pipeline already uses to push.
type ECRSource struct{ client ecrAPI }

// NewECRSource builds an ECR source for region using the default AWS
// credential chain.
func NewECRSource(ctx context.Context, region string) (*ECRSource, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return &ECRSource{client: ecr.NewFromConfig(cfg)}, nil
}

// Token returns a fresh ECR login.
func (s *ECRSource) Token(ctx context.Context) (Credential, error) {
	out, err := s.client.GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return Credential{}, fmt.Errorf("ecr: get authorization token: %w", err)
	}
	if len(out.AuthorizationData) == 0 || out.AuthorizationData[0].AuthorizationToken == nil {
		return Credential{}, errors.New("ecr: no authorization data returned")
	}
	data := out.AuthorizationData[0]
	raw, err := base64.StdEncoding.DecodeString(aws.ToString(data.AuthorizationToken))
	if err != nil {
		return Credential{}, fmt.Errorf("ecr: decode token: %w", err)
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok || user == "" || pass == "" {
		return Credential{}, errors.New("ecr: token is not user:password")
	}
	server := strings.TrimPrefix(strings.TrimPrefix(aws.ToString(data.ProxyEndpoint), "https://"), "http://")
	server = strings.TrimSuffix(server, "/")
	if server == "" {
		return Credential{}, errors.New("ecr: no registry endpoint returned")
	}
	return Credential{Server: server, Username: user, Password: pass, ExpiresAt: aws.ToTime(data.ExpiresAt)}, nil
}

// --- the syncer ---

// Syncer pushes a registry credential to every node and keeps doing so.
type Syncer struct {
	pusher      Pusher
	src         TokenSource
	secretNames []string
	interval    time.Duration

	// Overridable for tests.
	now        func() time.Time
	retryDelay []time.Duration

	mu     sync.Mutex
	cached Credential
}

// minRemaining is how much life a cached credential must have left to be reused.
const minRemaining = 4 * time.Hour

// NewSyncer builds a syncer that stores the credential under each of
// secretNames (the agent pods and the apps it builds may reference different
// secrets; empty and repeated names are dropped) and refreshes every interval (a
// few hours: well inside the 12-hour token life).
func NewSyncer(pusher Pusher, src TokenSource, secretNames []string, interval time.Duration) *Syncer {
	if interval <= 0 {
		interval = 3 * time.Hour
	}
	seen := map[string]bool{}
	var names []string
	for _, n := range secretNames {
		if n != "" && !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	return &Syncer{
		pusher: pusher, src: src, secretNames: names, interval: interval,
		now: time.Now, retryDelay: []time.Duration{0, 5 * time.Second, 20 * time.Second},
	}
}

// credential returns a usable credential, minting a new one when the cached one
// is near its end.
func (s *Syncer) credential(ctx context.Context) (Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cached.Password != "" && s.cached.ExpiresAt.Sub(s.now()) > minRemaining {
		return s.cached, nil
	}
	c, err := s.src.Token(ctx)
	if err != nil {
		return Credential{}, err
	}
	s.cached = c
	return c, nil
}

// PushTo sends the credential to one node.
func (s *Syncer) PushTo(ctx context.Context, providerID string) error {
	// An agent that never answers (an older build, a wedged node) must not hold
	// the loop up.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c, err := s.credential(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, name := range s.secretNames {
		if err := s.pusher.PushRegistryAuth(ctx, providerID, cluster.RegistryAuth{
			SecretName: name, Server: c.Server, Username: c.Username, Password: c.Password,
		}); err != nil {
			errs = append(errs, fmt.Errorf("secret %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// PushAll sends the credential to every connected node. One node failing does
// not stop the others.
func (s *Syncer) PushAll(ctx context.Context) {
	for _, id := range s.pusher.ProviderIDs() {
		if err := s.PushTo(ctx, id); err != nil {
			log.Printf("WARN: could not refresh the registry credential on node %s: %v", id, err)
		}
	}
}

// OnConnect hands a freshly connected node its credential, retrying a few times
// because the agent may still be settling when it registers. Safe to call from
// the registry's connect hook.
func (s *Syncer) OnConnect(providerID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var err error
	for _, d := range s.retryDelay {
		if d > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(d):
			}
		}
		if err = s.PushTo(ctx, providerID); err == nil {
			log.Printf("registry credential delivered to node %s on connect", providerID)
			return
		}
	}
	log.Printf("WARN: could not deliver the registry credential to node %s on connect: %v", providerID, err)
}

// Run pushes to every node shortly after start and then every interval, until
// ctx ends.
func (s *Syncer) Run(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(10 * time.Second):
	}
	s.PushAll(ctx)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.PushAll(ctx)
		}
	}
}
