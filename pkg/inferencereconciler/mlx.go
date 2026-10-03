// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package inferencereconciler

import (
	"fmt"

	"github.com/FlashbackAi/teepin-core/pkg/cluster"
	"github.com/FlashbackAi/teepin-core/pkg/inferencegateway"
	"github.com/FlashbackAi/teepin-core/pkg/nativeruntime"
)

const (
	engineMLX = "mlx"

	// defaultMLXCommand is mlx-lm's own OpenAI-compatible server entry
	// point, installed on the Mac by the modeld bootstrap script.
	defaultMLXCommand = "mlx_lm.server"
)

// buildMLXSpec builds the InstanceSpec for an MLX model on a native
// (teepin-modeld) node. MLX needs Metal, so it runs as a host process, not
// a container: no image, storage volume, or init container. The model is
// always a HuggingFace repo id — mlx-lm downloads it itself on first
// request — so a non-HF source is rejected rather than half-supported.
//
// The listen address is loopback only: mlx_lm.server has no auth flag, so
// it must never be reachable except through the agent's tunnel.
func buildMLXSpec(instanceID string, cfg inferencegateway.ModelServiceConfig, images engineConfig, nodeName, providerID string) (cluster.InstanceSpec, error) {
	repo, isHF, err := parseModelSource(cfg.ModelSource)
	if err != nil {
		return cluster.InstanceSpec{}, err
	}
	if !isHF {
		return cluster.InstanceSpec{}, fmt.Errorf(
			"engine %q requires a huggingface.co model_source; %q is not one", engineMLX, cfg.ModelSource)
	}

	command := images.MLXCommand
	if command == "" {
		command = defaultMLXCommand
	}

	return cluster.InstanceSpec{
		InstanceID: instanceID,
		Command:    []string{command},
		Args: []string{
			"--model", repo,
			"--host", "127.0.0.1",
			"--port", nativeruntime.PortPlaceholder,
			"--trust-remote-code",
		},
		NodeName:   nodeName,
		NodeClass:  "home",
		ProviderID: providerID,
		Labels:     map[string]string{hiddenLabel: "true"},
		Ports:      []cluster.PortMapping{{Container: servePort, Protocol: "tcp"}},
	}, nil
}
