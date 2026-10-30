package doltorphan

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
)

func mkStoreDir(t *testing.T, root, name string, markerDepth int, mtime time.Time) string {
	t.Helper()
	dir := filepath.Join(root, name)
	markerParent := dir
	for i := 1; i < markerDepth; i++ {
		markerParent = filepath.Join(markerParent, "level")
	}
	if err := os.MkdirAll(markerParent, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", markerParent, err)
	}
	if markerDepth > 0 {
		if err := os.MkdirAll(filepath.Join(markerParent, ".dolt", "noms"), 0o755); err != nil {
			t.Fatalf("MkdirAll(.dolt/noms): %v", err)
		}
	}
	if err := chtimesRecursive(dir, mtime); err != nil {
		t.Fatalf("chtimesRecursive(%s): %v", dir, err)
	}
	return dir
}

func chtimesRecursive(dir string, mtime time.Time) error {
	return filepath.Walk(dir, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(path, mtime, mtime)
	})
}

// storeDirFor mirrors mkStoreDir's own "level"-per-depth-step layout to
// compute the directory that actually owns the .dolt marker it creates —
// i.e. the removal target a fixed Sweep must use, as opposed to the
// top-level candidate dir mkStoreDir returns directly. For markerDepth 1
// the store dir is the top-level dir itself; for markerDepth 2 or 3 it is
// one or two "level" segments below it.
func storeDirFor(dir string, markerDepth int) string {
	store := dir
	for i := 1; i < markerDepth; i++ {
		store = filepath.Join(store, "level")
	}
	return store
}

func noLsofHits(context.Context) ([]byte, error) { return nil, nil }

func TestSweep_RemovesOldMarkedUnheldDir(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	dir := mkStoreDir(t, root, "orphan1", 1, old)

	result := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits})

	if len(result.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	if len(result.Removed) != 1 || result.Removed[0] != dir {
		t.Fatalf("Removed = %v, want [%s]", result.Removed, dir)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dir %s should have been removed, stat err = %v", dir, err)
	}
}

func TestSweep_SkipsDirYoungerThanMinAge(t *testing.T) {
	root := t.TempDir()
	recent := time.Now().Add(-5 * time.Minute)
	dir := mkStoreDir(t, root, "fresh1", 1, recent)

	result := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits})

	if len(result.Removed) != 0 {
		t.Fatalf("Removed = %v, want none (too young)", result.Removed)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir %s should still exist: %v", dir, err)
	}
}

func TestSweep_SkipsDirWithoutDoltMarker(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	dir := mkStoreDir(t, root, "nomarker1", 0, old)

	result := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits})

	if len(result.Removed) != 0 {
		t.Fatalf("Removed = %v, want none (no .dolt marker)", result.Removed)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir %s should still exist: %v", dir, err)
	}
}

func TestSweep_FindsMarkerAtEachAllowedDepth(t *testing.T) {
	for _, depth := range []int{1, 2, 3} {
		t.Run(string(rune('0'+depth)), func(t *testing.T) {
			root := t.TempDir()
			old := time.Now().Add(-2 * time.Hour)
			dir := mkStoreDir(t, root, "orphan", depth, old)
			storeDir := storeDirFor(dir, depth)

			result := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits})

			if len(result.Removed) != 1 {
				t.Fatalf("depth %d: Removed = %v, want exactly one removal", depth, result.Removed)
			}
			if result.Removed[0] != storeDir {
				t.Fatalf("depth %d: Removed = %v, want [%s] (the marker-owning store dir, not the top-level candidate)", depth, result.Removed, storeDir)
			}
			if depth > 1 {
				if _, err := os.Stat(dir); err != nil {
					t.Fatalf("depth %d: top-level candidate %s should survive; only the nested store dir is removed: %v", depth, dir, err)
				}
			}
		})
	}
}

func TestSweep_IgnoresMarkerBeyondMaxDepth(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	dir := mkStoreDir(t, root, "toodeep", 4, old)

	result := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits})

	if len(result.Removed) != 0 {
		t.Fatalf("Removed = %v, want none (.dolt marker beyond depth 3)", result.Removed)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir %s should still exist: %v", dir, err)
	}
}

func TestSweep_SkipsLsofHeldDir(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	dir := mkStoreDir(t, root, "held1", 2, old)

	held := func(context.Context) ([]byte, error) {
		return []byte("dolt    1234 root   12r   REG  8,1  4096 55555 " + dir + "/noms/oldgen/000001.chunk\n"), nil
	}

	result := Sweep(SweepConfig{Root: root, RunLsof: held})

	if len(result.Removed) != 0 {
		t.Fatalf("Removed = %v, want none (lsof-held)", result.Removed)
	}
	if result.Skipped != 1 {
		t.Fatalf("Skipped = %d, want 1", result.Skipped)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir %s should still exist: %v", dir, err)
	}
}

func TestSweep_LsofErrorFailsClosed(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	dir := mkStoreDir(t, root, "orphan1", 1, old)

	boom := errors.New("lsof: command not found")
	failing := func(context.Context) ([]byte, error) { return nil, boom }

	result := Sweep(SweepConfig{Root: root, RunLsof: failing})

	if len(result.Removed) != 0 {
		t.Fatalf("Removed = %v, want none when lsof fails (fail closed)", result.Removed)
	}
	if len(result.Errors) == 0 {
		t.Fatalf("expected an error to be reported when lsof fails")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir %s should still exist: %v", dir, err)
	}
}

func writeLsofStub(t *testing.T, body string) string {
	t.Helper()
	return writeRawLsofStub(t, "printf 'partial\\n'\n"+body)
}

func writeRawLsofStub(t *testing.T, body string) string {
	t.Helper()
	command := filepath.Join(t.TempDir(), "lsof-stub")
	if err := os.WriteFile(command, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(%s): %v", command, err)
	}
	return command
}

func assertSweepFailedClosed(t *testing.T, result SweepResult, dir string) {
	t.Helper()
	if len(result.Removed) != 0 {
		t.Fatalf("Removed = %v, want none when the lsof scan is truncated", result.Removed)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("Errors = %v, want exactly the lsof scan error", result.Errors)
	}
	if result.Skipped != 1 {
		t.Fatalf("Skipped = %d, want 1", result.Skipped)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir %s should still exist: %v", dir, err)
	}
}

func TestSweep_LsofTimeoutFailsClosed(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	dir := mkStoreDir(t, root, "orphan1", 1, old)
	command := writeLsofStub(t, "exec tail -f /dev/null")

	result := Sweep(SweepConfig{
		Root:            root,
		lsofCommand:     command,
		lsofScanTimeout: 50 * time.Millisecond,
	})

	assertSweepFailedClosed(t, result, dir)
	if !errors.Is(result.Errors[0], context.DeadlineExceeded) {
		t.Fatalf("Errors[0] = %v, want context.DeadlineExceeded", result.Errors[0])
	}
}

func TestSweep_LsofKilledBySignalFailsClosed(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	dir := mkStoreDir(t, root, "orphan1", 1, old)
	command := writeLsofStub(t, "kill -9 $$")

	result := Sweep(SweepConfig{Root: root, lsofCommand: command})

	assertSweepFailedClosed(t, result, dir)
	var exitErr *exec.ExitError
	if !errors.As(result.Errors[0], &exitErr) || exitErr.Exited() {
		t.Fatalf("Errors[0] = %v, want signal termination of lsof", result.Errors[0])
	}
}

func TestSweep_LsofUnexpectedExitStatusFailsClosed(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	dir := mkStoreDir(t, root, "orphan1", 1, old)
	command := writeLsofStub(t, "exit 127")

	result := Sweep(SweepConfig{Root: root, lsofCommand: command})

	assertSweepFailedClosed(t, result, dir)
	var exitErr *exec.ExitError
	if !errors.As(result.Errors[0], &exitErr) || exitErr.ExitCode() != 127 {
		t.Fatalf("Errors[0] = %v, want lsof exit status 127", result.Errors[0])
	}
}

func TestSweep_LsofEmptyOutputFailsClosed(t *testing.T) {
	for _, status := range []string{"0", "1"} {
		t.Run("exit "+status, func(t *testing.T) {
			root := t.TempDir()
			old := time.Now().Add(-2 * time.Hour)
			dir := mkStoreDir(t, root, "orphan1", 1, old)
			command := writeRawLsofStub(t, "exit "+status)

			result := Sweep(SweepConfig{Root: root, lsofCommand: command})

			assertSweepFailedClosed(t, result, dir)
			if !errors.Is(result.Errors[0], errLsofNoOutput) {
				t.Fatalf("Errors[0] = %v, want errLsofNoOutput", result.Errors[0])
			}
		})
	}
}

func TestSweep_LsofExitOneKeepsOutput(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	held := mkStoreDir(t, root, "held1", 1, old)
	orphan := mkStoreDir(t, root, "orphan1", 1, old)
	command := writeRawLsofStub(t, "printf '%s\\n' 'dolt 1 u cwd DIR 0,1 0 1 "+held+"/.dolt'\nexit 1")

	result := Sweep(SweepConfig{Root: root, lsofCommand: command})

	if len(result.Errors) != 0 {
		t.Fatalf("Errors = %v, want none for an lsof that exits 1 with complete output", result.Errors)
	}
	if len(result.Removed) != 1 || result.Removed[0] != orphan {
		t.Fatalf("Removed = %v, want [%s]", result.Removed, orphan)
	}
	if _, err := os.Stat(held); err != nil {
		t.Fatalf("held dir %s should still exist: %v", held, err)
	}
}

func TestSweep_ContinuesAfterOneRemovalFails(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	dirA := mkStoreDir(t, root, "orphanA", 1, old)
	dirB := mkStoreDir(t, root, "orphanB", 1, old)

	removeAll := func(path string) error {
		if path == dirA {
			return errors.New("permission denied")
		}
		return os.RemoveAll(path)
	}

	result := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits, RemoveAll: removeAll})

	if len(result.Removed) != 1 || result.Removed[0] != dirB {
		t.Fatalf("Removed = %v, want [%s]", result.Removed, dirB)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("Errors = %v, want exactly one", result.Errors)
	}
	if _, err := os.Stat(dirA); err != nil {
		t.Fatalf("dirA should still exist after failed removal: %v", err)
	}
	if _, err := os.Stat(dirB); !os.IsNotExist(err) {
		t.Fatalf("dirB should have been removed: %v", err)
	}
}

func TestSweep_SkipsNonDirectoryEntries(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "not-a-dir"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(root, "not-a-dir"), old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	result := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits})

	if len(result.Removed) != 0 || len(result.Errors) != 0 {
		t.Fatalf("result = %+v, want no-op for a plain file", result)
	}
}

func TestSweep_RootReadErrorIsReported(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	result := Sweep(SweepConfig{Root: missing, RunLsof: noLsofHits})

	if len(result.Errors) == 0 {
		t.Fatalf("expected an error reading a missing root")
	}
	if len(result.Removed) != 0 {
		t.Fatalf("Removed = %v, want none", result.Removed)
	}
}

func TestSweep_DefaultMinAgeAppliesWhenUnset(t *testing.T) {
	root := t.TempDir()
	justUnderDefault := time.Now().Add(-DefaultMinAge + time.Minute)
	dir := mkStoreDir(t, root, "borderline", 1, justUnderDefault)

	result := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits})

	if len(result.Removed) != 0 {
		t.Fatalf("Removed = %v, want none (younger than DefaultMinAge)", result.Removed)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir %s should still exist: %v", dir, err)
	}
}

func TestSweep_UsesInjectedClock(t *testing.T) {
	root := t.TempDir()
	fixed := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	dir := mkStoreDir(t, root, "orphan1", 1, fixed.Add(-2*time.Hour))

	fake := &clock.Fake{Time: fixed}
	result := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits, Clock: fake})

	if len(result.Removed) != 1 || result.Removed[0] != dir {
		t.Fatalf("Removed = %v, want [%s] under fake clock", result.Removed, dir)
	}
}

func TestSweep_MultipleCandidatesMixedOutcomes(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	recent := time.Now().Add(-time.Minute)

	removeMe := mkStoreDir(t, root, "remove-me", 2, old)
	removeMeStore := storeDirFor(removeMe, 2)
	tooYoung := mkStoreDir(t, root, "too-young", 2, recent)
	noMarker := mkStoreDir(t, root, "no-marker", 0, old)

	held := func(context.Context) ([]byte, error) {
		return []byte(filepath.Join(root, "held-dir") + "/noms/x.chunk\n"), nil
	}
	heldDir := mkStoreDir(t, root, "held-dir", 1, old)

	result := Sweep(SweepConfig{Root: root, RunLsof: held})

	if len(result.Removed) != 1 || result.Removed[0] != removeMeStore {
		t.Fatalf("Removed = %v, want exactly [%s]", result.Removed, removeMeStore)
	}
	if _, err := os.Stat(removeMe); err != nil {
		t.Fatalf("top-level container %s should survive; only its nested store dir is removed: %v", removeMe, err)
	}
	for _, d := range []string{tooYoung, noMarker, heldDir} {
		if _, err := os.Stat(d); err != nil {
			t.Fatalf("dir %s should still exist: %v", d, err)
		}
	}
}

// buildContainerWithNestedStoreAndSiblings recreates the ga-zuzfel incident
// shape (see the ga-txnhdk architect ruling and the repro test on
// repro/ga-zuzfel-vartmp-sweep): a top-level container that legitimately
// owns unrelated payload (a binary, a script) and also happens to hold a
// Dolt database copy three levels down, exactly at maxMarkerDepth. Before
// this fix, Sweep treated the whole container as the removal candidate
// once it found the nested marker; the fix must remove only the store dir
// that owns the marker and leave the container and its unrelated payload
// untouched.
func buildContainerWithNestedStoreAndSiblings(t *testing.T, root string, mtime time.Time) (container, storeDir, binary, script string) {
	t.Helper()

	container = filepath.Join(root, "container")
	storeDir = filepath.Join(container, "dropped-db", "shared")
	if err := os.MkdirAll(filepath.Join(storeDir, ".dolt", "noms"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.dolt/noms): %v", err)
	}

	binary = filepath.Join(container, "rollback-binary")
	if err := os.WriteFile(binary, []byte("binary payload"), 0o755); err != nil {
		t.Fatalf("WriteFile(binary): %v", err)
	}
	script = filepath.Join(container, "cleanup.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(script): %v", err)
	}

	// Age everything last: writing any child refreshes the parent's
	// mtime, which is the field Sweep reads for the top-level candidate.
	if err := chtimesRecursive(container, mtime); err != nil {
		t.Fatalf("chtimesRecursive(%s): %v", container, err)
	}
	return container, storeDir, binary, script
}

func TestSweep_IncidentShape_RemovesOnlyNestedStoreLeavesSiblingsIntact(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	container, storeDir, binary, script := buildContainerWithNestedStoreAndSiblings(t, root, old)

	result := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits})

	if len(result.Removed) != 1 || result.Removed[0] != storeDir {
		t.Fatalf("Removed = %v, want exactly [%s]", result.Removed, storeDir)
	}
	if _, err := os.Stat(storeDir); !os.IsNotExist(err) {
		t.Fatalf("store dir %s should have been removed, stat err = %v", storeDir, err)
	}
	if _, err := os.Stat(container); err != nil {
		t.Fatalf("top-level container %s should survive: %v", container, err)
	}
	if _, err := os.Stat(binary); err != nil {
		t.Fatalf("unrelated binary %s should survive sweep of its container: %v", binary, err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("unrelated script %s should survive sweep of its container: %v", script, err)
	}
}

// TestSweep_SentinelFileExemptsTopLevelCandidate covers the additive
// opt-out from the ga-txnhdk architect ruling: a literal ".no-orphan-sweep"
// file directly inside a top-level candidate exempts it entirely,
// regardless of age or any nested .dolt marker. This is a per-directory
// marker the owner places deliberately, not a name/prefix filter on the
// sweep candidate itself (that approach was rejected by the ruling).
func TestSweep_SentinelFileExemptsTopLevelCandidate(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	dir := mkStoreDir(t, root, "protected", 2, old)
	storeDir := storeDirFor(dir, 2)

	if err := os.WriteFile(filepath.Join(dir, ".no-orphan-sweep"), nil, 0o644); err != nil {
		t.Fatalf("WriteFile(sentinel): %v", err)
	}
	// Re-age dir: writing the sentinel just refreshed its mtime.
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatalf("Chtimes(%s): %v", dir, err)
	}

	result := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits})

	if len(result.Removed) != 0 {
		t.Fatalf("Removed = %v, want none (sentinel file present)", result.Removed)
	}
	if _, err := os.Stat(storeDir); err != nil {
		t.Fatalf("nested store dir %s should survive under sentinel exemption: %v", storeDir, err)
	}
}

// TestSweep_NoReapMarkerExemptsTopLevelCandidate covers the ".gc-no-reap"
// keep marker that packs/actual's vartmp-scratch-reaper already honors
// (ga-v83niq). Agents on a shared host mark a directory they keep on
// purpose with that name, so a sweep that honored only ".no-orphan-sweep"
// deleted a kept Dolt clone that carried it. Same contract as the sentinel
// above: a marker directly inside a top-level candidate exempts it
// regardless of age or any nested .dolt marker.
func TestSweep_NoReapMarkerExemptsTopLevelCandidate(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	dir := mkStoreDir(t, root, "kept-clone", 2, old)
	storeDir := storeDirFor(dir, 2)

	if err := os.WriteFile(filepath.Join(dir, ".gc-no-reap"), nil, 0o644); err != nil {
		t.Fatalf("WriteFile(.gc-no-reap): %v", err)
	}
	// Re-age dir: writing the marker just refreshed its mtime.
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatalf("Chtimes(%s): %v", dir, err)
	}

	result := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits})

	if len(result.Removed) != 0 {
		t.Fatalf("Removed = %v, want none (.gc-no-reap marker present)", result.Removed)
	}
	if _, err := os.Stat(storeDir); err != nil {
		t.Fatalf("nested store dir %s should survive under the .gc-no-reap marker: %v", storeDir, err)
	}
}

// TestSweep_SiblingStoresNeverWidenToContainer pins findDoltStoreDir's
// behavior when a top-level container holds stores in separate sibling
// subtrees: the first store in lexical order is removed this pass, and
// neither the container nor the other store is touched.
func TestSweep_SiblingStoresNeverWidenToContainer(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)

	container := filepath.Join(root, "c")
	storeA := filepath.Join(container, "a")
	storeB := filepath.Join(container, "b", "x")
	for _, store := range []string{storeA, storeB} {
		if err := os.MkdirAll(filepath.Join(store, ".dolt", "noms"), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s/.dolt/noms): %v", store, err)
		}
	}
	if err := chtimesRecursive(container, old); err != nil {
		t.Fatalf("chtimesRecursive(%s): %v", container, err)
	}

	result := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits})

	if len(result.Removed) != 1 || result.Removed[0] != storeA {
		t.Fatalf("Removed = %v, want exactly [%s]", result.Removed, storeA)
	}
	for _, d := range []string{container, storeB} {
		if _, err := os.Stat(d); err != nil {
			t.Fatalf("%s should survive sweeping sibling store %s: %v", d, storeA, err)
		}
	}
}

// TestSweep_KeepMarkersAndFailClosedLsofScanCompose pins the keep-marker and
// fail-closed guarantees together. One root holds a .gc-no-reap Dolt clone, a
// container whose nested store sits beside unrelated payload, and a flat
// orphan. While the lsof scan is incomplete nothing is removed, and only the
// container and the orphan count as skipped: the kept clone is exempt before
// the scan runs. Once the scan completes, exactly the nested store and the
// orphan go; the kept clone, the container and its payload survive.
func TestSweep_KeepMarkersAndFailClosedLsofScanCompose(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour)

	type fixture struct {
		root        string
		nestedStore string
		orphan      string
		// kept lists every path that must outlive any pass.
		kept []string
	}
	build := func(t *testing.T) fixture {
		t.Helper()
		root := t.TempDir()
		clone := mkStoreDir(t, root, "kept-clone", 2, old)
		if err := os.WriteFile(filepath.Join(clone, ".gc-no-reap"), nil, 0o644); err != nil {
			t.Fatalf("WriteFile(.gc-no-reap): %v", err)
		}
		// Re-age clone: writing the marker just refreshed its mtime.
		if err := os.Chtimes(clone, old, old); err != nil {
			t.Fatalf("Chtimes(%s): %v", clone, err)
		}
		container, nestedStore, binary, script := buildContainerWithNestedStoreAndSiblings(t, root, old)
		return fixture{
			root:        root,
			nestedStore: nestedStore,
			orphan:      mkStoreDir(t, root, "orphan1", 1, old),
			kept:        []string{storeDirFor(clone, 2), container, binary, script},
		}
	}
	assertExist := func(t *testing.T, paths ...string) {
		t.Helper()
		for _, p := range paths {
			if _, err := os.Stat(p); err != nil {
				t.Fatalf("%s should still exist: %v", p, err)
			}
		}
	}

	t.Run("incomplete scan removes nothing", func(t *testing.T) {
		f := build(t)

		result := Sweep(SweepConfig{Root: f.root, lsofCommand: writeLsofStub(t, "exit 127")})

		if len(result.Removed) != 0 {
			t.Fatalf("Removed = %v, want none when the lsof scan is incomplete", result.Removed)
		}
		if len(result.Errors) != 1 {
			t.Fatalf("Errors = %v, want exactly the lsof scan error", result.Errors)
		}
		if result.Skipped != 2 {
			t.Fatalf("Skipped = %d, want 2 (the container and the orphan, not the kept clone)", result.Skipped)
		}
		assertExist(t, append([]string{f.nestedStore, f.orphan}, f.kept...)...)
	})

	t.Run("complete scan removes only the stores", func(t *testing.T) {
		f := build(t)

		result := Sweep(SweepConfig{Root: f.root, lsofCommand: writeLsofStub(t, "exit 0")})

		if len(result.Errors) != 0 {
			t.Fatalf("Errors = %v, want none for a complete scan", result.Errors)
		}
		if len(result.Removed) != 2 || result.Removed[0] != f.nestedStore || result.Removed[1] != f.orphan {
			t.Fatalf("Removed = %v, want exactly [%s %s]", result.Removed, f.nestedStore, f.orphan)
		}
		for _, p := range result.Removed {
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Fatalf("%s should have been removed, stat err = %v", p, err)
			}
		}
		assertExist(t, f.kept...)
	})
}

// mkdirs creates each directory, parents included.
func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", d, err)
		}
	}
}

// writeFile creates path with placeholder content, parents included.
func writeFile(t *testing.T, path string) {
	t.Helper()
	mkdirs(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

// ageTree backdates everything under dir to two hours ago. Call it after the
// last write: creating or removing an entry refreshes its parent's mtime, which
// is the field Sweep reads for a top-level candidate.
func ageTree(t *testing.T, dir string) {
	t.Helper()
	if err := chtimesRecursive(dir, time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatalf("chtimesRecursive(%s): %v", dir, err)
	}
}

// pastMinAge anchors fake DefaultMinAge+5m past the real now and asserts that
// top's mtime is at least DefaultMinAge behind it. Removing an entry sets its
// container's mtime to the real now, so a later pass on a real or past-anchored
// clock never sees the container as old, and "nothing removed" would then pass
// on the age gate alone. Call it before every pass of a multi-pass test.
func pastMinAge(t *testing.T, fake *clock.Fake, top string) {
	t.Helper()
	fake.Time = time.Now().Add(DefaultMinAge + 5*time.Minute)
	info, err := os.Stat(top)
	if err != nil {
		t.Fatalf("Stat(%s): %v", top, err)
	}
	if age := fake.Now().Sub(info.ModTime()); age < DefaultMinAge {
		t.Fatalf("%s is only %v old on the fake clock, want at least %v: a pass here would be gated by age, not by the store rule", top, age, DefaultMinAge)
	}
}

func requireExist(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s should still exist: %v", p, err)
		}
	}
}

func requireNotExist(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatalf("%s should have been removed, stat err = %v", p, err)
		}
	}
}

// snapshot lists every path under dir, symlinks unfollowed, in lexical order.
func snapshot(t *testing.T, dir string) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(dir, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%s): %v", dir, err)
	}
	return paths
}

// requireSweepLeftTreeAlone asserts a pass found no candidate: nothing
// removed, skipped or reported, and every path under dir still present.
func requireSweepLeftTreeAlone(t *testing.T, result SweepResult, dir string, before []string) {
	t.Helper()
	if len(result.Removed) != 0 || result.Skipped != 0 || len(result.Errors) != 0 {
		t.Fatalf("result = %+v, want a no-op", result)
	}
	if after := snapshot(t, dir); !slices.Equal(before, after) {
		t.Fatalf("tree under %s changed:\nbefore %v\nafter  %v", dir, before, after)
	}
}

// TestSweep_AncestorMarkerNeverBecomesTarget is the ga-rsmp8s edge, run over
// three passes: a data dir that a `dolt sql-server --data-dir` left a
// bookkeeping .dolt in (no noms/), holding one real store and unrelated
// payload. Pass 1 removes only the store. A .dolt without noms/ is no store, so
// the data dir never becomes the target on a later pass and the payload
// survives. The store carries noms/ (ga-1bxwou: a bare .dolt is no store).
func TestSweep_AncestorMarkerNeverBecomesTarget(t *testing.T) {
	root := t.TempDir()
	x := filepath.Join(root, "x")
	db := filepath.Join(x, "db")
	bookkeeping := filepath.Join(x, ".dolt", "sql-server.info")
	binary := filepath.Join(x, "binary")
	keep := filepath.Join(x, "payload", "keep.txt")
	mkdirs(t, filepath.Join(db, ".dolt", "noms"))
	writeFile(t, bookkeeping)
	writeFile(t, binary)
	writeFile(t, keep)
	ageTree(t, x)

	fake := &clock.Fake{}
	for pass := 1; pass <= 3; pass++ {
		pastMinAge(t, fake, x)

		result := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits, Clock: fake})

		if len(result.Errors) != 0 {
			t.Fatalf("pass %d: unexpected errors: %v", pass, result.Errors)
		}
		var want []string
		if pass == 1 {
			want = []string{db}
		}
		if !slices.Equal(result.Removed, want) {
			t.Fatalf("pass %d: Removed = %v, want %v", pass, result.Removed, want)
		}
		requireExist(t, x, filepath.Dir(bookkeeping), bookkeeping, binary, keep)
	}
	requireNotExist(t, db)
}

// TestSweep_AncestorThatIsAStoreGoesWhole pins the other half: nested stores
// are preferred over their ancestor, and an ancestor that is itself a store
// goes whole (files beside its .dolt included) once no store is nested in it.
func TestSweep_AncestorThatIsAStoreGoesWhole(t *testing.T) {
	root := t.TempDir()
	c := filepath.Join(root, "c")
	db := filepath.Join(c, "db")
	mkdirs(t, filepath.Join(c, ".dolt", "noms"), filepath.Join(db, ".dolt", "noms"))
	writeFile(t, filepath.Join(c, "payload.txt"))
	ageTree(t, c)

	fake := &clock.Fake{}
	for pass, want := range [][]string{{db}, {c}} {
		pastMinAge(t, fake, c)

		result := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits, Clock: fake})

		if len(result.Errors) != 0 || !slices.Equal(result.Removed, want) {
			t.Fatalf("pass %d: Removed = %v, errors = %v, want %v", pass+1, result.Removed, result.Errors, want)
		}
	}
	requireNotExist(t, c)
}

// TestSweep_SiblingStoresConvergeOnePerAge pins the accepted one-store-per-pass
// behavior for two stores that are direct children of the top-level directory:
// removing the first refreshes the directory's mtime, so an immediate second
// sweep sees a young directory and removes nothing, and the second store goes
// once another MinAge has passed. Payload beside them survives throughout.
func TestSweep_SiblingStoresConvergeOnePerAge(t *testing.T) {
	root := t.TempDir()
	top := filepath.Join(root, "top")
	a, b := filepath.Join(top, "a"), filepath.Join(top, "b")
	payload := filepath.Join(top, "payload.txt")
	mkdirs(t, filepath.Join(a, ".dolt", "noms"), filepath.Join(b, ".dolt", "noms"))
	writeFile(t, payload)
	ageTree(t, top)

	first := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits})
	if len(first.Errors) != 0 || !slices.Equal(first.Removed, []string{a}) {
		t.Fatalf("first pass: Removed = %v, errors = %v, want [%s]", first.Removed, first.Errors, a)
	}

	// Same clock, immediately after: the removal made top young again.
	second := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits})
	if len(second.Removed) != 0 || len(second.Errors) != 0 {
		t.Fatalf("immediate second pass: Removed = %v, errors = %v, want none", second.Removed, second.Errors)
	}
	requireExist(t, b, payload)

	fake := &clock.Fake{}
	pastMinAge(t, fake, top)
	third := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits, Clock: fake})
	if len(third.Errors) != 0 || !slices.Equal(third.Removed, []string{b}) {
		t.Fatalf("pass after another MinAge: Removed = %v, errors = %v, want [%s]", third.Removed, third.Errors, b)
	}
	requireExist(t, top, payload)
	requireNotExist(t, a, b)
}

// TestSweep_HomeShapedTreeIsNeverACandidate covers a test HOME: its own .dolt
// is Dolt's global-config directory, not a store, so the aged HOME is not a
// candidate and the sweep never reaches the lsof scan.
func TestSweep_HomeShapedTreeIsNeverACandidate(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	for _, f := range []string{
		filepath.Join(".dolt", "config_global.json"),
		filepath.Join(".dolt", "eventsData", "events.json"),
		filepath.Join(".beads", "config.yaml"),
		filepath.Join(".claude", "settings.json"),
		".bash_history",
	} {
		writeFile(t, filepath.Join(home, f))
	}
	ageTree(t, home)
	before := snapshot(t, home)

	lsofCalls := 0
	runLsof := func(context.Context) ([]byte, error) {
		lsofCalls++
		return nil, nil
	}
	result := Sweep(SweepConfig{Root: root, RunLsof: runLsof})

	requireSweepLeftTreeAlone(t, result, home, before)
	if lsofCalls != 0 {
		t.Fatalf("RunLsof called %d times, want 0: a tree with no store is not a candidate", lsofCalls)
	}
}

// TestSweep_DoltDirWithoutNomsIsNotAStore covers every shape of .dolt that
// holds no database. Each is aged and sits directly in the top-level directory,
// so only the store rule stands between it and removal.
func TestSweep_DoltDirWithoutNomsIsNotAStore(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T, dolt string)
	}{
		{"empty .dolt", func(*testing.T, string) {}},
		{"sql-server.info only", func(t *testing.T, dolt string) {
			writeFile(t, filepath.Join(dolt, "sql-server.info"))
		}},
		{"tmp only", func(t *testing.T, dolt string) {
			mkdirs(t, filepath.Join(dolt, "tmp"))
		}},
		{"global config", func(t *testing.T, dolt string) {
			writeFile(t, filepath.Join(dolt, "config_global.json"))
			writeFile(t, filepath.Join(dolt, "eventsData", "events.json"))
		}},
		{"noms is a regular file", func(t *testing.T, dolt string) {
			writeFile(t, filepath.Join(dolt, "noms"))
		}},
		{"noms is a symlink to a directory", func(t *testing.T, dolt string) {
			target := filepath.Join(filepath.Dir(dolt), "elsewhere")
			mkdirs(t, target)
			if err := os.Symlink(target, filepath.Join(dolt, "noms")); err != nil {
				t.Fatalf("Symlink: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			top := filepath.Join(root, "top")
			dolt := filepath.Join(top, ".dolt")
			mkdirs(t, dolt)
			tc.build(t, dolt)
			writeFile(t, filepath.Join(top, "payload.txt"))
			ageTree(t, top)
			before := snapshot(t, top)

			result := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits})

			requireSweepLeftTreeAlone(t, result, top, before)
		})
	}
}

// TestSweep_UnreadableDoltDirIsNotAStore pins "a probe error means not a
// store": the sweep errs toward keeping what it cannot inspect.
func TestSweep_UnreadableDoltDirIsNotAStore(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode 0 directory")
	}
	root := t.TempDir()
	top := filepath.Join(root, "top")
	dolt := filepath.Join(top, ".dolt")
	mkdirs(t, filepath.Join(dolt, "noms"))
	ageTree(t, top)
	if err := os.Chmod(dolt, 0); err != nil {
		t.Fatalf("Chmod(%s): %v", dolt, err)
	}
	t.Cleanup(func() { _ = os.Chmod(dolt, 0o755) })

	result := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits})

	if len(result.Removed) != 0 || result.Skipped != 0 || len(result.Errors) != 0 {
		t.Fatalf("result = %+v, want a no-op", result)
	}
	requireExist(t, top, dolt)
}

// TestSweep_StoreAtTopLevelIsRemovedWhole pins whole-store-directory removal:
// a store that is itself the top-level directory goes with everything beside
// its .dolt.
func TestSweep_StoreAtTopLevelIsRemovedWhole(t *testing.T) {
	root := t.TempDir()
	top := filepath.Join(root, "top")
	mkdirs(t, filepath.Join(top, ".dolt", "noms"))
	writeFile(t, filepath.Join(top, "payload.txt"))
	ageTree(t, top)

	result := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits})

	if len(result.Errors) != 0 || !slices.Equal(result.Removed, []string{top}) {
		t.Fatalf("Removed = %v, errors = %v, want [%s]", result.Removed, result.Errors, top)
	}
	requireNotExist(t, top)
}

// TestSweep_StoreBeyondTheSearchWindowIsLeftAlone pins the documented limit of
// the search window. A .dolt is seen only when its directory is at most two
// levels below the top-level child (top/a/b/.dolt); the store top/a/b/db/.dolt
// is one level further out, so a bookkeeping .dolt beside it removes nothing.
// Update this test if maxMarkerDepth is ever widened.
func TestSweep_StoreBeyondTheSearchWindowIsLeftAlone(t *testing.T) {
	root := t.TempDir()
	top := filepath.Join(root, "top")
	b := filepath.Join(top, "a", "b")
	writeFile(t, filepath.Join(b, ".dolt", "sql-server.info"))
	mkdirs(t, filepath.Join(b, "db", ".dolt", "noms"))
	ageTree(t, top)
	before := snapshot(t, top)

	result := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits})

	requireSweepLeftTreeAlone(t, result, top, before)
}

// TestSweep_DoltDirIsNeverDescendedInto pins that a .dolt directory is a probe
// target and never a place to search for stores. A real store carries a nested
// <db>/.dolt/stats/.dolt/noms, so exploring .dolt would remove that
// Dolt-internal sub-store, partially gutting the store, instead of the store
// itself. A no-noms .dolt is no store, and a store tucked inside it is not
// reachable either.
func TestSweep_DoltDirIsNeverDescendedInto(t *testing.T) {
	t.Run("a store with a nested Dolt-internal store goes whole", func(t *testing.T) {
		root := t.TempDir()
		top := filepath.Join(root, "top")
		mkdirs(t, filepath.Join(top, ".dolt", "noms"), filepath.Join(top, ".dolt", "stats", ".dolt", "noms"))
		writeFile(t, filepath.Join(top, "payload.txt"))
		ageTree(t, top)

		result := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits})

		if len(result.Errors) != 0 || !slices.Equal(result.Removed, []string{top}) {
			t.Fatalf("Removed = %v, errors = %v, want [%s]", result.Removed, result.Errors, top)
		}
		requireNotExist(t, top)
	})

	t.Run("a store inside a no-noms .dolt is not a target", func(t *testing.T) {
		root := t.TempDir()
		top := filepath.Join(root, "top")
		mkdirs(t, filepath.Join(top, ".dolt", "inner", ".dolt", "noms"))
		writeFile(t, filepath.Join(top, "payload.txt"))
		ageTree(t, top)
		before := snapshot(t, top)

		result := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits})

		requireSweepLeftTreeAlone(t, result, top, before)
	})
}
