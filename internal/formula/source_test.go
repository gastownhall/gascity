package formula

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestFSSourceMatchesLegacyBehavior asserts FSSource is a faithful
// translation of the pre-#2030 os.Stat / os.ReadFile / os.ReadDir
// calls Resolve and ParseFile used to make inline.
func TestFSSourceMatchesLegacyBehavior(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a.toml")
	if err := os.WriteFile(target, []byte(`formula = "x"`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	src := FSSource{}

	if !src.Stat(target) {
		t.Fatal("Stat reported existing file as absent")
	}
	if src.Stat(filepath.Join(dir, "missing.toml")) {
		t.Fatal("Stat reported missing file as present")
	}
	if src.Stat(sub) {
		t.Fatal("Stat reported a directory as a regular file")
	}

	data, err := src.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), `formula = "x"`) {
		t.Fatalf("ReadFile content unexpected: %q", string(data))
	}

	names, err := src.ListDir(dir)
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	sort.Strings(names)
	want := []string{"a.toml"}
	if !equalStringSlices(names, want) {
		t.Fatalf("ListDir = %v, want %v (sub/ directory must be filtered)", names, want)
	}
}

// TestGitRefSourceReadsCommittedStateNotWorkingTree is the headline
// regression test for #2030. A formula file edited on the working
// tree must not be visible through a GitRefSource pinned to the
// committed state at a stable ref.
func TestGitRefSourceReadsCommittedStateNotWorkingTree(t *testing.T) {
	gitOK(t)
	root := initRepo(t)

	formulaPath := filepath.Join(root, "mol.toml")
	commitFile(t, root, "mol.toml", `formula = "mol"`+"\n"+`[vars.x]`+"\n"+`default = "COMMITTED"`+"\n")
	commitOnBranch(t, root, "main", "initial mol")

	// Branch off and rewrite the working tree.
	runGit(t, root, "checkout", "-b", "feature")
	if err := os.WriteFile(formulaPath, []byte(`formula = "mol"`+"\n"+`[vars.x]`+"\n"+`default = "FEATURE-DRAFT"`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Do NOT commit — this is the "polecat edited in bd-managed worktree" state.

	src := NewGitRefSource("main")

	if !src.Stat(formulaPath) {
		t.Fatal("GitRefSource.Stat reported committed file as absent at main")
	}
	data, err := src.ReadFile(formulaPath)
	if err != nil {
		t.Fatalf("GitRefSource.ReadFile: %v", err)
	}
	if !strings.Contains(string(data), `default = "COMMITTED"`) {
		t.Fatalf("GitRefSource read working-tree state instead of committed state: %q", string(data))
	}
	if strings.Contains(string(data), "FEATURE-DRAFT") {
		t.Fatalf("GitRefSource leaked working-tree state through: %q", string(data))
	}
}

// TestGitRefSourceStatRejectsUncommittedFile asserts a file that
// exists in the working tree but was never committed at the ref is
// reported as absent. This is the symmetric property to the read
// case: the existence probe and the read must agree.
func TestGitRefSourceStatRejectsUncommittedFile(t *testing.T) {
	gitOK(t)
	root := initRepo(t)
	commitFile(t, root, "committed.toml", "x = 1\n")
	commitOnBranch(t, root, "main", "init")

	if err := os.WriteFile(filepath.Join(root, "uncommitted.toml"), []byte("y = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	src := NewGitRefSource("main")
	if !src.Stat(filepath.Join(root, "committed.toml")) {
		t.Fatal("Stat missed a committed file")
	}
	if src.Stat(filepath.Join(root, "uncommitted.toml")) {
		t.Fatal("Stat reported an uncommitted file as present at main")
	}
}

// TestGitRefSourceListDirReflectsRef asserts ListDir reads the tree
// at the configured ref, not the working tree. New files in the
// working tree must not appear; deleted-on-ref files must not appear
// even if the working tree restored them.
func TestGitRefSourceListDirReflectsRef(t *testing.T) {
	gitOK(t)
	root := initRepo(t)
	formulaDir := filepath.Join(root, "formulas")
	if err := os.MkdirAll(formulaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	commitFile(t, root, "formulas/a.toml", "x = 1\n")
	commitFile(t, root, "formulas/b.toml", "x = 2\n")
	commitOnBranch(t, root, "main", "two formulas")

	// Working-tree-only addition; must NOT appear via ref read.
	if err := os.WriteFile(filepath.Join(formulaDir, "c.toml"), []byte("z = 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	src := NewGitRefSource("main")
	names, err := src.ListDir(formulaDir)
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	sort.Strings(names)
	want := []string{"a.toml", "b.toml"}
	if !equalStringSlices(names, want) {
		t.Fatalf("ListDir at main = %v, want %v (working-tree-only c.toml must not appear)", names, want)
	}
}

// TestGitRefSourceStatRejectsCommittedDirectory is the regression
// test for the PR #2537 Copilot finding: Stat must report `false` for
// directories (subtrees in git) per the Source contract — even when
// the tree at the configured ref contains a directory by that name.
// Earlier the implementation used `git cat-file -e` which succeeds
// for trees as well as blobs, leaking subtrees through.
func TestGitRefSourceStatRejectsCommittedDirectory(t *testing.T) {
	gitOK(t)
	root := initRepo(t)
	commitFile(t, root, "sub/inner.toml", "x = 1\n")
	commitOnBranch(t, root, "main", "init with subdir")

	src := NewGitRefSource("main")
	subDir := filepath.Join(root, "sub")
	if src.Stat(subDir) {
		t.Fatal("Stat reported a committed directory as a regular file; must filter to blobs only")
	}
	if !src.Stat(filepath.Join(subDir, "inner.toml")) {
		t.Fatal("Stat missed a committed blob under a subdirectory")
	}
}

// TestGitRefSourceListDirSkipsSubtrees is the regression test for the
// PR #2537 Copilot finding: ListDir must filter subdirectories out
// of its results, not return them alongside blob entries. Earlier
// the implementation used `git ls-tree --name-only` which emits
// subtree names without a type distinction.
func TestGitRefSourceListDirSkipsSubtrees(t *testing.T) {
	gitOK(t)
	root := initRepo(t)
	commitFile(t, root, "formulas/a.toml", "x = 1\n")
	commitFile(t, root, "formulas/sub/inner.toml", "y = 2\n")
	commitOnBranch(t, root, "main", "mixed entries")

	src := NewGitRefSource("main")
	names, err := src.ListDir(filepath.Join(root, "formulas"))
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	sort.Strings(names)
	want := []string{"a.toml"}
	if !equalStringSlices(names, want) {
		t.Fatalf("ListDir = %v, want %v (subtree 'sub' must not appear in results)", names, want)
	}
}

// TestGitRefSourceMissOutsideRepo asserts paths outside any git repo
// surface as misses (callers fall back via FallbackSource or report
// not-found). This must NOT panic or hang.
func TestGitRefSourceMissOutsideRepo(t *testing.T) {
	gitOK(t)
	dir := t.TempDir() // No git init.
	if err := os.WriteFile(filepath.Join(dir, "loose.toml"), []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := NewGitRefSource("main")
	if src.Stat(filepath.Join(dir, "loose.toml")) {
		t.Fatal("Stat reported a non-git path as present")
	}
	if _, err := src.ReadFile(filepath.Join(dir, "loose.toml")); err == nil {
		t.Fatal("ReadFile of a non-git path should error")
	}
}

// TestFallbackSourceComposesPrimaryThenFallback asserts the read path
// honors the primary when it claims a hit and drops to the fallback
// on miss. ListDir unions both sources, primary names first.
func TestFallbackSourceComposesPrimaryThenFallback(t *testing.T) {
	primary := stubSource{
		stat: map[string]bool{"/p/only.toml": true, "/shared/x.toml": true},
		read: map[string][]byte{"/p/only.toml": []byte("from-primary"), "/shared/x.toml": []byte("primary-wins")},
		list: map[string][]string{"/dir": {"primary.toml", "shared.toml"}},
	}
	fallback := stubSource{
		stat: map[string]bool{"/f/only.toml": true, "/shared/x.toml": true},
		read: map[string][]byte{"/f/only.toml": []byte("from-fallback"), "/shared/x.toml": []byte("fallback-loses")},
		list: map[string][]string{"/dir": {"shared.toml", "fallback.toml"}},
	}
	f := FallbackSource{Primary: primary, Fallback: fallback}

	cases := []struct {
		path string
		want string
	}{
		{"/p/only.toml", "from-primary"},
		{"/f/only.toml", "from-fallback"},
		{"/shared/x.toml", "primary-wins"},
	}
	for _, tc := range cases {
		got, err := f.ReadFile(tc.path)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", tc.path, err)
		}
		if string(got) != tc.want {
			t.Errorf("ReadFile(%s) = %q, want %q", tc.path, string(got), tc.want)
		}
	}

	names, err := f.ListDir("/dir")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	want := []string{"primary.toml", "shared.toml", "fallback.toml"}
	if !equalStringSlices(names, want) {
		t.Fatalf("ListDir = %v, want %v (primary order first, de-dup)", names, want)
	}
}

// TestSourceFromEnvUnsetReturnsFSSource asserts the env-var default
// preserves working-tree behavior. This is the safety invariant: no
// behavior change for any caller that does not opt in.
func TestSourceFromEnvUnsetReturnsFSSource(t *testing.T) {
	for _, val := range []string{"", "working-tree", "HEAD"} {
		t.Run("GC_FORMULA_REF="+val, func(t *testing.T) {
			t.Setenv("GC_FORMULA_REF", val)
			src := SourceFromEnv()
			if _, ok := src.(FSSource); !ok {
				t.Fatalf("SourceFromEnv=%q returned %T, want FSSource", val, src)
			}
		})
	}
}

// TestSourceFromEnvWithRefReturnsRepoAwareFallback asserts a non-
// sentinel env value produces a repo-aware fallback wrapping a
// GitRefSource. The composition must preserve ref-stability for
// paths inside a git repo while letting out-of-repo paths (user
// dirs, ad-hoc test dirs) fall back to the filesystem.
func TestSourceFromEnvWithRefReturnsRepoAwareFallback(t *testing.T) {
	t.Setenv("GC_FORMULA_REF", "main")
	src := SourceFromEnv()
	fb, ok := src.(gitRepoAwareFallback)
	if !ok {
		t.Fatalf("SourceFromEnv returned %T, want gitRepoAwareFallback", src)
	}
	if fb.git == nil || fb.git.Ref() != "main" {
		t.Fatalf("primary ref = %v, want a GitRefSource pinned to %q", fb.git, "main")
	}
}

// TestGitRepoAwareFallbackPreservesRefStabilityForInRepoPaths is the
// regression test for the PR #2537 Copilot finding on
// FallbackSource: an uncommitted file inside the git repo must NOT
// be visible via the env-built Source, because the documented
// contract is "ref-stable for in-repo paths". The previous
// FallbackSource composition leaked uncommitted state through.
func TestGitRepoAwareFallbackPreservesRefStabilityForInRepoPaths(t *testing.T) {
	gitOK(t)
	root := initRepo(t)
	commitFile(t, root, "committed.toml", "x = 1\n")
	commitOnBranch(t, root, "main", "init")

	// Working-tree-only file inside the repo — must be INVISIBLE
	// through the env-built Source.
	if err := os.WriteFile(filepath.Join(root, "uncommitted.toml"), []byte("y = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_FORMULA_REF", "main")
	src := SourceFromEnv()

	if !src.Stat(filepath.Join(root, "committed.toml")) {
		t.Fatal("ref-pinned Source missed a committed file")
	}
	if src.Stat(filepath.Join(root, "uncommitted.toml")) {
		t.Fatal("ref-pinned Source leaked an uncommitted in-repo file via working-tree fallback")
	}
}

// TestGitRepoAwareFallbackUsesFilesystemForOutOfRepoPaths asserts the
// fallback path: a file outside any git repo must remain reachable
// via FSSource semantics, so user-level formula layers
// (~/.beads/formulas) and ad-hoc test directories keep working
// under opt-in GC_FORMULA_REF.
func TestGitRepoAwareFallbackUsesFilesystemForOutOfRepoPaths(t *testing.T) {
	gitOK(t)
	outsideDir := t.TempDir()
	loose := filepath.Join(outsideDir, "loose.toml")
	if err := os.WriteFile(loose, []byte("z = 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_FORMULA_REF", "main")
	src := SourceFromEnv()

	if !src.Stat(loose) {
		t.Fatal("ref-pinned Source missed an out-of-repo file (FS fallback should apply)")
	}
	data, err := src.ReadFile(loose)
	if err != nil {
		t.Fatalf("ReadFile out-of-repo path: %v", err)
	}
	if string(data) != "z = 3\n" {
		t.Fatalf("out-of-repo ReadFile = %q, want %q", string(data), "z = 3\n")
	}
}

// TestParserHonorsSetSourceForLoadByName is the end-to-end test that
// proves the read path of loadFormula honors the configured Source.
// This is the test that would have caught the bug originally — a
// working-tree edit must not leak through when the Source is pinned
// to a committed ref.
func TestParserHonorsSetSourceForLoadByName(t *testing.T) {
	gitOK(t)
	root := initRepo(t)
	formulaDir := filepath.Join(root, "formulas")
	if err := os.MkdirAll(formulaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	commitFile(t, root, "formulas/mol.toml", "formula = \"mol\"\n[vars.x]\ndefault = \"COMMITTED\"\n")
	commitOnBranch(t, root, "main", "init")

	// Working-tree edit on a feature branch.
	runGit(t, root, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(formulaDir, "mol.toml"), []byte("formula = \"mol\"\n[vars.x]\ndefault = \"FEATURE-DRAFT\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// FS-backed parser sees the working-tree draft (current behavior).
	fsParser := NewParser(formulaDir)
	fsFormula, err := fsParser.LoadByName("mol")
	if err != nil {
		t.Fatalf("FSSource LoadByName: %v", err)
	}
	if got := derefString(fsFormula.Vars["x"].Default); got != "FEATURE-DRAFT" {
		t.Fatalf("FSSource read = %q, want FEATURE-DRAFT (working-tree)", got)
	}

	// Ref-pinned parser sees the committed state regardless of branch.
	refParser := NewParser(formulaDir).SetSource(NewGitRefSource("main"))
	refFormula, err := refParser.LoadByName("mol")
	if err != nil {
		t.Fatalf("GitRefSource LoadByName: %v", err)
	}
	if got := derefString(refFormula.Vars["x"].Default); got != "COMMITTED" {
		t.Fatalf("GitRefSource read = %q, want COMMITTED (ref-stable)", got)
	}
}

func TestParserHonorsSetSourceForDescriptionFile(t *testing.T) {
	gitOK(t)
	root := initRepo(t)
	formulaDir := filepath.Join(root, "formulas")
	if err := os.MkdirAll(filepath.Join(formulaDir, "descriptions"), 0o755); err != nil {
		t.Fatal(err)
	}
	commitFile(t, root, "formulas/mol.toml", "formula = \"mol\"\n\n[[steps]]\nid = \"work\"\ntitle = \"Work\"\ndescription_file = \"descriptions/work.md\"\n")
	commitFile(t, root, "formulas/descriptions/work.md", "COMMITTED")
	commitOnBranch(t, root, "main", "init")

	runGit(t, root, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(formulaDir, "descriptions", "work.md"), []byte("FEATURE-DRAFT"), 0o644); err != nil {
		t.Fatal(err)
	}

	fsParser := NewParser(formulaDir)
	fsFormula, err := fsParser.LoadByName("mol")
	if err != nil {
		t.Fatalf("FSSource LoadByName: %v", err)
	}
	if got := fsFormula.Steps[0].Description; got != "FEATURE-DRAFT" {
		t.Fatalf("FSSource description = %q, want FEATURE-DRAFT", got)
	}

	refParser := NewParser(formulaDir).SetSource(NewGitRefSource("main"))
	refFormula, err := refParser.LoadByName("mol")
	if err != nil {
		t.Fatalf("GitRefSource LoadByName: %v", err)
	}
	if got := refFormula.Steps[0].Description; got != "COMMITTED" {
		t.Fatalf("GitRefSource description = %q, want COMMITTED", got)
	}
}

// TestResolveWithSourcePrecedence verifies the layered last-wins
// semantic is preserved when Source is parameterized. Highest-priority
// layer that contains a Stat-hit wins, exactly as the legacy Resolve
// did.
func TestResolveWithSourcePrecedence(t *testing.T) {
	src := stubSource{
		stat: map[string]bool{
			"/low/mol.toml":  true,
			"/mid/mol.toml":  true,
			"/high/mol.toml": true,
		},
	}
	layers := []string{"/low", "/mid", "/high"}
	got, ok := ResolveWithSource(src, layers, "mol")
	if !ok {
		t.Fatal("ResolveWithSource missed")
	}
	if got != "/high/mol.toml" {
		t.Fatalf("got %q, want /high/mol.toml (highest-priority wins)", got)
	}

	// Highest-priority layer absent → next-highest wins.
	src.stat["/high/mol.toml"] = false
	got, ok = ResolveWithSource(src, layers, "mol")
	if !ok {
		t.Fatal("ResolveWithSource missed after dropping high layer")
	}
	if got != "/mid/mol.toml" {
		t.Fatalf("got %q, want /mid/mol.toml (fallthrough)", got)
	}
}

// --- helpers ---

func gitOK(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available in PATH")
	}
}

func initRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	runGit(t, root, "config", "user.email", "test@example.com")
	runGit(t, root, "config", "user.name", "test")
	runGit(t, root, "config", "commit.gpgsign", "false")
	return root
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func commitFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", rel)
}

func commitOnBranch(t *testing.T, root, _, msg string) {
	t.Helper()
	runGit(t, root, "commit", "-m", msg)
}

type stubSource struct {
	stat map[string]bool
	read map[string][]byte
	list map[string][]string
}

func (s stubSource) Stat(path string) bool { return s.stat[path] }

func (s stubSource) ReadFile(path string) ([]byte, error) {
	if b, ok := s.read[path]; ok {
		return b, nil
	}
	return nil, errors.New("not found")
}

func (s stubSource) ListDir(dir string) ([]string, error) {
	if l, ok := s.list[dir]; ok {
		return l, nil
	}
	return nil, errors.New("not found")
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
