//go:build (linux && !android) || (darwin && !ios)

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// proxyReaderPath replicates the deployed manifold proxy's reader transform
// (gc-manifold-proxy.go: sanitizeSession(affinity)+".json") INDEPENDENTLY of the
// writer's runMapFileName, so a round-trip test proves the writer publishes
// where the proxy actually reads rather than merely that the writer agrees with
// itself.
func proxyReaderPath(dir, affinity string) string {
	sanitized := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, affinity)
	return filepath.Join(dir, sanitized+".json")
}

// TestWriteRunMapMatchesProxyReaderContract is the writer↔reader round-trip: the
// proxy stamps X-Gc-Run-Id only if it can open
// <dir>/<sanitizeSession(affinity)>.json and decode run_id. It resolves that
// path with proxyReaderPath (the proxy's own transform, not runMapFileName) and
// asserts the writer's published file is readable there with the right run id —
// the exact cross-process break attempt-2's blocker flagged when the writer
// diverged onto a hashed filename.
func TestWriteRunMapMatchesProxyReaderContract(t *testing.T) {
	dir := privateRunMapTestDir(t)
	t.Setenv("GC_RUNMAP_DIR", dir)
	const affinity = "gascity/gc.implementation-worker-1"
	if err := writeRunMap("run-42", "bead-42", affinity); err != nil {
		t.Fatalf("writeRunMap: %v", err)
	}
	raw, err := os.ReadFile(proxyReaderPath(dir, affinity))
	if err != nil {
		t.Fatalf("writer did not publish at the proxy reader path: %v", err)
	}
	var m struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("proxy cannot decode the run-map entry: %v", err)
	}
	if m.RunID != "run-42" {
		t.Errorf("proxy would stamp run_id %q, want run-42", m.RunID)
	}
}

// TestWriteRunMapErrorsWhenAllPublishesFail proves the major fix: when every
// non-empty key fails to publish, writeRunMap returns the first failure instead
// of a silent nil. The only key's final name is pre-occupied by a directory, so
// os.Rename fails for it deterministically and uid-independently (not even root
// renames a file over a directory).
func TestWriteRunMapErrorsWhenAllPublishesFail(t *testing.T) {
	dir := privateRunMapTestDir(t)
	t.Setenv("GC_RUNMAP_DIR", dir)
	if err := os.Mkdir(filepath.Join(dir, runMapFileName("sess")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeRunMap("run-x", "bead-x", "sess"); err == nil {
		t.Fatal("expected an error when every per-key publish fails")
	}
}

// TestWriteRunMapErrorsWhenDirUnwritable is attempt-2's exact cited scenario: a
// dir that passes the owner-only publish-safety gate but is not writable by us,
// so os.CreateTemp EACCESes on every key. Before the fix writeRunMap returned a
// silent nil; now it surfaces the failure. Root bypasses mode bits, so skip
// under euid 0.
func TestWriteRunMapErrorsWhenDirUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write-permission bits")
	}
	ro := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(ro, 0o555); err != nil { // owner-only perms, but no write bit
		t.Fatal(err)
	}
	if !runMapDirSafeToPublish(ro) {
		t.Fatalf("an owner-only 0o555 dir should pass the publish-safety gate")
	}
	t.Setenv("GC_RUNMAP_DIR", ro)
	if err := writeRunMap("run-x", "bead-x", "sess"); err == nil {
		t.Fatal("expected an error when a gate-safe dir is not writable to us")
	}
}

// TestWriteRunMapBestEffortOnPartialFailure fixes the boundary of the major
// change: a per-key failure that still leaves at least one file published stays
// best-effort (nil); it is not promoted to an error. One key's target is blocked
// by a directory (rename fails); the other publishes normally.
func TestWriteRunMapBestEffortOnPartialFailure(t *testing.T) {
	dir := privateRunMapTestDir(t)
	t.Setenv("GC_RUNMAP_DIR", dir)
	if err := os.Mkdir(filepath.Join(dir, runMapFileName("blocked")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeRunMap("run-ok", "bead-ok", "blocked", "good"); err != nil {
		t.Fatalf("a partial failure with one publish should stay best-effort, got %v", err)
	}
	if _, err := os.Stat(proxyReaderPath(dir, "good")); err != nil {
		t.Errorf("the writable key should still publish: %v", err)
	}
}

// TestWriteRunMapSelfProvisionsOwnerOnlyDir exercises the fresh-directory create
// path: a non-existent GC_RUNMAP_DIR override is created owner-only (writable by
// neither group nor other) so it passes the publish-safety gate.
func TestWriteRunMapSelfProvisionsOwnerOnlyDir(t *testing.T) {
	base := t.TempDir()
	sub := filepath.Join(base, "fresh")
	t.Setenv("GC_RUNMAP_DIR", sub)
	if err := writeRunMap("run-fresh", "bead-fresh", "sess"); err != nil {
		t.Fatalf("writeRunMap: %v", err)
	}
	info, err := os.Stat(sub)
	if err != nil {
		t.Fatalf("expected self-provisioned dir: %v", err)
	}
	if info.Mode().Perm()&0o022 != 0 {
		t.Errorf("self-provisioned dir mode = %v, want writable by neither group nor other", info.Mode().Perm())
	}
	if !runMapDirSafeToPublish(sub) {
		t.Errorf("self-provisioned dir should be safe to publish into")
	}
	if _, err := os.Stat(filepath.Join(sub, runMapFileName("sess"))); err != nil {
		t.Errorf("expected published file in fresh dir: %v", err)
	}
}

// TestWriteRunMapRefusesGroupOtherWritableDir proves the CWE-732 gate: a
// non-sticky group/other-writable dir is refused with a surfaced error and no
// file is published, instead of silently trusting a dir any local user can
// clobber.
func TestWriteRunMapRefusesGroupOtherWritableDir(t *testing.T) {
	dir := privateRunMapTestDir(t)
	unsafe := filepath.Join(dir, "unsafe")
	if err := os.Mkdir(unsafe, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafe, 0o777); err != nil { // world-writable, no sticky
		t.Fatal(err)
	}
	t.Setenv("GC_RUNMAP_DIR", unsafe)
	err := writeRunMap("run-x", "bead-x", "sess")
	if err == nil {
		t.Fatalf("expected an error refusing a non-sticky world-writable dir")
	}
	entries, _ := os.ReadDir(unsafe)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" {
			t.Errorf("nothing should be published into a refused dir, found %s", e.Name())
		}
	}
}

// TestWriteRunMapAllowsStickyOwnedDir proves the intended multi-user handoff is
// still honored: a sticky dir owned by this user (the shape a root/self
// provisioner installs) is trusted and published into.
func TestWriteRunMapAllowsStickyOwnedDir(t *testing.T) {
	dir := privateRunMapTestDir(t)
	handoff := filepath.Join(dir, "handoff")
	if err := os.Mkdir(handoff, 0o755); err != nil {
		t.Fatal(err)
	}
	// os.FileMode carries the sticky bit as ModeSticky, not the literal 0o1000.
	if err := os.Chmod(handoff, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if !runMapDirSafeToPublish(handoff) {
		t.Fatalf("a sticky self-owned dir should be a trusted handoff")
	}
	t.Setenv("GC_RUNMAP_DIR", handoff)
	if err := writeRunMap("run-h", "bead-h", "sess"); err != nil {
		t.Fatalf("writeRunMap: %v", err)
	}
	if _, err := os.Stat(filepath.Join(handoff, runMapFileName("sess"))); err != nil {
		t.Errorf("expected published file in sticky handoff dir: %v", err)
	}
}

// TestPruneRunMapSkipsUnsafeDir proves the prune gate: the writer does not scan
// (or reap in) a shared group/other-writable dir, where an in-process reap is
// both a no-op and a claim-latency amplifier (CWE-400).
func TestPruneRunMapSkipsUnsafeDir(t *testing.T) {
	dir := privateRunMapTestDir(t)
	shared := filepath.Join(dir, "shared")
	if err := os.Mkdir(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o1777); err != nil { // group/other-writable
		t.Fatal(err)
	}
	stale := filepath.Join(shared, "dead.json")
	if err := os.WriteFile(stale, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-100 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if runMapDirPrunable(shared) {
		t.Fatalf("a group/other-writable dir must not be prunable")
	}
	pruneRunMap(shared, time.Now(), time.Hour)
	if _, err := os.Stat(stale); err != nil {
		t.Errorf("prune must skip an unsafe shared dir, but the stale file is gone: %v", err)
	}
}

// TestPruneRunMapBoundedByBudget proves a single claim-path prune scans at most
// runMapPruneScanBudget entries, so the reap cost never scales with directory
// size: with more stale files than the budget, one prune leaves at least the
// overflow behind.
func TestPruneRunMapBoundedByBudget(t *testing.T) {
	dir := privateRunMapTestDir(t)
	total := runMapPruneScanBudget + 10
	old := time.Now().Add(-100 * time.Hour)
	for i := 0; i < total; i++ {
		f := filepath.Join(dir, fmt.Sprintf("stale-%d.json", i))
		// A real run-map entry (run_id + bead_id) so pruneRunMap recognizes it as
		// this writer's own file and reaps it; the budget bounds the scan.
		if err := os.WriteFile(f, []byte(`{"run_id":"r","bead_id":"b"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(f, old, old); err != nil {
			t.Fatal(err)
		}
	}
	pruneRunMap(dir, time.Now(), time.Hour)
	entries, _ := os.ReadDir(dir)
	remaining := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" {
			remaining++
		}
	}
	if remaining < total-runMapPruneScanBudget {
		t.Errorf("one prune removed more than the budget: %d of %d remain, want >= %d",
			remaining, total, total-runMapPruneScanBudget)
	}
	if remaining >= total {
		t.Errorf("prune removed nothing: %d of %d remain", remaining, total)
	}
}

// TestDefaultRunMapDirMatchesProxy locks the zero-config default to the manifold
// proxy's own default directory (gc-manifold-proxy.go's runmapDir). The two must
// be byte-identical or the proxy reads a path the writer never publishes into
// and X-Gc-Run-Id is never stamped.
func TestDefaultRunMapDirMatchesProxy(t *testing.T) {
	if got, want := defaultRunMapDir(), "/run/gc-manifold-runmap"; got != want {
		t.Errorf("defaultRunMapDir() = %q, want %q (the proxy's runmapDir default)", got, want)
	}
}

// TestWriteRunMapRefusesSymlinkTarget proves the writer refuses a squatted
// target. A hostile co-tenant pre-plants the predictable <session>.json as a
// symlink to a file it controls; the proxy's os.ReadFile would follow it and
// trust an attacker-editable run_id. The writer must surface an error and neither
// follow the link (rewriting the attacker's file through it) nor report success.
func TestWriteRunMapRefusesSymlinkTarget(t *testing.T) {
	dir := privateRunMapTestDir(t)
	t.Setenv("GC_RUNMAP_DIR", dir)
	const key = "victim-session"
	forged := filepath.Join(dir, "attacker-forged.json")
	if err := os.WriteFile(forged, []byte(`{"run_id":"forged"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, runMapFileName(key))
	if err := os.Symlink(forged, target); err != nil {
		t.Fatal(err)
	}
	if err := writeRunMap("run-legit", "bead-legit", key); err == nil {
		t.Fatal("expected an error refusing a symlink run-map target (possible squat)")
	}
	li, err := os.Lstat(target)
	if err != nil {
		t.Fatalf("lstat target: %v", err)
	}
	if li.Mode()&os.ModeSymlink == 0 {
		t.Errorf("writer clobbered the symlink target instead of refusing it")
	}
	if raw, _ := os.ReadFile(forged); strings.Contains(string(raw), "run-legit") {
		t.Errorf("writer followed the symlink and wrote through it: %s", raw)
	}
}

// TestWriteRunMapSquatForcesErrorDespiteOtherPublish closes the silent-targeted
// -squat gap: squatting only the proxy-read session-name file, while another key
// publishes cleanly, previously returned a silent nil (published>0) and hid the
// forgery. The squat must now surface an error even though the clean key lands.
func TestWriteRunMapSquatForcesErrorDespiteOtherPublish(t *testing.T) {
	dir := privateRunMapTestDir(t)
	t.Setenv("GC_RUNMAP_DIR", dir)
	const squatted = "worker-1"    // the proxy-read session name, pre-squatted
	const clean = "worker-1-actor" // another key that publishes normally
	forged := filepath.Join(dir, "forged.json")
	if err := os.WriteFile(forged, []byte(`{"run_id":"forged"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(forged, filepath.Join(dir, runMapFileName(squatted))); err != nil {
		t.Fatal(err)
	}
	if err := writeRunMap("run-real", "bead-real", squatted, clean); err == nil {
		t.Fatal("a squatted proxy-read target must surface an error even when another key publishes")
	}
	if _, err := os.Stat(proxyReaderPath(dir, clean)); err != nil {
		t.Errorf("the clean key should still publish best-effort: %v", err)
	}
}

// TestWriteRunMapRefreshesOwnRegularFile proves the squat check does not break a
// normal refresh: a pre-existing regular file this user owns (a prior claim's
// publish) is overwritten with the new run id, not refused as a squat.
func TestWriteRunMapRefreshesOwnRegularFile(t *testing.T) {
	dir := privateRunMapTestDir(t)
	t.Setenv("GC_RUNMAP_DIR", dir)
	const key = "sess"
	if err := os.WriteFile(filepath.Join(dir, runMapFileName(key)), []byte(`{"run_id":"old"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeRunMap("run-new", "bead-new", key); err != nil {
		t.Fatalf("refreshing our own run-map file must succeed: %v", err)
	}
	raw, err := os.ReadFile(proxyReaderPath(dir, key))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.RunID != "run-new" {
		t.Errorf("refresh left run_id = %q, want run-new", m.RunID)
	}
}

// TestWriteRunMapToSilentWhenDefaultUncreatable proves the no-proxy-noise fix: a
// zero-config default that cannot be created (no proxy, non-root worker) is a
// silent no-op, while an explicit GC_RUNMAP_DIR override that cannot be created
// is still surfaced so an operator misconfiguration is diagnosable.
func TestWriteRunMapToSilentWhenDefaultUncreatable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can create under a read-only parent")
	}
	ro := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	uncreatable := filepath.Join(ro, "gc-manifold-runmap")
	if err := writeRunMapTo(uncreatable, false, "run-x", "bead-x", "sess"); err != nil {
		t.Fatalf("an uncreatable zero-config default must be a silent no-op, got %v", err)
	}
	if err := writeRunMapTo(uncreatable, true, "run-x", "bead-x", "sess"); err == nil {
		t.Fatal("an explicit GC_RUNMAP_DIR that cannot be created must surface an error")
	}
}
