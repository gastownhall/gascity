package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
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
// heartbeat hold, a wait hold, a start in flight, an operator's content
// hold on the conversation (SESSION-RUNTIME-027).
func TestWorkLadderHoldsUnderAGate(t *testing.T) {
	for name, meta := range map[string]func(now time.Time) map[string]string{
		"draining": func(now time.Time) map[string]string {
			return map[string]string{session.DrainIntentReasonKey: drainOrphaned, session.DrainIntentAtKey: now.Format(time.RFC3339), session.DrainIntentIncarnationKey: "7"}
		},
		"content held": func(now time.Time) map[string]string {
			return map[string]string{session.ContentHoldKey: session.ContentHold{By: "operator", At: now}.String()}
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
		if err != nil || !cell.Names(key, 7) {
			t.Fatalf("store %q: the ladder reads %q as %+v, %v; want it naming %v", store, raw, cell, err, key)
		}
	}
	// The two decoders of the one cell agree on what it names, resident and
	// unmarked entries alike, at every generation: the hook's writer and the
	// ladder's reader can never disagree on whether a claim is started.
	for _, resident := range []bool{false, true} {
		for _, entryGen := range []int64{0, 1, 2} {
			raw := session.EncodeWorkStarted(session.WorkStarted{}.With(session.WorkStartedEntry{
				Store: "rig:gascity", Bead: "gcg-1", Gen: entryGen, At: stepsTestNow, Via: session.WorkStartedViaClaim, Resident: resident,
			}))
			hook, err := session.DecodeWorkStarted(raw)
			if err != nil {
				t.Fatal(err)
			}
			ladderCell, err := workladder.DecodeStarted(raw)
			if err != nil {
				t.Fatal(err)
			}
			for _, k := range []workladder.ClaimKey{{Store: "city", Bead: "gcg-1"}, {Store: "rig:gascity", Bead: "gcg-1"}, {Store: "rig:other", Bead: "gcg-1"}, {Bead: "gcg-1"}, {Store: "city", Bead: "gcg-2"}} {
				for _, rowGen := range []int64{0, 1, 2, 3} {
					if h, l := hook.Names(k.Store, k.Bead, rowGen), ladderCell.Names(k, rowGen); h != l {
						t.Errorf("%s: key %+v at row gen %d: the hook reads %v, the ladder %v", raw, k, rowGen, h, l)
					}
				}
			}
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
