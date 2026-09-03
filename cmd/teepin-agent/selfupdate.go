// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// releasesAPI is GitHub's public, unauthenticated "latest release" — safe
// to hit with no credential because teepin-core is a public repo (Apache
// 2.0, per CLAUDE.md's Open Source Strategy). A private repo would need a
// distributed token to every home node, which this deliberately avoids.
const releasesAPI = "https://api.github.com/repos/FlashbackAi/teepin-core/releases/latest"

// selfUpdateCheckInterval bounds how often a node checks for a newer
// agent release. Home nodes are consumer machines an operator is not
// watching, so this exists to close the gap install.sh's own README
// flags: an update today requires a human to re-run install.sh on every
// node by hand — "we don't have the code there on that node" otherwise
// (no Go, no git checkout, by design — see the release tarball this
// pairs with). Daily is frequent enough to land a fix quickly without
// hammering GitHub's API across a growing fleet.
const selfUpdateCheckInterval = 24 * time.Hour

// githubRelease is the subset of GitHub's release API response this
// needs.
type githubRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// startSelfUpdateLoop runs for the lifetime of ctx, checking for a newer
// released agent version on selfUpdateCheckInterval and swapping this
// node's binary in place when one is found (see checkAndSelfUpdate). A
// dev build (Version == "dev" — a local `go build` with no -ldflags)
// never self-updates: a developer running from source should never have
// their binary silently replaced by a random tagged release.
//
// Only called from the enrolled home-node path (runEnrolledAgent) — a
// datacenter agent is deployed through its own operator-controlled image
// pipeline, not a curl-a-binary model, and must not compete with it.
func startSelfUpdateLoop(ctx context.Context) {
	if Version == "dev" {
		log.Println("self-update: disabled for a dev build (no -ldflags Version stamp)")
		return
	}

	// A random initial delay, not an immediate check: a fleet of nodes
	// all restarting around the same time (a mass power event, a
	// coordinated maintenance window) must not stampede GitHub's API in
	// the same instant. Bounded well under the regular interval.
	initialDelay := time.Duration(rand.IntN(600)) * time.Second
	select {
	case <-time.After(initialDelay):
	case <-ctx.Done():
		return
	}

	ticker := time.NewTicker(selfUpdateCheckInterval)
	defer ticker.Stop()
	for {
		if err := checkAndSelfUpdate(ctx); err != nil {
			log.Printf("WARN: self-update check failed: %v", err)
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

// checkAndSelfUpdate fetches the latest release, compares it against
// this binary's own baked-in Version, and — only if genuinely newer —
// downloads the matching linux/<GOARCH> tarball, verifies it against the
// release's published SHA256SUMS, extracts just the agent binary, and
// atomically swaps it into place at this process's own executable path
// (os.Executable(), not a hardcoded install path — correct regardless of
// where the node actually installed it).
//
// The swap itself never fails "Text file busy": install.sh's own doc
// comment explains why a plain overwrite does (the kernel refuses to
// write an inode an active process still maps) — os.Rename sidesteps it
// entirely, since renaming only repoints a directory entry and needs no
// write access to the currently-executing inode at all. The RUNNING
// process keeps executing the OLD (now unlinked-by-name) binary in
// memory until it exits; this function then exits deliberately so
// systemd's Restart=always (install.sh's own unit) relaunches it,
// picking up the new binary at the same path. A few seconds of downtime
// is the same class of interruption runForever's own reconnect-with-
// backoff already handles for a control-plane restart.
func checkAndSelfUpdate(ctx context.Context) error {
	rel, err := fetchLatestRelease(ctx)
	if err != nil {
		return fmt.Errorf("fetch latest release: %w", err)
	}
	if !isNewerVersion(rel.TagName, Version) {
		return nil
	}

	assetName := fmt.Sprintf("teepin-agent-linux-%s.tar.gz", runtime.GOARCH)
	tarballURL := assetURL(rel, assetName)
	if tarballURL == "" {
		return fmt.Errorf("release %s has no %s asset", rel.TagName, assetName)
	}
	sumsURL := assetURL(rel, "SHA256SUMS")
	if sumsURL == "" {
		return fmt.Errorf("release %s has no SHA256SUMS asset", rel.TagName)
	}

	log.Printf("self-update: %s available (running %s) — downloading %s", rel.TagName, Version, assetName)

	tmpDir, err := os.MkdirTemp("", "teepin-agent-update-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	tarballPath := filepath.Join(tmpDir, assetName)
	if err := downloadFile(ctx, tarballURL, tarballPath); err != nil {
		return fmt.Errorf("download %s: %w", assetName, err)
	}

	wantSHA, err := lookupChecksum(ctx, sumsURL, assetName)
	if err != nil {
		return fmt.Errorf("fetch checksum: %w", err)
	}
	gotSHA, err := sha256File(tarballPath)
	if err != nil {
		return fmt.Errorf("checksum downloaded file: %w", err)
	}
	if gotSHA != wantSHA {
		return fmt.Errorf("checksum mismatch for %s: got %s, want %s (corrupt or interrupted download — will retry next cycle)",
			assetName, gotSHA, wantSHA)
	}

	newBinary, err := extractBinaryFromTarball(tarballPath, tmpDir)
	if err != nil {
		return fmt.Errorf("extract agent binary: %w", err)
	}

	selfPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("determine own executable path: %w", err)
	}

	if err := os.Chmod(newBinary, 0755); err != nil {
		return fmt.Errorf("chmod new binary: %w", err)
	}
	if err := os.Rename(newBinary, selfPath); err != nil {
		return fmt.Errorf("install new binary at %s: %w", selfPath, err)
	}

	log.Printf("self-update: installed %s (was %s) at %s — exiting for systemd to restart", rel.TagName, Version, selfPath)
	os.Exit(0)
	return nil // unreachable
}

func fetchLatestRelease(ctx context.Context) (*githubRelease, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releasesAPI, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(body))
	}

	var rel githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("decode release JSON: %w", err)
	}
	return &rel, nil
}

func assetURL(rel *githubRelease, name string) string {
	for _, a := range rel.Assets {
		if a.Name == name {
			return a.URL
		}
	}
	return ""
}

func downloadFile(ctx context.Context, url, destPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned %d", resp.StatusCode)
	}

	out, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer out.Close()

	// Bounded: a 200 GB response body would otherwise be copied in full
	// before anything notices something is wrong. Comfortably past any
	// real release tarball (a Go binary is tens of MB).
	const maxDownloadBytes = 200 * 1024 * 1024
	if _, err := io.Copy(out, io.LimitReader(resp.Body, maxDownloadBytes)); err != nil {
		return err
	}
	return nil
}

// lookupChecksum fetches sumsURL (a plain `sha256sum`-format file — one
// "<hex>  <filename>" line per asset) and returns the hex digest for
// assetName. Fetched fresh each time rather than cached alongside the
// tarball download, since it is tiny and this keeps the two requests
// independent (no assumption about response ordering or a combined
// payload).
func lookupChecksum(ctx context.Context, sumsURL, assetName string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sumsURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("SHA256SUMS download returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == assetName {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("no checksum entry for %s in SHA256SUMS", assetName)
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractBinaryFromTarball pulls just the "teepin-agent" entry out of
// the release tarball (which also carries install.sh and friends for a
// FRESH install — irrelevant here, this is an in-place binary swap on an
// already-enrolled node) and writes it to destDir. Returns the extracted
// file's path.
func extractBinaryFromTarball(tarballPath, destDir string) (string, error) {
	f, err := os.Open(tarballPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("open gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return "", fmt.Errorf("tarball has no teepin-agent entry")
		}
		if err != nil {
			return "", fmt.Errorf("read tar entry: %w", err)
		}
		// Match the base name only — the workflow that builds this
		// tarball packages files flat, but matching by base name rather
		// than requiring an exact path is a cheap, harmless tolerance
		// for that packaging detail changing later.
		if filepath.Base(hdr.Name) != "teepin-agent" || hdr.Typeflag != tar.TypeReg {
			continue
		}
		outPath := filepath.Join(destDir, "teepin-agent")
		out, err := os.Create(outPath)
		if err != nil {
			return "", err
		}
		// Bounded for the same reason downloadFile's copy is — a
		// malformed or malicious tar header claiming a huge size must
		// not be copied in full before anything notices.
		const maxBinaryBytes = 500 * 1024 * 1024
		if _, err := io.Copy(out, io.LimitReader(tr, maxBinaryBytes)); err != nil {
			out.Close()
			return "", err
		}
		out.Close()
		return outPath, nil
	}
}

// isNewerVersion reports whether latest (a release tag like "agent-
// v1.2.3") is strictly newer than current (this binary's own Version,
// stamped from the same tag format at build time — see the release
// workflow's -ldflags). Deliberately not a plain string comparison:
// "agent-v1.9.0" > "agent-v1.10.0" as strings, which is wrong. Parses
// each dot-separated numeric component and compares in order; any
// unparseable tag on EITHER side is treated as "not newer" — a self-
// update that cannot confidently tell it is moving forward must not
// happen at all, not guess.
func isNewerVersion(latest, current string) bool {
	lp, ok1 := parseVersionParts(latest)
	cp, ok2 := parseVersionParts(current)
	if !ok1 || !ok2 {
		return false
	}
	for i := 0; i < len(lp) || i < len(cp); i++ {
		var l, c int
		if i < len(lp) {
			l = lp[i]
		}
		if i < len(cp) {
			c = cp[i]
		}
		if l != c {
			return l > c
		}
	}
	return false
}

// parseVersionParts turns "agent-v1.2.3" into [1, 2, 3]. Accepts an
// optional "agent-v" or "v" prefix so both the release tag format and a
// bare semver still parse.
func parseVersionParts(tag string) ([]int, bool) {
	s := strings.TrimPrefix(tag, "agent-")
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return nil, false
	}
	fields := strings.Split(s, ".")
	parts := make([]int, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil {
			return nil, false
		}
		parts = append(parts, n)
	}
	return parts, len(parts) > 0
}
