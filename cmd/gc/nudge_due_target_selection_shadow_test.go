package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/convergence"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/nudgeshadow"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/supervisor"
)

func TestNudgeDueTargetSelectionShadowOffIsInert(t *testing.T) {
	for _, tc := range []struct {
		name   string
		shadow *string
	}{
		{name: "omitted"},
		{name: "explicit off", shadow: nudgeShadowTestPtr("off")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.City{Daemon: config.DaemonConfig{
				NudgeShadow:       tc.shadow,
				NudgeDispatcher:   "legacy",
				SessionReconciler: "require",
			}}
			selection, err := nudgeshadow.Preflight(cfg, nudgeshadow.Requirements{
				CityPath:       "",
				TraceRecording: false,
			})
			if err != nil {
				t.Fatalf("Preflight: %v", err)
			}
			if selection.Required() {
				t.Fatalf("selection = %+v, want inert off", selection)
			}
			cr := &CityRuntime{nudgeShadowSelection: selection}
			if observer := cr.nudgeDueTargetSelectionObserver(); observer != nil {
				t.Fatal("off selection installed a due-target shadow observer")
			}
		})
	}
}

func TestNudgeDueTargetSelectionShadowRequiredPreflight(t *testing.T) {
	required := nudgeShadowTestPtr("required")
	valid := &config.City{Daemon: config.DaemonConfig{
		NudgeShadow:       required,
		NudgeDispatcher:   "supervisor",
		SessionReconciler: "off",
	}}
	selection, err := nudgeshadow.Preflight(valid, nudgeshadow.Requirements{
		CityPath:       "/city",
		TraceRecording: true,
	})
	if err != nil {
		t.Fatalf("valid Preflight: %v", err)
	}
	if !selection.Required() {
		t.Fatalf("selection = %+v, want required", selection)
	}

	tests := []struct {
		name         string
		cfg          *config.City
		requirements nudgeshadow.Requirements
		wantErr      string
	}{
		{
			name: "legacy dispatcher",
			cfg: &config.City{Daemon: config.DaemonConfig{
				NudgeShadow: required,
			}},
			requirements: nudgeshadow.Requirements{CityPath: "/city", TraceRecording: true},
			wantErr:      "supervisor",
		},
		{
			name: "session reconciler enabled",
			cfg: &config.City{Daemon: config.DaemonConfig{
				NudgeShadow:       required,
				NudgeDispatcher:   "supervisor",
				SessionReconciler: "auto",
			}},
			requirements: nudgeshadow.Requirements{CityPath: "/city", TraceRecording: true},
			wantErr:      "session reconciler",
		},
		{
			name: "blank city path",
			cfg: &config.City{Daemon: config.DaemonConfig{
				NudgeShadow:       required,
				NudgeDispatcher:   "supervisor",
				SessionReconciler: "off",
			}},
			requirements: nudgeshadow.Requirements{CityPath: " \t", TraceRecording: true},
			wantErr:      "city path",
		},
		{
			name: "trace unavailable",
			cfg: &config.City{Daemon: config.DaemonConfig{
				NudgeShadow:       required,
				NudgeDispatcher:   "supervisor",
				SessionReconciler: "off",
			}},
			requirements: nudgeshadow.Requirements{CityPath: "/city"},
			wantErr:      "trace",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := nudgeshadow.Preflight(tc.cfg, tc.requirements)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Preflight error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestNudgeDueTargetSelectionShadowStandaloneRefusesAtBootBoundary(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	stubManagedDoltStoreOpeners(t)
	t.Setenv("GC_SESSION_RECONCILER_TRACE", "0")
	t.Setenv("GC_BEADS", "file")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := requiredNudgeDueTargetSelectionShadowConfig()
	provider := runtime.NewFake()
	recorder := events.NewFake()
	var stdout, stderr bytes.Buffer
	// This refusal exits before runController's listener-owning section. The
	// resource census explicitly treats a function-value alias as a
	// manual-review boundary; the artifact assertions below prove that fact.
	runPreListenerBoundary := runController
	exitCode := runPreListenerBoundary(
		cityPath,
		filepath.Join(cityPath, "city.toml"),
		cfg,
		"config-revision",
		nil,
		nil,
		provider,
		newDrainOps(provider),
		nil,
		nil,
		nil,
		recorder,
		nil,
		&stdout,
		&stderr,
	)
	if exitCode != 1 {
		t.Fatalf("runController exit = %d, want 1; stdout=%q stderr=%q", exitCode, stdout.String(), stderr.String())
	}
	if len(recorder.Events) != 0 {
		t.Fatalf("controller lifecycle events = %#v, want none before preflight refusal", recorder.Events)
	}
	if strings.Contains(stdout.String(), "Controller started") || strings.Contains(stdout.String(), "Controller stopped") {
		t.Fatalf("controller announced lifecycle around preflight refusal: %q", stdout.String())
	}
	if calls := provider.SnapshotCalls(); len(calls) != 0 {
		t.Fatalf("provider calls after required shadow preflight refusal = %#v, want none", calls)
	}
	for _, path := range []string{
		controllerSocketPath(cityPath),
		filepath.Join(cityPath, ".gc", convergence.TokenFile),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("startup artifact %q exists after preflight refusal (stat error %v)", path, err)
		}
	}
}

func TestNudgeDueTargetSelectionShadowStandaloneLockRefusalDoesNotOpenTrace(t *testing.T) {
	clearGCEnv(t)
	t.Setenv("GC_SESSION_RECONCILER_TRACE", "1")
	t.Setenv("GC_BEADS", "file")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatalf("acquire controller lock: %v", err)
	}
	t.Cleanup(func() { _ = lock.Close() })

	cfg := requiredNudgeDueTargetSelectionShadowConfig()
	provider := runtime.NewFake()
	var stdout, stderr bytes.Buffer
	// Lock exclusion exits before runController's listener-owning section.
	// Keep this sanctioned manual-review boundary paired with the artifact
	// assertions below.
	runAfterLockExclusion := runController
	exitCode := runAfterLockExclusion(
		cityPath,
		filepath.Join(cityPath, "city.toml"),
		cfg,
		"config-revision",
		nil,
		nil,
		provider,
		newDrainOps(provider),
		nil,
		nil,
		nil,
		events.NewFake(),
		nil,
		&stdout,
		&stderr,
	)
	if exitCode != 1 {
		t.Fatalf("runController exit = %d, want lock refusal 1; stdout=%q stderr=%q",
			exitCode, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(traceCityRuntimeDir(cityPath)); !os.IsNotExist(err) {
		t.Fatalf("trace runtime opened before controller lock exclusion (stat error %v)", err)
	}
	for _, path := range []string{
		controllerSocketPath(cityPath),
		filepath.Join(cityPath, ".gc", convergence.TokenFile),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("startup artifact %q exists after lock refusal (stat error %v)", path, err)
		}
	}
}

func TestNudgeDueTargetSelectionShadowSupervisorRefusesAtBootBoundary(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_SESSION_RECONCILER_TRACE", "0")
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_HOME", t.TempDir())

	cityPath := t.TempDir()
	cfg := requiredNudgeDueTargetSelectionShadowConfig()
	cfg.Workspace.Name = "shadow-supervisor"
	cfg.Session.Provider = "fake"
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatalf("Marshal config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), data, 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "shadow-supervisor"); err != nil {
		t.Fatalf("register city: %v", err)
	}
	supervisorEvents := events.NewFake()
	registry := newCityRegistry()
	registry.SetSupervisorRecorder(supervisorEvents)
	if err := registry.StorePendingRequestID(cityPath, "request-shadow-preflight"); err != nil {
		t.Fatalf("store pending request: %v", err)
	}

	var stdout, stderr bytes.Buffer
	reconcileCities(reg, registry, supervisor.PublicationConfig{}, &stdout, &stderr)
	if done := registry.CancelCity(cityPath); done != nil {
		<-done
	}

	if len(supervisorEvents.Events) != 1 {
		t.Fatalf("supervisor events = %#v, want one typed init failure; stdout=%q stderr=%q",
			supervisorEvents.Events, stdout.String(), stderr.String())
	}
	var payload api.RequestFailedPayload
	if err := json.Unmarshal(supervisorEvents.Events[0].Payload, &payload); err != nil {
		t.Fatalf("decode supervisor failure: %v", err)
	}
	if supervisorEvents.Events[0].Type != events.RequestFailed ||
		payload.ErrorCode != "nudge_shadow_preflight_failed" ||
		payload.Operation != api.RequestOperationCityCreate {
		t.Fatalf("supervisor failure = event:%q payload:%+v", supervisorEvents.Events[0].Type, payload)
	}
	if strings.Contains(stdout.String(), "Launching city") {
		t.Fatalf("supervisor announced a city after preflight refusal: %q", stdout.String())
	}
	for _, path := range []string{
		controllerSocketPath(cityPath),
		filepath.Join(cityPath, ".gc", convergence.TokenFile),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("startup artifact %q exists after preflight refusal (stat error %v)", path, err)
		}
	}
	registry.ReadCallback(func(
		cities map[string]*managedCity,
		_ map[string]cityInitProgress,
		initFailures map[string]*initFailRecord,
		_ map[string]*panicRecord,
	) {
		if len(cities) != 0 {
			t.Fatalf("managed cities after preflight refusal = %#v, want none", cities)
		}
		failure := initFailures[cityPath]
		if failure == nil || !strings.Contains(failure.lastError, "trace recording") {
			t.Fatalf("init failure = %+v, want trace-recording preflight refusal", failure)
		}
	})
}

func TestNudgeDueTargetSelectionShadowReloadRejectsBootLatchViolations(t *testing.T) {
	tests := []struct {
		name              string
		bootMode          string
		reloadMode        string
		nudgeDispatcher   string
		sessionReconciler string
		wantObserver      bool
		wantError         string
	}{
		{
			name:              "legacy dispatcher",
			bootMode:          "required",
			reloadMode:        "required",
			nudgeDispatcher:   "legacy",
			sessionReconciler: "off",
			wantObserver:      true,
			wantError:         "nudge dispatcher",
		},
		{
			name:              "session reconciler enabled",
			bootMode:          "required",
			reloadMode:        "required",
			nudgeDispatcher:   "supervisor",
			sessionReconciler: "auto",
			wantObserver:      true,
			wantError:         "session reconciler",
		},
		{
			name:              "required to off",
			bootMode:          "required",
			reloadMode:        "off",
			nudgeDispatcher:   "supervisor",
			sessionReconciler: "off",
			wantObserver:      true,
			wantError:         "controller restart required",
		},
		{
			name:              "off to required",
			bootMode:          "off",
			reloadMode:        "required",
			nudgeDispatcher:   "supervisor",
			sessionReconciler: "off",
			wantObserver:      false,
			wantError:         "controller restart required",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			disableManagedDoltRecoveryForTest(t)
			t.Setenv("GC_SESSION_RECONCILER_TRACE", "1")
			t.Setenv("GC_BEADS", "file")

			cityPath := t.TempDir()
			tomlPath := filepath.Join(cityPath, "city.toml")
			initial := requiredNudgeDueTargetSelectionShadowConfig()
			initial.Session.Provider = "fake"
			initial.Daemon.NudgeShadow = nudgeShadowTestPtr(tc.bootMode)
			writeNudgeDueTargetSelectionShadowConfig(t, tomlPath, initial)
			cfg, revision := loadCityRuntimeControllerConfig(t, cityPath)

			selection, tracer, err := prepareNudgeShadowRuntime(cityPath, "shadow-city", cfg, io.Discard)
			if err != nil {
				t.Fatalf("prepare nudge shadow runtime: %v", err)
			}
			provider := runtime.NewFake()
			var stdout, stderr bytes.Buffer
			cr := newTestCityRuntime(t, CityRuntimeParams{
				CityPath:             cityPath,
				CityName:             "shadow-city",
				TomlPath:             tomlPath,
				ConfigRev:            revision,
				Cfg:                  cfg,
				SP:                   provider,
				Dops:                 newDrainOps(provider),
				Rec:                  events.Discard,
				NudgeShadowSelection: selection,
				Trace:                tracer,
				Stdout:               &stdout,
				Stderr:               &stderr,
			})
			if got := cr.nudgeDueTargetSelectionObserver() != nil; got != tc.wantObserver {
				t.Fatalf("boot observer installed = %t, want %t", got, tc.wantObserver)
			}
			oldCfg := cr.cfg
			providerCallsBefore := len(provider.SnapshotCalls())

			next := requiredNudgeDueTargetSelectionShadowConfig()
			next.Session.Provider = "fake"
			next.Daemon.NudgeShadow = nudgeShadowTestPtr(tc.reloadMode)
			next.Daemon.NudgeDispatcher = tc.nudgeDispatcher
			next.Daemon.SessionReconciler = tc.sessionReconciler
			writeNudgeDueTargetSelectionShadowConfig(t, tomlPath, next)

			lastProviderName := "fake"
			reply := cr.reloadConfigTraced(
				context.Background(),
				&lastProviderName,
				cityPath,
				nil,
				reloadSourceManual,
			)
			if reply.Outcome != reloadOutcomeFailed ||
				!strings.Contains(reply.Error, "nudge shadow") ||
				!strings.Contains(reply.Error, tc.wantError) {
				t.Fatalf("reload reply = %+v, want nudge-shadow failure; stdout=%q stderr=%q",
					reply, stdout.String(), stderr.String())
			}
			if cr.cfg != oldCfg {
				t.Fatal("invalid boot-latched reload replaced the active config")
			}
			if got := cr.nudgeDueTargetSelectionObserver() != nil; got != tc.wantObserver {
				t.Fatalf("observer installed after rejected reload = %t, want boot-latched %t",
					got, tc.wantObserver)
			}
			if calls := len(provider.SnapshotCalls()); calls != providerCallsBefore {
				t.Fatalf("provider calls across rejected reload = %d -> %d, want no reload provider effects",
					providerCallsBefore, calls)
			}
		})
	}
}

func TestNudgeDueTargetSelectionShadowRequiredNeverStartsKeyedController(t *testing.T) {
	selection := nudgeshadow.Selection{Mode: nudgeshadow.Required, Provenance: nudgeshadow.Config}
	cr := &CityRuntime{nudgeShadowSelection: selection}

	var keyedConstructed atomic.Bool
	originalFactory := newCityNudgeKeyController
	newCityNudgeKeyController = func(nudgeKeyControllerOptions) (*nudgeKeyController, error) {
		keyedConstructed.Store(true)
		return nil, nil
	}
	t.Cleanup(func() { newCityNudgeKeyController = originalFactory })

	if err := cr.ensureNudgeKeyControllerForSelection(t.Context()); err != nil {
		t.Fatalf("ensureNudgeKeyControllerForSelection: %v", err)
	}
	if keyedConstructed.Load() || cr.nudgeKeyControllerActive() {
		t.Fatal("required selection constructed or activated the keyed nudge controller")
	}
}

func TestNudgeDueTargetSelectionShadowComparisonOutcomes(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name             string
		candidateID      string
		legacyID         string
		wantOutcome      string
		wantLegacyCount  int
		wantCandidateCnt int
	}{
		{
			name:             "matched",
			candidateID:      "session-matched",
			legacyID:         "session-matched",
			wantOutcome:      nudgeDueTargetSelectionMatched,
			wantLegacyCount:  1,
			wantCandidateCnt: 1,
		},
		{
			name:             "mismatch",
			candidateID:      "session-candidate",
			legacyID:         "session-legacy",
			wantOutcome:      nudgeDueTargetSelectionMismatch,
			wantLegacyCount:  1,
			wantCandidateCnt: 1,
		},
		{
			name:             "candidate only",
			candidateID:      "session-candidate-only",
			wantOutcome:      nudgeDueTargetSelectionCandidateOnly,
			wantCandidateCnt: 1,
		},
		{
			name:            "legacy only",
			legacyID:        "session-legacy-only",
			wantOutcome:     nudgeDueTargetSelectionLegacyOnly,
			wantLegacyCount: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var snapshot *sessionBeadSnapshot
			if tc.legacyID != "" {
				snapshot = newSessionBeadSnapshot([]beads.Bead{{
					ID:     tc.legacyID,
					Status: "open",
					Metadata: map[string]string{
						"session_name": tc.legacyID,
						"agent_name":   "worker",
						"template":     "worker",
						"transport":    "acp",
					},
				}})
			} else {
				snapshot = newSessionBeadSnapshot(nil)
			}
			state := nudgequeue.State{Pending: []nudgequeue.Item{{
				ID:           "nudge-" + tc.name,
				Agent:        "worker",
				SessionID:    tc.candidateID,
				DeliverAfter: now.Add(-time.Minute),
			}}}
			var got nudgeDueTargetSelectionObservation
			called := 0
			_, _ = dispatchAllQueuedNudgesFromStateObserved(
				t.TempDir(),
				supervisorCfg(),
				nil,
				nil,
				runtime.NewFake(),
				snapshot,
				nil,
				state,
				7*time.Millisecond,
				func(observation nudgeDueTargetSelectionObservation) {
					called++
					got = observation
				},
				nil,
			)
			if called != 1 {
				t.Fatalf("observer calls = %d, want 1", called)
			}
			if got.Scope != nudgeshadow.ScopeQueuedExactDueTargetSelection ||
				got.ComparisonOutcome != tc.wantOutcome ||
				got.CandidateCount != tc.wantCandidateCnt ||
				got.LegacyCount != tc.wantLegacyCount {
				t.Fatalf("observation = %+v", got)
			}
			if got.QueueDuration != 7*time.Millisecond ||
				got.CandidateDuration < 0 ||
				got.LegacyDuration < 0 ||
				got.TotalDuration < got.QueueDuration {
				t.Fatalf("observation timings = %+v", got)
			}
			if len(got.CandidateDigest) != 64 || len(got.LegacyDigest) != 64 {
				t.Fatalf("digest lengths = %d/%d, want 64/64", len(got.CandidateDigest), len(got.LegacyDigest))
			}
			wire, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("Marshal observation: %v", err)
			}
			if (tc.candidateID != "" && strings.Contains(string(wire), tc.candidateID)) ||
				(tc.legacyID != "" && strings.Contains(string(wire), tc.legacyID)) {
				t.Fatalf("bounded observation leaked a raw session ID: %s", wire)
			}
			if !got.LegacyEffectOwner || got.ShadowEffectApplied {
				t.Fatalf("effect ownership = legacy:%t shadow:%t, want true/false",
					got.LegacyEffectOwner, got.ShadowEffectApplied)
			}
		})
	}
}

func TestNudgeDueTargetSelectionShadowKeepsExactlyOneLegacyProviderEffect(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	clearInheritedCityRoutingEnv(t)
	t.Setenv("GC_BEADS", "file")

	cityPath := t.TempDir()
	store := openNudgeBeadStore(cityPath)
	if store.Store == nil {
		t.Fatal("openNudgeBeadStore returned nil")
	}
	created, err := store.Create(beads.Bead{
		Title:  "Session: worker",
		Type:   session.BeadType,
		Status: "open",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "worker-session",
			"agent_name":   "worker",
			"template":     "worker",
			"transport":    "acp",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	item := newQueuedNudgeWithOptions(
		"worker",
		"one legacy effect",
		"session",
		time.Now().Add(-time.Minute),
		queuedNudgeOptions{SessionID: created.ID},
	)
	if err := enqueueQueuedNudgeWithStore(cityPath, store, item); err != nil {
		t.Fatalf("enqueue queued nudge: %v", err)
	}
	provider := runtime.NewFake()
	if err := provider.Start(context.Background(), "worker-session", runtime.Config{}); err != nil {
		t.Fatalf("start fake session: %v", err)
	}
	provider.SetActivity("worker-session", time.Now().Add(-10*time.Second))

	var observed nudgeDueTargetSelectionObservation
	delivered, err := dispatchAllQueuedNudgesObserved(
		cityPath,
		supervisorCfg(),
		store,
		store,
		provider,
		newSessionBeadSnapshot([]beads.Bead{created}),
		func(observation nudgeDueTargetSelectionObservation) { observed = observation },
		nil,
	)
	if err != nil {
		t.Fatalf("dispatchAllQueuedNudgesObserved: %v", err)
	}
	if delivered != 1 {
		t.Fatalf("delivered = %d, want 1", delivered)
	}
	if calls := provider.CountCalls("Nudge", "worker-session"); calls != 1 {
		t.Fatalf("legacy provider Nudge calls = %d, want exactly 1; all calls=%#v", calls, provider.SnapshotCalls())
	}
	if observed.ComparisonOutcome != nudgeDueTargetSelectionMatched ||
		!observed.LegacyEffectOwner ||
		observed.ShadowEffectApplied {
		t.Fatalf("observation = %+v, want matched legacy-only effect ownership", observed)
	}
}

func TestNudgeDueTargetSelectionShadowObserverPanicCannotPreemptLegacyProviderEffect(t *testing.T) {
	cr, store, provider, info := newNudgeDeliveryRuntime(t, rollout.Off)
	item := newQueuedNudgeWithOptions(
		"worker",
		"legacy delivery precedes observation",
		"session",
		time.Now().Add(-time.Minute),
		queuedNudgeOptions{SessionID: info.ID},
	)
	if err := enqueueQueuedNudgeWithStore(cr.cityPath, store, item); err != nil {
		t.Fatalf("enqueue queued nudge: %v", err)
	}
	sessionBead, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("get session bead: %v", err)
	}

	const observerPanic = "trace observer panic"
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_, _ = dispatchAllQueuedNudgesObserved(
			cr.cityPath,
			cr.cfg,
			store,
			store,
			provider,
			newSessionBeadSnapshot([]beads.Bead{sessionBead}),
			func(nudgeDueTargetSelectionObservation) { panic(observerPanic) },
			nil,
		)
	}()

	if recovered != observerPanic {
		t.Fatalf("observer panic = %#v, want %q", recovered, observerPanic)
	}
	if calls := provider.CountCalls("Nudge", info.SessionName); calls != 1 {
		t.Fatalf("legacy provider Nudge calls after observer panic = %d, want 1; all calls=%#v",
			calls, provider.SnapshotCalls())
	}
	assertTerminalNudgeShadow(t, store, item.ID)
}

func TestNudgeDueTargetSelectionShadowTraceIsBoundedAndUsesTraceProvenance(t *testing.T) {
	cityPath := t.TempDir()
	tracer := newSessionReconcilerTraceManager(cityPath, "shadow-city", io.Discard)
	if !tracer.Enabled() {
		t.Fatal("trace recorder is unavailable")
	}
	cr := &CityRuntime{
		cityPath: cityPath,
		cityName: "shadow-city",
		cfg:      supervisorCfg(),
		trace:    tracer,
		stderr:   io.Discard,
	}
	observation := nudgeDueTargetSelectionObservation{
		Scope:               nudgeshadow.ScopeQueuedExactDueTargetSelection,
		QueueItemCount:      2,
		CandidateCount:      1,
		CandidateDigest:     strings.Repeat("a", 64),
		LegacyCount:         1,
		LegacyDigest:        strings.Repeat("b", 64),
		ComparisonOutcome:   nudgeDueTargetSelectionMismatch,
		QueueDuration:       time.Millisecond,
		CandidateDuration:   2 * time.Millisecond,
		LegacyDuration:      3 * time.Millisecond,
		TotalDuration:       6 * time.Millisecond,
		LegacyEffectOwner:   true,
		ShadowEffectApplied: false,
	}
	cr.recordNudgeDueTargetSelection(observation)
	if err := tracer.Close(); err != nil {
		t.Fatalf("close tracer: %v", err)
	}

	records, err := ReadTraceRecords(traceCityRuntimeDir(cityPath), TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	var got *SessionReconcilerTraceRecord
	for i := range records {
		if records[i].SiteCode == TraceSiteCode("nudge.due_target_selection.shadow") {
			got = &records[i]
			break
		}
	}
	if got == nil {
		t.Fatal("nudge.due_target_selection.shadow trace record not found")
	}
	if got.GCCommit != commit || got.ControllerInstanceID == "" || got.ControllerPID == 0 {
		t.Fatalf("trace provenance = commit:%q controller:%q pid:%d, want collector-populated values",
			got.GCCommit, got.ControllerInstanceID, got.ControllerPID)
	}
	if _, ok := got.Fields["gc_commit"]; ok {
		t.Fatal("trace fields supplied gc_commit instead of using collector provenance")
	}
	if _, ok := got.Fields["controller_instance_id"]; ok {
		t.Fatal("trace fields supplied controller provenance instead of using collector provenance")
	}
	if len(got.Fields) != nudgeDueTargetSelectionTraceFieldLimit {
		t.Fatalf("trace field count = %d, want exactly %d", len(got.Fields), nudgeDueTargetSelectionTraceFieldLimit)
	}
	for _, field := range []string{
		"scope",
		"queue_item_count",
		"candidate_count",
		"candidate_digest",
		"legacy_count",
		"legacy_digest",
		"comparison_outcome",
		"queue_duration_ms",
		"candidate_duration_ms",
		"legacy_duration_ms",
		"total_duration_ms",
		"legacy_effect_owner",
		"shadow_effect_applied",
	} {
		if _, ok := got.Fields[field]; !ok {
			t.Errorf("trace field %q is missing from %#v", field, got.Fields)
		}
	}
	wire, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal trace: %v", err)
	}
	if strings.Contains(string(wire), "session-candidate") ||
		strings.Contains(string(wire), "session-legacy") {
		t.Fatalf("bounded trace leaked a raw session ID: %s", wire)
	}
}

func nudgeShadowTestPtr(value string) *string { return &value }

func requiredNudgeDueTargetSelectionShadowConfig() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "shadow-city"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon: config.DaemonConfig{
			NudgeShadow:       nudgeShadowTestPtr("required"),
			NudgeDispatcher:   "supervisor",
			SessionReconciler: "off",
			ShutdownTimeout:   "0s",
		},
	}
}

func writeNudgeDueTargetSelectionShadowConfig(t *testing.T, path string, cfg *config.City) {
	t.Helper()
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatalf("marshal city config: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write city config: %v", err)
	}
}
