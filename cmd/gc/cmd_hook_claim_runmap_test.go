package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func privateRunMapTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("make run-map test dir owner-only: %v", err)
	}
	return dir
}

func TestSanitizeRunMapKey(t *testing.T) {
	cases := map[string]string{
		"gc__review-synthesizer-mc-1kkqd": "gc__review-synthesizer-mc-1kkqd",
		"keep.dot_under-dash":             "keep.dot_under-dash",
		"a/b c":                           "a_b_c",
		"actor@beads.test":                "actor_beads.test",
		"x:y|z":                           "x_y_z",
	}
	for in, want := range cases {
		if got := sanitizeRunMapKey(in); got != want {
			t.Errorf("sanitizeRunMapKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWriteRunMapWritesPerKeyAtomically(t *testing.T) {
	dir := privateRunMapTestDir(t)
	t.Setenv("GC_RUNMAP_DIR", dir)

	// duplicate key ("sess/name" twice), an empty key, and a distinct key
	if err := writeRunMap("run-123", "bead-456", "sess/name", "sess/name", "", "actor@x"); err != nil {
		t.Fatalf("writeRunMap: %v", err)
	}

	for _, key := range []string{"sess/name", "actor@x"} {
		name := runMapFileName(key)
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("expected %s: %v", name, err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("bad json in %s: %v", name, err)
		}
		if m["run_id"] != "run-123" {
			t.Errorf("%s run_id = %v, want run-123", name, m["run_id"])
		}
		if m["bead_id"] != "bead-456" {
			t.Errorf("%s bead_id = %v, want bead-456", name, m["bead_id"])
		}
	}

	// exactly two files (duplicate key deduped, empty key skipped) and no
	// leftover .tmp files (atomic rename completed)
	entries, _ := os.ReadDir(dir)
	jsonCount := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("leftover tmp file: %s", e.Name())
		}
		if filepath.Ext(e.Name()) == ".json" {
			jsonCount++
		}
	}
	if jsonCount != 2 {
		t.Errorf("expected 2 run-map files, got %d", jsonCount)
	}
}

func TestWriteRunMapEmptyRunIDNoOp(t *testing.T) {
	dir := privateRunMapTestDir(t)
	t.Setenv("GC_RUNMAP_DIR", dir)
	if err := writeRunMap("", "bead-456", "sess"); err != nil {
		t.Fatalf("writeRunMap: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("empty runID should write nothing, got %d entries", len(entries))
	}
}

func TestWriteRunMapHonorsDirOverride(t *testing.T) {
	dir := privateRunMapTestDir(t)
	sub := filepath.Join(dir, "runmap")
	t.Setenv("GC_RUNMAP_DIR", sub)
	if err := writeRunMap("run-9", "bead-9", "only"); err != nil {
		t.Fatalf("writeRunMap: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sub, runMapFileName("only"))); err != nil {
		t.Fatalf("expected override dir to be created and written: %v", err)
	}
}

func TestRunMapTTL(t *testing.T) {
	t.Setenv("GC_RUNMAP_TTL", "")
	if got := runMapTTL(); got != 48*time.Hour {
		t.Errorf("default runMapTTL = %v, want 48h", got)
	}
	t.Setenv("GC_RUNMAP_TTL", "2h")
	if got := runMapTTL(); got != 2*time.Hour {
		t.Errorf("override runMapTTL = %v, want 2h", got)
	}
	// A bad/zero duration falls back to the default rather than disabling reaping.
	t.Setenv("GC_RUNMAP_TTL", "garbage")
	if got := runMapTTL(); got != 48*time.Hour {
		t.Errorf("bad GC_RUNMAP_TTL should fall back to 48h, got %v", got)
	}
}

func TestPruneRunMapReapsStaleKeepsFresh(t *testing.T) {
	dir := privateRunMapTestDir(t)
	fresh := filepath.Join(dir, "fresh.json")
	stale := filepath.Join(dir, "stale.json")
	notJSON := filepath.Join(dir, "keep.txt") // non-.json is never touched
	// A reapable file must decode as a real run-map entry (run_id + bead_id); that
	// is what the writer publishes and what pruneRunMap now requires before it will
	// unlink a stale .json.
	for _, f := range []string{fresh, stale, notJSON} {
		if err := os.WriteFile(f, []byte(`{"run_id":"r","bead_id":"b"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	old := now.Add(-100 * time.Hour)
	for _, f := range []string{stale, notJSON} {
		if err := os.Chtimes(f, old, old); err != nil {
			t.Fatal(err)
		}
	}
	pruneRunMap(dir, now, 48*time.Hour)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale .json should be pruned, stat err = %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh .json should be kept: %v", err)
	}
	if _, err := os.Stat(notJSON); err != nil {
		t.Errorf("non-.json file should never be pruned: %v", err)
	}
}

func TestWriteRunMapPrunesStaleOnWrite(t *testing.T) {
	dir := privateRunMapTestDir(t)
	t.Setenv("GC_RUNMAP_DIR", dir)
	t.Setenv("GC_RUNMAP_TTL", "1h")
	stale := filepath.Join(dir, "dead-session.json")
	if err := os.WriteFile(stale, []byte(`{"run_id":"r","bead_id":"b"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if err := writeRunMap("run-x", "bead-x", "live-session"); err != nil {
		t.Fatalf("writeRunMap: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale file should be pruned on write, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, runMapFileName("live-session"))); err != nil {
		t.Errorf("live session's file should be written: %v", err)
	}
}

// TestPruneRunMapReapsStaleTmpOrphans proves the tmp-cleanup fix: a process
// killed between CreateTemp and rename leaks a "<key>.json.<rand>.tmp" orphan
// that the ".json"-only prune used to leave forever. A .tmp older than the TTL is
// a dead-write orphan and is reaped; a fresh .tmp (a possible in-flight write) is
// left untouched.
func TestPruneRunMapReapsStaleTmpOrphans(t *testing.T) {
	dir := privateRunMapTestDir(t)
	staleTmp := filepath.Join(dir, "sess.json.1234.tmp")
	freshTmp := filepath.Join(dir, "live.json.5678.tmp")
	for _, f := range []string{staleTmp, freshTmp} {
		if err := os.WriteFile(f, []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-100 * time.Hour)
	if err := os.Chtimes(staleTmp, old, old); err != nil {
		t.Fatal(err)
	}
	pruneRunMap(dir, time.Now(), time.Hour)
	if _, err := os.Stat(staleTmp); !os.IsNotExist(err) {
		t.Errorf("stale .tmp orphan should be pruned, stat err = %v", err)
	}
	if _, err := os.Stat(freshTmp); err != nil {
		t.Errorf("fresh .tmp (a possible in-flight write) must be kept: %v", err)
	}
}

// TestWriteRunMapKeepsForeignFilesInExplicitDir proves the namespace-aware prune:
// pruneRunMap reaps only the writer's own run-map files, so an operator who points
// GC_RUNMAP_DIR at a directory that also holds unrelated files does not lose them
// on the claim hot path. An unrelated stale config.json (valid JSON, but not a
// run-map entry) and an unrelated stale cache.tmp (not the writer's
// "<stem>.json.<rand>.tmp" temp shape) both survive a writeRunMap that prunes,
// while the writer's own stale entry is still reaped and the live session
// publishes. Before the fix, prune matched any stale ".json"/".tmp" by mtime alone
// and silently deleted both foreign files.
func TestWriteRunMapKeepsForeignFilesInExplicitDir(t *testing.T) {
	dir := privateRunMapTestDir(t)

	foreignJSON := filepath.Join(dir, "config.json") // unrelated app config
	foreignTmp := filepath.Join(dir, "cache.tmp")    // unrelated temp file
	ownEntry := filepath.Join(dir, runMapFileName("dead-session"))
	if err := os.WriteFile(foreignJSON, []byte(`{"listen":":8080","debug":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreignTmp, []byte("cache blob"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ownEntry, []byte(`{"run_id":"r","bead_id":"b","ts":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Age all three past the default 48h TTL so they are reap candidates, and
	// publish via writeRunMapTo with the dir supplied directly (explicit=true, the
	// GC_RUNMAP_DIR-override case) so the test reads no environment.
	old := time.Now().Add(-50 * time.Hour)
	for _, f := range []string{foreignJSON, foreignTmp, ownEntry} {
		if err := os.Chtimes(f, old, old); err != nil {
			t.Fatal(err)
		}
	}

	if err := writeRunMapTo(dir, true, "run-live", "bead-live", "live-session"); err != nil {
		t.Fatalf("writeRunMapTo: %v", err)
	}

	// Unrelated files an explicit GC_RUNMAP_DIR happens to share must survive.
	if _, err := os.Stat(foreignJSON); err != nil {
		t.Errorf("unrelated config.json must not be pruned: %v", err)
	}
	if _, err := os.Stat(foreignTmp); err != nil {
		t.Errorf("unrelated cache.tmp must not be pruned: %v", err)
	}
	// The writer's own stale entry is still reaped, and the live session publishes.
	if _, err := os.Stat(ownEntry); !os.IsNotExist(err) {
		t.Errorf("the writer's own stale entry should still be pruned, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, runMapFileName("live-session"))); err != nil {
		t.Errorf("the live session's file should be written: %v", err)
	}
}

// TestRunMapTempOrphanName pins the writer's temp-file namespace: pruneRunMap
// reaps a "<stem>.json.<rand>.tmp" orphan (what os.CreateTemp produces from
// runMapFileName+".*.tmp") but never an unrelated .tmp that lacks the ".json."
// infix, so a foreign cache.tmp in an explicit GC_RUNMAP_DIR is left alone.
func TestRunMapTempOrphanName(t *testing.T) {
	owned := []string{
		"sess.json.1234.tmp",
		runMapFileName("gascity/gc.worker-1") + ".987654321.tmp",
		"a.json.x.tmp",
	}
	foreign := []string{
		"cache.tmp",     // no ".json." infix
		"config.json",   // a published file, not a temp
		"notes.txt",     // unrelated
		"data.json.tmp", // no random component CreateTemp always inserts
		"plain.tmp",     // no ".json." infix
	}
	for _, n := range owned {
		if !runMapTempOrphanName(n) {
			t.Errorf("runMapTempOrphanName(%q) = false, want true (writer temp shape)", n)
		}
	}
	for _, n := range foreign {
		if runMapTempOrphanName(n) {
			t.Errorf("runMapTempOrphanName(%q) = true, want false (foreign file)", n)
		}
	}
}

// TestRunMapEntryIsUnauthenticatedBestEffortTelemetry pins the shipped security
// contract documented on writeRunMap: the run-map, and the X-Gc-Run-Id header the
// manifold proxy stamps from it, are UNAUTHENTICATED best-effort telemetry that
// must not feed billing, authorization, or audit decisions. It guards the two
// load-bearing facts behind that contract so a later change cannot quietly
// promote the header into an authoritative signal without tripping this test and
// forcing the writeRunMap contract to be revisited:
//
//   - a same-uid regular file pre-planted at the predictable path is accepted and
//     overwritten, so the writer cannot distinguish a co-uid forgery from a
//     genuine publish (the residual intra-fleet-uid forgeability tracked in
//     ga-zzvsuls); and
//   - the published entry carries only run_id/bead_id/ts — no nonce, signature,
//     token, or any other integrity/authentication field a consumer could verify.
func TestRunMapEntryIsUnauthenticatedBestEffortTelemetry(t *testing.T) {
	dir := privateRunMapTestDir(t)
	const key = "sess"

	// A same-uid regular file at the predictable name simulates a co-uid cell's
	// forgery. The unauthenticated channel accepts and overwrites it — there is no
	// authenticity check that could reject a same-uid file — which is exactly why
	// the mapping is best-effort telemetry, not an authoritative signal. Publish
	// via writeRunMapTo with the dir supplied directly: this pins the writer's
	// publish behavior without depending on GC_RUNMAP_DIR resolution (covered by
	// TestWriteRunMapHonorsDirOverride).
	target := filepath.Join(dir, runMapFileName(key))
	if err := os.WriteFile(target, []byte(`{"run_id":"forged","bead_id":"forged","ts":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeRunMapTo(dir, true, "run-real", "bead-real", key); err != nil {
		t.Fatalf("a same-uid regular file must be accepted (unauthenticated best-effort channel), got %v", err)
	}

	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("expected the entry to be published: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("bad json in run-map entry: %v", err)
	}
	if m["run_id"] != "run-real" {
		t.Errorf("run_id = %v, want run-real (writer must overwrite the same-uid file)", m["run_id"])
	}
	// The payload is unauthenticated: exactly the three telemetry fields, with no
	// integrity/authentication token a consumer could verify. A new field here
	// means the wire contract changed — revisit the writeRunMap security contract
	// (and any downstream trust of X-Gc-Run-Id) before landing it.
	allowed := map[string]bool{"run_id": true, "bead_id": true, "ts": true}
	for k := range m {
		if !allowed[k] {
			t.Errorf("run-map entry carries unexpected field %q; the contract is unauthenticated telemetry (run_id/bead_id/ts only)", k)
		}
	}
}
