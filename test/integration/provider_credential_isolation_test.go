//go:build integration

package integration

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/worker/builtin"
)

// TestIntegrationEnvFor_IsolatesProviderHomes proves CODEX_HOME and
// CLAUDE_CONFIG_DIR are set to per-test directories under gcHome, that those
// directories exist, and that they are empty by default (no seeded
// auth.json/credentials) -- ga-5cokvb.1.
func TestIntegrationEnvFor_IsolatesProviderHomes(t *testing.T) {
	gcHome := t.TempDir()
	env := integrationEnvFor(t, gcHome, t.TempDir(), false)
	got := parseEnvList(env)

	cases := []struct {
		envVar string
		subdir string
	}{
		{"CODEX_HOME", ".codex"},
		{"CLAUDE_CONFIG_DIR", ".claude"},
	}

	for _, c := range cases {
		t.Run(c.envVar, func(t *testing.T) {
			want := filepath.Join(gcHome, c.subdir)
			gotVal, ok := got[c.envVar]
			if !ok {
				t.Fatalf("%s not set in integrationEnvFor's env", c.envVar)
			}
			if gotVal != want {
				t.Fatalf("%s = %q, want %q", c.envVar, gotVal, want)
			}

			entries, err := os.ReadDir(want)
			if err != nil {
				t.Fatalf("reading %s: %v (dir must exist)", want, err)
			}
			if len(entries) != 0 {
				names := make([]string, len(entries))
				for i, e := range entries {
					names[i] = e.Name()
				}
				t.Fatalf("%s is not empty: %v (must not seed credentials by default)", want, names)
			}
		})
	}
}

// literalCredentialEnvFloor is the hand-written set of provider credential
// variables integrationEnvFor must always strip, independent of the builtin
// provider catalog.
var literalCredentialEnvFloor = []string{
	"OPENAI_API_KEY",
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"GEMINI_API_KEY",
	"GOOGLE_API_KEY",
}

// TestIntegrationEnvFor_StripsProviderCredentialEnvVars proves every
// UpstreamAPIKeyEnv/UpstreamAuthTokenEnv value declared in the builtin
// provider catalog, plus GOOGLE_API_KEY, is absent from integrationEnvFor's
// returned env even when set in the test process's ambient environment.
//
// Reads the live catalog via builtin.BuiltinProviders() instead of a
// hardcoded list: a hand-transcribed copy of this same list already drifted
// once (it was missing GitHub Copilot's UpstreamAuthTokenEnv,
// COPILOT_GITHUB_TOKEN) -- ga-5cokvb.1.
func TestIntegrationEnvFor_StripsProviderCredentialEnvVars(t *testing.T) {
	credEnvVars := map[string]bool{}
	// Literal floor: the five variables that carry the operator's provider
	// credentials, spelled out here on purpose. The strip list in
	// integrationEnvFor is derived from the same catalog the loop below reads,
	// so a catalog edit that drops one of these names would silently drop it
	// from both the strip list and the derived set; the floor keeps that edit
	// a test failure -- ga-qnqmzy.2.
	for _, name := range literalCredentialEnvFloor {
		credEnvVars[name] = true
	}
	for _, spec := range builtin.BuiltinProviders() {
		if spec.UpstreamAPIKeyEnv != "" {
			credEnvVars[spec.UpstreamAPIKeyEnv] = true
		}
		if spec.UpstreamAuthTokenEnv != "" {
			credEnvVars[spec.UpstreamAuthTokenEnv] = true
		}
	}
	if len(credEnvVars) < 10 {
		t.Fatalf("only found %d credential env var names via builtin.BuiltinProviders(); catalog lookup is likely broken", len(credEnvVars))
	}

	for name := range credEnvVars {
		t.Setenv(name, "leaked-test-secret")
	}

	env := integrationEnvFor(t, t.TempDir(), t.TempDir(), false)
	got := parseEnvList(env)

	for name := range credEnvVars {
		if v, present := got[name]; present {
			t.Errorf("integrationEnvFor leaked %s=%q into the subprocess env", name, v)
		}
	}
}

// TestIntegrationEnvFor_IsolatedHomesAreNeverTheRealHome proves the
// CODEX_HOME/CLAUDE_CONFIG_DIR values integrationEnvFor sets can never
// resolve to the real host's ~/.codex or ~/.claude. The codex and claude
// CLIs read these env vars to locate their credential store, so a test
// run's provider CLI subprocesses cannot read or write the operator's real
// provider credentials through them -- ga-5cokvb.1.
//
// The guarantee is scoped to those two variables. gc's own readiness probes
// (probeCodex reads <HOME>/.codex/auth.json, for one) resolve their home from
// HOME and ignore CODEX_HOME; integrationEnvFor points HOME at a private
// directory under gcHome (isolateGCHomeEnv), so a gc process started under this
// env probes that directory. A test that execs gc under some other env, or a
// process that resolves its home from the passwd entry, is outside this test;
// reads of that kind are tracked by ga-qnqmzy.
func TestIntegrationEnvFor_IsolatedHomesAreNeverTheRealHome(t *testing.T) {
	realHome, err := realHomeDir()
	if err != nil {
		t.Fatalf("realHomeDir: %v", err)
	}
	realCodexHome := filepath.Join(realHome, ".codex")
	realClaudeConfigDir := filepath.Join(realHome, ".claude")

	env := integrationEnvFor(t, t.TempDir(), t.TempDir(), false)
	got := parseEnvList(env)

	if got["CODEX_HOME"] == "" {
		t.Fatal("CODEX_HOME not set at all -- a codex CLI subprocess would fall back to the real ~/.codex")
	}
	if got["CODEX_HOME"] == realCodexHome {
		t.Fatalf("CODEX_HOME resolved to the real host directory %s", realCodexHome)
	}
	if got["CLAUDE_CONFIG_DIR"] == "" {
		t.Fatal("CLAUDE_CONFIG_DIR not set at all -- a claude CLI subprocess would fall back to the real ~/.claude")
	}
	if got["CLAUDE_CONFIG_DIR"] == realClaudeConfigDir {
		t.Fatalf("CLAUDE_CONFIG_DIR resolved to the real host directory %s", realClaudeConfigDir)
	}
}

// TestBootstrapDriftEnv_InheritsProviderIsolation proves
// newDriftIsolatedEnvRoot's env -- what bootstrapDriftCity receives at every
// call site in start_drift_test.go -- traces through integrationEnvFor
// rather than being independently constructed, so it inherits both the
// CODEX_HOME/CLAUDE_CONFIG_DIR isolation and the credential-var stripping
// proven above -- ga-5cokvb.1.
func TestBootstrapDriftEnv_InheritsProviderIsolation(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "leaked-test-secret")

	gcHome, _, env := newDriftIsolatedEnvRoot(t)
	got := parseEnvList(env)

	wantCodexHome := filepath.Join(gcHome, ".codex")
	if got["CODEX_HOME"] != wantCodexHome {
		t.Fatalf("CODEX_HOME = %q, want %q (bootstrapDriftCity's env is not isolated)", got["CODEX_HOME"], wantCodexHome)
	}
	if v, present := got["OPENAI_API_KEY"]; present {
		t.Errorf("newDriftIsolatedEnvRoot leaked OPENAI_API_KEY=%q (bootstrapDriftCity's env is not stripped)", v)
	}
}

// TestRealHomeDir_IgnoresHOMEEnv proves the real home the isolation tests
// compare against comes from the passwd entry for the current uid and not from
// $HOME: os.UserHomeDir() returns $HOME, so a test process started with HOME
// already pointed at an isolated directory would compare the isolated homes
// against themselves and pass vacuously -- ga-qnqmzy.2.
func TestRealHomeDir_IgnoresHOMEEnv(t *testing.T) {
	lu, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		t.Skipf("no passwd entry for uid %d: %v", os.Getuid(), err)
	}
	if strings.TrimSpace(lu.HomeDir) == "" {
		t.Skipf("passwd entry for uid %d has no home directory", os.Getuid())
	}

	fake := t.TempDir()
	t.Setenv("HOME", fake)

	got, err := realHomeDir()
	if err != nil {
		t.Fatalf("realHomeDir: %v", err)
	}
	if got == fake {
		t.Fatalf("realHomeDir() followed $HOME (%s) instead of the passwd entry", fake)
	}
	if got != lu.HomeDir {
		t.Fatalf("realHomeDir() = %q, want the passwd home %q", got, lu.HomeDir)
	}
}
