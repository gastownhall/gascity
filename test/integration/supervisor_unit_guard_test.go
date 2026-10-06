//go:build integration

package integration

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Guard against the suite leaking platform supervisor units into the
// operator's home. gc's platform install path (ensureSupervisorRunning →
// doSupervisorInstall in cmd/gc/cmd_supervisor_lifecycle.go) names the unit
// after GC_HOME, so every isolated env root has exactly one unit name it could
// leak, and that name hashes a path only this test owns. Checking that one
// name is precise: it never flags the operator's or a sibling run's units, and
// it needs no systemd, so it holds in CI containers and under Bazel too.

// passwdHome returns the passwd-db home of the current uid, which is where
// gc's platform install writes regardless of the HOME a test hands it.
func passwdHome() (string, bool) {
	lu, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil || strings.TrimSpace(lu.HomeDir) == "" {
		return "", false
	}
	return lu.HomeDir, true
}

// systemdUserConfigDirs lists the systemd user configuration directories
// `systemctl --user enable` may link a unit into: $XDG_CONFIG_HOME/systemd/user
// when xdgConfigHome is an absolute path (the XDG spec ignores a relative
// one), and the default ~/.config/systemd/user. The user manager, not gc,
// resolves the directory from its own environment, which may differ from
// this process's, so both are candidates.
func systemdUserConfigDirs(realHome, xdgConfigHome string) []string {
	dirs := []string{filepath.Join(realHome, ".config", "systemd", "user")}
	if filepath.IsAbs(xdgConfigHome) {
		if dir := filepath.Join(xdgConfigHome, "systemd", "user"); dir != dirs[0] {
			dirs = append([]string{dir}, dirs...)
		}
	}
	return dirs
}

// platformSupervisorUnitPaths lists the files gc's platform install path
// writes under realHome for a supervisor whose GC_HOME is gcHome: on Linux
// the systemd user unit, under $HOME/.local/share/systemd/user whatever
// XDG_DATA_HOME says (cmd/gc supervisorSystemdServicePath), and its
// default.target.wants enable link in each systemdUserConfigDirs candidate
// for xdgConfigHome; on macOS the LaunchAgent plist.
func platformSupervisorUnitPaths(realHome, xdgConfigHome, gcHome string) []string {
	suffix := expectedSupervisorServiceSuffix(gcHome)
	if suffix == "" {
		return nil
	}
	switch runtime.GOOS {
	case "linux":
		unit := "gascity-supervisor-" + suffix + ".service"
		paths := []string{filepath.Join(realHome, ".local", "share", "systemd", "user", unit)}
		for _, dir := range systemdUserConfigDirs(realHome, xdgConfigHome) {
			paths = append(paths, filepath.Join(dir, "default.target.wants", unit))
		}
		return paths
	case "darwin":
		return []string{
			filepath.Join(realHome, "Library", "LaunchAgents", "com.gascity.supervisor."+suffix+".plist"),
		}
	default:
		return nil
	}
}

// leakedPlatformSupervisorUnits returns the platform unit files present for
// gcHome under realHome and xdgConfigHome. Lstat, so a dangling enable link
// still counts.
func leakedPlatformSupervisorUnits(realHome, xdgConfigHome, gcHome string) []string {
	var leaked []string
	for _, path := range platformSupervisorUnitPaths(realHome, xdgConfigHome, gcHome) {
		if _, err := os.Lstat(path); err == nil {
			leaked = append(leaked, path)
		}
	}
	return leaked
}

// removeLeakedPlatformSupervisorUnit stops and removes the unit gc installed
// for gcHome. The name hashes gcHome, a directory this run created, so this
// only ever touches a unit this run leaked.
func removeLeakedPlatformSupervisorUnit(gcHome string, leaked []string) {
	suffix := expectedSupervisorServiceSuffix(gcHome)
	var stop, reload []string
	switch runtime.GOOS {
	case "linux":
		stop = []string{"systemctl", "--user", "disable", "--now", "gascity-supervisor-" + suffix + ".service"}
		reload = []string{"systemctl", "--user", "daemon-reload"}
	case "darwin":
		stop = []string{"launchctl", "bootout", fmt.Sprintf("gui/%d/com.gascity.supervisor.%s", os.Getuid(), suffix)}
	}
	runServiceManager(stop)
	for _, path := range leaked {
		_ = os.Remove(path)
	}
	runServiceManager(reload)
}

// runServiceManager runs argv best-effort; the leak is already reported, and
// a missing service manager leaves nothing to stop.
func runServiceManager(argv []string) {
	if len(argv) == 0 {
		return
	}
	_ = exec.Command(argv[0], argv[1:]...).Run()
}

// platformUnitLeakReport checks gcHome for a leaked platform unit, removes
// it, and returns a description of the leak, or "" when there is none.
func platformUnitLeakReport(gcHome string) string {
	realHome, ok := passwdHome()
	if !ok {
		return ""
	}
	leaked := leakedPlatformSupervisorUnits(realHome, os.Getenv("XDG_CONFIG_HOME"), gcHome)
	if len(leaked) == 0 {
		return ""
	}
	removeLeakedPlatformSupervisorUnit(gcHome, leaked)
	return fmt.Sprintf("gc installed a platform supervisor unit for GC_HOME %s into the real home (%s); "+
		"the integration env must give gc an isolated HOME with %s=1 (see isolateGCHomeEnv). Removed it.",
		gcHome, strings.Join(leaked, ", "), supervisorIsolatedHomeEnv)
}

// registerPlatformUnitLeakGuard fails t if, by the time its cleanups run, gc
// installed a platform supervisor unit for gcHome. Register it before any
// supervisor is started so it runs after their stops (t.Cleanup is LIFO).
func registerPlatformUnitLeakGuard(t *testing.T, gcHome string) {
	t.Helper()
	// Resolve symlinks now, while gcHome surely exists: gc hashes the
	// resolved path into the unit name.
	normalized := gcHome
	if resolved, err := filepath.EvalSymlinks(gcHome); err == nil {
		normalized = resolved
	}
	t.Cleanup(func() {
		if report := platformUnitLeakReport(normalized); report != "" {
			t.Error(report)
		}
	})
}

func TestLeakedPlatformSupervisorUnitsFindsOnlyThisGCHome(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("no platform supervisor unit on " + runtime.GOOS)
	}
	realHome := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "gc-home")
	other := filepath.Join(t.TempDir(), "gc-home")
	for _, dir := range []string{gcHome, other} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := leakedPlatformSupervisorUnits(realHome, "", gcHome); len(got) != 0 {
		t.Fatalf("leaked units before install = %v, want none", got)
	}

	paths := platformSupervisorUnitPaths(realHome, "", other)
	if len(paths) == 0 {
		t.Fatal("no platform unit paths")
	}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(realHome, "deleted-target"), path); err != nil {
			t.Fatal(err)
		}
	}
	if got := leakedPlatformSupervisorUnits(realHome, "", gcHome); len(got) != 0 {
		t.Fatalf("leaked units for an untouched GC_HOME = %v, want none (another run's unit must not count)", got)
	}
	if got := leakedPlatformSupervisorUnits(realHome, "", other); len(got) != len(paths) {
		t.Fatalf("leaked units = %v, want %v (dangling enable links count)", got, paths)
	}
}

// An enable link under $XDG_CONFIG_HOME/systemd/user is a leak too: the
// user manager links units into its configuration directory, which is
// $XDG_CONFIG_HOME/systemd/user when that is set, not ~/.config/systemd/user.
func TestLeakedPlatformSupervisorUnitsHonourXDGConfigHome(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd user units are Linux-only")
	}
	realHome := t.TempDir()
	xdgConfigHome := filepath.Join(t.TempDir(), "xdg-config")
	gcHome := filepath.Join(t.TempDir(), "gc-home")
	if err := os.MkdirAll(gcHome, 0o755); err != nil {
		t.Fatal(err)
	}
	unit := "gascity-supervisor-" + expectedSupervisorServiceSuffix(gcHome) + ".service"
	link := filepath.Join(xdgConfigHome, "systemd", "user", "default.target.wants", unit)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(realHome, "deleted-target"), link); err != nil {
		t.Fatal(err)
	}
	if got := leakedPlatformSupervisorUnits(realHome, xdgConfigHome, gcHome); len(got) != 1 || got[0] != link {
		t.Errorf("leaked units with XDG_CONFIG_HOME=%s = %v, want [%s]", xdgConfigHome, got, link)
	}
	// The default ~/.config stays a candidate: the user manager's
	// environment, not this process's, picks the directory.
	defaultLink := filepath.Join(realHome, ".config", "systemd", "user", "default.target.wants", unit)
	if paths := platformSupervisorUnitPaths(realHome, xdgConfigHome, gcHome); !slices.Contains(paths, defaultLink) {
		t.Errorf("unit paths %v lack the default enable link %s", paths, defaultLink)
	}
	// The XDG spec ignores a relative XDG_CONFIG_HOME.
	if paths := platformSupervisorUnitPaths(realHome, filepath.Join("relative", "config"), gcHome); len(paths) != 2 {
		t.Errorf("unit paths with a relative XDG_CONFIG_HOME = %v, want the unit and the default link", paths)
	}
}

// Every env root the suite hands gc must opt into the isolated-HOME bare
// supervisor, or `gc init`/`gc start` installs a platform unit.
func TestIsolatedEnvRootsKeepGCOffTheRealHome(t *testing.T) {
	realHome, _ := passwdHome()
	drift := func(t *testing.T) (string, []string) {
		gcHome, _, env := newDriftIsolatedEnvRoot(t)
		return gcHome, env
	}
	isolated := func(t *testing.T) (string, []string) {
		gcHome, _, env := newIsolatedEnvRoot(t, false)
		return gcHome, env
	}
	shared := func(*testing.T) (string, []string) { return testGCHome, integrationEnv() }
	for name, build := range map[string]func(*testing.T) (string, []string){
		"newIsolatedEnvRoot":      isolated,
		"newDriftIsolatedEnvRoot": drift,
		"integrationEnv":          shared,
	} {
		t.Run(name, func(t *testing.T) {
			gcHome, env := build(t)
			got := parseEnvList(env)
			if got["GC_HOME"] != gcHome {
				t.Fatalf("GC_HOME = %q, want %q", got["GC_HOME"], gcHome)
			}
			if got["HOME"] != integrationIsolatedHome(gcHome) {
				t.Errorf("HOME = %q, want %q", got["HOME"], integrationIsolatedHome(gcHome))
			}
			if realHome != "" && got["HOME"] == realHome {
				t.Errorf("HOME is the passwd home %q", realHome)
			}
			if got[supervisorIsolatedHomeEnv] != "1" {
				t.Errorf("%s = %q, want 1", supervisorIsolatedHomeEnv, got[supervisorIsolatedHomeEnv])
			}
		})
	}
}

// envBuilderUse is a reference to buildIntegrationEnv: where it is and the
// declaration holding it.
type envBuilderUse struct {
	pos     token.Pos
	owner   string
	allowed bool
}

// envBuilders are the functions allowed to reference buildIntegrationEnv.
var envBuilders = map[string]bool{"integrationEnv": true, "integrationEnvDolt": true, "integrationEnvFor": true}

// buildIntegrationEnvUses lists every reference to buildIntegrationEnv in
// file: in function bodies, and at package scope, where a var could alias it
// past the check. Only a plain function among envBuilders is allowed one.
func buildIntegrationEnvUses(file *ast.File) []envBuilderUse {
	var uses []envBuilderUse
	for _, decl := range file.Decls {
		owner, allowed, node := "package scope", false, ast.Node(decl)
		if fn, ok := decl.(*ast.FuncDecl); ok {
			if fn.Body == nil {
				continue
			}
			owner, node = fn.Name.Name, fn.Body
			allowed = fn.Recv == nil && envBuilders[owner]
			if fn.Recv != nil {
				owner = "method " + owner
			}
		}
		ast.Inspect(node, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == "buildIntegrationEnv" {
				uses = append(uses, envBuilderUse{pos: id.Pos(), owner: owner, allowed: allowed})
			}
			return true
		})
	}
	return uses
}

// buildIntegrationEnvUses sees a reference wherever it hides: aliased at
// package scope, taken as a value, or in a method sharing a builder's name.
func TestBuildIntegrationEnvUsesSeesEveryReference(t *testing.T) {
	const src = `package integration

var aliased = buildIntegrationEnv

func buildIntegrationEnv(gcHome, runtimeDir string, useDolt bool) []string { return nil }

func integrationEnv() []string { return buildIntegrationEnv("", "", false) }

func helper() []string {
	f := buildIntegrationEnv
	return f("", "", false)
}

type builder struct{}

func (builder) integrationEnvFor() []string { return buildIntegrationEnv("", "", false) }
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "src.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, use := range buildIntegrationEnvUses(file) {
		got = append(got, fmt.Sprintf("%d %s %t", fset.Position(use.pos).Line, use.owner, use.allowed))
	}
	want := []string{
		"3 package scope false",
		"7 integrationEnv true",
		"10 helper false",
		"16 method integrationEnvFor false",
	}
	if !slices.Equal(got, want) {
		t.Errorf("buildIntegrationEnvUses = %q, want %q", got, want)
	}
}

// TestEveryIntegrationEnvHasALeakGuard: only the env builders use
// buildIntegrationEnv. A test that built gc's env from it directly would skip
// integrationEnvFor's leak guard and could leak a unit unnoticed.
func TestEveryIntegrationEnvHasALeakGuard(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no package sources in the working directory")
	}
	fset := token.NewFileSet()
	uses := 0
	for _, name := range files {
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, use := range buildIntegrationEnvUses(file) {
			uses++
			if !use.allowed {
				t.Errorf("%s: %s uses buildIntegrationEnv; call integrationEnvFor(t, ...) so the platform-unit leak guard covers its GC_HOME",
					fset.Position(use.pos), use.owner)
			}
		}
	}
	if uses == 0 {
		t.Error("found no use of buildIntegrationEnv: this check checks nothing")
	}
}
