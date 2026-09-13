package suspensionstate

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

// TestLoad_MissingFileReturnsEmpty verifies that Load on a fresh
// city returns a zero-value State instead of an error.
func TestLoad_MissingFileReturnsEmpty(t *testing.T) {
	cityDir := t.TempDir()

	st, err := Load(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("Load on missing file should not error: %v", err)
	}
	if st.City.Suspended != nil {
		t.Errorf("zero state should have nil City.Suspended, got %v", *st.City.Suspended)
	}
	if len(st.Rigs) != 0 {
		t.Errorf("zero state should have empty Rigs, got %d entries", len(st.Rigs))
	}
}

// TestLoad_InvalidJSONReturnsError makes sure malformed JSON surfaces
// as an error rather than silently producing a zero-value state.
func TestLoad_InvalidJSONReturnsError(t *testing.T) {
	cityDir := t.TempDir()
	p := citylayout.SuspensionStateFile(cityDir)
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

// TestLoad_PropagatesNonNotExistError makes sure ReadFile errors
// other than os.ErrNotExist are not swallowed.
func TestLoad_PropagatesNonNotExistError(t *testing.T) {
	cityDir := t.TempDir()
	p := citylayout.SuspensionStateFile(cityDir)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir at suspension-state.json path: %v", err)
	}
	_, err := Load(fsys.OSFS{}, cityDir)
	if err == nil {
		t.Fatal("Load should propagate non-NotExist read errors")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Errorf("Load should not mask error as NotExist, got: %v", err)
	}
}

// TestSaveAndLoadRoundTrip confirms what Save writes is what Load
// returns, including both city and rig overrides plus the stamped
// UpdatedAt.
func TestSaveAndLoadRoundTrip(t *testing.T) {
	cityDir := t.TempDir()
	before := time.Now().UTC()
	st := State{
		City: Override{Suspended: boolPtr(true)},
		Rigs: map[string]Override{
			"alpha": {Suspended: boolPtr(true)},
			"beta":  {Suspended: boolPtr(false)},
		},
	}
	if err := Save(fsys.OSFS{}, cityDir, st); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.City.Suspended == nil || !*got.City.Suspended {
		t.Error("round-tripped state should mark city suspended")
	}
	if got.Rigs["alpha"].Suspended == nil || !*got.Rigs["alpha"].Suspended {
		t.Error("alpha should round-trip as explicit suspend")
	}
	if got.Rigs["beta"].Suspended == nil || *got.Rigs["beta"].Suspended {
		t.Error("beta should round-trip as explicit resume (&false)")
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
	data, err := os.ReadFile(citylayout.SuspensionStateFile(cityDir))
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

// TestSave_JSONStructure pins the on-disk schema so future renames
// don't silently break consumers (or downstream tooling) reading the
// file. The "agents" slot is reserved for the follow-up tracked in
// issue #2407 and must remain in the schema.
func TestSave_JSONStructure(t *testing.T) {
	cityDir := t.TempDir()
	st := State{
		City: Override{Suspended: boolPtr(true)},
		Rigs: map[string]Override{"foo": {Suspended: boolPtr(true)}},
	}
	if err := Save(fsys.OSFS{}, cityDir, st); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(citylayout.SuspensionStateFile(cityDir))
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
	rigs, ok := raw["rigs"].(map[string]any)
	if !ok {
		t.Fatalf("expected top-level rigs object, got: %v", raw)
	}
	if foo, ok := rigs["foo"].(map[string]any); !ok || foo["suspended"] != true {
		t.Errorf("expected rigs.foo.suspended=true, got %v", rigs["foo"])
	}
	if _, ok := raw["updated_at"]; !ok {
		t.Error("expected updated_at field in JSON output")
	}
}

// TestLoad_ForwardCompatibleAgentsSlot guards the agent-slot reservation:
// a JSON document carrying a populated `agents` map must round-trip
// through Load without losing the field. The follow-up tracked in
// issue #2407 will start consuming it.
func TestLoad_ForwardCompatibleAgentsSlot(t *testing.T) {
	cityDir := t.TempDir()
	p := citylayout.SuspensionStateFile(cityDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := []byte(`{
  "city":   {},
  "rigs":   {},
  "agents": {"rig/worker": {"suspended": true}},
  "updated_at": "2026-05-20T00:00:00Z"
}`)
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	st, err := Load(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if a, ok := st.Agents["rig/worker"]; !ok || a.Suspended == nil || !*a.Suspended {
		t.Errorf("agents slot should round-trip; got Agents=%v", st.Agents)
	}
}

// TestIsCitySuspended_TriState covers all three explicit states.
func TestIsCitySuspended_TriState(t *testing.T) {
	if IsCitySuspended(State{}) {
		t.Error("nil Suspended should report not suspended")
	}
	if IsCitySuspended(State{City: Override{Suspended: boolPtr(false)}}) {
		t.Error("explicit resume (&false) should report not suspended via IsCitySuspended")
	}
	if !IsCitySuspended(State{City: Override{Suspended: boolPtr(true)}}) {
		t.Error("explicit suspend (&true) should report suspended")
	}
}

// TestExplicitCity_TriState exercises the three states.
func TestExplicitCity_TriState(t *testing.T) {
	if _, ok := ExplicitCity(State{}); ok {
		t.Error("nil Suspended should report no explicit preference")
	}
	if v, ok := ExplicitCity(State{City: Override{Suspended: boolPtr(false)}}); !ok || v {
		t.Errorf("explicit resume: got (%v, %v), want (false, true)", v, ok)
	}
	if v, ok := ExplicitCity(State{City: Override{Suspended: boolPtr(true)}}); !ok || !v {
		t.Errorf("explicit suspend: got (%v, %v), want (true, true)", v, ok)
	}
}

// TestEffectiveCitySuspended_RuntimeWinsOverConfig pins the merge rule.
func TestEffectiveCitySuspended_RuntimeWinsOverConfig(t *testing.T) {
	if EffectiveCitySuspended(State{City: Override{Suspended: boolPtr(false)}}, true) {
		t.Error("explicit resume must defeat workspace.suspended_on_start=true")
	}
	if !EffectiveCitySuspended(State{City: Override{Suspended: boolPtr(true)}}, false) {
		t.Error("explicit suspend must defeat workspace.suspended_on_start=false")
	}
}

// TestEffectiveCitySuspended_DefaultsToSuspendedOnStart confirms the
// no-runtime-override fallback to the workspace's SuspendedOnStart.
func TestEffectiveCitySuspended_DefaultsToSuspendedOnStart(t *testing.T) {
	if !EffectiveCitySuspended(State{}, true) {
		t.Error("no runtime override + SuspendedOnStart=true must yield suspended")
	}
	if EffectiveCitySuspended(State{}, false) {
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
	first, err := os.Stat(citylayout.SuspensionStateFile(cityDir))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := SetCitySuspended(fsys.OSFS{}, cityDir, boolPtr(true)); err != nil {
		t.Fatalf("second call: %v", err)
	}
	second, err := os.Stat(citylayout.SuspensionStateFile(cityDir))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !first.ModTime().Equal(second.ModTime()) {
		t.Errorf("no-op SetCitySuspended should not rewrite the file (mtime changed: %v -> %v)",
			first.ModTime(), second.ModTime())
	}
}

// TestSetCitySuspended_FullLifecycle exercises suspend → explicit
// resume → clear.
func TestSetCitySuspended_FullLifecycle(t *testing.T) {
	cityDir := t.TempDir()

	if err := SetCitySuspended(fsys.OSFS{}, cityDir, boolPtr(true)); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	st, err := Load(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !IsCitySuspended(st) {
		t.Fatal("city should be suspended after SetCitySuspended(&true)")
	}

	if err := SetCitySuspended(fsys.OSFS{}, cityDir, boolPtr(false)); err != nil {
		t.Fatalf("resume: %v", err)
	}
	st, err = Load(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if IsCitySuspended(st) {
		t.Error("city should not be suspended after SetCitySuspended(&false)")
	}
	if v, ok := ExplicitCity(st); !ok || v {
		t.Errorf("explicit resume must persist; got (%v, %v)", v, ok)
	}

	if err := SetCitySuspended(fsys.OSFS{}, cityDir, nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	st, err = Load(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := ExplicitCity(st); ok {
		t.Error("clearing to nil must drop the explicit preference")
	}
}

// TestIsRigSuspended exercises the explicit-only contract.
func TestIsRigSuspended(t *testing.T) {
	st := State{
		Rigs: map[string]Override{
			"yes":     {Suspended: boolPtr(true)},
			"resumed": {Suspended: boolPtr(false)},
		},
	}
	if !IsRigSuspended(st, "yes") {
		t.Error("explicit suspend should report suspended")
	}
	if IsRigSuspended(st, "resumed") {
		t.Error("explicit resume must not be reported as suspended via IsRigSuspended")
	}
	if IsRigSuspended(st, "absent") {
		t.Error("absent rig must not be reported as suspended")
	}
}

// TestExplicitRig_TriState exercises the three states.
func TestExplicitRig_TriState(t *testing.T) {
	st := State{
		Rigs: map[string]Override{
			"yes":     {Suspended: boolPtr(true)},
			"resumed": {Suspended: boolPtr(false)},
		},
	}
	if v, ok := ExplicitRig(st, "yes"); !ok || !v {
		t.Errorf("yes: got (%v, %v), want (true, true)", v, ok)
	}
	if v, ok := ExplicitRig(st, "resumed"); !ok || v {
		t.Errorf("resumed: got (%v, %v), want (false, true)", v, ok)
	}
	if _, ok := ExplicitRig(st, "absent"); ok {
		t.Error("absent rig should report no explicit preference")
	}
}

// TestEffectiveRigSuspended_RuntimeWinsOverConfig pins the merge rule.
func TestEffectiveRigSuspended_RuntimeWinsOverConfig(t *testing.T) {
	st := State{
		Rigs: map[string]Override{
			"resumed-but-default-suspended": {Suspended: boolPtr(false)},
			"suspended-but-default-resumed": {Suspended: boolPtr(true)},
		},
	}
	if EffectiveRigSuspended(st, "resumed-but-default-suspended", true) {
		t.Error("explicit resume must defeat suspended_on_start = true")
	}
	if !EffectiveRigSuspended(st, "suspended-but-default-resumed", false) {
		t.Error("explicit suspend must defeat suspended_on_start = false")
	}
}

// TestEffectiveRigSuspended_DefaultsToSuspendedOnStart confirms the
// no-runtime-override fallback.
func TestEffectiveRigSuspended_DefaultsToSuspendedOnStart(t *testing.T) {
	if !EffectiveRigSuspended(State{}, "missing", true) {
		t.Error("no runtime override + suspended_on_start=true must yield suspended")
	}
	if EffectiveRigSuspended(State{}, "missing", false) {
		t.Error("no runtime override + suspended_on_start=false must yield not suspended")
	}
}

// TestSetRig_SetsAndRemoves drives the lifecycle: &true creates an
// entry, nil removes it so the JSON stays minimal.
func TestSetRig_SetsAndRemoves(t *testing.T) {
	st := State{}

	SetRig(&st, "foo", boolPtr(true))
	if !IsRigSuspended(st, "foo") {
		t.Fatal("expected foo suspended after SetRig(&true)")
	}

	SetRig(&st, "foo", nil)
	if _, ok := st.Rigs["foo"]; ok {
		t.Error("clearing to nil should remove the rig entry entirely")
	}
}

// TestSetRig_ExplicitResumeRetainsEntry confirms that an explicit
// resume (&false) keeps the entry so a later EffectiveRigSuspended
// call sees the user's override instead of falling back to the
// authored default.
func TestSetRig_ExplicitResumeRetainsEntry(t *testing.T) {
	st := State{}

	SetRig(&st, "foo", boolPtr(false))
	if _, ok := st.Rigs["foo"]; !ok {
		t.Fatal("explicit resume must keep the rig entry so it overrides SuspendedOnStart")
	}
	if v, ok := ExplicitRig(st, "foo"); !ok || v {
		t.Errorf("explicit resume: got (%v, %v), want (false, true)", v, ok)
	}
}

// TestSetRigSuspended_NoOpWhenAlreadyDesired guards the no-rewrite
// optimization.
func TestSetRigSuspended_NoOpWhenAlreadyDesired(t *testing.T) {
	cityDir := t.TempDir()
	if err := SetRigSuspended(fsys.OSFS{}, cityDir, "foo", boolPtr(true)); err != nil {
		t.Fatalf("first call: %v", err)
	}
	first, err := os.Stat(citylayout.SuspensionStateFile(cityDir))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := SetRigSuspended(fsys.OSFS{}, cityDir, "foo", boolPtr(true)); err != nil {
		t.Fatalf("second call: %v", err)
	}
	second, err := os.Stat(citylayout.SuspensionStateFile(cityDir))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !first.ModTime().Equal(second.ModTime()) {
		t.Errorf("no-op SetRigSuspended should not rewrite the file (mtime changed: %v -> %v)",
			first.ModTime(), second.ModTime())
	}
}

// TestSetRigSuspended_FullLifecycle exercises suspend → explicit
// resume → clear via the convenience function.
func TestSetRigSuspended_FullLifecycle(t *testing.T) {
	cityDir := t.TempDir()

	if err := SetRigSuspended(fsys.OSFS{}, cityDir, "foo", boolPtr(true)); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	st, err := Load(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("load after suspend: %v", err)
	}
	if !IsRigSuspended(st, "foo") {
		t.Fatal("foo should be suspended after SetRigSuspended(&true)")
	}

	if err := SetRigSuspended(fsys.OSFS{}, cityDir, "foo", boolPtr(false)); err != nil {
		t.Fatalf("resume: %v", err)
	}
	st, err = Load(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("load after resume: %v", err)
	}
	if IsRigSuspended(st, "foo") {
		t.Error("foo should not be suspended after SetRigSuspended(&false)")
	}
	if _, ok := st.Rigs["foo"]; !ok {
		t.Error("explicit resume must retain the rig entry so SuspendedOnStart can't reassert")
	}

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

// TestSuspendedRigNames returns only the explicitly-suspended rigs
// (&true) and ignores explicit-resume (&false) and absent entries.
func TestSuspendedRigNames(t *testing.T) {
	st := State{
		Rigs: map[string]Override{
			"alpha": {Suspended: boolPtr(true)},
			"beta":  {Suspended: boolPtr(false)},
			"gamma": {Suspended: boolPtr(true)},
			"delta": {},
		},
	}
	names := SuspendedRigNames(st)
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
