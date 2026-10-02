package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// retainedDeadPaneProvider models a tmux session that remain-on-exit kept
// after the agent exited (gastownhall/gascity#6298). The name stays listed.
// presenceMeansRunning reports that listing as IsRunning — the issue's
// "presence is still true" reading. Leaving it false matches current tmux
// IsRunning, which excludes pane_dead=1. IsDeadRuntimeSession is the
// confirmed-dead answer. startErr makes every Start fail without replacing
// the corpse. livenessErr is an incomplete observation.
type retainedDeadPaneProvider struct {
	*runtime.Fake
	corpses              map[string]bool
	presenceMeansRunning bool
	startErr             error
	livenessErr          error
	deadErr              error
	startCalls           int
	stopCalls            map[string]int
}

func newRetainedDeadPaneProvider(name string) *retainedDeadPaneProvider {
	p := &retainedDeadPaneProvider{
		Fake:      runtime.NewFake(),
		corpses:   map[string]bool{name: true},
		stopCalls: map[string]int{},
	}
	if err := p.Fake.Start(context.Background(), name, runtime.Config{Command: "true"}); err != nil {
		panic(err)
	}
	return p
}

func (p *retainedDeadPaneProvider) IsRunning(name string) bool {
	if p.corpses[name] {
		return p.presenceMeansRunning
	}
	return p.Fake.IsRunning(name)
}

func (p *retainedDeadPaneProvider) IsDeadRuntimeSession(name string) (bool, error) {
	if p.deadErr != nil {
		return false, p.deadErr
	}
	return p.corpses[name], nil
}

func (p *retainedDeadPaneProvider) ObserveLivenessWithError(name string, processNames []string) (runtime.Liveness, error) {
	if p.livenessErr != nil {
		return runtime.Liveness{}, p.livenessErr
	}
	// The anonymous Provider does not implement LivenessObserverWithError, so
	// ObserveLiveness falls back to IsRunning instead of recursing here.
	return runtime.ObserveLiveness(struct{ runtime.Provider }{p}, name, processNames), nil
}

func (p *retainedDeadPaneProvider) ProcessAlive(name string, processNames []string) bool {
	if p.corpses[name] {
		return false
	}
	return p.Fake.ProcessAlive(name, processNames)
}

func (p *retainedDeadPaneProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	p.startCalls++
	if p.startErr != nil {
		return p.startErr
	}
	delete(p.corpses, name)
	if err := p.Fake.Stop(name); err != nil && !runtime.IsSessionGone(err) {
		return err
	}
	return p.Fake.Start(ctx, name, cfg)
}

func (p *retainedDeadPaneProvider) Stop(name string) error {
	p.stopCalls[name]++
	delete(p.corpses, name)
	return p.Fake.Stop(name)
}

func alwaysNamedDeadPaneEnv(t *testing.T, suspended bool) (*reconcilerTestEnv, string) {
	t.Helper()
	env := newReconcilerTestEnv()
	env.rec = events.NewFake()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city", SuspendedOnStart: suspended},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true"}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:                 "true",
		SessionName:             sessionName,
		TemplateName:            "worker",
		ConfiguredNamedIdentity: "worker",
		ConfiguredNamedMode:     "always",
	}
	return env, sessionName
}

func (e *reconcilerTestEnv) reconcileWithProvider(sessions []beads.Bead, sp runtime.Provider) {
	poolDesired := map[string]int{}
	for _, tp := range e.desiredState {
		if tp.TemplateName != "" {
			poolDesired[tp.TemplateName]++
		}
	}
	cfgNames := configuredSessionNames(e.cfg, "", e.store)
	reconcileSessionBeads(
		context.Background(), sessions, e.desiredState, cfgNames, e.cfg, sp,
		e.store, nil, nil, nil, e.dt, poolDesired, false, nil, "",
		nil, e.clk, e.rec, 0, 0, &e.stdout, &e.stderr,
		e.startOptions...,
	)
}

func openAlwaysNamedBead(env *reconcilerTestEnv, sessionName, state string) beads.Bead {
	session := env.createSessionBead(sessionName, "worker")
	wokeAt := env.clk.Now().Add(-6 * time.Hour).UTC().Format(time.RFC3339)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"state":                      state,
		"last_woke_at":               wokeAt,
		"session_key":                "old-key",
	})
	return session
}

func eventTypes(rec *events.Fake) []string {
	out := make([]string, 0, len(rec.Events))
	for _, ev := range rec.Events {
		out = append(out, ev.Type)
	}
	return out
}

func crashedBeforeWoke(rec *events.Fake) bool {
	crashed := -1
	woke := -1
	for i, ev := range rec.Events {
		if ev.Type == events.SessionCrashed && crashed < 0 {
			crashed = i
		}
		if ev.Type == events.SessionWoke && woke < 0 {
			woke = i
		}
	}
	return crashed >= 0 && woke >= 0 && crashed < woke
}

func crashReasonIsDeadPane(rec *events.Fake) bool {
	for _, ev := range rec.Events {
		if ev.Type == events.SessionCrashed && strings.Contains(string(ev.Payload), "dead pane") {
			return true
		}
	}
	return false
}

// TestCleanupDeadRuntimeSessionCorpsesReapsNamedAlwaysModeDeadPaneWithoutClosing
// pins gastownhall/gascity#6298 at the corpse sweep. A confirmed-dead
// remain-on-exit pane under an unsuspended always-mode named session is not
// a live runtime. The sweep must stop it so the name can be fresh-started,
// and must not close the bead: the named row is the durable identity.
func TestCleanupDeadRuntimeSessionCorpsesReapsNamedAlwaysModeDeadPaneWithoutClosing(t *testing.T) {
	const name = "city-worker"
	store, bead, snapshot := newDeadRuntimeCorpseRow(t, map[string]string{
		"session_name":                       name,
		"template":                           "worker",
		"alias":                              "rig/worker",
		"state":                              string(session.StateActive),
		session.NamedSessionMetadataKey:      "true",
		session.NamedSessionIdentityMetadata: "worker",
		session.NamedSessionModeMetadata:     "always",
	})
	sp := newDeadRuntimeArtifactProvider()
	sp.visible[name] = true
	sp.dead[name] = true
	sp.visible["city-worker-live"] = true
	sp.live["city-worker-live"] = true

	var stderr strings.Builder
	rec := events.NewFake()
	got := cleanupDeadRuntimeSessionCorpsesWithRecorder(store, nil, nil, snapshot, nil, sp, nil, rec, &stderr)
	if got != 1 || sp.stopCalls[name] != 1 {
		t.Fatalf("cleanup = %d (Stop %q calls = %d), want the named corpse reaped once; stderr=%q", got, name, sp.stopCalls[name], stderr.String())
	}
	if sp.stopCalls["city-worker-live"] != 0 {
		t.Fatalf("Stop(live) calls = %d, want 0", sp.stopCalls["city-worker-live"])
	}
	after, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("re-fetch bead: %v", err)
	}
	if after.Status != "open" {
		t.Fatalf("named bead status = %q, want open (closing releases the durable identity)", after.Status)
	}
	if after.Metadata["state"] == "dead-runtime" || after.Metadata["close_reason"] != "" {
		t.Fatalf("named bead was closed as a pool corpse: state=%q close_reason=%q", after.Metadata["state"], after.Metadata["close_reason"])
	}
	if !crashReasonIsDeadPane(rec) {
		t.Fatalf("cleanup events = %v, want session.crashed reason dead pane (the pane is gone before reconcile)", eventTypes(rec))
	}
}

func TestCleanupThenReconcileRecordsDeadPaneBeforeWake(t *testing.T) {
	env, name := alwaysNamedDeadPaneEnv(t, false)
	p := newRetainedDeadPaneProvider(name)
	p.presenceMeansRunning = true
	row := openAlwaysNamedBead(env, name, string(session.StateActive))
	snap := newSessionBeadSnapshot([]beads.Bead{row})

	got := cleanupDeadRuntimeSessionCorpsesWithRecorder(env.store, nil, nil, snap, nil, p, env.clk, env.rec, &env.stderr)
	if got != 1 || p.corpses[name] {
		t.Fatalf("cleanup = %d corpse=%v, want the named pane reaped before reconcile; stderr=%q", got, p.corpses[name], env.stderr.String())
	}

	env.reconcileWithProvider([]beads.Bead{row}, p)

	rec := env.rec.(*events.Fake)
	crashes := 0
	for _, ev := range rec.Events {
		if ev.Type == events.SessionCrashed {
			crashes++
		}
	}
	if crashes != 1 || !crashedBeforeWoke(rec) || !crashReasonIsDeadPane(rec) {
		t.Fatalf("events = %v, want one session.crashed (dead pane) before session.woke", eventTypes(rec))
	}
	if p.startCalls < 1 {
		final, _ := env.store.Get(row.ID)
		t.Fatalf("startCalls = %d, want a fresh start after the corpse sweep; state=%q stderr=%q", p.startCalls, final.Metadata["state"], env.stderr.String())
	}
}

// TestReconcileAlwaysNamedSessionOverDeadRetainedPane is the #6298
// reconciler contract. A retained dead pane is not healthy just because the
// tmux session exists. An unsuspended always-mode named session must be
// fresh-started, and that recovery must record session.crashed before
// session.woke. A provider that fails every start must not leave the bead
// active. Suspension and an incomplete liveness observation must not restart.
func TestReconcileAlwaysNamedSessionOverDeadRetainedPane(t *testing.T) {
	t.Run("current tmux isrunning false restarts", func(t *testing.T) {
		env, name := alwaysNamedDeadPaneEnv(t, false)
		p := newRetainedDeadPaneProvider(name)
		row := openAlwaysNamedBead(env, name, string(session.StateActive))

		env.reconcileWithProvider([]beads.Bead{row}, p)

		if p.startCalls < 1 || p.corpses[name] {
			final, _ := env.store.Get(row.ID)
			t.Fatalf("always-mode named session over a dead pane must fresh-start; startCalls=%d corpse=%v state=%q stderr=%q events=%v",
				p.startCalls, p.corpses[name], final.Metadata["state"], env.stderr.String(), eventTypes(env.rec.(*events.Fake)))
		}
		rec := env.rec.(*events.Fake)
		if !crashedBeforeWoke(rec) || !crashReasonIsDeadPane(rec) {
			t.Fatalf("recovery events = %v, want session.crashed (reason dead pane) before session.woke", eventTypes(rec))
		}
		final, _ := env.store.Get(row.ID)
		if final.Status != "open" {
			t.Fatalf("status = %q, want open", final.Status)
		}
	})

	t.Run("presence is not health", func(t *testing.T) {
		env, name := alwaysNamedDeadPaneEnv(t, false)
		p := newRetainedDeadPaneProvider(name)
		p.presenceMeansRunning = true
		row := openAlwaysNamedBead(env, name, string(session.StateActive))

		env.reconcileWithProvider([]beads.Bead{row}, p)

		final, _ := env.store.Get(row.ID)
		if p.corpses[name] && (final.Metadata["state"] == string(session.StateActive) || final.Metadata["state"] == string(session.StateAwake)) {
			t.Fatalf("session left %q over a confirmed-dead pane (presenceMeansRunning); startCalls=%d stderr=%q",
				final.Metadata["state"], p.startCalls, env.stderr.String())
		}
		if p.startCalls < 1 || p.corpses[name] {
			t.Fatalf("confirmed-dead always-mode session must be fresh-started; startCalls=%d corpse=%v state=%q stderr=%q",
				p.startCalls, p.corpses[name], final.Metadata["state"], env.stderr.String())
		}
		rec := env.rec.(*events.Fake)
		if !crashedBeforeWoke(rec) || !crashReasonIsDeadPane(rec) {
			t.Fatalf("recovery events = %v, want session.crashed (reason dead pane) before session.woke", eventTypes(rec))
		}
	})

	t.Run("provider fails every start", func(t *testing.T) {
		env, name := alwaysNamedDeadPaneEnv(t, false)
		p := newRetainedDeadPaneProvider(name)
		p.presenceMeansRunning = true
		p.startErr = errors.New("start refused")
		row := openAlwaysNamedBead(env, name, string(session.StateActive))

		env.reconcileWithProvider([]beads.Bead{row}, p)

		final, err := env.store.Get(row.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if final.Status != "open" {
			t.Fatalf("status = %q, want open", final.Status)
		}
		state := final.Metadata["state"]
		if state == string(session.StateActive) || state == string(session.StateAwake) {
			t.Fatalf("state = %q after a failed start over a dead pane, want not active; startCalls=%d stderr=%q",
				state, p.startCalls, env.stderr.String())
		}
		if p.startCalls < 1 {
			t.Fatalf("startCalls = %d, want the failed start to be attempted; state=%q stderr=%q", p.startCalls, state, env.stderr.String())
		}
		rec := env.rec.(*events.Fake)
		if !crashReasonIsDeadPane(rec) {
			t.Fatalf("events = %v, want a session.crashed dead-pane record even when start fails", eventTypes(rec))
		}
		for _, ev := range rec.Events {
			if ev.Type == events.SessionWoke {
				t.Fatalf("session.woke recorded for a start that failed: events=%v", eventTypes(rec))
			}
		}
	})

	t.Run("live pane untouched", func(t *testing.T) {
		env, name := alwaysNamedDeadPaneEnv(t, false)
		p := newRetainedDeadPaneProvider(name)
		p.corpses = map[string]bool{}
		p.presenceMeansRunning = true
		row := openAlwaysNamedBead(env, name, string(session.StateActive))

		env.reconcileWithProvider([]beads.Bead{row}, p)

		if p.startCalls != 0 || p.stopCalls[name] != 0 {
			t.Fatalf("live pane startCalls=%d stopCalls=%d, want no restart", p.startCalls, p.stopCalls[name])
		}
		final, _ := env.store.Get(row.ID)
		if final.Metadata["state"] != string(session.StateActive) && final.Metadata["state"] != string(session.StateAwake) {
			t.Fatalf("live session state = %q, want active", final.Metadata["state"])
		}
	})

	t.Run("city suspension suppresses respawn", func(t *testing.T) {
		env, name := alwaysNamedDeadPaneEnv(t, true)
		p := newRetainedDeadPaneProvider(name)
		row := openAlwaysNamedBead(env, name, string(session.StateActive))

		env.reconcileWithProvider([]beads.Bead{row}, p)

		if p.startCalls != 0 {
			final, _ := env.store.Get(row.ID)
			t.Fatalf("suspended city fresh-started a dead always-mode session; startCalls=%d state=%q", p.startCalls, final.Metadata["state"])
		}
		final, _ := env.store.Get(row.ID)
		if final.Status != "open" {
			t.Fatalf("status = %q, want the named bead kept open while suspended", final.Status)
		}
	})

	t.Run("session suspension suppresses respawn", func(t *testing.T) {
		env, name := alwaysNamedDeadPaneEnv(t, false)
		p := newRetainedDeadPaneProvider(name)
		// Authored session suspension is `gc session suspend`: state, a
		// user-hold sleep intent, and a future held_until. The hold gate
		// suppresses the always-mode wake.
		row := openAlwaysNamedBead(env, name, string(session.StateSuspended))
		env.setSessionMetadata(&row, map[string]string{
			"sleep_intent": "user-hold",
			"held_until":   env.clk.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})

		env.reconcileWithProvider([]beads.Bead{row}, p)

		if p.startCalls != 0 {
			final, _ := env.store.Get(row.ID)
			t.Fatalf("suspended session was respawned; startCalls=%d state=%q", p.startCalls, final.Metadata["state"])
		}
		final, _ := env.store.Get(row.ID)
		if final.Status != "open" {
			t.Fatalf("status = %q, want open", final.Status)
		}
	})

	t.Run("incomplete liveness does not restart", func(t *testing.T) {
		env, name := alwaysNamedDeadPaneEnv(t, false)
		p := newRetainedDeadPaneProvider(name)
		p.livenessErr = runtime.ErrRuntimeUnavailable
		row := openAlwaysNamedBead(env, name, string(session.StateActive))

		env.reconcileWithProvider([]beads.Bead{row}, p)

		if p.startCalls != 0 {
			t.Fatalf("startCalls = %d, want 0 when liveness observation failed", p.startCalls)
		}
		final, _ := env.store.Get(row.ID)
		if final.Metadata["state"] != string(session.StateActive) {
			t.Fatalf("state = %q, want active left untouched when liveness is unknown", final.Metadata["state"])
		}
		if final.Status != "open" {
			t.Fatalf("status = %q, want open", final.Status)
		}
	})
}
