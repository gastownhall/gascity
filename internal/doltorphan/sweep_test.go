package doltorphan

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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
		if err := os.MkdirAll(filepath.Join(markerParent, ".dolt"), 0o755); err != nil {
			t.Fatalf("MkdirAll(.dolt): %v", err)
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
	if err := os.MkdirAll(filepath.Join(storeDir, ".dolt"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.dolt): %v", err)
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
		if err := os.MkdirAll(filepath.Join(store, ".dolt"), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s/.dolt): %v", store, err)
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
