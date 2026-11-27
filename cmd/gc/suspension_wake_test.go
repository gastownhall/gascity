package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// handWrittenCitySuspension is the brake an operator writes from any shell.
const handWrittenCitySuspension = `{"city":{"suspended":true}}`

// writeSuspensionFile writes raw as the city's suspension file, as an
// operator would by hand with no gc process running.
func writeSuspensionFile(t *testing.T, cityPath, raw string) {
	t.Helper()
	p := citylayout.SuspensionStateFile(cityPath)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
}

// AC-1: a suspension file hand-written as exactly {"city":{"suspended":true}}
// holds every wake path the controller has: the wake read both reconcilers
// and the orders lane use, v2's gather, legacy's desired-state build (which
// then wants no session at all), and the order pass.
func TestHandWrittenCitySuspensionHoldsEveryControllerWakePath(t *testing.T) {
	t.Setenv("GC_SUSPENDED", "")
	cr, _ := newPhaseFixtureRuntime(t, false, false)
	writeSuspensionFile(t, cr.cityPath, handWrittenCitySuspension)

	st, err := readSuspensionForWake(cr.cityPath)
	if err != nil {
		t.Fatalf("readSuspensionForWake: %v", err)
	}
	if !effectiveCitySuspended(cr.cfg, st) {
		t.Fatal("the wake read does not see the hand-written city suspension")
	}

	host := cr.newPlannerHost()
	gst, gerr := host.gather.Suspension()
	if gerr != nil || !effectiveCitySuspended(cr.cfg, gst) {
		t.Fatalf("v2 gather's suspension read = (%+v, %v), want the city suspended", gst, gerr)
	}

	got := buildDesiredStateWithSessionBeadsAtOpts(cr.cityName, cr.cityPath, time.Now(), time.Now(), cr.cfg, cr.sp, nil, nil, nil, nil, io.Discard, desiredStateOptions{readOnly: true})
	if len(got.State) != 0 {
		t.Fatalf("legacy desired state under a hand-written suspension = %v, want none", got.State)
	}

	od := &countingOrderDispatcher{}
	cr.od = od
	cr.wispIndexMigrationApplied = true
	cr.dispatchOrdersLocked(context.Background(), cr.cityPath, 0, cr.cfg)
	if od.dispatches != 0 {
		t.Fatalf("dispatched %d order pass(es) under a hand-written suspension", od.dispatches)
	}
}

// AC-1, legacy: with the city suspended on disk, buildDesiredState (the
// source the legacy start wave plans from) materializes nothing. The
// unsuspended control proves the same city otherwise demands one session,
// so the brake is what empties the result.
func TestSuspendedCityBrakesLegacyDesiredState(t *testing.T) {
	t.Setenv("GC_SUSPENDED", "")
	cfg := workerCity(1)
	cfg.Agents[0].ScaleCheck = printCountCommand(1)

	t.Run("control: unsuspended city demands its pool", func(t *testing.T) {
		ds := buildDesiredState("test-city", t.TempDir(), time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
		if len(ds.State) != 1 {
			t.Fatalf("unsuspended desired state size = %d, want 1", len(ds.State))
		}
	})
	t.Run("suspended city demands nothing", func(t *testing.T) {
		cityPath := t.TempDir()
		writeSuspensionFile(t, cityPath, handWrittenCitySuspension)
		ds := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
		if len(ds.State) != 0 {
			t.Fatalf("suspended desired state size = %d, want 0: %#v", len(ds.State), ds.State)
		}
	})
}

// errUnreadableSuspension stands in for a suspension file that does not
// parse.
var errUnreadableSuspension = errors.New("reading suspension-state.json: unexpected end of JSON input")

// AC-2: gather carries an unreadable suspension file as an error and keeps
// the zero State, so the city is not suspended and nothing drains on it.
func TestGatherCarriesAnUnreadableSuspensionAsAnError(t *testing.T) {
	f := newGatherFixture(t, poolRow("gc-1", "worker", 1, "active"))
	f.env.Suspension = func() (suspensionstate.State, error) { return suspensionstate.State{}, errUnreadableSuspension }
	w := f.gather(t)
	if w.CitySuspended || len(w.SuspendedRigPaths) != 0 {
		t.Fatalf("CitySuspended=%t rigs=%v: an unreadable file must not suspend (it would drain)", w.CitySuspended, w.SuspendedRigPaths)
	}
	if !errors.Is(w.SuspensionErr, errUnreadableSuspension) {
		t.Fatalf("World.SuspensionErr = %v, want the reader's error", w.SuspensionErr)
	}
}

// AC-2: an unreadable suspension file holds every start and create with cause
// suspension-unreadable and nothing else; with no hold the same intents admit.
func TestAdmitStartHoldDefersOnlyStartsAndCreates(t *testing.T) {
	intents := func() []intent {
		return []intent{startOn(1, "", 0), poolCreate("w", ""), rowIntent(intentStop, 2), rowIntent(intentDrainBegin, 3)}
	}
	in := admitIn(5, 4)
	in.Hold = decideStartHold(startHoldInput{SuspensionErr: errUnreadableSuspension})
	res := admit(in, intents())
	causes := causesOf(res.Deferred)
	if len(res.Admitted) != 2 || causes["start:gc-1"] != causeSuspensionUnreadable || causes["create:"] != causeSuspensionUnreadable {
		t.Fatalf("admitted %v deferred %v: want the stop and drain through, the start and create held", keysOf(res.Admitted), causes)
	}
	if res.Bucket.Tokens != in.Bucket.Tokens {
		t.Fatalf("bucket tokens %v -> %v: a held start spent a wake token", in.Bucket.Tokens, res.Bucket.Tokens)
	}
	if res := admit(admitIn(5, 4), intents()); len(res.Admitted) != 4 {
		t.Fatalf("no hold: admitted %v deferred %v, want all four", keysOf(res.Admitted), causesOf(res.Deferred))
	}
}

// AC-2: decideStartHold holds on an unreadable file, never trickles it, and
// holds nothing on a readable one.
func TestDecideStartHoldOnSuspensionError(t *testing.T) {
	if h := decideStartHold(startHoldInput{SuspensionErr: errUnreadableSuspension}); h != (startHold{Cause: causeSuspensionUnreadable}) {
		t.Fatalf("unreadable file: hold %+v, want cause %q without trickle", h, causeSuspensionUnreadable)
	}
	if h := decideStartHold(startHoldInput{}); h != (startHold{}) {
		t.Fatalf("readable file: hold %+v, want none", h)
	}
}

// AC-2 (v2): the pass holds the create while the file is unreadable, and the
// first pass after the file reads again admits it.
func TestPassHoldsStartsWhileTheSuspensionFileIsUnreadable(t *testing.T) {
	f := newGatherFixture(t, routedDemandBead("gc-r1"))
	f.env.Suspension = func() (suspensionstate.State, error) { return suspensionstate.State{}, errUnreadableSuspension }
	rec := f.passRecord(t)
	if len(rec.Admitted) != 0 {
		t.Fatalf("admitted %v under the hold, want nothing", intentKeys(rec.Admitted))
	}
	var held int
	for _, it := range rec.Deferred {
		if it.Kind == intentCreate && it.Cause == causeSuspensionUnreadable {
			held++
		}
	}
	if held != 1 {
		t.Fatalf("deferred %+v, want the create held with cause %q", rec.Deferred, causeSuspensionUnreadable)
	}
	f.env.Suspension = func() (suspensionstate.State, error) { return suspensionstate.State{}, nil }
	rec = f.passRecord(t)
	var created int
	for _, it := range rec.Admitted {
		if it.Kind == intentCreate {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("after the file parses: admitted %v deferred %+v, want the create admitted", intentKeys(rec.Admitted), rec.Deferred)
	}
}

// AC-2 (legacy): the start hold defers every candidate before any start, and
// logs each as deferred_by_suspension_unreadable.
func TestLegacyStartHoldDefersEveryCandidate(t *testing.T) {
	e := newPrepareBudgetEnv(t, "a", "b")
	a := e.candidate(t, "a", time.Second)
	b := e.candidate(t, "b", time.Second)
	hold := withStartHold(decideStartHold(startHoldInput{SuspensionErr: errUnreadableSuspension}))
	if woken := e.runWith([]startExecutionOption{hold}, false, a, b); woken != 0 {
		t.Fatalf("woken = %d, want 0 under the hold\n%s", woken, e.log.String())
	}
	for _, name := range []string{"a", "b"} {
		if n := e.sp.CountCalls("Start", name); n != 0 {
			t.Fatalf("%s started %d time(s) under the hold", name, n)
		}
	}
	if got := strings.Count(e.log.String(), string(TraceOutcomeDeferredBySuspensionUnreadable)); got != 2 {
		t.Fatalf("log names %s %d time(s), want 2\n%s", TraceOutcomeDeferredBySuspensionUnreadable, got, e.log.String())
	}
	if woken := e.runWith(nil, false, a, b); woken != 2 {
		t.Fatalf("no hold: woken = %d, want 2\n%s", woken, e.log.String())
	}
}

// AC-2: the controller's wake read of a malformed file prints exactly one
// stderr line naming the file, the error and the remedy, however many passes
// read it; the orders lane skips its pass on it; and once the file parses the
// read is clean and orders run again.
func TestCityRuntimeWakeSuspensionUnreadable(t *testing.T) {
	t.Setenv("GC_SUSPENDED", "")
	for name, raw := range map[string]string{"truncated": `{"city":{"susp`, "empty": ""} {
		t.Run(name, func(t *testing.T) {
			cr, _ := newPhaseFixtureRuntime(t, false, false)
			var stderr strings.Builder
			cr.stderr = &stderr
			od := &countingOrderDispatcher{}
			cr.od = od
			cr.wispIndexMigrationApplied = true
			writeSuspensionFile(t, cr.cityPath, raw)
			host := cr.newPlannerHost()
			for i := 0; i < 3; i++ {
				if _, err := cr.wakeSuspension(); err == nil {
					t.Fatal("wake read of a malformed file returned no error")
				}
				if _, err := host.gather.Suspension(); err == nil {
					t.Fatal("the planner's gather read of a malformed file returned no error")
				}
				cr.dispatchOrdersLocked(context.Background(), cr.cityPath, 0, cr.cfg)
			}
			if od.dispatches != 0 {
				t.Fatalf("dispatched %d order pass(es) over an unreadable suspension file", od.dispatches)
			}
			lines := strings.Split(strings.TrimRight(stderr.String(), "\n"), "\n")
			if len(lines) != 1 {
				t.Fatalf("stderr has %d lines, want exactly one:\n%s", len(lines), stderr.String())
			}
			for _, want := range []string{citylayout.SuspensionStateFile(cr.cityPath), "JSON input", "gc suspend", "remove the file"} {
				if !strings.Contains(lines[0], want) {
					t.Fatalf("stderr line %q does not name %q", lines[0], want)
				}
			}
			writeSuspensionFile(t, cr.cityPath, `{}`)
			if _, err := cr.wakeSuspension(); err != nil {
				t.Fatalf("wake read after the file parses: %v", err)
			}
			cr.dispatchOrdersLocked(context.Background(), cr.cityPath, 0, cr.cfg)
			if od.dispatches != 1 {
				t.Fatalf("dispatches after the file parses = %d, want 1", od.dispatches)
			}
		})
	}
}
