// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// nodeConfig is the credential and identity the agent persists after a
// successful enrollment. It is written 0600 — it is the machine's secret.
type nodeConfig struct {
	Credential string `json:"credential"`
	NodeName   string `json:"node_name"`
	Class      string `json:"class"`
	// ControlPlane the node enrolled against, so `run` needs no re-config.
	ControlPlane string `json:"control_plane"`
}

// configPath is where the node credential lives. Overridable for tests and
// for operators who run several agents on one host.
func configPath() string {
	if p := os.Getenv("TEEPIN_AGENT_CONFIG"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".teepin", "agent.json")
}

func loadNodeConfig() (*nodeConfig, error) {
	data, err := os.ReadFile(configPath())
	if err != nil {
		return nil, err
	}
	var cfg nodeConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("corrupt agent config at %s: %w", configPath(), err)
	}
	return &cfg, nil
}

func saveNodeConfig(cfg *nodeConfig) error {
	dir := filepath.Dir(configPath())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	// 0600: the credential authenticates this node to the control plane.
	return os.WriteFile(configPath(), data, 0o600)
}

// hostSpecs detects what this machine offers: logical CPUs, total memory, OS
// and arch. Cores/OS/arch come from the runtime; memory is read from
// /proc/meminfo on Linux (the agent always runs inside Linux). Memory is 0 on
// a platform where it cannot be read — the operator's reservation is capped at
// detected specs, so an undetected value simply means "cannot rent memory
// until detected", never an over-offer. A consumer GPU is reported as an
// attribute in a later stage, never as sellable VRAM.
func hostSpecs() (cpuCores, memoryGB int, osName, arch string) {
	return runtime.NumCPU(), detectMemoryGB(), runtime.GOOS, runtime.GOARCH
}

// detectPECores resolves this node's P-core/E-core split for a hybrid
// consumer CPU. Both 0 means "no split detected" — never a guess. Called
// from BOTH `enroll` (this file) and `run`'s reconnect loop (main.go) —
// like hostSpecs, it is not enroll-time-only, so a fixed detector or a
// corrected reading only needs an agent restart, never a fresh
// enrollment token.
//
// Resolution order: (1) TEEPIN_PCORES/TEEPIN_ECORES env vars — set by the
// Windows/macOS bootstrap script from cmd/teepin-hostprobe's real host-OS
// detection, since this agent only ever runs as Linux (see this file's own
// package comment) and cannot call Windows/macOS-native detection APIs
// itself, and PERSISTED into this service's own systemd unit by
// install.sh's apply_pe_core_env so they survive past the one-time enroll
// shell into every later `run`; (2) a best-effort native Linux topology
// read, for a bare-metal Linux home node running this agent directly with
// no VM involved.
func detectPECores() (pCores, eCores int) {
	if p, e, ok := peCoresFromEnv(); ok {
		return p, e
	}
	if p, e := nativePECores(); p > 0 {
		return p, e // running natively on macOS — see hostspecs_darwin.go
	}
	return peCoresFromLinuxTopology()
}

func peCoresFromEnv() (pCores, eCores int, ok bool) {
	pStr, eStr := os.Getenv("TEEPIN_PCORES"), os.Getenv("TEEPIN_ECORES")
	if pStr == "" || eStr == "" {
		return 0, 0, false
	}
	p, errP := strconv.Atoi(pStr)
	e, errE := strconv.Atoi(eStr)
	if errP != nil || errE != nil || p <= 0 {
		return 0, 0, false
	}
	return p, e, true
}

var sysCPURE = regexp.MustCompile(`^cpu[0-9]+$`)

// sysCPUDir is /sys/devices/system/cpu, overridable in tests so
// peCoresFromLinuxTopology can be exercised against fixture files without a
// real /sys (the sandbox running the tests need not even be Linux) — same
// seam-by-variable pattern as pkg/agentrunner's own readFileFunc.
var sysCPUDir = "/sys/devices/system/cpu"

// peCoresFromLinuxTopology groups logical CPUs by
// /sys/devices/system/cpu/cpuN/cpu_capacity — the kernel's own per-core
// relative-performance figure, populated from ACPI CPPC / hardware feedback
// on hybrid-aware kernels (and on ARM big.LITTLE). Exactly two distinct
// capacity values is treated as a P/E split (the higher-capacity group is
// P-cores); anything else — the file missing on any core, only one distinct
// value (homogeneous), or more than two (a design this code does not model)
// — is not a split this code is confident enough to report, so it returns
// (0, 0) rather than guess.
func peCoresFromLinuxTopology() (pCores, eCores int) {
	entries, err := os.ReadDir(sysCPUDir)
	if err != nil {
		return 0, 0
	}

	countByCapacity := map[int]int{}
	for _, entry := range entries {
		if !sysCPURE.MatchString(entry.Name()) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(sysCPUDir, entry.Name(), "cpu_capacity"))
		if err != nil {
			return 0, 0 // any core missing the file — not confident enough to report a split
		}
		v, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			return 0, 0
		}
		countByCapacity[v]++
	}
	if len(countByCapacity) != 2 {
		return 0, 0
	}

	values := make([]int, 0, 2)
	for v := range countByCapacity {
		values = append(values, v)
	}
	sort.Ints(values)
	low, high := values[0], values[1]
	return countByCapacity[high], countByCapacity[low]
}

// detectMemoryGB returns total physical memory in whole GB, read from
// /proc/meminfo (MemTotal, in kB). Returns 0 if it cannot be read.
func detectMemoryGB() int {
	if gb := nativeMemoryGB(); gb > 0 {
		return gb // running natively on macOS — see hostspecs_darwin.go
	}
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		// Format: "MemTotal:       32797156 kB"
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if kb, err := strconv.Atoi(fields[1]); err == nil {
					// kB -> GB, rounded to the nearest whole GB.
					return int(math.Round(float64(kb) / (1024.0 * 1024.0)))
				}
			}
			break
		}
	}
	return 0
}

// runEnroll implements `teepin-agent enroll --token <t>`. It exchanges the
// one-time token for this node's own credential and persists it. It needs no
// Kubernetes and no prior credential — enrollment is the bootstrap.
//
// The class is NOT a flag: it is fixed on the token by the operator and
// returned by the server. An agent cannot ask to be a datacenter node.
func runEnroll(args []string) error {
	fs := newFlagSet("enroll")
	token := fs.String("token", "", "one-time enrollment token (required)")
	controlPlane := fs.String("control-plane", getEnv("TEEPIN_CONTROL_PLANE_HTTP", ""),
		"control plane base URL, e.g. https://api.teepin.com")
	nodeName := fs.String("node-name", "", "node name (default: hostname)")
	providerID := fs.String("provider-id", "", "provider id (default: node name)")
	region := fs.String("region", getEnv("TEEPIN_REGION", "home"), "region label")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *token == "" {
		return fmt.Errorf("--token is required")
	}
	if *controlPlane == "" {
		return fmt.Errorf("--control-plane is required (or set TEEPIN_CONTROL_PLANE_HTTP)")
	}

	name := *nodeName
	if name == "" {
		h, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("could not determine hostname; pass --node-name: %w", err)
		}
		name = h
	}
	provider := *providerID
	if provider == "" {
		provider = name
	}

	cores, memGB, osName, arch := hostSpecs()
	pCores, eCores := detectPECores()

	body, _ := json.Marshal(map[string]any{
		"token":         *token,
		"node_name":     name,
		"provider_id":   provider,
		"region":        *region,
		"cpu_cores":     cores,
		"memory_gb":     memGB,
		"p_cores":       pCores,
		"e_cores":       eCores,
		"os":            osName,
		"arch":          arch,
		"agent_version": Version,
	})

	url := *controlPlane + "/v1/nodes/enroll"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("enroll request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("enrollment rejected (%d): %s", resp.StatusCode, string(respBody))
	}

	var out struct {
		Credential string `json:"credential"`
		Node       struct {
			NodeName string `json:"node_name"`
			Class    string `json:"class"`
		} `json:"node"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return fmt.Errorf("could not parse enrollment response: %w", err)
	}
	if out.Credential == "" {
		return fmt.Errorf("enrollment response carried no credential")
	}

	if err := saveNodeConfig(&nodeConfig{
		Credential:   out.Credential,
		NodeName:     out.Node.NodeName,
		Class:        out.Node.Class,
		ControlPlane: *controlPlane,
	}); err != nil {
		return fmt.Errorf("could not save credential: %w", err)
	}

	fmt.Printf("Enrolled as %q (class=%s). Credential saved to %s.\n",
		out.Node.NodeName, out.Node.Class, configPath())
	fmt.Println("Start the agent with:  teepin-agent run")
	return nil
}
