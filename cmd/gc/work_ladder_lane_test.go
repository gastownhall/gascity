package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/workladder"
)

// ladderCity is one pool seat on a city whose session rows the work-ladder
// step reads and writes through a blind-write-refusing store (L8: every
// write of a session row must be a CAS), with its work on a store of its
// own. The fake runtime runs the seat and reads it quiet; the planner's
// inbox records the recycles it is posted.
type ladderCity struct {
	t        *testing.T
	sessions *beads.MemStore
	work     *beads.MemStore
	sp       *runtime.Fake
	rec      *events.Fake
	env      externalReadsEnv
	lane     *externalReadsLane
	now      time.Time
	seat     string // the session row's ID
	claims   []string
	trigger  string
	partial  bool
	posted   []executionStalledRequest
	nudgedAt []time.Duration // since start, one per delivered Nudge call
	start    time.Time
	wakes    *atomic.Int64 // the planner's wakes (the lane's onChange)
}

const ladderSeatName = "seat-a"

// newLadderCity is a running, quiet pool seat at generation 7.
func newLadderCity(t *testing.T) *ladderCity {
	t.Helper()
	sessions := beads.NewMemStore()
	stampedMemStore(t, sessions)
	work := beads.NewMemStore()
	m := map[string]string{
		"session_name": ladderSeatName, "pool_managed": "true", "template": "worker", "state": "active",
		"generation": "7", "instance_token": "tok-7", "provider": "claude",
	}
	row, err := sessions.Create(beads.Bead{Title: "seat", Type: "session", Labels: []string{sessionBeadLabel}, Metadata: m})
	if err != nil {
		t.Fatal(err)
	}
	sp := runningIdleClaimFake(t, ladderSeatName)
	cityPath := t.TempDir()
	cfg := &config.City{Workspace: config.Workspace{Name: "c"}, Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(4)}}}
	c := &ladderCity{t: t, sessions: sessions, work: work, sp: sp, rec: events.NewFake(), now: stepsTestNow, start: stepsTestNow, seat: row.ID}
	c.env = externalReadsEnv{
		CityPath: cityPath, CityName: "c", Cfg: cfg, CityStore: blindWriteRefusingStore{inner: sessions}, SP: sp,
		WorkLegs: testWorkLegs(cityPath, cfg, work, nil),
	}
	c.lane, c.wakes = newPoolStepsTestLane(c.env, c.summary, func(r executionStalledRequest) { c.posted = append(c.posted, r) })
	c.lane.now = func() time.Time { return c.now }
	c.lane.events = c.rec
	sp.SetActivity(ladderSeatName, stepsTestNow.Add(-time.Hour))
	return c
}

// claim adds an in_progress claim held by the seat, returning its ID.
func (c *ladderCity) claim(title string) string {
	c.t.Helper()
	b, err := c.work.Create(beads.Bead{Title: title, Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}})
	if err != nil {
		c.t.Fatal(err)
	}
	inProgress := "in_progress"
	if err := c.work.Update(b.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &c.seat}); err != nil {
		c.t.Fatal(err)
	}
	c.claims = append(c.claims, b.ID)
	return b.ID
}

// summary is the planner's last allocation at c.now: the seat's claims as
// they stand, and its trigger.
func (c *ladderCity) summary() *allocSummary {
	s := &allocSummary{At: c.now, Partial: c.partial}
	for _, id := range c.claims {
		b, err := c.work.Get(id)
		if err != nil || b.Status != "in_progress" {
			continue
		}
		s.AssignedWork, s.AssignedStores, s.AssignedStoreRefs = append(s.AssignedWork, b), append(s.AssignedStores, c.work), append(s.AssignedStoreRefs, "")
	}
	if c.trigger != "" {
		s.ReadyRouted, s.ReadyRoutedRefs = []beads.Bead{{ID: c.trigger, Status: "open"}}, []string{""}
	}
	return s
}

// pass runs the step once at c.now and records the nudges it delivered.
func (c *ladderCity) pass() {
	c.t.Helper()
	before := c.sp.CountCalls("Nudge", ladderSeatName)
	c.lane.workLadder(context.Background(), c.env)
	for range c.sp.CountCalls("Nudge", ladderSeatName) - before {
		c.nudgedAt = append(c.nudgedAt, c.now.Sub(c.start))
	}
}

// run passes every step until the seat has been watched for d.
func (c *ladderCity) run(d, step time.Duration, each func()) {
	for end := c.now.Add(d); c.now.Before(end); c.now = c.now.Add(step) {
		c.pass()
		if each != nil {
			each()
		}
	}
}

func (c *ladderCity) row() beads.Bead {
	c.t.Helper()
	b, err := c.sessions.Get(c.seat)
	if err != nil {
		c.t.Fatal(err)
	}
	return b
}

func (c *ladderCity) cell() workladder.Record {
	c.t.Helper()
	r, err := workladder.DecodeLadder(c.row().Metadata[session.WorkLadderKey])
	if err != nil {
		c.t.Fatal(err)
	}
	return r
}

func (c *ladderCity) entry(bead string) workladder.Entry {
	c.t.Helper()
	for _, e := range c.cell().Entries {
		if e.Key.Bead == bead {
			return e
		}
	}
	c.t.Fatalf("no ladder entry for %s in %s", bead, c.row().Metadata[session.WorkLadderKey])
	return workladder.Entry{}
}

// setRow writes kvs to the seat's row by a CAS: the work cells are CASOnly.
func (c *ladderCity) setRow(kvs map[string]string) {
	c.t.Helper()
	casSetRow(c.t, c.sessions, c.seat, kvs)
}

// casSetRow writes kvs to row id of store by a CAS at a fresh revision.
func casSetRow(t *testing.T, store *beads.MemStore, id string, kvs map[string]string) {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateIfMatch(id, b.Revision, beads.UpdateOpts{Metadata: kvs}); err != nil {
		t.Fatal(err)
	}
}

func (c *ladderCity) events(typ string) []events.Event {
	evts, _ := c.rec.List(events.Filter{Type: typ})
	return evts
}

// escalations are c's work.escalated reasons, in order.
func escalations(c *ladderCity) []string {
	var out []string
	for _, e := range c.events(events.WorkEscalated) {
		var p events.WorkLadderPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			c.t.Fatal(err)
		}
		out = append(out, p.Reason)
	}
	return out
}

// halfPast is m minutes and 30 seconds: the ladder's due times on a pass
// grid that starts on the minute.
func halfPast(m int) time.Duration { return time.Duration(m)*time.Minute + 30*time.Second }

// L1. The wedge, replayed (maintainer-city, 2026-10-17: nudge 3/3, a
// Cloudflare 524, eleven silent hours). A started seat that never answers
// is nudged after the grace, then +3, +6, +12, +24 and +48 minutes; recycled
// once, keeping its claim; woken at the next generation, nudged on a fresh
// grace six times more; then escalated, and nudged hourly for as long as it
// stays silent. Rolled back, today's engine sends three nudges and then
// nothing, which is the wedge.
func TestWorkLadderReplaysTheWedge(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("the step")
	c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id}) // the hook's stamp: a seed
	c.run(8*time.Hour, 30*time.Second, func() {
		if len(c.posted) > 0 && c.row().Metadata["generation"] == "7" {
			c.setRow(map[string]string{"generation": "8", "instance_token": "tok-8"}) // A16's drain, and the wake onto the kept claim
		}
	})
	want := []time.Duration{
		halfPast(1), halfPast(4), halfPast(10), halfPast(22), halfPast(46), halfPast(94),
		halfPast(156), halfPast(159), halfPast(165), halfPast(177), halfPast(201), halfPast(249),
		halfPast(309), halfPast(369), halfPast(429),
	}
	if fmt.Sprint(c.nudgedAt) != fmt.Sprint(want) {
		t.Fatalf("nudges at\n %v\nwant\n %v", c.nudgedAt, want)
	}
	if len(c.posted) != 1 || c.posted[0].ID != c.seat || c.posted[0].Generation != "7" {
		t.Fatalf("recycles posted %+v, want one at generation 7", c.posted)
	}
	if got := len(c.events(events.WorkRecycleRequested)); got != 1 {
		t.Fatalf("work.recycle_requested = %d, want 1", got)
	}
	if got := escalations(c); fmt.Sprint(got) != fmt.Sprint([]string{workladder.AlertStartedRecycled, workladder.AlertStartedUnanswered}) {
		t.Fatalf("work.escalated reasons = %v, want the recycle's alert, then started_unanswered", got)
	}
	if e := c.entry(id); e.Rung != workladder.RungEscalated || e.Recycles != 1 {
		t.Fatalf("entry %+v, want escalated after one recycle", e)
	}
	if b, _ := c.work.Get(id); b.Status != "in_progress" || b.Assignee != c.seat {
		t.Fatalf("claim %s %q %q: a started claim must stay on its seat", id, b.Status, b.Assignee)
	}
	last := c.sp.Calls[len(c.sp.Calls)-1]
	for i := len(c.sp.Calls) - 1; i >= 0; i-- {
		if c.sp.Calls[i].Method == "Nudge" {
			last = c.sp.Calls[i]
			break
		}
	}
	if !strings.Contains(last.Message, id) || strings.Contains(last.Message, "--drain-ack") {
		t.Fatalf("started nudge text %q: want the built-in text naming %s, never --drain-ack", last.Message, id)
	}

	old := newLadderCity(t)
	old.env.Cfg.Daemon.WorkLadder = "off"
	oid := old.claim("the step")
	old.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: oid})
	old.run(8*time.Hour, 30*time.Second, nil)
	if len(old.nudgedAt) > idleClaimNudgeMaxAttempts {
		t.Fatalf("rolled back: %d nudges; the replay assumes today's engine stops at %d", len(old.nudgedAt), idleClaimNudgeMaxAttempts)
	}
}

// L2. An unstarted claim on a quiet seat: three nudges, then the release
// CAS, the claim given back (open, unassigned) and the release cleared; the
// seat is neither stopped nor recycled.
func TestWorkLadderReleasesAnUnstartedClaim(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("never started")
	c.run(30*time.Minute, 30*time.Second, nil)
	if fmt.Sprint(c.nudgedAt) != fmt.Sprint([]time.Duration{halfPast(1), halfPast(4), halfPast(10)}) {
		t.Fatalf("nudges at %v, want 1m30s 4m30s 10m30s", c.nudgedAt)
	}
	b, _ := c.work.Get(id)
	if b.Status != "open" || b.Assignee != "" {
		t.Fatalf("claim %q assignee %q, want released", b.Status, b.Assignee)
	}
	if rel := c.events(events.WorkReleased); len(rel) != 1 || rel[0].Subject != id {
		t.Fatalf("work.released = %+v, want one for %s", rel, id)
	}
	if cell := c.cell(); cell.Release != nil || len(cell.Entries) != 0 {
		t.Fatalf("cell %s, want the release cleared and the entry gone", c.row().Metadata[session.WorkLadderKey])
	}
	if len(c.posted) != 0 || c.row().Status == "closed" {
		t.Fatalf("posted %+v, row %s: a release must not stop the seat", c.posted, c.row().Status)
	}
	if got := c.sp.CountCalls("Stop", ladderSeatName); got != 0 {
		t.Fatalf("stops = %d, want none", got)
	}
}

// agentRaceStore is a session store whose first CAS on row runs agent first: the
// agent's own write lands between the ladder's read and its CAS.
type agentRaceStore struct {
	*beads.MemStore
	row   string
	agent func()
	fired atomic.Bool
}

func (s *agentRaceStore) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	if id == s.row && s.fired.CompareAndSwap(false, true) {
		s.agent()
	}
	return s.MemStore.UpdateIfMatch(id, rev, opts)
}

// L3. The agent's start write lands between the ladder's decision to
// release and its CAS: the CAS loses, decides again on the fresh row, and
// writes the claim started, not the release. The claim stays.
func TestWorkLadderReleaseLosesToTheAgentsStart(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("started just now")
	key := workladder.ClaimKey{Store: "city", Bead: id}
	prev := workladder.Record{V: 1, Gen: 7, Entries: []workladder.Entry{{
		Key: key, Kind: workladder.KindClaim, Rung: workladder.RungClaimed, Since: c.now.Add(-time.Hour), Attempts: 3, Last: c.now.Add(-time.Hour),
	}}}
	c.setRow(map[string]string{session.WorkLadderKey: workladder.EncodeLadder(prev)})
	race := &agentRaceStore{MemStore: c.sessions, row: c.seat, agent: func() {
		started := workladder.EncodeStarted(workladder.Started{V: 1, Claims: []workladder.StartedClaim{{Key: key, Gen: 7, At: c.now, Via: workladder.ViaClaim}}})
		casSetRow(t, c.sessions, c.seat, map[string]string{session.WorkStartedKey: started})
	}}
	c.env.CityStore = blindWriteRefusingStore{inner: race}
	c.pass()
	if !race.fired.Load() {
		t.Fatal("the ladder never tried its CAS")
	}
	if b, _ := c.work.Get(id); b.Status != "in_progress" {
		t.Fatalf("claim %q: a release raced the start and won", b.Status)
	}
	if cell := c.cell(); cell.Release != nil || c.entry(id).Rung != workladder.RungStarted {
		t.Fatalf("cell %s, want the claim started and no release", c.row().Metadata[session.WorkLadderKey])
	}
	if got := len(c.events(events.WorkReleased)); got != 0 {
		t.Fatalf("work.released = %d", got)
	}
}

// A release that ran to an outcome is cleared, so the hook may hand the
// claim out again: here no lane serves the claim's route, so the release
// keeps it on the seat, and the ladder tries again only a backoff later.
func TestWorkLadderClearsAReleaseThatKeptTheClaim(t *testing.T) {
	c := newLadderCity(t)
	b, err := c.work.Create(beads.Bead{Title: "unserved", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "nowhere/lane"}})
	if err != nil {
		t.Fatal(err)
	}
	inProgress := "in_progress"
	if err := c.work.Update(b.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &c.seat}); err != nil {
		t.Fatal(err)
	}
	c.claims = append(c.claims, b.ID)
	key := workladder.ClaimKey{Store: "city", Bead: b.ID}
	prev := workladder.Record{V: 1, Gen: 7, Entries: []workladder.Entry{{
		Key: key, Kind: workladder.KindClaim, Rung: workladder.RungClaimed, Since: c.now.Add(-time.Hour), Attempts: 3, Last: c.now.Add(-time.Hour),
	}}}
	c.setRow(map[string]string{session.WorkLadderKey: workladder.EncodeLadder(prev)})
	c.pass()
	if got, _ := c.work.Get(b.ID); got.Status != "in_progress" {
		t.Fatalf("claim %q, want kept: no lane serves it", got.Status)
	}
	if r := c.cell().Release; r != nil {
		t.Fatalf("release %+v left standing after it kept the claim", r)
	}
	if e := c.entry(b.ID); !e.Last.Equal(c.now) {
		t.Fatalf("entry %+v, want its last stamped by the release", e)
	}
}

// A seed that names a claim (the hook's current-claim stamp) proves it
// started at the release CAS too, even on a generation the ladder already
// seeded: the release turns into a started claim, never a release.
func TestWorkLadderReleaseRefusesASeededClaim(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("stamped, cell unrecorded")
	key := workladder.ClaimKey{Store: "city", Bead: id}
	prev := workladder.Record{V: 1, Gen: 7, Entries: []workladder.Entry{{
		Key: key, Kind: workladder.KindClaim, Rung: workladder.RungClaimed, Since: c.now.Add(-time.Hour), Attempts: 3, Last: c.now.Add(-time.Hour),
	}}}
	c.setRow(map[string]string{session.WorkLadderKey: workladder.EncodeLadder(prev), beadmeta.CurrentClaimBeadIDMetadataKey: id})
	c.pass()
	if b, _ := c.work.Get(id); b.Status != "in_progress" {
		t.Fatalf("claim %q, want kept", b.Status)
	}
	if e := c.entry(id); e.Rung != workladder.RungStarted || e.Seed != workladder.SrcCurrentClaim || c.cell().Release != nil {
		t.Fatalf("cell %s, want the claim started by its seed", c.row().Metadata[session.WorkLadderKey])
	}
}

// L4. A started claim at its threshold is recycled once, keeping its claim;
// woken at the next generation its count restarts and its recycle is carried,
// so when the new incarnation's nudges go unanswered it escalates without a
// second recycle.
func TestWorkLadderRecyclesOnceThenEscalates(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("long silent")
	key := workladder.ClaimKey{Store: "city", Bead: id}
	agent := workladder.EncodeStarted(workladder.Started{V: 1, Claims: []workladder.StartedClaim{{Key: key, Gen: 7, Via: workladder.ViaClaim}}})
	prev := workladder.Record{V: 1, Gen: 7, Entries: []workladder.Entry{{
		Key: key, Kind: workladder.KindClaim, Rung: workladder.RungStarted, Since: c.now.Add(-3 * time.Hour), Attempts: 6, Last: c.now.Add(-61 * time.Minute), Seed: workladder.SrcAgent,
	}}}
	c.setRow(map[string]string{session.WorkLadderKey: workladder.EncodeLadder(prev), session.WorkStartedKey: agent})
	c.pass()
	if len(c.posted) != 1 || c.posted[0].Generation != "7" {
		t.Fatalf("posted %+v, want one recycle at generation 7", c.posted)
	}
	if b, _ := c.work.Get(id); b.Status != "in_progress" || b.Assignee != c.seat {
		t.Fatalf("claim %q %q: the recycle keeps it", b.Status, b.Assignee)
	}
	c.setRow(map[string]string{"generation": "8", "instance_token": "tok-8"}) // stopped, asleep holding the claim, woken
	c.now = c.now.Add(30 * time.Second)
	c.pass()
	if e := c.entry(id); e.Attempts != 0 || e.Recycles != 1 || c.cell().Gen != 8 {
		t.Fatalf("after the wake: %+v at gen %d, want a fresh count and the recycle carried", e, c.cell().Gen)
	}
	c.run(4*time.Hour, time.Minute, nil)
	if len(c.posted) != 1 {
		t.Fatalf("posted %d recycles, want R = 1", len(c.posted))
	}
	if e := c.entry(id); e.Rung != workladder.RungEscalated {
		t.Fatalf("entry %+v, want escalated", e)
	}
	if got := escalations(c); fmt.Sprint(got) != fmt.Sprint([]string{workladder.AlertStartedRecycled, workladder.AlertStartedUnanswered}) {
		t.Fatalf("work.escalated reasons = %v, want one recycle alert and one escalation", got)
	}
}

// L5 (WL8). Forty seats carrying the backstops' pacing at its cap: no
// recycle and no release in the first ten minutes after deploy; the old
// counts are never read, so each seat starts on its own grace.
func TestWorkLadderSeedsWithoutADrainWave(t *testing.T) {
	c := newLadderCity(t)
	sessions := c.sessions
	old := c.now.Add(-2 * time.Hour).Format(time.RFC3339)
	var unstarted []string
	for i := range 40 {
		name := fmt.Sprintf("seat-%02d", i)
		if err := c.sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		c.sp.SetActivity(name, c.now.Add(-time.Hour))
		row, err := sessions.Create(beads.Bead{Title: name, Type: "session", Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
			"session_name": name, "pool_managed": "true", "template": "worker", "state": "active", "generation": "3", "instance_token": "tok-3",
			idleClaimNudgeCountKey: "3", idleClaimNudgeAtKey: old, idleClaimNudgeTokenKey: "tok-3", idleClaimNudgeStalledKey: old, idleClaimNudgeStalledTokenKey: "tok-3",
			executionClaimNudgeCountKey: "3", executionClaimNudgeAtKey: old, executionClaimNudgeTokenKey: "tok-3",
			executionClaimNudgeStalledKey: old, executionClaimNudgeStalledTokenKey: "tok-3", continuationClaimNudgeCountKey: "3",
		}})
		if err != nil {
			t.Fatal(err)
		}
		b, err := c.work.Create(beads.Bead{Title: name, Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}})
		if err != nil {
			t.Fatal(err)
		}
		inProgress := "in_progress"
		if err := c.work.Update(b.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &row.ID}); err != nil {
			t.Fatal(err)
		}
		c.claims = append(c.claims, b.ID)
		markers := map[string]string{executionClaimNudgeWorkKey: b.ID, executionClaimNudgeStoreRefKey: "city"}
		if i%2 == 0 {
			markers[executionClaimStartedKey] = "tok-3|" + b.ID
		} else {
			unstarted = append(unstarted, b.ID)
		}
		if err := sessions.SetMetadataBatch(row.ID, markers); err != nil {
			t.Fatal(err)
		}
	}
	c.run(10*time.Minute, 30*time.Second, nil)
	if len(c.posted) != 0 {
		t.Fatalf("recycles posted in the first ten minutes: %+v", c.posted)
	}
	for _, id := range c.claims {
		if b, _ := c.work.Get(id); b.Status != "in_progress" {
			t.Fatalf("claim %s released in the first ten minutes", id)
		}
	}
	if n := len(c.events(events.WorkReleased)); n != 0 {
		t.Fatalf("work.released = %d in the first ten minutes", n)
	}
	// They are paced, not forgotten: the unstarted are released on their own
	// schedule once their three nudges go unanswered.
	c.run(15*time.Minute, 30*time.Second, nil)
	for _, id := range unstarted {
		if b, _ := c.work.Get(id); b.Status != "open" {
			t.Fatalf("unstarted claim %s %q after its third nudge went unanswered, want released", id, b.Status)
		}
	}
}

// L6 (WL4). With the seat's provider red in provider-health.json, a started
// claim's nudges go on, paused, at most ten minutes apart, and nothing
// escalates: no recycle, no alert, the count frozen.
func TestWorkLadderPausesWhileTheProviderIsDegraded(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("waiting out an outage")
	c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id})
	writeHealth := func() {
		dir := filepath.Join(c.env.CityPath, ".gc", "cache")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(providerHealthFileFormat{Providers: []providerHealthRecord{{Provider: "claude", Status: "unhealthy", ProbedAt: float64(time.Now().Unix())}}})
		if err := os.WriteFile(filepath.Join(dir, "provider-health.json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeHealth()
	c.run(5*time.Hour, 30*time.Second, writeHealth)
	if len(c.nudgedAt) < 5*60/10 {
		t.Fatalf("%d nudges in 5h of outage, want one at least every 10 minutes", len(c.nudgedAt))
	}
	for i := 1; i < len(c.nudgedAt); i++ {
		if gap := c.nudgedAt[i] - c.nudgedAt[i-1]; gap > workladder.DefaultPausedCap {
			t.Fatalf("a %v gap between paused nudges", gap)
		}
	}
	if len(c.posted) != 0 || len(c.events(events.WorkEscalated)) != 0 {
		t.Fatalf("escalated while degraded: posted %+v", c.posted)
	}
	if e := c.entry(id); e.Attempts != 0 || e.Paused == 0 {
		t.Fatalf("entry %+v, want only paused nudges", e)
	}
	for _, ev := range c.events(events.WorkNudged) {
		var p events.WorkLadderPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil || !p.Paused {
			t.Fatalf("work.nudged %s, want paused", ev.Payload)
		}
	}
}

// L7 (WL5). A held gate gets no nudge however long the seat is quiet: a
// stop request at the row's generation, a kill in progress, an agent's
// heartbeat hold, a wait hold, a start in flight.
func TestWorkLadderHoldsUnderAGate(t *testing.T) {
	for name, meta := range map[string]func(now time.Time) map[string]string{
		"draining": func(now time.Time) map[string]string {
			return map[string]string{session.DrainIntentReasonKey: drainOrphaned, session.DrainIntentAtKey: now.Format(time.RFC3339), session.DrainIntentIncarnationKey: "7"}
		},
		"kill pending": func(now time.Time) map[string]string {
			return map[string]string{"state": "asleep", "state_reason": session.KillPendingReason, "sleep_reason": "killed", "slept_at": now.Format(time.RFC3339)}
		},
		"heartbeat hold": func(now time.Time) map[string]string {
			return map[string]string{"held_until": now.Add(6 * time.Hour).Format(time.RFC3339)}
		},
		"wait hold":         func(time.Time) map[string]string { return map[string]string{"wait_hold": "true"} },
		"restart requested": func(time.Time) map[string]string { return map[string]string{"restart_requested": "true"} },
		"quarantined": func(now time.Time) map[string]string {
			return map[string]string{"quarantined_until": now.Add(6 * time.Hour).Format(time.RFC3339)}
		},
		"starting":  func(time.Time) map[string]string { return map[string]string{"state": "creating"} },
		"suspended": func(time.Time) map[string]string { return map[string]string{"sleep_intent": "user-hold"} },
	} {
		t.Run(name, func(t *testing.T) {
			c := newLadderCity(t)
			c.claim("held")
			c.setRow(meta(c.now))
			hold := 2 * time.Hour
			if name == "kill pending" {
				hold = session.KillPendingGrace - time.Minute // the fence is honored only within its grace
			}
			c.run(hold, 30*time.Second, nil)
			if len(c.nudgedAt) != 0 || len(c.posted) != 0 || len(c.events(events.WorkReleased)) != 0 {
				t.Fatalf("nudges %v, posted %+v under a %s gate", c.nudgedAt, c.posted, name)
			}
		})
	}
}

// Residue of a past hold on a running row is no gate: a sleep_reason left
// from a kill, a pending-create claim on an active row, a heartbeat hold
// already past, a stop request of an earlier generation.
func TestWorkLadderIgnoresHoldResidue(t *testing.T) {
	c := newLadderCity(t)
	c.claim("residue")
	c.setRow(map[string]string{
		"sleep_reason": "killed", "pending_create_claim": "true", "held_until": c.now.Add(-time.Hour).Format(time.RFC3339),
		session.DrainIntentReasonKey: drainOrphaned, session.DrainIntentAtKey: c.now.Format(time.RFC3339), session.DrainIntentIncarnationKey: "6",
	})
	c.run(5*time.Minute, 30*time.Second, nil)
	if len(c.nudgedAt) != 2 {
		t.Fatalf("nudges %v, want the ladder's first two", c.nudgedAt)
	}
}

// staleOnceStore reads the seat's row once with revision 0, the shape a
// CachingStore that has not yet loaded the revision serves (mc-3c8h5); a
// precondition failure evicts it, so the next read is fresh.
type staleOnceStore struct {
	*beads.MemStore
	row       string
	served    atomic.Bool
	casCalls  atomic.Int32
	casLanded atomic.Int32
}

func (s *staleOnceStore) Get(id string) (beads.Bead, error) {
	b, err := s.MemStore.Get(id)
	if id == s.row && s.served.CompareAndSwap(false, true) {
		b.Revision = 0
	}
	return b, err
}

func (s *staleOnceStore) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	s.casCalls.Add(1)
	err := s.MemStore.UpdateIfMatch(id, rev, opts)
	if err == nil {
		s.casLanded.Add(1)
	}
	return err
}

// L9. A stale read's CAS fails on its revision; the commit reads again and
// lands on the second attempt.
func TestWorkLadderCommitRetriesAStaleRevision(t *testing.T) {
	c := newLadderCity(t)
	c.claim("any")
	stale := &staleOnceStore{MemStore: c.sessions, row: c.seat}
	c.env.CityStore = blindWriteRefusingStore{inner: stale}
	c.pass()
	if stale.casCalls.Load() != 2 || stale.casLanded.Load() != 1 {
		t.Fatalf("CAS calls %d, landed %d; want the stale one refused and the second landed", stale.casCalls.Load(), stale.casLanded.Load())
	}
	if c.cell().V != 1 {
		t.Fatal("the cell was not written")
	}
}

// L10. [daemon] work_ladder = "off" runs the three backstops again, which
// write their markers and no ladder cell; on, the ladder writes its cell and
// no marker. Both read the same allocation.
func TestWorkLadderSwitch(t *testing.T) {
	for _, mode := range []string{"on", "off"} {
		t.Run(mode, func(t *testing.T) {
			env, store := nudgeStepsFixture(t)
			env.Cfg.Daemon.WorkLadder = mode
			if mode == "on" {
				stampedMemStore(t, store)
			}
			lane, _ := newPoolStepsTestLane(env, func() *allocSummary {
				return &allocSummary{At: stepsTestNow, AssignedWork: []beads.Bead{{ID: "work-a", Status: "open"}}}
			}, nil)
			lane.workLadder(context.Background(), env)
			row := mustGetTestBead(t, store, "session-bead-a")
			marker, cell := row.Metadata[idleClaimNudgeTriggerKey], row.Metadata[session.WorkLadderKey]
			if (mode == "off") != (marker == "work-a") || (mode == "on") != (cell != "") {
				t.Fatalf("mode %s: marker %q, cell %q", mode, marker, cell)
			}
		})
	}
}

// The ladder's trigger entry is the slot's own trigger from the last
// allocation (assigned, or ready routed), and nothing from no allocation or
// a stale one.
func TestWorkLadderUsesLastAllocation(t *testing.T) {
	work := []beads.Bead{{ID: "work-a", Status: "open"}}
	for _, tc := range []struct {
		name string
		sum  *allocSummary
		want bool
	}{
		{name: "assigned", sum: &allocSummary{At: stepsTestNow, AssignedWork: work}, want: true},
		{name: "ready routed", sum: &allocSummary{At: stepsTestNow, ReadyRouted: work, ReadyRoutedRefs: []string{""}}, want: true},
		{name: "no allocation"},
		{name: "stale", sum: &allocSummary{At: stepsTestNow.Add(-externalReadsFreshPatrols*backstopTestInterval - time.Second), AssignedWork: work}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, store := nudgeStepsFixture(t)
			stampedMemStore(t, store)
			lane, _ := newPoolStepsTestLane(env, func() *allocSummary { return tc.sum }, nil)
			lane.workLadder(context.Background(), env)
			r, err := workladder.DecodeLadder(mustGetTestBead(t, store, "session-bead-a").Metadata[session.WorkLadderKey])
			got := err == nil && len(r.Entries) == 1 && r.Entries[0].Key.Bead == "work-a" && r.Entries[0].Kind == workladder.KindTrigger
			if got != tc.want {
				t.Fatalf("cell %+v (%v), want a trigger entry: %v", r, err, tc.want)
			}
		})
	}
}

// A trigger that stays unclaimed is nudged with the agent's configured
// nudge, then recycled through the planner's inbox after its third nudge
// goes unanswered, once its live read still finds it open and unclaimed.
func TestWorkLadderRecyclesAnUnclaimedTrigger(t *testing.T) {
	c := newLadderCity(t)
	b, err := c.work.Create(beads.Bead{Title: "trigger", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	c.trigger = b.ID
	c.env.Cfg.Agents[0].Nudge = "configured claim nudge"
	c.setRow(map[string]string{beadmeta.TriggerBeadIDMetadataKey: b.ID})
	c.run(25*time.Minute, 30*time.Second, nil)
	if len(c.nudgedAt) != 4 || len(c.posted) != 1 {
		t.Fatalf("nudges %v, posted %+v; want three, then a recycle with the fourth", c.nudgedAt, c.posted)
	}
	for _, call := range c.sp.Calls {
		if call.Method == "Nudge" && call.Message != "configured claim nudge" {
			t.Fatalf("trigger nudge %q, want the configured one", call.Message)
		}
	}
}

// A recycle, and a nudge, re-read the trigger live first: one the agent
// claimed since the allocation was taken is neither recycled nor nudged,
// however long a stale allocation still shows it open.
func TestWorkLadderRecycleRereadsTheTrigger(t *testing.T) {
	c := newLadderCity(t)
	b, err := c.work.Create(beads.Bead{Title: "trigger", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	c.trigger = b.ID
	c.setRow(map[string]string{beadmeta.TriggerBeadIDMetadataKey: b.ID})
	c.run(15*time.Minute, 30*time.Second, nil)
	inProgress := "in_progress"
	if err := c.work.Update(b.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &c.seat}); err != nil {
		t.Fatal(err)
	}
	nudged := len(c.nudgedAt)
	c.run(time.Hour, 30*time.Second, nil) // the allocation still shows it open
	if len(c.posted) != 0 || len(c.nudgedAt) != nudged {
		t.Fatalf("posted %+v, nudges %v after the agent claimed the trigger", c.posted, c.nudgedAt[nudged:])
	}
}

// A nudge the runtime cannot take (runtime.ErrRuntimeUnavailable) is
// counted as paused, not as an attempt, and recorded as work.nudge_failed.
func TestWorkLadderRepausesAnUnavailableRuntime(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("any")
	c.sp.NudgeErrors = map[string]error{ladderSeatName: fmt.Errorf("pane gone: %w", runtime.ErrRuntimeUnavailable)}
	c.now = c.now.Add(-2 * time.Minute)
	c.pass() // observe
	c.now = c.now.Add(2 * time.Minute)
	c.pass() // the nudge, refused
	if e := c.entry(id); e.Attempts != 0 || e.Paused != 1 {
		t.Fatalf("entry %+v, want the attempt re-counted as paused", e)
	}
	if got := len(c.events(events.WorkNudgeFailed)); got != 1 {
		t.Fatalf("work.nudge_failed = %d, want 1", got)
	}
}

// Under an incomplete allocation the ladder keeps the work its cell holds
// that the snapshot lacks: no entry is dropped, no count reset.
func TestWorkLadderCarriesWorkUnderAPartialSnapshot(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("on a store that did not answer")
	c.run(5*time.Minute, 30*time.Second, nil)
	before := c.entry(id)
	c.partial = true
	inProgress := "in_progress"
	if err := c.work.Update(id, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatal(err)
	}
	c.claims = nil // the snapshot lost it
	c.now = c.now.Add(30 * time.Second)
	c.pass()
	if after := c.entry(id); after.Attempts != before.Attempts || !after.Since.Equal(before.Since) {
		t.Fatalf("entry %+v, want %+v carried", after, before)
	}
}

// staleListStore lists the session rows from snapshot (a read taken before
// the row moved, as a cached list can be) and reads and writes them live.
type staleListStore struct {
	*beads.MemStore
	snapshot *beads.MemStore
}

func (s staleListStore) List(q beads.ListQuery) ([]beads.Bead, error) { return s.snapshot.List(q) }

// snapshotOf is a store holding a copy of row id as it stands.
func snapshotOf(t *testing.T, store *beads.MemStore, id string) *beads.MemStore {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	b.Metadata = maps.Clone(b.Metadata)
	return beads.NewMemStoreFrom(0, []beads.Bead{b}, nil)
}

// ladderListFailingStore fails every listing: a seat's claims cannot be read.
type ladderListFailingStore struct{ *beads.MemStore }

func (ladderListFailingStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, fmt.Errorf("store down")
}

func (ladderListFailingStore) ListByAssignee(string, string, int) ([]beads.Bead, error) {
	return nil, fmt.Errorf("store down")
}

// unservedClaim is an in_progress claim the seat holds whose route no lane
// serves: its release keeps it.
func (c *ladderCity) unservedClaim() string {
	c.t.Helper()
	b, err := c.work.Create(beads.Bead{Title: "unserved", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "nowhere/lane"}})
	if err != nil {
		c.t.Fatal(err)
	}
	inProgress := "in_progress"
	if err := c.work.Update(b.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &c.seat}); err != nil {
		c.t.Fatal(err)
	}
	c.claims = append(c.claims, b.ID)
	return b.ID
}

// A release that keeps its claim (no lane serves the route) never leaves it
// silent: the claim is nudged hourly from then on, with one
// release_unfinished alert, and the release is retried at the same pace,
// each attempt recorded as work.release_unfinished.
func TestWorkLadderKeptReleaseKeepsNudging(t *testing.T) {
	c := newLadderCity(t)
	id := c.unservedClaim()
	c.run(5*time.Hour, 30*time.Second, nil)
	want := []time.Duration{halfPast(1), halfPast(4), halfPast(10), halfPast(82), halfPast(142), halfPast(202), halfPast(262)}
	if fmt.Sprint(c.nudgedAt) != fmt.Sprint(want) {
		t.Fatalf("nudges at %v, want %v: hourly after the kept release at 22m30s", c.nudgedAt, want)
	}
	if got := escalations(c); fmt.Sprint(got) != fmt.Sprint([]string{workladder.AlertReleaseUnfinished}) {
		t.Fatalf("work.escalated = %v, want one release_unfinished", got)
	}
	if n := len(c.events(events.WorkReleaseUnfinished)); n != 5 {
		t.Fatalf("work.release_unfinished = %d, want one per attempt (5)", n)
	}
	if b, _ := c.work.Get(id); b.Status != "in_progress" {
		t.Fatalf("claim %q, want kept", b.Status)
	}
}

// A release that fails (its read of the seat's claims errs) is settled as
// unfinished, not left pending: the mark is cleared (the hook may hand the
// claim out), the entry waits a Cap, and the failure is recorded.
func TestWorkLadderFailedReleaseBacksOff(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("on a store that cannot be listed")
	c.env.WorkLegs = testWorkLegs(c.env.CityPath, c.env.Cfg, ladderListFailingStore{MemStore: c.work}, nil)
	key := workladder.ClaimKey{Store: "city", Bead: id}
	c.setRow(map[string]string{session.WorkLadderKey: workladder.EncodeLadder(workladder.Record{V: 1, Gen: 7, Entries: []workladder.Entry{{
		Key: key, Kind: workladder.KindClaim, Rung: workladder.RungClaimed, Since: c.now.Add(-time.Hour), Attempts: 3, Last: c.now.Add(-time.Hour),
	}}})})
	c.pass()
	if b, _ := c.work.Get(id); b.Status != "in_progress" {
		t.Fatalf("claim %q", b.Status)
	}
	if e := c.entry(id); c.cell().Release != nil || !e.Unreleased || !e.Last.Equal(c.now) {
		t.Fatalf("cell %s: want the release cleared and the entry backed off", c.row().Metadata[session.WorkLadderKey])
	}
	unf := c.events(events.WorkReleaseUnfinished)
	if len(unf) != 1 || !strings.Contains(unf[0].Message, "failed") {
		t.Fatalf("work.release_unfinished = %+v, want one failure", unf)
	}
}

// At deploy, a seat holding two claims the hook handed it (each bead's
// gc.session_id is the seat) seeds both as started, though the row's
// current_claim stamp names only one: neither is released.
func TestWorkLadderSeedsEveryHandedOutClaim(t *testing.T) {
	c := newLadderCity(t)
	older, newer := c.claim("handed out first"), c.claim("handed out second")
	for _, id := range []string{older, newer} {
		if err := c.work.SetMetadataBatch(id, map[string]string{beadmeta.SessionIDMetadataKey: c.seat}); err != nil {
			t.Fatal(err)
		}
	}
	c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: newer})
	c.run(time.Hour, 30*time.Second, nil)
	for id, src := range map[string]workladder.StartSource{older: workladder.SrcClaimSession, newer: workladder.SrcCurrentClaim} {
		if b, _ := c.work.Get(id); b.Status != "in_progress" {
			t.Fatalf("claim %s %q: a handed-out claim was released", id, b.Status)
		}
		if e := c.entry(id); e.Rung != workladder.RungStarted || e.Seed != src || !e.Since.Equal(c.start) {
			t.Fatalf("entry %+v, want started by %v from the first observation", e, src)
		}
	}
}

// A claim stamped with the seat's gc.session_id after the allocation was
// read (its snapshot lacks the stamp) is found started by the release's live
// read: not released, and started from then on.
func TestWorkLadderReleaseFindsTheHandOver(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("stamped after the allocation was read")
	c.lane.summary = func() *allocSummary {
		s := c.summary()
		for i := range s.AssignedWork {
			s.AssignedWork[i].Metadata = maps.Clone(s.AssignedWork[i].Metadata)
			delete(s.AssignedWork[i].Metadata, beadmeta.SessionIDMetadataKey)
		}
		return s
	}
	c.run(5*time.Minute, 30*time.Second, nil)
	if err := c.work.SetMetadataBatch(id, map[string]string{beadmeta.SessionIDMetadataKey: c.seat}); err != nil {
		t.Fatal(err)
	}
	c.run(30*time.Minute, 30*time.Second, nil)
	if b, _ := c.work.Get(id); b.Status != "in_progress" {
		t.Fatalf("claim %q: released though handed to the seat", b.Status)
	}
	if e := c.entry(id); e.Rung != workladder.RungStarted || e.Seed != workladder.SrcClaimSession {
		t.Fatalf("entry %+v, want started by the hand-over stamp", e)
	}
	if n := len(c.events(events.WorkReleased)); n != 0 {
		t.Fatalf("work.released = %d", n)
	}
}

// Of two unstarted claims, a release gives back only the one its CAS
// proved, whichever the seat's live read lists first; the other stays.
func TestWorkLadderReleasesOnlyTheProvenClaim(t *testing.T) {
	c := newLadderCity(t)
	first, second := c.claim("first"), c.claim("second")
	due := workladder.Entry{
		Key: workladder.ClaimKey{Store: "city", Bead: second}, Kind: workladder.KindClaim, Rung: workladder.RungClaimed,
		Since: c.now.Add(-time.Hour), Attempts: 3, Last: c.now.Add(-time.Hour),
	}
	fresh := workladder.Entry{Key: workladder.ClaimKey{Store: "city", Bead: first}, Kind: workladder.KindClaim, Rung: workladder.RungClaimed, Since: c.now}
	c.setRow(map[string]string{session.WorkLadderKey: workladder.EncodeLadder(workladder.Record{V: 1, Gen: 7, Entries: []workladder.Entry{fresh, due}})})
	c.pass()
	if b, _ := c.work.Get(second); b.Status != "open" {
		t.Fatalf("the proven claim %s %q, want released", second, b.Status)
	}
	if b, _ := c.work.Get(first); b.Status != "in_progress" {
		t.Fatalf("the other claim %s %q, want kept", first, b.Status)
	}
}

// A release whose retry gives the claim back sends no nudge about it in the
// same pass: there is nothing left to claim.
func TestWorkLadderNoNudgeAboutAReleasedClaim(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("route fixed since")
	c.setRow(map[string]string{session.WorkLadderKey: workladder.EncodeLadder(workladder.Record{V: 1, Gen: 7, Entries: []workladder.Entry{{
		Key: workladder.ClaimKey{Store: "city", Bead: id}, Kind: workladder.KindClaim, Rung: workladder.RungClaimed,
		Since: c.now.Add(-2 * time.Hour), Attempts: 3, Last: c.now.Add(-61 * time.Minute), Unreleased: true,
	}}})})
	c.pass()
	if b, _ := c.work.Get(id); b.Status != "open" {
		t.Fatalf("claim %q, want the retry to release it", b.Status)
	}
	if len(c.nudgedAt) != 0 {
		t.Fatalf("nudged %v about a claim the same pass gave back", c.nudgedAt)
	}
}

// The hook's current-claim stamp landing between the ladder's read and its
// release CAS is seen by the CAS: the claim is started, not released.
func TestWorkLadderReleaseSeesASeedInItsCASWindow(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("stamped just now")
	key := workladder.ClaimKey{Store: "city", Bead: id}
	c.setRow(map[string]string{session.WorkLadderKey: workladder.EncodeLadder(workladder.Record{V: 1, Gen: 7, Entries: []workladder.Entry{{
		Key: key, Kind: workladder.KindClaim, Rung: workladder.RungClaimed, Since: c.now.Add(-time.Hour), Attempts: 3, Last: c.now.Add(-time.Hour),
	}}})})
	race := &agentRaceStore{MemStore: c.sessions, row: c.seat, agent: func() {
		casSetRow(t, c.sessions, c.seat, map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id})
	}}
	c.env.CityStore = blindWriteRefusingStore{inner: race}
	c.pass()
	if b, _ := c.work.Get(id); b.Status != "in_progress" {
		t.Fatalf("claim %q: released over a seed that landed in the CAS window", b.Status)
	}
	if e := c.entry(id); e.Rung != workladder.RungStarted || e.Seed != workladder.SrcCurrentClaim || c.cell().Release != nil {
		t.Fatalf("cell %s", c.row().Metadata[session.WorkLadderKey])
	}
}

// Each seed names a claim started at deploy, so it is nudged as started and
// never released.
func TestWorkLadderSeeds(t *testing.T) {
	for name, tc := range map[string]struct {
		meta func(id string) map[string]string
		src  workladder.StartSource
	}{
		"execution_claim_started": {func(id string) map[string]string { return map[string]string{executionClaimStartedKey: "tok-7|" + id} }, workladder.SrcExecutionClaimStarted},
		"current_claim": {func(id string) map[string]string {
			return map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id}
		}, workladder.SrcCurrentClaim},
		"currently_processing": {func(id string) map[string]string { return map[string]string{session.CurrentBeadIDKey: id} }, workladder.SrcCurrentlyProcessing},
		"agent cell, any store": {func(id string) map[string]string {
			return map[string]string{session.WorkStartedKey: session.EncodeWorkStarted(session.WorkStarted{}.With(session.WorkStartedEntry{Bead: id, Gen: 7, At: stepsTestNow, Via: session.WorkStartedViaClaim}))}
		}, workladder.SrcAgent},
	} {
		t.Run(name, func(t *testing.T) {
			c := newLadderCity(t)
			id := c.claim("seeded")
			c.setRow(tc.meta(id))
			c.run(time.Hour, 30*time.Second, nil)
			if b, _ := c.work.Get(id); b.Status != "in_progress" {
				t.Fatalf("claim %q: a seeded claim was released", b.Status)
			}
			if e := c.entry(id); e.Rung != workladder.RungStarted || e.Seed != tc.src {
				t.Fatalf("entry %+v, want started by %v", e, tc.src)
			}
		})
	}
	// A stamp of another incarnation is no evidence.
	c := newLadderCity(t)
	id := c.claim("stamped by a predecessor")
	c.setRow(map[string]string{executionClaimStartedKey: "tok-6|" + id})
	c.run(30*time.Minute, 30*time.Second, nil)
	if b, _ := c.work.Get(id); b.Status != "open" {
		t.Fatalf("claim %q: another incarnation's stamp kept it", b.Status)
	}
}

// The started text is built in: a configured nudge never replaces it.
func TestWorkLadderStartedTextIsBuiltIn(t *testing.T) {
	c := newLadderCity(t)
	c.env.Cfg.Agents[0].Nudge = "configured: gc hook --claim --drain-ack --json"
	id := c.claim("started")
	c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id})
	c.run(5*time.Minute, 30*time.Second, nil)
	for _, call := range c.sp.Calls {
		if call.Method == "Nudge" && call.Message != fmt.Sprintf(ladderStartedNudge, id) {
			t.Fatalf("started nudge %q, want the built-in text", call.Message)
		}
	}
	if len(c.nudgedAt) == 0 {
		t.Fatal("no nudge")
	}
}

// A seat is quiet only on a readable activity clock: no activity reading
// sends no nudge.
func TestWorkLadderNeedsAQuietClock(t *testing.T) {
	c := newLadderCity(t)
	c.claim("any")
	c.sp.SetActivity(ladderSeatName, time.Time{})
	c.run(30*time.Minute, 30*time.Second, nil)
	if len(c.nudgedAt) != 0 {
		t.Fatalf("nudges %v with no activity reading", c.nudgedAt)
	}
}

// busySnapshot is a runtime whose pane still shows its provider's busy
// indicator.
type busySnapshot struct{ *runtime.Fake }

func (busySnapshot) SnapshotIdle(string) (bool, error) { return false, nil }

// A busy-readable provider's busy indicator vetoes a stale activity clock.
func TestWorkLadderHonorsTheBusySnapshot(t *testing.T) {
	c := newLadderCity(t)
	c.claim("a long turn")
	c.env.SP = busySnapshot{Fake: c.sp}
	c.run(30*time.Minute, 30*time.Second, nil)
	if len(c.nudgedAt) != 0 {
		t.Fatalf("nudges %v while the pane shows busy", c.nudgedAt)
	}
}

// A recycle is posted at the generation the commit landed at, read fresh,
// not the one the pass listed.
func TestWorkLadderRecyclesAtTheCommittedGeneration(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("long silent")
	snap := snapshotOf(t, c.sessions, c.seat) // listed at generation 7
	key := workladder.ClaimKey{Store: "city", Bead: id}
	c.setRow(map[string]string{
		"generation": "8", "instance_token": "tok-8", beadmeta.CurrentClaimBeadIDMetadataKey: id,
		session.WorkLadderKey: workladder.EncodeLadder(workladder.Record{V: 1, Gen: 8, Entries: []workladder.Entry{{
			Key: key, Kind: workladder.KindClaim, Rung: workladder.RungStarted, Since: c.now.Add(-3 * time.Hour), Attempts: 6, Last: c.now.Add(-61 * time.Minute),
		}}}),
	})
	c.env.CityStore = blindWriteRefusingStore{inner: staleListStore{MemStore: c.sessions, snapshot: snap}}
	c.pass()
	if len(c.posted) != 1 || c.posted[0].Generation != "8" {
		t.Fatalf("posted %+v, want one recycle at generation 8", c.posted)
	}
}

// A row closed between the list and the commit gets no write and no act.
func TestWorkLadderCommitSkipsAClosedRow(t *testing.T) {
	c := newLadderCity(t)
	c.claim("any")
	c.now = c.now.Add(-5 * time.Minute)
	c.pass() // observed
	c.now = c.now.Add(5 * time.Minute)
	snap := snapshotOf(t, c.sessions, c.seat)
	before := c.row().Metadata[session.WorkLadderKey]
	if err := c.sessions.Close(c.seat); err != nil {
		t.Fatal(err)
	}
	c.env.CityStore = blindWriteRefusingStore{inner: staleListStore{MemStore: c.sessions, snapshot: snap}}
	c.pass()
	if len(c.nudgedAt) != 0 || c.row().Metadata[session.WorkLadderKey] != before {
		t.Fatalf("nudges %v, cell %s: acted on a closed row", c.nudgedAt, c.row().Metadata[session.WorkLadderKey])
	}
}

// getFailingStore fails every live read while fail is set.
type getFailingStore struct {
	beads.Store
	fail *bool
}

func (s getFailingStore) Get(id string) (beads.Bead, error) {
	if *s.fail {
		return beads.Bead{}, fmt.Errorf("store down")
	}
	return s.Store.Get(id)
}

// A claim whose live re-read cannot tell waits alone: it keeps its entry
// and its count, and the seat's other work is still nudged.
func TestWorkLadderHoldsOnlyTheUnreadableWork(t *testing.T) {
	c := newLadderCity(t)
	healthy := c.claim("on a healthy store")
	c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: healthy})
	other := beads.NewMemStore()
	ob, err := other.Create(beads.Bead{Title: "on a store whose reads fail", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	inProgress := "in_progress"
	if err := other.Update(ob.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &c.seat, Metadata: map[string]string{beadmeta.SessionIDMetadataKey: c.seat}}); err != nil {
		t.Fatal(err)
	}
	fail := false
	broken := getFailingStore{Store: other, fail: &fail}
	c.lane, _ = newPoolStepsTestLane(c.env, func() *allocSummary {
		s := c.summary()
		b, _ := other.Get(ob.ID)
		s.AssignedWork, s.AssignedStores, s.AssignedStoreRefs = append(s.AssignedWork, b), append(s.AssignedStores, broken), append(s.AssignedStoreRefs, "otherrig")
		return s
	}, func(r executionStalledRequest) { c.posted = append(c.posted, r) })
	c.lane.now = func() time.Time { return c.now }
	c.lane.events = c.rec
	c.run(10*time.Minute, 30*time.Second, nil)
	fail = true
	before := len(c.nudgedAt)
	held := workladder.ClaimKey{Store: "rig:otherrig", Bead: ob.ID}
	attempts := -1
	for _, e := range c.cell().Entries {
		if e.Key == held {
			attempts = e.Attempts
		}
	}
	c.run(3*time.Hour, 30*time.Second, nil)
	if len(c.nudgedAt) == before {
		t.Fatal("one unreadable claim silenced the whole seat")
	}
	found := false
	for _, e := range c.cell().Entries {
		if e.Key == held {
			found = true
			if e.Attempts != attempts {
				t.Fatalf("held entry %+v, want its count %d kept", e, attempts)
			}
		}
	}
	if !found {
		t.Fatal("the unreadable claim's entry was dropped")
	}
}

// Under an incomplete snapshot, carried work is re-read live before it is
// acted on: a claim the seat finished meanwhile is dropped, never nudged or
// recycled.
func TestWorkLadderRereadsCarriedWork(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("finished while a rig was down")
	c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id})
	c.run(2*time.Minute, 30*time.Second, nil)
	closed := "closed"
	if err := c.work.Update(id, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatal(err)
	}
	c.partial = true
	before := len(c.nudgedAt)
	c.run(4*time.Hour, 30*time.Second, nil)
	if len(c.nudgedAt) != before || len(c.posted) != 0 {
		t.Fatalf("a finished, carried claim: nudges %v, posted %+v", c.nudgedAt[before:], c.posted)
	}
	if len(c.cell().Entries) != 0 {
		t.Fatalf("cell %s, want the finished claim dropped", c.row().Metadata[session.WorkLadderKey])
	}
}

// A permission prompt (runtime.ErrPendingInteraction) is not the agent
// ignoring its work: the attempt is re-counted as paused.
func TestWorkLadderRepausesAPendingInteraction(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("any")
	c.sp.NudgeErrors = map[string]error{ladderSeatName: fmt.Errorf("prompt up: %w", runtime.ErrPendingInteraction)}
	c.now = c.now.Add(-2 * time.Minute)
	c.pass()
	c.now = c.now.Add(2 * time.Minute)
	c.pass()
	if e := c.entry(id); e.Attempts != 0 || e.Paused != 1 {
		t.Fatalf("entry %+v, want the attempt re-counted as paused", e)
	}
}

// Turning the ladder off drops a release it left pending, so the hook hands
// the claim out again; the cell's pacing is left as it was.
func TestWorkLadderOffClearsPendingReleases(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("being released")
	key := workladder.ClaimKey{Store: "city", Bead: id}
	cell := workladder.Record{
		V: 1, Gen: 7, Entries: []workladder.Entry{{Key: key, Kind: workladder.KindClaim, Rung: workladder.RungClaimed, Since: c.now, Attempts: 3, Last: c.now}},
		Release: &workladder.ReleaseMark{Key: key, Gen: 7, At: c.now},
	}
	c.setRow(map[string]string{session.WorkLadderKey: workladder.EncodeLadder(cell)})
	if !session.WorkLadderReleases(c.row().Metadata[session.WorkLadderKey], "city", id) {
		t.Fatal("the hook does not see the pending release")
	}
	c.env.Cfg.Daemon.WorkLadder = "off"
	c.pass()
	if session.WorkLadderReleases(c.row().Metadata[session.WorkLadderKey], "city", id) {
		t.Fatal("the pending release outlived the ladder")
	}
	if e := c.entry(id); e.Attempts != 3 {
		t.Fatalf("entry %+v, want the pacing kept", e)
	}
}

// W1 and W3 read each other's cells: the hook (W1) refuses to hand out a
// claim the ladder's release names, and the ladder reads the hook's record
// of a hand-over. W3 is built on W1, so a binary with the ladder always has
// the hook that writes its evidence.
func TestWorkLadderAndTheHookShareTheCells(t *testing.T) {
	key := workladder.ClaimKey{Store: "rig:repo", Bead: "work-1"}
	ladder := workladder.EncodeLadder(workladder.Record{V: 1, Gen: 7, Release: &workladder.ReleaseMark{Key: key, Gen: 7, At: stepsTestNow}})
	res, _, err := decideWorkStarted(session.Info{InstanceToken: "tok", Generation: "7", WorkLadderRaw: ladder},
		hookStartedFence{Token: "tok", Gen: 7, HasGen: true}, hookStartedKey{Store: key.Store, Bead: key.Bead}, session.WorkStartedViaClaim, stepsTestNow)
	if err != nil || res != hookStartedReleasing {
		t.Fatalf("hook decide = %v, %v; want it refusing the ladder's release", res, err)
	}
	for _, store := range []string{key.Store, ""} {
		raw := session.EncodeWorkStarted(session.WorkStarted{}.With(session.WorkStartedEntry{Store: store, Bead: key.Bead, Gen: 7, At: stepsTestNow, Via: session.WorkStartedViaAdopt}))
		cell, err := workladder.DecodeStarted(raw)
		if err != nil || !cell.Names(key) {
			t.Fatalf("store %q: the ladder reads %q as %+v, %v; want it naming %v", store, raw, cell, err, key)
		}
	}
	if _, err := workladder.DecodeStarted(`{"v":1,"claims":[{"store":"s","bead":"b","gen":1,"at":"","via":"claim"}]}`); err == nil {
		t.Fatal("the ladder reads a cell the hook cannot")
	}
	if _, err := session.DecodeWorkStarted(`{"v":1,"claims":[{"store":"s","bead":"b","gen":1,"at":"","via":"claim"}]}`); err == nil {
		t.Fatal("the hook reads a cell the ladder cannot")
	}
}

// Owner ruling 2026-10-18: work on a dispatch hold is not the ladder's. A
// claim the agent parked (hold:external) on a quiet seat gets no nudge, no
// recycle and no release for as long as the hold stands; once it lifts, the
// ladder takes the claim up afresh, from its grace.
func TestWorkLadderIgnoresHeldWork(t *testing.T) {
	for _, label := range []string{beadmeta.HoldExternalLabel, beadmeta.HoldMayorLabel} {
		t.Run(label, func(t *testing.T) {
			c := newLadderCity(t)
			id := c.claim("parked")
			if err := c.work.Update(id, beads.UpdateOpts{Labels: []string{label}, Metadata: map[string]string{beadmeta.SessionIDMetadataKey: c.seat}}); err != nil {
				t.Fatal(err)
			}
			c.run(8*time.Hour, 30*time.Second, func() {
				if r, _ := workladder.DecodeLadder(c.row().Metadata[session.WorkLadderKey]); len(r.Entries) != 0 {
					t.Fatalf("at %v the ladder paces held work: %s", c.now.Sub(c.start), c.row().Metadata[session.WorkLadderKey])
				}
			})
			if len(c.nudgedAt) != 0 || len(c.posted) != 0 || len(c.events(events.WorkReleased)) != 0 || len(escalations(c)) != 0 {
				t.Fatalf("held claim: nudges %v, recycles %+v, escalations %v", c.nudgedAt, c.posted, escalations(c))
			}
			if b, _ := c.work.Get(id); b.Status != "in_progress" || b.Assignee != c.seat {
				t.Fatalf("held claim %q %q, want it left with its agent", b.Status, b.Assignee)
			}
			if err := c.work.Update(id, beads.UpdateOpts{RemoveLabels: []string{label}}); err != nil {
				t.Fatal(err)
			}
			lifted := c.now
			c.run(5*time.Minute, 30*time.Second, nil)
			if len(c.nudgedAt) == 0 || c.nudgedAt[0] != lifted.Add(workladder.DefaultGrace).Sub(c.start) {
				t.Fatalf("after the hold lifted at %v: nudges %v, want the first a grace later", lifted.Sub(c.start), c.nudgedAt)
			}
		})
	}
}

// A hold placed between the snapshot and the nudge is read live: the held
// work is dropped, not nudged; a held trigger is not the ladder's either.
func TestWorkLadderRereadsTheHold(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("parked after the snapshot")
	c.lane.summary = func() *allocSummary {
		s := c.summary()
		for i := range s.AssignedWork {
			s.AssignedWork[i].Labels = nil // the snapshot predates the hold
		}
		return s
	}
	c.run(time.Minute, 30*time.Second, nil)
	if err := c.work.Update(id, beads.UpdateOpts{Labels: []string{beadmeta.HoldExternalLabel}}); err != nil {
		t.Fatal(err)
	}
	c.run(time.Hour, 30*time.Second, nil)
	if len(c.nudgedAt) != 0 {
		t.Fatalf("nudges %v about a claim parked since the snapshot", c.nudgedAt)
	}

	tc := newLadderCity(t)
	b, err := tc.work.Create(beads.Bead{Title: "trigger", Type: "task", Labels: []string{beadmeta.HoldMayorLabel}})
	if err != nil {
		t.Fatal(err)
	}
	tc.trigger = b.ID
	tc.lane.summary = func() *allocSummary {
		s := tc.summary()
		s.ReadyRouted = []beads.Bead{{ID: b.ID, Status: "open", Labels: []string{beadmeta.HoldMayorLabel}}}
		return s
	}
	tc.setRow(map[string]string{beadmeta.TriggerBeadIDMetadataKey: b.ID})
	tc.run(time.Hour, 30*time.Second, func() {
		if r, _ := workladder.DecodeLadder(tc.row().Metadata[session.WorkLadderKey]); len(r.Entries) != 0 {
			t.Fatalf("the ladder paces a held trigger: %s", tc.row().Metadata[session.WorkLadderKey])
		}
	})
	if len(tc.nudgedAt) != 0 || len(tc.posted) != 0 {
		t.Fatalf("held trigger: nudges %v, recycles %+v", tc.nudgedAt, tc.posted)
	}
}

// A seat stuck at a permission prompt refuses every nudge: it is retried no
// more often than every 10 minutes, its count does not move, and one
// nudge_refused alert is raised.
func TestWorkLadderBacksOffARefusingSeat(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("waiting on a permission prompt")
	c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id})
	c.sp.NudgeErrors = map[string]error{ladderSeatName: fmt.Errorf("prompt up: %w", runtime.ErrPendingInteraction)}
	c.run(6*time.Hour, 30*time.Second, nil)
	calls := c.sp.CountCalls("Nudge", ladderSeatName)
	if calls > 6*6+2 {
		t.Fatalf("%d delivery attempts in 6h", calls)
	}
	if got := escalations(c); fmt.Sprint(got) != fmt.Sprint([]string{workladder.AlertNudgeRefused}) {
		t.Fatalf("escalations %v, want one nudge_refused", got)
	}
	if e := c.entry(id); e.Attempts != 0 || len(c.posted) != 0 {
		t.Fatalf("entry %+v, recycles %+v: refusals moved it toward a recycle", e, c.posted)
	}
	c.sp.NudgeErrors = nil
	c.run(15*time.Minute, 30*time.Second, nil)
	if e := c.entry(id); e.Refused != 0 || e.Attempts == 0 {
		t.Fatalf("entry %+v after a taken nudge, want the refusals over and the count moving", e)
	}
}

// A claim closed between the snapshot and the release's live read is not a
// release that failed: no work.release_unfinished, and the release clears.
func TestWorkLadderReleaseOfAClosedClaimIsSilent(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("finished just now")
	c.setRow(map[string]string{session.WorkLadderKey: workladder.EncodeLadder(workladder.Record{V: 1, Gen: 7, Entries: []workladder.Entry{{
		Key: workladder.ClaimKey{Store: "city", Bead: id}, Kind: workladder.KindClaim, Rung: workladder.RungClaimed,
		Since: c.now.Add(-time.Hour), Attempts: 3, Last: c.now.Add(-time.Hour),
	}}})})
	snap := c.summary()
	c.lane.summary = func() *allocSummary { snap.At = c.now; return snap }
	closed := "closed"
	if err := c.work.Update(id, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatal(err)
	}
	c.pass()
	if n := len(c.events(events.WorkReleaseUnfinished)); n != 0 {
		t.Fatalf("work.release_unfinished = %d for a claim that closed", n)
	}
	if c.cell().Release != nil || c.entry(id).Unreleased {
		t.Fatalf("cell %s: want the release cleared, nothing unfinished", c.row().Metadata[session.WorkLadderKey])
	}
}

// stripHolds is c's allocation as a snapshot taken before any hold: the
// live beads may carry one the snapshot does not.
func stripHolds(c *ladderCity) func() *allocSummary {
	return func() *allocSummary {
		s := c.summary()
		for i := range s.AssignedWork {
			s.AssignedWork[i].Labels = nil
		}
		for i := range s.ReadyRouted {
			s.ReadyRouted[i].Labels = nil
		}
		return s
	}
}

// A hold placed between the release decision and the release's live read
// keeps the claim (owner ruling 2026-10-18): the release settles silently,
// nothing unfinished.
func TestWorkLadderReleaseRespectsAHoldPlacedLate(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("parked as the release decided")
	c.lane.summary = stripHolds(c)
	c.setRow(map[string]string{session.WorkLadderKey: workladder.EncodeLadder(workladder.Record{V: 1, Gen: 7, Entries: []workladder.Entry{{
		Key: workladder.ClaimKey{Store: "city", Bead: id}, Kind: workladder.KindClaim, Rung: workladder.RungClaimed,
		Since: c.now.Add(-time.Hour), Attempts: 3, Last: c.now.Add(-time.Hour),
	}}})})
	if err := c.work.Update(id, beads.UpdateOpts{Labels: []string{beadmeta.HoldMayorLabel}}); err != nil {
		t.Fatal(err)
	}
	c.pass()
	if b, _ := c.work.Get(id); b.Status != "in_progress" || b.Assignee != c.seat {
		t.Fatalf("held claim %q %q: released", b.Status, b.Assignee)
	}
	if n := len(c.events(events.WorkReleaseUnfinished)) + len(c.events(events.WorkReleased)); n != 0 || c.cell().Release != nil {
		t.Fatalf("release events %d, cell %s: want a silent settle", n, c.row().Metadata[session.WorkLadderKey])
	}
}

// A trigger put on hold after the snapshot is re-read live: never nudged
// or recycled.
func TestWorkLadderRereadsATriggerHold(t *testing.T) {
	c := newLadderCity(t)
	b, err := c.work.Create(beads.Bead{Title: "trigger", Type: "task", Labels: []string{beadmeta.HoldExternalLabel}})
	if err != nil {
		t.Fatal(err)
	}
	c.trigger = b.ID
	c.lane.summary = stripHolds(c)
	c.setRow(map[string]string{beadmeta.TriggerBeadIDMetadataKey: b.ID})
	c.run(time.Hour, 30*time.Second, nil)
	if len(c.nudgedAt) != 0 || len(c.posted) != 0 {
		t.Fatalf("a trigger held since the snapshot: nudges %v, recycles %+v", c.nudgedAt, c.posted)
	}
}

// Carried work (an incomplete snapshot) that is put on hold is re-read live
// and dropped: never nudged.
func TestWorkLadderRereadsACarriedHold(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("carried, then parked")
	c.run(2*time.Minute, 30*time.Second, nil)
	if err := c.work.Update(id, beads.UpdateOpts{Labels: []string{beadmeta.HoldExternalLabel}}); err != nil {
		t.Fatal(err)
	}
	c.partial, c.claims = true, nil // the snapshot lost it; the ladder carries it
	before := len(c.nudgedAt)
	c.run(2*time.Hour, 30*time.Second, nil)
	if len(c.nudgedAt) != before || len(c.cell().Entries) != 0 {
		t.Fatalf("carried held claim: nudges %v, cell %s", c.nudgedAt[before:], c.row().Metadata[session.WorkLadderKey])
	}
}

// A paused nudge (the provider degraded) that the seat cannot take records
// no refusal: it counted nothing, so there is nothing to re-count, and the
// degraded pace already holds it to PausedCap.
func TestWorkLadderPausedRefusalRecordsNothing(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("degraded and at a prompt")
	c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id})
	c.sp.NudgeErrors = map[string]error{ladderSeatName: fmt.Errorf("prompt up: %w", runtime.ErrPendingInteraction)}
	writeHealth := func() {
		dir := filepath.Join(c.env.CityPath, ".gc", "cache")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(providerHealthFileFormat{Providers: []providerHealthRecord{{Provider: "claude", Status: "unhealthy", ProbedAt: float64(time.Now().Unix())}}})
		if err := os.WriteFile(filepath.Join(dir, "provider-health.json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeHealth()
	c.run(time.Hour, 30*time.Second, writeHealth)
	if len(c.nudgedAt) == 0 {
		t.Fatal("no paused nudge was attempted")
	}
	if e := c.entry(id); e.Refused != 0 || e.Attempts != 0 || len(escalations(c)) != 0 {
		t.Fatalf("entry %+v, escalations %v: a paused nudge's refusal was recorded", e, escalations(c))
	}
}

// Rule 2 (audit 4, lever 2; user-approved 2026-10-19). A claimed step whose
// seat asked a person and waits for the answer says so: gc.awaiting_human
// names the seat (the adopt-pr human-approval step, once its approval mail
// is out). The ladder leaves it with its agent as it leaves a dispatch hold:
// no nudge, recycle or release while the key names the seat, however long
// the person takes, and no alert either. Once the seat clears it (the answer
// came, and it works the step again), the ladder takes the step up on a
// fresh grace. Without the rule, the step is nudged at +90s, +3 ... +48
// minutes, recycled at about 154 minutes, then nudged hourly for as long as
// the person takes.
func TestWorkLadderLeavesAHumanWaitAlone(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("Human review before merge")
	if err := c.work.Update(id, beads.UpdateOpts{Metadata: map[string]string{beadmeta.SessionIDMetadataKey: c.seat}}); err != nil {
		t.Fatal(err)
	}
	c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id})
	c.run(time.Minute, 30*time.Second, nil) // the step runs, then asks
	if c.entry(id).Rung != workladder.RungStarted {
		t.Fatalf("entry %+v, want the started claim paced before it asks", c.entry(id))
	}
	if err := c.work.Update(id, beads.UpdateOpts{Metadata: map[string]string{beadmeta.AwaitingHumanMetadataKey: c.seat}}); err != nil {
		t.Fatal(err)
	}
	c.run(3*24*time.Hour, 5*time.Minute, func() {
		if r := c.cell(); len(r.Entries) != 0 {
			t.Fatalf("at %v the ladder paces a step waiting on a person: %s", c.now.Sub(c.start), c.row().Metadata[session.WorkLadderKey])
		}
	})
	if len(c.nudgedAt) != 0 || len(c.posted) != 0 || len(c.events(events.WorkReleased)) != 0 || len(escalations(c)) != 0 {
		t.Fatalf("three days waiting on a person: nudges %v, recycles %+v, escalations %v", c.nudgedAt, c.posted, escalations(c))
	}
	if b, _ := c.work.Get(id); b.Status != "in_progress" || b.Assignee != c.seat {
		t.Fatalf("the waiting step is %q %q, want it left with its agent", b.Status, b.Assignee)
	}
	if v := c.row().Metadata["restart_requested"]; v != "" {
		t.Fatalf("restart_requested %q on a seat waiting on a person", v)
	}
	if err := c.work.Update(id, beads.UpdateOpts{Metadata: map[string]string{beadmeta.AwaitingHumanMetadataKey: ""}}); err != nil {
		t.Fatal(err)
	}
	answered := c.now
	c.run(5*time.Minute, 30*time.Second, nil)
	if len(c.nudgedAt) == 0 || c.nudgedAt[0] != answered.Add(workladder.DefaultGrace).Sub(c.start) {
		t.Fatalf("after the answer at %v: nudges %v, want the first a grace later", answered.Sub(c.start), c.nudgedAt)
	}
}

// The wait is the asking seat's, read live wherever the ladder re-reads a
// claim before it acts: written after the planner's snapshot, it keeps the
// step from a nudge, from a release and from carried pacing. And it is only
// that seat's: a step whose asker is gone, released and claimed by another
// seat, is the new seat's work, paced as ever (it asks again itself).
func TestWorkLadderRereadsAHumanWait(t *testing.T) {
	ask := func(c *ladderCity, id, seat string) {
		c.t.Helper()
		if err := c.work.Update(id, beads.UpdateOpts{Metadata: map[string]string{beadmeta.AwaitingHumanMetadataKey: seat}}); err != nil {
			c.t.Fatal(err)
		}
	}
	t.Run("nudge", func(t *testing.T) {
		c := newLadderCity(t)
		id := c.claim("asked after the snapshot")
		snapshot := c.summary
		c.lane.summary = func() *allocSummary { // the snapshot predates the ask
			s := snapshot()
			for i := range s.AssignedWork {
				delete(s.AssignedWork[i].Metadata, beadmeta.AwaitingHumanMetadataKey)
			}
			return s
		}
		c.run(time.Minute, 30*time.Second, nil)
		ask(c, id, c.seat)
		c.run(time.Hour, 30*time.Second, nil)
		if len(c.nudgedAt) != 0 {
			t.Fatalf("nudges %v about a step waiting on a person since the snapshot", c.nudgedAt)
		}
	})
	t.Run("release", func(t *testing.T) {
		c := newLadderCity(t)
		id := c.claim("asked as the release decided")
		snap := c.summary()
		c.lane.summary = func() *allocSummary { snap.At = c.now; return snap }
		c.setRow(map[string]string{session.WorkLadderKey: workladder.EncodeLadder(workladder.Record{V: 1, Gen: 7, Entries: []workladder.Entry{{
			Key: workladder.ClaimKey{Store: "city", Bead: id}, Kind: workladder.KindClaim, Rung: workladder.RungClaimed,
			Since: c.now.Add(-time.Hour), Attempts: 3, Last: c.now.Add(-time.Hour),
		}}})})
		ask(c, id, c.seat)
		c.pass()
		if b, _ := c.work.Get(id); b.Status != "in_progress" || b.Assignee != c.seat {
			t.Fatalf("a step waiting on a person is %q %q: released", b.Status, b.Assignee)
		}
		if n := len(c.events(events.WorkReleaseUnfinished)) + len(c.events(events.WorkReleased)); n != 0 || c.cell().Release != nil {
			t.Fatalf("release events %d, cell %s: want a silent settle", n, c.row().Metadata[session.WorkLadderKey])
		}
	})
	t.Run("carried", func(t *testing.T) {
		c := newLadderCity(t)
		id := c.claim("carried, then asked")
		c.run(2*time.Minute, 30*time.Second, nil)
		ask(c, id, c.seat)
		c.partial, c.claims = true, nil
		before := len(c.nudgedAt)
		c.run(2*time.Hour, 30*time.Second, nil)
		if len(c.nudgedAt) != before || len(c.cell().Entries) != 0 {
			t.Fatalf("carried human wait: nudges %v, cell %s", c.nudgedAt[before:], c.row().Metadata[session.WorkLadderKey])
		}
	})
	t.Run("another seat asked", func(t *testing.T) {
		c := newLadderCity(t)
		id := c.claim("re-claimed after its asker died")
		ask(c, id, "gcg-session-gone") // the seat that asked was closed; its claim was released and taken here
		c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id})
		c.run(5*time.Minute, 30*time.Second, nil)
		if want := []time.Duration{workladder.DefaultGrace, halfPast(4)}; fmt.Sprint(c.nudgedAt) != fmt.Sprint(want) {
			t.Fatalf("nudges %v, want the new seat paced as ever %v", c.nudgedAt, want)
		}
	})
}

// ladderTurns is a fake turn source for the rate-limit rule. Per
// conversation (session_key): how many of the seat's latest turns the
// provider refused with a pool-cooling 429 and when the newest ended, or,
// with served, that its newest turn was answered at that time. reads counts
// every read.
type ladderTurns struct {
	refused map[string]int       // by session_key
	at      map[string]time.Time // by session_key: when the newest turn ended
	served  map[string]bool      // by session_key: the newest turn completed
	open    map[string]time.Time // by session_key: a turn open since then
	err     error
	// failEvery fails every failEvery-th read (0: none).
	failEvery int
	reads     int
}

const poolCooling = "API Error: Request rejected (429) · pool cooling for another 3d 16h; every account is in a rate-limit cool-down, so retrying before then cannot succeed"

func newLadderTurns() *ladderTurns {
	return &ladderTurns{refused: map[string]int{}, at: map[string]time.Time{}, served: map[string]bool{}, open: map[string]time.Time{}}
}

// refuse records that conversation key's newest turn, ended at at, was
// refused: one more in a row.
func (l *ladderTurns) refuse(key string, at time.Time) {
	l.refused[key]++
	l.at[key], l.served[key] = at, false
}

// serve records that conversation key's newest turn, ended at at, was
// answered.
func (l *ladderTurns) serve(key string, at time.Time) {
	l.refused[key] = 0
	l.at[key], l.served[key] = at, true
}

// install makes l c's turn source.
func (l *ladderTurns) install(c *ladderCity) {
	c.lane.turnReaderFor = func(externalReadsEnv) ladderTurnReader {
		return func(_ context.Context, row beads.Bead) (runtime.TurnHistory, error) {
			l.reads++
			if l.err != nil {
				return runtime.TurnHistory{}, l.err
			}
			if l.failEvery > 0 && l.reads%l.failEvery == 0 {
				return runtime.TurnHistory{}, fmt.Errorf("transcript busy: %w", runtime.ErrRuntimeUnavailable)
			}
			key := row.Metadata["session_key"]
			var hist runtime.TurnHistory
			if at, ok := l.open[key]; ok {
				hist.Current, hist.CurrentStart = runtime.TurnState{TurnID: "open"}, at
			}
			if l.served[key] {
				hist.Recent = append(hist.Recent, runtime.EndedTurn{TurnID: "answered", Status: runtime.TurnStatusCompleted, EndedAt: l.at[key]})
			}
			for i := range l.refused[key] {
				hist.Recent = append(hist.Recent, runtime.EndedTurn{
					TurnID: fmt.Sprintf("refused-%d", i), Status: runtime.TurnStatusFailed, EndedAt: l.at[key].Add(-time.Duration(i) * time.Minute),
					Failure: runtime.TurnFailure{Code: "rate_limit", HTTPStatus: 429, RateLimited: true, Message: poolCooling},
				})
			}
			hist.Recent = append(hist.Recent, runtime.EndedTurn{TurnID: "worked", Status: runtime.TurnStatusCompleted, EndedAt: l.at[key].Add(-time.Hour)})
			return hist, nil
		}
	}
}

// refuseNudges is a pass callback: every nudge c delivers to its first seat
// is answered, a second later, by a turn the provider refuses. moves says
// whether that refusal moves the seat's activity clock (tmux discounts output
// that lands right after a nudge: it may not).
func (l *ladderTurns) refuseNudges(c *ladderCity, moves bool) func() {
	seen := len(c.nudgedAt)
	return func() {
		for ; seen < len(c.nudgedAt); seen++ {
			l.refuse(c.row().Metadata["session_key"], c.start.Add(c.nudgedAt[seen]+time.Second))
			if moves {
				c.sp.SetActivity(ladderSeatName, c.now)
			}
		}
	}
}

// ladderPayloads are c's typ events' payloads, in order.
func ladderPayloads(c *ladderCity, typ string) []events.WorkLadderPayload {
	var out []events.WorkLadderPayload
	for _, e := range c.events(typ) {
		var p events.WorkLadderPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			c.t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// Rule 1 (audit 4, lever 2; user-approved 2026-10-19), replaying the
// 2026-10-19 07:17Z GLM pool cooldown: the seat's step failed on "Request
// rejected (429) · pool cooling for another 3d 16h", and so did its answer
// to the ladder's first nudge. The seat is restarted at once to a fresh
// conversation (gc session reset: restart_requested and
// continuation_reset_pending land with the cell, in one CAS), keeping its
// claim, with no planner drain and no further nudge, and the planner is
// woken for it. The fresh seat is refused too: the pool itself. One
// provider_rate_limited alert names the provider, there is no second
// recycle, and the provider's seats are paced as degraded from then on. A
// turn the provider serves later ends that, and the seat is paced as usual.
// Without the rule, the seat is recycled only after six nudges, about 154
// minutes, through a drain that resumes the same conversation.
func TestWorkLadderRecyclesARateLimitedSeatFresh(t *testing.T) {
	for _, moves := range []bool{true, false} {
		t.Run(fmt.Sprintf("refusals move the activity clock %v", moves), func(t *testing.T) {
			c := newLadderCity(t)
			id := c.claim("review-gemini it1")
			c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id, "session_key": "conv-1"})
			turns := newLadderTurns()
			turns.refuse("conv-1", c.now.Add(-time.Hour)) // the step's own turn was refused
			turns.install(c)
			c.run(10*time.Minute, 30*time.Second, turns.refuseNudges(c, moves))

			row := c.row()
			if row.Metadata["restart_requested"] != "true" || row.Metadata["continuation_reset_pending"] != "true" {
				t.Fatalf("restart_requested %q, continuation_reset_pending %q: want a fresh restart requested", row.Metadata["restart_requested"], row.Metadata["continuation_reset_pending"])
			}
			if want := []time.Duration{workladder.DefaultGrace}; fmt.Sprint(c.nudgedAt) != fmt.Sprint(want) {
				t.Fatalf("nudges %v, want only the first, at the grace: its refusal is the second", c.nudgedAt)
			}
			if len(c.posted) != 0 {
				t.Fatalf("posted %+v to the planner: a fresh recycle is a restart, not a drain", c.posted)
			}
			if c.wakes.Load() == 0 {
				t.Fatal("the planner was not woken for the restart")
			}
			if e := c.entry(id); e.Recycles != 1 || !e.FreshRecycled || e.Rung != workladder.RungStarted {
				t.Fatalf("entry %+v, want the one recycle spent fresh", e)
			}
			if b, _ := c.work.Get(id); b.Status != "in_progress" || b.Assignee != c.seat {
				t.Fatalf("claim %q %q: a fresh recycle keeps it", b.Status, b.Assignee)
			}
			recycles := ladderPayloads(c, events.WorkRecycleRequested)
			if len(recycles) != 1 || recycles[0].Reason != "rate_limited" || recycles[0].Provider != "claude" || recycles[0].Generation != 7 {
				t.Fatalf("work.recycle_requested %+v, want one rate_limited recycle of the claude seat at generation 7", recycles)
			}
			alerts := ladderPayloads(c, events.WorkEscalated)
			if len(alerts) != 1 || alerts[0].Reason != workladder.AlertRateLimitedRecycled || alerts[0].Provider != "claude" {
				t.Fatalf("work.escalated %+v, want one rate_limited_recycled naming the provider", alerts)
			}
			if ev := c.events(events.WorkEscalated)[0]; !strings.Contains(ev.Message, "pool cooling for another 3d 16h") || !strings.Contains(ev.Message, "provider claude") {
				t.Fatalf("alert message %q, want the provider named in its own words", ev.Message)
			}

			// A13 restarts the seat in place: a new generation and conversation,
			// the request cleared. The fresh seat's first turn is refused too.
			before := len(c.nudgedAt)
			c.setRow(map[string]string{"generation": "8", "instance_token": "tok-8", "session_key": "conv-2", "restart_requested": "", "continuation_reset_pending": ""})
			turns.refuse("conv-2", c.now)
			c.sp.SetActivity(ladderSeatName, c.now)
			c.run(10*time.Minute, 30*time.Second, turns.refuseNudges(c, moves))
			alerts = ladderPayloads(c, events.WorkEscalated)
			if len(alerts) != 2 || alerts[1].Reason != workladder.AlertProviderRateLimited || alerts[1].Provider != "claude" || alerts[1].Generation != 8 {
				t.Fatalf("work.escalated %+v, want the pool alert naming the provider at generation 8", alerts)
			}
			if v := c.row().Metadata["restart_requested"]; v != "" {
				t.Fatalf("restart_requested %q: R = 1, the pool is not the seat", v)
			}
			if e := c.entry(id); e.Rung != workladder.RungEscalated || !e.RateLimitAlerted || e.Recycles != 1 {
				t.Fatalf("entry %+v, want escalated with the pool alert raised", e)
			}
			if !c.lane.canaries.refusing("claude", c.now) {
				t.Fatal("the provider is not refusing after its canary's fresh conversation was refused")
			}
			// The claim was escalated with the pool alert: it is nudged at Cap
			// from then on, never alerted or recycled again.
			c.run(90*time.Minute, time.Minute, turns.refuseNudges(c, moves))
			var gen8 []events.WorkLadderPayload
			for _, ev := range ladderPayloads(c, events.WorkNudged) {
				if ev.Generation == 8 {
					gen8 = append(gen8, ev)
				}
			}
			if len(gen8) < 2 || len(gen8) > 3 || len(ladderPayloads(c, events.WorkEscalated)) != 2 || len(c.events(events.WorkRecycleRequested)) != 1 {
				t.Fatalf("nudges %+v, %d alerts, %d recycles in 90 min of a refusing pool: want the escalated claim nudged hourly and nothing more",
					gen8, len(ladderPayloads(c, events.WorkEscalated)), len(c.events(events.WorkRecycleRequested)))
			}
			if c.lane.canaries.refusing("claude", c.now) {
				t.Fatalf("the provider still refusing %v after its first refusal: the hold is %v", 90*time.Minute, ladderRefusingFor)
			}
			_ = before
		})
	}
}

// Refusals the ladder did not cause are not the rule's: refusals already in
// the transcript when the ladder first sees the seat (11 hours old here, a
// cooldown long over) get a nudge, and an answered nudge ends it: no
// recycle, no restart wave when the ladder is turned on (WL8).
func TestWorkLadderStaleRefusalsGetANudgeFirst(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("refused hours ago")
	c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id, "session_key": "conv-1"})
	turns := newLadderTurns()
	for range 4 {
		turns.refuse("conv-1", c.now.Add(-11*time.Hour))
	}
	turns.install(c)
	c.run(30*time.Minute, 30*time.Second, nil) // the nudges are answered: no new refusal
	if v := c.row().Metadata["restart_requested"]; v != "" || len(c.events(events.WorkRecycleRequested)) != 0 || len(c.events(events.WorkEscalated)) != 0 {
		t.Fatalf("restart_requested %q, recycles %d, alerts %d on refusals from before the ladder", v, len(c.events(events.WorkRecycleRequested)), len(c.events(events.WorkEscalated)))
	}
	if len(c.nudgedAt) == 0 || c.nudgedAt[0] != workladder.DefaultGrace {
		t.Fatalf("nudges %v, want the first at the grace", c.nudgedAt)
	}
}

// One refusal is not the rule's: a provider that refused once may serve the
// next turn, and the ladder paces the seat as ever.
func TestWorkLadderOneRateLimitIsNotARecycle(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("refused once")
	c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id, "session_key": "conv-1"})
	turns := newLadderTurns()
	turns.refuse("conv-1", c.now.Add(-time.Minute))
	turns.install(c)
	c.run(15*time.Minute, 30*time.Second, nil) // its nudges are answered
	if v := c.row().Metadata["restart_requested"]; v != "" || len(c.events(events.WorkRecycleRequested)) != 0 {
		t.Fatalf("restart_requested %q after one refusal", v)
	}
	if want := []time.Duration{workladder.DefaultGrace, halfPast(4), halfPast(10)}; fmt.Sprint(c.nudgedAt) != fmt.Sprint(want) {
		t.Fatalf("nudges %v, want the ladder's own pace %v", c.nudgedAt, want)
	}
}

// The rule waits for what every other act waits for. While the provider is
// degraded (its health gate owns that outage, WL4), while a gate holds the
// seat, and while the seat is not quiet, refusals recycle nothing; a turn
// source that cannot be read reads as no refusal. A seat the rule cannot act
// on is not read, except a degraded provider's quiet seat, whose served turn
// would end a refusing canary.
func TestWorkLadderRateLimitRuleNeedsAnOpenQuietSeat(t *testing.T) {
	for name, setup := range map[string]func(*ladderCity, *ladderTurns){
		"degraded": func(c *ladderCity, _ *ladderTurns) {
			dir := filepath.Join(c.env.CityPath, ".gc", "cache")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				c.t.Fatal(err)
			}
			data, _ := json.Marshal(providerHealthFileFormat{Providers: []providerHealthRecord{{Provider: "claude", Status: "unhealthy", ProbedAt: float64(time.Now().Unix())}}})
			if err := os.WriteFile(filepath.Join(dir, "provider-health.json"), data, 0o644); err != nil {
				c.t.Fatal(err)
			}
		},
		"waiting": func(c *ladderCity, _ *ladderTurns) { c.setRow(map[string]string{"wait_hold": "true"}) },
		"working": func(c *ladderCity, _ *ladderTurns) { c.sp.SetActivity(ladderSeatName, c.now.Add(time.Hour)) },
		"unreadable": func(_ *ladderCity, l *ladderTurns) {
			l.err = fmt.Errorf("transcript: %w", runtime.ErrRuntimeUnavailable)
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := newLadderCity(t)
			id := c.claim("refused, but not the rule's now")
			c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id, "session_key": "conv-1"})
			turns := newLadderTurns()
			turns.refuse("conv-1", c.now.Add(-time.Minute))
			turns.install(c)
			setup(c, turns)
			c.run(5*time.Minute, 30*time.Second, turns.refuseNudges(c, true))
			if v := c.row().Metadata["restart_requested"]; v != "" || len(c.events(events.WorkRecycleRequested)) != 0 || len(c.events(events.WorkEscalated)) != 0 {
				t.Fatalf("restart_requested %q, recycles %d, alerts %d", v, len(c.events(events.WorkRecycleRequested)), len(c.events(events.WorkEscalated)))
			}
			switch name {
			case "waiting", "working":
				if turns.reads != 0 {
					t.Fatalf("%d turn reads of a seat the rule cannot act on", turns.reads)
				}
			case "unreadable":
				if turns.reads > 1+len(c.nudgedAt) {
					t.Fatalf("%d reads of an unreadable seat, %d nudges: want one per nudge at most", turns.reads, len(c.nudgedAt))
				}
			}
		})
	}
}

// A quiet seat is read again only when it moves or the ladder nudges it,
// and at least every ladderReadMaxAge: not every pass.
func TestWorkLadderReadsTurnsSparingly(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("quiet")
	c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id, "session_key": "conv-1"})
	turns := newLadderTurns()
	turns.install(c)
	c.run(2*time.Minute+30*time.Second, 30*time.Second, nil) // one nudge, at the grace, then a pass
	if turns.reads != 2 {
		t.Fatalf("%d reads in 2.5 minutes with one nudge, want 2 (the first, and after the nudge)", turns.reads)
	}
	c.run(time.Hour, 30*time.Second, nil)
	passes, maxAge := int((time.Hour+2*time.Minute)/(30*time.Second)), int(time.Hour/ladderReadMaxAge)
	if turns.reads > 2+len(c.nudgedAt)+maxAge || turns.reads >= passes/2 {
		t.Fatalf("%d reads in %d passes (%d nudges): want one per nudge and per %v at most", turns.reads, passes, len(c.nudgedAt), ladderReadMaxAge)
	}
	before := turns.reads
	c.sp.SetActivity(ladderSeatName, c.now.Add(-2*time.Minute)) // the seat moved
	c.pass()
	if turns.reads != before+1 {
		t.Fatalf("a seat whose activity moved was not read again")
	}
}

// The reading is the conversation's it was taken in: a row that moved to a
// new generation, or only to a new conversation, between the read and the
// commit is decided with no refusals, so the fresh restart is never
// requested for a conversation the reading did not see.
func TestWorkLadderRateLimitReadingIsTheConversations(t *testing.T) {
	for name, moved := range map[string]map[string]string{
		"new generation":   {"generation": "8", "instance_token": "tok-8"},
		"new conversation": {"session_key": "conv-2"},
	} {
		t.Run(name, func(t *testing.T) {
			c := newLadderCity(t)
			id := c.claim("restarted under the pass")
			key := workladder.ClaimKey{Store: "city", Bead: id}
			c.setRow(map[string]string{
				beadmeta.CurrentClaimBeadIDMetadataKey: id, "session_key": "conv-1",
				session.WorkLadderKey: workladder.EncodeLadder(workladder.Record{V: 1, Gen: 7, Entries: []workladder.Entry{{
					Key: key, Kind: workladder.KindClaim, Rung: workladder.RungStarted, Since: c.now.Add(-time.Hour), Attempts: 1, Last: c.now.Add(-2 * time.Minute),
				}}}),
			})
			snap := snapshotOf(t, c.sessions, c.seat) // listed in conv-1, generation 7
			turns := newLadderTurns()
			turns.refuse("conv-1", c.now.Add(-3*time.Minute))
			turns.refuse("conv-1", c.now.Add(-time.Minute)) // answered the nudge
			turns.install(c)
			c.setRow(moved)
			c.env.CityStore = blindWriteRefusingStore{inner: staleListStore{MemStore: c.sessions, snapshot: snap}}
			c.pass()
			if v := c.row().Metadata["restart_requested"]; v != "" || len(c.events(events.WorkRecycleRequested)) != 0 {
				t.Fatalf("restart_requested %q on conv-1's refusals for a row that moved (%v)", v, moved)
			}
		})
	}
}

// A turn read that fails is reported once per seat and conversation, not
// every pass; a seat whose provider records no turns (cursor, a codex
// family) is not the rule's at all, and is not reported.
func TestWorkLadderReportsAFailedTurnReadOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		lines int
	}{
		{"unreadable", fmt.Errorf("transcript: %w", runtime.ErrRuntimeUnavailable), 1},
		{"no turns recorded", fmt.Errorf("cursor: %w", runtime.ErrTurnIdentityUnsupported), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newLadderCity(t)
			id := c.claim("quiet")
			c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id, "session_key": "conv-1"})
			var stderr strings.Builder
			c.lane.stderr = &stderr
			turns := newLadderTurns()
			turns.err = tc.err
			turns.install(c)
			c.run(time.Hour, 30*time.Second, func() { c.sp.SetActivity(ladderSeatName, c.now.Add(-time.Hour)) })
			if n := strings.Count(stderr.String(), "reading its turns"); n != tc.lines {
				t.Fatalf("%d reports in an hour, want %d:\n%s", n, tc.lines, stderr.String())
			}
		})
	}
}

// addSeat adds a running, quiet pool seat named name to c, on c's provider,
// and returns its row ID.
func (c *ladderCity) addSeat(name string) string {
	c.t.Helper()
	m := map[string]string{
		"session_name": name, "pool_managed": "true", "template": "worker", "state": "active",
		"generation": "3", "instance_token": "tok-" + name, "provider": "claude", "session_key": "conv-" + name,
	}
	row, err := c.sessions.Create(beads.Bead{Title: name, Type: "session", Labels: []string{sessionBeadLabel}, Metadata: m})
	if err != nil {
		c.t.Fatal(err)
	}
	if err := c.sp.Start(context.Background(), name, runtime.Config{}); err != nil {
		c.t.Fatal(err)
	}
	c.sp.SetActivity(name, c.now.Add(-time.Hour))
	return row.ID
}

// claimFor adds an in_progress claim held by seat, and makes it the seat's
// current claim.
func (c *ladderCity) claimFor(seat, title string) {
	c.t.Helper()
	b, err := c.work.Create(beads.Bead{Title: title, Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker", beadmeta.CurrentClaimBeadIDMetadataKey: ""}})
	if err != nil {
		c.t.Fatal(err)
	}
	inProgress := "in_progress"
	if err := c.work.Update(b.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &seat}); err != nil {
		c.t.Fatal(err)
	}
	c.claims = append(c.claims, b.ID)
	casSetRow(c.t, c.sessions, seat, map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: b.ID})
}

// refusingSeats answers, a second later on the ladder clock, every nudge
// the ladder delivers to the named seats with a turn the provider refuses
// (keys maps a seat's runtime name to its row ID); a seat whose current
// conversation key is in served answers with a served turn instead, and
// keeps working (its activity moves). It is a pass callback.
func refusingSeats(c *ladderCity, turns *ladderTurns, names map[string]string, served map[string]bool) func() {
	seen := map[string]int{}
	return func() {
		for name, id := range names {
			row, err := c.sessions.Get(id)
			if err != nil {
				c.t.Fatal(err)
			}
			key := row.Metadata["session_key"]
			if served[key] {
				c.sp.SetActivity(name, c.now) // working on its fresh conversation
				continue
			}
			for n := c.sp.CountCalls("Nudge", name); seen[name] < n; seen[name]++ {
				turns.refuse(key, c.now.Add(time.Second))
			}
		}
	}
}

// restartFresh applies a fresh recycle the ladder requested on seat id, as
// A13 would: a new generation and conversation, the request cleared.
func restartFresh(c *ladderCity, id, key string) {
	c.t.Helper()
	b, err := c.sessions.Get(id)
	if err != nil {
		c.t.Fatal(err)
	}
	gen := rowGeneration(b.Metadata) + 1
	casSetRow(c.t, c.sessions, id, map[string]string{
		"generation": fmt.Sprint(gen), "instance_token": fmt.Sprintf("tok-%s-%d", id, gen), "session_key": key,
		"restart_requested": "", "continuation_reset_pending": "",
	})
}

// F4/R2: one canary per provider, and the seats it holds are never drained.
// Eight seats on one provider are refused in their own conversations; a
// fresh conversation is served (the refusal stuck to the conversation). One
// seat at a time is recycled fresh, the others paced paused meanwhile, so
// every seat gets its fresh conversation and none is drained back into the
// conversation that refuses it (no A16 recycle, no rate_limited_unrecycled).
func TestWorkLadderCanaryQueueNeverDrainsAHeldSeat(t *testing.T) {
	c := newLadderCity(t)
	idA := c.claim("seat a's step")
	c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: idA, "session_key": "conv-" + ladderSeatName})
	names := map[string]string{ladderSeatName: c.seat}
	for i := range 7 {
		name := fmt.Sprintf("seat-%d", i)
		id := c.addSeat(name)
		c.claimFor(id, name+"'s step")
		names[name] = id
	}
	turns := newLadderTurns()
	for name := range names {
		turns.refuse("conv-"+name, c.now.Add(-time.Hour))
	}
	turns.install(c)
	served := map[string]bool{}
	answer := refusingSeats(c, turns, names, served)
	recycledFresh := map[string]bool{}
	c.run(4*time.Hour, 30*time.Second, func() {
		answer()
		for _, id := range names {
			if b, _ := c.sessions.Get(id); b.Metadata["restart_requested"] == "true" {
				recycledFresh[id] = true
				key := "fresh-" + id
				served[key] = true
				restartFresh(c, id, key)
			}
		}
	})
	if len(recycledFresh) != len(names) {
		t.Fatalf("%d of %d seats got their fresh conversation in 4h", len(recycledFresh), len(names))
	}
	if len(c.posted) != 0 {
		t.Fatalf("drained %+v back into the conversations that refused them", c.posted)
	}
	for _, a := range ladderPayloads(c, events.WorkEscalated) {
		if a.Reason == workladder.AlertRateLimitedUnrecycled || a.Reason == workladder.AlertProviderRateLimited {
			t.Fatalf("alert %+v: no seat should have spent its recycle on a drain, and the pool served fresh conversations", a)
		}
	}
	counted := 0
	for _, n := range ladderPayloads(c, events.WorkNudged) {
		if !n.Paused {
			counted++
		}
	}
	if counted > 2*len(names) {
		t.Fatalf("%d counted nudges to %d refused seats: held seats must be paced paused", counted, len(names))
	}
}

// S2: a canary whose fresh conversation the provider serves is busy, so it
// is read even while it works: a completed turn of its fresh conversation
// shows it served (even with a newer turn open), and the next refused seat
// may go at once rather than when the flight runs out.
func TestWorkLadderServedCanaryReleasesTheQueue(t *testing.T) {
	c := newLadderCity(t)
	idA := c.claim("seat a's step")
	c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: idA, "session_key": "conv-" + ladderSeatName})
	names := map[string]string{ladderSeatName: c.seat}
	for i := range 3 {
		name := fmt.Sprintf("seat-%d", i)
		id := c.addSeat(name)
		c.claimFor(id, name+"'s step")
		names[name] = id
	}
	turns := newLadderTurns()
	for name := range names {
		turns.refuse("conv-"+name, c.now.Add(-time.Hour))
	}
	turns.install(c)
	served := map[string]bool{}
	answer := refusingSeats(c, turns, names, served)
	recycled := map[string]time.Time{}
	c.run(time.Hour, 30*time.Second, func() {
		answer()
		for _, id := range names {
			if b, _ := c.sessions.Get(id); b.Metadata["restart_requested"] == "true" {
				recycled[id] = c.now
				key := "fresh-" + id
				served[key] = true
				turns.serve(key, c.now.Add(30*time.Second)) // its fresh conversation's first turn: answered
				turns.open[key] = c.now.Add(time.Minute)    // and it works on
				restartFresh(c, id, key)
			}
		}
	})
	if len(recycled) != len(names) {
		t.Fatalf("%d of %d seats got their fresh conversation in an hour: a served canary must release the queue", len(recycled), len(names))
	}
	var at []time.Time
	for _, t := range recycled {
		at = append(at, t)
	}
	slices.SortFunc(at, func(a, b time.Time) int { return a.Compare(b) })
	for i := 1; i < len(at); i++ {
		if gap := at[i].Sub(at[i-1]); gap >= ladderCanaryFlight {
			t.Fatalf("fresh recycles %v apart: the queue waited out the flight", gap)
		}
	}
	if len(c.posted) != 0 {
		t.Fatalf("drained %+v", c.posted)
	}
}

// F4/R1: when the provider refuses a canary's fresh conversation too, its
// other refused seats keep their conversations (held, paced paused) for
// ladderRefusingFor, then the next refused seat may try; one alert names the
// provider. A seat on the provider that is not refused (a seat wedged by a
// 524, the W3 wedge) is paced as usual meanwhile: counted nudges, its recycle.
func TestWorkLadderRefusingProviderHoldsOnlyRefusedSeats(t *testing.T) {
	c := newLadderCity(t)
	idA := c.claim("seat a's step")
	c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: idA, "session_key": "conv-" + ladderSeatName})
	idB := c.addSeat("seat-b")
	c.claimFor(idB, "seat b's step")
	idW := c.addSeat("seat-w") // wedged, not refused
	c.claimFor(idW, "seat w's step")
	names := map[string]string{ladderSeatName: c.seat, "seat-b": idB}
	turns := newLadderTurns()
	turns.refuse("conv-"+ladderSeatName, c.now.Add(-time.Hour))
	turns.refuse("conv-seat-b", c.now.Add(-time.Hour))
	turns.install(c)
	answer := refusingSeats(c, turns, names, map[string]bool{})
	var canary string
	c.run(time.Hour, 30*time.Second, func() {
		answer()
		for _, id := range names {
			if b, _ := c.sessions.Get(id); b.Metadata["restart_requested"] == "true" {
				if canary != "" {
					t.Fatalf("a second fresh recycle (%s after %s) while the provider refuses", id, canary)
				}
				canary = id
				restartFresh(c, id, "fresh-"+id) // its fresh conversation is refused too
				turns.refuse("fresh-"+id, c.now)
			}
		}
	})
	if canary == "" || !c.lane.canaries.refusing("claude", c.now) {
		t.Fatalf("canary %q, refusing %v: want one canary refused and the provider latched", canary, c.lane.canaries.refusing("claude", c.now))
	}
	pool := 0
	for _, a := range ladderPayloads(c, events.WorkEscalated) {
		if a.Reason == workladder.AlertProviderRateLimited {
			pool++
		}
	}
	if pool != 1 {
		t.Fatalf("%d provider_rate_limited alerts, want 1", pool)
	}
	waiting := idB
	if canary == idB {
		waiting = c.seat
	}
	for _, n := range ladderPayloads(c, events.WorkNudged) {
		if n.SessionID == waiting && n.Attempt > 1 && !n.Paused {
			t.Fatalf("the held refused seat got counted nudge %+v", n)
		}
	}
	wNudges := 0
	for _, n := range ladderPayloads(c, events.WorkNudged) {
		if n.SessionID == idW {
			if n.Paused {
				t.Fatalf("the wedged seat, not refused, was paced paused: %+v", n)
			}
			wNudges++
		}
	}
	if wNudges < 4 {
		t.Fatalf("the wedged seat got %d counted nudges in an hour, want the ladder's pace", wNudges)
	}
	// The refusal holds for ladderRefusingFor, then the next refused seat
	// may try its own fresh conversation.
	c.run(ladderRefusingFor, 30*time.Second, answer)
	if c.lane.canaries.refusing("claude", c.now) {
		t.Fatal("the provider is still refusing past ladderRefusingFor")
	}
	if b, _ := c.sessions.Get(waiting); b.Metadata["restart_requested"] != "true" && len(ladderPayloads(c, events.WorkRecycleRequested)) < 2 {
		t.Fatal("the waiting seat never got its own fresh recycle after the refusal ran out")
	}
}

// A fresh restart stops a seat whatever is attached to it, so a seat a
// person is attached to is not recycled fresh; nor is a seat holding another
// claim that waits on a person, whose conversation a restart would lose.
// These holds have no end of their own, so both seats are paced as ever:
// their silence still reaches the drain and the alerts (re-review M1).
func TestWorkLadderFreshRecycleSparesAttachedAndWaitingSeats(t *testing.T) {
	for name, setup := range map[string]func(*ladderCity){
		"attached": func(c *ladderCity) { c.sp.SetAttached(ladderSeatName, true) },
		"waits on a person for another claim": func(c *ladderCity) {
			other := c.claim("Human review before merge")
			if err := c.work.Update(other, beads.UpdateOpts{Metadata: map[string]string{beadmeta.AwaitingHumanMetadataKey: c.seat}}); err != nil {
				c.t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := newLadderCity(t)
			id := c.claim("refused")
			c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id, "session_key": "conv-1"})
			setup(c)
			turns := newLadderTurns()
			turns.refuse("conv-1", c.now.Add(-time.Hour))
			turns.install(c)
			c.run(20*time.Minute, 30*time.Second, turns.refuseNudges(c, true))
			if v := c.row().Metadata["restart_requested"]; v != "" || len(c.events(events.WorkRecycleRequested)) != 0 {
				t.Fatalf("restart_requested %q: a fresh restart of a seat it must spare", v)
			}
			if want := []time.Duration{workladder.DefaultGrace, halfPast(4), halfPast(10)}; fmt.Sprint(c.nudgedAt[:min(3, len(c.nudgedAt))]) != fmt.Sprint(want) {
				t.Fatalf("nudges %v, want the ladder's own pace %v", c.nudgedAt, want)
			}
			for _, n := range ladderPayloads(c, events.WorkNudged) {
				if n.Paused {
					t.Fatalf("nudge %+v paused: a standing hold must not silence the seat", n)
				}
			}
			// Refused for hours: the drain (its one recycle) and its alert,
			// never a fresh restart (E5, E9).
			c.run(4*time.Hour, 30*time.Second, turns.refuseNudges(c, true))
			if v := c.row().Metadata["restart_requested"]; v != "" {
				t.Fatalf("restart_requested %q under a standing hold", v)
			}
			if len(c.posted) != 1 || len(escalations(c)) == 0 {
				t.Fatalf("drains %+v, alerts %v after 4h refused: a standing hold must still reach the drain and the alerts", c.posted, escalations(c))
			}
		})
	}
}

// The attach probe answers only for a seat the rule could recycle: one never
// refused is never probed; and a probe that cannot tell holds the fresh
// recycle (a person may be attached), pacing the seat paused instead.
func TestWorkLadderAttachProbe(t *testing.T) {
	t.Run("not refused, not probed", func(t *testing.T) {
		c := newLadderCity(t)
		id := c.claim("quiet")
		c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id, "session_key": "conv-1"})
		newLadderTurns().install(c)
		c.run(30*time.Minute, 30*time.Second, nil)
		if n := c.sp.CountCalls("IsAttachedWithError", ladderSeatName); n != 0 {
			t.Fatalf("%d attach probes of a seat its provider never refused", n)
		}
	})
	t.Run("a probe that cannot tell holds", func(t *testing.T) {
		c := newLadderCity(t)
		id := c.claim("refused")
		c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id, "session_key": "conv-1"})
		c.sp.AttachedErrors[ladderSeatName] = fmt.Errorf("probe: %w", runtime.ErrRuntimeUnavailable)
		turns := newLadderTurns()
		turns.refuse("conv-1", c.now.Add(-time.Hour))
		turns.install(c)
		c.run(20*time.Minute, 30*time.Second, turns.refuseNudges(c, true))
		if v := c.row().Metadata["restart_requested"]; v != "" {
			t.Fatalf("restart_requested %q on a seat whose attachment cannot be told", v)
		}
		if c.sp.CountCalls("IsAttachedWithError", ladderSeatName) == 0 {
			t.Fatal("the refused seat was never probed")
		}
	})
}

// A quiet seat the ladder does not nudge is read again at least every
// ladderReadMaxAge: a refusal the activity clock misses is still seen.
func TestWorkLadderReadingAges(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("escalated, nudged hourly")
	key := workladder.ClaimKey{Store: "city", Bead: id}
	c.setRow(map[string]string{
		beadmeta.CurrentClaimBeadIDMetadataKey: id, "session_key": "conv-1",
		session.WorkLadderKey: workladder.EncodeLadder(workladder.Record{V: 1, Gen: 7, LastNudge: c.now, Entries: []workladder.Entry{{
			Key: key, Kind: workladder.KindClaim, Rung: workladder.RungEscalated, Since: c.now.Add(-5 * time.Hour), Attempts: 9, Recycles: 1, Escalated: true, Last: c.now,
		}}}),
	})
	turns := newLadderTurns()
	turns.install(c)
	c.run(30*time.Minute, 30*time.Second, nil) // no nudge is due for an hour
	if len(c.nudgedAt) != 0 {
		t.Fatalf("nudges %v: the fixture should be between hourly nudges", c.nudgedAt)
	}
	if want := int(30*time.Minute/ladderReadMaxAge) - 1; turns.reads < want || turns.reads > want+2 {
		t.Fatalf("%d reads in 30 minutes of an unmoving seat, want one per %v (~%d)", turns.reads, ladderReadMaxAge, want+1)
	}
}

// A standing hold wins over a queued one: an attached seat on a provider
// whose canary was refused (a queued hold that would pause it) is paced as
// ever, since a pause on a seat that cannot be restarted would never end.
func TestWorkLadderStandingHoldWinsOverQueued(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("refused, attached, on a refusing provider")
	c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id, "session_key": "conv-1"})
	c.sp.SetAttached(ladderSeatName, true)
	c.lane.canaries.recycled("claude", "another seat", 1, c.now)
	until := c.lane.canaries.refused("claude", "another seat", c.now)
	turns := newLadderTurns()
	turns.refuse("conv-1", c.now.Add(-time.Hour))
	turns.install(c)
	c.run(until.Sub(c.now)-time.Minute, 30*time.Second, turns.refuseNudges(c, true))
	for _, n := range ladderPayloads(c, events.WorkNudged) {
		if n.Paused {
			t.Fatalf("nudge %+v paused: an attached seat must not wait on a canary it can never become", n)
		}
	}
	if len(c.nudgedAt) < 5 {
		t.Fatalf("nudges %v in the refusal's hour: want the ladder's own pace", c.nudgedAt)
	}
}

// A turn the provider serves ends its refusing hold early: a quiet seat on
// the provider holding a claim, whose newest turn the provider answered after
// the refusal, is read as served.
func TestWorkLadderServedTurnEndsTheRefusal(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("served after the refusal")
	c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id, "session_key": "conv-1"})
	c.lane.canaries.recycled("claude", "another seat", 1, c.now)
	c.lane.canaries.refused("claude", "another seat", c.now)
	turns := newLadderTurns()
	turns.serve("conv-1", c.now.Add(time.Minute))
	turns.install(c)
	c.now = c.now.Add(2 * time.Minute)
	c.pass()
	if c.lane.canaries.refusing("claude", c.now) {
		t.Fatal("the provider stays refusing after a seat on it was served")
	}
}

// Round-4 M1: how long a turn is open proves nothing: a provider can take
// minutes to refuse one. A canary whose fresh conversation's first turn is
// open for minutes and then refused does not release the queue, and its
// refusal latches the provider.
func TestWorkLadderSlowRefusalIsNotService(t *testing.T) {
	c := newLadderCity(t)
	idA := c.claim("seat a's step")
	c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: idA, "session_key": "conv-" + ladderSeatName})
	idB := c.addSeat("seat-b")
	c.claimFor(idB, "seat b's step")
	names := map[string]string{ladderSeatName: c.seat, "seat-b": idB}
	turns := newLadderTurns()
	turns.refuse("conv-"+ladderSeatName, c.now.Add(-time.Hour))
	turns.refuse("conv-seat-b", c.now.Add(-time.Hour))
	turns.install(c)
	answer := refusingSeats(c, turns, names, map[string]bool{})
	var canary string
	restarted := map[string]time.Time{}
	c.run(time.Hour, 30*time.Second, func() {
		answer()
		for _, id := range names {
			if b, _ := c.sessions.Get(id); b.Metadata["restart_requested"] == "true" {
				if canary != "" && id != canary {
					t.Fatalf("a second fresh recycle (%s) at %v while the canary's first turn was a slow refusal", id, c.now.Sub(c.start))
				}
				canary = id
				key := "fresh-" + id
				restartFresh(c, id, key)
				restarted[key] = c.now
				turns.open[key] = c.now // its first turn hangs on the provider ...
				c.sp.SetActivity(map[string]string{c.seat: ladderSeatName, idB: "seat-b"}[id], c.now)
			}
		}
		for key, at := range restarted {
			if _, open := turns.open[key]; open && c.now.Sub(at) >= 3*time.Minute { // ... then is refused
				delete(turns.open, key)
				turns.refuse(key, c.now)
			}
		}
	})
	if canary == "" || !c.lane.canaries.refusing("claude", c.now) {
		t.Fatalf("canary %q, refusing %v: want the slow refusal to latch the provider", canary, c.lane.canaries.refusing("claude", c.now))
	}
}

// Round-4 S1: a failed turn read does not take a queued claim out of its
// queue: the last good reading stands for ladderReadMaxAge, so the claim is
// not counted toward the drain.
func TestWorkLadderFailedReadKeepsTheQueue(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("queued behind another seat's canary")
	c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id, "session_key": "conv-1"})
	c.lane.canaries.recycled("claude", "another seat", 1, c.now)
	c.lane.canaries.refused("claude", "another seat", c.now)
	turns := newLadderTurns()
	turns.refuse("conv-1", c.now.Add(-time.Hour))
	turns.failEvery = 3
	turns.install(c)
	c.run(50*time.Minute, 30*time.Second, turns.refuseNudges(c, true))
	counted := 0
	for _, n := range ladderPayloads(c, events.WorkNudged) {
		if !n.Paused {
			counted++
		}
	}
	if counted > 1 {
		t.Fatalf("%d counted nudges of a queued claim with a third of its reads failing: want only the first", counted)
	}
}

// Reads that keep failing never silence a refused seat: past
// ladderUnknownFor from its last good reading, it is paced as ever.
func TestWorkLadderFailingReadsDoNotSilence(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("unreadable from now on")
	c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id, "session_key": "conv-1"})
	turns := newLadderTurns()
	turns.refuse("conv-1", c.now.Add(-time.Hour))
	turns.install(c)
	c.run(2*time.Minute, 30*time.Second, turns.refuseNudges(c, true)) // a good reading, a nudge refused
	turns.err = fmt.Errorf("transcript gone: %w", runtime.ErrRuntimeUnavailable)
	c.run(time.Hour, 30*time.Second, nil)
	counted := 0
	for _, n := range ladderPayloads(c, events.WorkNudged) {
		if !n.Paused {
			counted++
		}
	}
	if counted < 3 {
		t.Fatalf("%d counted nudges in the hour its turns could not be read: want the ladder's pace once %v passed", counted, ladderUnknownFor)
	}
}

// Round-5 S1: a canary whose fresh conversation the provider served (a
// completed turn) and refused later, within the hour, raises
// rate_limited_again, not the pool alert: it holds no other seat, and the
// provider's other refused seat gets its own fresh conversation at once.
func TestWorkLadderServedFreshConversationRefusedLaterIsNotThePool(t *testing.T) {
	c := newLadderCity(t)
	idA := c.claim("seat a's step")
	c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: idA, "session_key": "conv-" + ladderSeatName})
	turns := newLadderTurns()
	turns.refuse("conv-"+ladderSeatName, c.now.Add(-time.Hour))
	turns.install(c)
	answer := refusingSeats(c, turns, map[string]string{ladderSeatName: c.seat}, map[string]bool{})
	c.run(10*time.Minute, 30*time.Second, func() {
		answer()
		if c.row().Metadata["restart_requested"] == "true" {
			restartFresh(c, c.seat, "fresh-a")
			turns.serve("fresh-a", c.now.Add(30*time.Second)) // served: a completed turn
			c.sp.SetActivity(ladderSeatName, c.now)
		}
	})
	if c.row().Metadata["session_key"] != "fresh-a" {
		t.Fatalf("seat a was not recycled fresh: %v", c.row().Metadata)
	}
	// Then the provider refuses the fresh conversation too, within the hour.
	turns.refuse("fresh-a", c.now)
	turns.refused["fresh-a"] = 1
	c.sp.SetActivity(ladderSeatName, c.now.Add(-2*time.Minute))
	c.run(15*time.Minute, 30*time.Second, answer)
	var reasons []string
	for _, a := range ladderPayloads(c, events.WorkEscalated) {
		reasons = append(reasons, a.Reason)
	}
	if fmt.Sprint(reasons) != fmt.Sprint([]string{workladder.AlertRateLimitedRecycled, workladder.AlertRateLimitedAgain}) {
		t.Fatalf("alerts %v, want the fresh recycle's, then rate_limited_again", reasons)
	}
	if c.lane.canaries.refusing("claude", c.now) {
		t.Fatal("a served fresh conversation refused later latched the provider")
	}
}

// Round-6 residue: a served canary that worked past its flight and was
// refused later shows its service behind its refusals: the newest completed
// turn anywhere in its recent turns counts, so the refusal is
// rate_limited_again, not the pool.
func TestWorkLadderServiceBehindRefusalsCounts(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("served for twenty minutes, then refused")
	recycled := c.now.Add(-30 * time.Minute)
	key := workladder.ClaimKey{Store: "city", Bead: id}
	cell := workladder.Record{V: 1, Gen: 7, RecycledGen: 6, Entries: []workladder.Entry{{
		Key: key, Kind: workladder.KindClaim, Rung: workladder.RungStarted, Since: recycled, Attempts: 1, Last: c.now.Add(-2 * time.Minute),
		Recycles: 1, FreshRecycled: true, RecycleAt: recycled,
	}}}
	c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id, "session_key": "fresh-1", session.WorkLadderKey: workladder.EncodeLadder(cell)})
	// Its first turn completed; then it went on, and the provider refused
	// twice, the newest refusal answering the ladder's nudge.
	c.lane.turnReaderFor = func(externalReadsEnv) ladderTurnReader {
		return func(context.Context, beads.Bead) (runtime.TurnHistory, error) {
			refused := func(at time.Time) runtime.EndedTurn {
				return runtime.EndedTurn{TurnID: "r" + at.String(), Status: runtime.TurnStatusFailed, EndedAt: at, Failure: runtime.TurnFailure{Code: "rate_limit", HTTPStatus: 429, RateLimited: true, Message: poolCooling}}
			}
			return runtime.TurnHistory{Recent: []runtime.EndedTurn{
				refused(c.now.Add(-2*time.Minute + time.Second)), refused(c.now.Add(-3 * time.Minute)),
				{TurnID: "served", Status: runtime.TurnStatusCompleted, EndedAt: recycled.Add(20 * time.Minute)},
			}}, nil
		}
	}
	c.pass()
	alerts := ladderPayloads(c, events.WorkEscalated)
	if len(alerts) != 1 || alerts[0].Reason != workladder.AlertRateLimitedAgain {
		t.Fatalf("alerts %+v, want rate_limited_again: the fresh conversation was served", alerts)
	}
	if c.lane.canaries.refusing("claude", c.now) {
		t.Fatal("a served fresh conversation refused later latched the provider")
	}
}

// A failed read after the seat moved to a new conversation holds nothing on
// the old conversation's refusals.
func TestWorkLadderFailedReadOfANewConversationHoldsNothing(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("restarted")
	c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id, "session_key": "conv-1"})
	turns := newLadderTurns()
	turns.refuse("conv-1", c.now.Add(-time.Hour))
	turns.refuse("conv-1", c.now.Add(-time.Minute))
	turns.install(c)
	c.pass() // a good reading of conv-1: refused twice
	c.setRow(map[string]string{"session_key": "conv-2"})
	turns.err = fmt.Errorf("not written yet: %w", runtime.ErrRuntimeUnavailable)
	c.now = c.now.Add(2 * time.Minute)
	c.pass()
	if len(c.nudgedAt) != 1 {
		t.Fatalf("nudges %v: the new conversation's claim was held on the old conversation's refusals", c.nudgedAt)
	}
}

// A failed read holds a seat's claims only when its last good reading met
// the rule's threshold: one refusal is not the rule's.
func TestWorkLadderFailedReadAfterOneRefusalHoldsNothing(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("refused once")
	c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id, "session_key": "conv-1"})
	turns := newLadderTurns()
	turns.refuse("conv-1", c.now.Add(-time.Minute))
	turns.install(c)
	c.pass()
	turns.err = fmt.Errorf("busy: %w", runtime.ErrRuntimeUnavailable)
	c.now = c.now.Add(ladderReadMaxAge + time.Minute) // the good reading aged out of the cache, not of ladderUnknownFor
	c.pass()
	if turns.reads != 2 {
		t.Fatalf("%d reads: the failed read did not happen", turns.reads)
	}
	if len(c.nudgedAt) != 1 {
		t.Fatalf("nudges %v: one refusal held the seat's claims on a failed read", c.nudgedAt)
	}
}

// A canary just restarted may not have written its transcript yet: its
// failed read is not reported.
func TestWorkLadderNewCanaryUnreadIsQuiet(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("just restarted fresh")
	c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id, "session_key": "fresh-1"})
	c.lane.canaries.recycled("claude", c.seat, 6, c.now) // recycled from generation 6; the row is at 7
	var stderr strings.Builder
	c.lane.stderr = &stderr
	turns := newLadderTurns()
	turns.err = fmt.Errorf("no single transcript of conversation fresh-1 found: %w", runtime.ErrRuntimeUnavailable)
	turns.install(c)
	c.sp.SetActivity(ladderSeatName, c.now) // working: read as the canary
	c.pass()
	if turns.reads == 0 {
		t.Fatal("the in-flight canary was not read while it works")
	}
	if strings.Contains(stderr.String(), "reading its turns") {
		t.Fatalf("a just-restarted canary's missing transcript was reported:\n%s", stderr.String())
	}
}

// Round-4 M2: a claim recycled fresh a day ago, refused again now, says
// nothing about the provider's pool: rate_limited_again, and no hold on the
// provider's other seats.
func TestWorkLadderOldFreshRecycleDoesNotLatch(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("recycled fresh yesterday")
	key := workladder.ClaimKey{Store: "city", Bead: id}
	nudged := c.now.Add(-2 * time.Minute)
	cell := workladder.Record{V: 1, Gen: 7, RecycledGen: 6, Entries: []workladder.Entry{{
		Key: key, Kind: workladder.KindClaim, Rung: workladder.RungStarted, Since: c.now.Add(-25 * time.Hour), Attempts: 1, Last: nudged,
		Recycles: 1, FreshRecycled: true, RecycleAt: c.now.Add(-26 * time.Hour),
	}}}
	c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id, "session_key": "conv-1", session.WorkLadderKey: workladder.EncodeLadder(cell)})
	turns := newLadderTurns()
	turns.refuse("conv-1", nudged.Add(-time.Minute))
	turns.refuse("conv-1", nudged.Add(time.Second))
	turns.install(c)
	c.pass()
	alerts := ladderPayloads(c, events.WorkEscalated)
	if len(alerts) != 1 || alerts[0].Reason != workladder.AlertRateLimitedAgain {
		t.Fatalf("alerts %+v, want one rate_limited_again", alerts)
	}
	if c.lane.canaries.refusing("claude", c.now) {
		t.Fatal("an old fresh recycle refused again latched the provider")
	}
}

func TestRateLimitedTurns(t *testing.T) {
	at := time.Date(2026, 10, 19, 7, 0, 0, 0, time.UTC)
	refused := func(d time.Duration) runtime.EndedTurn {
		return runtime.EndedTurn{TurnID: "r", Status: runtime.TurnStatusFailed, EndedAt: at.Add(d), Failure: runtime.TurnFailure{Code: "rate_limit", HTTPStatus: 429, RateLimited: true, Message: "cooling"}}
	}
	overloaded := runtime.EndedTurn{TurnID: "o", Status: runtime.TurnStatusFailed, EndedAt: at, Failure: runtime.TurnFailure{Code: "server_error", HTTPStatus: 529}}
	answered := runtime.EndedTurn{TurnID: "a", Status: runtime.TurnStatusCompleted, EndedAt: at.Add(time.Hour)}
	servedBefore := runtime.EndedTurn{TurnID: "s", Status: runtime.TurnStatusCompleted, EndedAt: at.Add(-time.Hour)}
	for _, tc := range []struct {
		name      string
		hist      runtime.TurnHistory
		refused   int
		refusedAt time.Time
		servedAt  time.Time
	}{
		{"two refusals, newest first, after a served turn", runtime.TurnHistory{Recent: []runtime.EndedTurn{refused(2 * time.Minute), refused(0), servedBefore}}, 2, at.Add(2 * time.Minute), at.Add(-time.Hour)},
		{"an overload is not a rate limit", runtime.TurnHistory{Recent: []runtime.EndedTurn{overloaded, overloaded, refused(0)}}, 0, time.Time{}, time.Time{}},
		{"counted up to the first that was not refused", runtime.TurnHistory{Recent: []runtime.EndedTurn{refused(0), overloaded, refused(0)}}, 1, at, time.Time{}},
		{"a turn still open has no answer yet", runtime.TurnHistory{Current: runtime.TurnState{TurnID: "open"}, Recent: []runtime.EndedTurn{refused(0), refused(0)}}, 0, time.Time{}, time.Time{}},
		{"served", runtime.TurnHistory{Recent: []runtime.EndedTurn{answered, refused(0)}}, 0, time.Time{}, at.Add(time.Hour)},
		{"served, a newer turn open", runtime.TurnHistory{Current: runtime.TurnState{TurnID: "open"}, CurrentStart: at, Recent: []runtime.EndedTurn{answered}}, 0, time.Time{}, at.Add(time.Hour)},
		{"an open turn alone is not service", runtime.TurnHistory{Current: runtime.TurnState{TurnID: "open"}, CurrentStart: at.Add(-time.Hour), Recent: []runtime.EndedTurn{refused(0)}}, 0, time.Time{}, time.Time{}},
		{"nothing yet", runtime.TurnHistory{}, 0, time.Time{}, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, rat, last, served := rateLimitedTurns(tc.hist)
			if n != tc.refused || !rat.Equal(tc.refusedAt) || !served.Equal(tc.servedAt) || (n > 0) != (last.Message != "") {
				t.Fatalf("rateLimitedTurns = %d, %v, %+v, %v; want %d, %v, served %v", n, rat, last, served, tc.refused, tc.refusedAt, tc.servedAt)
			}
		})
	}
}

func TestLadderCanaries(t *testing.T) {
	now := time.Date(2026, 10, 19, 7, 0, 0, 0, time.UTC)
	var c ladderCanaries
	if c.held("glm", "a", now) || c.refusing("glm", now) {
		t.Fatal("no canary yet holds nothing")
	}
	c.recycled("glm", "a", 3, now)
	if c.held("glm", "a", now) || !c.held("glm", "b", now) || c.held("qwen", "b", now) {
		t.Fatal("an in-flight canary holds the provider's other seats only")
	}
	if c.inFlight("glm", "a", 3, now) || !c.inFlight("glm", "a", 4, now) || c.inFlight("glm", "b", 4, now) {
		t.Fatal("inFlight is the canary seat at a generation after its recycle")
	}
	c.served("glm", now.Add(-time.Minute)) // a turn from before it ends nothing
	if !c.held("glm", "b", now) {
		t.Fatal("a turn served before the canary ended it")
	}
	if c.held("glm", "b", now.Add(ladderCanaryFlight)) {
		t.Fatal("a canary past its flight still holds")
	}

	// Refusals in a row double the hold, up to ladderRefusingMax.
	at := now
	for i, want := range []time.Duration{ladderRefusingFor, 2 * ladderRefusingFor, 4 * ladderRefusingFor} {
		c.recycled("glm", "a", int64(i), at)
		until := c.refused("glm", "a", at.Add(5*time.Minute))
		if got := until.Sub(at.Add(5 * time.Minute)); got != want {
			t.Fatalf("refusal %d holds %v, want %v", i+1, got, want)
		}
		if !c.refusing("glm", until.Add(-time.Second)) || !c.held("glm", "b", until.Add(-time.Second)) || !c.held("glm", "a", until.Add(-time.Second)) {
			t.Fatalf("refusal %d does not hold every seat until %v", i+1, until)
		}
		c.recycled("glm", "b", 9, until.Add(-time.Minute)) // a recycle does not end a refusal
		if !c.refusing("glm", until.Add(-time.Second)) {
			t.Fatal("a recycle ended the refusal")
		}
		if c.refusing("glm", until) || c.held("glm", "b", until) {
			t.Fatalf("refusal %d still holds at %v", i+1, until)
		}
		at = until
	}
	for range 10 {
		c.recycled("glm", "a", 1, at)
		at = c.refused("glm", "a", at)
	}
	c.recycled("glm", "a", 1, at)
	if got := c.refused("glm", "a", at).Sub(at); got != ladderRefusingMax {
		t.Fatalf("a long streak holds %v, want the cap %v", got, ladderRefusingMax)
	}
	// A turn served after the last refusal ends it and starts the streak again.
	c.served("glm", at.Add(time.Minute))
	if c.refusing("glm", at.Add(2*time.Minute)) || c.held("glm", "b", at.Add(2*time.Minute)) {
		t.Fatal("a turn served after the refusal did not end it")
	}
	c.recycled("glm", "a", 1, at.Add(3*time.Minute))
	if got := c.refused("glm", "a", at.Add(4*time.Minute)).Sub(at.Add(4 * time.Minute)); got != ladderRefusingFor {
		t.Fatalf("after a served turn a refusal holds %v, want %v again", got, ladderRefusingFor)
	}
	// A day with no refusal starts a new episode.
	var e ladderCanaries
	e.recycled("glm", "a", 1, now)
	first := e.refused("glm", "a", now)
	e.recycled("glm", "a", 1, first)
	second := e.refused("glm", "a", first)
	if got := second.Sub(first); got != 2*ladderRefusingFor {
		t.Fatalf("the second refusal in a row holds %v, want %v", got, 2*ladderRefusingFor)
	}
	later := second.Add(ladderRefusingMax + time.Hour)
	e.recycled("glm", "a", 1, later)
	if got := e.refused("glm", "a", later).Sub(later); got != ladderRefusingFor {
		t.Fatalf("a refusal a day after the last holds %v, want %v again", got, ladderRefusingFor)
	}
	// A turn served while no canary is current still resets the streak.
	var d ladderCanaries
	d.recycled("glm", "a", 1, now)
	until := d.refused("glm", "a", now)
	d.served("glm", until.Add(time.Hour)) // after the hold ran out
	d.recycled("glm", "a", 1, until.Add(2*time.Hour))
	if got := d.refused("glm", "a", until.Add(2*time.Hour)).Sub(until.Add(2 * time.Hour)); got != ladderRefusingFor {
		t.Fatalf("a served turn after the hold did not reset the streak: holds %v", got)
	}
}
