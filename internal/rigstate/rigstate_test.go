package rigstate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
)

// TestLoad_MissingFileReturnsEmpty verifies that Load on a fresh city
// (where rig-state.json does not yet exist) returns an empty state with
// an initialized Rigs map rather than an error.
func TestLoad_MissingFileReturnsEmpty(t *testing.T) {
	cityDir := t.TempDir()

	st, err := Load(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("Load on missing file should not error: %v", err)
	}
	if st.Rigs == nil {
		t.Fatal("Load should initialize Rigs map even when file is missing")
	}
	if len(st.Rigs) != 0 {
		t.Fatalf("Load on missing file should return empty Rigs, got %d entries", len(st.Rigs))
	}
}

// TestLoad_NilRigsMapPopulated guarantees that a persisted JSON object
// with a null/missing "rigs" field still produces an initialized map so
// callers don't have to nil-guard.
func TestLoad_NilRigsMapPopulated(t *testing.T) {
	cityDir := t.TempDir()
	p := citylayout.RigStateFile(cityDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(`{"updated_at":"2025-01-01T00:00:00Z"}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	st, err := Load(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.Rigs == nil {
		t.Fatal("Load should initialize Rigs map when JSON omits it")
	}
}

// TestLoad_InvalidJSONReturnsError makes sure malformed JSON surfaces
// as an error rather than silently producing a zero-value state.
func TestLoad_InvalidJSONReturnsError(t *testing.T) {
	cityDir := t.TempDir()
	p := citylayout.RigStateFile(cityDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte("not json {{{"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := Load(fsys.OSFS{}, cityDir); err == nil {
		t.Fatal("Load should return an error for invalid JSON")
	}
}

// TestSaveAndLoadRoundTrip confirms that what Save writes is what Load
// returns, including the suspended flag and a freshly-stamped UpdatedAt.
func TestSaveAndLoadRoundTrip(t *testing.T) {
	cityDir := t.TempDir()
	st := SuspensionState{
		Rigs: map[string]RigOverride{
			"foo": {Suspended: true},
		},
	}
	before := time.Now().UTC()

	if err := Save(fsys.OSFS{}, cityDir, st); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.Rigs["foo"].Suspended {
		t.Error("round-tripped state should mark foo suspended")
	}
	if got.UpdatedAt.Before(before.Add(-time.Second)) {
		t.Errorf("Save should stamp UpdatedAt to current time, got %v (before %v)", got.UpdatedAt, before)
	}
}

// TestSave_CreatesRuntimeDirectory verifies Save provisions the
// .gc/runtime/ directory rather than failing when it does not exist.
func TestSave_CreatesRuntimeDirectory(t *testing.T) {
	cityDir := t.TempDir()
	if err := Save(fsys.OSFS{}, cityDir, SuspensionState{Rigs: map[string]RigOverride{}}); err != nil {
		t.Fatalf("Save into fresh city: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cityDir, ".gc", "runtime")); err != nil {
		t.Errorf("Save should create .gc/runtime/, got: %v", err)
	}
}

// TestSave_PersistsAtomicallyWithTrailingNewline confirms the file is
// human-friendly: indented JSON ending in a newline so editors don't
// flag a missing-EOL diff.
func TestSave_PersistsAtomicallyWithTrailingNewline(t *testing.T) {
	cityDir := t.TempDir()
	if err := Save(fsys.OSFS{}, cityDir, SuspensionState{Rigs: map[string]RigOverride{"a": {Suspended: true}}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(citylayout.RigStateFile(cityDir))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Error("Save should write a trailing newline")
	}
	// Indent makes the file diff-friendly for humans browsing .gc/runtime.
	if !strings.Contains(string(data), "\n  ") {
		t.Error("Save should indent JSON for readability")
	}
}

// TestIsSuspended_AbsentReturnsFalse covers the lookup path where the
// rig has never been recorded — the zero value must report not-suspended.
func TestIsSuspended_AbsentReturnsFalse(t *testing.T) {
	st := SuspensionState{Rigs: map[string]RigOverride{}}
	if IsSuspended(st, "no-such-rig") {
		t.Error("absent rig must report not suspended")
	}
}

// TestIsSuspended_PresentButNotSuspended covers the corner case where a
// rig has an entry whose Suspended field is false (e.g. before deletion).
func TestIsSuspended_PresentButNotSuspended(t *testing.T) {
	st := SuspensionState{Rigs: map[string]RigOverride{"foo": {}}}
	if IsSuspended(st, "foo") {
		t.Error("rig present but not suspended must report not suspended")
	}
}

// TestSetSuspended_SetsAndRemoves drives the lifecycle: setting
// Suspended=true creates an entry; clearing it deletes the entry so the
// JSON file stays minimal.
func TestSetSuspended_SetsAndRemoves(t *testing.T) {
	st := SuspensionState{Rigs: map[string]RigOverride{}}

	SetSuspended(&st, "foo", true)
	if !IsSuspended(st, "foo") {
		t.Fatal("expected foo suspended after SetSuspended(true)")
	}

	SetSuspended(&st, "foo", false)
	if _, ok := st.Rigs["foo"]; ok {
		t.Error("clearing the only override should remove the rig entry entirely")
	}
}

// TestSetRigSuspended_NoOpWhenAlreadyDesired guards a subtle property:
// if state on disk already matches what we want, Save is skipped so we
// don't churn UpdatedAt or rewrite the file unnecessarily.
func TestSetRigSuspended_NoOpWhenAlreadyDesired(t *testing.T) {
	cityDir := t.TempDir()
	if err := SetRigSuspended(fsys.OSFS{}, cityDir, "foo", true); err != nil {
		t.Fatalf("first call: %v", err)
	}
	first, err := os.Stat(citylayout.RigStateFile(cityDir))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	// Sleep just enough that a real Save would yield a newer mtime.
	time.Sleep(20 * time.Millisecond)
	if err := SetRigSuspended(fsys.OSFS{}, cityDir, "foo", true); err != nil {
		t.Fatalf("second call: %v", err)
	}
	second, err := os.Stat(citylayout.RigStateFile(cityDir))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !first.ModTime().Equal(second.ModTime()) {
		t.Errorf("no-op SetRigSuspended should not rewrite the file (mtime changed: %v -> %v)",
			first.ModTime(), second.ModTime())
	}
}

// TestSetRigSuspended_TogglesSuspension exercises the suspend → resume
// path through the convenience function and confirms the entry is
// removed once the rig is no longer suspended.
func TestSetRigSuspended_TogglesSuspension(t *testing.T) {
	cityDir := t.TempDir()

	if err := SetRigSuspended(fsys.OSFS{}, cityDir, "foo", true); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	st, err := Load(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("load after suspend: %v", err)
	}
	if !IsSuspended(st, "foo") {
		t.Fatal("foo should be suspended after SetRigSuspended(true)")
	}

	if err := SetRigSuspended(fsys.OSFS{}, cityDir, "foo", false); err != nil {
		t.Fatalf("resume: %v", err)
	}
	st, err = Load(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("load after resume: %v", err)
	}
	if IsSuspended(st, "foo") {
		t.Error("foo should not be suspended after SetRigSuspended(false)")
	}
	if _, ok := st.Rigs["foo"]; ok {
		t.Error("foo entry should be removed after resume")
	}
}

// TestSuspendedNames returns only the suspended rigs and ignores entries
// that exist but have Suspended=false (a future override might keep the
// entry around for other reasons).
func TestSuspendedNames(t *testing.T) {
	st := SuspensionState{
		Rigs: map[string]RigOverride{
			"alpha": {Suspended: true},
			"beta":  {Suspended: false},
			"gamma": {Suspended: true},
		},
	}
	names := SuspendedNames(st)
	if len(names) != 2 {
		t.Fatalf("expected 2 suspended names, got %d: %v", len(names), names)
	}
	if !names["alpha"] || !names["gamma"] {
		t.Errorf("expected alpha and gamma suspended, got %v", names)
	}
	if names["beta"] {
		t.Error("beta should not be in suspended names (Suspended=false)")
	}
}

// TestLoad_PropagatesNonNotExistError makes sure ReadFile errors other
// than os.ErrNotExist are not swallowed. We exercise this by placing a
// directory at the rig-state.json path so ReadFile fails with EISDIR.
func TestLoad_PropagatesNonNotExistError(t *testing.T) {
	cityDir := t.TempDir()
	p := citylayout.RigStateFile(cityDir)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir at rig-state.json path: %v", err)
	}
	_, err := Load(fsys.OSFS{}, cityDir)
	if err == nil {
		t.Fatal("Load should propagate non-NotExist read errors")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Errorf("Load should not mask error as NotExist, got: %v", err)
	}
}

// TestSave_JSONStructure pins the on-disk schema so future renames don't
// silently break consumers (or downstream tooling) reading the file.
func TestSave_JSONStructure(t *testing.T) {
	cityDir := t.TempDir()
	if err := Save(fsys.OSFS{}, cityDir, SuspensionState{Rigs: map[string]RigOverride{"foo": {Suspended: true}}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(citylayout.RigStateFile(cityDir))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	rigs, ok := raw["rigs"].(map[string]any)
	if !ok {
		t.Fatalf("expected top-level rigs map, got: %v", raw)
	}
	foo, ok := rigs["foo"].(map[string]any)
	if !ok {
		t.Fatalf("expected foo entry, got: %v", rigs)
	}
	if foo["suspended"] != true {
		t.Errorf("expected foo.suspended=true, got %v", foo["suspended"])
	}
	if _, ok := raw["updated_at"]; !ok {
		t.Error("expected updated_at field in JSON output")
	}
}
