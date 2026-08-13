// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
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

// hostSpecs detects what this machine offers. Deliberately minimal for the
// pilot: cores, OS and arch come from the runtime; memory is left to the
// control plane's later inventory. A consumer GPU is reported as an
// attribute in a later stage, never as sellable VRAM.
func hostSpecs() (cpuCores int, osName, arch string) {
	return runtime.NumCPU(), runtime.GOOS, runtime.GOARCH
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

	cores, osName, arch := hostSpecs()

	body, _ := json.Marshal(map[string]any{
		"token":         *token,
		"node_name":     name,
		"provider_id":   provider,
		"region":        *region,
		"cpu_cores":     cores,
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
