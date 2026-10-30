package main

import (
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
	"github.com/gastownhall/gascity/internal/workladder"
)

// The ladder reads a binding-resident claim's work_started entry in either
// spelling (ga-ylpbsy.41): the residence the hook writes now, and the rig-leg
// spelling 8d6r4's hook wrote, at the row's generation only.

// bindingCensusRef is the census's ref for the relocated binding's legs: a
// binding-resident claim's AssignedStoreRef, which claimStoreRef folds to city.
var bindingCensusRef = string(storeref.ClassRef([]coordclass.Class{coordclass.ClassGraph, coordclass.ClassSessions}))

// bindingClaim adds an in_progress graph step held by the seat with the
// given id, which the planner's allocation lists under the binding's census
// ref. sessionID is the bead's gc.session_id: the seat's when its hook handed
// it over, "" when the snapshot was read before the stamp.
func (c *ladderCity) bindingClaim(id, sessionID string) {
	c.t.Helper()
	c.work.HonorExplicitIDs = true
	meta := map[string]string{beadmeta.RoutedToMetadataKey: "worker"}
	if sessionID != "" {
		meta[beadmeta.SessionIDMetadataKey] = sessionID
	}
	if _, err := c.work.Create(beads.Bead{ID: id, Title: "graph step " + id, Type: "task", Metadata: meta}); err != nil {
		c.t.Fatal(err)
	}
	inProgress := "in_progress"
	if err := c.work.Update(id, beads.UpdateOpts{Status: &inProgress, Assignee: &c.seat}); err != nil {
		c.t.Fatal(err)
	}
	c.claims = append(c.claims, id)
	c.lane.summary = func() *allocSummary {
		s := c.summary()
		for i := range s.AssignedStoreRefs {
			s.AssignedStoreRefs[i] = bindingCensusRef
		}
		return s
	}
}

// liveRow puts the seat in a live row's state: its generation, incarnation,
// work cells and current-claim stamp, at the moment the ladder nudged it.
func (c *ladderCity) liveRow(gen, workStarted, workLadder string, extra map[string]string) {
	c.t.Helper()
	kv := map[string]string{"generation": gen, "instance_token": "tok-" + gen, session.WorkStartedKey: workStarted, session.WorkLadderKey: workLadder}
	maps.Copy(kv, extra)
	c.setRow(kv)
}

// TestWorkLadderReadsTheLiveRowsLegSpelling replays the two live rows: a rig
// seat's 8d6r4 hook spelled its graph step rig:gascity, the ladder keyed the
// claim city at the same generation, read it unstarted and nudged it as
// claimed while the seat worked (gc.implementation-worker-7's shape got that
// nudge at 18:14:27Z). The unmarked entry at the row's generation names the
// claim: started on the agent's own evidence, from the first pass, and no
// claimed-rung nudge. Kills the store-exact reader.
func TestWorkLadderReadsTheLiveRowsLegSpelling(t *testing.T) {
	for _, live := range claimStoreRefLiveRows {
		t.Run(live.seat, func(t *testing.T) {
			c := newLadderCity(t)
			c.now = time.Date(2026, 10, 20, 18, 14, 27, 0, time.UTC)
			c.start = c.now
			c.sp.SetActivity(ladderSeatName, c.now.Add(-time.Hour))
			c.bindingClaim(live.bead, c.seat)
			c.liveRow(live.gen, live.workStarted, live.workLadder, map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: live.bead})
			c.pass()
			if e := c.entry(live.bead); e.Key.Store != "city" || e.Rung != workladder.RungStarted || e.Seed != workladder.SrcAgent {
				t.Fatalf("entry %+v, want city, started by the agent's own entry", e)
			}
			for _, n := range c.events(events.WorkNudged) {
				if strings.Contains(string(n.Payload), `"rung":"claimed"`) {
					t.Fatalf("a claimed-rung nudge to a started seat: %s", n.Payload)
				}
			}
		})
	}
}

// TestWorkLadderIgnoresALegSpellingFromAnotherGeneration (#8133's M1 shape):
// an unmarked entry recorded by an older incarnation proves nothing about
// this one; its leg spelling is not read as the binding claim, which stays
// claimed and is paced as claimed.
func TestWorkLadderIgnoresALegSpellingFromAnotherGeneration(t *testing.T) {
	c := newLadderCity(t)
	const bead = "gcg--9223372036837583987"
	c.bindingClaim(bead, "")
	cell := fmt.Sprintf(`{"v":1,"claims":[{"store":"rig:gascity","bead":%q,"gen":1,"at":"2026-10-07T11:00:00Z","via":"claim"}]}`, bead)
	ladder := workladder.EncodeLadder(workladder.Record{V: 1, Gen: 2, Entries: []workladder.Entry{{
		Key: workladder.ClaimKey{Store: "city", Bead: bead}, Kind: workladder.KindClaim, Rung: workladder.RungClaimed, Since: c.now.Add(-time.Minute),
	}}})
	c.liveRow("2", cell, ladder, nil)
	c.run(2*time.Minute, 30*time.Second, nil)
	if e := c.entry(bead); e.Rung != workladder.RungClaimed || e.Seed != 0 {
		t.Fatalf("entry %+v: an older incarnation's leg spelling started the claim", e)
	}
	if len(c.nudgedAt) == 0 {
		t.Fatal("the unstarted claim was not paced")
	}
}

// TestWorkLadderReadsAResidentEntryExactly: the hook's residence spelling is
// exact. city names the binding claim; another store's resident entry never
// does, at any generation.
func TestWorkLadderReadsAResidentEntryExactly(t *testing.T) {
	const bead = "gcg-100"
	for _, c := range []struct {
		store   string
		started bool
	}{
		{"city", true},
		{"rig:gascity", false},
	} {
		t.Run(c.store, func(t *testing.T) {
			lc := newLadderCity(t)
			lc.bindingClaim(bead, "")
			cell := session.EncodeWorkStarted(session.WorkStarted{}.With(session.WorkStartedEntry{Store: c.store, Bead: bead, Gen: 7, At: lc.now, Via: session.WorkStartedViaClaim, Resident: true}))
			lc.setRow(map[string]string{session.WorkStartedKey: cell, session.WorkLadderKey: workladder.EncodeLadder(workladder.Record{V: 1, Gen: 7})})
			lc.pass()
			if got := lc.entry(bead).Rung == workladder.RungStarted; got != c.started {
				t.Fatalf("resident %s entry: started=%v, want %v (entry %+v)", c.store, got, c.started, lc.entry(bead))
			}
		})
	}
}

// TestWorkLadderWritesNoReleaseOverALegSpelledStart (§6.4, the ladder's
// side): a claimed-rung entry due for release whose claim the hook recorded
// under its rig leg at this generation, with no seed and no gc.session_id
// stamp anywhere. The commit's CAS reads the agent's entry as naming the
// claim: no release is written, the claim is started. On 8d6r4 the CAS
// missed the entry, wrote the release, and the live read, finding no stamp,
// gave the started claim back.
func TestWorkLadderWritesNoReleaseOverALegSpelledStart(t *testing.T) {
	c := newLadderCity(t)
	const bead = "gcg-100"
	c.bindingClaim(bead, "")
	cell := fmt.Sprintf(`{"v":1,"claims":[{"store":"rig:gascity","bead":%q,"gen":7,"at":"2026-10-07T11:00:00Z","via":"claim"}]}`, bead)
	c.setRow(map[string]string{session.WorkStartedKey: cell, session.WorkLadderKey: workladder.EncodeLadder(workladder.Record{V: 1, Gen: 7, Entries: []workladder.Entry{{
		Key: workladder.ClaimKey{Store: "city", Bead: bead}, Kind: workladder.KindClaim, Rung: workladder.RungClaimed,
		Since: c.now.Add(-time.Hour), Attempts: 3, Last: c.now.Add(-time.Hour),
	}}})})
	c.pass()
	if b, _ := c.work.Get(bead); b.Status != "in_progress" || b.Assignee != c.seat {
		t.Fatalf("claim %q %q: a started claim was released", b.Status, b.Assignee)
	}
	if e := c.entry(bead); e.Rung != workladder.RungStarted || e.Seed != workladder.SrcAgent || c.cell().Release != nil {
		t.Fatalf("cell %s: want started by the agent, no release", c.row().Metadata[session.WorkLadderKey])
	}
}

// TestWorkLadderReleasesLiveReadIsTheLastGuard (the dependency commit's
// comment states): the snapshot was read before the hook stamped the claim's
// gc.session_id, so the CAS has no seed and writes the release. The release's
// live read finds the stamp: the claim is found started and the bead is not
// written at all.
func TestWorkLadderReleasesLiveReadIsTheLastGuard(t *testing.T) {
	c := newLadderCity(t)
	const bead = "gcg-100"
	c.bindingClaim(bead, c.seat) // stamped live
	snapshot := c.lane.summary
	c.lane.summary = func() *allocSummary { // read before the stamp
		s := snapshot()
		for i := range s.AssignedWork {
			s.AssignedWork[i].Metadata = maps.Clone(s.AssignedWork[i].Metadata)
			delete(s.AssignedWork[i].Metadata, beadmeta.SessionIDMetadataKey)
		}
		return s
	}
	c.setRow(map[string]string{session.WorkLadderKey: workladder.EncodeLadder(workladder.Record{V: 1, Gen: 7, Entries: []workladder.Entry{{
		Key: workladder.ClaimKey{Store: "city", Bead: bead}, Kind: workladder.KindClaim, Rung: workladder.RungClaimed,
		Since: c.now.Add(-time.Hour), Attempts: 3, Last: c.now.Add(-time.Hour),
	}}})})
	before, err := c.work.Get(bead)
	if err != nil {
		t.Fatal(err)
	}
	c.pass()
	after, err := c.work.Get(bead)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision || after.Status != "in_progress" {
		t.Fatalf("bead revision %d -> %d, status %q: the live read must keep a stamped claim without writing it", before.Revision, after.Revision, after.Status)
	}
	if e := c.entry(bead); e.Rung != workladder.RungStarted || e.Seed != workladder.SrcClaimSession {
		t.Fatalf("entry %+v, want found started by the live stamp", e)
	}
	if n := len(c.events(events.WorkReleased)); n != 0 {
		t.Fatalf("work.released = %d", n)
	}
}

// TestWorkLadderReadsTheLegSpellingAtTheRowsGeneration: the relaxation is
// read at the ROW's generation, never the ladder cell's. A cell left at an
// older generation (6, a recycle ago) beside an unmarked entry of that
// generation names nothing for the row at 7: with every seed stripped, the
// claim stays claimed.
func TestWorkLadderReadsTheLegSpellingAtTheRowsGeneration(t *testing.T) {
	c := newLadderCity(t)
	const bead = "gcg-100"
	c.bindingClaim(bead, "")
	cell := fmt.Sprintf(`{"v":1,"claims":[{"store":"rig:gascity","bead":%q,"gen":6,"at":"2026-10-07T11:00:00Z","via":"claim"}]}`, bead)
	ladder := workladder.EncodeLadder(workladder.Record{V: 1, Gen: 6, Entries: []workladder.Entry{{
		Key: workladder.ClaimKey{Store: "city", Bead: bead}, Kind: workladder.KindClaim, Rung: workladder.RungClaimed, Since: c.now.Add(-time.Minute),
	}}})
	c.setRow(map[string]string{session.WorkStartedKey: cell, session.WorkLadderKey: ladder})
	c.pass()
	if e := c.entry(bead); e.Rung != workladder.RungClaimed || e.Seed != 0 {
		t.Fatalf("entry %+v: the ladder cell's generation started the claim", e)
	}
}
