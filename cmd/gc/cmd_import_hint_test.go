package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/gitcred"
	"github.com/gastownhall/gascity/internal/packman"
)

func authErrorSeam(matched bool) error {
	return fmt.Errorf("head probe failed: %w", &gitcred.AuthError{
		Host:       "github.com",
		OrgPrefix:  "github.com/gascity",
		Repo:       "https://github.com/gascity/repo",
		Matched:    matched,
		RuleOrigin: "/city/.gc/credentials.toml",
		Output:     "terminal prompts disabled",
	})
}

func setupHintCity(t *testing.T) string {
	t.Helper()
	city := t.TempDir()
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	return city
}

func stubHeadCommitAuthError(t *testing.T, matched bool) {
	t.Helper()
	prevResolve := resolveImportVersion
	prevHead := resolveImportHeadCommit
	t.Cleanup(func() {
		resolveImportVersion = prevResolve
		resolveImportHeadCommit = prevHead
	})
	resolveImportVersion = func(_, _, _ string) (packman.ResolvedVersion, error) {
		return packman.ResolvedVersion{}, packman.ErrNoSemverTags
	}
	resolveImportHeadCommit = func(_, _ string) (string, error) {
		return "", authErrorSeam(matched)
	}
}

func TestDoImportAddPrintsUnmatchedHint(t *testing.T) {
	city := setupHintCity(t)
	stubHeadCommitAuthError(t, false)
	var stdout, stderr strings.Builder
	rc := doImportAdd(fsys.OSFS{}, city, "https://github.com/gascity/repo", "", "", &stdout, &stderr)
	if rc == 0 {
		t.Fatalf("expected failure")
	}
	if !strings.Contains(stderr.String(), "register a pack credential and retry") {
		t.Fatalf("unmatched hint missing: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "gc import credential add github.com/gascity") {
		t.Fatalf("hint missing the suggested command: %q", stderr.String())
	}
}

func TestDoImportAddPrintsMatchedHint(t *testing.T) {
	city := setupHintCity(t)
	stubHeadCommitAuthError(t, true)
	var stdout, stderr strings.Builder
	rc := doImportAdd(fsys.OSFS{}, city, "https://github.com/gascity/repo", "", "", &stdout, &stderr)
	if rc == 0 {
		t.Fatalf("expected failure")
	}
	if !strings.Contains(stderr.String(), "matched but the remote rejected it") {
		t.Fatalf("matched hint missing: %q", stderr.String())
	}
}

func TestDoImportAddNoHintForNonAuthError(t *testing.T) {
	city := setupHintCity(t)
	prevResolve := resolveImportVersion
	prevHead := resolveImportHeadCommit
	t.Cleanup(func() {
		resolveImportVersion = prevResolve
		resolveImportHeadCommit = prevHead
	})
	resolveImportVersion = func(_, _, _ string) (packman.ResolvedVersion, error) {
		return packman.ResolvedVersion{}, packman.ErrNoSemverTags
	}
	resolveImportHeadCommit = func(_, _ string) (string, error) {
		return "", fmt.Errorf("some network blip")
	}
	var stdout, stderr strings.Builder
	rc := doImportAdd(fsys.OSFS{}, city, "https://github.com/gascity/repo", "", "", &stdout, &stderr)
	if rc == 0 {
		t.Fatalf("expected failure")
	}
	if strings.Contains(stderr.String(), "hint:") {
		t.Fatalf("non-auth error must not print a hint: %q", stderr.String())
	}
}
