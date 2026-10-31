package workertest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	worker "github.com/gastownhall/gascity/internal/worker"
)

// nudgeHandleProfile labels the WC-NUDGE-001 result: a global handle-behavior
// contract, not a per-fixture-profile rule.
const nudgeHandleProfile ProfileID = "worker-handle"

// idleSnapshotProvider is a Fake whose one-look idle snapshot answers idle
// and err (runtime.IdleSnapshotProvider).
type idleSnapshotProvider struct {
	*runtime.Fake
	idle bool
	err  error
}

func (p *idleSnapshotProvider) SnapshotIdle(string) (bool, error) { return p.idle, p.err }

// waitIdleReading is one pre-read a wait-idle nudge may take (an idle
// snapshot, and the pending interaction the runtime reports), and whether the
// nudge must then wait for idle and deliver.
type waitIdleReading struct {
	name      string
	idle      bool
	err       error
	pending   *runtime.PendingInteraction
	delivered bool
}

var waitIdleReadings = []waitIdleReading{
	{name: "busy"},
	{
		name:    "pending",
		err:     fmt.Errorf("scan: %w", runtime.ErrIdleIndeterminate),
		pending: &runtime.PendingInteraction{RequestID: "tmux-1", Kind: "approval", Prompt: "Allow Bash?"},
	},
	{name: "at the boundary", idle: true, delivered: true},
	{name: "indeterminate with nothing pending", err: fmt.Errorf("scan: %w", runtime.ErrIdleIndeterminate), delivered: true},
}

// TestNudgeHandleConformance emits WC-NUDGE-001
// (RequirementWaitIdleOutOfReachQueuesAtOnce) through the SuiteReporter
// artifact path, driving both handles through the exported worker API.
func TestNudgeHandleConformance(t *testing.T) {
	reporter := NewSuiteReporter(t, "nudge-handle", map[string]string{
		"tier": "worker-core",
	})
	reporter.Require(t, waitIdleOutOfReachResult(t))
}

func TestNudgeHandleCatalogRegistersWaitIdleRequirement(t *testing.T) {
	catalog := NudgeHandleCatalog()
	if len(catalog) != 1 || catalog[0].Code != RequirementWaitIdleOutOfReachQueuesAtOnce {
		t.Fatalf("NudgeHandleCatalog() = %+v, want only %s", catalog, RequirementWaitIdleOutOfReachQueuesAtOnce)
	}
	if catalog[0].Group == "" || catalog[0].Description == "" {
		t.Fatalf("requirement %s has empty group or description", catalog[0].Code)
	}
}

// waitIdleOutOfReachResult evaluates WC-NUDGE-001 for every reading on both
// handles. Setup failures fail the test loudly; only a contract violation is
// a failing result.
func waitIdleOutOfReachResult(t *testing.T) Result {
	t.Helper()
	var violations []string
	for _, reading := range waitIdleReadings {
		for handleKind, nudge := range map[string]func(*testing.T, waitIdleReading) (worker.NudgeResult, *runtime.Fake, string){
			"SessionHandle": sessionHandleWaitIdleNudge,
			"RuntimeHandle": runtimeHandleWaitIdleNudge,
		} {
			result, fake, name := nudge(t, reading)
			waited := fake.CountCalls("WaitForIdle", name) != 0
			typed := fake.CountCalls("Nudge", name)+fake.CountCalls("NudgeNow", name) != 0
			if result.Delivered != reading.delivered || waited != reading.delivered || typed != reading.delivered {
				violations = append(violations, fmt.Sprintf("%s %s: delivered=%t waited=%t typed=%t, want all %t",
					handleKind, reading.name, result.Delivered, waited, typed, reading.delivered))
			}
			if handleKind == "RuntimeHandle" && !reading.delivered && result.Undelivered != worker.NudgeUndeliveredNoIdleBoundary {
				violations = append(violations, fmt.Sprintf("RuntimeHandle %s: undelivered %q, want %q",
					reading.name, result.Undelivered, worker.NudgeUndeliveredNoIdleBoundary))
			}
		}
	}
	if len(violations) > 0 {
		return Fail(nudgeHandleProfile, RequirementWaitIdleOutOfReachQueuesAtOnce, strings.Join(violations, "; "))
	}
	return Pass(nudgeHandleProfile, RequirementWaitIdleOutOfReachQueuesAtOnce,
		"busy and pending pre-reads queue at once on both handles; a boundary or indeterminate pre-read waits and delivers")
}

// sessionHandleWaitIdleNudge sends a wait-idle nudge through a claude
// SessionHandle whose runtime pre-reads reading.
func sessionHandleWaitIdleNudge(t *testing.T, reading waitIdleReading) (worker.NudgeResult, *runtime.Fake, string) {
	t.Helper()
	fake := runtime.NewFake()
	sp := &idleSnapshotProvider{Fake: fake, idle: reading.idle, err: reading.err}
	manager := sessionpkg.NewManagerWithOptions(beads.NewMemStore(), sp, sessionpkg.WithCityPath(t.TempDir()))
	handle, err := worker.NewSessionHandle(worker.SessionHandleConfig{
		Manager: manager,
		Session: worker.SessionSpec{
			Profile:  worker.ProfileClaudeTmuxCLI,
			Template: "probe",
			Title:    "Probe",
			Command:  "claude",
			WorkDir:  t.TempDir(),
			Provider: "claude",
		},
	})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}
	if err := handle.Start(context.Background(), managerActor(t, manager, sessionpkg.ActorAgent)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The nudge comes from the background, as mail and sling nudges do.
	actor := managerActor(t, manager, sessionpkg.ActorBackground)
	state, err := handle.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	fake.WaitForIdleErrors[state.SessionName] = nil
	fake.SetPendingInteraction(state.SessionName, reading.pending)
	result, err := handle.Nudge(context.Background(), actor, worker.NudgeRequest{
		Text:     "check deploy status",
		Delivery: worker.NudgeDeliveryWaitIdle,
		Source:   "mail",
	})
	if err != nil {
		t.Fatalf("SessionHandle Nudge(wait_idle) %s: %v", reading.name, err)
	}
	return result, fake, state.SessionName
}

// runtimeHandleWaitIdleNudge sends a wait-idle nudge through a claude
// RuntimeHandle whose runtime pre-reads reading.
func runtimeHandleWaitIdleNudge(t *testing.T, reading waitIdleReading) (worker.NudgeResult, *runtime.Fake, string) {
	t.Helper()
	const name = "nudge-worker"
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), name, runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fake.WaitForIdleErrors[name] = nil
	fake.SetPendingInteraction(name, reading.pending)
	handle, err := worker.NewRuntimeHandle(worker.RuntimeHandleConfig{
		Provider:     &idleSnapshotProvider{Fake: fake, idle: reading.idle, err: reading.err},
		SessionName:  name,
		ProviderName: "claude",
	})
	if err != nil {
		t.Fatalf("NewRuntimeHandle: %v", err)
	}
	result, err := handle.Nudge(context.Background(), testAgent(t), worker.NudgeRequest{
		Text:     "check deploy status",
		Delivery: worker.NudgeDeliveryWaitIdle,
		Source:   "mail",
	})
	if err != nil {
		t.Fatalf("RuntimeHandle Nudge(wait_idle) %s: %v", reading.name, err)
	}
	return result, fake, name
}
