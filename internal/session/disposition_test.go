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

// dispSeeds are the dispositions the table golden starts from: each operator
// value, each sub-state on its own, the combinations a cell treats apart,
// and an unreadable timer.
func dispSeeds() []struct {
	name string
	d    Disposition
} {
	hour := dispT0.Add(time.Hour)
	return []struct {
		name string
		d    Disposition
	}{
		{"none", Disposition{}},
		{"none+wait", Disposition{Wait: true}},
		{"none+heartbeat", Disposition{Heartbeat: hour}},
		{"none+heartbeat-expired", Disposition{Heartbeat: dispT0.Add(-time.Minute)}},
		{"none+idle-stop", Disposition{IdleStop: true}},
		{"none+wait+idle-stop(legacy)", Disposition{Wait: true, IdleStop: true}},
		{"none+quarantine", Disposition{Quarantine: hour}},
		{"none+quarantine-expired", Disposition{Quarantine: dispT0.Add(-time.Minute)}},
		{"none+unknown-timer", Disposition{Unknown: UnknownHeldUntil, rawUntil: "soon"}},
		{"none+unknown-quarantine", Disposition{Unknown: UnknownQuarantine, rawQuarantine: "later"}},
		{"suspended", Disposition{Operator: OperatorSuspended, Until: dispT0.Add(IndefiniteHoldDuration), Since: dispT0}},
		{"suspended+wait", Disposition{Operator: OperatorSuspended, Until: dispT0.Add(IndefiniteHoldDuration), Wait: true}},
		{"suspended-finite-expired", Disposition{Operator: OperatorSuspended, Until: dispT0.Add(-time.Minute), Since: dispT0.Add(-time.Hour)}},
		{"suspended-soft", Disposition{Operator: OperatorSuspendedSoft, Since: dispT0}},
	}
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
	enc := d.Encode()
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
				fmt.Fprintf(&b, "  %-28s life=%-8s -> %s\n", s.name, life, result)
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
	meta := map[string]string(d.Encode())
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
				if got := DecodeDisposition(encodedRow(d)); !reflect.DeepEqual(got, d) {
					t.Fatalf("seed %s, walk %d: Decode(Encode(%+v)) = %+v", s.name, walk, d, got)
				}
			}
		}
	}
}

// TestDispositionEncodeWritesTheWholeClass: every encoding names all five
// keys, so a write of it replaces the class whole.
func TestDispositionEncodeWritesTheWholeClass(t *testing.T) {
	for _, s := range dispSeeds() {
		enc := s.d.Encode()
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

// TestDispositionProjectionsMatchHolds: over every combination of the keys'
// values, SuppressesWake and BlocksConsume answer as HoldSet does, and
// KeepsSlot is HoldUser: a HoldSet reader switches to the disposition with
// no change in what it decides.
func TestDispositionProjectionsMatchHolds(t *testing.T) {
	future, past := dispT0.Add(time.Hour).Format(time.RFC3339), dispT0.Add(-time.Hour).Format(time.RFC3339)
	values := map[string][]string{
		"state":             {"", string(StateAsleep), string(StateSuspended), string(StateActive), string(StateDraining)},
		"sleep_intent":      {"", string(SleepReasonUserHold), string(SleepReasonWaitHold), string(sleepIntentIdleStopPending), "other"},
		"wait_hold":         {"", "true"},
		"held_until":        {"", future, past, "soon"},
		"quarantined_until": {"", future, past, "soon"},
		"sleep_reason":      {"", string(SleepReasonKilled), string(SleepReasonIdle)},
	}
	keys := []string{"state", "sleep_intent", "wait_hold", "held_until", "quarantined_until", "sleep_reason"}
	var walk func(i int, meta map[string]string)
	n := 0
	walk = func(i int, meta map[string]string) {
		if i == len(keys) {
			n++
			h, d := Holds(meta, dispT0), DecodeDisposition(meta)
			if h.SuppressesWake() != d.SuppressesWake(dispT0) || h.BlocksConsume() != d.BlocksConsume(dispT0) ||
				(h.In&HoldUser != 0) != d.KeepsSlot() {
				t.Errorf("%v: HoldSet %+v (suppress %v, block %v), disposition %+v (suppress %v, block %v, keeps %v)",
					meta, h, h.SuppressesWake(), h.BlocksConsume(), d, d.SuppressesWake(dispT0), d.BlocksConsume(dispT0), d.KeepsSlot())
			}
			return
		}
		for _, v := range values[keys[i]] {
			meta[keys[i]] = v
			walk(i+1, meta)
		}
	}
	walk(0, map[string]string{})
	if n != 5*5*2*4*4*3 {
		t.Fatalf("walked %d rows", n)
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

// TestDispositionHeartbeatHeld: only a live held_until no suspend, wait or
// idle stop explains is a heartbeat's.
func TestDispositionHeartbeatHeld(t *testing.T) {
	future := dispT0.Add(time.Hour).Format(time.RFC3339)
	for _, c := range []struct {
		meta map[string]string
		held bool
	}{
		{map[string]string{"held_until": future}, true},
		{map[string]string{"held_until": dispT0.Add(-time.Hour).Format(time.RFC3339)}, false},
		{map[string]string{"held_until": future, "sleep_intent": string(SleepReasonUserHold)}, false},
		{map[string]string{"held_until": future, "wait_hold": "true"}, false},
		{map[string]string{"held_until": future, "sleep_intent": string(sleepIntentIdleStopPending)}, false},
		{map[string]string{"held_until": future, "state": string(StateSuspended)}, true},
	} {
		if got := DecodeDisposition(c.meta).HeartbeatHeld(dispT0); got != c.held {
			t.Errorf("%v: HeartbeatHeld = %v, want %v", c.meta, got, c.held)
		}
	}
}
