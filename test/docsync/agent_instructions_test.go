package docsync

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
)

// rootAgentsBudgetBytes caps the always-loaded root AGENTS.md. Codex stops
// reading project instructions at 32 KiB combined (user file + every
// AGENTS.md from the repo root down), so the root file must leave room for
// the nested files below it.
const rootAgentsBudgetBytes = 20000

// agentInstructionProductDirs hold AGENTS.md files that ship to users' agents
// as pack or provider overlays. They are product assets, not contributor
// guidance, so the contributor-file rules below do not apply to them.
var agentInstructionProductDirs = []string{"internal/bootstrap/packs"}

// agentPathRefRE matches backticked repo-relative paths in agent instruction
// files. Globs and brace sets are not paths and are skipped by the caller.
var agentPathRefRE = regexp.MustCompile("`((?:\\.github|\\.githooks|cmd|contrib|docs|engdocs|examples|internal|scripts|specs|test|tools)/[^`\\s]*)`")

// agentTestRefRE matches backticked Go test names cited as guards.
var agentTestRefRE = regexp.MustCompile("`(Test[A-Z][A-Za-z0-9_]*)`")

var goTestFuncRE = regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)

func TestRootAgentsFileWithinBudget(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(), "AGENTS.md"))
	if err != nil {
		t.Fatalf("reading root AGENTS.md: %v", err)
	}
	if len(data) > rootAgentsBudgetBytes {
		t.Errorf("root AGENTS.md is %d bytes, budget is %d. Move area-specific rules "+
			"verbatim into the owning directory's AGENTS.md and add a row to the "+
			"\"Read first\" table instead of growing the root file.", len(data), rootAgentsBudgetBytes)
	}
}

func TestNestedAgentsFilesHaveClaudeSymlink(t *testing.T) {
	root := repoRoot()
	for _, rel := range nestedContributorAgentsFiles(t, root) {
		dir := filepath.Dir(filepath.Join(root, rel))
		claude := filepath.Join(dir, "CLAUDE.md")
		// Claude Code skips a nested AGENTS.md whenever a CLAUDE.md exists at
		// or above the working directory, which the repo root always has. A
		// sibling CLAUDE.md symlink is what makes it load the nested rules.
		if bazeltest.IsBazel() {
			// Runfiles materialize inputs as links into the execroot or as
			// plain copies, so the link itself is not observable here; the
			// content must still be identical.
			want, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
			if err != nil {
				t.Fatalf("reading %s: %v", rel, err)
			}
			got, err := os.ReadFile(claude)
			if err != nil {
				t.Errorf("%s has no sibling CLAUDE.md; add `ln -s AGENTS.md CLAUDE.md` in %s", rel, filepath.Dir(rel))
				continue
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s/CLAUDE.md differs from AGENTS.md; it must be a symlink to AGENTS.md", filepath.Dir(rel))
			}
			continue
		}
		info, err := os.Lstat(claude)
		if err != nil {
			t.Errorf("%s has no sibling CLAUDE.md; add `ln -s AGENTS.md CLAUDE.md` in %s", rel, filepath.Dir(rel))
			continue
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s/CLAUDE.md is a regular file; replace it with `ln -s AGENTS.md CLAUDE.md`", filepath.Dir(rel))
			continue
		}
		target, err := os.Readlink(claude)
		if err != nil {
			t.Fatalf("reading link %s: %v", claude, err)
		}
		if target != "AGENTS.md" {
			t.Errorf("%s/CLAUDE.md links to %q, want %q", filepath.Dir(rel), target, "AGENTS.md")
		}
	}
}

func TestRootAgentsRoutesEveryNestedAgentsFile(t *testing.T) {
	root := repoRoot()
	data, err := os.ReadFile(filepath.Join(root, "AGENTS.md"))
	if err != nil {
		t.Fatalf("reading root AGENTS.md: %v", err)
	}
	// Codex never loads AGENTS.md files below the directory it starts in, so
	// the root "Read first" table is the only route to them for some agents.
	for _, rel := range nestedContributorAgentsFiles(t, root) {
		if !bytes.Contains(data, []byte("`"+rel+"`")) {
			t.Errorf("root AGENTS.md does not route to %s; add a row to its \"Read first\" table", rel)
		}
	}
}

func TestAgentInstructionReferencesResolve(t *testing.T) {
	root := repoRoot()
	files := append([]string{"AGENTS.md"}, nestedContributorAgentsFiles(t, root)...)
	tests := goTestFuncNames(t, root)

	var broken []string
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		text := string(data)
		for _, m := range agentPathRefRE.FindAllStringSubmatch(text, -1) {
			ref := m[1]
			if strings.ContainsAny(ref, "*{}<>$") {
				continue
			}
			// "file.go:Symbol()" cites a symbol inside a file; check the file.
			ref, _, _ = strings.Cut(ref, ":")
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(ref))); err != nil {
				broken = append(broken, rel+": path `"+ref+"` does not exist")
			}
		}
		for _, target := range extractMarkdownLinks(text) {
			if isExternalLink(target) || strings.HasPrefix(target, "#") {
				continue
			}
			target, _, _ = strings.Cut(target, "#")
			resolved := filepath.Join(filepath.Dir(filepath.Join(root, rel)), filepath.FromSlash(target))
			if _, err := os.Stat(resolved); err != nil {
				broken = append(broken, rel+": link "+target+" does not resolve")
			}
		}
		for _, m := range agentTestRefRE.FindAllStringSubmatch(text, -1) {
			if !tests[m[1]] {
				broken = append(broken, rel+": guard test "+m[1]+" does not exist")
			}
		}
	}
	sort.Strings(broken)
	for _, b := range broken {
		t.Error(b)
	}
}

// nestedContributorAgentsFiles returns repo-relative paths of every
// AGENTS.md below the root that carries contributor guidance.
func nestedContributorAgentsFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if path == root {
				return nil
			}
			if skipScanDir(root, path, d.Name()) || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			for _, p := range agentInstructionProductDirs {
				if rel == p {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if d.Name() == "AGENTS.md" && rel != "AGENTS.md" {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking repo for AGENTS.md: %v", err)
	}
	sort.Strings(out)
	return out
}

// skipScanDir reports whether a whole-repo walk should skip the directory at
// path. Hidden and dependency directories are skipped at any depth; the
// scratch, worktree, and session-scaffold heuristics apply only to top-level
// entries, because tests create runtime state such as cmd/gc/.gc inside
// source packages and those packages must still be scanned.
func skipScanDir(root, path, name string) bool {
	if strings.HasPrefix(name, ".") || name == "node_modules" {
		return true
	}
	if filepath.Dir(path) != root {
		return false
	}
	return isBeadScratchRoot(name) || isNestedWorktreeRoot(path) || isSessionScaffoldRoot(path)
}

// goTestFuncNames returns the names of every top-level Test function in the
// repository's Go test files.
func goTestFuncNames(t *testing.T, root string) map[string]bool {
	t.Helper()
	names := make(map[string]bool)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && skipScanDir(root, path, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range goTestFuncRE.FindAllSubmatch(data, -1) {
			names[string(m[1])] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("collecting Go test names: %v", err)
	}
	return names
}
