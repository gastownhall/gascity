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
	"github.com/spf13/cobra"
)

const defaultRegistryPublishURL = "https://registry.gascity.com"

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
	cmd.AddCommand(newRegistryPublishCmd(stdout, stderr))
	return cmd
}

type registryPublishOptions struct {
	RegistryURL   string
	Version       string
	Ref           string
	Description   string
	SessionCookie string
	CSRFToken     string
	DryRun        bool
	Validate      bool
	DevAuth       bool
	DevAuthHandle string
}

func newRegistryPublishCmd(stdout, stderr io.Writer) *cobra.Command {
	opts := registryPublishOptions{
		RegistryURL:   registryFirstNonEmpty(os.Getenv("GC_REGISTRY_URL"), defaultRegistryPublishURL),
		SessionCookie: os.Getenv("GC_REGISTRY_SESSION"),
		CSRFToken:     os.Getenv("GC_REGISTRY_CSRF_TOKEN"),
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
		RunE: func(_ *cobra.Command, args []string) error {
			if doRegistryPublish(args[0], opts, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.RegistryURL, "registry-url", opts.RegistryURL, "registry app base URL")
	cmd.Flags().StringVar(&opts.Version, "version", "", "release version; defaults to [pack].version")
	cmd.Flags().StringVar(&opts.Ref, "ref", "", "release ref label; defaults to the upstream branch name")
	cmd.Flags().StringVar(&opts.Description, "description", "", "release description; defaults to [pack].description")
	cmd.Flags().StringVar(&opts.SessionCookie, "session-cookie", opts.SessionCookie, "registry_session cookie value or Cookie header")
	cmd.Flags().StringVar(&opts.CSRFToken, "csrf-token", opts.CSRFToken, "registry CSRF token")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "print the publish request without submitting")
	cmd.Flags().BoolVar(&opts.Validate, "validate", opts.Validate, "ask the registry to validate the request immediately")
	cmd.Flags().BoolVar(&opts.DevAuth, "dev-auth", false, "create a local dev-auth session before submitting; localhost only")
	cmd.Flags().StringVar(&opts.DevAuthHandle, "dev-auth-handle", opts.DevAuthHandle, "dev-auth handle when --dev-auth is used")
	return cmd
}

func doRegistryPublish(packRoot string, opts registryPublishOptions, stdout, stderr io.Writer) int {
	request, err := buildRegistryPublishRequest(packRoot, opts)
	if err != nil {
		fmt.Fprintf(stderr, "gc registry publish: %v\n", err) //nolint:errcheck
		return 1
	}

	baseURL, err := normalizeRegistryPublishBaseURL(opts.RegistryURL)
	if err != nil {
		fmt.Fprintf(stderr, "gc registry publish: %v\n", err) //nolint:errcheck
		return 1
	}

	if opts.DryRun {
		writeRegistryPublishDryRun(stdout, baseURL, request)
		return 0
	}

	auth := registryPublishAuth{
		SessionCookie: strings.TrimSpace(opts.SessionCookie),
		CSRFToken:     strings.TrimSpace(opts.CSRFToken),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if opts.DevAuth {
		var err error
		auth, err = registryPublishDevAuth(ctx, registryPublishHTTPClient, baseURL, opts.DevAuthHandle)
		if err != nil {
			fmt.Fprintf(stderr, "gc registry publish: %v\n", err) //nolint:errcheck
			return 1
		}
	}
	if auth.SessionCookie == "" || auth.CSRFToken == "" {
		fmt.Fprintln(stderr, "gc registry publish: authentication required; set GC_REGISTRY_SESSION and GC_REGISTRY_CSRF_TOKEN, pass --session-cookie/--csrf-token, or use --dev-auth against a local registry") //nolint:errcheck
		return 1
	}

	submitted, err := submitRegistryPublishRequest(ctx, registryPublishHTTPClient, baseURL, request, auth, opts.Validate)
	if err != nil {
		fmt.Fprintf(stderr, "gc registry publish: %v\n", err) //nolint:errcheck
		return 1
	}
	writeRegistryPublishSubmitted(stdout, baseURL, submitted)
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

func buildRegistryPublishRequest(packRoot string, opts registryPublishOptions) (registryPublishRequest, error) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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
	upstream, err := gitOutput(ctx, repoRoot, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	if err != nil {
		return registryPublishRequest{}, errors.New("current branch has no upstream; run `git push -u` before publishing")
	}
	upstreamCommit, err := gitOutput(ctx, repoRoot, "rev-parse", "@{u}")
	if err != nil {
		return registryPublishRequest{}, fmt.Errorf("resolving upstream %s: %w", upstream, err)
	}
	if upstreamCommit != commit {
		return registryPublishRequest{}, fmt.Errorf("HEAD %s is not pushed to upstream %s (%s)", shortCommit(commit), upstream, shortCommit(upstreamCommit))
	}
	remoteName, upstreamBranch, err := splitGitUpstream(upstream)
	if err != nil {
		return registryPublishRequest{}, err
	}
	remoteURL, err := gitOutput(ctx, repoRoot, "remote", "get-url", remoteName)
	if err != nil {
		return registryPublishRequest{}, fmt.Errorf("reading remote %q URL: %w", remoteName, err)
	}
	repoURL, err := normalizeGitHubRemoteURL(remoteURL)
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
	ref := strings.TrimSpace(registryFirstNonEmpty(opts.Ref, upstreamBranch))
	description := strings.TrimSpace(registryFirstNonEmpty(opts.Description, manifest.Pack.Description))
	return registryPublishRequest{
		RepoURL:              repoURL,
		Commit:               commit,
		PackPath:             packPath,
		RequestedName:        strings.TrimSpace(manifest.Pack.Name),
		RequestedVersion:     version,
		RequestedRef:         ref,
		RequestedDescription: description,
	}, nil
}

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
	if manifest.Pack.Name == "" {
		return registryPackManifest{}, fmt.Errorf("%s is missing [pack].name", packToml)
	}
	return manifest, nil
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
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
	if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", errors.New("pack root is not inside the Git repository")
	}
	return filepath.ToSlash(rel), nil
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
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("registry URL must use http or https: %q", raw)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

type registryPublishAuth struct {
	SessionCookie string
	CSRFToken     string
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
	defer resp.Body.Close()
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
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetching registry session: HTTP %d", resp.StatusCode)
	}
	var payload struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("decoding registry session: %w", err)
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
	host := strings.ToLower(u.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
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
	req.Header.Set("X-CSRF-Token", auth.CSRFToken)
	req.Header.Set("Cookie", registryPublishCookieHeader(auth.SessionCookie))
	resp, err := client.Do(req)
	if err != nil {
		return registryPublishSubmitted{}, fmt.Errorf("submitting publish request: %w", err)
	}
	defer resp.Body.Close()
	var raw registryPublishAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return registryPublishSubmitted{}, fmt.Errorf("decoding registry response: %w", err)
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

func registryPublishCookieHeader(value string) string {
	value = strings.TrimSpace(value)
	if strings.Contains(value, "=") {
		return value
	}
	return "registry_session=" + url.QueryEscape(value)
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

func registryFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
