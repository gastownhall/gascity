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

func boolPtr(b bool) *bool { return &b }

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
			"foo": {Suspended: boolPtr(true)},
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
	if got.Rigs["foo"].Suspended == nil || !*got.Rigs["foo"].Suspended {
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
	if err := Save(fsys.OSFS{}, cityDir, SuspensionState{Rigs: map[string]RigOverride{"a": {Suspended: boolPtr(true)}}}); err != nil {
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

// TestIsSuspended_ExplicitResumeReturnsFalse confirms that an explicit
// resume (&false) is reported as not-suspended by IsSuspended even
// though an entry exists.
func TestIsSuspended_ExplicitResumeReturnsFalse(t *testing.T) {
	st := SuspensionState{Rigs: map[string]RigOverride{"foo": {Suspended: boolPtr(false)}}}
	if IsSuspended(st, "foo") {
		t.Error("explicit resume (&false) must report not suspended via IsSuspended")
	}
}

// TestExplicitSuspended_TriState exercises the three states (nil,
// &true, &false) that the runtime override can be in.
func TestExplicitSuspended_TriState(t *testing.T) {
	st := SuspensionState{
		Rigs: map[string]RigOverride{
			"suspended": {Suspended: boolPtr(true)},
			"resumed":   {Suspended: boolPtr(false)},
		},
	}
	if v, ok := ExplicitSuspended(st, "suspended"); !ok || !v {
		t.Errorf("explicit suspended: got (%v, %v), want (true, true)", v, ok)
	}
	if v, ok := ExplicitSuspended(st, "resumed"); !ok || v {
		t.Errorf("explicit resumed: got (%v, %v), want (false, true)", v, ok)
	}
	if _, ok := ExplicitSuspended(st, "missing"); ok {
		t.Error("absent rig should report no explicit preference")
	}
}

// TestEffectiveSuspended_RuntimeWinsOverConfig pins the merge rule:
// when the runtime override is non-nil it overrides the rig's
// SuspendedOnStart, regardless of direction.
func TestEffectiveSuspended_RuntimeWinsOverConfig(t *testing.T) {
	st := SuspensionState{
		Rigs: map[string]RigOverride{
			"resumed-but-config-says-suspended": {Suspended: boolPtr(false)},
			"suspended-but-config-says-resumed": {Suspended: boolPtr(true)},
		},
	}
	if EffectiveSuspended(st, "resumed-but-config-says-suspended", true) {
		t.Error("explicit resume must defeat suspended_on_start = true")
	}
	if !EffectiveSuspended(st, "suspended-but-config-says-resumed", false) {
		t.Error("explicit suspend must defeat suspended_on_start = false")
	}
}

// TestEffectiveSuspended_DefaultsToSuspendedOnStart confirms that
// without a runtime override the rig's SuspendedOnStart is the answer.
func TestEffectiveSuspended_DefaultsToSuspendedOnStart(t *testing.T) {
	st := SuspensionState{Rigs: map[string]RigOverride{}}
	if !EffectiveSuspended(st, "missing", true) {
		t.Error("no runtime override → SuspendedOnStart=true must yield suspended")
	}
	if EffectiveSuspended(st, "missing", false) {
		t.Error("no runtime override → SuspendedOnStart=false must yield not suspended")
	}
}

// TestSetSuspended_SetsAndRemoves drives the lifecycle: setting
// Suspended=&true creates an entry; clearing it (nil) deletes the entry
// so the JSON file stays minimal.
func TestSetSuspended_SetsAndRemoves(t *testing.T) {
	st := SuspensionState{Rigs: map[string]RigOverride{}}

	SetSuspended(&st, "foo", boolPtr(true))
	if !IsSuspended(st, "foo") {
		t.Fatal("expected foo suspended after SetSuspended(&true)")
	}

	SetSuspended(&st, "foo", nil)
	if _, ok := st.Rigs["foo"]; ok {
		t.Error("clearing to nil should remove the rig entry entirely")
	}
}

// TestSetSuspended_ExplicitResumeRetainsEntry confirms that an explicit
// resume (&false) keeps the entry around so a later EffectiveSuspended
// call sees the user's override instead of falling back to SuspendedOnStart.
func TestSetSuspended_ExplicitResumeRetainsEntry(t *testing.T) {
	st := SuspensionState{Rigs: map[string]RigOverride{}}

	SetSuspended(&st, "foo", boolPtr(false))
	if _, ok := st.Rigs["foo"]; !ok {
		t.Fatal("explicit resume must keep the rig entry so it overrides SuspendedOnStart")
	}
	if v, ok := ExplicitSuspended(st, "foo"); !ok || v {
		t.Errorf("explicit resume: got (%v, %v), want (false, true)", v, ok)
	}
}

// TestSetRigSuspended_NoOpWhenAlreadyDesired guards a subtle property:
// if state on disk already matches what we want, Save is skipped so we
// don't churn UpdatedAt or rewrite the file unnecessarily.
func TestSetRigSuspended_NoOpWhenAlreadyDesired(t *testing.T) {
	cityDir := t.TempDir()
	if err := SetRigSuspended(fsys.OSFS{}, cityDir, "foo", boolPtr(true)); err != nil {
		t.Fatalf("first call: %v", err)
	}
	first, err := os.Stat(citylayout.RigStateFile(cityDir))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	// Sleep just enough that a real Save would yield a newer mtime.
	time.Sleep(20 * time.Millisecond)
	if err := SetRigSuspended(fsys.OSFS{}, cityDir, "foo", boolPtr(true)); err != nil {
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

// TestSetRigSuspended_TogglesSuspension exercises suspend → resume →
// clear through the convenience function and confirms the entry is
// removed only when the preference is cleared to nil.
func TestSetRigSuspended_TogglesSuspension(t *testing.T) {
	cityDir := t.TempDir()

	if err := SetRigSuspended(fsys.OSFS{}, cityDir, "foo", boolPtr(true)); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	st, err := Load(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("load after suspend: %v", err)
	}
	if !IsSuspended(st, "foo") {
		t.Fatal("foo should be suspended after SetRigSuspended(&true)")
	}

	// Explicit resume keeps the entry so it sticks across restarts.
	if err := SetRigSuspended(fsys.OSFS{}, cityDir, "foo", boolPtr(false)); err != nil {
		t.Fatalf("resume: %v", err)
	}
	st, err = Load(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("load after resume: %v", err)
	}
	if IsSuspended(st, "foo") {
		t.Error("foo should not be suspended after SetRigSuspended(&false)")
	}
	if _, ok := st.Rigs["foo"]; !ok {
		t.Error("explicit resume must retain the rig entry so SuspendedOnStart can't reassert")
	}

	// Clearing to nil drops the entry entirely.
	if err := SetRigSuspended(fsys.OSFS{}, cityDir, "foo", nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	st, err = Load(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("load after clear: %v", err)
	}
	if _, ok := st.Rigs["foo"]; ok {
		t.Error("clearing to nil should remove the rig entry")
	}
}

// TestSuspendedNames returns only the explicitly-suspended rigs (&true)
// and ignores explicit-resume (&false) and absent entries.
func TestSuspendedNames(t *testing.T) {
	st := SuspensionState{
		Rigs: map[string]RigOverride{
			"alpha": {Suspended: boolPtr(true)},
			"beta":  {Suspended: boolPtr(false)},
			"gamma": {Suspended: boolPtr(true)},
			"delta": {},
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
		t.Error("beta should not be in suspended names (explicit resume)")
	}
	if names["delta"] {
		t.Error("delta should not be in suspended names (no preference)")
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
	if err := Save(fsys.OSFS{}, cityDir, SuspensionState{Rigs: map[string]RigOverride{"foo": {Suspended: boolPtr(true)}}}); err != nil {
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
