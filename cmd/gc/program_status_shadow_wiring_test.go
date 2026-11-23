package main

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
)

// The shadow decision points the 24 h shadow found silent on the paths
// production takes (ga-f4ovjm.69): the op lane under [ops] authoritative,
// and the v2 reconciler's idle sleeps.

// shadowWorking arms f's session name under the shadow switch with a live
// working root and, when sink, hands f's comparisons to the returned list;
// without sink f's leaf is not shadowing, as under the switch off.
func shadowWorking(f *runtime.Fake, name string, sink bool) func() []runtime.ProgramStatusComparison {
	f.SetProgramPane(name, runtime.ProgramPaneRow{
		Mode: runtime.ProgramStatusShadow, CaptureGate: true, PaneID: "%3", PanePipe: true, Tap: "r1.%3",
		TapLiveness: runtime.ProgramTapAlive, RecordForm: runtime.ProgramRecordDecoded,
		Record: runtime.ProgramStatus{State: runtime.ProgramWorking, Nonce: "r1.%3", Seq: 2, TurnStarts: 1, Progress: -1, Reported: true, Since: time.Now()},
	})
	var mu sync.Mutex
	var got []runtime.ProgramStatusComparison
	if sink {
		f.ProgramShadowSink = func(c runtime.ProgramStatusComparison) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, c)
		}
	}
	return func() []runtime.ProgramStatusComparison {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}

// shadowTurnEnded ends the turn shadowWorking's record reports on f's
// session name: the record's next report, idle at an input boundary.
func shadowTurnEnded(f *runtime.Fake, name string) {
	row, _ := f.ProgramPane(name)
	row.Record.State, row.Record.Seq, row.Record.TurnEnds = runtime.ProgramIdle, 3, 1
	f.SetProgramPane(name, row)
}

// atPoint is the comparisons in got at point.
func atPoint(got []runtime.ProgramStatusComparison, point runtime.ProgramStatusPoint) []runtime.ProgramStatusComparison {
	var out []runtime.ProgramStatusComparison
	for _, c := range got {
		if c.Point == point {
			out = append(out, c)
		}
	}
	return out
}

// Kills an op-lane delivery whose wait-idle decisions go unrecorded
// (ga-esmigw): with [ops] authoritative listing message, a wait-idle nudge
// is a lane op, which takes neither the worker's pre-read nor the leaf's
// WaitForIdle. The lane's screen reading before a boundary delivery is its
// pre-read (wait_idle_preread), and the delivery at the boundary it waited
// for is its idle wait returning (wait_for_idle); a look whose screen shows
// a turn records its pre-read busy and delivers nothing. The lane decides as
// with the switch off, which records nothing.
func TestLaneRecordsItsWaitIdleDecisions(t *testing.T) {
	pass := func(t *testing.T, shadow, turn bool) ([]runtime.ProgramStatusComparison, opjournal.PassResult, int) {
		t.Helper()
		f, deps, st := newScreenTurnFixture(t, "claude")
		got := shadowWorking(f.sp, f.name, shadow)
		f.showTurn(turn)
		f.sp.SetActivity(f.name, f.clk.Now().Add(-3400*time.Millisecond))
		l := f.lane(t, st)
		f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "please run the tests"))
		v, row := f.view(t)
		e := newConversationLaneExec(deps, row)
		quiescenceOnFixtureClock(f, e)
		res, err := l.Pass(context.Background(), v, e)
		if err != nil {
			t.Fatalf("Pass: %v", err)
		}
		return got(), res, len(f.nudges())
	}
	for _, c := range []struct {
		name     string
		turn     bool
		preread  runtime.ProgramVerdict
		waits    int
		delivers int
	}{
		{"at a boundary", false, runtime.ProgramVerdictIdle, 1, 1},
		{"a turn on screen", true, runtime.ProgramVerdictBusy, 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			offGot, offRes, offNudges := pass(t, false, c.turn)
			got, res, nudges := pass(t, true, c.turn)
			if res.Done != offRes.Done || nudges != offNudges || nudges != c.delivers {
				t.Fatalf("shadow pass %+v delivered %d, off %+v delivered %d; want the same, %d deliveries", res, nudges, offRes, offNudges, c.delivers)
			}
			if len(offGot) != 0 {
				t.Fatalf("the switch off recorded %+v", offGot)
			}
			pre := atPoint(got, runtime.ProgramPointWaitIdlePreread)
			if len(pre) == 0 || pre[0].Scrape != c.preread || pre[0].Program != runtime.ProgramVerdictBusy || pre[0].Family != "claude" {
				t.Fatalf("pre-reads %+v, want wait_idle_preread %s/busy for claude", pre, c.preread)
			}
			waits := atPoint(got, runtime.ProgramPointWaitForIdle)
			if len(waits) != c.waits {
				t.Fatalf("idle waits %+v, want %d", waits, c.waits)
			}
			for _, w := range waits {
				if w.Scrape != runtime.ProgramVerdictIdle || w.Program != runtime.ProgramVerdictBusy {
					t.Fatalf("idle wait %+v, want wait_for_idle idle/busy", w)
				}
			}
		})
	}
}

// Kills a legacy wait-idle nudge whose busy pre-read goes unrecorded: the
// CLI queues a busy Claude target before the worker's pre-read runs, so the
// CLI's reading is the pre-read (wait_idle_preread). The nudge is queued as
// with the switch off, which records nothing.
func TestLegacyWaitIdleBusyPrereadIsRecorded(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	prev := startNudgePoller
	startNudgePoller = func(string, string, string) error { return nil }
	t.Cleanup(func() { startNudgePoller = prev })
	for _, shadow := range []bool{false, true} {
		fake := runtime.NewFake()
		if err := fake.Start(context.Background(), "sess-worker", runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		fake.SetActivity("sess-worker", time.Now())
		got := shadowWorking(fake, "sess-worker", shadow)
		target := nudgeTarget{cityPath: t.TempDir(), agent: config.Agent{Name: "worker"}, resolved: &config.ResolvedProvider{Name: "claude"}, sessionName: "sess-worker"}
		var stdout, stderr bytes.Buffer
		if code := deliverSessionNudgeWithProvider(target, runtime.FullFake{Fake: fake}, nudgeDeliveryWaitIdle, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "Queued nudge for worker") {
			t.Fatalf("shadow %v: deliver = %d, stdout %q, stderr %q; want queued", shadow, code, stdout.String(), stderr.String())
		}
		all := got()
		pre := atPoint(all, runtime.ProgramPointWaitIdlePreread)
		switch {
		case !shadow && len(all) != 0:
			t.Fatalf("the switch off recorded %+v", all)
		case shadow && (len(pre) != 1 || pre[0].Scrape != runtime.ProgramVerdictBusy || pre[0].Program != runtime.ProgramVerdictBusy):
			t.Fatalf("shadow recorded %+v, want one wait_idle_preread busy/busy", all)
		}
	}
}

// Kills a v2 idle sleep that records no sleep proof (ga-f4ovjm.69): a
// non-interactive idle begin is decided without the idle proof, and the idle
// timeout's stop re-proves idleness on its own reads, so neither reaches
// provedIdle. Each records its decision beside the record's sleep proof
// (proved_idle), over a reading taken before its reads and one taken as it
// decides: a turn that ends while the fence reads leaves the record's proof
// busy. The effect settles as with the switch off, which records nothing.
func TestV2IdleSleepsRecordTheirProof(t *testing.T) {
	type outcome struct {
		s   settlement
		got []runtime.ProgramStatusComparison
	}
	begin := func(turnEnds bool) func(t *testing.T, shadow bool) outcome {
		return func(t *testing.T, shadow bool) outcome {
			p, it, _, leaf := admittedBegin(t, func(e *selectionEntry) { e.Desired, e.Reason = desireSleep, reasonIdleSleep })
			got := shadowWorking(leaf.Fake, "s-gc-1", shadow)
			if turnEnds {
				// The turn ends while the fence reads its attach leg: two
				// readings taken after the legs would both read it idle.
				leaf.OnAttachRead = func(name string) { shadowTurnEnded(leaf.Fake, name) }
			}
			return outcome{runBegin(p, it), got()}
		}
	}
	stop := func(active bool) func(t *testing.T, shadow bool) outcome {
		return func(t *testing.T, shadow bool) outcome {
			p, it, _, leaf := idleOn(t, false)
			if active {
				leaf.SetActivity("s-gc-1", time.Now())
			}
			got := shadowWorking(leaf.Fake, "s-gc-1", shadow)
			return outcome{runTx(context.Background(), p, it, effectSpecs[it.Kind], nil), got()}
		}
	}
	for _, c := range []struct {
		name   string
		run    func(*testing.T, bool) outcome
		scrape runtime.ProgramVerdict
		lands  bool
	}{
		{"a non-interactive idle begin", begin(false), runtime.ProgramVerdictIdle, true},
		{"a non-interactive idle begin whose turn ends during its reads", begin(true), runtime.ProgramVerdictIdle, true},
		{"an idle-timeout stop", stop(false), runtime.ProgramVerdictIdle, true},
		{"an idle-timeout stop active since the pass", stop(true), runtime.ProgramVerdictBusy, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			off, sh := c.run(t, false), c.run(t, true)
			if off.s.Outcome != sh.s.Outcome || off.s.Cause != sh.s.Cause || (sh.s.Outcome == settledLanded) != c.lands {
				t.Fatalf("shadow settled %+v, off %+v; want the same, landed %v", sh.s, off.s, c.lands)
			}
			if len(off.got) != 0 {
				t.Fatalf("the switch off recorded %+v", off.got)
			}
			proofs := atPoint(sh.got, runtime.ProgramPointProvedIdle)
			if len(proofs) != 1 || proofs[0].Scrape != c.scrape || proofs[0].Program != runtime.ProgramVerdictBusy {
				t.Fatalf("recorded %+v, want one proved_idle %s/busy", sh.got, c.scrape)
			}
		})
	}
}
