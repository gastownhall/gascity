package testenv_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testenv"
)

// The bd recursion fuse (ga-kihcmff, incident ga-2t6u9dy): every Go function
// that launches a bd binary calls beads.GuardBdLaunch, which stamps the
// GC_BD_DEPTH counter on the child and refuses at a fixed bound. This lint
// keeps a new launch site from silently skipping the fuse.
//
// The scan is deliberately syntactic. A function counts as a bd launch when it
// calls exec.Command or exec.CommandContext and mentions bd by the string
// literal "bd" or by an identifier that spells bd as a word (bdPath, execBd,
// customTypesBDCommand, BD_BIN). It must then call GuardBdLaunch somewhere in
// its body. The scan cannot see that the call precedes the launch or that the
// stamped env reaches it: the per-seam behavior tests own that. Launches whose
// binary arrives under an unrelated name are covered by the required-sites
// census below, not by the heuristic.

// bdWord matches an identifier that spells bd as a word: a bd prefix, or
// Bd/BD inside a camel-case name, followed by an uppercase letter, an
// underscore, a digit or the end of the name. "dbDir" and "Add" do not match.
var bdWord = regexp.MustCompile(`(^bd|Bd|BD)([A-Z_0-9]|$)`)

// bdLaunchFunc is one function that starts a subprocess.
type bdLaunchFunc struct {
	Name    string // "Recv.method" or "func"
	BdIsh   bool   // names bd as a literal or a word identifier
	Guarded bool   // calls GuardBdLaunch
}

// scanLaunchFuncs returns the functions of file that call exec.Command or
// exec.CommandContext.
func scanLaunchFuncs(file *ast.File) []bdLaunchFunc {
	var out []bdLaunchFunc
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		var f bdLaunchFunc
		f.Name = fn.Name.Name
		if fn.Recv != nil && len(fn.Recv.List) == 1 {
			t := fn.Recv.List[0].Type
			if star, ok := t.(*ast.StarExpr); ok {
				t = star.X
			}
			if id, ok := t.(*ast.Ident); ok {
				f.Name = id.Name + "." + f.Name
			}
		}
		launches := false
		ast.Inspect(fn, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BasicLit:
				if x.Kind == token.STRING && (x.Value == `"bd"` || x.Value == "`bd`") {
					f.BdIsh = true
				}
			case *ast.Ident:
				if bdWord.MatchString(x.Name) {
					f.BdIsh = true
				}
			case *ast.CallExpr:
				switch fun := x.Fun.(type) {
				case *ast.SelectorExpr:
					if pkg, ok := fun.X.(*ast.Ident); ok && pkg.Name == "exec" && (fun.Sel.Name == "Command" || fun.Sel.Name == "CommandContext") {
						launches = true
					}
					if fun.Sel.Name == "GuardBdLaunch" {
						f.Guarded = true
					}
				case *ast.Ident:
					if fun.Name == "GuardBdLaunch" {
						f.Guarded = true
					}
				}
			}
			return true
		})
		if launches {
			out = append(out, f)
		}
	}
	return out
}

// bdLaunchExemption lets a launch the heuristic flags stay unguarded, for a
// stated reason and only until a date, so an exemption cannot outlive the
// reason for it. Empty today: every bd launch is guarded.
type bdLaunchExemption struct {
	Reason  string
	Expires time.Time
}

var bdLaunchExemptions = map[string]bdLaunchExemption{}

// requiredBdLaunchSites are the launches of a bd binary that the heuristic
// cannot see, because the binary reaches them under a name that does not
// spell bd (probeBinaryVersion's path, dashboardbff's command argument), plus
// the launches the fuse's census named. Every one must exist and be guarded.
var requiredBdLaunchSites = []string{
	"cmd/gc/bd_inventory_trust.go:bdGuardRead",
	"cmd/gc/bd_inventory_trust.go:bdHelpRunnerFor",
	"cmd/gc/cmd_bd.go:doBd",
	"cmd/gc/cmd_bd_show_watch.go:bdWatchRunner.run",
	"internal/api/dashboardbff/exec.go:execRunner.run",
	"internal/beads/bdstore.go:execCommandRunner",
	"internal/beads/bdstore.go:execPurge",
	"internal/beads/binary_versions.go:probeBinaryVersion",
	"internal/doctor/checks_custom_types.go:customTypesBDCommand",
	"internal/doctor/checks_dolt_local_only.go:removeDoltRemote",
	"internal/doctor/checks_proxied_backup_coverage.go:readBdBackupStatus",
}

// bdLaunchViolations compares the launch functions found per file
// (key "rel/path.go") with the rules and returns one message per violation.
func bdLaunchViolations(found map[string][]bdLaunchFunc, now time.Time) []string {
	var out []string
	used := map[string]bool{}
	for path, funcs := range found {
		for _, f := range funcs {
			if !f.BdIsh || f.Guarded {
				continue
			}
			key := path + ":" + f.Name
			if ex, ok := bdLaunchExemptions[key]; ok {
				used[key] = true
				if !now.Before(ex.Expires) {
					out = append(out, key+": exemption expired "+ex.Expires.Format("2006-01-02")+" ("+ex.Reason+"); guard the launch")
				}
				continue
			}
			out = append(out, key+": launches a bd binary without calling GuardBdLaunch; route it through the internal/beads depth guard (ga-kihcmff)")
		}
	}
	for key := range bdLaunchExemptions {
		if !used[key] {
			out = append(out, key+": stale exemption, no unguarded bd launch matches it; delete it")
		}
	}
	for _, key := range requiredBdLaunchSites {
		path, name, _ := strings.Cut(key, ":")
		var site *bdLaunchFunc
		for i := range found[path] {
			if found[path][i].Name == name {
				site = &found[path][i]
			}
		}
		switch {
		case site == nil:
			out = append(out, key+": required bd launch site not found; if it moved, update requiredBdLaunchSites")
		case !site.Guarded:
			out = append(out, key+": required bd launch site does not call GuardBdLaunch")
		}
	}
	sort.Strings(out)
	return out
}

// scanSource scans in-memory source. The fixtures write CMD( for the
// exec package's Command call so that no literal subprocess launch of bd
// appears in this file: internal/beads/bdboundary greps test files for one.
func scanSource(t *testing.T, src string) []bdLaunchFunc {
	t.Helper()
	src = strings.ReplaceAll(src, "CMD(", "exec.Command(")
	file, err := parser.ParseFile(token.NewFileSet(), "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	return scanLaunchFuncs(file)
}

// TestBdLaunchSitesUseDepthGuard fails when a Go function launches a bd binary
// without calling beads.GuardBdLaunch.
func TestBdLaunchSitesUseDepthGuard(t *testing.T) {
	root := repoRoot(t)
	found := map[string][]bdLaunchFunc{}
	scanned := 0
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if skipRepoLintDir(path, root, d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			scanned++
			rel, _ := filepath.Rel(root, path)
			if funcs := scanLaunchFuncs(file); len(funcs) > 0 {
				found[filepath.ToSlash(rel)] = funcs
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", top, err)
		}
	}
	// An empty or broken walk must not pass as "nothing launches bd".
	if scanned < 200 {
		t.Fatalf("scanned only %d Go files under cmd/ and internal/, want the whole tree", scanned)
	}
	if v := bdLaunchViolations(found, time.Now()); len(v) > 0 {
		t.Fatalf("bd launch sites out of line with the recursion fuse:\n  %s", strings.Join(v, "\n  "))
	}
}

func TestBdLaunchScanClassifiesSources(t *testing.T) {
	cases := []struct {
		name        string
		src         string
		wantBdIsh   bool
		wantGuarded bool
	}{
		{"literal bd, unguarded", `package p
import "os/exec"
func Run() { CMD("bd", "list").Run() }`, true, false},
		{"bd identifier, guarded", `package p
import ("os/exec"; "x/beads")
func Run(bdPath string) { env, _ := beads.GuardBdLaunch(bdPath, nil); c := CMD(bdPath); c.Env = env }`, true, true},
		{"camel-case BD identifier", `package p
import "os/exec"
func customTypesBDCommand(bin string) { CMD(bin) }`, true, false},
		{"dbDir is not bd", `package p
import "os/exec"
func Sync(dbDir string) { CMD("dolt", dbDir) }`, false, false},
		{"Add is not bd", `package p
import "os/exec"
func Add() { CMD("git", "add") }`, false, false},
		{"bare call guarded", `package p
import "os/exec"
func Run() { GuardBdLaunch("bd", nil); CMD("bd") }`, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			funcs := scanSource(t, c.src)
			if len(funcs) != 1 {
				t.Fatalf("found %d launch funcs, want 1", len(funcs))
			}
			if funcs[0].BdIsh != c.wantBdIsh || funcs[0].Guarded != c.wantGuarded {
				t.Fatalf("got bdIsh=%v guarded=%v, want bdIsh=%v guarded=%v", funcs[0].BdIsh, funcs[0].Guarded, c.wantBdIsh, c.wantGuarded)
			}
		})
	}
	t.Run("methods are named with their receiver", func(t *testing.T) {
		funcs := scanSource(t, `package p
import "os/exec"
type R struct{}
func (r *R) run() { CMD("bd") }`)
		if len(funcs) != 1 || funcs[0].Name != "R.run" {
			t.Fatalf("got %+v, want one func named R.run", funcs)
		}
	})
	t.Run("a function that launches nothing is not listed", func(t *testing.T) {
		if funcs := scanSource(t, "package p\nfunc F() string { return \"bd\" }"); len(funcs) != 0 {
			t.Fatalf("got %+v, want none", funcs)
		}
	})
}

func TestBdLaunchViolationRules(t *testing.T) {
	// withRequired returns found plus every required site, guarded, so a case
	// can isolate the rule it tests.
	withRequired := func(found map[string][]bdLaunchFunc) map[string][]bdLaunchFunc {
		for _, key := range requiredBdLaunchSites {
			path, name, _ := strings.Cut(key, ":")
			found[path] = append(found[path], bdLaunchFunc{Name: name, BdIsh: true, Guarded: true})
		}
		return found
	}
	now := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)

	t.Run("clean tree passes", func(t *testing.T) {
		if v := bdLaunchViolations(withRequired(map[string][]bdLaunchFunc{}), now); len(v) != 0 {
			t.Fatalf("violations = %v, want none", v)
		}
	})
	t.Run("an unguarded bd launch fails", func(t *testing.T) {
		found := withRequired(map[string][]bdLaunchFunc{})
		found["internal/x/x.go"] = []bdLaunchFunc{{Name: "RunBD", BdIsh: true}}
		v := bdLaunchViolations(found, now)
		if len(v) != 1 || !strings.Contains(v[0], "internal/x/x.go:RunBD") {
			t.Fatalf("violations = %v, want exactly the unguarded RunBD", v)
		}
	})
	t.Run("an unguarded non-bd launch passes", func(t *testing.T) {
		found := withRequired(map[string][]bdLaunchFunc{})
		found["internal/x/x.go"] = []bdLaunchFunc{{Name: "RunGit"}}
		if v := bdLaunchViolations(found, now); len(v) != 0 {
			t.Fatalf("violations = %v, want none", v)
		}
	})
	t.Run("a missing required site fails", func(t *testing.T) {
		v := bdLaunchViolations(map[string][]bdLaunchFunc{}, now)
		if len(v) != len(requiredBdLaunchSites) {
			t.Fatalf("got %d violations, want one per required site (%d): %v", len(v), len(requiredBdLaunchSites), v)
		}
	})
	t.Run("an unguarded required site fails", func(t *testing.T) {
		found := withRequired(map[string][]bdLaunchFunc{})
		path, name, _ := strings.Cut(requiredBdLaunchSites[0], ":")
		for i := range found[path] {
			if found[path][i].Name == name {
				found[path][i].Guarded = false
			}
		}
		if v := bdLaunchViolations(found, now); len(v) == 0 {
			t.Fatal("an unguarded required site passed")
		}
	})
	t.Run("exemptions", func(t *testing.T) {
		old := bdLaunchExemptions
		t.Cleanup(func() { bdLaunchExemptions = old })
		found := withRequired(map[string][]bdLaunchFunc{})
		found["internal/x/x.go"] = []bdLaunchFunc{{Name: "RunBD", BdIsh: true}}

		bdLaunchExemptions = map[string]bdLaunchExemption{"internal/x/x.go:RunBD": {Reason: "test", Expires: now.Add(24 * time.Hour)}}
		if v := bdLaunchViolations(found, now); len(v) != 0 {
			t.Errorf("a live exemption still failed: %v", v)
		}
		bdLaunchExemptions = map[string]bdLaunchExemption{"internal/x/x.go:RunBD": {Reason: "test", Expires: now.Add(-24 * time.Hour)}}
		if v := bdLaunchViolations(found, now); len(v) != 1 || !strings.Contains(v[0], "expired") {
			t.Errorf("an expired exemption: violations = %v, want one naming the expiry", v)
		}
		bdLaunchExemptions = map[string]bdLaunchExemption{"internal/gone/gone.go:Old": {Reason: "test", Expires: now.Add(24 * time.Hour)}}
		if v := bdLaunchViolations(withRequired(map[string][]bdLaunchFunc{}), now); len(v) != 1 || !strings.Contains(v[0], "stale") {
			t.Errorf("a stale exemption: violations = %v, want one naming it stale", v)
		}
	})
}

// TestBdDepthEnvIsScrubbedBetweenTests keeps a counter leaked from an agent
// session (itself launched by gc bd) from tripping the fuse inside a test.
func TestBdDepthEnvIsScrubbedBetweenTests(t *testing.T) {
	for _, name := range testenv.LeakVectorVars {
		if name == "GC_BD_DEPTH" {
			return
		}
	}
	t.Fatal("GC_BD_DEPTH is not in testenv.LeakVectorVars; an inherited counter would make bd launches in tests refuse")
}
