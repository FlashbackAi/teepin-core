// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Package githubstore pushes each Kumbha session's checkpointed workspace
// to a Teepin-owned repo under the TeepinWebServices GitHub org — a
// backing store using git as a robust, diffable version-storage backend,
// not a customer-facing collaboration surface. The repo this package
// creates is NEVER exposed to the customer: they only ever get the
// existing ZIP download (kumbha.Store.BuildArchive /
// GET /v1/kumbha/sessions/:id/workspace/archive). Both exported methods
// below return only what a caller needs to keep working, never a repo
// name/URL in a form pkg/api could accidentally surface in a customer
// response — PushSnapshot returns only error, and the repo name is
// derived deterministically from the session ID rather than handed back
// anywhere a response builder might reach for it.
//
// Authenticates as a GitHub App (bradleyfalzon/ghinstallation), not an
// OAuth App or a personal access token — the GitHub-recommended mechanism
// for a service acting AS AN ORGANIZATION rather than as one human:
// fine-grained permissions (Contents: write, Administration: write — the
// latter specifically so a not-yet-created repo can be created at all),
// short-lived installation tokens minted from the App's own private key
// (ghinstallation.Transport handles the JWT exchange and refresh
// internally), and commits attributed to the App's own bot identity
// rather than a person's.
package githubstore

import (
	"context"
	"fmt"
	"net/http"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v88/github"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/kumbha"
)

// defaultBranch matches GitHub's own platform-wide default (since 2020)
// for a freshly created repo — nothing here configures or overrides it,
// so this must track whatever a fresh repo under the org actually gets.
const defaultBranch = "main"

// Service pushes Kumbha workspace snapshots to Teepin's own GitHub org.
type Service struct {
	client *github.Client
	org    string
}

// NewService builds a Service authenticated as the given GitHub App
// installation. privateKeyPEM is the App's own private key (downloaded
// once from GitHub's App settings UI), org is the GitHub org repos are
// created under (e.g. "TeepinWebServices").
func NewService(appID, installationID int64, privateKeyPEM []byte, org string) (*Service, error) {
	tr, err := ghinstallation.New(http.DefaultTransport, appID, installationID, privateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("githubstore: build installation transport: %w", err)
	}
	client, err := github.NewClient(github.WithTransport(tr))
	if err != nil {
		return nil, fmt.Errorf("githubstore: build client: %w", err)
	}
	return &Service{
		client: client,
		org:    org,
	}, nil
}

// repoName derives THE deterministic repo name for a session — the same
// value on every call, so ProvisionRepo/PushSnapshot never need it passed
// in or looked up. Mirrors the existing "kumbha-agent-<short-id>" /
// "inst-<short-id>" naming convention already used elsewhere in this
// codebase (pkg/kumbha/agent.go's LaunchAgent, pkg/compute's instance
// IDs).
func repoName(sessionID uuid.UUID) string {
	return "kumbha-" + sessionID.String()[:8]
}

// ProvisionRepo creates a new, private repo for sessionID if one doesn't
// already exist, and returns its name either way — idempotent, same
// "already provisioned" short-circuit pattern as
// harbor.Service.ProvisionProjectRegistry. Callers are expected to persist
// the result (kumbha.Store.SetGithubRepo) and only call this again for a
// session that has never been provisioned, so the common case never pays
// this existence check at all — but calling it redundantly is still safe.
func (s *Service) ProvisionRepo(ctx context.Context, sessionID uuid.UUID) (string, error) {
	name := repoName(sessionID)

	_, resp, err := s.client.Repositories.Get(ctx, s.org, name)
	if err == nil {
		return name, nil
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		return "", fmt.Errorf("githubstore: check repo existence: %w", err)
	}

	_, _, err = s.client.Repositories.Create(ctx, s.org, &github.Repository{
		Name: github.String(name),
		// Private, never AutoInit: the FIRST real content is PushSnapshot's
		// own commit, not GitHub's own README/gitignore scaffold — nothing
		// in this repo the customer didn't put in their own ZIP.
		Private:  github.Bool(true),
		AutoInit: github.Bool(false),
	})
	if err != nil {
		return "", fmt.Errorf("githubstore: create repo: %w", err)
	}
	return name, nil
}

// PushSnapshot commits files to sessionID's repo (see ProvisionRepo) as
// one new commit on the default branch — the git data API's tree-based
// commit flow (create a tree, create a commit, move the branch ref),
// not a local clone: the control plane has no git binary and no working
// filesystem state to manage, and this needs neither. Handles both "first
// commit to an empty repo" (GitHub returns 409 for a ref lookup against a
// repo with zero commits — a well-documented API quirk, not treated as a
// real error) and "commit on top of existing history".
//
// files is exactly the []kumbha.WorkspaceFile shape SaveVersion already
// builds — plain decoded text, no encoding step needed here. Binary files
// are already excluded upstream (kumbha.SkippedFile), matching the
// existing ZIP/version-history behaviour — this does not introduce a new
// limitation.
func (s *Service) PushSnapshot(ctx context.Context, sessionID uuid.UUID, files []kumbha.WorkspaceFile, message string) error {
	name := repoName(sessionID)

	ref, resp, err := s.client.Git.GetRef(ctx, s.org, name, "refs/heads/"+defaultBranch)
	emptyRepo := false
	switch {
	case err == nil:
		// Existing history — proceed with ref.Object.SHA as the parent.
	case resp != nil && resp.StatusCode == http.StatusConflict:
		// "Git Repository is empty." — no commits yet, nothing to branch from.
		emptyRepo = true
	case resp != nil && resp.StatusCode == http.StatusNotFound:
		// No such ref (e.g. the org's default branch name differs from
		// "main") — treated the same as empty: this call creates it.
		emptyRepo = true
	default:
		return fmt.Errorf("githubstore: get ref: %w", err)
	}

	entries := make([]*github.TreeEntry, 0, len(files))
	for _, f := range files {
		entries = append(entries, &github.TreeEntry{
			Path:    github.String(f.Path),
			Mode:    github.String("100644"),
			Type:    github.String("blob"),
			Content: github.String(f.Content),
		})
	}

	baseTree := ""
	if !emptyRepo {
		baseTree = ref.GetObject().GetSHA()
	}
	newTree, _, err := s.client.Git.CreateTree(ctx, s.org, name, baseTree, entries)
	if err != nil {
		return fmt.Errorf("githubstore: create tree: %w", err)
	}

	commit := github.Commit{
		Message: github.String(message),
		Tree:    newTree,
	}
	if !emptyRepo {
		commit.Parents = []*github.Commit{{SHA: github.String(ref.GetObject().GetSHA())}}
	}
	newCommit, _, err := s.client.Git.CreateCommit(ctx, s.org, name, commit, nil)
	if err != nil {
		return fmt.Errorf("githubstore: create commit: %w", err)
	}

	if emptyRepo {
		if _, _, err := s.client.Git.CreateRef(ctx, s.org, name, github.CreateRef{
			Ref: "refs/heads/" + defaultBranch,
			SHA: newCommit.GetSHA(),
		}); err != nil {
			return fmt.Errorf("githubstore: create ref: %w", err)
		}
		return nil
	}
	if _, _, err := s.client.Git.UpdateRef(ctx, s.org, name, "heads/"+defaultBranch, github.UpdateRef{
		SHA: newCommit.GetSHA(),
	}); err != nil {
		return fmt.Errorf("githubstore: update ref: %w", err)
	}
	return nil
}
