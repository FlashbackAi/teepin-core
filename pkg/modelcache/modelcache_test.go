// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package modelcache

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// makeModel lays out a cache entry the way huggingface_hub does: bytes in
// blobs/, symlinks to them under snapshots/.
func makeModel(t *testing.T, hub, repo string, blobBytes int) {
	t.Helper()
	root := filepath.Join(hub, folderName(repo))
	blobs := filepath.Join(root, "blobs")
	snap := filepath.Join(root, "snapshots", "abc123")
	if err := os.MkdirAll(blobs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(snap, 0o755); err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(blobs, "deadbeef")
	if err := os.WriteFile(blob, make([]byte, blobBytes), 0o644); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(blob, filepath.Join(snap, "model.safetensors")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestList_SizesEachModelOnceAndSortsLargestFirst(t *testing.T) {
	hub := t.TempDir()
	makeModel(t, hub, "org/small", 100)
	makeModel(t, hub, "DreamFoundries/K2-Horizon-MoVA-36B-A4B-MLX-4bit", 5000)
	// Not model folders: must be ignored.
	_ = os.MkdirAll(filepath.Join(hub, ".locks"), 0o755)
	_ = os.MkdirAll(filepath.Join(hub, "datasets--x--y"), 0o755)

	got, err := New(hub).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("List = %+v, want 2 models", got)
	}
	if got[0].RepoID != "DreamFoundries/K2-Horizon-MoVA-36B-A4B-MLX-4bit" || got[0].SizeBytes != 5000 {
		t.Errorf("first = %+v, want the larger model at exactly 5000 bytes (symlinks must not double count)", got[0])
	}
	if got[1].RepoID != "org/small" || got[1].SizeBytes != 100 {
		t.Errorf("second = %+v", got[1])
	}
}

func TestList_MissingCacheIsEmptyNotAnError(t *testing.T) {
	got, err := New(filepath.Join(t.TempDir(), "nope")).List()
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("List = %v, %v; want an empty non-nil slice and no error", got, err)
	}
}

func TestDelete_RemovesOnlyThatModel(t *testing.T) {
	hub := t.TempDir()
	makeModel(t, hub, "org/a", 10)
	makeModel(t, hub, "org/b", 10)
	_ = os.MkdirAll(filepath.Join(hub, ".locks", folderName("org/a")), 0o755)

	c := New(hub)
	if err := c.Delete("org/a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(hub, folderName("org/a"))); !os.IsNotExist(err) {
		t.Error("model directory still exists")
	}
	if _, err := os.Stat(filepath.Join(hub, ".locks", folderName("org/a"))); !os.IsNotExist(err) {
		t.Error("lock directory was not cleaned up")
	}
	if _, err := os.Stat(filepath.Join(hub, folderName("org/b"))); err != nil {
		t.Error("an unrelated model was removed")
	}
}

func TestDelete_NotFound(t *testing.T) {
	if err := New(t.TempDir()).Delete("org/missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// The repo id comes from the network, so anything that is not a plain
// org/name must be refused before any path is built.
func TestDelete_RejectsHostileRepoIDs(t *testing.T) {
	hub := t.TempDir()
	makeModel(t, hub, "org/a", 10)
	bad := []string{
		"", "org", "/org/a", "org/a/", "org//a", "../etc", "org/..", "../org/a",
		"org/a/../../x", "org/a b", "org/a;rm", "org\\a", ".hidden/a", "org/.a", "a/b/c",
	}
	c := New(hub)
	for _, id := range bad {
		if err := c.Delete(id); !errors.Is(err, ErrInvalidRepo) {
			t.Errorf("Delete(%q) = %v, want ErrInvalidRepo", id, err)
		}
	}
	if _, err := os.Stat(filepath.Join(hub, folderName("org/a"))); err != nil {
		t.Error("a rejected delete removed a real model")
	}
}

// A symlinked model folder could point outside the cache; it must be refused,
// not followed.
func TestDelete_RefusesSymlinkedModelFolder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevated rights on windows")
	}
	hub, outside := t.TempDir(), t.TempDir()
	victim := filepath.Join(outside, "precious")
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(hub, folderName("org/evil"))); err != nil {
		t.Fatal(err)
	}
	if err := New(hub).Delete("org/evil"); !errors.Is(err, ErrInvalidRepo) {
		t.Fatalf("err = %v, want ErrInvalidRepo", err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatal("a file outside the cache was deleted through a symlink")
	}
}

func TestRepoFromFolder_RoundTrip(t *testing.T) {
	for _, repo := range []string{"org/name", "mlx-community/Qwen3-30B-A3B-4bit", "a/b.c_d-e"} {
		if got := repoFromFolder(folderName(repo)); got != repo {
			t.Errorf("round trip %q -> %q", repo, got)
		}
	}
	for _, bad := range []string{"datasets--a--b", "models--", "models--onlyone", "models----x"} {
		if got := repoFromFolder(bad); got != "" {
			t.Errorf("repoFromFolder(%q) = %q, want empty", bad, got)
		}
	}
}
