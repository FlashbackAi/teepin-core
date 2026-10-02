// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package main

import (
	"context"
	"log"
	"regexp"
	"time"

	"github.com/FlashbackAi/teepin-core/pkg/cluster"
	"github.com/FlashbackAi/teepin-core/pkg/registryauth"
)

var ecrHostRe = regexp.MustCompile(`^(\d{12})\.dkr\.ecr\.([a-z0-9-]+)\.amazonaws\.com/`)

// ecrRegionOf returns the AWS region of an ECR image reference, or "" when the
// image is not on ECR.
func ecrRegionOf(image string) string {
	if m := ecrHostRe.FindStringSubmatch(image); m != nil {
		return m[2]
	}
	return ""
}

// startRegistryAuthSync keeps each connected node's ECR pull secret fresh from
// the control plane: delivered when a node connects and again every few hours,
// so no one refreshes a token by hand on a node and a node that sleeps past the
// 12-hour window is repaired the moment it reconnects. Off unless the agent
// image lives on ECR and agents can connect. Off with
// TEEPIN_REGISTRY_AUTH_PUSH=false.
func startRegistryAuthSync(ctx context.Context, registry *cluster.Registry, agentImage string, secretNames ...string) {
	if registry == nil || !getEnvBool("TEEPIN_REGISTRY_AUTH_PUSH", true) {
		return
	}
	region := ecrRegionOf(agentImage)
	if region == "" {
		return
	}
	src, err := registryauth.NewECRSource(ctx, region)
	if err != nil {
		log.Printf("WARN: node registry credentials not pushed (could not set up ECR access): %v", err)
		return
	}
	secrets := append([]string{}, secretNames...)
	if extra := getEnv("TEEPIN_REGISTRY_AUTH_SECRET_NAME", ""); extra != "" {
		secrets = append(secrets, extra)
	}
	if len(secrets) == 0 {
		secrets = []string{"teepin-kumbha-ecr"}
	}
	syncer := registryauth.NewSyncer(registry, src, secrets, time.Duration(getEnvInt("TEEPIN_REGISTRY_AUTH_INTERVAL_MINUTES", 180))*time.Minute)
	registry.SetOnConnect(syncer.OnConnect)
	go syncer.Run(ctx)
	log.Printf("Node registry credentials: ECR login pushed to every agent as secrets %v (on connect and every few hours)", secrets)
}
