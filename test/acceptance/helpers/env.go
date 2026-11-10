package acceptancehelpers

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/testutil"
)

// supervisorIsolatedHomeEnv is gc's opt-in (cmd/gc supervisorIsolatedHomeEnv)
// for bare-starting its supervisor under a HOME that is not the passwd home.
const supervisorIsolatedHomeEnv = "GC_SUPERVISOR_ISOLATED_HOME"

// acceptanceGitConfig mirrors scripts/test-gitconfig-path. beads.role is the
// load-bearing key: gc doctor errors without it and gc-beads-bd's
// ensure_beads_role otherwise tries to write it to the host.
const acceptanceGitConfig = `[user]
	name = gc-test
	email = gc-test@test.local
[beads]
	role = maintainer
[safe]
	directory = *
`

// Env builds an isolated environment for acceptance tests.
// It filters the host environment to a safe allowlist, then layers
// test-specific overrides on top.
type Env struct {
	vars map[string]string
	// toolHome is the HOME every bd and dolt child of this Env gets in place
	// of the real one. See tool_home.go.
	toolHome string
	// hostClaudeState lets the Claude state seeding write under the real user
	// home. Only WithHostClaudeState sets it; see claude_state.go.
	hostClaudeState bool
	// realBD is the bd binary gc runs unwrapped (BD_BIN and the lead of PATH),
	// or "" when gc runs bd through the tool-home wrapper. NewEnv sets it to
	// the configured bd; WithWrappedBD, WithHostHome and any HOME other than
	// the isolated one, or none, clear it.
	realBD string
}

// realBDDirName is the directory under GC_HOME that holds the real-bd links
// WithRealBD puts on PATH, one subdirectory per bd binary.
const realBDDirName = "real-bd-bin"

// NewEnv creates an isolated environment with the minimum inherited
// variables (PATH, TMPDIR, locale, shell) plus test-specific overrides
// for GC_HOME, HOME and XDG_RUNTIME_DIR.
func NewEnv(gcBinary, gcHome, runtimeDir string) *Env {
	e := &Env{vars: make(map[string]string)}

	// Inherit minimum from host. HOME is not inherited: gc gets an isolated
	// one under GC_HOME (below), and bd and dolt the tool home (see
	// tool_home.go).
	for _, key := range []string{
		"PATH", "TMPDIR", "LANG", "LC_ALL", "USER",
		"SHELL", "SSH_AUTH_SOCK", "TERM",
		// The Makefile's TEST_ENV points these at scripts/test-gitconfig-path, a
		// seeded writable global gitconfig carrying user.name, user.email and
		// beads.role=maintainer. Dropping them sent the child's reads at the
		// runner's real global config, which has no beads.role, so `gc doctor`
		// failed its beads-role check on any host that had never opted in.
		"GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM",
		// internal/testenv points this at a template whose config turns off git's
		// detached auto-maintenance; the child carries it so the repos it creates
		// with git init or git clone have that off too (ga-zoe1wr).
		"GIT_TEMPLATE_DIR",
		"CLAUDE_CONFIG_DIR", // Claude Code reads OAuth credentials from here
		"ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_BASE_URL",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL",
		"ANTHROPIC_DEFAULT_OPUS_MODEL",
		"ANTHROPIC_DEFAULT_SONNET_MODEL",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC",
		"CLAUDE_CODE_EFFORT_LEVEL",
		"CLAUDE_CODE_SUBAGENT_MODEL",
		"OPENAI_API_KEY",
		"GEMINI_API_KEY",
		"GOOGLE_GENERATIVE_AI_API_KEY",
		"GOOGLE_API_KEY",
		"OLLAMA_API_KEY",
		"GOOGLE_APPLICATION_CREDENTIALS",
		"GOOGLE_CLOUD_PROJECT",
		"GOOGLE_CLOUD_PROJECT_ID",
		"GOOGLE_CLOUD_LOCATION",
	} {
		if v := os.Getenv(key); v != "" {
			e.vars[key] = v
		}
	}

	// bd and dolt children never see HOME; they resolve user-level state under
	// this tool home instead (see tool_home.go).
	e.toolHome = filepath.Join(gcHome, "tool-home")
	if err := os.MkdirAll(e.toolHome, 0o755); err != nil {
		panic(fmt.Sprintf("acceptance: creating bd/dolt tool home under %s: %v", gcHome, err))
	}
	// The configured bd is staged twice. Behind the tool-home wrapper, ahead of
	// every host copy, for WithWrappedBD and the host-home tiers; and as itself
	// (WithRealBD, applied once gc's HOME is isolated below), which is what gc
	// runs by default: a split city's gc bd refuses the wrapper. Suites that
	// stage their own bd (TopologyEnv, proxiedEnv, BD_BIN pins) put it earlier
	// still.
	bdPath := FindBD()
	if bdPath != "" {
		bdDir := filepath.Join(gcHome, "beads-bin")
		if _, err := InstallBeadsTooling(e, bdDir, bdPath, ""); err != nil {
			panic(fmt.Sprintf("acceptance: staging bd under %s: %v", gcHome, err))
		}
		e.vars["PATH"] = bdDir + ":" + e.vars["PATH"]
	}

	// Prepend gc binary dir to PATH.
	if gcBinary != "" {
		e.vars["GC_ACCEPTANCE_GC_BIN"] = gcBinary
		e.vars["PATH"] = filepath.Dir(gcBinary) + ":" + e.vars["PATH"]
	}

	// Prepend shims for the platform service managers so `gc init` never
	// hands the supervisor off to the real host launchd/systemd. The
	// shims exit non-zero, which causes ensureSupervisorRunning to fall
	// through to doSupervisorStart (bare fork). Without this on Mac,
	// launchctl load succeeds and launchd starts a supervisor that
	// doesn't inherit the test's isolation env vars, so the K8s session
	// provider fires for hyperscale and fails on missing kubeconfig.
	//
	// Panic on failure: silently dropping the shim would look like a
	// random hyperscale infra regression on Mac with no breadcrumb.
	shimDir, err := installServiceManagerShims(gcHome)
	if err != nil {
		panic(fmt.Sprintf("acceptance: installing service-manager shims under %s: %v", gcHome, err))
	}
	e.vars["PATH"] = shimDir + ":" + e.vars["PATH"]

	// gc gets a HOME of its own under GC_HOME, in every mode. Handing it the
	// operator's home made results depend on the host: gc reads user-level
	// state through HOME in-process (gc doctor's proxied-shared-server check
	// reports on ~/.beads/shared-server), so a host running a bd shared server
	// failed doctor-green while clean workers passed. The platform supervisor
	// refuses a HOME that differs from the passwd entry, so the env opts into
	// a bare-started supervisor under this HOME (GC_SUPERVISOR_ISOLATED_HOME);
	// gc never installs a launchd/systemd unit from it. Being writable, it
	// also holds the Claude state the tests seed (a CLAUDE_CONFIG_DIR from the
	// host still wins: it carries the operator's credentials).
	home := IsolatedHome(gcHome)
	if err := os.MkdirAll(home, 0o755); err != nil {
		panic(fmt.Sprintf("acceptance: creating isolated HOME under %s: %v", gcHome, err))
	}
	e.vars["HOME"] = home
	e.vars[supervisorIsolatedHomeEnv] = "1"
	e.vars["GC_HOME"] = gcHome

	// gc carries its environment into every bd it forks, including ones that
	// reach a bd not wrapped by the tool-home wrapper (a raw BD_BIN). Pin bd's
	// shared-server mode off there too; a test that exercises the user-level
	// layer on purpose sets it to "".
	e.vars[bdSharedServerConfigEnv] = "false"
	e.vars[bdDisableMetricsEnv] = "1"

	// Dolt reads its global config from $DOLT_ROOT_PATH/.dolt/config_global.json,
	// falling back to $HOME. gc's init preflight (checkDoltAuthorIdentity) probes
	// it with `dolt config --global --get` and has no fallback to any other
	// config source, so a shape that drops GC_DOLT=skip refuses to init on a
	// machine whose home never ran `dolt config --global`. Leaning on the host's
	// ambient identity made that a coin flip: green on a developer box, red on a
	// fresh CI runner. Seed under GC_HOME rather than the real home so a run
	// never writes host Dolt state; test/integration seeds the same config. It
	// also turns dolt's usage metrics and release version check off, so no dolt
	// the suite runs calls eventsapi.dolthub.com or stalls in `dolt version` on a
	// slow network.
	if err := testutil.SeedDoltGlobalConfig(gcHome); err != nil {
		panic(fmt.Sprintf("acceptance: %v", err))
	}
	e.vars["DOLT_ROOT_PATH"] = gcHome
	// That config does not stop dolt forking a detached `dolt send-metrics` after
	// each command; only the presence of this variable does. NewEnv hands a child
	// only the vars set here, so the one internal/testenv sets for the test
	// process never reaches it.
	e.vars["DOLT_DISABLE_EVENT_FLUSH"] = "1"

	// The Makefile points GIT_CONFIG_GLOBAL at scripts/test-gitconfig-path, but a
	// caller that runs `go test -tags acceptance_a` directly supplies no seed at
	// all, and the child then reads the host's real global config. That is how
	// gc doctor's beads-role check failed on a CI runner that had never opted in
	// — and it is also how gc-beads-bd's ensure_beads_role could write into a
	// developer's own global config. Seed one under GC_HOME instead.
	if e.vars["GIT_CONFIG_GLOBAL"] == "" {
		gitConfig := filepath.Join(gcHome, "gitconfig")
		if err := os.WriteFile(gitConfig, []byte(acceptanceGitConfig), 0o644); err != nil {
			panic(fmt.Sprintf("acceptance: seeding global git config at %s: %v", gitConfig, err))
		}
		e.vars["GIT_CONFIG_GLOBAL"] = gitConfig
		e.vars["GIT_CONFIG_NOSYSTEM"] = "1"
	}

	e.vars["XDG_RUNTIME_DIR"] = runtimeDir
	tmuxTmpDir := filepath.Join(runtimeDir, "tmux")
	if err := os.MkdirAll(tmuxTmpDir, 0o700); err != nil {
		panic(fmt.Sprintf("acceptance: creating tmux socket root under %s: %v", runtimeDir, err))
	}
	// TestMain callers that invoke tmux in the current process must call
	// tmuxtest.ConfigureProcessEnv with this same root before building Env.
	e.vars["TMUX_TMPDIR"] = tmuxTmpDir
	e.vars["GC_DOLT"] = "skip"
	beadsProvider := "file"
	if override := os.Getenv("GC_ACCEPTANCE_BEADS_PROVIDER"); override != "" {
		beadsProvider = override
	}
	e.vars["GC_BEADS"] = beadsProvider
	e.vars["GC_SESSION"] = "subprocess"

	if bdPath != "" {
		abs, err := filepath.Abs(bdPath)
		if err != nil {
			panic(fmt.Sprintf("acceptance: resolving bd path %q: %v", bdPath, err))
		}
		e.WithRealBD(abs)
	}
	return e
}

// installServiceManagerShims writes no-op launchctl/systemctl stubs under
// gcHome/bin and returns that directory so the acceptance env can prepend
// it to PATH. The stubs exit 1 so gc's supervisor-install logic falls
// back to an in-process supervisor start instead of delegating to the
// host's real service manager (which would also inherit the wrong env).
func installServiceManagerShims(gcHome string) (string, error) {
	shimDir := filepath.Join(gcHome, "bin")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		return "", err
	}
	const body = "#!/bin/sh\n# acceptance-test shim: force gc to bare-start the supervisor.\nexit 1\n"
	for _, name := range []string{"launchctl", "systemctl"} {
		p := filepath.Join(shimDir, name)
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			return "", err
		}
	}
	return shimDir, nil
}

// Clone returns an independent copy. Tier A shares one Env across the whole
// package, and With mutates in place, so a test that needs its own PATH or
// provider selection must take a copy rather than reach into the shared one.
func (e *Env) Clone() *Env {
	clone := &Env{vars: make(map[string]string, len(e.vars)), toolHome: e.toolHome, hostClaudeState: e.hostClaudeState, realBD: e.realBD}
	for k, v := range e.vars {
		clone.vars[k] = v
	}
	return clone
}

// With sets a variable, returning the Env for chaining. Setting HOME to
// anything but NewEnv's isolated home puts bd back behind the tool-home
// wrapper (WithWrappedBD): an unwrapped bd would resolve user-level state
// under that HOME.
func (e *Env) With(key, val string) *Env {
	e.vars[key] = val
	if key == "HOME" && !e.homeIsIsolated() {
		e.WithWrappedBD()
	}
	return e
}

// homeIsIsolated reports whether gc's HOME is NewEnv's isolated home.
func (e *Env) homeIsIsolated() bool {
	gcHome := e.vars["GC_HOME"]
	return gcHome != "" && e.vars["HOME"] == IsolatedHome(gcHome)
}

// WithHostHome hands gc the operator's own HOME in place of the isolated one,
// returning the Env for chaining. It is for the tiers that drive a real
// provider CLI authenticated through that home (tier C, tutorial goldens,
// worker inference); those runs are not hermetic, and no Bazel CI lane runs
// them. bd and dolt children still get the tool home.
func (e *Env) WithHostHome() *Env {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		lu, lookupErr := user.LookupId(strconv.Itoa(os.Getuid()))
		if lookupErr != nil || strings.TrimSpace(lu.HomeDir) == "" {
			panic(fmt.Sprintf("acceptance: resolving the host home: %v / %v", err, lookupErr))
		}
		home = lu.HomeDir
	}
	return e.With("HOME", home)
}

// WithRealBD makes bdPath itself the bd gc runs, returning the Env for
// chaining: BD_BIN names it, and a directory holding only a bd link to it
// goes on PATH (replacing any earlier real-bd directory) just ahead of the
// first directory that holds another bd, so every scope gc serves resolves it
// ahead of the tool-home wrapper while directories with no bd — the hermetic
// provider doubles — keep their place in front. NewEnv applies it to the
// configured bd; a test calls it to run a different bd.
//
// A split-storage city needs it. gc bd there forwards only to bd's own Go
// binary, which gc identifies by its Go build info (the bd trust policy in
// docs/runbooks/split-storage-classes.md). The wrapper is a shell script and
// has none, so gc refuses every forwarded command: the city's gate-sweep order
// on every cycle, and any gc bd a test or worker runs on a work bead.
//
// The cost is the wrapper's re-homing: every bd gc forks then resolves
// user-level state under gc's HOME instead of the tool home. That keeps the
// operator's home out of reach only because NewEnv isolated gc's HOME and
// pinned bd's shared-server mode and metrics off, so it panics on an Env whose
// gc has any other HOME, and leaving that HOME (WithHostHome, With("HOME"),
// Without("HOME")) drops back to the wrapper. A failed bd init also no longer
// prints its dolt logs (ParseBDInitDiagnostics); a test that needs them calls
// WithWrappedBD.
func (e *Env) WithRealBD(bdPath string) *Env {
	if !filepath.IsAbs(bdPath) {
		panic(fmt.Sprintf("acceptance: WithRealBD needs an absolute bd path, got %q (gc ignores a relative BD_BIN)", bdPath))
	}
	if !e.homeIsIsolated() {
		panic(fmt.Sprintf("acceptance: WithRealBD on an Env whose gc HOME %q is not NewEnv's isolated home: an unwrapped bd would resolve user-level state there", e.vars["HOME"]))
	}
	dir, err := linkRealBD(filepath.Join(e.vars["GC_HOME"], realBDDirName), bdPath)
	if err != nil {
		panic(fmt.Sprintf("acceptance: %v", err))
	}
	e.dropRealBDFromPath()
	e.vars["BD_BIN"] = bdPath
	e.vars["PATH"] = insertAheadOfFirstBD(e.vars["PATH"], dir)
	e.realBD = bdPath
	return e
}

// insertAheadOfFirstBD returns pathList with dir inserted just before the
// first entry that holds a bd, or at the front when none does.
func insertAheadOfFirstBD(pathList, dir string) string {
	entries := filepath.SplitList(pathList)
	at := 0
	for i, entry := range entries {
		if entry == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(entry, "bd")); err == nil {
			at = i
			break
		}
	}
	out := make([]string, 0, len(entries)+1)
	out = append(out, entries[:at]...)
	out = append(out, dir)
	out = append(out, entries[at:]...)
	return strings.Join(out, string(os.PathListSeparator))
}

// WithWrappedBD makes gc run bd through the tool-home wrapper NewEnv staged,
// returning the Env for chaining: BD_BIN is unset and the real-bd directory
// leaves PATH. It is for tests that assert wrapper behavior (re-homing, the
// dolt logs of a failed bd init); a split city refuses the wrapper.
func (e *Env) WithWrappedBD() *Env {
	if e.realBD == "" {
		return e
	}
	e.dropRealBDFromPath()
	if e.vars["BD_BIN"] == e.realBD {
		delete(e.vars, "BD_BIN")
	}
	e.realBD = ""
	return e
}

// RunsRealBD reports whether gc runs bd unwrapped (WithRealBD) rather than
// through the tool-home wrapper.
func (e *Env) RunsRealBD() bool {
	return e.realBD != ""
}

// dropRealBDFromPath removes every real-bd directory WithRealBD put on PATH.
func (e *Env) dropRealBDFromPath() {
	gcHome := e.vars["GC_HOME"]
	if gcHome == "" {
		return
	}
	root := filepath.Join(gcHome, realBDDirName) + string(os.PathSeparator)
	entries := filepath.SplitList(e.vars["PATH"])
	kept := entries[:0]
	for _, dir := range entries {
		if !strings.HasPrefix(dir+string(os.PathSeparator), root) {
			kept = append(kept, dir)
		}
	}
	e.vars["PATH"] = strings.Join(kept, string(os.PathListSeparator))
}

// linkRealBD returns a directory under root holding only a bd symlink to
// bdPath. Each bd gets its own directory, and an existing link is replaced
// atomically, so Envs sharing a GC_HOME never see each other's bd or a
// missing one.
func linkRealBD(root, bdPath string) (string, error) {
	dir := filepath.Join(root, fmt.Sprintf("%016x", fnv64(bdPath)))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}
	link := filepath.Join(dir, "bd")
	if target, err := os.Readlink(link); err == nil && target == bdPath {
		return dir, nil
	}
	tmp, err := os.MkdirTemp(root, ".link-")
	if err != nil {
		return "", fmt.Errorf("staging a bd link under %s: %w", root, err)
	}
	defer os.RemoveAll(tmp) //nolint:errcheck // best-effort cleanup of the staging dir
	staged := filepath.Join(tmp, "bd")
	if err := os.Symlink(bdPath, staged); err != nil {
		return "", fmt.Errorf("linking %s to %s: %w", staged, bdPath, err)
	}
	if err := os.Rename(staged, link); err != nil {
		return "", fmt.Errorf("installing %s: %w", link, err)
	}
	return dir, nil
}

// fnv64 is the 64-bit FNV-1a hash of s.
func fnv64(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

// WithHostClaudeState asks to let the harness seed Claude onboarding and
// project trust into the real user home's ~/.claude.json, returning the Env
// for chaining. It takes effect only where HostClaudeStateAllowed — a CI
// runner's throwaway home, or an explicit GC_TEST_ALLOW_HOST_CLAUDE=1 — and
// every other write under the real home is refused (claude_state.go). It is
// for a tier that drives the operator's own Claude CLI through the host HOME
// (tutorial goldens in host mode).
func (e *Env) WithHostClaudeState() *Env {
	e.hostClaudeState = true
	return e
}

// IsolatedHome is the HOME NewEnv gives gc for gcHome. A tier that stages
// provider credentials for gc's sessions stages them here.
func IsolatedHome(gcHome string) string {
	return filepath.Join(gcHome, "home")
}

// Without removes a variable. Removing HOME puts bd back behind the tool-home
// wrapper (WithWrappedBD), as setting it elsewhere does: an unwrapped bd with
// no HOME would resolve user-level state from the passwd home.
func (e *Env) Without(key string) *Env {
	delete(e.vars, key)
	if key == "HOME" {
		e.WithWrappedBD()
	}
	return e
}

// List returns the environment as a sorted []string for exec.Cmd.Env.
// Sorted for deterministic output in logs and debugging.
func (e *Env) List() []string {
	keys := make([]string, 0, len(e.vars))
	for k := range e.vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+e.vars[k])
	}
	return out
}

// Get returns a variable's value.
func (e *Env) Get(key string) string {
	return e.vars[key]
}

// WriteSupervisorConfig writes a supervisor.toml with an isolated port.
func WriteSupervisorConfig(gcHome string) error {
	port, err := reservePort()
	if err != nil {
		return fmt.Errorf("reserving supervisor port: %w", err)
	}
	cfg := fmt.Sprintf("[supervisor]\nport = %d\nbind = \"127.0.0.1\"\n", port)
	return os.WriteFile(filepath.Join(gcHome, "supervisor.toml"), []byte(cfg), 0o644)
}

// reservePort returns a loopback port that, on Linux, no other
// ReserveLoopbackPort caller in the network namespace is handed while this
// test binary lives (testutil.ReserveLoopbackPort). A listen-then-close
// reservation let concurrent shards on one rbe-west worker share a
// supervisor port (ga-96smfk.45).
func reservePort() (int, error) {
	return testutil.ReserveLoopbackPort()
}

// RunGC runs the gc binary with the given args in the given environment.
func RunGC(env *Env, dir string, args ...string) (string, error) {
	gcPath, err := ResolveGCPath(env)
	if err != nil {
		return "", err
	}
	cmd := exec.Command(gcPath, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = env.List()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// RunGCStreams runs gc and returns stdout and stderr separately. Use this
// when a test parses gc output positionally: config-load advisories go to
// stderr, and CombinedOutput() would interleave them into the parse.
func RunGCStreams(env *Env, dir string, args ...string) (string, string, error) {
	gcPath, err := ResolveGCPath(env)
	if err != nil {
		return "", "", err
	}
	cmd := exec.Command(gcPath, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = env.List()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	return stdout.String(), stderr.String(), err
}

// ResolveGCPath returns the exact gc binary path for this acceptance env.
func ResolveGCPath(env *Env) (string, error) {
	if env == nil {
		return "", fmt.Errorf("gc env is nil")
	}
	if gcPath := strings.TrimSpace(env.Get("GC_ACCEPTANCE_GC_BIN")); gcPath != "" {
		return gcPath, nil
	}
	gcPath := findInPath(env.Get("PATH"), "gc")
	if gcPath == "" {
		return "", fmt.Errorf("gc not found in PATH")
	}
	return gcPath, nil
}

// VerifyPathGC reports an error unless the gc that env's PATH finds is the
// binary under test, the one RunGC runs. gc's exec orders and pack scripts run
// bare `gc` through PATH: with no gc there they exit 127, and with another gc
// first they run that one, while every RunGC call still runs the binary under
// test.
func VerifyPathGC(env *Env) error {
	want, err := ResolveGCPath(env)
	if err != nil {
		return err
	}
	wantInfo, err := os.Stat(want)
	if err != nil {
		return fmt.Errorf("binary under test: %w", err)
	}
	got := findInPath(env.Get("PATH"), "gc")
	if got == "" {
		return fmt.Errorf("no gc on PATH, so exec orders cannot run the binary under test %s", want)
	}
	gotInfo, err := os.Stat(got)
	if err != nil {
		return fmt.Errorf("gc on PATH: %w", err)
	}
	if !os.SameFile(gotInfo, wantInfo) {
		return fmt.Errorf("the gc on PATH is %s, not the binary under test %s", got, want)
	}
	return nil
}

func findInPath(pathEnv, name string) string {
	for _, dir := range strings.Split(pathEnv, ":") {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
