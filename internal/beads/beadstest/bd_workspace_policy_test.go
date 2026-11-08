package beadstest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
)

// realBDTiers are the repository trees whose tests run a real bd binary
// against a directory the test made. They are the tiers GuardedBdWorkspaceDir
// exists for: a real bd walks up from its working directory toward / for a
// .beads, so a workspace dir that is not its own git root inherits whatever
// .beads sits above it (ga-l7otw9, ga-vevy46).
var realBDTiers = []string{
	"cmd/gc",
	"test/integration",
	"test/acceptance",
}

// notABdWorkspaceGap lists functions the scan flags that were read and are not
// a gap, each with the reason. The scan sees one function at a time, so a dir
// bounded in another function (a fixture builder) looks unbounded here.
var notABdWorkspaceGap = map[string]string{
	"test/acceptance/beads_proxied_default_test.go: TestBeadsProxiedDefaultRig": "every bd init dir is a rig from newProxiedDefaultCity, " +
		"made by createNamedGitRig (a git root); the temp dirs in the function are a bin dir and an unrelated clone target",
}

// TestRealBDTiersMakeBdWorkspaceDirsTheirOwnGitRoot pins the rule behind
// GuardedBdWorkspaceDir for the real-bd tiers: a function that makes a temp dir
// and reaches a `bd init` must take the dir from GuardedBdWorkspaceDir (or make
// it a git root itself). GuardedTempDir and t.TempDir do not bound bd's walk.
//
// The scan is static and name-based, so it is a ratchet and not a proof: a dir
// handed in from another package's helper is not seen. The decoy-above check
// inside GuardedBdWorkspaceDir is what catches an escape at run time.
func TestRealBDTiersMakeBdWorkspaceDirsTheirOwnGitRoot(t *testing.T) {
	root := bazeltest.RepoRoot(t)
	sources := map[string][]byte{}
	for _, tier := range realBDTiers {
		base := filepath.Join(root, filepath.FromSlash(tier))
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			sources[filepath.ToSlash(rel)] = data
			return nil
		})
		if err != nil {
			t.Fatalf("scan %s: %v", tier, err)
		}
	}
	if len(sources) == 0 {
		t.Fatalf("scanned no Go files under %v: a go_test data dep is missing, so the policy would pass on nothing", realBDTiers)
	}
	for _, v := range unboundedBdWorkspaceDirs(t, sources) {
		if _, ok := notABdWorkspaceGap[v]; ok {
			continue
		}
		t.Errorf("%s: builds a bd workspace dir with GuardedTempDir/t.TempDir and reaches `bd init`; "+
			"use beadstest.GuardedBdWorkspaceDir so the dir is its own git root", v)
	}
}

func TestUnboundedBdWorkspaceDirsFlagsAndExempts(t *testing.T) {
	const header = "package p\n\nimport \"testing\"\n\n"
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{
			name: "guarded temp dir runs bd init directly",
			src: header + `func TestA(t *testing.T) {
	dir := beadstest.GuardedTempDir(t)
	runExternal(t, dir, bdPath, "init", "-p", "ct")
}`,
			want: []string{"a_test.go: TestA"},
		},
		{
			name: "t.TempDir reaches bd init through a helper",
			src: header + `func TestA(t *testing.T) {
	dir := t.TempDir()
	runBDInit(t, dir)
}

func runBDInit(t *testing.T, dir string) {
	exec.Command(bdBinary, "init", "--server")
}`,
			want: []string{"a_test.go: TestA"},
		},
		{
			name: "guarded bd workspace dir is exempt",
			src: header + `func TestA(t *testing.T) {
	dir := beadstest.GuardedBdWorkspaceDir(t)
	runExternal(t, dir, bdPath, "init", "-p", "ct")
}`,
		},
		{
			name: "a test that runs git init first is exempt",
			src: header + `func TestA(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "git", "init")
	runExternal(t, dir, bdPath, "init")
}`,
		},
		{
			name: "gc init is not a bd workspace",
			src: header + `func TestA(t *testing.T) {
	dir := t.TempDir()
	runExternal(t, dir, gcBin, "init", dir)
}`,
		},
		{
			name: "requireBD wrapper with an init literal is a bd init",
			src: header + `func TestA(t *testing.T) {
	dir := beadstest.GuardedTempDir(t)
	requireBD(t, dir, "init", "-p", "ct")
}`,
			want: []string{"a_test.go: TestA"},
		},
		{
			name: "argv slice starting with init, run as bd",
			src: header + `func TestA(t *testing.T) {
	dir := t.TempDir()
	args := []string{"init", "--server"}
	exec.Command(bdBinary, args...)
}`,
			want: []string{"a_test.go: TestA"},
		},
		{
			name: "a fixture that runs git init through an argv slice is exempt",
			src: header + `func rigWorkspace(t *testing.T) string {
	dir := t.TempDir()
	exec.Command("git", []string{"init", dir}...)
	return dir
}

func TestA(t *testing.T) {
	dir := t.TempDir()
	rigWorkspace(t)
	runExternal(t, dir, bdPath, "init")
}`,
		},
		{
			name: "a stub bd is not the real bd",
			src: header + `func TestA(t *testing.T) {
	scope := t.TempDir()
	bd := fakeInitBD{}.install(t)
	runWrapper(t, bd, scope, "init")
}`,
		},
		{
			name: "a temp dir with no bd init is not flagged",
			src: header + `func TestA(t *testing.T) {
	dir := t.TempDir()
	_ = dir
}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := unboundedBdWorkspaceDirs(t, map[string][]byte{"a_test.go": []byte(tc.src)})
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("violations = %q, want %q", got, tc.want)
			}
		})
	}
}

// bdArgName matches the identifiers a bd binary travels under in call
// arguments: bdPath, bdBinary, bd, realBD, ...
var bdArgName = regexp.MustCompile(`(?i)^(bd|bd[a-z]*(path|bin|binary)|real_?bd[a-z]*|[a-z]*bd(path|bin|binary))$`)

// bdWrapperName matches the test helpers that run the real bd with the dir and
// argv they are given: requireBD, runBD, bdRun, bdRunWithEnv, ...
var bdWrapperName = regexp.MustCompile(`(?i)^((require|run|must)bd|bdrun)(with[a-z]*)?$`)

// dirParamName matches the parameter names a directory travels under:
// dir, wsDir, workspace, rigDir, root, cwd.
var dirParamName = regexp.MustCompile(`(?i)(dir|workspace|^ws|root|cwd)$`)

// fnFacts is what the scan records about one function declaration.
type fnFacts struct {
	label        string // "<file>: <Func>"
	name         string // the bare function name
	dirSource    bool   // calls GuardedTempDir, t.TempDir or a TempDir helper
	bounded      bool   // takes GuardedBdWorkspaceDir or makes a git root itself
	makesGitRoot bool   // runs git init itself
	bdInit       bool   // runs a call with a bd-looking argument then "init"
	mentionBD    bool   // names a bd-looking identifier anywhere
	initArgs     bool   // builds an argv slice that starts with "init"
	takesDir     bool   // has a parameter that names a directory
	gitLit       bool   // has a "git" string literal (exec.Command("git", ...))
	fakeBD       bool   // names a fake* identifier: the bd under test is a stub
	calls        map[string]bool
}

// unboundedBdWorkspaceDirs returns "<file>: <Func>" for every function that
// makes a temp dir without bounding it and reaches a bd init, directly or
// through any chain of functions by name. A function that bounds its dir does
// not pass the reach on to its callers: it has already handled the dir.
func unboundedBdWorkspaceDirs(t testing.TB, sources map[string][]byte) []string {
	t.Helper()
	fset := token.NewFileSet()
	var fns []*fnFacts
	files := make([]string, 0, len(sources))
	for name := range sources {
		files = append(files, name)
	}
	sort.Strings(files)
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, sources[name], parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			facts := scanFunc(fd)
			facts.label = name + ": " + fd.Name.Name
			facts.name = fd.Name.Name
			fns = append(fns, facts)
		}
	}

	// A function that calls a git-root maker (gitInitWorkspace, createGitRig, ...)
	// hands its dir to that helper, so it is bounded too.
	gitRoot := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for _, fn := range fns {
			if gitRoot[fn.name] {
				continue
			}
			hit := fn.makesGitRoot
			for callee := range fn.calls {
				if gitRoot[callee] {
					hit = true
				}
			}
			if hit {
				gitRoot[fn.name] = true
				fn.bounded = true
				changed = true
			}
		}
	}

	// reaches holds the dir-taking helpers that end in a bd init: functions
	// that take a dir and run a bd init, or call such a function, and make no
	// temp dir themselves. A function
	// that makes its own dir is a test or fixture, reported below, and the
	// reach does not pass through it.
	reaches := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for _, fn := range fns {
			if fn.bounded || fn.dirSource || !fn.takesDir {
				continue
			}
			if reaches[fn.name] {
				continue
			}
			hit := fn.runsBdInit()
			for callee := range fn.calls {
				if reaches[callee] {
					hit = true
				}
			}
			if hit {
				reaches[fn.name] = true
				changed = true
			}
		}
	}

	var out []string
	for _, fn := range fns {
		if !fn.dirSource || fn.bounded {
			continue
		}
		hit := fn.runsBdInit()
		for callee := range fn.calls {
			if reaches[callee] {
				hit = true
			}
		}
		if hit {
			out = append(out, fn.label)
		}
	}
	return out
}

// runsBdInit reports whether the function runs a bd init itself: either one
// call carrying a bd binary then "init", or an argv slice starting with "init"
// in a function that names a bd binary (the exec.Command(bdBinary, args...)
// shape).
func (f *fnFacts) runsBdInit() bool {
	if f.fakeBD {
		return false
	}
	return f.bdInit || (f.mentionBD && f.initArgs)
}

// scanFunc records the facts of one declaration, including its closures.
func scanFunc(fd *ast.FuncDecl) *fnFacts {
	facts := &fnFacts{calls: map[string]bool{}}
	for _, field := range fd.Type.Params.List {
		for _, id := range field.Names {
			if dirParamName.MatchString(id.Name) {
				facts.takesDir = true
			}
		}
	}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			if n.Sel.Name == "GuardedBdWorkspaceDir" {
				facts.bounded = true
			}
		case *ast.Ident:
			if n.Name == "GuardedBdWorkspaceDir" {
				facts.bounded = true
			}
			if bdArgName.MatchString(n.Name) {
				facts.mentionBD = true
			}
			if strings.HasPrefix(strings.ToLower(n.Name), "fake") {
				facts.fakeBD = true
			}
		case *ast.CompositeLit:
			if len(n.Elts) > 0 {
				if lit, ok := n.Elts[0].(*ast.BasicLit); ok && lit.Kind == token.STRING && lit.Value == `"init"` {
					facts.initArgs = true
				}
			}
		case *ast.BasicLit:
			if n.Kind == token.STRING && n.Value == `"git"` {
				facts.gitLit = true
			}
		case *ast.CallExpr:
			name := calleeName(n.Fun)
			if name != "" {
				facts.calls[name] = true
			}
			switch name {
			case "GuardedTempDir", "TempDir":
				facts.dirSource = true
			}
			if gitInitCall(n, name) {
				facts.makesGitRoot = true
				facts.bounded = true
			}
			if bdInitCall(n, name) {
				facts.bdInit = true
			}
		}
		return true
	})
	if facts.initArgs && facts.gitLit {
		// {"init", dir} run as exec.Command("git", args...): a git init.
		facts.makesGitRoot = true
		facts.bounded = true
	}
	return facts
}

// calleeName is the called function's bare name: f for f(), x.f for x.f().
func calleeName(fun ast.Expr) string {
	switch fun := fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		return fun.Sel.Name
	}
	return ""
}

// stringArgs returns the unquoted string-literal arguments of a call.
func stringArgs(call *ast.CallExpr) []string {
	var out []string
	for _, arg := range call.Args {
		if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if s, err := strconv.Unquote(lit.Value); err == nil {
				out = append(out, s)
			}
		}
	}
	return out
}

// gitInitCall reports whether call runs `git init`: an "init" literal argument
// on a call that names git, by its callee (runGit, gitInitWorkspace), a "git"
// literal, or a git-looking identifier argument.
func gitInitCall(call *ast.CallExpr, callee string) bool {
	if !hasLit(stringArgs(call), "init") {
		return false
	}
	if strings.Contains(strings.ToLower(callee), "git") || hasLit(stringArgs(call), "git") {
		return true
	}
	for _, arg := range call.Args {
		if id, ok := arg.(*ast.Ident); ok && strings.Contains(strings.ToLower(id.Name), "git") {
			return true
		}
	}
	return false
}

func hasLit(lits []string, want string) bool {
	for _, l := range lits {
		if l == want {
			return true
		}
	}
	return false
}

// bdInitCall reports whether call passes a bd binary argument followed by an
// "init" literal, or is a bd-named wrapper given one, the shape of every real
// `bd init` in the tiers (exec.Command(bdBinary, "init", ...),
// runExternal(t, dir, bdPath, "init"), bdRun(t, bdPath, dir, "init"),
// requireBD(t, dir, "init")).
func bdInitCall(call *ast.CallExpr, callee string) bool {
	// requireBD(t, dir, "init", ...) and bdRun(...) carry the binary in the callee.
	sawBD := bdWrapperName.MatchString(callee)
	for _, arg := range call.Args {
		switch arg := arg.(type) {
		case *ast.Ident:
			if bdArgName.MatchString(arg.Name) {
				sawBD = true
			}
		case *ast.SelectorExpr:
			if bdArgName.MatchString(arg.Sel.Name) {
				sawBD = true
			}
		case *ast.BasicLit:
			if arg.Kind != token.STRING {
				continue
			}
			s, err := strconv.Unquote(arg.Value)
			if err != nil {
				continue
			}
			if s == "bd" {
				sawBD = true
			}
			if s == "init" && sawBD {
				return true
			}
		}
	}
	return false
}
