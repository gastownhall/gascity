//go:build windows

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gcruntime "github.com/gastownhall/gascity/internal/runtime"
)

func TestRunMapHasNoZeroConfigDirOnWindows(t *testing.T) {
	if runMapHasZeroConfigDir() {
		t.Fatal("Windows must not have a zero-config run-map dir")
	}
}

// On Windows "/run/gc-manifold-runmap" names a directory at the root of the
// current drive. With GC_RUNMAP_DIR unset, a claim's run-map publish must
// create nothing there and report no error.
func TestWriteRunMapWithoutOverrideCreatesNothingOnWindows(t *testing.T) {
	t.Setenv("GC_RUNMAP_DIR", "")
	if runMapHasZeroConfigDir() {
		t.Fatal("Windows must not have a zero-config run-map dir")
	}
	root, err := filepath.Abs(`\run`)
	if err != nil {
		t.Fatal(err)
	}
	rootExisted := false
	if _, err := os.Lstat(root); err == nil {
		rootExisted = true
		t.Logf("%s already exists, so this test only checks the zero-config predicate", root)
	} else if !os.IsNotExist(err) {
		t.Fatalf("checking %s: %v", root, err)
	}
	// A regression creates the directory and then refuses to publish into it, so
	// it stays empty; remove only what this test made.
	t.Cleanup(func() {
		if !rootExisted {
			_ = os.Remove(filepath.Join(root, "gc-manifold-runmap"))
			_ = os.Remove(root)
		}
	})

	if err := writeRunMap("run-1", "ga-1", "gate-worker-ga-1", "ga-1"); err != nil {
		t.Errorf("writeRunMap() = %v, want nil with GC_RUNMAP_DIR unset", err)
	}
	if !rootExisted {
		if _, err := os.Lstat(root); err == nil {
			t.Errorf("writeRunMap created %s", root)
		} else if !os.IsNotExist(err) {
			t.Fatalf("checking %s after writeRunMap: %v", root, err)
		}
	}
}

func privateWindowsRunMapDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "runmap")
	if err := gcruntime.EnsurePrivateDir(dir); err != nil {
		t.Fatalf("creating private run-map dir: %v", err)
	}
	return dir
}

func grantEveryoneRunMapWrite(t *testing.T, dir string) {
	t.Helper()
	out, err := exec.Command("icacls.exe", dir, "/grant", "*S-1-1-0:(M)").CombinedOutput()
	if err != nil {
		t.Fatalf("granting Everyone write to %s: %v\n%s", dir, err, out)
	}
}

func grantEveryoneRunMapRights(t *testing.T, dir, rights string) {
	t.Helper()
	out, err := exec.Command("icacls.exe", dir, "/grant", "*S-1-1-0:"+rights).CombinedOutput()
	if err != nil {
		t.Fatalf("granting Everyone %s to %s: %v\n%s", rights, dir, err, out)
	}
}

func TestWriteRunMapPublishesInPrivateOverrideOnWindows(t *testing.T) {
	dir := privateWindowsRunMapDir(t)
	if !runMapDirSafeToPublish(dir) {
		t.Fatalf("private run-map dir %s should be safe to publish into", dir)
	}
	t.Setenv("GC_RUNMAP_DIR", dir)
	if err := writeRunMap("run-private", "bead-private", "worker-1"); err != nil {
		t.Fatalf("writeRunMap: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, runMapFileName("worker-1")))
	if err != nil {
		t.Fatalf("reading published run-map entry: %v", err)
	}
	var entry runMapEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatalf("decoding published run-map entry: %v", err)
	}
	if entry.RunID != "run-private" || entry.BeadID != "bead-private" {
		t.Fatalf("published entry = %+v, want run-private/bead-private", entry)
	}
}

func TestWriteRunMapRefusesEveryoneWritableOverrideOnWindows(t *testing.T) {
	dir := privateWindowsRunMapDir(t)
	grantEveryoneRunMapWrite(t, dir)
	if runMapDirSafeToPublish(dir) {
		t.Fatalf("Everyone-writable run-map dir %s must not be safe to publish into", dir)
	}
	t.Setenv("GC_RUNMAP_DIR", dir)
	err := writeRunMap("run-public", "bead-public", "worker-1")
	if err == nil {
		t.Fatal("writeRunMap accepted an Everyone-writable run-map dir")
	}
	if !strings.Contains(err.Error(), "trusted system principal") || !strings.Contains(err.Error(), "refusing to publish") {
		t.Fatalf("writeRunMap error = %v, want a DACL-based publish refusal", err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("refused run-map dir contains published entries: %v", matches)
	}
}

func TestWriteRunMapRefusesInheritOnlyEntryWriteOnWindows(t *testing.T) {
	dir := privateWindowsRunMapDir(t)
	grantEveryoneRunMapRights(t, dir, "(OI)(IO)(M)")
	if runMapDirSafeToPublish(dir) {
		t.Fatalf("run-map dir %s must not be safe when created entries inherit Everyone write access", dir)
	}
	if err := verifyRunMapDirNotWritableByOthers(dir); err == nil || !strings.Contains(err.Error(), "inheritable access") {
		t.Fatalf("verifyRunMapDirNotWritableByOthers = %v, want an inherited-entry refusal", err)
	}
	if runMapDirPrunable(dir) {
		t.Fatalf("run-map dir %s must not be prunable when created entries inherit Everyone write access", dir)
	}
	t.Setenv("GC_RUNMAP_DIR", dir)
	err := writeRunMap("run-public-entry", "bead-public-entry", "worker-1")
	if err == nil {
		t.Fatal("writeRunMap accepted a run-map dir whose entries inherit Everyone write access")
	}
	if !strings.Contains(err.Error(), "refusing to publish") {
		t.Fatalf("writeRunMap error = %v, want a publish refusal", err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("refused run-map dir contains published entries: %v", matches)
	}
}

func TestWriteRunMapRefusesReplaceableAncestorOnWindows(t *testing.T) {
	dir := privateWindowsRunMapDir(t)
	parent := filepath.Dir(dir)
	link := filepath.Join(t.TempDir(), "junction")
	out, err := exec.Command("cmd.exe", "/c", "mklink", "/J", link, parent).CombinedOutput()
	if err != nil {
		t.Fatalf("creating junction to %s: %v\n%s", parent, err, out)
	}
	defer os.Remove(link)
	viaJunction := filepath.Join(link, filepath.Base(dir))
	if runMapDirSafeToPublish(viaJunction) {
		t.Fatalf("run-map dir %s must not be safe when reached through an ancestor junction", viaJunction)
	}

	grantEveryoneRunMapRights(t, parent, "(F)")
	if runMapDirSafeToPublish(dir) {
		t.Fatalf("run-map dir %s must not be safe under an Everyone-replaceable parent", dir)
	}
	if err := verifyRunMapDirNotWritableByOthers(dir); err == nil || !strings.Contains(err.Error(), "ancestor") {
		t.Fatalf("verifyRunMapDirNotWritableByOthers = %v, want an ancestor refusal", err)
	}
	if runMapDirPrunable(dir) {
		t.Fatalf("run-map dir %s must not be prunable under an Everyone-replaceable parent", dir)
	}
	t.Setenv("GC_RUNMAP_DIR", dir)
	err = writeRunMap("run-replaceable-parent", "bead-replaceable-parent", "worker-1")
	if err == nil {
		t.Fatal("writeRunMap accepted a run-map dir under an Everyone-replaceable parent")
	}
	if !strings.Contains(err.Error(), "refusing to publish") {
		t.Fatalf("writeRunMap error = %v, want a publish refusal", err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("refused run-map dir contains published entries: %v", matches)
	}
}

func TestPruneRunMapRefusesEveryoneWritableOverrideOnWindows(t *testing.T) {
	dir := privateWindowsRunMapDir(t)
	stale := filepath.Join(dir, runMapFileName("stale"))
	if err := os.WriteFile(stale, []byte(`{"run_id":"old","bead_id":"old"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-100 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	grantEveryoneRunMapWrite(t, dir)
	if runMapDirPrunable(dir) {
		t.Fatalf("Everyone-writable run-map dir %s must not be prunable", dir)
	}
	pruneRunMap(dir, time.Now(), time.Hour)
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("pruneRunMap removed an entry from an Everyone-writable dir: %v", err)
	}
}
