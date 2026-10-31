package genshell

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestRunKeepsShellVariablesCaseSensitive(t *testing.T) {
	var childEnv []string
	opts := Options{
		Env: []string{
			"PATH=" + t.TempDir(),
			"TARGET=fromenv",
			"R=envR",
		},
		Exec: func(_ context.Context, p Process) error {
			childEnv = slices.Clone(p.Env)
			return nil
		},
	}
	stdout, stderr, err := runScript(t, `printf "target=[%s] TARGET=[%s]\n" "$target" "$TARGET"; target=fromscript; r=scriptR; printf "R=[%s] r=[%s]\n" "$R" "$r"; bd a`, opts)
	if err != nil {
		t.Fatalf("Run: %v (stderr %q)", err, stderr)
	}
	want := "target=[] TARGET=[fromenv]\nR=[envR] r=[scriptR]\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	if !hasExactEnv(childEnv, "TARGET", "fromenv") || !hasExactEnv(childEnv, "R", "envR") {
		t.Fatalf("child env = %q, want inherited TARGET and R unchanged", childEnv)
	}
	if hasExactEnvName(childEnv, "target") || hasExactEnvName(childEnv, "r") {
		t.Fatalf("child env = %q, want unexported lowercase shell locals omitted", childEnv)
	}
}

func TestRunExportsReassignedVariableOnce(t *testing.T) {
	var childEnv []string
	opts := Options{
		Env: []string{
			"PATH=" + t.TempDir(),
			"OVER=old",
			"KEEP=1",
		},
		Exec: func(_ context.Context, p Process) error {
			childEnv = slices.Clone(p.Env)
			return nil
		},
	}
	_, stderr, err := runScript(t, `OVER=new; bd a`, opts)
	if err != nil {
		t.Fatalf("Run: %v (stderr %q)", err, stderr)
	}
	if got := exactEnvValues(childEnv, "OVER"); !slices.Equal(got, []string{"new"}) {
		t.Fatalf("OVER entries = %q in env %q, want exactly [new]", got, childEnv)
	}
	if !hasExactEnv(childEnv, "KEEP", "1") {
		t.Fatalf("child env = %q, want KEEP=1 preserved", childEnv)
	}
}

func hasExactEnv(env []string, name, value string) bool {
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if ok && k == name && v == value {
			return true
		}
	}
	return false
}

func hasExactEnvName(env []string, name string) bool {
	for _, kv := range env {
		k, _, ok := strings.Cut(kv, "=")
		if ok && k == name {
			return true
		}
	}
	return false
}

func exactEnvValues(env []string, name string) []string {
	var values []string
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if ok && k == name {
			values = append(values, v)
		}
	}
	return values
}
