package acceptancehelpers

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
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
	// Without metrics.disabled every host dolt the suite runs makes a
	// best-effort send-metrics call to eventsapi.dolthub.com.
	if !strings.Contains(string(body), `"metrics.disabled":"true"`) {
		t.Errorf("seeded dolt config does not disable dolt usage metrics: %s", body)
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

// hostHomeWithBdSharedServer makes the test process look like a developer box
// running a bd shared server: HOME holds ~/.beads/shared-server with a
// database named like the acceptance city (the state that turned
// TestBeadsProxiedDefaultInit/doctor-green red on such a host). It returns that
// home.
func hostHomeWithBdSharedServer(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, dir := range []string{
		filepath.Join(home, ".beads", "shared-server", "dolt", "hq"),
		filepath.Join(home, ".config", "bd"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, ".beads", "config.yaml"), []byte("dolt:\n  shared-server: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	return home
}

// passwdHomeForTest returns the invoking user's passwd home, or "" when the
// lookup has none.
func passwdHomeForTest(t *testing.T) string {
	t.Helper()
	lu, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(lu.HomeDir)
}

// The acceptance env must never hand gc the operator's home, in any mode: gc
// doctor (and every other in-process HOME read in gc) would otherwise report on
// the host — e.g. a bd shared server under ~/.beads/shared-server — and a cached
// PASS would depend on which machine ran it. Under Bazel the action HOME is
// TEST_TMPDIR, which the env used to swap for the passwd home.
func TestNewEnvIsolatesHomeFromTheHost(t *testing.T) {
	for _, mode := range []string{"go-test", "bazel"} {
		t.Run(mode, func(t *testing.T) {
			hostHome := hostHomeWithBdSharedServer(t)
			if mode == "bazel" {
				t.Setenv("TEST_TMPDIR", hostHome)
			} else {
				t.Setenv("TEST_TMPDIR", "")
			}
			gcHome := t.TempDir()

			env := NewEnv("", gcHome, t.TempDir())

			home := env.Get("HOME")
			if home == "" {
				t.Fatal("NewEnv() HOME is empty")
			}
			for _, host := range []string{hostHome, passwdHomeForTest(t)} {
				if host != "" && filepath.Clean(home) == filepath.Clean(host) {
					t.Fatalf("NewEnv() HOME = %q, the host's home; want an isolated home under GC_HOME %q", home, gcHome)
				}
			}
			if !strings.HasPrefix(home, gcHome+string(filepath.Separator)) {
				t.Fatalf("NewEnv() HOME = %q, want a directory under GC_HOME %q", home, gcHome)
			}
			if !dirWritable(home) {
				t.Fatalf("NewEnv() HOME %q is not a writable directory", home)
			}
			if _, err := os.Stat(filepath.Join(home, ".beads")); !os.IsNotExist(err) {
				t.Fatalf("the host's ~/.beads is visible through the isolated HOME %q (stat err=%v)", home, err)
			}
			for _, kv := range env.List() {
				key, value, _ := strings.Cut(kv, "=")
				if key != "PATH" && strings.Contains(value, hostHome) {
					t.Errorf("NewEnv() %s = %q names the host home %q", key, value, hostHome)
				}
			}
			if got := env.Get("GC_SUPERVISOR_ISOLATED_HOME"); got != "1" {
				t.Errorf("NewEnv() GC_SUPERVISOR_ISOLATED_HOME = %q, want \"1\" so gc bare-starts its supervisor under the isolated HOME", got)
			}
		})
	}
}

// The tiers that drive a real provider CLI authenticate through the operator's
// home and opt back into it explicitly.
func TestEnvWithHostHomeRestoresTheHostHome(t *testing.T) {
	hostHome := hostHomeWithBdSharedServer(t)

	env := NewEnv("", t.TempDir(), t.TempDir()).WithHostHome()

	if got := env.Get("HOME"); got != hostHome {
		t.Fatalf("WithHostHome() HOME = %q, want the host's %q", got, hostHome)
	}
}

// Claude state the tests seed lands in the isolated HOME, never the host's.
func TestNewCitySeedsClaudeStateUnderIsolatedHome(t *testing.T) {
	hostHome := hostHomeWithBdSharedServer(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	env := NewEnv("", t.TempDir(), t.TempDir())
	city := NewCity(t, env)

	if got := env.Get("CLAUDE_CONFIG_DIR"); got != "" {
		t.Fatalf("NewEnv() CLAUDE_CONFIG_DIR = %q, want it unset: the isolated HOME is writable", got)
	}
	assertClaudeProjectTrustedForTest(t, filepath.Join(env.Get("HOME"), ".claude.json"), city.Dir, nil, nil)
	if _, err := os.Stat(filepath.Join(hostHome, ".claude.json")); !os.IsNotExist(err) {
		t.Fatalf("Claude state was written into the host home (stat err=%v)", err)
	}
}

// A CLAUDE_CONFIG_DIR from the host carries the operator's credentials, so the
// env keeps it.
func TestNewCityKeepsInheritedClaudeConfigDir(t *testing.T) {
	hostHomeWithBdSharedServer(t)
	inherited := filepath.Join(t.TempDir(), "claude")
	t.Setenv("CLAUDE_CONFIG_DIR", inherited)

	env := NewEnv("", t.TempDir(), t.TempDir())
	city := NewCity(t, env)

	if got := env.Get("CLAUDE_CONFIG_DIR"); got != inherited {
		t.Fatalf("NewEnv() CLAUDE_CONFIG_DIR = %q, want the inherited %q", got, inherited)
	}
	assertClaudeProjectTrustedForTest(t, filepath.Join(inherited, ".claude.json"), city.Dir, nil, nil)
}
