// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package inferencereconciler

import (
	"strings"
	"testing"

	"github.com/FlashbackAi/teepin-core/pkg/inferencegateway"
)

// TestParseModelSource_HuggingFaceURL is the regression test for the real
// finding that motivated this whole design: DreamFoundries/
// K2-Horizon-MoVA-36B-A4B-MLX-4bit is a genuine 21GB, 4-sharded-safetensors
// repo, confirmed live against the real Hugging Face file listing — a
// plain curl against the repo PAGE would only fetch HTML, never the model.
func TestParseModelSource_HuggingFaceURL(t *testing.T) {
	repo, isHF, err := parseModelSource("https://huggingface.co/DreamFoundries/K2-Horizon-MoVA-36B-A4B-MLX-4bit")
	if err != nil {
		t.Fatalf("parseModelSource: %v", err)
	}
	if !isHF {
		t.Fatal("not detected as a HuggingFace URL")
	}
	if repo != "DreamFoundries/K2-Horizon-MoVA-36B-A4B-MLX-4bit" {
		t.Errorf("repo = %q, want the parsed org/repo id", repo)
	}
}

// TestParseModelSource_HuggingFaceURLWithTreeSuffix proves the parser
// strips a "/tree/main" (or similar) suffix rather than treating it as
// part of the repo id — a real, common way people copy HF URLs (e.g. from
// the Files tab), verified live in this session.
func TestParseModelSource_HuggingFaceURLWithTreeSuffix(t *testing.T) {
	repo, isHF, err := parseModelSource("https://huggingface.co/Qwen/Qwen3-Omni-7B-Instruct/tree/main")
	if err != nil {
		t.Fatalf("parseModelSource: %v", err)
	}
	if !isHF {
		t.Fatal("not detected as a HuggingFace URL")
	}
	if repo != "Qwen/Qwen3-Omni-7B-Instruct" {
		t.Errorf("repo = %q, want Qwen/Qwen3-Omni-7B-Instruct (tree/main suffix must be stripped)", repo)
	}
}

func TestParseModelSource_NonHuggingFaceURLPassesThrough(t *testing.T) {
	value, isHF, err := parseModelSource("https://example.com/models/my-model.tar.gz")
	if err != nil {
		t.Fatalf("parseModelSource: %v", err)
	}
	if isHF {
		t.Error("example.com URL misdetected as HuggingFace")
	}
	if value != "https://example.com/models/my-model.tar.gz" {
		t.Errorf("value = %q, want the URL unchanged", value)
	}
}

func TestParseModelSource_RejectsBlank(t *testing.T) {
	if _, _, err := parseModelSource("   "); err == nil {
		t.Error("blank model_source accepted")
	}
}

func TestParseModelSource_RejectsHuggingFaceURLWithNoRepoPath(t *testing.T) {
	if _, _, err := parseModelSource("https://huggingface.co/"); err == nil {
		t.Error("a bare huggingface.co URL with no org/repo path was accepted")
	}
}

func TestDirectURLInitContainer_RequiresArchiveExtension(t *testing.T) {
	if _, err := directURLInitContainer("https://example.com/model.safetensors"); err == nil {
		t.Error("a non-archive URL was accepted for the direct-download path")
	}
}

func TestDirectURLInitContainer_BuildsDownloadScript(t *testing.T) {
	init, err := directURLInitContainer("https://example.com/model.tar.gz")
	if err != nil {
		t.Fatalf("directURLInitContainer: %v", err)
	}
	if init.Image != downloaderImage {
		t.Errorf("Image = %q, want %q", init.Image, downloaderImage)
	}
	if init.Env["MODEL_URL"] != "https://example.com/model.tar.gz" {
		t.Errorf("MODEL_URL env = %q, want the source URL", init.Env["MODEL_URL"])
	}
	script := strings.Join(init.Command, " ")
	if !strings.Contains(script, "curl") || !strings.Contains(script, "tar -xzf") {
		t.Errorf("init container command does not look like a download+extract script: %q", script)
	}
}

func TestBuildInstanceSpec_RequiresStorageCPUMemory(t *testing.T) {
	base := inferencegateway.ModelServiceConfig{
		Engine:      "vllm",
		ModelSource: "https://huggingface.co/Qwen/Qwen3-Omni-7B-Instruct",
	}
	images := engineConfig{VLLMImage: "vllm/vllm-openai:latest"}

	if _, err := buildInstanceSpec("infsvc-1", base, images, "node-a", "", ""); err == nil {
		t.Error("missing storage_gb/cpu_units/memory_gb accepted")
	}
}

func TestBuildInstanceSpec_UnsupportedEngineErrorsClearly(t *testing.T) {
	cfg := inferencegateway.ModelServiceConfig{
		Engine:      "llamacpp",
		ModelSource: "https://huggingface.co/Qwen/Qwen3-Omni-7B-Instruct",
		StorageGB:   50, CPUUnits: 4, MemoryGB: 16,
	}
	_, err := buildInstanceSpec("infsvc-1", cfg, engineConfig{VLLMImage: "vllm/vllm-openai:latest"}, "node-a", "", "")
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("unknown engine err = %v, want a clear not-supported error", err)
	}
}

func TestBuildInstanceSpec_MLXOnNativeHomeNode(t *testing.T) {
	cfg := inferencegateway.ModelServiceConfig{
		Engine:      "mlx",
		ModelSource: "https://huggingface.co/DreamFoundries/K2-Horizon-MoVA-36B-A4B-MLX-4bit",
	}
	spec, err := buildInstanceSpec("infsvc-m", cfg, engineConfig{}, "mac", "provider-mac", "home")
	if err != nil {
		t.Fatalf("buildInstanceSpec: %v", err)
	}
	if len(spec.Command) != 1 || spec.Command[0] != "mlx_lm.server" {
		t.Errorf("Command = %v, want default mlx_lm.server", spec.Command)
	}
	if !containsArg(spec.Args, "DreamFoundries/K2-Horizon-MoVA-36B-A4B-MLX-4bit") || !containsArg(spec.Args, "127.0.0.1") || !containsArg(spec.Args, "${TEEPIN_PORT}") {
		t.Errorf("Args = %v, want repo id, loopback host, port placeholder", spec.Args)
	}
	if spec.ProviderID != "provider-mac" || spec.NodeClass != "home" {
		t.Errorf("routing = %q/%q", spec.ProviderID, spec.NodeClass)
	}
	if spec.InitContainer != nil || spec.StorageGB != 0 {
		t.Error("native spec must not carry storage or an init container")
	}
}

func TestBuildInstanceSpec_MLXCustomCommand(t *testing.T) {
	cfg := inferencegateway.ModelServiceConfig{Engine: "mlx", ModelSource: "https://huggingface.co/a/b"}
	spec, err := buildInstanceSpec("i", cfg, engineConfig{MLXCommand: "/opt/venv/bin/mlx_lm.server"}, "mac", "p", "home")
	if err != nil || spec.Command[0] != "/opt/venv/bin/mlx_lm.server" {
		t.Fatalf("spec=%v err=%v", spec.Command, err)
	}
}

func TestBuildInstanceSpec_MLXRejectsNonHFAndNonHome(t *testing.T) {
	hf := inferencegateway.ModelServiceConfig{Engine: "mlx", ModelSource: "https://huggingface.co/a/b"}
	if _, err := buildInstanceSpec("i", hf, engineConfig{}, "dc", "", "datacenter"); err == nil {
		t.Error("mlx accepted on a datacenter node")
	}
	nonHF := inferencegateway.ModelServiceConfig{Engine: "mlx", ModelSource: "https://example.com/m.tar.gz"}
	if _, err := buildInstanceSpec("i", nonHF, engineConfig{}, "mac", "p", "home"); err == nil {
		t.Error("mlx accepted a non-HF source")
	}
}

// TestBuildInstanceSpec_HuggingFaceSourceUsesRepoIDDirectly proves the
// engine's --model argument gets the parsed repo ID (letting vllm's own
// HF integration download it), with no init container and an HF_HOME
// cache directory on the persistent volume.
func TestBuildInstanceSpec_HuggingFaceSourceUsesRepoIDDirectly(t *testing.T) {
	cfg := inferencegateway.ModelServiceConfig{
		Engine:      "vllm-omni",
		ModelSource: "https://huggingface.co/Qwen/Qwen3-Omni-7B-Instruct",
		StorageGB:   200, CPUUnits: 8, MemoryGB: 32, GPUCount: 1,
	}
	spec, err := buildInstanceSpec("infsvc-1", cfg, engineConfig{VLLMOmniImage: "vllm-omni/vllm-omni:latest"}, "node-a", "", "")
	if err != nil {
		t.Fatalf("buildInstanceSpec: %v", err)
	}
	if spec.InitContainer != nil {
		t.Error("a HuggingFace source should not need an init container — the engine downloads it natively")
	}
	if spec.Env["HF_HOME"] != hfCacheDir {
		t.Errorf("HF_HOME = %q, want %q (persistent cache)", spec.Env["HF_HOME"], hfCacheDir)
	}
	if !containsArg(spec.Args, "Qwen/Qwen3-Omni-7B-Instruct") {
		t.Errorf("Args %v does not pass the repo id as --model", spec.Args)
	}
	if spec.GPUResource != "nvidia.com/gpu" || spec.GPUQuantity != 1 {
		t.Errorf("GPU request = %q x%d, want a whole nvidia.com/gpu", spec.GPUResource, spec.GPUQuantity)
	}
	if spec.Image != "vllm-omni/vllm-omni:latest" {
		t.Errorf("Image = %q, want the configured vllm-omni image", spec.Image)
	}
	if spec.Labels[hiddenLabel] != "true" {
		t.Error("instance not hidden from the customer Compute list")
	}
}

// TestBuildInstanceSpec_DirectURLSourceGetsInitContainer proves the other
// branch: a non-HF source gets a real init container, and the engine's
// --model argument points at the local extraction path, not a URL.
func TestBuildInstanceSpec_DirectURLSourceGetsInitContainer(t *testing.T) {
	cfg := inferencegateway.ModelServiceConfig{
		Engine:      "vllm",
		ModelSource: "https://example.com/models/custom.tar.gz",
		StorageGB:   100, CPUUnits: 4, MemoryGB: 16,
	}
	spec, err := buildInstanceSpec("infsvc-2", cfg, engineConfig{VLLMImage: "vllm/vllm-openai:latest"}, "node-a", "", "")
	if err != nil {
		t.Fatalf("buildInstanceSpec: %v", err)
	}
	if spec.InitContainer == nil {
		t.Fatal("a direct-URL source must get an init container")
	}
	if spec.InitContainer.Env["MODEL_URL"] != "https://example.com/models/custom.tar.gz" {
		t.Errorf("init container URL = %q, want the configured source", spec.InitContainer.Env["MODEL_URL"])
	}
	if !containsArg(spec.Args, modelDir) {
		t.Errorf("Args %v does not point --model at the local extraction path %q", spec.Args, modelDir)
	}
}

// TestBuildInstanceSpec_HomeNodePinsProviderAndClass proves a home-class
// node routes through ProviderID/NodeClass the same way an ordinary home
// CPU instance already does — the admin's explicit node choice must reach
// the exact right agent session, not an arbitrary one.
func TestBuildInstanceSpec_HomeNodePinsProviderAndClass(t *testing.T) {
	cfg := inferencegateway.ModelServiceConfig{
		Engine:      "vllm",
		ModelSource: "https://huggingface.co/Qwen/Qwen3-Omni-7B-Instruct",
		StorageGB:   100, CPUUnits: 4, MemoryGB: 16,
	}
	spec, err := buildInstanceSpec("infsvc-3", cfg, engineConfig{VLLMImage: "vllm/vllm-openai:latest"}, "srialla", "provider-srialla", "home")
	if err != nil {
		t.Fatalf("buildInstanceSpec: %v", err)
	}
	if spec.NodeClass != "home" || spec.ProviderID != "provider-srialla" {
		t.Errorf("NodeClass=%q ProviderID=%q, want home/provider-srialla", spec.NodeClass, spec.ProviderID)
	}
	if spec.NodeName != "srialla" {
		t.Errorf("NodeName = %q, want the pinned node", spec.NodeName)
	}
}

// TestBuildInstanceSpec_DatacenterNodeLeavesProviderEmpty guards the other
// direction — a datacenter node must not carry a ProviderID/NodeClass,
// matching AgentClient's Any() fallback path for the single-provider case.
func TestBuildInstanceSpec_DatacenterNodeLeavesProviderEmpty(t *testing.T) {
	cfg := inferencegateway.ModelServiceConfig{
		Engine:      "vllm",
		ModelSource: "https://huggingface.co/Qwen/Qwen3-Omni-7B-Instruct",
		StorageGB:   100, CPUUnits: 4, MemoryGB: 16,
	}
	spec, err := buildInstanceSpec("infsvc-4", cfg, engineConfig{VLLMImage: "vllm/vllm-openai:latest"}, "dc-node-1", "some-provider", "datacenter")
	if err != nil {
		t.Fatalf("buildInstanceSpec: %v", err)
	}
	if spec.NodeClass != "" || spec.ProviderID != "" {
		t.Errorf("NodeClass=%q ProviderID=%q, want both empty for a datacenter node", spec.NodeClass, spec.ProviderID)
	}
}

func TestInstanceIDFor_Deterministic(t *testing.T) {
	id := "5c1f0a2e-1234-4abc-9def-000000000000"
	if instanceIDFor(id) != instanceIDFor(id) {
		t.Error("instanceIDFor is not deterministic")
	}
	if !strings.HasPrefix(instanceIDFor(id), "infsvc-") {
		t.Errorf("instanceIDFor(%q) = %q, want an infsvc- prefix", id, instanceIDFor(id))
	}
}

func containsArg(args []string, value string) bool {
	for _, a := range args {
		if a == value {
			return true
		}
	}
	return false
}
