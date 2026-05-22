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

	if err := ensureGitignoreEntries(f, "/city", []string{".gc/", ".beads/*", "!.beads/config.yaml", "!.beads/metadata.json"}); err != nil {
		t.Fatalf("ensureGitignoreEntries: %v", err)
	}

	got := string(f.Files[filepath.Join("/city", ".gitignore")])
	for _, want := range []string{".gc/", ".beads/*", "!.beads/config.yaml", "!.beads/metadata.json"} {
		if !strings.Contains(got, want) {
			t.Errorf(".gitignore missing %q; got:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "# Gas City") {
		t.Error(".gitignore missing section header '# Gas City'")
	}
}

func TestEnsureGitignoreEntries_RigEntriesTrackCanonicalBeadsFilesOnly(t *testing.T) {
	f := fsys.NewFake()

	if err := ensureGitignoreEntries(f, "/rig", rigGitignoreEntries); err != nil {
		t.Fatalf("ensureGitignoreEntries: %v", err)
	}

	got := string(f.Files[filepath.Join("/rig", ".gitignore")])
	for _, want := range rigGitignoreEntries {
		if !strings.Contains(got, want) {
			t.Errorf("rig .gitignore missing %q; got:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{".gc/", "hooks/", ".runtime/"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("rig .gitignore should not contain %q; got:\n%s", forbidden, got)
		}
	}
}

func TestEnsureGitignoreEntries_SkipsExisting(t *testing.T) {
	f := fsys.NewFake()
	f.Files[filepath.Join("/city", ".gitignore")] = []byte(".gc/\n.beads/\n/.beads/\nnode_modules/\n")

	if err := ensureGitignoreEntries(f, "/city", []string{".gc/", ".beads/*", "!.beads/config.yaml", "!.beads/metadata.json"}); err != nil {
		t.Fatalf("ensureGitignoreEntries: %v", err)
	}

	got := string(f.Files[filepath.Join("/city", ".gitignore")])
	// .gc/ should appear only once (the original).
	if strings.Count(got, ".gc/") != 1 {
		t.Errorf(".gc/ appears %d times, want 1; got:\n%s", strings.Count(got, ".gc/"), got)
	}
	// Canonical .beads rules should be added.
	for _, want := range []string{".beads/*", "!.beads/config.yaml", "!.beads/metadata.json"} {
		if !strings.Contains(got, want) {
			t.Errorf(".gitignore missing %q; got:\n%s", want, got)
		}
	}
	for _, legacy := range []string{"\n.beads/\n", "\n/.beads/\n"} {
		if strings.Contains("\n"+got, legacy) {
			t.Errorf("legacy .beads ignore %q should be removed; got:\n%s", strings.TrimSpace(legacy), got)
		}
	}
	// Original content preserved.
	if !strings.Contains(got, "node_modules/") {
		t.Errorf("original content lost; got:\n%s", got)
	}
}

func TestEnsureGitignoreEntries_Idempotent(t *testing.T) {
	f := fsys.NewFake()

	entries := cityGitignoreEntries
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
	original := ".gc/\n.beads/*\n!.beads/config.yaml\n!.beads/metadata.json\n!.beads/identity.toml\nhooks/\n.runtime/\n"
	f.Files[filepath.Join("/city", ".gitignore")] = []byte(original)

	if err := ensureGitignoreEntries(f, "/city", cityGitignoreEntries); err != nil {
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
	code := doInit(f, "/bright-lights", defaultWizardConfig(), "", &stdout, &stderr, false)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0; stderr: %s", code, stderr.String())
	}

	gitignorePath := filepath.Join("/bright-lights", ".gitignore")
	data, ok := f.Files[gitignorePath]
	if !ok {
		t.Fatal(".gitignore not created by doInit")
	}
	got := string(data)
	for _, want := range cityGitignoreEntries {
		if !strings.Contains(got, want) {
			t.Errorf(".gitignore missing %q; got:\n%s", want, got)
		}
	}
}

func TestDoInit_GitignoreIdempotent(t *testing.T) {
	f := fsys.NewFake()

	var stdout, stderr bytes.Buffer
	code := doInit(f, "/bright-lights", defaultWizardConfig(), "", &stdout, &stderr, false)
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

// TestEnsureGitignoreEntries_IdentityTomlNegationPresent locks designer §6:
// both city- and rig-rendered .gitignore must contain
// "!.beads/identity.toml" so the identity file is committed alongside
// .beads/config.yaml and .beads/metadata.json.
func TestEnsureGitignoreEntries_IdentityTomlNegationPresent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dir     string
		entries []string
	}{
		{"city", "/city", cityGitignoreEntries},
		{"rig", "/rig", rigGitignoreEntries},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fsys.NewFake()
			if err := ensureGitignoreEntries(f, tc.dir, tc.entries); err != nil {
				t.Fatalf("ensureGitignoreEntries: %v", err)
			}
			got := string(f.Files[filepath.Join(tc.dir, ".gitignore")])
			if !strings.Contains(got, "!.beads/identity.toml") {
				t.Errorf("%s .gitignore missing %q; got:\n%s", tc.name, "!.beads/identity.toml", got)
			}
		})
	}
}

// TestEnsureGitignoreEntries_IdentityTomlNegationAfterGlob locks the
// ordering invariant: "!.beads/identity.toml" must appear AFTER
// ".beads/*" in the rendered output, otherwise the negation is inert.
// This catches the most common regression — alphabetical reorders of
// the slice.
func TestEnsureGitignoreEntries_IdentityTomlNegationAfterGlob(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dir     string
		entries []string
	}{
		{"city", "/city", cityGitignoreEntries},
		{"rig", "/rig", rigGitignoreEntries},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fsys.NewFake()
			if err := ensureGitignoreEntries(f, tc.dir, tc.entries); err != nil {
				t.Fatalf("ensureGitignoreEntries: %v", err)
			}
			got := string(f.Files[filepath.Join(tc.dir, ".gitignore")])
			globIdx := strings.Index(got, ".beads/*")
			negIdx := strings.Index(got, "!.beads/identity.toml")
			if globIdx < 0 {
				t.Fatalf("%s .gitignore missing %q; got:\n%s", tc.name, ".beads/*", got)
			}
			if negIdx < 0 {
				t.Fatalf("%s .gitignore missing %q; got:\n%s", tc.name, "!.beads/identity.toml", got)
			}
			if negIdx < globIdx {
				t.Errorf("%s .gitignore: %q must appear AFTER %q (negation requires it); got:\n%s",
					tc.name, "!.beads/identity.toml", ".beads/*", got)
			}
		})
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
