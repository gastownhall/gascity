package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	c.lane, _ = newPoolStepsTestLane(c.env, c.summary, func(r executionStalledRequest) { c.posted = append(c.posted, r) })
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
	r, err := workladder.DecodeLadder(c.row().Metadata[workLadderKey])
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
	c.t.Fatalf("no ladder entry for %s in %s", bead, c.row().Metadata[workLadderKey])
	return workladder.Entry{}
}

func (c *ladderCity) setRow(kvs map[string]string) {
	c.t.Helper()
	if err := c.sessions.SetMetadataBatch(c.seat, kvs); err != nil {
		c.t.Fatal(err)
	}
}

func (c *ladderCity) events(typ string) []events.Event {
	evts, _ := c.rec.List(events.Filter{Type: typ})
	return evts
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
	if esc := c.events(events.WorkEscalated); len(esc) != 1 || !strings.Contains(string(esc[0].Payload), workladder.AlertStartedUnanswered) {
		t.Fatalf("work.escalated = %+v, want one started_unanswered", esc)
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
		t.Fatalf("cell %s, want the release cleared and the entry gone", c.row().Metadata[workLadderKey])
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
	c.setRow(map[string]string{workLadderKey: workladder.EncodeLadder(prev)})
	race := &agentRaceStore{MemStore: c.sessions, row: c.seat, agent: func() {
		started := workladder.EncodeStarted(workladder.Started{V: 1, Claims: []workladder.StartedClaim{{Key: key, Gen: 7, At: c.now, Via: workladder.ViaClaim}}})
		if err := c.sessions.SetMetadataBatch(c.seat, map[string]string{workStartedKey: started}); err != nil {
			t.Error(err)
		}
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
		t.Fatalf("cell %s, want the claim started and no release", c.row().Metadata[workLadderKey])
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
	c.setRow(map[string]string{workLadderKey: workladder.EncodeLadder(prev)})
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
	c.setRow(map[string]string{workLadderKey: workladder.EncodeLadder(prev), beadmeta.CurrentClaimBeadIDMetadataKey: id})
	c.pass()
	if b, _ := c.work.Get(id); b.Status != "in_progress" {
		t.Fatalf("claim %q, want kept", b.Status)
	}
	if e := c.entry(id); e.Rung != workladder.RungStarted || e.Seed != workladder.SrcCurrentClaim || c.cell().Release != nil {
		t.Fatalf("cell %s, want the claim started by its seed", c.row().Metadata[workLadderKey])
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
	c.setRow(map[string]string{workLadderKey: workladder.EncodeLadder(prev), workStartedKey: agent})
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
	if got := len(c.events(events.WorkEscalated)); got != 1 {
		t.Fatalf("work.escalated = %d, want 1", got)
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
		"wait hold": func(time.Time) map[string]string { return map[string]string{"wait_hold": "true"} },
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
			marker, cell := row.Metadata[idleClaimNudgeTriggerKey], row.Metadata[workLadderKey]
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
			r, err := workladder.DecodeLadder(mustGetTestBead(t, store, "session-bead-a").Metadata[workLadderKey])
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
