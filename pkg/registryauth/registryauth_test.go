// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package registryauth

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecr/types"

	"github.com/FlashbackAi/teepin-core/pkg/cluster"
)

type fakeSource struct {
	mu    sync.Mutex
	calls int
	cred  Credential
	err   error
}

func (f *fakeSource) Token(context.Context) (Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.cred, f.err
}

type push struct {
	provider string
	auth     cluster.RegistryAuth
}

type fakePusher struct {
	mu        sync.Mutex
	providers []string
	pushes    []push
	failFor   map[string]int // provider -> failures left
}

func (f *fakePusher) ProviderIDs() []string { return f.providers }
func (f *fakePusher) PushRegistryAuth(_ context.Context, id string, a cluster.RegistryAuth) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n := f.failFor[id]; n > 0 {
		f.failFor[id] = n - 1
		return errors.New("agent busy")
	}
	f.pushes = append(f.pushes, push{id, a})
	return nil
}

func goodCred(now time.Time) Credential {
	return Credential{Server: "123.dkr.ecr.us-east-1.amazonaws.com", Username: "AWS", Password: "tok", ExpiresAt: now.Add(12 * time.Hour)}
}

func TestPushAll_ReachesEveryNodeAndOneFailureDoesNotStopTheRest(t *testing.T) {
	src := &fakeSource{cred: goodCred(time.Now())}
	p := &fakePusher{providers: []string{"a", "b", "c"}, failFor: map[string]int{"b": 99}}
	s := NewSyncer(p, src, []string{"teepin-build-ecr"}, time.Hour)
	s.PushAll(context.Background())
	if len(p.pushes) != 2 || p.pushes[0].provider != "a" || p.pushes[1].provider != "c" {
		t.Fatalf("pushes = %+v, want a and c", p.pushes)
	}
	got := p.pushes[0].auth
	if got.SecretName != "teepin-build-ecr" || got.Server != "123.dkr.ecr.us-east-1.amazonaws.com" || got.Username != "AWS" || got.Password != "tok" {
		t.Errorf("auth = %+v", got)
	}
}

func TestOnConnect_RetriesUntilTheAgentTakesIt(t *testing.T) {
	src := &fakeSource{cred: goodCred(time.Now())}
	p := &fakePusher{providers: []string{"n"}, failFor: map[string]int{"n": 2}}
	s := NewSyncer(p, src, []string{"x"}, time.Hour)
	s.retryDelay = []time.Duration{0, time.Millisecond, time.Millisecond}
	s.OnConnect("n")
	if len(p.pushes) != 1 {
		t.Fatalf("pushes = %d, want delivery on the third try", len(p.pushes))
	}
}

func TestOnConnect_GivesUpAfterTheRetries(t *testing.T) {
	src := &fakeSource{cred: goodCred(time.Now())}
	p := &fakePusher{failFor: map[string]int{"n": 99}}
	s := NewSyncer(p, src, []string{"x"}, time.Hour)
	s.retryDelay = []time.Duration{0, time.Millisecond}
	s.OnConnect("n") // must return, not hang
	if len(p.pushes) != 0 {
		t.Errorf("pushes = %d", len(p.pushes))
	}
}

func TestCredential_IsReusedWhileFreshAndRenewedNearItsEnd(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	src := &fakeSource{cred: goodCred(now)}
	p := &fakePusher{providers: []string{"a"}}
	s := NewSyncer(p, src, []string{"x"}, time.Hour)
	s.now = func() time.Time { return now }

	s.PushAll(context.Background())
	s.PushAll(context.Background())
	if src.calls != 1 {
		t.Errorf("token minted %d times for two pushes within its life, want 1", src.calls)
	}
	now = now.Add(9 * time.Hour) // 3 hours left: under the 4-hour floor
	src.cred = goodCred(now)
	s.PushAll(context.Background())
	if src.calls != 2 {
		t.Errorf("a credential with under 4h left must be renewed: %d mints", src.calls)
	}
}

func TestPush_NothingIsSentWhenTheTokenCannotBeMinted(t *testing.T) {
	src := &fakeSource{err: errors.New("AccessDenied")}
	p := &fakePusher{providers: []string{"a"}}
	NewSyncer(p, src, []string{"x"}, time.Hour).PushAll(context.Background())
	if len(p.pushes) != 0 {
		t.Errorf("pushes = %+v", p.pushes)
	}
}

type fakeECR struct {
	out *ecr.GetAuthorizationTokenOutput
	err error
}

func (f fakeECR) GetAuthorizationToken(context.Context, *ecr.GetAuthorizationTokenInput, ...func(*ecr.Options)) (*ecr.GetAuthorizationTokenOutput, error) {
	return f.out, f.err
}

func TestECRSource_DecodesTheToken(t *testing.T) {
	exp := time.Now().Add(12 * time.Hour)
	src := &ECRSource{client: fakeECR{out: &ecr.GetAuthorizationTokenOutput{AuthorizationData: []types.AuthorizationData{{
		AuthorizationToken: aws.String(base64.StdEncoding.EncodeToString([]byte("AWS:s3cret:with:colons"))),
		ProxyEndpoint:      aws.String("https://880254196251.dkr.ecr.us-east-1.amazonaws.com"),
		ExpiresAt:          aws.Time(exp),
	}}}}}
	c, err := src.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if c.Username != "AWS" || c.Password != "s3cret:with:colons" || c.Server != "880254196251.dkr.ecr.us-east-1.amazonaws.com" || !c.ExpiresAt.Equal(exp) {
		t.Errorf("credential = %+v", c)
	}
}

func TestECRSource_RefusesAMalformedToken(t *testing.T) {
	for name, out := range map[string]*ecr.GetAuthorizationTokenOutput{
		"empty":     {},
		"not b64":   {AuthorizationData: []types.AuthorizationData{{AuthorizationToken: aws.String("!!!"), ProxyEndpoint: aws.String("https://x")}}},
		"no colon":  {AuthorizationData: []types.AuthorizationData{{AuthorizationToken: aws.String(base64.StdEncoding.EncodeToString([]byte("nocolon"))), ProxyEndpoint: aws.String("https://x")}}},
		"no server": {AuthorizationData: []types.AuthorizationData{{AuthorizationToken: aws.String(base64.StdEncoding.EncodeToString([]byte("AWS:p")))}}},
	} {
		if _, err := (&ECRSource{client: fakeECR{out: out}}).Token(context.Background()); err == nil {
			t.Errorf("%s: a malformed token was accepted", name)
		}
	}
	if _, err := (&ECRSource{client: fakeECR{err: errors.New("denied")}}).Token(context.Background()); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Errorf("an AWS error must surface: %v", err)
	}
}

func TestPush_EverySecretTheNodePodsReferenceIsRefreshed(t *testing.T) {
	src := &fakeSource{cred: goodCred(time.Now())}
	p := &fakePusher{providers: []string{"a"}}
	// The agent's and the built apps' secrets differ; repeats and blanks are dropped.
	s := NewSyncer(p, src, []string{"agent-secret", "", "app-secret", "agent-secret"}, time.Hour)
	s.PushAll(context.Background())
	if len(p.pushes) != 2 || p.pushes[0].auth.SecretName != "agent-secret" || p.pushes[1].auth.SecretName != "app-secret" {
		t.Fatalf("pushes = %+v", p.pushes)
	}
	if src.calls != 1 {
		t.Errorf("one token must serve both secrets: %d mints", src.calls)
	}
}
