package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
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

// withAuthoredSuspension gives cfg the authored default of a variant: none,
// suspended_on_start on the city, or on a rig at rigPath.
func withAuthoredSuspension(cfg *config.City, variant, rigPath string) {
	switch variant {
	case "city suspended_on_start":
		cfg.Workspace.SuspendedOnStart = true
	case "rig suspended_on_start":
		cfg.Rigs = append(cfg.Rigs, config.Rig{Name: "parked", Path: rigPath, SuspendedOnStart: true})
	}
}

// authoredSuspensionVariants are the authored defaults an unreadable file
// must not fall back to.
var authoredSuspensionVariants = []string{"no authored default", "city suspended_on_start", "rig suspended_on_start"}

// AC-2: gather carries an unreadable suspension file as an error and
// suspends nothing on it, whatever the authored defaults: the city is not
// suspended, no rig is, and the live-claim veto's scope suspends no agent, so
// nothing drains. The readable control, where the same defaults apply, shows
// each variant suspends something when the file reads.
func TestGatherCarriesAnUnreadableSuspensionAsAnError(t *testing.T) {
	t.Setenv("GC_SUSPENDED", "")
	for _, variant := range authoredSuspensionVariants {
		t.Run(variant, func(t *testing.T) {
			rigPath := filepath.Join(t.TempDir(), "parked")
			f := newGatherFixture(t, poolRow("gc-1", "worker", 1, "active"))
			cfg := f.cur.Load().Cfg
			withAuthoredSuspension(cfg, variant, rigPath)

			f.env.Suspension = func() (suspensionstate.State, error) { return suspensionstate.State{}, errUnreadableSuspension }
			w := f.gather(t)
			if w.CitySuspended || len(w.SuspendedRigPaths) != 0 {
				t.Fatalf("CitySuspended=%t rigs=%v: an unreadable file must not suspend (it would drain)", w.CitySuspended, w.SuspendedRigPaths)
			}
			if !errors.Is(w.SuspensionErr, errUnreadableSuspension) {
				t.Fatalf("World.SuspensionErr = %v, want the reader's error", w.SuspensionErr)
			}
			for i := range cfg.Agents {
				if isAgentEffectivelySuspendedWith(cfg, f.env.CityPath, &cfg.Agents[i], w.Suspension) {
					t.Fatalf("agent %s is suspended in the pass's suspension state %+v", cfg.Agents[i].QualifiedName(), w.Suspension)
				}
			}

			f.env.Suspension = func() (suspensionstate.State, error) { return suspensionstate.State{}, nil }
			w = f.gather(t)
			switch variant {
			case "city suspended_on_start":
				if !w.CitySuspended {
					t.Fatal("control: a readable file with no override left the suspended_on_start city unsuspended")
				}
			case "rig suspended_on_start":
				if !w.SuspendedRigPaths[filepath.Clean(rigPath)] {
					t.Fatalf("control: suspended rigs %v, want the suspended_on_start rig", w.SuspendedRigPaths)
				}
			}
		})
	}
}

// AC-2: the pass's suspension state after a failed read is an explicit
// resume of the city and every rig, so neither the file's lost overrides nor
// the authored suspended_on_start defaults suspend anything, while
// GC_SUSPENDED and an agent's own suspended flag still do. A read that
// succeeded is passed through.
func TestSuspensionForPassSuspendsNothingAfterAFailedRead(t *testing.T) {
	t.Setenv("GC_SUSPENDED", "")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "c", SuspendedOnStart: true},
		Rigs:      []config.Rig{{Name: "parked", Path: "/rigs/parked", SuspendedOnStart: true}},
		Agents:    []config.Agent{{Name: "polecat", Dir: "parked"}, {Name: "frozen", Suspended: true}},
	}
	st := suspensionForPass(cfg, suspensionstate.State{}, errUnreadableSuspension)
	if effectiveCitySuspended(cfg, st) || len(suspendedRigPathsWithState(cfg, st)) != 0 {
		t.Fatalf("after a failed read: city suspended %t, rigs %v, want neither", effectiveCitySuspended(cfg, st), suspendedRigPathsWithState(cfg, st))
	}
	if isAgentEffectivelySuspendedWith(cfg, "/city", &cfg.Agents[0], st) {
		t.Fatal("after a failed read: the suspended_on_start rig's agent is suspended")
	}
	if !isAgentEffectivelySuspendedWith(cfg, "/city", &cfg.Agents[1], st) {
		t.Fatal("after a failed read: an agent suspended in config is no longer suspended")
	}
	t.Setenv("GC_SUSPENDED", "1")
	if !effectiveCitySuspended(cfg, st) {
		t.Fatal("after a failed read: GC_SUSPENDED=1 no longer suspends the city")
	}
	t.Setenv("GC_SUSPENDED", "")

	read := suspensionstate.State{Rigs: map[string]suspensionstate.Override{"parked": {}}}
	if got := suspensionForPass(cfg, read, nil); !reflect.DeepEqual(got, read) {
		t.Fatalf("after a good read: %+v, want the state read %+v", got, read)
	}
}

// AC-2 (legacy): an unreadable suspension file suspends nothing the legacy
// desired-state build decides. A city or rig whose authored default is
// suspended_on_start = true still demands its sessions, whose starts the
// pass holds, where the zero State the failed read leaves would have emptied
// them and drained the fleet. The readable controls show the default is what
// empties them otherwise.
func TestUnreadableSuspensionSuspendsNothingInTheLegacyDesiredState(t *testing.T) {
	t.Setenv("GC_SUSPENDED", "")
	const unreadable = `{"city":{"susp`
	t.Run("city suspended_on_start", func(t *testing.T) {
		cfg := workerCity(1)
		cfg.Agents[0].ScaleCheck = printCountCommand(1)
		cfg.Workspace.SuspendedOnStart = true
		for _, tc := range []struct {
			file string
			want int
		}{{file: unreadable, want: 1}, {file: `{}`, want: 0}, {file: `{"city":{"suspended":false}}`, want: 1}} {
			cityPath := t.TempDir()
			writeSuspensionFile(t, cityPath, tc.file)
			ds := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
			if len(ds.State) != tc.want {
				t.Fatalf("suspension file %q: desired state size = %d, want %d", tc.file, len(ds.State), tc.want)
			}
		}
	})
	t.Run("rig suspended_on_start", func(t *testing.T) {
		for _, tc := range []struct {
			file   string
			probed bool
		}{{file: unreadable, probed: true}, {file: `{}`, probed: false}} {
			cityPath := t.TempDir()
			rigPath := filepath.Join(cityPath, "rigs", "parked")
			if err := os.MkdirAll(rigPath, 0o755); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(cityPath, "parked.probed")
			cfg := &config.City{
				Workspace: config.Workspace{Name: "test-city"},
				Rigs:      []config.Rig{{Name: "parked", Path: rigPath, SuspendedOnStart: true}},
				Agents:    []config.Agent{{Name: "beta", Dir: "parked", ScaleCheck: scaleCheckMarkerCommand(marker, 1)}},
			}
			writeSuspensionFile(t, cityPath, tc.file)
			ds := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), beads.NewMemStore(), io.Discard)
			if _, demanded := ds.ScaleCheckCounts["parked/beta"]; demanded != tc.probed {
				t.Fatalf("suspension file %q: the suspended_on_start rig's agent demanded = %t, want %t (counts %v)", tc.file, demanded, tc.probed, ds.ScaleCheckCounts)
			}
		}
	})
}

// AC-2 (legacy tick): an unreadable suspension file suspends nothing a legacy
// tick decides, so the running named session of a suspended_on_start city the
// operator resumed is neither drained nor slept while the pass holds its
// starts. The tick reads the file in its awake input and in its wake arm
// (loadSuspensionStateForPass). The readable controls show the explicit resume
// keeps the session, and the authored default drains it otherwise.
func TestUnreadableSuspensionDrainsNothingInALegacyTick(t *testing.T) {
	t.Setenv("GC_SUSPENDED", "")
	for _, tc := range []struct {
		name, file string
		drained    bool
	}{
		{name: "unreadable file", file: `{"city":{"susp`},
		{name: "readable explicit resume", file: `{"city":{"suspended":false}}`},
		{name: "readable, no override", file: `{}`, drained: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newReconcilerTestEnv(t)
			env.cfg = &config.City{
				Workspace:     config.Workspace{Name: "test-city", SuspendedOnStart: true},
				Agents:        []config.Agent{{Name: "worker", StartCommand: "test-cmd"}},
				NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
			}
			sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
			env.desiredState[sessionName] = TemplateParams{
				Command:                 "test-cmd",
				SessionName:             sessionName,
				TemplateName:            "worker",
				ConfiguredNamedIdentity: "worker",
				ConfiguredNamedMode:     "always",
			}
			session := env.createSessionBead(sessionName, "worker")
			env.setSessionMetadata(&session, map[string]string{
				namedSessionMetadataKey:      "true",
				namedSessionIdentityMetadata: "worker",
				namedSessionModeMetadata:     "always",
			})
			env.markSessionActive(&session)
			if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "test-cmd"}); err != nil {
				t.Fatalf("Start: %v", err)
			}
			writeSuspensionFile(t, env.city, tc.file)

			env.reconcile([]beads.Bead{session})

			ds := env.dt.get(session.ID)
			if tc.drained {
				if ds == nil || ds.reason != "suspended" {
					t.Fatalf("drain = %+v, want the authored default's suspend drain", ds)
				}
				return
			}
			if ds != nil {
				t.Fatalf("drain %+v of the resumed city's running session, want none\nstdout:\n%s\nstderr:\n%s", *ds, env.stdout.String(), env.stderr.String())
			}
			if !env.sp.IsRunning(sessionName) {
				t.Fatal("the resumed city's running session was stopped")
			}
			b, err := env.store.Get(session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if b.Status != "open" || b.Metadata["state"] != "awake" || b.Metadata["sleep_reason"] != "" {
				t.Fatalf("session status %q state %q sleep_reason %q, want it open and awake", b.Status, b.Metadata["state"], b.Metadata["sleep_reason"])
			}
		})
	}
}

// AC-2 (one-shot gc start): the pass a one-shot gc start runs holds every
// start over an unreadable suspension file, as a controller's pass does, and
// prints the file, the error and the remedy; a readable or missing file holds
// nothing and prints nothing.
func TestOneShotStartHoldsOnAnUnreadableSuspensionFile(t *testing.T) {
	for _, tc := range []struct {
		name, file  string
		write, held bool
	}{
		{name: "unreadable", file: `{"city":{"susp`, write: true, held: true},
		{name: "readable", file: `{}`, write: true},
		{name: "missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			if tc.write {
				writeSuspensionFile(t, cityPath, tc.file)
			}
			var stderr strings.Builder
			h := oneShotStartHold(cityPath, &stderr)
			if !tc.held {
				if h.held() || stderr.Len() != 0 {
					t.Fatalf("hold %+v stderr %q, want no hold and no output", h, stderr.String())
				}
				return
			}
			if h != (startHold{Cause: causeSuspensionUnreadable}) {
				t.Fatalf("hold %+v, want cause %q without trickle", h, causeSuspensionUnreadable)
			}
			for _, want := range []string{"gc start", citylayout.SuspensionStateFile(cityPath), "gc suspend"} {
				if !strings.Contains(stderr.String(), want) {
					t.Fatalf("stderr %q does not name %q", stderr.String(), want)
				}
			}
		})
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

// AC-2 (v2): the pass holds the create while the file is unreadable, even in
// a city whose authored default is suspended_on_start = true (which the
// failed read must not fall back to), and the first pass after the file
// reads again admits it.
func TestPassHoldsStartsWhileTheSuspensionFileIsUnreadable(t *testing.T) {
	t.Setenv("GC_SUSPENDED", "")
	resumed := false
	for _, tc := range []struct {
		name             string
		suspendedOnStart bool
		parsed           suspensionstate.State
	}{
		{name: "no authored default"},
		{name: "city suspended_on_start", suspendedOnStart: true, parsed: suspensionstate.State{City: suspensionstate.Override{Suspended: &resumed}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGatherFixture(t, routedDemandBead("gc-r1"))
			f.cur.Load().Cfg.Workspace.SuspendedOnStart = tc.suspendedOnStart
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
			f.env.Suspension = func() (suspensionstate.State, error) { return tc.parsed, nil }
			rec = f.passRecord(t)
			var created int
			for _, it := range rec.Admitted {
				if it.Kind == intentCreate {
					created++
				}
			}
			if created != 1 {
				t.Fatalf("after the file reads: admitted %v deferred %+v, want the create admitted", intentKeys(rec.Admitted), rec.Deferred)
			}
		})
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
