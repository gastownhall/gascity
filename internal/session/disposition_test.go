package session

import (
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

var updateDispositionGolden = flag.Bool("update-golden", false, "rewrite the disposition table's golden from Apply")

var dispT0 = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

// dispSeeds are the dispositions the table golden starts from, each decoded
// from a row: each operator value, each sub-state on its own, the
// combinations a cell treats apart, legacy's shapes (a bare wait_hold, a
// wait with an idle stop) and unreadable timers.
func dispSeeds() []struct {
	name string
	d    Disposition
} {
	at := func(d time.Duration) string { return dispT0.Add(d).Format(time.RFC3339) }
	rows := []struct {
		name string
		meta map[string]string
	}{
		{"none", map[string]string{}},
		{"none+wait", map[string]string{"wait_hold": "true", "sleep_intent": "wait-hold"}},
		{"none+bare-wait", map[string]string{"wait_hold": "true"}},
		{"none+bare-wait+heartbeat", map[string]string{"wait_hold": "true", "held_until": at(time.Hour)}},
		{"none+heartbeat", map[string]string{"held_until": at(time.Hour)}},
		{"none+heartbeat-expired", map[string]string{"held_until": at(-time.Minute)}},
		{"none+heartbeat-now", map[string]string{"held_until": at(0)}},
		{"none+idle-stop", map[string]string{"sleep_intent": "idle-stop-pending"}},
		{"none+wait+idle-stop(legacy)", map[string]string{"wait_hold": "true", "sleep_intent": "idle-stop-pending"}},
		{"none+quarantine", map[string]string{"quarantined_until": at(time.Hour)}},
		{"none+quarantine-expired", map[string]string{"quarantined_until": at(-time.Minute)}},
		{"none+unknown-timer", map[string]string{"held_until": "soon"}},
		{"none+unknown-quarantine", map[string]string{"quarantined_until": "later"}},
		{"none+unknown-intent", map[string]string{"sleep_intent": "someday"}},
		{"none+unreadable-stamp", map[string]string{"suspended_at": "garbage"}},
		{"suspended", map[string]string{"sleep_intent": "user-hold", "held_until": at(IndefiniteHoldDuration), "suspended_at": at(0)}},
		{"suspended+wait", map[string]string{"sleep_intent": "user-hold", "held_until": at(IndefiniteHoldDuration), "wait_hold": "true"}},
		{"suspended-finite-expired", map[string]string{"sleep_intent": "user-hold", "held_until": at(-time.Minute), "suspended_at": at(-time.Hour)}},
		{"suspended-no-until", map[string]string{"sleep_intent": "user-hold"}},
		{"suspended-unknown-until", map[string]string{"sleep_intent": "user-hold", "held_until": "soon"}},
		{"suspended-soft", map[string]string{"state": "suspended", "suspended_at": at(0)}},
		{"suspended+unreadable-stamp", map[string]string{"sleep_intent": "user-hold", "held_until": at(IndefiniteHoldDuration), "suspended_at": "garbage"}},
		{"suspended-soft+unreadable", map[string]string{"state": "suspended", "suspended_at": "garbage"}},
	}
	out := make([]struct {
		name string
		d    Disposition
	}, len(rows))
	for i, r := range rows {
		out[i].name, out[i].d = r.name, DecodeDisposition(r.meta)
	}
	return out
}

// dispEvents are the table's events, each by the actors whose cells differ.
func dispEvents() []struct {
	by ActorKind
	ev DispEvent
} {
	later := dispT0.Add(2 * time.Hour)
	var out []struct {
		by ActorKind
		ev DispEvent
	}
	add := func(ev DispEvent, kinds ...ActorKind) {
		for _, by := range kinds {
			out = append(out, struct {
				by ActorKind
				ev DispEvent
			}{by, ev})
		}
	}
	add(DispEvent{Kind: EvSuspend}, ActorOperator, ActorController)
	add(DispEvent{Kind: EvSuspend, Until: later}, ActorOperator)
	add(DispEvent{Kind: EvSuspendSoft}, ActorController, ActorSweep)
	add(DispEvent{Kind: EvKill}, ActorOperator, ActorController)
	add(DispEvent{Kind: EvResume}, ActorOperator, ActorAgent, ActorBackground, ActorController)
	add(DispEvent{Kind: EvResume, Running: true}, ActorAgent, ActorBackground)
	add(DispEvent{Kind: EvRestart}, ActorAgent)
	add(DispEvent{Kind: EvHeartbeat, Until: later}, ActorAgent)
	add(DispEvent{Kind: EvHeartbeat, Until: dispT0.Add(30 * time.Minute)}, ActorAgent)
	add(DispEvent{Kind: EvWaitBegin}, ActorAgent)
	add(DispEvent{Kind: EvWaitEnd}, ActorController)
	add(DispEvent{Kind: EvSleep}, ActorController)
	add(DispEvent{Kind: EvPreWake}, ActorController)
	add(DispEvent{Kind: EvIdleStopBegin}, ActorController)
	add(DispEvent{Kind: EvIdleStopEnd}, ActorController)
	add(DispEvent{Kind: EvQuarantine, Until: later}, ActorController)
	add(DispEvent{Kind: EvTimerTick}, ActorController)
	add(DispEvent{Kind: EvWake}, ActorOperator, ActorController)
	add(DispEvent{Kind: EvRetire}, ActorController)
	return out
}

var actorNames = map[ActorKind]string{
	ActorOperator: "operator", ActorAgent: "agent", ActorBackground: "background",
	ActorController: "controller", ActorSweep: "sweep",
}

// render is d as one golden token: its encoded keys, sorted, empty ones left
// out, with times relative to dispT0.
func (d Disposition) render() string {
	enc := d.Encode().Fold()
	if d.Operator == OperatorSuspendedSoft {
		enc["state"] = string(StateSuspended)
	}
	var parts []string
	for k, v := range enc {
		if v == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			switch delta := t.Sub(dispT0); {
			case delta > 50*365*24*time.Hour:
				v = "∞"
			default:
				v = "t0" + fmt.Sprintf("%+v", delta)
			}
		}
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	if len(parts) == 0 {
		return "(none)"
	}
	return strings.Join(parts, " ")
}

// TestDispositionTableGolden renders every cell: each event, by each actor
// whose cell differs, from each seed (an operator resume also while the row
// drains). A cell that changes shows in review as a golden diff.
func TestDispositionTableGolden(t *testing.T) {
	var b strings.Builder
	b.WriteString("# The disposition table (ARCH-RESTRUCTURE-2 R7): event by actor, from seed -> result.\n")
	b.WriteString("# Regenerate: go test ./internal/session -run TestDispositionTableGolden -update-golden\n")
	for _, e := range dispEvents() {
		fmt.Fprintf(&b, "\n%s by %s", e.ev.Kind, actorNames[e.by])
		if !e.ev.Until.IsZero() {
			fmt.Fprintf(&b, " until t0%+v", e.ev.Until.Sub(dispT0))
		}
		if e.ev.Running {
			b.WriteString(" (runtime running)")
		}
		b.WriteString("\n")
		lives := []State{StateAsleep}
		if e.ev.Kind == EvResume && e.by == ActorOperator {
			lives = append(lives, StateDraining)
		}
		for _, s := range dispSeeds() {
			for _, life := range lives {
				got, err := s.d.Apply(e.by, e.ev, life, dispT0)
				result := got.render()
				var refused *ErrDispositionRefused
				switch {
				case errors.As(err, &refused):
					result = "REFUSED: " + refused.Why
				case err != nil:
					t.Fatalf("%s by %d from %s: %v", e.ev.Kind, e.by, s.name, err)
				}
				fmt.Fprintf(&b, "  %-29s life=%-8s -> %s\n", s.name, life, result)
			}
		}
	}
	path := filepath.Join("testdata", "disposition_table.golden")
	if *updateDispositionGolden {
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the golden (regenerate with -update-golden): %v", err)
	}
	if string(want) != b.String() {
		t.Fatalf("the disposition table moved from %s; review the cells and regenerate with -update-golden:\n%s", path, firstDiff(string(want), b.String()))
	}
}

func firstDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			return fmt.Sprintf("line %d:\n  want %q\n  got  %q", i+1, wl, gl)
		}
	}
	return ""
}

// TestDispositionEveryEventHasACell: Apply knows every event kind, and
// refuses an unknown one.
func TestDispositionEveryEventHasACell(t *testing.T) {
	for k := EvSuspend; k <= EvRetire; k++ {
		if strings.HasPrefix(k.String(), "DispEventKind(") {
			t.Errorf("event %d has no name", k)
		}
		_, err := Disposition{}.Apply(ActorOperator, DispEvent{Kind: k, Until: dispT0.Add(time.Hour)}, StateAsleep, dispT0)
		var refused *ErrDispositionRefused
		if errors.As(err, &refused) && refused.Why == "no such event" {
			t.Errorf("event %s has no cell", k)
		}
	}
	if _, err := (Disposition{}).Apply(ActorOperator, DispEvent{Kind: EvRetire + 1}, StateAsleep, dispT0); err == nil {
		t.Error("an unknown event was applied")
	}
}

// encodedRow is the row Encode writes d onto: its five keys, and the state an
// auto-suspended row carries.
func encodedRow(d Disposition) map[string]string {
	meta := map[string]string(d.Encode().Fold())
	meta["state"] = string(StateAsleep)
	if d.Operator == OperatorSuspendedSoft {
		meta["state"] = string(StateSuspended)
	}
	return meta
}

// TestDispositionRoundTrip: every disposition the table reaches from the
// seeds, by random event sequences, decodes from its encoding unchanged.
func TestDispositionRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	events := dispEvents()
	for _, s := range dispSeeds() {
		for walk := 0; walk < 200; walk++ {
			d, now := s.d, dispT0
			for step := 0; step < 8; step++ {
				e := events[rng.Intn(len(events))]
				now = now.Add(time.Duration(rng.Intn(180)) * time.Minute)
				if next, err := d.Apply(e.by, e.ev, StateAsleep, now); err == nil {
					d = next
				}
				if got := DecodeDisposition(encodedRow(d)); !got.sameAs(d) {
					t.Fatalf("seed %s, walk %d: Decode(Encode(%+v)) = %+v", s.name, walk, d, got)
				}
			}
		}
	}
}

// sameAs reports whether d and o mean the same disposition, whatever text
// each was decoded from.
func (d Disposition) sameAs(o Disposition) bool {
	d.raw, o.raw = [len(dispositionKeys)]string{}, [len(dispositionKeys)]string{}
	return d.canon() == o.canon() && d.Operator == o.Operator && d.Reason == o.Reason && d.Unknown == o.Unknown &&
		d.Wait == o.Wait && d.WaitIntent == o.WaitIntent && d.IdleStop == o.IdleStop
}

// TestDispositionEncodeWritesTheWholeClass: every encoding names all five
// keys, so a write of it replaces the class whole.
func TestDispositionEncodeWritesTheWholeClass(t *testing.T) {
	for _, s := range dispSeeds() {
		enc := s.d.Encode().Fold()
		for _, k := range []string{"held_until", "sleep_intent", "wait_hold", "quarantined_until", "suspended_at"} {
			if _, ok := enc[k]; !ok {
				t.Errorf("%s: Encode leaves out %s", s.name, k)
			}
		}
		if len(enc) != 5 {
			t.Errorf("%s: Encode writes %d keys, want the five", s.name, len(enc))
		}
	}
}

// dispositionGrid walks every combination of the class keys' values with
// the state and sleep_reason they are read beside (the max review's 15,000
// rows, plus a legacy wait_hold reason): live, expired, exactly-now and unreadable timers, every intent and
// an unknown one, a bare wait_hold, and an unreadable suspended_at.
func dispositionGrid(fn func(meta map[string]string)) int {
	future, past, now := dispT0.Add(time.Hour).Format(time.RFC3339), dispT0.Add(-time.Hour).Format(time.RFC3339), dispT0.Format(time.RFC3339)
	values := map[string][]string{
		"state":             {"", string(StateAsleep), string(StateSuspended), string(StateActive), string(StateDraining)},
		"sleep_intent":      {"", string(SleepReasonUserHold), string(SleepReasonWaitHold), string(sleepIntentIdleStopPending), "other"},
		"wait_hold":         {"", "true", "w"},
		"held_until":        {"", future, past, "soon", now},
		"quarantined_until": {"", future, past, "soon"},
		"sleep_reason":      {"", string(SleepReasonKilled), string(SleepReasonIdle), string(SleepReasonUserHold), string(SleepReasonCityStop)},
		"suspended_at":      {"", past, "garbage"},
	}
	keys := []string{"state", "sleep_intent", "wait_hold", "held_until", "quarantined_until", "sleep_reason", "suspended_at"}
	n := 0
	var walk func(i int, meta map[string]string)
	walk = func(i int, meta map[string]string) {
		if i == len(keys) {
			n++
			row := make(map[string]string, len(meta))
			for k, v := range meta {
				row[k] = v
			}
			fn(row)
			return
		}
		for _, v := range values[keys[i]] {
			meta[keys[i]] = v
			walk(i+1, meta)
		}
	}
	walk(0, map[string]string{})
	return n
}

// TestDispositionHeartbeatHeldMatchesSESS602: over the grid, HeartbeatHeld is
// legacy SESS-602's: a live held_until under an empty sleep_intent, within a
// heartbeat's horizon (v5.9i; the grid's holds all lie inside it).
func TestDispositionHeartbeatHeldMatchesSESS602(t *testing.T) {
	n := dispositionGrid(func(meta map[string]string) {
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(meta["held_until"]))
		legacy := err == nil && at.After(dispT0) && strings.TrimSpace(meta["sleep_intent"]) == ""
		if got := DecodeDisposition(meta).HeartbeatHeld(dispT0); got != legacy {
			t.Errorf("%v: HeartbeatHeld = %v, SESS-602 %v", meta, got, legacy)
		}
	})
	if n != 5*5*3*5*4*5*3 {
		t.Fatalf("walked %d rows", n)
	}
}

// TestDispositionEncodeIsLossless: over the grid and legacy's other shapes,
// Encode(Decode(row)) is the row on every class key, character for
// character: a write of an unmoved disposition changes nothing.
func TestDispositionEncodeIsLossless(t *testing.T) {
	check := func(meta map[string]string) {
		d := DecodeDisposition(meta)
		enc := d.Encode().Fold()
		for _, k := range dispositionKeys {
			if enc[k] != meta[k] {
				t.Errorf("%v: Encode rewrites %s %q -> %q", meta, k, meta[k], enc[k])
			}
		}
		if changed := d.Changed().Fold(); len(changed) != 0 {
			t.Errorf("%v: an unmoved disposition changes %v", meta, changed)
		}
	}
	dispositionGrid(check)
	for _, meta := range []map[string]string{
		{"held_until": "2026-10-10T13:00:00.123456789Z"},
		{"held_until": "2026-10-10T14:00:00+01:00"},
		{"held_until": " 2026-10-10T13:00:00Z "},
		{"quarantined_until": "2026-10-10T13:00:00.5Z"},
		{"suspended_at": "2026-10-10T11:00:00.5Z"},
		{"wait_hold": "w"},
		{"held_until": "0001-01-01T00:00:00Z"},
	} {
		check(meta)
	}
}

// TestDispositionChangedIsTheMove: a move writes the keys whose meaning it
// moved and no other: a heartbeat on a bare wait writes held_until alone.
func TestDispositionChangedIsTheMove(t *testing.T) {
	d := DecodeDisposition(map[string]string{"wait_hold": "w", "suspended_at": "garbage", "sleep_intent": "other"})
	moved, err := d.Apply(ActorAgent, DispEvent{Kind: EvHeartbeat, Until: dispT0.Add(time.Hour)}, StateActive, dispT0)
	if err != nil {
		t.Fatal(err)
	}
	if got := moved.Changed().Fold(); len(got) != 1 || got["held_until"] != dispT0.Add(time.Hour).Format(time.RFC3339) {
		t.Fatalf("a heartbeat on a bare wait changes %v, want held_until alone", got)
	}
}

// TestDispositionOfInfoMatchesDecode: the typed row's disposition is its
// metadata's.
func TestDispositionOfInfoMatchesDecode(t *testing.T) {
	meta := map[string]string{
		"state": string(StateAsleep), "sleep_intent": string(SleepReasonUserHold),
		"held_until": dispT0.Add(time.Hour).Format(time.RFC3339), "wait_hold": "true", "suspended_at": dispT0.Format(time.RFC3339),
		"quarantined_until": "soon", "sleep_reason": string(SleepReasonKilled),
	}
	info := Info{
		MetadataState: meta["state"], SleepIntent: meta["sleep_intent"], HeldUntil: meta["held_until"],
		WaitHold: meta["wait_hold"], SuspendedAt: meta["suspended_at"], QuarantinedUntil: meta["quarantined_until"],
		SleepReason: meta["sleep_reason"],
	}
	if got, want := DispositionOfInfo(info), DecodeDisposition(meta); !reflect.DeepEqual(got, want) {
		t.Fatalf("DispositionOfInfo = %+v, want %+v", got, want)
	}
}

// TestDispositionOperatorDormant: an operator's suspend, kill or city stop,
// a wait and a live timer hold a row dormant; an auto-suspend, an idle stop,
// an expired timer and a lifecycle sleep reason do not.
func TestDispositionOperatorDormant(t *testing.T) {
	future, past := dispT0.Add(time.Hour).Format(time.RFC3339), dispT0.Add(-time.Hour).Format(time.RFC3339)
	for _, c := range []struct {
		meta    map[string]string
		dormant bool
	}{
		{map[string]string{}, false},
		{map[string]string{"sleep_reason": string(SleepReasonKilled)}, true},
		{map[string]string{"sleep_reason": string(SleepReasonUserHold)}, true},
		{map[string]string{"sleep_reason": string(SleepReasonCityStop)}, true},
		{map[string]string{"sleep_reason": string(SleepReasonIdle)}, false},
		{map[string]string{"sleep_intent": string(SleepReasonUserHold)}, true},
		{map[string]string{"state": string(StateSuspended)}, false},
		{map[string]string{"sleep_intent": string(sleepIntentIdleStopPending)}, false},
		{map[string]string{"wait_hold": "true"}, true},
		{map[string]string{"held_until": future}, true},
		{map[string]string{"held_until": past}, false},
		{map[string]string{"quarantined_until": future}, true},
		{map[string]string{"quarantined_until": past}, false},
	} {
		if got := DecodeDisposition(c.meta).OperatorDormant(dispT0); got != c.dormant {
			t.Errorf("%v: OperatorDormant = %v, want %v", c.meta, got, c.dormant)
		}
	}
}

// TestDispositionRefusesPreWake (CONTRACT v5.9u): an operator's suspend
// refuses the controller's PreWake and A17's heartbeat respawn; a soft
// suspend (the stop and rig-restart sweeps') refuses neither. A live timer
// refuses the PreWake alone; a wait and an unreadable timer refuse neither.
func TestDispositionRefusesPreWake(t *testing.T) {
	future := dispT0.Add(time.Hour).Format(time.RFC3339)
	for _, c := range []struct {
		meta             map[string]string
		preWake, respawn bool
	}{
		{map[string]string{}, false, false},
		{map[string]string{"state": string(StateSuspended), "suspended_at": future}, false, false},
		{map[string]string{"state": string(StateSuspended), "sleep_intent": string(SleepReasonUserHold), "held_until": future}, true, true},
		{map[string]string{"sleep_intent": string(SleepReasonUserHold)}, true, true},
		{map[string]string{"held_until": future}, true, false},
		{map[string]string{"quarantined_until": future}, true, true},
		{map[string]string{"wait_hold": "true"}, false, false},
		{map[string]string{"held_until": "soon"}, false, false},
	} {
		d := DecodeDisposition(c.meta)
		if got := d.RefusesPreWake(dispT0); got != c.preWake {
			t.Errorf("%v: RefusesPreWake = %v, want %v", c.meta, got, c.preWake)
		}
		if got := d.RefusesHeartbeatRespawn(dispT0); got != c.respawn {
			t.Errorf("%v: RefusesHeartbeatRespawn = %v, want %v", c.meta, got, c.respawn)
		}
	}
}

// TestDispositionKeepsSlot: over the decoder's grid, only an operator's
// suspend (sleep_intent=user-hold) keeps a slot; a soft suspend (the chat
// idle auto-suspend, the city stop sweep), a wait, a timer and a kill do not.
func TestDispositionKeepsSlot(t *testing.T) {
	future := dispT0.Add(time.Hour).Format(time.RFC3339)
	n := 0
	for _, state := range []string{"", string(StateSuspended), " suspended ", string(StateAsleep), string(StateActive)} {
		for _, intent := range []string{"", string(SleepReasonUserHold), " user-hold ", string(SleepReasonWaitHold), string(sleepIntentIdleStopPending), "zzz"} {
			for _, wait := range []string{"", "true"} {
				for _, held := range []string{"", future, "bad"} {
					for _, reason := range []string{"", string(SleepReasonKilled), string(SleepReasonCityStop)} {
						meta := map[string]string{"state": state, "sleep_intent": intent, "wait_hold": wait, "held_until": held, "sleep_reason": reason}
						want := strings.TrimSpace(intent) == string(SleepReasonUserHold)
						if got := DecodeDisposition(meta).KeepsSlot(); got != want {
							t.Errorf("%v: KeepsSlot = %v, want %v", meta, got, want)
						}
						n++
					}
				}
			}
		}
	}
	if n != 5*6*2*3*3 {
		t.Fatalf("%d rows", n)
	}
}

// TestDispositionHoldsDeliberately: A21's held pool row (§12.2 #27) is an
// operator's suspend, a wait, or a live or unreadable timer; a soft suspend,
// an idle stop and an expired timer are not.
func TestDispositionHoldsDeliberately(t *testing.T) {
	future, past := dispT0.Add(time.Hour).Format(time.RFC3339), dispT0.Add(-time.Hour).Format(time.RFC3339)
	for _, c := range []struct {
		meta map[string]string
		held bool
	}{
		{map[string]string{}, false},
		{map[string]string{"sleep_intent": string(SleepReasonUserHold)}, true},
		{map[string]string{"state": string(StateSuspended)}, false},
		{map[string]string{"state": string(StateSuspended), "wait_hold": "true"}, true},
		{map[string]string{"sleep_intent": string(sleepIntentIdleStopPending)}, false},
		{map[string]string{"sleep_reason": string(SleepReasonKilled)}, false},
		{map[string]string{"wait_hold": "true"}, true},
		{map[string]string{"held_until": future}, true},
		{map[string]string{"held_until": past}, false},
		{map[string]string{"held_until": "soon"}, true},
		{map[string]string{"quarantined_until": future}, true},
		{map[string]string{"quarantined_until": past}, false},
		{map[string]string{"quarantined_until": "soon"}, true},
	} {
		if got := DecodeDisposition(c.meta).HoldsDeliberately(dispT0); got != c.held {
			t.Errorf("%v: HoldsDeliberately = %v, want %v", c.meta, got, c.held)
		}
	}
}

// TestDispositionHeartbeatHeld: only a live held_until under no sleep intent
// is a heartbeat's; a bare wait_hold beside it does not stop it.
func TestDispositionHeartbeatHeld(t *testing.T) {
	future := dispT0.Add(time.Hour).Format(time.RFC3339)
	for _, c := range []struct {
		meta map[string]string
		held bool
	}{
		{map[string]string{"held_until": future}, true},
		{map[string]string{"held_until": dispT0.Add(-time.Hour).Format(time.RFC3339)}, false},
		{map[string]string{"held_until": future, "sleep_intent": string(SleepReasonUserHold)}, false},
		{map[string]string{"held_until": future, "wait_hold": "true"}, true}, // a bare wait: SESS-602 keeps it up
		{map[string]string{"held_until": future, "sleep_intent": string(SleepReasonWaitHold)}, false},
		{map[string]string{"held_until": future, "sleep_intent": string(sleepIntentIdleStopPending)}, false},
		{map[string]string{"held_until": future, "state": string(StateSuspended)}, true},
		{map[string]string{"held_until": dispT0.Add(MaxHeartbeatDuration + time.Hour).Format(time.RFC3339)}, true}, // the horizon's edge
		{map[string]string{"held_until": dispT0.Add(MaxHeartbeatDuration + time.Hour + time.Second).Format(time.RFC3339)}, false},
		{map[string]string{"held_until": dispT0.AddDate(100, 0, 0).Format(time.RFC3339)}, false}, // an operator hold whose intent was cleared
	} {
		if got := DecodeDisposition(c.meta).HeartbeatHeld(dispT0); got != c.held {
			t.Errorf("%v: HeartbeatHeld = %v, want %v", c.meta, got, c.held)
		}
	}
}
