package acceptancehelpers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewEnvInheritsClaudeGatewayVariables(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "synthetic-token")
	t.Setenv("ANTHROPIC_BASE_URL", "https://api.synthetic.new/anthropic")
	t.Setenv("ANTHROPIC_DEFAULT_HAIKU_MODEL", "hf:zai-org/GLM-4.7-Flash")
	t.Setenv("ANTHROPIC_DEFAULT_SONNET_MODEL", "hf:moonshotai/Kimi-K2.5")
	t.Setenv("ANTHROPIC_DEFAULT_OPUS_MODEL", "hf:moonshotai/Kimi-K2.5")
	t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "hf:moonshotai/Kimi-K2.5")
	t.Setenv("CLAUDE_CODE_EFFORT_LEVEL", "auto")
	t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1")

	env := NewEnv("", t.TempDir(), t.TempDir())

	for key, want := range map[string]string{
		"ANTHROPIC_AUTH_TOKEN":                     "synthetic-token",
		"ANTHROPIC_BASE_URL":                       "https://api.synthetic.new/anthropic",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":            "hf:zai-org/GLM-4.7-Flash",
		"ANTHROPIC_DEFAULT_SONNET_MODEL":           "hf:moonshotai/Kimi-K2.5",
		"ANTHROPIC_DEFAULT_OPUS_MODEL":             "hf:moonshotai/Kimi-K2.5",
		"CLAUDE_CODE_SUBAGENT_MODEL":               "hf:moonshotai/Kimi-K2.5",
		"CLAUDE_CODE_EFFORT_LEVEL":                 "auto",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
	} {
		if got := env.Get(key); got != want {
			t.Fatalf("NewEnv() %s = %q, want %q", key, got, want)
		}
	}
}

func TestNewEnvDefaultsBeadsProviderToFile(t *testing.T) {
	t.Setenv("GC_ACCEPTANCE_BEADS_PROVIDER", "")

	env := NewEnv("", t.TempDir(), t.TempDir())

	if got := env.Get("GC_BEADS"); got != "file" {
		t.Fatalf("NewEnv() GC_BEADS = %q, want %q", got, "file")
	}
}

func TestNewEnvUsesAcceptanceBeadsProviderOverride(t *testing.T) {
	t.Setenv("GC_ACCEPTANCE_BEADS_PROVIDER", "sqlite")

	env := NewEnv("", t.TempDir(), t.TempDir())

	if got := env.Get("GC_BEADS"); got != "sqlite" {
		t.Fatalf("NewEnv() GC_BEADS = %q, want %q", got, "sqlite")
	}
}

// Tier A shapes that drop GC_DOLT=skip reach gc's Dolt author-identity
// preflight, which shells `dolt config --global --get` and has no fallback to
// any other config source. Before this was seeded the suite borrowed whatever
// identity the host happened to have, so it passed on a developer box and
// blocked `gc init` outright on a fresh CI runner.
func TestNewEnvSeedsDoltAuthorIdentity(t *testing.T) {
	gcHome := t.TempDir()

	env := NewEnv("", gcHome, t.TempDir())

	if got := env.Get("DOLT_ROOT_PATH"); got != gcHome {
		t.Fatalf("NewEnv() DOLT_ROOT_PATH = %q, want %q", got, gcHome)
	}
	body, err := os.ReadFile(filepath.Join(gcHome, ".dolt", "config_global.json"))
	if err != nil {
		t.Fatalf("reading seeded dolt config: %v", err)
	}
	for _, want := range []string{"user.name", "user.email"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("seeded dolt config is missing %q: %s", want, body)
		}
	}
}

// With no seed from the caller — a bare `go test -tags acceptance_a`, which is
// what the CI topology job runs — NewEnv must supply its own. Without it the
// child read the runner's real global config and gc doctor errored on
// beads-role.
func TestNewEnvSeedsGitConfigWhenTheCallerSuppliesNone(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "")
	gcHome := t.TempDir()

	env := NewEnv("", gcHome, t.TempDir())

	seed := env.Get("GIT_CONFIG_GLOBAL")
	if seed == "" {
		t.Fatal("NewEnv() left GIT_CONFIG_GLOBAL empty; the child would read the host's global config")
	}
	body, err := os.ReadFile(seed)
	if err != nil {
		t.Fatalf("reading seeded git config: %v", err)
	}
	if !strings.Contains(string(body), "role = maintainer") {
		t.Errorf("seeded git config is missing beads.role: %s", body)
	}
	if got := env.Get("GIT_CONFIG_NOSYSTEM"); got != "1" {
		t.Errorf("NewEnv() GIT_CONFIG_NOSYSTEM = %q, want %q", got, "1")
	}
}

// The Makefile points these at a seeded global gitconfig carrying
// beads.role=maintainer. Dropping them sent the child at the host's real
// global config, and `gc doctor` then failed its beads-role check.
func TestNewEnvForwardsSeededGitConfig(t *testing.T) {
	seed := filepath.Join(t.TempDir(), "gitconfig")
	t.Setenv("GIT_CONFIG_GLOBAL", seed)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	env := NewEnv("", t.TempDir(), t.TempDir())

	if got := env.Get("GIT_CONFIG_GLOBAL"); got != seed {
		t.Fatalf("NewEnv() GIT_CONFIG_GLOBAL = %q, want %q", got, seed)
	}
	if got := env.Get("GIT_CONFIG_NOSYSTEM"); got != "1" {
		t.Fatalf("NewEnv() GIT_CONFIG_NOSYSTEM = %q, want %q", got, "1")
	}
}

// unwritableHomeForTest returns a HOME nothing can be created in, whoever runs
// the test: its parent is a regular file. Permission bits would not bind root,
// and only a mount makes a real directory read-only.
func unwritableHomeForTest(t *testing.T) string {
	t.Helper()
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatalf("writing %s: %v", blocker, err)
	}
	return filepath.Join(blocker, "home")
}

// Bazel's local sandbox, which is where a fork PR's CI job runs (a fork has no
// remote executor), mounts the runner's real HOME read-only. gc still needs that
// HOME — the platform supervisor refuses any other — so it is the Claude state
// the tests seed that has to move, into a CLAUDE_CONFIG_DIR the Env owns.
func TestNewCityWithUnwritableHomeSeedsClaudeStateUnderGCHome(t *testing.T) {
	home := unwritableHomeForTest(t)
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	gcHome := t.TempDir()

	env := NewEnv("", gcHome, t.TempDir())
	city := NewCity(t, env)

	if got := env.Get("HOME"); got != home {
		t.Errorf("NewEnv() HOME = %q, want the ambient %q untouched (gc's supervisor refuses any other)", got, home)
	}
	configDir := env.Get("CLAUDE_CONFIG_DIR")
	if !strings.HasPrefix(configDir, gcHome+string(filepath.Separator)) {
		t.Fatalf("NewEnv() CLAUDE_CONFIG_DIR = %q, want a directory under GC_HOME %q", configDir, gcHome)
	}
	assertClaudeProjectTrustedForTest(t, filepath.Join(configDir, ".claude.json"), city.Dir, nil, nil)
}

// A CLAUDE_CONFIG_DIR from the host carries the operator's credentials, so an
// unwritable HOME must not replace it.
func TestNewCityWithUnwritableHomeKeepsInheritedClaudeConfigDir(t *testing.T) {
	t.Setenv("HOME", unwritableHomeForTest(t))
	inherited := filepath.Join(t.TempDir(), "claude")
	t.Setenv("CLAUDE_CONFIG_DIR", inherited)

	env := NewEnv("", t.TempDir(), t.TempDir())
	city := NewCity(t, env)

	if got := env.Get("CLAUDE_CONFIG_DIR"); got != inherited {
		t.Fatalf("NewEnv() CLAUDE_CONFIG_DIR = %q, want the inherited %q", got, inherited)
	}
	assertClaudeProjectTrustedForTest(t, filepath.Join(inherited, ".claude.json"), city.Dir, nil, nil)
}

// The fallback is for a HOME that cannot be written and nothing else: the tiers
// that run a real Claude keep the operator's own state and credentials.
func TestNewCityWithWritableHomeSeedsClaudeStateUnderHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	env := NewEnv("", t.TempDir(), t.TempDir())
	city := NewCity(t, env)

	if got := env.Get("CLAUDE_CONFIG_DIR"); got != "" {
		t.Fatalf("NewEnv() CLAUDE_CONFIG_DIR = %q, want it left unset while HOME is writable", got)
	}
	assertClaudeProjectTrustedForTest(t, filepath.Join(home, ".claude.json"), city.Dir, nil, nil)
}
