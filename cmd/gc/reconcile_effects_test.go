package main

import (
	"context"
	"errors"
	"io"
	"maps"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// The planner executor's tests on a fake clock (CONTRACT v5 P3, P7; N5).

// fakeClockExecutor is an executor on clk whose settlements arrive on the
// returned channel.
func fakeClockExecutor(clk *fakePlannerClock) (*effectExecutor, chan settlement) {
	posted := make(chan settlement, 64)
	x := newEffectExecutor(func(s settlement) { posted <- s }, io.Discard)
	x.clock = clk
	return x, posted
}

// waitTimersAt waits until n of clk's timers are armed for at. Each effect
// arms two: the executor's settlement timer and its context's deadline.
func waitTimersAt(t *testing.T, clk *fakePlannerClock, at time.Time, n int) {
	t.Helper()
	guard := time.After(plannerTestGuard)
	for {
		clk.mu.Lock()
		armed := 0
		for _, tm := range clk.timers {
			if tm.active && tm.at.Equal(at) {
				armed++
			}
		}
		changed := clk.changed
		clk.mu.Unlock()
		if armed == n {
			return
		}
		select {
		case <-changed:
		case <-guard:
			t.Fatalf("%d timers armed for %s, want %d", armed, at.Sub(plannerT0), n)
		}
	}
}

func receive(t *testing.T, posted <-chan settlement) settlement {
	t.Helper()
	select {
	case s := <-posted:
		return s
	case <-time.After(plannerTestGuard):
		t.Fatal("no settlement posted")
		return settlement{}
	}
}

// hungEffect ignores its context until release closes.
func hungEffect(kind string, seq uint64, deadline time.Time, release <-chan struct{}) sessionEffect {
	return sessionEffect{Kind: kind, Seq: seq, Deadline: deadline, Run: func(context.Context) settlement {
		<-release
		return settlement{Outcome: settledLanded}
	}}
}

// Kills dependence on the real clock (N5) and a second settlement: an
// effect that ignores its context settles failed when the injected clock
// reaches its deadline, echoing its key, kind and seq, and its late return,
// with no facts, posts nothing more.
func TestExecutorSettlesOnceUnderFakeClock(t *testing.T) {
	clk := newFakePlannerClock(plannerT0)
	x, posted := fakeClockExecutor(clk)
	var spawned sync.WaitGroup
	x.spawn = func(f func()) {
		spawned.Add(1)
		go func() {
			defer spawned.Done()
			f()
		}()
	}
	release := make(chan struct{})
	k := rowKey{Leg: rowLeg, ID: "gc-1"}
	if err := x.submit(k, hungEffect(intentStart, 7, plannerT0.Add(70*time.Second), release)); err != nil {
		t.Fatal(err)
	}
	waitTimersAt(t, clk, plannerT0.Add(70*time.Second), 2) // the settlement timer and Run's deadline
	clk.Advance(70 * time.Second)
	s := receive(t, posted)
	if s.Outcome != settledFailed || s.Cause != causeDeadline || s.Key != k || s.Kind != intentStart || s.Seq != 7 {
		t.Fatalf("settlement %+v, want failed at the deadline for %v start seq 7", s, k)
	}
	close(release)
	spawned.Wait() // the executor's goroutines, Run's included, have returned
	if len(posted) != 0 {
		t.Fatalf("a second settlement after the late return: %+v", <-posted)
	}
}

// Kills a start admitted before a provider swap's pause running during it
// (P7): while starts are closed a start runs nothing and settles refused
// with cause swap-pause; other kinds still run; reopened, starts run.
func TestExecutorRefusesStartsWhileClosed(t *testing.T) {
	clk := newFakePlannerClock(plannerT0)
	x, posted := fakeClockExecutor(clk)
	ran := make(chan string, 4)
	effect := func(kind string) sessionEffect {
		return sessionEffect{Kind: kind, Deadline: plannerT0.Add(time.Minute), Run: func(context.Context) settlement {
			ran <- kind
			return settlement{Outcome: settledLanded}
		}}
	}
	x.closeStarts()
	if err := x.submit(rowKey{ID: "a"}, effect(intentStart)); err != nil {
		t.Fatal(err)
	}
	if s := receive(t, posted); s.Outcome != settledRefused || s.Cause != causeSwapPause || s.Key.ID != "a" {
		t.Fatalf("closed start: settlement %+v, want refused with cause %q", s, causeSwapPause)
	}
	if err := x.submit(rowKey{ID: "b"}, effect(intentRowHeal)); err != nil {
		t.Fatal(err)
	}
	receive(t, posted)
	x.openStarts()
	if err := x.submit(rowKey{ID: "c"}, effect(intentStart)); err != nil {
		t.Fatal(err)
	}
	receive(t, posted)
	if got := []string{<-ran, <-ran}; !slices.Equal(got, []string{intentRowHeal, intentStart}) || len(ran) != 0 {
		t.Fatalf("ran %v, want the row write and the reopened start only", got)
	}
}

// Kills an unregistered kind that runs something, never settles, or wedges
// its in-flight entry: it settles refused with cause no-effect, echoing its
// key, kind and seq. The registered row heal builds the row-write effect.
func TestUnregisteredKindRefusesNoEffect(t *testing.T) {
	clk := newFakePlannerClock(plannerT0)
	x, posted := fakeClockExecutor(clk)
	k := rowKey{Leg: rowLeg, ID: "gc-1"}
	if err := x.submitIntent(&effectPass{}, intent{Kind: "drift-drain", Key: k, Deadline: plannerT0.Add(time.Minute)}, 3); err != nil {
		t.Fatal(err)
	}
	if s := receive(t, posted); s.Outcome != settledRefused || s.Cause != causeNoEffect || s.Key != k || s.Kind != "drift-drain" || s.Seq != 3 {
		t.Fatalf("settlement %+v, want refused with cause %q for %v seq 3", s, causeNoEffect, k)
	}
	if !effectSpecs[intentRowHeal].runs() {
		t.Fatal("the row heal has no effect")
	}
}

// withSpecs replaces effectSpecs for the test with a copy that edit
// changes. Tests that call it must not run in parallel.
func withSpecs(t *testing.T, edit func(map[string]effectSpec)) {
	t.Helper()
	saved := effectSpecs
	effectSpecs = maps.Clone(saved)
	edit(effectSpecs)
	t.Cleanup(func() { effectSpecs = saved })
}

// Kills a per-kind exception to P3's deadlines in the executor: every kind,
// submitted as the pass submits it, runs under exactly the deadline
// admission gives its intent (a start startup_timeout + 10s, a row write
// 30s, every other effect 60s), and settles there.
func TestDeadlinesHaveNoException(t *testing.T) {
	clk := newFakePlannerClock(plannerT0)
	x, posted := fakeClockExecutor(clk)
	release := make(chan struct{})
	defer close(release)
	withSpecs(t, func(specs map[string]effectSpec) {
		for kind, spec := range specs {
			spec.body = func(context.Context, *effectPass, intent) settlement {
				<-release
				return settlement{Outcome: settledLanded}
			}
			specs[kind] = spec
		}
	})
	startup := 2 * time.Minute
	kinds := slices.Sorted(maps.Keys(effectSpecs))
	armed := make(map[time.Time]int)
	for i, kind := range kinds {
		want := providerEffectDeadline
		switch effectSpecs[kind].class {
		case capStarts:
			want = startup + 10*time.Second
		case capRowWrites:
			want = 30 * time.Second
		}
		if got := effectSpecs[kind].deadline(startup); got != want {
			t.Fatalf("%s: admission's deadline %v, want %v", kind, got, want)
		}
		if err := x.submitIntent(&effectPass{}, intent{Kind: kind, Key: rowKey{ID: kind}, Deadline: plannerT0.Add(want)}, uint64(i+1)); err != nil {
			t.Fatal(err)
		}
		armed[plannerT0.Add(want)] += 2
	}
	for at, n := range armed {
		waitTimersAt(t, clk, at, n)
	}
	clk.Advance(startup + 10*time.Second)
	for range kinds {
		if s := receive(t, posted); s.Cause != causeDeadline {
			t.Fatalf("settlement %+v, want every kind settled at its deadline", s)
		}
	}
}

// Kills a finalize settlement without the finalize prefix, so a stop
// verb's finalize would never wait on its own failures (v5.4 P4): a
// finalize that panics, has no effect, outlives its deadline, or whose own
// Run refuses or fails settles with causeFinalizePrefix on its cause, once,
// and admission then holds the finalize back. A landing carries no prefix.
func TestExecutorCausesCarryTheFinalizePrefix(t *testing.T) {
	clk := newFakePlannerClock(plannerT0)
	x, posted := fakeClockExecutor(clk)
	release := make(chan struct{})
	defer close(release)
	runs := map[string]settlement{
		"refuse":   {Outcome: settledRefused, Cause: "cas"},
		"prefixed": {Outcome: settledFailed, Cause: causeFinalizePrefix + "write-error"},
		"landed":   {Outcome: settledLanded},
	}
	withSpecs(t, func(specs map[string]effectSpec) {
		stop := specs[intentStop]
		stop.body = func(_ context.Context, _ *effectPass, it intent) settlement {
			if s, ok := runs[it.Key.ID]; ok {
				return s
			}
			if it.Key.ID == "panic" {
				panic("finalize exploded")
			}
			<-release
			return settlement{Outcome: settledLanded}
		}
		specs[intentStop] = stop
	})
	finalize := func(kind, id string) intent {
		return intent{Kind: kind, Key: rowKey{Leg: rowLeg, ID: id}, Finalize: true, Deadline: plannerT0.Add(time.Minute)}
	}
	for i, c := range []struct {
		it   intent
		want string
	}{
		{finalize(intentStop, "panic"), causeFinalizePrefix + causePanic},
		{finalize("drift-drain", "no-effect"), causeFinalizePrefix + causeNoEffect},
		{finalize(intentStop, "refuse"), causeFinalizePrefix + "cas"},
		{finalize(intentStop, "prefixed"), causeFinalizePrefix + "write-error"},
		{finalize(intentStop, "landed"), ""},
	} {
		if err := x.submitIntent(&effectPass{}, c.it, uint64(i+1)); err != nil {
			t.Fatal(err)
		}
		if s := receive(t, posted); s.Cause != c.want {
			t.Fatalf("%s: cause %q, want %q", c.it.Key.ID, s.Cause, c.want)
		}
	}
	if err := x.submitIntent(&effectPass{}, finalize(intentStop, "deadline"), 9); err != nil {
		t.Fatal(err)
	}
	waitTimersAt(t, clk, plannerT0.Add(time.Minute), 2)
	clk.Advance(time.Minute)
	s := receive(t, posted)
	if s.Cause != causeFinalizePrefix+causeDeadline {
		t.Fatalf("deadline: cause %q, want %q", s.Cause, causeFinalizePrefix+causeDeadline)
	}
	if (intent{Kind: intentStop, Finalize: true}).finalizesOnly(backoffRecord{Cause: s.Cause}) {
		t.Fatal("admission would not hold the finalize back on its own deadline")
	}
}

// Kills an effect context with no deadline (start-path code keys on
// DeadlineExceeded): Run's context carries the intent's deadline on the
// real clock, and on the fake clock ends with cause DeadlineExceeded.
func TestEffectContextCarriesTheDeadline(t *testing.T) {
	deadlines := make(chan time.Time, 1)
	x := newEffectExecutor(func(settlement) {}, io.Discard)
	far := time.Now().Add(time.Hour)
	if err := x.submit(rowKey{ID: "a"}, sessionEffect{Kind: intentRowHeal, Deadline: far, Run: func(ctx context.Context) settlement {
		d, _ := ctx.Deadline()
		deadlines <- d
		return settlement{Outcome: settledLanded}
	}}); err != nil {
		t.Fatal(err)
	}
	if d := <-deadlines; !d.Equal(far) {
		t.Fatalf("Run's deadline %v, want %v", d, far)
	}

	clk := newFakePlannerClock(plannerT0)
	fx, posted := fakeClockExecutor(clk)
	causes := make(chan error, 1)
	if err := fx.submit(rowKey{ID: "b"}, sessionEffect{Kind: intentRowHeal, Deadline: plannerT0.Add(time.Minute), Run: func(ctx context.Context) settlement {
		<-ctx.Done()
		causes <- context.Cause(ctx)
		return settlement{Outcome: settledFailed}
	}}); err != nil {
		t.Fatal(err)
	}
	waitTimersAt(t, clk, plannerT0.Add(time.Minute), 2)
	clk.Advance(time.Minute)
	receive(t, posted)
	if err := <-causes; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run's context ended with %v, want DeadlineExceeded", err)
	}
}

// Kills facts lost to a late landing, its drain transition among them: an
// effect that lands after the executor settled it at its deadline still has
// its facts posted, alone, and the drain applies them.
func TestExecutorPostsALateLandingsEvent(t *testing.T) {
	clk := newFakePlannerClock(plannerT0)
	x, posted := fakeClockExecutor(clk)
	release := make(chan struct{})
	ev := events.Event{Type: "session.test"}
	if err := x.submit(rowKey{ID: "a"}, sessionEffect{Kind: intentRowHeal, Deadline: plannerT0.Add(time.Minute), Run: func(context.Context) settlement {
		<-release
		return settlement{Outcome: settledLanded, Facts: effectFacts{Events: []events.Event{ev}, Transition: &drainTransition{Name: "s-a", Reason: "idle", Transition: "cancel"}}}
	}}); err != nil {
		t.Fatal(err)
	}
	waitTimersAt(t, clk, plannerT0.Add(time.Minute), 2)
	clk.Advance(time.Minute)
	if s := receive(t, posted); s.Cause != causeDeadline {
		t.Fatalf("settlement %+v, want failed at the deadline", s)
	}
	close(release)
	late := receive(t, posted)
	if len(late.Facts.Events) != 1 || late.Facts.Events[0].Type != ev.Type || late.Facts.Transition == nil || late.Key.ID != "" || late.Outcome != 0 {
		t.Fatalf("late post %+v, want its facts alone", late)
	}
	var recorded []string
	saved := recordDrainTransition
	recordDrainTransition = func(_ context.Context, name, _, transition string) {
		recorded = append(recorded, name+"/"+transition)
	}
	t.Cleanup(func() { recordDrainTransition = saved })
	rec := &memRecorder{}
	p := newPlanner(clk, func() time.Duration { return time.Minute }, nil, newInflightMap(), nil, io.Discard)
	p.rec = rec
	p.settlements.post(late)
	p.drainSettlements(plannerT0)
	if len(rec.events) != 1 || len(recorded) != 1 {
		t.Fatalf("drained late facts: events %v, transitions %v; want both applied", rec.events, recorded)
	}
}

// Kills an effect defined in a file the effect lint does not cover: the
// transaction, and every Decide and body in effectSpecs, are defined in
// reconcile_effect_*.go or reconcile_steps_*.go.
func TestEffectSpecFuncsAreLinted(t *testing.T) {
	linted := effectLintFiles(t)
	check := func(kind string, fn any) {
		pc := reflect.ValueOf(fn).Pointer()
		if file, _ := goruntime.FuncForPC(pc).FileLine(pc); !slices.Contains(linted, filepath.Base(file)) {
			t.Errorf("%s's effect is defined in %s, outside the effect lint", kind, file)
		}
	}
	check("every kind", runTx)
	n := 0
	for kind, spec := range effectSpecs {
		if spec.body != nil {
			check(kind, spec.body)
			n++
		}
		for _, sec := range spec.sections {
			check(kind, sec.Decide)
			n++
		}
	}
	if n == 0 {
		t.Fatal("no kind has an effect")
	}
}
