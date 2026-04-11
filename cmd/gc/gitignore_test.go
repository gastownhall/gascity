package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestEnsureGitignoreEntries_CreatesNewFile(t *testing.T) {
	f := fsys.NewFake()

	if err := ensureGitignoreEntries(f, "/city", []string{".gc/", ".beads/"}); err != nil {
		t.Fatalf("ensureGitignoreEntries: %v", err)
	}

	got := string(f.Files[filepath.Join("/city", ".gitignore")])
	for _, want := range []string{".gc/", ".beads/"} {
		if !strings.Contains(got, want) {
			t.Errorf(".gitignore missing %q; got:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "# Gas City") {
		t.Error(".gitignore missing section header '# Gas City'")
	}
}

func TestEnsureGitignoreEntries_SkipsExisting(t *testing.T) {
	f := fsys.NewFake()
	f.Files[filepath.Join("/city", ".gitignore")] = []byte(".gc/\nnode_modules/\n")

	if err := ensureGitignoreEntries(f, "/city", []string{".gc/", ".beads/"}); err != nil {
		t.Fatalf("ensureGitignoreEntries: %v", err)
	}

	got := string(f.Files[filepath.Join("/city", ".gitignore")])
	// .gc/ should appear only once (the original).
	if strings.Count(got, ".gc/") != 1 {
		t.Errorf(".gc/ appears %d times, want 1; got:\n%s", strings.Count(got, ".gc/"), got)
	}
	// .beads/ should be added.
	if !strings.Contains(got, ".beads/") {
		t.Errorf(".gitignore missing .beads/; got:\n%s", got)
	}
	// Original content preserved.
	if !strings.Contains(got, "node_modules/") {
		t.Errorf("original content lost; got:\n%s", got)
	}
}

func TestEnsureGitignoreEntries_Idempotent(t *testing.T) {
	f := fsys.NewFake()

	entries := []string{".gc/", ".beads/", "hooks/", ".runtime/"}
	for i := 0; i < 3; i++ {
		if err := ensureGitignoreEntries(f, "/city", entries); err != nil {
			t.Fatalf("pass %d: ensureGitignoreEntries: %v", i, err)
		}
	}

	got := string(f.Files[filepath.Join("/city", ".gitignore")])
	for _, entry := range entries {
		if strings.Count(got, entry) != 1 {
			t.Errorf("%q appears %d times after 3 passes, want 1; got:\n%s",
				entry, strings.Count(got, entry), got)
		}
	}
}

func TestEnsureGitignoreEntries_NoOpWhenAllPresent(t *testing.T) {
	f := fsys.NewFake()
	original := ".gc/\n.beads/\nhooks/\n.runtime/\n"
	f.Files[filepath.Join("/city", ".gitignore")] = []byte(original)

	if err := ensureGitignoreEntries(f, "/city", []string{".gc/", ".beads/", "hooks/", ".runtime/"}); err != nil {
		t.Fatalf("ensureGitignoreEntries: %v", err)
	}

	got := string(f.Files[filepath.Join("/city", ".gitignore")])
	if got != original {
		t.Errorf("file was modified when it shouldn't have been;\nwant: %q\ngot:  %q", original, got)
	}
}

func TestDoInit_WritesGitignoreEntries(t *testing.T) {
	f := fsys.NewFake()

	var stdout, stderr bytes.Buffer
	code := doInit(f, "/bright-lights", defaultWizardConfig(), "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0; stderr: %s", code, stderr.String())
	}

	gitignorePath := filepath.Join("/bright-lights", ".gitignore")
	data, ok := f.Files[gitignorePath]
	if !ok {
		t.Fatal(".gitignore not created by doInit")
	}
	got := string(data)
	for _, want := range []string{".gc/", ".beads/", "hooks/", ".runtime/"} {
		if !strings.Contains(got, want) {
			t.Errorf(".gitignore missing %q; got:\n%s", want, got)
		}
	}
}

func TestDoInit_GitignoreIdempotent(t *testing.T) {
	f := fsys.NewFake()

	var stdout, stderr bytes.Buffer
	code := doInit(f, "/bright-lights", defaultWizardConfig(), "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("first doInit = %d, want 0; stderr: %s", code, stderr.String())
	}

	first := string(f.Files[filepath.Join("/bright-lights", ".gitignore")])

	// Run ensureGitignoreEntries again (simulating a second init-like operation).
	if err := ensureGitignoreEntries(f, "/bright-lights", cityGitignoreEntries); err != nil {
		t.Fatalf("second ensureGitignoreEntries: %v", err)
	}

	second := string(f.Files[filepath.Join("/bright-lights", ".gitignore")])
	if first != second {
		t.Errorf("gitignore changed on second pass;\nfirst:  %q\nsecond: %q", first, second)
	}
}

func TestDoInit_GitignorePreservesUserEntries(t *testing.T) {
	f := fsys.NewFake()
	// Pre-populate a .gitignore with user content.
	userContent := "node_modules/\n*.log\n"
	f.Files[filepath.Join("/bright-lights", ".gitignore")] = []byte(userContent)
	// Pre-populate city.toml so doInit sees it as existing city (bootstrap path).
	// Instead, just test ensureGitignoreEntries directly since doInit won't
	// run on a directory that already has a scaffold.
	if err := ensureGitignoreEntries(f, "/bright-lights", cityGitignoreEntries); err != nil {
		t.Fatalf("ensureGitignoreEntries: %v", err)
	}

	got := string(f.Files[filepath.Join("/bright-lights", ".gitignore")])
	if !strings.Contains(got, "node_modules/") {
		t.Error("user entry 'node_modules/' was lost")
	}
	if !strings.Contains(got, "*.log") {
		t.Error("user entry '*.log' was lost")
	}
	for _, want := range cityGitignoreEntries {
		if !strings.Contains(got, want) {
			t.Errorf("missing city entry %q; got:\n%s", want, got)
		}
	}
}
