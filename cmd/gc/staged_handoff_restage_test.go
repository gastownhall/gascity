package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// redHandoffSupersededByKey is the marker a superseded staged brief carries,
// naming the brief that replaced it.
const redHandoffSupersededByKey = "mail.superseded_by"

func TestRestageSupersedesPendingStagedBrief(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead := seedRestartableHandoffSession(t, store, "worker", "origin-instance")
	recorder := events.NewFake()

	mustStageSelfHandoff(t, store, recorder, nil, "first brief")
	first := onlyOpenHandoffMessage(t, store)
	stdout := mustStageSelfHandoff(t, store, recorder, nil, "second brief")

	second := onlyOpenHandoffMessage(t, store)
	if second.ID == first.ID {
		t.Fatalf("second staging reused %q, want a fresh staged brief", first.ID)
	}
	if second.Description != "second brief" {
		t.Fatalf("staged brief body = %q, want the latest brief", second.Description)
	}
	assertHandoffProvisionalRecord(t, second, "origin-instance")
	assertHandoffSuperseded(t, store, first.ID, second.ID)
	assertSessionStagesHandoff(t, store, sessionBead.ID, second.ID)
	if visible, err := beadmail.New(store).Check("worker"); err != nil || len(visible) != 0 {
		t.Fatalf("Check after restage = (%#v, %v), want no visible handoff", visible, err)
	}
	if !strings.Contains(stdout, first.ID) {
		t.Fatalf("stdout = %q, want it to name the superseded brief %q", stdout, first.ID)
	}
	assertEventCount(t, recorder.Events, redSessionHandoffStagedEvent, 2)
	assertEventCount(t, recorder.Events, redSessionHandoffRestartAcceptedEvent, 2)
	assertNoEventType(t, recorder.Events, events.MailSent)
}

func TestRetryAfterFailedRestartSupersedesProvisionalBrief(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead := seedPinnedHandoffSession(t, store, "worker", "origin-instance")
	recorder := events.NewFake()

	rejected, _, _ := runSelfHandoff(store, recorder, func() error { return errors.New("restart rejected") }, "first brief")
	if rejected.code != 1 || rejected.restartRequested {
		t.Fatalf("first handoff outcome = %#v, want failed restart", rejected)
	}
	first := onlyOpenHandoffMessage(t, store)

	mustStageSelfHandoff(t, store, recorder, func() error { return nil }, "retried brief")

	second := onlyOpenHandoffMessage(t, store)
	if second.ID == first.ID || second.Description != "retried brief" {
		t.Fatalf("retry left %q (%q) as the staged brief, want a fresh one carrying the retried body", second.ID, second.Description)
	}
	assertHandoffProvisionalRecord(t, second, "origin-instance")
	assertHandoffSuperseded(t, store, first.ID, second.ID)
	assertSessionStagesHandoff(t, store, sessionBead.ID, second.ID)
	assertEventCount(t, recorder.Events, redSessionHandoffFailedEvent, 1)
	assertEventCount(t, recorder.Events, redSessionHandoffStagedEvent, 2)
	assertEventCount(t, recorder.Events, redSessionHandoffRestartAcceptedEvent, 1)
}

func TestRestageAfterStagedTimeoutRestartsReleaseLifecycle(t *testing.T) {
	env, sessionBead := newHandoffReconcileSession(t, "origin-instance")
	recorder := events.NewFake()
	env.rec = recorder

	mustStageSelfHandoff(t, env.store, recorder, nil, "first brief")
	first := onlyOpenHandoffMessage(t, env.store)
	failStagedHandoffByTimeout(t, env, &sessionBead)
	assertEventCount(t, recorder.Events, redSessionHandoffFailedEvent, 1)

	mustStageSelfHandoff(t, env.store, recorder, nil, "retried brief")

	second := onlyOpenHandoffMessage(t, env.store)
	assertHandoffSuperseded(t, env.store, first.ID, second.ID)
	assertSessionStagesHandoff(t, env.store, sessionBead.ID, second.ID)
	restaged, err := env.store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("get session after retry: %v", err)
	}
	if marker := restaged.Metadata[redHandoffReleaseAttemptedAtKey]; marker != "" {
		t.Fatalf("retry kept release-attempt marker %q, so the retried brief could never be released", marker)
	}

	adoptHandoffSuccessor(t, env, &restaged, "successor-instance")
	env.reconcile([]beads.Bead{restaged})

	assertHandoffReleased(t, env.store, sessionBead.ID, second.ID)
	assertEventCount(t, recorder.Events, redSessionHandoffReleasedEvent, 1)
	visible, err := beadmail.New(env.store).Check("worker")
	if err != nil {
		t.Fatalf("Check after successor: %v", err)
	}
	if len(visible) != 1 || visible[0].ID != second.ID {
		t.Fatalf("Check after successor = %#v, want only the retried brief %q", visible, second.ID)
	}
}

func TestRestageDefersToControllerWhenEarlierBriefIsReleasable(t *testing.T) {
	env, sessionBead := newHandoffReconcileSession(t, "origin-instance")
	recorder := events.NewFake()
	env.rec = recorder

	mustStageSelfHandoff(t, env.store, recorder, nil, "first brief")
	first := onlyOpenHandoffMessage(t, env.store)
	// A successor has adopted the session but the controller has not reconciled
	// it yet: the first brief is releasable to this incarnation and undelivered.
	adoptHandoffSuccessor(t, env, &sessionBead, "successor-instance")

	outcome, _, stderr := runSelfHandoff(env.store, recorder, nil, "successor brief")
	if outcome.code != 1 || outcome.restartRequested {
		t.Fatalf("restage outcome = %#v, want it refused while the earlier brief awaits release", outcome)
	}
	if !strings.Contains(stderr, first.ID) {
		t.Fatalf("stderr = %q, want it to name the brief awaiting release %q", stderr, first.ID)
	}
	untouched := onlyOpenHandoffMessage(t, env.store)
	if untouched.ID != first.ID {
		t.Fatalf("refused restage left %q open, want only the earlier brief %q", untouched.ID, first.ID)
	}
	assertHandoffProvisionalRecord(t, untouched, "origin-instance")
	assertSessionStagesHandoff(t, env.store, sessionBead.ID, first.ID)

	refreshed, err := env.store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("get session before reconcile: %v", err)
	}
	env.reconcile([]beads.Bead{refreshed})
	assertHandoffReleased(t, env.store, sessionBead.ID, first.ID)
	assertEventCount(t, recorder.Events, redSessionHandoffReleasedEvent, 1)

	mustStageSelfHandoff(t, env.store, recorder, nil, "successor brief")
	delivered, err := env.store.Get(first.ID)
	if err != nil {
		t.Fatalf("get delivered brief: %v", err)
	}
	if delivered.Status != "open" || delivered.Metadata[redHandoffSupersededByKey] != "" {
		t.Fatalf("released brief was disturbed by the later restage: status=%q metadata=%#v", delivered.Status, delivered.Metadata)
	}
	staged := openStagedHandoffs(t, env.store)
	if len(staged) != 1 {
		t.Fatalf("staged briefs after restage = %#v, want exactly the successor's own", staged)
	}
	assertHandoffProvisionalRecord(t, staged[0], "successor-instance")
}

func TestRestageSupersedesWrittenOffBriefEvenAfterTokenChange(t *testing.T) {
	env, sessionBead := newHandoffReconcileSession(t, "origin-instance")
	recorder := events.NewFake()
	env.rec = recorder

	mustStageSelfHandoff(t, env.store, recorder, nil, "first brief")
	first := onlyOpenHandoffMessage(t, env.store)
	failStagedHandoffByTimeout(t, env, &sessionBead)
	// The controller wrote the first brief off and will never release it, so a
	// later incarnation staging its own brief must not be blocked behind it.
	adoptHandoffSuccessor(t, env, &sessionBead, "successor-instance")

	mustStageSelfHandoff(t, env.store, recorder, nil, "successor brief")

	second := onlyOpenHandoffMessage(t, env.store)
	assertHandoffSuperseded(t, env.store, first.ID, second.ID)
	assertHandoffProvisionalRecord(t, second, "successor-instance")
	assertSessionStagesHandoff(t, env.store, sessionBead.ID, second.ID)
}

// Staging supersedes only a brief that is still outstanding. These cases pass
// both before and after the supersede change; they pin that the new lookup
// never closes or re-stamps a brief something else already resolved.
func TestRestageLeavesResolvedPriorBriefAlone(t *testing.T) {
	tests := []struct {
		name       string
		resolve    func(store beads.Store, id string) error
		wantStatus string
		wantStaged string
	}{
		{
			// The release path flips the message before it clears the session
			// pointer, so a crash between the two leaves the pointer naming a
			// brief that was already delivered.
			name: "released but pointer not yet cleared",
			resolve: func(store beads.Store, id string) error {
				return store.Update(id, beads.UpdateOpts{
					RemoveLabels: []string{redHandoffStagedLabel},
					Metadata:     map[string]string{redHandoffStagedMetadataKey: "false"},
				})
			},
			wantStatus: "open",
			wantStaged: "false",
		},
		{
			name:       "archived by an operator",
			resolve:    func(store beads.Store, id string) error { return store.Close(id) },
			wantStatus: "closed",
			wantStaged: "true",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := beads.NewMemStore()
			sessionBead := seedRestartableHandoffSession(t, store, "worker", "origin-instance")
			recorder := events.NewFake()
			mustStageSelfHandoff(t, store, recorder, nil, "first brief")
			first := onlyOpenHandoffMessage(t, store)
			if err := test.resolve(store, first.ID); err != nil {
				t.Fatalf("resolve prior brief: %v", err)
			}
			// A brief is released only once a successor holds the session, so the
			// session token now differs from the one the prior brief was staged
			// for. That is what makes a wrongly-outstanding prior visible: it would
			// be refused as awaiting release instead of merely skipped.
			if err := store.SetMetadataBatch(sessionBead.ID, map[string]string{"instance_token": "successor-instance"}); err != nil {
				t.Fatalf("adopt successor: %v", err)
			}

			outcome, _, stderr := runSelfHandoff(store, recorder, nil, "second brief")
			if outcome.code != 0 || !outcome.restartRequested || stderr != "" {
				t.Fatalf("restage outcome = %#v stderr=%q, want a clean accepted restart", outcome, stderr)
			}

			prior, err := store.Get(first.ID)
			if err != nil {
				t.Fatalf("get prior brief: %v", err)
			}
			if prior.Status != test.wantStatus ||
				prior.Metadata[redHandoffStagedMetadataKey] != test.wantStaged ||
				prior.Metadata[redHandoffSupersededByKey] != "" {
				t.Fatalf("resolved prior brief was disturbed: status=%q metadata=%#v", prior.Status, prior.Metadata)
			}
			staged := openStagedHandoffs(t, store)
			if len(staged) != 1 || staged[0].ID == first.ID {
				t.Fatalf("staged briefs after restage = %#v, want exactly the new one", staged)
			}
			assertSessionStagesHandoff(t, store, sessionBead.ID, staged[0].ID)
		})
	}
}

func TestRestageIgnoresDanglingStagedPointer(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead := seedRestartableHandoffSession(t, store, "worker", "origin-instance")
	if err := store.SetMetadataBatch(sessionBead.ID, map[string]string{
		redHandoffStagedMessageIDKey: "gc-gone",
	}); err != nil {
		t.Fatalf("point session at a missing brief: %v", err)
	}

	outcome, _, stderr := runSelfHandoff(store, events.NewFake(), nil, "first brief")
	if outcome.code != 0 || !outcome.restartRequested || stderr != "" {
		t.Fatalf("handoff outcome = %#v stderr=%q, want a clean accepted restart", outcome, stderr)
	}
	staged := onlyOpenHandoffMessage(t, store)
	assertSessionStagesHandoff(t, store, sessionBead.ID, staged.ID)
}

// TestRestageRoutesEachClassToItsOwnStore stages twice with the message beads and
// the session bead on two different stores, the shape a relocated messaging or
// sessions class takes. Every other restage test hands one store in as both, and
// on one store a brief that is looked up or closed through the session store, or
// a pointer written through the message store, finds the same bead the right
// store would, so a swap between the two goes unnoticed.
func TestRestageRoutesEachClassToItsOwnStore(t *testing.T) {
	msgStore := &beads.MemStore{IDPrefix: "msg"}
	sessStore := &beads.MemStore{IDPrefix: "ses"}
	sessionBead := seedRestartableHandoffSession(t, sessStore, "worker", "origin-instance")
	recorder := events.NewFake()

	stage := func(brief string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		outcome := doHandoffWithOutcome(msgStore, sessStore, recorder, newFakeDrainOps(), nil, "worker", "worker",
			[]string{"context cycle", brief}, &stdout, &stderr)
		if outcome.code != 0 || !outcome.restartRequested {
			t.Fatalf("handoff %q outcome = %#v, want accepted restart; stdout=%q stderr=%q", brief, outcome, stdout.String(), stderr.String())
		}
	}

	stage("first brief")
	first := onlyOpenHandoffMessage(t, msgStore)
	stage("second brief")
	second := onlyOpenHandoffMessage(t, msgStore)
	if second.ID == first.ID {
		t.Fatalf("second staging reused %q, want a fresh staged brief", first.ID)
	}
	assertHandoffProvisionalRecord(t, second, "origin-instance")
	assertHandoffSuperseded(t, msgStore, first.ID, second.ID)
	assertSessionStagesHandoff(t, sessStore, sessionBead.ID, second.ID)

	for _, id := range []string{first.ID, second.ID} {
		if _, err := sessStore.Get(id); !errors.Is(err, beads.ErrNotFound) {
			t.Errorf("message bead %s reached the session store (err = %v), want it in the message store only", id, err)
		}
	}
	if _, err := msgStore.Get(sessionBead.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("session bead %s reached the message store (err = %v), want it in the session store only", sessionBead.ID, err)
	}
	if visible, err := beadmail.NewWithStores(msgStore, sessStore).Check("worker"); err != nil || len(visible) != 0 {
		t.Fatalf("Check with the classes split = (%#v, %v), want no visible handoff", visible, err)
	}
}

// TestHandoffStagedArmLandsInTheBindingOnAMigratedCity drives the staged
// self-handoff through the real command root on a city whose messaging and
// sessions classes have moved to a binding, then has the controller release the
// surviving brief to a successor. It drives cmdHandoffWithForce rather than
// doHandoffWithOutcome for the reason TestHandoffMailWritesTheBindingOnAMigratedCity
// does: the defect is at the root, in which stores the command derives, and a
// test that hands routed stores in would pass unrouted. The restartable arm is
// the one the --auto, named on-demand and --target rows cannot reach, and it
// writes session-class state they never do: the staged-brief pointer and the
// restart marker.
func TestHandoffStagedArmLandsInTheBindingOnAMigratedCity(t *testing.T) {
	cityPath, cfg := migratedOneShotCLICity(t)
	captureCLIStorageStderr(t)
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_ALIAS", "worker")
	t.Setenv("GC_SESSION_NAME", "worker")
	oldBuild := buildSessionProviderByName
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return &stagedHandoffStoppedProvider{Fake: runtime.NewFake()}, nil
	}
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })
	startFakeControllerSocket(t, cityPath, "ok\n")

	work, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("opening the city work store: %v", err)
	}
	t.Cleanup(func() { _ = closeBeadStoreHandle(work) })
	sessStore := cliSessionStore(work, cfg, cityPath)
	msgStore := cliMailStore(work, cfg, cityPath).Store
	sessionBead := seedRestartableHandoffSession(t, sessStore, "worker", "origin-instance")

	handoff := func(brief string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := cmdHandoffWithForce([]string{"context cycle", brief}, "", false, "", false, &stdout, &stderr); code != 0 {
			t.Fatalf("gc handoff %q exited %d; stdout=%q stderr=%q", brief, code, stdout.String(), stderr.String())
		}
	}
	handoff("first brief")
	first := onlyOpenHandoffMessage(t, msgStore)
	handoff("second brief")
	second := onlyOpenHandoffMessage(t, msgStore)
	assertHandoffProvisionalRecord(t, second, "origin-instance")
	assertHandoffSuperseded(t, msgStore, first.ID, second.ID)
	assertSessionStagesHandoff(t, sessStore, sessionBead.ID, second.ID)
	staged, err := sessStore.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	for _, key := range []string{"restart_requested", "continuation_reset_pending"} {
		if staged.Metadata[key] != "true" {
			t.Errorf("session %s = %q, want true: the restart marker belongs to the sessions class", key, staged.Metadata[key])
		}
	}
	if visible, err := beadmail.NewWithStores(msgStore, sessStore).Check("worker"); err != nil || len(visible) != 0 {
		t.Fatalf("Check before a successor = (%#v, %v), want no visible handoff", visible, err)
	}

	// A successor incarnation adopts the session; the controller releases the
	// surviving brief and only that one.
	if err := sessStore.SetMetadataBatch(sessionBead.ID, map[string]string{"instance_token": "successor-instance"}); err != nil {
		t.Fatalf("adopt successor: %v", err)
	}
	adopted, err := sessStore.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("get adopted session: %v", err)
	}
	rows := sessionpkg.ReconcileRowsFromBeads([]beads.Bead{adopted})
	if released, failed := releaseStagedSelfHandoffs(sessStore, rows, cfg, clock.Real{}, events.NewFake()); released != 1 || failed != 0 {
		t.Fatalf("releaseStagedSelfHandoffs = (released=%d, failed=%d), want (1, 0)", released, failed)
	}
	visible, err := beadmail.NewWithStores(msgStore, sessStore).Check("worker")
	if err != nil || len(visible) != 1 || visible[0].ID != second.ID {
		t.Fatalf("Check after release = (%#v, %v), want exactly the second brief %q", visible, err, second.ID)
	}

	// Durable bytes, not the funnel's open handle. Every bead is in the binding
	// AND absent from the work store the migration retained: "in the binding"
	// alone passes on a co-resident write.
	if err := closeCLIStorageRoutes(); err != nil {
		t.Fatalf("closing the one-shot routes: %v", err)
	}
	binding := openMigratedDestination(t, mustResolveInfraTarget(t, cityPath, cfg))
	retained, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("opening the retained work store: %v", err)
	}
	t.Cleanup(func() { _ = closeBeadStoreHandle(retained) })
	for _, id := range []string{first.ID, second.ID, sessionBead.ID} {
		if _, err := binding.Get(id); err != nil {
			t.Errorf("%s did not land in the binding: %v", id, err)
		}
		if _, err := retained.Get(id); err == nil {
			t.Errorf("%s also landed in the work store; a relocated class must be served from its binding only", id)
		} else if !errors.Is(err, beads.ErrNotFound) {
			t.Errorf("reading the retained work store for %s: %v", id, err)
		}
	}
	assertHandoffSuperseded(t, binding, first.ID, second.ID)
	assertHandoffReleased(t, binding, sessionBead.ID, second.ID)
}

// runSelfHandoff drives one gc handoff for the "worker" session through the
// real staging path and returns its outcome with the captured stdout and stderr.
func runSelfHandoff(store beads.Store, rec events.Recorder, persistRestart func() error, brief string) (handoffOutcome, string, string) {
	var stdout, stderr bytes.Buffer
	outcome := doHandoffWithOutcome(store, store, rec, newFakeDrainOps(), persistRestart, "worker", "worker",
		[]string{"context cycle", brief}, &stdout, &stderr)
	return outcome, stdout.String(), stderr.String()
}

// mustStageSelfHandoff runs a self-handoff that must be accepted and returns
// what it printed.
func mustStageSelfHandoff(t *testing.T, store beads.Store, rec events.Recorder, persistRestart func() error, brief string) string {
	t.Helper()
	outcome, stdout, stderr := runSelfHandoff(store, rec, persistRestart, brief)
	if outcome.code != 0 || !outcome.restartRequested {
		t.Fatalf("handoff %q outcome = %#v, want accepted restart; stdout=%q stderr=%q", brief, outcome, stdout, stderr)
	}
	return stdout
}

// assertHandoffSuperseded fails unless the earlier staged brief is terminal,
// names the brief that replaced it, and is still hidden from every mail read
// surface.
func assertHandoffSuperseded(t *testing.T, store beads.Store, oldID, newID string) {
	t.Helper()
	old, err := store.Get(oldID)
	if err != nil {
		t.Fatalf("get superseded handoff %s: %v", oldID, err)
	}
	if old.Status != "closed" {
		t.Fatalf("superseded handoff %s status = %q, want closed: an open, unreferenced staged brief is an orphan", oldID, old.Status)
	}
	if got := old.Metadata[redHandoffSupersededByKey]; got != newID {
		t.Fatalf("superseded handoff %s %s = %q, want %q", oldID, redHandoffSupersededByKey, got, newID)
	}
	if got := old.Metadata[redHandoffStagedMetadataKey]; got != "true" {
		t.Fatalf("superseded handoff %s mail.staged = %q, want it to stay true so the brief is never readable", oldID, got)
	}
}

// assertSessionStagesHandoff fails unless the session's durable staged-handoff
// pointer names messageID.
func assertSessionStagesHandoff(t *testing.T, store beads.Store, sessionID, messageID string) {
	t.Helper()
	session, err := store.Get(sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got := session.Metadata[redHandoffStagedMessageIDKey]; got != messageID {
		t.Fatalf("handoff_staged_message_id = %q, want %q", got, messageID)
	}
}

// openStagedHandoffs returns the open message beads still marked staged.
func openStagedHandoffs(t *testing.T, store beads.Store) []beads.Bead {
	t.Helper()
	var staged []beads.Bead
	for _, message := range listOpenMessagesBothTiers(t, store) {
		if message.Metadata[redHandoffStagedMetadataKey] == "true" {
			staged = append(staged, message)
		}
	}
	return staged
}

// failStagedHandoffByTimeout ages the session's staged handoff past the release
// timeout and reconciles until the controller has written it off: failed once
// and stamped with the release-attempt marker.
func failStagedHandoffByTimeout(t *testing.T, env *reconcilerTestEnv, sessionBead *beads.Bead) {
	t.Helper()
	env.setSessionMetadata(sessionBead, map[string]string{
		redHandoffStageCommittedAtKey: env.clk.Now().Add(-controllerRestartTimeout(env.cfg) - time.Second).Format(time.RFC3339),
	})
	for range 2 {
		refreshed, err := env.store.Get(sessionBead.ID)
		if err != nil {
			t.Fatalf("get session during timeout reconcile: %v", err)
		}
		env.reconcile([]beads.Bead{refreshed})
	}
	written, err := env.store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("get session after timeout: %v", err)
	}
	if written.Metadata[redHandoffReleaseAttemptedAtKey] == "" {
		t.Fatal("timed-out staged handoff has no release-attempt marker")
	}
}

// adoptHandoffSuccessor models a successor incarnation taking over the session
// before the controller has reconciled it.
func adoptHandoffSuccessor(t *testing.T, env *reconcilerTestEnv, sessionBead *beads.Bead, token string) {
	t.Helper()
	env.setSessionMetadata(sessionBead, map[string]string{"instance_token": token})
	if err := env.sp.SetMeta("worker", "GC_INSTANCE_TOKEN", token); err != nil {
		t.Fatalf("SetMeta(GC_INSTANCE_TOKEN): %v", err)
	}
}
