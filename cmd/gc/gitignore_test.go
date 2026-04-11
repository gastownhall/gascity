package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestEnsureGitignoreEntries_CreatesFileWhenMissing(t *testing.T) {
	f := fsys.NewFake()
	f.Dirs["/city"] = true

	if err := ensureGitignoreEntries(f, "/city", []string{".gc/", ".beads/", "hooks/"}); err != nil {
		t.Fatalf("ensureGitignoreEntries: %v", err)
	}

	got := string(f.Files["/city/.gitignore"])
	want := "# Gas City managed state\n.gc/\n.beads/\nhooks/\n"
	if got != want {
		t.Errorf("gitignore =\n%q\nwant\n%q", got, want)
	}
}

func TestEnsureGitignoreEntries_AppendsToBdInitFile(t *testing.T) {
	f := fsys.NewFake()
	f.Dirs["/city"] = true
	// Mirrors the content bd init writes today.
	f.Files["/city/.gitignore"] = []byte("# Beads / Dolt files (added by bd init)\n.dolt/\n*.db\n.beads-credential-key\n")

	if err := ensureGitignoreEntries(f, "/city", []string{".gc/", ".beads/", "hooks/"}); err != nil {
		t.Fatalf("ensureGitignoreEntries: %v", err)
	}

	got := string(f.Files["/city/.gitignore"])
	want := "# Beads / Dolt files (added by bd init)\n.dolt/\n*.db\n.beads-credential-key\n\n# Gas City managed state\n.gc/\n.beads/\nhooks/\n"
	if got != want {
		t.Errorf("gitignore =\n%q\nwant\n%q", got, want)
	}
}

func TestEnsureGitignoreEntries_NoOpWhenAllPresent(t *testing.T) {
	f := fsys.NewFake()
	f.Dirs["/city"] = true
	f.Files["/city/.gitignore"] = []byte(".gc/\n.beads/\nhooks/\n")

	if err := ensureGitignoreEntries(f, "/city", []string{".gc/", ".beads/", "hooks/"}); err != nil {
		t.Fatalf("ensureGitignoreEntries: %v", err)
	}

	for _, c := range f.Calls {
		if c.Method == "WriteFile" && c.Path == "/city/.gitignore" {
			t.Errorf("unexpected WriteFile when entries already present")
		}
	}
}

func TestEnsureGitignoreEntries_PartialOverlap(t *testing.T) {
	f := fsys.NewFake()
	f.Dirs["/city"] = true
	f.Files["/city/.gitignore"] = []byte(".gc/\n")

	if err := ensureGitignoreEntries(f, "/city", []string{".gc/", ".beads/", "hooks/"}); err != nil {
		t.Fatalf("ensureGitignoreEntries: %v", err)
	}

	got := string(f.Files["/city/.gitignore"])
	want := ".gc/\n\n# Gas City managed state\n.beads/\nhooks/\n"
	if got != want {
		t.Errorf("gitignore =\n%q\nwant\n%q", got, want)
	}
}

func TestEnsureGitignoreEntries_PreservesUserEntriesAndComments(t *testing.T) {
	f := fsys.NewFake()
	f.Dirs["/city"] = true
	f.Files["/city/.gitignore"] = []byte("# user comment\nnode_modules/\n\n# another comment\n.gc/\n")

	if err := ensureGitignoreEntries(f, "/city", []string{".gc/", ".beads/"}); err != nil {
		t.Fatalf("ensureGitignoreEntries: %v", err)
	}

	got := string(f.Files["/city/.gitignore"])
	if !strings.Contains(got, "# user comment") {
		t.Errorf("lost user comment:\n%s", got)
	}
	if !strings.Contains(got, "node_modules/") {
		t.Errorf("lost user entry:\n%s", got)
	}
	if strings.Count(got, ".gc/") != 1 {
		t.Errorf(".gc/ should appear exactly once:\n%s", got)
	}
	if !strings.Contains(got, "\n.beads/\n") {
		t.Errorf(".beads/ not appended:\n%s", got)
	}
}

func TestEnsureGitignoreEntries_HandlesMissingTrailingNewline(t *testing.T) {
	f := fsys.NewFake()
	f.Dirs["/city"] = true
	f.Files["/city/.gitignore"] = []byte(".dolt/")

	if err := ensureGitignoreEntries(f, "/city", []string{".gc/"}); err != nil {
		t.Fatalf("ensureGitignoreEntries: %v", err)
	}

	got := string(f.Files["/city/.gitignore"])
	want := ".dolt/\n\n# Gas City managed state\n.gc/\n"
	if got != want {
		t.Errorf("gitignore =\n%q\nwant\n%q", got, want)
	}
}

func TestEnsureGitignoreEntries_Idempotent(t *testing.T) {
	f := fsys.NewFake()
	f.Dirs["/city"] = true

	for i := 0; i < 3; i++ {
		if err := ensureGitignoreEntries(f, "/city", []string{".gc/", ".beads/", "hooks/"}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	got := string(f.Files["/city/.gitignore"])
	want := "# Gas City managed state\n.gc/\n.beads/\nhooks/\n"
	if got != want {
		t.Errorf("gitignore after 3 calls =\n%q\nwant\n%q", got, want)
	}
}

func TestEnsureGitignoreEntries_RigSet(t *testing.T) {
	f := fsys.NewFake()
	f.Dirs["/rig"] = true
	f.Files["/rig/.gitignore"] = []byte("# Beads / Dolt files (added by bd init)\n.dolt/\n*.db\n.beads-credential-key\n")

	if err := ensureGitignoreEntries(f, "/rig", rigGitignoreEntries); err != nil {
		t.Fatalf("ensureGitignoreEntries: %v", err)
	}

	got := string(f.Files["/rig/.gitignore"])
	if !strings.Contains(got, ".beads/") {
		t.Errorf(".beads/ not appended to rig gitignore:\n%s", got)
	}
	if strings.Contains(got, ".gc/") {
		t.Errorf("rig gitignore unexpectedly contains .gc/:\n%s", got)
	}
	if strings.Contains(got, "hooks/") {
		t.Errorf("rig gitignore unexpectedly contains hooks/:\n%s", got)
	}
}
