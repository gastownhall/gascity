package workladder

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata/decision_table.golden")

// TestDecisionTable renders what Decide does for a due entry in every
// (kind, rung, count) state under each seat condition, for review. A change
// to the policy or the steps shows here; regenerate with -update.
func TestDecisionTable(t *testing.T) {
	now := t0.Add(10 * time.Hour)
	type row struct {
		name string
		e    Entry
	}
	ent := func(k Kind, r Rung, attempts int, mut func(*Entry)) Entry {
		e := Entry{Key: key("x"), Kind: k, Rung: r, Since: t0, Attempts: attempts, Last: t0}
		if mut != nil {
			mut(&e)
		}
		return e
	}
	rows := []row{
		{"trigger/assigned, attempts < 3", ent(KindTrigger, RungAssigned, 2, nil)},
		{"trigger/assigned, attempts >= 3", ent(KindTrigger, RungAssigned, 3, nil)},
		{"trigger/assigned, >= 3, recycled 10m ago", ent(KindTrigger, RungAssigned, 4, func(e *Entry) { e.RecycleAt = now.Add(-10 * time.Minute) })},
		{"continuation/assigned, attempts < 6", ent(KindContinuation, RungAssigned, 5, nil)},
		{"continuation/assigned, attempts >= 6", ent(KindContinuation, RungAssigned, 6, nil)},
		{"continuation/assigned, >= 6, alerted", ent(KindContinuation, RungAssigned, 7, func(e *Entry) { e.Escalated = true })},
		{"claim/claimed, attempts < 3", ent(KindClaim, RungClaimed, 2, nil)},
		{"claim/claimed, attempts >= 3", ent(KindClaim, RungClaimed, 3, nil)},
		{"claim/claimed, release did not finish", ent(KindClaim, RungClaimed, 3, func(e *Entry) { e.Unreleased = true })},
		{"claim/started, attempts < 6", ent(KindClaim, RungStarted, 5, nil)},
		{"claim/started, >= 6, recycles 0 < R", ent(KindClaim, RungStarted, 6, nil)},
		{"claim/started, >= 6, recycles 1 = R", ent(KindClaim, RungStarted, 6, func(e *Entry) { e.Recycles = 1 })},
		{"claim/escalated", ent(KindClaim, RungEscalated, 9, func(e *Entry) { e.Recycles, e.Escalated = 1, true })},
		{"claim/escalated, recycled fresh long ago", ent(KindClaim, RungEscalated, 9, func(e *Entry) { e.Recycles, e.Escalated, e.FreshRecycled = 1, true, true })},
		{"claim/escalated, recycled fresh just now", ent(KindClaim, RungEscalated, 9, func(e *Entry) {
			e.Recycles, e.Escalated, e.FreshRecycled, e.RecycleAt = 1, true, true, now.Add(-5*time.Minute)
		})},
		{"claim/escalated, recycled fresh just now, served since", ent(KindClaim, RungEscalated, 9, func(e *Entry) {
			e.Recycles, e.Escalated, e.FreshRecycled, e.FreshServed, e.RecycleAt = 1, true, true, true, now.Add(-5*time.Minute)
		})},
		{"claim/escalated, rate limit alerted", ent(KindClaim, RungEscalated, 9, func(e *Entry) { e.Recycles, e.Escalated, e.RateLimitAlerted = 1, true, true })},
	}
	type col struct {
		name string
		mut  func(*Observation)
	}
	cols := []col{
		{"quiet", func(*Observation) {}},
		{"quiet, degraded", func(o *Observation) { o.Provider.Degraded = true }},
		{"gate held", func(o *Observation) { o.Gate.Held = true }},
		{"not quiet", func(o *Observation) { o.Quiet.State = NotQuiet }},
		{"runtime dead", func(o *Observation) { o.Runtime = TriFalse }},
		{"quiet, nudge refused 2x", func(o *Observation) { o.RateLimited, o.RateLimitedAt = 2, o.Now }},
		{"quiet, nudge refused 2x, fresh queued", func(o *Observation) { o.RateLimited, o.RateLimitedAt, o.FreshHold = 2, o.Now, FreshHoldQueued }},
		{"quiet, nudge refused 2x, fresh standing", func(o *Observation) { o.RateLimited, o.RateLimitedAt, o.FreshHold = 2, o.Now, FreshHoldStanding }},
		{"quiet, degraded, nudge refused 2x", func(o *Observation) { o.Provider.Degraded, o.RateLimited, o.RateLimitedAt = true, 2, o.Now }},
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# workladder decision table (DefaultPolicy; a due entry; next = when the entry is next due)\n\n")
	fmt.Fprintf(&b, "| state |")
	for _, c := range cols {
		fmt.Fprintf(&b, " %s |", c.name)
	}
	fmt.Fprintf(&b, "\n|---|%s\n", strings.Repeat("---|", len(cols)))
	for _, r := range rows {
		fmt.Fprintf(&b, "| %s |", r.name)
		for _, c := range cols {
			o := quiet(now, 1, Work{Key: r.e.Key, Kind: r.e.Kind})
			if r.e.Rung >= RungStarted {
				o.Started[r.e.Key] = SrcAgent
			}
			c.mut(&o)
			d := Decide(rec(1, r.e), o, p)
			var acts []string
			for _, a := range effects(d) {
				acts = append(acts, render(a))
			}
			if len(acts) == 0 {
				acts = []string{"-"}
			}
			fmt.Fprintf(&b, " %s; next %v |", strings.Join(acts, " + "), nextLook(d, now, o.Provider.Degraded))
		}
		b.WriteString("\n")
	}
	got := b.String()
	path := filepath.Join("testdata", "decision_table.golden")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (create with -update): %v", err)
	}
	if got != string(want) {
		t.Fatalf("decision table drifted; review and regenerate with -update:\n%s", got)
	}
}

func render(a Action) string {
	switch a := a.(type) {
	case Nudge:
		if a.Paused {
			return "paused nudge"
		}
		return fmt.Sprintf("nudge %d", a.Attempt)
	case Recycle:
		switch {
		case a.Fresh:
			return "fresh recycle (keep claim)"
		case a.KeepClaim:
			return "recycle (keep claim)"
		}
		return "recycle"
	case Release:
		return "release"
	case Alert:
		return "alert " + a.Reason
	}
	return fmt.Sprintf("%T", a)
}

// nextLook is when the table's entry is next due after this pass, from now:
// "-" when the pass leaves it due (it waits on a hold).
func nextLook(d Decision, now time.Time, degraded bool) string {
	if len(d.Next.Entries) == 0 || (d.Next.Release != nil && d.Next.Release.Key == d.Next.Entries[0].Key) {
		return "-"
	}
	at := p.dueAt(d.Next.Entries[0], degraded)
	if !at.After(now) {
		return "now"
	}
	return at.Sub(now).String()
}
