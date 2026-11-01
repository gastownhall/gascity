package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// claimStallFixture is testdata/claim_stall_oct31.txt: maintainer-city's pool
// seats on 2026-10-31 and the routed work they were spawned for, from the
// formula-overhead-audit-6 session model.
type claimStallFixture struct {
	start    time.Time
	seats    []claimStallFixtureSeat
	triggers map[int]claimStallFixtureTrigger
	moves    []claimStallFixtureMove
}

type claimStallFixtureSeat struct {
	template, provider         string
	created, awake, claim, end time.Time // zero: never
	failedCreate               bool
	trigger                    int // 0: no trigger
}

type claimStallFixtureTrigger struct {
	template                string
	ready, claimed, closedA time.Time
}

type claimStallFixtureMove struct {
	at                 time.Time
	template, provider string
}

func loadClaimStallFixture(t *testing.T) claimStallFixture {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "claim_stall_oct31.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // read-only
	fx := claimStallFixture{start: time.Date(2026, 10, 31, 4, 0, 0, 0, time.UTC), triggers: map[int]claimStallFixtureTrigger{}}
	templates, providers := map[string]string{}, map[string]string{}
	at := func(s string) time.Time {
		if s == "-" {
			return time.Time{}
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			t.Fatalf("time %q: %v", s, err)
		}
		return fx.start.Add(time.Duration(n) * time.Second)
	}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		c := strings.Split(line, ",")
		switch c[0] {
		case "N":
			templates[c[1]] = c[2]
		case "P":
			providers[c[1]] = c[2]
		case "S":
			trigger, _ := strconv.Atoi(c[8])
			fx.seats = append(fx.seats, claimStallFixtureSeat{
				template: templates[c[1]], provider: providers[c[2]],
				created: at(c[3]), awake: at(c[4]), claim: at(c[5]), end: at(c[6]),
				failedCreate: c[7] == "1", trigger: trigger,
			})
		case "T":
			id, _ := strconv.Atoi(c[1])
			fx.triggers[id] = claimStallFixtureTrigger{template: templates[c[2]], ready: at(c[3]), claimed: at(c[4]), closedA: at(c[5])}
		case "C":
			fx.moves = append(fx.moves, claimStallFixtureMove{at: at(c[1]), template: templates[c[2]], provider: c[3]})
		default:
			t.Fatalf("fixture line %q", line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return fx
}

// waiting reports whether trigger is routed demand at now: ready, not yet
// claimed or closed.
func (tr claimStallFixtureTrigger) waiting(now time.Time) bool {
	if tr.ready.IsZero() || now.Before(tr.ready) {
		return false
	}
	return (tr.claimed.IsZero() || now.Before(tr.claimed)) && (tr.closedA.IsZero() || now.Before(tr.closedA))
}

// claimStallReplay drives a guard through the fixture: each seat's
// notifications at their times, the operator's provider moves, and a demand
// pass every patrol interval over the work that waits.
type claimStallReplay struct {
	h        *claimStallHarness
	fx       claimStallFixture
	provider map[string]string // template → its provider now
}

func newClaimStallReplay(t *testing.T) *claimStallReplay {
	t.Helper()
	fx := loadClaimStallFixture(t)
	h := newClaimStallHarness(t)
	h.now = fx.start
	r := &claimStallReplay{h: h, fx: fx, provider: map[string]string{}}
	for _, s := range fx.seats {
		if _, ok := r.provider[s.template]; !ok {
			r.provider[s.template] = s.provider
		}
	}
	r.configure()
	return r
}

// configure is the city config as it stood: every template on its provider.
func (r *claimStallReplay) configure() {
	cfg := &config.City{}
	for name, provider := range r.provider {
		dir, short, _ := strings.Cut(name, "/")
		cfg.Agents = append(cfg.Agents, config.Agent{Name: short, Dir: dir, Provider: provider})
	}
	r.h.guard.configure(cfg)
}

type claimStallStep struct {
	at   time.Time
	rank int // at one instant: a move, then wakes, claims, ends
	do   func()
}

func (r *claimStallReplay) run(until time.Time, patrol time.Duration) {
	h := r.h
	var steps []claimStallStep
	for i := range r.fx.seats {
		s := r.fx.seats[i]
		trigger := ""
		if s.trigger != 0 {
			trigger = fmt.Sprintf("trigger-%d", s.trigger)
		}
		seat := stallSeat{id: fmt.Sprintf("gcg-session-oct31-%d", i), template: s.template, trigger: trigger, created: s.created}
		steps = append(steps, claimStallStep{s.created, 1, func() {
			h.seatEvent(events.BeadCreated, seat.id, seat.template, seat.created, map[string]string{"gc.trigger_bead_id": trigger, "state": "start-pending"})
		}})
		if !s.awake.IsZero() {
			steps = append(steps, claimStallStep{s.awake, 2, func() { h.wake(seat) }})
		}
		if !s.claim.IsZero() {
			steps = append(steps, claimStallStep{s.claim, 3, func() { h.claim(seat) }})
		}
		if !s.end.IsZero() {
			state := "idle"
			if s.failedCreate {
				state = "failed-create"
			}
			awake := s.awake
			steps = append(steps, claimStallStep{s.end, 4, func() { h.close(seat, state, awake) }})
		}
	}
	for _, m := range r.fx.moves {
		steps = append(steps, claimStallStep{m.at, 0, func() {
			r.provider[m.template] = m.provider
			r.configure()
		}})
	}
	sort.SliceStable(steps, func(i, j int) bool {
		if !steps[i].at.Equal(steps[j].at) {
			return steps[i].at.Before(steps[j].at)
		}
		return steps[i].rank < steps[j].rank
	})
	next := 0
	for now := r.fx.start; now.Before(until); now = now.Add(patrol) {
		for next < len(steps) && !steps[next].at.After(now) {
			h.now = steps[next].at
			steps[next].do()
			next++
		}
		h.now = now
		h.demand = map[string][]string{}
		ids := make([]int, 0, len(r.fx.triggers))
		for id := range r.fx.triggers {
			ids = append(ids, id)
		}
		sort.Ints(ids)
		for _, id := range ids {
			tr := r.fx.triggers[id]
			if tr.template != "" && tr.waiting(now) {
				h.demand[tr.template] = append(h.demand[tr.template], fmt.Sprintf("trigger-%d", id))
			}
		}
		h.pass("")
	}
}

// replayAlerts is each template's stalled and resumed records, in order.
func (r *claimStallReplay) alerts() map[string][]events.Event {
	out := map[string][]events.Event{}
	for _, e := range r.h.alerts {
		out[e.Subject] = append(out[e.Subject], e)
	}
	return out
}

func oct31(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, "2026-10-31T"+s+"Z")
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// TestClaimStallGuardReplaysTheOct31Outage replays 2026-10-31 (audit 6 §6.1):
// the Cursor pool ran out of usage and gascity's review-codex lane (cursor-
// grok47-high) and cp-grok lane (cursor-grok-xhigh), then beads' review-codex
// lane, stopped claiming until the operator moved them to codex-astra-high at
// 17:44:43Z. Nothing alerted that day. The guard alerts on gascity's lane
// within three minutes of its first failed create (05:33:51); its cursor seats
// then worked again from 06:11 to 06:58, which ends that episode. When the
// lane died for good it took three dead seats to be sure, and seats come only
// as work does: the first spawned 07:12:30, the third 07:21:54, and the guard
// alerts at 07:25, thirteen minutes after the first. beads' lane alerts within
// four minutes of its first failed create (11:04:35). No other template,
// including every healthy lane on the same city that day, ever stalls.
func TestClaimStallGuardReplaysTheOct31Outage(t *testing.T) {
	r := newClaimStallReplay(t)
	r.run(oct31(t, "19:59:59"), 15*time.Second)
	byTemplate := r.alerts()

	firstStall := func(template string, after time.Time) (time.Time, events.AgentClaimsPayload) {
		t.Helper()
		for _, e := range byTemplate[template] {
			if e.Type == events.AgentClaimsStalled && !e.Ts.Before(after) {
				return e.Ts, stallPayload(t, e)
			}
		}
		t.Fatalf("%s never stalled after %v; alerts %v", template, after.Format("15:04:05"), byTemplate[template])
		return time.Time{}, events.AgentClaimsPayload{}
	}
	within := func(template string, got, from time.Time, limit time.Duration) {
		t.Helper()
		if got.Before(from) || got.Sub(from) > limit {
			t.Errorf("%s stalled at %s, want within %v of %s", template, got.Format("15:04:05"), limit, from.Format("15:04:05"))
		}
	}

	// gascity review-codex, first onset: failed creates from 05:33:51.
	at, p := firstStall("gascity/adopt-pr-review-codex", oct31(t, "05:00:00"))
	within("gascity/adopt-pr-review-codex", at, oct31(t, "05:33:51"), 3*time.Minute)
	if p.Provider != "cursor-grok47-high" || p.Action != events.AgentClaimsHeld {
		t.Errorf("first stall %+v, want cursor-grok47-high held", p)
	}
	// Its cursor seats claimed again from 06:11: the episode ends.
	resumed := false
	for _, e := range byTemplate["gascity/adopt-pr-review-codex"] {
		if e.Type == events.AgentClaimsResumed && e.Ts.After(at) && e.Ts.Before(oct31(t, "07:00:00")) {
			resumed = true
		}
	}
	if !resumed {
		t.Errorf("gascity review-codex never resumed in the 06:11-06:58 window: %v", byTemplate["gascity/adopt-pr-review-codex"])
	}
	// Second onset: the first idle dead seat after the 07:04 restart.
	at, _ = firstStall("gascity/adopt-pr-review-codex", oct31(t, "07:00:00"))
	within("gascity/adopt-pr-review-codex", at, oct31(t, "07:12:30"), 14*time.Minute)

	// cp-grok's first idle dead seat of the day and beads' first failed
	// create.
	at, p = firstStall("gascity/cp-grok", oct31(t, "05:00:00"))
	if p.Provider != "cursor-grok-xhigh" || at.After(oct31(t, "07:45:00")) {
		t.Errorf("cp-grok stalled at %s on %s, want cursor-grok-xhigh by 07:45", at.Format("15:04:05"), p.Provider)
	}
	at, _ = firstStall("beads/adopt-pr-review-codex", oct31(t, "05:00:00"))
	within("beads/adopt-pr-review-codex", at, oct31(t, "11:04:35"), 5*time.Minute)

	// While a lane stays held its alerts are the probes', a few an hour at
	// most, never a storm.
	for template, alerts := range byTemplate {
		stalls := 0
		for _, e := range alerts {
			if e.Type == events.AgentClaimsStalled {
				stalls++
			}
		}
		if stalls > 30 {
			t.Errorf("%s raised %d stall alerts in 16 hours", template, stalls)
		}
	}

	// The negative control: every other template on the city that day.
	dead := map[string]bool{"gascity/adopt-pr-review-codex": true, "beads/adopt-pr-review-codex": true, "gascity/cp-grok": true}
	for template, alerts := range byTemplate {
		if !dead[template] && len(alerts) > 0 {
			t.Errorf("healthy %s stalled: %v", template, alerts)
		}
	}
}
