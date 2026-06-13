package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/spf13/cobra"
)

const (
	defaultRegistryPublishURL     = "https://registry.gascity.com"
	registryGitHubActionsAudience = "gascity-registry"
)

var registryPublishHTTPClient = &http.Client{Timeout: 30 * time.Second}

func newRegistryCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "registry",
		Short: "Publish packs to Gas City Registry",
		Long:  "Publish packs to the hosted Gas City Registry.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newRegistryLoginCmd(stdout, stderr))
	cmd.AddCommand(newRegistryPublishCmd(stdout, stderr))
	cmd.AddCommand(newRegistryWhoamiCmd(stdout, stderr))
	return cmd
}

type registryPublishOptions struct {
	RegistryURL   string
	Name          string
	Version       string
	Ref           string
	Description   string
	Token         string
	SessionCookie string
	CSRFToken     string
	DryRun        bool
	Validate      bool
	DevAuth       bool
	DevAuthHandle string
}

func newRegistryPublishCmd(stdout, stderr io.Writer) *cobra.Command {
	opts := registryPublishOptions{
		Validate:      true,
		DevAuthHandle: "local-cli",
	}
	cmd := &cobra.Command{
		Use:   "publish <path-to-pack-root>",
		Short: "Submit a pack publish request",
		Long: `Submit a pack publish request to Gas City Registry.

The command requires a clean Git checkout whose current HEAD matches its
configured upstream branch, then submits the GitHub repository, commit, pack
path, pack name, and version to the registry API.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if doRegistryPublish(cmd.Context(), args[0], opts, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.RegistryURL, "registry-url", "", "registry app base URL; defaults to GC_REGISTRY_URL, the stored login default, then "+defaultRegistryPublishURL)
	cmd.Flags().StringVar(&opts.Name, "name", "", "registry pack name; defaults to [pack].name")
	cmd.Flags().StringVar(&opts.Version, "version", "", "release version; defaults to [pack].version")
	cmd.Flags().StringVar(&opts.Ref, "ref", "", "release ref label; defaults to the upstream branch name")
	cmd.Flags().StringVar(&opts.Description, "description", "", "release description; defaults to [pack].description")
	cmd.Flags().StringVar(&opts.Token, "token", "", "registry API token; defaults to GC_REGISTRY_TOKEN")
	cmd.Flags().StringVar(&opts.SessionCookie, "session-cookie", "", "registry_session cookie value or Cookie header; defaults to GC_REGISTRY_SESSION")
	cmd.Flags().StringVar(&opts.CSRFToken, "csrf-token", "", "registry CSRF token; defaults to GC_REGISTRY_CSRF_TOKEN")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "print the publish request without submitting")
	cmd.Flags().BoolVar(&opts.Validate, "validate", opts.Validate, "ask the registry to validate the request immediately; a rejected validation exits non-zero")
	cmd.Flags().BoolVar(&opts.DevAuth, "dev-auth", false, "create a local dev-auth session before submitting; localhost only")
	cmd.Flags().StringVar(&opts.DevAuthHandle, "dev-auth-handle", opts.DevAuthHandle, "dev-auth handle when --dev-auth is used")
	return cmd
}

func doRegistryPublish(ctx context.Context, packRoot string, opts registryPublishOptions, stdout, stderr io.Writer) int {
	// Secrets resolve at execution time, never as flag defaults, so help
	// output cannot render credential values from the environment.
	opts.Token = registryFirstNonEmpty(opts.Token, os.Getenv("GC_REGISTRY_TOKEN"))
	opts.SessionCookie = registryFirstNonEmpty(opts.SessionCookie, os.Getenv("GC_REGISTRY_SESSION"))
	opts.CSRFToken = registryFirstNonEmpty(opts.CSRFToken, os.Getenv("GC_REGISTRY_CSRF_TOKEN"))

	baseURL, err := resolveRegistryPublishBaseURL(opts.RegistryURL)
	if err != nil {
		fmt.Fprintf(stderr, "gc registry publish: %v\n", err) //nolint:errcheck
		return 1
	}

	// Resolve the publish auth mode before building the request. The GitHub
	// Actions repo/ref fallback lets a detached or upstream-less CI checkout skip
	// the local pushed-HEAD proof, but that is only sound when this publish
	// actually authenticates through the GitHub Actions OIDC mint path: the mint
	// exchange is what proves the run really executed in GitHub Actions for this
	// repository and commit. Any pre-supplied credential (--token, env token,
	// stored login token, --session-cookie/--csrf-token, or --dev-auth) skips the
	// OIDC mint, so it must keep proving HEAD is pushed to its upstream and must
	// not trust spoofable GITHUB_*/ACTIONS_* environment variables.
	auth := registryPublishAuth{
		Token:         strings.TrimSpace(opts.Token),
		SessionCookie: strings.TrimSpace(opts.SessionCookie),
		CSRFToken:     strings.TrimSpace(opts.CSRFToken),
	}
	if !auth.hasCredentials() && !opts.DevAuth {
		configuredToken, err := readRegistryConfiguredToken(baseURL)
		if err != nil {
			fmt.Fprintf(stderr, "gc registry publish: %v\n", err) //nolint:errcheck
			return 1
		}
		auth.Token = strings.TrimSpace(configuredToken)
	}
	useGitHubActionsOIDC := !auth.hasCredentials() && !opts.DevAuth && registryGitHubActionsOIDCAvailable()

	request, err := buildRegistryPublishRequest(ctx, packRoot, opts, useGitHubActionsOIDC)
	if err != nil {
		fmt.Fprintf(stderr, "gc registry publish: %v\n", err) //nolint:errcheck
		return 1
	}

	if opts.DryRun {
		writeRegistryPublishDryRun(stdout, baseURL, request)
		return 0
	}

	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if opts.DevAuth {
		var err error
		auth, err = registryPublishDevAuth(ctx, registryPublishHTTPClient, baseURL, opts.DevAuthHandle)
		if err != nil {
			fmt.Fprintf(stderr, "gc registry publish: %v\n", err) //nolint:errcheck
			return 1
		}
	}
	if useGitHubActionsOIDC {
		oidcToken, err := registryRequestGitHubActionsOIDCToken(ctx, registryPublishHTTPClient, registryGitHubActionsAudience)
		if err != nil {
			fmt.Fprintf(stderr, "gc registry publish: %v\n", err) //nolint:errcheck
			return 1
		}
		publishToken, err := registryMintGitHubActionsPublishToken(ctx, registryPublishHTTPClient, baseURL, request, oidcToken)
		if err != nil {
			fmt.Fprintf(stderr, "gc registry publish: %v\n", err) //nolint:errcheck
			return 1
		}
		auth.Token = publishToken
	}
	if !auth.hasCredentials() {
		fmt.Fprintln(stderr, "gc registry publish: authentication required; run `gc registry login`, set GC_REGISTRY_TOKEN, pass --token, set GC_REGISTRY_SESSION and GC_REGISTRY_CSRF_TOKEN, or use --dev-auth against a local registry") //nolint:errcheck
		return 1
	}

	submitted, err := submitRegistryPublishRequest(ctx, registryPublishHTTPClient, baseURL, request, auth, opts.Validate)
	if err != nil {
		fmt.Fprintf(stderr, "gc registry publish: %v\n", err) //nolint:errcheck
		return 1
	}
	writeRegistryPublishSubmitted(stdout, baseURL, submitted)
	if opts.Validate {
		if failure := registryPublishValidationFailure(submitted); failure != "" {
			fmt.Fprintf(stderr, "gc registry publish: validation failed: %s\n", failure) //nolint:errcheck
			return 1
		}
	}
	return 0
}

type registryPublishRequest struct {
	RepoURL              string `json:"repoUrl"`
	Commit               string `json:"commit"`
	PackPath             string `json:"packPath"`
	RequestedName        string `json:"requestedName"`
	RequestedVersion     string `json:"requestedVersion"`
	RequestedRef         string `json:"requestedRef,omitempty"`
	RequestedDescription string `json:"requestedDescription,omitempty"`
}

type registryPublishSubmitted struct {
	ID               string
	Status           string
	RequestedName    string
	RequestedVersion string
	Repository       string
	StatusReason     string
	ValidationError  string
	Hash             string
}

type registryPackManifest struct {
	Pack struct {
		Name        string `toml:"name"`
		Version     string `toml:"version"`
		Description string `toml:"description"`
	} `toml:"pack"`
}

func buildRegistryPublishRequest(ctx context.Context, packRoot string, opts registryPublishOptions, allowGitHubActionsFallback bool) (registryPublishRequest, error) {
	absPackRoot, err := filepath.Abs(packRoot)
	if err != nil {
		return registryPublishRequest{}, fmt.Errorf("resolving pack root: %w", err)
	}
	if resolved, evalErr := filepath.EvalSymlinks(absPackRoot); evalErr == nil {
		absPackRoot = resolved
	}
	manifest, err := readRegistryPackManifest(absPackRoot)
	if err != nil {
		return registryPublishRequest{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	repoRoot, err := gitOutput(ctx, absPackRoot, "rev-parse", "--show-toplevel")
	if err != nil {
		return registryPublishRequest{}, fmt.Errorf("pack root must be inside a Git repository: %w", err)
	}
	if resolved, evalErr := filepath.EvalSymlinks(repoRoot); evalErr == nil {
		repoRoot = resolved
	}
	status, err := gitOutput(ctx, repoRoot, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return registryPublishRequest{}, fmt.Errorf("checking Git status: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		return registryPublishRequest{}, errors.New("working tree has uncommitted or untracked changes; commit, stash, or remove them before publishing")
	}
	commit, err := gitOutput(ctx, repoRoot, "rev-parse", "HEAD")
	if err != nil {
		return registryPublishRequest{}, fmt.Errorf("resolving HEAD: %w", err)
	}
	if !fullGitSHARE.MatchString(commit) {
		return registryPublishRequest{}, fmt.Errorf("HEAD resolved to %q, not a full lowercase Git SHA", commit)
	}
	repoRef, err := resolveRegistryPublishRepoRef(ctx, repoRoot, commit, opts, allowGitHubActionsFallback)
	if err != nil {
		return registryPublishRequest{}, err
	}
	packPath, err := registryPublishPackPath(repoRoot, absPackRoot)
	if err != nil {
		return registryPublishRequest{}, err
	}

	version := strings.TrimSpace(registryFirstNonEmpty(opts.Version, manifest.Pack.Version))
	if version == "" {
		return registryPublishRequest{}, errors.New("release version is required; set [pack].version or pass --version")
	}
	name := strings.TrimSpace(registryFirstNonEmpty(opts.Name, manifest.Pack.Name))
	if name == "" {
		return registryPublishRequest{}, errors.New("pack name is required; set [pack].name or pass --name")
	}
	ref := repoRef.Ref
	description := strings.TrimSpace(registryFirstNonEmpty(opts.Description, manifest.Pack.Description))
	return registryPublishRequest{
		RepoURL:              repoRef.RepoURL,
		Commit:               commit,
		PackPath:             packPath,
		RequestedName:        name,
		RequestedVersion:     version,
		RequestedRef:         ref,
		RequestedDescription: description,
	}, nil
}

// registryPublishRepoRef carries the GitHub repository URL and release ref
// label for a publish request. They come from the local upstream tracking
// branch, or from GitHub Actions runner metadata when a detached or
// upstream-less CI checkout has no `@{u}` but a trusted OIDC environment.
type registryPublishRepoRef struct {
	RepoURL string
	Ref     string
}

// resolveRegistryPublishRepoRef resolves the GitHub repository URL and release
// ref for a publish request. It prefers the local upstream tracking branch and
// verifies HEAD is pushed there. When the branch has no upstream, it falls back
// to GitHub Actions runner metadata only if allowGitHubActionsFallback is set,
// which the caller enables exclusively for the GitHub Actions OIDC mint path.
// A publish carrying any other (non-OIDC) credential keeps requiring a pushed
// upstream, so spoofable GITHUB_*/ACTIONS_* environment variables cannot skip
// the pushed-HEAD proof.
func resolveRegistryPublishRepoRef(ctx context.Context, repoRoot, commit string, opts registryPublishOptions, allowGitHubActionsFallback bool) (registryPublishRepoRef, error) {
	upstream, err := gitOutput(ctx, repoRoot, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	if err != nil {
		if allowGitHubActionsFallback {
			if ghRef, ok := registryGitHubActionsRepoRef(commit, opts); ok {
				return ghRef, nil
			}
		}
		return registryPublishRepoRef{}, errors.New("current branch has no upstream; run `git push -u` before publishing")
	}
	upstreamCommit, err := gitOutput(ctx, repoRoot, "rev-parse", "@{u}")
	if err != nil {
		return registryPublishRepoRef{}, fmt.Errorf("resolving upstream %s: %w", upstream, err)
	}
	if upstreamCommit != commit {
		return registryPublishRepoRef{}, fmt.Errorf("HEAD %s is not pushed to upstream %s (%s)", shortCommit(commit), upstream, shortCommit(upstreamCommit))
	}
	remoteName, upstreamBranch, err := splitGitUpstream(upstream)
	if err != nil {
		return registryPublishRepoRef{}, err
	}
	remoteURL, err := gitOutput(ctx, repoRoot, "remote", "get-url", remoteName)
	if err != nil {
		return registryPublishRepoRef{}, fmt.Errorf("reading remote %q URL: %w", remoteName, err)
	}
	repoURL, err := normalizeGitHubRemoteURL(remoteURL)
	if err != nil {
		return registryPublishRepoRef{}, err
	}
	return registryPublishRepoRef{
		RepoURL: repoURL,
		Ref:     strings.TrimSpace(registryFirstNonEmpty(opts.Ref, upstreamBranch)),
	}, nil
}

// registryGitHubActionsRepoRef derives the publish repository URL and ref from
// GitHub Actions runner metadata. It returns ok=false unless the OIDC request
// environment is present, the runner identifies the repository, and the
// runner's commit SHA matches the local HEAD, so stale or spoofed metadata
// cannot redirect a publish to the wrong repository or commit. Environment
// presence alone is not a trust signal: callers must restrict this fallback to
// the GitHub Actions OIDC mint path, where the minted token is exchanged with
// the registry and proves the run actually executed in GitHub Actions. Callers
// fall back to the upstream requirement when ok is false.
func registryGitHubActionsRepoRef(commit string, opts registryPublishOptions) (registryPublishRepoRef, bool) {
	if !registryGitHubActionsOIDCAvailable() {
		return registryPublishRepoRef{}, false
	}
	repoSlug := strings.TrimSpace(os.Getenv("GITHUB_REPOSITORY"))
	if repoSlug == "" {
		return registryPublishRepoRef{}, false
	}
	// The runner's commit must be present and match the checked-out HEAD so
	// publish records the exact tree the workflow built, never an unrelated or
	// unverified commit. An absent GITHUB_SHA falls back to the upstream
	// requirement rather than trusting unconfirmed runner metadata.
	sha := strings.TrimSpace(os.Getenv("GITHUB_SHA"))
	if sha == "" || !strings.EqualFold(sha, commit) {
		return registryPublishRepoRef{}, false
	}
	serverURL := strings.TrimSpace(os.Getenv("GITHUB_SERVER_URL"))
	if serverURL == "" {
		serverURL = "https://github.com"
	}
	repoURL, err := normalizeGitHubRemoteURL(strings.TrimRight(serverURL, "/") + "/" + repoSlug)
	if err != nil {
		return registryPublishRepoRef{}, false
	}
	return registryPublishRepoRef{
		RepoURL: repoURL,
		Ref:     strings.TrimSpace(registryFirstNonEmpty(opts.Ref, os.Getenv("GITHUB_REF_NAME"))),
	}, true
}

// readRegistryPackManifest loads pack.toml from packRoot. It does not require
// [pack].name: buildRegistryPublishRequest applies the --name fallback first
// and reports a missing name only when neither the manifest nor the flag
// supplies one, so the advertised --name override actually takes effect.
func readRegistryPackManifest(packRoot string) (registryPackManifest, error) {
	packToml := filepath.Join(packRoot, "pack.toml")
	data, err := os.ReadFile(packToml)
	if err != nil {
		return registryPackManifest{}, fmt.Errorf("reading %s: %w", packToml, err)
	}
	var manifest registryPackManifest
	if err := toml.Unmarshal(data, &manifest); err != nil {
		return registryPackManifest{}, fmt.Errorf("parsing %s: %w", packToml, err)
	}
	manifest.Pack.Name = strings.TrimSpace(manifest.Pack.Name)
	return manifest, nil
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// Strip git-locating env vars (GIT_DIR, GIT_WORK_TREE, GIT_INDEX_FILE, ...)
	// so publish introspection resolves dir, not a parent repo leaked through a
	// pre-commit hook or nested worktree tooling.
	cmd.Env = git.SanitizedEnv()
	out, err := cmd.Output()
	if err != nil {
		if exitErr := new(exec.ExitError); errors.As(err, &exitErr) {
			msg := strings.TrimSpace(string(exitErr.Stderr))
			if msg != "" {
				return "", errors.New(msg)
			}
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

var fullGitSHARE = regexp.MustCompile(`^[0-9a-f]{40}$`)

func splitGitUpstream(upstream string) (remote, branch string, err error) {
	parts := strings.SplitN(strings.TrimSpace(upstream), "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("unsupported upstream %q", upstream)
	}
	return parts[0], parts[1], nil
}

func normalizeGitHubRemoteURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimSuffix(raw, ".git")
	switch {
	case strings.HasPrefix(raw, "git@github.com:"):
		path := strings.TrimPrefix(raw, "git@github.com:")
		return normalizeGitHubOwnerRepo(path)
	case strings.HasPrefix(raw, "ssh://"):
		u, err := url.Parse(raw)
		if err != nil {
			return "", fmt.Errorf("parsing Git remote URL: %w", err)
		}
		if strings.EqualFold(u.Hostname(), "github.com") {
			return normalizeGitHubOwnerRepo(strings.TrimPrefix(u.Path, "/"))
		}
	case strings.HasPrefix(raw, "https://") || strings.HasPrefix(raw, "http://"):
		u, err := url.Parse(raw)
		if err != nil {
			return "", fmt.Errorf("parsing Git remote URL: %w", err)
		}
		if strings.EqualFold(u.Hostname(), "github.com") {
			return normalizeGitHubOwnerRepo(strings.TrimPrefix(u.Path, "/"))
		}
	}
	return "", fmt.Errorf("publish requires a GitHub remote, got %q", raw)
}

var githubOwnerRepoRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func normalizeGitHubOwnerRepo(path string) (string, error) {
	path = strings.Trim(strings.TrimSuffix(path, ".git"), "/")
	if !githubOwnerRepoRE.MatchString(path) {
		return "", fmt.Errorf("invalid GitHub owner/repo path %q", path)
	}
	return "https://github.com/" + path, nil
}

func registryPublishPackPath(repoRoot, packRoot string) (string, error) {
	rel, err := filepath.Rel(repoRoot, packRoot)
	if err != nil {
		return "", fmt.Errorf("resolving pack path relative to repository: %w", err)
	}
	if rel == "." {
		return ".", nil
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", errors.New("pack root is not inside the Git repository")
	}
	return filepath.ToSlash(rel), nil
}

// resolveRegistryPublishBaseURL resolves the registry base URL from the
// explicit flag value, the GC_REGISTRY_URL environment variable, the stored
// login default, then defaultRegistryPublishURL, and normalizes the winner.
func resolveRegistryPublishBaseURL(explicit string) (string, error) {
	raw := registryFirstNonEmpty(explicit, os.Getenv("GC_REGISTRY_URL"))
	if raw == "" {
		cfg, err := loadRegistryCLIConfig(registryCLIConfigPath())
		if err != nil {
			return "", err
		}
		raw = cfg.DefaultRegistryURL
	}
	return normalizeRegistryPublishBaseURL(raw)
}

func normalizeRegistryPublishBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = defaultRegistryPublishURL
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid registry URL %q", raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		// publish, login, and whoami carry bearer tokens, session cookies, and
		// CSRF tokens. Cleartext http only stays off the wire for a loopback
		// host, so reject http for any non-local registry.
		if !isLocalRegistryHost(u.Hostname()) {
			return "", fmt.Errorf("registry URL must use https for non-local hosts: %q", raw)
		}
	default:
		return "", fmt.Errorf("registry URL must use http or https: %q", raw)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

type registryPublishAuth struct {
	Token         string
	SessionCookie string
	CSRFToken     string
}

func (a registryPublishAuth) hasCredentials() bool {
	return strings.TrimSpace(a.Token) != "" ||
		(strings.TrimSpace(a.SessionCookie) != "" && strings.TrimSpace(a.CSRFToken) != "")
}

func registryPublishDevAuth(ctx context.Context, client *http.Client, baseURL, handle string) (registryPublishAuth, error) {
	if !isLocalRegistryURL(baseURL) {
		return registryPublishAuth{}, errors.New("--dev-auth is only allowed for localhost registry URLs")
	}
	handle = strings.TrimSpace(handle)
	if handle == "" {
		handle = "local-cli"
	}
	loginURL := baseURL + "/api/dev/sign-in?handle=" + url.QueryEscape(handle) + "&redirect=/api/me"
	devClient := *client
	devClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, loginURL, nil)
	if err != nil {
		return registryPublishAuth{}, err
	}
	resp, err := devClient.Do(req)
	if err != nil {
		return registryPublishAuth{}, fmt.Errorf("creating dev auth session: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var sessionCookie string
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "registry_session" {
			sessionCookie = cookie.Value
			break
		}
	}
	if sessionCookie == "" {
		return registryPublishAuth{}, fmt.Errorf("creating dev auth session: registry returned HTTP %d without registry_session cookie", resp.StatusCode)
	}
	csrf, err := registryPublishFetchCSRF(ctx, client, baseURL, sessionCookie)
	if err != nil {
		return registryPublishAuth{}, err
	}
	return registryPublishAuth{SessionCookie: sessionCookie, CSRFToken: csrf}, nil
}

func registryPublishFetchCSRF(ctx context.Context, client *http.Client, baseURL, sessionCookie string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/me", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Cookie", registryPublishCookieHeader(sessionCookie))
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching registry session: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var payload struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := registryDecodeJSONResponse(resp, &payload); err != nil {
		return "", fmt.Errorf("fetching registry session: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetching registry session: HTTP %d", resp.StatusCode)
	}
	if strings.TrimSpace(payload.CSRFToken) == "" {
		return "", errors.New("registry session did not include a CSRF token")
	}
	return payload.CSRFToken, nil
}

func isLocalRegistryURL(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	return isLocalRegistryHost(u.Hostname())
}

// isLocalRegistryHost reports whether host is a loopback host that may exchange
// registry credentials over cleartext http.
func isLocalRegistryHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

func submitRegistryPublishRequest(ctx context.Context, client *http.Client, baseURL string, payload registryPublishRequest, auth registryPublishAuth, validate bool) (registryPublishSubmitted, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return registryPublishSubmitted{}, err
	}
	endpoint := baseURL + "/api/publish-requests"
	if validate {
		endpoint += "?validate=1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return registryPublishSubmitted{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if strings.TrimSpace(auth.Token) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(auth.Token))
	} else {
		req.Header.Set("X-CSRF-Token", auth.CSRFToken)
		req.Header.Set("Cookie", registryPublishCookieHeader(auth.SessionCookie))
	}
	resp, err := client.Do(req)
	if err != nil {
		return registryPublishSubmitted{}, fmt.Errorf("submitting publish request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var raw registryPublishAPIResponse
	if err := registryDecodeJSONResponse(resp, &raw); err != nil {
		return registryPublishSubmitted{}, fmt.Errorf("submitting publish request: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if raw.Error.Message != "" {
			return registryPublishSubmitted{}, fmt.Errorf("registry rejected publish request (%s): %s", raw.Error.Code, raw.Error.Message)
		}
		return registryPublishSubmitted{}, fmt.Errorf("registry rejected publish request: HTTP %d", resp.StatusCode)
	}
	request := raw.PublishRequest
	if request.ID == "" && raw.Direct.ID != "" {
		request = raw.Direct
	}
	if request.ID == "" {
		return registryPublishSubmitted{}, errors.New("registry response did not include a publish request")
	}
	return registryPublishSubmitted{
		ID:               request.ID,
		Status:           request.Status,
		RequestedName:    registryFirstNonEmpty(request.RequestedName, payload.RequestedName),
		RequestedVersion: registryFirstNonEmpty(request.RequestedVersion, payload.RequestedVersion),
		Repository:       request.Repository.FullName,
		StatusReason:     request.StatusReason,
		ValidationError:  request.ValidationError,
		Hash:             request.RegistryEntry.Release.Hash,
	}, nil
}

type registryPublishAPIResponse struct {
	PublishRequest registryPublishAPIRequest `json:"publishRequest"`
	Direct         registryPublishAPIRequest `json:"-"`
	Error          struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type registryPublishAPIRequest struct {
	ID               string `json:"id"`
	Status           string `json:"status"`
	RequestedName    string `json:"requestedName"`
	RequestedVersion string `json:"requestedVersion"`
	StatusReason     string `json:"statusReason"`
	ValidationError  string `json:"validationError"`
	Repository       struct {
		FullName string `json:"fullName"`
	} `json:"repository"`
	RegistryEntry struct {
		Release struct {
			Hash string `json:"hash"`
		} `json:"release"`
	} `json:"registryEntry"`
}

func (r *registryPublishAPIResponse) UnmarshalJSON(data []byte) error {
	type alias registryPublishAPIResponse
	var wrapped alias
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return err
	}
	*r = registryPublishAPIResponse(wrapped)
	var direct registryPublishAPIRequest
	if err := json.Unmarshal(data, &direct); err == nil && direct.ID != "" {
		r.Direct = direct
	}
	return nil
}

// registryPublishCookieHeader renders the Cookie header for a --session-cookie
// input. Only explicitly header-shaped values pass through verbatim; bare
// session values are wrapped unescaped so legal cookie characters such as
// base64 `=` padding survive the round trip back to the registry.
func registryPublishCookieHeader(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "registry_session=") || strings.Contains(value, ";") {
		return value
	}
	return "registry_session=" + value
}

// registryDecodeJSONResponse decodes a bounded registry response body into
// out. Registries sit behind proxies and CDNs that answer with HTML or empty
// bodies, so a non-2xx response that fails to decode reports the HTTP status
// instead of a JSON syntax error.
func registryDecodeJSONResponse(resp *http.Response, out any) error {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("reading registry response: %w", err)
	}
	decodeErr := json.Unmarshal(body, out)
	if decodeErr == nil {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if snippet := registryResponseSnippet(body); snippet != "" {
			return fmt.Errorf("registry returned HTTP %d: %s", resp.StatusCode, snippet)
		}
		return fmt.Errorf("registry returned HTTP %d", resp.StatusCode)
	}
	return fmt.Errorf("decoding registry response (HTTP %d): %w", resp.StatusCode, decodeErr)
}

// registryResponseSnippet condenses a non-JSON response body for error text.
func registryResponseSnippet(body []byte) string {
	s := strings.Join(strings.Fields(string(body)), " ")
	const maxRunes = 200
	if runes := []rune(s); len(runes) > maxRunes {
		s = string(runes[:maxRunes]) + "..."
	}
	return s
}

func writeRegistryPublishDryRun(stdout io.Writer, baseURL string, request registryPublishRequest) {
	fmt.Fprintf(stdout, "Registry: %s\n", baseURL)                                        //nolint:errcheck
	fmt.Fprintf(stdout, "Repository: %s\n", request.RepoURL)                              //nolint:errcheck
	fmt.Fprintf(stdout, "Commit: %s\n", request.Commit)                                   //nolint:errcheck
	fmt.Fprintf(stdout, "Pack path: %s\n", request.PackPath)                              //nolint:errcheck
	fmt.Fprintf(stdout, "Pack: %s %s\n", request.RequestedName, request.RequestedVersion) //nolint:errcheck
	if request.RequestedRef != "" {
		fmt.Fprintf(stdout, "Ref: %s\n", request.RequestedRef) //nolint:errcheck
	}
	if request.RequestedDescription != "" {
		fmt.Fprintf(stdout, "Description: %s\n", request.RequestedDescription) //nolint:errcheck
	}
	fmt.Fprintln(stdout, "Dry run: publish request was not submitted.") //nolint:errcheck
}

func writeRegistryPublishSubmitted(stdout io.Writer, baseURL string, result registryPublishSubmitted) {
	fmt.Fprintf(stdout, "Submitted publish request %s to %s\n", result.ID, baseURL)     //nolint:errcheck
	fmt.Fprintf(stdout, "Pack: %s %s\n", result.RequestedName, result.RequestedVersion) //nolint:errcheck
	if result.Repository != "" {
		fmt.Fprintf(stdout, "Repository: %s\n", result.Repository) //nolint:errcheck
	}
	fmt.Fprintf(stdout, "Status: %s\n", result.Status) //nolint:errcheck
	if result.Hash != "" {
		fmt.Fprintf(stdout, "Hash: %s\n", result.Hash) //nolint:errcheck
	}
	if result.StatusReason != "" {
		fmt.Fprintf(stdout, "Message: %s\n", result.StatusReason) //nolint:errcheck
	} else if result.ValidationError != "" {
		fmt.Fprintf(stdout, "Message: %s\n", result.ValidationError) //nolint:errcheck
	}
}

// registryPublishValidationRejectedStatuses lists publish-request statuses that
// represent a terminal validation rejection. A `gc registry publish --validate`
// run that lands in one of these states failed validation; statuses outside
// this set (for example queued or pending-review states) are not treated as
// failures, so a successfully queued request still exits zero.
var registryPublishValidationRejectedStatuses = map[string]bool{
	"rejected": true,
	"invalid":  true,
	"failed":   true,
	"error":    true,
	"denied":   true,
}

// registryPublishValidationFailure reports a human-readable reason when a
// validated publish request did not pass registry validation, or "" when it
// did. A populated ValidationError is always a failure; otherwise a terminal
// rejected/invalid status is treated as a failure so `gc registry publish
// --validate` exits non-zero instead of masking a pack the registry rejected
// inside a 2xx response as a successful publish.
func registryPublishValidationFailure(result registryPublishSubmitted) string {
	if msg := strings.TrimSpace(result.ValidationError); msg != "" {
		return msg
	}
	if registryPublishValidationRejectedStatuses[strings.ToLower(strings.TrimSpace(result.Status))] {
		if reason := strings.TrimSpace(result.StatusReason); reason != "" {
			return fmt.Sprintf("status %q: %s", result.Status, reason)
		}
		return fmt.Sprintf("status %q", result.Status)
	}
	return ""
}

func registryFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
