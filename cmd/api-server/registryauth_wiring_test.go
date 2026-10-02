// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package main

import "testing"

func TestECRRegionOf(t *testing.T) {
	for image, want := range map[string]string{
		"880254196251.dkr.ecr.us-east-1.amazonaws.com/teepin/kumbha-agent-dev:latest": "us-east-1",
		"123456789012.dkr.ecr.eu-west-2.amazonaws.com/x:y":                            "eu-west-2",
		"docker.io/library/nginx:latest":                                              "",
		"registry.teepin.com/acme/app:v1":                                             "",
		"":                                                                            "",
		"880254196251.dkr.ecr.us-east-1.amazonaws.com":                                "", // no repository path
	} {
		if got := ecrRegionOf(image); got != want {
			t.Errorf("%q -> %q, want %q", image, got, want)
		}
	}
}
