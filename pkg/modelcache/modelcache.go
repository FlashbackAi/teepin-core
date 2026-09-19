// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Package modelcache lists and removes model weights downloaded onto a host
// by Hugging Face tooling (mlx-lm, transformers, vLLM all share one cache
// layout). It exists so an operator can see, from Control Center, what is
// occupying a node's disk and delete what is no longer needed, without a
// shell on the machine.
//
// Deletion is deliberately narrow: only a plain "org/name" repo id is
// accepted, it is mapped to exactly one directory that must be a direct child
// of the cache's hub directory, and symlinks are never followed. Nothing else
// on the host is reachable through this package.
package modelcache

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	// ErrInvalidRepo means the repo id is not a plain "org/name".
	ErrInvalidRepo = errors.New("modelcache: invalid repo id")
	// ErrNotFound means the model is not in the cache.
	ErrNotFound = errors.New("modelcache: model not in cache")
)

// repoPattern is deliberately strict: two segments of letters, digits, dot,
// underscore and dash, and neither segment may be "." or "..".
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)

const dirPrefix = "models--"

// Model is one downloaded model.
type Model struct {
	RepoID    string
	SizeBytes int64
}

// Cache is a Hugging Face hub cache directory.
type Cache struct {
	hub string
}

// New returns a Cache rooted at hubDir (the directory containing the
// "models--org--name" folders).
func New(hubDir string) *Cache { return &Cache{hub: hubDir} }

// DefaultHub resolves the hub cache directory the same way Hugging Face
// tooling does: HF_HUB_CACHE, else $HF_HOME/hub, else ~/.cache/huggingface/hub.
func DefaultHub() (string, error) {
	if v := os.Getenv("HF_HUB_CACHE"); v != "" {
		return v, nil
	}
	if v := os.Getenv("HUGGINGFACE_HUB_CACHE"); v != "" {
		return v, nil
	}
	if v := os.Getenv("HF_HOME"); v != "" {
		return filepath.Join(v, "hub"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("modelcache: cannot resolve home directory: %w", err)
	}
	return filepath.Join(home, ".cache", "huggingface", "hub"), nil
}

// folderName maps "org/name" to the cache's on-disk directory name.
func folderName(repoID string) string {
	return dirPrefix + strings.ReplaceAll(repoID, "/", "--")
}

// repoFromFolder is the inverse, or "" if the folder is not a model folder.
func repoFromFolder(name string) string {
	if !strings.HasPrefix(name, dirPrefix) {
		return ""
	}
	rest := strings.TrimPrefix(name, dirPrefix)
	i := strings.Index(rest, "--")
	if i <= 0 || i+2 >= len(rest) {
		return ""
	}
	return rest[:i] + "/" + rest[i+2:]
}

// List returns every downloaded model with its size on disk, largest first.
// A missing cache directory is an empty cache, not an error.
func (c *Cache) List() ([]Model, error) {
	entries, err := os.ReadDir(c.hub)
	if errors.Is(err, fs.ErrNotExist) {
		return []Model{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("modelcache: read %s: %w", c.hub, err)
	}
	out := []Model{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		repo := repoFromFolder(e.Name())
		if repo == "" {
			continue
		}
		out = append(out, Model{RepoID: repo, SizeBytes: dirSize(filepath.Join(c.hub, e.Name()))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SizeBytes > out[j].SizeBytes })
	return out, nil
}

// dirSize sums regular files without following symlinks. The cache's
// "snapshots" tree is symlinks into "blobs", so this counts each byte once.
func dirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// Delete removes one model. See the package comment for what it refuses.
func (c *Cache) Delete(repoID string) error {
	if !repoPattern.MatchString(repoID) || strings.Contains(repoID, "..") {
		return fmt.Errorf("%w: %q", ErrInvalidRepo, repoID)
	}
	target := filepath.Join(c.hub, folderName(repoID))

	// The target must be a real directory directly inside the hub dir, never
	// a symlink (which could point anywhere).
	info, err := os.Lstat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s", ErrNotFound, repoID)
	}
	if err != nil {
		return fmt.Errorf("modelcache: stat %s: %w", target, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%w: %q is not a plain cache directory", ErrInvalidRepo, repoID)
	}
	if filepath.Dir(target) != filepath.Clean(c.hub) {
		return fmt.Errorf("%w: %q escapes the cache directory", ErrInvalidRepo, repoID)
	}
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("modelcache: remove %s: %w", target, err)
	}
	// Hugging Face keeps per-repo lock files under .locks; remove them too so
	// a deleted model leaves nothing behind. Best-effort.
	_ = os.RemoveAll(filepath.Join(c.hub, ".locks", folderName(repoID)))
	return nil
}
