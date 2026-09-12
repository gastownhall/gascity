package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/pkg/eventexport"
)

// The seat-claim-backstop rows (ga-evxqd). Every backstop that came before this
// one either ends at the claim keyed on a bead the seat was BOUND to, or starts
// after the claim. The residual is the seat's OWN ready work sitting open and
// unclaimed: routed to the seat's identity, zero unmet dependencies, and never
// picked up, while the seat sits awake and quiet. These rows own the rule that
// it converges by re-nudging forever on a bounded cadence and that nothing here
// ever drains a seat over work it merely has not started.

type claimBackstopFixture struct {
	cfg      *config.City
	store    beads.Store
	depStore beads.Store // what the lane is handed; wraps store to inject read failures
	sp       *runtime.Fake
	session  beads.Bead
	work     beads.Bead
	rec      *events.Fake
	now      time.Time
	stdout   bytes.Buffer
	sessName string
	identity string
	partial  bool
}

// newClaimBackstopFixture seeds the ga-evxqd shape: a configured NAMED seat,
// awake and quiet, with one OPEN bead routed to its own identity and no
// assignee — the exact row the #374 pilot rerun left sitting for three hours.
func newClaimBackstopFixture(t *testing.T) *claimBackstopFixture {
	t.Helper()
	f := &claimBackstopFixture{
		sessName: "test-city--named-seat",
		identity: "named-seat",
		now:      time.Now().UTC(),
		rec:      events.NewFake(),
		sp:       runtime.NewFake(),
	}
	f.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:  "seat",
			Nudge: "gc hook --claim --drain-ack --json",
		}},
	}
	f.store = beads.NewMemStore()
	f.depStore = f.store
	session, err := f.store.Create(beads.Bead{
		Title:  "session",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"session_name":               f.sessName,
			"template":                   "seat",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: f.identity,
			"state":                      "active",
		},
	})
	if err != nil {
		t.Fatalf("seeding the session bead: %v", err)
	}
	f.session = session
	f.work = f.routedWork(t, "canonicalize input")
	if err := f.sp.Start(context.Background(), f.sessName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("starting the fake session: %v", err)
	}
	return f
}

// routedWork seeds an OPEN bead routed to the seat's identity with no assignee —
// the shape a dispatcher leaves behind for a named seat to claim.
func (f *claimBackstopFixture) routedWork(t *testing.T, title string) beads.Bead {
	t.Helper()
	work, err := f.store.Create(beads.Bead{
		Title: title,
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: "root-1",
			beadmeta.RoutedToMetadataKey:   f.identity,
		},
	})
	if err != nil {
		t.Fatalf("seeding the routed work bead: %v", err)
	}
	return work
}

// asPoolSeat re-stamps the seeded seat as an ordinary pool slot and re-routes
// its work onto the slot's own session name as an ASSIGNEE — the 09-10
// idle-killer flavor, where the slot holds its own open assigned bead and the
// trigger-bead-keyed pool lane cannot see it.
func (f *claimBackstopFixture) asPoolSeat(t *testing.T) {
	t.Helper()
	f.restamp(t, map[string]string{
		namedSessionMetadataKey:      "",
		namedSessionIdentityMetadata: "",
		"pool_managed":               "true",
	})
	f.assignWorkToTheSeat(t)
}

// assignWorkToTheSeat re-points the seeded row off gc.routed_to and onto the
// seat's session name as an ASSIGNEE, leaving the seat itself alone. Named seats
// own rows this way too: graphroute's SessionName and DirectSessionID branches
// bind a concrete session and set step.Assignee (ApplyGraphRouteBinding), and
// only the pool MetadataOnly branch leaves the row unassigned.
func (f *claimBackstopFixture) assignWorkToTheSeat(t *testing.T) {
	t.Helper()
	if err := f.store.SetMetadataBatch(f.work.ID, map[string]string{beadmeta.RoutedToMetadataKey: ""}); err != nil {
		t.Fatalf("clearing the routed marker: %v", err)
	}
	if err := f.store.Update(f.work.ID, beads.UpdateOpts{Assignee: &f.sessName}); err != nil {
		t.Fatalf("assigning the work bead to the seat: %v", err)
	}
	f.work = f.reread(t, f.work.ID)
}

// stampContinuation puts the preassigned-graph-v2-successor pair on the seat's
// row. A shared-context drain stamps exactly this on every executable item step
// (applySharedDrainContext, internal/dispatch/drain.go) whatever the step routes
// to, so the same shape reaches pool slots and named seats alike — which is why
// both continuation rows below stamp it identically and differ only in the seat.
func (f *claimBackstopFixture) stampContinuation(t *testing.T) {
	t.Helper()
	if err := f.store.SetMetadataBatch(f.work.ID, map[string]string{
		beadmeta.ContinuationGroupMetadataKey: "grp-shared-drain-1",
		beadmeta.SessionAffinityMetadataKey:   "require",
	}); err != nil {
		t.Fatalf("stamping the continuation metadata: %v", err)
	}
	f.work = f.reread(t, f.work.ID)
}

func (f *claimBackstopFixture) restamp(t *testing.T, meta map[string]string) {
	t.Helper()
	if err := f.store.SetMetadataBatch(f.session.ID, meta); err != nil {
		t.Fatalf("re-stamping the session bead: %v", err)
	}
	f.session = f.reread(t, f.session.ID)
}

func (f *claimBackstopFixture) reread(t *testing.T, id string) beads.Bead {
	t.Helper()
	current, err := f.store.Get(id)
	if err != nil {
		t.Fatalf("re-reading %s: %v", id, err)
	}
	return current
}

// tick runs one reconcile tick of the lane, assembling the two work snapshots
// exactly as the reconciler does: the assignee-keyed assigned-work triple and
// the open/unassigned/routed triple.
func (f *claimBackstopFixture) tick(t *testing.T) {
	t.Helper()
	sessions, err := loadSessionBeads(f.store)
	if err != nil {
		t.Fatalf("loading session beads: %v", err)
	}
	all, err := f.store.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("listing work: %v", err)
	}
	var assigned, routed []beads.Bead
	var assignedStores, routedStores []beads.Store
	var assignedRefs, routedRefs []string
	for _, b := range all {
		if b.Type == sessionBeadType {
			continue
		}
		switch {
		case strings.TrimSpace(b.Assignee) != "":
			assigned = append(assigned, b)
			assignedStores = append(assignedStores, f.depStore)
			assignedRefs = append(assignedRefs, "city")
		case strings.EqualFold(strings.TrimSpace(b.Status), "open") &&
			strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]) != "":
			routed = append(routed, b)
			routedStores = append(routedStores, f.depStore)
			routedRefs = append(routedRefs, "city")
		}
	}
	nudgeStalledSeatClaims(
		f.sp, f.cfg, f.store, sessions,
		assigned, assignedStores, assignedRefs,
		routed, routedStores, routedRefs,
		f.partial, f.now, f.rec, &f.stdout,
	)
}

// idleFor backdates the runtime's last-activity so the lane observes a seat that
// has done nothing for d.
func (f *claimBackstopFixture) idleFor(t *testing.T, d time.Duration) {
	t.Helper()
	f.sp.SetActivity(f.sessName, f.now.Add(-d))
}

// advance moves past one full backoff step with the seat still quiet.
func (f *claimBackstopFixture) advance(t *testing.T) {
	t.Helper()
	f.now = f.now.Add(idleClaimNudgeGrace + idleClaimNudgeBackoff)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
}

func (f *claimBackstopFixture) sessionMeta(t *testing.T, key string) string {
	t.Helper()
	return strings.TrimSpace(f.reread(t, f.session.ID).Metadata[key])
}

func (f *claimBackstopFixture) nudgeCount() int {
	return strings.Count(f.stdout.String(), "seat-claim-nudge: nudged")
}

// rotations counts the probe-window rotations the lane reported on stdout. Each
// one is also a session-bead write, so this doubles as the write-cost meter.
func (f *claimBackstopFixture) rotations() int {
	return strings.Count(f.stdout.String(), "rotating the readiness probe window")
}

func (f *claimBackstopFixture) stalledEvents() int {
	n := 0
	for _, e := range f.rec.Events {
		if e.Type == events.ExecutionClaimStalled {
			n++
		}
	}
	return n
}

func (f *claimBackstopFixture) lastNudge() string {
	msg := ""
	for _, call := range f.sp.SnapshotCalls() {
		if call.Method == "Nudge" && call.Name == f.sessName {
			msg = call.Message
		}
	}
	return msg
}

// blockOn adds an unmet plain `blocks` edge from the seat's work onto a fresh
// open blocker, and returns the blocker so a row can later satisfy it.
func (f *claimBackstopFixture) blockOn(t *testing.T) beads.Bead {
	t.Helper()
	return f.blockBead(t, f.work.ID)
}

// blockBead is blockOn for any of the seat's rows.
func (f *claimBackstopFixture) blockBead(t *testing.T, id string) beads.Bead {
	t.Helper()
	blocker, err := f.store.Create(beads.Bead{Title: "blocker for " + id, Type: "task"})
	if err != nil {
		t.Fatalf("seeding the blocker: %v", err)
	}
	if err := f.store.DepAdd(id, blocker.ID, "blocks"); err != nil {
		t.Fatalf("adding the blocking edge: %v", err)
	}
	return blocker
}

// routedWorkWithID seeds another OPEN row routed to the seat under an explicit
// bead id. The lane orders a seat's own rows by (store ref, bead id), so pinning
// the id is how a test pins where a row sorts in that queue.
func (f *claimBackstopFixture) routedWorkWithID(t *testing.T, id, title string) beads.Bead {
	t.Helper()
	mem, ok := f.store.(*beads.MemStore)
	if !ok {
		t.Fatalf("fixture store is %T, want *beads.MemStore to pin bead ids", f.store)
	}
	mem.HonorExplicitIDs = true
	work, err := f.store.Create(beads.Bead{
		ID:    id,
		Title: title,
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: "root-1",
			beadmeta.RoutedToMetadataKey:   f.identity,
		},
	})
	if err != nil {
		t.Fatalf("seeding routed work %s: %v", id, err)
	}
	return work
}

// TestSeatClaimBackstopNudgesANamedSeatOnItsOwnRoutedWork is the ga-evxqd core
// row. The seat is awake and quiet, the bead is open, routed to the seat's own
// identity, unassigned, and has zero unmet dependencies — and nothing in the
// fleet re-delivers a claim nudge for it. The lane observes first, then delivers
// exactly one nudge once the grace window elapses, with the attempt durably
// reserved before delivery.
func TestSeatClaimBackstopNudgesANamedSeatOnItsOwnRoutedWork(t *testing.T) {
	f := newClaimBackstopFixture(t)
	f.idleFor(t, 10*time.Minute)

	f.tick(t) // first sighting: start the grace clock, do not nudge
	if got := f.nudgeCount(); got != 0 {
		t.Fatalf("nudges after the first sighting = %d, want 0 (observe first)", got)
	}
	if got := f.sessionMeta(t, seatClaimNudgeWorkKey); got != f.work.ID {
		t.Fatalf("persisted work marker = %q, want %q", got, f.work.ID)
	}

	f.now = f.now.Add(idleClaimNudgeGrace + time.Second)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	if got := f.nudgeCount(); got != 1 {
		t.Fatalf("nudges after the grace = %d, want exactly 1; stdout=%s", got, f.stdout.String())
	}
	if got := f.sessionMeta(t, seatClaimNudgeCountKey); got != "1" {
		t.Fatalf("persisted attempt count = %q, want 1", got)
	}
	if got := f.lastNudge(); got != f.cfg.Agents[0].Nudge {
		t.Fatalf("delivered nudge text = %q, want the seat's configured claim nudge %q", got, f.cfg.Agents[0].Nudge)
	}

	// Inside the backoff nothing else is delivered.
	f.now = f.now.Add(time.Second)
	f.tick(t)
	if got := f.nudgeCount(); got != 1 {
		t.Fatalf("nudges inside the backoff = %d, want still 1", got)
	}
}

// Control A: a WORKING seat. The runtime reports recent activity, so the lane
// holds — zero nudges and, just as importantly, zero writes, because a backstop
// that marks a busy session is one that will eventually nudge it (the #312 churn
// failure this shape exists to avoid).
func TestSeatClaimBackstopIsSilentForAWorkingSeat(t *testing.T) {
	f := newClaimBackstopFixture(t)

	for i := 0; i < 5; i++ {
		f.idleFor(t, time.Second)
		f.tick(t)
		f.now = f.now.Add(idleClaimNudgeGrace + idleClaimNudgeBackoff)
	}

	if got := f.nudgeCount(); got != 0 {
		t.Fatalf("nudges to a working seat = %d, want 0; stdout=%s", got, f.stdout.String())
	}
	if got := f.sessionMeta(t, seatClaimNudgeWorkKey); got != "" {
		t.Fatalf("persisted work marker = %q, want no write at all for a working seat", got)
	}
	if got := f.sessionMeta(t, seatClaimNudgeAtKey); got != "" {
		t.Fatalf("persisted timestamp = %q, want no write at all for a working seat", got)
	}
}

// Control B: an UNKNOWN activity signal is not idleness. A runtime that reports
// no activity at all for the seat must hold, never nudge — the one-line
// statement of the #312 lesson in the one place it applies.
func TestSeatClaimBackstopHoldsWhenActivityIsUnknown(t *testing.T) {
	f := newClaimBackstopFixture(t)

	// No SetActivity call at all: the fake reports a zero time.
	for i := 0; i < 4; i++ {
		f.tick(t)
		f.now = f.now.Add(idleClaimNudgeGrace + idleClaimNudgeBackoff)
	}

	if got := f.nudgeCount(); got != 0 {
		t.Fatalf("nudges on an unknown activity signal = %d, want 0; stdout=%s", got, f.stdout.String())
	}
	if got := f.sessionMeta(t, seatClaimNudgeWorkKey); got != "" {
		t.Fatalf("persisted work marker = %q, want no write on an unknown activity signal", got)
	}
}

// TestSeatClaimBackstopYieldsWhileTheSeatHoldsAClaim is the no-double-nudge
// boundary with the execution backstop (#6287), and it has to be set up with a
// SECOND row to mean anything: a seat whose only bead went in_progress has no
// open work left, so it would fall silent here even with no boundary at all.
// The real collision is a seat that holds a claim AND has more of its own work
// queued behind it — the execution lane is already nudging that seat about the
// claim, and this lane matching the queued row would deliver two nudges per tick
// to one seat. An in_progress row hands the whole seat to that lane.
func TestSeatClaimBackstopYieldsWhileTheSeatHoldsAClaim(t *testing.T) {
	f := newClaimBackstopFixture(t)

	// The seat claims its first bead...
	inProgress := "in_progress"
	if err := f.store.Update(f.work.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &f.sessName}); err != nil {
		t.Fatalf("claiming the work bead: %v", err)
	}
	// ...and a second row is routed to it while it holds that claim.
	queued := f.routedWork(t, "queued input")

	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	for i := 0; i < idleClaimNudgeMaxAttempts+3; i++ {
		f.advance(t)
	}

	if got := f.nudgeCount(); got != 0 {
		t.Fatalf("nudges while the seat holds a claim = %d, want 0 (the execution backstop owns it); stdout=%s", got, f.stdout.String())
	}
	if got := f.stalledEvents(); got != 0 {
		t.Fatalf("stalled events while the seat holds a claim = %d, want 0", got)
	}
	if got := f.sessionMeta(t, seatClaimNudgeWorkKey); got != "" {
		t.Fatalf("persisted work marker = %q, want no pacing write while another lane owns this seat", got)
	}

	// The claim completes. The queued row is now this lane's, which is what makes
	// the silence above a HANDOFF rather than the lane being blind to the seat.
	if err := f.store.Close(f.work.ID); err != nil {
		t.Fatalf("completing the claim: %v", err)
	}
	f.advance(t) // first sighting of the queued row: observe
	if got := f.sessionMeta(t, seatClaimNudgeWorkKey); got != queued.ID {
		t.Fatalf("work marker after the claim completed = %q, want the queued row %q", got, queued.ID)
	}
	f.advance(t)
	if got := f.nudgeCount(); got != 1 {
		t.Fatalf("nudges after the claim completed = %d, want exactly 1; stdout=%s", got, f.stdout.String())
	}
}

// TestSeatClaimBackstopClearsWhenTheWorkGoesAway is the completion row: the bead
// closes, the lane wipes its pacing state so the seat's next assignment starts a
// fresh window, and nothing is ever delivered.
func TestSeatClaimBackstopClearsWhenTheWorkGoesAway(t *testing.T) {
	f := newClaimBackstopFixture(t)
	f.idleFor(t, 10*time.Minute)
	f.tick(t) // observe
	if got := f.sessionMeta(t, seatClaimNudgeWorkKey); got != f.work.ID {
		t.Fatalf("grace clock did not start: work marker = %q, want %q", got, f.work.ID)
	}

	if err := f.store.Close(f.work.ID); err != nil {
		t.Fatalf("closing the work bead: %v", err)
	}
	f.advance(t)

	if got := f.nudgeCount(); got != 0 {
		t.Fatalf("nudges after the work closed = %d, want 0; stdout=%s", got, f.stdout.String())
	}
	if got := f.sessionMeta(t, seatClaimNudgeWorkKey); got != "" {
		t.Fatalf("persisted work marker = %q, want cleared once nothing is outstanding", got)
	}
}

// TestSeatClaimBackstopHoldsWhileAHumanIsAttached is the never-act-under-a-
// human's-hands guard. A named interactive seat is exactly the session an
// operator attaches to and drives by hand; a nudge would inject keystrokes into
// that terminal. The lane must hold — no nudge, no pacing write — for as long as
// the attach lasts, then resume its ordinary cadence once the human detaches.
// The resume phase is what makes the first half non-vacuous: "no nudge, no
// marker" is also what a lane that never governed this seat looks like.
func TestSeatClaimBackstopHoldsWhileAHumanIsAttached(t *testing.T) {
	f := newClaimBackstopFixture(t)
	f.sp.SetAttached(f.sessName, true)

	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	for i := 0; i < idleClaimNudgeMaxAttempts+2; i++ {
		f.advance(t)
	}
	if got := f.nudgeCount(); got != 0 {
		t.Fatalf("nudges to an attached seat = %d, want 0; stdout=%s", got, f.stdout.String())
	}
	if got := f.sessionMeta(t, seatClaimNudgeWorkKey); got != "" {
		t.Fatalf("persisted work marker = %q, want no pacing write while attached", got)
	}

	f.sp.SetAttached(f.sessName, false)
	f.idleFor(t, 10*time.Minute)
	f.tick(t) // first sighting of the now-unattended seat
	if got := f.sessionMeta(t, seatClaimNudgeWorkKey); got != f.work.ID {
		t.Fatalf("work marker after the human detached = %q, want the grace clock started on %q", got, f.work.ID)
	}
	f.now = f.now.Add(idleClaimNudgeGrace + time.Second)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	if got := f.nudgeCount(); got != 1 {
		t.Fatalf("nudges after the human detached = %d, want exactly 1 (the cadence resumes); stdout=%s", got, f.stdout.String())
	}
}

// TestSeatClaimBackstopIgnoresWorkWithAnUnmetDependency pins the readiness half
// of the predicate on REAL dependency edges rather than bd's denormalized
// is_blocked projection, which BdStore leaves unset on every read. A bead the
// seat cannot claim yet is not a stall, and nudging over one would train seats
// to ignore the nudge. Satisfying the blocker then flips the same fixture to a
// delivered nudge, so the silence above is proved to be the dependency and not
// the lane being inert.
func TestSeatClaimBackstopIgnoresWorkWithAnUnmetDependency(t *testing.T) {
	f := newClaimBackstopFixture(t)
	blocker := f.blockOn(t)

	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	for i := 0; i < idleClaimNudgeMaxAttempts+1; i++ {
		f.advance(t)
	}
	if got := f.nudgeCount(); got != 0 {
		t.Fatalf("nudges over a dependency-blocked bead = %d, want 0; stdout=%s", got, f.stdout.String())
	}
	if got := f.sessionMeta(t, seatClaimNudgeWorkKey); got != "" {
		t.Fatalf("persisted work marker = %q, want no pacing write for unclaimable work", got)
	}

	// The blocker closes. The identical fixture now nudges.
	if err := f.store.Close(blocker.ID); err != nil {
		t.Fatalf("closing the blocker: %v", err)
	}
	f.advance(t) // first sighting of the now-ready bead: observe
	if got := f.sessionMeta(t, seatClaimNudgeWorkKey); got != f.work.ID {
		t.Fatalf("work marker after the blocker closed = %q, want %q", got, f.work.ID)
	}
	f.advance(t)
	if got := f.nudgeCount(); got != 1 {
		t.Fatalf("nudges after the blocker closed = %d, want exactly 1; stdout=%s", got, f.stdout.String())
	}
}

// depFailingStore fails every DepList, modeling a store outage on the readiness
// read. It is the fail-closed control: an unreadable dependency view is not
// proof that a bead is claimable.
type depFailingStore struct {
	beads.Store
	err error
}

func (s depFailingStore) DepList(string, string) ([]beads.Dep, error) { return nil, s.err }

// TestSeatClaimBackstopHoldsWhenTheDependencyReadFails is the fail-closed row.
// A dependency read that errors is not evidence of readiness, so the lane must
// deliver nothing and write nothing rather than manufacture a stall out of an
// unreadable store.
func TestSeatClaimBackstopHoldsWhenTheDependencyReadFails(t *testing.T) {
	f := newClaimBackstopFixture(t)
	f.depStore = depFailingStore{Store: f.store, err: context.DeadlineExceeded}

	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	for i := 0; i < idleClaimNudgeMaxAttempts+1; i++ {
		f.advance(t)
	}

	if got := f.nudgeCount(); got != 0 {
		t.Fatalf("nudges on an unreadable dependency view = %d, want 0; stdout=%s", got, f.stdout.String())
	}
	if got := f.sessionMeta(t, seatClaimNudgeWorkKey); got != "" {
		t.Fatalf("persisted work marker = %q, want no pacing write on an unreadable dependency view", got)
	}
}

// TestSeatClaimBackstopIgnoresUnclaimableRows pins the rest of the claimability
// predicate, one row per reason a bead is present but not the seat's to start.
// Each is a bead the seat is CORRECT to be sitting next to, so nudging over it
// would be a false alarm on every tick forever.
func TestSeatClaimBackstopIgnoresUnclaimableRows(t *testing.T) {
	deferUntil := time.Now().UTC().Add(24 * time.Hour)
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, f *claimBackstopFixture)
	}{
		{"deferred until a future time", func(t *testing.T, f *claimBackstopFixture) {
			// defer_until is settable only at create time on this store, so
			// retire the seeded row and stand up its deferred twin.
			if err := f.store.Close(f.work.ID); err != nil {
				t.Fatalf("retiring the seeded work bead: %v", err)
			}
			deferred, err := f.store.Create(beads.Bead{
				Title:      "deferred input",
				Type:       "task",
				DeferUntil: &deferUntil,
				Metadata:   map[string]string{beadmeta.RoutedToMetadataKey: f.identity},
			})
			if err != nil {
				t.Fatalf("seeding the deferred work bead: %v", err)
			}
			f.work = deferred
		}},
		{"paused by a hold label", func(t *testing.T, f *claimBackstopFixture) {
			if err := f.store.Update(f.work.ID, beads.UpdateOpts{Labels: []string{"hold:external"}}); err != nil {
				t.Fatalf("hold-labeling the work bead: %v", err)
			}
		}},
		{"a drain control step", func(t *testing.T, f *claimBackstopFixture) {
			if err := f.store.SetMetadataBatch(f.work.ID, map[string]string{
				beadmeta.KindMetadataKey: beadmeta.KindDrain,
			}); err != nil {
				t.Fatalf("stamping the drain kind: %v", err)
			}
		}},
		{"a workflow topology root", func(t *testing.T, f *claimBackstopFixture) {
			if err := f.store.SetMetadataBatch(f.work.ID, map[string]string{
				beadmeta.KindMetadataKey: beadmeta.KindWorkflow,
			}); err != nil {
				t.Fatalf("stamping the workflow kind: %v", err)
			}
		}},
		{"a session-labeled bead", func(t *testing.T, f *claimBackstopFixture) {
			if err := f.store.Update(f.work.ID, beads.UpdateOpts{Labels: []string{"gc:session"}}); err != nil {
				t.Fatalf("labeling the work bead: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newClaimBackstopFixture(t)
			tc.mutate(t, f)

			f.idleFor(t, 10*time.Minute)
			f.tick(t)
			for i := 0; i < idleClaimNudgeMaxAttempts+1; i++ {
				f.advance(t)
			}

			if got := f.nudgeCount(); got != 0 {
				t.Fatalf("nudges over %s = %d, want 0; stdout=%s", tc.name, got, f.stdout.String())
			}
			if got := f.sessionMeta(t, seatClaimNudgeWorkKey); got != "" {
				t.Fatalf("persisted work marker = %q, want no pacing write over %s", got, tc.name)
			}
		})
	}
}

// TestSeatClaimBackstopReNudgesForeverAndReportsOnce is the escalation contract,
// and it is deliberately NOT the execution backstop's. Nothing is stranded
// in_progress here: the seat merely has not started work it still owns, so a
// drain would kill a live seat over work it could still do. The lane therefore
// keeps re-nudging on the same bounded cadence indefinitely, and turns the first
// exhausted budget into exactly ONE typed observability event — re-arming rather
// than latching silent, which is the one-shot-latch failure this shape avoids.
func TestSeatClaimBackstopReNudgesForeverAndReportsOnce(t *testing.T) {
	f := newClaimBackstopFixture(t)
	f.idleFor(t, 10*time.Minute)
	f.tick(t) // observe

	for i := 0; i < idleClaimNudgeMaxAttempts; i++ {
		f.advance(t)
	}
	if got := f.nudgeCount(); got != idleClaimNudgeMaxAttempts {
		t.Fatalf("nudges before exhaustion = %d, want the attempt cap %d; stdout=%s", got, idleClaimNudgeMaxAttempts, f.stdout.String())
	}
	if got := f.stalledEvents(); got != 0 {
		t.Fatalf("stalled events before exhaustion = %d, want 0", got)
	}

	// The budget is spent: report once, then re-arm.
	f.advance(t)
	if got := f.stalledEvents(); got != 1 {
		t.Fatalf("stalled events at exhaustion = %d, want exactly 1; stdout=%s", got, f.stdout.String())
	}

	// And the lane keeps going. Two more full ladders must deliver two more
	// full rounds of nudges and never a second event for the same bead.
	before := f.nudgeCount()
	for i := 0; i < 2*(idleClaimNudgeMaxAttempts+1); i++ {
		f.advance(t)
	}
	if got := f.nudgeCount(); got <= before {
		t.Fatalf("nudges after exhaustion = %d, want more than %d (the lane re-arms, it does not latch silent); stdout=%s", got, before, f.stdout.String())
	}
	if got := f.stalledEvents(); got != 1 {
		t.Fatalf("stalled events after re-arming = %d, want still exactly 1 for the same bead", got)
	}

	// The escalation names the bead and the seat, so an operator can act on it.
	var stalled events.Event
	for _, e := range f.rec.Events {
		if e.Type == events.ExecutionClaimStalled {
			stalled = e
		}
	}
	if stalled.Subject != f.work.ID {
		t.Fatalf("event subject = %q, want the unclaimed bead %q", stalled.Subject, f.work.ID)
	}
	if stalled.SessionID != f.session.ID {
		t.Fatalf("event session = %q, want the seat's session bead %q", stalled.SessionID, f.session.ID)
	}
}

// TestSeatClaimBackstopFallsBackToTheDefaultNudge pins the same fallback the
// claim and execution lanes already carry. The named seats this lane rescues
// configure no [agent] nudge at all, so reading the raw configured value would
// make the lane inert for exactly the population it exists for.
func TestSeatClaimBackstopFallsBackToTheDefaultNudge(t *testing.T) {
	f := newClaimBackstopFixture(t)
	f.cfg.Agents[0].Nudge = ""

	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	f.advance(t)

	if got := f.nudgeCount(); got != 1 {
		t.Fatalf("default-nudge deliveries = %d, want exactly 1; stdout=%s", got, f.stdout.String())
	}
	if got := f.lastNudge(); got != defaultPoolClaimNudge {
		t.Fatalf("delivered nudge text = %q, want the default %q", got, defaultPoolClaimNudge)
	}
}

// TestSeatClaimBackstopCoversAPoolSeatsOwnAssignedWork is the 09-10 idle-killer
// flavor. poolClaimBackstop keys on the slot's gc.trigger_bead_id, so a slot
// holding its OWN open assigned bead with no trigger binding is invisible to it
// — the 22h-idle reviewers. The same predicate covers it here, because "the
// seat's own open work" is an identity question, not a binding one.
func TestSeatClaimBackstopCoversAPoolSeatsOwnAssignedWork(t *testing.T) {
	f := newClaimBackstopFixture(t)
	f.asPoolSeat(t)

	f.idleFor(t, 10*time.Minute)
	f.tick(t) // observe
	if got := f.sessionMeta(t, seatClaimNudgeWorkKey); got != f.work.ID {
		t.Fatalf("grace clock did not start for a pool seat: work marker = %q, want %q", got, f.work.ID)
	}
	f.advance(t)
	if got := f.nudgeCount(); got != 1 {
		t.Fatalf("nudges to a pool seat on its own assigned open bead = %d, want exactly 1; stdout=%s", got, f.stdout.String())
	}
}

// TestSeatClaimBackstopLeavesTheBoundTriggerBeadToThePoolLane is the other
// no-double-nudge boundary. A pool slot's bound trigger bead is already
// nudgeStalledPoolClaims' target on exactly this cadence; matching it here too
// would deliver two nudges per tick to the same slot for the same bead.
func TestSeatClaimBackstopLeavesTheBoundTriggerBeadToThePoolLane(t *testing.T) {
	f := newClaimBackstopFixture(t)
	f.asPoolSeat(t)
	f.restamp(t, map[string]string{
		beadmeta.TriggerBeadIDMetadataKey:       f.work.ID,
		beadmeta.TriggerBeadStoreRefMetadataKey: "city",
	})

	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	for i := 0; i < idleClaimNudgeMaxAttempts+1; i++ {
		f.advance(t)
	}

	if got := f.nudgeCount(); got != 0 {
		t.Fatalf("nudges over the slot's bound trigger bead = %d, want 0 (the pool lane owns it); stdout=%s", got, f.stdout.String())
	}
	if got := f.sessionMeta(t, seatClaimNudgeWorkKey); got != "" {
		t.Fatalf("persisted work marker = %q, want no pacing write over the bound trigger bead", got)
	}
}

// TestSeatClaimBackstopLeavesPreassignedContinuationRowsToTheContinuationLane is
// the third no-double-nudge boundary, and the one that costs the most to get
// wrong. A preassigned graph-v2 successor — open, assigned to the seat, carrying
// gc.continuation_group with gc.session_affinity=require — is
// nudgeStalledPoolContinuations' candidate shape verbatim, and it is the most
// common handoff row a graph-v2 molecule produces. Matching it here would put
// two independent persisted ladders on one bead; and because that lane latches
// silent at its three-attempt cap while this one re-arms forever, this lane
// would go on re-nudging past the point the continuation lane deliberately
// stopped — overriding another lane's designed give-up, not just doubling its
// nudges.
//
// The second half is what makes the first non-vacuous: drop the continuation
// stamp and the identical fixture nudges, so the silence above is the exclusion
// rather than an inert lane.
func TestSeatClaimBackstopLeavesPreassignedContinuationRowsToTheContinuationLane(t *testing.T) {
	f := newClaimBackstopFixture(t)
	f.asPoolSeat(t)
	f.stampContinuation(t)

	// The pool seat is the whole premise: poolContinuationBackstop.governs takes
	// pool slots and nothing else, so this is the only seat class where the two
	// lanes can collide and the only one where deferring reaches anybody.
	if !(poolContinuationBackstop{}).governs(f.session) {
		t.Fatalf("fixture seat is not pool-managed, so the continuation lane never serves this row and deferring to it would strand the row")
	}

	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	for i := 0; i < idleClaimNudgeMaxAttempts+2; i++ {
		f.advance(t)
	}

	if got := f.nudgeCount(); got != 0 {
		t.Fatalf("nudges over a preassigned continuation row = %d, want 0 (the continuation lane owns it); stdout=%s", got, f.stdout.String())
	}
	if got := f.sessionMeta(t, seatClaimNudgeWorkKey); got != "" {
		t.Fatalf("persisted work marker = %q, want no pacing write over a continuation row", got)
	}
	if got := f.stalledEvents(); got != 0 {
		t.Fatalf("stalled events over a continuation row = %d, want 0", got)
	}

	// The row loses its continuation group: it is nobody else's now, so it is
	// this lane's again.
	if err := f.store.SetMetadataBatch(f.work.ID, map[string]string{
		beadmeta.ContinuationGroupMetadataKey: "",
	}); err != nil {
		t.Fatalf("clearing the continuation group: %v", err)
	}
	f.advance(t) // first sighting of the now-ordinary row: observe
	if got := f.sessionMeta(t, seatClaimNudgeWorkKey); got != f.work.ID {
		t.Fatalf("work marker after the continuation group cleared = %q, want %q", got, f.work.ID)
	}
	f.advance(t)
	if got := f.nudgeCount(); got != 1 {
		t.Fatalf("nudges over the plain assigned row = %d, want exactly 1; stdout=%s", got, f.stdout.String())
	}
}

// TestSeatClaimBackstopServesAContinuationRowOnANamedSeat is the other side of
// that boundary, and the side a shape-only exclusion gets wrong.
// nudgeStalledPoolContinuations governs pool slots and nothing else, so the
// IDENTICAL row on a named seat is not its population at all: excluding it here
// defers to nobody, and the row is covered by neither lane — permanently silent
// with a claimable bead in front of it, which is the ga-evxqd symptom on the
// exact population this lane was built for.
//
// The shape is not exotic on a named seat. A shared-context drain stamps
// gc.continuation_group + gc.session_affinity=require on every executable item
// step whatever the step routes to (applySharedDrainContext,
// internal/dispatch/drain.go); graphroute's SessionName and DirectSessionID
// branches then assign the step to a concrete session while leaving that pair
// intact, since only the pool MetadataOnly branch rewrites it
// (ApplyGraphRouteBinding); and preassignHookContinuationGroup pins a molecule's
// open siblings to whoever claims first on ANY gc hook --claim, named seats
// included.
//
// So the exclusion is scoped to the seats the continuation lane actually serves.
// Here there is no second ladder to collide with, and the double-nudge rationale
// that justifies the pool-side exclusion simply does not apply.
func TestSeatClaimBackstopServesAContinuationRowOnANamedSeat(t *testing.T) {
	f := newClaimBackstopFixture(t)
	f.assignWorkToTheSeat(t)
	f.stampContinuation(t)

	// Pinned from the other side: if the continuation lane ever widens to named
	// seats, this row stops being ours and the exclusion has to widen with it.
	if (poolContinuationBackstop{}).governs(f.session) {
		t.Fatalf("the continuation lane governs this named seat, so serving the row here would double-nudge it")
	}

	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	if got := f.sessionMeta(t, seatClaimNudgeWorkKey); got != f.work.ID {
		t.Fatalf("grace clock never started on the named seat's continuation row: work marker = %q, want %q; stdout=%s", got, f.work.ID, f.stdout.String())
	}

	f.advance(t)
	if got := f.nudgeCount(); got != 1 {
		t.Fatalf("nudges over a named seat's continuation row = %d, want exactly 1 (no other lane serves it); stdout=%s", got, f.stdout.String())
	}
	if got := f.lastNudge(); got != f.cfg.Agents[0].Nudge {
		t.Fatalf("delivered nudge = %q, want the seat's configured claim nudge %q", got, f.cfg.Agents[0].Nudge)
	}
}

// TestSeatClaimBackstopRotatesItsProbeWindowPastALongBlockedQueue is the
// probe-budget row, and it reproduces the ga-evxqd symptom INSIDE the lane that
// exists to cure it. Readiness costs a live dependency read per row, so one tick
// may only probe maxSeatClaimReadinessProbes of them. With a fixed prefix, a
// seat holding ten of its own open rows whose one ready row sorts last is probed
// over the same nine blocked rows on every tick forever: permanently silent,
// with a claimable bead sitting in front of it — the incident, recreated by the
// budget meant to bound it.
//
// The window rotates instead. The persisted cursor advances by one window only
// when the window came up empty, so the search reaches the tail on the next tick
// and then freezes on the row it found for the rest of the ladder. The spent
// budget also has to be AUDIBLE: before, it was indistinguishable on stdout from
// a seat with nothing ready.
func TestSeatClaimBackstopRotatesItsProbeWindowPastALongBlockedQueue(t *testing.T) {
	f := newClaimBackstopFixture(t)

	// Nine blocked rows sorting ahead of one ready row. The seeded row (a minted
	// "gc-<n>" id) sorts first; "gc-b*" fill the rest of the prefix and "gc-z1"
	// is the tail the fixed-prefix probe could never reach.
	f.blockOn(t)
	for i := 1; i <= 8; i++ {
		blocked := f.routedWorkWithID(t, fmt.Sprintf("gc-b%d", i), "blocked input")
		f.blockBead(t, blocked.ID)
	}
	ready := f.routedWorkWithID(t, "gc-z1", "the one ready row, sorting last")

	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	if got := f.nudgeCount(); got != 0 {
		t.Fatalf("nudges on the first tick = %d, want 0 (the window is all blocked rows)", got)
	}
	if got := f.rotations(); got != 1 {
		t.Fatalf("rotations after a spent probe budget = %d, want 1 signal on stdout; stdout=%s", got, f.stdout.String())
	}

	// Second tick: the rotated window reaches the tail and starts its clock.
	f.advance(t)
	if got := f.sessionMeta(t, seatClaimNudgeWorkKey); got != ready.ID {
		t.Fatalf("work marker after one rotation = %q, want the ready tail row %q; stdout=%s", got, ready.ID, f.stdout.String())
	}

	// Third tick: the window has frozen on that row, so the ladder proceeds.
	f.advance(t)
	if got := f.nudgeCount(); got != 1 {
		t.Fatalf("nudges after the window reached the ready row = %d, want exactly 1; stdout=%s", got, f.stdout.String())
	}
	if got := f.sessionMeta(t, seatClaimNudgeWorkKey); got != ready.ID {
		t.Fatalf("work marker after the nudge = %q, want the window to stay on %q", got, ready.ID)
	}
}

// TestSeatClaimBackstopStillReportsAbsenceWhenTheWindowCoversEveryRow is the
// rotation's control. Rotation must not turn "this seat owns nothing claimable"
// into a permanent hold: a queue the window covers whole is still definite
// evidence of absence, and the lane must clear its pacing state on it.
func TestSeatClaimBackstopStillReportsAbsenceWhenTheWindowCoversEveryRow(t *testing.T) {
	f := newClaimBackstopFixture(t)
	f.idleFor(t, 10*time.Minute)
	f.tick(t) // observe the seeded ready row
	if got := f.sessionMeta(t, seatClaimNudgeWorkKey); got != f.work.ID {
		t.Fatalf("grace clock did not start: work marker = %q, want %q", got, f.work.ID)
	}

	f.blockOn(t) // the only row is now blocked, and the window sees all of it
	f.advance(t)

	if got := f.sessionMeta(t, seatClaimNudgeWorkKey); got != "" {
		t.Fatalf("persisted work marker = %q, want cleared once every candidate was probed and none was ready", got)
	}
	if got := f.rotations(); got != 0 {
		t.Fatalf("a fully-covered queue rotated its window %d times; stdout=%s", got, f.stdout.String())
	}
}

// TestSeatClaimBackstopPacesItsProbeWindowRotation bounds what the rotation
// costs when nothing is wrong. A seat whose blocked queue outruns the probe
// budget wants to rotate on EVERY patrol tick, and each rotation is a session-
// bead write: unpaced, that is a steady-state write per seat per tick for as
// long as the queue stays long — the unpaced-write class behind the
// cache-reconcile flood, produced here by a lane that is supposed to cost
// nothing while it waits.
//
// So the rotation rides the lane's own backoff clock. Ticks inside one window
// are free and silent; the first tick past it advances the cursor by exactly one
// window. The coverage guarantee survives unchanged — the windows still tile the
// whole ring, on the cadence the rest of this state machine already runs on.
func TestSeatClaimBackstopPacesItsProbeWindowRotation(t *testing.T) {
	f := newClaimBackstopFixture(t)

	// Ten of the seat's own rows, every one of them blocked, so the window never
	// finds a target and every tick has a reason to rotate.
	f.blockOn(t)
	for i := 1; i <= 9; i++ {
		blocked := f.routedWorkWithID(t, fmt.Sprintf("gc-b%d", i), "blocked input")
		f.blockBead(t, blocked.ID)
	}

	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	if got := f.rotations(); got != 1 {
		t.Fatalf("rotations on the first tick = %d, want 1; stdout=%s", got, f.stdout.String())
	}
	const firstCursor = "8" // one full window off a fresh cursor
	if got := f.sessionMeta(t, seatClaimProbeCursorKey); got != firstCursor {
		t.Fatalf("cursor after the first rotation = %q, want %q", got, firstCursor)
	}

	// Four more patrol ticks inside the same backoff window. The queue is just as
	// long and just as blocked, and none of them may write.
	for i := 0; i < 4; i++ {
		f.tick(t)
	}
	if got := f.rotations(); got != 1 {
		t.Fatalf("rotations across one backoff window = %d, want 1 (the rest are paced out); stdout=%s", got, f.stdout.String())
	}
	if got := f.sessionMeta(t, seatClaimProbeCursorKey); got != firstCursor {
		t.Fatalf("cursor moved inside one backoff window: %q -> %q", firstCursor, got)
	}

	// Past the window the ring keeps tiling.
	f.advance(t)
	if got := f.rotations(); got != 2 {
		t.Fatalf("rotations after the backoff window elapsed = %d, want 2; stdout=%s", got, f.stdout.String())
	}
	const secondCursor = "6" // (8 + 8) mod 10
	if got := f.sessionMeta(t, seatClaimProbeCursorKey); got != secondCursor {
		t.Fatalf("cursor after the second rotation = %q, want %q", got, secondCursor)
	}
}

// TestSeatClaimBackstopDoesNotReplayAfterAControllerRestart pins the persistence
// invariant (test-5il): the state machine lives on the session bead, so a fresh
// controller process resumes the ladder rather than restarting it. The in-memory
// grace map of the reverted #312 nudger is exactly what made that one storm.
func TestSeatClaimBackstopDoesNotReplayAfterAControllerRestart(t *testing.T) {
	f := newClaimBackstopFixture(t)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	f.advance(t)
	if got := f.nudgeCount(); got != 1 {
		t.Fatalf("nudges before the restart = %d, want 1; stdout=%s", got, f.stdout.String())
	}

	// A fresh controller: new provider, new stdout, same store.
	f.sp = runtime.NewFake()
	if err := f.sp.Start(context.Background(), f.sessName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("restarting the fake session: %v", err)
	}
	f.stdout.Reset()
	f.now = f.now.Add(time.Second)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)

	if got := f.nudgeCount(); got != 0 {
		t.Fatalf("nudges immediately after a controller restart = %d, want 0 (resume, never replay); stdout=%s", got, f.stdout.String())
	}
	if got := f.sessionMeta(t, seatClaimNudgeCountKey); got != "1" {
		t.Fatalf("persisted attempt count across the restart = %q, want the pre-restart 1", got)
	}
}

// TestSeatClaimBackstopIsSilentOnAPartialSnapshot: an incomplete work or session
// view makes a seat's own work look absent or a claimed bead look open. Neither
// is evidence, so the whole lane is disabled for that tick.
func TestSeatClaimBackstopIsSilentOnAPartialSnapshot(t *testing.T) {
	f := newClaimBackstopFixture(t)
	f.partial = true

	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	for i := 0; i < idleClaimNudgeMaxAttempts+1; i++ {
		f.advance(t)
	}

	if got := f.nudgeCount(); got != 0 {
		t.Fatalf("nudges on a partial snapshot = %d, want 0; stdout=%s", got, f.stdout.String())
	}
	if got := f.sessionMeta(t, seatClaimNudgeWorkKey); got != "" {
		t.Fatalf("persisted work marker = %q, want no pacing write on a partial snapshot", got)
	}
}

// TestSeatClaimBackstopGovernsTheSeatTaxonomy pins WHICH seats this lane covers,
// one row per shape, because the exclusions are the part that is easy to get
// backwards from the flag names alone.
//
// A manual seat is a human's own session, never an orchestration slot. A
// dependency floor is excluded here — unlike in the execution lane, where the
// floor holds a real claim nothing else will release: a floor is minted to
// satisfy someone else's dependency gate rather than to serve a work binding,
// so "open work matches its identity" is not evidence it was handed anything.
func TestSeatClaimBackstopGovernsTheSeatTaxonomy(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta map[string]string
		want bool
	}{
		{"pool slot", map[string]string{"pool_managed": "true"}, true},
		{"configured named seat", map[string]string{
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "named-seat",
		}, true},
		{"dependency floor pool slot", map[string]string{"pool_managed": "true", "dependency_only": "true"}, false},
		{"manual seat", map[string]string{"session_origin": "manual", "pool_managed": "true"}, false},
		{"legacy manual seat", map[string]string{"manual_session": boolMetadata(true), "pool_managed": "true"}, false},
		{"manual seat wearing named markers", map[string]string{
			"session_origin":             "manual",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "named-seat",
		}, false},
		{"neither pool-managed nor named", map[string]string{"template": "seat"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := seatClaimBackstop{}.governs(beads.Bead{Type: sessionBeadType, Metadata: tc.meta})
			if got != tc.want {
				t.Fatalf("governs(%v) = %v, want %v", tc.meta, got, tc.want)
			}
		})
	}
}

// TestSeatClaimBackstopIgnoresAnotherSeatsWork: identity resolution is the whole
// predicate, so a bead routed to somebody else must be invisible here however
// idle this seat is.
func TestSeatClaimBackstopIgnoresAnotherSeatsWork(t *testing.T) {
	f := newClaimBackstopFixture(t)
	if err := f.store.SetMetadataBatch(f.work.ID, map[string]string{
		beadmeta.RoutedToMetadataKey: "some-other-seat",
	}); err != nil {
		t.Fatalf("re-routing the work bead: %v", err)
	}

	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	for i := 0; i < idleClaimNudgeMaxAttempts+1; i++ {
		f.advance(t)
	}

	if got := f.nudgeCount(); got != 0 {
		t.Fatalf("nudges over another seat's work = %d, want 0; stdout=%s", got, f.stdout.String())
	}
}

// TestBeadReconcileTick_SeatClaimCallSite_CarriesTheOpenRoutedSnapshot is the
// production-wiring control for this lane, and the only test here that runs the
// REAL tick. Every row above calls nudgeStalledSeatClaims directly with a
// hand-built pair of work snapshots, so all of them stay green if the two seams
// that feed it in production come apart:
//
//   - buildDesiredState must publish the BROAD open/unassigned/routed triple on
//     DesiredStateResult (OpenRoutedWorkBeads/Stores/StoreRefs, populated from
//     collectOpenUnassignedRoutedWork). The narrowed ReadyUnassignedRoutedWork
//     view is pool-demand selection, which a named seat's routed work is not
//     part of, so it can be empty on exactly the rows this lane exists for.
//   - beadReconcileTick must hand that triple to nudgeStalledSeatClaims.
//
// Drop either — re-scope the field, or stop passing it at the call site — and
// this row goes red while the unit rows stay green. The assertion is the first
// observation the lane makes: the seat's grace clock started on its own routed
// bead, which it can only have learned from that snapshot.
func TestBeadReconcileTick_SeatClaimCallSite_CarriesTheOpenRoutedSnapshot(t *testing.T) {
	store := beads.NewMemStore()
	cityName := "seam-city"
	sessName := "seam-city--seat"
	identity := "seat-seam"

	session, err := store.Create(beads.Bead{
		Title:  "the named seat",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "agent:seat"},
		Metadata: map[string]string{
			"session_name":               sessName,
			"template":                   "seat",
			"agent_name":                 "seat",
			"state":                      "active",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: identity,
		},
	})
	if err != nil {
		t.Fatalf("seeding the session bead: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "open work routed to the seat, never claimed",
		Type:     "task",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: identity},
	})
	if err != nil {
		t.Fatalf("seeding the routed work bead: %v", err)
	}

	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), sessName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("starting the fake session: %v", err)
	}
	sp.SetActivity(sessName, time.Now().Add(-10*time.Minute))

	cfg := &config.City{
		Workspace: config.Workspace{Name: cityName},
		Agents: []config.Agent{{
			Name:  "seat",
			Nudge: "gc hook --claim --drain-ack --json",
		}},
	}
	cr := &CityRuntime{
		cityPath:            t.TempDir(),
		cityName:            cityName,
		cfg:                 cfg,
		sp:                  sp,
		standaloneCityStore: store,
		sessionDrains:       newDrainTracker(),
		rec:                 events.Discard,
		stdout:              io.Discard,
		stderr:              io.Discard,
	}

	result := buildDesiredState(cityName, cr.cityPath, time.Now(), cfg, sp, store, io.Discard)

	// Seam 1: the builder publishes the broad view, index-aligned.
	found := false
	for _, b := range result.OpenRoutedWorkBeads {
		if b.ID == work.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("OpenRoutedWorkBeads = %v, want the seat's routed row %s (collectOpenUnassignedRoutedWork is no longer published)", beadIDs(result.OpenRoutedWorkBeads), work.ID)
	}
	if len(result.OpenRoutedWorkStores) != len(result.OpenRoutedWorkBeads) ||
		len(result.OpenRoutedWorkStoreRefs) != len(result.OpenRoutedWorkBeads) {
		t.Fatalf("open-routed triple is not index-aligned: %d beads, %d stores, %d refs",
			len(result.OpenRoutedWorkBeads), len(result.OpenRoutedWorkStores), len(result.OpenRoutedWorkStoreRefs))
	}
	if result.OpenRoutedWorkQueryPartial {
		t.Fatal("OpenRoutedWorkQueryPartial on a healthy in-memory read; the lane would disable itself")
	}

	// Seam 2: the tick carries it to the lane.
	cr.beadReconcileTick(context.Background(), result, nil, nil, false)

	current, err := store.Get(session.ID)
	if err != nil {
		t.Fatalf("re-reading the session bead: %v", err)
	}
	if got := strings.TrimSpace(current.Metadata[seatClaimNudgeWorkKey]); got != work.ID {
		t.Fatalf("seat-claim marker after the real tick = %q, want the grace clock started on %q — the lane did not receive the open-routed snapshot", got, work.ID)
	}
}

// TestExecutionClaimStalledStaysOffTheExportAllowlist is the explicit egress
// decision, pinned rather than implied — the sibling of the execution lane's
// row. execution.claim_stalled is a controller liveness signal, not an execution
// FACT the export contract carries. Widening the default-deny allowlist is a
// separate, reviewable change; this row fails if it happens silently.
func TestExecutionClaimStalledStaysOffTheExportAllowlist(t *testing.T) {
	if eventexport.IsAllowed(events.ExecutionClaimStalled) {
		t.Fatal("execution.claim_stalled is on the redacted-export allowlist; that is an egress-surface change and needs its own review")
	}
}
