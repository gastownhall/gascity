package acceptancehelpers

import (
	"bytes"
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
	// Without versioncheck.disabled `dolt version` fetches the latest release
	// over HTTPS before it prints, and gc's and bd's version probes time out on
	// a slow network.
	if !strings.Contains(string(body), `"versioncheck.disabled":"true"`) {
		t.Errorf("seeded dolt config does not disable dolt's release version check: %s", body)
	}
}

// NewEnv hands a child only the variables it lists, so the process-wide
// DOLT_DISABLE_EVENT_FLUSH that internal/testenv sets never reaches it. Without
// its own entry every dolt a tier A, B or C test forks leaves a detached
// `dolt send-metrics` behind, even though the seeded config sets
// metrics.disabled.
func TestNewEnvDisablesDoltEventFlush(t *testing.T) {
	env := NewEnv("", t.TempDir(), t.TempDir())

	if got := env.Get("DOLT_DISABLE_EVENT_FLUSH"); got != "1" {
		t.Fatalf("NewEnv() DOLT_DISABLE_EVENT_FLUSH = %q, want %q", got, "1")
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

// TestNewEnvForwardsGitTemplateDir pins the host-env allowlist entry for the
// hermetic git template internal/testenv installs. NewEnv builds the child's
// environment from an allowlist, so a gc or git the child runs makes repos
// carrying maintenance.auto=false only if GIT_TEMPLATE_DIR is on it.
func TestNewEnvForwardsGitTemplateDir(t *testing.T) {
	template := filepath.Join(t.TempDir(), "gc-test-gittemplate")
	t.Setenv("GIT_TEMPLATE_DIR", template)

	env := NewEnv("", t.TempDir(), t.TempDir())

	if got := env.Get("GIT_TEMPLATE_DIR"); got != template {
		t.Fatalf("NewEnv() GIT_TEMPLATE_DIR = %q, want %q", got, template)
	}
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

// gc's exec orders and pack scripts run bare `gc` through PATH, so the env
// built from BuildGC's override must resolve that gc to the binary under test.
func TestVerifyPathGCAcceptsTheEnvBuiltFromBuildGC(t *testing.T) {
	t.Setenv("GC_ACCEPTANCE_GC_BIN", writeFakeGC(t, filepath.Join(t.TempDir(), "gc-external")))

	env := NewEnv(BuildGC(t.TempDir()), t.TempDir(), t.TempDir())

	if err := VerifyPathGC(env); err != nil {
		t.Fatalf("VerifyPathGC() = %v, want nil", err)
	}
}

func TestVerifyPathGCRefusesAPathWhoseGCIsNotTheBinaryUnderTest(t *testing.T) {
	unnamed := writeFakeGC(t, filepath.Join(t.TempDir(), "gc-external"))
	underTest := writeFakeGC(t, filepath.Join(t.TempDir(), "gc"))
	host := writeFakeGC(t, filepath.Join(t.TempDir(), "gc"))
	for name, vars := range map[string]map[string]string{
		// Exec orders exit 127: only the binary's own directory is on PATH,
		// and the binary is not named gc.
		"no gc on PATH": {"GC_ACCEPTANCE_GC_BIN": unnamed, "PATH": filepath.Dir(unnamed)},
		// Exec orders test the host's gc while RunGC tests the binary.
		"another gc first": {
			"GC_ACCEPTANCE_GC_BIN": underTest,
			"PATH":                 filepath.Dir(host) + string(os.PathListSeparator) + filepath.Dir(underTest),
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := VerifyPathGC(&Env{vars: vars})
			if err == nil {
				t.Fatal("VerifyPathGC() = nil, want an error")
			}
			if !strings.Contains(err.Error(), vars["GC_ACCEPTANCE_GC_BIN"]) {
				t.Fatalf("VerifyPathGC() = %v, want it to name the binary under test %s", err, vars["GC_ACCEPTANCE_GC_BIN"])
			}
		})
	}
}

// A split city's gc bd forwards only to bd's own Go binary, so WithRealBD must
// put that binary ahead of the tool-home wrapper both as BD_BIN and on PATH.
func TestWithRealBDLeadsPathAndPinsBDBin(t *testing.T) {
	bd := filepath.Join(t.TempDir(), "bd-real")
	if err := os.WriteFile(bd, []byte("binary"), 0o755); err != nil { //nolint:gosec // stands in for an executable
		t.Fatal(err)
	}
	env := NewEnv("", t.TempDir(), t.TempDir()).WithRealBD(bd)

	if got := env.Get("BD_BIN"); got != bd {
		t.Fatalf("BD_BIN = %q, want %q", got, bd)
	}
	assertFirstPathBDIs(t, env, bd)
	dir := filepath.Dir(findInPath(env.Get("PATH"), "bd"))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("the real-bd directory %s holds %d entries, want only the bd link (anything else would shadow host tools)", dir, len(entries))
	}
}

// Tier A's hermetic provider doubles (GC_HOME/bin) must lead PATH; the real bd
// goes behind them, ahead of every other bd.
func TestNewEnvKeepsTheProviderDoublesFirst(t *testing.T) {
	bd := writeFakeBD(t)
	t.Setenv("GC_ACCEPTANCE_BD_BIN", bd)
	gcHome := t.TempDir()

	env := NewEnv("", gcHome, t.TempDir())

	if first := filepath.SplitList(env.Get("PATH"))[0]; first != filepath.Join(gcHome, "bin") {
		t.Fatalf("PATH starts with %s, want the provider doubles directory %s", first, filepath.Join(gcHome, "bin"))
	}
	assertFirstPathBDIs(t, env, bd)
}

// An unwrapped bd resolves user-level state under gc's HOME, so WithRealBD
// refuses an Env whose gc HOME is anything but NewEnv's isolated one — the
// operator's own (WithHostHome) above all.
func TestWithRealBDRefusesAHomeThatIsNotTheIsolatedOne(t *testing.T) {
	env := NewEnv("", t.TempDir(), t.TempDir()).With("HOME", t.TempDir())
	defer func() {
		if recover() == nil {
			t.Fatal("WithRealBD accepted an Env whose gc HOME is not NewEnv's isolated home")
		}
	}()
	env.WithRealBD(filepath.Join(t.TempDir(), "bd"))
}

func TestWithRealBDRefusesARelativePath(t *testing.T) {
	env := NewEnv("", t.TempDir(), t.TempDir())
	defer func() {
		if recover() == nil {
			t.Fatal("WithRealBD accepted a relative bd path, which gc ignores as BD_BIN")
		}
	}()
	env.WithRealBD("bin/bd")
}

// gc carries its environment into a bd the tool-home wrapper does not re-home
// (WithRealBD, a raw BD_BIN), so NewEnv pins both of the wrapper's bd switches.
func TestNewEnvPinsTheWrapperBdSwitches(t *testing.T) {
	env := NewEnv("", t.TempDir(), t.TempDir())
	for key, want := range map[string]string{bdSharedServerConfigEnv: "false", bdDisableMetricsEnv: "1"} {
		if got := env.Get(key); got != want {
			t.Errorf("NewEnv() %s = %q, want %q", key, got, want)
		}
	}
}

// writeFakeBD stands in for a real bd binary at an absolute path.
func writeFakeBD(t *testing.T) string {
	t.Helper()
	bd := filepath.Join(t.TempDir(), "bd-real")
	if err := os.WriteFile(bd, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // stands in for an executable
		t.Fatal(err)
	}
	return bd
}

// assertFirstPathBDIs fails unless the first bd on env's PATH resolves to want.
func assertFirstPathBDIs(t *testing.T, env *Env, want string) {
	t.Helper()
	found := findInPath(env.Get("PATH"), "bd")
	if found == "" {
		t.Fatalf("no bd on PATH %q", env.Get("PATH"))
	}
	got, err := filepath.EvalSymlinks(found)
	if err != nil {
		t.Fatal(err)
	}
	wantResolved, err := filepath.EvalSymlinks(want)
	if err != nil {
		t.Fatal(err)
	}
	if got != wantResolved {
		t.Fatalf("the first bd on PATH (%s) resolves to %s, want %s", found, got, wantResolved)
	}
}

// assertWrappedBD fails unless env runs bd through the tool-home wrapper: no
// BD_BIN, and the first bd on PATH is the wrapper script rather than bd itself.
func assertWrappedBD(t *testing.T, env *Env, bd string) {
	t.Helper()
	if got := env.Get("BD_BIN"); got != "" {
		t.Fatalf("BD_BIN = %q, want it unset so gc resolves the tool-home wrapper", got)
	}
	if env.RunsRealBD() {
		t.Fatal("RunsRealBD() = true, want false")
	}
	found := findInPath(env.Get("PATH"), "bd")
	if found == "" {
		t.Fatalf("no bd on PATH %q", env.Get("PATH"))
	}
	resolved, err := filepath.EvalSymlinks(found)
	if err != nil {
		t.Fatal(err)
	}
	if resolved == bd {
		t.Fatalf("the first bd on PATH resolves to the real bd %s, want the tool-home wrapper", bd)
	}
	body, err := os.ReadFile(found)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(body), "#!") || !strings.Contains(string(body), "bd-tool-home") {
		t.Fatalf("the first bd on PATH (%s) is not the tool-home wrapper:\n%s", found, body)
	}
}

// Split cities refuse the wrapper, so every Env runs the real bd unless a test
// asks for the wrapper (A2a).
func TestNewEnvRunsTheRealBDByDefault(t *testing.T) {
	bd := writeFakeBD(t)
	t.Setenv("GC_ACCEPTANCE_BD_BIN", bd)

	env := NewEnv("", t.TempDir(), t.TempDir())

	if got := env.Get("BD_BIN"); got != bd {
		t.Fatalf("BD_BIN = %q, want the real bd %q", got, bd)
	}
	if !env.RunsRealBD() {
		t.Fatal("RunsRealBD() = false, want true")
	}
	assertFirstPathBDIs(t, env, bd)
}

func TestNewEnvWithoutABDPinsNoBDBin(t *testing.T) {
	t.Setenv("GC_ACCEPTANCE_BD_BIN", filepath.Join(t.TempDir(), "missing-bd"))

	env := NewEnv("", t.TempDir(), t.TempDir())

	if got := env.Get("BD_BIN"); got != "" {
		t.Fatalf("BD_BIN = %q, want it unset when no bd is configured", got)
	}
	if env.RunsRealBD() {
		t.Fatal("RunsRealBD() = true with no bd configured")
	}
}

func TestWithWrappedBDRestoresTheToolHomeWrapper(t *testing.T) {
	bd := writeFakeBD(t)
	t.Setenv("GC_ACCEPTANCE_BD_BIN", bd)

	env := NewEnv("", t.TempDir(), t.TempDir()).WithWrappedBD()

	assertWrappedBD(t, env, bd)
	for _, dir := range filepath.SplitList(env.Get("PATH")) {
		if strings.Contains(dir, realBDDirName) {
			t.Fatalf("PATH still holds the real-bd directory %s", dir)
		}
	}
}

// An unwrapped bd resolves user-level state under gc's HOME, so any HOME but
// NewEnv's isolated one puts the Env back on the wrapper.
func TestEnvLeavesTheRealBDWhenGCLeavesTheIsolatedHome(t *testing.T) {
	for name, apply := range map[string]func(*Env) *Env{
		"WithHostHome": func(e *Env) *Env { return e.WithHostHome() },
		"With HOME":    func(e *Env) *Env { return e.With("HOME", t.TempDir()) },
		"Without HOME": func(e *Env) *Env { return e.Without("HOME") },
	} {
		t.Run(name, func(t *testing.T) {
			hostHomeWithBdSharedServer(t)
			bd := writeFakeBD(t)
			t.Setenv("GC_ACCEPTANCE_BD_BIN", bd)

			env := apply(NewEnv("", t.TempDir(), t.TempDir()))

			assertWrappedBD(t, env, bd)
		})
	}
}

func TestEnvWithTheIsolatedHomeKeepsTheRealBD(t *testing.T) {
	bd := writeFakeBD(t)
	t.Setenv("GC_ACCEPTANCE_BD_BIN", bd)
	env := NewEnv("", t.TempDir(), t.TempDir())

	env.With("HOME", env.Get("HOME"))

	if !env.RunsRealBD() || env.Get("BD_BIN") != bd {
		t.Fatalf("re-setting the isolated HOME dropped the real bd (BD_BIN=%q)", env.Get("BD_BIN"))
	}
}

func TestCloneKeepsTheBDMode(t *testing.T) {
	bd := writeFakeBD(t)
	t.Setenv("GC_ACCEPTANCE_BD_BIN", bd)
	env := NewEnv("", t.TempDir(), t.TempDir())

	if !env.Clone().RunsRealBD() {
		t.Fatal("Clone() of a real-bd Env runs the wrapper")
	}
	if env.Clone().WithWrappedBD(); !env.RunsRealBD() {
		t.Fatal("WithWrappedBD on a clone changed the original Env")
	}
}

// WithRealBD on an Env that already runs a real bd, or twice in a row, leaves
// exactly one real-bd directory on PATH, leading it.
func TestWithRealBDReplacesAnEarlierRealBD(t *testing.T) {
	first := writeFakeBD(t)
	t.Setenv("GC_ACCEPTANCE_BD_BIN", first)
	second := writeFakeBD(t)

	env := NewEnv("", t.TempDir(), t.TempDir()).WithRealBD(second).WithRealBD(second)

	if got := env.Get("BD_BIN"); got != second {
		t.Fatalf("BD_BIN = %q, want %q", got, second)
	}
	assertFirstPathBDIs(t, env, second)
	count := 0
	for _, dir := range filepath.SplitList(env.Get("PATH")) {
		if strings.Contains(dir, realBDDirName) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("PATH holds %d real-bd directories, want 1: %s", count, env.Get("PATH"))
	}
}

// A topology on a default Env keeps the real bd its gc runs; one on a wrapped
// Env keeps the wrapper.
func TestTopologyEnvFollowsTheBaseBDMode(t *testing.T) {
	bd := writeFakeBD(t)
	t.Setenv("GC_ACCEPTANCE_BD_BIN", bd)

	realEnv := TopologyEnv(t, NewEnv("", t.TempDir(), t.TempDir()), t.TempDir(), bd, "/bin/true")
	if got := realEnv.Get("BD_BIN"); got != bd {
		t.Fatalf("TopologyEnv on a default Env: BD_BIN = %q, want %q", got, bd)
	}
	assertFirstPathBDIs(t, realEnv, bd)

	wrapped := TopologyEnv(t, NewEnv("", t.TempDir(), t.TempDir()).WithWrappedBD(), t.TempDir(), bd, "/bin/true")
	assertWrappedBD(t, wrapped, bd)
}

// The topology matrix's rig add recovers a proved lost backend port once
// (RigAddRecovery, #8171), and its only proof is the dolt log the tool-home
// wrapper prints after a failed bd init: gc rig add removes the rig's new
// .beads on the way out, and a real bd prints nothing. gc's provider script
// runs every bd init as "${BD_BIN:-bd}" init, so a topology shape must resolve
// the wrapper there.
func TestTopologyRunEnvKeepsTheRigAddRecoveryProof(t *testing.T) {
	bd, _ := fakeInitBD{
		status:    1,
		stderr:    "Error: failed to open uow provider: uow: ping db: dial tcp 127.0.0.1:43599: connect: connection refused\n",
		serverLog: strPtr("Starting server\n" + portConflictLine + "\n"),
	}.install(t)
	t.Setenv("GC_ACCEPTANCE_BD_BIN", bd)
	env := topologyRunEnv(t, NewEnv("", t.TempDir(), t.TempDir()), t.TempDir(), BeadsTopology{}, bd, "/bin/true")

	rig := filepath.Join(t.TempDir(), "rig")
	cmd := ToolCommand(t, "/bin/sh", "-c", `"${BD_BIN:-bd}" init --proxied-server`)
	cmd.Env = append(env.List(), "BEADS_DIR="+filepath.Join(rig, ".beads"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	initErr := cmd.Run()
	if initErr == nil {
		t.Fatal("the fake bd's init succeeded")
	}
	out := "gc rig add: provider-owned beads init: " + stderr.String()
	if _, err := portCollisionProof(out, initErr, rigRoots(rig)); err != nil {
		t.Fatalf("a topology shape's failed bd init leaves RigAddRecovery no proof (%v); BD_BIN=%q:\n%s", err, env.Get("BD_BIN"), out)
	}
}
