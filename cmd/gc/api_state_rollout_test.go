package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
)

// countRolloutLogLines counts captured stderr transition lines containing sub.
func countRolloutLogLines(logs []string, sub string) int {
	n := 0
	for _, l := range logs {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

// TestNewControllerStateLatchesRolloutFlags proves the boot config's rollout
// gates are resolved once and latched on the controllerState.
func TestNewControllerStateLatchesRolloutFlags(t *testing.T) {
	stubManagedDoltStoreOpeners(t)
	dir := t.TempDir()
	toml := "[workspace]\nname = \"t\"\n\n[beads]\nconditional_writes = \"require\"\n"
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(toml))
	if err != nil {
		t.Fatal(err)
	}
	cs := newControllerState(context.Background(), cfg, nil, nil, "t", dir)
	if got := cs.RolloutFlags().BeadsConditionalWrites(); got != rollout.Require {
		t.Errorf("boot RolloutFlags beads = %q, want require", got)
	}
	if got := cs.RolloutFlags().OriginOf("beads.conditional_writes"); got != rollout.OriginConfig {
		t.Errorf("boot origin = %q, want config", got)
	}
}

// TestControllerStateBootResolveErrorZeroFlags proves an out-of-enum config value
// warns and latches the zero (degraded-safe/legacy) Flags rather than aborting
// construction.
func TestControllerStateBootResolveErrorZeroFlags(t *testing.T) {
	stubManagedDoltStoreOpeners(t)
	dir := t.TempDir()
	// config.Parse rejects this typo at load now; construct the City directly
	// to cover the defensive boot behavior for a value arriving through a
	// non-Parse path.
	cfg := &config.City{Beads: config.BeadsConfig{ConditionalWrites: "requre"}}
	cs := newControllerState(context.Background(), cfg, nil, nil, "t", dir)
	if got := cs.RolloutFlags().BeadsConditionalWrites(); got != rollout.ModeUnset {
		t.Errorf("boot RolloutFlags after resolve error = %q, want ModeUnset (zero Flags)", got)
	}
}

// TestPreflightConditionalWritesRequire proves the boot-time require probe:
// every controller-owned store that cannot fence gets a loud ERROR line at
// startup (instead of a silent boot that refuses on the first fenced write),
// capable stores stay quiet, and the probe is require-only — auto's degrade
// surface is the resolve latch, not a boot scan. Stores come through the real
// command front door (openStoreResultAtForCityWithMode) so the factory stamp
// is the production one; the incapable store simulates a post-open capability
// loss, which is exactly the gap the boot probe exists to surface.
func TestPreflightConditionalWritesRequire(t *testing.T) {
	openStamped := func(t *testing.T, mode gate.Mode) beads.Store {
		t.Helper()
		dir := t.TempDir()
		toml := "[workspace]\nname = \"t\"\nprefix = \"ga\"\n\n[beads]\nprovider = \"file\"\n"
		if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
			t.Fatal(err)
		}
		result, err := openStoreResultAtForCityWithMode(dir, dir, mode, true)
		if err != nil {
			t.Fatalf("openStoreResultAtForCityWithMode: %v", err)
		}
		return result.Store
	}
	disableFencing := func(t *testing.T, s beads.Store) beads.Store {
		t.Helper()
		// The front door wraps the store (policy store, cache); walk the
		// declared resolve targets to the FileStore that owns the capability.
		inner := s
		for {
			target, ok := inner.(beads.ConditionalWritesResolveTargeter)
			if !ok {
				break
			}
			inner = target.ConditionalWritesResolveTarget()
		}
		fs, ok := inner.(*beads.FileStore)
		if !ok {
			t.Fatalf("front door resolve target is %T, want *beads.FileStore", inner)
		}
		fs.DisableConditionalWrites = true
		return s
	}

	var logs []string
	cs := &controllerState{
		rolloutFlags: rollout.ForTest(rollout.WithBeadsConditionalWrites(rollout.Require)),
		rolloutLogf:  func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
	}
	cs.cityBeadStore = openStamped(t, gate.Require)
	cs.beadStores = map[string]beads.Store{
		"good": openStamped(t, gate.Require),
		"bad":  disableFencing(t, openStamped(t, gate.Require)),
	}
	cs.preflightConditionalWrites()
	var errLines []string
	for _, l := range logs {
		if strings.Contains(l, "ERROR") {
			errLines = append(errLines, l)
		}
	}
	if len(errLines) != 1 || !strings.Contains(errLines[0], "rig/bad") {
		t.Fatalf("require preflight ERROR lines = %v, want exactly one naming rig/bad", errLines)
	}

	// Auto never boot-scans: silence even with an incapable store present.
	logs = nil
	cs = &controllerState{
		rolloutFlags: rollout.ForTest(rollout.WithBeadsConditionalWrites(rollout.Auto)),
		rolloutLogf:  func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
	}
	cs.cityBeadStore = disableFencing(t, openStamped(t, gate.Auto))
	cs.preflightConditionalWrites()
	if len(logs) != 0 {
		t.Fatalf("auto preflight logged %v, want silence (degrade fires from the resolve latch)", logs)
	}
}

// TestControllerStateRolloutDrift proves noteRolloutDrift is level-triggered:
// it records the raw on-disk spelling, updates when the drift target changes,
// warns+records (never silently drops) an invalid on-disk value, never
// re-latches the boot value, clears on convergence, and logs once per
// transition (not per reload).
func TestControllerStateRolloutDrift(t *testing.T) {
	var logs []string
	cs := &controllerState{
		rolloutFlags: rollout.ForTest(rollout.WithBeadsConditionalWrites(rollout.Require)),
		rolloutLogf:  func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
	}
	if cs.RolloutDriftNotices() != nil {
		t.Fatal("a fresh state should have no drift")
	}

	// Two identical divergent reloads: drift recorded, logged once (per transition).
	off := &config.City{Beads: config.BeadsConfig{ConditionalWrites: "off"}}
	cs.noteRolloutDrift(off)
	cs.noteRolloutDrift(off)
	n := cs.RolloutDriftNotices()
	if len(n) != 1 || n[0].Kind != rollout.NoticePendingRestart || n[0].FlagKey != rollout.KeyBeadsConditionalWrites {
		t.Fatalf("divergent reload: want one NoticePendingRestart for the beads gate, got %+v", n)
	}
	if n[0].ConfigValue != "off" {
		t.Errorf("notice ConfigValue = %q, want the raw on-disk spelling %q", n[0].ConfigValue, "off")
	}
	if got := cs.RolloutFlags().BeadsConditionalWrites(); got != rollout.Require {
		t.Errorf("reload re-latched the gate: RolloutFlags = %q, want require (boot value)", got)
	}
	if got := countRolloutLogLines(logs, "restart to apply"); got != 1 {
		t.Errorf("drift transition logged %d times across two identical reloads, want exactly 1", got)
	}

	// Drift target changes off→auto: the notice value updates, not stale.
	cs.noteRolloutDrift(&config.City{Beads: config.BeadsConfig{ConditionalWrites: "auto"}})
	if v := cs.RolloutDriftNotices()[0].ConfigValue; v != "auto" {
		t.Errorf("after off→auto, notice ConfigValue = %q, want auto (not the stale off)", v)
	}

	// Invalid on-disk value: warn once and replace the notice with an "invalid"
	// one — never silently drop a live drift (a restart would fall back to legacy).
	cs.noteRolloutDrift(&config.City{Beads: config.BeadsConfig{ConditionalWrites: "requre"}})
	inv := cs.RolloutDriftNotices()
	if len(inv) != 1 || !strings.Contains(inv[0].Message, "invalid") || inv[0].ConfigValue != "requre" {
		t.Fatalf("invalid reload: want one 'invalid' notice carrying the raw value, got %+v", inv)
	}
	if countRolloutLogLines(logs, "invalid") == 0 {
		t.Errorf("invalid on-disk value produced no warn line; logs=%v", logs)
	}

	// Convergent reload clears the drift and logs the back-in-sync transition once.
	cs.noteRolloutDrift(&config.City{Beads: config.BeadsConfig{ConditionalWrites: "require"}})
	if cs.RolloutDriftNotices() != nil {
		t.Errorf("convergent reload should clear drift, got %+v", cs.RolloutDriftNotices())
	}
	if got := countRolloutLogLines(logs, "back in sync"); got != 1 {
		t.Errorf("back-in-sync logged %d times, want 1; logs=%v", got, logs)
	}
}

// TestNoteRolloutDriftIsolatedFromSiblingGate is the regression for the
// cross-gate contamination a shared rollout.Resolve introduces: an out-of-enum
// value on a SIBLING gate (beads.guarded_release) fails the whole-config
// resolve, which noteRolloutDrift must NOT misattribute to conditional_writes.
// Before the CW-scoped resolve, a valid conditional_writes reload alongside a
// guarded_release typo falsely reported conditional_writes as invalid.
func TestNoteRolloutDriftIsolatedFromSiblingGate(t *testing.T) {
	var logs []string
	cs := &controllerState{
		rolloutFlags: rollout.ForTest(rollout.WithBeadsConditionalWrites(rollout.Require)),
		rolloutLogf:  func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
	}

	// Valid conditional_writes that DRIFTS (require→auto) with an invalid sibling
	// guarded_release: the notice must describe the conditional_writes DRIFT, not
	// claim conditional_writes is invalid.
	cs.noteRolloutDrift(&config.City{Beads: config.BeadsConfig{
		ConditionalWrites: "auto",
		GuardedRelease:    "requre",
	}})
	n := cs.RolloutDriftNotices()
	if len(n) != 1 || n[0].FlagKey != rollout.KeyBeadsConditionalWrites {
		t.Fatalf("want one conditional_writes notice, got %+v", n)
	}
	if strings.Contains(n[0].Message, "invalid") {
		t.Errorf("sibling guarded_release typo misattributed as conditional_writes invalid: %q", n[0].Message)
	}
	if n[0].ConfigValue != "auto" || !strings.Contains(n[0].Message, "resolves to") {
		t.Errorf("want a conditional_writes drift notice for auto, got %+v", n[0])
	}

	// Valid conditional_writes that is IN SYNC with the boot latch, again with an
	// invalid sibling: drift must clear entirely — no spurious notice at all.
	cs.noteRolloutDrift(&config.City{Beads: config.BeadsConfig{
		ConditionalWrites: "require",
		GuardedRelease:    "requre",
	}})
	if got := cs.RolloutDriftNotices(); got != nil {
		t.Errorf("in-sync conditional_writes with an invalid sibling should clear drift, got %+v", got)
	}
}

// TestControllerStateRolloutDriftThroughReloadSeams proves the PRODUCTION reload
// seams — update() and updateConfigAndProviderOnly() — actually invoke
// noteRolloutDrift, and that a reload never re-latches the boot gate. Deleting
// either noteRolloutDrift call, or adding a re-latch inside a reload path, fails
// this test (the direct-call drift test above cannot see those seams).
func TestControllerStateRolloutDriftThroughReloadSeams(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	rig := t.TempDir()
	cityOf := func(mode string) *config.City {
		return &config.City{
			Workspace: config.Workspace{Name: "c"},
			Rigs:      []config.Rig{{Name: "rig1", Path: rig}},
			Beads:     config.BeadsConfig{ConditionalWrites: mode},
		}
	}

	cs := newControllerState(context.Background(), cityOf("require"), runtime.NewFake(), events.NewFake(), "c", t.TempDir())
	if got := cs.RolloutFlags().BeadsConditionalWrites(); got != rollout.Require {
		t.Fatalf("boot latch = %q, want require", got)
	}

	// Reload via update(): on-disk drops to off → drift recorded, gate NOT re-latched.
	cs.update(cityOf("off"), runtime.NewFake())
	if got := cs.RolloutFlags().BeadsConditionalWrites(); got != rollout.Require {
		t.Errorf("update() re-latched the gate: %q, want require", got)
	}
	if n := cs.RolloutDriftNotices(); len(n) != 1 || n[0].Kind != rollout.NoticePendingRestart {
		t.Fatalf("update() did not record drift through noteRolloutDrift: %+v", n)
	}

	// Reload via updateConfigAndProviderOnly(): back to require → drift clears.
	cs.updateConfigAndProviderOnly(cityOf("require"), runtime.NewFake())
	if n := cs.RolloutDriftNotices(); n != nil {
		t.Errorf("convergent reload via updateConfigAndProviderOnly did not clear drift: %+v", n)
	}
}

// TestConditionalWritesStatusBlock proves the §12.5 status-wire block renders
// the daemon's own latched snapshot: boot mode + origin, per-store verdicts
// from the side-effect-free inspector, retained notices, and the aggregate
// effective verdict (fail_closed beats degraded beats pending_restart beats
// active; off short-circuits).
func TestConditionalWritesStatusBlock(t *testing.T) {
	openStamped := func(t *testing.T, mode gate.Mode) beads.Store {
		t.Helper()
		dir := t.TempDir()
		toml := "[workspace]\nname = \"t\"\nprefix = \"ga\"\n\n[beads]\nprovider = \"file\"\n"
		if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
			t.Fatal(err)
		}
		result, err := openStoreResultAtForCityWithMode(dir, dir, mode, true)
		if err != nil {
			t.Fatalf("openStoreResultAtForCityWithMode: %v", err)
		}
		return result.Store
	}
	disableFencing := func(t *testing.T, s beads.Store) beads.Store {
		t.Helper()
		inner := s
		for {
			target, ok := inner.(beads.ConditionalWritesResolveTargeter)
			if !ok {
				break
			}
			inner = target.ConditionalWritesResolveTarget()
		}
		fs, ok := inner.(*beads.FileStore)
		if !ok {
			t.Fatalf("front door resolve target is %T, want *beads.FileStore", inner)
		}
		fs.DisableConditionalWrites = true
		return s
	}

	t.Run("require with an incapable store is fail_closed", func(t *testing.T) {
		cs := &controllerState{
			rolloutFlags: rollout.ForTest(rollout.WithBeadsConditionalWrites(rollout.Require)),
		}
		cs.cityBeadStore = openStamped(t, gate.Require)
		cs.beadStores = map[string]beads.Store{
			"good": openStamped(t, gate.Require),
			"bad":  disableFencing(t, openStamped(t, gate.Require)),
		}
		got := cs.ConditionalWritesStatus()
		if got == nil {
			t.Fatal("nil status block")
		}
		if got.Mode != "require" || got.Effective != "fail_closed" {
			t.Fatalf("mode=%q effective=%q, want require/fail_closed", got.Mode, got.Effective)
		}
		if len(got.Stores) != 3 {
			t.Fatalf("stores = %d rows, want 3 (city + 2 rigs)", len(got.Stores))
		}
		byID := map[string]api.StatusConditionalWriteStoreVerdict{}
		for _, v := range got.Stores {
			byID[v.StoreID] = v
		}
		if v := byID["rig/bad"]; v.Capable || v.Probe != "incapable" || v.Reason == "" {
			t.Fatalf("rig/bad verdict = %+v, want incapable with reason", v)
		}
		if v := byID["city"]; !v.Capable || v.Kind != "file" {
			t.Fatalf("city verdict = %+v, want capable kind=file", v)
		}
	})

	t.Run("auto with an incapable store is degraded", func(t *testing.T) {
		cs := &controllerState{
			rolloutFlags: rollout.ForTest(rollout.WithBeadsConditionalWrites(rollout.Auto)),
		}
		cs.cityBeadStore = disableFencing(t, openStamped(t, gate.Auto))
		got := cs.ConditionalWritesStatus()
		if got.Effective != "degraded" {
			t.Fatalf("effective = %q, want degraded", got.Effective)
		}
	})

	t.Run("all capable is active; drift downgrades to pending_restart", func(t *testing.T) {
		cs := &controllerState{
			rolloutFlags: rollout.ForTest(rollout.WithBeadsConditionalWrites(rollout.Auto)),
		}
		cs.cityBeadStore = openStamped(t, gate.Auto)
		if got := cs.ConditionalWritesStatus(); got.Effective != "active" {
			t.Fatalf("effective = %q, want active", got.Effective)
		}
		cs.noteRolloutDrift(&config.City{Beads: config.BeadsConfig{ConditionalWrites: "require"}})
		got := cs.ConditionalWritesStatus()
		if got.Effective != "pending_restart" {
			t.Fatalf("effective after drift = %q, want pending_restart", got.Effective)
		}
		if len(got.Notices) == 0 {
			t.Fatal("drift produced no wire notice")
		}
	})

	t.Run("off renders off with no store rows", func(t *testing.T) {
		cs := &controllerState{rolloutFlags: rollout.ForTest(rollout.WithBeadsConditionalWrites(rollout.Off))}
		cs.cityBeadStore = openStamped(t, gate.Off)
		got := cs.ConditionalWritesStatus()
		if got.Mode != "off" || got.Effective != "off" {
			t.Fatalf("mode=%q effective=%q, want off/off", got.Mode, got.Effective)
		}
		if len(got.Stores) != 0 {
			t.Fatalf("off mode rendered %d store rows, want 0", len(got.Stores))
		}
	})

	t.Run("zero flags render as off", func(t *testing.T) {
		cs := &controllerState{}
		if got := cs.ConditionalWritesStatus(); got.Mode != "off" || got.Effective != "off" {
			t.Fatalf("zero-flags block = %+v, want off/off", got)
		}
	})
}
