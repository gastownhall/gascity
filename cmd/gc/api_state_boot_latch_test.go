package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestAssertConditionalWritesBootReadyAbsentOrigin proves the boot latch
// refuses to start when beads.conditional_writes is entirely unconfigured
// (origin=builtin) — arm 1's "nobody has opted this in yet" case — and that
// the failure reason names the origin, not merely the value.
func TestAssertConditionalWritesBootReadyAbsentOrigin(t *testing.T) {
	stubManagedDoltStoreOpeners(t)
	dir := t.TempDir()
	toml := "[workspace]\nname = \"t\"\n"
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(toml))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Beads.ConditionalWrites; got != "" {
		t.Fatalf("test invariant broken: cfg.Beads.ConditionalWrites = %q, want empty (unset)", got)
	}

	cs, bootErr := newControllerState(context.Background(), cfg, nil, nil, "t", dir)
	if bootErr == nil {
		t.Fatal("newControllerState succeeded with beads.conditional_writes unset, want a boot-latch error")
	}
	if cs == nil {
		t.Fatal("newControllerState returned a nil controllerState on latch failure, want it always non-nil")
	}
	if !strings.Contains(bootErr.Error(), string(rollout.OriginBuiltin)) {
		t.Fatalf("absent-origin error = %q, want it to name origin=%s", bootErr.Error(), rollout.OriginBuiltin)
	}
}

// TestAssertConditionalWritesBootReadyExplicitOff proves the boot latch
// refuses to start when beads.conditional_writes is explicitly "off", with a
// DISTINCT message from the absent-origin case: an explicit "off" is a
// different operator mistake than never having set the field, and the two
// messages must be told apart by whoever reads a startup failure.
func TestAssertConditionalWritesBootReadyExplicitOff(t *testing.T) {
	stubManagedDoltStoreOpeners(t)
	dir := t.TempDir()
	toml := "[workspace]\nname = \"t\"\n\n[beads]\nconditional_writes = \"off\"\n"
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(toml))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Beads.ConditionalWrites; got != "off" {
		t.Fatalf("test invariant broken: cfg.Beads.ConditionalWrites = %q, want \"off\"", got)
	}

	cs, bootErr := newControllerState(context.Background(), cfg, nil, nil, "t", dir)
	if bootErr == nil {
		t.Fatal("newControllerState succeeded with beads.conditional_writes=off, want a boot-latch error")
	}
	if cs == nil {
		t.Fatal("newControllerState returned a nil controllerState on latch failure, want it always non-nil")
	}
	if !strings.Contains(bootErr.Error(), string(rollout.OriginConfig)) {
		t.Fatalf("explicit-off error = %q, want it to name origin=%s", bootErr.Error(), rollout.OriginConfig)
	}

	// The two failure reasons must be textually distinct: an equal message
	// would mean origin is not actually being read, just the value.
	_, absentErr := newControllerState(context.Background(), &config.City{Workspace: config.Workspace{Name: "t"}}, nil, nil, "t", t.TempDir())
	if absentErr == nil {
		t.Fatal("test invariant broken: an unset-config newControllerState unexpectedly succeeded")
	}
	if bootErr.Error() == absentErr.Error() {
		t.Fatalf("explicit-off and absent-origin produced the SAME message %q; origin is not distinguishing them", bootErr.Error())
	}
}

// TestAssertConditionalWritesBootReadyIncapableBdStore proves arm 2: a scope
// whose store structurally satisfies MetadataCASWriter (as every BdStore
// does) but fails the LIVE capability probe must still refuse startup. This
// is the exact false-green trap the spec calls out — MetadataCASWriterFor
// alone would report this store capable regardless of what the installed bd
// CLI actually supports. The fake bd runner here mirrors today's real
// fleet: --if-revision is absent from bd's --help output, so every BdStore
// scope is expected to fail this check until steps 4/5 of the epic land.
func TestAssertConditionalWritesBootReadyIncapableBdStore(t *testing.T) {
	incapableHelp := []byte("Usage:\n  bd update [flags]\n\nFlags:\n  --json   emit JSON\n")
	bad := beads.NewBdStore(t.TempDir(), func(_, _ string, _ ...string) ([]byte, error) {
		return incapableHelp, nil
	})
	if _, ok := beads.MetadataCASWriterFor(bad); !ok {
		t.Fatal("test invariant broken: BdStore must structurally satisfy MetadataCASWriter")
	}

	cs := &controllerState{
		rolloutFlags: rollout.ForTest(rollout.WithBeadsConditionalWrites(rollout.Require)),
	}
	cs.cityBeadStore = beads.NewMemStore()
	cs.beadStores = map[string]beads.Store{"bad": bad}

	err := cs.assertConditionalWritesBootReady()
	if err == nil {
		t.Fatal("assertConditionalWritesBootReady succeeded with an incapable BdStore scope, want an error")
	}
	if !strings.Contains(err.Error(), "bad") {
		t.Fatalf("incapable-store error = %q, want it to name the failing scope", err.Error())
	}
}

// TestAssertConditionalWritesBootReadySucceedsForCapableFileStore proves the
// success path: conditional_writes=require, resolved with an explicit
// (config) origin, and every scope backed by a store that is genuinely
// runtime-capable (a plain FileStore) must let startup proceed.
func TestAssertConditionalWritesBootReadySucceedsForCapableFileStore(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	rig := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "c"},
		Rigs:      []config.Rig{{Name: "rig1", Path: rig}},
		Beads:     config.BeadsConfig{ConditionalWrites: "require"},
	}

	cs, err := newControllerState(context.Background(), cfg, runtime.NewFake(), events.NewFake(), "c", t.TempDir())
	if err != nil {
		t.Fatalf("newControllerState refused startup for a fully-capable require+FileStore city: %v", err)
	}
	if cs == nil {
		t.Fatal("newControllerState returned a nil controllerState")
	}
	if got := cs.RolloutFlags().BeadsConditionalWrites(); got != rollout.Require {
		t.Fatalf("boot flags = %q, want require", got)
	}
}

// TestAssertConditionalWritesBootReadyDoesNotAssertGuardedRelease proves the
// struck beads.guarded_release gate plays no part in the boot latch: leaving
// it at its zero/off default must never by itself cause a refusal, since
// asserting it would be an (unmade) ADR amendment, not an implementation of
// the existing one.
func TestAssertConditionalWritesBootReadyDoesNotAssertGuardedRelease(t *testing.T) {
	cs := &controllerState{
		rolloutFlags: rollout.ForTest(rollout.WithBeadsConditionalWrites(rollout.Require)),
	}
	cs.cityBeadStore = beads.NewMemStore()
	cs.beadStores = map[string]beads.Store{"rig1": beads.NewMemStore()}

	if err := cs.assertConditionalWritesBootReady(); err != nil {
		t.Fatalf("assertConditionalWritesBootReady failed on capable stores with guarded_release at its zero default: %v", err)
	}
}
