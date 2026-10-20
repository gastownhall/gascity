package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/guard"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storehealth"
	"github.com/gastownhall/gascity/internal/storeref"
	"github.com/gastownhall/gascity/internal/supervisor"
)

// newWorkerSiteCity returns a bootstrapped city directory (city.toml and
// .gc/) that a worker-site descriptor has not yet been written into.
func newWorkerSiteCity(t *testing.T) string {
	t.Helper()
	clearGCEnv(t)
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"box-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return cityDir
}

func TestWorkerSiteResolveWorkerSpecReturnsTheGuard(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	t.Setenv("GC_BEADS", "file")
	spec := runtime.WorkerSpec{Runtime: "fake", Transport: config.SessionTransportTmux}

	local, err := resolveWorkerSpec(nil, spec, config.SessionConfig{}, "box-city", cityDir)
	if err != nil {
		t.Fatalf("local resolveWorkerSpec: %v", err)
	}
	if _, isGuard := local.(*guard.Provider); isGuard {
		t.Fatal("a local city resolved the guard, want its configured runtime")
	}

	makeWorkerSite(t, cityDir)
	sp, err := resolveWorkerSpec(nil, spec, config.SessionConfig{}, "box-city", cityDir)
	if err != nil {
		t.Fatalf("worker-site resolveWorkerSpec: %v", err)
	}
	if _, isGuard := sp.(*guard.Provider); !isGuard {
		t.Fatalf("a worker site resolved %T, want *guard.Provider", sp)
	}

	// Through the composed session provider too: nothing it returns can
	// start a runtime.
	composed, err := newSessionProviderForCity(&config.City{}, cityDir)
	if err != nil {
		t.Fatalf("newSessionProviderForCity: %v", err)
	}
	if err := composed.Start(context.Background(), "s-1", runtime.Config{}); !errors.Is(err, guard.ErrControllerOwnedRuntime) {
		t.Fatalf("composed provider Start = %v, want guard.ErrControllerOwnedRuntime", err)
	}
}

// TestWorkerSiteSendersDialNothing proves every sender this slice converts
// returns its typed result on a worker site without reaching a socket: the
// counting dialer stays at zero, and pokeController, reloadControllerConfig
// and enqueueController (the seam pin and wake poke through) return the bare
// sentinel, which only their short-circuit returns (the controller sender
// would fail with its own error and the supervisor fallback wraps the
// sentinel). enqueueController's keyed send dials the controller socket
// directly, past the counting dialer, so a fake controller listening there
// is its evidence.
func TestWorkerSiteSendersDialNothing(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	dials := countSocketDials(t)

	// The controls: on a local city the poke reaches the supervisor fallback,
	// so the counter is wired to the dial the senders use, and the keyed send
	// reaches the fake controller.
	if err := pokeController(cityDir); err == nil || errors.Is(err, errWorkerSiteNoController) {
		t.Fatalf("local pokeController = %v, want a dial failure", err)
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("local pokeController dials = %d, want 1 (the supervisor fallback)", got)
	}
	dials.Store(0)
	accepted := startFakeControllerSocket(t, cityDir, "ok\n")
	if err := enqueueController(cityDir, reconcilekey.Session("s-1")); err != nil {
		t.Fatalf("local enqueueController = %v, want the fake controller's ok", err)
	}
	select {
	case <-accepted:
	default:
		t.Fatal("local enqueueController did not reach the fake controller")
	}

	makeWorkerSite(t, cityDir)

	if !cityUsesManagedReconciler(cityDir) {
		t.Error("cityUsesManagedReconciler = false on a worker site, want true")
	}
	if err := pokeController(cityDir); err != errWorkerSiteNoController { //nolint:errorlint // identity proves the short-circuit
		t.Errorf("pokeController = %v, want the bare errWorkerSiteNoController", err)
	}
	if err := reloadControllerConfig(cityDir); err != errWorkerSiteNoController { //nolint:errorlint // identity proves the short-circuit
		t.Errorf("reloadControllerConfig = %v, want the bare errWorkerSiteNoController", err)
	}
	if err := enqueueController(cityDir, reconcilekey.Session("s-1")); err != errWorkerSiteNoController { //nolint:errorlint // identity proves the short-circuit
		t.Errorf("enqueueController = %v, want the bare errWorkerSiteNoController", err)
	}
	if err := pokeSupervisor(); !errors.Is(err, errWorkerSiteNoController) {
		t.Errorf("pokeSupervisor = %v, want errWorkerSiteNoController", err)
	}
	pingNudgeWakeSocket(cityDir)
	var pinOut bytes.Buffer
	pokeSessionPinController(nil, cityDir, "s-1", "gc session pin", &pinOut)
	assertWorkerSitePokeNotice(t, pinOut.String())

	if got := dials.Load(); got != 0 {
		t.Errorf("worker-site senders dialed %d times, want 0", got)
	}
	select {
	case <-accepted:
		t.Error("a worker-site sender reached the controller socket")
	default:
	}
}

// assertWorkerSitePokeNotice fails unless out reports a poke that found no
// controller on a worker site as the worker-site notice, not as a failed poke.
func assertWorkerSitePokeNotice(t *testing.T, out string) {
	t.Helper()
	if !strings.Contains(out, errWorkerSiteNoController.Error()) || strings.Contains(out, "poke failed") {
		t.Fatalf("output = %q, want the worker-site notice and no poke-failed warning", out)
	}
}

// dormantWorkerSiteSession returns a worker site's store and runtime holding
// one session asleep without a hold, and the nudge target that addresses it,
// so a nudge to it takes the managed-wake path.
func dormantWorkerSiteSession(t *testing.T) (beads.Store, *runtime.Fake, nudgeTarget) {
	t.Helper()
	store, fake, target := suspendedWorkerSiteSession(t)
	sleepUnheld(t, store, target.sessionID)
	return store, fake, target
}

// suspendedWorkerSiteSession is dormantWorkerSiteSession with the operator's
// suspend hold left on the session.
func suspendedWorkerSiteSession(t *testing.T) (beads.Store, *runtime.Fake, nudgeTarget) {
	t.Helper()
	store, fake, target := activeWorkerSiteSession(t)
	mgr := newSessionManagerWithConfig(target.cityPath, store, fake, nil)
	if err := mgr.Suspend(context.Background(), testActorIn(t, session.ActorOperator, target.cityPath), target.sessionID, false); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	return store, fake, target
}

// activeWorkerSiteSession returns a worker site's store and runtime holding
// one session active, as CreateSession leaves it, and the nudge target that
// addresses it: a session that needs no wake.
func activeWorkerSiteSession(t *testing.T) (beads.Store, *runtime.Fake, nudgeTarget) {
	t.Helper()
	resetControlPlaneForTest(t)
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	makeWorkerSite(t, dir)
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store, fake, nil)

	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: "claude", WorkDir: dir, Provider: "claude", Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return store, fake, nudgeTarget{
		cityPath:    dir,
		cfg:         &config.City{Agents: []config.Agent{{Name: "worker", Provider: "claude"}}},
		sessionID:   info.ID,
		sessionName: info.SessionName,
		identity:    "worker",
		agent:       config.Agent{Name: "worker", Provider: "claude"},
	}
}

// assertWakeRecordedForTheController fails unless the session holds an
// explicit wake request and the runtime saw no start or nudge after
// beforeCalls: the wake is the remote controller's to act on.
func assertWakeRecordedForTheController(t *testing.T, store beads.Store, fake *runtime.Fake, id string, beforeCalls int) {
	t.Helper()
	updated, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := updated.Metadata["wake_request"]; got != string(session.WakeCauseExplicit) {
		t.Fatalf("wake_request = %q, want %q", got, session.WakeCauseExplicit)
	}
	for _, call := range fake.Calls[beforeCalls:] {
		switch call.Method {
		case "Start", "Nudge", "NudgeNow":
			t.Fatalf("a worker-site nudge to a dormant session reached the runtime: %+v", call)
		}
	}
}

// assertNoNudgeQueued fails if cityDir's nudge queue file holds any item. On a
// worker site that file is box-local, and the remote controller never reads
// it, so an item there is a nudge nobody delivers.
func assertNoNudgeQueued(t *testing.T, cityDir string) {
	t.Helper()
	state, err := nudgequeue.LoadState(cityDir)
	if err != nil {
		t.Fatalf("loading the box's nudge queue: %v", err)
	}
	if n := len(state.Pending) + len(state.InFlight) + len(state.Dead); n != 0 {
		t.Fatalf("the box's nudge queue holds %d item(s), want none: %+v", n, state)
	}
}

// assertNudgeNotQueuedNotice fails unless out says, as the worker-site
// notice, that a courtesy nudge's text was not queued, and reports no failed
// poke. It promises the remote controller's reconcile exactly when
// wakeRecorded, since a recorded wake is all that reaches that controller.
func assertNudgeNotQueuedNotice(t *testing.T, out string, wakeRecorded bool) {
	t.Helper()
	if !strings.Contains(out, workerSiteNudgeNotQueuedNotice) || strings.Contains(out, "poke failed") {
		t.Fatalf("output = %q, want the not-queued notice and no poke-failed warning", out)
	}
	if promised := strings.Contains(out, "next reconcile"); promised != wakeRecorded {
		t.Fatalf("output = %q promises the remote controller's reconcile = %v, want %v", out, promised, wakeRecorded)
	}
	if said := strings.Contains(out, "no wake was recorded"); said == wakeRecorded {
		t.Fatalf("output = %q says no wake was recorded = %v, want %v", out, said, !wakeRecorded)
	}
}

// TestWorkerSiteSessionNudgeRefusesBeforeWriting: no delivery mode reaches a
// session from a box. The guard types into no runtime, and the box's nudge
// queue is a local file the remote controller never reads, so gc session
// nudge refuses, says nothing was recorded, and writes neither the text nor a
// wake, rather than reporting a nudge it queued where nothing delivers it. A
// target that does not exist gets the same refusal, which pins the refusal
// ahead of target resolution, a path that can materialize a named session.
func TestWorkerSiteSessionNudgeRefusesBeforeWriting(t *testing.T) {
	for _, mode := range []nudgeDeliveryMode{nudgeDeliveryImmediate, nudgeDeliveryWaitIdle, nudgeDeliveryQueue} {
		t.Run(string(mode), func(t *testing.T) {
			cityDir, sessionID := newWorkerSiteSessionCity(t, map[string]string{"state": string(session.StateAsleep)})
			dials := countSocketDials(t)

			for _, target := range []string{sessionID, "no-such-session"} {
				var stdout, stderr bytes.Buffer
				if code := cmdSessionNudge([]string{target, "check", "deploy", "status"}, mode, "", false, &stdout, &stderr); code != 1 {
					t.Fatalf("gc session nudge %s on a worker site = %d, want 1; stdout=%q stderr=%q", target, code, stdout.String(), stderr.String())
				}
				if !strings.Contains(stderr.String(), errWorkerSiteDeliveryNeedsController.Error()) || strings.Contains(stderr.String(), "recorded change") {
					t.Fatalf("gc session nudge %s: stderr = %q, want the delivery refusal and no claimed recorded change", target, stderr.String())
				}
				if stdout.Len() != 0 {
					t.Fatalf("gc session nudge %s: stdout = %q, want nothing", target, stdout.String())
				}
			}
			if got := dials.Load(); got != 0 {
				t.Fatalf("the refused nudge dialed %d gc sockets, want 0", got)
			}
			assertNoNudgeQueued(t, cityDir)
			all := localSessionBeads(t, cityDir)
			if len(all) != 1 || all[0].Metadata["wake_request"] != "" {
				t.Fatalf("the refused nudge changed the sessions: %+v", all)
			}
		})
	}
}

// TestWorkerSiteSessionSubmitRefusesBeforeWriting: submit delivers text the
// way nudge does, by typing into the runtime or, deferred, through the nudge
// queue, so on a box it refuses before it resolves the target or writes
// anything, as gc session nudge does. The seeded session is start-pending,
// the arm that defers a submit into the queue instead of touching a runtime.
func TestWorkerSiteSessionSubmitRefusesBeforeWriting(t *testing.T) {
	cityDir, sessionID := newWorkerSiteSessionCity(t, map[string]string{"state": string(session.StateStartPending)})
	dials := countSocketDials(t)

	var stdout, stderr bytes.Buffer
	if code := cmdSessionSubmit([]string{sessionID, "check", "deploy", "status"}, session.SubmitIntentDefault, "", false, &stdout, &stderr); code != 1 {
		t.Fatalf("gc session submit on a worker site = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), errWorkerSiteDeliveryNeedsController.Error()) {
		t.Fatalf("stderr = %q, want the delivery refusal", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want nothing", stdout.String())
	}
	if got := dials.Load(); got != 0 {
		t.Fatalf("the refused submit dialed %d gc sockets, want 0", got)
	}
	assertNoNudgeQueued(t, cityDir)
	all := localSessionBeads(t, cityDir)
	if len(all) != 1 || all[0].Metadata["wake_request"] != "" || all[0].Metadata["state"] != string(session.StateStartPending) {
		t.Fatalf("the refused submit changed the sessions: %+v", all)
	}
}

// TestWorkerSiteMailNotifyToADormantTargetRecordsOnlyAWake: mail notify on a
// box queues no text, since the box's nudge queue is a local file the remote
// controller never reads. It records the dormant session's wake, which the
// remote controller acts on, says the text was not queued, and dials nothing.
func TestWorkerSiteMailNotifyToADormantTargetRecordsOnlyAWake(t *testing.T) {
	store, fake, target := dormantWorkerSiteSession(t)
	dials := countSocketDials(t)
	var warnings bytes.Buffer
	prevWarnings := nudgeWarningWriter
	nudgeWarningWriter = &warnings
	t.Cleanup(func() { nudgeWarningWriter = prevWarnings })
	beforeCalls := len(fake.Calls)

	if err := sendMailNotifyWithWorker(target, store, fake, ""); err != nil {
		t.Fatalf("sendMailNotifyWithWorker on a worker site: %v", err)
	}
	assertNudgeNotQueuedNotice(t, warnings.String(), true)
	assertWakeRecordedForTheController(t, store, fake, target.sessionID, beforeCalls)
	assertNoNudgeQueued(t, target.cityPath)
	if got := dials.Load(); got != 0 {
		t.Fatalf("mail notify dialed %d times through the gc socket seam, want 0", got)
	}
}

// TestWorkerSiteMailNotifyWithoutARecordedWakeNotifiesNobody: on a box the
// recipient's wake is the only notice mail notify can leave, so a recipient it
// records no wake for is not notified. A held recipient keeps its hold, an
// active one needs no wake, and a target with no session bead has no row to
// record one on. Each returns errWorkerSiteNotifyNotDelivered, which gc mail
// reports as notified false, and writes, queues and dials nothing.
func TestWorkerSiteMailNotifyWithoutARecordedWakeNotifiesNobody(t *testing.T) {
	for _, tc := range []struct {
		name      string
		recipient func(t *testing.T) (beads.Store, *runtime.Fake, nudgeTarget)
		noBead    bool
	}{
		{name: "held recipient", recipient: suspendedWorkerSiteSession},
		{name: "active recipient", recipient: activeWorkerSiteSession},
		{name: "recipient with no session bead", recipient: dormantWorkerSiteSession, noBead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, fake, target := tc.recipient(t)
			sessionID := target.sessionID
			if tc.noBead {
				target.sessionID = ""
			}
			before, err := store.Get(sessionID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			dials := countSocketDials(t)
			var warnings bytes.Buffer
			prevWarnings := nudgeWarningWriter
			nudgeWarningWriter = &warnings
			t.Cleanup(func() { nudgeWarningWriter = prevWarnings })
			beforeCalls := len(fake.Calls)

			err = sendMailNotifyWithWorker(target, store, fake, "")
			if !errors.Is(err, errWorkerSiteNotifyNotDelivered) {
				t.Fatalf("sendMailNotifyWithWorker = %v, want errWorkerSiteNotifyNotDelivered", err)
			}
			assertNudgeNotQueuedNotice(t, err.Error(), false)
			if warnings.Len() != 0 {
				t.Fatalf("warnings = %q, want none beside the returned error", warnings.String())
			}
			after, err := store.Get(sessionID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if !maps.Equal(after.Metadata, before.Metadata) {
				t.Fatalf("mail notify changed the session: %v, was %v", after.Metadata, before.Metadata)
			}
			if calls := fake.Calls[beforeCalls:]; len(calls) != 0 {
				t.Fatalf("mail notify reached the runtime: %+v", calls)
			}
			assertNoNudgeQueued(t, target.cityPath)
			if got := dials.Load(); got != 0 {
				t.Fatalf("mail notify dialed %d gc sockets, want 0", got)
			}
		})
	}
}

// TestWorkerSiteMailSendNotifiedMeansAWakeWasRecorded: gc mail send --notify
// on a box says notified only when it recorded the recipient's wake, the one
// notice that reaches the remote controller. An asleep recipient's wake is
// recorded; an awake recipient needs none, so it is not notified, and the
// send says so while the mail stays on the ledger.
func TestWorkerSiteMailSendNotifiedMeansAWakeWasRecorded(t *testing.T) {
	for _, tc := range []struct {
		state    session.State
		notified bool
	}{
		{state: session.StateAsleep, notified: true},
		{state: session.StateAwake, notified: false},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			cityDir, sessionID := newWorkerSiteSessionCity(t, map[string]string{"state": string(tc.state), "alias": "worker"})
			dials := countSocketDials(t)

			var stdout, stderr bytes.Buffer
			if code := cmdMailSendJSON([]string{"worker", "check the deploy"}, true, false, "human", "", "", "", "", true, &stdout, &stderr); code != 0 {
				t.Fatalf("gc mail send --notify on a worker site = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			var result struct {
				Notified bool `json:"notified"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatalf("decoding the send result %q: %v", stdout.String(), err)
			}
			if result.Notified != tc.notified {
				t.Fatalf("notified = %v, want %v; stderr=%q", result.Notified, tc.notified, stderr.String())
			}
			if failed := strings.Contains(stderr.String(), errWorkerSiteNotifyNotDelivered.Error()); failed == tc.notified {
				t.Fatalf("stderr = %q reports the recipient not notified = %v, want %v", stderr.String(), failed, !tc.notified)
			}
			if got := dials.Load(); got != 0 {
				t.Fatalf("the send dialed %d gc sockets, want 0", got)
			}
			assertNoNudgeQueued(t, cityDir)
			store := localLedger(t, cityDir)
			if n := len(messageBeadsIn(t, store)); n != 1 {
				t.Fatalf("the ledger holds %d mail(s), want the one sent", n)
			}
			recipient, err := store.Get(sessionID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if wake := recipient.Metadata["wake_request"]; (wake != "") != tc.notified {
				t.Fatalf("wake_request = %q with notified %v: the flag must mean a recorded wake", wake, tc.notified)
			}
		})
	}
}

// A worker-site mail notice records only the recipient's wake and never an
// op (sendMailNotifyWithOp), so whether the op journal can record does not
// decide whether it is sent: under require, with an op store that cannot
// record and with the box's own store, the wake is recorded.
func TestWorkerSiteMailNotifyUnderRequireRecordsTheWake(t *testing.T) {
	for _, tc := range []struct {
		name      string
		incapable bool
	}{
		{name: "op store cannot record", incapable: true},
		{name: "box store"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityDir, sessionID := newWorkerSiteSessionCity(t, map[string]string{"state": string(session.StateAsleep), "alias": "worker"})
			if tc.incapable {
				h := newCLIOpHarness(t, map[string]string{"GC_OPS_CONTENT": "require"})
				h.opStore = cliOpIncapableStore{&beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}}
			} else {
				t.Setenv("GC_OPS_CONTENT", "require")
			}
			var stdout, stderr bytes.Buffer
			if code := cmdMailSendJSON([]string{"worker", "check the deploy"}, true, false, "human", "", "", "", "", true, &stdout, &stderr); code != 0 {
				t.Fatalf("gc mail send --notify = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			var result struct {
				Notified bool `json:"notified"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatalf("decoding %q: %v", stdout.String(), err)
			}
			recipient, err := localLedger(t, cityDir).Get(sessionID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if !result.Notified || recipient.Metadata["wake_request"] == "" {
				t.Fatalf("notified = %v, wake_request = %q; stderr=%q: the notice records no op, so require must not refuse it", result.Notified, recipient.Metadata["wake_request"], stderr.String())
			}
		})
	}
}

// TestWorkerSiteSlingNudgePokesAreNotices: gc sling's courtesy nudge on a box
// queues no text, since the box's nudge queue is a local file the remote
// controller never reads, and claims no poke. A pool with no running member
// has only the poke, which finds no controller and is the worker-site notice.
// An asleep session gets its wake recorded on the ledger for the remote
// controller, and only then does the notice promise its reconcile. A held one
// keeps its hold, since the wake is never written over an operator's suspend,
// and an active one needs no wake: for both the notice says none was recorded.
func TestWorkerSiteSlingNudgePokesAreNotices(t *testing.T) {
	workerSite := func(t *testing.T) string {
		t.Helper()
		resetControlPlaneForTest(t)
		t.Setenv("GC_BEADS", "file")
		dir := t.TempDir()
		makeWorkerSite(t, dir)
		return dir
	}

	t.Run("pool with no running member", func(t *testing.T) {
		dir := workerSite(t)
		dials := countSocketDials(t)
		a := namepoolAgentForNudgeTest()
		cfg := &config.City{Workspace: config.Workspace{Name: "box-city"}, Agents: []config.Agent{a}}
		sp := runtime.NewFake()
		deps, stdout, stderr := testDeps(cfg, sp, newFakeRunner().run)

		doSlingNudge(&a, deps.CityName, dir, cfg, sp, deps.Store, stdout, stderr)

		assertWorkerSitePokeNotice(t, stderr.String())
		if strings.Contains(stdout.String(), "poked controller") {
			t.Fatalf("stdout = %q, want no claimed poke", stdout.String())
		}
		if got := dials.Load(); got != 0 {
			t.Fatalf("the sling nudge dialed %d gc sockets, want 0", got)
		}
	})

	// stubPoller keeps a nudge poller from being spawned for a session the
	// fake runtime does not run.
	stubPoller := func(t *testing.T) {
		t.Helper()
		prevPoller := startNudgePoller
		startNudgePoller = func(string, string, string) error { return nil }
		t.Cleanup(func() { startNudgePoller = prevPoller })
	}
	// slingNudge returns the nudge's stdout once its stderr carried the
	// not-queued notice, promising the remote controller's reconcile exactly
	// when wakeRecorded.
	slingNudge := func(t *testing.T, store beads.Store, fake *runtime.Fake, target nudgeTarget, wakeRecorded bool) string {
		t.Helper()
		dials := countSocketDials(t)
		var out, errOut bytes.Buffer
		deliverSlingNudge(target, fake, store, target.cityPath, &out, &errOut)
		if got := dials.Load(); got != 0 {
			t.Fatalf("the sling nudge dialed %d gc sockets, want 0", got)
		}
		assertNudgeNotQueuedNotice(t, errOut.String(), wakeRecorded)
		if strings.Contains(out.String(), "Queued nudge") || strings.Contains(out.String(), "poked controller") {
			t.Fatalf("stdout = %q, want no queued nudge and no claimed poke", out.String())
		}
		assertNoNudgeQueued(t, target.cityPath)
		return out.String()
	}

	t.Run("asleep session", func(t *testing.T) {
		store, fake, target := dormantWorkerSiteSession(t)
		stubPoller(t)
		beforeCalls := len(fake.Calls)

		stdout := slingNudge(t, store, fake, target, true)

		assertWakeRecordedForTheController(t, store, fake, target.sessionID, beforeCalls)
		if !strings.Contains(stdout, "recorded a wake for the remote controller") {
			t.Fatalf("stdout = %q, want the recorded wake", stdout)
		}
	})

	t.Run("held session", func(t *testing.T) {
		store, fake, target := suspendedWorkerSiteSession(t)
		stubPoller(t)

		stdout := slingNudge(t, store, fake, target, false)

		held, err := store.Get(target.sessionID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if held.Metadata["wake_request"] != "" || held.Metadata["state"] != string(session.StateSuspended) {
			t.Fatalf("the sling nudge woke a held session: %v", held.Metadata)
		}
		if strings.Contains(stdout, "recorded a wake") {
			t.Fatalf("stdout = %q claims a wake the hold refused", stdout)
		}
	})

	t.Run("active session", func(t *testing.T) {
		store, fake, target := activeWorkerSiteSession(t)
		stubPoller(t)
		before, err := store.Get(target.sessionID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}

		stdout := slingNudge(t, store, fake, target, false)

		after, err := store.Get(target.sessionID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !maps.Equal(after.Metadata, before.Metadata) {
			t.Fatalf("the sling nudge changed an active session: %v, was %v", after.Metadata, before.Metadata)
		}
		if strings.Contains(stdout, "recorded a wake") {
			t.Fatalf("stdout = %q claims a wake an active session does not need", stdout)
		}
	})
}

// TestWorkerSiteHookContinuationNudgeQueuesNothing: gc hook --claim's
// continuation self-nudge on a box queues no text and starts no nudge poller,
// since the box's nudge queue is a local file the remote controller never
// reads and the box's guard types into no runtime. The claimed work is on the
// ledger either way.
func TestWorkerSiteHookContinuationNudgeQueuesNothing(t *testing.T) {
	cityDir, sessionID := newWorkerSiteSessionCity(t, map[string]string{"state": string(session.StateActive)})
	dials := countSocketDials(t)
	var pollers []string
	prevPoller := startNudgePoller
	startNudgePoller = func(_, _, sessionName string) error {
		pollers = append(pollers, sessionName)
		return nil
	}
	t.Cleanup(func() { startNudgePoller = prevPoller })

	hookContinuationNudgeEnqueue(sessionID)

	assertNoNudgeQueued(t, cityDir)
	if len(pollers) != 0 {
		t.Fatalf("the continuation nudge started nudge pollers %v on a worker site, want none", pollers)
	}
	if got := dials.Load(); got != 0 {
		t.Fatalf("the continuation nudge dialed %d gc sockets, want 0", got)
	}
}

// newWorkerSiteSessionCity returns a box city with a file ledger and one
// template, worker, as a command on the box finds it: GC_CITY names the city
// and the descriptor is on disk, but no invocation is marked, so the
// command's own context resolution marks it. A non-nil seed is created as a
// session bead of the worker template before the descriptor is written, so
// the command under test is the only writer on the worker site.
func newWorkerSiteSessionCity(t *testing.T, seed map[string]string) (cityDir, sessionID string) {
	t.Helper()
	resetControlPlaneForTest(t)
	cityDir = newWorkerSiteCity(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_CITY", cityDir)
	cityTOML := "[workspace]\nname = \"box-city\"\n\n[beads]\nprovider = \"file\"\n\n[[agent]]\nname = \"worker\"\nstart_command = \"echo\"\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	if seed != nil {
		store, err := openCityStoreAt(cityDir)
		if err != nil {
			t.Fatalf("openCityStoreAt: %v", err)
		}
		meta := map[string]string{"template": "worker", "session_name": "worker-1"}
		maps.Copy(meta, seed)
		b, err := store.Create(beads.Bead{
			Title:    "worker",
			Type:     session.BeadType,
			Labels:   []string{session.LabelSession, "template:worker"},
			Metadata: meta,
		})
		if err != nil {
			t.Fatalf("seeding the session: %v", err)
		}
		sessionID = b.ID
	}
	writeControlPlaneFixture(t, cityDir, testDescriptor(cityDir, nil))
	return cityDir, sessionID
}

// localLedger opens cityDir's ledger as a Local reader would: with the
// descriptor gone and no plane or worker-site flag left in the process.
func localLedger(t *testing.T, cityDir string) beads.Store {
	t.Helper()
	if err := os.Remove(filepath.Join(cityDir, citylayout.ControlPlaneDescriptorPath)); err != nil {
		t.Fatal(err)
	}
	clearControlPlaneState()
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	return store
}

// localSessionBeads reads every session bead in cityDir's ledger, closed ones
// too, through localLedger.
func localSessionBeads(t *testing.T, cityDir string) []beads.Bead {
	t.Helper()
	return sessionBeadsIn(t, localLedger(t, cityDir))
}

func sessionBeadsIn(t *testing.T, store beads.Store) []beads.Bead {
	t.Helper()
	all, err := store.ListByLabel(session.LabelSession, 0, beads.IncludeClosed)
	if err != nil {
		t.Fatalf("listing session beads: %v", err)
	}
	return all
}

// messageBeadsIn reads every mail bead in store, closed ones too.
func messageBeadsIn(t *testing.T, store beads.Store) []beads.Bead {
	t.Helper()
	all, err := store.List(beads.ListQuery{Type: "message", IncludeClosed: true})
	if err != nil {
		t.Fatalf("listing message beads: %v", err)
	}
	return all
}

// assertOneDeferredSession fails unless all holds exactly one session bead and
// it is open and waiting for a controller to start it: a create the box tried
// to start itself would have been refused and rolled back, closing its bead.
func assertOneDeferredSession(t *testing.T, all []beads.Bead) beads.Bead {
	t.Helper()
	if len(all) != 1 {
		t.Fatalf("session beads = %d, want exactly one: %+v", len(all), all)
	}
	got := all[0]
	if got.Status == "closed" {
		t.Fatalf("the session bead was closed (close_reason %q), want it open for the remote controller", got.Metadata["close_reason"])
	}
	if got.Metadata["state"] != string(session.StateStartPending) || got.Metadata["pending_create_claim"] != "true" {
		t.Fatalf("session state = %q (pending_create_claim %q), want a deferred create", got.Metadata["state"], got.Metadata["pending_create_claim"])
	}
	return got
}

// workerSiteACPTemplates adds the two ways a template reaches the ACP
// transport to a worker-site city: acp-explicit asks for it, and
// acp-default's provider, the opencode builtin, defaults to it. path_check
// keeps provider resolution off the PATH. The guard on a box routes neither
// transport, so these prove a create is validated against config alone.
const workerSiteACPTemplates = `
[[agent]]
name = "acp-explicit"
provider = "acp-stub"
session = "acp"

[[agent]]
name = "acp-default"
provider = "opencode"

[providers.acp-stub]
command = "true"
supports_acp = true
acp_args = ["acp"]

[providers.opencode]
base = "builtin:opencode"
path_check = "true"
`

// workerSiteACPProviders is workerSiteACPTemplates' provider catalog, for
// tests that build their config in Go.
func workerSiteACPProviders() map[string]config.ProviderSpec {
	return map[string]config.ProviderSpec{
		"acp-stub": {Command: "true", SupportsACP: boolPtr(true), ACPArgs: []string{"acp"}},
		"opencode": {Base: stringPtr("builtin:opencode"), PathCheck: "true"},
	}
}

// TestWorkerSiteSessionNewRecordsADeferredSession: no controller answers a
// poke on a box, so gc session new records the session for the remote
// controller to start instead of starting it here, where the guard refuses
// the start and the create is rolled back. It does not attach either: the
// runtime is the remote controller's. An ACP template is recorded too: the
// remote controller's runtime routes ACP, whatever the box's guard says.
func TestWorkerSiteSessionNewRecordsADeferredSession(t *testing.T) {
	for _, tc := range []struct{ template, transport string }{
		{template: "worker"},
		{template: "acp-explicit", transport: config.SessionTransportACP},
		{template: "acp-default", transport: config.SessionTransportACP},
	} {
		for _, noAttach := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/no-attach=%v", tc.template, noAttach), func(t *testing.T) {
				cityDir, _ := newWorkerSiteSessionCity(t, nil)
				appendCityTOML(t, cityDir, workerSiteACPTemplates)
				dials := countSocketDials(t)

				var stdout, stderr bytes.Buffer
				if code := cmdSessionNew([]string{tc.template}, "", "", "", noAttach, false, time.Second, &stdout, &stderr); code != 0 {
					t.Fatalf("gc session new on a worker site = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
				}
				if !strings.Contains(stdout.String(), "reconciler will start it") {
					t.Fatalf("stdout = %q, want the deferred-start confirmation", stdout.String())
				}
				if !strings.Contains(stderr.String(), errWorkerSiteNoController.Error()) {
					t.Fatalf("stderr = %q, want the worker-site notice", stderr.String())
				}
				if strings.Contains(stdout.String(), "Waiting for session") {
					t.Fatalf("stdout = %q, want no wait for a runtime the box does not own", stdout.String())
				}
				if !noAttach && !strings.Contains(stdout.String(), "not attaching") {
					t.Fatalf("stdout = %q, want the not-attaching note", stdout.String())
				}
				if got := dials.Load(); got != 0 {
					t.Fatalf("gc session new dialed %d gc sockets, want 0", got)
				}
				got := assertOneDeferredSession(t, localSessionBeads(t, cityDir))
				if tc.transport != "" && got.Metadata["transport"] != tc.transport {
					t.Fatalf("transport = %q, want %q", got.Metadata["transport"], tc.transport)
				}
			})
		}
	}
}

// TestWorkerSiteSessionTransportChecksTheConfigOnly: on a box the remote
// controller's runtime routes the session, so validateSessionTransportAt
// checks the transport against the configuration alone. A transport the
// configuration allows passes whatever the box's guard routes, and the
// configuration's own refusals still hold: ACP on a provider without it, and
// a transport gc does not know.
func TestWorkerSiteSessionTransportChecksTheConfigOnly(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	makeWorkerSite(t, cityDir)
	acp := &config.ResolvedProvider{Name: "acp-capable", SupportsACP: true}
	noACP := &config.ResolvedProvider{Name: "tmux-only"}
	for _, tc := range []struct {
		name      string
		resolved  *config.ResolvedProvider
		transport string
		wantErr   string
	}{
		{name: "default", resolved: noACP},
		{name: "tmux", resolved: noACP, transport: config.SessionTransportTmux},
		{name: "acp", resolved: acp, transport: config.SessionTransportACP},
		{name: "acp without support", resolved: noACP, transport: config.SessionTransportACP, wantErr: `provider "tmux-only" does not support ACP transport`},
		{name: "unknown", resolved: noACP, transport: "carrier-pigeon", wantErr: `unknown session transport "carrier-pigeon"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSessionTransportAt(cityDir, tc.resolved, tc.transport, guard.New())
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("validateSessionTransportAt = %v, want nil: the remote controller's runtime routes it", err)
			case tc.wantErr != "" && (err == nil || err.Error() != tc.wantErr):
				t.Fatalf("validateSessionTransportAt = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// TestWorkerSiteMaterializeRecordsADeferredSession: the named-session and
// template materializers behind sling, nudge, pin, wake, attach and submit
// record a deferred session on a box, as gc session new does, ACP sessions
// included.
func TestWorkerSiteMaterializeRecordsADeferredSession(t *testing.T) {
	for _, agent := range []struct {
		name      string
		agent     config.Agent
		transport string
	}{
		{name: "start command", agent: config.Agent{StartCommand: "true"}},
		{name: "explicit acp", agent: config.Agent{Provider: "acp-stub", Session: config.SessionTransportACP}, transport: config.SessionTransportACP},
		{name: "acp default", agent: config.Agent{Provider: "opencode"}, transport: config.SessionTransportACP},
	} {
		for _, named := range []bool{true, false} {
			mode := "template"
			if named {
				mode = "named session"
			}
			t.Run(agent.name+"/"+mode, func(t *testing.T) {
				resetControlPlaneForTest(t)
				cityDir := newWorkerSiteCity(t)
				t.Setenv("GC_BEADS", "file")
				t.Setenv("GC_SESSION", "fake")
				t.Setenv("GC_CITY", cityDir)
				makeWorkerSite(t, cityDir)
				dials := countSocketDials(t)
				store := beads.NewMemStore()
				target := agent.agent
				target.Name = "worker"
				cfg := &config.City{
					Workspace: config.Workspace{Name: "box-city"},
					Agents:    []config.Agent{target},
					Providers: workerSiteACPProviders(),
				}
				if named {
					cfg.NamedSessions = []config.NamedSession{{Template: "worker"}}
				}

				var stderr bytes.Buffer
				sessionName, err := ensureSessionForTemplate(cityDir, cfg, store, "worker", &stderr)
				if err != nil {
					t.Fatalf("materializing worker on a worker site: %v; stderr=%s", err, stderr.String())
				}
				all, err := store.ListByLabel(session.LabelSession, 0, beads.IncludeClosed)
				if err != nil {
					t.Fatal(err)
				}
				got := assertOneDeferredSession(t, all)
				if got.Metadata["session_name"] != sessionName {
					t.Fatalf("materialized %q, but the bead's session_name is %q", sessionName, got.Metadata["session_name"])
				}
				if agent.transport != "" && got.Metadata["transport"] != agent.transport {
					t.Fatalf("transport = %q, want %q", got.Metadata["transport"], agent.transport)
				}
				if !strings.Contains(stderr.String(), errWorkerSiteNoController.Error()) {
					t.Fatalf("stderr = %q, want the worker-site notice", stderr.String())
				}
				if got := dials.Load(); got != 0 {
					t.Fatalf("materializing dialed %d gc sockets, want 0", got)
				}
			})
		}
	}
}

// TestWorkerSiteSessionResetRefusesBeforeWriting: reset needs the controller
// (its breaker clear goes over the controller socket), so on a box it refuses
// before it records anything and says so, rather than reporting a change the
// remote controller will act on.
func TestWorkerSiteSessionResetRefusesBeforeWriting(t *testing.T) {
	cityDir, sessionID := newWorkerSiteSessionCity(t, map[string]string{"state": "awake"})
	dials := countSocketDials(t)

	var stdout, stderr bytes.Buffer
	if code := cmdSessionReset([]string{sessionID}, "", &stdout, &stderr, false); code != 1 {
		t.Fatalf("gc session reset on a worker site = %d, want 1; stdout=%q", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), errWorkerSiteResetNeedsController.Error()) {
		t.Fatalf("stderr = %q, want the reset refusal", stderr.String())
	}
	if strings.Contains(stderr.String(), "recorded change") {
		t.Fatalf("stderr = %q claims a recorded change the refusal did not write", stderr.String())
	}
	if got := dials.Load(); got != 0 {
		t.Fatalf("the refused reset dialed %d gc sockets, want 0", got)
	}
	all := localSessionBeads(t, cityDir)
	if len(all) != 1 || all[0].Status == "closed" || all[0].Metadata["restart_requested"] != "" {
		t.Fatalf("the refused reset changed the session: %+v", all)
	}
}

// TestWorkerSiteSessionSuspendWritesTheOperatorHold: suspend on a box takes the
// managed arm, the operator hold the remote controller drains, and reports
// managed mode, instead of the direct arm, which asks the guard to stop a
// runtime the box does not own.
func TestWorkerSiteSessionSuspendWritesTheOperatorHold(t *testing.T) {
	cityDir, sessionID := newWorkerSiteSessionCity(t, map[string]string{"state": "awake"})
	dials := countSocketDials(t)

	var stdout, stderr bytes.Buffer
	if code := cmdSessionSuspend([]string{sessionID}, "", &stdout, &stderr, true); code != 0 {
		t.Fatalf("gc session suspend on a worker site = %d, want 0; stderr=%s", code, stderr.String())
	}
	var result sessionActionResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decoding %q: %v", stdout.String(), err)
	}
	if result.Mode != "managed" || result.State != "suspended" {
		t.Fatalf("suspend reported mode %q state %q, want managed suspended", result.Mode, result.State)
	}
	if !strings.Contains(stderr.String(), errWorkerSiteNoController.Error()) {
		t.Fatalf("stderr = %q, want the worker-site notice", stderr.String())
	}
	if got := dials.Load(); got != 0 {
		t.Fatalf("gc session suspend dialed %d gc sockets, want 0", got)
	}
	all := localSessionBeads(t, cityDir)
	if len(all) != 1 {
		t.Fatalf("session beads = %d, want 1", len(all))
	}
	if meta := all[0].Metadata; meta["state"] != "suspended" || meta["sleep_intent"] != "user-hold" || meta["held_until"] == "" {
		t.Fatalf("suspend left %v, want the operator hold", meta)
	}
}

// TestWorkerSiteSessionWakeRecordsTheWakeWithANotice: wake on a box records the
// wake and says the remote controller acts on it, without the poke-failed
// warning. A fake controller stands at the socket path, which only the keyed
// send dials, and accepts nothing.
func TestWorkerSiteSessionWakeRecordsTheWakeWithANotice(t *testing.T) {
	cityDir, sessionID := newWorkerSiteSessionCity(t, map[string]string{
		"state":        "suspended",
		"held_until":   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"sleep_reason": "user-hold",
	})
	accepted := startFakeControllerSocket(t, cityDir, "ok\n")
	dials := countSocketDials(t)

	var stdout, stderr bytes.Buffer
	if code := cmdSessionWake([]string{sessionID}, "", &stdout, &stderr, false); code != 0 {
		t.Fatalf("gc session wake on a worker site = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "wake requested") {
		t.Fatalf("stdout = %q, want the wake confirmation", stdout.String())
	}
	assertWorkerSitePokeNotice(t, stderr.String())
	select {
	case <-accepted:
		t.Fatal("gc session wake reached the controller socket from a worker site")
	default:
	}
	if got := dials.Load(); got != 0 {
		t.Fatalf("gc session wake dialed %d gc sockets, want 0", got)
	}
	all := localSessionBeads(t, cityDir)
	if len(all) != 1 || all[0].Metadata["wake_request"] == "" {
		t.Fatalf("the wake was not recorded: %+v", all)
	}
}

// TestWorkerSiteSessionListKeepsThePersistedState: the box's guard runs no
// session, so on a worker site its IsRunning is not the remote runtime's
// liveness, and gc session list reports the state the ledger records rather
// than downgrading every active session to asleep. The control is the same
// ledger under local control, where the live overlay applies.
func TestWorkerSiteSessionListKeepsThePersistedState(t *testing.T) {
	cityDir, sessionID := newWorkerSiteSessionCity(t, map[string]string{"state": string(session.StateActive)})
	listState := func(t *testing.T) session.State {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := doSessionListFallback("", "", true, &stdout, &stderr); code != 0 {
			t.Fatalf("gc session list = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		var listed sessionListJSON
		if err := json.Unmarshal(stdout.Bytes(), &listed); err != nil {
			t.Fatalf("decoding %q: %v", stdout.String(), err)
		}
		if len(listed.Sessions) != 1 || listed.Sessions[0].ID != sessionID {
			t.Fatalf("listed %+v, want only %s", listed.Sessions, sessionID)
		}
		return listed.Sessions[0].State
	}

	if got := listState(t); got != session.StateActive {
		t.Fatalf("worker-site list state = %q, want the persisted %q", got, session.StateActive)
	}

	if err := os.Remove(filepath.Join(cityDir, citylayout.ControlPlaneDescriptorPath)); err != nil {
		t.Fatal(err)
	}
	clearControlPlaneState()
	if got := listState(t); got != session.StateAsleep {
		t.Fatalf("local list state = %q, want the live overlay's %q for a session nothing runs", got, session.StateAsleep)
	}
}

func TestWorkerSiteEventsLocalFallbackIsRefused(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	if err := os.WriteFile(filepath.Join(cityDir, ".gc", "events.jsonl"), []byte(`{"seq":1,"type":"session.woke","ts":"2026-10-16T00:00:00Z","actor":"gc"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scope := eventsAPIScope{cityName: "box-city", cityPath: cityDir, localOnly: true}

	// The control: a local city that is stopped reads its own log.
	var stdout, stderr bytes.Buffer
	if code := doEvents(scope, "", "", nil, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "session.woke") {
		t.Fatalf("local doEvents = %d, stdout=%q stderr=%q; want the local log", code, stdout.String(), stderr.String())
	}

	makeWorkerSite(t, cityDir)
	_, handled, err := readLocalCityEvents(scope, stoppedCityLocalFallbackError(scope), "", "", io.Discard)
	var refusal errWorkerSiteLocalState
	if !handled || !errors.As(err, &refusal) || refusal.what != "events" {
		t.Fatalf("readLocalCityEvents = (handled %v, %v), want errWorkerSiteLocalState{events}", handled, err)
	}
	if _, handled, err := readLocalCityHeadIndex(scope, stoppedCityLocalFallbackError(scope)); !handled || !errors.As(err, &refusal) {
		t.Fatalf("readLocalCityHeadIndex = (handled %v, %v), want errWorkerSiteLocalState{events}", handled, err)
	}
	for name, run := range map[string]func(io.Writer, io.Writer) int{
		"events": func(o, e io.Writer) int { return doEvents(scope, "", "", nil, o, e) },
		"seq":    func(o, e io.Writer) int { return doEventsSeq(scope, o, e) },
	} {
		stdout.Reset()
		stderr.Reset()
		if code := run(&stdout, &stderr); code == 0 {
			t.Errorf("gc events %s on a worker site = 0, want a refusal", name)
		}
		if strings.Contains(stdout.String(), "session.woke") {
			t.Errorf("gc events %s printed the box's local log: %s", name, stdout.String())
		}
		if !strings.Contains(stderr.String(), "worker site") {
			t.Errorf("gc events %s stderr = %q, want the worker-site refusal", name, stderr.String())
		}
	}
}

// TestWorkerSiteStoreHealthReadsNoLocalEvents: gc status' StoreHealth takes the
// last store maintenance from the city's events log. A box's events.jsonl
// holds only what the box wrote, so on a worker site StoreHealth opens no
// events log and reports no last maintenance, as gc events refuses the same
// fallback. The control is the same city under local control.
func TestWorkerSiteStoreHealthReadsNoLocalEvents(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	rec, err := events.NewFileRecorder(filepath.Join(cityDir, citylayout.RuntimeRoot, "events.jsonl"), io.Discard)
	if err != nil {
		t.Fatalf("NewFileRecorder: %v", err)
	}
	payload, err := json.Marshal(events.StoreMaintenanceDonePayload{DurationSeconds: 5})
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.RecordAck(events.Event{Type: events.StoreMaintenanceDone, Ts: time.Now().UTC(), Actor: "gc", Payload: payload}); err != nil {
		t.Fatalf("recording the maintenance: %v", err)
	}
	if err := rec.Close(); err != nil {
		t.Fatalf("closing the recorder: %v", err)
	}

	local := defaultOpenStoreHealthEvents(cityDir, io.Discard)
	if local == nil {
		t.Fatal("local city: StoreHealth opened no events log, want the city's own")
	}
	lastAt, _ := storehealth.LastMaintenance(local)
	if closer, ok := local.(io.Closer); ok {
		_ = closer.Close()
	}
	if lastAt.IsZero() {
		t.Fatal("local city: no last maintenance, want the recorded one")
	}

	makeWorkerSite(t, cityDir)
	if ep := defaultOpenStoreHealthEvents(cityDir, io.Discard); ep != nil {
		t.Fatalf("worker site: StoreHealth opened the box's local events log (%T), want none", ep)
	}
}

// TestWorkerSiteAnalyzeReliabilityRefusesTheLocalEventsLog: gc
// analyze-reliability finds a city's events log by --city or by discovery. On
// a worker site that log holds only what the box wrote, so both refuse, as gc
// events does. An explicit --events file is the operator's own choice and is
// read. The control is the same city under local control.
func TestWorkerSiteAnalyzeReliabilityRefusesTheLocalEventsLog(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	t.Setenv("GC_CITY", cityDir)
	cases := map[string]reliabilityCmdOptions{
		"--city":    {cityPath: cityDir},
		"discovery": {},
	}
	want := filepath.Join(cityDir, citylayout.RuntimeRoot, "events.jsonl")
	for name, opts := range cases {
		if got, err := resolveEventsPath(opts); err != nil || got != want {
			t.Fatalf("local city, %s: resolveEventsPath = (%q, %v), want %q", name, got, err, want)
		}
	}

	makeWorkerSite(t, cityDir)
	for name, opts := range cases {
		_, err := resolveEventsPath(opts)
		var refusal errWorkerSiteLocalState
		if !errors.As(err, &refusal) || refusal.what != "events" {
			t.Errorf("worker site, %s: resolveEventsPath = %v, want errWorkerSiteLocalState{events}", name, err)
		}
	}
	explicit := filepath.Join(t.TempDir(), "events.jsonl")
	if got, err := resolveEventsPath(reliabilityCmdOptions{eventPath: explicit, cityPath: cityDir}); err != nil || got != explicit {
		t.Fatalf("worker site, --events: resolveEventsPath = (%q, %v), want the explicit file", got, err)
	}
}

// TestWorkerSiteResolveClassStoreRefusesUnattachedBindings opens a real split
// city whose infrastructure classes sit on a sqlite-beads binding. On a worker
// site a class resolves only to a binding a descriptor ledger lists.
func TestWorkerSiteResolveClassStoreRefusesUnattachedBindings(t *testing.T) {
	cityPath, cfg := migratedOneShotCLICity(t)
	resetControlPlaneForTest(t)
	routes := cliStorageRoutes(cityPath)
	bindingStore, relocated := routes.storeFor(coordclassFor(config.BeadClassSessions))
	if !relocated {
		t.Fatal("fixture: sessions is not relocated to the binding")
	}
	work := beads.NewMemStore()

	assertRefused := func(t *testing.T, class string) {
		t.Helper()
		store := resolveClassStore(routes, work, cfg, cityPath, class, nil)
		_, err := store.Get("any-1")
		var refusal errWorkerSiteLocalState
		if !errors.As(err, &refusal) || refusal.what != class+" store" {
			t.Fatalf("%s store Get = %v, want errWorkerSiteLocalState{%q}", class, err, class+" store")
		}
	}

	t.Run("a sqlite-beads binding no ledger lists", func(t *testing.T) {
		resetControlPlaneForTest(t)
		writeControlPlaneFixture(t, cityPath, testDescriptor(cityPath, nil))
		for _, class := range []string{config.BeadClassSessions, config.BeadClassGraph, config.BeadClassMessaging, config.BeadClassOrders, config.BeadClassNudges} {
			assertRefused(t, class)
		}
		if got := resolveClassStore(routes, work, cfg, cityPath, config.BeadClassWork, nil); got != beads.Store(work) {
			t.Fatalf("the work class resolved %T, want the work store the city ledger serves", got)
		}
	})

	t.Run("a binding left out of the bundle", func(t *testing.T) {
		resetControlPlaneForTest(t)
		writeControlPlaneFixture(t, cityPath, testDescriptor(cityPath, map[string][]string{"archive": {"orders", "nudges"}}))
		assertRefused(t, config.BeadClassSessions)
	})

	t.Run("a listed binding opens", func(t *testing.T) {
		resetControlPlaneForTest(t)
		writeControlPlaneFixture(t, cityPath, testDescriptor(cityPath, map[string][]string{
			"infra": {"graph", "sessions", "messaging", "orders", "nudges"},
		}))
		if got := resolveClassStore(routes, work, cfg, cityPath, config.BeadClassSessions, nil); got != bindingStore {
			t.Fatalf("an attached binding resolved %T, want the binding's store", got)
		}
	})

	t.Run("a local city is unchanged", func(t *testing.T) {
		resetControlPlaneForTest(t)
		if err := os.Remove(filepath.Join(cityPath, ".gc", "control-plane.toml")); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		forgetControlPlane(cityPath)
		if got := resolveClassStore(routes, work, cfg, cityPath, config.BeadClassSessions, nil); got != bindingStore {
			t.Fatalf("a local split city resolved %T, want the binding's store", got)
		}
	})
}

func TestWorkerSiteStartRefusesController(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	makeWorkerSite(t, cityDir)
	for name, controllerMode := range map[string]bool{"supervisor": false, "foreground": true} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := doStartWithNameOverrideJSON([]string{cityDir}, controllerMode, &stdout, &stderr, "", false); code == 0 {
				t.Fatalf("gc start on a worker site = 0; stdout=%s", stdout.String())
			}
			if !strings.Contains(stderr.String(), errWorkerSiteRefusesController.Error()) {
				t.Fatalf("stderr = %q, want the worker-site refusal", stderr.String())
			}
		})
	}
}

func TestWorkerSiteSupervisorRunRefusesController(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	makeWorkerSite(t, cityDir)
	t.Setenv("GC_CITY", cityDir)
	ran := false
	prev := runSupervisorFunc
	runSupervisorFunc = func(io.Writer, io.Writer) int { ran = true; return 0 }
	t.Cleanup(func() { runSupervisorFunc = prev })

	var stdout, stderr bytes.Buffer
	if code := doSupervisorRun(&stdout, &stderr); code == 0 {
		t.Fatal("gc supervisor run on a worker site = 0")
	}
	if ran {
		t.Fatal("gc supervisor run started the supervisor loop on a worker site")
	}
	if !strings.Contains(stderr.String(), errWorkerSiteRefusesController.Error()) {
		t.Fatalf("stderr = %q, want the worker-site refusal", stderr.String())
	}

	// From a fresh process whose flag is not set yet: the ambient city
	// resolves the plane.
	clearControlPlaneState()
	stderr.Reset()
	if code := doSupervisorRun(&stdout, &stderr); code == 0 || ran {
		t.Fatalf("gc supervisor run from the box's city = %d (ran %v), want a refusal", code, ran)
	}
}

func TestWorkerSiteSupervisorRunOnALocalCityRuns(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	t.Setenv("GC_CITY", cityDir)
	ran := false
	prev := runSupervisorFunc
	runSupervisorFunc = func(io.Writer, io.Writer) int { ran = true; return 0 }
	t.Cleanup(func() { runSupervisorFunc = prev })
	if code := doSupervisorRun(io.Discard, io.Discard); code != 0 || !ran {
		t.Fatalf("gc supervisor run on a local city = %d (ran %v), want the loop", code, ran)
	}
}

// TestWorkerSiteSessionWaitSleepSucceedsAfterItsHold is the durable-write
// caller: the hold is written, the poke has no controller to reach, and the
// command reports success with a notice.
func TestWorkerSiteSessionWaitSleepSucceedsAfterItsHold(t *testing.T) {
	const sessionID, depID = "gc-1", "gc-dep"
	now := time.Date(2026, time.October, 16, 9, 0, 0, 0, time.UTC)
	store := beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: sessionID, Title: "worker session", Type: sessionBeadType, Status: "open", Labels: []string{sessionBeadLabel}, CreatedAt: now, UpdatedAt: now, Revision: 1, Metadata: map[string]string{"session_name": "worker", "continuation_epoch": "1"}},
		{ID: depID, Title: "blocking work", Type: "task", Status: "open", CreatedAt: now, UpdatedAt: now, Revision: 1},
	}, nil)
	deps := func(poke func() error) sessionWaitDeps {
		return sessionWaitDeps{
			sessions:       sessionFrontDoor(store),
			dependencies:   waitDependencyReaderFunc(store.Get),
			now:            func() time.Time { return now },
			pokeController: poke,
		}
	}

	var stdout, stderr bytes.Buffer
	code := doSessionWait(sessionID, []string{depID}, false, "until the work lands", true, &stdout, &stderr, deps(func() error {
		return fmt.Errorf("connecting to supervisor: %w", errWorkerSiteNoController)
	}))
	if code != 0 {
		t.Fatalf("doSessionWait --sleep = %d on a worker site, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "draining to sleep") {
		t.Fatalf("stdout = %q, want the sleep confirmation", stdout.String())
	}
	if lines := strings.Split(strings.TrimSpace(stderr.String()), "\n"); len(lines) != 1 || !strings.Contains(lines[0], "worker site") {
		t.Fatalf("stderr = %q, want one worker-site notice line", stderr.String())
	}
	if got := mustGetBead(t, store, sessionID).Metadata["wait_hold"]; got == "" {
		t.Fatal("the wait hold was not written before the poke")
	}

	// Any other poke failure stays fatal.
	stdout.Reset()
	stderr.Reset()
	if code := doSessionWait(sessionID, []string{depID}, false, "again", true, &stdout, &stderr, deps(func() error { return errors.New("controller refused") })); code == 0 {
		t.Fatal("doSessionWait --sleep = 0 on an ordinary poke failure, want 1")
	}
}

func TestWorkerSiteConvoyPokeSucceedsWithANotice(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	makeWorkerSite(t, cityDir)
	dials := countSocketDials(t)
	t.Setenv("GC_CITY", cityDir)

	var stderr bytes.Buffer
	cmd := newConvoyPokeCmd(io.Discard, &stderr)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("gc convoy poke = %v on a worker site, want success; stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "worker site") {
		t.Fatalf("stderr = %q, want the worker-site notice", stderr.String())
	}
	// pokeControlDispatch short-circuits before its control-dispatcher send, so
	// neither that direct controller dial nor the seam's dials happen; this
	// counter sees only the seam's.
	if got := dials.Load(); got != 0 {
		t.Fatalf("gc convoy poke dialed %d times through the gc socket seam, want 0", got)
	}
}

// TestWorkerSiteDrainCheckAnswersFromTheRow reads drain state through the
// guard: its meta refusal is "meta not applicable", so the row alone answers.
func TestWorkerSiteDrainCheckAnswersFromTheRow(t *testing.T) {
	dops := newDrainOps(guard.New())
	for _, tc := range []struct {
		row  bool
		want int
	}{{row: true, want: 0}, {row: false, want: 1}} {
		var stderr bytes.Buffer
		if code := doRuntimeDrainCheck(dops, drainCheckRow(tc.row, nil), "worker", "worker", false, io.Discard, &stderr); code != tc.want {
			t.Errorf("row draining=%v: code = %d, want %d; stderr=%s", tc.row, code, tc.want, stderr.String())
		}
		if stderr.Len() != 0 {
			t.Errorf("row draining=%v: stderr = %q, want empty", tc.row, stderr.String())
		}
	}
	// A row that cannot be read is still unknown: the guard answers nothing.
	if code := doRuntimeDrainCheck(dops, drainCheckRow(false, errors.New("ledger unreachable")), "worker", "worker", false, io.Discard, io.Discard); code != drainCheckUnknownExit {
		t.Errorf("unreadable row: code = %d, want %d", code, drainCheckUnknownExit)
	}
}

// writeMalformedDescriptor makes cityDir hold a descriptor that does not
// decode, as a newer gc's descriptor would look to this one, without marking
// any invocation.
func writeMalformedDescriptor(t *testing.T, cityDir string) {
	t.Helper()
	writeControlPlaneBytes(t, cityDir, []byte("schema = ["))
}

// TestWorkerSiteMalformedDescriptorFailsClosedAtTheSeams: presence makes a
// worker site, so a descriptor that does not decode leaves every seam unable
// to act or dial, whether or not the command went through the gate.
func TestWorkerSiteMalformedDescriptorFailsClosedAtTheSeams(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	t.Setenv("GC_BEADS", "file")
	writeMalformedDescriptor(t, cityDir)
	dials := countSocketDials(t)
	spec := runtime.WorkerSpec{Runtime: "fake", Transport: config.SessionTransportTmux}

	sp, err := resolveWorkerSpec(nil, spec, config.SessionConfig{}, "box-city", cityDir)
	if _, isGuard := sp.(*guard.Provider); err != nil || !isGuard {
		t.Fatalf("resolveWorkerSpec = (%T, %v), want the guard", sp, err)
	}
	if !cityUsesManagedReconciler(cityDir) {
		t.Error("cityUsesManagedReconciler = false, want true")
	}
	for name, poke := range map[string]func(string) error{
		"pokeController":         pokeController,
		"reloadControllerConfig": reloadControllerConfig,
		"pokeControlDispatch":    pokeControlDispatch,
	} {
		if err := poke(cityDir); err != errWorkerSiteNoController { //nolint:errorlint // identity proves the short-circuit
			t.Errorf("%s = %v, want the bare errWorkerSiteNoController", name, err)
		}
	}
	pingNudgeWakeSocket(cityDir)
	var descErr *errControlPlaneDescriptor
	if err := refuseControllerOnWorkerSite(cityDir); !errors.As(err, &descErr) {
		t.Errorf("refuseControllerOnWorkerSite = %v, want the descriptor error", err)
	}
	if err := workerSiteClassRefusal(cityDir, config.BeadClassSessions, config.StorageWorkBinding); !errors.As(err, &descErr) {
		t.Errorf("workerSiteClassRefusal = %v, want the descriptor error", err)
	}
	if got := dials.Load(); got != 0 {
		t.Errorf("seams dialed %d times on a malformed descriptor, want 0", got)
	}

	// An exempt command resolves the city, and its invocation is marked: the
	// city-less senders and a provider built with no city path refuse too.
	prev := controlPlaneGateExempt
	t.Cleanup(func() { controlPlaneGateExempt = prev })
	controlPlaneGateExempt = true
	t.Setenv("GC_CITY", cityDir)
	if _, err := resolveContext(); err != nil {
		t.Fatalf("exempt resolveContext = %v, want the city", err)
	}
	if _, err := dialGCSocket(supervisorSocketPath(), time.Millisecond); !errors.Is(err, errWorkerSiteNoController) {
		t.Errorf("dialGCSocket after an exempt resolution = %v, want errWorkerSiteNoController", err)
	}
	if sp, _ := resolveWorkerSpec(nil, spec, config.SessionConfig{}, "", ""); !isGuardProvider(sp) {
		t.Errorf("resolveWorkerSpec with no city path = %T, want the guard", sp)
	}
	if got := dials.Load(); got != 0 {
		t.Errorf("dialed %d times after the exempt resolution, want 0", got)
	}
}

func isGuardProvider(sp runtime.Provider) bool {
	_, ok := sp.(*guard.Provider)
	return ok
}

// TestWorkerSiteMalformedDescriptorThroughRun runs the verbs that find their
// city from the session environment, not the context resolver: each names
// the descriptor and acts on nothing.
func TestWorkerSiteMalformedDescriptorThroughRun(t *testing.T) {
	for _, tc := range []struct {
		args []string
		// hook verbs exit 0 by design: a hook that fails breaks the agent.
		wantExit0 bool
		wantCode  int // when non-zero, the exact exit code
	}{
		{args: []string{"handoff", "--auto", "context cycle"}},
		{args: []string{"runtime", "request-restart"}},
		// Unknown (3), not the Local answer "not draining" (1).
		{args: []string{"runtime", "drain-check"}, wantCode: drainCheckUnknownExit},
		{args: []string{"nudge", "drain", "--inject"}, wantExit0: true},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			resetControlPlaneForTest(t)
			f := newHookBudgetFixture(t)
			countBeads := func() int {
				t.Helper()
				all, err := openNudgeBeadStore(f.cityDir).List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
				if err != nil {
					t.Fatalf("counting beads: %v", err)
				}
				return len(all)
			}
			before := countBeads()
			writeMalformedDescriptor(t, f.cityDir)
			dials := countSocketDials(t)

			var stdout, stderr bytes.Buffer
			code := run(tc.args, &stdout, &stderr)
			if tc.wantExit0 != (code == 0) || (tc.wantCode != 0 && code != tc.wantCode) {
				t.Fatalf("exit = %d, want exit-0 %v (code %d); stderr=%s", code, tc.wantExit0, tc.wantCode, stderr.String())
			}
			if !strings.Contains(stderr.String(), "control-plane descriptor") {
				t.Fatalf("stderr does not name the descriptor:\n%s", stderr.String())
			}
			if got := dials.Load(); got != 0 {
				t.Fatalf("dialed %d gc sockets through dialGCSocket, want 0", got)
			}
			// Count with the descriptor gone, as a Local reader would.
			if err := os.Remove(filepath.Join(f.cityDir, ".gc", "control-plane.toml")); err != nil {
				t.Fatal(err)
			}
			forgetControlPlane(f.cityDir)
			resetHookBudgetProcessState(t)
			if after := countBeads(); after != before {
				t.Fatalf("the command wrote %d beads on a malformed descriptor, want none", after-before)
			}
			if stdout.Len() != 0 && !tc.wantExit0 {
				t.Fatalf("stdout = %q, want nothing", stdout.String())
			}
		})
	}
}

// TestWorkerSiteIsDecidedPerCity is the multi-city host: the supervisor looks
// up a worker-site city and then a Local one in the same process. The Local
// city keeps its own provider, its dials and its controller.
func TestWorkerSiteIsDecidedPerCity(t *testing.T) {
	resetControlPlaneForTest(t)
	boxCity := newWorkerSiteCity(t)
	localCity := t.TempDir()
	t.Setenv("GC_BEADS", "file")
	writeControlPlaneFixture(t, boxCity, testDescriptor(boxCity, nil))
	spec := runtime.WorkerSpec{Runtime: "fake", Transport: config.SessionTransportTmux}

	// The host's lookups for the box city.
	if sp, _ := resolveWorkerSpec(nil, spec, config.SessionConfig{}, "box", boxCity); !isGuardProvider(sp) {
		t.Fatalf("the box city resolved %T, want the guard", sp)
	}
	if err := refuseControllerOnWorkerSite(boxCity); !errors.Is(err, errWorkerSiteRefusesController) {
		t.Fatalf("refuseControllerOnWorkerSite(box) = %v, want the refusal", err)
	}
	if workerSiteProcess.Load() {
		t.Fatal("a host's lookup of one city marked the whole process a worker site")
	}

	dials := countSocketDials(t)
	if sp, err := resolveWorkerSpec(nil, spec, config.SessionConfig{}, "local", localCity); err != nil || isGuardProvider(sp) {
		t.Fatalf("the Local city resolved (%T, %v), want its configured runtime", sp, err)
	}
	if err := refuseControllerOnWorkerSite(localCity); err != nil {
		t.Fatalf("refuseControllerOnWorkerSite(local) = %v, want nil", err)
	}
	if err := pokeController(localCity); err == nil || errors.Is(err, errWorkerSiteNoController) {
		t.Fatalf("pokeController(local) = %v, want a dial failure", err)
	}
	pingNudgeWakeSocket(localCity)
	if got := dials.Load(); got != 2 {
		t.Fatalf("the Local city dialed %d times, want 2 (the supervisor fallback and the wake ping)", got)
	}
}

// TestWorkerSitePingNudgeWakeSocketDialsOnlyLocally is the wake ping's own
// control: one counted dial on the Local plane, none on a worker site whose
// plane only the ping's city path says.
func TestWorkerSitePingNudgeWakeSocketDialsOnlyLocally(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	dials := countSocketDials(t)
	pingNudgeWakeSocket(cityDir)
	if got := dials.Load(); got != 1 {
		t.Fatalf("Local wake ping dials = %d, want 1", got)
	}
	writeControlPlaneFixture(t, cityDir, testDescriptor(cityDir, nil))
	pingNudgeWakeSocket(cityDir)
	if got := dials.Load(); got != 1 {
		t.Fatalf("worker-site wake ping reached the dialer: dials = %d, want 1", got)
	}
}

// TestWorkerSiteControllerStopDialsNothing: gc stop's controller client dials
// through the city seam and fails closed on a worker site, whose controller is
// the remote one, whether or not anything stands at the socket path. A file
// stands there first so the Local control gets past the client's stat to the
// dial.
func TestWorkerSiteControllerStopDialsNothing(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	sock := controllerSocketPath(cityDir)
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dials := countSocketDials(t)
	if res := sendControllerStop(cityDir, false); res.outcome == controllerStopAcknowledged {
		t.Fatal("the counting dialer acknowledged a stop")
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("Local controller stop dials = %d, want 1", got)
	}
	writeControlPlaneFixture(t, cityDir, testDescriptor(cityDir, nil))
	for _, leg := range []string{"socket file present", "no socket file"} {
		if leg == "no socket file" {
			if err := os.Remove(sock); err != nil {
				t.Fatal(err)
			}
		}
		res := sendControllerStop(cityDir, false)
		if res.outcome != controllerStopOutcomeInvalid || !errors.Is(res.err, errWorkerSiteNoController) {
			t.Fatalf("%s: worker-site stop = %v (%v), want invalid wrapping errWorkerSiteNoController", leg, res.outcome, res.err)
		}
		if got := dials.Load(); got != 1 {
			t.Fatalf("%s: worker-site controller stop reached the dialer: dials = %d, want 1", leg, got)
		}
	}
}

// TestWorkerSiteCmdStopBodyRefuses: the stop body refuses a worker site before
// it asks a controller to stop or cleans up directly. A box has no controller
// socket, so without the refusal the direct path would mark the box's sessions
// asleep, stop them and retire the bead store behind the remote controller.
func TestWorkerSiteCmdStopBodyRefuses(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	t.Setenv("GC_BEADS", "file")
	active, err := openNudgeBeadStore(cityDir).Create(beads.Bead{
		Title:    "active",
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel},
		Metadata: map[string]string{"state": "active", "session_name": "active"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "active", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	oldFactory := sessionProviderForStopCity
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
	sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return sp, nil }
	shutdowns := 0
	overrideShutdownBeadsProviderForStop(t, func(string) error {
		shutdowns++
		return nil
	})
	writeControlPlaneFixture(t, cityDir, testDescriptor(cityDir, nil))
	dials := countSocketDials(t)
	cfg := &config.City{
		Workspace: config.Workspace{Name: "box-city"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "0s"},
	}

	var stdout, stderr lockedBuffer
	if code := cmdStopBody(cityDir, cfg, false, &stdout, &stderr); code == 0 {
		t.Fatalf("cmdStopBody on a worker site = 0, want a refusal; stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), errWorkerSiteRefusesController.Error()) {
		t.Fatalf("stderr = %q, want the worker-site refusal", stderr.String())
	}
	if got := dials.Load(); got != 0 {
		t.Fatalf("the refused stop dialed %d gc sockets, want 0", got)
	}
	if shutdowns != 0 {
		t.Fatalf("the refused stop shut the bead store down %d times, want 0", shutdowns)
	}
	if !sp.IsRunning("active") {
		t.Fatal("the refused stop stopped a box session")
	}
	// Read with the descriptor gone, as a Local reader would.
	if err := os.Remove(filepath.Join(cityDir, ".gc", "control-plane.toml")); err != nil {
		t.Fatal(err)
	}
	forgetControlPlane(cityDir)
	got, err := openNudgeBeadStore(cityDir).Get(active.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reason := got.Metadata["sleep_reason"]; reason != "" {
		t.Fatalf("the refused stop marked the session asleep: sleep_reason = %q", reason)
	}
}

// TestWorkerSiteStorageFunnelRefusesUnattachedBindings drives the one-shot
// storage funnel itself on a split city whose infrastructure classes sit on a
// sqlite-beads binding the descriptor does not attach: every door (the class
// store, the by-id resolver, the graph binding) gets a standing refusal, work
// ids still resolve to work, and the binding is never opened.
func TestWorkerSiteStorageFunnelRefusesUnattachedBindings(t *testing.T) {
	cityPath, cfg := migratedOneShotCLICity(t)
	resetControlPlaneForTest(t)
	stderr := captureCLIStorageStderr(t)

	// Seed a graph bead into the binding while the city is still Local.
	graphStore, relocated := cliStorageRoutes(cityPath).storeFor(coordclassFor(config.BeadClassGraph))
	if !relocated {
		t.Fatal("fixture: graph is not relocated")
	}
	graphBead, err := graphStore.Create(beads.Bead{Title: "lives only in the binding", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the binding: %v", err)
	}
	if !bdIDIsClassReserved(graphBead.ID) {
		t.Fatalf("fixture: %q is not in a reserved class namespace", graphBead.ID)
	}
	if err := closeCLIStorageRoutes(); err != nil {
		t.Fatal(err)
	}
	writeControlPlaneFixture(t, cityPath, testDescriptor(cityPath, nil))
	work := beads.NewMemStore()
	workBead, err := work.Create(beads.Bead{Title: "work", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}

	assertWorkerSiteRefusal := func(t *testing.T, what string, err error) {
		t.Helper()
		var refusal errWorkerSiteLocalState
		if !errors.As(err, &refusal) {
			t.Fatalf("%s = %v, want errWorkerSiteLocalState", what, err)
		}
	}

	routes := cliStorageRoutes(cityPath)
	store, relocated := routes.storeFor(coordclassFor(config.BeadClassSessions))
	if !relocated {
		t.Fatal("a worker site's unattached sessions class resolved to the work store")
	}
	_, err = store.Get("any-1")
	assertWorkerSiteRefusal(t, "the funnel's sessions store Get", err)
	if !storeref.IsStandingRefusal(err) {
		t.Fatalf("the funnel's refusal %v is not a standing refusal", err)
	}
	if !strings.Contains(stderr.String(), "worker site") {
		t.Fatalf("funnel stderr = %q, want the worker-site refusal once", stderr.String())
	}

	_, err = classRoutedStoreForID(cityPath, graphBead.ID, work)
	assertWorkerSiteRefusal(t, "classRoutedStoreForID(graph id)", err)
	if got, err := classRoutedStoreForID(cityPath, workBead.ID, work); err != nil || got != beads.Store(work) {
		t.Fatalf("classRoutedStoreForID(work id) = (%T, %v), want the work store", got, err)
	}
	graph := cityGraphClassBinding(cityPath)
	if graph == nil {
		t.Fatal("cityGraphClassBinding = nil, want the refused store, not the work store")
	}
	_, err = graph.Get(graphBead.ID)
	assertWorkerSiteRefusal(t, "the graph binding's Get", err)
	_, err = resolveClassStore(routes, work, cfg, cityPath, config.BeadClassGraph, nil).Get(graphBead.ID)
	assertWorkerSiteRefusal(t, "resolveClassStore(graph).Get", err)
	if got := resolveClassStore(routes, work, cfg, cityPath, config.BeadClassWork, nil); got != beads.Store(work) {
		t.Fatalf("the work class resolved %T, want the work store", got)
	}
}

// TestWorkerSiteStorageFunnelCreatesNoBindingRoot is the box as the export
// leaves it: a sqlite-beads binding configured, nothing on disk. The Local
// control shows the funnel would create the binding root; on a worker site it
// creates nothing.
func TestWorkerSiteStorageFunnelCreatesNoBindingRoot(t *testing.T) {
	for _, workerSite := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "worker site"}[workerSite], func(t *testing.T) {
			resetControlPlaneForTest(t)
			bindingRoot := filepath.Join(t.TempDir(), "store")
			cityPath := oneShotCLICity(t, bindingRoot)
			stubInfraControllerPing(t, 0)
			captureCLIStorageStderr(t)
			if workerSite {
				writeControlPlaneFixture(t, cityPath, testDescriptor(cityPath, nil))
			}
			_ = cliStorageRoutes(cityPath)
			_, err := os.Stat(bindingRoot)
			if created := err == nil; created == workerSite {
				t.Fatalf("binding root created = %v on worker site %v", created, workerSite)
			}
		})
	}
}

// TestWorkerSiteClassStoreRefusesABoxStorageThatDisagrees: the descriptor
// puts sessions on the infra ledger but the box's [storage] leaves it on
// work. The work binding is attached, but not for sessions, so the class
// refuses instead of opening the wrong ledger.
func TestWorkerSiteClassStoreRefusesABoxStorageThatDisagrees(t *testing.T) {
	resetControlPlaneForTest(t)
	cityPath := oneShotCLICity(t, "")
	captureCLIStorageStderr(t)
	writeControlPlaneFixture(t, cityPath, testDescriptor(cityPath, map[string][]string{"infra": {"sessions"}}))
	work := beads.NewMemStore()
	routes := cliStorageRoutes(cityPath)
	_, err := resolveClassStore(routes, work, nil, cityPath, config.BeadClassSessions, nil).Get("any-1")
	var refusal errWorkerSiteLocalState
	if !errors.As(err, &refusal) || refusal.what != "sessions store" {
		t.Fatalf("sessions store Get = %v, want errWorkerSiteLocalState{sessions store}", err)
	}
	if got := resolveClassStore(routes, work, nil, cityPath, config.BeadClassWork, nil); got != beads.Store(work) {
		t.Fatalf("the work class resolved %T, want the work store", got)
	}
}

func TestWorkerSiteRegisterRefuses(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	t.Setenv("GC_HOME", t.TempDir())
	writeControlPlaneFixture(t, cityDir, testDescriptor(cityDir, nil))
	reached := false
	prev := registerCityWithSupervisorTestHook
	registerCityWithSupervisorTestHook = func(string, string, io.Writer, io.Writer) (bool, int) {
		reached = true
		return true, 0
	}
	t.Cleanup(func() { registerCityWithSupervisorTestHook = prev })

	var stdout, stderr bytes.Buffer
	if code := registerCityWithSupervisorNamed(cityDir, "", &stdout, &stderr, "gc register", false); code == 0 || reached {
		t.Fatalf("register on a worker site = %d (reached the registry write %v), want a refusal", code, reached)
	}
	if !strings.Contains(stderr.String(), errWorkerSiteRefusesController.Error()) {
		t.Fatalf("stderr = %q, want the worker-site refusal", stderr.String())
	}
}

// TestWorkerSiteSupervisorStartAndInstallRefuse: HOME is moved, so if the
// worker-site refusal were missing the home-override guard would stop the
// command before it forks or installs anything.
func TestWorkerSiteSupervisorStartAndInstallRefuse(t *testing.T) {
	if goruntime.GOOS != "linux" && goruntime.GOOS != "darwin" {
		t.Skip("the home-override backstop only applies on linux/darwin")
	}
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	writeControlPlaneFixture(t, cityDir, testDescriptor(cityDir, nil))
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	for name, cmd := range map[string]func(io.Writer, io.Writer) int{
		"start":   doSupervisorStart,
		"install": doSupervisorInstall,
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := cmd(&stdout, &stderr); code == 0 {
				t.Fatalf("gc supervisor %s on a worker site = 0", name)
			}
			if !strings.Contains(stderr.String(), errWorkerSiteRefusesController.Error()) {
				t.Fatalf("stderr = %q, want the worker-site refusal", stderr.String())
			}
		})
	}
}

// TestWorkerSiteSupervisorRefusesToStartAWorkerSiteCity is the supervisor's
// per-city start: a registered city holding a descriptor is refused before
// any of its init runs, whatever city the supervisor was started in. The
// process cached the city as Local before the descriptor was written, so the
// start attempt must read the file as it is now.
func TestWorkerSiteSupervisorRefusesToStartAWorkerSiteCity(t *testing.T) {
	resetControlPlaneForTest(t)
	t.Setenv("GC_HOME", t.TempDir())
	cityDir := newWorkerSiteCity(t)
	writeControlPlaneFixture(t, cityDir, testDescriptor(cityDir, nil))
	// Cache the city as Local with the descriptor moved aside, then put it
	// back without telling the cache.
	descriptorPath := filepath.Join(cityDir, citylayout.ControlPlaneDescriptorPath)
	if err := os.Rename(descriptorPath, descriptorPath+".aside"); err != nil {
		t.Fatal(err)
	}
	if plane, err := controlPlaneFor(cityDir); err != nil || plane.Worker() {
		t.Fatalf("controlPlaneFor without the descriptor = (%+v, %v), want Local", plane, err)
	}
	if err := os.Rename(descriptorPath+".aside", descriptorPath); err != nil {
		t.Fatal(err)
	}
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityDir, "box-city"); err != nil {
		t.Fatal(err)
	}
	cr := newCityRegistry()
	var stdout, stderr bytes.Buffer
	startOneCity(supervisor.CityEntry{Path: cityDir, Name: "box-city"}, reg, cr, supervisor.PublicationConfig{}, &stdout, &stderr)
	if !strings.Contains(stderr.String(), errWorkerSiteRefusesController.Error()) {
		t.Fatalf("stderr = %q, want the worker-site refusal", stderr.String())
	}
	if cr.Has(cityDir) {
		t.Fatal("the supervisor published a managed city for a worker site")
	}
	if workerSiteProcess.Load() {
		t.Fatal("refusing one managed city marked the supervisor process a worker site")
	}
}

func TestWorkerSiteRestartRefuses(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	writeControlPlaneFixture(t, cityDir, testDescriptor(cityDir, nil))
	var stdout, stderr bytes.Buffer
	if code := cmdRestartJSON([]string{cityDir}, &stdout, &stderr, false); code == 0 {
		t.Fatalf("gc restart on a worker site = 0; stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), errWorkerSiteRefusesController.Error()) {
		t.Fatalf("stderr = %q, want the worker-site refusal", stderr.String())
	}
}

// TestRequireBootstrappedCityMalformedDescriptorHasOneRemedy: the city is
// there, so the gc init hint would contradict the descriptor's own remedy.
func TestRequireBootstrappedCityMalformedDescriptorHasOneRemedy(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	writeMalformedDescriptor(t, cityDir)
	_, err := requireBootstrappedCity(cityDir)
	var descErr *errControlPlaneDescriptor
	if !errors.As(err, &descErr) {
		t.Fatalf("requireBootstrappedCity = %v, want the descriptor error", err)
	}
	if strings.Contains(err.Error(), "gc init") {
		t.Fatalf("error = %v, want no gc init hint", err)
	}
}

// TestWorkerSiteV2DrainAckSucceedsOnItsRowAck: under v2 the row is the ack,
// so the guard's refusal to mirror it into a runtime is a warning after the
// row commit, not a failure. Under legacy there is no row ack and the guard's
// refusal stays loud (D-DRAINACK).
func TestWorkerSiteV2DrainAckSucceedsOnItsRowAck(t *testing.T) {
	for _, tc := range []struct {
		reconciler string
		wantCode   int
	}{{reconciler: "v2", wantCode: 0}, {reconciler: "", wantCode: 1}} {
		t.Run("reconciler="+tc.reconciler, func(t *testing.T) {
			resetControlPlaneForTest(t)
			cityPath, store, id := drainAckCity(t, tc.reconciler)
			writeControlPlaneFixture(t, cityPath, testDescriptor(cityPath, nil))
			// The stub keeps the poke hermetic. On a worker site the real
			// seam, enqueueController, returns the same sentinel without
			// dialing (TestWorkerSiteSendersDialNothing).
			prevPoke := drainAckPokeController
			drainAckPokeController = func(string, reconcilekey.Key) error { return errWorkerSiteNoController }
			t.Cleanup(func() { drainAckPokeController = prevPoke })

			target := sessionRuntimeTarget{cityPath: cityPath, display: "worker", sessionName: "worker"}
			var stdout, stderr bytes.Buffer
			code := runtimeDrainAck(newDrainOps(guard.New()), target, false, "tok-a", false, &stdout, &stderr)
			if code != tc.wantCode {
				t.Fatalf("drain-ack = %d, want %d; stderr=%s", code, tc.wantCode, stderr.String())
			}
			if tc.wantCode != 0 {
				return
			}
			if ack := rowAck(t, store, id); ack == "" {
				t.Fatal("the row ack was not written")
			}
			if !strings.Contains(stderr.String(), "row ack is in force") {
				t.Fatalf("stderr = %q, want the not-applicable warning", stderr.String())
			}
			assertWorkerSitePokeNotice(t, stderr.String())
			if !strings.Contains(stdout.String(), "remote controller") || strings.Contains(stdout.String(), "Controller poked") {
				t.Fatalf("stdout = %q, want the recorded-ack line, not a claimed poke", stdout.String())
			}
		})
	}
}

// assertNoControllerReached fails if anything dialed through the gc socket
// seam or reached the fake controller on the city's socket, which a keyed poke
// dials directly, past the counting dialer.
func assertNoControllerReached(t *testing.T, dials *atomic.Int32, accepted <-chan struct{}) {
	t.Helper()
	if got := dials.Load(); got != 0 {
		t.Fatalf("dialed %d gc sockets through the seam, want 0", got)
	}
	select {
	case <-accepted:
		t.Fatal("a worker-site command reached the controller socket")
	default:
	}
}

// workerSiteRestartSeeds are the sessions a restart request reaches: an
// ordinary one, whose bead marker backs up the runtime flag on a Local city,
// and a pinned named one, whose marker is mandatory there. On a worker site
// the marker is the whole request for both.
var workerSiteRestartSeeds = []struct {
	name string
	seed map[string]string
}{
	{name: "session", seed: map[string]string{"state": "awake", "alias": "worker"}},
	{name: "pinned named session", seed: map[string]string{
		"state": "awake", "alias": "worker",
		"configured_named_session": "true", "configured_named_mode": "always", "pin_awake": "true",
	}},
}

// assertRestartMarkerRecorded fails unless the one session bead carries the
// restart marker the remote controller's reconciler reads, with the reset that
// lifts a pinned session's kill protection.
func assertRestartMarkerRecorded(t *testing.T, all []beads.Bead) {
	t.Helper()
	if len(all) != 1 {
		t.Fatalf("session beads = %d, want exactly one: %+v", len(all), all)
	}
	if got := all[0].Metadata; got["restart_requested"] != "true" || got["continuation_reset_pending"] != "true" {
		t.Fatalf("restart_requested = %q, continuation_reset_pending = %q; want both recorded for the remote controller", got["restart_requested"], got["continuation_reset_pending"])
	}
}

// TestWorkerSiteRequestRestartRecordsTheBeadMarker: the guard holds no runtime
// to set the restart flag on, so on a worker site the bead marker the remote
// controller's reconciler reads is the whole request, for an ordinary session
// and a pinned one alike. The follow-up poke is the worker-site notice and
// dials nothing, and the command says the remote controller acts.
func TestWorkerSiteRequestRestartRecordsTheBeadMarker(t *testing.T) {
	for _, tc := range workerSiteRestartSeeds {
		t.Run(tc.name, func(t *testing.T) {
			cityDir, _ := newWorkerSiteSessionCity(t, tc.seed)
			t.Setenv("GC_ALIAS", "worker")
			t.Setenv("GC_SESSION_NAME", "worker-1")
			accepted := startFakeControllerSocket(t, cityDir, "ok\n")
			dials := countSocketDials(t)

			var stdout, stderr bytes.Buffer
			if code := cmdRuntimeRequestRestart(&stdout, &stderr); code != 0 {
				t.Fatalf("gc runtime request-restart = %d on a worker site, want 0; stderr=%s", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), "remote controller") || strings.Contains(stdout.String(), "controller notified") {
				t.Fatalf("stdout = %q, want the recorded-restart line, not a claimed notification", stdout.String())
			}
			assertWorkerSitePokeNotice(t, stderr.String())
			assertNoControllerReached(t, dials, accepted)
			assertRestartMarkerRecorded(t, localSessionBeads(t, cityDir))
		})
	}
}

// TestWorkerSiteRestartWithoutItsMarkerRequestsNothing: the marker is the
// whole request on a worker site, so request-restart and handoff fail without
// it and promise no restart, for an ordinary session as for a pinned one.
func TestWorkerSiteRestartWithoutItsMarkerRequestsNothing(t *testing.T) {
	for _, tc := range []struct {
		name           string
		persistRestart func() error
	}{
		{name: "no restart persistence"},
		{name: "marker write fails", persistRestart: func() error { return errors.New("ledger unreachable") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			dops := newDrainOps(guard.New())

			rec := events.NewFake()
			var stdout, stderr bytes.Buffer
			if code := doRuntimeRequestRestart(dops, tc.persistRestart, false, rec, "worker", "worker", t.TempDir(), &stdout, &stderr); code != 1 {
				t.Fatalf("request-restart = %d, want 1; stderr=%s", code, stderr.String())
			}
			if strings.Contains(stdout.String(), "Restart requested") || len(rec.Events) != 0 {
				t.Fatalf("request-restart promised a restart it did not record: stdout=%q events=%v", stdout.String(), rec.Events)
			}

			store := beads.NewMemStore()
			if _, err := store.Create(beads.Bead{Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: map[string]string{"session_name": "worker"}}); err != nil {
				t.Fatalf("seeding the session: %v", err)
			}
			rec = events.NewFake()
			stdout.Reset()
			stderr.Reset()
			outcome := doHandoffWithOutcome(store, store, rec, dops, tc.persistRestart, "worker", "worker", []string{"HANDOFF: context full"}, &stdout, &stderr)
			if outcome.code != 1 || outcome.restartRequested || strings.Contains(stdout.String(), "requesting restart") {
				t.Fatalf("handoff = %+v, stdout = %q; want 1 and no restart promised; stderr=%s", outcome, stdout.String(), stderr.String())
			}
			for _, e := range rec.Events {
				if e.Type == events.SessionDraining {
					t.Fatalf("handoff recorded %s without a restart marker: %v", e.Type, rec.Events)
				}
			}
		})
	}
}

// TestWorkerSiteHandoffRecordsTheRestart: a self handoff on a worker site sends
// its mail and records the restart as the bead marker, the one restart signal a
// box can leave for the remote controller, and its poke is the worker-site
// notice. For a named session the controller does not restart, the handoff
// keeps its mail and clears a stale marker from the bead; the guard holds no
// runtime flag to clear.
func TestWorkerSiteHandoffRecordsTheRestart(t *testing.T) {
	for _, tc := range workerSiteRestartSeeds {
		t.Run(tc.name, func(t *testing.T) {
			cityDir, _ := newWorkerSiteSessionCity(t, tc.seed)
			t.Setenv("GC_ALIAS", "worker")
			t.Setenv("GC_SESSION_NAME", "worker-1")
			accepted := startFakeControllerSocket(t, cityDir, "ok\n")
			dials := countSocketDials(t)

			var stdout, stderr bytes.Buffer
			if code := cmdHandoffWithForce([]string{"HANDOFF: context full"}, "", false, "", false, &stdout, &stderr); code != 0 {
				t.Fatalf("gc handoff = %d on a worker site, want 0; stderr=%s", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), "requesting restart") {
				t.Fatalf("stdout = %q, want the restart request", stdout.String())
			}
			assertWorkerSitePokeNotice(t, stderr.String())
			assertNoControllerReached(t, dials, accepted)
			ledger := localLedger(t, cityDir)
			assertRestartMarkerRecorded(t, sessionBeadsIn(t, ledger))
			if got := messageBeadsIn(t, ledger); len(got) != 1 {
				t.Fatalf("handoff mail beads = %d, want one: %+v", len(got), got)
			}
		})
	}
	t.Run("named session the controller does not restart", func(t *testing.T) {
		cityDir, _ := newWorkerSiteSessionCity(t, map[string]string{
			"state": "awake", "alias": "worker",
			"configured_named_session": "true", "configured_named_mode": "on_demand",
			"restart_requested": "true", "continuation_reset_pending": "true",
		})
		t.Setenv("GC_ALIAS", "worker")
		t.Setenv("GC_SESSION_NAME", "worker-1")
		accepted := startFakeControllerSocket(t, cityDir, "ok\n")
		dials := countSocketDials(t)

		var stdout, stderr bytes.Buffer
		if code := cmdHandoffWithForce([]string{"HANDOFF: context full"}, "", false, "", false, &stdout, &stderr); code != 0 {
			t.Fatalf("gc handoff = %d on a worker site, want 0; stderr=%s", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "restart skipped") {
			t.Fatalf("stdout = %q, want the skipped restart", stdout.String())
		}
		assertNoControllerReached(t, dials, accepted)
		ledger := localLedger(t, cityDir)
		all := sessionBeadsIn(t, ledger)
		if len(all) != 1 || all[0].Metadata["restart_requested"] != "" || all[0].Metadata["continuation_reset_pending"] != "" {
			t.Fatalf("the stale restart marker was not cleared: %+v", all)
		}
		if got := messageBeadsIn(t, ledger); len(got) != 1 {
			t.Fatalf("handoff mail beads = %d, want one: %+v", len(got), got)
		}
	})
}

// TestWorkerSiteRuntimeDrainAndUndrainRefuseBeforeWriting: the agent learns of
// a drain from the runtime drain flag, which only the controller's runtime
// holds, so on a worker site gc runtime drain and undrain refuse, say nothing
// was recorded, and leave the row as it was. A target that does not exist gets
// the same refusal, which pins the refusal ahead of target resolution.
func TestWorkerSiteRuntimeDrainAndUndrainRefuseBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(args []string, stdout, stderr io.Writer) int
		seed map[string]string
	}{
		{name: "drain", run: func(args []string, stdout, stderr io.Writer) int {
			return cmdRuntimeDrain(args, false, stdout, stderr)
		}, seed: map[string]string{"state": "awake"}},
		{name: "undrain", run: func(args []string, stdout, stderr io.Writer) int {
			return cmdRuntimeUndrain(args, false, stdout, stderr)
		}, seed: map[string]string{"state": "awake", session.OperatorDrainKey: "1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityDir, sessionID := newWorkerSiteSessionCity(t, tc.seed)
			dials := countSocketDials(t)

			for _, target := range []string{sessionID, "no-such-session"} {
				var stdout, stderr bytes.Buffer
				if code := tc.run([]string{target}, &stdout, &stderr); code != 1 {
					t.Fatalf("gc runtime %s %s on a worker site = %d, want 1; stdout=%q stderr=%q", tc.name, target, code, stdout.String(), stderr.String())
				}
				if !strings.Contains(stderr.String(), errWorkerSiteDrainNeedsController.Error()) {
					t.Fatalf("gc runtime %s %s: stderr = %q, want the drain refusal", tc.name, target, stderr.String())
				}
				if stdout.Len() != 0 {
					t.Fatalf("gc runtime %s %s: stdout = %q, want nothing", tc.name, target, stdout.String())
				}
			}
			if got := dials.Load(); got != 0 {
				t.Fatalf("the refused %s dialed %d gc sockets, want 0", tc.name, got)
			}
			all := localSessionBeads(t, cityDir)
			if len(all) != 1 || all[0].Metadata[session.OperatorDrainKey] != tc.seed[session.OperatorDrainKey] {
				t.Fatalf("the refused %s changed the session: %+v", tc.name, all)
			}
		})
	}
}

// TestWorkerSiteHandoffTargetRefusesBeforeWriting: a handoff to another
// session stops that session's runtime, which only the controller can do, and
// the guard cannot tell whether that runtime is live. So on a worker site gc
// handoff --target refuses before it resolves the target or sends its mail,
// rather than mailing a session it then reports as not running.
func TestWorkerSiteHandoffTargetRefusesBeforeWriting(t *testing.T) {
	cityDir, sessionID := newWorkerSiteSessionCity(t, map[string]string{"state": "awake", "alias": "worker"})
	t.Setenv("GC_ALIAS", "worker")
	dials := countSocketDials(t)

	for _, target := range []string{sessionID, "no-such-session"} {
		var stdout, stderr bytes.Buffer
		if code := cmdHandoffWithForce([]string{"HANDOFF: context full"}, target, false, "", false, &stdout, &stderr); code != 1 {
			t.Fatalf("gc handoff --target %s on a worker site = %d, want 1; stdout=%q stderr=%q", target, code, stdout.String(), stderr.String())
		}
		if !strings.Contains(stderr.String(), errWorkerSiteHandoffTargetNeedsController.Error()) {
			t.Fatalf("gc handoff --target %s: stderr = %q, want the handoff refusal", target, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Fatalf("gc handoff --target %s: stdout = %q, want nothing", target, stdout.String())
		}
	}
	if got := dials.Load(); got != 0 {
		t.Fatalf("the refused handoff dialed %d gc sockets, want 0", got)
	}
	if got := messageBeadsIn(t, localLedger(t, cityDir)); len(got) != 0 {
		t.Fatalf("the refused handoff sent %d mail beads, want none: %+v", len(got), got)
	}
}

// TestWorkerSiteStorageFunnelFailsClosedOnAMalformedDescriptor: the storage
// funnel fails closed on a descriptor that does not decode. It opens and
// creates no binding in the box, and the relocated class refuses with the
// descriptor error as a standing refusal.
func TestWorkerSiteStorageFunnelFailsClosedOnAMalformedDescriptor(t *testing.T) {
	resetControlPlaneForTest(t)
	bindingRoot := filepath.Join(t.TempDir(), "store")
	cityPath := oneShotCLICity(t, bindingRoot)
	stubInfraControllerPing(t, 0)
	captureCLIStorageStderr(t)
	writeControlPlaneBytes(t, cityPath, []byte("schema = ["))

	routes := cliStorageRoutes(cityPath)
	if _, err := os.Stat(bindingRoot); err == nil {
		t.Fatal("the funnel created the binding root on a malformed descriptor")
	}
	store, relocated := routes.storeFor(coordclassFor(config.BeadClassSessions))
	if !relocated {
		t.Fatal("sessions resolved to the work store")
	}
	_, err := store.Get("any-1")
	var descErr *errControlPlaneDescriptor
	if !errors.As(err, &descErr) || !storeref.IsStandingRefusal(err) {
		t.Fatalf("sessions Get = %v, want a standing descriptor refusal", err)
	}
	if !strings.Contains(err.Error(), "control-plane descriptor") {
		t.Fatalf("err = %v", err)
	}
}

// TestWorkerSiteCityStartRereadsTheDescriptor is the supervisor's per-city
// start after an operator removes a stray descriptor: the refusal is decided
// from the file as it is now, not from the plane the process cached.
func TestWorkerSiteCityStartRereadsTheDescriptor(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	writeControlPlaneFixture(t, cityDir, testDescriptor(cityDir, nil))
	if err := refuseCityStartOnWorkerSite(cityDir); !errors.Is(err, errWorkerSiteRefusesController) {
		t.Fatalf("refuseCityStartOnWorkerSite with the descriptor = %v, want the refusal", err)
	}
	if err := os.Remove(filepath.Join(cityDir, citylayout.ControlPlaneDescriptorPath)); err != nil {
		t.Fatal(err)
	}
	if err := refuseCityStartOnWorkerSite(cityDir); err != nil {
		t.Fatalf("refuseCityStartOnWorkerSite after the descriptor was removed = %v, want nil", err)
	}
	if workerSiteProcess.Load() {
		t.Fatal("the per-city start check marked the process a worker site")
	}
}
