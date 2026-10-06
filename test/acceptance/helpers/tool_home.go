package acceptancehelpers

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/test/toolhome"
)

// The acceptance suite runs gc with an isolated HOME under GC_HOME (see
// NewEnv); only the real-provider tiers hand it the operator's own
// (WithHostHome). bd and dolt see neither: every Env carries a tool home, and
// each bd or dolt child is re-homed under it — through ToolList or ToolCommand
// when the suite forks it, through LinkBeadsTooling's wrapper when gc finds it
// on PATH or via BD_BIN.
// See package toolhome for what bd resolves from HOME and the incident this
// guards against.

// bdSharedServerConfigEnv is bd's viper env binding for dolt.shared-server.
const bdSharedServerConfigEnv = toolhome.SharedServerConfigEnv

// IsolatedToolEnv is toolhome.Environ: base with HOME and the XDG base
// directories moved under home, host BEADS_*/BD_* dropped (except names in
// keep), and bd's shared-server mode pinned off.
func IsolatedToolEnv(base []string, home string, keep ...string) []string {
	return toolhome.Environ(base, home, keep...)
}

// ToolHome is the home directory bd and dolt children of this Env resolve
// user-level state under. It is never the operator's HOME.
func (e *Env) ToolHome() string {
	return e.toolHome
}

// ToolList is List for a bd or dolt the suite forks directly: the same
// variables, with HOME (and any XDG base directory the Env does not set) moved
// under ToolHome and bd's shared-server mode pinned off unless the Env says
// otherwise. The Env's own BEADS_*/BD_* variables are explicit by construction
// — NewEnv inherits none from the host — so they survive.
func (e *Env) ToolList() []string {
	if e.toolHome == "" {
		panic("acceptance: Env has no tool home; build it with NewEnv")
	}
	list := e.List()
	return toolhome.Environ(list, e.toolHome, toolhome.Explicit(list)...)
}

// ToolCommand is a bounded exec.Command for a bd or dolt the suite runs outside
// any Env (a version or capability probe): the test process's environment,
// re-homed under a fresh per-test directory.
func ToolCommand(t *testing.T, bin string, args ...string) *exec.Cmd {
	t.Helper()
	cmd, _ := toolCommand(t, acceptanceToolCommandTimeout, bin, args...)
	return cmd
}

func toolCommand(t *testing.T, timeout time.Duration, bin string, args ...string) (*exec.Cmd, context.Context) {
	t.Helper()
	home := filepath.Join(TempDir(t), "tool-home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("create tool home: %v", err)
	}
	cmd, ctx := boundedToolCommand(t, timeout, bin, args...)
	cmd.Env = toolhome.Environ(os.Environ(), home)
	// bd walks up from its working directory looking for a .beads. The test
	// process's cwd is its package directory, which on a developer box sits
	// under the real home, so an inherited cwd reads ~/.beads/config.yaml
	// whatever HOME says. A caller that needs a workspace sets Dir itself.
	cmd.Dir = home
	return cmd, ctx
}

// LinkBeadsTooling puts this run's bd and dolt in dir for gc to find on PATH and
// returns the bd it wrote, which is also what to hand gc as BD_BIN.
//
// dolt is a symlink. bd is toolhome's wrapper around bdPath, re-homed under the
// Env's tool home, because gc forks bd with the Env's real HOME.
func LinkBeadsTooling(t *testing.T, env *Env, dir, bdPath, doltPath string) string {
	t.Helper()
	wrapper, err := InstallBeadsTooling(env, dir, bdPath, doltPath)
	if err != nil {
		t.Fatal(err)
	}
	return wrapper
}

// InstallBeadsTooling is LinkBeadsTooling for a TestMain, which has no
// testing.T. An empty doltPath links bd alone.
func InstallBeadsTooling(env *Env, dir, bdPath, doltPath string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create tooling dir %s: %w", dir, err)
	}
	if doltPath != "" {
		if err := os.Symlink(doltPath, filepath.Join(dir, "dolt")); err != nil && !os.IsExist(err) {
			return "", fmt.Errorf("link dolt into %s: %w", dir, err)
		}
	}
	wrapper := filepath.Join(dir, "bd")
	if err := toolhome.WriteWrapper(wrapper, env.ToolHome(), bdPath); err != nil {
		return "", err
	}
	return wrapper, nil
}
