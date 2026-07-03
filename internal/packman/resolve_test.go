package packman

import (
	"errors"
	"testing"
)

func TestResolveVersionLatestMatchingConstraint(t *testing.T) {
	prev := runGit
	runGit = func(_ string, _ ...string) (string, error) {
		return "aaa\trefs/tags/v1.2.0\nbbb\trefs/tags/v1.3.1\nccc\trefs/tags/v2.0.0\n", nil
	}
	t.Cleanup(func() { runGit = prev })

	got, err := ResolveVersion("https://github.com/example/repo", "^1.2")
	if err != nil {
		t.Fatalf("ResolveVersion: %v", err)
	}
	if got.Version != "1.3.1" || got.Commit != "bbb" {
		t.Fatalf("ResolveVersion = %#v", got)
	}
}

func TestResolveVersionSupportsComparators(t *testing.T) {
	prev := runGit
	runGit = func(_ string, _ ...string) (string, error) {
		return "aaa\trefs/tags/v1.2.0\nbbb\trefs/tags/v1.2.5\nccc\trefs/tags/v1.3.0\n", nil
	}
	t.Cleanup(func() { runGit = prev })

	got, err := ResolveVersion("https://github.com/example/repo", ">=1.2.0,<1.3.0")
	if err != nil {
		t.Fatalf("ResolveVersion: %v", err)
	}
	if got.Version != "1.2.5" {
		t.Fatalf("Version = %q, want %q", got.Version, "1.2.5")
	}
}

func TestResolveVersionSupportsSHA(t *testing.T) {
	got, err := ResolveVersion("https://github.com/example/repo", "sha:deadbeef")
	if err != nil {
		t.Fatalf("ResolveVersion: %v", err)
	}
	if got.Version != "sha:deadbeef" || got.Commit != "deadbeef" {
		t.Fatalf("ResolveVersion = %#v", got)
	}
}

func TestSelectVersionPicksHighestMatching(t *testing.T) {
	candidates := map[string]string{
		"1.2.0": "aaa",
		"1.3.1": "bbb",
		"2.0.0": "ccc",
	}
	got, err := SelectVersion(candidates, "^1.2")
	if err != nil {
		t.Fatalf("SelectVersion: %v", err)
	}
	if got.Version != "1.3.1" || got.Commit != "bbb" {
		t.Fatalf("SelectVersion = %#v, want {1.3.1 bbb}", got)
	}
}

func TestSelectVersionSupportsComparators(t *testing.T) {
	candidates := map[string]string{
		"1.2.0": "aaa",
		"1.2.5": "bbb",
		"1.3.0": "ccc",
	}
	got, err := SelectVersion(candidates, ">=1.2.0,<1.3.0")
	if err != nil {
		t.Fatalf("SelectVersion: %v", err)
	}
	if got.Version != "1.2.5" || got.Commit != "bbb" {
		t.Fatalf("SelectVersion = %#v, want {1.2.5 bbb}", got)
	}
}

func TestSelectVersionEmptyConstraintPicksHighest(t *testing.T) {
	candidates := map[string]string{"1.2.0": "aaa", "2.0.0": "ccc"}
	got, err := SelectVersion(candidates, "")
	if err != nil {
		t.Fatalf("SelectVersion: %v", err)
	}
	if got.Version != "2.0.0" || got.Commit != "ccc" {
		t.Fatalf("SelectVersion = %#v, want {2.0.0 ccc}", got)
	}
}

func TestSelectVersionNoMatchReturnsSentinel(t *testing.T) {
	candidates := map[string]string{"1.2.0": "aaa", "1.3.1": "bbb"}
	_, err := SelectVersion(candidates, ">=2.0.0")
	if !errors.Is(err, ErrNoMatchingVersion) {
		t.Fatalf("err = %v, want ErrNoMatchingVersion", err)
	}
}

func TestSelectVersionEmptyCandidatesReturnsSentinel(t *testing.T) {
	_, err := SelectVersion(map[string]string{}, ">=1.0.0")
	if !errors.Is(err, ErrNoMatchingVersion) {
		t.Fatalf("err = %v, want ErrNoMatchingVersion", err)
	}
}

func TestSelectVersionSkipsUnparseableCandidates(t *testing.T) {
	candidates := map[string]string{
		"main":  "deadbeef",
		"1.4.2": "bbb",
	}
	got, err := SelectVersion(candidates, "")
	if err != nil {
		t.Fatalf("SelectVersion: %v", err)
	}
	if got.Version != "1.4.2" || got.Commit != "bbb" {
		t.Fatalf("SelectVersion = %#v, want {1.4.2 bbb}", got)
	}
}

func TestResolveVersionNoMatchPreservesTagError(t *testing.T) {
	prev := runGit
	runGit = func(_ string, _ ...string) (string, error) {
		return "aaa\trefs/tags/v1.2.0\n", nil
	}
	t.Cleanup(func() { runGit = prev })

	_, err := ResolveVersion("https://github.com/example/repo", ">=2.0.0")
	if err == nil {
		t.Fatal("expected no-match error")
	}
	if errors.Is(err, ErrNoMatchingVersion) {
		t.Fatalf("err leaked sentinel %v; want wrapped tag message", err)
	}
}

func TestDefaultConstraint(t *testing.T) {
	got, err := DefaultConstraint("1.4.2")
	if err != nil {
		t.Fatalf("DefaultConstraint: %v", err)
	}
	if got != "^1.4" {
		t.Fatalf("DefaultConstraint = %q, want %q", got, "^1.4")
	}
}
