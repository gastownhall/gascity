package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/session"
)

const (
	standbyProvider = "codex-astra"
	standbyCityPath = "/city"
)

// standbyCity is the harness city: review-codex on cursor-grok with the
// codex-astra standby, and review-claude on opus.
const standbyCity = `
[workspace]
name = "test"

[providers.cursor-grok]
command = "cursor-agent"

[providers.codex-astra]
command = "codex"

[providers.opus]
command = "claude"

[[agent]]
name = "review-codex"
dir = "gascity"
provider = "cursor-grok"
standby_provider = "codex-astra"

[[agent]]
name = "review-claude"
dir = "gascity"
provider = "opus"
`

// standbyHarness is a claimStallHarness whose template declares a standby,
// with its city.toml and standby record on a fake filesystem, and a
// controller that reloads the config, composed as every gc process composes
// it (config.LoadWithIncludes), whenever the guard rewrites the record.
type standbyHarness struct {
	*claimStallHarness
	fs      *fsys.Fake
	allowed bool
}

func newStandbyHarness(t *testing.T) *standbyHarness {
	t.Helper()
	return newStandbyHarnessWith(t, standbyCity)
}

func newStandbyHarnessWith(t *testing.T, city string) *standbyHarness {
	t.Helper()
	h := &standbyHarness{claimStallHarness: newClaimStallHarness(t), fs: fsys.NewFake(), allowed: true}
	h.fs.Files[standbyCityPath+"/city.toml"] = []byte(city)
	h.guard.withStandbyRecord(h.fs, standbyCityPath)
	h.guard.restored = false
	h.reconfigure()
	return h
}

// composed is the config every gc process composes from the city and the
// standby record.
func (h *standbyHarness) composed() *config.City {
	h.t.Helper()
	cfg, _, err := config.LoadWithIncludes(h.fs, standbyCityPath+"/city.toml")
	if err != nil {
		h.t.Fatal(err)
	}
	return cfg
}

func (h *standbyHarness) reconfigure() { h.guard.configure(h.composed(), h.allowed) }

// tick is the controller's tick: the guard syncs its standby record, and a
// changed record reloads the config.
func (h *standbyHarness) tick(health *providerHealthSnapshot) bool {
	h.t.Helper()
	changed, err := h.guard.syncStandby(health)
	if err != nil {
		h.t.Fatal(err)
	}
	if changed {
		h.reconfigure()
	}
	return changed
}

// cityWorks is a seat of another template, on another provider, claiming:
// the evidence that the city itself works.
func (h *standbyHarness) cityWorks() {
	h.t.Helper()
	h.claim(h.spawn(otherTemplate, "other-step"))
}

func (h *standbyHarness) record() config.StandbyFile {
	h.t.Helper()
	f, _, err := config.LoadStandby(h.fs, standbyCityPath)
	if err != nil {
		h.t.Fatal(err)
	}
	return f
}

// tripOnOwn trips the template on its own provider with evidence: three seats
// on three beads of work fail while another provider's seat claims.
func (h *standbyHarness) tripOnOwn() {
	h.t.Helper()
	h.demand[stallTemplate] = []string{"step-1", "step-2", "step-3", "step-4", "step-5"}
	h.overdueSeat("step-1")
	h.cityWorks()
	h.overdueSeat("step-2")
	h.overdueSeat("step-3")
}

func (h *standbyHarness) lastStall() events.AgentClaimsPayload {
	h.t.Helper()
	stalls := h.stalls()
	if len(stalls) == 0 {
		h.t.Fatal("no stall")
	}
	return stallPayload(h.t, stalls[len(stalls)-1])
}

// TestClaimStallGuardMovesAStalledAgentToItsStandby: a stall on the agent's
// own provider, with the provider's evidence, moves it to its standby instead
// of holding it: the alert says so, the record names the move, and once the
// config reloads the template's demand counts in full, for seats on the
// standby.
func TestClaimStallGuardMovesAStalledAgentToItsStandby(t *testing.T) {
	h := newStandbyHarness(t)
	h.tripOnOwn()
	p := h.lastStall()
	if p.Action != events.AgentClaimsStandby || p.Standby != standbyProvider || p.Provider != stallProvider || p.Engagements != 1 || p.Until == nil || p.Until.Sub(h.now) != claimStallStandbyHold {
		t.Fatalf("stall %+v, want a move to %s for %v", p, standbyProvider, claimStallStandbyHold)
	}
	if !h.tick(nil) {
		t.Fatal("the move was not written")
	}
	rec := h.record()
	if len(rec.Agents) != 1 || rec.Agents[0].Name != stallTemplate || rec.Agents[0].Provider != standbyProvider || rec.Agents[0].Replaced != stallProvider || rec.Agents[0].Episode != p.Episode {
		t.Fatalf("record %+v, want review-codex on %s in place of %s for episode %s", rec, standbyProvider, stallProvider, p.Episode)
	}
	engaged := h.ofType(events.AgentStandbyEngaged)
	if len(engaged) != 1 || stallPayload(t, engaged[0]).Standby != standbyProvider {
		t.Fatalf("engaged %v, want one record of the move", engaged)
	}
	if got := h.pass(stallTemplate); len(got) != 5 {
		t.Fatalf("demand %v on the standby, want all of it counted", got)
	}
	if h.tick(nil) {
		t.Fatal("an unchanged move was rewritten")
	}
}

// Without the provider's evidence a stall is held, not moved: one bead of
// work failing for every seat may be that bead, and no claim on any other
// provider may be the city itself failing.
func TestClaimStallGuardHoldsWithoutTheProvidersEvidence(t *testing.T) {
	t.Run("one bead", func(t *testing.T) {
		h := newStandbyHarness(t)
		h.demand[stallTemplate] = []string{"step-1"}
		h.cityWorks()
		for range claimStallTrip {
			h.overdueSeat("step-1")
		}
		if p := h.lastStall(); p.Action != events.AgentClaimsHeld {
			t.Fatalf("stall %+v, want held", p)
		}
		if h.tick(nil) {
			t.Fatal("a held stall was recorded as a move")
		}
	})
	t.Run("no claim elsewhere", func(t *testing.T) {
		h := newStandbyHarness(t)
		h.demand[stallTemplate] = []string{"step-1", "step-2", "step-3"}
		for _, trigger := range []string{"step-1", "step-2", "step-3"} {
			h.overdueSeat(trigger)
		}
		if p := h.lastStall(); p.Action != events.AgentClaimsHeld {
			t.Fatalf("stall %+v, want held", p)
		}
		if !strings.Contains(h.stderr.String(), "no seat on another provider claimed") {
			t.Fatalf("stderr %q, want the reason named", h.stderr.String())
		}
	})
	t.Run("legacy reconciler", func(t *testing.T) {
		h := newStandbyHarness(t)
		h.allowed = false
		h.reconfigure()
		h.tripOnOwn()
		if p := h.lastStall(); p.Action != events.AgentClaimsHeld {
			t.Fatalf("stall %+v under the legacy reconciler, want held", p)
		}
	})
}

// The standby's cold start is not a stall: its seats' failures in the first
// claimStallStandbyGrace do not count. After it, a standby that fails too is
// held like any provider.
func TestClaimStallGuardStandbyGrace(t *testing.T) {
	h := newStandbyHarness(t)
	h.tripOnOwn()
	h.tick(nil)
	h.cityWorks()
	for _, trigger := range []string{"step-1", "step-2", "step-3"} {
		h.overdueSeat(trigger)
	}
	if n := len(h.stalls()); n != 1 {
		t.Fatalf("stalls = %d: the standby's cold start counted", n)
	}
	h.advance(claimStallStandbyGrace)
	h.cityWorks()
	for _, trigger := range []string{"step-1", "step-2", "step-3"} {
		h.overdueSeat(trigger)
	}
	stalls := h.stalls()
	if len(stalls) != 2 {
		t.Fatalf("stalls = %d, want the standby's own stall after the grace", len(stalls))
	}
	if p := stallPayload(t, stalls[1]); p.Provider != standbyProvider || p.Action != events.AgentClaimsHeld {
		t.Fatalf("standby stall %+v, want %s held", p, standbyProvider)
	}
	if got := h.pass(stallTemplate); len(got) != 0 {
		t.Fatalf("demand %v while the standby is held, want none", got)
	}
}

// When the move's hold runs out the template returns to its own provider and
// one probe seat goes there. A probe that fails moves it back with the hold
// doubled; one that claims ends the episode.
func TestClaimStallGuardProbesItsOwnProviderAndMovesBack(t *testing.T) {
	h := newStandbyHarness(t)
	h.tripOnOwn()
	episode := h.lastStall().Episode
	h.tick(nil)

	h.advance(claimStallStandbyHold)
	if !h.tick(nil) {
		t.Fatal("the move was not released when its hold ran out")
	}
	if rec := h.record(); len(rec.Agents) != 0 {
		t.Fatalf("record %+v after the release, want none", rec)
	}
	released := h.ofType(events.AgentStandbyReleased)
	if len(released) != 1 || stallPayload(t, released[0]).Cause != events.AgentClaimsCauseHoldEnded {
		t.Fatalf("released %v, want one, the hold ended", released)
	}
	if got := h.pass(stallTemplate); len(got) != 1 {
		t.Fatalf("demand %v back on the own provider, want one probe", got)
	}
	h.cityWorks()
	h.overdueSeat("step-1")
	p := h.lastStall()
	if p.Cause != events.AgentClaimsCauseProbe || p.Action != events.AgentClaimsStandby || p.Engagements != 2 || p.Episode != episode || p.Until.Sub(h.now) != 2*claimStallStandbyHold {
		t.Fatalf("probe stall %+v, want move 2 of %s for %v", p, episode, 2*claimStallStandbyHold)
	}
	h.tick(nil)
	if rec := h.record(); len(rec.Agents) != 1 || rec.Agents[0].Engagements != 2 {
		t.Fatalf("record %+v, want the second move", rec)
	}

	h.advance(2 * claimStallStandbyHold)
	h.tick(nil)
	if got := h.pass(stallTemplate); len(got) != 1 {
		t.Fatalf("demand %v, want one probe", got)
	}
	h.claim(h.spawn(stallTemplate, "step-1"))
	resumed := h.ofType(events.AgentClaimsResumed)
	if len(resumed) != 1 || stallPayload(t, resumed[0]).Episode != episode || stallPayload(t, resumed[0]).Provider != stallProvider {
		t.Fatalf("resumed %v, want the probe's claim to end %s", resumed, episode)
	}
	if got := h.pass(stallTemplate); len(got) != 5 {
		t.Fatalf("demand %v after the episode ended, want all of it", got)
	}
	if h.tick(nil) {
		t.Fatal("the record changed after the episode ended")
	}
}

// Another template's seat claiming on the replaced provider proves it back:
// the move ends at once, and its own probe follows.
func TestClaimStallGuardReleasesWhenTheOwnProviderProvesItself(t *testing.T) {
	h := newStandbyHarness(t)
	h.tripOnOwn()
	h.tick(nil)
	h.advance(time.Minute)
	h.guard.mu.Lock()
	h.guard.lastClaim[stallProvider] = h.now
	h.guard.mu.Unlock()
	if !h.tick(nil) {
		t.Fatal("the move stood after the own provider served another template")
	}
	released := h.ofType(events.AgentStandbyReleased)
	if len(released) != 1 || stallPayload(t, released[0]).Cause != events.AgentClaimsCauseClaimed {
		t.Fatalf("released %v, want one, by a claim", released)
	}
}

// A restarted controller keeps the move the record holds, and ends it when
// the recorded hold runs out.
func TestClaimStallGuardResumesTheRecordAfterARestart(t *testing.T) {
	h := newStandbyHarness(t)
	h.tripOnOwn()
	h.tick(nil)
	until := h.record().Agents[0].Until

	restarted := newClaimStallGuard(func(e events.Event) { h.alerts = append(h.alerts, e) }, &h.stderr).withStandbyRecord(h.fs, standbyCityPath)
	restarted.now = func() time.Time { return h.now }
	h.guard = restarted
	h.reconfigure()
	if got := h.pass(stallTemplate); len(got) != 5 {
		t.Fatalf("demand %v on the resumed standby, want all of it", got)
	}
	// Seats of the own provider from before the move, still around after the
	// restart, are not the standby's failures.
	h.advance(claimStallStandbyGrace)
	stalls := len(h.stalls())
	for _, trigger := range []string{"step-1", "step-2", "step-3"} {
		old := stallSeat{id: "gcg-session-old-" + trigger, template: stallTemplate, trigger: trigger, created: h.now.Add(-time.Hour)}
		h.seatEvent(events.BeadClosed, old.id, old.template, old.created, map[string]string{"gc.trigger_bead_id": trigger, "state": "idle", "creation_complete_at": old.created.Add(30 * time.Second).UTC().Format(time.RFC3339)})
		h.advance(time.Minute)
	}
	if len(h.stalls()) != stalls {
		t.Fatal("own-provider seats from before the move counted as the resumed standby's failures")
	}
	h.now = until
	if !h.tick(nil) {
		t.Fatal("the resumed move did not end at its recorded hold")
	}
	if got := h.pass(stallTemplate); len(got) != 1 {
		t.Fatalf("demand %v after the resumed move ended, want one probe", got)
	}
}

// healthSnapshot is the provider-health record a pack writes: each
// provider at its status.
func healthSnapshot(t *testing.T, statuses map[string]string) *providerHealthSnapshot {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc", "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	var records []providerHealthRecord
	for provider, status := range statuses {
		records = append(records, providerHealthRecord{Provider: provider, Status: status, ProbedAt: float64(time.Now().Unix())})
	}
	data, err := json.Marshal(providerHealthFileFormat{Providers: records})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gc", "cache", "provider-health.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return loadProviderHealthSnapshot(dir)
}

// A pack that classifies provider errors marks the own provider unhealthy in
// provider-health.json: the template moves at once, without a seat failing,
// stays moved while the record says so (past the hold: the reconciler would
// start no probe seat there), and returns when the record stops saying so.
func TestClaimStallGuardMovesOnProviderHealth(t *testing.T) {
	h := newStandbyHarness(t)
	red := healthSnapshot(t, map[string]string{stallProvider: "unhealthy"})
	if h.tick(red) {
		t.Fatal("an idle lane moved: nothing waits for it")
	}
	if h.tick(healthSnapshot(t, map[string]string{stallProvider: "unhealthy", standbyProvider: "unhealthy"})) {
		t.Fatal("moved to a standby the record marks unhealthy too")
	}
	h.demand[stallTemplate] = []string{"step-1"}
	h.pass(stallTemplate)
	if h.tick(healthSnapshot(t, map[string]string{stallProvider: "unhealthy", standbyProvider: "unhealthy"})) {
		t.Fatal("moved to a standby the record marks unhealthy too")
	}
	if !h.tick(red) {
		t.Fatal("an unhealthy own provider did not move a lane with waiting work")
	}
	p := h.lastStall()
	if p.Cause != events.AgentClaimsCauseProviderHealth || p.Action != events.AgentClaimsStandby || p.Standby != standbyProvider {
		t.Fatalf("stall %+v, want a move on the provider-health record", p)
	}
	if rec := h.record(); len(rec.Agents) != 1 || rec.Agents[0].Cause != events.AgentClaimsCauseProviderHealth {
		t.Fatalf("record %+v, want the move's cause recorded", rec)
	}
	h.advance(2 * claimStallStandbyHoldMax)
	if h.tick(red) {
		t.Fatal("the move ended while the record still marks the own provider unhealthy")
	}
	if !h.tick(healthSnapshot(t, map[string]string{stallProvider: "healthy"})) {
		t.Fatal("the move stood after the record marked the own provider healthy")
	}
	released := h.ofType(events.AgentStandbyReleased)
	if len(released) != 1 || stallPayload(t, released[0]).Cause != events.AgentClaimsCauseProviderHealth {
		t.Fatalf("released %v, want one, by the provider-health record", released)
	}
	if got := h.pass(stallTemplate); len(got) > 1 {
		t.Fatalf("demand %v back on the own provider, want at most one probe", got)
	}
}

// A move the failed seats made does not end when a provider-health record
// marks the own provider healthy: the record judged the endpoint, the seats
// the account. It does not end while the record marks it unhealthy either.
func TestClaimStallGuardSeatMoveIgnoresAHealthyRecord(t *testing.T) {
	h := newStandbyHarness(t)
	h.tripOnOwn()
	h.tick(nil)
	if h.tick(healthSnapshot(t, map[string]string{stallProvider: "healthy"})) {
		t.Fatal("a seat-made move ended on a healthy provider-health record")
	}
	h.advance(claimStallStandbyHold)
	if h.tick(healthSnapshot(t, map[string]string{stallProvider: "unhealthy"})) {
		t.Fatal("a move ended at its hold while the record marks the own provider unhealthy")
	}
	if !h.tick(nil) {
		t.Fatal("the move did not end at its hold once the record cleared")
	}
}

// A person who changes the agent's own provider, or its standby, ends the
// move: the record drops it and the release says why.
func TestClaimStallGuardConfigChangeEndsTheMove(t *testing.T) {
	h := newStandbyHarness(t)
	h.tripOnOwn()
	h.tick(nil)
	h.guard.configure(&config.City{Agents: []config.Agent{
		{Name: "review-codex", Dir: "gascity", Provider: standbyProvider},
		{Name: "review-claude", Dir: "gascity", Provider: "opus"},
	}}, true)
	if !h.tick(nil) {
		t.Fatal("the record kept a move the config no longer allows")
	}
	if rec := h.record(); len(rec.Agents) != 0 {
		t.Fatalf("record %+v, want the move dropped", rec)
	}
	released := h.ofType(events.AgentStandbyReleased)
	if len(released) != 1 || stallPayload(t, released[0]).Cause != events.AgentClaimsCauseConfigChanged {
		t.Fatalf("released %v, want one, by the config change", released)
	}
	resumed := h.ofType(events.AgentClaimsResumed)
	if len(resumed) != 1 || stallPayload(t, resumed[0]).Cause != events.AgentClaimsCauseConfigChanged {
		t.Fatalf("resumed %v, want the person's change to end the stall", resumed)
	}
}

// TestClaimStallGuardStandbySimulatesTheOct31Outage runs gascity's
// review-codex lane through 2026-10-31 under the guard, with a standby
// declared: the routed work is the day's real review-codex steps (the ready
// times in testdata/claim_stall_oct31.txt), the seats are simulated. The own
// provider is dead from 07:00 to 17:44, as Cursor was; the standby and every
// other provider work. On the day 48 runs waited a median 412 minutes and
// ~1,400 seats were spawned in vain. Here the lane moves to its standby once
// three of its seats have failed (thirteen minutes after the first, as seats
// come only as work does), probes its own provider a handful of times on a
// doubling hold, and returns once a probe claims after 17:44; the work never
// waits long, and the other lane is never touched.
func TestClaimStallGuardStandbySimulatesTheOct31Outage(t *testing.T) {
	fx := loadClaimStallFixture(t)
	h := newStandbyHarness(t)
	h.now = fx.start
	outageFrom, outageTo := oct31(t, "07:00:00"), oct31(t, "17:44:43")
	dead := func(provider string, at time.Time) bool {
		return provider == stallProvider && !at.Before(outageFrom) && at.Before(outageTo)
	}

	type step struct {
		id        string
		ready     time.Time
		claimedAt time.Time
	}
	var steps []*step
	for id, tr := range fx.triggers {
		if tr.template == "gascity/adopt-pr-review-codex" && !tr.ready.IsZero() {
			steps = append(steps, &step{id: fmt.Sprintf("trigger-%d", id), ready: tr.ready})
		}
	}
	sort.Slice(steps, func(i, j int) bool {
		if !steps[i].ready.Equal(steps[j].ready) {
			return steps[i].ready.Before(steps[j].ready)
		}
		return steps[i].id < steps[j].id
	})

	type simSeat struct {
		seat     stallSeat
		step     *step
		provider string
		wakeAt   time.Time
		claimAt  time.Time // zero: never
		closeAt  time.Time
		woke     bool
		claimed  bool
		closed   bool
	}
	var seats []*simSeat
	live := map[*step]*simSeat{}
	spawnedOnOwn := map[bool]int{} // during the outage?
	var firstDead time.Time
	provider := func() string {
		for _, a := range h.composed().Agents {
			if a.QualifiedName() == stallTemplate {
				return a.Provider
			}
		}
		t.Fatalf("no %s in the composed config", stallTemplate)
		return ""
	}
	nextOther := fx.start
	end := oct31(t, "23:59:00")
	for now := fx.start; now.Before(end); now = now.Add(15 * time.Second) {
		h.now = now
		for _, s := range seats {
			switch {
			case !s.woke && !now.Before(s.wakeAt):
				s.woke = true
				h.now = s.wakeAt
				h.wake(s.seat)
			case s.woke && !s.claimed && !s.claimAt.IsZero() && !now.Before(s.claimAt):
				s.claimed = true
				s.step.claimedAt = s.claimAt
				h.now = s.claimAt
				h.workEvent(events.BeadUpdated, s.step.id, "in_progress", s.seat.id)
				h.claim(s.seat)
			case s.woke && !s.closed && !now.Before(s.closeAt):
				s.closed = true
				h.now = s.closeAt
				h.close(s.seat, "idle", s.wakeAt)
				if live[s.step] == s {
					delete(live, s.step)
				}
			}
		}
		h.now = now
		if !now.Before(nextOther) {
			h.cityWorks()
			nextOther = now.Add(5 * time.Minute)
		}
		h.tick(nil)
		var waiting []string
		byID := map[string]*step{}
		for _, st := range steps {
			if !st.ready.After(now) && st.claimedAt.IsZero() {
				waiting = append(waiting, st.id)
				byID[st.id] = st
			}
		}
		_, admitted := h.guard.withhold(map[string]int{stallTemplate: len(waiting)}, map[string]scaleCheckDemand{stallTemplate: {Count: len(waiting), WorkBeadIDs: waiting}})
		for _, id := range admitted[stallTemplate].WorkBeadIDs {
			st := byID[id]
			if live[st] != nil {
				continue
			}
			p := provider()
			s := &simSeat{seat: h.spawn(stallTemplate, id), step: st, provider: p, wakeAt: now.Add(30 * time.Second)}
			if dead(p, now) {
				s.closeAt = s.wakeAt.Add(10 * time.Minute)
				spawnedOnOwn[true]++
				if firstDead.IsZero() {
					firstDead = now
				}
			} else {
				s.claimAt = s.wakeAt.Add(20 * time.Second)
				s.closeAt = s.claimAt.Add(5 * time.Minute)
			}
			seats = append(seats, s)
			live[st] = s
		}
	}

	stalls := h.stalls()
	if len(stalls) == 0 {
		t.Fatal("the outage never tripped the guard")
	}
	first := stallPayload(t, stalls[0])
	if first.Action != events.AgentClaimsStandby || stalls[0].Ts.Before(firstDead) || stalls[0].Ts.Sub(firstDead) > 14*time.Minute {
		t.Fatalf("first stall at %s %+v, want a move to the standby within 14m of the first seat on the dead provider (%s)", stalls[0].Ts.Format("15:04:05"), first, firstDead.Format("15:04:05"))
	}
	engaged := h.ofType(events.AgentStandbyEngaged)
	if len(engaged) < 2 || len(engaged) > 7 {
		t.Fatalf("%d moves to the standby, want the first and a few after failed probes", len(engaged))
	}
	if spawnedOnOwn[true] > 30 {
		t.Fatalf("%d seats spawned on the dead own provider, want a few probes (the day spawned ~1,400)", spawnedOnOwn[true])
	}
	resumed := h.ofType(events.AgentClaimsResumed)
	if len(resumed) != 1 || resumed[0].Ts.Before(outageTo) || resumed[0].Ts.Sub(outageTo) > claimStallStandbyHoldMax+time.Hour {
		t.Fatalf("resumed %v, want one, after the own provider came back at %s", resumed, outageTo.Format("15:04"))
	}
	if p := stallPayload(t, resumed[0]); p.Provider != stallProvider || p.Episode != first.Episode {
		t.Fatalf("resumed %+v, want episode %s on %s", p, first.Episode, stallProvider)
	}
	var waits []time.Duration
	for _, st := range steps {
		if !st.ready.Before(outageFrom) && st.ready.Before(outageTo) && !st.claimedAt.IsZero() {
			waits = append(waits, st.claimedAt.Sub(st.ready))
		}
	}
	sort.Slice(waits, func(i, j int) bool { return waits[i] < waits[j] })
	if len(waits) == 0 || waits[len(waits)/2] > 20*time.Minute {
		t.Fatalf("outage steps waited %v (median), want minutes, not the day's 412", waits)
	}
	for _, e := range h.alerts {
		if e.Subject == otherTemplate {
			t.Fatalf("the healthy lane was touched: %+v", e)
		}
	}
	t.Logf("first move %s; %d moves; %d seats on the dead provider; resumed %s; outage steps waited median %v, max %v",
		stalls[0].Ts.Format("15:04:05"), len(engaged), spawnedOnOwn[true], resumed[0].Ts.Format("15:04:05"), waits[len(waits)/2], waits[len(waits)-1])
}

// An agent that takes its provider from [workspace] provider moves like any
// other: its own provider is the workspace's, the record names it as
// replaced, and the composed config applies the move.
func TestClaimStallGuardMovesAWorkspaceProviderAgent(t *testing.T) {
	city := strings.Replace(strings.Replace(standbyCity, "name = \"test\"\n", "name = \"test\"\nprovider = \"cursor-grok\"\n", 1),
		"dir = \"gascity\"\nprovider = \"cursor-grok\"\n", "dir = \"gascity\"\n", 1)
	h := newStandbyHarnessWith(t, city)
	h.tripOnOwn()
	if p := h.lastStall(); p.Action != events.AgentClaimsStandby || p.Provider != stallProvider {
		t.Fatalf("stall %+v, want a move off the workspace provider %s", p, stallProvider)
	}
	h.tick(nil)
	if rec := h.record(); len(rec.Agents) != 1 || rec.Agents[0].Replaced != stallProvider {
		t.Fatalf("record %+v, want the workspace provider replaced", rec)
	}
	for _, a := range h.composed().Agents {
		if a.QualifiedName() == stallTemplate && (a.Provider != standbyProvider || a.StandbyFor != stallProvider) {
			t.Fatalf("composed agent %+v, want it on %s in place of %s", a, standbyProvider, stallProvider)
		}
	}
	if got := h.pass(stallTemplate); len(got) != 5 {
		t.Fatalf("demand %v on the standby, want all of it (the move must not leave it held)", got)
	}
	if h.tick(nil) {
		t.Fatal("the guard dropped a move the composed config applies")
	}
}

// A person ends a move by deleting its entry and reloading: the agent
// returns to its own provider for one probe seat, the release says why, and
// the guard never writes the entry back.
func TestClaimStallGuardManualReleaseOnReload(t *testing.T) {
	h := newStandbyHarness(t)
	h.tripOnOwn()
	h.tick(nil)
	delete(h.fs.Files, config.StandbyPath(standbyCityPath))
	h.reconfigure() // gc reload
	if got := h.pass(stallTemplate); len(got) != 1 {
		t.Fatalf("demand %v after the manual release, want one probe on the own provider", got)
	}
	if !h.tick(nil) {
		t.Fatal("the released move was not reported")
	}
	released := h.ofType(events.AgentStandbyReleased)
	if len(released) != 1 || stallPayload(t, released[0]).Cause != events.AgentClaimsCauseConfigChanged {
		t.Fatalf("released %v, want one, by the person's change", released)
	}
	if rec := h.record(); len(rec.Agents) != 0 {
		t.Fatalf("record %+v, want the deleted entry kept deleted", rec)
	}
}

// A deleted entry the person did not reload for is noticed at the next tick,
// which reloads, and is never written back.
func TestClaimStallGuardManualReleaseWithoutReload(t *testing.T) {
	h := newStandbyHarness(t)
	h.tripOnOwn()
	h.tick(nil)
	delete(h.fs.Files, config.StandbyPath(standbyCityPath))
	if !h.tick(nil) {
		t.Fatal("the deleted entry was not noticed")
	}
	if rec := h.record(); len(rec.Agents) != 0 {
		t.Fatalf("record %+v, want the deleted entry kept deleted", rec)
	}
	if released := h.ofType(events.AgentStandbyReleased); len(released) != 1 {
		t.Fatalf("released %v, want one", released)
	}
}

// Another agent claiming on the own provider ends only a first move early:
// once a probe of this agent has failed there, the failure was its own.
func TestClaimStallGuardEarlyReleaseOnlyOnTheFirstMove(t *testing.T) {
	h := newStandbyHarness(t)
	h.tripOnOwn()
	h.tick(nil)
	h.advance(claimStallStandbyHold)
	h.tick(nil) // the hold ran out: back to the own provider
	h.pass(stallTemplate)
	h.cityWorks()
	h.overdueSeat("step-1") // the probe fails: move 2
	h.tick(nil)
	if rec := h.record(); len(rec.Agents) != 1 || rec.Agents[0].Engagements != 2 {
		t.Fatalf("record %+v, want the second move", rec)
	}
	h.advance(time.Minute)
	h.guard.mu.Lock()
	h.guard.lastClaim[stallProvider] = h.now
	h.guard.mu.Unlock()
	if h.tick(nil) {
		t.Fatal("a second move ended early on another agent's claim")
	}
}

// While the guard and the demand pass run different configs (a move's
// reload in flight), the template spawns nothing: no seat may start on a
// provider the guard does not count it under.
func TestClaimStallGuardWaitsForThePassToAgree(t *testing.T) {
	h := newStandbyHarness(t)
	before := h.composed() // the pass still runs this one
	h.tripOnOwn()
	h.tick(nil) // the record is written and the guard adopts the moved config
	counts := map[string]int{stallTemplate: 2}
	demand := map[string]scaleCheckDemand{stallTemplate: {Count: 2, WorkBeadIDs: []string{"step-4", "step-5"}}}
	if _, got := h.guard.withholdUnder(before, counts, demand); len(got[stallTemplate].WorkBeadIDs) != 0 {
		t.Fatalf("demand %v under the pre-move config, want none", got[stallTemplate].WorkBeadIDs)
	}
	if _, got := h.guard.withholdUnder(h.composed(), counts, demand); len(got[stallTemplate].WorkBeadIDs) != 2 {
		t.Fatalf("demand %v once the pass runs the moved config, want all of it", got[stallTemplate].WorkBeadIDs)
	}
}

// A stall of the standby itself that ends (a standby seat claims after the
// grace) ends that episode only: the agent stays on its standby.
func TestClaimStallGuardAStandbyStallEndingKeepsTheMove(t *testing.T) {
	h := newStandbyHarness(t)
	h.tripOnOwn()
	h.tick(nil)
	h.advance(claimStallStandbyGrace)
	h.cityWorks()
	for _, trigger := range []string{"step-1", "step-2", "step-3"} {
		h.overdueSeat(trigger)
	}
	if p := h.lastStall(); p.Provider != standbyProvider {
		t.Fatalf("stall %+v, want the standby's own stall", p)
	}
	h.claim(h.spawn(stallTemplate, "step-4"))
	if resumed := h.ofType(events.AgentClaimsResumed); len(resumed) != 1 || stallPayload(t, resumed[0]).Provider != standbyProvider {
		t.Fatalf("resumed %v, want the standby's stall to end", resumed)
	}
	if h.tick(nil) {
		t.Fatal("the move ended when the standby's own stall did")
	}
	if rec := h.record(); len(rec.Agents) != 1 {
		t.Fatalf("record %+v, want the move to stand", rec)
	}
}

// A standby the provider-health record marks unhealthy is not moved to, and
// a move whose standby turns unhealthy ends: the reconciler starts no seat on
// a red provider, so the agent would get none.
func TestClaimStallGuardRespectsTheStandbysHealth(t *testing.T) {
	t.Run("no move to a red standby", func(t *testing.T) {
		h := newStandbyHarness(t)
		h.tick(healthSnapshot(t, map[string]string{standbyProvider: "unhealthy"}))
		h.tripOnOwn()
		if p := h.lastStall(); p.Action != events.AgentClaimsHeld {
			t.Fatalf("stall %+v, want held: the standby is red", p)
		}
		if !strings.Contains(h.stderr.String(), "unhealthy") {
			t.Fatalf("stderr %q, want the reason named", h.stderr.String())
		}
	})
	t.Run("a standby that turns red ends the move", func(t *testing.T) {
		h := newStandbyHarness(t)
		h.tripOnOwn()
		h.tick(nil)
		if !h.tick(healthSnapshot(t, map[string]string{standbyProvider: "unhealthy"})) {
			t.Fatal("the move stood on a red standby")
		}
		released := h.ofType(events.AgentStandbyReleased)
		if len(released) != 1 || stallPayload(t, released[0]).Cause != events.AgentClaimsCauseProviderHealth {
			t.Fatalf("released %v, want one, by the provider-health record", released)
		}
	})
}

// A move is recorded before anything can end it early: a claim on the own
// provider between the decision and the write does not drop it unseen.
func TestClaimStallGuardRecordsAMoveBeforeReleasingIt(t *testing.T) {
	h := newStandbyHarness(t)
	h.tripOnOwn()
	h.guard.mu.Lock()
	h.guard.lastClaim[stallProvider] = h.now.Add(time.Second)
	h.guard.mu.Unlock()
	h.advance(time.Minute)
	if !h.tick(nil) {
		t.Fatal("the move was not written")
	}
	if engaged := h.ofType(events.AgentStandbyEngaged); len(engaged) != 1 {
		t.Fatalf("engaged %v, want the move recorded before any early release", engaged)
	}
}

// A reused seat's later claims prove its provider too: a session that takes a
// second bead of work is as much evidence that the city works as a fresh
// seat's first claim, whether its session bead or the work bead says so first.
// A notification that only repeats a claim the guard has seen (the seat's
// session bead updated for another reason) is no new claim.
func TestClaimStallGuardAReusedSeatsClaimProvesItsProvider(t *testing.T) {
	// trip trips the template while another provider's seat, which claimed
	// before the streak began, does what between says after the first failure.
	trip := func(h *standbyHarness, between func(other stallSeat, claimedAt time.Time)) events.AgentClaimsPayload {
		h.t.Helper()
		other := h.spawn(otherTemplate, "other-step-1")
		h.claim(other)
		claimedAt := h.now
		h.advance(time.Minute)
		h.demand[stallTemplate] = []string{"step-1", "step-2", "step-3"}
		h.overdueSeat("step-1")
		between(other, claimedAt)
		h.overdueSeat("step-2")
		h.overdueSeat("step-3")
		return h.lastStall()
	}
	t.Run("a second claim in its session bead", func(t *testing.T) {
		h := newStandbyHarness(t)
		p := trip(h, func(other stallSeat, _ time.Time) {
			other.trigger = "other-step-2"
			h.claim(other)
		})
		if p.Action != events.AgentClaimsStandby {
			t.Fatalf("stall %+v, want a move: the reused seat's new claim proves the city works; log: %s", p, h.stderr.String())
		}
	})
	t.Run("a second work bead taken", func(t *testing.T) {
		h := newStandbyHarness(t)
		p := trip(h, func(other stallSeat, _ time.Time) {
			h.demand[otherTemplate] = []string{"other-step-2"}
			h.pass(otherTemplate)
			h.workEvent(events.BeadUpdated, "other-step-2", "in_progress", other.id)
		})
		if p.Action != events.AgentClaimsStandby {
			t.Fatalf("stall %+v, want a move: the reused seat's new claim proves the city works; log: %s", p, h.stderr.String())
		}
	})
	t.Run("the same claim again", func(t *testing.T) {
		h := newStandbyHarness(t)
		p := trip(h, func(other stallSeat, claimedAt time.Time) {
			cell := fmt.Sprintf(`{"v":1,"claims":[{"store":"city","bead":%q,"gen":2,"at":%q,"via":"claim"}]}`, other.trigger, claimedAt.UTC().Format(time.RFC3339Nano))
			h.seatEvent(events.BeadUpdated, other.id, other.template, other.created, map[string]string{
				beadmeta.TriggerBeadIDMetadataKey: other.trigger, "state": "asleep", session.WorkStartedKey: cell,
			})
		})
		if p.Action != events.AgentClaimsHeld || !strings.Contains(h.stderr.String(), "no seat on another provider claimed") {
			t.Fatalf("stall %+v, want it held: a repeated claim is no evidence; log: %s", p, h.stderr.String())
		}
	})
}

// A standby that turned red ended its move, and the probe that followed on
// the own provider failed: the template is held, the hold doubling as any
// failed probe's does, and no move is announced (the reconciler would start no
// seat on the red standby, and the next tick would drop the move and admit
// another probe at once). The stall keeps its standby: once the record no
// longer marks the standby unhealthy, the next failed probe moves it there.
func TestClaimStallGuardAFailedProbeHoldsWhileItsStandbyIsRed(t *testing.T) {
	h := newStandbyHarness(t)
	h.tripOnOwn()
	episode := h.lastStall().Episode
	h.tick(nil)
	red := healthSnapshot(t, map[string]string{standbyProvider: "unhealthy"})
	if !h.tick(red) {
		t.Fatal("the move stood on a red standby")
	}
	if got := h.pass(stallTemplate); len(got) != 1 {
		t.Fatalf("demand %v back on the own provider, want one probe", got)
	}
	h.overdueSeat("step-1")
	p := h.lastStall()
	if p.Action != events.AgentClaimsHeld || p.Cause != events.AgentClaimsCauseProbe || p.Episode != episode || p.Until == nil || p.Until.Sub(h.now) != claimStallHold(2) {
		t.Fatalf("probe stall %+v, want %s held %v: its standby is red; log: %s", p, episode, claimStallHold(2), h.stderr.String())
	}
	if h.tick(red) {
		t.Fatal("a move to the red standby was written")
	}
	if got := h.pass(stallTemplate); len(got) != 0 {
		t.Fatalf("demand %v after the failed probe, want it held, not probed again at once", got)
	}
	moves := 0
	for _, e := range h.stalls() {
		if stallPayload(t, e).Action == events.AgentClaimsStandby {
			moves++
		}
	}
	if moves != 1 {
		t.Fatalf("%d stall alerts announce a move, want the trip's only", moves)
	}

	h.advance(claimStallHold(2))
	h.tick(nil)
	if got := h.pass(stallTemplate); len(got) != 1 {
		t.Fatalf("demand %v when the hold ran out, want one probe", got)
	}
	h.overdueSeat("step-1")
	if p := h.lastStall(); p.Action != events.AgentClaimsStandby || p.Episode != episode || p.Engagements != 2 {
		t.Fatalf("probe stall %+v, want move 2 of %s: the standby is healthy again", p, episode)
	}
	if !h.tick(nil) {
		t.Fatal("the move to the recovered standby was not written")
	}
}

// A stall held at its trip because its standby was red has the provider's
// evidence all the same: once the standby recovers, a failed probe moves it.
func TestClaimStallGuardMovesAStallHeldForItsRedStandbyOnceItRecovers(t *testing.T) {
	h := newStandbyHarness(t)
	h.tick(healthSnapshot(t, map[string]string{standbyProvider: "unhealthy"}))
	h.tripOnOwn()
	p := h.lastStall()
	if p.Action != events.AgentClaimsHeld {
		t.Fatalf("stall %+v, want held: the standby is red", p)
	}
	h.advance(p.Until.Sub(h.now))
	h.tick(nil)
	if got := h.pass(stallTemplate); len(got) != 1 {
		t.Fatalf("demand %v when the hold ran out, want one probe", got)
	}
	h.overdueSeat("step-1")
	if p := h.lastStall(); p.Action != events.AgentClaimsStandby || p.Engagements != 1 {
		t.Fatalf("probe stall %+v, want the first move: the standby recovered", p)
	}
}

// A move chosen while the standby was healthy, whose standby the next tick
// finds red before it is written, was never made: nothing is written or
// announced as engaged, the template is held as a failed probe holds it (no
// probe at once, as releasing a made move would admit), and the move is not
// counted, so the first move made later is move 1.
func TestClaimStallGuardTakesBackAMoveItsStandbyTurnedRedFor(t *testing.T) {
	h := newStandbyHarness(t)
	h.tripOnOwn()
	episode := h.lastStall().Episode
	if h.tick(healthSnapshot(t, map[string]string{standbyProvider: "unhealthy"})) {
		t.Fatal("a move to a standby that turned red was written")
	}
	if n := len(h.ofType(events.AgentStandbyEngaged)) + len(h.ofType(events.AgentStandbyReleased)); n != 0 {
		t.Fatalf("%d standby records for a move never made", n)
	}
	p := h.lastStall()
	if p.Action != events.AgentClaimsHeld || p.Episode != episode || p.Until == nil || !p.Until.After(h.now) {
		t.Fatalf("stall %+v, want the move taken back and %s held", p, episode)
	}
	if got := h.pass(stallTemplate); len(got) != 0 {
		t.Fatalf("demand %v, want it held, not probed at once", got)
	}
	h.advance(p.Until.Sub(h.now))
	h.tick(nil)
	if got := h.pass(stallTemplate); len(got) != 1 {
		t.Fatalf("demand %v when the hold ran out, want one probe", got)
	}
	h.overdueSeat("step-1")
	if p := h.lastStall(); p.Action != events.AgentClaimsStandby || p.Episode != episode || p.Engagements != 1 {
		t.Fatalf("probe stall %+v, want move 1 of %s", p, episode)
	}
}

// standbyLaunchCheck is the v2 planner's transport check on a host whose PATH
// lacks the commands named.
func standbyLaunchCheck(missing ...string) func(*config.City, *config.Agent) error {
	lookPath := func(bin string) (string, error) {
		for _, m := range missing {
			if bin == m {
				return "", fmt.Errorf("%s: not on PATH", bin)
			}
		}
		return "/usr/bin/" + bin, nil
	}
	return func(cfg *config.City, a *config.Agent) error {
		return validateAgentSessionTransport(&cfg.Workspace, cfg.Providers, lookPath, nil, a, a.QualifiedName())
	}
}

// A standby the reconciler's own transport check refuses (its command is not
// on this host's PATH) is never moved to: the v2 planner would refuse every
// seat of the moved template, and the lane would sit seatless and unseen for
// the whole hold. The stall is held and says why, the provider-health record
// moves it nowhere either, and a move the record holds ends when its standby
// stops passing.
func TestClaimStallGuardMovesOnlyToAStandbyItsSeatsCanStartOn(t *testing.T) {
	t.Run("a standby that passes", func(t *testing.T) {
		h := newStandbyHarness(t)
		h.guard.withLaunchCheck(standbyLaunchCheck())
		h.reconfigure()
		h.tripOnOwn()
		if p := h.lastStall(); p.Action != events.AgentClaimsStandby {
			t.Fatalf("stall %+v, want a move to the standby the check passes; log: %s", p, h.stderr.String())
		}
	})
	t.Run("held, not moved", func(t *testing.T) {
		h := newStandbyHarness(t)
		h.guard.withLaunchCheck(standbyLaunchCheck("codex"))
		h.reconfigure()
		h.tripOnOwn()
		if p := h.lastStall(); p.Action != events.AgentClaimsHeld {
			t.Fatalf("stall %+v, want held: the standby cannot start its seats", p)
		}
		if !strings.Contains(h.stderr.String(), "provider not found in PATH") {
			t.Fatalf("stderr %q, want the reason named", h.stderr.String())
		}
		if h.tick(nil) {
			t.Fatal("a held stall was recorded as a move")
		}
		if h.tick(healthSnapshot(t, map[string]string{stallProvider: "unhealthy"})) {
			t.Fatal("the provider-health record moved the agent to a standby its seats cannot start on")
		}
	})
	t.Run("a move ends when its standby stops passing", func(t *testing.T) {
		h := newStandbyHarness(t)
		h.tripOnOwn()
		h.tick(nil)
		h.guard.withLaunchCheck(standbyLaunchCheck("codex"))
		h.reconfigure() // the standby's command left PATH
		if !h.tick(nil) {
			t.Fatal("the record kept a move to a standby its seats cannot start on")
		}
		if rec := h.record(); len(rec.Agents) != 0 {
			t.Fatalf("record %+v, want the move dropped", rec)
		}
		if got := h.pass(stallTemplate); len(got) != 1 {
			t.Fatalf("demand %v back on the own provider, want one probe", got)
		}
	})
}
