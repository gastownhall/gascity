package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/workladder"
)

// W3 rule 1 (#8132 split C3): a seat's rate-limit episode, end to end
// through the lane.

// coolingRefusal is the provider's own words at maintainer-city's 07:17Z
// pool cooldown, as the agent CLI classified them.
var coolingRefusal = runtime.TurnFailure{
	Code: "rate_limit", HTTPStatus: 429, RateLimited: true,
	Message: "API Error: Request rejected (429) · pool cooling for another 3d 16h; every account is in a rate-limit cool-down, so retrying before then cannot succeed",
}

// rlSeat is one seat of an rlWorld: it answers every nudge it takes with a
// turn that ends delay later, which its provider refuses (refuse, by
// conversation) or serves, and a stand-in reconciler restarts it on a new
// conversation restartAfter passes after the ladder asks (only the
// generation moves when loseRestart is set).
type rlSeat struct {
	id, name     string
	claims       []string
	stores       map[string]beads.Store // a claim's store for live reads, when not the city's work store
	delay        time.Duration
	refuse       func(conv int64) bool
	failure      runtime.TurnFailure
	quiet        bool  // answers move no activity clock, and no turn shows open (07:17Z: the 429 moved nothing)
	readErr      error // the seat's turns cannot be read
	restartAfter int
	loseRestart  bool

	delivered []time.Time // nudges the seat took, in its current conversation
	hist      runtime.TurnHistory
	pending   int
	restarts  []time.Time
	nudges    int
	reads     int
}

// rlWorld is a ladderCity of one or more seats on a provider that refuses
// with a rate limit. With lc set, seat 0's nudges are lane notices, which
// the world delivers as soon as they are proposed (the attempt starts and
// succeeds at the pass's time).
type rlWorld struct {
	*ladderCity
	lc    *ladderLaneCity
	seats []*rlSeat
	wakes int
}

func alwaysRefuse(int64) bool { return true }

// newRLWorld is a world whose seat 0 is the city's seat; each provider names
// one seat, in order.
func newRLWorld(t *testing.T, lane bool, providers ...string) *rlWorld {
	t.Helper()
	w := &rlWorld{}
	if lane {
		w.lc = newLadderLaneCity(t, "message")
		w.ladderCity = w.lc.ladderCity
	} else {
		w.ladderCity = newLadderCity(t)
	}
	for i, provider := range providers {
		s := &rlSeat{id: w.seat, name: ladderSeatName, delay: 2 * time.Minute, refuse: alwaysRefuse, failure: coolingRefusal, restartAfter: 2}
		if i == 0 {
			w.setRow(map[string]string{"provider": provider})
		} else {
			s.name = fmt.Sprintf("seat-%c", 'a'+i)
			row, err := w.sessions.Create(beads.Bead{Title: "seat", Type: "session", Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
				"session_name": s.name, "pool_managed": "true", "template": "worker", "state": "active",
				"generation": "7", "instance_token": "tok-7-" + s.name, "provider": provider,
			}})
			if err != nil {
				t.Fatal(err)
			}
			s.id = row.ID
			if err := w.sp.Start(context.Background(), s.name, runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			w.sp.SetActivity(s.name, w.now.Add(-time.Hour))
		}
		w.seats = append(w.seats, s)
	}
	w.install(w.lane)
	return w
}

// install points lane at the world: its allocation, its turns, the planner's
// wake.
func (w *rlWorld) install(lane *externalReadsLane) {
	lane.summary = w.summary
	lane.now = func() time.Time { return w.now }
	lane.events = w.rec
	lane.onChange = func() { w.wakes++ }
	lane.turnReaderFor = func(externalReadsEnv) ladderTurnReader {
		return func(_ context.Context, row beads.Bead) (runtime.TurnHistory, error) {
			for _, s := range w.seats {
				if s.id == row.ID {
					s.reads++
					return s.hist, s.readErr
				}
			}
			return runtime.TurnHistory{}, fmt.Errorf("no seat %s", row.ID)
		}
	}
}

// restartController replaces the lane with a new one, as a controller
// restart does: everything it held in memory is gone.
func (w *rlWorld) restartController() {
	w.t.Helper()
	lane, _ := newPoolStepsTestLane(w.env, w.summary, func(r executionStalledRequest) { w.posted = append(w.posted, r) })
	w.install(lane)
	w.lane = lane
}

// claimFor adds an in_progress claim the hook handed to s (its bead's
// gc.session_id names the seat, so it is started).
func (w *rlWorld) claimFor(s *rlSeat, title string) string {
	w.t.Helper()
	b, err := w.work.Create(beads.Bead{Title: title, Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}})
	if err != nil {
		w.t.Fatal(err)
	}
	inProgress := "in_progress"
	if err := w.work.Update(b.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &s.id, Metadata: map[string]string{beadmeta.SessionIDMetadataKey: s.id}}); err != nil {
		w.t.Fatal(err)
	}
	s.claims = append(s.claims, b.ID)
	return b.ID
}

// summary is the planner's allocation: every seat's claims as they stand.
func (w *rlWorld) summary() *allocSummary {
	sum := &allocSummary{At: w.now}
	for _, s := range w.seats {
		for _, id := range s.claims {
			b, err := w.work.Get(id)
			if err != nil || b.Status != "in_progress" {
				continue
			}
			var st beads.Store = w.work
			if o := s.stores[id]; o != nil {
				st = o
			}
			sum.AssignedWork, sum.AssignedStores, sum.AssignedStoreRefs = append(sum.AssignedWork, b), append(sum.AssignedStores, st), append(sum.AssignedStoreRefs, "")
		}
	}
	if w.trigger != "" {
		sum.ReadyRouted, sum.ReadyRoutedRefs = []beads.Bead{{ID: w.trigger, Status: "open"}}, []string{""}
	}
	return sum
}

func (w *rlWorld) rowOf(s *rlSeat) beads.Bead {
	w.t.Helper()
	b, err := w.sessions.Get(s.id)
	if err != nil {
		w.t.Fatal(err)
	}
	return b
}

func (w *rlWorld) cellOf(s *rlSeat) workladder.Record {
	w.t.Helper()
	r, err := workladder.DecodeLadder(w.rowOf(s).Metadata[session.WorkLadderKey])
	if err != nil {
		w.t.Fatal(err)
	}
	return r
}

// turnsOf builds s's turns at now: one per nudge it took in its current
// conversation, ended delay after it, refused or served.
func (w *rlWorld) turnsOf(s *rlSeat) {
	conv := rowConversation(w.rowOf(s).Metadata)
	s.hist = runtime.TurnHistory{ObservedAt: w.now}
	var last time.Time
	open := false
	for i, d := range s.delivered {
		end := d.Add(s.delay)
		if end.After(w.now) {
			open = true
			continue
		}
		turn := runtime.EndedTurn{TurnID: fmt.Sprintf("c%d-t%d", conv, i), StartedAt: d, EndedAt: end, Status: runtime.TurnStatusCompleted}
		if s.refuse(conv) {
			turn.Status, turn.Failure = runtime.TurnStatusFailed, s.failure
		}
		s.hist.Recent = append([]runtime.EndedTurn{turn}, s.hist.Recent...)
		last = end
	}
	switch {
	case s.quiet:
	case open:
		s.hist.Current = runtime.TurnState{TurnID: "open"}
		w.sp.SetActivity(s.name, w.now.Add(-10*time.Second)) // busy
	case !last.IsZero():
		w.sp.SetActivity(s.name, last)
	}
}

// step runs one pass at w.now, then the seats take what it sent and the
// stand-in reconciler restarts the seats the ladder asked it to.
func (w *rlWorld) step() {
	w.t.Helper()
	for _, s := range w.seats {
		w.turnsOf(s)
	}
	w.pass()
	for i, s := range w.seats {
		if n := w.sp.CountCalls("Nudge", s.name); n > s.nudges {
			for range n - s.nudges {
				s.delivered = append(s.delivered, w.now)
			}
			s.nudges = n
		}
		if i == 0 && w.lc != nil {
			for _, op := range w.lc.ladderOps() {
				w.lc.f.clk.mu.Lock()
				w.lc.f.clk.now = w.now
				w.lc.f.clk.mu.Unlock()
				w.lc.startOp(op.ID)
				w.lc.closeOp(op.ID, succeeded)
				s.delivered = append(s.delivered, w.now)
			}
		}
		w.reconcile(s)
	}
}

// reconcile restarts s fresh once restartAfter passes saw its request: a
// new generation and incarnation on a new conversation (the same one when
// the restart is lost).
func (w *rlWorld) reconcile(s *rlSeat) {
	row := w.rowOf(s)
	if row.Metadata["restart_requested"] != "true" {
		return
	}
	if s.pending++; s.pending < s.restartAfter {
		return
	}
	gen := rowGeneration(row.Metadata) + 1
	conv := rowConversation(row.Metadata)
	if !s.loseRestart {
		conv++
	}
	casSetRow(w.t, w.sessions, s.id, map[string]string{
		"generation": strconv.FormatInt(gen, 10), "instance_token": fmt.Sprintf("tok-%d-%s", gen, s.name),
		"continuation_epoch": strconv.FormatInt(conv, 10), "restart_requested": "", "continuation_reset_pending": "",
	})
	s.pending, s.delivered = 0, nil
	s.restarts = append(s.restarts, w.now)
	w.sp.SetActivity(s.name, w.now)
}

// runUntil steps every 30s until done reports true or d has passed, and
// reports whether done did.
func (w *rlWorld) runUntil(d time.Duration, done func() bool) bool {
	for end := w.now.Add(d); w.now.Before(end); w.now = w.now.Add(30 * time.Second) {
		w.step()
		if done != nil && done() {
			w.now = w.now.Add(30 * time.Second)
			return true
		}
	}
	return false
}

// moves are s's work.rate_limited payloads, in order.
func (w *rlWorld) moves(s *rlSeat) []events.WorkRateLimitedPayload {
	w.t.Helper()
	var out []events.WorkRateLimitedPayload
	for _, e := range w.events(events.WorkRateLimited) {
		var p events.WorkRateLimitedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			w.t.Fatal(err)
		}
		if e.Subject != p.SessionID || e.SessionID != p.SessionID {
			w.t.Fatalf("work.rate_limited envelope subject %q session %q, payload seat %q", e.Subject, e.SessionID, p.SessionID)
		}
		if p.SessionID == s.id {
			out = append(out, p)
		}
	}
	return out
}

// transitions are s's moves as transition(cause).
func (w *rlWorld) transitions(s *rlSeat) []string {
	var out []string
	for _, p := range w.moves(s) {
		m := p.Transition
		if p.Cause != "" {
			m += "(" + p.Cause + ")"
		}
		out = append(out, m)
	}
	return out
}

// alertsOf are s's work.escalated reasons and messages, in order.
func (w *rlWorld) alertsOf(s *rlSeat) (reasons, messages []string) {
	w.t.Helper()
	for _, e := range w.events(events.WorkEscalated) {
		var p events.WorkLadderPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			w.t.Fatal(err)
		}
		if p.SessionID == s.id {
			reasons, messages = append(reasons, p.Reason), append(messages, e.Message)
		}
	}
	return reasons, messages
}

func (w *rlWorld) phaseOf(s *rlSeat) workladder.RateLimitPhase { return w.cellOf(s).RateLimit.Phase }

// unhealthy marks provider unhealthy in provider-health.json (a pack's hold).
func (w *rlWorld) unhealthy(providers ...string) {
	w.t.Helper()
	dir := filepath.Join(w.env.CityPath, ".gc", "cache")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		w.t.Fatal(err)
	}
	var recs []providerHealthRecord
	for _, provider := range providers {
		if provider != "" {
			recs = append(recs, providerHealthRecord{Provider: provider, Status: "unhealthy", ProbedAt: float64(time.Now().Unix())})
		}
	}
	data, _ := json.Marshal(providerHealthFileFormat{Providers: recs})
	if err := os.WriteFile(filepath.Join(dir, "provider-health.json"), data, 0o644); err != nil {
		w.t.Fatal(err)
	}
}

// ga-ylpbsy.40, replayed. A refusal that answers a nudge the seat took opens
// the seat's episode on the first pass that may act on it and reads it,
// however long after the delivery it ends (a slow refusal ending past
// ProgressReset was itself the progress that disarmed #8132's rule), on the
// direct route and on a lane notice. The episode restarts the seat fresh
// once FreshConfirm has passed: the request lands with the cell (the claim
// kept, no planner drain, the planner woken), and the entry's count was
// never reset by the slow refusals.
func TestWorkLadderSlowRefusalOpensItsEpisode(t *testing.T) {
	for _, route := range []string{"direct", "lane"} {
		for _, delay := range []time.Duration{2 * time.Second, 9*time.Minute + 59*time.Second, 10*time.Minute + 30*time.Second, 19 * time.Minute} {
			t.Run(fmt.Sprintf("%s/%v", route, delay), func(t *testing.T) {
				w := newRLWorld(t, route == "lane", "claude")
				s := w.seats[0]
				s.delay, s.restartAfter = delay, 1<<30
				id := w.claimFor(s, "the step")
				var firstSince, openedAt time.Time
				restarted := w.runUntil(8*time.Hour, func() bool {
					if r := w.cellOf(s); len(r.Entries) > 0 && firstSince.IsZero() {
						firstSince = r.Entries[0].Since
					}
					if openedAt.IsZero() && w.phaseOf(s) != workladder.RateLimitClear {
						openedAt = w.now
					}
					return w.rowOf(s).Metadata["restart_requested"] == "true"
				})
				r, row := w.cellOf(s), w.rowOf(s)
				if !restarted || len(s.delivered) < 2 {
					t.Fatalf("no fresh restart in 8h: delivered %v, cell %s", s.delivered, row.Metadata[session.WorkLadderKey])
				}
				since := s.delivered[1].Add(delay) // the second refusal answered the second nudge
				if !r.RateLimit.Since.Equal(since) {
					t.Fatalf("episode since %v, want the refusal answering the second nudge (%v)", r.RateLimit.Since, since)
				}
				if gap := openedAt.Sub(since); gap < 0 || gap > idleClaimNudgeGrace+time.Minute {
					t.Fatalf("opened %v after the refusal, want the first acting pass that read it", gap)
				}
				restartAt := w.now.Add(-30 * time.Second)
				if gap := restartAt.Sub(since); gap < workladder.DefaultPausedCap || gap > workladder.DefaultPausedCap+delay+workladder.DefaultPausedCap+5*time.Minute {
					t.Fatalf("restarted %v after the episode opened, want FreshConfirm (%v) or one paced answer later", gap, workladder.DefaultPausedCap)
				}
				if row.Metadata["continuation_reset_pending"] != "true" || r.RateLimit.Phase != workladder.RateLimitRestarted ||
					!r.RateLimit.LastFresh.Equal(restartAt) || r.RecycledGen != 7 {
					t.Fatalf("row %v, cell %s: want gc session reset's request beside the restarted episode", row.Metadata, row.Metadata[session.WorkLadderKey])
				}
				e := r.Entries[0]
				if !e.Since.Equal(firstSince) || e.Attempts == 0 {
					t.Fatalf("entry %+v (first since %v): the slow refusals restarted its count", e, firstSince)
				}
				if b, _ := w.work.Get(id); b.Status != "in_progress" || b.Assignee != s.id || len(w.posted) != 0 || w.wakes == 0 {
					t.Fatalf("claim %q %q, drains %+v, wakes %d: want the claim kept, no drain, the planner woken", b.Status, b.Assignee, w.posted, w.wakes)
				}
				if got := w.transitions(s); !slices.Equal(got, []string{"opened", "restarted"}) {
					t.Fatalf("work.rate_limited %v", got)
				}
				m := w.moves(s)[1]
				if m.Provider != "claude" || m.ErrorCode != "rate_limit" || m.HTTPStatus != 429 || !slices.Equal(m.Claims, []string{id}) || m.FreshAt == nil || m.Generation != 7 {
					t.Fatalf("restarted payload %+v", m)
				}
			})
		}
	}
}

// The 07:17Z pool cooldown, replayed (ported from #8132's
// TestWorkLadderRecyclesARateLimitedSeatFresh): every account cools for
// days. The episode opens, restarts the seat fresh after FreshConfirm, and
// when the fresh conversation is refused too it raises its one alert,
// quoting the provider's words; the seat is then paced paused, never
// restarted or alerted again, until the provider serves it, which closes the
// episode. Whether the refusals move the activity clock or not.
func TestWorkLadderReplaysThePoolCooldown(t *testing.T) {
	for _, quiet := range []bool{false, true} {
		t.Run(fmt.Sprintf("refusals quiet %v", quiet), func(t *testing.T) {
			w := newRLWorld(t, false, "claude")
			s := w.seats[0]
			s.quiet = quiet
			id := w.claimFor(s, "the step")
			if !w.runUntil(4*time.Hour, func() bool { return w.phaseOf(s) == workladder.RateLimitExhausted }) {
				t.Fatalf("not exhausted in 4h: %v, cell %s", w.transitions(s), w.rowOf(s).Metadata[session.WorkLadderKey])
			}
			if got := w.transitions(s); !slices.Equal(got, []string{"opened", "restarted", "exhausted(fresh_refused)"}) {
				t.Fatalf("work.rate_limited %v", got)
			}
			r := w.cellOf(s)
			if len(s.restarts) != 1 || r.RateLimit.FreshAt.Sub(r.RateLimit.Since) < workladder.DefaultPausedCap {
				t.Fatalf("restarts %v, episode %+v: want one, FreshConfirm after it opened", s.restarts, r.RateLimit)
			}
			reasons, messages := w.alertsOf(s)
			if !slices.Equal(reasons, []string{workladder.AlertRateLimitedFreshRefused}) ||
				!strings.Contains(messages[0], "provider claude") || !strings.Contains(messages[0], "pool cooling for another 3d 16h") {
				t.Fatalf("alerts %v %q, want one fresh_refused quoting the provider", reasons, messages)
			}
			if m := w.moves(s); !m[2].Alerted || m[2].Conversation != 2 {
				t.Fatalf("exhausted move %+v, want it to carry the alert, in the fresh conversation", m[2])
			}

			before, attempts := len(s.delivered), w.cellOf(s).Entries[0].Attempts
			at := w.now
			w.runUntil(90*time.Minute, nil)
			if reasons, _ := w.alertsOf(s); len(reasons) != 1 || len(s.restarts) != 1 || len(w.transitions(s)) != 3 {
				t.Fatalf("over 90m paced paused: alerts %v, restarts %v, moves %v", reasons, s.restarts, w.transitions(s))
			}
			took := append([]time.Time{at}, s.delivered[before:]...)
			if len(took) < 5 {
				t.Fatalf("nudges taken while exhausted %v, want them paced on", took)
			}
			for i := 2; i < len(took); i++ {
				if gap := took[i].Sub(took[i-1]); gap < workladder.DefaultPausedCap {
					t.Fatalf("paused nudges %v apart while exhausted", gap)
				}
			}
			if got := w.cellOf(s).Entries[0].Attempts; got != attempts || len(w.posted) != 0 {
				t.Fatalf("attempts %d -> %d, drains %+v: an exhausted episode counts nothing", attempts, got, w.posted)
			}

			s.refuse = func(int64) bool { return false }
			if !w.runUntil(time.Hour, func() bool { return w.phaseOf(s) == workladder.RateLimitClear }) {
				t.Fatal("a served seat's episode never closed")
			}
			if got := w.transitions(s); got[len(got)-1] != "closed(served)" {
				t.Fatalf("work.rate_limited %v, want closed by service", got)
			}
			if r := w.cellOf(s); !r.RateLimit.IsZero() && w.now.Sub(r.RateLimit.LastFresh) >= workladder.DefaultCap {
				t.Fatalf("closed episode %+v: want only a fresh restart younger than both gaps kept", r.RateLimit)
			}
			if b, _ := w.work.Get(id); b.Status != "in_progress" || b.Assignee != s.id {
				t.Fatalf("the claim is %q %q", b.Status, b.Assignee)
			}
		})
	}
}

// The episode is the seat's, not each claim's: a seat holding two started
// claims and a trigger gets one fresh restart, one alert, and one
// work.rate_limited per move, naming both claims.
func TestWorkLadderRateLimitIsTheSeats(t *testing.T) {
	w := newRLWorld(t, false, "claude")
	s := w.seats[0]
	a, b := w.claimFor(s, "first step"), w.claimFor(s, "second step")
	trig, err := w.work.Create(beads.Bead{Title: "trigger", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}})
	if err != nil {
		t.Fatal(err)
	}
	w.trigger = trig.ID
	w.setRow(map[string]string{beadmeta.TriggerBeadIDMetadataKey: trig.ID})
	if !w.runUntil(4*time.Hour, func() bool { return w.phaseOf(s) == workladder.RateLimitExhausted }) {
		t.Fatalf("not exhausted: %v", w.transitions(s))
	}
	w.runUntil(2*time.Hour, nil)
	reasons, _ := w.alertsOf(s)
	if got := w.transitions(s); !slices.Equal(got, []string{"opened", "restarted", "exhausted(fresh_refused)"}) || len(s.restarts) != 1 ||
		!slices.Equal(reasons, []string{workladder.AlertRateLimitedFreshRefused}) {
		t.Fatalf("moves %v, restarts %v, alerts %v: want one of each", got, s.restarts, reasons)
	}
	want := []string{a, b}
	slices.Sort(want)
	for _, m := range w.moves(s) {
		if got := slices.Sorted(slices.Values(m.Claims)); !slices.Equal(got, want) {
			t.Fatalf("%s move names claims %v, want %v", m.Transition, m.Claims, want)
		}
	}
	if len(w.posted) != 0 {
		t.Fatalf("drains %+v: an episode recycles nothing", w.posted)
	}
}

// Every decision input is durable (WL6, RL6): a controller restart in the
// open episode, and another after its fresh restart, repeat neither the
// restart nor the alert.
func TestWorkLadderRateLimitSurvivesControllerRestart(t *testing.T) {
	w := newRLWorld(t, false, "claude")
	s := w.seats[0]
	w.claimFor(s, "the step")
	swapped := map[workladder.RateLimitPhase]bool{}
	w.runUntil(4*time.Hour, func() bool {
		if ph := w.phaseOf(s); (ph == workladder.RateLimitOpen || ph == workladder.RateLimitRestarted) && !swapped[ph] {
			swapped[ph] = true
			w.restartController()
		}
		return w.phaseOf(s) == workladder.RateLimitExhausted
	})
	w.restartController()
	w.runUntil(2*time.Hour, nil)
	reasons, _ := w.alertsOf(s)
	if !swapped[workladder.RateLimitOpen] || !swapped[workladder.RateLimitRestarted] {
		t.Fatalf("the controller was restarted in %v", swapped)
	}
	if got := w.transitions(s); !slices.Equal(got, []string{"opened", "restarted", "exhausted(fresh_refused)"}) || len(s.restarts) != 1 || len(reasons) != 1 {
		t.Fatalf("moves %v, restarts %v, alerts %v across controller restarts", got, s.restarts, reasons)
	}
}

// A refusal answers only a nudge that reached the seat: after a notice that
// was withdrawn, refused by the seat, or failed, the seat's Delivered does
// not move and no episode opens, however the provider refused its turns.
func TestWorkLadderRefusalAfterAnUndeliveredNoticeIsNoAnswer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		settle func(lc *ladderLaneCity, op opjournal.Op)
		opens  bool
	}{
		{"delivered", func(lc *ladderLaneCity, op opjournal.Op) { lc.startOp(op.ID); lc.closeOp(op.ID, succeeded) }, true},
		{"withdrawn", func(lc *ladderLaneCity, op opjournal.Op) {
			if _, _, err := lc.st().Cancel(op.ID, "operator", "not now"); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"refused by the seat", func(lc *ladderLaneCity, op opjournal.Op) {
			lc.closeOp(op.ID, failed(opjournal.ReasonProviderUnavailable))
		}, false},
		{"failed", func(lc *ladderLaneCity, op opjournal.Op) {
			lc.startOp(op.ID)
			lc.closeOp(op.ID, failed(opjournal.ReasonPoison))
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lc := newLadderLaneCity(t, "message")
			lc.startedClaim()
			turns := &fakeLadderTurns{}
			turns.install(lc.ladderCity)
			lc.at(0)
			lc.at(halfPast(1))
			tc.settle(lc, lc.onlyOp())
			refused := func(id string, at time.Duration) runtime.EndedTurn {
				return runtime.EndedTurn{TurnID: id, EndedAt: lc.start.Add(at), Status: runtime.TurnStatusFailed, Failure: coolingRefusal}
			}
			turns.hist = runtime.TurnHistory{Recent: []runtime.EndedTurn{refused("t2", halfPast(1)+40*time.Second), refused("t1", halfPast(1)+20*time.Second)}}
			lc.at(halfPast(1) + time.Minute)
			r := lc.cell()
			if got := r.RateLimit.Phase != workladder.RateLimitClear; got != tc.opens {
				t.Fatalf("episode %+v (delivered %v), want opened %v", r.RateLimit, r.Delivered, tc.opens)
			}
			if got := !r.Delivered.IsZero(); got != tc.opens {
				t.Fatalf("delivered %v, want moved %v", r.Delivered, tc.opens)
			}
		})
	}
}

// A notice reaches the seat when the attempt that delivered it starts (its
// executing transition), not when the ladder reserved it: refusals that
// ended before that start answer an earlier nudge, never this one.
func TestWorkLadderDeliveredIsTheAttemptStart(t *testing.T) {
	for _, tc := range []struct {
		name    string
		started time.Duration // the delivering attempt's start, after the reservation at 1m30s
		opens   bool
	}{
		{"the attempt started before the refusals", 10 * time.Second, true},
		{"the attempt started after the refusals", 30 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lc := newLadderLaneCity(t, "message")
			lc.startedClaim()
			turns := &fakeLadderTurns{}
			turns.install(lc.ladderCity)
			lc.at(0)
			lc.at(halfPast(1))
			op := lc.onlyOp()
			lc.f.clk.Advance(halfPast(1) + tc.started)
			lc.startOp(op.ID)
			lc.f.clk.Advance(20 * time.Second) // the attempt runs past the refusals before it closes
			lc.closeOp(op.ID, succeeded)
			refused := func(id string, at time.Duration) runtime.EndedTurn {
				return runtime.EndedTurn{TurnID: id, EndedAt: lc.start.Add(halfPast(1) + at), Status: runtime.TurnStatusFailed, Failure: coolingRefusal}
			}
			turns.hist = runtime.TurnHistory{Recent: []runtime.EndedTurn{refused("t2", 25*time.Second), refused("t1", 20*time.Second)}}
			lc.at(halfPast(1) + time.Minute)
			r := lc.cell()
			if !r.Delivered.Equal(lc.start.Add(halfPast(1) + tc.started)) {
				t.Fatalf("delivered %v, want the attempt's start", r.Delivered)
			}
			if got := r.RateLimit.Phase != workladder.RateLimitClear; got != tc.opens {
				t.Fatalf("episode %+v, want opened %v", r.RateLimit, tc.opens)
			}
		})
	}
}

// ProviderFreshGap: three seats on two providers, every one refused. Each
// is restarted fresh once; the two on one provider at least 30 minutes
// apart (read off each other's cells, so a controller restart between them
// does not lose the gap), the one on the other provider unhindered by them.
func TestWorkLadderFreshRestartsAreSpacedPerProvider(t *testing.T) {
	w := newRLWorld(t, false, "glm", "glm", "opus")
	for _, s := range w.seats {
		w.claimFor(s, "step of "+s.name)
	}
	restarted := false
	w.runUntil(4*time.Hour, func() bool {
		if !restarted && len(w.seats[0].restarts)+len(w.seats[1].restarts) > 0 {
			restarted = true
			w.restartController()
		}
		return false
	})
	a, b, other := w.seats[0], w.seats[1], w.seats[2]
	if len(a.restarts) != 1 || len(b.restarts) != 1 || len(other.restarts) != 1 {
		t.Fatalf("restarts %v %v %v, want one per seat", a.restarts, b.restarts, other.restarts)
	}
	first, second := a.restarts[0], b.restarts[0]
	if second.Before(first) {
		first, second = second, first
	}
	if second.Sub(first) < workladder.DefaultProviderFreshGap {
		t.Fatalf("two fresh restarts on one provider %v apart", second.Sub(first))
	}
	if d := other.restarts[0].Sub(first); d < -workladder.DefaultProviderFreshGap/2 || d > workladder.DefaultProviderFreshGap/2 {
		t.Fatalf("the other provider's seat restarted %v from the first: held by another provider's gap", d)
	}
	for _, s := range w.seats {
		if reasons, _ := w.alertsOf(s); slices.Contains(reasons, workladder.AlertRateLimitedHeld) {
			t.Fatalf("%s alerted held: the gap is no block past its bound", s.name)
		}
	}
}

// A person attached to the seat, or work of it awaiting a person, holds its
// fresh restart: one held alert, then the restart once the hold ends, with
// no second alert.
func TestWorkLadderAttachedSeatIsNotRestarted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause string
		hold  func(w *rlWorld, s *rlSeat) func()
	}{
		{"attached", string(workladder.RateLimitCauseAttached), func(w *rlWorld, s *rlSeat) func() {
			w.sp.SetAttached(s.name, true)
			return func() { w.sp.SetAttached(s.name, false) }
		}},
		{"awaiting a person", string(workladder.RateLimitCauseAwaitingHuman), func(w *rlWorld, s *rlSeat) func() {
			id := w.claimFor(s, "asked a person")
			if err := w.work.Update(id, beads.UpdateOpts{Metadata: map[string]string{beadmeta.AwaitingHumanMetadataKey: s.id}}); err != nil {
				t.Fatal(err)
			}
			return func() {
				if err := w.work.Update(id, beads.UpdateOpts{Metadata: map[string]string{beadmeta.AwaitingHumanMetadataKey: ""}}); err != nil {
					t.Fatal(err)
				}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newRLWorld(t, false, "claude")
			s := w.seats[0]
			w.claimFor(s, "the step")
			release := tc.hold(w, s)
			w.runUntil(time.Hour, nil)
			reasons, messages := w.alertsOf(s)
			if len(s.restarts) != 0 || w.rowOf(s).Metadata["restart_requested"] != "" {
				t.Fatalf("restarted a held seat: %v", s.restarts)
			}
			if !slices.Equal(reasons, []string{workladder.AlertRateLimitedHeld}) || !strings.Contains(messages[0], "pool cooling") {
				t.Fatalf("alerts %v %q, want one held alert", reasons, messages)
			}
			if got := w.transitions(s); !slices.Equal(got, []string{"opened", "held(" + tc.cause + ")"}) {
				t.Fatalf("moves %v", got)
			}
			release()
			if !w.runUntil(time.Hour, func() bool { return len(s.restarts) > 0 }) {
				t.Fatalf("not restarted after the hold ended: %v", w.transitions(s))
			}
			if reasons, _ := w.alertsOf(s); len(reasons) != 1 {
				t.Fatalf("alerts %v: one per episode", reasons)
			}
		})
	}
}

// Before a fresh restart every claim of the seat is re-read live: one whose
// read cannot tell blocks the restart, alerting held (blocked) once past
// FreshConfirm + Cap; once it reads again the seat is restarted.
func TestWorkLadderRestartRereadsEveryClaim(t *testing.T) {
	w := newRLWorld(t, false, "claude")
	s := w.seats[0]
	w.claimFor(s, "readable")
	unread := w.claimFor(s, "on a store that stops answering")
	fail := false
	s.stores = map[string]beads.Store{unread: getFailingStore{Store: w.work, fail: &fail}}
	if !w.runUntil(time.Hour, func() bool { return w.phaseOf(s) == workladder.RateLimitOpen }) {
		t.Fatal("no episode")
	}
	fail = true
	since := w.cellOf(s).RateLimit.Since
	w.runUntil(since.Add(workladder.DefaultPausedCap+workladder.DefaultCap+15*time.Minute).Sub(w.now), nil)
	reasons, _ := w.alertsOf(s)
	if len(s.restarts) != 0 || !slices.Equal(reasons, []string{workladder.AlertRateLimitedHeld}) {
		t.Fatalf("restarts %v, alerts %v: want the unread claim to block the restart, alerting once", s.restarts, reasons)
	}
	if got := w.transitions(s); !slices.Equal(got, []string{"opened", "held(blocked)"}) {
		t.Fatalf("moves %v", got)
	}
	fail = false
	if !w.runUntil(time.Hour, func() bool { return len(s.restarts) > 0 }) {
		t.Fatal("not restarted once the claim read again")
	}
}

// A pack's hold (the provider unhealthy in provider-health.json) freezes the
// episode: no restart, no transition but closing, which service still does.
// Once the hold lifts, the provider's confirmed seats restart one per
// ProviderFreshGap.
func TestWorkLadderDegradedProviderHoldsTheEpisode(t *testing.T) {
	t.Run("served while held", func(t *testing.T) {
		w := newRLWorld(t, false, "claude")
		s := w.seats[0]
		w.claimFor(s, "the step")
		if !w.runUntil(time.Hour, func() bool { return w.phaseOf(s) == workladder.RateLimitOpen }) {
			t.Fatal("no episode")
		}
		w.unhealthy("claude")
		w.runUntil(2*time.Hour, func() bool { w.unhealthy("claude"); return false })
		if len(s.restarts) != 0 || !slices.Equal(w.transitions(s), []string{"opened"}) {
			t.Fatalf("held provider: restarts %v, moves %v", s.restarts, w.transitions(s))
		}
		s.refuse = func(int64) bool { return false }
		if !w.runUntil(time.Hour, func() bool { w.unhealthy("claude"); return w.phaseOf(s) == workladder.RateLimitClear }) {
			t.Fatal("service under a hold did not close the episode")
		}
		if got := w.transitions(s); !slices.Equal(got, []string{"opened", "closed(served)"}) {
			t.Fatalf("moves %v", got)
		}
	})
	t.Run("restarts serialized after the hold", func(t *testing.T) {
		w := newRLWorld(t, false, "glm", "glm")
		for _, s := range w.seats {
			w.claimFor(s, "step of "+s.name)
		}
		if !w.runUntil(time.Hour, func() bool {
			return w.phaseOf(w.seats[0]) == workladder.RateLimitOpen && w.phaseOf(w.seats[1]) == workladder.RateLimitOpen
		}) {
			t.Fatal("no episodes")
		}
		w.unhealthy("glm")
		w.runUntil(30*time.Minute, func() bool { w.unhealthy("glm"); return false })
		w.unhealthy()
		w.runUntil(3*time.Hour, nil)
		a, b := w.seats[0].restarts, w.seats[1].restarts
		if len(a) != 1 || len(b) != 1 {
			t.Fatalf("restarts %v %v", a, b)
		}
		if d := b[0].Sub(a[0]); d < workladder.DefaultProviderFreshGap && -d < workladder.DefaultProviderFreshGap {
			t.Fatalf("restarts %v apart after the hold lifted", d)
		}
	})
}

// A restart that did not take (the seat came back in the conversation it
// left) and is refused again exhausts the episode with its alert.
func TestWorkLadderRestartThatDidNotTakeAlerts(t *testing.T) {
	w := newRLWorld(t, false, "claude")
	s := w.seats[0]
	s.loseRestart = true
	w.claimFor(s, "the step")
	if !w.runUntil(4*time.Hour, func() bool { return w.phaseOf(s) == workladder.RateLimitExhausted }) {
		t.Fatalf("not exhausted: %v", w.transitions(s))
	}
	reasons, _ := w.alertsOf(s)
	if got := w.transitions(s); !slices.Equal(got, []string{"opened", "restarted", "exhausted(restart_lost)"}) ||
		!slices.Equal(reasons, []string{workladder.AlertRateLimitedFreshRefused}) {
		t.Fatalf("moves %v, alerts %v", got, reasons)
	}
}

// An episode whose acting passes read no answered refusal for
// RateLimitEvidenceMaxAge (the seat's turns cannot be read) closes stale,
// and the normal ladder resumes.
func TestWorkLadderStaleEpisodeCloses(t *testing.T) {
	w := newRLWorld(t, false, "claude")
	s := w.seats[0]
	w.claimFor(s, "the step")
	if !w.runUntil(time.Hour, func() bool { return w.phaseOf(s) == workladder.RateLimitOpen }) {
		t.Fatal("no episode")
	}
	checked := w.cellOf(s).RateLimit.Checked
	s.readErr = fmt.Errorf("transcript not found")
	if !w.runUntil(2*time.Hour, func() bool { return w.phaseOf(s) == workladder.RateLimitClear }) {
		t.Fatal("never closed")
	}
	// Within EvidenceMaxAge of acting passes: the seat's paced nudges keep it
	// busy for a few minutes of each ten.
	if d := w.now.Sub(checked); d < workladder.DefaultCap || d > workladder.DefaultCap+10*time.Minute {
		t.Fatalf("closed %v after its last answered refusal", d)
	}
	if got := w.transitions(s); !slices.Equal(got, []string{"opened", "closed(stale)"}) {
		t.Fatalf("moves %v", got)
	}
}

// A seat refused again within SeatFreshGap of its last fresh restart is not
// restarted again: the new episode is exhausted with rate_limited_again.
func TestWorkLadderSeatGapAlertsAgain(t *testing.T) {
	w := newRLWorld(t, false, "claude")
	s := w.seats[0]
	s.refuse = func(conv int64) bool { return conv == 1 }
	w.claimFor(s, "the step")
	if !w.runUntil(2*time.Hour, func() bool { return len(s.restarts) == 1 && w.phaseOf(s) == workladder.RateLimitClear }) {
		t.Fatalf("the fresh conversation was not served: %v", w.transitions(s))
	}
	s.refuse = alwaysRefuse
	if !w.runUntil(time.Hour, func() bool { return w.phaseOf(s) == workladder.RateLimitExhausted }) {
		t.Fatalf("not exhausted: %v", w.transitions(s))
	}
	reasons, _ := w.alertsOf(s)
	if got := w.transitions(s); !slices.Equal(got, []string{"opened", "restarted", "closed(served)", "opened", "exhausted(again)"}) ||
		!slices.Equal(reasons, []string{workladder.AlertRateLimitedAgain}) || len(s.restarts) != 1 {
		t.Fatalf("moves %v, alerts %v, restarts %v", got, reasons, s.restarts)
	}
}

// A refusal its source classed a spent usage quota (Codex's
// usage_limit_exceeded, TurnFailure.Quota) is never restarted fresh: a new
// conversation draws on the same allowance. The episode is exhausted with
// one rate_limited_quota alert.
func TestWorkLadderQuotaIsNeverRestarted(t *testing.T) {
	w := newRLWorld(t, false, "codex")
	s := w.seats[0]
	s.failure = runtime.TurnFailure{Code: "usage_limit_exceeded", RateLimited: true, Quota: true, Message: "You've hit your usage limit."}
	w.claimFor(s, "the step")
	w.runUntil(3*time.Hour, nil)
	reasons, messages := w.alertsOf(s)
	if got := w.transitions(s); !slices.Equal(got, []string{"opened", "exhausted(quota)"}) || len(s.restarts) != 0 ||
		!slices.Equal(reasons, []string{workladder.AlertRateLimitedQuota}) || !strings.Contains(messages[0], "usage limit") {
		t.Fatalf("moves %v, restarts %v, alerts %v %q", got, s.restarts, reasons, messages)
	}
}

// work.rate_limited is typed: the provider's text is bounded and its control
// characters are spaces, and the move names the seat's started claims.
func TestWorkLadderRateLimitedPayload(t *testing.T) {
	w := newRLWorld(t, false, "claude")
	s := w.seats[0]
	s.failure.Message = "API Error:\x1b[31m 429\nrate limited\t" + strings.Repeat("é", 400)
	id := w.claimFor(s, "the step")
	if !w.runUntil(time.Hour, func() bool { return w.phaseOf(s) == workladder.RateLimitOpen }) {
		t.Fatal("no episode")
	}
	m := w.moves(s)[0]
	if m.Transition != events.WorkRateLimitedOpened || m.Refused < 2 || m.RefusedAt == nil || m.FreshAt != nil || m.Since.IsZero() ||
		!slices.Equal(m.Claims, []string{id}) || m.SessionName != ladderSeatName || m.Template != "worker" || m.Conversation != session.DefaultContinuationEpoch {
		t.Fatalf("opened payload %+v", m)
	}
	if len(m.ErrorText) > maxLadderErrorText || strings.ContainsFunc(m.ErrorText, unicode.IsControl) || !strings.HasPrefix(m.ErrorText, "API Error: [31m 429 rate limited ") {
		t.Fatalf("error text %q", m.ErrorText)
	}
}

// Cells written by the per-claim-flag builds (8d6r5, 8df992dfb3) decode as
// they stand: the ladder keeps their pacing, opens an episode on a refusal
// that answers its next nudge, and its first write drops the flags.
func TestWorkLadderRewritesAPerClaimFlagCell(t *testing.T) {
	w := newRLWorld(t, false, "claude")
	s := w.seats[0]
	id := w.claimFor(s, "the step")
	last := w.now.Add(-2 * time.Minute)
	old := fmt.Sprintf(`{"v":1,"gen":7,"last_nudge":%q,"entries":[{"store":"city","bead":%q,"kind":"claim","rung":"started","since":%q,"attempts":1,"paused":0,`+
		`"last":%q,"next":%q,"recycles":1,"escalated":false,"seed":"claim_session","recycle_at":%q,"rate_limit_alerted":true,"fresh_recycled":true,"fresh_served":false,"fresh_queued":true}]}`,
		last.Format(time.RFC3339Nano), id, last.Add(-time.Hour).Format(time.RFC3339Nano), last.Format(time.RFC3339Nano), last.Add(3*time.Minute).Format(time.RFC3339Nano), last.Add(-30*time.Minute).Format(time.RFC3339Nano))
	if _, err := workladder.DecodeLadder(old); err != nil {
		t.Fatalf("the old cell does not decode: %v", err)
	}
	w.setRow(map[string]string{session.WorkLadderKey: old})
	var stderr strings.Builder
	w.lane.stderr = &stderr
	if !w.runUntil(time.Hour, func() bool { return w.phaseOf(s) == workladder.RateLimitOpen }) {
		t.Fatalf("no episode on the old cell: %s", w.rowOf(s).Metadata[session.WorkLadderKey])
	}
	text := w.rowOf(s).Metadata[session.WorkLadderKey]
	for _, flag := range []string{"rate_limit_alerted", "fresh_recycled", "fresh_served", "fresh_queued"} {
		if strings.Contains(text, flag) {
			t.Fatalf("the rewrite keeps %s: %s", flag, text)
		}
	}
	if e := w.cellOf(s).Entries[0]; e.Recycles != 1 || e.Kind != workladder.KindClaim || e.Key.Bead != id {
		t.Fatalf("entry %+v, want the old pacing carried", e)
	}
	if strings.Contains(stderr.String(), "afresh") {
		t.Fatalf("the old cell was re-seeded: %s", stderr.String())
	}
}

// The events package's vocabulary for a move is the ladder's, word for word.
func TestWorkLadderRateLimitVocabularyMatchesTheEvent(t *testing.T) {
	for core, wire := range map[string]string{
		string(workladder.RateLimitTransitionOpened):         events.WorkRateLimitedOpened,
		string(workladder.RateLimitTransitionRestarted):      events.WorkRateLimitedRestarted,
		string(workladder.RateLimitTransitionHeld):           events.WorkRateLimitedHeld,
		string(workladder.RateLimitTransitionExhausted):      events.WorkRateLimitedExhausted,
		string(workladder.RateLimitTransitionClosed):         events.WorkRateLimitedClosed,
		string(workladder.RateLimitCauseAttached):            events.WorkRateLimitedCauseAttached,
		string(workladder.RateLimitCauseAwaitingHuman):       events.WorkRateLimitedCauseAwaitingHuman,
		string(workladder.RateLimitCauseBlocked):             events.WorkRateLimitedCauseBlocked,
		string(workladder.RateLimitCauseFreshRefused):        events.WorkRateLimitedCauseFreshRefused,
		string(workladder.RateLimitCauseRestartLost):         events.WorkRateLimitedCauseRestartLost,
		string(workladder.RateLimitCauseAgain):               events.WorkRateLimitedCauseAgain,
		string(workladder.RateLimitCauseQuota):               events.WorkRateLimitedCauseQuota,
		string(workladder.RateLimitCauseServed):              events.WorkRateLimitedCauseServed,
		string(workladder.RateLimitCauseStale):               events.WorkRateLimitedCauseStale,
		string(workladder.RateLimitCauseConversationChanged): events.WorkRateLimitedCauseConversationChanged,
	} {
		if core != wire {
			t.Errorf("workladder %q is %q on the wire", core, wire)
		}
	}
}

// slowNudgeProvider is a runtime whose Nudge returns only once took has run:
// a tmux nudge returns after its submit is confirmed, seconds after the
// prompt landed.
type slowNudgeProvider struct {
	runtime.Provider
	took func()
}

func (s slowNudgeProvider) Nudge(name string, content []runtime.ContentBlock) error {
	err := s.Provider.Nudge(name, content)
	s.took()
	return err
}

// A provider refuses within a second of the prompt, before a direct
// delivery returns (2.2s here): the refusal answers the nudge, so its
// episode opens. Delivered is the attempt's start, read before the call,
// not the lane's clock once the call returned.
func TestWorkLadderFastRefusalAnswersASlowDelivery(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("the step")
	c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id})
	turns := &fakeLadderTurns{}
	turns.install(c)
	var starts []time.Time
	c.env.SP = slowNudgeProvider{Provider: c.sp, took: func() {
		starts = append(starts, c.now)
		c.now = c.now.Add(2200 * time.Millisecond)
	}}
	const refusedAfter = time.Second
	for end := c.now.Add(2 * time.Hour); c.now.Before(end) && c.cell().RateLimit.Phase == workladder.RateLimitClear; c.now = c.now.Add(30 * time.Second) {
		turns.hist = runtime.TurnHistory{}
		for i, s := range starts {
			if at := s.Add(refusedAfter); !at.After(c.now) {
				turns.hist.Recent = append([]runtime.EndedTurn{{TurnID: fmt.Sprint(i), StartedAt: s, EndedAt: at, Status: runtime.TurnStatusFailed, Failure: coolingRefusal}}, turns.hist.Recent...)
				c.sp.SetActivity(ladderSeatName, at)
			}
		}
		c.pass()
	}
	r := c.cell()
	if r.RateLimit.Phase != workladder.RateLimitOpen || len(starts) < 2 {
		t.Fatalf("nudged at %v, cell %s: want the fast refusals to open an episode", starts, c.row().Metadata[session.WorkLadderKey])
	}
	if want := starts[len(starts)-1].Add(refusedAfter); !r.RateLimit.Since.Equal(want) || !r.Delivered.Equal(starts[len(starts)-1]) {
		t.Fatalf("episode since %v, delivered %v: want the refusal at %v answering the delivery started at %v", r.RateLimit.Since, r.Delivered, want, starts[len(starts)-1])
	}
}

// refusedSeat is a seat holding one started claim, its last nudge delivered
// five minutes ago and answered by two refusals a minute ago, its episode
// rl (in the seat's conversation).
func refusedSeat(t *testing.T, rl workladder.RateLimit) *ladderCity {
	t.Helper()
	c := newLadderCity(t)
	id := c.claim("the step")
	key := workladder.ClaimKey{Store: "city", Bead: id}
	delivered := c.now.Add(-5 * time.Minute)
	rl.Conv = rowConversation(c.row().Metadata)
	cell := workladder.Record{V: 1, Gen: 7, Delivered: delivered, LastNudge: delivered, RateLimit: rl, Entries: []workladder.Entry{{
		Key: key, Kind: workladder.KindClaim, Rung: workladder.RungStarted, Since: c.now.Add(-2 * time.Hour), Attempts: 2, Last: delivered,
	}}}
	c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id, session.WorkLadderKey: workladder.EncodeLadder(cell)})
	turns := &fakeLadderTurns{}
	turns.install(c)
	refused := func(id string, ago time.Duration) runtime.EndedTurn {
		return runtime.EndedTurn{TurnID: id, EndedAt: c.now.Add(-ago), Status: runtime.TurnStatusFailed, Failure: coolingRefusal}
	}
	turns.hist = runtime.TurnHistory{Recent: []runtime.EndedTurn{refused("t2", time.Minute), refused("t1", 2*time.Minute)}}
	return c
}

// raceCell rewrites c's cell, between the pass's read and its CAS, with
// change applied.
func raceCell(t *testing.T, c *ladderCity, change func(*workladder.Record)) *agentRaceStore {
	t.Helper()
	race := &agentRaceStore{MemStore: c.sessions, row: c.seat, agent: func() {
		r := c.cell()
		change(&r)
		casSetRow(t, c.sessions, c.seat, map[string]string{session.WorkLadderKey: workladder.EncodeLadder(r)})
	}}
	c.env.CityStore = blindWriteRefusingStore{inner: race}
	return race
}

// L9. An episode confirmed only on the revision the CAS reads (another
// write moved its start back past FreshConfirm) was not confirmed when the
// pass read the seat, so the pass read nothing that holds a restart: here a
// person is attached. The CAS restarts nothing; the next pass reads the
// attachment and holds the restart, alerting held (attached).
func TestWorkLadderRestartConfirmedOnlyAtTheCASWaits(t *testing.T) {
	c := refusedSeat(t, workladder.RateLimit{Phase: workladder.RateLimitOpen, Since: stepsTestNow.Add(-5 * time.Minute), Checked: stepsTestNow.Add(-5 * time.Minute)})
	c.sp.SetAttached(ladderSeatName, true)
	race := raceCell(t, c, func(r *workladder.Record) {
		r.RateLimit.Since = c.now.Add(-workladder.DefaultPausedCap - time.Minute)
	})
	c.pass()
	if !race.fired.Load() {
		t.Fatal("the ladder never tried its CAS")
	}
	if got := c.row().Metadata["restart_requested"]; got != "" {
		t.Fatalf("restart_requested %q: a restart confirmed only at the CAS skipped its holds", got)
	}
	c.now = c.now.Add(30 * time.Second)
	c.pass()
	if got := c.row().Metadata["restart_requested"]; got != "" || c.cell().RateLimit.Alerted != workladder.AlertRateLimitedHeld {
		t.Fatalf("restart_requested %q, cell %s: want the attached seat held", got, c.row().Metadata[session.WorkLadderKey])
	}
}

// LOW-3. The pass read a confirmed episode and decided no restart (the
// seat was restarted fresh within SeatFreshGap: exhausted); another write
// lifted that block before the CAS, whose decision would restart the seat.
// The pass re-read none of the seat's claims for a restart, so the CAS
// holds its work, and the restart with it, for the pass; the next pass,
// reading them, restarts it.
func TestWorkLadderRestartTheCASAloneDecidedWaits(t *testing.T) {
	since := stepsTestNow.Add(-workladder.DefaultPausedCap - time.Minute)
	// LastFresh within SeatFreshGap: the pass's decision exhausts the episode.
	c := refusedSeat(t, workladder.RateLimit{Phase: workladder.RateLimitOpen, Since: since, Checked: since, LastFresh: stepsTestNow.Add(-30 * time.Minute)})
	race := raceCell(t, c, func(r *workladder.Record) { r.RateLimit.LastFresh = time.Time{} })
	c.pass()
	if !race.fired.Load() {
		t.Fatal("the ladder never tried its CAS")
	}
	cell := c.cell()
	if got := c.row().Metadata["restart_requested"]; got != "" || cell.RateLimit.Phase != workladder.RateLimitOpen {
		t.Fatalf("restart_requested %q, cell %s: a restart only the CAS decided landed", got, c.row().Metadata[session.WorkLadderKey])
	}
	c.now = c.now.Add(30 * time.Second)
	c.pass()
	if got := c.row().Metadata["restart_requested"]; got != "true" || c.cell().RateLimit.Phase != workladder.RateLimitRestarted {
		t.Fatalf("restart_requested %q, cell %s: want the next pass to restart the seat", got, c.row().Metadata[session.WorkLadderKey])
	}
}

// L12. A closed move reports the episode it closed: its start and its fresh
// restart, gone from the landed cell.
func TestWorkLadderClosedPayloadNamesTheClosedEpisode(t *testing.T) {
	w := newRLWorld(t, false, "claude")
	s := w.seats[0]
	first := rowConversation(w.rowOf(s).Metadata)
	s.refuse = func(conv int64) bool { return conv == first } // the fresh conversation is served
	s.restartAfter = 1
	w.claimFor(s, "the step")
	if !w.runUntil(4*time.Hour, func() bool { return len(w.moves(s)) >= 3 }) {
		t.Fatalf("moves %v", w.transitions(s))
	}
	if got := w.transitions(s); !slices.Equal(got, []string{"opened", "restarted", "closed(served)"}) {
		t.Fatalf("moves %v", got)
	}
	m := w.moves(s)
	opened, restarted, closed := m[0], m[1], m[2]
	if closed.Since.IsZero() || !closed.Since.Equal(opened.Since) {
		t.Fatalf("closed since %v, want the closed episode's (%v)", closed.Since, opened.Since)
	}
	if closed.FreshAt == nil || restarted.FreshAt == nil || !closed.FreshAt.Equal(*restarted.FreshAt) {
		t.Fatalf("closed fresh_at %v, want the closed episode's restart (%v)", closed.FreshAt, restarted.FreshAt)
	}
}
