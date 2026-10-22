package session

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

func startedEntry(store, bead string) WorkStartedEntry {
	return WorkStartedEntry{Store: store, Bead: bead, Gen: 7, At: time.Date(2026, 10, 17, 20, 0, 0, 0, time.UTC), Via: WorkStartedViaClaim}
}

// TestWorkStartedCodec pins the cell's wire form (§6.1) and its read rules:
// an absent cell is empty, and one the codec cannot read, bad JSON or a
// version it does not know, is an error, never an empty cell.
func TestWorkStartedCodec(t *testing.T) {
	w := WorkStarted{}.With(startedEntry("rig:gascity", "ga-12ab"))
	raw := EncodeWorkStarted(w)
	if want := `{"v":1,"claims":[{"store":"rig:gascity","bead":"ga-12ab","gen":7,"at":"2026-10-17T20:00:00Z","via":"claim"}]}`; raw != want {
		t.Fatalf("encoded %s, want %s", raw, want)
	}
	back, err := DecodeWorkStarted(raw)
	if err != nil || !back.Names("rig:gascity", "ga-12ab", 7) || len(back.Claims) != 1 {
		t.Fatalf("decode(%s) = %+v, %v; want the one entry back", raw, back, err)
	}
	if empty, err := DecodeWorkStarted(""); err != nil || len(empty.Claims) != 0 {
		t.Fatalf("absent cell = %+v, %v; want empty", empty, err)
	}
	for _, bad := range []string{"{", `{"v":2,"claims":[]}`, `{"claims":[]}`, `[]`} {
		if _, err := DecodeWorkStarted(bad); err == nil {
			t.Errorf("decode(%s) read it; want an error, which readers take as every claim started", bad)
		}
	}
}

// TestWorkStartedNames: an entry names its own (store, bead); one recorded
// with no store names its bead in every store, and a lookup with no store
// matches the bead in any; another store or bead does not match. The entries
// here are resident (the residence spelling), so a store is exact at every
// generation; work_cells_resident_test.go has the unmarked entries.
func TestWorkStartedNames(t *testing.T) {
	w := WorkStarted{}.With(residentEntry("rig:gascity", "ga-1", 7)).With(startedEntry("", "ga-2"))
	for _, c := range []struct {
		store, bead string
		want        bool
	}{
		{"rig:gascity", "ga-1", true},
		{"", "ga-1", true},
		{"city", "ga-1", false},
		{"city", "ga-2", true},
		{"rig:gascity", "ga-3", false},
		{"rig:gascity", "", false},
	} {
		if got := w.Names(c.store, c.bead, 7); got != c.want {
			t.Errorf("Names(%q, %q, 7) = %v, want %v", c.store, c.bead, got, c.want)
		}
	}
	if w = w.Without("rig:gascity", "ga-1", 7); w.Names("rig:gascity", "ga-1", 7) || !w.Names("city", "ga-2", 7) {
		t.Errorf("Without dropped the wrong entries: %+v", w)
	}
	// With matches as Names and Without do: recording a claim drops every
	// entry naming it, an unknown store's included, and keeps another store's.
	w = WorkStarted{}.With(startedEntry("", "ga-5")).With(residentEntry("city", "ga-6", 7)).With(residentEntry("rig:x", "ga-6", 7))
	w = w.With(residentEntry("city", "ga-5", 7))
	if len(w.Claims) != 3 || w.Claims[0] != residentEntry("city", "ga-5", 7) || !w.Names("city", "ga-6", 7) || !w.Names("rig:x", "ga-6", 7) {
		t.Errorf("With over a storeless entry: %+v; want it replaced, the two ga-6 stores kept", w.Claims)
	}
}

// TestWorkStartedCap (T12): at most 8 entries, newest first, the oldest
// dropped; recording a claim the cell holds moves it first, never twice.
func TestWorkStartedCap(t *testing.T) {
	var w WorkStarted
	for i := 1; i <= 10; i++ {
		w = w.With(startedEntry("city", fmt.Sprintf("b-%d", i)))
	}
	if len(w.Claims) != workStartedCap || w.Claims[0].Bead != "b-10" || w.Claims[7].Bead != "b-3" || w.Names("city", "b-2", 7) {
		t.Fatalf("after 10 records: %+v; want b-10..b-3, newest first", w.Claims)
	}
	w = w.With(startedEntry("city", "b-5"))
	if len(w.Claims) != workStartedCap || w.Claims[0].Bead != "b-5" || w.Claims[1].Bead != "b-10" || w.Claims[7].Bead != "b-3" {
		t.Fatalf("re-recording b-5: %+v; want it first, once, nothing else dropped", w.Claims)
	}
}

// TestDecodeWorkLadderRelease reads only the ladder's pending release, and
// reads none from a cell it cannot read.
func TestDecodeWorkLadderRelease(t *testing.T) {
	for _, c := range []struct {
		raw         string
		store, bead string
		ok          bool
	}{
		{"", "", "", false},
		{`{"v":1,"gen":7,"entries":[]}`, "", "", false},
		{`{"v":1,"release":{"store":"rig:gascity","bead":"ga-99zz","gen":7,"at":"x"}}`, "rig:gascity", "ga-99zz", true},
		{`{"v":1,"release":{"store":"rig:gascity","bead":""}}`, "", "", false},
		{`{"v":2,"release":{"store":"rig:gascity","bead":"ga-99zz"}}`, "", "", false},
		{"{", "", "", false},
	} {
		store, bead, ok := DecodeWorkLadderRelease(c.raw)
		if store != c.store || bead != c.bead || ok != c.ok {
			t.Errorf("DecodeWorkLadderRelease(%s) = %q %q %v, want %q %q %v", c.raw, store, bead, ok, c.store, c.bead, c.ok)
		}
	}
	release := `{"v":1,"release":{"store":"rig:gascity","bead":"ga-99zz"}}`
	if !WorkLadderReleases(release, "rig:gascity", "ga-99zz") || !WorkLadderReleases(release, "", "ga-99zz") || WorkLadderReleases(release, "city", "ga-99zz") {
		t.Error("WorkLadderReleases matched the wrong claims")
	}
}

// TestGuardSessionKeysRefusesBlindCASOnlyWrites (T11): a CASOnly key written
// through no conditional verb fails the guard, a fixture's seed and a blind
// set alike; the conditional verbs and UpdateRowFenced pass. Kills
// registering the work cells without CASOnly, and a guard that ignores it.
func TestGuardSessionKeysRefusesBlindCASOnlyWrites(t *testing.T) {
	for _, key := range []string{WorkStartedKey, WorkLadderKey} {
		if f, ok := LookupField(key); !ok || !f.CASOnly || f.Class != ClassWork || f.InPremise() || !f.NewKey {
			t.Fatalf("%s registered as %+v (found %v); want ClassWork, CASOnly, NewKey, outside the premise", key, f, ok)
		}
	}
	var failed []string
	GuardSessionKeys(func(msg string) { failed = append(failed, msg) })
	defer GuardSessionKeys(func(msg string) { panic(msg) })
	defer func() { keyViolations.Lock(); keyViolations.msgs = nil; keyViolations.Unlock() }() // its own, deliberate
	mem := beads.NewMemStore()
	store := stampedMemStore(t, gate.Auto, mem)
	raw := EncodeWorkStarted(WorkStarted{}.With(startedEntry("city", "b-1")))
	row, err := mem.Create(beads.Bead{Type: BeadType, Metadata: map[string]string{"state": "active"}})
	if err != nil {
		t.Fatal(err)
	}
	blind := map[string]func() error{
		"a seed": func() error {
			_, err := mem.Create(beads.Bead{Type: BeadType, Metadata: map[string]string{WorkStartedKey: raw}})
			return err
		},
		"SetMetadata":      func() error { return mem.SetMetadata(row.ID, WorkStartedKey, raw) },
		"SetMetadataBatch": func() error { return mem.SetMetadataBatch(row.ID, map[string]string{WorkLadderKey: "{}"}) },
		"Update": func() error {
			return mem.Update(row.ID, beads.UpdateOpts{Metadata: map[string]string{WorkStartedKey: raw}})
		},
	}
	for name, write := range blind {
		failed = nil
		if err := write(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(failed) != 1 || !strings.Contains(failed[0], "CASOnly but was written blind") {
			t.Errorf("%s: guard reported %q, want one blind-write failure", name, failed)
		}
	}
	failed = nil
	fresh, err := mem.Get(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.UpdateIfMatch(row.ID, fresh.Revision, beads.UpdateOpts{Metadata: map[string]string{WorkStartedKey: raw}}); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.CompareAndSetMetadataKey(row.ID, WorkLadderKey, "{}", `{"v":1}`); err != nil {
		t.Fatal(err)
	}
	wrote, err := NewStore(beads.SessionStore{Store: store}).UpdateRowFenced(row.ID, 1, func(Info, PersistedResponse) (RowPatch, bool) {
		return RowPatch{Metadata: MetadataPatch{WorkStartedKey: EncodeWorkStarted(WorkStarted{})}}, true
	})
	if err != nil || !wrote {
		t.Fatalf("UpdateRowFenced = %v, %v", wrote, err)
	}
	if len(failed) != 0 {
		t.Errorf("conditional writes failed the guard: %q", failed)
	}
}

// TestInfoProjectsWorkCells: Info carries both cells raw, so a fenced
// write's decide sees them.
func TestInfoProjectsWorkCells(t *testing.T) {
	info := infoFromPersistedBead(beads.Bead{ID: "s-1", Type: BeadType, Metadata: map[string]string{WorkStartedKey: "a", WorkLadderKey: "b"}})
	if info.WorkStartedRaw != "a" || info.WorkLadderRaw != "b" {
		t.Fatalf("projected %q %q, want the raw cells", info.WorkStartedRaw, info.WorkLadderRaw)
	}
}
