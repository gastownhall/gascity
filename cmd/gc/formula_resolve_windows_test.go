//go:build windows

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestResolveFormulasWindowsFallbackCopiesAreManaged(t *testing.T) {
	attempts := forceFormulaSymlinkPrivilegeError(t)

	dir := t.TempDir()
	layer1 := filepath.Join(dir, "layer1")
	layer2 := filepath.Join(dir, "layer2")
	writeFormulaFile(t, layer1, "mol-a.toml", "layer1")
	writeFormulaFile(t, layer1, "mol-b.toml", "b v1")
	writeFormulaFile(t, layer2, "mol-a.toml", "layer2 v1")

	target := filepath.Join(dir, "rig")
	if err := ResolveFormulas(target, []string{layer1, layer2}); err != nil {
		t.Fatalf("ResolveFormulas first: %v", err)
	}
	if *attempts == 0 {
		t.Fatal("ResolveFormulas did not attempt to create a symlink before falling back")
	}
	formulaDir := filepath.Join(target, ".beads", "formulas")
	assertManagedFormulaCopy(t, filepath.Join(formulaDir, "mol-a.toml"), []byte("layer2 v1"))
	assertManagedFormulaCopy(t, filepath.Join(formulaDir, "mol-a.formula.toml"), []byte("layer2 v1"))
	assertManagedFormulaCopy(t, filepath.Join(formulaDir, "mol-b.toml"), []byte("b v1"))
	assertManagedFormulaCopy(t, filepath.Join(formulaDir, "mol-b.formula.toml"), []byte("b v1"))

	before := snapshotFormulaDir(t, formulaDir)
	if err := ResolveFormulas(target, []string{layer1, layer2}); err != nil {
		t.Fatalf("ResolveFormulas idempotent: %v", err)
	}
	after := snapshotFormulaDir(t, formulaDir)
	if !equalSnapshots(before, after) {
		t.Fatalf("second ResolveFormulas changed managed copies:\nbefore=%#v\nafter=%#v", before, after)
	}

	writeFormulaFile(t, layer2, "mol-a.toml", "layer2 v2")
	if err := ResolveFormulas(target, []string{layer1, layer2}); err != nil {
		t.Fatalf("ResolveFormulas after content change: %v", err)
	}
	assertManagedFormulaCopy(t, filepath.Join(formulaDir, "mol-a.toml"), []byte("layer2 v2"))
	assertManagedFormulaCopy(t, filepath.Join(formulaDir, "mol-a.formula.toml"), []byte("layer2 v2"))

	if err := os.Remove(filepath.Join(layer1, "mol-b.toml")); err != nil {
		t.Fatal(err)
	}
	if err := ResolveFormulas(target, []string{layer1, layer2}); err != nil {
		t.Fatalf("ResolveFormulas after stale removal: %v", err)
	}
	for _, name := range []string{"mol-b.toml", "mol-b.formula.toml"} {
		if _, err := os.Lstat(filepath.Join(formulaDir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s should have been removed as a stale managed copy; err=%v", name, err)
		}
	}
}

func TestResolveFormulasWindowsFallbackPreservesUnmanagedRealFiles(t *testing.T) {
	forceFormulaSymlinkPrivilegeError(t)

	dir := t.TempDir()
	layer := filepath.Join(dir, "layer")
	writeFormulaFile(t, layer, "mol-real.toml", "winner")

	target := filepath.Join(dir, "rig")
	formulaDir := filepath.Join(target, ".beads", "formulas")
	if err := os.MkdirAll(formulaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	unmanaged := filepath.Join(formulaDir, "mol-real.toml")
	if err := os.WriteFile(unmanaged, []byte("unmanaged"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ResolveFormulas(target, []string{layer}); err != nil {
		t.Fatalf("ResolveFormulas: %v", err)
	}
	got, err := os.ReadFile(unmanaged)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "unmanaged" {
		t.Fatalf("unmanaged real file was overwritten with %q", got)
	}
	assertManagedFormulaCopy(t, filepath.Join(formulaDir, "mol-real.formula.toml"), []byte("winner"))

	if err := os.Remove(filepath.Join(layer, "mol-real.toml")); err != nil {
		t.Fatal(err)
	}
	if err := ResolveFormulas(target, []string{layer}); err != nil {
		t.Fatalf("ResolveFormulas cleanup: %v", err)
	}
	got, err = os.ReadFile(unmanaged)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "unmanaged" {
		t.Fatalf("unmanaged real file was removed or changed to %q", got)
	}
	if _, err := os.Lstat(filepath.Join(formulaDir, "mol-real.formula.toml")); !os.IsNotExist(err) {
		t.Fatalf("managed legacy alias should have been removed; err=%v", err)
	}
}

func TestResolveFormulasWindowsPreservesModifiedManagedCopy(t *testing.T) {
	forceFormulaSymlinkPrivilegeError(t)

	dir := t.TempDir()
	layer := filepath.Join(dir, "layer")
	writeFormulaFile(t, layer, "mol-user.toml", "generated v1")

	target := filepath.Join(dir, "rig")
	if err := ResolveFormulas(target, []string{layer}); err != nil {
		t.Fatalf("ResolveFormulas first: %v", err)
	}

	formulaDir := filepath.Join(target, ".beads", "formulas")
	path := filepath.Join(formulaDir, "mol-user.toml")
	if err := os.WriteFile(path, []byte("user authored"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFormulaFile(t, layer, "mol-user.toml", "generated v2")

	if err := ResolveFormulas(target, []string{layer}); err != nil {
		t.Fatalf("ResolveFormulas update: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "user authored" {
		t.Fatalf("modified managed copy was not preserved: %q", got)
	}
	if managedFormulaManifestHasEntryForTest(t, formulaDir, "mol-user.toml") {
		t.Fatal("modified managed copy still recorded in manifest")
	}
}

func TestFormulaCleanupPreservesReplacement(t *testing.T) {
	forceFormulaSymlinkPrivilegeError(t)

	dir := t.TempDir()
	layer := filepath.Join(dir, "layer")
	writeFormulaFile(t, layer, "mol-clean.toml", "generated")

	target := filepath.Join(dir, "rig")
	if err := ResolveFormulas(target, []string{layer}); err != nil {
		t.Fatalf("ResolveFormulas first: %v", err)
	}

	formulaDir := filepath.Join(target, ".beads", "formulas")
	path := filepath.Join(formulaDir, "mol-clean.toml")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("user replacement"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(layer, "mol-clean.toml")); err != nil {
		t.Fatal(err)
	}

	if err := ResolveFormulas(target, []string{layer}); err != nil {
		t.Fatalf("ResolveFormulas cleanup: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "user replacement" {
		t.Fatalf("replacement changed during cleanup: %q", got)
	}
}

func TestFormulaSymlinkPrivilegeRecoveryPreservesReplacement(t *testing.T) {
	orig := formulaSymlink
	formulaSymlink = func(oldname, newname string) error {
		return &os.LinkError{Op: "symlink", Old: oldname, New: newname, Err: windows.ERROR_PRIVILEGE_NOT_HELD}
	}
	t.Cleanup(func() {
		formulaSymlink = orig
	})

	dir := t.TempDir()
	layer := filepath.Join(dir, "layer")
	writeFormulaFile(t, layer, "mol-link.toml", "generated")

	target := filepath.Join(dir, "rig")
	if err := ResolveFormulas(target, []string{layer}); err != nil {
		t.Fatalf("ResolveFormulas first: %v", err)
	}

	formulaDir := filepath.Join(target, ".beads", "formulas")
	path := filepath.Join(formulaDir, "mol-link.toml")
	if err := os.WriteFile(path, []byte("user replacement"), 0o644); err != nil {
		t.Fatal(err)
	}

	replacementAttempts := 0
	formulaSymlink = func(_, newname string) error {
		if strings.HasPrefix(filepath.Base(newname), ".mol-link.toml.symlink-") {
			replacementAttempts++
		}
		return os.WriteFile(newname, []byte("replacement symlink stand-in"), 0o644)
	}

	if err := ResolveFormulas(target, []string{layer}); err != nil {
		t.Fatalf("ResolveFormulas with symlink privilege restored: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "user replacement" {
		t.Fatalf("replacement was overwritten when symlink privilege returned: %q", got)
	}
	if replacementAttempts != 0 {
		t.Fatalf("attempted to replace user-owned copy with symlink %d time(s)", replacementAttempts)
	}
}

func TestResolveFormulasWindowsRecoversCopyWrittenBeforeManifest(t *testing.T) {
	forceFormulaSymlinkPrivilegeError(t)

	dir := t.TempDir()
	layer := filepath.Join(dir, "layer")
	writeFormulaFile(t, layer, "mol-crash.toml", "v1")

	target := filepath.Join(dir, "rig")
	formulaDir := filepath.Join(target, ".beads", "formulas")
	if err := os.MkdirAll(formulaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	state := &formulaResolutionState{manifest: formulaManagedManifest{
		Version: 1,
		Entries: map[string]formulaManagedCopy{},
	}}
	path := filepath.Join(formulaDir, "mol-crash.toml")
	if err := ensureFormulaEntry(formulaDir, "mol-crash.toml", filepath.Join(layer, "mol-crash.toml"), state); err != nil {
		t.Fatal(err)
	}

	writeFormulaFile(t, layer, "mol-crash.toml", "v2")
	if err := ResolveFormulas(target, []string{layer}); err != nil {
		t.Fatalf("ResolveFormulas after interrupted pass: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v2" {
		t.Fatalf("orphaned copy was not refreshed: %q", got)
	}
}

func TestResolveFormulasWindowsCompletesInterruptedManagedCopyPublication(t *testing.T) {
	forceFormulaSymlinkPrivilegeError(t)

	for _, tc := range []struct {
		name     string
		existing string
		recorded string
		pending  string
	}{
		{
			name:    "intent saved before first rename",
			pending: "v1",
		},
		{
			name:     "update intent saved before rename",
			existing: "old",
			recorded: "old",
			pending:  "new",
		},
		{
			name:     "update renamed before final manifest save",
			existing: "new",
			recorded: "old",
			pending:  "new",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			layer := filepath.Join(dir, "layer")
			writeFormulaFile(t, layer, "mol-pending.toml", "new")
			target := filepath.Join(dir, "rig")
			formulaDir := filepath.Join(target, ".beads", "formulas")
			if err := os.MkdirAll(formulaDir, 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(formulaDir, "mol-pending.toml")
			if tc.existing != "" {
				if err := os.WriteFile(path, []byte(tc.existing), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			writeManagedFormulaManifestForTest(t, formulaDir, "mol-pending.toml", filepath.Join(layer, "mol-pending.toml"), tc.recorded, tc.pending)

			if err := ResolveFormulas(target, []string{layer}); err != nil {
				t.Fatalf("ResolveFormulas: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "new" {
				t.Fatalf("copy content = %q, want new", got)
			}
			entry := readManagedFormulaManifestEntryForTest(t, formulaDir, "mol-pending.toml")
			if got := entry["sha256"]; got != sha256Bytes([]byte("new")) {
				t.Fatalf("recorded hash = %q, want hash of new", got)
			}
			if pending, ok := entry["pending_sha256"]; ok && pending != "" {
				t.Fatalf("pending hash still recorded: %q", pending)
			}
		})
	}
}

func TestResolveFormulasWindowsDoubleInterruptedUpdateKeepsOwnership(t *testing.T) {
	forceFormulaSymlinkPrivilegeError(t)

	dir := t.TempDir()
	layer := filepath.Join(dir, "layer")
	writeFormulaFile(t, layer, "mol-double.toml", "p2")
	target := filepath.Join(dir, "rig")
	formulaDir := filepath.Join(target, ".beads", "formulas")
	if err := os.MkdirAll(formulaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(formulaDir, "mol-double.toml")
	if err := os.WriteFile(path, []byte("p1"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeManagedFormulaManifestForTest(t, formulaDir, "mol-double.toml", filepath.Join(layer, "mol-double.toml"), "s0", "p1")

	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := ResolveFormulas(target, []string{layer}); err == nil {
		t.Fatal("ResolveFormulas unexpectedly completed the simulated interrupted update")
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ResolveFormulas(target, []string{layer}); err != nil {
		t.Fatalf("ResolveFormulas after double interruption: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "p2" {
		t.Fatalf("double-interrupted copy was abandoned: %q", got)
	}
	entry := readManagedFormulaManifestEntryForTest(t, formulaDir, "mol-double.toml")
	if got := entry["sha256"]; got != sha256Bytes([]byte("p2")) {
		t.Fatalf("recorded hash = %q, want hash of p2", got)
	}
	if pending, ok := entry["pending_sha256"]; ok && pending != "" {
		t.Fatalf("pending hash still recorded: %q", pending)
	}
}

func TestResolveFormulasWindowsConcurrentResolversKeepOwnership(t *testing.T) {
	orig := formulaSymlink
	t.Cleanup(func() {
		formulaSymlink = orig
	})

	dir := t.TempDir()
	layerA := filepath.Join(dir, "layer-a")
	layerB := filepath.Join(dir, "layer-b")
	writeFormulaFile(t, layerA, "mol-a.toml", "a1")
	writeFormulaFile(t, layerB, "mol-b.toml", "b1")
	target := filepath.Join(dir, "rig")

	var mu sync.Mutex
	aArrived := make(chan struct{})
	bArrived := make(chan struct{})
	aDone := make(chan struct{})
	aClosed := false
	bClosed := false
	formulaSymlink = func(oldname, newname string) error {
		switch filepath.Base(newname) {
		case "mol-a.toml":
			mu.Lock()
			if !aClosed {
				close(aArrived)
				aClosed = true
			}
			mu.Unlock()
			select {
			case <-bArrived:
			case <-time.After(500 * time.Millisecond):
			}
		case "mol-b.toml":
			mu.Lock()
			if !bClosed {
				close(bArrived)
				bClosed = true
			}
			mu.Unlock()
			select {
			case <-aDone:
			case <-time.After(5 * time.Second):
			}
		}
		return &os.LinkError{Op: "symlink", Old: oldname, New: newname, Err: windows.ERROR_PRIVILEGE_NOT_HELD}
	}

	errs := make(chan error, 2)
	go func() {
		err := ResolveFormulas(target, []string{layerA})
		close(aDone)
		errs <- err
	}()
	select {
	case <-aArrived:
	case <-time.After(5 * time.Second):
		t.Fatal("first resolver did not reach formula publication")
	}
	go func() {
		errs <- ResolveFormulas(target, []string{layerB})
	}()
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent ResolveFormulas: %v", err)
		}
	}

	writeFormulaFile(t, layerA, "mol-a.toml", "a2")
	writeFormulaFile(t, layerB, "mol-b.toml", "b2")
	if err := ResolveFormulas(target, []string{layerA, layerB}); err != nil {
		t.Fatalf("ResolveFormulas after source update: %v", err)
	}

	formulaDir := filepath.Join(target, ".beads", "formulas")
	assertManagedFormulaCopy(t, filepath.Join(formulaDir, "mol-a.toml"), []byte("a2"))
	assertManagedFormulaCopy(t, filepath.Join(formulaDir, "mol-b.toml"), []byte("b2"))
}

func TestResolveFormulasWindowsPendingUpdatePreservesUserEdit(t *testing.T) {
	forceFormulaSymlinkPrivilegeError(t)

	dir := t.TempDir()
	layer := filepath.Join(dir, "layer")
	writeFormulaFile(t, layer, "mol-pending-user.toml", "new")
	target := filepath.Join(dir, "rig")
	formulaDir := filepath.Join(target, ".beads", "formulas")
	if err := os.MkdirAll(formulaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(formulaDir, "mol-pending-user.toml")
	if err := os.WriteFile(path, []byte("user edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeManagedFormulaManifestForTest(t, formulaDir, "mol-pending-user.toml", filepath.Join(layer, "mol-pending-user.toml"), "old", "new")

	if err := ResolveFormulas(target, []string{layer}); err != nil {
		t.Fatalf("ResolveFormulas: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "user edit" {
		t.Fatalf("pending update treated user edit as managed: %q", got)
	}
	if managedFormulaManifestHasEntryForTest(t, formulaDir, "mol-pending-user.toml") {
		t.Fatal("user edit remains recorded as managed")
	}
}

func TestResolveFormulasWindowsCorruptManifestNamesFileAndRemedy(t *testing.T) {
	dir := t.TempDir()
	layer := filepath.Join(dir, "layer")
	writeFormulaFile(t, layer, "mol-bad.toml", "generated")
	target := filepath.Join(dir, "rig")
	formulaDir := filepath.Join(target, ".beads", "formulas")
	if err := os.MkdirAll(formulaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifestPath := formulaManifestPath(formulaDir)
	if err := os.WriteFile(manifestPath, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := ResolveFormulas(target, []string{layer})
	if err == nil {
		t.Fatal("ResolveFormulas succeeded with corrupt manifest")
	}
	message := err.Error()
	if !strings.Contains(message, manifestPath) {
		t.Fatalf("error %q does not name manifest %q", message, manifestPath)
	}
	if !strings.Contains(strings.ToLower(message), "remove") {
		t.Fatalf("error %q does not include recovery remedy", message)
	}
}

func forceFormulaSymlinkPrivilegeError(t *testing.T) *int {
	t.Helper()
	attempts := 0
	orig := formulaSymlink
	formulaSymlink = func(oldname, newname string) error {
		attempts++
		return &os.LinkError{Op: "symlink", Old: oldname, New: newname, Err: windows.ERROR_PRIVILEGE_NOT_HELD}
	}
	t.Cleanup(func() {
		formulaSymlink = orig
	})
	return &attempts
}

func writeManagedFormulaManifestForTest(t *testing.T, formulaDir, name, source, recorded, pending string) {
	t.Helper()
	entry := map[string]string{
		"source": source,
	}
	if recorded != "" {
		entry["sha256"] = sha256Bytes([]byte(recorded))
	}
	if pending != "" {
		entry["pending_sha256"] = sha256Bytes([]byte(pending))
	}
	manifest := map[string]any{
		"version": 1,
		"entries": map[string]map[string]string{
			name: entry,
		},
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(formulaManifestPath(formulaDir), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readManagedFormulaManifestEntryForTest(t *testing.T, formulaDir, name string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(formulaManifestPath(formulaDir))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Entries map[string]map[string]string `json:"entries"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	entry, ok := manifest.Entries[name]
	if !ok {
		t.Fatalf("manifest missing entry %q: %#v", name, manifest.Entries)
	}
	return entry
}

func managedFormulaManifestHasEntryForTest(t *testing.T, formulaDir, name string) bool {
	t.Helper()
	data, err := os.ReadFile(formulaManifestPath(formulaDir))
	if err != nil {
		if os.IsNotExist(err) {
			return false
		}
		t.Fatal(err)
	}
	var manifest struct {
		Entries map[string]map[string]string `json:"entries"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	_, ok := manifest.Entries[name]
	return ok
}

func assertManagedFormulaCopy(t *testing.T, path string, want []byte) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat(%s): %v", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("%s is a symlink, want managed copy", path)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("ReadFile(%s) = %q, want %q", path, got, want)
	}
}

func snapshotFormulaDir(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := make(map[string][]byte, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", path, err)
		}
		snapshot[entry.Name()] = data
	}
	return snapshot
}

func equalSnapshots(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for name, av := range a {
		bv, ok := b[name]
		if !ok || !bytes.Equal(av, bv) {
			return false
		}
	}
	return true
}
