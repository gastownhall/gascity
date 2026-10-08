package convergence

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveConditionPathClassifiesUnlaunchableTargets pins the typed
// classification of a condition path that passed every containment check but
// names nothing executable (gastownhall/gascity#4239): callers map these to a
// hold-open disposition instead of a terminal refusal, so the classification
// must be typed rather than string-matched.
func TestResolveConditionPathClassifiesUnlaunchableTargets(t *testing.T) {
	cases := []struct {
		name        string
		setup       func(t *testing.T, dir string) string
		wantIs      error
		wantMessage string
	}{
		{
			name:   "missing relative file",
			setup:  func(_ *testing.T, _ string) string { return "scripts/missing.sh" },
			wantIs: fs.ErrNotExist,
		},
		{
			name:   "missing absolute file",
			setup:  func(_ *testing.T, dir string) string { return filepath.Join(dir, "scripts", "missing.sh") },
			wantIs: fs.ErrNotExist,
		},
		{
			name: "dangling symlink inside the envelope",
			setup: func(t *testing.T, dir string) string {
				t.Helper()
				if err := os.Symlink(filepath.Join(dir, "gone.sh"), filepath.Join(dir, "link.sh")); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				return "link.sh"
			},
			wantIs: fs.ErrNotExist,
		},
		{
			name: "directory",
			setup: func(t *testing.T, dir string) string {
				t.Helper()
				if err := os.MkdirAll(filepath.Join(dir, "check.sh"), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				return "check.sh"
			},
			wantIs:      ErrConditionNotRegular,
			wantMessage: "not a regular file",
		},
		{
			name: "regular file without execute bit",
			setup: func(t *testing.T, dir string) string {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, "check.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
					t.Fatalf("write: %v", err)
				}
				return "check.sh"
			},
			wantIs:      ErrConditionNotExecutable,
			wantMessage: "file is not executable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			conditionPath := tc.setup(t, dir)
			_, err := ResolveConditionPath(dir, dir, conditionPath)
			if err == nil {
				t.Fatalf("ResolveConditionPath(%q) = nil error, want unlaunchable", conditionPath)
			}
			if !errors.Is(err, tc.wantIs) {
				t.Fatalf("errors.Is(%v, %v) = false", err, tc.wantIs)
			}
			if !IsConditionUnlaunchable(err) {
				t.Fatalf("IsConditionUnlaunchable(%v) = false, want true", err)
			}
			if tc.wantMessage != "" && !strings.Contains(err.Error(), tc.wantMessage) {
				t.Fatalf("error %q lost its message fragment %q", err.Error(), tc.wantMessage)
			}
		})
	}
}

// TestResolveConditionPathContainmentRefusalsAreNotUnlaunchable pins that the
// security refusals and argument errors never classify as unlaunchable: they
// must keep their terminal disposition rather than hold a step open.
func TestResolveConditionPathContainmentRefusalsAreNotUnlaunchable(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, root string) (envelope, base, conditionPath string)
	}{
		{
			// Lexical traversal is refused before any filesystem access, so a
			// target that is also missing must still read as a refusal.
			name: "traversal to a missing target",
			setup: func(_ *testing.T, root string) (string, string, string) {
				city := filepath.Join(root, "city")
				return city, city, "../outside.sh"
			},
		},
		{
			name: "relative symlink resolving outside both roots",
			setup: func(t *testing.T, root string) (string, string, string) {
				t.Helper()
				city := filepath.Join(root, "city")
				if err := os.MkdirAll(city, 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				outside := filepath.Join(root, "outside.sh")
				if err := os.WriteFile(outside, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
					t.Fatalf("write: %v", err)
				}
				if err := os.Symlink(outside, filepath.Join(city, "escape.sh")); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				return city, city, "escape.sh"
			},
		},
		{
			name: "empty path",
			setup: func(_ *testing.T, root string) (string, string, string) {
				return root, root, ""
			},
		},
		{
			name: "empty envelope",
			setup: func(_ *testing.T, root string) (string, string, string) {
				return "", root, "check.sh"
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			envelope, base, conditionPath := tc.setup(t, t.TempDir())
			_, err := ResolveConditionPath(envelope, base, conditionPath)
			if err == nil {
				t.Fatalf("ResolveConditionPath(%q) = nil error, want refusal", conditionPath)
			}
			if IsConditionUnlaunchable(err) {
				t.Fatalf("IsConditionUnlaunchable(%v) = true, want false for a refusal", err)
			}
		})
	}
}

// TestIsConditionUnlaunchableNil pins that the predicate is false for nil and
// for errors that carry none of the unlaunchable sentinels.
func TestIsConditionUnlaunchableNil(t *testing.T) {
	if IsConditionUnlaunchable(nil) {
		t.Fatal("IsConditionUnlaunchable(nil) = true, want false")
	}
	if IsConditionUnlaunchable(errors.New("not a regular file")) {
		t.Fatal("IsConditionUnlaunchable(plain error) = true, want false: classification must be typed, not textual")
	}
}
