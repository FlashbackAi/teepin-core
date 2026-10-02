// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cluster

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	agentpb "github.com/FlashbackAi/teepin-core/pkg/agentpb"
)

func testAuth() RegistryAuth {
	return RegistryAuth{SecretName: "teepin-kumbha-ecr", Server: "880254196251.dkr.ecr.us-east-1.amazonaws.com", Username: "AWS", Password: "token-1"}
}

func TestRegistryAuth_Validate(t *testing.T) {
	if err := testAuth().Validate(); err != nil {
		t.Fatalf("a complete credential: %v", err)
	}
	for name, mut := range map[string]func(*RegistryAuth){
		"no secret name": func(a *RegistryAuth) { a.SecretName = "" },
		"no server":      func(a *RegistryAuth) { a.Server = "" },
		"no username":    func(a *RegistryAuth) { a.Username = "" },
		"no password":    func(a *RegistryAuth) { a.Password = "" },
	} {
		a := testAuth()
		mut(&a)
		if a.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestApplyRegistryAuth_CreatesThenReplacesThePullSecret(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	c := NewDirectClient(k8s, nil, nil, "nvidia")
	ctx := context.Background()

	if err := c.ApplyRegistryAuth(ctx, testAuth()); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := k8s.CoreV1().Secrets(workloadNamespace).Get(ctx, "teepin-kumbha-ecr", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("secret not stored: %v", err)
	}
	if got.Type != corev1.SecretTypeDockerConfigJson {
		t.Errorf("type = %s, want dockerconfigjson", got.Type)
	}
	var cfg struct {
		Auths map[string]struct{ Username, Password, Auth string } `json:"auths"`
	}
	if err := json.Unmarshal(got.Data[corev1.DockerConfigJsonKey], &cfg); err != nil {
		t.Fatalf("content is not docker config JSON: %v", err)
	}
	e := cfg.Auths["880254196251.dkr.ecr.us-east-1.amazonaws.com"]
	if e.Username != "AWS" || e.Password != "token-1" || e.Auth != base64.StdEncoding.EncodeToString([]byte("AWS:token-1")) {
		t.Errorf("entry = %+v", e)
	}

	// A later credential replaces it in place.
	next := testAuth()
	next.Password = "token-2"
	if err := c.ApplyRegistryAuth(ctx, next); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ = k8s.CoreV1().Secrets(workloadNamespace).Get(ctx, "teepin-kumbha-ecr", metav1.GetOptions{})
	_ = json.Unmarshal(got.Data[corev1.DockerConfigJsonKey], &cfg)
	if cfg.Auths["880254196251.dkr.ecr.us-east-1.amazonaws.com"].Password != "token-2" {
		t.Error("the secret still holds the old token")
	}
}

func TestApplyRegistryAuth_ReplacesASecretOfTheWrongType(t *testing.T) {
	// The node's old hand-made secret may be an Opaque one; a secret's type
	// cannot be edited, so it is replaced.
	k8s := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "teepin-kumbha-ecr", Namespace: workloadNamespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"x": []byte("y")},
	})
	c := NewDirectClient(k8s, nil, nil, "nvidia")
	if err := c.ApplyRegistryAuth(context.Background(), testAuth()); err != nil {
		t.Fatal(err)
	}
	got, _ := k8s.CoreV1().Secrets(workloadNamespace).Get(context.Background(), "teepin-kumbha-ecr", metav1.GetOptions{})
	if got.Type != corev1.SecretTypeDockerConfigJson {
		t.Errorf("type = %s", got.Type)
	}
}

func TestApplyRegistryAuth_RefusesAnIncompleteCredentialAndTouchesNothing(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	c := NewDirectClient(k8s, nil, nil, "nvidia")
	bad := testAuth()
	bad.Password = ""
	if err := c.ApplyRegistryAuth(context.Background(), bad); err == nil {
		t.Fatal("accepted")
	}
	if list, _ := k8s.CoreV1().Secrets(workloadNamespace).List(context.Background(), metav1.ListOptions{}); len(list.Items) != 0 {
		t.Errorf("a secret was written for an invalid credential")
	}
}

func TestRegistryPushRegistryAuth(t *testing.T) {
	reg := NewRegistry()
	if err := reg.PushRegistryAuth(context.Background(), "nobody", testAuth()); !errors.Is(err, ErrProviderOffline) {
		t.Fatalf("offline: %v", err)
	}
	sentCh := make(chan *agentpb.ControlMessage, 1)
	session := NewAgentSession("prov", "r", "v", "", func(m *agentpb.ControlMessage) error { sentCh <- m; return nil })
	reg.Add(session)

	run := func(res *agentpb.CommandResult) (*agentpb.ControlMessage, error) {
		go func() { m := <-sentCh; sentCh <- m; session.deliverResult(m.RequestId, res) }()
		err := reg.PushRegistryAuth(context.Background(), "prov", testAuth())
		return <-sentCh, err
	}
	msg, err := run(&agentpb.CommandResult{Success: true})
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	cmd := msg.GetRegistryAuth()
	if cmd.GetSecretName() != "teepin-kumbha-ecr" || cmd.GetServer() != testAuth().Server || cmd.GetPassword() != "token-1" {
		t.Errorf("sent %+v", cmd)
	}
	if _, err := run(&agentpb.CommandResult{ErrorMessage: "cannot write secret", ErrorCode: agentpb.ErrorCode_ERROR_CODE_CLUSTER_ERROR}); err == nil {
		t.Error("an agent failure must surface")
	}
	if err := reg.PushRegistryAuth(context.Background(), "prov", RegistryAuth{}); err == nil {
		t.Error("an invalid credential must be refused before it is sent")
	}
}

func TestRegistry_OnConnectHookRunsForEachNewAgent(t *testing.T) {
	reg := NewRegistry()
	var n atomic.Int32
	got := make(chan string, 4)
	reg.SetOnConnect(func(id string) { n.Add(1); got <- id })
	reg.Add(NewAgentSession("p1", "r", "v", "", func(*agentpb.ControlMessage) error { return nil }))
	reg.Add(NewAgentSession("p2", "r", "v", "", func(*agentpb.ControlMessage) error { return nil }))
	for i := 0; i < 2; i++ {
		select {
		case <-got:
		case <-time.After(2 * time.Second):
			t.Fatalf("hook ran %d times, want 2", n.Load())
		}
	}
	ids := reg.ProviderIDs()
	if len(ids) != 2 {
		t.Errorf("ProviderIDs = %v", ids)
	}
}
