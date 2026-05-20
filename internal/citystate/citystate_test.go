package citystate

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
// returns a zero-value state instead of an error.
func TestLoad_MissingFileReturnsEmpty(t *testing.T) {
	cityDir := t.TempDir()

	st, err := Load(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("Load on missing file should not error: %v", err)
	}
	if st.City.Suspended != nil {
		t.Errorf("zero state should have nil Suspended, got %v", *st.City.Suspended)
	}
}

// TestLoad_InvalidJSONReturnsError makes sure malformed JSON surfaces
// as an error rather than silently producing a zero-value state.
func TestLoad_InvalidJSONReturnsError(t *testing.T) {
	cityDir := t.TempDir()
	p := citylayout.CityStateFile(cityDir)
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

// TestSaveAndLoadRoundTrip confirms what Save writes is what Load
// returns, including the suspended pointer and a freshly-stamped
// UpdatedAt.
func TestSaveAndLoadRoundTrip(t *testing.T) {
	cityDir := t.TempDir()
	before := time.Now().UTC()
	if err := Save(fsys.OSFS{}, cityDir, State{City: Override{Suspended: boolPtr(true)}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.City.Suspended == nil || !*got.City.Suspended {
		t.Error("round-tripped state should mark city suspended")
	}
	if got.UpdatedAt.Before(before.Add(-time.Second)) {
		t.Errorf("Save should stamp UpdatedAt, got %v (before %v)", got.UpdatedAt, before)
	}
}

// TestSave_CreatesRuntimeDirectory verifies Save provisions the
// .gc/runtime/ directory rather than failing when it does not exist.
func TestSave_CreatesRuntimeDirectory(t *testing.T) {
	cityDir := t.TempDir()
	if err := Save(fsys.OSFS{}, cityDir, State{}); err != nil {
		t.Fatalf("Save into fresh city: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cityDir, ".gc", "runtime")); err != nil {
		t.Errorf("Save should create .gc/runtime/, got: %v", err)
	}
}

// TestSave_PersistsAtomicallyWithTrailingNewline confirms the file is
// human-friendly: indented JSON ending in a newline.
func TestSave_PersistsAtomicallyWithTrailingNewline(t *testing.T) {
	cityDir := t.TempDir()
	if err := Save(fsys.OSFS{}, cityDir, State{City: Override{Suspended: boolPtr(true)}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(citylayout.CityStateFile(cityDir))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Error("Save should write a trailing newline")
	}
	if !strings.Contains(string(data), "\n  ") {
		t.Error("Save should indent JSON for readability")
	}
}

// TestIsSuspended_TriState covers all three explicit states.
func TestIsSuspended_TriState(t *testing.T) {
	if IsSuspended(State{}) {
		t.Error("nil Suspended should report not suspended")
	}
	if IsSuspended(State{City: Override{Suspended: boolPtr(false)}}) {
		t.Error("explicit resume (&false) should report not suspended via IsSuspended")
	}
	if !IsSuspended(State{City: Override{Suspended: boolPtr(true)}}) {
		t.Error("explicit suspend (&true) should report suspended")
	}
}

// TestExplicitSuspended_TriState exercises the three states.
func TestExplicitSuspended_TriState(t *testing.T) {
	if _, ok := ExplicitSuspended(State{}); ok {
		t.Error("nil Suspended should report no explicit preference")
	}
	if v, ok := ExplicitSuspended(State{City: Override{Suspended: boolPtr(false)}}); !ok || v {
		t.Errorf("explicit resume: got (%v, %v), want (false, true)", v, ok)
	}
	if v, ok := ExplicitSuspended(State{City: Override{Suspended: boolPtr(true)}}); !ok || !v {
		t.Errorf("explicit suspend: got (%v, %v), want (true, true)", v, ok)
	}
}

// TestEffectiveSuspended_RuntimeWinsOverConfig pins the merge rule.
func TestEffectiveSuspended_RuntimeWinsOverConfig(t *testing.T) {
	if EffectiveSuspended(State{City: Override{Suspended: boolPtr(false)}}, true) {
		t.Error("explicit resume must defeat workspace.suspended_on_start=true")
	}
	if !EffectiveSuspended(State{City: Override{Suspended: boolPtr(true)}}, false) {
		t.Error("explicit suspend must defeat workspace.suspended_on_start=false")
	}
}

// TestEffectiveSuspended_DefaultsToSuspendedOnStart confirms the
// no-runtime-override fallback to the workspace's SuspendedOnStart.
func TestEffectiveSuspended_DefaultsToSuspendedOnStart(t *testing.T) {
	if !EffectiveSuspended(State{}, true) {
		t.Error("no runtime override + SuspendedOnStart=true must yield suspended")
	}
	if EffectiveSuspended(State{}, false) {
		t.Error("no runtime override + SuspendedOnStart=false must yield not suspended")
	}
}

// TestSetCitySuspended_NoOpWhenAlreadyDesired guards the no-rewrite
// optimization: if state on disk already matches, skip Save.
func TestSetCitySuspended_NoOpWhenAlreadyDesired(t *testing.T) {
	cityDir := t.TempDir()
	if err := SetCitySuspended(fsys.OSFS{}, cityDir, boolPtr(true)); err != nil {
		t.Fatalf("first call: %v", err)
	}
	first, err := os.Stat(citylayout.CityStateFile(cityDir))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := SetCitySuspended(fsys.OSFS{}, cityDir, boolPtr(true)); err != nil {
		t.Fatalf("second call: %v", err)
	}
	second, err := os.Stat(citylayout.CityStateFile(cityDir))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !first.ModTime().Equal(second.ModTime()) {
		t.Errorf("no-op SetCitySuspended should not rewrite the file (mtime changed: %v -> %v)",
			first.ModTime(), second.ModTime())
	}
}

// TestSetCitySuspended_TogglesSuspension exercises the full lifecycle:
// suspend → explicit resume → clear.
func TestSetCitySuspended_TogglesSuspension(t *testing.T) {
	cityDir := t.TempDir()

	if err := SetCitySuspended(fsys.OSFS{}, cityDir, boolPtr(true)); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	st, err := Load(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !IsSuspended(st) {
		t.Fatal("city should be suspended after SetCitySuspended(&true)")
	}

	if err := SetCitySuspended(fsys.OSFS{}, cityDir, boolPtr(false)); err != nil {
		t.Fatalf("resume: %v", err)
	}
	st, err = Load(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if IsSuspended(st) {
		t.Error("city should not be suspended after SetCitySuspended(&false)")
	}
	if v, ok := ExplicitSuspended(st); !ok || v {
		t.Errorf("explicit resume must persist; got (%v, %v)", v, ok)
	}

	if err := SetCitySuspended(fsys.OSFS{}, cityDir, nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	st, err = Load(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := ExplicitSuspended(st); ok {
		t.Error("clearing to nil must drop the explicit preference")
	}
}

// TestLoad_PropagatesNonNotExistError makes sure ReadFile errors other
// than os.ErrNotExist are not swallowed.
func TestLoad_PropagatesNonNotExistError(t *testing.T) {
	cityDir := t.TempDir()
	p := citylayout.CityStateFile(cityDir)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_, err := Load(fsys.OSFS{}, cityDir)
	if err == nil {
		t.Fatal("Load should propagate non-NotExist read errors")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Errorf("Load should not mask error as NotExist, got: %v", err)
	}
}

// TestSave_JSONStructure pins the on-disk schema so future renames
// don't silently break consumers reading the file.
func TestSave_JSONStructure(t *testing.T) {
	cityDir := t.TempDir()
	if err := Save(fsys.OSFS{}, cityDir, State{City: Override{Suspended: boolPtr(true)}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(citylayout.CityStateFile(cityDir))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	city, ok := raw["city"].(map[string]any)
	if !ok {
		t.Fatalf("expected top-level city object, got: %v", raw)
	}
	if city["suspended"] != true {
		t.Errorf("expected city.suspended=true, got %v", city["suspended"])
	}
	if _, ok := raw["updated_at"]; !ok {
		t.Error("expected updated_at field in JSON output")
	}
}
