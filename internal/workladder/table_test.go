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
// (kind, rung, count) state under each seat condition, for review; "count
// reset" marks a pass whose judgment of the seat's progress restarted the
// count. A change to the policy or the steps shows here; regenerate with
// -update.
func TestDecisionTable(t *testing.T) {
	now := t0.Add(10 * time.Hour)
	type row struct {
		name string
		e    Entry
		// other is a second entry the seat holds, whose notice is pending.
		other *Entry
	}
	ent := func(k Kind, r Rung, attempts int, mut func(*Entry)) Entry {
		e := Entry{Key: key("x"), Kind: k, Rung: r, Since: t0, Attempts: attempts, Last: t0}
		if mut != nil {
			mut(&e)
		}
		return e
	}
	rows := []row{
		{"trigger/assigned, attempts < 3", ent(KindTrigger, RungAssigned, 2, nil), nil},
		{"trigger/assigned, attempts >= 3", ent(KindTrigger, RungAssigned, 3, nil), nil},
		{"trigger/assigned, >= 3, recycled 10m ago", ent(KindTrigger, RungAssigned, 4, func(e *Entry) { e.RecycleAt = now.Add(-10 * time.Minute) }), nil},
		{"continuation/assigned, attempts < 6", ent(KindContinuation, RungAssigned, 5, nil), nil},
		{"continuation/assigned, attempts >= 6", ent(KindContinuation, RungAssigned, 6, nil), nil},
		{"continuation/assigned, >= 6, alerted", ent(KindContinuation, RungAssigned, 7, func(e *Entry) { e.Escalated = true }), nil},
		{"claim/claimed, attempts < 3", ent(KindClaim, RungClaimed, 2, nil), nil},
		{"claim/claimed, attempts >= 3", ent(KindClaim, RungClaimed, 3, nil), nil},
		{"claim/claimed, release did not finish", ent(KindClaim, RungClaimed, 3, func(e *Entry) { e.Unreleased = true }), nil},
		{"claim/started, attempts < 6", ent(KindClaim, RungStarted, 5, nil), nil},
		{"claim/started, >= 6, recycles 0 < R", ent(KindClaim, RungStarted, 6, nil), nil},
		{"claim/started, >= 6, recycles 1 = R", ent(KindClaim, RungStarted, 6, func(e *Entry) { e.Recycles = 1 }), nil},
		{"claim/escalated", ent(KindClaim, RungEscalated, 9, func(e *Entry) { e.Recycles, e.Escalated = 1, true }), nil},
		{"claim/started, attempts < 6, its notice pending", ent(KindClaim, RungStarted, 5, func(e *Entry) { e.Notice = NoticeMark{Op: "op", At: now} }), nil},
		{"claim/claimed, attempts >= 3, its notice pending", ent(KindClaim, RungClaimed, 3, func(e *Entry) { e.Notice = NoticeMark{Op: "op", At: now} }), nil},
		{"claim/started, attempts < 6, another's notice pending", ent(KindClaim, RungStarted, 5, nil), pendingOther(now)},
		{"claim/claimed, attempts >= 3, another's notice pending", ent(KindClaim, RungClaimed, 3, nil), pendingOther(now)},
		{"claim/started, >= 6, recycles 0 < R, another's notice pending", ent(KindClaim, RungStarted, 6, nil), pendingOther(now)},
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
		{"quiet, worked since", workedSince(nil)},
		{"not quiet, worked since", workedSince(func(o *Observation) { o.Quiet.State = NotQuiet })},
		{"gate held, worked since", workedSince(func(o *Observation) { o.Gate.Held = true })},
		{"quiet, worked since, newest turn failed", workedSince(func(o *Observation) { o.Turns = TurnReading{Read: true, Conv: 1, LastFailed: true} })},
		{"quiet, newest turn failed, served since", workedSince(func(o *Observation) {
			o.Turns = TurnReading{Read: true, Conv: 1, LastFailed: true, ServedAt: o.Now.Add(-5 * time.Minute)}
		})},
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
			prev := rec(1, r.e)
			if r.other != nil {
				o.Outstanding = append(o.Outstanding, Work{Key: r.other.Key, Kind: r.other.Kind})
				prev = rec(1, r.e, *r.other)
			}
			d := Decide(prev, o, p)
			var acts []string
			for _, a := range effects(d) {
				acts = append(acts, render(a))
			}
			if len(acts) == 0 {
				acts = []string{"-"}
			}
			reset := ""
			if len(d.Next.Entries) > 0 && !d.Next.Entries[0].Since.Equal(r.e.Since) {
				reset = "count reset; "
			}
			fmt.Fprintf(&b, " %s%s; next %v |", reset, strings.Join(acts, " + "), nextLook(d, now, o.Provider.Degraded))
		}
		b.WriteString("\n")
	}
	rateLimitTable(&b, now)
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

// workedSince is a column whose seat worked five minutes ago, long after
// the table's last nudge, with mut applied.
func workedSince(mut func(*Observation)) func(*Observation) {
	return func(o *Observation) {
		o.Conv, o.LastActivity = 1, o.Now.Add(-5*time.Minute)
		if mut != nil {
			mut(o)
		}
	}
}

// pendingOther is a trigger the table's seat also holds, its nudge's notice
// pending.
func pendingOther(now time.Time) *Entry {
	return &Entry{Key: key("y"), Kind: KindTrigger, Rung: RungAssigned, Since: t0, Attempts: 1, Last: now, Notice: NoticeMark{Op: "op", At: now}}
}

func render(a Action) string {
	switch a := a.(type) {
	case Nudge:
		if a.Paused {
			return "paused nudge"
		}
		return fmt.Sprintf("nudge %d", a.Attempt)
	case Recycle:
		if a.KeepClaim {
			return "recycle (keep claim)"
		}
		return "recycle"
	case Release:
		return "release"
	case Alert:
		return "alert " + a.Reason
	case FreshRestart:
		return "fresh restart"
	case RateLimitMove:
		if a.Cause == "" {
			return string(a.Transition)
		}
		return string(a.Transition) + " " + string(a.Cause)
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

// rateLimitTable renders the seat's rate-limit episode (ratelimit.go)
// for review: each phase against each reading, on a quiet
// seat holding one started claim whose last nudge reached it 3m ago. A cell
// is the phase after the pass and the pass's actions.
func rateLimitTable(b *strings.Builder, now time.Time) {
	delivered := now.Add(-3 * time.Minute)
	open := func(age time.Duration, mut func(*RateLimit)) RateLimit {
		rl := RateLimit{Phase: RateLimitOpen, Conv: 1, Since: now.Add(-age), Checked: now.Add(-3 * time.Minute)}
		if mut != nil {
			mut(&rl)
		}
		return rl
	}
	type row struct {
		name string
		rl   RateLimit
		conv int64
	}
	fresh := now.Add(-15 * time.Minute)
	rows := []row{
		{"clear", RateLimit{}, 1},
		{"clear, restarted fresh 30m ago", RateLimit{LastFresh: now.Add(-30 * time.Minute)}, 1},
		{"open 5m", open(5*time.Minute, nil), 1},
		{"open 10m (confirmed)", open(10*time.Minute, nil), 1},
		{"open 10m, restarted fresh 30m ago", open(10*time.Minute, func(rl *RateLimit) { rl.LastFresh = now.Add(-30 * time.Minute) }), 1},
		{"open 70m (past confirm + cap)", open(70*time.Minute, nil), 1},
		{"open 70m, alerted held", open(70*time.Minute, func(rl *RateLimit) { rl.Alerted = AlertRateLimitedHeld }), 1},
		{"restarted 15m ago", RateLimit{Phase: RateLimitRestarted, Conv: 1, Since: fresh.Add(-10 * time.Minute), Checked: fresh, FreshAt: fresh, FromConv: 1, LastFresh: fresh}, 2},
		{"restarted 15m ago, did not take", RateLimit{Phase: RateLimitRestarted, Conv: 1, Since: fresh.Add(-10 * time.Minute), Checked: fresh, FreshAt: fresh, FromConv: 1, LastFresh: fresh}, 1},
		{"exhausted", open(30*time.Minute, func(rl *RateLimit) { rl.Phase, rl.Alerted = RateLimitExhausted, AlertRateLimitedFreshRefused }), 1},
	}
	refused := func(n int, at time.Time) func(*Record, *Observation) {
		return func(_ *Record, o *Observation) {
			o.Turns = TurnReading{Read: true, Conv: o.Conv, LastFailed: true, Refused: n, RefusedAt: at}
		}
	}
	answered := refused(2, now.Add(-time.Minute))
	with := func(f func(*Record, *Observation), g func(*Observation)) func(*Record, *Observation) {
		return func(r *Record, o *Observation) { f(r, o); g(o) }
	}
	type col struct {
		name string
		mut  func(*Record, *Observation)
	}
	cols := []col{
		{"no reading", func(*Record, *Observation) {}},
		{"refused x2, answered", answered},
		{"answered, degraded", with(answered, func(o *Observation) { o.Provider.Degraded = true })},
		{"answered, a spent quota", with(answered, func(o *Observation) { o.Turns.Quota = true })},
		{"answered, attached", with(answered, func(o *Observation) { o.RestartHold = RestartHoldAttached })},
		{"answered, another seat on the provider restarted 10m ago", with(answered, func(o *Observation) { o.ProviderFreshAt = now.Add(-10 * time.Minute) })},
		{"answered, a claim unread", with(answered, func(o *Observation) { o.Hold = map[ClaimKey]bool{key("x"): true} })},
		{"refused x1", refused(1, now.Add(-time.Minute))},
		{"refused x2 before the delivery", refused(2, delivered.Add(-time.Minute))},
		{"served 1m ago", func(_ *Record, o *Observation) {
			o.Turns = TurnReading{Read: true, Conv: o.Conv, ServedAt: now.Add(-time.Minute)}
		}},
		{"no answer for 61m", func(r *Record, _ *Observation) {
			r.RateLimit.Checked = minTime(r.RateLimit.Checked, now.Add(-61*time.Minute))
		}},
		{"conversation changed", func(_ *Record, o *Observation) { o.Conv++ }},
		{"busy, answered", with(answered, func(o *Observation) { o.Quiet = Quiet{State: NotQuiet} })},
	}
	fmt.Fprintf(b, "\n# rate-limit episode (a started claim, last nudge delivered 3m ago; phase after the pass: actions)\n\n| phase |")
	for _, c := range cols {
		fmt.Fprintf(b, " %s |", c.name)
	}
	fmt.Fprintf(b, "\n|---|%s\n", strings.Repeat("---|", len(cols)))
	for _, r := range rows {
		fmt.Fprintf(b, "| %s |", r.name)
		for _, c := range cols {
			prev := rec(1, Entry{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 2, Last: delivered})
			prev.Delivered, prev.LastNudge, prev.RateLimit = delivered, delivered, r.rl
			o := startedBy1(quiet(now, 1, claim("x")), "x", SrcAgent)
			o.Conv = r.conv
			c.mut(&prev, &o)
			d := Decide(prev, o, p)
			var acts []string
			for _, a := range d.Actions {
				acts = append(acts, render(a))
			}
			if len(acts) == 0 {
				acts = []string{"-"}
			}
			fmt.Fprintf(b, " %v: %s |", d.Next.RateLimit.Phase, strings.Join(acts, " + "))
		}
		b.WriteString("\n")
	}
}
