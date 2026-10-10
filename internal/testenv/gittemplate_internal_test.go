package testenv

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// newTestTemplate writes a git template under a fresh root and returns both.
func newTestTemplate(t *testing.T) (root, dir string) {
	t.Helper()
	root = t.TempDir()
	dir, err := ensureGitTemplate(root)
	if err != nil {
		t.Fatalf("ensureGitTemplate(%q) on a fresh root: %v", root, err)
	}
	return root, dir
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("no symlinks on this platform: %v", err)
	}
}

func mustRemoveAll(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("RemoveAll(%q): %v", path, err)
	}
}

// assertTemplateRefused fails unless ensureGitTemplate(root) rejects the
// template at dir with an error that names the offending path (dir/rel, or dir
// itself when rel is empty) and says to remove it.
func assertTemplateRefused(t *testing.T, root, dir, rel string) {
	t.Helper()
	_, err := ensureGitTemplate(root)
	if err == nil {
		t.Fatalf("ensureGitTemplate accepted a template that had been changed at %q", rel)
	}
	offender := dir
	if rel != "" {
		offender = filepath.Join(dir, rel)
	}
	for _, want := range []string{offender, "remove"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

// assertOnlyEntry fails unless dir is the one and only thing under root, which
// is how a leftover temp dir from a creator that lost a race would show.
func assertOnlyEntry(t *testing.T, root, dir string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", root, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != filepath.Base(dir) {
		t.Errorf("%s holds %q, want only %q", root, names, filepath.Base(dir))
	}
}

// TestEnsureGitTemplateWritesExactlyTheTemplateOnceAndReusesIt pins what git
// gets to copy into every test repo: a config, an empty hooks/ and an empty
// info/exclude, and nothing else. Asking again finds the same directory, and
// another root gets the same name, since the name is derived from the content.
func TestEnsureGitTemplateWritesExactlyTheTemplateOnceAndReusesIt(t *testing.T) {
	root, dir := newTestTemplate(t)
	if got := filepath.Dir(dir); got != root {
		t.Fatalf("template %q is under %q, want it directly under %q", dir, got, root)
	}

	const isDir = "<dir>"
	got := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." {
			return err
		}
		if d.IsDir() {
			got[rel] = isDir
			return nil
		}
		b, err := os.ReadFile(path)
		got[rel] = string(b)
		return err
	})
	if err != nil {
		t.Fatalf("walking %q: %v", dir, err)
	}
	var names []string
	for name := range got {
		names = append(names, name)
	}
	sort.Strings(names)
	if want := []string{"config", "hooks", "info", filepath.Join("info", "exclude")}; strings.Join(names, "|") != strings.Join(want, "|") {
		t.Errorf("template holds %q, want %q", names, want)
	}
	if got["hooks"] != isDir || got[filepath.Join("info", "exclude")] != "" {
		t.Errorf("hooks = %q, info/exclude = %q; want an empty directory and an empty file", got["hooks"], got[filepath.Join("info", "exclude")])
	}
	for _, want := range []string{"[maintenance]", "auto = false"} {
		if !strings.Contains(got["config"], want) {
			t.Errorf("config does not contain %q:\n%s", want, got["config"])
		}
	}

	again, err := ensureGitTemplate(root)
	if err != nil || again != dir {
		t.Errorf("second ensureGitTemplate(%q) = %q, %v; want %q, nil", root, again, err, dir)
	}
	assertOnlyEntry(t, root, dir)

	_, other := newTestTemplate(t)
	if filepath.Base(other) != filepath.Base(dir) {
		t.Errorf("a second root named its template %q, want the same name as %q", filepath.Base(other), filepath.Base(dir))
	}
}

// TestEnsureGitTemplateRefusesATemplateItDidNotWrite covers the ways a
// directory with the right name can differ from what ensureGitTemplate writes.
// Each is refused with an error naming the path to look at, because git copies
// a template's hooks/ into every repo made from it, so one planted there would
// run on every test's commit.
func TestEnsureGitTemplateRefusesATemplateItDidNotWrite(t *testing.T) {
	cases := []struct {
		name   string
		rel    string // the path the error must name, relative to the template; "" for the template itself
		tamper func(t *testing.T, dir string)
	}{
		{"config rewritten", "config", func(t *testing.T, dir string) {
			mustWrite(t, filepath.Join(dir, "config"), "[maintenance]\n\tauto = true\n")
		}},
		{"config replaced by a symlink to a copy", "config", func(t *testing.T, dir string) {
			config := filepath.Join(dir, "config")
			b, err := os.ReadFile(config)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			copyPath := filepath.Join(t.TempDir(), "config")
			mustWrite(t, copyPath, string(b))
			mustRemoveAll(t, config)
			mustSymlink(t, copyPath, config)
		}},
		{"hook planted", filepath.Join("hooks", "pre-commit"), func(t *testing.T, dir string) {
			mustWrite(t, filepath.Join(dir, "hooks", "pre-commit"), "#!/bin/sh\nexit 0\n")
		}},
		{"hooks replaced by a symlink to an empty directory", "hooks", func(t *testing.T, dir string) {
			hooks := filepath.Join(dir, "hooks")
			mustRemoveAll(t, hooks)
			mustSymlink(t, t.TempDir(), hooks)
		}},
		{"hooks replaced by a file", "hooks", func(t *testing.T, dir string) {
			hooks := filepath.Join(dir, "hooks")
			mustRemoveAll(t, hooks)
			mustWrite(t, hooks, "")
		}},
		{"extra top-level entry", "branches", func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, "branches"), 0o755); err != nil {
				t.Fatalf("Mkdir: %v", err)
			}
		}},
		{"info/exclude with a rule in it", filepath.Join("info", "exclude"), func(t *testing.T, dir string) {
			mustWrite(t, filepath.Join(dir, "info", "exclude"), "*.go\n")
		}},
		{"extra entry under info", filepath.Join("info", "attributes"), func(t *testing.T, dir string) {
			mustWrite(t, filepath.Join(dir, "info", "attributes"), "")
		}},
		{"info missing", "info", func(t *testing.T, dir string) {
			mustRemoveAll(t, filepath.Join(dir, "info"))
		}},
		{"template replaced by a symlink to a pristine copy", "", func(t *testing.T, dir string) {
			_, pristine := newTestTemplate(t)
			mustRemoveAll(t, dir)
			mustSymlink(t, pristine, dir)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, dir := newTestTemplate(t)
			tc.tamper(t, dir)
			assertTemplateRefused(t, root, dir, tc.rel)
		})
	}
}

// TestEnsureGitTemplateRejectsAnUnusableRoot verifies a root that cannot hold
// the template fails with an error that names it, rather than falling back to
// somewhere else.
func TestEnsureGitTemplateRejectsAnUnusableRoot(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	notADir := filepath.Join(t.TempDir(), "file")
	mustWrite(t, notADir, "")
	for _, root := range []string{missing, notADir} {
		if _, err := ensureGitTemplate(root); err == nil || !strings.Contains(err.Error(), root) {
			t.Errorf("ensureGitTemplate(%q) error = %v, want one naming the root", root, err)
		}
	}
}

// TestInstallGitTemplateYieldsToAValidWinner is the lost-race branch made
// deterministic: the target already exists, as it does for the creator whose
// rename lost. It must accept a winner that is the template, and leave no
// temp dir behind.
func TestInstallGitTemplateYieldsToAValidWinner(t *testing.T) {
	root, dir := newTestTemplate(t)
	if err := installGitTemplate(root, dir); err != nil {
		t.Fatalf("installGitTemplate over a valid template: %v", err)
	}
	assertOnlyEntry(t, root, dir)
}

// TestInstallGitTemplateRefusesAWinnerThatIsNotTheTemplate is the same branch
// with a winner that was planted rather than written by a racing creator.
func TestInstallGitTemplateRefusesAWinnerThatIsNotTheTemplate(t *testing.T) {
	root, dir := newTestTemplate(t)
	hook := filepath.Join(dir, "hooks", "pre-commit")
	mustWrite(t, hook, "#!/bin/sh\nexit 0\n")
	if err := installGitTemplate(root, dir); err == nil || !strings.Contains(err.Error(), hook) {
		t.Fatalf("installGitTemplate over a template with a planted hook = %v, want an error naming %q", err, hook)
	}
	assertOnlyEntry(t, root, dir)
}

// TestEnsureGitTemplateIsSafeUnderConcurrentCreation has many callers race to
// make the template in a fresh root: every one must get the same verified
// directory, and the callers that lost must leave nothing behind. A creator
// that built the template in place would let a late caller see it half
// written.
func TestEnsureGitTemplateIsSafeUnderConcurrentCreation(t *testing.T) {
	const rounds, callers = 25, 16
	for round := range rounds {
		root := t.TempDir()
		dirs := make([]string, callers)
		errs := make([]error, callers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range callers {
			wg.Go(func() {
				<-start
				dirs[i], errs[i] = ensureGitTemplate(root)
			})
		}
		close(start)
		wg.Wait()

		for i := range callers {
			if errs[i] != nil {
				t.Fatalf("round %d, caller %d: %v", round, i, errs[i])
			}
			if dirs[i] != dirs[0] {
				t.Fatalf("round %d: caller %d got %q, caller 0 got %q", round, i, dirs[i], dirs[0])
			}
		}
		assertOnlyEntry(t, root, dirs[0])
	}
}
