package gitcred

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// clearCredEnv resets the credential env so a test starts from a clean slate.
func clearCredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv(EnvCredentialsFile, "")
	t.Setenv(EnvCredentialCommand, "")
	// Clear the ambient GitHub token env so the built-in github.com default rule
	// stays inert and tests observe only their own configured rules.
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
}

func stubExe(t *testing.T, path string) {
	t.Helper()
	prev := osExecutable
	osExecutable = func() (string, error) { return path, nil }
	t.Cleanup(func() { osExecutable = prev })
}

func TestInjectionZeroWhenNoFilesNoMatch(t *testing.T) {
	clearCredEnv(t)
	inj, err := CredentialedNetworkArgs("/usr/bin/gc", "", "https://github.com/org/repo")
	if err != nil {
		t.Fatalf("CredentialedNetworkArgs: %v", err)
	}
	if len(inj.CfgArgs) != 0 || len(inj.Env) != 0 || inj.Matched {
		t.Fatalf("expected zero injection, got %+v", inj)
	}
}

func TestInjectionZeroWhenTransportIncompatibleOnly(t *testing.T) {
	home := t.TempDir()
	clearCredEnv(t)
	t.Setenv("GC_HOME", home)
	writeCredFile(t, filepath.Join(home, "credentials.toml"),
		"[[credential]]\nmatch=\"github.com/org\"\nssh_key_file=\"~/.ssh/id\"\n", 0o600)
	// https URL with only an ssh rule → no match → zero injection.
	inj, err := CredentialedNetworkArgs("/usr/bin/gc", "", "https://github.com/org/repo")
	if err != nil {
		t.Fatalf("CredentialedNetworkArgs: %v", err)
	}
	if len(inj.CfgArgs) != 0 || len(inj.Env) != 0 || inj.Matched {
		t.Fatalf("expected zero injection, got %+v", inj)
	}
}

func TestInjectionHTTPSMatch(t *testing.T) {
	home := t.TempDir()
	clearCredEnv(t)
	t.Setenv("GC_HOME", home)
	writeCredFile(t, filepath.Join(home, "credentials.toml"),
		"[[credential]]\nmatch=\"github.com/org\"\nhelper=\"gh auth token\"\n", 0o600)

	gcExe := "/opt/my gc/bin/gc" // path with a space to exercise sq-quoting.
	inj, err := CredentialedNetworkArgs(gcExe, "/city", "https://github.com/org/repo")
	if err != nil {
		t.Fatalf("CredentialedNetworkArgs: %v", err)
	}
	if !inj.Matched {
		t.Fatalf("expected matched injection")
	}
	wantCfg := []string{
		"-c", "credential.helper=",
		"-c", "credential.helper=!'/opt/my gc/bin/gc' git-credential",
		"-c", "credential.useHttpPath=true",
	}
	if !reflect.DeepEqual(inj.CfgArgs, wantCfg) {
		t.Fatalf("CfgArgs = %#v\nwant %#v", inj.CfgArgs, wantCfg)
	}
	wantEnv := []string{"GIT_TERMINAL_PROMPT=0", "GC_CREDENTIAL_CITY=/city"}
	if !reflect.DeepEqual(inj.Env, wantEnv) {
		t.Fatalf("Env = %#v\nwant %#v", inj.Env, wantEnv)
	}
}

func TestInjectionOmitsCityWhenEmpty(t *testing.T) {
	home := t.TempDir()
	clearCredEnv(t)
	t.Setenv("GC_HOME", home)
	writeCredFile(t, filepath.Join(home, "credentials.toml"),
		"[[credential]]\nmatch=\"github.com\"\nhelper=\"gh auth token\"\n", 0o600)
	inj, err := CredentialedNetworkArgs("/usr/bin/gc", "", "https://github.com/org/repo")
	if err != nil {
		t.Fatalf("CredentialedNetworkArgs: %v", err)
	}
	for _, e := range inj.Env {
		if strings.HasPrefix(e, EnvCredentialCity+"=") {
			t.Fatalf("GC_CREDENTIAL_CITY must be omitted when cityRoot is empty, got %v", inj.Env)
		}
	}
}

func TestInjectionSSHMatch(t *testing.T) {
	home := t.TempDir()
	clearCredEnv(t)
	t.Setenv("GC_HOME", home)
	writeCredFile(t, filepath.Join(home, "credentials.toml"),
		"[[credential]]\nmatch=\"github.com/org\"\nssh_key_file=\"/keys/id ed\"\n", 0o600)
	inj, err := CredentialedNetworkArgs("/usr/bin/gc", "", "git@github.com:org/repo.git")
	if err != nil {
		t.Fatalf("CredentialedNetworkArgs: %v", err)
	}
	if inj.CfgArgs != nil {
		t.Fatalf("ssh match must have nil CfgArgs, got %#v", inj.CfgArgs)
	}
	want := "GIT_SSH_COMMAND=ssh -i '/keys/id ed' -o IdentitiesOnly=yes -o BatchMode=yes"
	if len(inj.Env) != 1 || inj.Env[0] != want {
		t.Fatalf("Env = %#v\nwant [%q]", inj.Env, want)
	}
}

func TestInjectionResolvesExeViaSeamWhenEmpty(t *testing.T) {
	home := t.TempDir()
	clearCredEnv(t)
	t.Setenv("GC_HOME", home)
	writeCredFile(t, filepath.Join(home, "credentials.toml"),
		"[[credential]]\nmatch=\"github.com\"\nhelper=\"gh auth token\"\n", 0o600)
	stubExe(t, "/seam/gc")
	inj, err := CredentialedNetworkArgs("", "", "https://github.com/org/repo")
	if err != nil {
		t.Fatalf("CredentialedNetworkArgs: %v", err)
	}
	if !strings.Contains(strings.Join(inj.CfgArgs, " "), "'/seam/gc' git-credential") {
		t.Fatalf("expected seam exe in helper, got %#v", inj.CfgArgs)
	}
}

func TestInjectionFailsClosedOnBadPerms(t *testing.T) {
	city := t.TempDir()
	clearCredEnv(t)
	writeCredFile(t, filepath.Join(city, ".gc", "credentials.toml"),
		"[[credential]]\nmatch=\"github.com\"\nhelper=\"x\"\n", 0o644)
	if _, err := CredentialedNetworkArgs("/usr/bin/gc", city, "https://github.com/org/repo"); err == nil {
		t.Fatalf("expected fail-closed error on bad perms")
	}
}

func TestInjectionMatchedUnresolvableExe(t *testing.T) {
	home := t.TempDir()
	clearCredEnv(t)
	t.Setenv("GC_HOME", home)
	writeCredFile(t, filepath.Join(home, "credentials.toml"),
		"[[credential]]\nmatch=\"github.com\"\nhelper=\"x\"\n", 0o600)
	prev := osExecutable
	osExecutable = func() (string, error) { return "", os.ErrNotExist }
	t.Cleanup(func() { osExecutable = prev })
	if _, err := CredentialedNetworkArgs("", "", "https://github.com/org/repo"); err == nil {
		t.Fatalf("expected error when matched rule has unresolvable gcExe")
	}
}

func TestInjectionCommandLayerWiresHelper(t *testing.T) {
	clearCredEnv(t)
	t.Setenv(EnvCredentialCommand, "my-helper get")
	inj, err := CredentialedNetworkArgs("/usr/bin/gc", "", "https://github.com/org/repo")
	if err != nil {
		t.Fatalf("CredentialedNetworkArgs: %v", err)
	}
	if len(inj.CfgArgs) == 0 {
		t.Fatalf("command layer must wire gc as the helper for https URLs")
	}
	if inj.Matched {
		t.Fatalf("command-layer fallback is not a rule match")
	}
}

func TestInjectionGitHubDefaultTokenFromEnv(t *testing.T) {
	// With no credentials.toml but an ambient GitHub token, the built-in
	// github.com default rule authenticates an https github.com pack clone.
	clearCredEnv(t)
	t.Setenv("GITHUB_TOKEN", "ghp_example")
	stubExe(t, "/usr/bin/gc")
	inj, err := CredentialedNetworkArgs("/usr/bin/gc", "", "https://github.com/org/repo")
	if err != nil {
		t.Fatalf("CredentialedNetworkArgs: %v", err)
	}
	if !inj.Matched {
		t.Fatalf("expected the built-in github.com default to match, got %+v", inj)
	}
	if len(inj.CfgArgs) == 0 {
		t.Fatalf("expected credential config args, got none: %+v", inj)
	}
}

func TestInjectionGitHubDefaultPrefersGhToken(t *testing.T) {
	// GH_TOKEN takes priority over GITHUB_TOKEN, matching the gh CLI convention
	// the default cites: a broad GH_TOKEN PAT set alongside the workflow's
	// repo-scoped GITHUB_TOKEN is the usual cross-repo override, so it must win.
	clearCredEnv(t)
	t.Setenv("GITHUB_TOKEN", "ghp_example")
	t.Setenv("GH_TOKEN", "gh_example")
	rules, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rule, ok := rules.MatchSource("https://github.com/org/repo")
	if !ok {
		t.Fatalf("expected github.com default rule to match")
	}
	if rule.TokenEnv != "GH_TOKEN" {
		t.Fatalf("TokenEnv = %q, want GH_TOKEN (priority over GITHUB_TOKEN)", rule.TokenEnv)
	}
}

func TestInjectionGitHubDefaultOnlyGitHubHost(t *testing.T) {
	// The default token is scoped to the exact github.com host and never offered
	// elsewhere, including look-alike hosts that merely contain "github.com" as a
	// substring, suffix, or subdomain label.
	clearCredEnv(t)
	t.Setenv("GITHUB_TOKEN", "ghp_example")
	rules, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, src := range []string{
		"https://gitlab.example.com/org/repo",
		"https://github.com.evil.com/org/repo",
		"https://evil-github.com/org/repo",
		"https://github.com.attacker.io/org/repo",
		"https://notgithub.com/org/repo",
	} {
		if _, ok := rules.MatchSource(src); ok {
			t.Fatalf("github.com default must not match look-alike host %q", src)
		}
	}
}

func TestInjectionGitHubDefaultHTTPSOnly(t *testing.T) {
	// The built-in ambient token is an HTTPS convenience; it must never be
	// offered to a plaintext http github.com clone, which would leak the bearer
	// token over cleartext.
	clearCredEnv(t)
	t.Setenv("GITHUB_TOKEN", "ghp_example")
	stubExe(t, "/usr/bin/gc")
	inj, err := CredentialedNetworkArgs("/usr/bin/gc", "", "http://github.com/org/repo")
	if err != nil {
		t.Fatalf("CredentialedNetworkArgs: %v", err)
	}
	if inj.Matched || len(inj.CfgArgs) != 0 || len(inj.Env) != 0 {
		t.Fatalf("plaintext http github.com must not receive the ambient token, got %+v", inj)
	}
}

func TestInjectionGitHubDefaultInertWithoutToken(t *testing.T) {
	// No ambient token → byte-identical to today (no rule, no injection).
	clearCredEnv(t)
	inj, err := CredentialedNetworkArgs("/usr/bin/gc", "", "https://github.com/org/repo")
	if err != nil {
		t.Fatalf("CredentialedNetworkArgs: %v", err)
	}
	if len(inj.CfgArgs) != 0 || len(inj.Env) != 0 || inj.Matched {
		t.Fatalf("expected zero injection without a token, got %+v", inj)
	}
}

func TestInjectionCommandLayerBeatsGitHubDefault(t *testing.T) {
	// With both a configured command layer and an ambient GitHub token, a
	// github.com clone must route through the command-layer fallback
	// (Matched=false, command-layer origin), not the built-in github.com
	// default. This pins the contract the Load comment promises: an explicitly
	// configured command helper outranks the ambient-token convenience default.
	clearCredEnv(t)
	t.Setenv("GITHUB_TOKEN", "ghp_example")
	t.Setenv(EnvCredentialCommand, "my-helper get")
	stubExe(t, "/usr/bin/gc")
	inj, err := CredentialedNetworkArgs("/usr/bin/gc", "", "https://github.com/org/repo")
	if err != nil {
		t.Fatalf("CredentialedNetworkArgs: %v", err)
	}
	if inj.Matched {
		t.Fatalf("command layer is a no-rule fallback; want Matched=false, got %+v", inj)
	}
	if inj.RuleOrigin != commandLayerOrigin {
		t.Fatalf("RuleOrigin = %q, want %q (command-layer fallback, not the github.com default)", inj.RuleOrigin, commandLayerOrigin)
	}
	if len(inj.CfgArgs) == 0 {
		t.Fatalf("expected credential-helper wiring for the command layer, got none: %+v", inj)
	}
}
