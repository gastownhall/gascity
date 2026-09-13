package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The per-prompt UserPromptSubmit drain (gc nudge drain --inject) loads the
// city config once in resolveNudgeTarget and then opened the nudge store up to
// three more times, each open reloading city.toml plus every pack (sys-2b9jfa,
// upstream #6157). These tests pin that a caller holding the config reaches
// the store without a second load, while the nil path keeps loading.
func writeReuseTestCity(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	toml := "[workspace]\nname = \"t\"\n\n[beads]\nprovider = \"file\"\n"
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestOpenNudgeBeadStoreWithConfigSkipsLoad(t *testing.T) {
	dir := writeReuseTestCity(t)
	cfg, err := loadCityConfig(dir, io.Discard)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	before := loadCityConfigCalls.Load()
	if s := openNudgeBeadStoreWithConfig(dir, cfg); s.Store == nil {
		t.Fatal("openNudgeBeadStoreWithConfig(cfg) returned no store")
	}
	if grew := loadCityConfigCalls.Load() - before; grew != 0 {
		t.Fatalf("openNudgeBeadStoreWithConfig re-parsed city config %d times despite a non-nil cfg", grew)
	}

	before = loadCityConfigCalls.Load()
	if s := openNudgeBeadStore(dir); s.Store == nil {
		t.Fatal("openNudgeBeadStore returned no store")
	}
	if grew := loadCityConfigCalls.Load() - before; grew != 1 {
		t.Fatalf("openNudgeBeadStore (nil cfg) parsed city config %d times, want exactly 1 (fallback load)", grew)
	}
}

func TestOpenWispStepStoreWithConfigSkipsLoad(t *testing.T) {
	dir := writeReuseTestCity(t)
	t.Setenv("GC_RIG_ROOT", "")
	cfg, err := loadCityConfig(dir, io.Discard)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	before := loadCityConfigCalls.Load()
	if openWispStepStore(dir, cfg) == nil {
		t.Fatal("openWispStepStore(cfg) returned nil")
	}
	if grew := loadCityConfigCalls.Load() - before; grew != 0 {
		t.Fatalf("openWispStepStore re-parsed city config %d times despite a non-nil cfg", grew)
	}
}

// TestClaimDueQueuedNudgesForTargetReusesConfig drives the drain's claim step
// with a non-empty queue (so the maintenance store actually opens) and a target
// that carries the config, as resolveNudgeTarget populates it.
func TestClaimDueQueuedNudgesForTargetReusesConfig(t *testing.T) {
	dir := writeReuseTestCity(t)
	cfg, err := loadCityConfig(dir, io.Discard)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	now := time.Now()
	item := newQueuedNudgeWithOptions("worker", "do work", "session", now, queuedNudgeOptions{ID: "n-reuse"})
	if err := enqueueQueuedNudge(dir, item); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	target := nudgeTarget{cityPath: dir, cfg: cfg, alias: "worker", identity: "worker"}

	before := loadCityConfigCalls.Load()
	if _, err := claimDueQueuedNudgesForTarget(dir, target, now); err != nil {
		t.Fatalf("claimDueQueuedNudgesForTarget: %v", err)
	}
	if grew := loadCityConfigCalls.Load() - before; grew != 0 {
		t.Fatalf("claimDueQueuedNudgesForTarget re-parsed city config %d times despite target.cfg", grew)
	}
}

// TestDrainHelpersBorrowTheTargetStore pins the second half of sys-2b9jfa: when
// the drain hands its already-open target store to the claim and ack helpers,
// they borrow it (zero further opens, so zero further bd-context preflights)
// and leave closing it to the drain.
func TestDrainHelpersBorrowTheTargetStore(t *testing.T) {
	opens, closes := installCountingNudgeStoreSeam(t)
	dir := t.TempDir()
	now := time.Now()
	item := newQueuedNudgeWithOptions("worker", "do work", "session", now, queuedNudgeOptions{ID: "n-borrow"})
	if err := enqueueQueuedNudge(dir, item); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	shared := openNudgeBeadStore(dir) // one open, via the counting seam
	target := nudgeTarget{cityPath: dir, store: shared, alias: "worker", identity: "worker"}

	before, closesBefore := *opens, *closes // enqueue above opened and closed its own store
	claimed, err := claimDueQueuedNudgesForTarget(dir, target, now)
	if err != nil {
		t.Fatalf("claimDueQueuedNudgesForTarget: %v", err)
	}
	if err := ackQueuedNudgesWithOutcomeUsingStore(dir, nil, shared, queuedNudgeIDs(claimed), "injected", "", "test"); err != nil {
		t.Fatalf("ackQueuedNudgesWithOutcomeUsingStore: %v", err)
	}
	if grew := *opens - before; grew != 0 {
		t.Fatalf("drain helpers opened %d store(s) despite a shared target store", grew)
	}
	if closed := *closes - closesBefore; closed != 0 {
		t.Fatalf("drain helpers closed the caller-owned shared store (%d close(s))", closed)
	}
}
