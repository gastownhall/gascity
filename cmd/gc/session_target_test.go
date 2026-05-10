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
	if canonicalTestPath(got.cityPath) != canonicalTestPath(cityDir) {
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
	if canonicalTestPath(got.cityPath) != canonicalTestPath(cityDir) {
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
	if canonicalTestPath(got.cityPath) != canonicalTestPath(cityDir) {
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

// TestEventActorFallsBackToBeadsActorBeforeHuman covers the supervisor and
// order-subprocess paths. bd shell hooks (on_close, on_update, on_create)
// fan out to `gc event emit ...` without --actor; that subprocess inherits
// no GC_ALIAS/GC_AGENT/GC_SESSION_ID from the controller. Without this
// fallback, every supervisor-initiated bead change shows up in the
// dashboard as "human". With BEADS_ACTOR set by controller bd wrappers
// or by orderExecEnv to "order:<name>", the fallback
// recovers the right identity.
func TestEventActorFallsBackToBeadsActorBeforeHuman(t *testing.T) {
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "")
	t.Setenv("GC_SESSION_ID", "")
	t.Setenv("BEADS_ACTOR", "controller")
	if got := eventActor(); got != "controller" {
		t.Fatalf("eventActor() with only BEADS_ACTOR=controller = %q, want controller", got)
	}

	t.Setenv("BEADS_ACTOR", "order:order-tracking-sweep")
	if got := eventActor(); got != "order:order-tracking-sweep" {
		t.Fatalf("eventActor() with BEADS_ACTOR=order:... = %q, want order:order-tracking-sweep", got)
	}

	t.Setenv("BEADS_ACTOR", "")
	if got := eventActor(); got != "human" {
		t.Fatalf("eventActor() with no identity = %q, want human (final fallback)", got)
	}
}

// TestEventActorBeadsActorRanksBelowGCAlias verifies the priority order:
// session contexts that set both GC_ALIAS and BEADS_ACTOR (template
// resolve sets both to the same name, but the ranking matters if they
// ever diverge) keep using GC_ALIAS so the existing session-attribution
// behavior is preserved.
func TestEventActorBeadsActorRanksBelowGCAlias(t *testing.T) {
	t.Setenv("GC_ALIAS", "mayor")
	t.Setenv("BEADS_ACTOR", "controller")
	if got := eventActor(); got != "mayor" {
		t.Fatalf("eventActor() with GC_ALIAS=mayor and BEADS_ACTOR=controller = %q, want mayor (alias wins)", got)
	}
}
