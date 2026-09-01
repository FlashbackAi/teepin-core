// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package githubstore

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-github/v88/github"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/kumbha"
)

// newTestService builds a Service against a fake GitHub API server —
// go-github's Client.baseURL is unexported, so WithEnterpriseURLs (which
// requires and auto-appends the "/api/v3/" path GitHub Enterprise/the
// test server below both serve under) is the supported way to redirect
// it, the same technique go-github's own test suite uses.
func newTestService(t *testing.T, mux *http.ServeMux) *Service {
	t.Helper()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client, err := github.NewClient(github.WithEnterpriseURLs(server.URL, server.URL))
	if err != nil {
		t.Fatalf("build test client: %v", err)
	}
	return &Service{client: client, org: "test-org"}
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, v any) {
	t.Helper()
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func TestProvisionRepo_AlreadyExistsIsIdempotent(t *testing.T) {
	sessionID := uuid.New()
	name := repoName(sessionID)
	createCalled := false

	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/api/v3/repos/test-org/%s", name), func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, &github.Repository{Name: github.String(name)})
	})
	mux.HandleFunc("/api/v3/orgs/test-org/repos", func(w http.ResponseWriter, r *http.Request) {
		createCalled = true
		writeJSON(t, w, http.StatusCreated, &github.Repository{Name: github.String(name)})
	})

	svc := newTestService(t, mux)
	got, err := svc.ProvisionRepo(t.Context(), sessionID)
	if err != nil {
		t.Fatalf("ProvisionRepo: %v", err)
	}
	if got != name {
		t.Errorf("got repo name %q, want %q", got, name)
	}
	if createCalled {
		t.Error("ProvisionRepo called Create for a repo that already exists — must be idempotent")
	}
}

func TestProvisionRepo_CreatesWhenMissing(t *testing.T) {
	sessionID := uuid.New()
	name := repoName(sessionID)
	created := false

	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/api/v3/repos/test-org/%s", name), func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusNotFound, &github.ErrorResponse{Message: "Not Found"})
	})
	mux.HandleFunc("/api/v3/orgs/test-org/repos", func(w http.ResponseWriter, r *http.Request) {
		created = true
		var body github.Repository
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode create request: %v", err)
		}
		if body.GetName() != name {
			t.Errorf("Create called with name %q, want %q", body.GetName(), name)
		}
		if !body.GetPrivate() {
			t.Error("Create called with Private=false, want true — this repo must never be public")
		}
		if body.GetAutoInit() {
			t.Error("Create called with AutoInit=true — must not inject a README/scaffold the customer never wrote")
		}
		writeJSON(t, w, http.StatusCreated, &github.Repository{Name: github.String(name)})
	})

	svc := newTestService(t, mux)
	got, err := svc.ProvisionRepo(t.Context(), sessionID)
	if err != nil {
		t.Fatalf("ProvisionRepo: %v", err)
	}
	if got != name {
		t.Errorf("got repo name %q, want %q", got, name)
	}
	if !created {
		t.Error("ProvisionRepo did not call Create for a repo that does not exist")
	}
}

func TestPushSnapshot_FirstCommitToEmptyRepo(t *testing.T) {
	sessionID := uuid.New()
	name := repoName(sessionID)
	var sawBaseTree *string
	var sawParents int
	refCreated := false
	refUpdated := false

	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/api/v3/repos/test-org/%s/git/ref/heads/main", name), func(w http.ResponseWriter, r *http.Request) {
		// GitHub returns 409 for a ref lookup against a repo with zero
		// commits — the exact quirk PushSnapshot's emptyRepo detection
		// handles.
		writeJSON(t, w, http.StatusConflict, &github.ErrorResponse{Message: "Git Repository is empty."})
	})
	mux.HandleFunc(fmt.Sprintf("/api/v3/repos/test-org/%s/git/trees", name), func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			BaseTree *string `json:"base_tree"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		sawBaseTree = body.BaseTree
		writeJSON(t, w, http.StatusCreated, &github.Tree{SHA: github.String("tree-sha-1")})
	})
	mux.HandleFunc(fmt.Sprintf("/api/v3/repos/test-org/%s/git/commits", name), func(w http.ResponseWriter, r *http.Request) {
		// CreateCommit's wire format flattens Parents into a plain SHA
		// string array (see git_commits.go's own createCommit type), not
		// the nested-object shape the Commit struct itself uses.
		var body struct {
			Parents []string `json:"parents"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		sawParents = len(body.Parents)
		writeJSON(t, w, http.StatusCreated, &github.Commit{SHA: github.String("commit-sha-1")})
	})
	mux.HandleFunc(fmt.Sprintf("/api/v3/repos/test-org/%s/git/refs", name), func(w http.ResponseWriter, r *http.Request) {
		refCreated = true
		writeJSON(t, w, http.StatusCreated, &github.Reference{Ref: github.String("refs/heads/main")})
	})
	mux.HandleFunc(fmt.Sprintf("/api/v3/repos/test-org/%s/git/refs/heads/main", name), func(w http.ResponseWriter, r *http.Request) {
		refUpdated = true
		writeJSON(t, w, http.StatusOK, &github.Reference{Ref: github.String("refs/heads/main")})
	})

	svc := newTestService(t, mux)
	files := []kumbha.WorkspaceFile{{Path: "index.html", Content: "<html></html>"}}
	if err := svc.PushSnapshot(t.Context(), sessionID, files, "Deploy: v1"); err != nil {
		t.Fatalf("PushSnapshot: %v", err)
	}

	if sawBaseTree != nil {
		t.Errorf("CreateTree called with base_tree = %v, want nil for a first commit to an empty repo", *sawBaseTree)
	}
	if sawParents != 0 {
		t.Errorf("commit had %d parents, want 0 for the first commit", sawParents)
	}
	if !refCreated {
		t.Error("expected CreateRef to be called for an empty repo's first commit")
	}
	if refUpdated {
		t.Error("UpdateRef must not be called for an empty repo's first commit")
	}
}

func TestPushSnapshot_CommitOnTopOfHistory(t *testing.T) {
	sessionID := uuid.New()
	name := repoName(sessionID)
	const existingSHA = "existing-head-sha"
	var sawBaseTree *string
	var sawParentSHA string
	refCreated := false
	refUpdated := false

	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/api/v3/repos/test-org/%s/git/ref/heads/main", name), func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, &github.Reference{
			Ref:    github.String("refs/heads/main"),
			Object: &github.GitObject{SHA: github.String(existingSHA)},
		})
	})
	mux.HandleFunc(fmt.Sprintf("/api/v3/repos/test-org/%s/git/trees", name), func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			BaseTree *string `json:"base_tree"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		sawBaseTree = body.BaseTree
		writeJSON(t, w, http.StatusCreated, &github.Tree{SHA: github.String("tree-sha-2")})
	})
	mux.HandleFunc(fmt.Sprintf("/api/v3/repos/test-org/%s/git/commits", name), func(w http.ResponseWriter, r *http.Request) {
		// CreateCommit's wire format flattens Parents into a plain SHA
		// string array (see git_commits.go's own createCommit type), not
		// the nested-object shape the Commit struct itself uses.
		var body struct {
			Parents []string `json:"parents"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if len(body.Parents) == 1 {
			sawParentSHA = body.Parents[0]
		}
		writeJSON(t, w, http.StatusCreated, &github.Commit{SHA: github.String("commit-sha-2")})
	})
	mux.HandleFunc(fmt.Sprintf("/api/v3/repos/test-org/%s/git/refs", name), func(w http.ResponseWriter, r *http.Request) {
		refCreated = true
		writeJSON(t, w, http.StatusCreated, &github.Reference{})
	})
	mux.HandleFunc(fmt.Sprintf("/api/v3/repos/test-org/%s/git/refs/heads/main", name), func(w http.ResponseWriter, r *http.Request) {
		refUpdated = true
		writeJSON(t, w, http.StatusOK, &github.Reference{})
	})

	svc := newTestService(t, mux)
	files := []kumbha.WorkspaceFile{{Path: "index.html", Content: "<html>v2</html>"}}
	if err := svc.PushSnapshot(t.Context(), sessionID, files, "Deploy: v2"); err != nil {
		t.Fatalf("PushSnapshot: %v", err)
	}

	if sawBaseTree == nil || *sawBaseTree != existingSHA {
		t.Errorf("CreateTree base_tree = %v, want %q", sawBaseTree, existingSHA)
	}
	if sawParentSHA != existingSHA {
		t.Errorf("commit parent SHA = %q, want %q", sawParentSHA, existingSHA)
	}
	if refCreated {
		t.Error("CreateRef must not be called when the branch already has history")
	}
	if !refUpdated {
		t.Error("expected UpdateRef to be called to move the branch forward")
	}
}

// TestPushSnapshot_SignatureCannotLeakRepoIdentity is a compile-time
// guarantee, not a runtime check: PushSnapshot returns only error, so
// there is no way for a caller (pkg/api) to receive a repo name/URL back
// from it and accidentally thread it into a customer-facing response —
// the constraint is enforced by the type system, not by convention. This
// test exists so the guarantee is documented and would fail to compile
// (not fail at runtime) if the signature ever grew a second return value.
func TestPushSnapshot_SignatureCannotLeakRepoIdentity(t *testing.T) {
	var f func(*Service) error = func(s *Service) error {
		return s.PushSnapshot(t.Context(), uuid.New(), nil, "")
	}
	_ = f
}
