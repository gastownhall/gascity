package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/credentialprovider"
)

func TestBuildRegistryPublishRequestUsesCleanPushedGitHubHead(t *testing.T) {
	repo, packDir := setupRegistryPublishRepo(t)

	request, err := buildRegistryPublishRequest(t.Context(), packDir, registryPublishOptions{}, false)
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

func TestBuildRegistryPublishRequestAcceptsWebFormFieldOverrides(t *testing.T) {
	_, packDir := setupRegistryPublishRepo(t)

	request, err := buildRegistryPublishRequest(t.Context(), packDir, registryPublishOptions{
		Name:        "renamed-demo-pack",
		Version:     "1.2.3",
		Ref:         "release/v1.2.3",
		Description: "Operator supplied release note.",
	}, false)
	if err != nil {
		t.Fatalf("buildRegistryPublishRequest: %v", err)
	}

	if request.RequestedName != "renamed-demo-pack" {
		t.Fatalf("RequestedName = %q", request.RequestedName)
	}
	if request.RequestedVersion != "1.2.3" {
		t.Fatalf("RequestedVersion = %q", request.RequestedVersion)
	}
	if request.RequestedRef != "release/v1.2.3" {
		t.Fatalf("RequestedRef = %q", request.RequestedRef)
	}
	if request.RequestedDescription != "Operator supplied release note." {
		t.Fatalf("RequestedDescription = %q", request.RequestedDescription)
	}
}

func TestBuildRegistryPublishRequestRejectsDirtyTree(t *testing.T) {
	_, packDir := setupRegistryPublishRepo(t)
	if err := os.WriteFile(filepath.Join(packDir, "README.md"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := buildRegistryPublishRequest(t.Context(), packDir, registryPublishOptions{}, false)
	if err == nil || !strings.Contains(err.Error(), "working tree") {
		t.Fatalf("err = %v, want dirty working tree error", err)
	}
}

func TestBuildRegistryPublishRequestNameFlagSatisfiesMissingManifestName(t *testing.T) {
	const manifestWithoutName = `[pack]
version = "0.2.0"
schema = 2
description = "Demo pack for registry publishing."
`
	_, packDir := setupRegistryPublishRepoManifest(t, manifestWithoutName)

	request, err := buildRegistryPublishRequest(t.Context(), packDir, registryPublishOptions{
		Name: "flag-supplied-name",
	}, false)
	if err != nil {
		t.Fatalf("buildRegistryPublishRequest: %v", err)
	}
	if request.RequestedName != "flag-supplied-name" {
		t.Fatalf("RequestedName = %q, want flag-supplied-name", request.RequestedName)
	}
}

func TestBuildRegistryPublishRequestRequiresNameWithoutFlagOrManifest(t *testing.T) {
	const manifestWithoutName = `[pack]
version = "0.2.0"
schema = 2
`
	_, packDir := setupRegistryPublishRepoManifest(t, manifestWithoutName)

	_, err := buildRegistryPublishRequest(t.Context(), packDir, registryPublishOptions{}, false)
	if err == nil || !strings.Contains(err.Error(), "pack name is required") {
		t.Fatalf("err = %v, want pack name required error", err)
	}
}

// TestBuildRegistryPublishRequestIgnoresPoisonedGitEnv proves the publish
// request is derived from the pack repository even when git-locating
// environment variables point elsewhere. Running `gc pack registry publish` inside a
// pre-commit hook or nested worktree tooling exports GIT_DIR/GIT_WORK_TREE/
// GIT_INDEX_FILE for an unrelated repository; the publish git subprocesses must
// strip those so status, HEAD, upstream, and remote URL are read from the pack
// repo rather than the leaked parent.
func TestBuildRegistryPublishRequestIgnoresPoisonedGitEnv(t *testing.T) {
	repo, packDir := setupRegistryPublishRepo(t)
	wantCommit := runRegistryPublishGit(t, repo, "rev-parse", "HEAD")

	// A second, unrelated repo whose git-locating env vars, if inherited by the
	// publish subprocesses, would redirect resolution away from the pack repo.
	poison := t.TempDir()
	runRegistryPublishGit(t, poison, "init", "-b", "main")
	runRegistryPublishGit(t, poison, "config", "user.email", "poison@example.com")
	runRegistryPublishGit(t, poison, "config", "user.name", "Poison Repo")
	if err := os.WriteFile(filepath.Join(poison, "POISON"), []byte("not the pack repo\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(poison): %v", err)
	}
	runRegistryPublishGit(t, poison, "add", ".")
	runRegistryPublishGit(t, poison, "commit", "-m", "poison commit")
	poisonCommit := runRegistryPublishGit(t, poison, "rev-parse", "HEAD")
	if poisonCommit == wantCommit {
		t.Fatalf("poison repo HEAD unexpectedly equals pack repo HEAD %s", wantCommit)
	}

	// Poison only after both repos are built so setup is unaffected.
	t.Setenv("GIT_DIR", filepath.Join(poison, ".git"))
	t.Setenv("GIT_WORK_TREE", poison)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(poison, ".git", "index"))

	request, err := buildRegistryPublishRequest(t.Context(), packDir, registryPublishOptions{}, false)
	if err != nil {
		t.Fatalf("buildRegistryPublishRequest with poisoned git env: %v", err)
	}
	if request.Commit != wantCommit {
		t.Fatalf("Commit = %q, want pack repo HEAD %q (must ignore poisoned GIT_DIR)", request.Commit, wantCommit)
	}
	if request.RepoURL != "https://github.com/gastownhall/demo-packs" {
		t.Fatalf("RepoURL = %q, want pack repo remote", request.RepoURL)
	}
	if request.PackPath != "packs/demo" {
		t.Fatalf("PackPath = %q, want packs/demo", request.PackPath)
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

func TestRegistryLoginStoresVerifiedToken(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "registry.json")
	t.Setenv(registryCLIConfigEnv, configPath)
	oldClient := registryPublishHTTPClient
	defer func() { registryPublishHTTPClient = oldClient }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/me" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer gcr_manual_token" {
			t.Fatalf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user":{"id":"usr_test","handle":"publisher","displayName":"Publisher"}}`))
	}))
	defer server.Close()
	registryPublishHTTPClient = server.Client()

	var stdout, stderr bytes.Buffer
	code := doRegistryLogin(t.Context(), registryLoginOptions{
		RegistryURL: server.URL,
		Token:       "gcr_manual_token",
		Timeout:     time.Second,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRegistryLogin = %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Logged in") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	token, err := readRegistryConfiguredToken(server.URL)
	if err != nil {
		t.Fatalf("readRegistryConfiguredToken: %v", err)
	}
	if token != "gcr_manual_token" {
		t.Fatalf("stored token = %q", token)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("Stat config: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config mode = %o, want 0600", got)
	}
}

func TestDoRegistryPublishUsesStoredLoginToken(t *testing.T) {
	_, packDir := setupRegistryPublishRepo(t)
	configPath := filepath.Join(t.TempDir(), "registry.json")
	clearRegistryEnv(t)
	t.Setenv(registryCLIConfigEnv, configPath)
	oldClient := registryPublishHTTPClient
	defer func() { registryPublishHTTPClient = oldClient }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer gcr_stored_token" {
			t.Fatalf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"publishRequest": {
				"id": "prq_stored",
				"status": "pending_review",
				"requestedName": "demo-pack",
				"requestedVersion": "0.2.0",
				"repository": {"fullName": "gastownhall/demo-packs"}
			}
		}`))
	}))
	defer server.Close()
	registryPublishHTTPClient = server.Client()
	if err := writeRegistryConfiguredToken(server.URL, "gcr_stored_token"); err != nil {
		t.Fatalf("writeRegistryConfiguredToken: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := doRegistryPublish(t.Context(), packDir, registryPublishOptions{
		RegistryURL: server.URL,
		Validate:    true,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRegistryPublish = %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "prq_stored") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestDoRegistryPublishUsesGasworksProviderWithoutPersistingEIA(t *testing.T) {
	_, packDir := setupRegistryPublishRepo(t)
	clearRegistryEnv(t)
	const baseURL = defaultRegistryPublishURL
	if token, err := readRegistryConfiguredToken(baseURL); err != nil || token != "" {
		t.Fatalf("pre-existing test registry token = %q, err=%v", token, err)
	}

	oldClient := registryPublishHTTPClient
	oldFactory := registryNewCredentialSource
	t.Cleanup(func() {
		registryPublishHTTPClient = oldClient
		registryNewCredentialSource = oldFactory
	})

	var gotArgv []string
	var gotRequest credentialprovider.Request
	var forceRefresh []bool
	registryNewCredentialSource = func(argv []string, request credentialprovider.Request) (registryCredentialSource, error) {
		gotArgv = append([]string(nil), argv...)
		gotRequest = request
		return func(_ context.Context, force bool) (string, error) {
			forceRefresh = append(forceRefresh, force)
			return "sts-registry-eia", nil
		}, nil
	}

	registryPublishHTTPClient = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("Authorization"); got != "Bearer sts-registry-eia" {
			t.Fatalf("Authorization = %q", got)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{
			"publishRequest": {
				"id": "prq_provider",
				"status": "pending_review",
				"requestedName": "demo-pack",
				"requestedVersion": "0.2.0",
				"repository": {"fullName": "gastownhall/demo-packs"}
			}
		}`)),
			Request: r,
		}, nil
	})}

	var stdout, stderr bytes.Buffer
	code := doRegistryPublish(t.Context(), packDir, registryPublishOptions{
		RegistryURL: baseURL,
		Validate:    true,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRegistryPublish = %d, stderr=%q", code, stderr.String())
	}
	if strings.Join(gotArgv, "\x00") != strings.Join([]string{"gasworks", "credential-provider"}, "\x00") {
		t.Fatalf("provider argv = %q", gotArgv)
	}
	if gotRequest.Audience != "registry" || gotRequest.Org != "" || gotRequest.ForceRefresh ||
		len(gotRequest.RequiredScopes) != 1 || gotRequest.RequiredScopes[0] != "registry:publish" {
		t.Fatalf("provider request = %+v", gotRequest)
	}
	if len(forceRefresh) != 1 || forceRefresh[0] {
		t.Fatalf("force refresh calls = %v, want [false]", forceRefresh)
	}
	if !strings.Contains(stdout.String(), "prq_provider") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if token, err := readRegistryConfiguredToken(baseURL); err != nil || token != "" {
		t.Fatalf("provider EIA persisted as registry token = %q, err=%v", token, err)
	}
}

func TestDoRegistryPublishDoesNotMintGasworksCredentialForCustomRegistry(t *testing.T) {
	_, packDir := setupRegistryPublishRepo(t)

	for _, tc := range []struct {
		name  string
		setup func(*testing.T)
		opts  registryPublishOptions
	}{
		{
			name:  "flag",
			setup: func(t *testing.T) { clearRegistryEnv(t) },
			opts:  registryPublishOptions{RegistryURL: "https://registry.attacker.test"},
		},
		{
			name: "environment",
			setup: func(t *testing.T) {
				clearRegistryEnv(t, registryTestEnv{
					name:  "GC_REGISTRY_URL",
					value: "https://registry.attacker.test",
				})
			},
		},
		{
			name: "stored default",
			setup: func(t *testing.T) {
				clearRegistryEnv(t)
				if err := saveRegistryCLIConfig(registryCLIConfigPath(), registryCLIConfig{
					DefaultRegistryURL: "https://registry.attacker.test",
					Registries:         map[string]registryCLIConfigEntry{},
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup(t)
			oldFactory := registryNewCredentialSource
			t.Cleanup(func() { registryNewCredentialSource = oldFactory })
			registryNewCredentialSource = func([]string, credentialprovider.Request) (registryCredentialSource, error) {
				t.Fatal("custom registry invoked the Gasworks credential provider")
				return nil, nil
			}

			var stdout, stderr bytes.Buffer
			code := doRegistryPublish(t.Context(), packDir, tc.opts, &stdout, &stderr)
			if code == 0 {
				t.Fatalf("doRegistryPublish succeeded, stdout=%q", stdout.String())
			}
			if !strings.Contains(stderr.String(), defaultRegistryPublishURL) ||
				!strings.Contains(stderr.String(), "native registry credential") {
				t.Fatalf("stderr = %q, want canonical-origin remediation", stderr.String())
			}
		})
	}
}

func TestRegistryGasworksCredentialOriginAllowsOnlyCanonicalProductionOrigin(t *testing.T) {
	for _, tc := range []struct {
		baseURL string
		want    bool
	}{
		{baseURL: defaultRegistryPublishURL, want: true},
		{baseURL: "https://REGISTRY.GASCITY.COM", want: true},
		{baseURL: "https://registry.gascity.com:443", want: true},
		{baseURL: "http://registry.gascity.com"},
		{baseURL: "https://registry.gascity.com:444"},
		{baseURL: "https://user@registry.gascity.com"},
		{baseURL: "https://registry.gascity.com.attacker.test"},
		{baseURL: "https://registry.gascity.com/api"},
		{baseURL: "https://localhost:8443"},
		{baseURL: "https://registry.attacker.test"},
	} {
		t.Run(tc.baseURL, func(t *testing.T) {
			if got := registryGasworksCredentialOriginAllowed(tc.baseURL); got != tc.want {
				t.Fatalf("registryGasworksCredentialOriginAllowed(%q) = %v, want %v", tc.baseURL, got, tc.want)
			}
		})
	}
}

func TestDoRegistryPublishProviderRefreshesOnceAfter401(t *testing.T) {
	_, packDir := setupRegistryPublishRepo(t)
	clearRegistryEnv(t)
	const baseURL = defaultRegistryPublishURL

	oldClient := registryPublishHTTPClient
	oldFactory := registryNewCredentialSource
	t.Cleanup(func() {
		registryPublishHTTPClient = oldClient
		registryNewCredentialSource = oldFactory
	})

	var forceRefresh []bool
	registryNewCredentialSource = func(_ []string, _ credentialprovider.Request) (registryCredentialSource, error) {
		return func(_ context.Context, force bool) (string, error) {
			forceRefresh = append(forceRefresh, force)
			if force {
				return "sts-refreshed", nil
			}
			return "sts-initial", nil
		}, nil
	}

	requests := 0
	registryPublishHTTPClient = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		if requests == 1 {
			if got := r.Header.Get("Authorization"); got != "Bearer sts-initial" {
				t.Fatalf("first Authorization = %q", got)
			}
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"unauthorized","message":"expired"}}`)),
				Request:    r,
			}, nil
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sts-refreshed" {
			t.Fatalf("retry Authorization = %q", got)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{
				"publishRequest": {
					"id": "prq_refreshed",
					"status": "pending_review",
					"requestedName": "demo-pack",
					"requestedVersion": "0.2.0"
				}
			}`)),
			Request: r,
		}, nil
	})}

	var stdout, stderr bytes.Buffer
	code := doRegistryPublish(t.Context(), packDir, registryPublishOptions{
		RegistryURL: baseURL,
		Validate:    true,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRegistryPublish = %d, stderr=%q", code, stderr.String())
	}
	if requests != 2 {
		t.Fatalf("publish requests = %d, want 2", requests)
	}
	if len(forceRefresh) != 2 || forceRefresh[0] || !forceRefresh[1] {
		t.Fatalf("force refresh calls = %v, want [false true]", forceRefresh)
	}
	if !strings.Contains(stdout.String(), "prq_refreshed") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestRegistryProviderReauthRoundTripperRetriesOnlyEligible401(t *testing.T) {
	t.Run("repeated 401 refreshes once", func(t *testing.T) {
		requests := 0
		refreshes := 0
		rt := &registryProviderReauthRoundTripper{
			base: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				requests++
				want := "Bearer initial"
				if requests == 2 {
					want = "Bearer refreshed"
				}
				if got := r.Header.Get("Authorization"); got != want {
					t.Fatalf("request %d Authorization = %q, want %q", requests, got, want)
				}
				return &http.Response{
					StatusCode: http.StatusUnauthorized,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"error":"unauthorized"}`)),
					Request:    r,
				}, nil
			}),
			refresh: func(_ context.Context, force bool) (string, error) {
				refreshes++
				if !force {
					t.Fatal("401 refresh was not forced")
				}
				return "refreshed", nil
			},
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://registry.example/api/publish-requests", bytes.NewReader([]byte("payload")))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer initial")
		resp, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusUnauthorized || requests != 2 || refreshes != 1 {
			t.Fatalf("status=%d requests=%d refreshes=%d", resp.StatusCode, requests, refreshes)
		}
	})

	for _, tc := range []struct {
		name       string
		status     int
		bearer     string
		replayable bool
	}{
		{name: "403", status: http.StatusForbidden, bearer: "Bearer initial", replayable: true},
		{name: "unauthenticated 401", status: http.StatusUnauthorized, replayable: true},
		{name: "non-bearer 401", status: http.StatusUnauthorized, bearer: "Basic abc", replayable: true},
		{name: "non-replayable 401", status: http.StatusUnauthorized, bearer: "Bearer initial", replayable: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			refreshes := 0
			rt := &registryProviderReauthRoundTripper{
				base: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
					requests++
					return &http.Response{
						StatusCode: tc.status,
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader(`{}`)),
						Request:    r,
					}, nil
				}),
				refresh: func(context.Context, bool) (string, error) {
					refreshes++
					return "refreshed", nil
				},
			}
			var req *http.Request
			var err error
			if tc.replayable {
				req, err = http.NewRequestWithContext(t.Context(), http.MethodPost, "https://registry.example/api/publish-requests", bytes.NewReader([]byte("payload")))
			} else {
				req, err = http.NewRequestWithContext(t.Context(), http.MethodPost, "https://registry.example/api/publish-requests", io.NopCloser(strings.NewReader("payload")))
			}
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", tc.bearer)
			resp, err := rt.RoundTrip(req)
			if err != nil {
				t.Fatalf("RoundTrip: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if requests != 1 || refreshes != 0 {
				t.Fatalf("requests=%d refreshes=%d, want 1/0", requests, refreshes)
			}
		})
	}
}

func TestRegistryProviderReauthRoundTripperRefreshHonorsCancellation(t *testing.T) {
	requests := 0
	rt := &registryProviderReauthRoundTripper{
		base: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			requests++
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{}`)),
				Request:    r,
			}, nil
		}),
		refresh: func(ctx context.Context, force bool) (string, error) {
			if !force {
				t.Fatal("401 refresh was not forced")
			}
			return "", ctx.Err()
		},
	}
	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://registry.example/api/publish-requests", bytes.NewReader([]byte("payload")))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer initial")
	cancel()

	resp, err := rt.RoundTrip(req)
	if resp != nil {
		t.Fatalf("response = %v, want nil after refresh cancellation", resp)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want no replay after cancellation", requests)
	}
}

func TestRegistryCredentialClientRefusesEveryRedirect(t *testing.T) {
	for _, status := range []int{
		http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusSeeOther,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			requests := 0
			refreshes := 0
			client := registryHTTPClientWithCredentialRefresh(&http.Client{
				Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
					requests++
					if requests > 1 {
						t.Fatalf("redirect target reached with Authorization %q", r.Header.Get("Authorization"))
					}
					return &http.Response{
						StatusCode: status,
						Header:     http.Header{"Location": []string{"https://capture.attacker.test/token"}},
						Body:       io.NopCloser(strings.NewReader("redirect")),
						Request:    r,
					}, nil
				}),
			}, func(context.Context, bool) (string, error) {
				refreshes++
				return "must-not-refresh", nil
			})
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, defaultRegistryPublishURL+"/api/me", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer initial")
			resp, err := client.Do(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if !errors.Is(err, errRegistryCredentialRedirect) {
				t.Fatalf("client.Do error = %v, want %v", err, errRegistryCredentialRedirect)
			}
			if requests != 1 || refreshes != 0 {
				t.Fatalf("requests=%d refreshes=%d, want 1/0", requests, refreshes)
			}
		})
	}
}

func TestRegistryCredentialClientAllowsOneDirect401ReplayBeforeRefusingRedirect(t *testing.T) {
	requests := 0
	refreshes := 0
	client := registryHTTPClientWithCredentialRefresh(&http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			requests++
			switch requests {
			case 1:
				return &http.Response{
					StatusCode: http.StatusUnauthorized,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"error":"expired"}`)),
					Request:    r,
				}, nil
			case 2:
				if got := r.Header.Get("Authorization"); got != "Bearer refreshed" {
					t.Fatalf("replay Authorization = %q", got)
				}
				return &http.Response{
					StatusCode: http.StatusTemporaryRedirect,
					Header:     http.Header{"Location": []string{"https://capture.attacker.test/token"}},
					Body:       io.NopCloser(strings.NewReader("redirect")),
					Request:    r,
				}, nil
			default:
				t.Fatalf("unexpected transport attempt %d", requests)
				return nil, nil
			}
		}),
	}, func(_ context.Context, force bool) (string, error) {
		refreshes++
		if !force {
			t.Fatal("401 refresh was not forced")
		}
		return "refreshed", nil
	})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, defaultRegistryPublishURL+"/api/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer initial")
	resp, err := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, errRegistryCredentialRedirect) {
		t.Fatalf("client.Do error = %v, want %v", err, errRegistryCredentialRedirect)
	}
	if requests != 2 || refreshes != 1 {
		t.Fatalf("requests=%d refreshes=%d, want 2/1", requests, refreshes)
	}
}

func TestDoRegistryPublishExplicitNativeTokenDoesNotRefreshAfter401(t *testing.T) {
	_, packDir := setupRegistryPublishRepo(t)
	clearRegistryEnv(t)

	oldClient := registryPublishHTTPClient
	oldFactory := registryNewCredentialSource
	t.Cleanup(func() {
		registryPublishHTTPClient = oldClient
		registryNewCredentialSource = oldFactory
	})
	registryNewCredentialSource = func([]string, credentialprovider.Request) (registryCredentialSource, error) {
		t.Fatal("native token path invoked the Gasworks credential provider")
		return nil, nil
	}

	requests := 0
	registryPublishHTTPClient = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		if got := r.Header.Get("Authorization"); got != "Bearer gcr_explicit" {
			t.Fatalf("Authorization = %q", got)
		}
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"unauthorized","message":"expired"}}`)),
			Request:    r,
		}, nil
	})}

	var stdout, stderr bytes.Buffer
	code := doRegistryPublish(t.Context(), packDir, registryPublishOptions{
		RegistryURL: "https://registry-native.test",
		Token:       "gcr_explicit",
		Validate:    true,
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doRegistryPublish succeeded, stdout=%q", stdout.String())
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want exactly one native-token attempt", requests)
	}
}

// TestDoRegistryPublishValidateFailsOnValidationError covers the registry
// returning a 2xx publish response that nonetheless reports a validation
// rejection. With --validate, that must exit non-zero so CI cannot treat a
// failed validation as a successful publish (gastownhall/gascity#3343 review
// attempt-8 major).
func TestDoRegistryPublishValidateFailsOnValidationError(t *testing.T) {
	_, packDir := setupRegistryPublishRepo(t)
	configPath := filepath.Join(t.TempDir(), "registry.json")
	clearRegistryEnv(t)
	t.Setenv(registryCLIConfigEnv, configPath)
	oldClient := registryPublishHTTPClient
	defer func() { registryPublishHTTPClient = oldClient }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"publishRequest": {
				"id": "prq_invalid",
				"status": "rejected",
				"requestedName": "demo-pack",
				"requestedVersion": "0.2.0",
				"validationError": "pack.toml is missing a description"
			}
		}`))
	}))
	defer server.Close()
	registryPublishHTTPClient = server.Client()
	if err := writeRegistryConfiguredToken(server.URL, "gcr_stored_token"); err != nil {
		t.Fatalf("writeRegistryConfiguredToken: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := doRegistryPublish(t.Context(), packDir, registryPublishOptions{
		RegistryURL: server.URL,
		Validate:    true,
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doRegistryPublish = 0, want non-zero on validation rejection; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "validation failed") ||
		!strings.Contains(stderr.String(), "pack.toml is missing a description") {
		t.Fatalf("stderr = %q, want it to report the validation failure", stderr.String())
	}
}

// TestDoRegistryPublishValidateFailsOnRejectedStatus covers a 2xx publish
// response with a terminal rejected status and no explicit validationError.
// --validate must still exit non-zero.
func TestDoRegistryPublishValidateFailsOnRejectedStatus(t *testing.T) {
	_, packDir := setupRegistryPublishRepo(t)
	configPath := filepath.Join(t.TempDir(), "registry.json")
	clearRegistryEnv(t)
	t.Setenv(registryCLIConfigEnv, configPath)
	oldClient := registryPublishHTTPClient
	defer func() { registryPublishHTTPClient = oldClient }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"publishRequest": {
				"id": "prq_denied",
				"status": "invalid",
				"statusReason": "name already published at a higher version",
				"requestedName": "demo-pack",
				"requestedVersion": "0.2.0"
			}
		}`))
	}))
	defer server.Close()
	registryPublishHTTPClient = server.Client()
	if err := writeRegistryConfiguredToken(server.URL, "gcr_stored_token"); err != nil {
		t.Fatalf("writeRegistryConfiguredToken: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := doRegistryPublish(t.Context(), packDir, registryPublishOptions{
		RegistryURL: server.URL,
		Validate:    true,
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doRegistryPublish = 0, want non-zero on rejected status; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "validation failed") ||
		!strings.Contains(stderr.String(), "name already published") {
		t.Fatalf("stderr = %q, want it to report the rejected status reason", stderr.String())
	}
}

// TestDoRegistryPublishStoredTokenSurvivesPartialCookieEnv covers a lone
// GC_REGISTRY_SESSION (cookie without CSRF). That is not a usable credential,
// so it must not suppress loading a valid stored login token
// (gastownhall/gascity#3343 review attempt-8 minor).
func TestDoRegistryPublishStoredTokenSurvivesPartialCookieEnv(t *testing.T) {
	_, packDir := setupRegistryPublishRepo(t)
	configPath := filepath.Join(t.TempDir(), "registry.json")
	clearRegistryEnv(t)
	t.Setenv(registryCLIConfigEnv, configPath)
	t.Setenv("GC_REGISTRY_SESSION", "stale-session-cookie")
	oldClient := registryPublishHTTPClient
	defer func() { registryPublishHTTPClient = oldClient }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer gcr_stored_token" {
			t.Fatalf("Authorization = %q, want stored bearer token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"publishRequest": {
				"id": "prq_partial",
				"status": "pending_review",
				"requestedName": "demo-pack",
				"requestedVersion": "0.2.0"
			}
		}`))
	}))
	defer server.Close()
	registryPublishHTTPClient = server.Client()
	if err := writeRegistryConfiguredToken(server.URL, "gcr_stored_token"); err != nil {
		t.Fatalf("writeRegistryConfiguredToken: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := doRegistryPublish(t.Context(), packDir, registryPublishOptions{
		RegistryURL: server.URL,
		Validate:    true,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRegistryPublish = %d, want 0; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "prq_partial") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestDoRegistryPublishUsesGitHubActionsOIDC(t *testing.T) {
	_, packDir := setupRegistryPublishRepo(t)
	clearRegistryEnv(t)
	t.Setenv(registryCLIConfigEnv, filepath.Join(t.TempDir(), "registry.json"))
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "actions-request-token")
	oldClient := registryPublishHTTPClient
	oldFactory := registryNewCredentialSource
	t.Cleanup(func() {
		registryPublishHTTPClient = oldClient
		registryNewCredentialSource = oldFactory
	})
	registryNewCredentialSource = func([]string, credentialprovider.Request) (registryCredentialSource, error) {
		t.Fatal("GitHub OIDC path invoked the Gasworks credential provider")
		return nil, nil
	}

	var sawMint bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/actions/oidc":
			if got := r.Header.Get("Authorization"); got != "Bearer actions-request-token" {
				t.Fatalf("OIDC Authorization = %q", got)
			}
			if got := r.URL.Query().Get("audience"); got != registryGitHubActionsAudience {
				t.Fatalf("audience = %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"value":"github-oidc-jwt"}`))
		case "/api/publish-tokens/github-actions/mint":
			var payload struct {
				registryPublishRequest
				GitHubOIDCToken string `json:"githubOidcToken"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("Decode mint: %v", err)
			}
			if payload.GitHubOIDCToken != "github-oidc-jwt" {
				t.Fatalf("githubOidcToken = %q", payload.GitHubOIDCToken)
			}
			if payload.RequestedName != "demo-pack" || payload.RequestedVersion != "0.2.0" {
				t.Fatalf("mint payload = %+v", payload.registryPublishRequest)
			}
			sawMint = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"gcr_actions_publish","token_type":"bearer"}`))
		case "/api/publish-requests":
			if !sawMint {
				t.Fatalf("publish happened before mint")
			}
			if got := r.Header.Get("Authorization"); got != "Bearer gcr_actions_publish" {
				t.Fatalf("Authorization = %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"publishRequest": {
					"id": "prq_actions",
					"status": "pending_review",
					"requestedName": "demo-pack",
					"requestedVersion": "0.2.0",
					"repository": {"fullName": "gastownhall/demo-packs"}
				}
			}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", server.URL+"/actions/oidc")
	registryPublishHTTPClient = server.Client()

	var stdout, stderr bytes.Buffer
	code := doRegistryPublish(t.Context(), packDir, registryPublishOptions{
		RegistryURL: server.URL,
		Validate:    true,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRegistryPublish = %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "prq_actions") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestDoRegistryPublishUsesGitHubActionsOIDCWithoutUpstream(t *testing.T) {
	packDir, headSHA := setupRegistryPublishRepoDetached(t)
	clearRegistryEnv(t)
	t.Setenv(registryCLIConfigEnv, filepath.Join(t.TempDir(), "registry.json"))
	// A detached actions/checkout has no `@{u}`; the runner metadata is the
	// authoritative repository and ref source for the publish request.
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "actions-request-token")
	t.Setenv("GITHUB_REPOSITORY", "gastownhall/demo-packs")
	t.Setenv("GITHUB_SERVER_URL", "https://github.com")
	t.Setenv("GITHUB_SHA", headSHA)
	t.Setenv("GITHUB_REF_NAME", "main")
	oldClient := registryPublishHTTPClient
	defer func() { registryPublishHTTPClient = oldClient }()

	var sawMint bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/actions/oidc":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"value":"github-oidc-jwt"}`))
		case "/api/publish-tokens/github-actions/mint":
			var payload struct {
				registryPublishRequest
				GitHubOIDCToken string `json:"githubOidcToken"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("Decode mint: %v", err)
			}
			if payload.RepoURL != "https://github.com/gastownhall/demo-packs" {
				t.Fatalf("mint repoUrl = %q", payload.RepoURL)
			}
			if payload.Commit != headSHA {
				t.Fatalf("mint commit = %q, want %q", payload.Commit, headSHA)
			}
			if payload.RequestedRef != "main" {
				t.Fatalf("mint requestedRef = %q", payload.RequestedRef)
			}
			sawMint = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"gcr_actions_publish","token_type":"bearer"}`))
		case "/api/publish-requests":
			if !sawMint {
				t.Fatalf("publish happened before mint")
			}
			if got := r.Header.Get("Authorization"); got != "Bearer gcr_actions_publish" {
				t.Fatalf("Authorization = %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"publishRequest": {
					"id": "prq_actions_detached",
					"status": "pending_review",
					"requestedName": "demo-pack",
					"requestedVersion": "0.2.0",
					"repository": {"fullName": "gastownhall/demo-packs"}
				}
			}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", server.URL+"/actions/oidc")
	registryPublishHTTPClient = server.Client()

	var stdout, stderr bytes.Buffer
	code := doRegistryPublish(t.Context(), packDir, registryPublishOptions{
		RegistryURL: server.URL,
		Validate:    true,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRegistryPublish = %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "prq_actions_detached") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestDoRegistryPublishWithoutUpstreamOrActionsFails(t *testing.T) {
	packDir, _ := setupRegistryPublishRepoDetached(t)
	clearRegistryEnv(t)
	t.Setenv(registryCLIConfigEnv, filepath.Join(t.TempDir(), "registry.json"))
	// No GitHub Actions OIDC environment: a detached checkout must still report
	// the upstream requirement rather than silently deriving a repository.
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "")
	t.Setenv("GITHUB_REPOSITORY", "gastownhall/demo-packs")

	var stdout, stderr bytes.Buffer
	code := doRegistryPublish(t.Context(), packDir, registryPublishOptions{
		RegistryURL: "https://registry.example.com",
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doRegistryPublish = 0, want failure; stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "current branch has no upstream") {
		t.Fatalf("stderr = %q, want upstream requirement", stderr.String())
	}
}

// roundTripperFunc adapts a function to http.RoundTripper so a test can assert
// that no network call is made on a path that must fail locally.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// failingRegistryHTTPClient installs an HTTP client that fails the test on any
// request and restores the previous client at cleanup. Use it to prove a
// publish is rejected locally before contacting the registry or OIDC endpoint.
func failingRegistryHTTPClient(t *testing.T) {
	t.Helper()
	old := registryPublishHTTPClient
	t.Cleanup(func() { registryPublishHTTPClient = old })
	registryPublishHTTPClient = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			t.Fatalf("unexpected network call to %s", r.URL)
			return nil, context.Canceled
		}),
	}
}

// setSpoofedGitHubActionsEnv sets GitHub Actions runner and OIDC-request
// environment variables that match a detached HEAD. These values are trivially
// spoofable outside CI, so a publish that does not authenticate through the OIDC
// mint path must not let them skip the pushed-HEAD requirement.
func setSpoofedGitHubActionsEnv(t *testing.T, headSHA string) {
	t.Helper()
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "actions-request-token")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "https://example.test/oidc")
	t.Setenv("GITHUB_REPOSITORY", "gastownhall/demo-packs")
	t.Setenv("GITHUB_SERVER_URL", "https://github.com")
	t.Setenv("GITHUB_SHA", headSHA)
	t.Setenv("GITHUB_REF_NAME", "main")
}

// TestBuildRegistryPublishRequestDetachedRequiresUpstreamWithoutOIDCMint proves
// the request builder consults the GitHub Actions repo/ref fallback only when
// the caller is on the OIDC mint path. With the fallback disabled, a detached
// checkout must still require a pushed upstream even though spoofable runner
// metadata is present; with it enabled, the runner metadata resolves the repo.
func TestBuildRegistryPublishRequestDetachedRequiresUpstreamWithoutOIDCMint(t *testing.T) {
	packDir, headSHA := setupRegistryPublishRepoDetached(t)
	setSpoofedGitHubActionsEnv(t, headSHA)

	if _, err := buildRegistryPublishRequest(t.Context(), packDir, registryPublishOptions{}, false); err == nil ||
		!strings.Contains(err.Error(), "current branch has no upstream") {
		t.Fatalf("err = %v, want no-upstream error when GitHub Actions fallback is disabled", err)
	}

	request, err := buildRegistryPublishRequest(t.Context(), packDir, registryPublishOptions{}, true)
	if err != nil {
		t.Fatalf("buildRegistryPublishRequest(allow=true): %v", err)
	}
	if request.RepoURL != "https://github.com/gastownhall/demo-packs" {
		t.Fatalf("RepoURL = %q, want runner-derived repo", request.RepoURL)
	}
	if request.RequestedRef != "main" {
		t.Fatalf("RequestedRef = %q, want main", request.RequestedRef)
	}
	if request.Commit != headSHA {
		t.Fatalf("Commit = %q, want %q", request.Commit, headSHA)
	}
}

// TestDoRegistryPublishDetachedWithExplicitTokenRequiresUpstream proves that a
// detached/no-upstream publish carrying an explicit --token does not take the
// GitHub Actions repo/ref fallback even when the Actions environment is present.
// The token short-circuits the OIDC mint path, so the publish must fail the
// upstream requirement locally rather than trusting spoofable runner metadata.
func TestDoRegistryPublishDetachedWithExplicitTokenRequiresUpstream(t *testing.T) {
	packDir, headSHA := setupRegistryPublishRepoDetached(t)
	clearRegistryEnv(t)
	t.Setenv(registryCLIConfigEnv, filepath.Join(t.TempDir(), "registry.json"))
	setSpoofedGitHubActionsEnv(t, headSHA)
	failingRegistryHTTPClient(t)

	var stdout, stderr bytes.Buffer
	code := doRegistryPublish(t.Context(), packDir, registryPublishOptions{
		RegistryURL: "https://registry.example.com",
		Token:       "gcr_explicit_token",
		Validate:    true,
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doRegistryPublish = 0, want failure; stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "current branch has no upstream") {
		t.Fatalf("stderr = %q, want upstream requirement despite spoofed Actions env", stderr.String())
	}
}

// TestDoRegistryPublishDetachedWithStoredTokenRequiresUpstream is the stored
// login token analog: a detached/no-upstream publish that authenticates with a
// stored token must also keep proving HEAD is pushed and must not be redirected
// by spoofable GitHub Actions runner metadata.
func TestDoRegistryPublishDetachedWithStoredTokenRequiresUpstream(t *testing.T) {
	packDir, headSHA := setupRegistryPublishRepoDetached(t)
	clearRegistryEnv(t)
	t.Setenv(registryCLIConfigEnv, filepath.Join(t.TempDir(), "registry.json"))
	setSpoofedGitHubActionsEnv(t, headSHA)
	failingRegistryHTTPClient(t)
	if err := writeRegistryConfiguredToken("https://registry.example.com", "gcr_stored_token"); err != nil {
		t.Fatalf("writeRegistryConfiguredToken: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := doRegistryPublish(t.Context(), packDir, registryPublishOptions{
		RegistryURL: "https://registry.example.com",
		Validate:    true,
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doRegistryPublish = 0, want failure; stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "current branch has no upstream") {
		t.Fatalf("stderr = %q, want upstream requirement despite spoofed Actions env", stderr.String())
	}
}

func TestRegistryGitHubActionsRepoRef(t *testing.T) {
	const headSHA = "0123456789abcdef0123456789abcdef01234567"
	setActionsEnv := func(t *testing.T) {
		t.Helper()
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "https://example.test/oidc")
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "req-token")
		t.Setenv("GITHUB_REPOSITORY", "gastownhall/demo-packs")
		t.Setenv("GITHUB_SERVER_URL", "https://github.com")
		t.Setenv("GITHUB_SHA", headSHA)
		t.Setenv("GITHUB_REF_NAME", "main")
	}

	t.Run("derives repo and ref from runner metadata", func(t *testing.T) {
		setActionsEnv(t)
		got, ok := registryGitHubActionsRepoRef(headSHA, registryPublishOptions{})
		if !ok {
			t.Fatal("ok = false, want true")
		}
		if got.RepoURL != "https://github.com/gastownhall/demo-packs" {
			t.Fatalf("RepoURL = %q", got.RepoURL)
		}
		if got.Ref != "main" {
			t.Fatalf("Ref = %q", got.Ref)
		}
	})

	t.Run("flag ref overrides runner ref", func(t *testing.T) {
		setActionsEnv(t)
		got, ok := registryGitHubActionsRepoRef(headSHA, registryPublishOptions{Ref: "v1.2.3"})
		if !ok || got.Ref != "v1.2.3" {
			t.Fatalf("got = %+v, ok = %v", got, ok)
		}
	})

	t.Run("default server url when unset", func(t *testing.T) {
		setActionsEnv(t)
		t.Setenv("GITHUB_SERVER_URL", "")
		got, ok := registryGitHubActionsRepoRef(headSHA, registryPublishOptions{})
		if !ok || got.RepoURL != "https://github.com/gastownhall/demo-packs" {
			t.Fatalf("got = %+v, ok = %v", got, ok)
		}
	})

	t.Run("no OIDC environment", func(t *testing.T) {
		setActionsEnv(t)
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "")
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "")
		if _, ok := registryGitHubActionsRepoRef(headSHA, registryPublishOptions{}); ok {
			t.Fatal("ok = true, want false without OIDC environment")
		}
	})

	t.Run("missing repository", func(t *testing.T) {
		setActionsEnv(t)
		t.Setenv("GITHUB_REPOSITORY", "")
		if _, ok := registryGitHubActionsRepoRef(headSHA, registryPublishOptions{}); ok {
			t.Fatal("ok = true, want false without GITHUB_REPOSITORY")
		}
	})

	t.Run("commit mismatch", func(t *testing.T) {
		setActionsEnv(t)
		t.Setenv("GITHUB_SHA", "ffffffffffffffffffffffffffffffffffffffff")
		if _, ok := registryGitHubActionsRepoRef(headSHA, registryPublishOptions{}); ok {
			t.Fatal("ok = true, want false when GITHUB_SHA != HEAD")
		}
	})

	t.Run("missing commit sha", func(t *testing.T) {
		setActionsEnv(t)
		t.Setenv("GITHUB_SHA", "")
		if _, ok := registryGitHubActionsRepoRef(headSHA, registryPublishOptions{}); ok {
			t.Fatal("ok = true, want false when GITHUB_SHA is unset")
		}
	})
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
	clearRegistryEnv(t)
	t.Setenv(registryCLIConfigEnv, filepath.Join(t.TempDir(), "registry.json"))
	var stdout, stderr bytes.Buffer
	code := doRegistryPublish(t.Context(), packDir, registryPublishOptions{
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

func TestRegistryHelpDoesNotLeakEnvironmentSecrets(t *testing.T) {
	t.Setenv("GC_REGISTRY_TOKEN", "s3cr3t-token")
	t.Setenv("GC_REGISTRY_SESSION", "s3cr3t-session")
	t.Setenv("GC_REGISTRY_CSRF_TOKEN", "s3cr3t-csrf")

	for _, sub := range []string{"publish", "login", "whoami"} {
		var help bytes.Buffer
		cmd := newPackRegistryCmd(io.Discard, io.Discard)
		cmd.SetOut(&help)
		cmd.SetErr(&help)
		cmd.SetArgs([]string{sub, "--help"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("pack registry %s --help: %v", sub, err)
		}
		if strings.Contains(help.String(), "s3cr3t") {
			t.Fatalf("pack registry %s --help leaks environment secrets:\n%s", sub, help.String())
		}
	}
}

func TestRegistryCommandErrorsUsePackNamespace(t *testing.T) {
	tests := []struct {
		name string
		run  func(io.Writer) int
	}{
		{
			name: "login",
			run: func(stderr io.Writer) int {
				return doRegistryLogin(t.Context(), registryLoginOptions{RegistryURL: "http://registry.example"}, io.Discard, stderr)
			},
		},
		{
			name: "publish",
			run: func(stderr io.Writer) int {
				return doRegistryPublish(t.Context(), "", registryPublishOptions{RegistryURL: "http://registry.example"}, io.Discard, stderr)
			},
		},
		{
			name: "whoami",
			run: func(stderr io.Writer) int {
				return doRegistryWhoami(t.Context(), registryLoginOptions{RegistryURL: "http://registry.example"}, io.Discard, stderr)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if code := test.run(&stderr); code == 0 {
				t.Fatalf("%s unexpectedly succeeded", test.name)
			}
			want := "gc pack registry " + test.name + ":"
			if !strings.HasPrefix(stderr.String(), want) {
				t.Fatalf("stderr = %q, want prefix %q", stderr.String(), want)
			}
		})
	}
}

func TestRegistryCredentialProviderArgvDefaultsToGasworks(t *testing.T) {
	argv, err := parseRegistryCredentialProviderArgv("", false)
	if err != nil {
		t.Fatalf("parseRegistryCredentialProviderArgv: %v", err)
	}
	want := []string{"gasworks", "credential-provider"}
	if strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q, want %q", argv, want)
	}
}

func TestRegistryCredentialProviderArgvUsesExactJSONOverride(t *testing.T) {
	argv, err := parseRegistryCredentialProviderArgv(
		`["/opt/Gas Works/gasworks","credential-provider","--profile","team one"]`, true,
	)
	if err != nil {
		t.Fatalf("parseRegistryCredentialProviderArgv: %v", err)
	}
	want := []string{"/opt/Gas Works/gasworks", "credential-provider", "--profile", "team one"}
	if strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q, want exact direct-exec argv %q", argv, want)
	}
}

func TestRegistryCredentialProviderArgvRejectsMalformedOrEmptyOverride(t *testing.T) {
	for _, raw := range []string{"", `{`, `null`, `[]`, `[""]`, `["gasworks",7]`} {
		t.Run(raw, func(t *testing.T) {
			if _, err := parseRegistryCredentialProviderArgv(raw, true); err == nil {
				t.Fatalf("parseRegistryCredentialProviderArgv accepted %q", raw)
			}
		})
	}
}

func TestDoRegistryPublishUsesEnvironmentToken(t *testing.T) {
	_, packDir := setupRegistryPublishRepo(t)
	clearRegistryEnv(t)
	t.Setenv(registryCLIConfigEnv, filepath.Join(t.TempDir(), "registry.json"))
	t.Setenv("GC_REGISTRY_TOKEN", "gcr_env_token")
	oldClient := registryPublishHTTPClient
	defer func() { registryPublishHTTPClient = oldClient }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer gcr_env_token" {
			t.Fatalf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"publishRequest": {
				"id": "prq_env",
				"status": "pending_review",
				"requestedName": "demo-pack",
				"requestedVersion": "0.2.0",
				"repository": {"fullName": "gastownhall/demo-packs"}
			}
		}`))
	}))
	defer server.Close()
	registryPublishHTTPClient = server.Client()

	var stdout, stderr bytes.Buffer
	code := doRegistryPublish(t.Context(), packDir, registryPublishOptions{
		RegistryURL: server.URL,
		Validate:    true,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRegistryPublish = %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "prq_env") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestDoRegistryWhoamiUsesStoredDefaultRegistryURL(t *testing.T) {
	clearRegistryEnv(t)
	t.Setenv(registryCLIConfigEnv, filepath.Join(t.TempDir(), "registry.json"))
	oldClient := registryPublishHTTPClient
	oldFactory := registryNewCredentialSource
	t.Cleanup(func() {
		registryPublishHTTPClient = oldClient
		registryNewCredentialSource = oldFactory
	})
	registryNewCredentialSource = func([]string, credentialprovider.Request) (registryCredentialSource, error) {
		t.Fatal("stored native token invoked the Gasworks credential provider")
		return nil, nil
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/me" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer gcr_default_token" {
			t.Fatalf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user":{"id":"usr_test","handle":"publisher","displayName":"Publisher"}}`))
	}))
	defer server.Close()
	registryPublishHTTPClient = server.Client()
	if err := writeRegistryConfiguredToken(server.URL, "gcr_default_token"); err != nil {
		t.Fatalf("writeRegistryConfiguredToken: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := doRegistryWhoami(t.Context(), registryLoginOptions{Timeout: time.Second}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRegistryWhoami = %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "@publisher") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestDoRegistryWhoamiUsesGasworksProviderAndRefreshesWithoutPersistingEIA(t *testing.T) {
	clearRegistryEnv(t)
	oldClient := registryPublishHTTPClient
	oldFactory := registryNewCredentialSource
	t.Cleanup(func() {
		registryPublishHTTPClient = oldClient
		registryNewCredentialSource = oldFactory
	})

	var gotArgv []string
	var gotRequest credentialprovider.Request
	var forceRefresh []bool
	registryNewCredentialSource = func(argv []string, request credentialprovider.Request) (registryCredentialSource, error) {
		gotArgv = append([]string(nil), argv...)
		gotRequest = request
		return func(_ context.Context, force bool) (string, error) {
			forceRefresh = append(forceRefresh, force)
			if force {
				return "sts-refreshed", nil
			}
			return "sts-initial", nil
		}, nil
	}
	requests := 0
	registryPublishHTTPClient = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		if requests == 1 {
			if got := r.Header.Get("Authorization"); got != "Bearer sts-initial" {
				t.Fatalf("first Authorization = %q", got)
			}
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"unauthorized","message":"expired"}}`)),
				Request:    r,
			}, nil
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sts-refreshed" {
			t.Fatalf("retry Authorization = %q", got)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"user":{"id":"usr_provider","handle":"provider-user"}}`)),
			Request:    r,
		}, nil
	})}

	var stdout, stderr bytes.Buffer
	code := doRegistryWhoami(t.Context(), registryLoginOptions{
		RegistryURL: defaultRegistryPublishURL,
		Timeout:     time.Second,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRegistryWhoami = %d, stderr=%q", code, stderr.String())
	}
	if strings.Join(gotArgv, "\x00") != strings.Join([]string{"gasworks", "credential-provider"}, "\x00") {
		t.Fatalf("provider argv = %q", gotArgv)
	}
	if gotRequest.Audience != registryCredentialAudience ||
		len(gotRequest.RequiredScopes) != 1 || gotRequest.RequiredScopes[0] != registryPublishScope {
		t.Fatalf("provider request = %+v", gotRequest)
	}
	if len(forceRefresh) != 2 || forceRefresh[0] || !forceRefresh[1] {
		t.Fatalf("force refresh calls = %v, want [false true]", forceRefresh)
	}
	if got := stdout.String(); !strings.Contains(got, "@provider-user (usr_provider)") {
		t.Fatalf("stdout = %q", got)
	}
	if token, err := readRegistryConfiguredToken(defaultRegistryPublishURL); err != nil || token != "" {
		t.Fatalf("provider EIA persisted as registry token = %q, err=%v", token, err)
	}
}

func TestDoRegistryWhoamiRejectsNonLocalHTTPRegistry(t *testing.T) {
	clearRegistryEnv(t)
	t.Setenv(registryCLIConfigEnv, filepath.Join(t.TempDir(), "registry.json"))

	var stdout, stderr bytes.Buffer
	code := doRegistryWhoami(t.Context(), registryLoginOptions{
		RegistryURL: "http://registry.example",
		Token:       "gcr_should_not_be_sent",
		Timeout:     time.Second,
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doRegistryWhoami = 0, want rejection of cleartext registry; stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "https") {
		t.Fatalf("stderr = %q, want https requirement for non-local registry", stderr.String())
	}
}

func TestRegistryCLIConfigPathUsesGCHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	t.Setenv(registryCLIConfigEnv, "")
	if got, want := registryCLIConfigPath(), filepath.Join(home, "registry.json"); got != want {
		t.Fatalf("registryCLIConfigPath() = %q, want %q", got, want)
	}

	override := filepath.Join(t.TempDir(), "custom", "registry.json")
	t.Setenv(registryCLIConfigEnv, override)
	if got := registryCLIConfigPath(); got != override {
		t.Fatalf("registryCLIConfigPath() override = %q, want %q", got, override)
	}
}

func TestSaveRegistryCLIConfigAtomicallyTightensPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	cfg := registryCLIConfig{
		DefaultRegistryURL: "https://registry.example",
		Registries: map[string]registryCLIConfigEntry{
			"https://registry.example": {Token: "gcr_atomic_token", UpdatedAt: "2026-06-13T00:00:00Z"},
		},
	}
	if err := saveRegistryCLIConfig(path, cfg); err != nil {
		t.Fatalf("saveRegistryCLIConfig: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config mode = %o, want 0600 after rewriting a 0644 file", got)
	}
	loaded, err := loadRegistryCLIConfig(path)
	if err != nil {
		t.Fatalf("loadRegistryCLIConfig: %v", err)
	}
	if loaded.DefaultRegistryURL != "https://registry.example" || loaded.Registries["https://registry.example"].Token != "gcr_atomic_token" {
		t.Fatalf("loaded = %+v", loaded)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestRegistryFetchCurrentUserReportsHTTPStatusForNonJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html><body>Bad gateway</body></html>"))
	}))
	defer server.Close()

	_, err := registryFetchCurrentUser(t.Context(), server.Client(), server.URL, "tok")
	if err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("err = %v, want HTTP 502 in error", err)
	}
	if strings.Contains(err.Error(), "invalid character") {
		t.Fatalf("err = %v, want status reported before JSON decode failure", err)
	}
}

func TestSubmitRegistryPublishRequestReportsHTTPStatusForNonJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("<html>upstream unavailable</html>"))
	}))
	defer server.Close()

	_, err := submitRegistryPublishRequest(
		t.Context(),
		server.Client(),
		server.URL,
		registryPublishRequest{
			RepoURL:          "https://github.com/gastownhall/demo-packs",
			Commit:           strings.Repeat("1", 40),
			PackPath:         ".",
			RequestedName:    "demo-pack",
			RequestedVersion: "0.2.0",
		},
		registryPublishAuth{Token: "tok"},
		false,
	)
	if err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("err = %v, want HTTP 503 in error", err)
	}
}

func TestRegistryPublishFetchCSRFReturnsToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/me" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		cookie, err := r.Cookie("registry_session")
		if err != nil || cookie.Value != "session-csrf" {
			t.Fatalf("cookie = %v %v", cookie, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"csrfToken":"csrf-token"}`))
	}))
	defer server.Close()

	csrf, err := registryPublishFetchCSRF(t.Context(), server.Client(), server.URL, "session-csrf")
	if err != nil {
		t.Fatalf("registryPublishFetchCSRF: %v", err)
	}
	if csrf != "csrf-token" {
		t.Fatalf("csrf = %q, want csrf-token", csrf)
	}
}

func TestRegistryPublishFetchCSRFReportsHTTPStatusForNonJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html><body>Bad gateway</body></html>"))
	}))
	defer server.Close()

	_, err := registryPublishFetchCSRF(t.Context(), server.Client(), server.URL, "session-csrf")
	if err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("err = %v, want HTTP 502 in error", err)
	}
	if strings.Contains(err.Error(), "invalid character") {
		t.Fatalf("err = %v, want status reported before JSON decode failure", err)
	}
	if !strings.Contains(err.Error(), "Bad gateway") {
		t.Fatalf("err = %v, want bounded response snippet in error", err)
	}
}

func TestRegistryPublishFetchCSRFRejectsMissingToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"csrfToken":"   "}`))
	}))
	defer server.Close()

	_, err := registryPublishFetchCSRF(t.Context(), server.Client(), server.URL, "session-csrf")
	if err == nil || !strings.Contains(err.Error(), "did not include a CSRF token") {
		t.Fatalf("err = %v, want missing CSRF token error", err)
	}
}

func TestRegistryPublishCookieHeader(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare value", "abc", "registry_session=abc"},
		{"bare value with base64 padding", "abc==", "registry_session=abc=="},
		{"bare value trimmed", "  abc  ", "registry_session=abc"},
		{"full session pair", "registry_session=xyz", "registry_session=xyz"},
		{"session pair with extra cookie", "registry_session=xyz; other=1", "registry_session=xyz; other=1"},
		{"foreign multi-cookie header", "a=b; c=d", "a=b; c=d"},
	}
	for _, tc := range cases {
		if got := registryPublishCookieHeader(tc.in); got != tc.want {
			t.Errorf("%s: registryPublishCookieHeader(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestNormalizeGitHubRemoteURL(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"scp-like with .git", "git@github.com:gastownhall/demo-packs.git", "https://github.com/gastownhall/demo-packs", false},
		{"scp-like without .git", "git@github.com:gastownhall/demo-packs", "https://github.com/gastownhall/demo-packs", false},
		{"ssh scheme", "ssh://git@github.com/gastownhall/demo-packs.git", "https://github.com/gastownhall/demo-packs", false},
		{"https with .git", "https://github.com/gastownhall/demo-packs.git", "https://github.com/gastownhall/demo-packs", false},
		{"http", "http://github.com/gastownhall/demo-packs", "https://github.com/gastownhall/demo-packs", false},
		{"https trailing slash", "https://github.com/gastownhall/demo-packs/", "https://github.com/gastownhall/demo-packs", false},
		{"non-github https", "https://gitlab.com/owner/repo", "", true},
		{"non-github ssh", "ssh://git@bitbucket.org/owner/repo", "", true},
		{"missing repo segment", "git@github.com:invalid", "", true},
		{"extra path segment", "https://github.com/owner/repo/extra", "", true},
	}
	for _, tc := range cases {
		got, err := normalizeGitHubRemoteURL(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: normalizeGitHubRemoteURL(%q) = %q, want error", tc.name, tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: normalizeGitHubRemoteURL(%q): %v", tc.name, tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: normalizeGitHubRemoteURL(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestNormalizeRegistryPublishBaseURL(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"empty uses production default", "", defaultRegistryPublishURL, false},
		{"trailing slash trimmed", "https://reg.example/", "https://reg.example", false},
		{"path query fragment trimmed", "https://reg.example/app/?q=1#frag", "https://reg.example/app", false},
		{"local http allowed", "http://localhost:3000", "http://localhost:3000", false},
		{"loopback ip http allowed", "http://127.0.0.1:8080", "http://127.0.0.1:8080", false},
		{"non-local http rejected", "http://registry.example", "", true},
		{"non-http scheme rejected", "ftp://reg.example", "", true},
		{"missing scheme rejected", "not a url", "", true},
		{"missing host rejected", "https://", "", true},
	}
	for _, tc := range cases {
		got, err := normalizeRegistryPublishBaseURL(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: normalizeRegistryPublishBaseURL(%q) = %q, want error", tc.name, tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: normalizeRegistryPublishBaseURL(%q): %v", tc.name, tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: normalizeRegistryPublishBaseURL(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestSplitGitUpstream(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		wantRemote string
		wantBranch string
		wantErr    bool
	}{
		{"simple", "origin/main", "origin", "main", false},
		{"branch with slash", "origin/feature/x", "origin", "feature/x", false},
		{"no branch", "origin", "", "", true},
		{"empty remote", "/main", "", "", true},
		{"empty branch", "origin/", "", "", true},
		{"empty", "", "", "", true},
	}
	for _, tc := range cases {
		remote, branch, err := splitGitUpstream(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: splitGitUpstream(%q) = (%q, %q), want error", tc.name, tc.in, remote, branch)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: splitGitUpstream(%q): %v", tc.name, tc.in, err)
			continue
		}
		if remote != tc.wantRemote || branch != tc.wantBranch {
			t.Errorf("%s: splitGitUpstream(%q) = (%q, %q), want (%q, %q)", tc.name, tc.in, remote, branch, tc.wantRemote, tc.wantBranch)
		}
	}
}

func TestRegistryPublishPackPath(t *testing.T) {
	repoRoot := filepath.Join(t.TempDir(), "repo")
	cases := []struct {
		name     string
		packRoot string
		want     string
		wantErr  bool
	}{
		{"repo root", repoRoot, ".", false},
		{"nested pack", filepath.Join(repoRoot, "packs", "demo"), "packs/demo", false},
		{"dot-dot-prefixed name inside repo", filepath.Join(repoRoot, "..foo"), "..foo", false},
		{"outside repo", filepath.Join(repoRoot, "..", "elsewhere"), "", true},
	}
	for _, tc := range cases {
		got, err := registryPublishPackPath(repoRoot, tc.packRoot)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: registryPublishPackPath(%q) = %q, want error", tc.name, tc.packRoot, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: registryPublishPackPath(%q): %v", tc.name, tc.packRoot, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: registryPublishPackPath(%q) = %q, want %q", tc.name, tc.packRoot, got, tc.want)
		}
	}
}

func TestRegistryPollDeviceToken(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		body         string
		wantToken    string
		wantInterval int
		wantPending  bool
		wantErr      string
	}{
		{"authorization pending", http.StatusBadRequest, `{"error":"authorization_pending","interval":7}`, "", 7, true, ""},
		{"slow down applies default interval", http.StatusBadRequest, `{"error":"slow_down"}`, "", 10, true, ""},
		{"access denied", http.StatusBadRequest, `{"error":"access_denied"}`, "", 0, false, "device login denied"},
		{"expired token", http.StatusBadRequest, `{"error":"expired_token"}`, "", 0, false, "device login expired"},
		{"success", http.StatusOK, `{"access_token":"gcr_device_token","token_type":"bearer"}`, "gcr_device_token", 0, false, ""},
		{"unknown error", http.StatusBadRequest, `{"error":"weird"}`, "", 0, false, "device login failed: HTTP 400"},
		{"proxy html error", http.StatusBadGateway, `<html>bad gateway</html>`, "", 0, false, "HTTP 502"},
	}
	for _, tc := range cases {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		token, interval, pending, err := registryPollDeviceToken(t.Context(), server.Client(), server.URL, "device-code-test")
		server.Close()
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: err = %v, want %q", tc.name, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: registryPollDeviceToken: %v", tc.name, err)
			continue
		}
		if token != tc.wantToken || interval != tc.wantInterval || pending != tc.wantPending {
			t.Errorf("%s: got (token=%q, interval=%d, pending=%v), want (token=%q, interval=%d, pending=%v)",
				tc.name, token, interval, pending, tc.wantToken, tc.wantInterval, tc.wantPending)
		}
	}
}

// TestRegistryDeviceLoginCompletesAfterPending drives the device-code login
// orchestration end to end: it requests a device code, prints the verification
// instructions, polls through an authorization_pending response, and returns the
// access token once the registry approves. This covers gc pack registry login
// --device above the registryPollDeviceToken helper unit test.
func TestRegistryDeviceLoginCompletesAfterPending(t *testing.T) {
	var mu sync.Mutex
	var codeRequests, tokenRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/cli/device/code":
			var payload struct {
				Label string `json:"label"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decode device code request: %v", err)
			}
			if payload.Label != "GC CLI login" {
				t.Errorf("label = %q, want GC CLI login", payload.Label)
			}
			mu.Lock()
			codeRequests++
			mu.Unlock()
			_, _ = w.Write([]byte(`{
				"device_code": "dev-code-123",
				"user_code": "WXYZ-1234",
				"verification_uri": "https://registry.example/device",
				"verification_uri_complete": "https://registry.example/device?code=WXYZ-1234",
				"expires_in": 60,
				"interval": 1
			}`))
		case "/api/cli/device/token":
			var payload struct {
				DeviceCode string `json:"device_code"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decode device token request: %v", err)
			}
			if payload.DeviceCode != "dev-code-123" {
				t.Errorf("device_code = %q, want dev-code-123", payload.DeviceCode)
			}
			mu.Lock()
			tokenRequests++
			attempt := tokenRequests
			mu.Unlock()
			if attempt == 1 {
				// First poll: not yet authorized. The CLI must keep polling.
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"authorization_pending","interval":1}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"gcr_device_login","token_type":"bearer"}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()

	var stdout bytes.Buffer
	token, err := registryDeviceLogin(t.Context(), server.Client(), server.URL, "GC CLI login", &stdout)
	if err != nil {
		t.Fatalf("registryDeviceLogin: %v", err)
	}
	if token != "gcr_device_login" {
		t.Fatalf("token = %q, want gcr_device_login", token)
	}
	mu.Lock()
	gotCode, gotToken := codeRequests, tokenRequests
	mu.Unlock()
	if gotCode != 1 {
		t.Fatalf("device code requests = %d, want 1", gotCode)
	}
	if gotToken < 2 {
		t.Fatalf("device token polls = %d, want >= 2 (pending then success)", gotToken)
	}
	for _, want := range []string{"WXYZ-1234", "https://registry.example/device"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestRegistryBrowserLoginHandlerRejectsInvalidCallbacks(t *testing.T) {
	const state = "test-state-token"
	cases := []struct {
		name       string
		method     string
		body       string
		wantStatus int
	}{
		{"non-post method", http.MethodGet, "", http.StatusMethodNotAllowed},
		{"malformed json", http.MethodPost, "{not json", http.StatusBadRequest},
		{"state mismatch", http.MethodPost, `{"token":"gcr_tok","state":"wrong-state"}`, http.StatusForbidden},
		{"missing token", http.MethodPost, `{"token":"   ","state":"test-state-token"}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resultCh := make(chan browserLoginResult, 1)
			server := httptest.NewServer(registryBrowserLoginHandler(state, resultCh))
			defer server.Close()

			var body io.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			}
			req, err := http.NewRequestWithContext(t.Context(), tc.method, server.URL+"/token", body)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			select {
			case got := <-resultCh:
				t.Fatalf("invalid callback delivered a result: %+v", got)
			default:
			}
		})
	}
}

func TestRegistryBrowserLoginHandlerDeliversValidCallback(t *testing.T) {
	const state = "test-state-token"
	resultCh := make(chan browserLoginResult, 1)
	server := httptest.NewServer(registryBrowserLoginHandler(state, resultCh))
	defer server.Close()

	body := `{"token":"gcr_browser_token","registry":"https://registry.example","state":"test-state-token"}`
	resp, err := server.Client().Post(server.URL+"/token", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	select {
	case got := <-resultCh:
		if got.Token != "gcr_browser_token" || got.Registry != "https://registry.example" {
			t.Fatalf("delivered result = %+v", got)
		}
	default:
		t.Fatal("valid callback did not deliver a result")
	}
}

func TestRegistryBrowserLoginHandlerServesCallbackPage(t *testing.T) {
	server := httptest.NewServer(registryBrowserLoginHandler("state", make(chan browserLoginResult, 1)))
	defer server.Close()

	resp, err := server.Client().Get(server.URL + "/callback")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	page, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !strings.Contains(string(page), "Gas City CLI Login") {
		t.Fatalf("callback page = %q", string(page))
	}
}

func TestResolveRegistryBrowserLoginResult(t *testing.T) {
	const baseURL = "https://registry.example"

	t.Run("matching registry returns token", func(t *testing.T) {
		ch := make(chan browserLoginResult, 1)
		ch <- browserLoginResult{Token: "gcr_tok", Registry: baseURL}
		token, err := resolveRegistryBrowserLoginResult(t.Context(), baseURL, ch)
		if err != nil || token != "gcr_tok" {
			t.Fatalf("token=%q err=%v", token, err)
		}
	})

	t.Run("empty registry returns token", func(t *testing.T) {
		ch := make(chan browserLoginResult, 1)
		ch <- browserLoginResult{Token: "gcr_tok"}
		token, err := resolveRegistryBrowserLoginResult(t.Context(), baseURL, ch)
		if err != nil || token != "gcr_tok" {
			t.Fatalf("token=%q err=%v", token, err)
		}
	})

	t.Run("mismatched registry rejected", func(t *testing.T) {
		ch := make(chan browserLoginResult, 1)
		ch <- browserLoginResult{Token: "gcr_tok", Registry: "https://evil.example"}
		_, err := resolveRegistryBrowserLoginResult(t.Context(), baseURL, ch)
		if err == nil || !strings.Contains(err.Error(), "registry callback returned") {
			t.Fatalf("err = %v, want registry mismatch", err)
		}
	})

	t.Run("canceled context times out", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := resolveRegistryBrowserLoginResult(ctx, baseURL, make(chan browserLoginResult, 1))
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("err = %v, want timeout", err)
		}
	})
}

func TestRegistryBrowserLoginReturnsTokenEndToEnd(t *testing.T) {
	const baseURL = "https://registry.example"
	out := &registrySyncBuffer{}
	type loginResult struct {
		token string
		err   error
	}
	done := make(chan loginResult, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	go func() {
		token, err := registryBrowserLogin(ctx, baseURL, "GC CLI login", out, false)
		done <- loginResult{token: token, err: err}
	}()

	tokenURL, state := waitForRegistryCallbackTokenURL(t, out)
	body := `{"token":"gcr_e2e_token","registry":"` + baseURL + `","state":"` + state + `"}`
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(tokenURL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST token: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token POST status = %d, want 200", resp.StatusCode)
	}

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("registryBrowserLogin: %v", got.err)
		}
		if got.token != "gcr_e2e_token" {
			t.Fatalf("token = %q, want gcr_e2e_token", got.token)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("registryBrowserLogin did not return after callback")
	}
}

// registrySyncBuffer is an io.Writer safe for concurrent writes from the login
// goroutine and reads from the test goroutine.
type registrySyncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *registrySyncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *registrySyncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitForRegistryCallbackTokenURL polls the login output until the printed auth
// URL appears, then returns the local /token endpoint and CSRF state parsed
// from its redirect_uri and state query parameters.
func waitForRegistryCallbackTokenURL(t *testing.T, out *registrySyncBuffer) (tokenURL, state string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, line := range strings.Split(out.String(), "\n") {
			line = strings.TrimSpace(line)
			if !strings.Contains(line, "/cli/auth?") {
				continue
			}
			parsed, err := url.Parse(line)
			if err != nil {
				t.Fatalf("parse auth URL %q: %v", line, err)
			}
			q := parsed.Query()
			redirect := q.Get("redirect_uri")
			stateParam := q.Get("state")
			if redirect == "" || stateParam == "" {
				continue
			}
			return strings.TrimSuffix(redirect, "/callback") + "/token", stateParam
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("registry auth URL not printed; output:\n%s", out.String())
	return "", ""
}

type registryTestEnv struct {
	name  string
	value string
}

// clearRegistryEnv neutralizes ambient registry credentials so direct
// do-function calls resolve exactly what each test configures.
func clearRegistryEnv(t *testing.T, overrides ...registryTestEnv) {
	t.Helper()
	variables := []registryTestEnv{
		{name: "GC_REGISTRY_URL"},
		{name: "GC_REGISTRY_TOKEN"},
		{name: "GC_REGISTRY_SESSION"},
		{name: "GC_REGISTRY_CSRF_TOKEN"},
		{name: "ACTIONS_ID_TOKEN_REQUEST_TOKEN"},
		{name: "ACTIONS_ID_TOKEN_REQUEST_URL"},
		{name: registryCredentialProviderEnv, value: `["gasworks","credential-provider"]`},
		{name: registryCLIConfigEnv, value: filepath.Join(t.TempDir(), "registry.json")},
	}
	for _, override := range overrides {
		for i := range variables {
			if variables[i].name == override.name {
				variables[i].value = override.value
				break
			}
		}
	}
	for _, variable := range variables {
		t.Setenv(variable.name, variable.value)
	}
}

const registryPublishDemoManifest = `[pack]
name = "demo-pack"
version = "0.2.0"
schema = 2
description = "Demo pack for registry publishing."
`

func setupRegistryPublishRepo(t *testing.T) (repo string, packDir string) {
	t.Helper()
	return setupRegistryPublishRepoManifest(t, registryPublishDemoManifest)
}

// setupRegistryPublishRepoManifest builds a committed pack repo with the given
// pack.toml body and a pushed origin/main upstream pointing at a GitHub remote,
// so buildRegistryPublishRequest resolves a clean, pushed HEAD.
func setupRegistryPublishRepoManifest(t *testing.T, manifest string) (repo string, packDir string) {
	t.Helper()
	repo, packDir = writeRegistryPublishPackRepoManifest(t, manifest)
	root := filepath.Dir(repo)
	remote := filepath.Join(root, "remote.git")
	runRegistryPublishGit(t, root, "init", "--bare", remote)
	runRegistryPublishGit(t, repo, "remote", "add", "origin", remote)
	runRegistryPublishGit(t, repo, "push", "-u", "origin", "HEAD:main")
	runRegistryPublishGit(t, repo, "remote", "set-url", "origin", "git@github.com:gastownhall/demo-packs.git")
	return repo, packDir
}

// setupRegistryPublishRepoDetached builds a committed pack repo whose HEAD is
// detached, so the branch has no `@{u}` upstream. This mirrors an
// actions/checkout CI checkout, where publish must fall back to GitHub Actions
// runner metadata instead of an upstream tracking branch.
func setupRegistryPublishRepoDetached(t *testing.T) (packDir string, headSHA string) {
	t.Helper()
	var repo string
	repo, packDir = writeRegistryPublishPackRepo(t)
	headSHA = runRegistryPublishGit(t, repo, "rev-parse", "HEAD")
	runRegistryPublishGit(t, repo, "checkout", "--detach", "HEAD")
	return packDir, headSHA
}

// writeRegistryPublishPackRepo initializes a Git repo containing a single demo
// pack and commits it, returning the repo root and the pack directory. It does
// not configure a remote or upstream; callers add whatever publish topology
// they need.
func writeRegistryPublishPackRepo(t *testing.T) (repo string, packDir string) {
	t.Helper()
	return writeRegistryPublishPackRepoManifest(t, registryPublishDemoManifest)
}

// writeRegistryPublishPackRepoManifest initializes a Git repo containing a
// single demo pack whose pack.toml holds the given body, commits it, and
// returns the repo root and the pack directory.
func writeRegistryPublishPackRepoManifest(t *testing.T, manifest string) (repo string, packDir string) {
	t.Helper()
	root := t.TempDir()
	repo = filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, "packs", "demo"), 0o755); err != nil {
		t.Fatalf("MkdirAll(repo): %v", err)
	}
	runRegistryPublishGit(t, repo, "init", "-b", "main")
	runRegistryPublishGit(t, repo, "config", "user.email", "publisher@example.com")
	runRegistryPublishGit(t, repo, "config", "user.name", "Pack Publisher")
	packDir = filepath.Join(repo, "packs", "demo")
	if err := os.WriteFile(filepath.Join(packDir, "pack.toml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}
	runRegistryPublishGit(t, repo, "add", ".")
	runRegistryPublishGit(t, repo, "commit", "-m", "add demo pack")
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
