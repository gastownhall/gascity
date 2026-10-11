package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The started-work policy of every release path is one table.
func TestReleasePoliciesTable(t *testing.T) {
	want := map[releaseWhy]releasePolicy{
		releaseKilledSeat:     {statuses: []string{"open", "in_progress"}, startedKeeps: true, lane: laneDirect, seatRoute: true},
		releaseDrainAck:       {statuses: []string{"in_progress"}, lane: laneCarried},
		releaseCloseCascade:   {statuses: []string{"open", "in_progress"}, lane: laneCarried, seatRoute: true},
		releaseStrandedRepair: {statuses: []string{"open", "in_progress"}, lane: laneCarried, seatRoute: true},
		releaseOrphan:         {statuses: []string{"open", "in_progress"}, lane: laneDirect, pool: true},
		releaseRetired:        {statuses: []string{"open", "in_progress"}, lane: laneCarried, seatRoute: true},
	}
	if !reflect.DeepEqual(releasePolicies, want) {
		t.Fatalf("releasePolicies = %+v\nwant %+v", releasePolicies, want)
	}
}

// strandCase is one row of NEW-5's strand table (mc-zndi7.87): a claim no
// lane would serve once released, except "served", the control.
type strandCase struct {
	name     string
	meta     map[string]string
	released bool
}

var strandCases = []strandCase{
	{"served", map[string]string{beadmeta.RoutedToMetadataKey: "repo/other"}, true},
	{"unserved route", map[string]string{beadmeta.RoutedToMetadataKey: "nowhere/lane"}, false},
	{"expanded root, no route", map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow, beadmeta.WorkflowExpandedMetadataKey: "true"}, false},
	{"expanded root, served run_target", map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow, beadmeta.WorkflowExpandedMetadataKey: "true", beadmeta.RunTargetMetadataKey: "repo/worker"}, false},
}

// strandCity is a city serving repo/worker and repo/other, with one seat.
type strandCity struct {
	path  string
	cfg   *config.City
	store *beads.MemStore
	seat  beads.Bead
	claim map[string]beads.Bead // by strandCase name
	rec   *events.Fake
}

func newStrandCity(t *testing.T, status string, seatMeta map[string]string) *strandCity {
	t.Helper()
	c := &strandCity{path: t.TempDir(), store: beads.NewMemStore(), claim: map[string]beads.Bead{}, rec: events.NewFake()}
	c.cfg = &config.City{Workspace: config.Workspace{Name: "strand-town"}, Agents: []config.Agent{persistentWorker(), otherLane()}}
	meta := map[string]string{"session_name": "repo--worker-1", "template": "repo/worker", "pool_managed": "true", "state": "asleep"}
	for k, v := range seatMeta {
		meta[k] = v
	}
	seat, err := c.store.Create(beads.Bead{Title: "seat", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: meta})
	if err != nil {
		t.Fatal(err)
	}
	c.seat = seat
	for _, sc := range strandCases {
		c.claim[sc.name] = createWork(t, c.store, status, seat.ID, sc.meta)
	}
	return c
}

func (c *strandCity) sw() *SeatWork {
	sw := testSeatWork(c.path, c.cfg, c.store, nil)
	sw.alerts = &strandedClaimAlerts{rec: c.rec}
	return sw
}

func (c *strandCity) info() session.Info {
	b, _ := c.store.Get(c.seat.ID)
	return seedSessionInfo(b)
}

// assertStrandTable checks every row: the control released, every strand
// kept assigned to the seat with its status.
func (c *strandCity) assertStrandTable(t *testing.T, status string) {
	t.Helper()
	for _, sc := range strandCases {
		got, err := c.store.Get(c.claim[sc.name].ID)
		if err != nil {
			t.Fatal(err)
		}
		if sc.released && (got.Status != "open" || got.Assignee != "") {
			t.Errorf("%s: status=%q assignee=%q, want released", sc.name, got.Status, got.Assignee)
		}
		if !sc.released && (got.Status != status || got.Assignee != c.seat.ID) {
			t.Errorf("%s: status=%q assignee=%q, want kept %s by %s", sc.name, got.Status, got.Assignee, status, c.seat.ID)
		}
	}
}

// unservedIDs are the claims a seat keeps, sorted.
func (c *strandCity) unservedIDs() []string {
	var ids []string
	for _, sc := range strandCases {
		if !sc.released {
			ids = append(ids, c.claim[sc.name].ID)
		}
	}
	slices.Sort(ids)
	return ids
}

// alerts are the session.stranded_claims payloads recorded.
func (c *strandCity) alerts(t *testing.T) []api.SessionStrandedClaimsPayload {
	t.Helper()
	var got []api.SessionStrandedClaimsPayload
	for _, e := range c.rec.Events {
		if e.Type == events.SessionStrandedClaims {
			var p api.SessionStrandedClaimsPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatal(err)
			}
			got = append(got, p)
		}
	}
	return got
}

// assertAlerted checks n session.stranded_claims, the last naming every
// kept claim with its route and reason, for kind.
func (c *strandCity) assertAlerted(t *testing.T, kind string, n int) {
	t.Helper()
	got := c.alerts(t)
	if len(got) != n {
		t.Fatalf("session.stranded_claims events = %d, want %d", len(got), n)
	}
	if n == 0 {
		return
	}
	last := got[n-1]
	var ids []string
	for _, cl := range last.Claims {
		ids = append(ids, cl.BeadID)
		if cl.Reason != keptUnservedRoute {
			t.Errorf("claim %s reason = %q, want %q", cl.BeadID, cl.Reason, keptUnservedRoute)
		}
		if want := c.claim["unserved route"].ID; cl.BeadID == want && cl.Route != "nowhere/lane" {
			t.Errorf("claim %s route = %q, want nowhere/lane", cl.BeadID, cl.Route)
		}
	}
	if last.Kind != kind || last.Template != "repo/worker" || last.SessionID != c.seat.ID || !slices.Equal(ids, c.unservedIDs()) {
		t.Fatalf("alert = %+v, want kind %q, the seat's template and beads %v", last, kind, c.unservedIDs())
	}
}

// NEW-5, every row on every path: each path releases the served claim and
// keeps every claim no lane would serve.
func TestReleasePathsKeepEveryStrand(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	t.Run("killed seat", func(t *testing.T) {
		c := newStrandCity(t, "open", map[string]string{"sleep_reason": string(session.SleepReasonKilled), "slept_at": now.Add(-time.Minute).Format(time.RFC3339)})
		releaseUnexecutedClaimsOnKill(c.sw(), c.store, now, c.info(), io.Discard)
		c.assertStrandTable(t, "open")
		c.assertAlerted(t, "", 0) // the seat is open: nothing is stranded
	})
	t.Run("drain-ack", func(t *testing.T) {
		c := newStrandCity(t, "in_progress", nil)
		releaseUnexecutedClaimsOnDrainAck(testWorkLegs(c.path, c.cfg, c.store, nil), c.seat, time.Minute, io.Discard)
		c.assertStrandTable(t, "in_progress")
	})
	t.Run("stranded repair", func(t *testing.T) {
		c := newStrandCity(t, "in_progress", map[string]string{strandedEventEmittedKey: now.Add(-strandedRepairConfirmGrace - time.Minute).Format(time.RFC3339)})
		var stderr bytes.Buffer
		if !repairStrandedPoolWorkerBead(c.sw(), c.store, c.info(), &clock.Fake{Time: now}, &stderr) {
			t.Fatalf("the repair did not close the seat (O5: a close never refuses over unserved claims); stderr=%q", stderr.String())
		}
		c.assertStrandTable(t, "in_progress")
		c.assertAlerted(t, strandedRepairCloseReason, 1)
	})
	t.Run("confirmed orphan", func(t *testing.T) {
		c := newStrandCity(t, "open", nil)
		var assigned []beads.Bead
		for _, sc := range strandCases {
			assigned = append(assigned, c.claim[sc.name])
		}
		releaseConfirmedOrphanSessionWork(c.cfg, c.store, nil, assigned, nil, c.info())
		c.assertStrandTable(t, "open")
	})
	t.Run("dead assignee", func(t *testing.T) {
		c := newStrandCity(t, "open", nil)
		var assigned []beads.Bead
		for _, sc := range strandCases {
			assigned = append(assigned, c.claim[sc.name])
		}
		// No open session holds the claims: their assignee is dead.
		if err := c.store.Close(c.seat.ID); err != nil {
			t.Fatal(err)
		}
		releaseOrphanedPoolAssignments(c.store, beads.SessionStore{Store: c.store}, c.cfg, c.path, nil, assigned, nil, nil, nil, nil, nil)
		c.assertStrandTable(t, "open")
	})
	t.Run("retired session", func(t *testing.T) {
		c := newStrandCity(t, "in_progress", nil)
		unclaimWorkAssignedToRetiredSessionBead(testWorkLegs(c.path, c.cfg, c.store, nil), c.store, c.seat, "", io.Discard)
		c.assertStrandTable(t, "in_progress")
	})
}

// A close never refuses over claims no lane serves (O5, revised): it lands,
// keeps them on the closed row, and raises session.stranded_claims; the
// detector raises it again on D6's cadence while they stay, and the episode
// ends once they are rerouted or released.
func TestCloseCompletesAndStrandedClaimsAlertUntilRerouted(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	c := newStrandCity(t, "open", nil)
	sw := c.sw()
	if !closeBead(c.store, sw, c.info(), "orphaned", now, io.Discard) {
		t.Fatal("the close refused over claims no lane serves")
	}
	c.assertStrandTable(t, "open")
	c.assertAlerted(t, "orphaned", 1)

	detect := func(at time.Time) {
		t.Helper()
		detectStrandedClaims(testWorkLegs(c.path, c.cfg, c.store, nil), c.store, nil, sw.alerts, at)
	}
	detect(now) // the scan finds the same episode, not yet due
	c.assertAlerted(t, "orphaned", 1)
	detect(now.Add(strandedRealertEvery / 2)) // the scan itself is not due
	c.assertAlerted(t, "orphaned", 1)
	detect(now.Add(strandedRealertEvery))
	c.assertAlerted(t, "orphaned", 2)

	// Rerouted to a served lane (the dead-assignee sweep's to release) or
	// closed, nothing is stranded: the episode ends without an alert.
	for _, sc := range strandCases[1:] {
		b := c.claim[sc.name]
		if sc.name == "unserved route" {
			if err := c.store.SetMetadata(b.ID, beadmeta.RoutedToMetadataKey, "repo/other"); err != nil {
				t.Fatal(err)
			}
			continue
		}
		closeWork(t, c.store, b)
	}
	detect(now.Add(2 * strandedRealertEvery))
	c.assertAlerted(t, "orphaned", 2)
	if len(sw.alerts.episodes) != 0 {
		t.Fatalf("episodes = %v, want the seat's ended", sw.alerts.episodes)
	}
}

// A close with nothing stranded ends the seat's episode: a later strand on
// the same set alerts at once (M19).
func TestStrandedEpisodeEndsWithACleanClose(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	c := newStrandCity(t, "open", nil)
	sw := c.sw()
	seat := releaseSeatOfBead(c.seat)
	claims := []strandedClaim{{id: "w-1", reason: keptUnservedRoute}}
	sw.alerts.observe(seat, "orphaned", claims, now)
	for _, sc := range strandCases[1:] {
		closeWork(t, c.store, c.claim[sc.name])
	}
	if !closeBead(c.store, sw, c.info(), "orphaned", now, io.Discard) {
		t.Fatal("the close refused")
	}
	sw.alerts.observe(seat, "orphaned", claims, now)
	if got := len(c.alerts(t)); got != 2 {
		t.Fatalf("session.stranded_claims events = %d, want 2: the clean close ended the episode", got)
	}
}

// Route recovery restores a workflow root's run_target only while the root is
// unexpanded: an expanded root is neither demanded nor claimable (#5900).
func TestCarriedPoolRouteSkipsAnExpandedRoot(t *testing.T) {
	root := beads.Bead{Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow, beadmeta.RunTargetMetadataKey: "repo/worker"}}
	if got := carriedPoolRoute(root); got != "repo/worker" {
		t.Fatalf("unexpanded root's carried route = %q, want repo/worker", got)
	}
	root.Metadata[beadmeta.WorkflowExpandedMetadataKey] = "true"
	if got := carriedPoolRoute(root); got != "" {
		t.Fatalf("expanded root's carried route = %q, want none: recovery would resurrect it", got)
	}
}

// laneDirect needs an agent that runs generic sessions and a route of the
// claim's own; laneCarried takes any configured agent and a plain claim's
// run_target (M04).
func TestReleaseRouteLanes(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{persistentWorker(), {Name: "fixed", Dir: "repo", StartCommand: "true", MaxActiveSessions: intPtr(0)}}}
	routed := beads.Bead{Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "repo/fixed"}}
	targeted := beads.Bead{Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "repo/worker"}}
	for _, tc := range []struct {
		name   string
		b      beads.Bead
		lane   laneRule
		served bool
	}{
		{"routed to a fixed agent, direct", routed, laneDirect, false},
		{"routed to a fixed agent, carried", routed, laneCarried, false},
		{"routed to a fixed agent a named session backs, carried", routed, laneCarried, true},
		{"a plain run_target, direct", targeted, laneDirect, false},
		{"a plain run_target, carried", targeted, laneCarried, true},
	} {
		c := cfg
		if strings.Contains(tc.name, "named session") {
			named := *cfg
			named.NamedSessions = []config.NamedSession{{Name: "fixed-one", Template: "repo/fixed"}}
			c = &named
		}
		if _, served := releaseRoute(c, tc.b, "", tc.lane); served != tc.served {
			t.Errorf("%s: served = %v, want %v", tc.name, served, tc.served)
		}
	}
}

// A write past the release's deadline fails with failedBudget and leaves the
// claim assigned (M07).
func TestReleaseClaimsStopsAtItsDeadline(t *testing.T) {
	c := newStrandCity(t, "in_progress", nil)
	served := c.claim["served"]
	scope := releaseScopeOfBead(c.seat, c.cfg)
	out, err := ReleaseClaims(c.sw(), scope, releaseSeatOfBead(c.seat), releaseCloseCascade, ReleaseOpts{Deadline: time.Now().Add(-time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if out.Why[served.ID] != failedBudget || len(out.Released) != 0 {
		t.Fatalf("outcome = %+v, want the served claim failed on the budget", out)
	}
	if got, _ := c.store.Get(served.ID); got.Assignee != c.seat.ID {
		t.Fatalf("served claim assignee = %q, want kept past the deadline", got.Assignee)
	}
}

// Only a claim kept for want of a lane is stranded: started or moved claims
// are not (M10).
func TestUnservedIsOnlyTheUnservedRoute(t *testing.T) {
	out := ReleaseOutcome{
		Kept:  []seatWorkHit{{bead: beads.Bead{ID: "a"}}, {bead: beads.Bead{ID: "b"}}, {bead: beads.Bead{ID: "c"}}},
		Why:   map[string]string{"a": keptStarted, "b": keptMoved, "c": keptUnservedRoute},
		Route: map[string]string{"c": "nowhere/lane"},
	}
	if got := out.Unserved(); !reflect.DeepEqual(got, []strandedClaim{{id: "c", route: "nowhere/lane", reason: keptUnservedRoute}}) {
		t.Fatalf("Unserved = %+v, want only c", got)
	}
}

// Without the city config no route can be judged: the release defers,
// releasing nothing and keeping nothing as unserved.
func TestReleaseClaimsDefersWithoutConfig(t *testing.T) {
	c := newStrandCity(t, "in_progress", nil)
	scope := releaseScopeOfBead(c.seat, c.cfg)
	out, err := ReleaseClaims(testSeatWork(c.path, nil, c.store, nil), scope, releaseSeatOfBead(c.seat), releaseCloseCascade, ReleaseOpts{})
	if !errors.Is(err, errReleaseNoConfig) || len(out.Released)+len(out.Kept) != 0 {
		t.Fatalf("outcome = %+v, %v; want deferred", out, err)
	}
	if got, _ := c.store.Get(c.claim["served"].ID); got.Assignee != c.seat.ID {
		t.Fatal("a release without config released a claim")
	}
}

// The stranded repair's own release gives a routeless claim the seat's
// route, so a lane serves it (ga-n2d.2, M16).
func TestStrandedRepairReleaseStampsTheSeatRoute(t *testing.T) {
	c := newStrandCity(t, "in_progress", nil)
	plain := createWork(t, c.store, "in_progress", c.seat.ID, nil)
	unclaimWorkAssignedToRetiredSessionInfo(c.sw(), c.store, c.info(), io.Discard)
	got, err := c.store.Get(plain.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Assignee != "" || got.Metadata[beadmeta.RunTargetMetadataKey] != "repo/worker" {
		t.Fatalf("routeless claim: assignee=%q run_target=%q, want released into the seat's route repo/worker", got.Assignee, got.Metadata[beadmeta.RunTargetMetadataKey])
	}
}

// The retired-session sweep releases under the seat's own identities only,
// never the config-derived stable alias, which the identity's next seat
// claims under (M24).
func TestRetiredSweepLeavesTheStableAliasClaim(t *testing.T) {
	c := newStrandCity(t, "in_progress", map[string]string{"alias": "ada", "pool_slot": "1"})
	c.cfg.Agents[0].NamepoolNames = []string{"ada"}
	if alias := stableAssignmentAliasForConfig(c.seat, c.cfg); alias != "ada" {
		t.Fatalf("fixture alias = %q, want the stable namepool alias", alias)
	}
	aliased := createWork(t, c.store, "in_progress", "ada", routedClaim())
	unclaimWorkAssignedToRetiredSessionBead(testWorkLegs(c.path, c.cfg, c.store, nil), c.store, c.seat, "", io.Discard)
	if got, _ := c.store.Get(aliased.ID); got.Assignee != "ada" {
		t.Fatalf("alias claim assignee = %q, want it left to the identity", got.Assignee)
	}
	c.assertStrandTable(t, "in_progress") // the seat's own claims still go
}

// The live-claim veto fails safe: legs it cannot read count as the seat
// owning its stamped claim (M34).
func TestSessionOwnsLiveClaimFailsSafeOnUnreadableLegs(t *testing.T) {
	c := newStrandCity(t, "in_progress", map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: "w-1"})
	owns, claim, err := sessionOwnsLiveClaim(c.path, c.cfg, c.store, newSeatWork(WorkLegs{}), c.info())
	if !owns || claim != "w-1" || err == nil {
		t.Fatalf("sessionOwnsLiveClaim = %v, %q, %v; want owned, w-1 and the read's error", owns, claim, err)
	}
}

// The legacy tick runs the stranded-work detector over its assigned-work
// snapshot: a claim no lane serves on a closed session raises
// session.stranded_claims.
func TestBeadReconcileTickDetectsStrandedClaims(t *testing.T) {
	store := beads.NewMemStore()
	seat, err := store.Create(beads.Bead{Title: "seat", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{"session_name": "worker-gone", "template": "worker", "state": "drained"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(seat.ID); err != nil {
		t.Fatal(err)
	}
	work := createWork(t, store, "in_progress", seat.ID, map[string]string{beadmeta.RoutedToMetadataKey: "nowhere/lane", beadmeta.StrandedMetadataKey: "true", beadmeta.StrandedSessionMetadataKey: seat.ID})
	rec := events.NewFake()
	cr := &CityRuntime{
		cityPath: t.TempDir(), cityName: "strand-town",
		cfg:                 &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}}},
		sp:                  runtime.NewFake(),
		standaloneCityStore: store,
		sessionDrains:       newDrainTracker(),
		rec:                 rec,
		stdout:              io.Discard,
		stderr:              io.Discard,
	}
	result := DesiredStateResult{State: map[string]TemplateParams{}, ScaleCheckCounts: map[string]int{"worker": 0}, AssignedWorkBeads: []beads.Bead{work}}
	cr.strandedClaimAlerter().scanned = time.Now().Add(-strandedRealertEvery) // a scan is due
	cr.beadReconcileTick(context.Background(), newSeatWork(cr.workLegs()), result, newSessionBeadSnapshot(nil), nil, false)
	if evts, _ := rec.List(events.Filter{Type: events.SessionStrandedClaims}); len(evts) != 1 || evts[0].Subject != seat.ID {
		t.Fatalf("session.stranded_claims = %+v, want one for %s", evts, seat.ID)
	}
}

// Retiring a removed named session alerts on the claims it keeps.
func TestRetireRemovedNamedSessionAlertsOnKeptClaims(t *testing.T) {
	store := beads.NewMemStore()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	b, err := store.Create(beads.Bead{Title: "retired reviewer", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
		"session_name": "reviewer-runtime", "template": "worker", "state": "asleep",
		namedSessionMetadataKey: "true", namedSessionIdentityMetadata: "reviewer", namedSessionModeMetadata: "on_demand",
	}})
	if err != nil {
		t.Fatal(err)
	}
	kept := createWork(t, store, "in_progress", b.ID, map[string]string{beadmeta.RoutedToMetadataKey: "nowhere/lane"})
	rec := events.NewFake()
	sw := testSeatWork(t.TempDir(), servingCity("worker"), store, nil)
	sw.alerts = &strandedClaimAlerts{rec: rec}
	if !retireRemovedConfiguredNamedSessionBead(t.TempDir(), store, sw, runtime.NewFake(), b, now, io.Discard) {
		t.Fatal("retire failed")
	}
	evts, _ := rec.List(events.Filter{Type: events.SessionStrandedClaims})
	if len(evts) != 1 || !strings.Contains(string(evts[0].Payload), kept.ID) || !strings.Contains(string(evts[0].Payload), `"kind":"retired"`) {
		t.Fatalf("session.stranded_claims = %+v, want one naming %s, kind retired", evts, kept.ID)
	}
}

// closedSessionFor names a closed session by bead ID or session_name, and

// The detector reads the sessions store only when its cadence is due, and

// The killed seat's pre-gate costs no read for a seat whose open claims have
// no route: laneDirect would keep them all (M26).
func TestKilledSeatPreGateWantsARoutedClaim(t *testing.T) {
	info := session.Info{ID: "gc-1", SessionNameMetadata: "worker-1"}
	plain := beads.Bead{ID: "w-1", Status: "open", Assignee: "gc-1"}
	routed := beads.Bead{ID: "w-2", Status: "open", Assignee: "gc-1", Metadata: routedClaim()}
	if killedSeatSnapshotHasRoutedClaim(nil, []beads.Bead{plain}, info) {
		t.Fatal("a routeless open claim passed the pre-gate")
	}
	if !killedSeatSnapshotHasRoutedClaim(nil, []beads.Bead{plain, routed}, info) {
		t.Fatal("a routed open claim did not pass the pre-gate")
	}
}

// The confirmed-orphan close completes over the claims its release kept for
// want of a lane (O5, revised; the canonical NEW-5 trigger: an agent removed
// while its seat holds claims routed to it): the claims stay on the closed
// row, stamped, and the cascade alerts. A served claim is still released.
func TestOrphanCloseCompletesOverStrandedClaims(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta map[string]string
	}{
		{"routed to a removed agent", map[string]string{beadmeta.RoutedToMetadataKey: "repo/gone"}},
		{"expanded root, run_target only", map[string]string{beadmeta.RoutedToMetadataKey: "", beadmeta.KindMetadataKey: beadmeta.KindWorkflow, beadmeta.WorkflowExpandedMetadataKey: "true", beadmeta.RunTargetMetadataKey: "repo/polecat"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newCloseOrphanEnv(t)
			if err := env.store.SetMetadataBatch(env.work.ID, tc.meta); err != nil {
				t.Fatal(err)
			}
			if err := env.store.Update(env.work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
				t.Fatal(err)
			}
			rec := events.NewFake()
			cr := &CityRuntime{
				cityPath: env.cityDir, cityName: "trace-town", cfg: env.cfg, sp: env.sp, standaloneCityStore: env.store,
				sessionDrains: newDrainTracker(), rec: rec, stdout: io.Discard, stderr: io.Discard,
			}
			cr.beadReconcileTick(context.Background(), cr.newSeatWork(), DesiredStateResult{State: map[string]TemplateParams{}, AssignedWorkBeads: []beads.Bead{env.get(t, env.work.ID)}},
				newSessionBeadSnapshot([]beads.Bead{env.session}), nil, false)
			if got := env.get(t, env.session.ID); got.Status != "closed" {
				t.Fatalf("seat status = %q, want closed over the kept claim", got.Status)
			}
			work := env.get(t, env.work.ID)
			if work.Assignee != "polecat-1" || work.Metadata[beadmeta.StrandedSessionMetadataKey] != env.session.ID {
				t.Fatalf("claim assignee=%q stranded_session=%q, want kept on the closed seat and stamped", work.Assignee, work.Metadata[beadmeta.StrandedSessionMetadataKey])
			}
			if evts, _ := rec.List(events.Filter{Type: events.SessionStrandedClaims}); len(evts) != 1 {
				t.Fatalf("session.stranded_claims = %d, want 1", len(evts))
			}
		})
	}
}

// A killed seat of a max_active_sessions = 1 pool holding only an open claim
// on an unserved route is not desired, and the orphan close completes over
// it, keeping and stamping the claim (the reviewer's PB2).
func TestKilledSingletonSeatClosesOverAStrandedClaim(t *testing.T) {
	agent := persistentWorker()
	agent.MaxActiveSessions = intPtr(1)
	e := newKilledSeatEnv(t, agent)
	work := e.addWork(t, "open", e.seat.ID, map[string]string{beadmeta.RoutedToMetadataKey: "nowhere/lane"})
	e.tick(t, claims(e.reload(t, work.ID)))
	seat, w := e.reload(t, e.seat.ID), e.reload(t, work.ID)
	if seat.Status != "closed" || w.Assignee != e.seat.ID || w.Metadata[beadmeta.StrandedSessionMetadataKey] != e.seat.ID {
		t.Fatalf("seat %q, claim assignee %q stranded_session %q; want the seat closed and the claim kept and stamped", seat.Status, w.Assignee, w.Metadata[beadmeta.StrandedSessionMetadataKey])
	}
}

// detectorReadCounter counts a store's reads.
type detectorReadCounter struct {
	beads.Store
	gets, lists int
}

func (s *detectorReadCounter) Get(id string) (beads.Bead, error) { s.gets++; return s.Store.Get(id) }
func (s *detectorReadCounter) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.lists++
	return s.Store.List(q)
}

// The detector reads the stamp, so it finds what a seat's identities cannot:
// a claim under a namepool alias (PC) and a removed named session's claims
// on its archived, still-open row (PE). It re-alerts on the cadence, at once
// when the set changes (D07), and keeps a stamped claim only a run_target
// routes, which the dead-assignee sweep never releases (D01).
func TestDetectorFollowsTheStamp(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	t.Run("namepool alias", func(t *testing.T) {
		c := newStrandCity(t, "in_progress", map[string]string{"alias": "repo/ada", "pool_slot": "1"})
		c.cfg.Agents[0].NamepoolNames = []string{"ada"}
		createWork(t, c.store, "in_progress", "repo/ada", map[string]string{beadmeta.RoutedToMetadataKey: "nowhere/lane"})
		sw := c.sw()
		if !closeBead(c.store, sw, c.info(), "orphaned", now, io.Discard) {
			t.Fatal("close refused")
		}
		before := len(c.alerts(t))
		sw.alerts.scanned = now // a scan was due an interval ago
		detectStrandedClaims(testWorkLegs(c.path, c.cfg, c.store, nil), c.store, nil, sw.alerts, now.Add(strandedRealertEvery))
		if got := c.alerts(t); len(got) != before+1 || len(got[len(got)-1].Claims) != len(c.unservedIDs())+1 {
			t.Fatalf("alerts = %d (last %+v), want one more naming the alias claim too", len(got), got[len(got)-1])
		}
	})
	t.Run("retired named session", func(t *testing.T) {
		store := beads.NewMemStore()
		b, err := store.Create(beads.Bead{Title: "retired reviewer", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
			"session_name": "reviewer-runtime", "template": "worker", "state": "asleep", "alias": "reviewer",
			namedSessionMetadataKey: "true", namedSessionIdentityMetadata: "reviewer", namedSessionModeMetadata: "on_demand",
		}})
		if err != nil {
			t.Fatal(err)
		}
		createWork(t, store, "in_progress", b.ID, map[string]string{beadmeta.RoutedToMetadataKey: "nowhere/lane"})
		rec := events.NewFake()
		cfg := servingCity("worker")
		sw := testSeatWork(t.TempDir(), cfg, store, nil)
		sw.alerts = &strandedClaimAlerts{rec: rec}
		if !retireRemovedConfiguredNamedSessionBead(t.TempDir(), store, sw, runtime.NewFake(), b, now, io.Discard) {
			t.Fatal("retire failed")
		}
		row, _ := store.Get(b.ID)
		sw.alerts.scanned = now
		detectStrandedClaims(testWorkLegs("", cfg, store, nil), store, sessionInfosFromBeads([]beads.Bead{row}), sw.alerts, now.Add(strandedRealertEvery))
		if evts, _ := rec.List(events.Filter{Type: events.SessionStrandedClaims}); len(evts) != 2 {
			t.Fatalf("session.stranded_claims = %d, want the retirement's and the scan's", len(evts))
		}
	})
	t.Run("set change and run_target only", func(t *testing.T) {
		store := beads.NewMemStore()
		seat, err := store.Create(beads.Bead{Title: "seat", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{"session_name": "s-1", "template": "repo/worker"}})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Close(seat.ID); err != nil {
			t.Fatal(err)
		}
		stamp := func(meta map[string]string) {
			meta[beadmeta.StrandedMetadataKey], meta[beadmeta.StrandedSessionMetadataKey] = "true", seat.ID
			createWork(t, store, "in_progress", seat.ID, meta)
		}
		stamp(map[string]string{beadmeta.RunTargetMetadataKey: "repo/worker"})
		rec := events.NewFake()
		alerts := &strandedClaimAlerts{rec: rec, scanned: now.Add(-strandedRealertEvery)}
		legs := testWorkLegs("", &config.City{Agents: []config.Agent{persistentWorker()}}, store, nil)
		detectStrandedClaims(legs, store, nil, alerts, now)
		stamp(map[string]string{beadmeta.RoutedToMetadataKey: "nowhere/lane"})
		alerts.scanned = now.Add(-strandedRealertEvery) // the next scan is due; the episode's repeat is not
		detectStrandedClaims(legs, store, nil, alerts, now.Add(time.Minute))
		if evts, _ := rec.List(events.Filter{Type: events.SessionStrandedClaims}); len(evts) != 2 {
			t.Fatalf("session.stranded_claims = %d, want the run_target claim's and the changed set's at once", len(evts))
		}
	})
}

// A scan reads one list per leg, and a seat's row once: a stamped claim on a
// served route costs no session read, and nothing is read between scans.
func TestDetectorScanCost(t *testing.T) {
	mem := beads.NewMemStore()
	seat, err := mem.Create(beads.Bead{Title: "seat", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{"session_name": "s-1"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"nowhere/lane", "repo/worker"} {
		createWork(t, mem, "in_progress", seat.ID, map[string]string{beadmeta.RoutedToMetadataKey: route, beadmeta.StrandedMetadataKey: "true", beadmeta.StrandedSessionMetadataKey: seat.ID})
	}
	work, sessions := &detectorReadCounter{Store: mem}, &detectorReadCounter{Store: mem}
	legs := testWorkLegs("", &config.City{Agents: []config.Agent{persistentWorker()}}, work, nil)
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	alerts := &strandedClaimAlerts{}
	for i, at := range []time.Time{now.Add(-time.Hour), now, now.Add(time.Minute), now.Add(strandedRealertEvery)} {
		detectStrandedClaims(legs, sessions, nil, alerts, at)
		if i == 0 {
			if work.lists != 0 {
				t.Fatalf("the first call read %d lists, want none: it starts the clock", work.lists)
			}
			continue
		}
		wantLists := []int{0, 1, 1, 2}[i]
		if work.lists != wantLists || work.gets != 0 || sessions.gets != 1 {
			t.Fatalf("after scan %d: work lists %d gets %d, session gets %d; want %d, 0, 1", i, work.lists, work.gets, sessions.gets, wantLists)
		}
	}
}

// Without the city config a seat with no claims releases nothing and
// reports no error (D18).
func TestReleaseClaimsWithoutConfigAndNoClaims(t *testing.T) {
	store := beads.NewMemStore()
	seat, _ := store.Create(beads.Bead{Title: "seat", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{"session_name": "s-1"}})
	scope := releaseScopeOfBead(seat, nil)
	if _, err := ReleaseClaims(testSeatWork("", nil, store, nil), scope, releaseSeatOfBead(seat), releaseCloseCascade, ReleaseOpts{}); err != nil {
		t.Fatalf("err = %v, want none for a seat holding nothing", err)
	}
}

// A release clears a claim's stranded stamp (SessionAffinityMetadataKeys),
// so the detector forgets it.
func TestReleaseClearsTheStrandedStamp(t *testing.T) {
	c := newStrandCity(t, "in_progress", nil)
	served := c.claim["served"]
	if err := c.store.SetMetadataBatch(served.ID, map[string]string{beadmeta.StrandedMetadataKey: "true", beadmeta.StrandedSessionMetadataKey: c.seat.ID}); err != nil {
		t.Fatal(err)
	}
	unclaimWorkAssignedToRetiredSessionBead(testWorkLegs(c.path, c.cfg, c.store, nil), c.store, c.seat, "", io.Discard)
	got, _ := c.store.Get(served.ID)
	if got.Assignee != "" || got.Metadata[beadmeta.StrandedMetadataKey] != "" || got.Metadata[beadmeta.StrandedSessionMetadataKey] != "" {
		t.Fatalf("released claim: assignee %q, stamp %q/%q; want released and unstamped", got.Assignee, got.Metadata[beadmeta.StrandedMetadataKey], got.Metadata[beadmeta.StrandedSessionMetadataKey])
	}
}

// A leg the scan cannot read ends no episode: its claims may still stand.
func TestDetectorKeepsEpisodesOverADarkLeg(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	alerts := &strandedClaimAlerts{scanned: now.Add(-strandedRealertEvery)}
	alerts.observe(releaseSeat{id: "gc-1"}, "drained", []strandedClaim{{id: "w-1", reason: keptUnservedRoute}}, now)
	detectStrandedClaims(testWorkLegs("", servingCity("worker"), failingListStore{Store: beads.NewMemStore()}, nil), beads.NewMemStore(), nil, alerts, now.Add(strandedRealertEvery))
	if _, ok := alerts.episodes["gc-1"]; !ok {
		t.Fatal("an unreadable leg ended the seat's episode")
	}
}
