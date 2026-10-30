package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/rollout/gate"
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
	return func(token string) error { return mgr.RequestFreshRestartFenced(id, token, nil) }
}

// restartVerbs are the two in-session restart commands, run end to end
// against the explicit-restart fixture.
var restartVerbs = []struct {
	name, verb string
	sendsMail  bool // a recorded request also sends the handoff mail
	run        func(stdout, stderr *bytes.Buffer) int
}{
	{"request-restart", "gc runtime request-restart", false, func(stdout, stderr *bytes.Buffer) int {
		return cmdRuntimeRequestRestart("", stdout, stderr)
	}},
	{"self handoff", "gc handoff", true, func(stdout, stderr *bytes.Buffer) int {
		return handoffForTest([]string{"context cycle", "resume from the brief"}, "", false, stdout, stderr)
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
			wantMail := 0
			if v.sendsMail {
				wantMail = 1
			}
			if got := len(listOpenMessagesBothTiers(t, fixture.store)); got != wantMail {
				t.Fatalf("open messages = %d, want %d", got, wantMail)
			}
			want := v.verb + ": warning: restart recorded; controller not notified, it will act on its next pass: "
			if !strings.HasPrefix(withoutOpJournalConfigWarning(stderr.String()), want) {
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
// nothing to provider meta, records no event, dials no controller and sends
// no handoff mail. Neither diagnostic, the stale incarnation's nor that of a
// caller with no GC_INSTANCE_TOKEN, gives the refused caller a command that
// restarts the session: gc session reset is unfenced, so a left-over process
// that ran it would restart its successor.
func TestRowFirstRestartFailedWriteExitsNonZeroWritesNoMeta(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(t *testing.T, fixture explicitRestartFixture)
		wantStderr string
	}{
		{"another incarnation", func(t *testing.T, _ explicitRestartFixture) {
			t.Setenv("GC_INSTANCE_TOKEN", "tok-dead-incarnation")
		}, "restart not recorded: this process does not run in session worker's current incarnation (neither its row nor its live runtime carries this process's GC_INSTANCE_TOKEN); a process left over from an earlier incarnation must exit, and only an operator on the controller host can restart the session\n"},
		{"no instance token", func(t *testing.T, _ explicitRestartFixture) {
			t.Setenv("GC_INSTANCE_TOKEN", "")
		}, "restart not recorded: GC_INSTANCE_TOKEN is not set, so this process cannot prove which incarnation of session worker it runs in (an agent CLI that filters *TOKEN* variables from its shell removes it); keep GC_INSTANCE_TOKEN in the agent's environment, and only an operator on the controller host can restart the session\n"},
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
				if msgs := listOpenMessagesBothTiers(t, fixture.store); len(msgs) != 0 {
					t.Fatalf("open messages = %d, want no handoff mail when nothing is recorded", len(msgs))
				}
				if got, want := withoutOpJournalConfigWarning(stderr.String()), v.verb+": "+tc.wantStderr; got != want {
					t.Fatalf("stderr = %q, want %q", got, want)
				}
				if strings.Contains(stderr.String(), "gc session reset") {
					t.Fatalf("stderr = %q names gc session reset to the refused caller", stderr.String())
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
	return simRowFirstRestartVia(t, s, s.legs[0].backing, runtime.NewFake(), token, hint)
}

// simRowFirstRestartVia is simRowFirstRestart with the fenced write made
// over store and the runtime proof read from sp.
func simRowFirstRestartVia(t *testing.T, s *sim, store beads.Store, sp runtime.Provider, token string, hint drainOps) rowFirstRestart {
	t.Helper()
	write := fencedRecorder(t, store, sp, "gc-1")
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

// TestRowFirstRestartV2CityNeverTakesTheCheckedWrite pins the v2 owner's
// sign-off (2026-10-27, Q1): a v2 city never records a restart by the checked
// write. With the session store resolving no conditional writer (here the
// agent's environment turns conditional writes off), a city whose controller
// latched v2 refuses both verbs: exit 1, the row unwritten, no runtime hint,
// no handoff mail, the controller not poked (the stub stands in for the
// identify dial that answers the latched mode). The same store under a legacy
// controller still records by the checked write (condition 3).
func TestRowFirstRestartV2CityNeverTakesTheCheckedWrite(t *testing.T) {
	original := controllerLatchedReconcilerMode
	t.Cleanup(func() { controllerLatchedReconcilerMode = original })
	for _, v := range restartVerbs {
		for _, mode := range []reconcilerMode{reconcilerV2, reconcilerLegacy} {
			t.Run(v.name+"/"+mode.String(), func(t *testing.T) {
				fixture := newExplicitRestartFixture(t)
				t.Setenv("GC_BEADS_CONDITIONAL_WRITES", "off")
				asked := 0
				controllerLatchedReconcilerMode = func(string, *config.City) (reconcilerMode, bool) {
					asked++
					return mode, true
				}
				accepted := startFakeControllerSocket(t, fixture.cityPath, "ok\n")
				dials := countSocketDials(t)
				var stdout, stderr bytes.Buffer
				code := v.run(&stdout, &stderr)
				if asked == 0 {
					t.Fatal("the reconciler mode was never asked; a store without a conditional writer must ask it")
				}
				if mode == reconcilerLegacy {
					if code != 0 {
						t.Fatalf("legacy exit = %d, want 0 (the checked write); stderr=%q", code, stderr.String())
					}
					got, err := fixture.store.Get(fixture.sessionID)
					if err != nil {
						t.Fatal(err)
					}
					if got.Metadata["restart_requested"] != "true" || got.Metadata["continuation_reset_pending"] != "true" {
						t.Fatalf("legacy row = %v, want the restart recorded", got.Metadata)
					}
					return
				}
				if code != 1 {
					t.Fatalf("v2 exit = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
				}
				assertRestartNotRecorded(t, fixture)
				assertNoControllerReached(t, dials, accepted)
				if msgs := listOpenMessagesBothTiers(t, fixture.store); len(msgs) != 0 {
					t.Fatalf("open messages = %d, want no handoff mail when nothing is recorded", len(msgs))
				}
				if want := v.verb + ": restart not recorded: " + session.ErrRestartRequestNeedsConditionalWrite.Error() + "\n"; withoutOpJournalConfigWarning(stderr.String()) != want {
					t.Fatalf("stderr = %q, want %q", stderr.String(), want)
				}
			})
		}
	}
}

// TestRowFirstRestartRequireRefusesAnIncapableStore pins the other half of
// the sign-off's Q1 condition: under require, a session store that cannot
// fence refuses the fenced write, and the command exits 1 with no runtime
// hint, no event and no poke.
func TestRowFirstRestartRequireRefusesAnIncapableStore(t *testing.T) {
	store := beads.NewMemStore()
	if err := beads.StampOpenedStore(store, "MemStore", gate.Require, false, nil, nil); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	b, err := store.Create(beads.Bead{Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: map[string]string{
		"session_name": "worker", "state": "awake", "instance_token": testRestartToken,
	}})
	if err != nil {
		t.Fatal(err)
	}
	store.DisableConditionalWrites = true
	hint := newFakeDrainOps()
	var stderr bytes.Buffer
	restart := testRowFirstRestart("gc runtime request-restart", "worker", hint, &stderr)
	mgr := session.NewManagerWithOptions(store, runtime.NewFake(), session.WithCityPath(t.TempDir()))
	var recordErr error
	restart.record = func(token string) error {
		// casOnly is nil, as on a legacy city: only the require refusal can
		// refuse this write, so the v2 refusal cannot stand in for it.
		recordErr = mgr.RequestFreshRestartFenced(b.ID, token, nil)
		return recordErr
	}
	poked := false
	restart.poke = func(string, reconcilekey.Key) error { poked = true; return nil }
	rec := events.NewFake()
	if code := doRuntimeRequestRestart(restart, rec, io.Discard); code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%q", code, stderr.String())
	}
	if !beads.IsConditionalWritesRequired(recordErr) {
		t.Fatalf("record err = %v, want the require refusal", recordErr)
	}
	if want := "gc runtime request-restart: restart not recorded: " + recordErr.Error() + "\n"; stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
	if hint.restartRequested["worker"] || poked || len(rec.Events) != 0 {
		t.Fatalf("hint %t, poked %t, events %v; want nothing past the refused write", hint.restartRequested["worker"], poked, rec.Events)
	}
	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["restart_requested"] != "" || got.Metadata["continuation_reset_pending"] != "" {
		t.Fatalf("row = %v, want nothing recorded", got.Metadata)
	}
}

// TestRowFirstRestartWorkerSiteTakesTheCheckedWrite pins the worker-site
// exemption as current behavior: on a worker site restartRecordsOnlyByCAS
// answers false without asking the reconciler mode, so a session store with
// no conditional writer takes the checked write even under a v2 controller.
// It is safe today only because v2 refuses exec, k8s, ssh and pack runtimes
// (PAR-EXEC), so no v2 city has worker sites. Lifting PAR-EXEC must revisit
// this exemption: a v2 worker site would then take the checked write.
func TestRowFirstRestartWorkerSiteTakesTheCheckedWrite(t *testing.T) {
	original := controllerLatchedReconcilerMode
	t.Cleanup(func() { controllerLatchedReconcilerMode = original })
	for _, v := range restartVerbs {
		t.Run(v.name, func(t *testing.T) {
			cityDir, _ := newWorkerSiteSessionCity(t, workerSiteRestartSeeds[0].seed)
			t.Setenv("GC_ALIAS", "worker")
			t.Setenv("GC_SESSION_NAME", "worker-1")
			t.Setenv("GC_INSTANCE_TOKEN", workerSiteRestartToken)
			t.Setenv("GC_BEADS_CONDITIONAL_WRITES", "off")
			asked := 0
			controllerLatchedReconcilerMode = func(string, *config.City) (reconcilerMode, bool) {
				asked++
				return reconcilerV2, true
			}

			if restartRecordsOnlyByCAS(cityDir, nil)() {
				t.Fatal("restartRecordsOnlyByCAS = true on a worker site, want false (the exemption)")
			}
			var stdout, stderr bytes.Buffer
			if code := v.run(&stdout, &stderr); code != 0 {
				t.Fatalf("exit = %d, want 0 (the checked write); stderr=%s", code, stderr.String())
			}
			assertRestartMarkerRecorded(t, localSessionBeads(t, cityDir))
			if asked != 0 {
				t.Fatalf("reconciler mode asked %d times, want none on a worker site", asked)
			}
		})
	}
}

// TestRestartWorkerSiteExemptionNeedsV2ToRefuseRemoteRuntimes pins what keeps
// the worker-site exemption (TestRowFirstRestartWorkerSiteTakesTheCheckedWrite)
// inside the v2 owner's Q1 condition: a city that places sessions on worker
// sites, through the exec, a pack, the ssh, the k8s or the hybrid session
// runtime, never runs v2. An explicit v2 is refused, and an unset key falls
// back to legacy. When v2 admits one of these runtimes, this fails first:
// restartRecordsOnlyByCAS must then stop exempting worker sites, or a v2
// city's box takes the checked write.
func TestRestartWorkerSiteExemptionNeedsV2ToRefuseRemoteRuntimes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		runtime func(*config.City)
	}{
		{"exec", func(c *config.City) { c.Session.Provider = "exec:/opt/bin/runner" }},
		{"pack runtime", func(c *config.City) {
			c.Session.Provider = "cloudflare"
			c.Runtimes = map[string]config.DiscoveredRuntime{"cloudflare": {Name: "cloudflare", PackName: "runtime-cloudflare"}}
		}},
		{"ssh", func(c *config.City) { c.Session.Provider = "ssh:ops@box:22" }},
		{"k8s", func(c *config.City) { c.Session.Provider = "k8s" }},
		{"hybrid", func(c *config.City) { c.Session.Provider = "hybrid" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if mode, err := latchReconcilerMode(v2City(tc.runtime), noEnv); err == nil {
				t.Fatalf("an explicit v2 with the %s session runtime latches %s; a v2 city can then have worker sites, which take the checked restart write: revisit restartRecordsOnlyByCAS", tc.name, mode)
			}
			if mode, err := latchReconcilerMode(v2DefaultCity(tc.runtime), noEnv); err != nil || mode != reconcilerLegacy {
				t.Fatalf("an unset session_reconciler with the %s session runtime latches %s (err %v), want the legacy fallback; a v2 city can then have worker sites, which take the checked restart write: revisit restartRecordsOnlyByCAS", tc.name, mode, err)
			}
		})
	}
}

// simPaneIdentity reads a sim runtime's identity environment through
// GetMeta, as tmux's show-environment reads a pane's: the runtime-token
// proof's view of the live runtime.
type simPaneIdentity struct{ *simProvider }

func (p simPaneIdentity) GetMeta(name, key string) (string, error) {
	env, err := p.GetAllEnvironment(name)
	if err != nil {
		return "", err
	}
	if v, ok := env[key]; ok {
		return v, nil
	}
	return p.simProvider.GetMeta(name, key)
}

// casHookStore runs before once, at the first UpdateIfMatch, and then makes
// it: a write landing between the fenced request's deciding read and its CAS.
type casHookStore struct {
	beads.Store
	before func()
	fired  bool
}

func (w *casHookStore) ConditionalWritesModeSource() beads.Store { return w.Store }

func (w *casHookStore) cw() beads.ConditionalWriter {
	cw, _ := beads.ConditionalWriterFor(w.Store)
	return cw
}

func (w *casHookStore) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	if !w.fired {
		w.fired = true
		w.before()
	}
	return w.cw().UpdateIfMatch(id, rev, opts)
}

func (w *casHookStore) CloseIfMatch(id string, rev int64) error { return w.cw().CloseIfMatch(id, rev) }

func (w *casHookStore) DeleteIfMatch(id string, rev int64) error {
	return w.cw().DeleteIfMatch(id, rev)
}

func (w *casHookStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	return w.cw().CompareAndSetMetadataKey(id, key, expected, next)
}

// TestRowFirstRestartV2StaleSelfRuntimeProof pins the v2 owner's sign-off
// (2026-10-27) on the runtime-token proof: on a v2 StaleSelf row (a legacy
// warm reuse minted generation 2 and tok-new on the row while the pane runs
// generation 1 under tok-1), a request from the pane, proven by the live
// runtime's token, is recorded, and it produces exactly one restart into a
// new incarnation, fenced by A13 to the row's incarnation, however A3's rekey
// of the row to tok-1 interleaves with it:
//   - "request, then the parked rekey": A3's rekey is admitted before the
//     request and lands after it;
//   - "rekey, then request": the rekey lands first and the row's own token
//     proves the caller;
//   - "rekey inside the request": the rekey's write (A3's patch, through the
//     controller's cache) lands between the request's deciding read and its
//     CAS, as another process's would; the swap loses, and the re-decide
//     records on the row token. The admitted rekey is released afterwards and
//     finds nothing to do.
//
// In every order the pane is stopped once, and later passes stop nothing.
func TestRowFirstRestartV2StaleSelfRuntimeProof(t *testing.T) {
	for _, order := range []string{"request, then the parked rekey", "rekey, then request", "rekey inside the request"} {
		t.Run(order, func(t *testing.T) {
			s := converged(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-1", "session_key", "k-1", "started_config_hash", "h-1", "last_woke_at", rowAt(-time.Hour)), gc1Work())
			// The warm-reuse residue: the row names (2, tok-new), the live
			// pane still runs (1, tok-1).
			s.legs[0].cache.ApplyEvent("bead.updated", s.operator("gc-1", "generation", "2", "instance_token", "tok-new"))
			proof := simPaneIdentity{s.sp}
			hint := newDrainOps(s.sp)
			admitRekey := func() *simEffect {
				t.Helper()
				s.inventory()
				s.pass()
				for _, e := range s.parked {
					if e.it.Kind == intentRekey && e.it.Key.ID == "gc-1" {
						if got := e.it.Patch["instance_token"]; got != "tok-1" {
							t.Fatalf("A3's rekey patch names %q, want the pane's tok-1", got)
						}
						return e
					}
				}
				t.Fatalf("no rekey admitted for the StaleSelf row; parked %d", len(s.parked))
				return nil
			}
			releaseAll := func() {
				for len(s.parked) > 0 {
					s.release(0)
				}
				s.audit("v2")
			}

			var restart rowFirstRestart
			switch order {
			case "request, then the parked rekey":
				admitRekey()
				restart = simRowFirstRestartVia(t, s, s.legs[0].backing, proof, "tok-1", hint)
				if code := doRuntimeRequestRestart(restart, events.Discard, io.Discard); code != 0 {
					t.Fatalf("request-restart exit = %d, want 0 (the runtime proves the caller)", code)
				}
				releaseAll()
			case "rekey, then request":
				s.settle()
				if got := s.gc1()["instance_token"]; got != "tok-1" {
					t.Fatalf("row token after A3 = %q, want tok-1", got)
				}
				restart = simRowFirstRestartVia(t, s, s.legs[0].backing, proof, "tok-1", hint)
				if code := doRuntimeRequestRestart(restart, events.Discard, io.Discard); code != 0 {
					t.Fatalf("request-restart exit = %d, want 0", code)
				}
			case "rekey inside the request":
				rekey := admitRekey()
				hooked := &casHookStore{Store: s.legs[0].backing}
				hooked.before = func() {
					b, err := s.legs[0].backing.Get("gc-1")
					if err != nil {
						t.Fatal(err)
					}
					cw, ok := beads.ConditionalWriterFor(s.legs[0].cache)
					if !ok {
						t.Fatal("the controller's cache has no conditional writer")
					}
					if err := cw.UpdateIfMatch("gc-1", b.Revision, beads.UpdateOpts{Metadata: map[string]string(rekey.it.Patch)}); err != nil {
						t.Fatalf("A3's rekey write: %v", err)
					}
					s.audit("v2")
				}
				restart = simRowFirstRestartVia(t, s, hooked, proof, "tok-1", hint)
				if code := doRuntimeRequestRestart(restart, events.Discard, io.Discard); code != 0 {
					t.Fatalf("request-restart exit = %d, want 0 (the re-decide reads the rekeyed row)", code)
				}
				if !hooked.fired {
					t.Fatal("the rekey never landed inside the request")
				}
				releaseAll()
			}
			if m := s.gc1(); m["restart_requested"] != "true" && len(s.sp.kills) == 0 {
				t.Fatalf("row %v: the request was not recorded", m)
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
			s.sp.mu.Unlock()
			m := b.Metadata
			if starts != 1 || kills != 1 || m["instance_token"] == "tok-1" || m["instance_token"] == "tok-new" || !s.livePane(b) || m["restart_requested"] != "" {
				t.Fatalf("starts %d, kills %d, row %v, live %t; want exactly one restart into a new incarnation", starts, kills, m, s.livePane(b))
			}

			// Later passes stop nothing more.
			for range 3 {
				s.settle()
				s.advance(simPatrol)
			}
			s.sp.mu.Lock()
			kills = len(s.sp.kills)
			s.sp.mu.Unlock()
			if kills != 1 {
				t.Fatalf("kills = %d after the restart, want 1: a second stop", kills)
			}
			s.noViolations(t)
		})
	}
}

// TestRowFirstRestartRuntimeProofNeverLogsAToken pins the capability rule on
// the runtime-token proof path (the v2 owner's sign-off of 2026-10-27), as
// the drain-ack's refusal pin does for its self form: when the row names
// another token than the caller's and only the live runtime can prove the
// caller, neither token reaches stdout, stderr, the process log or the
// city's event log, whether the runtime proves the caller (recorded) or not
// (refused).
func TestRowFirstRestartRuntimeProofNeverLogsAToken(t *testing.T) {
	const rowToken = "tok-row-warm-reuse-mint"
	for _, v := range restartVerbs {
		for _, proven := range []bool{true, false} {
			name := v.name + "/refused"
			if proven {
				name = v.name + "/proven"
			}
			t.Run(name, func(t *testing.T) {
				fixture := newExplicitRestartFixture(t)
				if err := fixture.store.SetMetadata(fixture.sessionID, "instance_token", rowToken); err != nil {
					t.Fatal(err)
				}
				paneToken := explicitRestartToken
				if !proven {
					paneToken = "tok-pane-successor"
				}
				if err := fixture.provider.SetMeta(fixture.sessionKey, runtime.InstanceTokenEnv, paneToken); err != nil {
					t.Fatal(err)
				}
				var logged bytes.Buffer
				prevOut, prevFlags := log.Writer(), log.Flags()
				log.SetOutput(&logged)
				prevSlog := slog.Default()
				slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
				t.Cleanup(func() {
					log.SetOutput(prevOut)
					log.SetFlags(prevFlags)
					slog.SetDefault(prevSlog)
				})
				startFakeControllerSocket(t, fixture.cityPath, "ok\n")

				var stdout, stderr bytes.Buffer
				code := v.run(&stdout, &stderr)
				if want := map[bool]int{true: 0, false: 1}[proven]; code != want {
					t.Fatalf("exit = %d, want %d; stderr=%q", code, want, stderr.String())
				}
				eventLog, err := os.ReadFile(filepath.Join(fixture.cityPath, citylayout.RuntimeRoot, "events.jsonl"))
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				for where, out := range map[string]string{"stdout": stdout.String(), "stderr": stderr.String(), "log": logged.String(), "event log": string(eventLog)} {
					for _, tok := range []string{explicitRestartToken, rowToken, paneToken} {
						if strings.Contains(out, tok) {
							t.Fatalf("%s carries the token %q: %q", where, tok, out)
						}
					}
				}
			})
		}
	}
}
