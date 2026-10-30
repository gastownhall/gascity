package session

import (
	"encoding/json"
	"testing"
	"time"
)

// The residence spelling of a claim (gastownhall/gascity ga-ylpbsy.41).
//
// The hook's work_started writer used to key a claim by the WORK LEG it ran
// in. Every controller reader keys it by the store that holds it, the census
// way. The two agree except for a bead resident in a relocated class binding
// claimed through a rig seat's leg: a graph step, which the old writer spelled
// rig:<rig> and every reader spells city (2026-10-20, maintainer-city on
// 8d6r4: the ladder read six working seats' started claims as unstarted).
// A resident entry carries the residence; an unmarked one is the old writer's,
// matched by bead alone at its own generation and nowhere else.

func residentEntry(store, bead string, gen int64) WorkStartedEntry {
	e := startedEntry(store, bead)
	e.Gen, e.Resident = gen, true
	return e
}

func legacyEntry(store, bead string, gen int64) WorkStartedEntry {
	e := startedEntry(store, bead)
	e.Gen = gen
	return e
}

// TestWorkStartedNamesAtGen: a resident entry names exactly its own store
// and bead, at any generation (the cell is claim-sticky). An unmarked entry
// does too, and, only at its own generation, names its bead under any store:
// the old writer's leg spelling of a binding-resident claim. An unmarked
// entry from another generation does not get the relaxation (the M1 shape of
// #8133: the claim and the current generation sit in different entries), and
// neither does a generation the reader could not read.
func TestWorkStartedNamesAtGen(t *testing.T) {
	cases := []struct {
		name  string
		entry WorkStartedEntry
		store string
		gen   int64
		want  bool
	}{
		{"resident, its own store", residentEntry("city", "gcg-1", 2), "city", 2, true},
		{"resident, its own store, another generation", residentEntry("city", "gcg-1", 1), "city", 2, true},
		{"resident, another store", residentEntry("rig:gascity", "gcg-1", 2), "city", 2, false},
		{"resident, storeless lookup", residentEntry("city", "gcg-1", 2), "", 2, true},
		{"legacy, its own store", legacyEntry("rig:gascity", "gcg-1", 1), "rig:gascity", 2, true},
		{"legacy leg spelling at the row's generation", legacyEntry("rig:gascity", "gcg-1", 2), "city", 2, true},
		{"legacy leg spelling at an older generation", legacyEntry("rig:gascity", "gcg-1", 1), "city", 2, false},
		{"legacy leg spelling at a newer generation", legacyEntry("rig:gascity", "gcg-1", 3), "city", 2, false},
		{"legacy, unreadable row generation", legacyEntry("rig:gascity", "gcg-1", 0), "city", 0, false},
		{"legacy storeless entry", legacyEntry("", "gcg-1", 1), "city", 2, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.entry.NamesAt(c.store, "gcg-1", c.gen); got != c.want {
				t.Fatalf("%+v NamesAt(%q, gcg-1, %d) = %v, want %v", c.entry, c.store, c.gen, got, c.want)
			}
			if got := (WorkStarted{Claims: []WorkStartedEntry{c.entry}}).Names(c.store, "gcg-1", c.gen); got != c.want {
				t.Fatalf("Names = %v, want %v", got, c.want)
			}
			if c.entry.NamesAt(c.store, "gcg-2", c.gen) {
				t.Fatal("named another bead")
			}
		})
	}
}

// TestWorkStartedWithReplacesTheLegSpelling: recording the residence spelling
// of a claim replaces the old writer's entry for it at that generation, and
// leaves an older generation's alone (the relaxation does not reach it, so
// neither does the dedup).
func TestWorkStartedWithReplacesTheLegSpelling(t *testing.T) {
	w := WorkStarted{Claims: []WorkStartedEntry{legacyEntry("rig:gascity", "gcg-1", 2), legacyEntry("rig:gascity", "gcg-2", 1)}}
	w = w.With(residentEntry("city", "gcg-1", 2)).With(residentEntry("city", "gcg-2", 2))
	if len(w.Claims) != 3 || w.Claims[0] != residentEntry("city", "gcg-2", 2) || w.Claims[1] != residentEntry("city", "gcg-1", 2) ||
		w.Claims[2] != legacyEntry("rig:gascity", "gcg-2", 1) {
		t.Fatalf("cell %+v: want gcg-1's gen-2 leg spelling replaced, gcg-2's gen-1 one kept behind its record", w.Claims)
	}
	if w = w.Without("city", "gcg-1", 2); w.Names("city", "gcg-1", 2) || len(w.Claims) != 2 {
		t.Fatalf("Without left %+v", w.Claims)
	}
	if w = (WorkStarted{Claims: []WorkStartedEntry{legacyEntry("rig:gascity", "gcg-3", 2)}}).Without("city", "gcg-3", 2); len(w.Claims) != 0 {
		t.Fatalf("Without at the entry's generation kept its leg spelling: %+v", w.Claims)
	}
	if w = (WorkStarted{Claims: []WorkStartedEntry{legacyEntry("rig:gascity", "gcg-3", 1)}}).Without("city", "gcg-3", 2); len(w.Claims) != 1 {
		t.Fatalf("Without reached an older generation's entry: %+v", w.Claims)
	}
}

// TestWorkStartedWithKeepsTheProofBesideAnUnmarkedEntry: an unmarked entry
// proves no residence, so recording one drops no resident entry of its
// generation (the proof a minted claim or a readback left): that stays
// beside it. Older generations' entries of the claim, resident or not, are
// superseded as before, and a resident entry supersedes every one.
func TestWorkStartedWithKeepsTheProofBesideAnUnmarkedEntry(t *testing.T) {
	proof := residentEntry("rig:gascity", "gcg-1", 2)
	w := WorkStarted{Claims: []WorkStartedEntry{proof, residentEntry("rig:gascity", "gcg-1", 1), legacyEntry("rig:gascity", "gcg-1", 1)}}
	spelling := legacyEntry("rig:gascity", "gcg-1", 2)
	if got := w.With(spelling); len(got.Claims) != 2 || got.Claims[0] != spelling || got.Claims[1] != proof {
		t.Fatalf("With(unmarked) = %+v; want the unmarked entry beside this generation's proof, the older entries superseded", got.Claims)
	}
	if got := w.With(proof); len(got.Claims) != 1 || got.Claims[0] != proof {
		t.Fatalf("With(resident) = %+v; want the resident entry superseding every entry of its claim", got.Claims)
	}
}

// TestWorkStartedRespelled: the hook's migration of a claim it recorded
// under its leg: the entry at the row's generation is re-spelled as resident
// in the residence store, keeping its generation, time, way and place in the
// cell. Older generations, other beads and resident entries are untouched.
func TestWorkStartedRespelled(t *testing.T) {
	old := legacyEntry("rig:gascity", "gcg-1", 2)
	old.Via = WorkStartedViaAdopt
	w := WorkStarted{Claims: []WorkStartedEntry{legacyEntry("rig:gascity", "gcg-9", 2), old, legacyEntry("rig:gascity", "gcg-1", 1)}}
	got, changed := w.Respelled("city", "gcg-1", 2)
	want := old
	want.Store, want.Resident = "city", true
	if !changed || len(got.Claims) != 3 || got.Claims[1] != want || got.Claims[0] != w.Claims[0] || got.Claims[2] != w.Claims[2] {
		t.Fatalf("Respelled = %+v, %v; want only the gen-2 gcg-1 entry re-spelled in place", got.Claims, changed)
	}
	if again, changed := got.Respelled("city", "gcg-1", 2); changed || len(again.Claims) != 3 {
		t.Fatalf("re-spelling a resident entry changed the cell: %+v", again.Claims)
	}
	if _, changed := w.Respelled("", "gcg-1", 2); changed {
		t.Fatal("an unknown store was written as a residence")
	}
	// Another generation's entry is no record of this incarnation, even one
	// already spelled as the residence: it is left as it is.
	older := WorkStarted{Claims: []WorkStartedEntry{legacyEntry("city", "gcg-1", 1)}}
	if got, changed := older.Respelled("city", "gcg-1", 2); changed || got.Claims[0] != older.Claims[0] {
		t.Fatalf("an older generation's entry was re-spelled: %+v", got.Claims)
	}
	// A storeless entry already names the bead in every store: re-spelling it
	// would narrow it.
	wild := WorkStarted{Claims: []WorkStartedEntry{legacyEntry("", "gcg-1", 2)}}
	if got, changed := wild.Respelled("city", "gcg-1", 2); changed || got.Claims[0] != wild.Claims[0] {
		t.Fatalf("a storeless entry was re-spelled: %+v", got.Claims)
	}
	// Two leg spellings of one claim at this generation (an 8d6r4 cell
	// written through two legs) become one resident entry, not two.
	twoLegs := WorkStarted{Claims: []WorkStartedEntry{legacyEntry("rig:a", "gcg-1", 2), legacyEntry("rig:b", "gcg-1", 2)}}
	if got, changed := twoLegs.Respelled("city", "gcg-1", 2); !changed || len(got.Claims) != 1 || got.Claims[0] != residentEntry("city", "gcg-1", 2) {
		t.Fatalf("Respelled over two leg spellings = %+v, %v; want one resident entry", got.Claims, changed)
	}
	// A resident entry of another generation is no record of this one: the
	// leg spelling at this generation is re-spelled, not dropped.
	olderResident := WorkStarted{Claims: []WorkStartedEntry{residentEntry("city", "gcg-1", 1), legacyEntry("rig:gascity", "gcg-1", 2)}}
	if got, changed := olderResident.Respelled("city", "gcg-1", 2); !changed || len(got.Claims) != 2 || got.Claims[1] != residentEntry("city", "gcg-1", 2) {
		t.Fatalf("Respelled beside an older resident entry = %+v, %v; want this generation's entry kept, re-spelled", got.Claims, changed)
	}
	// A resident entry already names the claim: the leg spelling beside it is
	// dropped, not re-spelled into a duplicate.
	both := WorkStarted{Claims: []WorkStartedEntry{residentEntry("city", "gcg-1", 2), legacyEntry("rig:gascity", "gcg-1", 2)}}
	if got, changed := both.Respelled("city", "gcg-1", 2); !changed || len(got.Claims) != 1 || got.Claims[0] != residentEntry("city", "gcg-1", 2) {
		t.Fatalf("Respelled beside a resident entry = %+v, %v; want the leg spelling dropped", got.Claims, changed)
	}
}

// TestWorkStartedRecordsBead: the entries naming a bead under every store at a
// generation: a storeless one, at any generation, and an unmarked one at its
// own; never a resident one, which names its own store alone, and never
// another bead.
func TestWorkStartedRecordsBead(t *testing.T) {
	for _, c := range []struct {
		name  string
		entry WorkStartedEntry
		gen   int64
		want  bool
	}{
		{"resident, its generation", residentEntry("city", "gcg-1", 2), 2, false},
		{"resident, another generation", residentEntry("city", "gcg-1", 1), 2, false},
		{"storeless, another generation", legacyEntry("", "gcg-1", 1), 2, true},
		{"storeless, an unreadable generation", legacyEntry("", "gcg-1", 0), 0, true},
		{"unmarked, its generation", legacyEntry("rig:gascity", "gcg-1", 2), 2, true},
		{"unmarked, another generation", legacyEntry("rig:gascity", "gcg-1", 1), 2, false},
		{"unmarked, an unreadable generation", legacyEntry("rig:gascity", "gcg-1", 0), 0, false},
	} {
		w := WorkStarted{Claims: []WorkStartedEntry{c.entry}}
		if got := w.RecordsBead("gcg-1", c.gen); got != c.want {
			t.Errorf("%s: RecordsBead = %v, want %v", c.name, got, c.want)
		}
		if w.RecordsBead("gcg-2", c.gen) {
			t.Errorf("%s: recorded another bead", c.name)
		}
	}
}

// TestWorkStartedResidentWire: the marker is one optional field; a cell
// without it (every cell the old writer wrote) decodes unmarked, and the
// cell's version is unchanged, so the old decoders read the new cells.
func TestWorkStartedResidentWire(t *testing.T) {
	raw := EncodeWorkStarted(WorkStarted{}.With(residentEntry("city", "gcg-1", 2)))
	if want := `{"v":1,"claims":[{"store":"city","bead":"gcg-1","gen":2,"at":"2026-10-17T20:00:00Z","via":"claim","resident":true}]}`; raw != want {
		t.Fatalf("encoded %s, want %s", raw, want)
	}
	back, err := DecodeWorkStarted(raw)
	if err != nil || len(back.Claims) != 1 || back.Claims[0] != residentEntry("city", "gcg-1", 2) {
		t.Fatalf("decode = %+v, %v", back, err)
	}
	legacy, err := DecodeWorkStarted(`{"v":1,"claims":[{"store":"rig:gascity","bead":"gcg-1","gen":2,"at":"2026-10-20T18:10:13.755491787Z","via":"claim"}]}`)
	if err != nil || len(legacy.Claims) != 1 || legacy.Claims[0].Resident {
		t.Fatalf("an old writer's cell decoded as %+v, %v; want one unmarked entry", legacy, err)
	}
}

// The 8d6r4 reader, frozen: its entry shape and its Names rule, verbatim.
// A rollback to 8d6r4 runs exactly this over the cells the fixed hook writes.
type rollbackEntry struct {
	Store string    `json:"store"`
	Bead  string    `json:"bead"`
	Gen   int64     `json:"gen"`
	At    time.Time `json:"at"`
	Via   string    `json:"via"`
}

func rollbackNames(raw, store, bead string) bool {
	var w struct {
		V      int             `json:"v"`
		Claims []rollbackEntry `json:"claims"`
	}
	if json.Unmarshal([]byte(raw), &w) != nil || w.V != workCellVersion {
		return false
	}
	for _, e := range w.Claims {
		if e.Bead != "" && e.Bead == bead && (e.Store == "" || store == "" || e.Store == store) {
			return true
		}
	}
	return false
}

// TestWorkStartedRollbackReadsTheResidence pins the rollback: an 8d6r4
// binary reading a resident entry spelled city matches it with its old rule
// (city == city), which is the spelling 8d6r4's ladder and backstop already
// key a binding-resident claim by. An 8d6r4 hook rewriting the cell drops the
// marker; the entry then reads here as the old writer's, which the relaxation
// still matches at its generation.
func TestWorkStartedRollbackReadsTheResidence(t *testing.T) {
	raw := EncodeWorkStarted(WorkStarted{}.With(residentEntry("city", "gcg--9223372036837583987", 2)))
	if !rollbackNames(raw, "city", "gcg--9223372036837583987") {
		t.Fatal("the 8d6r4 rule does not match a resident city entry with its city key")
	}
	if rollbackNames(raw, "rig:gascity", "gcg--9223372036837583987") {
		t.Fatal("the 8d6r4 rule matched the leg spelling against a resident entry")
	}
	var old struct {
		V      int             `json:"v"`
		Claims []rollbackEntry `json:"claims"`
	}
	if err := json.Unmarshal([]byte(raw), &old); err != nil {
		t.Fatal(err)
	}
	rewritten, _ := json.Marshal(old) // an 8d6r4 hook's With re-encodes every entry it keeps
	back, err := DecodeWorkStarted(string(rewritten))
	if err != nil || len(back.Claims) != 1 || back.Claims[0].Resident || !back.Names("city", "gcg--9223372036837583987", 2) {
		t.Fatalf("after an 8d6r4 rewrite: %+v, %v; want an unmarked entry that still names the claim at its generation", back, err)
	}
}
