// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestIsNewerVersion(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"agent-v1.2.3", "agent-v1.2.2", true},
		{"agent-v1.2.3", "agent-v1.2.3", false},
		{"agent-v1.2.2", "agent-v1.2.3", false},
		// The exact case a plain string comparison gets wrong.
		{"agent-v1.10.0", "agent-v1.9.0", true},
		{"agent-v1.9.0", "agent-v1.10.0", false},
		{"agent-v2.0.0", "agent-v1.99.99", true},
		// Dev builds and malformed tags never count as "newer".
		{"agent-v1.0.0", "dev", false},
		{"not-a-version", "agent-v1.0.0", false},
		{"agent-v1.0.0", "not-a-version", false},
	}
	for _, tc := range cases {
		if got := isNewerVersion(tc.latest, tc.current); got != tc.want {
			t.Errorf("isNewerVersion(%q, %q) = %v, want %v", tc.latest, tc.current, got, tc.want)
		}
	}
}

func TestParseVersionParts(t *testing.T) {
	cases := []struct {
		tag  string
		want []int
		ok   bool
	}{
		{"agent-v1.2.3", []int{1, 2, 3}, true},
		{"v1.2.3", []int{1, 2, 3}, true},
		{"1.2.3", []int{1, 2, 3}, true},
		{"dev", nil, false},
		{"", nil, false},
		{"agent-vabc", nil, false},
	}
	for _, tc := range cases {
		got, ok := parseVersionParts(tc.tag)
		if ok != tc.ok {
			t.Errorf("parseVersionParts(%q) ok = %v, want %v", tc.tag, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("parseVersionParts(%q) = %v, want %v", tc.tag, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("parseVersionParts(%q) = %v, want %v", tc.tag, got, tc.want)
				break
			}
		}
	}
}

// TestFetchLatestRelease_ParsesResponse exercises the same JSON
// decoding and asset-lookup logic checkAndSelfUpdate itself uses,
// against a fake server rather than the real (hardcoded) releasesAPI
// constant — checkAndSelfUpdate's own "newer version found" path is
// deliberately NOT driven end-to-end in a unit test, since a genuine
// update ends in os.Exit(0), which would kill the test binary. Its
// individual pieces (this, TestLookupChecksum, TestSha256File,
// TestExtractBinaryFromTarball_*) are each covered directly instead, and
// TestIsNewerVersion locks in the "is this actually an update" decision
// checkAndSelfUpdate's early-return depends on.
func TestFetchLatestRelease_ParsesResponse(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tag_name":"agent-v2.3.4","assets":[
			{"name":"teepin-agent-linux-amd64.tar.gz","browser_download_url":"https://example.com/a.tar.gz"},
			{"name":"SHA256SUMS","browser_download_url":"https://example.com/SHA256SUMS"}
		]}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var rel githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rel.TagName != "agent-v2.3.4" {
		t.Errorf("TagName = %q, want agent-v2.3.4", rel.TagName)
	}
	if assetURL(&rel, "teepin-agent-linux-amd64.tar.gz") != "https://example.com/a.tar.gz" {
		t.Errorf("assetURL did not resolve the amd64 asset: %+v", rel.Assets)
	}
	if assetURL(&rel, "does-not-exist") != "" {
		t.Error("assetURL should return empty for a missing asset name")
	}
}

func TestLookupChecksum(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  teepin-agent-linux-amd64.tar.gz\n"+
			"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb  teepin-agent-linux-arm64.tar.gz\n")
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	got, err := lookupChecksum(context.Background(), server.URL+"/SHA256SUMS", "teepin-agent-linux-arm64.tar.gz")
	if err != nil {
		t.Fatalf("lookupChecksum: %v", err)
	}
	if got != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Errorf("got %q", got)
	}

	if _, err := lookupChecksum(context.Background(), server.URL+"/SHA256SUMS", "no-such-file.tar.gz"); err == nil {
		t.Error("expected an error for a file not listed in SHA256SUMS")
	}
}

func TestSha256File(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := os.WriteFile(path, []byte("hello world"), 0644); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte("hello world"))
	got, err := sha256File(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != hex.EncodeToString(want[:]) {
		t.Errorf("sha256File = %q, want %x", got, want)
	}
}

// buildTestTarball writes a gzipped tar containing one entry named name
// with the given content, returning the archive path.
func buildTestTarball(t *testing.T, dir, archiveName, entryName string, content []byte) string {
	t.Helper()
	path := filepath.Join(dir, archiveName)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	hdr := &tar.Header{Name: entryName, Mode: 0755, Size: int64(len(content)), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractBinaryFromTarball_FindsEntryByBaseName(t *testing.T) {
	dir := t.TempDir()
	content := []byte("#!/fake binary\n")
	tarballPath := buildTestTarball(t, dir, "release.tar.gz", "teepin-agent", content)

	outPath, err := extractBinaryFromTarball(tarballPath, dir)
	if err != nil {
		t.Fatalf("extractBinaryFromTarball: %v", err)
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("extracted content = %q, want %q", got, content)
	}
}

func TestExtractBinaryFromTarball_NoEntryIsAnError(t *testing.T) {
	dir := t.TempDir()
	tarballPath := buildTestTarball(t, dir, "release.tar.gz", "install.sh", []byte("#!/bin/bash\n"))

	if _, err := extractBinaryFromTarball(tarballPath, dir); err == nil {
		t.Error("expected an error when the tarball has no teepin-agent entry")
	}
}
