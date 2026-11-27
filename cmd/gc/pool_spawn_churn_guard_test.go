package main

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// The spawn-churn guard's trip (ga-s5l1ms, pool_spawn_churn.go): three
// churned closes of counted blind seats, spaced at least the minimum span
// and within the window, hold the template's blind demand; anything less,
// tighter, or older does not.

type poolSpawnChurnHarness struct {
	t      *testing.T
	guard  *poolSpawnChurnGuard
	now    time.Time
	stderr *bytes.Buffer
	seats  int
}

func newPoolSpawnChurnHarness(t *testing.T) *poolSpawnChurnHarness {
	t.Helper()
	h := &poolSpawnChurnHarness{
		t:      t,
		now:    time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC),
		stderr: &bytes.Buffer{},
	}
	h.guard = newPoolSpawnChurnGuard(func(events.Event) {}, h.stderr)
	h.guard.now = func() time.Time { return h.now }
	return h
}

// seatEvent feeds one session-bead notification about id, stamped now.
func (h *poolSpawnChurnHarness) seatEvent(typ, id string, created time.Time, md map[string]string) {
	h.t.Helper()
	meta := map[string]string{"template": "claude"}
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
func (h *poolSpawnChurnHarness) churnBlindSeat() {
	h.t.Helper()
	h.seats++
	id := fmt.Sprintf("gcg-session-%d", h.seats)
	pool := map[string]string{"pool_managed": "true"}
	h.seatEvent(events.BeadCreated, id, h.now, pool)
	h.seatEvent(events.BeadUpdated, id, h.now, map[string]string{
		"pool_managed":         "true",
		"creation_complete_at": h.now.UTC().Format(time.RFC3339),
	})
	h.seatEvent(events.BeadClosed, id, h.now, pool)
}

func TestPoolSpawnChurnGuardTripsAfterThreeChurnedCloses(t *testing.T) {
	h := newPoolSpawnChurnHarness(t)
	base := h.now
	for i := 0; i < 3; i++ {
		h.now = base.Add(time.Duration(i) * 3 * time.Minute)
		h.churnBlindSeat()
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
		h.churnBlindSeat()
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
		h.churnBlindSeat()
	}
	allow := h.guard.allowance()
	if _, ok := allow["claude"]; ok {
		t.Fatalf("three closes inside the minimum span tripped a hold: %v, want none: one tick's burst is one event", allow)
	}
	// A fourth close minutes later is churn over time, not one burst: the
	// streak carries the burst and the span now qualifies.
	h.now = base.Add(3 * time.Minute)
	h.churnBlindSeat()
	if got := h.guard.allowance()["claude"]; got != 0 {
		t.Fatalf("allowance after a burst plus one later close = %d, want 0", got)
	}
}

func TestPoolSpawnChurnGuardStreakDropsClosesOlderThanTheWindow(t *testing.T) {
	h := newPoolSpawnChurnHarness(t)
	base := h.now
	h.churnBlindSeat() // 35 minutes before the rest: out of the window
	h.now = base.Add(35 * time.Minute)
	h.churnBlindSeat()
	h.now = base.Add(38 * time.Minute)
	h.churnBlindSeat()
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
func (h *poolSpawnChurnHarness) churnSeatOpts(md map[string]string) {
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
	h.seatEvent(events.BeadCreated, id, h.now, merge(md))
	h.seatEvent(events.BeadUpdated, id, h.now, merge(map[string]string{
		"creation_complete_at": h.now.UTC().Format(time.RFC3339),
	}))
	h.seatEvent(events.BeadClosed, id, h.now, merge(md))
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
				h.churnSeatOpts(tc.md)
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
		h.seatEvent(events.BeadCreated, id, h.now, bound)
		h.seatEvent(events.BeadUpdated, id, h.now, map[string]string{
			"pool_managed":         "true",
			"creation_complete_at": h.now.UTC().Format(time.RFC3339),
		}) // the stamp is gone from this snapshot on
		h.seatEvent(events.BeadClosed, id, h.now, pool)
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
		h.seatEvent(events.BeadUpdated, id, h.now, map[string]string{
			"pool_managed":         "true",
			"creation_complete_at": h.now.UTC().Format(time.RFC3339),
		})
		h.seatEvent(events.BeadClosed, id, h.now, pool)
	}
	if allow := h.guard.allowance(); len(allow) != 0 {
		t.Fatalf("a seat first seen mid-life counted: %v", allow)
	}
}
