// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package inferencereconciler

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/FlashbackAi/teepin-core/pkg/cluster"
	"github.com/FlashbackAi/teepin-core/pkg/inferencegateway"
)

const (
	servePort  = 8000
	dataDir    = "/data"
	hfCacheDir = dataDir + "/hf-cache"
	modelDir   = dataDir + "/model"

	// downloaderImage runs the init container for a non-HuggingFace
	// ModelSource. A small, well-known image with curl and tar — nothing
	// about model serving, deliberately: its only job is fetching bytes.
	downloaderImage = "curlimages/curl:8.10.1"

	// hiddenLabel MUST match pkg/cluster's own private labelKumbhaAgent
	// constant (direct.go) — that package excludes anything carrying it
	// from a customer's Compute list. No shared exported constant exists
	// for this; pkg/kumbha/agent.go's own agentLabel redeclares the same
	// string for the same reason, so this follows that established
	// convention rather than inventing a different one.
	hiddenLabel = "teepin.io/kumbha-agent"
)

// engineConfig names the container image that serves each engine this
// reconciler knows how to run. Only engines pkg/inferencegateway can
// actually dispatch to belong here — see ModelServiceConfig's own doc
// comment for why MLX is a different, not-yet-built reconciler
// (teepin-modeld), never this one.
type engineConfig struct {
	VLLMImage     string
	VLLMOmniImage string

	// MLXCommand is the host executable that serves MLX models on a
	// native (teepin-modeld) node. Empty means "mlx_lm.server" on PATH.
	MLXCommand string

	// NodeReserveGB is memory held back on a native node for the OS and the
	// operator; the rest is the model budget. Zero means 6.
	NodeReserveGB int
}

func (e engineConfig) forEngine(engine string) (string, bool) {
	switch engine {
	case "vllm":
		return e.VLLMImage, e.VLLMImage != ""
	case "vllm-omni":
		return e.VLLMOmniImage, e.VLLMOmniImage != ""
	default:
		return "", false
	}
}

// parseModelSource splits an operator-given ModelSource into either a
// HuggingFace repo ID or a plain download URL.
//
// This distinction is load-bearing, not cosmetic: a real HF repo (verified
// live against DreamFoundries/K2-Horizon-MoVA-36B-A4B-MLX-4bit, a genuine
// 21GB, 4-sharded-safetensors repo needing trust_remote_code) is several
// files, not one — a plain curl against the repo PAGE only fetches HTML.
// For a huggingface.co URL, the repo ID is handed straight to the engine's
// own --model argument, letting vllm/mlx-lm's own tested HuggingFace
// integration handle the real multi-file download, resuming, and
// trust_remote_code. Anything else is treated as a single direct-download
// URL (see directURLInitContainer) — genuinely appropriate there, since a
// non-HF URL has no engine-native downloader to delegate to.
func parseModelSource(source string) (value string, isHuggingFace bool, err error) {
	trimmed := strings.TrimSpace(source)
	if trimmed == "" {
		return "", false, fmt.Errorf("model_source is required")
	}

	u, err := url.Parse(trimmed)
	if err != nil {
		return "", false, fmt.Errorf("invalid model_source URL: %w", err)
	}
	host := strings.ToLower(u.Host)
	if host != "huggingface.co" && host != "www.huggingface.co" {
		return trimmed, false, nil
	}

	// A repo path looks like "/Org/Repo" or "/Org/Repo/tree/main" —
	// the repo ID is always exactly the first two segments.
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", false, fmt.Errorf("could not parse a repo id (org/repo) from %q", source)
	}
	return parts[0] + "/" + parts[1], true, nil
}

// directURLInitContainer builds the init container that downloads a
// non-HuggingFace ModelSource. Scoped deliberately to .tar.gz/.tgz
// archives of a packaged HF-format model directory (config.json,
// tokenizer, safetensors) — vllm/vllm-omni's --model argument expects a
// directory, and guessing at a single-raw-file layout for two engines
// that don't ship a documented convention for one would be inventing
// behaviour rather than building on something confirmed. Idempotent via
// a marker file, so a pod restart does not always re-download from
// scratch.
func directURLInitContainer(sourceURL string) (*cluster.InitContainerSpec, error) {
	lower := strings.ToLower(sourceURL)
	if !strings.HasSuffix(lower, ".tar.gz") && !strings.HasSuffix(lower, ".tgz") {
		return nil, fmt.Errorf(
			"model_source %q is not a huggingface.co URL and does not look like a .tar.gz/.tgz archive — "+
				"vllm/vllm-omni need a packaged model directory", sourceURL)
	}

	const script = `set -eu
if [ -f "$DATA_DIR/.ready" ]; then
  echo "model already present, skipping download"
  exit 0
fi
curl -fL "$MODEL_URL" -o /tmp/model.tar.gz
mkdir -p "$MODEL_DIR"
tar -xzf /tmp/model.tar.gz -C "$MODEL_DIR"
rm -f /tmp/model.tar.gz
touch "$DATA_DIR/.ready"
`
	return &cluster.InitContainerSpec{
		Image:   downloaderImage,
		Command: []string{"sh", "-c", script},
		Env: map[string]string{
			"MODEL_URL": sourceURL,
			"DATA_DIR":  dataDir,
			"MODEL_DIR": modelDir,
		},
	}, nil
}

// vllmArgs builds the CLI arguments for the official vllm/vllm-openai
// image, whose documented ENTRYPOINT already invokes vllm's OpenAI-
// compatible server — these are passed as Args, with Command left nil to
// keep that entrypoint intact. --trust-remote-code is unconditional: the
// verified real-world test case (K2-Horizon-MoVA) ships custom modeling
// code and needs it; it is a no-op for a model that doesn't.
//
// NOT independently verified against a real vllm-omni image/tag — that
// project's exact CLI surface should be confirmed before its first real
// mount; this assumes it accepts the same flags as upstream vllm, since
// it's documented as an extension of it, not a replacement.
func vllmArgs(model, servedName string) []string {
	args := []string{
		"--model", model,
		"--host", "0.0.0.0",
		"--port", strconv.Itoa(servePort),
		"--trust-remote-code",
	}
	if servedName != "" && servedName != model {
		args = append(args, "--served-model-name", servedName)
	}
	return args
}

// buildInstanceSpec turns one node_services row's config into a
// placement-ready InstanceSpec, already pinned to the given node — the
// caller resolved that node explicitly (the admin already chose it when
// mounting), so this never goes through the general capacity allocator.
func buildInstanceSpec(instanceID string, cfg inferencegateway.ModelServiceConfig, images engineConfig, nodeName, providerID, nodeClass string) (cluster.InstanceSpec, error) {
	if cfg.Engine == engineMLX {
		if nodeClass != "home" {
			return cluster.InstanceSpec{}, fmt.Errorf("engine %q runs only on a native home node (teepin-modeld), not a %q node", engineMLX, nodeClass)
		}
		return buildMLXSpec(instanceID, cfg, images, nodeName, providerID)
	}
	if cfg.StorageGB <= 0 {
		return cluster.InstanceSpec{}, fmt.Errorf("storage_gb must be > 0")
	}
	if cfg.CPUUnits <= 0 {
		return cluster.InstanceSpec{}, fmt.Errorf("cpu_units must be > 0")
	}
	if cfg.MemoryGB <= 0 {
		return cluster.InstanceSpec{}, fmt.Errorf("memory_gb must be > 0")
	}

	image, ok := images.forEngine(cfg.Engine)
	if !ok {
		return cluster.InstanceSpec{}, fmt.Errorf(
			"engine %q is not supported by this reconciler (supported: vllm, vllm-omni, mlx)", cfg.Engine)
	}

	modelValue, isHF, err := parseModelSource(cfg.ModelSource)
	if err != nil {
		return cluster.InstanceSpec{}, err
	}

	servedName := cfg.BackendModel
	if servedName == "" {
		servedName = modelValue
	}

	spec := cluster.InstanceSpec{
		InstanceID: instanceID,
		Image:      image,
		CPUUnits:   cfg.CPUUnits,
		MemoryGB:   cfg.MemoryGB,
		StorageGB:  cfg.StorageGB,
		NodeName:   nodeName,
		Labels:     map[string]string{hiddenLabel: "true"},
		Ports:      []cluster.PortMapping{{Container: servePort, Protocol: "tcp"}},
	}
	if nodeClass == "home" {
		spec.NodeClass = "home"
		spec.ProviderID = providerID
	}
	if cfg.GPUCount > 0 {
		spec.GPUResource = "nvidia.com/gpu"
		spec.GPUQuantity = cfg.GPUCount
	}

	if isHF {
		spec.Env = map[string]string{"HF_HOME": hfCacheDir}
		spec.Args = vllmArgs(modelValue, servedName)
	} else {
		initContainer, err := directURLInitContainer(modelValue)
		if err != nil {
			return cluster.InstanceSpec{}, err
		}
		spec.InitContainer = initContainer
		spec.Args = vllmArgs(modelDir, servedName)
	}

	return spec, nil
}

// instanceIDFor derives a stable, deterministic instance ID from a
// node_services row's own ID — so mount/reconcile/unmount all address the
// exact same underlying instance without needing a separate mapping
// table, the same "the row's own ID is the idempotency key" pattern
// compute.instances already uses via InstanceID.
func instanceIDFor(rowID string) string {
	compact := strings.ReplaceAll(rowID, "-", "")
	if len(compact) > 8 {
		compact = compact[:8]
	}
	return "infsvc-" + compact
}
