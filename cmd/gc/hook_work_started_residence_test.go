package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/workladder"
)

// The hook's work_started key is the claim's RESIDENCE (claimStoreRef), the
// store the census and so the ladder and the backstop key it by — not the
// work leg the hook ran in. On 8d6r4 a rig seat's hook spelled a graph step it
// claimed out of the relocated binding by its rig leg (rig:gascity) while
// every reader keyed it city, so the ladder read started claims as unstarted
// (maintainer-city, 2026-10-20, ga-ylpbsy.41).

// residenceHook is a rig-scoped seat (its work leg is startedStoreRef, a rig)
// on a split city: the graph step resides in the relocated class binding, and
// the leg holds none of it.
type residenceHook struct {
	row   *startedRow
	log   *startedLog
	ops   hookClaimOps
	class beads.Store
	route *hookClaimClassRoute
}

func newResidenceHook(t *testing.T, workQuery string, cell string, legRead func(beadID string) (beads.Bead, error)) *residenceHook {
	t.Helper()
	class := newClaimRouteClassStore(t)
	route := newClaimRouteFor(t, class)
	route.recorder = func() events.Recorder { return events.Discard }
	var meta map[string]string
	if cell != "" {
		meta = map[string]string{session.WorkStartedKey: cell}
	}
	row := newStartedRow(t, meta)
	l := &startedLog{}
	base := l.ops(workQuery, row, nil)
	base.Claim = func(context.Context, string, []string, string, string) (beads.Bead, bool, error) {
		return beads.Bead{}, false, beads.ErrNotFound // the rig leg does not hold a graph step
	}
	if legRead == nil {
		legRead = func(string) (beads.Bead, error) { return beads.Bead{}, beads.ErrNotFound }
	}
	base.ReadWorkMeta = func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, error) {
		return legRead(beadID)
	}
	base.StampWorkMeta = func(context.Context, string, []string, string, string, map[string]string) error {
		return beads.ErrNotFound
	}
	return &residenceHook{row: row, log: l, ops: classRoutedHookClaimOps(base, route), class: class, route: route}
}

func (h *residenceHook) run(t *testing.T) (int, string) {
	t.Helper()
	stdout := &orderedWriter{l: h.log}
	var stderr bytes.Buffer
	code := doHookClaim("q", "/rig", startedOpts(startedEnv(startedToken, "7")), h.ops, stdout, &stderr)
	return code, stderr.String()
}

const residenceStep = "gcg-100"

// TestHookRecordsABindingResidentClaimByItsResidence (the live shape: row
// gcg-session-5029588e..., a gascity rig seat, bead gcg--9223372036837583987):
// a rig seat mints a claim on a graph step its class route takes out of the
// binding. The entry names the binding's residence, city, marked resident.
// Kills the leg key (rig:repo here, rig:gascity live).
func TestHookRecordsABindingResidentClaimByItsResidence(t *testing.T) {
	h := newResidenceHook(t, `[{"id":"gcg-100","status":"open","metadata":{"gc.routed_to":"worker"}}]`, "", nil)
	mintClaimRouteBead(t, h.class, residenceStep, map[string]string{"gc.routed_to": "worker"})
	if code, stderr := h.run(t); code != 0 {
		t.Fatalf("exit %d; stderr %s", code, stderr)
	}
	if b, err := h.class.Get(residenceStep); err != nil || b.Assignee != "worker-1" {
		t.Fatalf("binding holds %+v, %v; want the claim landed there", b, err)
	}
	cell := h.row.cell(t)
	if len(cell.Claims) != 1 {
		t.Fatalf("cell %+v, want one entry", cell.Claims)
	}
	if e := cell.Claims[0]; e.Store != "city" || !e.Resident || e.Bead != residenceStep || e.Gen != 7 || e.Via != session.WorkStartedViaClaim {
		t.Fatalf("entry %+v: want the residence city, marked resident, at the fenced generation", e)
	}
	if cell.Names(startedStoreRef, residenceStep, 7) {
		t.Fatal("the entry still names the leg the hook ran in")
	}
}

// TestHookRecordsABindingResidentAdoptionByItsResidence: the seat's claim is
// re-served (adopted); the adoption's canonical readback runs through the
// class route, which proves the binding holds it: city, resident.
func TestHookRecordsABindingResidentAdoptionByItsResidence(t *testing.T) {
	h := newResidenceHook(t, `[{"id":"gcg-100","status":"in_progress","assignee":"worker-1","metadata":{"gc.routed_to":"worker"}}]`, "", nil)
	mintClaimRouteBead(t, h.class, residenceStep, map[string]string{"gc.routed_to": "worker"})
	if _, ok, err := h.class.(interface {
		Claim(id, assignee string) (beads.Bead, bool, error)
	}).Claim(residenceStep, "worker-1"); err != nil || !ok {
		t.Fatalf("claiming the step in the binding: ok=%v err=%v", ok, err)
	}
	if code, stderr := h.run(t); code != 0 {
		t.Fatalf("exit %d; stderr %s", code, stderr)
	}
	if e := h.row.cell(t).Claims; len(e) != 1 || e[0].Store != "city" || !e[0].Resident || e[0].Via != session.WorkStartedViaAdopt {
		t.Fatalf("cell %+v: want the adopted claim's residence city, resident", e)
	}
}

// TestHookLeavesAnUnprovenAdoptionUnmarked: an adoption whose readback
// failed before the route could ask the binding is recorded under the leg the
// hook ran in, as the older hook wrote every entry, and unmarked: the readers
// match it by bead at its generation rather than trust a store nobody proved.
// Kills a hook that marks an unproven leg spelling resident (which no reader
// could then match to the binding's claim).
func TestHookLeavesAnUnprovenAdoptionUnmarked(t *testing.T) {
	h := newResidenceHook(t, `[{"id":"gcg-100","status":"in_progress","assignee":"worker-1","metadata":{"gc.routed_to":"worker"}}]`, "",
		func(string) (beads.Bead, error) { return beads.Bead{}, errors.New("bd: connection refused") })
	if code, stderr := h.run(t); code != 0 {
		t.Fatalf("exit %d; stderr %s", code, stderr)
	}
	cell := h.row.cell(t)
	if len(cell.Claims) != 1 || cell.Claims[0].Store != startedStoreRef || cell.Claims[0].Resident {
		t.Fatalf("cell %+v: want the leg spelling, unmarked", cell.Claims)
	}
	if !cell.Names("city", residenceStep, 7) || cell.Names("city", residenceStep, 8) {
		t.Fatal("the unmarked entry must name the binding's claim at its generation, and only there")
	}
}

// TestHookAdoptionRecordsStartedOnceOnARelocatingCity (T2 on a city that
// relocates classes, the dominant path there): a rig seat mints a claim in
// its leg and re-serves it twice, each readback finding it in the leg, then
// once more after the row's generation moved. The minted entry, resident,
// records every hand-over: one write in all, whether the city's route opened
// or degraded. Kills a hook that takes a leg readback for no proof of
// residence: its first re-serve rewrites the minted entry unmarked, as does
// each new generation's, and the city key then reads every store's copy of
// the id as started.
func TestHookAdoptionRecordsStartedOnceOnARelocatingCity(t *testing.T) {
	for _, c := range []struct {
		name     string
		relocate func(*testing.T, hookClaimOps) hookClaimOps
	}{
		{"a class route", func(t *testing.T, ops hookClaimOps) hookClaimOps {
			route := newClaimRouteFor(t, newClaimRouteClassStore(t))
			route.recorder = func() events.Recorder { return events.Discard }
			return classRoutedHookClaimOps(ops, route)
		}},
		{"a route that degraded", func(_ *testing.T, ops hookClaimOps) hookClaimOps {
			ops.ClassesRelocated = true
			return ops
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			row := newStartedRow(t, nil)
			for i, turn := range []struct{ work, gen, via string }{
				{startedRoutedWork, "7", session.WorkStartedViaClaim},
				{startedAssignedWork, "7", session.WorkStartedViaAdopt},
				{startedAssignedWork, "7", session.WorkStartedViaAdopt},
				{startedAssignedWork, "8", session.WorkStartedViaAdopt},
			} {
				b, err := row.store.Get(row.id)
				if err != nil {
					t.Fatal(err)
				}
				if b.Metadata["generation"] != turn.gen {
					// The controller's write, not the hook's: uncounted.
					if err := row.store.MemStore.UpdateIfMatch(row.id, b.Revision, beads.UpdateOpts{Metadata: map[string]string{"generation": turn.gen}}); err != nil {
						t.Fatal(err)
					}
				}
				l := &startedLog{}
				stdout := &orderedWriter{l: l}
				var stderr bytes.Buffer
				if code := doHookClaim("q", "/rig", startedOpts(startedEnv(startedToken, turn.gen)), c.relocate(t, l.ops(turn.work, row, nil)), stdout, &stderr); code != 0 {
					t.Fatalf("turn %d: code %d; stderr=%s", i, code, stderr.String())
				}
				if l.index("record:work-1:"+turn.via) < 0 {
					t.Fatalf("turn %d: effects %v; want the hand-over recorded as %s", i, l.log, turn.via)
				}
			}
			want := session.WorkStartedEntry{Store: startedStoreRef, Bead: "work-1", Gen: 7, At: time.Date(2026, 10, 17, 20, 0, 0, 0, time.UTC), Via: session.WorkStartedViaClaim, Resident: true}
			if cell := row.cell(t); row.store.rowWrites != 1 || !slices.Equal(cell.Claims, []session.WorkStartedEntry{want}) {
				t.Fatalf("conditional row writes = %d, cell %+v; want the minted write alone, its entry kept resident", row.store.rowWrites, cell.Claims)
			}
		})
	}
}

// TestHookRespellsALegSpelledEntry: the cell's migration. A seat whose
// 8d6r4 hook recorded its graph step under its rig leg at this generation
// (the live cell, verbatim but for this row's leg) re-serves the claim: the
// entry is re-spelled as the residence, in place, keeping its generation, its
// time and its way; an older generation's leg spelling of the same bead is
// left to age out of the cell, and the claim is recorded at this one.
func TestHookRespellsALegSpelledEntry(t *testing.T) {
	adopted := `[{"id":"gcg-100","status":"in_progress","assignee":"worker-1","metadata":{"gc.routed_to":"worker"}}]`
	at := "2026-10-20T18:10:13.755491787Z"
	t.Run("this generation", func(t *testing.T) {
		h := newResidenceHook(t, adopted, fmt.Sprintf(`{"v":1,"claims":[{"store":%q,"bead":"gcg-100","gen":7,"at":%q,"via":"claim"}]}`, startedStoreRef, at), nil)
		mintClaimRouteBead(t, h.class, residenceStep, map[string]string{"gc.routed_to": "worker"})
		_, _, _ = h.class.(interface {
			Claim(id, assignee string) (beads.Bead, bool, error)
		}).Claim(residenceStep, "worker-1")
		if code, stderr := h.run(t); code != 0 {
			t.Fatalf("exit %d; stderr %s", code, stderr)
		}
		cell := h.row.cell(t)
		want, _ := time.Parse(time.RFC3339Nano, at)
		if len(cell.Claims) != 1 || cell.Claims[0] != (session.WorkStartedEntry{Store: "city", Bead: residenceStep, Gen: 7, At: want, Via: session.WorkStartedViaClaim, Resident: true}) {
			t.Fatalf("cell %+v: want the leg spelling re-spelled city in place, its generation, time and way kept", cell.Claims)
		}
	})
	t.Run("an older generation", func(t *testing.T) {
		h := newResidenceHook(t, adopted, fmt.Sprintf(`{"v":1,"claims":[{"store":%q,"bead":"gcg-100","gen":6,"at":%q,"via":"claim"}]}`, startedStoreRef, at), nil)
		mintClaimRouteBead(t, h.class, residenceStep, map[string]string{"gc.routed_to": "worker"})
		_, _, _ = h.class.(interface {
			Claim(id, assignee string) (beads.Bead, bool, error)
		}).Claim(residenceStep, "worker-1")
		if code, stderr := h.run(t); code != 0 {
			t.Fatalf("exit %d; stderr %s", code, stderr)
		}
		cell := h.row.cell(t)
		if len(cell.Claims) != 2 || cell.Claims[0].Store != "city" || !cell.Claims[0].Resident || cell.Claims[0].Gen != 7 ||
			cell.Claims[0].Via != session.WorkStartedViaAdopt || cell.Claims[1].Store != startedStoreRef || cell.Claims[1].Gen != 6 {
			t.Fatalf("cell %+v: want this generation's record first, the older leg spelling kept behind it", cell.Claims)
		}
	})
}

// TestHookFenceMatchesTheLaddersReleaseAcrossSpellings (§6.4): the hook
// refuses to hand out a claim the ladder is releasing. The ladder keys its
// release by the residence (city); the hook's proven key is the residence too,
// so it matches whatever the cell holds; an unproven leg key matches by bead.
// On 8d6r4 the hook's key was the leg's (rig:repo here), a resident key that
// missed the city release: the last row is that miss, which the new hook can
// no longer produce for a binding claim.
func TestHookFenceMatchesTheLaddersReleaseAcrossSpellings(t *testing.T) {
	release := func(store, bead string) string {
		return fmt.Sprintf(`{"v":1,"gen":7,"entries":[],"release":{"store":%q,"bead":%q,"gen":7,"at":"2026-10-20T18:14:27Z"}}`, store, bead)
	}
	legacyCell := session.EncodeWorkStarted(session.WorkStarted{Claims: []session.WorkStartedEntry{{Store: startedStoreRef, Bead: residenceStep, Gen: 7, At: stepsTestNow, Via: session.WorkStartedViaClaim}}})
	residentCell := session.EncodeWorkStarted(session.WorkStarted{}.With(session.WorkStartedEntry{Store: "city", Bead: residenceStep, Gen: 7, At: stepsTestNow, Via: session.WorkStartedViaClaim, Resident: true}))
	for _, c := range []struct {
		name         string
		ladder, cell string
		key          hookStartedKey
		want         hookStartedResult
	}{
		{"residence key, census release", release("city", residenceStep), "", hookStartedKey{Store: "city", Bead: residenceStep, Resident: true}, hookStartedReleasing},
		{"residence key, cell still holds the leg spelling", release("city", residenceStep), legacyCell, hookStartedKey{Store: "city", Bead: residenceStep, Resident: true}, hookStartedReleasing},
		{"residence key, cell holds the residence", release("city", residenceStep), residentCell, hookStartedKey{Store: "city", Bead: residenceStep, Resident: true}, hookStartedReleasing},
		{"unproven leg key, census release", release("city", residenceStep), "", hookStartedKey{Store: startedStoreRef, Bead: residenceStep}, hookStartedReleasing},
		{"release of another bead", release("city", "gcg-200"), "", hookStartedKey{Store: "city", Bead: residenceStep, Resident: true}, hookStartedRecorded},
		{"8d6r4's resident leg key misses the city release", release("city", residenceStep), "", hookStartedKey{Store: startedStoreRef, Bead: residenceStep, Resident: true}, hookStartedRecorded},
	} {
		t.Run(c.name, func(t *testing.T) {
			fresh := session.Info{InstanceToken: "tok", Generation: "7", WorkLadderRaw: c.ladder, WorkStartedRaw: c.cell}
			res, _, err := decideWorkStarted(fresh, hookStartedFence{Token: "tok", Gen: 7, HasGen: true}, c.key, session.WorkStartedViaClaim, stepsTestNow)
			if err != nil || res != c.want {
				t.Fatalf("decide = %v, %v; want %v", res, err, c.want)
			}
		})
	}
}

// TestHookStartedKeyForIsTheResidence pins hookStartedKeyFor's table: the
// binding when the route proved it; the work leg, resident, for a minted
// claim, for an adoption whose readback found it there, and for any claim on
// a city with no route; the leg unproven for an adoption whose readback found
// it in no store, on a city whose route opened or degraded; and no store at
// all when the leg is unknown.
func TestHookStartedKeyForIsTheResidence(t *testing.T) {
	route := newClaimRouteFor(t, newClaimRouteClassStore(t))
	route.resident["gcg-1"] = true
	leg := func(string) string { return "rig:gascity" }
	for _, c := range []struct {
		name    string
		ops     hookClaimOps
		bead    string
		verdict hookAdoptionVerdict
		want    hookStartedKey
	}{
		{"binding claim, minted", hookClaimOps{StoreRef: leg, ClassRoute: route}, "gcg-1", hookClaimMinted, hookStartedKey{Store: "city", Bead: "gcg-1", Resident: true}},
		{"binding claim, adopted", hookClaimOps{StoreRef: leg, ClassRoute: route}, "gcg-1", hookAdoptionOwned, hookStartedKey{Store: "city", Bead: "gcg-1", Resident: true}},
		{"work-leg claim, minted on a split city", hookClaimOps{StoreRef: leg, ClassRoute: route}, "ga-1", hookClaimMinted, hookStartedKey{Store: "rig:gascity", Bead: "ga-1", Resident: true}},
		{"work-leg claim, adopted on a split city, the leg readback found it", hookClaimOps{StoreRef: leg, ClassRoute: route}, "ga-1", hookAdoptionOwned, hookStartedKey{Store: "rig:gascity", Bead: "ga-1", Resident: true}},
		{"work-leg claim, adopted on a split city, the readback failed", hookClaimOps{StoreRef: leg, ClassRoute: route}, "ga-1", hookAdoptionUnverified, hookStartedKey{Store: "rig:gascity", Bead: "ga-1"}},
		{"adopted on a city with no route, the readback failed", hookClaimOps{StoreRef: leg}, "ga-1", hookAdoptionUnverified, hookStartedKey{Store: "rig:gascity", Bead: "ga-1", Resident: true}},
		{"adopted on a city whose route degraded, the leg readback found it", hookClaimOps{StoreRef: leg, ClassesRelocated: true}, "ga-1", hookAdoptionOwned, hookStartedKey{Store: "rig:gascity", Bead: "ga-1", Resident: true}},
		{"adopted on a city whose route degraded, no route could ask the binding", hookClaimOps{StoreRef: leg, ClassesRelocated: true}, "gcg-1", hookAdoptionUnverified, hookStartedKey{Store: "rig:gascity", Bead: "gcg-1"}},
		{"minted on a city whose route degraded", hookClaimOps{StoreRef: leg, ClassesRelocated: true}, "ga-1", hookClaimMinted, hookStartedKey{Store: "rig:gascity", Bead: "ga-1", Resident: true}},
		{"unknown leg", hookClaimOps{StoreRef: func(string) string { return "" }}, " ga-1 ", hookClaimMinted, hookStartedKey{Bead: "ga-1"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := hookStartedKeyFor(c.ops, "/rig", c.bead, c.verdict); got != c.want {
				t.Fatalf("hookStartedKeyFor = %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestHookDrainAckFenceReadsTheBindingsEntries (T7 for a graph step): the
// drain-ack fence fails safe on a failed read of an entry this context can
// read. A rig seat's route reads through to the binding, where a graph step's
// resident entry (city) points, so its failed read holds the ack, as the leg
// spelling's did; with no route, city is a store this context cannot read
// and its failed read is skipped, as before.
func TestHookDrainAckFenceReadsTheBindingsEntries(t *testing.T) {
	cell := startedCell(session.WorkStartedEntry{Store: "city", Bead: residenceStep, Gen: 7, Resident: true})
	other := startedCell(session.WorkStartedEntry{Store: "rig:other", Bead: residenceStep, Gen: 7, Resident: true})
	route := newClaimRouteFor(t, newClaimRouteClassStore(t))
	for _, c := range []struct {
		name  string
		cell  string
		route *hookClaimClassRoute
		leg   string
		acked bool
	}{
		{"a rig seat with a class route", cell, route, startedStoreRef, false},
		{"a rig seat on a city with no route", cell, nil, startedStoreRef, true},
		// A leg this context cannot name skips nothing, the binding's
		// entries or any other store's, as before.
		{"an unknown leg with a class route", other, route, "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := &startedLog{}
			ops := l.ops(`[]`, nil, nil)
			ops.StoreRef = func(string) string { return c.leg }
			ops.ReadWorkStarted = func(string) (string, error) { return c.cell, nil }
			ops.ReadWorkMeta = func(context.Context, string, []string, string, string) (beads.Bead, error) {
				return beads.Bead{}, errors.New("sqlite: database is locked")
			}
			ops.ClassRoute = c.route
			stdout := &orderedWriter{l: l}
			var stderr bytes.Buffer
			doHookClaim("q", "/rig", startedOpts(startedEnv(startedToken, "7")), ops, stdout, &stderr)
			got := decodeTurnBoundResult(t, stdout.buf.String())
			if l.acked != c.acked || got.DrainAcknowledged != c.acked {
				t.Fatalf("result %+v, acked %v, stderr %q; want acked=%v", got, l.acked, strings.TrimSpace(stderr.String()), c.acked)
			}
		})
	}
}

// TestUnwindForgetsTheClaimsKey: an unwind gives a claim back under the key
// its record wrote (the residence city here), and a released claim must not
// read as started to a reader keying it so: were the seat to claim it again, a
// stale record would make the new claim read started before any turn began
// it. The forget drops the key's store's entries at every generation, a
// storeless entry, and an unmarked one of this generation (an older hook's leg
// spelling of the claim). It keeps a resident entry of another store, which
// the record took for a co-resident copy's
// (TestHookResidenceKeyIsExactOverAnotherStoresEntry) and which this unwind
// does not give back, and other beads' entries.
func TestUnwindForgetsTheClaimsKey(t *testing.T) {
	copyEntry := session.WorkStartedEntry{Store: "rig:other", Bead: residenceStep, Gen: 5, At: stepsTestNow, Via: session.WorkStartedViaClaim, Resident: true}
	otherBead := session.WorkStartedEntry{Store: startedStoreRef, Bead: "gcg-200", Gen: 6, At: stepsTestNow, Via: session.WorkStartedViaClaim}
	cell := session.EncodeWorkStarted(session.WorkStarted{Claims: []session.WorkStartedEntry{
		{Store: startedStoreRef, Bead: residenceStep, Gen: 7, At: stepsTestNow, Via: session.WorkStartedViaClaim},
		{Store: "city", Bead: residenceStep, Gen: 6, At: stepsTestNow, Via: session.WorkStartedViaClaim, Resident: true},
		{Bead: residenceStep, Gen: 4, At: stepsTestNow, Via: session.WorkStartedViaClaim},
		copyEntry,
		otherBead,
	}})
	row := newStartedRow(t, map[string]string{session.WorkStartedKey: cell})
	l := &startedLog{}
	ops := l.ops(`[]`, row, nil)
	forgetHookWorkStarted(startedOpts(startedEnv(startedToken, "7")), ops, hookStartedKey{Store: "city", Bead: residenceStep, Resident: true})
	got := row.cell(t)
	if !slices.Equal(got.Claims, []session.WorkStartedEntry{copyEntry, otherBead}) {
		t.Fatalf("cell %+v: want the claim's records forgotten, the co-resident copy's and gcg-200's kept", got.Claims)
	}
	if got.Names("city", residenceStep, 7) {
		t.Fatal("the given-back claim still reads started under its key")
	}
}

// TestHookUnwindForgetsARouteProvenClaim (F-C through the route): a rig seat
// mints a binding-resident graph step, records it (city, resident), and its
// result line cannot be written. The unwind forgets the entry and gives the
// claim back in the binding, where it landed.
func TestHookUnwindForgetsARouteProvenClaim(t *testing.T) {
	h := newResidenceHook(t, `[{"id":"gcg-100","status":"open","metadata":{"gc.routed_to":"worker"}}]`, "", nil)
	mintClaimRouteBead(t, h.class, residenceStep, map[string]string{"gc.routed_to": "worker"})
	stdout := &orderedWriter{l: h.log, err: syscall.EPIPE}
	var stderr bytes.Buffer
	if code := doHookClaim("q", "/rig", startedOpts(startedEnv(startedToken, "7")), h.ops, stdout, &stderr); code == 0 {
		t.Fatalf("exit 0 though the result could not be written; stderr %s", stderr.String())
	}
	if h.log.index("record:"+residenceStep+":claim") < 0 {
		t.Fatalf("effects %v: want the claim recorded before the result write", h.log.log)
	}
	if cell := h.row.cell(t); cell.Names("city", residenceStep, 7) || len(cell.Claims) != 0 {
		t.Fatalf("cell %+v: the given-back claim still reads started", cell.Claims)
	}
	if b, err := h.class.Get(residenceStep); err != nil || b.Assignee != "" || b.Status != "open" {
		t.Fatalf("binding holds %s as %q/%q (%v); want it given back", residenceStep, b.Status, b.Assignee, err)
	}
}

// TestHookUnwindForgetsTheKeyItRecorded (F-C after the residence memo moved):
// a rig seat mints a claim in its leg and records it there (resident). Before
// its result line fails, the class route's memo learns that the binding holds
// the id too, as a continuation listing's probe can on a city holding a
// co-resident copy. The unwind forgets the key the record wrote, not one
// recomputed from the moved memo (city), and keeps the copy's own entry.
// Kills a forget that recomputes the key at unwind time, and one that forgets
// the bead in every store.
func TestHookUnwindForgetsTheKeyItRecorded(t *testing.T) {
	copyEntry := session.WorkStartedEntry{Store: "city", Bead: "work-1", Gen: 5, At: stepsTestNow, Via: session.WorkStartedViaClaim, Resident: true}
	row := newStartedRow(t, map[string]string{session.WorkStartedKey: session.EncodeWorkStarted(session.WorkStarted{Claims: []session.WorkStartedEntry{copyEntry}})})
	l := &startedLog{}
	route := newClaimRouteFor(t, newClaimRouteClassStore(t))
	route.recorder = func() events.Recorder { return events.Discard }
	ops := classRoutedHookClaimOps(l.ops(startedRoutedWork, row, nil), route)
	var recorded, forgotten []hookStartedKey
	record, forget, publish := ops.RecordWorkStarted, ops.ForgetWorkStarted, ops.PublishRunMap
	ops.RecordWorkStarted = func(sessionID string, fence hookStartedFence, key hookStartedKey, via string) (hookStartedResult, error) {
		recorded = append(recorded, key)
		return record(sessionID, fence, key, via)
	}
	ops.ForgetWorkStarted = func(sessionID string, key hookStartedKey) error {
		forgotten = append(forgotten, key)
		return forget(sessionID, key)
	}
	// After the record and before the result line: the memo learns the id.
	ops.PublishRunMap = func(runID, beadID string, sessionKeys ...string) error {
		route.resident["work-1"] = true
		return publish(runID, beadID, sessionKeys...)
	}
	stdout := &orderedWriter{l: l, err: syscall.EPIPE}
	var stderr bytes.Buffer
	if code := doHookClaim("q", "/rig", startedOpts(startedEnv(startedToken, "7")), ops, stdout, &stderr); code != 1 {
		t.Fatalf("exit %d though the result could not be written; stderr %s", code, stderr.String())
	}
	want := hookStartedKey{Store: startedStoreRef, Bead: "work-1", Resident: true}
	if !slices.Equal(recorded, []hookStartedKey{want}) || !slices.Equal(forgotten, []hookStartedKey{want}) {
		t.Fatalf("recorded %+v, forgot %+v; want the one key %+v both times", recorded, forgotten, want)
	}
	if got := row.cell(t); !slices.Equal(got.Claims, []session.WorkStartedEntry{copyEntry}) {
		t.Fatalf("cell %+v: want the given-back claim forgotten and the copy's entry kept", got.Claims)
	}
}

// TestHookUnprovenAdoptionRecordsNothingNew (the respell guard): an adoption
// whose residence the hook could not prove finds the claim already recorded
// when an entry names its bead under every store at this generation: an older
// hook's leg spelling of it, through this leg or another, or a storeless
// entry. It writes nothing (it must not mark an unproven leg as the
// residence, which no reader keyed city could match). On a row whose
// generation nobody can read, the readers match exactly, so its own leg
// spelling is its record and a second such hand-over writes nothing either.
func TestHookUnprovenAdoptionRecordsNothingNew(t *testing.T) {
	entry := func(store string, gen int64) string {
		return session.EncodeWorkStarted(session.WorkStarted{Claims: []session.WorkStartedEntry{{Store: store, Bead: residenceStep, Gen: gen, At: stepsTestNow, Via: session.WorkStartedViaClaim}}})
	}
	legacy := entry(startedStoreRef, 7)
	fenced := hookStartedFence{Token: "tok", Gen: 7, HasGen: true}
	for _, c := range []struct {
		name, cell, gen string
		fence           hookStartedFence
	}{
		{"an older hook's leg spelling", legacy, "7", fenced},
		{"an older hook's spelling through another leg", entry("rig:other", 7), "7", fenced},
		{"a storeless entry, a generation ago", entry("", 5), "7", fenced},
		{"its own leg spelling, no readable generation", entry(startedStoreRef, 0), "", hookStartedFence{Token: "tok"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			fresh := session.Info{InstanceToken: "tok", Generation: c.gen, WorkStartedRaw: c.cell}
			res, patch, err := decideWorkStarted(fresh, c.fence,
				hookStartedKey{Store: startedStoreRef, Bead: residenceStep}, session.WorkStartedViaAdopt, stepsTestNow)
			if err != nil || res != hookStartedAlreadyRecorded || patch != nil {
				t.Fatalf("decide = %v, patch %v, %v; want already recorded and nothing written", res, patch, err)
			}
		})
	}
	t.Run("through the hook, the readback failing", func(t *testing.T) {
		h := newResidenceHook(t, `[{"id":"gcg-100","status":"in_progress","assignee":"worker-1","metadata":{"gc.routed_to":"worker"}}]`, legacy,
			func(string) (beads.Bead, error) { return beads.Bead{}, errors.New("bd: connection refused") })
		before := h.row.store.rowWrites
		if code, stderr := h.run(t); code != 0 {
			t.Fatalf("exit %d; stderr %s", code, stderr)
		}
		if h.row.store.rowWrites != before || h.row.meta(t, session.WorkStartedKey) != legacy {
			t.Fatalf("the cell was rewritten to %s", h.row.meta(t, session.WorkStartedKey))
		}
	})
}

// TestHookUnprovenAdoptionTakesNoResidentEntryForIt: an adoption whose
// residence the hook could not prove counts no resident entry as its record,
// whatever store it names and whatever its generation. A resident entry is
// exact to its store, and the readers may key this claim by another: a graph
// step adopted through a rig seat's leg is keyed city, and on a migrated city
// a co-resident copy of the id may hold the entry. The adoption records its
// leg spelling, unmarked, at this generation, which every reader matches
// there; it keeps the resident entries of other stores, and its own leg's of
// this generation (the proof a minted claim or a readback left), and
// supersedes its own leg's older one. Over the binding's entry that is a
// duplicate, which the next proven hand-over drops (Respelled). Kills an
// unproven dedup that takes a resident entry for the claim, through
// RecordsBead or through Names over the leg, and an unproven write over its
// leg's proof of this generation.
func TestHookUnprovenAdoptionTakesNoResidentEntryForIt(t *testing.T) {
	fence := hookStartedFence{Token: "tok", Gen: 7, HasGen: true}
	spelling := session.WorkStartedEntry{Store: startedStoreRef, Bead: residenceStep, Gen: 7, At: stepsTestNow, Via: session.WorkStartedViaAdopt}
	resident := func(store string, gen int64) session.WorkStartedEntry {
		return session.WorkStartedEntry{Store: store, Bead: residenceStep, Gen: gen, At: stepsTestNow, Via: session.WorkStartedViaClaim, Resident: true}
	}
	cellOf := func(claims ...session.WorkStartedEntry) string {
		return session.EncodeWorkStarted(session.WorkStarted{Claims: claims})
	}
	for _, c := range []struct {
		name  string
		prior session.WorkStartedEntry
		want  []session.WorkStartedEntry
	}{
		{"another store's, a generation ago", resident("rig:other", 5), []session.WorkStartedEntry{spelling, resident("rig:other", 5)}},
		{"another store's, this generation", resident("rig:other", 7), []session.WorkStartedEntry{spelling, resident("rig:other", 7)}},
		{"the leg's, a generation ago", resident(startedStoreRef, 5), []session.WorkStartedEntry{spelling}},
		{"the leg's, this generation", resident(startedStoreRef, 7), []session.WorkStartedEntry{spelling, resident(startedStoreRef, 7)}},
		{"the binding's", resident("city", 7), []session.WorkStartedEntry{spelling, resident("city", 7)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			fresh := session.Info{InstanceToken: "tok", Generation: "7", WorkStartedRaw: cellOf(c.prior)}
			res, patch, err := decideWorkStarted(fresh, fence, hookStartedKey{Store: startedStoreRef, Bead: residenceStep}, session.WorkStartedViaAdopt, stepsTestNow)
			if err != nil || res != hookStartedRecorded {
				t.Fatalf("decide = %v, %v; want the claim recorded", res, err)
			}
			cell, err := session.DecodeWorkStarted(patch[session.WorkStartedKey])
			if err != nil || !slices.Equal(cell.Claims, c.want) {
				t.Fatalf("cell %+v, %v; want %+v", cell.Claims, err, c.want)
			}
			if !cell.Names("city", residenceStep, 7) {
				t.Fatal("the binding's key still reads the claim unstarted")
			}
		})
	}
	for _, c := range []struct{ name, store string }{{"the binding's", "city"}, {"the leg's", startedStoreRef}} {
		t.Run(c.name+", at the next proven hand-over", func(t *testing.T) {
			fresh := session.Info{InstanceToken: "tok", Generation: "7", WorkStartedRaw: cellOf(spelling, resident(c.store, 7))}
			res, patch, err := decideWorkStarted(fresh, fence, hookStartedKey{Store: c.store, Bead: residenceStep, Resident: true}, session.WorkStartedViaAdopt, stepsTestNow)
			folded, decodeErr := session.DecodeWorkStarted(patch[session.WorkStartedKey])
			if err != nil || res != hookStartedAlreadyRecorded || decodeErr != nil || !slices.Equal(folded.Claims, []session.WorkStartedEntry{resident(c.store, 7)}) {
				t.Fatalf("decide = %v, %v, cell %+v (%v); want the duplicate leg spelling dropped beside the residence", res, err, folded.Claims, decodeErr)
			}
		})
	}
	// The live failure: the readback fails, the route never asks the binding,
	// and the ladder and the backstop key the claim city.
	t.Run("through the hook, the readback failing", func(t *testing.T) {
		h := newResidenceHook(t, `[{"id":"gcg-100","status":"in_progress","assignee":"worker-1","metadata":{"gc.routed_to":"worker"}}]`, cellOf(resident("rig:other", 5)),
			func(string) (beads.Bead, error) { return beads.Bead{}, errors.New("bd: connection refused") })
		if code, stderr := h.run(t); code != 0 {
			t.Fatalf("exit %d; stderr %s", code, stderr)
		}
		row, err := h.row.store.Get(h.row.id)
		if err != nil {
			t.Fatal(err)
		}
		raw := row.Metadata[session.WorkStartedKey]
		if ladder, err := workladder.DecodeStarted(raw); err != nil || !ladder.Names(workladder.ClaimKey{Store: "city", Bead: residenceStep}, 7) {
			t.Fatalf("the ladder's city key reads %s unstarted (%v)", raw, err)
		}
		if !workStartedAtGen(row, executionClaim{StoreRef: "city", BeadID: residenceStep}) {
			t.Fatalf("the backstop's city key reads %s unstarted", raw)
		}
	})
}

// respellFailingStore fails every conditional write to the session row with a
// hard store error.
type respellFailingStore struct{ *casCountingStore }

func (s respellFailingStore) UpdateIfMatch(id string, expectedRevision int64, opts beads.UpdateOpts) error {
	if id == s.rowID {
		return errors.New("sqlite: disk I/O error")
	}
	return s.casCountingStore.UpdateIfMatch(id, expectedRevision, opts)
}

// TestHookRespellFailureIsNotUnrecorded: re-spelling an older hook's entry is
// a migration of a claim the cell already records. A store error on that
// write leaves it recorded under the leg's spelling (which the readers still
// match at its generation): the claim is delivered as recorded, with one
// stderr line, and no work.started_unrecorded.
func TestHookRespellFailureIsNotUnrecorded(t *testing.T) {
	legacy := session.EncodeWorkStarted(session.WorkStarted{Claims: []session.WorkStartedEntry{{Store: startedStoreRef, Bead: residenceStep, Gen: 7, At: stepsTestNow, Via: session.WorkStartedViaClaim}}})
	row := newStartedRow(t, map[string]string{session.WorkStartedKey: legacy})
	front := session.NewStore(beads.SessionStore{Store: blindWriteRefusingStore{inner: respellFailingStore{row.store}}})
	res, err := recordWorkStarted(front, row.id, hookStartedFence{Token: startedToken, Gen: 7, HasGen: true},
		hookStartedKey{Store: "city", Bead: residenceStep, Resident: true}, session.WorkStartedViaAdopt, stepsTestNow)
	if res != hookStartedAlreadyRecorded || !errors.Is(err, errWorkStartedRespell) {
		t.Fatalf("record = %v, %v; want already recorded with the re-spell failure", res, err)
	}
	l := &startedLog{}
	ops := l.ops(`[]`, nil, func(string, hookStartedFence, hookStartedKey, string) (hookStartedResult, error) { return res, err })
	var stderr bytes.Buffer
	if got := recordHookWorkStarted(beads.Bead{ID: residenceStep}, startedOpts(startedEnv(startedToken, "7")), ops, hookStartedKey{Store: "city", Bead: residenceStep, Resident: true}, false, &stderr); got != hookStartedAlreadyRecorded {
		t.Fatalf("recordHookWorkStarted = %v, want already recorded", got)
	}
	if len(l.unrecorded) != 0 || !strings.Contains(stderr.String(), "migrating") {
		t.Fatalf("unrecorded %v, stderr %q; want no work.started_unrecorded and one note", l.unrecorded, stderr.String())
	}
}

// TestHookClaimClassesRelocated: a route that opened, or one a binding with no
// claim CAS degraded to nil, both mean the city relocates; a city with no
// binding, or a resolution that failed outright (the claim is refused before
// any key is computed), does not.
func TestHookClaimClassesRelocated(t *testing.T) {
	route := newClaimRouteFor(t, newClaimRouteClassStore(t))
	for _, c := range []struct {
		name   string
		opened *hookClaimClassRoute
		err    error
		want   bool
	}{
		{"a route opened", route, nil, true},
		{"a binding with no claim CAS", nil, fmt.Errorf("probe: %w", errClaimRouteBindingCannotClaim), true},
		{"a city that relocates nothing", nil, nil, false},
		{"a resolution that failed", nil, errors.New("binding unreadable"), false},
	} {
		if got := hookClaimClassesRelocated(c.opened, c.err); got != c.want {
			t.Errorf("%s: relocated = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestRoutedHookClaimOpsMarksARelocatedCity: the ops a class-route resolution
// yields. A route that opened wraps them and takes the recorder; a binding
// with no claim CAS degrades to no route but still marks the city as one
// that relocates (so an adoption there whose readback found it in no store is
// no proven residence); a city that relocates nothing is neither; any other
// failure refuses the claim.
func TestRoutedHookClaimOpsMarksARelocatedCity(t *testing.T) {
	route := newClaimRouteFor(t, newClaimRouteClassStore(t))
	recorder := func() events.Recorder { return events.Discard }
	leg := func(string) string { return startedStoreRef }
	for _, c := range []struct {
		name               string
		opened             *hookClaimClassRoute
		err                error
		proceed, relocated bool
		routed             bool
	}{
		{"a route opened", route, nil, true, true, true},
		{"a binding with no claim CAS", nil, fmt.Errorf("probe: %w", errClaimRouteBindingCannotClaim), true, true, false},
		{"a city that relocates nothing", nil, nil, true, false, false},
		{"any other failure", nil, errors.New("binding unreadable"), false, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var stderr bytes.Buffer
			ops, proceed := routedHookClaimOps(hookClaimOps{StoreRef: leg}, c.opened, c.err, recorder, &stderr)
			if proceed != c.proceed {
				t.Fatalf("proceed = %v, want %v", proceed, c.proceed)
			}
			if !proceed {
				return
			}
			if ops.ClassesRelocated != c.relocated || (ops.ClassRoute != nil) != c.routed {
				t.Fatalf("relocated %v, routed %v; want %v, %v", ops.ClassesRelocated, ops.ClassRoute != nil, c.relocated, c.routed)
			}
			if c.routed && ops.ClassRoute.recorder == nil {
				t.Fatal("the opened route did not take the recorder")
			}
			key := hookStartedKeyFor(ops, "/rig", "gcg-9", hookAdoptionUnverified)
			if key.Resident == c.relocated {
				t.Fatalf("an unverified adoption's key %+v on relocated=%v: want unmarked exactly when the city relocates", key, c.relocated)
			}
		})
	}
}

// TestHookResidenceKeyIsExactOverAnotherStoresEntry: a proven residence key
// is never matched by bead alone. Over a resident entry of another store for
// the same id (a co-resident copy on a migrated city, a generation ago), the
// hook records the claim under its residence rather than taking that entry
// for it; otherwise the ladder's city key would read the claim unstarted.
func TestHookResidenceKeyIsExactOverAnotherStoresEntry(t *testing.T) {
	other := session.WorkStartedEntry{Store: startedStoreRef, Bead: residenceStep, Gen: 5, At: stepsTestNow, Via: session.WorkStartedViaClaim, Resident: true}
	fresh := session.Info{InstanceToken: "tok", Generation: "7", WorkStartedRaw: session.EncodeWorkStarted(session.WorkStarted{Claims: []session.WorkStartedEntry{other}})}
	res, patch, err := decideWorkStarted(fresh, hookStartedFence{Token: "tok", Gen: 7, HasGen: true},
		hookStartedKey{Store: "city", Bead: residenceStep, Resident: true}, session.WorkStartedViaClaim, stepsTestNow)
	if err != nil || res != hookStartedRecorded {
		t.Fatalf("decide = %v, %v; want the claim recorded", res, err)
	}
	cell, err := session.DecodeWorkStarted(patch[session.WorkStartedKey])
	if err != nil || len(cell.Claims) != 2 || cell.Claims[0] != (session.WorkStartedEntry{Store: "city", Bead: residenceStep, Gen: 7, At: stepsTestNow, Via: session.WorkStartedViaClaim, Resident: true}) {
		t.Fatalf("cell %+v, %v: want the residence recorded first, the other store's entry kept", cell.Claims, err)
	}
}
