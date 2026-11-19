package main

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// A seat parked on pre-assigned blocked work (owner ruling 2026-10-18,
// ga-ylpbsy.16): the classifier every reconciler path reads.

var parkNow = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

func parkRFC(d time.Duration) string { return parkNow.Add(d).UTC().Format(time.RFC3339) }

func parkCity(agents ...config.Agent) *config.City {
	if len(agents) == 0 {
		agents = []config.Agent{{Name: "worker", StartCommand: "true"}}
	}
	return &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: agents}
}

// parkedRow is an asleep pool seat a drain-ack finalize left holding work:
// asleep, sleep_reason idle, slept an hour ago.
func parkedRow(id string, mutate ...func(*session.Info)) session.Info {
	info := session.Info{
		ID:                  id,
		Template:            "worker",
		SessionNameMetadata: "worker-" + id,
		PoolManaged:         true,
		PoolSlot:            "1",
		MetadataState:       "asleep",
		State:               session.StateAsleep,
		SleepReason:         "idle",
		SleptAt:             parkRFC(-time.Hour),
	}
	for _, m := range mutate {
		m(&info)
	}
	return info
}

// parkRows is parkSeats over one set of rows.
func parkRows(cfg *config.City, rows []session.Info, work []beads.Bead, ready []bool, now time.Time) parkedSeats {
	return parkSeats(cfg, [][]session.Info{rows}, work, ready, now)
}

// pin is an open bead pre-assigned to the seat id.
func pin(id, assignee string) beads.Bead {
	return beads.Bead{ID: id, Type: "task", Status: "open", Assignee: assignee, Metadata: map[string]string{"gc.routed_to": "worker"}}
}

func TestParkSeatsClassifiesOneSeat(t *testing.T) {
	inProgress := func(b beads.Bead) beads.Bead { b.Status = "in_progress"; return b }
	for _, tc := range []struct {
		name         string
		cfg          *config.City
		row          session.Info
		work         []beads.Bead
		ready        []bool
		wantEligible bool
		wantParked   bool
	}{
		{name: "an open, unready, unstarted pin parks", row: parkedRow("s1"), work: []beads.Bead{pin("w1", "s1")}, ready: []bool{false}, wantEligible: true, wantParked: true},
		{name: "a pin assigned by session name parks", row: parkedRow("s1"), work: []beads.Bead{pin("w1", "worker-s1")}, ready: []bool{false}, wantEligible: true, wantParked: true},
		{name: "two blocked pins park", row: parkedRow("s1"), work: []beads.Bead{pin("w1", "s1"), pin("w2", "s1")}, ready: []bool{false, false}, wantEligible: true, wantParked: true},
		{name: "a ready pin wakes the seat, it does not park", row: parkedRow("s1"), work: []beads.Bead{pin("w1", "s1")}, ready: []bool{true}, wantEligible: true},
		{name: "a missing readiness flag reads not ready, as the awake set reads it", row: parkedRow("s1"), work: []beads.Bead{pin("w1", "s1")}, ready: []bool{}, wantEligible: true, wantParked: true},
		{name: "an in-progress claim is a real stranding", row: parkedRow("s1"), work: []beads.Bead{inProgress(pin("w1", "s1"))}, ready: []bool{false}, wantEligible: true},
		{name: "a blocked pin beside an in-progress claim is a real stranding", row: parkedRow("s1"), work: []beads.Bead{pin("w1", "s1"), inProgress(pin("w2", "s1"))}, ready: []bool{false, false}, wantEligible: true},
		{name: "a blocked pin beside a ready one wakes", row: parkedRow("s1"), work: []beads.Bead{pin("w1", "s1"), pin("w2", "s1")}, ready: []bool{false, true}, wantEligible: true},
		{name: "an open bead the seat claimed is started", row: parkedRow("s1", func(i *session.Info) { i.CurrentClaimBeadID = "w1" }), work: []beads.Bead{pin("w1", "s1")}, ready: []bool{false}, wantEligible: true},
		{name: "an open bead the seat was woken for is started", row: parkedRow("s1", func(i *session.Info) { i.CurrentlyProcessingBeadID = "w1" }), work: []beads.Bead{pin("w1", "s1")}, ready: []bool{false}, wantEligible: true},
		{name: "an open bead the work_started cell names is started", row: parkedRow("s1", func(i *session.Info) {
			i.WorkStartedRaw = `{"v":1,"claims":[{"store":"","bead":"w1","gen":2,"at":"2026-10-18T11:00:00Z","via":"claim"}]}`
		}), work: []beads.Bead{pin("w1", "s1")}, ready: []bool{false}, wantEligible: true},
		{name: "an unreadable work_started cell reads every claim started", row: parkedRow("s1", func(i *session.Info) { i.WorkStartedRaw = "{not json" }), work: []beads.Bead{pin("w1", "s1")}, ready: []bool{false}, wantEligible: true},
		{name: "another seat's pin is not this seat's work", row: parkedRow("s1"), work: []beads.Bead{pin("w1", "s2")}, ready: []bool{false}, wantEligible: true},
		{name: "a workless seat is not parked", row: parkedRow("s1"), ready: []bool{}, wantEligible: true},
		{name: "a runtime that died holding a pin (runtime-missing) is not a voluntary sleep", row: parkedRow("s1", func(i *session.Info) { i.SleepReason = string(session.SleepReasonRuntimeMissing) }), work: []beads.Bead{pin("w1", "s1")}, ready: []bool{false}},
		{name: "an idle-timeout stop is not the seat's own: only sleep_reason idle parks (D4)", row: parkedRow("s1", func(i *session.Info) { i.SleepReason = string(session.SleepReasonIdleTimeout) }), work: []beads.Bead{pin("w1", "s1")}, ready: []bool{false}},
		{name: "a killed seat takes B1", row: parkedRow("s1", func(i *session.Info) { i.SleepReason = string(session.SleepReasonKilled) }), work: []beads.Bead{pin("w1", "s1")}, ready: []bool{false}},
		{name: "a terminal provider error is not a voluntary sleep", row: parkedRow("s1", func(i *session.Info) { i.SleepReason = string(session.SleepReasonProviderTerminalError) }), work: []beads.Bead{pin("w1", "s1")}, ready: []bool{false}},
		{name: "a quarantined seat is not parked", row: parkedRow("s1", func(i *session.Info) {
			i.SleepReason, i.QuarantinedUntil = string(session.SleepReasonQuarantine), parkRFC(time.Hour)
		}), work: []beads.Bead{pin("w1", "s1")}, ready: []bool{false}},
		{name: "an operator-held seat is not parked", row: parkedRow("s1", func(i *session.Info) { i.SleepIntent = string(session.SleepReasonUserHold) }), work: []beads.Bead{pin("w1", "s1")}, ready: []bool{false}},
		{name: "a drained seat holds nothing to park", row: parkedRow("s1", func(i *session.Info) { i.MetadataState = "drained" }), work: []beads.Bead{pin("w1", "s1")}, ready: []bool{false}},
		{name: "an awake seat is not parked", row: parkedRow("s1", func(i *session.Info) { i.MetadataState, i.State = "awake", session.StateActive }), work: []beads.Bead{pin("w1", "s1")}, ready: []bool{false}},
		{name: "a closed seat is not parked", row: parkedRow("s1", func(i *session.Info) { i.Closed = true }), work: []beads.Bead{pin("w1", "s1")}, ready: []bool{false}},
		{name: "a row wake_mode=fresh has no context to keep", row: parkedRow("s1", func(i *session.Info) { i.WakeMode = "fresh" }), work: []beads.Bead{pin("w1", "s1")}, ready: []bool{false}},
		{name: "a template wake_mode=fresh has no context to keep", cfg: parkCity(config.Agent{Name: "worker", StartCommand: "true", WakeMode: "fresh"}), row: parkedRow("s1"), work: []beads.Bead{pin("w1", "s1")}, ready: []bool{false}},
		{name: "past the bound the seat is not parked", row: parkedRow("s1", func(i *session.Info) { i.SleptAt = parkRFC(-parkedWorkMaxHold - time.Minute) }), work: []beads.Bead{pin("w1", "s1")}, ready: []bool{false}},
		{name: "an unreadable slept_at has no bound to hold to", row: parkedRow("s1", func(i *session.Info) { i.SleptAt = "soon" }), work: []beads.Bead{pin("w1", "s1")}, ready: []bool{false}},
		{name: "a slept_at far ahead of the clock has no bound to hold to", row: parkedRow("s1", func(i *session.Info) { i.SleptAt = parkRFC(48 * time.Hour) }), work: []beads.Bead{pin("w1", "s1")}, ready: []bool{false}},
		{name: "a slept_at the skew ahead of the clock is not parked", row: parkedRow("s1", func(i *session.Info) { i.SleptAt = parkRFC(parkedSleptAtFutureSkew) }), work: []beads.Bead{pin("w1", "s1")}, ready: []bool{false}},
		{name: "a slept_at just inside the skew still parks", row: parkedRow("s1", func(i *session.Info) { i.SleptAt = parkRFC(parkedSleptAtFutureSkew - time.Second) }), work: []beads.Bead{pin("w1", "s1")}, ready: []bool{false}, wantEligible: true, wantParked: true},
		{name: "a seat a pass already stranded parks again while parked-shaped", row: parkedRow("s1", func(i *session.Info) { i.StrandedEventEmittedAt = parkRFC(-time.Minute) }), work: []beads.Bead{pin("w1", "s1")}, ready: []bool{false}, wantEligible: true, wantParked: true},
		{name: "a deferred pin is no pool demand, so it does not park", row: parkedRow("s1"), work: []beads.Bead{func() beads.Bead {
			b := pin("w1", "s1")
			later := parkNow.Add(time.Hour)
			b.DeferUntil = &later
			return b
		}()}, ready: []bool{false}, wantEligible: true},
		{name: "a named seat is not a pool seat", row: parkedRow("s1", func(i *session.Info) { i.ConfiguredNamedSession, i.PoolManaged, i.PoolSlot = true, false, "" }), work: []beads.Bead{pin("w1", "s1")}, ready: []bool{false}},
		{name: "a row whose template is not configured is not parked", row: parkedRow("s1", func(i *session.Info) { i.Template = "gone" }), work: []beads.Bead{pin("w1", "s1")}, ready: []bool{false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			if cfg == nil {
				cfg = parkCity()
			}
			got := parkRows(cfg, []session.Info{tc.row}, tc.work, tc.ready, parkNow)[tc.row.ID]
			if got.Eligible != tc.wantEligible || got.Parked != tc.wantParked {
				t.Fatalf("verdict %+v, want eligible=%v parked=%v", got, tc.wantEligible, tc.wantParked)
			}
			if !got.Parked {
				return
			}
			slept, err := time.Parse(time.RFC3339, tc.row.SleptAt)
			if err != nil {
				t.Fatalf("parked on an unreadable slept_at %q: %v", tc.row.SleptAt, err)
			}
			if !got.Until.Equal(slept.Add(parkedWorkMaxHold)) {
				t.Fatalf("until %v, want slept_at %s plus %v", got.Until, tc.row.SleptAt, parkedWorkMaxHold)
			}
		})
	}
}

// Readiness unknown (no readiness verdict read this pass) parks nothing: a
// pass that cannot tell blocked from ready keeps today's path.
func TestParkSeatsNeedsAReadinessVerdict(t *testing.T) {
	cfg := parkCity()
	rows := []session.Info{parkedRow("s1")}
	work := []beads.Bead{pin("w1", "s1")}
	if got := parkRows(cfg, rows, work, nil, parkNow); len(got) != 0 {
		t.Fatalf("verdicts %+v, want none without readiness", got)
	}
	if got := parkRows(cfg, rows, work, []bool{false}, time.Time{}); len(got) != 0 {
		t.Fatalf("verdicts %+v, want none without a decision time", got)
	}
	if got := parkRows(nil, rows, work, []bool{false}, parkNow); len(got) != 0 {
		t.Fatalf("verdicts %+v, want none without a config", got)
	}
}

// A slot-bounded pool parks only while it has a free slot: when its open
// rows reach max_active_sessions, its oldest parked seats fall back to the
// stranded path until one slot is free again (owner decision D2b).
func TestParkSeatsSlotHeadroom(t *testing.T) {
	bounded := func(n int) *config.City {
		return parkCity(config.Agent{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(n)})
	}
	sleptAgo := func(d time.Duration) func(*session.Info) {
		return func(i *session.Info) { i.SleptAt = parkRFC(-d) }
	}
	awake := func(i *session.Info) {
		i.MetadataState, i.State, i.SleepReason, i.SleptAt = "awake", session.StateActive, "", ""
	}
	rows := func() []session.Info {
		return []session.Info{
			parkedRow("old", sleptAgo(3*time.Hour)),
			parkedRow("mid", sleptAgo(2*time.Hour)),
			parkedRow("new", sleptAgo(time.Hour)),
		}
	}
	work := []beads.Bead{pin("w-old", "old"), pin("w-mid", "mid"), pin("w-new", "new")}
	ready := []bool{false, false, false}
	parkedSet := func(v parkedSeats) map[string]bool {
		out := map[string]bool{}
		for id, p := range v {
			if p.Parked {
				out[id] = true
			}
		}
		return out
	}
	t.Run("headroom parks every seat", func(t *testing.T) {
		got := parkedSet(parkRows(bounded(4), rows(), work, ready, parkNow))
		if len(got) != 3 {
			t.Fatalf("parked %v, want all three with a free slot", got)
		}
	})
	t.Run("a full pool unparks its oldest seat", func(t *testing.T) {
		v := parkRows(bounded(3), rows(), work, ready, parkNow)
		if got := parkedSet(v); len(got) != 2 || got["old"] {
			t.Fatalf("parked %v, want mid and new: the oldest falls back", got)
		}
		if v["old"].Eligible {
			t.Fatalf("old %+v, want ineligible: it takes the stranded path", v["old"])
		}
	})
	t.Run("an over-full pool unparks until one slot is free", func(t *testing.T) {
		got := parkedSet(parkRows(bounded(2), rows(), work, ready, parkNow))
		if len(got) != 1 || !got["new"] {
			t.Fatalf("parked %v, want only the youngest", got)
		}
	})
	t.Run("an awake seat holds a slot too", func(t *testing.T) {
		all := append(rows(), parkedRow("busy", awake))
		got := parkedSet(parkRows(bounded(4), all, work, ready, parkNow))
		if len(got) != 2 || got["old"] {
			t.Fatalf("parked %v, want mid and new", got)
		}
	})
	t.Run("a seat already on the stranded path frees the slot first", func(t *testing.T) {
		all := rows()
		all[1].StrandedEventEmittedAt = parkRFC(-time.Minute) // mid, stamped: its repair frees a slot
		got := parkedSet(parkRows(bounded(3), all, work, ready, parkNow))
		if len(got) != 2 || !got["old"] || !got["new"] {
			t.Fatalf("parked %v, want old and new: the stamped seat already frees one slot", got)
		}
	})
	t.Run("with headroom back, a seat a pass stranded parks again", func(t *testing.T) {
		all := rows()
		all[1].StrandedEventEmittedAt = parkRFC(-time.Minute)
		got := parkedSet(parkRows(bounded(4), all, work, ready, parkNow))
		if len(got) != 3 {
			t.Fatalf("parked %v, want all three once a slot is free again", got)
		}
	})
	t.Run("a workless seat the pool-slot close ends this pass holds no slot", func(t *testing.T) {
		all := append(rows(), parkedRow("closing"))
		got := parkedSet(parkRows(bounded(4), all, work, ready, parkNow))
		if len(got) != 3 {
			t.Fatalf("parked %v, want all three: the workless idle seat is closing", got)
		}
	})
	t.Run("a workless seat that is awake holds its slot", func(t *testing.T) {
		all := append(rows(), parkedRow("idle-awake", awake))
		got := parkedSet(parkRows(bounded(4), all, work, ready, parkNow))
		if len(got) != 2 || got["old"] {
			t.Fatalf("parked %v, want mid and new", got)
		}
	})
	t.Run("an in-flight create's stand-in holds a slot", func(t *testing.T) {
		standIn := parkedRow("in-flight", func(i *session.Info) {
			i.MetadataState, i.State, i.SleepReason, i.SleptAt, i.PendingCreateClaim = "start-pending", session.StateStartPending, "", "", true
		})
		got := parkedSet(parkSeats(bounded(4), [][]session.Info{rows(), {standIn}}, work, ready, parkNow))
		if len(got) != 2 || got["old"] {
			t.Fatalf("parked %v, want mid and new: the stand-in fills the fourth slot", got)
		}
	})
	t.Run("a failed create holds its name, not a slot", func(t *testing.T) {
		all := append(rows(), parkedRow("failed", func(i *session.Info) { i.MetadataState, i.SleepReason = "failed-create", "" }))
		got := parkedSet(parkRows(bounded(4), all, work, ready, parkNow))
		if len(got) != 3 {
			t.Fatalf("parked %v, want all three: a failed create holds no slot", got)
		}
	})
	t.Run("an unbounded pool never unparks for slots", func(t *testing.T) {
		got := parkedSet(parkRows(parkCity(), rows(), work, ready, parkNow))
		if len(got) != 3 {
			t.Fatalf("parked %v, want all three", got)
		}
	})
	t.Run("a namepool bounds its slots", func(t *testing.T) {
		cfg := parkCity(config.Agent{Name: "worker", StartCommand: "true", NamepoolNames: []string{"a", "b", "c"}})
		got := parkedSet(parkRows(cfg, rows(), work, ready, parkNow))
		if len(got) != 2 || got["old"] {
			t.Fatalf("parked %v, want mid and new under a three-name pool", got)
		}
	})
	t.Run("a pool with one slot never parks", func(t *testing.T) {
		got := parkedSet(parkRows(bounded(1), rows()[2:], work, ready, parkNow))
		if len(got) != 0 {
			t.Fatalf("parked %v, want none: the only slot must stay free", got)
		}
	})
	t.Run("ties fall back by bead ID", func(t *testing.T) {
		all := []session.Info{parkedRow("b", sleptAgo(time.Hour)), parkedRow("a", sleptAgo(time.Hour))}
		got := parkedSet(parkRows(bounded(2), all, []beads.Bead{pin("wa", "a"), pin("wb", "b")}, []bool{false, false}, parkNow))
		if len(got) != 1 || !got["b"] {
			t.Fatalf("parked %v, want b: a falls back first on a slept_at tie", got)
		}
	})
	t.Run("pools count their own rows", func(t *testing.T) {
		cfg := parkCity(
			config.Agent{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(2)},
			config.Agent{Name: "other", StartCommand: "true", MaxActiveSessions: intPtr(1)},
		)
		all := []session.Info{rows()[2], parkedRow("o1", func(i *session.Info) { i.Template = "other"; i.SleptAt = parkRFC(-5 * time.Hour) })}
		w := []beads.Bead{pin("w-new", "new"), pin("w-o1", "o1")}
		got := parkedSet(parkRows(cfg, all, w, []bool{false, false}, parkNow))
		if len(got) != 1 || !got["new"] {
			t.Fatalf("parked %v, want new only (other has one slot)", got)
		}
	})
}

// A parked seat's wake continues its conversation (Continues): the verdict
// holds while the seat parks and once its work turns ready, for a seat its
// pool's slots unpark too, and never for a seat that is not parkable or
// holds work that is not an open, unstarted, undeferred pin.
func TestParkSeatsContinuesThroughTheWake(t *testing.T) {
	inProgress := func(b beads.Bead) beads.Bead { b.Status = "in_progress"; return b }
	deferred := func(b beads.Bead) beads.Bead {
		later := parkNow.Add(time.Hour)
		b.DeferUntil = &later
		return b
	}
	for _, tc := range []struct {
		name          string
		row           session.Info
		work          []beads.Bead
		ready         []bool
		wantParked    bool
		wantContinues bool
	}{
		{name: "a blocked pin parks and continues", row: parkedRow("s1"), work: []beads.Bead{pin("w1", "s1")}, ready: []bool{false}, wantParked: true, wantContinues: true},
		{name: "a ready pin wakes the seat, which continues", row: parkedRow("s1"), work: []beads.Bead{pin("w1", "s1")}, ready: []bool{true}, wantContinues: true},
		{name: "a ready pin beside a blocked one continues", row: parkedRow("s1"), work: []beads.Bead{pin("w1", "s1"), pin("w2", "s1")}, ready: []bool{true, false}, wantContinues: true},
		{name: "an in-progress claim does not", row: parkedRow("s1"), work: []beads.Bead{pin("w1", "s1"), inProgress(pin("w2", "s1"))}, ready: []bool{true, false}},
		{name: "a started pin does not", row: parkedRow("s1", func(i *session.Info) { i.CurrentClaimBeadID = "w1" }), work: []beads.Bead{pin("w1", "s1")}, ready: []bool{true}},
		{name: "a deferred pin does not", row: parkedRow("s1"), work: []beads.Bead{deferred(pin("w1", "s1"))}, ready: []bool{false}},
		{name: "a workless seat does not", row: parkedRow("s1"), ready: []bool{}},
		{name: "a seat whose runtime died does not", row: parkedRow("s1", func(i *session.Info) { i.SleepReason = string(session.SleepReasonRuntimeMissing) }), work: []beads.Bead{pin("w1", "s1")}, ready: []bool{true}},
		{name: "a fresh-wake seat does not", row: parkedRow("s1", func(i *session.Info) { i.WakeMode = "fresh" }), work: []beads.Bead{pin("w1", "s1")}, ready: []bool{true}},
		{name: "a seat past its bound does not", row: parkedRow("s1", func(i *session.Info) { i.SleptAt = parkRFC(-parkedWorkMaxHold - time.Minute) }), work: []beads.Bead{pin("w1", "s1")}, ready: []bool{true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parkRows(parkCity(), []session.Info{tc.row}, tc.work, tc.ready, parkNow)[tc.row.ID]
			if got.Parked != tc.wantParked || got.Continues != tc.wantContinues {
				t.Fatalf("verdict %+v, want parked=%v continues=%v", got, tc.wantParked, tc.wantContinues)
			}
		})
	}
	t.Run("a seat a full pool unparks still continues", func(t *testing.T) {
		cfg := parkCity(config.Agent{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(1)})
		got := parkRows(cfg, []session.Info{parkedRow("s1")}, []beads.Bead{pin("w1", "s1")}, []bool{false}, parkNow)["s1"]
		if got.Eligible || got.Parked || !got.Continues {
			t.Fatalf("verdict %+v, want ineligible and unparked (its pool's only slot must stay free), its wake still continuing", got)
		}
	})
}

// The verdict is a pure function of persisted fields, so a controller restart
// decides it again unchanged: no marker, no memory.
func TestParkSeatsIsDeterministic(t *testing.T) {
	cfg := parkCity(config.Agent{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(3)})
	var rows []session.Info
	var work []beads.Bead
	var ready []bool
	for i := range 5 {
		id := fmt.Sprintf("s%d", i)
		rows = append(rows, parkedRow(id, func(info *session.Info) { info.SleptAt = parkRFC(-time.Duration(i+1) * time.Hour) }))
		work = append(work, pin("w"+id, id))
		ready = append(ready, false)
	}
	first := parkRows(cfg, rows, work, ready, parkNow)
	for range 10 {
		again := parkRows(cfg, rows, work, ready, parkNow)
		for id, v := range first {
			if again[id] != v {
				t.Fatalf("%s: %+v then %+v", id, v, again[id])
			}
		}
	}
}

// The recorded work dir a parked resume keeps over the derived one: the
// canonical key, else the legacy one, and none when the request is not a
// parked continuation, nothing is recorded, or the dirs agree.
func TestParkedWorkDirKept(t *testing.T) {
	info := session.Info{ID: "s1", WorkDirCanonical: "/rec", WorkDir: "/rec-legacy"}
	req := SessionRequest{Tier: "resume", SessionBeadID: "s1", WorkBeadID: "w1", ParkedContinuation: true}
	for _, tc := range []struct {
		name    string
		info    func(*session.Info)
		req     func(*SessionRequest)
		derived string
		want    string
	}{
		{name: "the canonical key", derived: "/derived", want: "/rec"},
		{name: "the legacy key alone", info: func(i *session.Info) { i.WorkDirCanonical = "" }, derived: "/derived", want: "/rec-legacy"},
		{name: "the derived dir itself", derived: "/rec"},
		{name: "no derived dir", derived: ""},
		{name: "nothing recorded", info: func(i *session.Info) { i.WorkDirCanonical, i.WorkDir = "", "" }, derived: "/derived"},
		{name: "not a parked continuation", req: func(r *SessionRequest) { r.ParkedContinuation = false }, derived: "/derived"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i, r := info, req
			if tc.info != nil {
				tc.info(&i)
			}
			if tc.req != nil {
				tc.req(&r)
			}
			if got := parkedWorkDirKept(i, r, tc.derived); got != tc.want {
				t.Fatalf("kept %q, want %q", got, tc.want)
			}
		})
	}
}

// A kept dir that cannot host the resume rebinds the derived one: both work
// dir keys move to it (only those that differ), the rest of the patch stays,
// and the caller's patch is not written.
func TestWithRederivedWorkDir(t *testing.T) {
	patch := session.MetadataPatch{beadmeta.TriggerBeadIDMetadataKey: "w1"}
	got := withRederivedWorkDir(patch, session.Info{WorkDirCanonical: "/rec", WorkDir: "/rec"}, "/derived")
	want := session.MetadataPatch{beadmeta.TriggerBeadIDMetadataKey: "w1", beadmeta.WorkDirMetadataKey: "/derived", beadmeta.LegacyWorkDirMetadataKey: "/derived"}
	if !maps.Equal(got, want) {
		t.Fatalf("patch %v, want %v", got, want)
	}
	if len(patch) != 1 {
		t.Fatalf("the caller's patch was written: %v", patch)
	}
	if got := withRederivedWorkDir(nil, session.Info{WorkDirCanonical: "/rec", WorkDir: "/derived"}, "/derived"); !maps.Equal(got, session.MetadataPatch{beadmeta.WorkDirMetadataKey: "/derived"}) {
		t.Fatalf("patch %v, want only the canonical key moved", got)
	}
}

// A parked resume's recorded work dir hosts the resume only while it is
// still a directory in the rig its derived dir is in, or, when no rig owns
// the derived dir, in the seat's own rig. A rig owns a dir its checkout holds
// or its worktree's .beads redirect names (rigOwningDir).
func TestParkedWorkDirUsable(t *testing.T) {
	city := t.TempDir()
	rigA, rigB := filepath.Join(city, "rigs", "a"), filepath.Join(t.TempDir(), "b")
	mkdir := func(dir string) string {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	mkdir(filepath.Join(rigA, ".beads"))
	mkdir(filepath.Join(rigB, ".beads"))
	// worktreeOf is a worktree under the city whose .beads redirect names
	// store, as a rig worktree's does.
	worktreeOf := func(name, store string) string {
		t.Helper()
		dir := mkdir(filepath.Join(city, ".gc", "worktrees", name, ".beads"))
		if err := os.WriteFile(filepath.Join(dir, "redirect"), []byte(store+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return filepath.Dir(dir)
	}
	plain := mkdir(filepath.Join(city, ".gc", "worktrees", "plain"))
	aWorktree := worktreeOf("a-wt", filepath.Join(rigA, ".beads"))
	bWorktree := worktreeOf("b-wt", filepath.Join(rigB, ".beads"))
	stray := worktreeOf("stray", filepath.Join(t.TempDir(), ".beads"))
	file := filepath.Join(city, "a-file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "a", Path: rigA}, {Name: "b", Path: rigB}}}
	citySeat, rigSeat := &config.Agent{Name: "worker"}, &config.Agent{Name: "worker", Dir: "a"}
	unbuilt := filepath.Join(city, ".gc", "worktrees", "worker-1") // a derived dir no launch made yet
	for _, tc := range []struct {
		name              string
		agent             *config.Agent
		recorded, derived string
		usable            bool
	}{
		{name: "a city dir for a city seat", agent: citySeat, recorded: plain, derived: unbuilt, usable: true},
		{name: "a city-relative dir", agent: citySeat, recorded: filepath.Join(".gc", "worktrees", "plain"), derived: unbuilt, usable: true},
		{name: "a worktree of the derived dir's rig", agent: citySeat, recorded: aWorktree, derived: rigA, usable: true},
		{name: "a worktree of the seat's rig, no rig owning the derived dir", agent: rigSeat, recorded: aWorktree, derived: unbuilt, usable: true},
		{name: "the seat's rig checkout", agent: rigSeat, recorded: mkdir(filepath.Join(rigA, "sub")), derived: unbuilt, usable: true},
		{name: "another rig's checkout", agent: citySeat, recorded: mkdir(filepath.Join(rigB, "sub")), derived: unbuilt},
		{name: "another rig's worktree", agent: rigSeat, recorded: bWorktree, derived: rigA},
		{name: "a city dir for a rig seat", agent: rigSeat, recorded: plain, derived: unbuilt},
		{name: "a worktree redirected outside the city's rigs", agent: citySeat, recorded: stray, derived: unbuilt},
		{name: "a dir that is gone", agent: citySeat, recorded: filepath.Join(city, "gone"), derived: unbuilt},
		{name: "a file", agent: citySeat, recorded: file, derived: unbuilt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := parkedWorkDirUsable(cfg, city, tc.agent, tc.recorded, tc.derived)
			if (err == nil) != tc.usable {
				t.Fatalf("usable error %v, want usable=%v", err, tc.usable)
			}
		})
	}
}
