package main

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

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
func (h *poolSpawnChurnHarness) churnBlindSeat() {
	h.t.Helper()
	h.seats++
	id := fmt.Sprintf("gcg-session-%d", h.seats)
	pool := map[string]string{"pool_managed": "true"}
	h.seatEvent(events.BeadCreated, id, "claude", h.now, pool)
	h.seatEvent(events.BeadUpdated, id, "claude", h.now, map[string]string{
		"pool_managed":         "true",
		"creation_complete_at": h.now.UTC().Format(time.RFC3339),
	})
	h.seatEvent(events.BeadClosed, id, "claude", h.now, pool)
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
