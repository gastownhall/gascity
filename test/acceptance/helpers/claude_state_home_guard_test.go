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

// WithHostClaudeState is the one explicit opt-in, and it is not inherited by
// accident: only an Env (or a Clone of one) that called it may write there.
func TestWithHostClaudeStateIsTheOnlyWayIntoTheRealHome(t *testing.T) {
	home, _ := canaryRealHome(t)
	project := filepath.Join(t.TempDir(), "city")
	base := NewEnv("", t.TempDir(), t.TempDir()).With("HOME", home).Without("CLAUDE_CONFIG_DIR")
	if err := EnsureClaudeProjectState(base.Clone(), project); err == nil {
		t.Fatalf("a Clone of an Env without the opt-in wrote the real home")
	}
	if err := EnsureClaudeProjectState(base.Clone().WithHostClaudeState(), project); err != nil {
		t.Fatalf("EnsureClaudeProjectState with WithHostClaudeState: %v", err)
	}
	state := readClaudeStateForTest(t, filepath.Join(home, ".claude.json"))
	if projects, _ := state["projects"].(map[string]any); projects[project] == nil {
		t.Errorf("opted-in write did not seed %s: %#v", project, state)
	}
}
