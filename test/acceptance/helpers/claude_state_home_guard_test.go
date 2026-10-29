package acceptancehelpers

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// canaryRealHome stands a temp directory in for the real user home, with a
// sentinel ~/.claude.json and ~/.claude/.claude.json, and returns it plus a
// check that both sentinels are byte-identical and nothing else appeared.
//
// It is the #6838 canary shape: the harness must leave a planted copy of the
// developer's own state untouched, and the only way to prove an absence of
// writes is to plant something a write would change.
func canaryRealHome(t *testing.T) (string, func()) {
	t.Helper()
	home := t.TempDir()
	prev := realUserHome
	realUserHome = func() string { return home }
	t.Cleanup(func() { realUserHome = prev })

	sentinel := []byte("{\"canary\": \"the developer's own Claude state\"}\n")
	files := []string{
		filepath.Join(home, ".claude.json"),
		filepath.Join(home, ".claude", ".claude.json"),
	}
	for _, f := range files {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, sentinel, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home, func() {
		t.Helper()
		for _, f := range files {
			got, err := os.ReadFile(f)
			if err != nil {
				t.Errorf("canary %s: %v", f, err)
				continue
			}
			if !bytes.Equal(got, sentinel) {
				t.Errorf("canary %s was rewritten:\n%s", f, got)
			}
		}
		var extra []string
		_ = filepath.Walk(home, func(path string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() && path != files[0] && path != files[1] {
				extra = append(extra, path)
			}
			return nil
		})
		if len(extra) > 0 {
			t.Errorf("files appeared under the canary home: %v", extra)
		}
	}
}

func TestClaudeStateRefusesTheRealUserHome(t *testing.T) {
	home, untouched := canaryRealHome(t)
	project := filepath.Join(t.TempDir(), "city")

	if err := EnsureClaudeStateFile(home); err == nil || !strings.Contains(err.Error(), "real user home") {
		t.Errorf("EnsureClaudeStateFile(real home) = %v, want a refusal naming the real user home", err)
	}
	env := NewEnv("", t.TempDir(), t.TempDir()).With("HOME", home).Without("CLAUDE_CONFIG_DIR")
	if err := EnsureClaudeProjectState(env, project); err == nil || !strings.Contains(err.Error(), "real user home") {
		t.Errorf("EnsureClaudeProjectState(HOME=real home) = %v, want a refusal naming the real user home", err)
	}
	// A config dir under the real home is refused the same way.
	env = env.Clone().With("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	if err := EnsureClaudeProjectState(env, project); err == nil {
		t.Errorf("EnsureClaudeProjectState(CLAUDE_CONFIG_DIR under the real home) succeeded, want a refusal")
	}
	// The Env's own HOME is not what decides: a HOME elsewhere with the
	// default config dir is fine, and a lookalike path is not "under" home.
	if underRealUserHome(home + "-sibling") {
		t.Errorf("%s-sibling reads as under the real home %s", home, home)
	}
	untouched()
}

// A real-home HOME next to an isolated CLAUDE_CONFIG_DIR — tier C's shape —
// seeds the config dir only, which is the file Claude reads when the dir is
// set.
func TestClaudeStateSeedsOnlyTheIsolatedConfigDirBesideTheRealHome(t *testing.T) {
	home, untouched := canaryRealHome(t)
	configDir := filepath.Join(t.TempDir(), "claude")
	project := filepath.Join(t.TempDir(), "city")

	if err := EnsureClaudeStateFile(home, configDir); err != nil {
		t.Fatalf("EnsureClaudeStateFile(real home, isolated config dir): %v", err)
	}
	env := NewEnv("", t.TempDir(), t.TempDir()).With("HOME", home).With("CLAUDE_CONFIG_DIR", configDir)
	if err := EnsureClaudeProjectState(env, project); err != nil {
		t.Fatalf("EnsureClaudeProjectState(real home, isolated config dir): %v", err)
	}
	state := readClaudeStateForTest(t, filepath.Join(configDir, ".claude.json"))
	projects, _ := state["projects"].(map[string]any)
	if _, ok := projects[project]; !ok {
		t.Errorf("isolated config dir state has no trust entry for %s: %#v", project, state)
	}
	untouched()
}

// The default acceptance Env never reaches the real home at all.
func TestNewEnvClaudeStateStaysInTheIsolatedHome(t *testing.T) {
	_, untouched := canaryRealHome(t)
	env := NewEnv("", t.TempDir(), t.TempDir()).Without("CLAUDE_CONFIG_DIR")
	if err := EnsureClaudeProjectState(env, filepath.Join(t.TempDir(), "city")); err != nil {
		t.Fatalf("EnsureClaudeProjectState on the default Env: %v", err)
	}
	untouched()
}

// devBox clears both host-Claude opt-ins, so a test runs as on a developer
// box whatever CI runner it is on.
func devBox(t *testing.T) {
	t.Helper()
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv(EnvAllowHostClaude, "")
}

// On a developer box WithHostClaudeState does not open the real home: the
// refusal stands and names the opt-in.
func TestWithHostClaudeStateIsRefusedOnADevBox(t *testing.T) {
	devBox(t)
	home, untouched := canaryRealHome(t)
	env := NewEnv("", t.TempDir(), t.TempDir()).With("HOME", home).Without("CLAUDE_CONFIG_DIR").WithHostClaudeState()
	err := EnsureClaudeProjectState(env, filepath.Join(t.TempDir(), "city"))
	if err == nil || !strings.Contains(err.Error(), EnvAllowHostClaude) {
		t.Fatalf("EnsureClaudeProjectState(WithHostClaudeState) on a dev box = %v, want a refusal naming %s", err, EnvAllowHostClaude)
	}
	if HostClaudeStateAllowed() {
		t.Fatal("HostClaudeStateAllowed() with neither opt-in set")
	}
	untouched()
}

// On a CI runner (GITHUB_ACTIONS=true), or with GC_TEST_ALLOW_HOST_CLAUDE=1,
// an Env that asked may write the real home — and one that did not ask still
// may not, nor may a Clone of it.
func TestWithHostClaudeStateIsAllowedOnCIOrExplicitOptIn(t *testing.T) {
	for _, tc := range []struct{ name, key, val string }{
		{"github-actions", "GITHUB_ACTIONS", "true"},
		{"explicit-opt-in", EnvAllowHostClaude, "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			devBox(t)
			t.Setenv(tc.key, tc.val)
			if !HostClaudeStateAllowed() {
				t.Fatalf("HostClaudeStateAllowed() with %s=%s", tc.key, tc.val)
			}
			home, _ := canaryRealHome(t)
			project := filepath.Join(t.TempDir(), "city")
			base := NewEnv("", t.TempDir(), t.TempDir()).With("HOME", home).Without("CLAUDE_CONFIG_DIR")
			if err := EnsureClaudeProjectState(base.Clone(), project); err == nil {
				t.Fatalf("an Env without WithHostClaudeState wrote the real home")
			}
			if err := EnsureClaudeProjectState(base.Clone().WithHostClaudeState(), project); err != nil {
				t.Fatalf("EnsureClaudeProjectState with WithHostClaudeState: %v", err)
			}
			state := readClaudeStateForTest(t, filepath.Join(home, ".claude.json"))
			if projects, _ := state["projects"].(map[string]any); projects[project] == nil {
				t.Errorf("opted-in write did not seed %s: %#v", project, state)
			}
		})
	}
	// Anything other than the exact spellings is not an opt-in.
	devBox(t)
	t.Setenv("GITHUB_ACTIONS", "1")
	t.Setenv(EnvAllowHostClaude, "true")
	if HostClaudeStateAllowed() {
		t.Errorf("HostClaudeStateAllowed() accepted GITHUB_ACTIONS=1 / %s=true", EnvAllowHostClaude)
	}
}

// StageClaudeAuthHome copies only the CLI's credentials out of the real home,
// leaves the real home byte-identical, and refuses to stage into it.
func TestStageClaudeAuthHomeCopiesOnlyTheCredentials(t *testing.T) {
	hostHome := t.TempDir()
	prev := realUserHome
	realUserHome = func() string { return hostHome }
	t.Cleanup(func() { realUserHome = prev })

	creds := []byte(`{"claudeAiOauth":{"accessToken":"a","refreshToken":"r"}}`)
	state := []byte(`{"oauthAccount":{"emailAddress":"dev@example.com"},"userID":"u1","projects":{"/home/dev/secret":{"hasTrustDialogAccepted":true}},"history":["x"]}`)
	if err := os.MkdirAll(filepath.Join(hostHome, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostHome, ".claude", ".credentials.json"), creds, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostHome, ".claude.json"), state, 0o600); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "home")
	found, err := StageClaudeAuthHome(hostHome, dst)
	if err != nil || !found {
		t.Fatalf("StageClaudeAuthHome = %v, %v; want found, nil", found, err)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, ".claude", ".credentials.json")); !bytes.Equal(got, creds) {
		t.Errorf("staged credentials = %s, want %s", got, creds)
	}
	staged := readClaudeStateForTest(t, filepath.Join(dst, ".claude.json"))
	if staged["oauthAccount"] == nil || staged["userID"] != "u1" {
		t.Errorf("staged state lacks the auth fields: %#v", staged)
	}
	for _, private := range []string{"projects", "history"} {
		if _, ok := staged[private]; ok {
			t.Errorf("staged state carries the operator's %q", private)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(hostHome, ".claude.json")); !bytes.Equal(got, state) {
		t.Errorf("the real ~/.claude.json changed: %s", got)
	}
	if got, _ := os.ReadFile(filepath.Join(hostHome, ".claude", ".credentials.json")); !bytes.Equal(got, creds) {
		t.Errorf("the real credentials changed: %s", got)
	}

	if _, err := StageClaudeAuthHome(hostHome, filepath.Join(hostHome, "nested")); err == nil {
		t.Error("StageClaudeAuthHome staged into the real home")
	}
	empty := t.TempDir()
	if found, err := StageClaudeAuthHome(empty, filepath.Join(t.TempDir(), "home")); err != nil || found {
		t.Errorf("StageClaudeAuthHome(no credentials) = %v, %v; want not found, nil", found, err)
	}
}
