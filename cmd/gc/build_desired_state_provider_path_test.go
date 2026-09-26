package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// missingProviderCommand is a binary name no host will have on PATH. The
// reproduction depends on a real exec.LookPath miss (newAgentBuildParams
// installs exec.LookPath unconditionally), so the name has to be one nothing
// could plausibly install.
const missingProviderCommand = "gc-provider-that-is-not-installed-ob-woag"

func missingProviderCity() *config.City {
	return &config.City{
		Agents: []config.Agent{
			{Name: "worker", Provider: "ghost"},
		},
		NamedSessions: []config.NamedSession{
			{Name: "worker", Template: "worker", Mode: "always"},
		},
		Providers: map[string]config.ProviderSpec{
			"ghost": {Command: missingProviderCommand},
		},
	}
}

// TestBuildDesiredState_MissingProviderBinary_ReportsDroppedTemplates is the
// ob-woag reproduction. A city whose only always-on template names a provider
// whose binary is absent reconciles to an EMPTY desired set — that part is
// correct and unchanged, because there is no way to start the session. What
// was wrong is that the emptiness was all the caller got back: the drop sites
// print one line to a stderr that under the supervisor is a log file, and
// return a quietly smaller State. `gc start` then reported success and the
// city stayed empty with no surface naming the cause.
//
// The assertion that matters is the second one: the result must carry the
// reason for its own emptiness.
func TestBuildDesiredState_MissingProviderBinary_ReportsDroppedTemplates(t *testing.T) {
	tmpDir := t.TempDir()
	var stderr bytes.Buffer

	result := buildDesiredStateWithSessionBeads(
		"test-city", tmpDir, time.Now(), missingProviderCity(), &localMockProvider{},
		beads.NewMemStore(), nil, &sessionBeadSnapshot{}, nil, &stderr,
	)

	if len(result.State) != 0 {
		t.Fatalf("desired sessions = %d, want 0 (the session cannot be built without its provider); keys=%v", len(result.State), mapKeys(result.State))
	}
	if len(result.ProviderUnresolved) != 1 {
		t.Fatalf("ProviderUnresolved = %+v, want exactly one group; a build that dropped every session must say why. stderr=%q",
			result.ProviderUnresolved, stderr.String())
	}
	group := result.ProviderUnresolved[0]
	if group.Provider != "ghost" {
		t.Errorf("group.Provider = %q, want %q", group.Provider, "ghost")
	}
	if group.Command != missingProviderCommand {
		t.Errorf("group.Command = %q, want %q", group.Command, missingProviderCommand)
	}
	if group.DroppedTemplates < 1 {
		t.Errorf("group.DroppedTemplates = %d, want at least 1", group.DroppedTemplates)
	}
	if len(group.Templates) == 0 || !strings.Contains(strings.Join(group.Templates, ","), "worker") {
		t.Errorf("group.Templates = %v, want the dropped template named", group.Templates)
	}
}

// TestBuildDesiredState_ResolvableProvider_ReportsNothing pins the negative:
// the report is a fault signal, not a field that is always populated. "true"
// is on PATH everywhere this test suite runs.
func TestBuildDesiredState_ResolvableProvider_ReportsNothing(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := missingProviderCity()
	cfg.Providers["ghost"] = config.ProviderSpec{Command: "true"}

	result := buildDesiredStateWithSessionBeads(
		"test-city", tmpDir, time.Now(), cfg, &localMockProvider{},
		beads.NewMemStore(), nil, &sessionBeadSnapshot{}, nil, io.Discard,
	)

	if len(result.ProviderUnresolved) != 0 {
		t.Fatalf("ProviderUnresolved = %+v, want empty for a provider that resolves", result.ProviderUnresolved)
	}
}

// TestBuildDesiredState_MissingProviderBinary_RecordsTrace covers the second
// surface ob-woag named: the session-reconciler trace computed
// desired_session_count=0 and the string "provider" appeared ZERO times in it,
// so the trace read as "no demand" rather than "broken host".
func TestBuildDesiredState_MissingProviderBinary_RecordsTrace(t *testing.T) {
	cityDir := t.TempDir()
	writeCityTOML(t, cityDir, "provider-path-town", "mayor")
	cfg := missingProviderCity()

	tracer := newSessionReconcilerTracer(cityDir, "provider-path-town", io.Discard)
	if !tracer.Enabled() {
		t.Fatal("tracer should be enabled")
	}
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	cycle := mustBeginCycle(t, tracer, now, cfg)
	traceID := cycle.traceID

	buildDesiredStateWithSessionBeads(
		"provider-path-town", cityDir, now, cfg, &localMockProvider{},
		beads.NewMemStore(), nil, &sessionBeadSnapshot{}, cycle, io.Discard,
	)

	if err := cycle.End(TraceCompletionCompleted, traceRecordPayload{"phase": "tick"}); err != nil {
		t.Fatalf("cycle.End: %v", err)
	}
	if err := tracer.Close(); err != nil {
		t.Fatalf("tracer.Close: %v", err)
	}

	records, err := ReadTraceRecords(traceCityRuntimeDir(cityDir), TraceFilter{TraceID: traceID})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	var found bool
	for _, rec := range records {
		if rec.SiteCode != TraceSiteDesiredStateProviderUnresolved {
			continue
		}
		found = true
		if rec.ReasonCode != TraceReasonProviderNotInPath {
			t.Errorf("reason_code = %q, want %q", rec.ReasonCode, TraceReasonProviderNotInPath)
		}
		if got := rec.Fields["provider"]; got != "ghost" {
			t.Errorf("trace provider field = %#v, want %q", got, "ghost")
		}
		if got := rec.Fields["command"]; got != missingProviderCommand {
			t.Errorf("trace command field = %#v, want %q", got, missingProviderCommand)
		}
	}
	if !found {
		t.Fatalf("no %q record in %d trace records; the trace still cannot explain an empty desired set",
			TraceSiteDesiredStateProviderUnresolved, len(records))
	}
}

// TestReportUnresolvedProviders_AlertsOncePerEpisode covers the operator-facing
// half: a controller log line and a .gc/events.jsonl record, each written once
// per episode rather than once per tick. ob-woag found nothing in either place;
// the failure mode of a naive fix is writing to both on every tick forever.
func TestReportUnresolvedProviders_AlertsOncePerEpisode(t *testing.T) {
	var stderr bytes.Buffer
	rec := events.NewFake()
	cr := &CityRuntime{logPrefix: "gc supervisor", stdout: io.Discard, stderr: &stderr, rec: rec}

	failing := DesiredStateResult{
		ProviderUnresolved: []ProviderUnresolvedGroup{{
			Provider:         "ghost",
			Command:          missingProviderCommand,
			DroppedTemplates: 2,
			Templates:        []string{"worker", "worker-2"},
		}},
	}

	cr.reportUnresolvedProviders(failing)
	first := stderr.String()
	if !strings.Contains(first, "ghost") || !strings.Contains(first, missingProviderCommand) {
		t.Fatalf("first alert = %q, want the provider and command named", first)
	}
	if !strings.Contains(first, "desired_session_count=0") {
		t.Errorf("first alert = %q, want the desired_session_count that made the city look normal", first)
	}
	if len(rec.Events) != 1 {
		t.Fatalf("recorded events = %d, want 1", len(rec.Events))
	}
	if rec.Events[0].Type != events.ProviderUnresolved {
		t.Errorf("event type = %q, want %q", rec.Events[0].Type, events.ProviderUnresolved)
	}
	if rec.Events[0].Subject != "ghost" {
		t.Errorf("event subject = %q, want %q", rec.Events[0].Subject, "ghost")
	}

	// Same failure, next tick: latched.
	cr.reportUnresolvedProviders(failing)
	if got := stderr.String(); got != first {
		t.Errorf("second tick wrote another line; a per-tick alert is its own outage:\n%s", got)
	}
	if len(rec.Events) != 1 {
		t.Errorf("recorded events = %d after a second identical tick, want 1", len(rec.Events))
	}

	// Provider installed: the latch clears, so a later removal alerts again.
	cr.reportUnresolvedProviders(DesiredStateResult{})
	if len(rec.Events) != 1 {
		t.Errorf("recovery recorded an event: %+v", rec.Events)
	}
	cr.reportUnresolvedProviders(failing)
	if len(rec.Events) != 2 {
		t.Fatalf("recorded events = %d after a recurrence, want 2", len(rec.Events))
	}
}

// TestBeadReconcileTick_MissingProvider_CarriesCountInTrace pins the field that
// answers the original question directly: desired_session_count and
// provider_unresolved_count are in the SAME cycle-input record, so a zero
// desired count can no longer be read as an idle city.
func TestBeadReconcileTick_MissingProvider_CarriesCountInTrace(t *testing.T) {
	cityDir := t.TempDir()
	writeCityTOML(t, cityDir, "provider-path-town", "mayor")
	cfg := missingProviderCity()

	tracer := newSessionReconcilerTracer(cityDir, "provider-path-town", io.Discard)
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	cycle := mustBeginCycle(t, tracer, now, cfg)
	traceID := cycle.traceID

	var stderr bytes.Buffer
	cr := &CityRuntime{
		cityPath:            cityDir,
		cityName:            "provider-path-town",
		cfg:                 cfg,
		sp:                  runtime.NewFake(),
		trace:               tracer,
		standaloneCityStore: beads.NewMemStore(),
		sessionDrains:       newDrainTracker(),
		rec:                 events.NewFake(),
		logPrefix:           "gc supervisor",
		stdout:              io.Discard,
		stderr:              &stderr,
	}

	result := DesiredStateResult{
		State: map[string]TemplateParams{},
		ProviderUnresolved: []ProviderUnresolvedGroup{{
			Provider:         "ghost",
			Command:          missingProviderCommand,
			DroppedTemplates: 1,
			Templates:        []string{"worker"},
		}},
	}
	cr.beadReconcileTick(context.Background(), result, newSessionBeadSnapshot(nil), cycle, false)
	if err := cycle.End(TraceCompletionCompleted, traceRecordPayload{"phase": "tick"}); err != nil {
		t.Fatalf("cycle.End: %v", err)
	}
	if err := tracer.Close(); err != nil {
		t.Fatalf("tracer.Close: %v", err)
	}

	records, err := ReadTraceRecords(traceCityRuntimeDir(cityDir), TraceFilter{TraceID: traceID})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	var sawSnapshot bool
	for _, r := range records {
		if r.RecordType != TraceRecordCycleInputSnapshot {
			continue
		}
		sawSnapshot = true
		if got := traceFieldInt(r.Fields["desired_session_count"]); got != 0 {
			t.Errorf("desired_session_count = %d, want 0", got)
		}
		if got := traceFieldInt(r.Fields["provider_unresolved_count"]); got != 1 {
			t.Errorf("provider_unresolved_count = %#v, want 1 beside desired_session_count=0", r.Fields["provider_unresolved_count"])
		}
	}
	if !sawSnapshot {
		t.Fatal("no cycle input snapshot record")
	}
	if !strings.Contains(stderr.String(), "not on PATH") {
		t.Errorf("controller log = %q, want the provider alert", stderr.String())
	}
}

// mustBeginCycle opens a trace cycle or fails the test. It exists so callers
// get a value they can use directly: an inline `if cycle == nil { t.Fatal }`
// followed by a later field read is what staticcheck's SA5011 reports, and the
// warning is fair — the check reads as though nil were survivable here.
func mustBeginCycle(t *testing.T, tracer *SessionReconcilerTracer, now time.Time, cfg *config.City) *SessionReconcilerTraceCycle {
	t.Helper()
	cycle := tracer.BeginCycle(TraceTickTriggerPatrol, "controller_tick", now, cfg)
	if cycle == nil {
		t.Fatal("BeginCycle returned nil")
	}
	return cycle
}
