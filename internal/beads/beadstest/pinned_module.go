package beadstest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// PinnedBeadsModulePath is the module gc links its native store against.
const PinnedBeadsModulePath = "github.com/steveyegge/beads"

// PinnedBeadsModuleDir resolves the unpacked source directory of the beads
// version this module requires, and FAILS the calling test when it cannot.
//
// Failing rather than skipping is the point. The packages that call this import
// github.com/steveyegge/beads, so if the test binary compiled at all then cmd/go
// resolved the pinned module — which means an absent directory does not say "the
// module is not here", it says the resolution below disagreed with cmd/go's. That
// is exactly the situation the drift check must shout about: a skip would let a
// resolver bug read as green, and the check it silences is the one standing
// between gc and a library that migrates somebody's shared database on open. A
// quiet skip was demonstrated with nothing more than GOMODCACHE pointed
// somewhere else.
//
// It resolves the cache the way the go command does rather than by reading
// GOMODCACHE, because `go test` does not export that variable into the test
// process: a check that read it ran only under wrappers that happen to forward
// it, and went quiet under every plain `go test`, an editor runner and a git
// hook. A contract check that is honest under one wrapper is not a contract
// check. (internal/beads/contract/migrate_journal_test.go learned this the hard
// way and states the reasoning at length.)
//
// It also deliberately shells out to nothing. `go env` or `go list -m` would
// answer in one line, and a subprocess in a test grows the repo's source
// resource census — a shrink-only ratchet — so the resolution is inlined
// instead.
func PinnedBeadsModuleDir(t *testing.T) string {
	t.Helper()
	return pinnedBeadsModuleDirOrFatal(t, goModuleCache(t), PinnedBeadsVersion(t))
}

// moduleDirReporter is the subset of *testing.T the seam below uses.
//
// It exists so that "an unresolved cache is FATAL, never a skip" can be asserted
// by a recorder in this package's own tests. The only other way to observe it is
// a *testing.T in a subprocess, and a subprocess in a test grows the repo's
// shrink-only resource census — the same reason this file resolves the module
// cache inline instead of asking `go env`. Skipf is part of the interface on
// purpose: a revert to it still compiles, and is caught by a test rather than by
// a reviewer.
type moduleDirReporter interface {
	Helper()
	Fatalf(format string, args ...any)
	Skipf(format string, args ...any)
}

// pinnedBeadsModuleDirOrFatal reports an unresolved module cache as a test
// failure. See PinnedBeadsModuleDir for why it cannot be a skip.
func pinnedBeadsModuleDirOrFatal(t moduleDirReporter, cache, version string) string {
	t.Helper()
	dir, err := pinnedBeadsModuleDir(cache, version)
	if err != nil {
		t.Fatalf("%v\n"+
			"The test binary links %s, so the go command resolved it; this resolution did not. "+
			"Check GOMODCACHE, GOPATH and GOENV (resolved cache: %s), and whether go.mod gained a "+
			"replace or the build moved to vendor mode — the pinned-cursor drift check cannot run "+
			"without the module's own migration directories, and it must not pass without running.",
			err, PinnedBeadsModulePath, cache)
	}
	return dir
}

// pinnedBeadsModuleDir is the resolution itself, separated from the test so that
// the failure path has a test of its own.
func pinnedBeadsModuleDir(cache, version string) (string, error) {
	dir := filepath.Join(cache, filepath.FromSlash(PinnedBeadsModulePath)+"@"+version)
	info, err := os.Stat(dir)
	switch {
	case err != nil:
		return "", fmt.Errorf("pinned beads %s is not unpacked in the module cache at %s: %w", version, dir, err)
	case !info.IsDir():
		return "", fmt.Errorf("pinned beads %s resolved to %s, which is not a directory", version, dir)
	default:
		return dir, nil
	}
}

// PinnedBeadsVersion reads the beads version this module requires from go.mod.
func PinnedBeadsVersion(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(RepositoryRoot(t), "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 2 && fields[0] == PinnedBeadsModulePath {
			return fields[1]
		}
		if len(fields) >= 3 && fields[0] == "require" && fields[1] == PinnedBeadsModulePath {
			return fields[2]
		}
	}
	t.Fatalf("go.mod does not require %s", PinnedBeadsModulePath)
	return ""
}

// RepositoryRoot walks up from the working directory to the module root.
func RepositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolve working directory: %v", err)
	}
	for {
		if info, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && !info.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the package directory")
		}
		dir = parent
	}
}

// goModuleCache resolves the module cache the way cmd/go/internal/cfg does: the
// process environment first, then the `go env -w` config file ($GOENV, else
// os.UserConfigDir()/go/env, with "off" meaning no file), then the default
// $GOPATH/pkg/mod with GOPATH resolved by the same two steps and defaulting to
// $HOME/go. Only the first element of a GOPATH list holds the module cache.
func goModuleCache(t *testing.T) string {
	t.Helper()
	if dir := goEnvValue("GOMODCACHE"); dir != "" {
		return dir
	}
	gopath := goEnvValue("GOPATH")
	if gopath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatalf("resolve home directory to default GOPATH: %v", err)
		}
		gopath = filepath.Join(home, "go")
	}
	roots := filepath.SplitList(gopath)
	if len(roots) == 0 || roots[0] == "" {
		t.Fatalf("GOPATH %q has no usable first element", gopath)
	}
	return filepath.Join(roots[0], "pkg", "mod")
}

// goEnvValue reads one go environment variable: process environment, then the
// `go env -w` config file.
func goEnvValue(name string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return goEnvFileValue(name)
}

// goEnvFileValue reads one key out of the go env config file.
func goEnvFileValue(name string) string {
	path := strings.TrimSpace(os.Getenv("GOENV"))
	switch path {
	case "off":
		return ""
	case "", "auto":
		dir, err := os.UserConfigDir()
		if err != nil {
			return ""
		}
		path = filepath.Join(dir, "go", "env")
	}
	data, err := os.ReadFile(path) // #nosec G304 -- the go env config path, resolved the way the go command resolves it
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && key == name {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
