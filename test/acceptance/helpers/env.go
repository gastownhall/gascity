package acceptancehelpers

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

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
}

// NewEnv creates an isolated environment with the minimum inherited
// variables (PATH, TMPDIR, locale, shell) plus test-specific overrides
// for GC_HOME and XDG_RUNTIME_DIR.
func NewEnv(gcBinary, gcHome, runtimeDir string) *Env {
	e := &Env{vars: make(map[string]string)}

	// Inherit minimum from host. Keep the real HOME: the platform
	// supervisor path now validates that HOME matches the OS user home
	// and acceptance isolation should flow through GC_HOME instead.
	for _, key := range []string{
		"PATH", "TMPDIR", "LANG", "LC_ALL", "USER", "HOME",
		"SHELL", "SSH_AUTH_SOCK", "TERM",
		// The Makefile's TEST_ENV points these at scripts/test-gitconfig-path, a
		// seeded writable global gitconfig carrying user.name, user.email and
		// beads.role=maintainer. Dropping them sent the child's reads at the
		// runner's real global config, which has no beads.role, so `gc doctor`
		// failed its beads-role check on any host that had never opted in.
		"GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM",
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

	if e.vars["HOME"] == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			e.vars["HOME"] = home
		}
	}
	e.vars["GC_HOME"] = gcHome

	// Dolt reads its global config from $DOLT_ROOT_PATH/.dolt/config_global.json,
	// falling back to $HOME. gc's init preflight (checkDoltAuthorIdentity) probes
	// it with `dolt config --global --get` and has no fallback to any other
	// config source, so a shape that drops GC_DOLT=skip refuses to init on a
	// machine whose home never ran `dolt config --global`. Leaning on the host's
	// ambient identity made that a coin flip: green on a developer box, red on a
	// fresh CI runner. Seed under GC_HOME rather than the real home so a run
	// never writes host Dolt state -- the same pattern tier_c, tutorial_goldens
	// and test/integration already use.
	doltRoot := filepath.Join(gcHome, ".dolt")
	if err := os.MkdirAll(doltRoot, 0o755); err != nil {
		panic(fmt.Sprintf("acceptance: creating dolt root under %s: %v", gcHome, err))
	}
	doltCfg := `{"user.name":"gc-test","user.email":"gc-test@test.local"}`
	if err := os.WriteFile(filepath.Join(doltRoot, "config_global.json"), []byte(doltCfg), 0o644); err != nil {
		panic(fmt.Sprintf("acceptance: seeding dolt identity under %s: %v", doltRoot, err))
	}
	e.vars["DOLT_ROOT_PATH"] = gcHome

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
	clone := &Env{vars: make(map[string]string, len(e.vars))}
	for k, v := range e.vars {
		clone.vars[k] = v
	}
	return clone
}

// With sets a variable, returning the Env for chaining.
func (e *Env) With(key, val string) *Env {
	e.vars[key] = val
	return e
}

// Without removes a variable.
func (e *Env) Without(key string) *Env {
	delete(e.vars, key)
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

// reservePort finds a free port using the listen-then-close pattern.
// Known TOCTOU race: between Close() and the supervisor binding, another
// process can claim the port. This matches the existing integration test
// pattern (reserveLoopbackPort) and is an accepted risk.
func reservePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port, nil
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

func findInPath(pathEnv, name string) string {
	for _, dir := range strings.Split(pathEnv, ":") {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
