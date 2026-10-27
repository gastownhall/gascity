package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/guard"
	"github.com/gastownhall/gascity/internal/session"
)

// testRestartToken is the caller's GC_INSTANCE_TOKEN in unit-level
// rowFirstRestart tests.
const testRestartToken = "tok-test"

// testRowFirstRestart is a rowFirstRestart over fakes: the row record and the
// poke succeed unless a test replaces them.
func testRowFirstRestart(verb, sessionName string, hint drainOps, stderr io.Writer) rowFirstRestart {
	if stderr == nil {
		stderr = io.Discard
	}
	return rowFirstRestart{
		verb:        verb,
		sessionName: sessionName,
		display:     sessionName,
		token:       testRestartToken,
		record:      func(string) error { return nil },
		hint:        hint,
		poke:        func(string, reconcilekey.Key) error { return nil },
		stderr:      stderr,
	}
}

// fencedRecorder records through session.Manager.RequestFreshRestartFenced
// on store over sp, the write the worker boundary makes.
func fencedRecorder(t *testing.T, store beads.Store, sp runtime.Provider, id string) func(string) error {
	t.Helper()
	mgr := session.NewManagerWithOptions(store, sp, session.WithCityPath(t.TempDir()))
	return func(token string) error { return mgr.RequestFreshRestartFenced(id, token) }
}

// restartVerbs are the two in-session restart commands, run end to end
// against the explicit-restart fixture.
var restartVerbs = []struct {
	name, verb string
	run        func(stdout, stderr *bytes.Buffer) int
}{
	{"request-restart", "gc runtime request-restart", func(stdout, stderr *bytes.Buffer) int {
		return cmdRuntimeRequestRestart(stdout, stderr)
	}},
	{"self handoff", "gc handoff", func(stdout, stderr *bytes.Buffer) int {
		return cmdHandoffWithForce([]string{"context cycle", "resume from the brief"}, "", false, "", false, stdout, stderr)
	}},
}

// restartHint reads the runtime hint the command left on the fixture.
func restartHint(t *testing.T, fixture explicitRestartFixture) string {
	t.Helper()
	v, err := fixture.provider.GetMeta(fixture.sessionKey, "GC_RESTART_REQUESTED")
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	return v
}

func assertRestartNotRecorded(t *testing.T, fixture explicitRestartFixture) {
	t.Helper()
	got, err := fixture.store.Get(fixture.sessionID)
	if err != nil {
		t.Fatalf("get session bead: %v", err)
	}
	if got.Metadata["restart_requested"] != "" || got.Metadata["continuation_reset_pending"] != "" {
		t.Fatalf("row = %v, want no restart recorded", got.Metadata)
	}
	if hint := restartHint(t, fixture); hint != "" {
		t.Fatalf("GC_RESTART_REQUESTED = %q, want unset when nothing was recorded", hint)
	}
}

// TestRowFirstRestartDurableWriteFailedPokeExitsZero (ruling condition 4): a
// durable row write followed by a failed poke exits 0 with the pinned
// warning, and the runtime hint is set (local mode).
func TestRowFirstRestartDurableWriteFailedPokeExitsZero(t *testing.T) {
	for _, v := range restartVerbs {
		t.Run(v.name, func(t *testing.T) {
			fixture := newExplicitRestartFixture(t)
			var stdout, stderr bytes.Buffer
			if code := v.run(&stdout, &stderr); code != 0 {
				t.Fatalf("exit = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			assertExplicitRestartPersisted(t, fixture)
			if restartHint(t, fixture) == "" {
				t.Fatal("GC_RESTART_REQUESTED unset, want the local-mode hint after the row write")
			}
			want := v.verb + ": warning: restart recorded; controller not notified, it will act on its next pass: "
			if !strings.HasPrefix(stderr.String(), want) {
				t.Fatalf("stderr = %q, want prefix %q", stderr.String(), want)
			}
			if strings.Contains(stderr.String(), explicitRestartToken) || strings.Contains(stdout.String(), explicitRestartToken) {
				t.Fatal("output carries the instance token")
			}
		})
	}
}

// TestRowFirstRestartFailedWriteExitsNonZeroWritesNoMeta (ruling condition
// 4): when the row write is refused, the command exits non-zero, writes
// nothing to provider meta, records no event and dials no controller.
func TestRowFirstRestartFailedWriteExitsNonZeroWritesNoMeta(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(t *testing.T, fixture explicitRestartFixture)
		wantStderr string
	}{
		{"another incarnation", func(t *testing.T, _ explicitRestartFixture) {
			t.Setenv("GC_INSTANCE_TOKEN", "tok-dead-incarnation")
		}, "restart not recorded: this process does not run in session worker's current incarnation (neither its row nor its live runtime carries this process's GC_INSTANCE_TOKEN); from outside the session, gc session reset worker restarts it"},
		{"no instance token", func(t *testing.T, _ explicitRestartFixture) {
			t.Setenv("GC_INSTANCE_TOKEN", "")
		}, "restart not recorded: GC_INSTANCE_TOKEN is not set"},
	}
	for _, v := range restartVerbs {
		for _, tc := range cases {
			t.Run(v.name+"/"+tc.name, func(t *testing.T) {
				fixture := newExplicitRestartFixture(t)
				tc.setup(t, fixture)
				accepted := startFakeControllerSocket(t, fixture.cityPath, "ok\n")
				dials := countSocketDials(t)
				var stdout, stderr bytes.Buffer
				if code := v.run(&stdout, &stderr); code != 1 {
					t.Fatalf("exit = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
				}
				assertRestartNotRecorded(t, fixture)
				assertNoControllerReached(t, dials, accepted)
				if !strings.Contains(stderr.String(), v.verb+": ") || !strings.Contains(stderr.String(), tc.wantStderr) {
					t.Fatalf("stderr = %q, want %q", stderr.String(), tc.wantStderr)
				}
				if strings.Contains(stdout.String(), "requesting restart") || strings.Contains(stdout.String(), "Restart requested") {
					t.Fatalf("stdout = %q, promised a restart it did not record", stdout.String())
				}
			})
		}
	}
}

// TestRowFirstRestartNotRecordedRecordsNoEvent pins the unit-level half of
// condition 4: a refused row write records no session.draining.
func TestRowFirstRestartNotRecordedRecordsNoEvent(t *testing.T) {
	for _, err := range []error{session.ErrStaleRestartRequest, session.ErrSessionClosed, session.ErrRestartRequestContended, errors.New("store down")} {
		t.Run(err.Error(), func(t *testing.T) {
			dops := newFakeDrainOps()
			rec := events.NewFake()
			var stdout, stderr bytes.Buffer
			restart := testRowFirstRestart("gc runtime request-restart", "worker", dops, &stderr)
			restart.record = func(string) error { return err }
			poked := false
			restart.poke = func(string, reconcilekey.Key) error { poked = true; return nil }
			if code := doRuntimeRequestRestart(restart, rec, &stdout); code != 1 {
				t.Fatalf("exit = %d, want 1", code)
			}
			if len(rec.Events) != 0 || poked || dops.restartRequested["worker"] {
				t.Fatalf("events %v, poked %t, hint %t; want nothing after a refused row write", rec.Events, poked, dops.restartRequested["worker"])
			}
			if !strings.HasPrefix(stderr.String(), "gc runtime request-restart: restart not recorded: ") {
				t.Fatalf("stderr = %q, want the not-recorded diagnostic", stderr.String())
			}
		})
	}
}

// TestRowFirstRestartNotifiedLines pins the stdout line per notice.
func TestRowFirstRestartNotifiedLines(t *testing.T) {
	for _, tc := range []struct {
		name       string
		pokeErr    error
		wantStdout string
		wantStderr string
	}{
		{"notified", nil, "Restart requested; controller notified for immediate reconcile.\n", ""},
		{
			"worker site", errWorkerSiteNoController, "Restart requested; the remote controller restarts this session at its next reconcile.\n",
			"gc runtime request-restart: " + errWorkerSiteNoController.Error() + "\n",
		},
		{
			"poke failed", errors.New("dial refused"), "Restart requested; the controller restarts this session at its next pass.\n",
			"gc runtime request-restart: warning: restart recorded; controller not notified, it will act on its next pass: dial refused\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			restart := testRowFirstRestart("gc runtime request-restart", "worker", newFakeDrainOps(), &stderr)
			restart.poke = func(string, reconcilekey.Key) error { return tc.pokeErr }
			rec := events.NewFake()
			if code := doRuntimeRequestRestart(restart, rec, &stdout); code != 0 {
				t.Fatalf("exit = %d, want 0", code)
			}
			if stdout.String() != tc.wantStdout || stderr.String() != tc.wantStderr {
				t.Fatalf("stdout %q stderr %q; want %q and %q", stdout.String(), stderr.String(), tc.wantStdout, tc.wantStderr)
			}
			if len(rec.Events) != 1 || rec.Events[0].Type != events.SessionDraining {
				t.Fatalf("events = %v, want one session.draining", rec.Events)
			}
		})
	}
}

// rowCheckingHint fails the test unless the row already records the restart
// when the runtime hint is set.
type rowCheckingHint struct {
	*fakeDrainOps
	t     *testing.T
	store beads.Store
	id    string
}

func (h rowCheckingHint) setRestartRequested(name string) error {
	b, err := h.store.Get(h.id)
	if err != nil {
		h.t.Fatalf("Get: %v", err)
	}
	if b.Metadata["restart_requested"] != "true" {
		h.t.Fatal("the runtime hint was set before the row recorded the restart")
	}
	return h.fakeDrainOps.setRestartRequested(name)
}

// TestRowFirstRestartWritesTheRowBeforeTheHint pins the order: row, then hint.
func TestRowFirstRestartWritesTheRowBeforeTheHint(t *testing.T) {
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: map[string]string{
		"session_name": "worker", "state": "awake", "instance_token": testRestartToken,
	}})
	if err != nil {
		t.Fatal(err)
	}
	hint := rowCheckingHint{fakeDrainOps: newFakeDrainOps(), t: t, store: store, id: b.ID}
	restart := testRowFirstRestart("gc runtime request-restart", "worker", hint, nil)
	restart.record = fencedRecorder(t, store, runtime.NewFake(), b.ID)
	if code := doRuntimeRequestRestart(restart, events.Discard, io.Discard); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !hint.restartRequested["worker"] {
		t.Fatal("hint not set after the row write")
	}
}

// countingMetaProvider counts SetMeta calls.
type countingMetaProvider struct {
	runtime.Provider
	setMeta int
}

func (p *countingMetaProvider) SetMeta(name, key, value string) error {
	p.setMeta++
	return p.Provider.SetMeta(name, key, value)
}

// TestRowFirstRestartWorkerSiteSkipsMeta pins that a worker site writes no
// runtime hint: the request recorded on a worker site never calls the
// provider's SetMeta, and both verbs record the row there end to end.
func TestRowFirstRestartWorkerSiteSkipsMeta(t *testing.T) {
	for _, v := range restartVerbs {
		t.Run(v.name, func(t *testing.T) {
			cityDir, _ := newWorkerSiteSessionCity(t, workerSiteRestartSeeds[0].seed)
			t.Setenv("GC_ALIAS", "worker")
			t.Setenv("GC_SESSION_NAME", "worker-1")
			t.Setenv("GC_INSTANCE_TOKEN", workerSiteRestartToken)

			// The composed request on a worker site: restartHintOps over a
			// counting provider, then recordRestart.
			counting := &countingMetaProvider{Provider: runtime.NewFake()}
			restart := testRowFirstRestart(v.verb, "worker-1", restartHintOps(cityDir, counting), nil)
			restart.cityPath = cityDir
			if err := restart.recordRestart(); err != nil {
				t.Fatalf("recordRestart: %v", err)
			}
			if counting.setMeta != 0 {
				t.Fatalf("SetMeta calls = %d, want none on a worker site", counting.setMeta)
			}

			var stdout, stderr bytes.Buffer
			if code := v.run(&stdout, &stderr); code != 0 {
				t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
			}
			assertRestartMarkerRecorded(t, localSessionBeads(t, cityDir))
		})
	}
}

// TestRowFirstRestartRefusesWithoutToken pins that a request naming no
// incarnation is refused before the row is read or written.
func TestRowFirstRestartRefusesWithoutToken(t *testing.T) {
	restart := testRowFirstRestart("gc runtime request-restart", "worker", newFakeDrainOps(), nil)
	restart.token = " "
	called := false
	restart.record = func(string) error { called = true; return nil }
	if err := restart.recordRestart(); !errors.Is(err, session.ErrRestartRequestUnfenced) {
		t.Fatalf("recordRestart = %v, want ErrRestartRequestUnfenced", err)
	}
	if called {
		t.Fatal("the row was written without an instance token")
	}
}

// --- The reconciler matrix: {v2, legacy} x {local, worker site} ---

// simRowFirstRestart is the CLI's request against the v2 sim's row gc-1: the
// fenced write lands on the backing store, as another process's would, and
// reaches the controller's cache through the ledger event.
func simRowFirstRestart(t *testing.T, s *sim, token string, hint drainOps) rowFirstRestart {
	t.Helper()
	write := fencedRecorder(t, s.legs[0].backing, runtime.NewFake(), "gc-1")
	restart := testRowFirstRestart("gc runtime request-restart", "s-gc-1", hint, nil)
	restart.token = token
	restart.record = func(token string) error {
		if err := write(token); err != nil {
			return err
		}
		before := len(s.legs[0].events)
		s.audit("operator")
		if len(s.legs[0].events) > before {
			s.legs[0].cache.ApplyEvent("bead.updated", s.legs[0].events[len(s.legs[0].events)-1])
		}
		return nil
	}
	return restart
}

// TestRowFirstRestartV2Matrix drives the CLI's row-first request through the
// v2 reconciler, locally (row and hint) and on a worker site (row only, no
// meta call), then proves a late request from the dead incarnation is
// refused and leaves the successor untouched (condition 1).
func TestRowFirstRestartV2Matrix(t *testing.T) {
	for _, site := range []string{"local", "local, hint failed", "worker site"} {
		t.Run(site, func(t *testing.T) {
			s := converged(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-1", "session_key", "k-1", "started_config_hash", "h-1", "last_woke_at", rowAt(-time.Hour)), gc1Work())
			var hint drainOps
			switch site {
			case "local":
				hint = newDrainOps(s.sp)
			case "local, hint failed":
				failing := newFakeDrainOps()
				failing.err = errors.New("pane gone")
				hint = failing
			}
			if code := doRuntimeRequestRestart(simRowFirstRestart(t, s, "tok-1", hint), events.Discard, io.Discard); code != 0 {
				t.Fatalf("request-restart exit = %d, want 0", code)
			}
			for range 4 {
				s.settle()
				s.advance(simPatrol)
			}
			s.settle()
			b, err := s.legs[0].backing.Get("gc-1")
			if err != nil {
				t.Fatal(err)
			}
			s.sp.mu.Lock()
			starts, kills := len(s.sp.starts), len(s.sp.kills)
			marked := s.sp.rts["s-gc-1"] != nil && s.sp.rts["s-gc-1"].restart
			s.sp.mu.Unlock()
			m := b.Metadata
			if starts != 1 || kills != 1 || marked || m["instance_token"] == "tok-1" || m["generation"] == "1" || !s.livePane(b) || m["restart_requested"] != "" {
				t.Fatalf("starts %d, kills %d, marked %t, row %v; want one restart into a new incarnation", starts, kills, marked, m)
			}

			// The dead incarnation's late request is refused.
			if code := doRuntimeRequestRestart(simRowFirstRestart(t, s, "tok-1", hint), events.Discard, io.Discard); code != 1 {
				t.Fatalf("late request exit = %d, want 1", code)
			}
			for range 3 {
				s.settle()
				s.advance(simPatrol)
			}
			after, err := s.legs[0].backing.Get("gc-1")
			if err != nil {
				t.Fatal(err)
			}
			s.sp.mu.Lock()
			kills = len(s.sp.kills)
			s.sp.mu.Unlock()
			if kills != 1 || after.Metadata["instance_token"] != m["instance_token"] || after.Metadata["restart_requested"] != "" {
				t.Fatalf("kills %d, row %v; want the successor untouched", kills, after.Metadata)
			}
			s.noViolations(t)
		})
	}
}

// TestRowFirstRestartLegacyMatrix drives the CLI's row-first request through
// the legacy reconciler, whose drain ops read the runtime hint as the
// controller's do: locally (row and hint), with the hint failed, after a warm
// reuse (the row minted a new token in PreWake while the pane kept the
// caller's, so the live runtime proves the caller's incarnation), and on a
// worker site, where the provider refuses the runtime flag as the exec
// runtime does. Legacy restarts from the row marker alone (condition 3,
// option (a)); a late request from the dead incarnation is refused.
func TestRowFirstRestartLegacyMatrix(t *testing.T) {
	for _, site := range []string{"local", "local, hint failed", "local, warm reuse", "worker site"} {
		t.Run(site, func(t *testing.T) {
			env := newRestartRequestTestEnv(t)
			env.cfg = &config.City{
				Workspace:     config.Workspace{Name: "test-city"},
				Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
				NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
			}
			sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
			env.desiredState[sessionName] = TemplateParams{
				Command:          "true",
				SessionName:      sessionName,
				TemplateName:     "worker",
				ResolvedProvider: &config.ResolvedProvider{SessionIDFlag: "--session-id"},
			}
			row := env.createSessionBead(sessionName)
			env.setSessionMetadata(&row, map[string]string{
				namedSessionMetadataKey:      "true",
				namedSessionIdentityMetadata: "worker",
				namedSessionModeMetadata:     "on_demand",
				"state":                      "active",
				"session_key":                "original-key",
			})
			if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
				t.Fatalf("start session: %v", err)
			}
			if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", row.ID); err != nil {
				t.Fatalf("SetMeta: %v", err)
			}
			token := row.Metadata["instance_token"]
			if err := env.sp.SetMeta(sessionName, runtime.InstanceTokenEnv, token); err != nil {
				t.Fatalf("SetMeta: %v", err)
			}
			var hint drainOps
			switch site {
			case "local":
				hint = newDrainOps(env.sp)
			case "local, hint failed":
				failing := newFakeDrainOps()
				failing.err = errors.New("pane gone")
				hint = failing
			case "local, warm reuse":
				hint = newDrainOps(env.sp)
				env.setSessionMetadata(&row, map[string]string{"generation": "2", "instance_token": "tok-warm-reuse-mint"})
			case "worker site":
				env.sp.GetMetaErrors[sessionName] = map[string]error{"GC_RESTART_REQUESTED": guard.ErrControllerOwnedRuntime}
			}
			restart := testRowFirstRestart("gc runtime request-restart", sessionName, hint, nil)
			restart.token = token
			restart.record = fencedRecorder(t, env.store, env.sp, row.ID)
			if code := doRuntimeRequestRestart(restart, events.Discard, io.Discard); code != 0 {
				t.Fatalf("request-restart exit = %d, want 0", code)
			}
			if site == "local" && restartHintOf(t, env.sp, sessionName) == "" {
				t.Fatal("GC_RESTART_REQUESTED unset; the local cell must also leave the hint")
			}
			if site == "local, hint failed" && restartHintOf(t, env.sp, sessionName) != "" {
				t.Fatal("GC_RESTART_REQUESTED set; the hint-failed cell restarts from the row alone")
			}
			// The controller's drain ops read the runtime hint.
			controllerOps := newDrainOps(env.sp)

			got, err := env.store.Get(row.ID)
			if err != nil {
				t.Fatal(err)
			}
			env.reconcileWithDrainOps([]beads.Bead{got}, controllerOps)
			if env.sp.IsRunning(sessionName) {
				t.Fatalf("session %q still running; legacy must restart on the row marker", sessionName)
			}
			got, err = env.store.Get(row.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Metadata["state"] != "asleep" || got.Metadata["session_key"] == "original-key" {
				t.Fatalf("row %v, want the restart handoff applied", got.Metadata)
			}
			env.reconcileWithDrainOps([]beads.Bead{got}, controllerOps)
			if !env.sp.IsRunning(sessionName) {
				t.Fatalf("session %q not started again on the next tick", sessionName)
			}
			successor, err := env.store.Get(row.ID)
			if err != nil {
				t.Fatal(err)
			}
			if successor.Metadata["instance_token"] == token {
				t.Fatalf("successor kept the old token %q; want a new incarnation", token)
			}
			// The successor's runtime carries the token it started under, as
			// a tmux session's environment does.
			if err := env.sp.SetMeta(sessionName, runtime.InstanceTokenEnv, lastStartEnv(t, env.sp, sessionName)[runtime.InstanceTokenEnv]); err != nil {
				t.Fatalf("SetMeta: %v", err)
			}

			// The dead incarnation's late request is refused.
			if code := doRuntimeRequestRestart(restart, events.Discard, io.Discard); code != 1 {
				t.Fatalf("late request exit = %d, want 1", code)
			}
			after, err := env.store.Get(row.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Metadata["restart_requested"] != "" {
				t.Fatalf("late request recorded restart_requested on the successor: %v", after.Metadata)
			}
			env.reconcileWithDrainOps([]beads.Bead{after}, controllerOps)
			if !env.sp.IsRunning(sessionName) {
				t.Fatal("the successor was stopped by the dead incarnation's request")
			}
		})
	}
}

// restartHintOf reads name's runtime restart hint.
func restartHintOf(t *testing.T, sp runtime.Provider, name string) string {
	t.Helper()
	v, err := sp.GetMeta(name, "GC_RESTART_REQUESTED")
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	return v
}

// lastStartEnv is the environment of the fake's last Start of name.
func lastStartEnv(t *testing.T, sp *runtime.Fake, name string) map[string]string {
	t.Helper()
	calls := sp.SnapshotCalls()
	for i := len(calls) - 1; i >= 0; i-- {
		if c := calls[i]; c.Method == "Start" && c.Name == name {
			return c.Config.Env
		}
	}
	t.Fatalf("no Start of %q", name)
	return nil
}
