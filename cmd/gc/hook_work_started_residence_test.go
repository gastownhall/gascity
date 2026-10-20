package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
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
	route := newClaimRouteFor(t, newClaimRouteClassStore(t))
	for _, c := range []struct {
		name  string
		route *hookClaimClassRoute
		acked bool
	}{
		{"a rig seat with a class route", route, false},
		{"a rig seat on a city with no route", nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := &startedLog{}
			ops := l.ops(`[]`, nil, nil)
			ops.ReadWorkStarted = func(string) (string, error) { return cell, nil }
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

// TestForgetDropsALegSpelledEntryOfTheClaim: an unwind gives a claim back,
// and a released claim must not read as started. If the cell still holds an
// older hook's leg spelling of it at this generation (a re-spell the hook
// could not land), the forget drops that entry too; an older generation's
// leg spelling of the same bead is no record of this incarnation and stays.
func TestForgetDropsALegSpelledEntryOfTheClaim(t *testing.T) {
	cell := session.EncodeWorkStarted(session.WorkStarted{Claims: []session.WorkStartedEntry{
		{Store: startedStoreRef, Bead: residenceStep, Gen: 7, At: stepsTestNow, Via: session.WorkStartedViaClaim},
		{Store: startedStoreRef, Bead: "gcg-200", Gen: 6, At: stepsTestNow, Via: session.WorkStartedViaClaim},
	}})
	row := newStartedRow(t, map[string]string{session.WorkStartedKey: cell})
	for _, bead := range []string{residenceStep, "gcg-200"} {
		if err := forgetWorkStarted(row.front, row.id, hookStartedKey{Store: "city", Bead: bead, Resident: true}); err != nil {
			t.Fatal(err)
		}
	}
	got := row.cell(t)
	if len(got.Claims) != 1 || got.Claims[0].Bead != "gcg-200" || got.Claims[0].Gen != 6 {
		t.Fatalf("cell %+v: want this generation's leg spelling dropped, the older one kept", got.Claims)
	}
}
