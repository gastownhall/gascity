package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// The spawn-churn guard's trip (ga-s5l1ms, pool_spawn_churn.go): three
// churned closes of counted blind seats, spaced at least the minimum span
// and within the window, hold the template's blind demand; anything less,
// tighter, or older does not.

type poolSpawnChurnHarness struct {
	t        *testing.T
	guard    *poolSpawnChurnGuard
	now      time.Time
	stderr   *bytes.Buffer
	seats    int
	recorded []events.Event
}

func newPoolSpawnChurnHarness(t *testing.T) *poolSpawnChurnHarness {
	t.Helper()
	h := &poolSpawnChurnHarness{
		t:      t,
		now:    time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC),
		stderr: &bytes.Buffer{},
	}
	h.guard = newPoolSpawnChurnGuard(func(e events.Event) { h.recorded = append(h.recorded, e) }, h.stderr)
	h.guard.now = func() time.Time { return h.now }
	return h
}

// seatEvent feeds one session-bead notification about id, stamped now.
func (h *poolSpawnChurnHarness) seatEvent(typ, id, template string, created time.Time, md map[string]string) {
	h.t.Helper()
	meta := map[string]string{"template": template}
	for k, v := range md {
		meta[k] = v
	}
	b := beads.Bead{ID: id, Title: id, Status: "open", Type: session.BeadType, CreatedAt: created, Metadata: meta}
	if typ == events.BeadClosed {
		b.Status = "closed"
	}
	payload, err := beads.EncodeBeadEventPayload(b)
	if err != nil {
		h.t.Fatal(err)
	}
	h.guard.observe(events.Event{Type: typ, Ts: h.now, Subject: id, Payload: payload})
}

// churnBlindSeat feeds one blind pool seat through its whole short life:
// created by the pool, woken, closed churned — everything stamped at h.now.
func (h *poolSpawnChurnHarness) churnBlindSeat(template string) {
	h.t.Helper()
	h.seats++
	id := fmt.Sprintf("gcg-session-%d", h.seats)
	pool := map[string]string{"pool_managed": "true"}
	h.seatEvent(events.BeadCreated, id, template, h.now, pool)
	h.seatEvent(events.BeadUpdated, id, template, h.now, map[string]string{
		"pool_managed":         "true",
		"creation_complete_at": h.now.UTC().Format(time.RFC3339),
	})
	h.seatEvent(events.BeadClosed, id, template, h.now, pool)
}

func TestPoolSpawnChurnGuardTripsAfterThreeChurnedCloses(t *testing.T) {
	h := newPoolSpawnChurnHarness(t)
	base := h.now
	for i := 0; i < 3; i++ {
		h.now = base.Add(time.Duration(i) * 3 * time.Minute)
		h.churnBlindSeat("claude")
	}
	// cursor churns too, below the trip: its own streak, never claude's hold.
	for i := 0; i < 2; i++ {
		h.now = base.Add(time.Duration(i) * 3 * time.Minute)
		h.churnBlindSeat("cursor")
	}
	allow := h.guard.allowance()
	if got, ok := allow["claude"]; !ok || got != 0 {
		t.Fatalf("allowance after three spaced churned closes = %v, want claude: 0 while the hold stands", allow)
	}
	// The hold runs out and exactly one probe seat is admitted again.
	h.now = base.Add(6*time.Minute + 5*time.Minute + time.Second)
	if got := h.guard.allowance()["claude"]; got != 1 {
		t.Fatalf("allowance after the hold runs out = %d, want 1 probe seat", got)
	}
}

func TestPoolSpawnChurnGuardTwoClosesDoNotTrip(t *testing.T) {
	h := newPoolSpawnChurnHarness(t)
	base := h.now
	for i := 0; i < 2; i++ {
		h.now = base.Add(time.Duration(i) * 3 * time.Minute)
		h.churnBlindSeat("claude")
	}
	allow := h.guard.allowance()
	if _, ok := allow["claude"]; ok {
		t.Fatalf("two churned closes tripped a hold: %v, want none", allow)
	}
}

func TestPoolSpawnChurnGuardOneBurstDoesNotTripAlone(t *testing.T) {
	h := newPoolSpawnChurnHarness(t)
	base := h.now
	for i := 0; i < 3; i++ {
		h.now = base.Add(time.Duration(i) * 30 * time.Second)
		h.churnBlindSeat("claude")
	}
	allow := h.guard.allowance()
	if _, ok := allow["claude"]; ok {
		t.Fatalf("three closes inside the minimum span tripped a hold: %v, want none: one tick's burst is one event", allow)
	}
	// A fourth close minutes later is churn over time, not one burst: the
	// streak carries the burst and the span now qualifies.
	h.now = base.Add(3 * time.Minute)
	h.churnBlindSeat("claude")
	if got := h.guard.allowance()["claude"]; got != 0 {
		t.Fatalf("allowance after a burst plus one later close = %d, want 0", got)
	}
}

func TestPoolSpawnChurnGuardStreakDropsClosesOlderThanTheWindow(t *testing.T) {
	h := newPoolSpawnChurnHarness(t)
	base := h.now
	h.churnBlindSeat("claude") // 35 minutes before the rest: out of the window
	h.now = base.Add(35 * time.Minute)
	h.churnBlindSeat("claude")
	h.now = base.Add(38 * time.Minute)
	h.churnBlindSeat("claude")
	allow := h.guard.allowance()
	if _, ok := allow["claude"]; ok {
		t.Fatalf("a close 35 minutes old still counted: %v, want only the two recent closes in the streak", allow)
	}
}

func TestPoolSpawnChurnGuardRegistryRoundtrip(t *testing.T) {
	h := newPoolSpawnChurnHarness(t)
	dir := t.TempDir()
	unregister := registerPoolSpawnChurnGuard(context.TODO(), dir, h.guard)
	if poolSpawnChurnGuardFor(dir) != h.guard {
		t.Fatalf("registry did not return the registered guard")
	}
	unregister()
	if poolSpawnChurnGuardFor(dir) != nil {
		t.Fatalf("registry returned a guard after unregister")
	}
	if poolSpawnChurnGuardFor("") != nil {
		t.Fatalf("a blank city path resolved a guard")
	}
}

// churnSeatOpts feeds one pool seat through its life with md merged into
// every event's metadata.
func (h *poolSpawnChurnHarness) churnSeatOpts(template string, md map[string]string) {
	h.t.Helper()
	h.seats++
	id := fmt.Sprintf("gcg-session-%d", h.seats)
	pool := map[string]string{"pool_managed": "true"}
	merge := func(extra map[string]string) map[string]string {
		out := map[string]string{"pool_managed": "true"}
		for k, v := range pool {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	h.seatEvent(events.BeadCreated, id, template, h.now, merge(md))
	h.seatEvent(events.BeadUpdated, id, template, h.now, merge(map[string]string{
		"creation_complete_at": h.now.UTC().Format(time.RFC3339),
	}))
	h.seatEvent(events.BeadClosed, id, template, h.now, merge(md))
}

// AC-4 (ga-s5l1ms): the closes that never count as churn — a bound seat, a
// claimed seat, an operational claims_errored drain, a failed create, a
// named or manual session — do not add to any template's streak. The plain
// churned close still trips (TestPoolSpawnChurnGuardTripsAfterThreeChurnedCloses).
func TestPoolSpawnChurnGuardExcludedClosesDoNotCount(t *testing.T) {
	claimCell := fmt.Sprintf(`{"v":1,"claims":[{"store":"city","bead":"bd-9","gen":2,"at":%q,"via":"claim"}]}`,
		time.Date(2026, 11, 19, 11, 0, 0, 0, time.UTC).Format(time.RFC3339Nano))
	for _, tc := range []struct {
		name string
		md   map[string]string
	}{
		{name: "bound to a trigger bead", md: map[string]string{beadmeta.TriggerBeadIDMetadataKey: "bd-9"}},
		{name: "claimed work in work_started", md: map[string]string{"work_started": claimCell}},
		{name: "claims_errored close", md: map[string]string{"sleep_reason": "claims_errored"}},
		{name: "failed-create close", md: map[string]string{"state": "failed-create"}},
		{name: "named session", md: map[string]string{session.NamedSessionMetadataKey: "true"}},
		{name: "manual session", md: map[string]string{"manual_session": "true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPoolSpawnChurnHarness(t)
			base := h.now
			for i := 0; i < 3; i++ {
				h.now = base.Add(time.Duration(i) * 3 * time.Minute)
				h.churnSeatOpts("claude", tc.md)
			}
			if allow := h.guard.allowance(); len(allow) != 0 {
				t.Fatalf("excluded closes counted toward a hold: %v", allow)
			}
		})
	}
}

// AC-4: a trigger stamp cleared mid-life still binds — provenance is sticky,
// so a late clear cannot turn a bound seat into churn.
func TestPoolSpawnChurnGuardClearedStampStillBinds(t *testing.T) {
	h := newPoolSpawnChurnHarness(t)
	base := h.now
	for i := 0; i < 3; i++ {
		h.now = base.Add(time.Duration(i) * 3 * time.Minute)
		h.seats++
		id := fmt.Sprintf("gcg-session-%d", h.seats)
		pool := map[string]string{"pool_managed": "true"}
		bound := map[string]string{"pool_managed": "true", beadmeta.TriggerBeadIDMetadataKey: "bd-9"}
		h.seatEvent(events.BeadCreated, id, "claude", h.now, bound)
		h.seatEvent(events.BeadUpdated, id, "claude", h.now, map[string]string{
			"pool_managed":         "true",
			"creation_complete_at": h.now.UTC().Format(time.RFC3339),
		}) // the stamp is gone from this snapshot on
		h.seatEvent(events.BeadClosed, id, "claude", h.now, pool)
	}
	if allow := h.guard.allowance(); len(allow) != 0 {
		t.Fatalf("a cleared stamp unbound a seat: %v", allow)
	}
}

// AC-4: a seat whose creation the guard did not observe — first seen through
// an update or a close, as after a controller restart — never counts, so the
// guard re-learns from genuinely new seats only.
func TestPoolSpawnChurnGuardSeatFirstSeenMidLifeDoesNotCount(t *testing.T) {
	h := newPoolSpawnChurnHarness(t)
	base := h.now
	for i := 0; i < 3; i++ {
		h.now = base.Add(time.Duration(i) * 3 * time.Minute)
		h.seats++
		id := fmt.Sprintf("gcg-session-%d", h.seats)
		pool := map[string]string{"pool_managed": "true"}
		h.seatEvent(events.BeadUpdated, id, "claude", h.now, map[string]string{
			"pool_managed":         "true",
			"creation_complete_at": h.now.UTC().Format(time.RFC3339),
		})
		h.seatEvent(events.BeadClosed, id, "claude", h.now, pool)
	}
	if allow := h.guard.allowance(); len(allow) != 0 {
		t.Fatalf("a seat first seen mid-life counted: %v", allow)
	}
}

// AC-5 (ga-s5l1ms): a trip records exactly one cooling-down event with the
// template, the episode, the cause, the seats, and the hold's since and
// until; repeated demand passes within the hold record nothing more.
func TestPoolSpawnChurnGuardRecordsCoolingDownOncePerEpisode(t *testing.T) {
	h := newPoolSpawnChurnHarness(t)
	base := h.now
	since := h.now
	for i := 0; i < 3; i++ {
		h.now = base.Add(time.Duration(i) * 3 * time.Minute)
		h.churnBlindSeat("claude")
	}
	for i := 0; i < 3; i++ {
		h.guard.allowance() // repeated passes within the hold
	}
	var cooling []events.Event
	for _, e := range h.recorded {
		if e.Type == events.SessionPoolSpawnChurnCoolingDown {
			cooling = append(cooling, e)
		}
	}
	if len(cooling) != 1 {
		t.Fatalf("recorded %d cooling-down events, want exactly 1: %+v", len(cooling), h.recorded)
	}
	e := cooling[0]
	if e.Subject != "claude" || e.Actor != "controller" {
		t.Fatalf("event subject %q actor %q, want claude/controller", e.Subject, e.Actor)
	}
	var p events.PoolSpawnChurnPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	wantUntil := base.Add(6 * time.Minute).Add(5 * time.Minute)
	if p.Template != "claude" || p.Cause != events.PoolSpawnChurnCauseTripped || p.Seats != 3 ||
		!p.Since.Equal(since) || p.Until == nil || !p.Until.Equal(wantUntil) || p.Episode == "" {
		t.Fatalf("payload = %+v, want claude/tripped/3 seats/since %s/until %s", p, since, wantUntil)
	}
}

// AC-6 (ga-s5l1ms): after a hold runs out exactly one probe seat is
// admitted; its churn doubles the hold (capped at 30 min) and records the
// cooling-down again with cause probe_failed; a close of a seat created
// before the probe is a straggler and changes nothing.
func TestPoolSpawnChurnGuardProbeFailureDoublesTheHold(t *testing.T) {
	h := newPoolSpawnChurnHarness(t)
	base := h.now
	for i := 0; i < 3; i++ {
		h.now = base.Add(time.Duration(i) * 3 * time.Minute)
		h.churnBlindSeat("claude")
	}
	probeFailures := func() (n int, last events.PoolSpawnChurnPayload) {
		for _, e := range h.recorded {
			if e.Type != events.SessionPoolSpawnChurnCoolingDown {
				continue
			}
			var p events.PoolSpawnChurnPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				h.t.Fatal(err)
			}
			if p.Cause == events.PoolSpawnChurnCauseProbeFailed {
				n++
				last = p
			}
		}
		return n, last
	}
	// Hold until base+11m; the first pass after admits one probe seat.
	h.now = base.Add(11*time.Minute + time.Second)
	if got := h.guard.allowance()["claude"]; got != 1 {
		t.Fatalf("allowance after the hold = %d, want 1 probe seat", got)
	}
	holds := []time.Duration{10 * time.Minute, 20 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	at := base.Add(12 * time.Minute)
	wantUntil := base.Add(11 * time.Minute) // first probe admission reference
	_ = wantUntil
	for i, hold := range holds {
		h.now = at
		h.churnBlindSeat("claude") // the probe seat churns
		wantUntil = at.Add(hold)
		if got := h.guard.allowance()["claude"]; got != 0 {
			t.Fatalf("allowance after probe failure %d = %d, want 0", i+1, got)
		}
		n, last := probeFailures()
		if n != i+1 {
			t.Fatalf("probe-failure events = %d after %d failures, want %d", n, i+1, i+1)
		}
		if last.Cause != events.PoolSpawnChurnCauseProbeFailed || last.Until == nil || !last.Until.Equal(wantUntil) || last.Seats != 4+i {
			t.Fatalf("probe-failure %d payload = %+v, want until %s and %d seats", i+1, last, wantUntil, 4+i)
		}
		at = wantUntil.Add(time.Second)
		h.now = at
		if got := h.guard.allowance()["claude"]; got != 1 {
			t.Fatalf("allowance after doubled hold %d = %d, want 1 probe seat", i+1, got)
		}
		at = at.Add(time.Minute)
	}
}

// AC-6: a close of a seat created before the probe is a straggler — work the
// pool had already started — and neither fails the probe nor trips again.
func TestPoolSpawnChurnGuardIgnoresStragglerClosesDuringAProbe(t *testing.T) {
	h := newPoolSpawnChurnHarness(t)
	base := h.now
	for i := 0; i < 3; i++ {
		h.now = base.Add(time.Duration(i) * 3 * time.Minute)
		h.churnBlindSeat("claude")
	}
	h.now = base.Add(11*time.Minute + time.Second)
	if got := h.guard.allowance()["claude"]; got != 1 {
		t.Fatalf("allowance after the hold = %d, want 1 probe seat", got)
	}
	// A seat created during the hold, before the probe admission, closes now.
	h.now = base.Add(12 * time.Minute)
	h.seats++
	id := fmt.Sprintf("gcg-session-%d", h.seats)
	pool := map[string]string{"pool_managed": "true"}
	h.seatEvent(events.BeadCreated, id, "claude", base.Add(7*time.Minute), pool)
	h.seatEvent(events.BeadUpdated, id, "claude", base.Add(8*time.Minute), map[string]string{
		"pool_managed":         "true",
		"creation_complete_at": base.Add(8 * time.Minute).UTC().Format(time.RFC3339),
	})
	h.seatEvent(events.BeadClosed, id, "claude", h.now, pool)
	if got := h.guard.allowance()["claude"]; got != 1 {
		t.Fatalf("a straggler close ended the probe: allowance = %d, want still 1", got)
	}
	for _, e := range h.recorded {
		if e.Type == events.SessionPoolSpawnChurnCoolingDown {
			var p events.PoolSpawnChurnPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatal(err)
			}
			if p.Cause == events.PoolSpawnChurnCauseProbeFailed {
				t.Fatalf("a straggler close recorded a probe failure: %+v", p)
			}
		}
	}
}

// AC-6: a counted seat's claim ends the episode — the resumed event is
// recorded with cause claimed and the template's full blind count returns.
func TestPoolSpawnChurnGuardClaimEndsEpisodeWithResumedEvent(t *testing.T) {
	h := newPoolSpawnChurnHarness(t)
	base := h.now
	for i := 0; i < 3; i++ {
		h.now = base.Add(time.Duration(i) * 3 * time.Minute)
		h.churnBlindSeat("claude")
	}
	h.now = base.Add(7 * time.Minute)
	h.seats++
	id := fmt.Sprintf("gcg-session-%d", h.seats)
	pool := map[string]string{"pool_managed": "true"}
	h.seatEvent(events.BeadCreated, id, "claude", h.now, pool)
	h.seatEvent(events.BeadUpdated, id, "claude", h.now, map[string]string{
		"pool_managed":         "true",
		"creation_complete_at": h.now.UTC().Format(time.RFC3339),
	})
	claimCell := fmt.Sprintf(`{"v":1,"claims":[{"store":"city","bead":"bd-7","gen":2,"at":%q,"via":"claim"}]}`, h.now.UTC().Format(time.RFC3339Nano))
	h.seatEvent(events.BeadUpdated, id, "claude", h.now, map[string]string{
		"pool_managed": "true",
		"work_started": claimCell,
	})
	allow := h.guard.allowance()
	if _, ok := allow["claude"]; ok {
		t.Fatalf("a claim did not end the episode: %v, want no hold (full blind count returns)", allow)
	}
	var resumed int
	for _, e := range h.recorded {
		if e.Type != events.SessionPoolSpawnChurnResumed {
			continue
		}
		resumed++
		var p events.PoolSpawnChurnPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Cause != events.PoolSpawnChurnCauseClaimed || p.Template != "claude" || p.Seats != 3 || p.Episode == "" {
			t.Fatalf("resumed payload = %+v, want claude/claimed/3 seats", p)
		}
	}
	if resumed != 1 {
		t.Fatalf("resumed events = %d, want exactly 1", resumed)
	}
}

// AC-6: a config change to the template's scale_check, work_query,
// authoritative flag or provider ends the hold: the change may have been the
// fix, so the guard drops the template's state and records the resume.
func TestPoolSpawnChurnGuardConfigChangeEndsHold(t *testing.T) {
	h := newPoolSpawnChurnHarness(t)
	base := h.now
	h.guard.configure(&config.City{Agents: []config.Agent{{Name: "claude"}}})
	for i := 0; i < 3; i++ {
		h.now = base.Add(time.Duration(i) * 3 * time.Minute)
		h.churnBlindSeat("claude")
	}
	if got := h.guard.allowance()["claude"]; got != 0 {
		t.Fatalf("allowance under the hold = %d, want 0", got)
	}
	h.guard.configure(&config.City{Agents: []config.Agent{{Name: "claude", ScaleCheck: "echo 1"}}})
	allow := h.guard.allowance()
	if _, ok := allow["claude"]; ok {
		t.Fatalf("a config change kept the hold: %v", allow)
	}
	var resumed int
	for _, e := range h.recorded {
		if e.Type != events.SessionPoolSpawnChurnResumed {
			continue
		}
		resumed++
		var p events.PoolSpawnChurnPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Cause != events.PoolSpawnChurnCauseConfigChanged || p.Template != "claude" {
			t.Fatalf("resumed payload = %+v, want claude/config_changed", p)
		}
	}
	if resumed != 1 {
		t.Fatalf("resumed events = %d, want exactly 1", resumed)
	}
}

// AC-7 (ga-s5l1ms): a hold is one template's: another template's blind
// demand is unaffected, at the guard and in the planner.
func TestPoolSpawnChurnGuardHoldsOnlyTheChurningTemplate(t *testing.T) {
	h := newPoolSpawnChurnHarness(t)
	base := h.now
	for i := 0; i < 3; i++ {
		h.now = base.Add(time.Duration(i) * 3 * time.Minute)
		h.churnBlindSeat("claude")
	}
	// cursor churns too, below the trip: its own streak, never claude's hold.
	for i := 0; i < 2; i++ {
		h.now = base.Add(time.Duration(i) * 3 * time.Minute)
		h.churnBlindSeat("cursor")
	}
	allow := h.guard.allowance()
	if got, ok := allow["claude"]; !ok || got != 0 {
		t.Fatalf("claude's allowance = %v, want 0 under its hold", allow)
	}
	if _, ok := allow["cursor"]; ok {
		t.Fatalf("cursor picked up claude's hold: %v", allow)
	}
}
