package acceptancehelpers

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

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

	// Inherit minimum from host. HOME is NOT inherited — it's set to
	// a test-specific directory to prevent gc from reading ~/.gc/,
	// ~/.gitconfig, or other real user state.
	for _, key := range []string{
		"PATH", "TMPDIR", "LANG", "LC_ALL", "USER",
		"SHELL", "SSH_AUTH_SOCK", "TERM",
		"CLAUDE_CONFIG_DIR", // Claude Code reads OAuth credentials from here
		"ANTHROPIC_API_KEY",
		"OPENAI_API_KEY",
		"GEMINI_API_KEY",
		"GOOGLE_API_KEY",
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

	// Test isolation: HOME points to gcHome so gc never reads real user state.
	e.vars["HOME"] = gcHome
	e.vars["GC_HOME"] = gcHome
	e.vars["XDG_RUNTIME_DIR"] = runtimeDir
	e.vars["GC_DOLT"] = "skip"
	e.vars["GC_BEADS"] = "file"
	e.vars["GC_SESSION"] = "subprocess"

	return e
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
