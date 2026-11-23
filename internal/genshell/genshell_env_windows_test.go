//go:build windows

package genshell

import (
	"context"
	"slices"
	"testing"
)

func TestRunAvoidsPrivateReplacementNameCollisionOnWindows(t *testing.T) {
	stdout, stderr, err := runScript(t,
		`__gascity_genshell_target=original; target=local; printf '[%s][%s]\n' "$__gascity_genshell_target" "$target"`,
		Options{Env: []string{"PATH=" + t.TempDir(), "TARGET=fromenv"}},
	)
	if err != nil {
		t.Fatalf("Run: %v (stderr %q)", err, stderr)
	}
	if want := "[original][local]\n"; stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func TestRunExportsCaseConflictUnderOriginalNameOnWindows(t *testing.T) {
	var childEnv []string
	opts := Options{
		Env: []string{"PATH=" + t.TempDir(), "TARGET=fromenv"},
		Exec: func(_ context.Context, p Process) error {
			childEnv = slices.Clone(p.Env)
			return nil
		},
	}
	_, stderr, err := runScript(t, `target=value; export target; bd a`, opts)
	if err != nil {
		t.Fatalf("Run: %v (stderr %q)", err, stderr)
	}
	if !hasExactEnv(childEnv, "target", "value") {
		t.Fatalf("child env = %q, want exported target=value under the script's name", childEnv)
	}
	if hasExactEnvName(childEnv, "__gascity_genshell_target") || hasExactEnvName(childEnv, "TARGET") {
		t.Fatalf("child env = %q, want no private name and no folded inherited TARGET", childEnv)
	}
}
