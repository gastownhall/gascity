package packman

import (
	"errors"
	"strings"
	"testing"
)

func TestResolveVersionLatestMatchingConstraint(t *testing.T) {
	prev := runNetworkGit
	runNetworkGit = func(_, _, _ string, _ ...string) (string, error) {
		return "aaa\trefs/tags/v1.2.0\nbbb\trefs/tags/v1.3.1\nccc\trefs/tags/v2.0.0\n", nil
	}
	t.Cleanup(func() { runNetworkGit = prev })

	got, err := ResolveVersion("", "https://github.com/example/repo", "^1.2")
	if err != nil {
		t.Fatalf("ResolveVersion: %v", err)
	}
	if got.Version != "1.3.1" || got.Commit != "bbb" {
		t.Fatalf("ResolveVersion = %#v", got)
	}
}

func TestResolveVersionSupportsComparators(t *testing.T) {
	prev := runNetworkGit
	runNetworkGit = func(_, _, _ string, _ ...string) (string, error) {
		return "aaa\trefs/tags/v1.2.0\nbbb\trefs/tags/v1.2.5\nccc\trefs/tags/v1.3.0\n", nil
	}
	t.Cleanup(func() { runNetworkGit = prev })

	got, err := ResolveVersion("", "https://github.com/example/repo", ">=1.2.0,<1.3.0")
	if err != nil {
		t.Fatalf("ResolveVersion: %v", err)
	}
	if got.Version != "1.2.5" {
		t.Fatalf("Version = %q, want %q", got.Version, "1.2.5")
	}
}

func TestResolveVersionSupportsSHA(t *testing.T) {
	got, err := ResolveVersion("", "https://github.com/example/repo", "sha:deadbeef")
	if err != nil {
		t.Fatalf("ResolveVersion: %v", err)
	}
	if got.Version != "sha:deadbeef" || got.Commit != "deadbeef" {
		t.Fatalf("ResolveVersion = %#v", got)
	}
}

func TestResolveVersionRedactsUserinfoInError(t *testing.T) {
	prev := runNetworkGit
	runNetworkGit = func(_, _, _ string, _ ...string) (string, error) {
		return "", errors.New("git failed")
	}
	t.Cleanup(func() { runNetworkGit = prev })

	_, err := ResolveVersion("", "https://user:ghp_secret@github.com/example/repo", "^1.0")
	if err == nil {
		t.Fatalf("expected an error from the failing ls-remote")
	}
	if strings.Contains(err.Error(), "ghp_secret") {
		t.Fatalf("error leaked the userinfo token: %v", err)
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
