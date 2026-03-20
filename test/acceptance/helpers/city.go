package acceptancehelpers

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// City is the acceptance test DSL. It wraps a city directory and the
// isolated environment, providing high-level methods that shell out to
// the real gc binary.
type City struct {
	t       *testing.T
	Dir     string
	Env     *Env
	started bool
}

// NewCity creates a temp directory for a city and returns the DSL handle.
// The city is NOT initialized — call Init() or InitFrom() next.
func NewCity(t *testing.T, env *Env) *City {
	t.Helper()
	dir := t.TempDir()
	cityDir := filepath.Join(dir, uniqueName())
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatalf("acceptance: creating city dir: %v", err)
	}
	return &City{t: t, Dir: cityDir, Env: env}
}

// Init runs gc init with the given provider (non-interactive).
func (c *City) Init(provider string) {
	c.t.Helper()
	args := []string{"init", "--skip-provider-readiness"}
	if provider != "" {
		args = append(args, "--provider", provider)
	}
	args = append(args, c.Dir)
	out, err := RunGC(c.Env, "", args...)
	if err != nil {
		c.t.Fatalf("gc init failed: %v\n%s", err, out)
	}
	c.t.Cleanup(func() {
		RunGC(c.Env, c.Dir, "stop", c.Dir) //nolint:errcheck
	})
}

// InitFrom runs gc init --from to copy an example city directory.
func (c *City) InitFrom(srcDir string) {
	c.t.Helper()
	out, err := RunGC(c.Env, "", "init", "--from", srcDir, "--skip-provider-readiness", c.Dir)
	if err != nil {
		c.t.Fatalf("gc init --from %s failed: %v\n%s", srcDir, err, out)
	}
	c.t.Cleanup(func() {
		RunGC(c.Env, c.Dir, "stop", c.Dir) //nolint:errcheck
	})
}

// WriteConfig overwrites city.toml with the given content.
func (c *City) WriteConfig(toml string) {
	c.t.Helper()
	if err := os.WriteFile(filepath.Join(c.Dir, "city.toml"), []byte(toml), 0o644); err != nil {
		c.t.Fatalf("writing city.toml: %v", err)
	}
}

// Stop runs gc stop.
func (c *City) Stop() {
	if !c.started {
		return
	}
	c.started = false
	// Best-effort stop — don't fail the test on cleanup errors.
	RunGC(c.Env, c.Dir, "stop", c.Dir) //nolint:errcheck
}

// AgentEnv reads an agent's environment by inspecting the session metadata.
// Uses gc agent env <name> which dumps the resolved env for the agent.
func (c *City) AgentEnv(name string) map[string]string {
	c.t.Helper()
	out, err := RunGC(c.Env, c.Dir, "config", "explain", "--agent", name)
	if err != nil {
		c.t.Fatalf("gc config explain --agent %s: %v\n%s", name, err, out)
	}
	return parseKeyValues(out)
}

// HasFile checks if a file exists relative to the city directory.
func (c *City) HasFile(rel string) bool {
	_, err := os.Stat(filepath.Join(c.Dir, rel))
	return err == nil
}

// ReadFile reads a file relative to the city directory.
func (c *City) ReadFile(rel string) string {
	c.t.Helper()
	data, err := os.ReadFile(filepath.Join(c.Dir, rel))
	if err != nil {
		c.t.Fatalf("reading %s: %v", rel, err)
	}
	return string(data)
}

// WaitForFile polls until a file exists or timeout.
func (c *City) WaitForFile(rel string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c.HasFile(rel) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// GC runs an arbitrary gc command in the city directory.
func (c *City) GC(args ...string) (string, error) {
	return RunGC(c.Env, c.Dir, args...)
}

func parseKeyValues(s string) map[string]string {
	m := make(map[string]string)
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if k, v, ok := strings.Cut(line, "="); ok {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return m
}

func uniqueName() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "at-fallback"
	}
	return "at-" + hex.EncodeToString(b)
}

// ExamplesDir returns the absolute path to the examples/ directory
// in the source tree.
func ExamplesDir() string {
	return filepath.Join(FindModuleRoot(), "examples")
}

// FormatConfig builds a minimal city.toml from structured fields.
func FormatConfig(name, provider string, agents []AgentConfig, rigs []RigConfig) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[workspace]\nname = %q\n", name)
	if provider != "" {
		fmt.Fprintf(&b, "provider = %q\n", provider)
	}
	for _, r := range rigs {
		fmt.Fprintf(&b, "\n[[rigs]]\nname = %q\npath = %q\n", r.Name, r.Path)
	}
	for _, a := range agents {
		fmt.Fprintf(&b, "\n[[agent]]\nname = %q\n", a.Name)
		if a.StartCommand != "" {
			fmt.Fprintf(&b, "start_command = %q\n", a.StartCommand)
		}
		if a.Dir != "" {
			fmt.Fprintf(&b, "dir = %q\n", a.Dir)
		}
		if a.WorkDir != "" {
			fmt.Fprintf(&b, "work_dir = %q\n", a.WorkDir)
		}
		if a.Pool != nil {
			fmt.Fprintf(&b, "[agent.pool]\nmin = %d\nmax = %d\n", a.Pool.Min, a.Pool.Max)
			if a.Pool.ScaleCheck != "" {
				fmt.Fprintf(&b, "scale_check = %q\n", a.Pool.ScaleCheck)
			}
		}
	}
	return b.String()
}

// AgentConfig describes an agent for FormatConfig.
type AgentConfig struct {
	Name         string
	StartCommand string
	Dir          string
	WorkDir      string
	Pool         *PoolConfig
}

// PoolConfig describes pool settings.
type PoolConfig struct {
	Min        int
	Max        int
	ScaleCheck string
}

// RigConfig describes a rig.
type RigConfig struct {
	Name string
	Path string
}
