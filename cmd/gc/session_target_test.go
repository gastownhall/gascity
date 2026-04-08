package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCurrentSessionRuntimeTargetUsesAlias(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_ALIAS", "mayor")
	t.Setenv("GC_SESSION_ID", "gc-42")
	t.Setenv("GC_SESSION_NAME", "s-gc-42")

	got, err := currentSessionRuntimeTarget()
	if err != nil {
		t.Fatalf("currentSessionRuntimeTarget(): %v", err)
	}
	if got.cityPath != cityDir {
		t.Fatalf("cityPath = %q, want %q", got.cityPath, cityDir)
	}
	if got.display != "mayor" {
		t.Fatalf("display = %q, want mayor", got.display)
	}
	if got.sessionName != "s-gc-42" {
		t.Fatalf("sessionName = %q, want s-gc-42", got.sessionName)
	}
}

func TestCurrentSessionRuntimeTargetFallsBackToCityPathEnv(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_ALIAS", "mayor")
	t.Setenv("GC_SESSION_ID", "gc-42")
	t.Setenv("GC_SESSION_NAME", "s-gc-42")

	got, err := currentSessionRuntimeTarget()
	if err != nil {
		t.Fatalf("currentSessionRuntimeTarget(): %v", err)
	}
	if got.cityPath != cityDir {
		t.Fatalf("cityPath = %q, want %q", got.cityPath, cityDir)
	}
}

func TestCurrentSessionRuntimeTargetFallsBackToGCDir(t *testing.T) {
	cityDir := t.TempDir()
	workDir := filepath.Join(cityDir, "rigs", "demo")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	t.Setenv("GC_DIR", workDir)
	t.Setenv("GC_ALIAS", "mayor")
	t.Setenv("GC_SESSION_ID", "gc-42")
	t.Setenv("GC_SESSION_NAME", "s-gc-42")

	got, err := currentSessionRuntimeTarget()
	if err != nil {
		t.Fatalf("currentSessionRuntimeTarget(): %v", err)
	}
	if got.cityPath != cityDir {
		t.Fatalf("cityPath = %q, want %q", got.cityPath, cityDir)
	}
}

func TestEventActorPrefersAliasThenSessionID(t *testing.T) {
	t.Setenv("GC_ALIAS", "mayor")
	t.Setenv("GC_SESSION_ID", "gc-42")
	if got := eventActor(); got != "mayor" {
		t.Fatalf("eventActor() = %q, want mayor", got)
	}

	t.Setenv("GC_ALIAS", "")
	if got := eventActor(); got != "gc-42" {
		t.Fatalf("eventActor() without alias = %q, want gc-42", got)
	}
}
