package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildRegistryPublishRequestUsesCleanPushedGitHubHead(t *testing.T) {
	repo, packDir := setupRegistryPublishRepo(t)

	request, err := buildRegistryPublishRequest(packDir, registryPublishOptions{})
	if err != nil {
		t.Fatalf("buildRegistryPublishRequest: %v", err)
	}

	commit := runRegistryPublishGit(t, repo, "rev-parse", "HEAD")
	if request.RepoURL != "https://github.com/gastownhall/demo-packs" {
		t.Fatalf("RepoURL = %q", request.RepoURL)
	}
	if request.Commit != commit {
		t.Fatalf("Commit = %q, want %q", request.Commit, commit)
	}
	if request.PackPath != "packs/demo" {
		t.Fatalf("PackPath = %q", request.PackPath)
	}
	if request.RequestedName != "demo-pack" || request.RequestedVersion != "0.2.0" {
		t.Fatalf("pack identity = %s %s", request.RequestedName, request.RequestedVersion)
	}
	if request.RequestedRef != "main" {
		t.Fatalf("RequestedRef = %q", request.RequestedRef)
	}
	if request.RequestedDescription != "Demo pack for registry publishing." {
		t.Fatalf("RequestedDescription = %q", request.RequestedDescription)
	}
}

func TestBuildRegistryPublishRequestRejectsDirtyTree(t *testing.T) {
	_, packDir := setupRegistryPublishRepo(t)
	if err := os.WriteFile(filepath.Join(packDir, "README.md"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := buildRegistryPublishRequest(packDir, registryPublishOptions{})
	if err == nil || !strings.Contains(err.Error(), "working tree") {
		t.Fatalf("err = %v, want dirty working tree error", err)
	}
}

func TestSubmitRegistryPublishRequestSendsAuthenticatedPayload(t *testing.T) {
	var got registryPublishRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		if r.URL.Path != "/api/publish-requests" || r.URL.Query().Get("validate") != "1" {
			t.Fatalf("url = %s", r.URL.String())
		}
		if r.Header.Get("X-CSRF-Token") != "csrf-test" {
			t.Fatalf("csrf = %q", r.Header.Get("X-CSRF-Token"))
		}
		cookie, err := r.Cookie("registry_session")
		if err != nil || cookie.Value != "session-test" {
			t.Fatalf("cookie = %v %v", cookie, err)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("Decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"publishRequest": {
				"id": "prq_test",
				"status": "pending_review",
				"requestedName": "demo-pack",
				"requestedVersion": "0.2.0",
				"repository": {"fullName": "gastownhall/demo-packs"},
				"registryEntry": {"release": {"hash": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
			}
		}`))
	}))
	defer server.Close()

	submitted, err := submitRegistryPublishRequest(
		t.Context(),
		server.Client(),
		server.URL,
		registryPublishRequest{
			RepoURL:          "https://github.com/gastownhall/demo-packs",
			Commit:           strings.Repeat("1", 40),
			PackPath:         "packs/demo",
			RequestedName:    "demo-pack",
			RequestedVersion: "0.2.0",
			RequestedRef:     "main",
		},
		registryPublishAuth{SessionCookie: "session-test", CSRFToken: "csrf-test"},
		true,
	)
	if err != nil {
		t.Fatalf("submitRegistryPublishRequest: %v", err)
	}
	if got.RequestedName != "demo-pack" || got.RequestedVersion != "0.2.0" {
		t.Fatalf("submitted body = %+v", got)
	}
	if submitted.ID != "prq_test" || submitted.Status != "pending_review" {
		t.Fatalf("submitted = %+v", submitted)
	}
	if submitted.Hash == "" {
		t.Fatalf("submitted hash missing: %+v", submitted)
	}
}

func TestSubmitRegistryPublishRequestSendsBearerToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer gcr_test_token" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := r.Header.Get("X-CSRF-Token"); got != "" {
			t.Fatalf("csrf = %q, want empty with bearer auth", got)
		}
		if got := r.Header.Get("Cookie"); got != "" {
			t.Fatalf("cookie = %q, want empty with bearer auth", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"publishRequest": {
				"id": "prq_token",
				"status": "pending_review",
				"requestedName": "demo-pack",
				"requestedVersion": "0.2.0",
				"repository": {"fullName": "gastownhall/demo-packs"}
			}
		}`))
	}))
	defer server.Close()

	submitted, err := submitRegistryPublishRequest(
		t.Context(),
		server.Client(),
		server.URL,
		registryPublishRequest{
			RepoURL:          "https://github.com/gastownhall/demo-packs",
			Commit:           strings.Repeat("1", 40),
			PackPath:         "packs/demo",
			RequestedName:    "demo-pack",
			RequestedVersion: "0.2.0",
		},
		registryPublishAuth{Token: "gcr_test_token"},
		true,
	)
	if err != nil {
		t.Fatalf("submitRegistryPublishRequest: %v", err)
	}
	if submitted.ID != "prq_token" {
		t.Fatalf("submitted = %+v", submitted)
	}
}

func TestRegistryPublishDevAuthFetchesLocalSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/dev/sign-in":
			if r.URL.Query().Get("handle") != "cli-test" {
				t.Fatalf("handle = %q", r.URL.Query().Get("handle"))
			}
			http.SetCookie(w, &http.Cookie{Name: "registry_session", Value: "session-dev", Path: "/"})
			w.Header().Set("Location", "/api/me")
			w.WriteHeader(http.StatusFound)
		case "/api/me":
			cookie, err := r.Cookie("registry_session")
			if err != nil || cookie.Value != "session-dev" {
				t.Fatalf("cookie = %v %v", cookie, err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"csrfToken":"csrf-dev"}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	auth, err := registryPublishDevAuth(t.Context(), server.Client(), server.URL, "cli-test")
	if err != nil {
		t.Fatalf("registryPublishDevAuth: %v", err)
	}
	if auth.SessionCookie != "session-dev" || auth.CSRFToken != "csrf-dev" {
		t.Fatalf("auth = %+v", auth)
	}
}

func TestDoRegistryPublishDryRunPrintsRequest(t *testing.T) {
	_, packDir := setupRegistryPublishRepo(t)
	var stdout, stderr bytes.Buffer
	code := doRegistryPublish(packDir, registryPublishOptions{
		RegistryURL: "http://127.0.0.1:8080",
		DryRun:      true,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRegistryPublish = %d, stderr=%q", code, stderr.String())
	}
	for _, want := range []string{
		"Registry: http://127.0.0.1:8080",
		"Repository: https://github.com/gastownhall/demo-packs",
		"Pack path: packs/demo",
		"Pack: demo-pack 0.2.0",
		"Dry run: publish request was not submitted.",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout.String())
		}
	}
}

func setupRegistryPublishRepo(t *testing.T) (repo string, packDir string) {
	t.Helper()
	root := t.TempDir()
	repo = filepath.Join(root, "repo")
	remote := filepath.Join(root, "remote.git")
	if err := os.MkdirAll(filepath.Join(repo, "packs", "demo"), 0o755); err != nil {
		t.Fatalf("MkdirAll(repo): %v", err)
	}
	runRegistryPublishGit(t, repo, "init", "-b", "main")
	runRegistryPublishGit(t, repo, "config", "user.email", "publisher@example.com")
	runRegistryPublishGit(t, repo, "config", "user.name", "Pack Publisher")
	packDir = filepath.Join(repo, "packs", "demo")
	if err := os.WriteFile(filepath.Join(packDir, "pack.toml"), []byte(`[pack]
name = "demo-pack"
version = "0.2.0"
schema = 2
description = "Demo pack for registry publishing."
`), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}
	runRegistryPublishGit(t, repo, "add", ".")
	runRegistryPublishGit(t, repo, "commit", "-m", "add demo pack")
	runRegistryPublishGit(t, root, "init", "--bare", remote)
	runRegistryPublishGit(t, repo, "remote", "add", "origin", remote)
	runRegistryPublishGit(t, repo, "push", "-u", "origin", "HEAD:main")
	runRegistryPublishGit(t, repo, "remote", "set-url", "origin", "git@github.com:gastownhall/demo-packs.git")
	return repo, packDir
}

func runRegistryPublishGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, string(out))
	}
	return strings.TrimSpace(string(out))
}
