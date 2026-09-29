//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/worker/builtin"
)

// TestIntegrationEnvFor_IsolatesProviderHomes proves CODEX_HOME and
// CLAUDE_CONFIG_DIR are set to per-test directories under gcHome, that those
// directories exist, and that they are empty by default (no seeded
// auth.json/credentials) -- ga-5cokvb.1.
func TestIntegrationEnvFor_IsolatesProviderHomes(t *testing.T) {
	gcHome := t.TempDir()
	env := integrationEnvFor(gcHome, t.TempDir(), false)
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
	credEnvVars := map[string]bool{"GOOGLE_API_KEY": true}
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

	env := integrationEnvFor(t.TempDir(), t.TempDir(), false)
	got := parseEnvList(env)

	for name := range credEnvVars {
		if v, present := got[name]; present {
			t.Errorf("integrationEnvFor leaked %s=%q into the subprocess env", name, v)
		}
	}
}

// TestIntegrationEnvFor_IsolatedHomesAreNeverTheRealHome proves the
// CODEX_HOME/CLAUDE_CONFIG_DIR values integrationEnvFor sets can never
// resolve to the real host's ~/.codex or ~/.claude. Both the codex and
// claude CLIs read these env vars to locate their credential store, so this
// is the concrete guarantee that a test/integration run cannot read or write
// the operator's real provider credentials -- ga-5cokvb.1.
func TestIntegrationEnvFor_IsolatedHomesAreNeverTheRealHome(t *testing.T) {
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir: %v", err)
	}
	realCodexHome := filepath.Join(realHome, ".codex")
	realClaudeConfigDir := filepath.Join(realHome, ".claude")

	env := integrationEnvFor(t.TempDir(), t.TempDir(), false)
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
