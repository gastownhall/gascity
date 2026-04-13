package api

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestReadinessRegistrySync(t *testing.T) {
	for item := range readinessProbeSpecs {
		if _, ok := supportedReadiness[item]; !ok {
			t.Fatalf("readiness probe spec %q missing from supportedReadiness", item)
		}
	}

	for item := range supportedReadiness {
		if _, ok := readinessProbeSpecs[item]; !ok {
			t.Fatalf("supported readiness item %q missing probe spec", item)
		}
	}

	for _, item := range defaultReadinessItems {
		if _, ok := supportedReadiness[item]; !ok {
			t.Fatalf("default readiness item %q missing from supportedReadiness", item)
		}
	}

	for _, item := range defaultProviderReadinessItems {
		if _, ok := supportedProviderReadiness[item]; !ok {
			t.Fatalf("default provider readiness item %q missing from supportedProviderReadiness", item)
		}
		if spec, ok := readinessProbeSpecs[item]; !ok {
			t.Fatalf("default provider readiness item %q missing probe spec", item)
		} else if spec.kind != probeKindProvider {
			t.Fatalf("default provider readiness item %q kind = %q, want %q", item, spec.kind, probeKindProvider)
		}
	}

	wantProviders := slices.Clone(defaultProviderReadinessItems)
	slices.Sort(wantProviders)
	if got := slices.Sorted(maps.Keys(supportedProviderReadiness)); !slices.Equal(got, wantProviders) {
		t.Fatalf("supportedProviderReadiness keys = %v, want %v", got, wantProviders)
	}
}

func TestProbeCommandEnvPreservesXDGOverridesWhenGHConfigDirIsSet(t *testing.T) {
	homeDir := t.TempDir()
	xdgConfigHome := filepath.Join(homeDir, "xdg-config")
	xdgStateHome := filepath.Join(homeDir, "xdg-state")
	ghConfigDir := filepath.Join(homeDir, "gh")
	t.Setenv("XDG_CONFIG_HOME", xdgConfigHome)
	t.Setenv("XDG_STATE_HOME", xdgStateHome)
	t.Setenv("GH_CONFIG_DIR", ghConfigDir)

	env := probeCommandEnv(homeDir)
	if !slices.Contains(env, "XDG_CONFIG_HOME="+xdgConfigHome) {
		t.Fatalf("probeCommandEnv missing XDG_CONFIG_HOME override: %v", env)
	}
	if !slices.Contains(env, "XDG_STATE_HOME="+xdgStateHome) {
		t.Fatalf("probeCommandEnv missing XDG_STATE_HOME override: %v", env)
	}
	if !slices.Contains(env, "GH_CONFIG_DIR="+ghConfigDir) {
		t.Fatalf("probeCommandEnv missing GH_CONFIG_DIR override: %v", env)
	}
}

func TestProbeCommandEnvExcludesClaudeOAuthToken(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "token-value")

	env := probeCommandEnv(homeDir)
	// CLAUDE_CODE_OAUTH_TOKEN must NOT be in the shared env — it is
	// appended only in probeClaude to avoid leaking credentials to
	// unrelated probe subprocesses (e.g., gh auth status).
	if slices.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=token-value") {
		t.Fatalf("probeCommandEnv should not contain CLAUDE_CODE_OAUTH_TOKEN: %v", env)
	}
}

func TestProviderProbeSearchDirsIncludesUserLocalAndLinuxDefaults(t *testing.T) {
	homeDir := t.TempDir()
	got := providerProbeSearchDirs(homeDir, "linux", "/usr/local/bin:/usr/bin:/bin")
	want := []string{
		filepath.Join(homeDir, ".local", "bin"),
		filepath.Join(homeDir, "bin"),
		"/usr/local/bin",
		"/usr/bin",
		"/bin",
		"/snap/bin",
		"/home/linuxbrew/.linuxbrew/bin",
		"/home/linuxbrew/.linuxbrew/sbin",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("providerProbeSearchDirs(linux) = %v, want %v", got, want)
	}
}

func TestProviderProbeSearchDirsIncludesMacUserLocalAndHomebrewPaths(t *testing.T) {
	homeDir := t.TempDir()
	got := providerProbeSearchDirs(homeDir, "darwin", "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin")
	want := []string{
		filepath.Join(homeDir, ".local", "bin"),
		filepath.Join(homeDir, "bin"),
		"/usr/local/bin",
		"/usr/bin",
		"/bin",
		"/usr/sbin",
		"/sbin",
		"/opt/homebrew/bin",
		"/opt/homebrew/sbin",
		"/opt/local/bin",
		"/opt/local/sbin",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("providerProbeSearchDirs(darwin) = %v, want %v", got, want)
	}
}

func TestFindProbeBinaryUsesUserLocalInstallDir(t *testing.T) {
	homeDir := t.TempDir()
	userBin := filepath.Join(homeDir, ".local", "bin")
	if err := os.MkdirAll(userBin, 0o755); err != nil {
		t.Fatalf("mkdir user bin: %v", err)
	}
	writeExecutable(t, userBin, "claude", "#!/bin/sh\nexit 0\n")

	originalPathEnv := providerProbePathEnv
	originalGOOS := providerProbeGOOS
	providerProbePathEnv = "/usr/local/bin:/usr/bin:/bin"
	providerProbeGOOS = "linux"
	defer func() {
		providerProbePathEnv = originalPathEnv
		providerProbeGOOS = originalGOOS
	}()

	got, ok := findProbeBinary("claude", homeDir)
	if !ok {
		t.Fatal("findProbeBinary did not find ~/.local/bin/claude")
	}
	want := filepath.Join(userBin, "claude")
	if got != want {
		t.Fatalf("findProbeBinary = %q, want %q", got, want)
	}
}

func TestFindProbeBinaryUsesNVMInstallDir(t *testing.T) {
	homeDir := t.TempDir()
	nvmBin := filepath.Join(homeDir, ".nvm", "versions", "node", "v22.14.0", "bin")
	if err := os.MkdirAll(nvmBin, 0o755); err != nil {
		t.Fatalf("mkdir nvm bin: %v", err)
	}
	writeExecutable(t, nvmBin, "claude", "#!/bin/sh\nexit 0\n")

	originalPathEnv := providerProbePathEnv
	originalGOOS := providerProbeGOOS
	providerProbePathEnv = "/usr/local/bin:/usr/bin:/bin"
	providerProbeGOOS = "darwin"
	defer func() {
		providerProbePathEnv = originalPathEnv
		providerProbeGOOS = originalGOOS
	}()

	got, ok := findProbeBinary("claude", homeDir)
	if !ok {
		t.Fatal("findProbeBinary did not find nvm-installed claude")
	}
	want := filepath.Join(nvmBin, "claude")
	if got != want {
		t.Fatalf("findProbeBinary = %q, want %q", got, want)
	}
}

func TestProbeCommandEnvUsesCuratedProbePath(t *testing.T) {
	homeDir := t.TempDir()

	originalPathEnv := providerProbePathEnv
	originalGOOS := providerProbeGOOS
	providerProbePathEnv = "/usr/local/bin:/usr/bin:/bin"
	providerProbeGOOS = "linux"
	defer func() {
		providerProbePathEnv = originalPathEnv
		providerProbeGOOS = originalGOOS
	}()

	env := probeCommandEnv(homeDir)
	wantPath := "PATH=" + strings.Join([]string{
		filepath.Join(homeDir, ".local", "bin"),
		filepath.Join(homeDir, "bin"),
		"/usr/local/bin",
		"/usr/bin",
		"/bin",
		"/snap/bin",
		"/home/linuxbrew/.linuxbrew/bin",
		"/home/linuxbrew/.linuxbrew/sbin",
	}, string(os.PathListSeparator))
	if !slices.Contains(env, wantPath) {
		t.Fatalf("probeCommandEnv missing curated PATH %q in %v", wantPath, env)
	}
}

func TestProbeCommandEnvIncludesNVMInstallDir(t *testing.T) {
	homeDir := t.TempDir()
	nvmBin := filepath.Join(homeDir, ".nvm", "versions", "node", "v22.14.0", "bin")
	if err := os.MkdirAll(nvmBin, 0o755); err != nil {
		t.Fatalf("mkdir nvm bin: %v", err)
	}

	originalPathEnv := providerProbePathEnv
	originalGOOS := providerProbeGOOS
	providerProbePathEnv = "/usr/local/bin:/usr/bin:/bin"
	providerProbeGOOS = "darwin"
	defer func() {
		providerProbePathEnv = originalPathEnv
		providerProbeGOOS = originalGOOS
	}()

	env := probeCommandEnv(homeDir)
	pathEntry := ""
	for _, entry := range env {
		if strings.HasPrefix(entry, "PATH=") {
			pathEntry = strings.TrimPrefix(entry, "PATH=")
			break
		}
	}
	if pathEntry == "" {
		t.Fatalf("probeCommandEnv missing PATH in %v", env)
	}
	if !slices.Contains(filepath.SplitList(pathEntry), nvmBin) {
		t.Fatalf("probeCommandEnv PATH %q missing nvm bin %q", pathEntry, nvmBin)
	}
}

func TestHandleProviderReadinessReturnsConfiguredStatuses(t *testing.T) {
	homeDir := t.TempDir()
	binDir := filepath.Join(homeDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeExecutable(t, binDir, "claude", `#!/bin/sh
printf '%s\n' '{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty"}'
`)
	writeExecutable(t, binDir, "codex", "#!/bin/sh\nexit 0\n")
	writeExecutable(t, binDir, "gemini", "#!/bin/sh\nexit 0\n")

	if err := os.MkdirAll(filepath.Join(homeDir, ".codex"), 0o755); err != nil {
		t.Fatalf("mkdir codex dir: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(homeDir, ".codex", "auth.json"),
		[]byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"token"}}`),
		0o600,
	); err != nil {
		t.Fatalf("write codex auth: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(homeDir, ".gemini"), 0o755); err != nil {
		t.Fatalf("mkdir gemini dir: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(homeDir, ".gemini", "settings.json"),
		[]byte(`{"security":{"auth":{"selectedType":"oauth-personal"}}}`),
		0o600,
	); err != nil {
		t.Fatalf("write gemini settings: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(homeDir, ".gemini", "oauth_creds.json"),
		[]byte(`{"refresh_token":"token"}`),
		0o600,
	); err != nil {
		t.Fatalf("write gemini creds: %v", err)
	}

	t.Setenv("HOME", homeDir)
	originalPathEnv := providerProbePathEnv
	originalCommandContext := providerProbeCommandContext
	providerProbePathEnv = binDir
	providerProbeCommandContext = exec.CommandContext
	defer func() {
		providerProbePathEnv = originalPathEnv
		providerProbeCommandContext = originalCommandContext
	}()

	srv := New(newFakeState(t))
	req := httptest.NewRequest(http.MethodGet, "/v0/provider-readiness?providers=claude,codex,gemini", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp providerReadinessResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := resp.Providers["claude"].Status; got != probeStatusConfigured {
		t.Errorf("claude status = %q, want %q", got, probeStatusConfigured)
	}
	if got := resp.Providers["codex"].Status; got != probeStatusConfigured {
		t.Errorf("codex status = %q, want %q", got, probeStatusConfigured)
	}
	if got := resp.Providers["gemini"].Status; got != probeStatusConfigured {
		t.Errorf("gemini status = %q, want %q", got, probeStatusConfigured)
	}
}

func TestHandleProviderReadinessAcceptsClaudeOAuthTokenAuth(t *testing.T) {
	homeDir := t.TempDir()
	binDir := filepath.Join(homeDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeExecutable(t, binDir, "claude", `#!/bin/sh
printf '%s\n' '{"loggedIn":true,"authMethod":"oauth_token","apiProvider":"firstParty"}'
`)

	t.Setenv("HOME", homeDir)
	originalPathEnv := providerProbePathEnv
	originalCommandContext := providerProbeCommandContext
	providerProbePathEnv = binDir
	providerProbeCommandContext = exec.CommandContext
	defer func() {
		providerProbePathEnv = originalPathEnv
		providerProbeCommandContext = originalCommandContext
	}()

	srv := New(newFakeState(t))
	assertProviderStatus(t, srv, "/v0/provider-readiness?providers=claude", "claude", probeStatusConfigured)
}

func TestHandleReadinessReturnsConfiguredStatuses(t *testing.T) {
	homeDir := t.TempDir()
	binDir := filepath.Join(homeDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeExecutable(t, binDir, "claude", `#!/bin/sh
printf '%s\n' '{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty"}'
`)
	writeExecutable(t, binDir, "codex", "#!/bin/sh\nexit 0\n")
	writeExecutable(t, binDir, "gemini", "#!/bin/sh\nexit 0\n")
	writeExecutable(t, binDir, "gh", "#!/bin/sh\nexit 0\n")

	if err := os.MkdirAll(filepath.Join(homeDir, ".codex"), 0o755); err != nil {
		t.Fatalf("mkdir codex dir: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(homeDir, ".codex", "auth.json"),
		[]byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"token"}}`),
		0o600,
	); err != nil {
		t.Fatalf("write codex auth: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(homeDir, ".gemini"), 0o755); err != nil {
		t.Fatalf("mkdir gemini dir: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(homeDir, ".gemini", "settings.json"),
		[]byte(`{"security":{"auth":{"selectedType":"oauth-personal"}}}`),
		0o600,
	); err != nil {
		t.Fatalf("write gemini settings: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(homeDir, ".gemini", "oauth_creds.json"),
		[]byte(`{"refresh_token":"token"}`),
		0o600,
	); err != nil {
		t.Fatalf("write gemini creds: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(homeDir, ".config", "gh"), 0o755); err != nil {
		t.Fatalf("mkdir gh config dir: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(homeDir, ".config", "gh", "hosts.yml"),
		[]byte("github.com:\n    user: octocat\n    oauth_token: token\n"),
		0o600,
	); err != nil {
		t.Fatalf("write gh hosts: %v", err)
	}

	t.Setenv("HOME", homeDir)
	originalPathEnv := providerProbePathEnv
	originalCommandContext := providerProbeCommandContext
	providerProbePathEnv = binDir
	providerProbeCommandContext = exec.CommandContext
	defer func() {
		providerProbePathEnv = originalPathEnv
		providerProbeCommandContext = originalCommandContext
	}()

	srv := New(newFakeState(t))
	req := httptest.NewRequest(http.MethodGet, "/v0/readiness?items=claude,codex,gemini,github_cli", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp readinessResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, item := range []string{"claude", "codex", "gemini", "github_cli"} {
		if got := resp.Items[item].Status; got != probeStatusConfigured {
			t.Errorf("%s status = %q, want %q", item, got, probeStatusConfigured)
		}
	}
	if got := resp.Items["github_cli"].Kind; got != probeKindTool {
		t.Errorf("github_cli kind = %q, want %q", got, probeKindTool)
	}
}

func TestHandleProviderReadinessFreshBypassesCache(t *testing.T) {
	homeDir := t.TempDir()
	binDir := filepath.Join(homeDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeExecutable(t, binDir, "claude", `#!/bin/sh
/bin/cat "$HOME/claude-status.json"
`)
	if err := os.WriteFile(
		filepath.Join(homeDir, "claude-status.json"),
		[]byte(`{"loggedIn":false,"authMethod":"claude.ai","apiProvider":"firstParty"}`),
		0o600,
	); err != nil {
		t.Fatalf("write claude status: %v", err)
	}

	t.Setenv("HOME", homeDir)
	originalPathEnv := providerProbePathEnv
	originalCommandContext := providerProbeCommandContext
	providerProbePathEnv = binDir
	providerProbeCommandContext = exec.CommandContext
	defer func() {
		providerProbePathEnv = originalPathEnv
		providerProbeCommandContext = originalCommandContext
	}()

	srv := New(newFakeState(t))
	assertProviderStatus(t, srv, "/v0/provider-readiness?providers=claude&fresh=0", "claude", probeStatusNeedsAuth)

	if err := os.WriteFile(
		filepath.Join(homeDir, "claude-status.json"),
		[]byte(`{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty"}`),
		0o600,
	); err != nil {
		t.Fatalf("rewrite claude status: %v", err)
	}

	assertProviderStatus(t, srv, "/v0/provider-readiness?providers=claude&fresh=0", "claude", probeStatusNeedsAuth)
	assertProviderStatus(t, srv, "/v0/provider-readiness?providers=claude&fresh=1", "claude", probeStatusConfigured)
}

func TestHandleProviderReadinessRejectsUnknownProviders(t *testing.T) {
	srv := New(newFakeState(t))
	req := httptest.NewRequest(http.MethodGet, "/v0/provider-readiness?providers=claude,unknown", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleReadinessRejectsUnknownItems(t *testing.T) {
	srv := New(newFakeState(t))
	req := httptest.NewRequest(http.MethodGet, "/v0/readiness?items=claude,unknown", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleProviderReadinessReturnsNeedsAuthForCodexWithoutTokens(t *testing.T) {
	homeDir := t.TempDir()
	binDir := filepath.Join(homeDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeExecutable(t, binDir, "codex", "#!/bin/sh\nexit 0\n")

	if err := os.MkdirAll(filepath.Join(homeDir, ".codex"), 0o755); err != nil {
		t.Fatalf("mkdir codex dir: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(homeDir, ".codex", "auth.json"),
		[]byte(`{"auth_mode":"chatgpt","tokens":null}`),
		0o600,
	); err != nil {
		t.Fatalf("write codex auth: %v", err)
	}

	t.Setenv("HOME", homeDir)
	originalPathEnv := providerProbePathEnv
	providerProbePathEnv = binDir
	defer func() {
		providerProbePathEnv = originalPathEnv
	}()

	srv := New(newFakeState(t))
	req := httptest.NewRequest(http.MethodGet, "/v0/provider-readiness?providers=codex", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp providerReadinessResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := resp.Providers["codex"].Status; got != probeStatusNeedsAuth {
		t.Errorf("codex status = %q, want %q", got, probeStatusNeedsAuth)
	}
}

func TestHandleProviderReadinessReturnsNeedsAuthForCodexWithEmptyTokensObject(t *testing.T) {
	homeDir := t.TempDir()
	binDir := filepath.Join(homeDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeExecutable(t, binDir, "codex", "#!/bin/sh\nexit 0\n")

	if err := os.MkdirAll(filepath.Join(homeDir, ".codex"), 0o755); err != nil {
		t.Fatalf("mkdir codex dir: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(homeDir, ".codex", "auth.json"),
		[]byte(`{"auth_mode":"chatgpt","tokens":{}}`),
		0o600,
	); err != nil {
		t.Fatalf("write codex auth: %v", err)
	}

	t.Setenv("HOME", homeDir)
	originalPathEnv := providerProbePathEnv
	providerProbePathEnv = binDir
	defer func() {
		providerProbePathEnv = originalPathEnv
	}()

	srv := New(newFakeState(t))
	assertProviderStatus(t, srv, "/v0/provider-readiness?providers=codex&fresh=1", "codex", probeStatusNeedsAuth)
}

func TestHandleProviderReadinessReturnsNeedsAuthForLoggedOutClaude(t *testing.T) {
	homeDir := t.TempDir()
	binDir := filepath.Join(homeDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeExecutable(t, binDir, "claude", `#!/bin/sh
printf '%s\n' '{"loggedIn":false,"authMethod":"claude.ai","apiProvider":"firstParty"}'
`)

	t.Setenv("HOME", homeDir)
	originalPathEnv := providerProbePathEnv
	originalCommandContext := providerProbeCommandContext
	providerProbePathEnv = binDir
	providerProbeCommandContext = exec.CommandContext
	defer func() {
		providerProbePathEnv = originalPathEnv
		providerProbeCommandContext = originalCommandContext
	}()

	srv := New(newFakeState(t))
	assertProviderStatus(t, srv, "/v0/provider-readiness?providers=claude&fresh=1", "claude", probeStatusNeedsAuth)
}

func TestHandleProviderReadinessReturnsProbeErrorForClaudeInvalidJSON(t *testing.T) {
	homeDir := t.TempDir()
	binDir := filepath.Join(homeDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeExecutable(t, binDir, "claude", `#!/bin/sh
printf '%s\n' 'not-json'
`)

	t.Setenv("HOME", homeDir)
	originalPathEnv := providerProbePathEnv
	originalCommandContext := providerProbeCommandContext
	providerProbePathEnv = binDir
	providerProbeCommandContext = exec.CommandContext
	defer func() {
		providerProbePathEnv = originalPathEnv
		providerProbeCommandContext = originalCommandContext
	}()

	srv := New(newFakeState(t))
	assertProviderStatus(t, srv, "/v0/provider-readiness?providers=claude&fresh=1", "claude", probeStatusProbeError)
}

func TestHandleProviderReadinessReturnsNotInstalledWhenBinaryMissing(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	originalPathEnv := providerProbePathEnv
	providerProbePathEnv = filepath.Join(homeDir, "bin")
	defer func() {
		providerProbePathEnv = originalPathEnv
	}()

	srv := New(newFakeState(t))
	req := httptest.NewRequest(http.MethodGet, "/v0/provider-readiness?providers=claude,codex,gemini", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp providerReadinessResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, provider := range []string{"claude", "codex", "gemini"} {
		if got := resp.Providers[provider].Status; got != probeStatusNotInstalled {
			t.Errorf("%s status = %q, want %q", provider, got, probeStatusNotInstalled)
		}
	}
}

func TestHandleProviderReadinessReturnsInvalidConfigurationForUnsupportedAuthModes(t *testing.T) {
	homeDir := t.TempDir()
	binDir := filepath.Join(homeDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeExecutable(t, binDir, "codex", "#!/bin/sh\nexit 0\n")
	writeExecutable(t, binDir, "gemini", "#!/bin/sh\nexit 0\n")

	if err := os.MkdirAll(filepath.Join(homeDir, ".codex"), 0o755); err != nil {
		t.Fatalf("mkdir codex dir: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(homeDir, ".codex", "auth.json"),
		[]byte(`{"auth_mode":"api_key","tokens":{"access_token":"token"}}`),
		0o600,
	); err != nil {
		t.Fatalf("write codex auth: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(homeDir, ".gemini"), 0o755); err != nil {
		t.Fatalf("mkdir gemini dir: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(homeDir, ".gemini", "settings.json"),
		[]byte(`{"security":{"auth":{"selectedType":"gemini-api-key"}}}`),
		0o600,
	); err != nil {
		t.Fatalf("write gemini settings: %v", err)
	}

	t.Setenv("HOME", homeDir)
	originalPathEnv := providerProbePathEnv
	providerProbePathEnv = binDir
	defer func() {
		providerProbePathEnv = originalPathEnv
	}()

	srv := New(newFakeState(t))
	req := httptest.NewRequest(http.MethodGet, "/v0/provider-readiness?providers=codex,gemini", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp providerReadinessResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := resp.Providers["codex"].Status; got != probeStatusInvalidConfiguration {
		t.Errorf("codex status = %q, want %q", got, probeStatusInvalidConfiguration)
	}
	if got := resp.Providers["gemini"].Status; got != probeStatusInvalidConfiguration {
		t.Errorf("gemini status = %q, want %q", got, probeStatusInvalidConfiguration)
	}
}

func TestHandleProviderReadinessReturnsNeedsAuthForGeminiWithoutSelectedType(t *testing.T) {
	homeDir := t.TempDir()
	binDir := filepath.Join(homeDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeExecutable(t, binDir, "gemini", "#!/bin/sh\nexit 0\n")

	if err := os.MkdirAll(filepath.Join(homeDir, ".gemini"), 0o755); err != nil {
		t.Fatalf("mkdir gemini dir: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(homeDir, ".gemini", "settings.json"),
		[]byte(`{"security":{"auth":{"selectedType":""}}}`),
		0o600,
	); err != nil {
		t.Fatalf("write gemini settings: %v", err)
	}

	t.Setenv("HOME", homeDir)
	originalPathEnv := providerProbePathEnv
	providerProbePathEnv = binDir
	defer func() {
		providerProbePathEnv = originalPathEnv
	}()

	srv := New(newFakeState(t))
	assertProviderStatus(t, srv, "/v0/provider-readiness?providers=gemini&fresh=1", "gemini", probeStatusNeedsAuth)
}

func TestHandleProviderReadinessReturnsNeedsAuthForGeminiWithoutRefreshToken(t *testing.T) {
	homeDir := t.TempDir()
	binDir := filepath.Join(homeDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeExecutable(t, binDir, "gemini", "#!/bin/sh\nexit 0\n")

	if err := os.MkdirAll(filepath.Join(homeDir, ".gemini"), 0o755); err != nil {
		t.Fatalf("mkdir gemini dir: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(homeDir, ".gemini", "settings.json"),
		[]byte(`{"security":{"auth":{"selectedType":"oauth-personal"}}}`),
		0o600,
	); err != nil {
		t.Fatalf("write gemini settings: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(homeDir, ".gemini", "oauth_creds.json"),
		[]byte(`{}`),
		0o600,
	); err != nil {
		t.Fatalf("write gemini creds: %v", err)
	}

	t.Setenv("HOME", homeDir)
	originalPathEnv := providerProbePathEnv
	providerProbePathEnv = binDir
	defer func() {
		providerProbePathEnv = originalPathEnv
	}()

	srv := New(newFakeState(t))
	assertProviderStatus(t, srv, "/v0/provider-readiness?providers=gemini&fresh=1", "gemini", probeStatusNeedsAuth)
}

func TestHandleReadinessReturnsNeedsAuthForGitHubCLIWithoutHostsFile(t *testing.T) {
	homeDir := t.TempDir()
	binDir := filepath.Join(homeDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeGitHubCLIAuthStatusScript(t, binDir, 1)
	unsetGitHubCLITokenEnv(t)

	t.Setenv("HOME", homeDir)
	originalPathEnv := providerProbePathEnv
	providerProbePathEnv = binDir
	defer func() {
		providerProbePathEnv = originalPathEnv
	}()

	srv := New(newFakeState(t))
	req := httptest.NewRequest(http.MethodGet, "/v0/readiness?items=github_cli&fresh=1", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp readinessResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := resp.Items["github_cli"].Status; got != probeStatusNeedsAuth {
		t.Fatalf("github_cli status = %q, want %q", got, probeStatusNeedsAuth)
	}
}

func TestHandleReadinessReturnsConfiguredForGitHubCLIEnvToken(t *testing.T) {
	homeDir := t.TempDir()
	binDir := filepath.Join(homeDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeExecutable(t, binDir, "gh", "#!/bin/sh\nexit 0\n")
	unsetGitHubCLITokenEnv(t)
	t.Setenv("GH_TOKEN", "token")
	t.Setenv("HOME", homeDir)

	originalPathEnv := providerProbePathEnv
	providerProbePathEnv = binDir
	defer func() {
		providerProbePathEnv = originalPathEnv
	}()

	srv := New(newFakeState(t))
	assertGitHubCLIReadinessStatus(t, srv, probeStatusConfigured)
}

func TestHandleReadinessReturnsConfiguredForGitHubCLICustomConfigDir(t *testing.T) {
	homeDir := t.TempDir()
	binDir := filepath.Join(homeDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeGitHubCLIAuthStatusScript(t, binDir, 1)
	configDir := filepath.Join(homeDir, "custom-gh")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir custom gh config dir: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(configDir, "hosts.yml"),
		[]byte("github.com:\n    user: octocat\n    oauth_token: token\n"),
		0o600,
	); err != nil {
		t.Fatalf("write custom gh hosts: %v", err)
	}
	unsetGitHubCLITokenEnv(t)
	t.Setenv("GH_CONFIG_DIR", configDir)
	t.Setenv("HOME", homeDir)

	originalPathEnv := providerProbePathEnv
	providerProbePathEnv = binDir
	defer func() {
		providerProbePathEnv = originalPathEnv
	}()

	srv := New(newFakeState(t))
	assertGitHubCLIReadinessStatus(t, srv, probeStatusConfigured)
}

func TestHandleReadinessReturnsNotInstalledForGitHubCLIWithoutBinary(t *testing.T) {
	homeDir := t.TempDir()
	unsetGitHubCLITokenEnv(t)
	t.Setenv("HOME", homeDir)

	originalPathEnv := providerProbePathEnv
	originalGOOS := providerProbeGOOS
	providerProbePathEnv = filepath.Join(homeDir, "bin")
	defer func() {
		providerProbePathEnv = originalPathEnv
		providerProbeGOOS = originalGOOS
	}()
	providerProbeGOOS = "linux"

	srv := New(newFakeState(t))
	assertGitHubCLIReadinessStatus(t, srv, probeStatusNotInstalled)
}

func TestHandleReadinessReturnsNeedsAuthForGitHubCLIWithoutStoredTokens(t *testing.T) {
	homeDir := t.TempDir()
	binDir := filepath.Join(homeDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeGitHubCLIAuthStatusScript(t, binDir, 1)
	if err := os.MkdirAll(filepath.Join(homeDir, ".config", "gh"), 0o755); err != nil {
		t.Fatalf("mkdir gh config dir: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(homeDir, ".config", "gh", "hosts.yml"),
		[]byte("github.com:\n    user: octocat\n    git_protocol: https\n"),
		0o600,
	); err != nil {
		t.Fatalf("write gh hosts: %v", err)
	}
	unsetGitHubCLITokenEnv(t)
	t.Setenv("HOME", homeDir)

	originalPathEnv := providerProbePathEnv
	providerProbePathEnv = binDir
	defer func() {
		providerProbePathEnv = originalPathEnv
	}()

	srv := New(newFakeState(t))
	assertGitHubCLIReadinessStatus(t, srv, probeStatusNeedsAuth)
}

func TestHandleReadinessReturnsConfiguredForGitHubCLIAuthStatusFallback(t *testing.T) {
	homeDir := t.TempDir()
	binDir := filepath.Join(homeDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeGitHubCLIAuthStatusScript(t, binDir, 0)
	if err := os.MkdirAll(filepath.Join(homeDir, ".config", "gh"), 0o755); err != nil {
		t.Fatalf("mkdir gh config dir: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(homeDir, ".config", "gh", "hosts.yml"),
		[]byte("github.com:\n    user: octocat\n"),
		0o600,
	); err != nil {
		t.Fatalf("write gh hosts: %v", err)
	}
	unsetGitHubCLITokenEnv(t)
	t.Setenv("HOME", homeDir)

	originalPathEnv := providerProbePathEnv
	providerProbePathEnv = binDir
	defer func() {
		providerProbePathEnv = originalPathEnv
	}()

	srv := New(newFakeState(t))
	assertGitHubCLIReadinessStatus(t, srv, probeStatusConfigured)
}

func TestHandleReadinessReturnsProbeErrorForGitHubCLIMalformedHostsFile(t *testing.T) {
	homeDir := t.TempDir()
	binDir := filepath.Join(homeDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeGitHubCLIAuthStatusScript(t, binDir, 1)
	if err := os.MkdirAll(filepath.Join(homeDir, ".config", "gh"), 0o755); err != nil {
		t.Fatalf("mkdir gh config dir: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(homeDir, ".config", "gh", "hosts.yml"),
		[]byte("github.com: ["),
		0o600,
	); err != nil {
		t.Fatalf("write gh hosts: %v", err)
	}
	unsetGitHubCLITokenEnv(t)
	t.Setenv("HOME", homeDir)

	originalPathEnv := providerProbePathEnv
	providerProbePathEnv = binDir
	defer func() {
		providerProbePathEnv = originalPathEnv
	}()

	srv := New(newFakeState(t))
	assertGitHubCLIReadinessStatus(t, srv, probeStatusProbeError)
}

func writeExecutable(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func writeGitHubCLIAuthStatusScript(t *testing.T, dir string, exitCode int) {
	t.Helper()
	writeExecutable(t, dir, "gh", fmt.Sprintf(`#!/bin/sh
if [ "$1" = "auth" ] && [ "$2" = "status" ]; then
	if [ %d -eq 0 ]; then
		exit 0
	fi
	echo "not logged in" >&2
	exit %d
fi
echo "unexpected gh args: $*" >&2
exit 2
`, exitCode, exitCode))
}

func assertProviderStatus(t *testing.T, srv *Server, path, provider, want string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp providerReadinessResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := resp.Providers[provider].Status; got != want {
		t.Fatalf("%s status = %q, want %q", provider, got, want)
	}
}

func assertGitHubCLIReadinessStatus(t *testing.T, srv *Server, want string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v0/readiness?items=github_cli&fresh=1", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp readinessResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := resp.Items["github_cli"].Status; got != want {
		t.Fatalf("github_cli status = %q, want %q", got, want)
	}
}

func unsetGitHubCLITokenEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_ENTERPRISE_TOKEN", "")
	t.Setenv("GITHUB_ENTERPRISE_TOKEN", "")
	// Clear config-dir overrides so githubCLIHostsPath falls through
	// to the HOME-based default path.
	t.Setenv("GH_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
}
