package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
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
// binding when the route proved it, the work leg for a minted claim or a city
// with no route (both resident), the leg unproven for an adoption the route
// did not prove, and no store at all when the leg is unknown.
func TestHookStartedKeyForIsTheResidence(t *testing.T) {
	route := newClaimRouteFor(t, newClaimRouteClassStore(t))
	route.resident["gcg-1"] = true
	leg := func(string) string { return "rig:gascity" }
	for _, c := range []struct {
		name   string
		ops    hookClaimOps
		bead   string
		minted bool
		want   hookStartedKey
	}{
		{"binding claim, minted", hookClaimOps{StoreRef: leg, ClassRoute: route}, "gcg-1", true, hookStartedKey{Store: "city", Bead: "gcg-1", Resident: true}},
		{"binding claim, adopted", hookClaimOps{StoreRef: leg, ClassRoute: route}, "gcg-1", false, hookStartedKey{Store: "city", Bead: "gcg-1", Resident: true}},
		{"work-leg claim, minted on a split city", hookClaimOps{StoreRef: leg, ClassRoute: route}, "ga-1", true, hookStartedKey{Store: "rig:gascity", Bead: "ga-1", Resident: true}},
		{"work-leg claim, adopted on a split city", hookClaimOps{StoreRef: leg, ClassRoute: route}, "ga-1", false, hookStartedKey{Store: "rig:gascity", Bead: "ga-1"}},
		{"adopted on a city with no route", hookClaimOps{StoreRef: leg}, "ga-1", false, hookStartedKey{Store: "rig:gascity", Bead: "ga-1", Resident: true}},
		{"adopted on a city whose route degraded", hookClaimOps{StoreRef: leg, ClassesRelocated: true}, "gcg-1", false, hookStartedKey{Store: "rig:gascity", Bead: "gcg-1"}},
		{"minted on a city whose route degraded", hookClaimOps{StoreRef: leg, ClassesRelocated: true}, "ga-1", true, hookStartedKey{Store: "rig:gascity", Bead: "ga-1", Resident: true}},
		{"unknown leg", hookClaimOps{StoreRef: func(string) string { return "" }}, " ga-1 ", true, hookStartedKey{Bead: "ga-1"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := hookStartedKeyFor(c.ops, "/rig", c.bead, c.minted); got != c.want {
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

// TestUnwindForgetsEveryRecordOfTheBead: an unwind gives a claim back, and a
// released claim must not read as started under any spelling or generation:
// were the seat to claim it again, a stale record would make the new claim
// read started before any turn began it. The unwind forgets by bead, so it
// does not depend on the class route's memo still agreeing with the key the
// record wrote. Other beads' entries stay.
func TestUnwindForgetsEveryRecordOfTheBead(t *testing.T) {
	cell := session.EncodeWorkStarted(session.WorkStarted{Claims: []session.WorkStartedEntry{
		{Store: startedStoreRef, Bead: residenceStep, Gen: 7, At: stepsTestNow, Via: session.WorkStartedViaClaim},
		{Store: "city", Bead: residenceStep, Gen: 6, At: stepsTestNow, Via: session.WorkStartedViaClaim, Resident: true},
		{Store: startedStoreRef, Bead: "gcg-200", Gen: 6, At: stepsTestNow, Via: session.WorkStartedViaClaim},
	}})
	row := newStartedRow(t, map[string]string{session.WorkStartedKey: cell})
	l := &startedLog{}
	ops := l.ops(`[]`, row, nil)
	forgetHookWorkStarted(beads.Bead{ID: residenceStep}, startedOpts(startedEnv(startedToken, "7")), ops, "/rig")
	got := row.cell(t)
	if len(got.Claims) != 1 || got.Claims[0].Bead != "gcg-200" {
		t.Fatalf("cell %+v: want every record of %s forgotten and gcg-200 kept", got.Claims, residenceStep)
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

// TestHookUnprovenAdoptionRecordsNothingNew (the respell guard): an adoption
// whose residence the hook could not prove finds the claim already recorded.
// Over an older hook's leg spelling it writes nothing (it must not mark an
// unproven leg as the residence, which no reader keyed city could match);
// over a resident entry an earlier proven hand-over wrote, it adds no
// duplicate leg spelling.
func TestHookUnprovenAdoptionRecordsNothingNew(t *testing.T) {
	legacy := session.EncodeWorkStarted(session.WorkStarted{Claims: []session.WorkStartedEntry{{Store: startedStoreRef, Bead: residenceStep, Gen: 7, At: stepsTestNow, Via: session.WorkStartedViaClaim}}})
	proven := session.EncodeWorkStarted(session.WorkStarted{}.With(session.WorkStartedEntry{Store: "city", Bead: residenceStep, Gen: 7, At: stepsTestNow, Via: session.WorkStartedViaClaim, Resident: true}))
	for name, cell := range map[string]string{"an older hook's leg spelling": legacy, "a resident entry": proven} {
		t.Run(name, func(t *testing.T) {
			fresh := session.Info{InstanceToken: "tok", Generation: "7", WorkStartedRaw: cell}
			res, patch, err := decideWorkStarted(fresh, hookStartedFence{Token: "tok", Gen: 7, HasGen: true},
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
	if got := recordHookWorkStarted(beads.Bead{ID: residenceStep}, startedOpts(startedEnv(startedToken, "7")), ops, "/rig", false, &stderr); got != hookStartedAlreadyRecorded {
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
// that relocates (so an adoption there is no proven residence); a city that
// relocates nothing is neither; any other failure refuses the claim.
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
			key := hookStartedKeyFor(ops, "/rig", "gcg-9", false)
			if key.Resident == c.relocated {
				t.Fatalf("an adoption's key %+v on relocated=%v: want unmarked exactly when the city relocates", key, c.relocated)
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
