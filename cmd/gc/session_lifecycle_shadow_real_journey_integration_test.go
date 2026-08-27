//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/test/tmuxtest"
)

type sessionLifecycleShadowJourneyStartCall struct {
	Name        string
	EnteredAt   time.Time
	CompletedAt time.Time
	Err         error
}

type sessionLifecycleShadowJourneyProvider struct {
	runtime.Provider

	mu         sync.Mutex
	startCalls []sessionLifecycleShadowJourneyStartCall
}

func (p *sessionLifecycleShadowJourneyProvider) Start(
	ctx context.Context,
	name string,
	cfg runtime.Config,
) error {
	call := sessionLifecycleShadowJourneyStartCall{
		Name:      name,
		EnteredAt: time.Now().UTC(),
	}
	call.Err = p.Provider.Start(ctx, name, cfg)
	call.CompletedAt = time.Now().UTC()

	p.mu.Lock()
	p.startCalls = append(p.startCalls, call)
	p.mu.Unlock()
	return call.Err
}

func (p *sessionLifecycleShadowJourneyProvider) snapshotStartCalls() []sessionLifecycleShadowJourneyStartCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]sessionLifecycleShadowJourneyStartCall(nil), p.startCalls...)
}

type sessionLifecycleShadowJourneySample struct {
	SessionID                   string    `json:"session_id"`
	ObservedAt                  time.Time `json:"observed_at"`
	EnqueuedAt                  time.Time `json:"enqueued_at"`
	ShadowStartedAt             time.Time `json:"shadow_started_at"`
	ShadowCompletedAt           time.Time `json:"shadow_completed_at"`
	LegacyProviderEnteredAt     time.Time `json:"legacy_provider_entered_at"`
	LegacyProviderCompletedAt   time.Time `json:"legacy_provider_completed_at"`
	ObservedToEnqueuedNS        int64     `json:"observed_to_enqueued_ns"`
	QueueLatencyNS              int64     `json:"queue_latency_ns"`
	PlanningLatencyNS           int64     `json:"planning_latency_ns"`
	ObservedToShadowCompletedNS int64     `json:"observed_to_shadow_completed_ns"`
	ObservedToLegacyEntryNS     int64     `json:"observed_to_legacy_entry_ns"`
	LegacyProviderCallNS        int64     `json:"legacy_provider_call_ns"`
	LegacyStartEffects          int       `json:"legacy_start_effects"`
	ComparisonOutcome           string    `json:"comparison_outcome"`
	ComparisonReason            string    `json:"comparison_reason"`
}

// TestSessionLifecycleStartShadowRealTmuxJourney is the singular real-boundary
// proof for the local keyed start-selection shadow. Reproduce a comparable
// sample cohort with:
//
//	go test -tags integration ./cmd/gc \
//	  -run '^TestSessionLifecycleStartShadowRealTmuxJourney$' -count=20 -v
func TestSessionLifecycleStartShadowRealTmuxJourney(t *testing.T) {
	guard := tmuxtest.NewGuard(t)
	cityPath := t.TempDir()
	sessionName := guard.SessionName("worker")
	sessionConfig := config.SessionConfig{
		Socket:             guard.SocketName(),
		SetupTimeout:       "3s",
		NudgeReadyTimeout:  "2s",
		NudgeRetryInterval: "50ms",
		NudgeLockTimeout:   "2s",
		StartupTimeout:     "10s",
	}
	baseProvider, err := newSessionProviderForCityByName(
		nil,
		"",
		sessionConfig,
		guard.CityName(),
		cityPath,
	)
	if err != nil {
		t.Fatalf("construct isolated tmux provider: %v", err)
	}
	provider := &sessionLifecycleShadowJourneyProvider{Provider: baseProvider}
	t.Cleanup(func() {
		if err := provider.Stop(sessionName); err != nil {
			t.Errorf("cleanup isolated tmux session: %v", err)
		}
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	evaluations := make(chan sessionLifecycleStartShadowEvaluation, 1)
	worker, err := newSessionLifecycleShadowWorker(
		1,
		func(evaluation sessionLifecycleStartShadowEvaluation) {
			evaluations <- evaluation
		},
		&stderr,
	)
	if err != nil {
		t.Fatalf("construct lifecycle shadow worker: %v", err)
	}
	cityRuntime := &CityRuntime{
		lifecycleShadowWorker: worker,
		logPrefix:             "shadow-journey",
		stderr:                &stderr,
	}
	cityRuntime.startSessionLifecycleShadowWorker()
	t.Cleanup(cityRuntime.stopSessionLifecycleShadowWorker)
	shadowOption := cityRuntime.sessionLifecycleShadowStartOption(&SessionReconcilerTraceCycle{
		directDetailArms: map[string]sessionLifecycleStartShadowAdmission{
			"worker": {
				Template:  "worker",
				Source:    TraceSourceManual,
				ExpiresAt: time.Now().UTC().Add(time.Minute),
			},
		},
	})
	if shadowOption == nil {
		t.Fatal("started CityRuntime did not publish a shadow start option")
	}

	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents:  []config.Agent{{Name: "worker"}},
		Session: sessionConfig,
	}
	template := TemplateParams{
		Command:      "sleep 600",
		WorkDir:      cityPath,
		SessionName:  sessionName,
		TemplateName: "worker",
	}
	env.desiredState[sessionName] = template
	sessionBead := env.createSessionBead(sessionName, "worker")
	env.markSessionCreating(&sessionBead)
	env.setSessionMetadata(&sessionBead, map[string]string{
		"live_hash": runtime.LiveFingerprint(templateParamsToConfig(template)),
	})

	if provider.IsRunning(sessionName) || guard.HasSession(sessionName) {
		t.Fatalf("isolated tmux session %q existed before reconcile", sessionName)
	}
	woken := reconcileSessionBeadsAtPath(
		t.Context(),
		cityPath,
		[]beads.Bead{sessionBead},
		env.desiredState,
		configuredSessionNames(env.cfg, guard.CityName(), env.store),
		env.cfg,
		provider,
		env.store,
		nil,
		nil,
		nil,
		nil,
		env.dt,
		map[string]int{"worker": 1},
		false,
		nil,
		guard.CityName(),
		nil,
		clock.Real{},
		events.Discard,
		sessionConfig.StartupTimeoutDuration(),
		0,
		&stdout,
		&stderr,
		withStartStabilityWaiter(immediateStartStabilityWaiter),
		withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
		shadowOption,
	)
	if woken != 1 {
		t.Fatalf("legacy wake attempts = %d, want 1; stderr=%q", woken, stderr.String())
	}

	evaluation := receiveShadowEvaluation(t, evaluations)
	startCalls := provider.snapshotStartCalls()
	if len(startCalls) != 1 {
		t.Fatalf("legacy provider Start calls = %d, want exactly 1: %#v", len(startCalls), startCalls)
	}
	startCall := startCalls[0]
	if startCall.Name != sessionName || startCall.Err != nil {
		t.Fatalf("legacy provider Start = %#v, want successful %q start", startCall, sessionName)
	}
	if !provider.IsRunning(sessionName) || !guard.HasSession(sessionName) {
		t.Fatalf("legacy provider returned without live isolated tmux session %q", sessionName)
	}

	if evaluation.Observation.Input.Info.ID != sessionBead.ID ||
		!evaluation.Observation.LegacySelected ||
		evaluation.Comparison.Plan.Outcome != sessionLifecycleStartSelectionPrepare ||
		evaluation.Comparison.Outcome != sessionLifecycleStartSelectionComparisonMatched {
		t.Fatalf("shadow evaluation = %+v, want matching selected start", evaluation)
	}
	observedAt := evaluation.Observation.Input.ObservedAt
	if observedAt.IsZero() ||
		evaluation.EnqueuedAt.Before(observedAt) ||
		evaluation.StartedAt.Before(evaluation.EnqueuedAt) ||
		evaluation.CompletedAt.Before(evaluation.StartedAt) ||
		startCall.EnteredAt.Before(evaluation.EnqueuedAt) ||
		startCall.CompletedAt.Before(startCall.EnteredAt) {
		t.Fatalf("invalid journey ordering: evaluation=%+v start=%+v", evaluation, startCall)
	}
	if evaluation.QueueLatency < 0 || evaluation.PlanningLatency < 0 {
		t.Fatalf(
			"negative shadow latency: queue=%s planning=%s",
			evaluation.QueueLatency,
			evaluation.PlanningLatency,
		)
	}

	sample := sessionLifecycleShadowJourneySample{
		SessionID:                   sessionBead.ID,
		ObservedAt:                  observedAt,
		EnqueuedAt:                  evaluation.EnqueuedAt.UTC(),
		ShadowStartedAt:             evaluation.StartedAt.UTC(),
		ShadowCompletedAt:           evaluation.CompletedAt.UTC(),
		LegacyProviderEnteredAt:     startCall.EnteredAt,
		LegacyProviderCompletedAt:   startCall.CompletedAt,
		ObservedToEnqueuedNS:        evaluation.EnqueuedAt.Sub(observedAt).Nanoseconds(),
		QueueLatencyNS:              evaluation.QueueLatency.Nanoseconds(),
		PlanningLatencyNS:           evaluation.PlanningLatency.Nanoseconds(),
		ObservedToShadowCompletedNS: evaluation.CompletedAt.Sub(observedAt).Nanoseconds(),
		ObservedToLegacyEntryNS:     startCall.EnteredAt.Sub(observedAt).Nanoseconds(),
		LegacyProviderCallNS:        startCall.CompletedAt.Sub(startCall.EnteredAt).Nanoseconds(),
		LegacyStartEffects:          len(startCalls),
		ComparisonOutcome:           string(evaluation.Comparison.Outcome),
		ComparisonReason:            string(evaluation.Comparison.Reason),
	}
	wire, err := json.Marshal(sample)
	if err != nil {
		t.Fatalf("marshal shadow latency sample: %v", err)
	}
	t.Logf("SHADOW_LATENCY_SAMPLE %s", wire)

	if err := provider.Stop(sessionName); err != nil {
		t.Fatalf("stop isolated tmux session: %v", err)
	}
	if provider.IsRunning(sessionName) || guard.HasSession(sessionName) {
		t.Fatalf("isolated tmux session %q remained after Stop", sessionName)
	}
}
