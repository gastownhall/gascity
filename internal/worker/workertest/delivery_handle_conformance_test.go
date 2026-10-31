package workertest

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	worker "github.com/gastownhall/gascity/internal/worker"
)

// deliveryHandleProfile labels the WC-DELIVERY-001 result: a single
// handle-behavior contract, not a per-fixture-profile rule.
const deliveryHandleProfile ProfileID = "worker-handle"

// TestDeliveryHandleConformance emits WC-DELIVERY-001
// (RequirementDeliveryQueuesBehindPending) through the SuiteReporter
// artifact path: a bead-backed SessionHandle queues a default nudge behind a
// pending interaction and refuses an immediate one.
func TestDeliveryHandleConformance(t *testing.T) {
	reporter := NewSuiteReporter(t, "delivery-handle", map[string]string{
		"tier": "worker-core",
	})
	reporter.Require(t, deliveryQueuesBehindPendingResult(t))
}

func TestDeliveryHandleCatalogRegistersQueueRequirement(t *testing.T) {
	catalog := DeliveryHandleCatalog()
	if len(catalog) != 1 {
		t.Fatalf("DeliveryHandleCatalog() entries = %d, want 1", len(catalog))
	}
	req := catalog[0]
	if req.Code != RequirementDeliveryQueuesBehindPending {
		t.Fatalf("code = %q, want %q", req.Code, RequirementDeliveryQueuesBehindPending)
	}
	if req.Group == "" || req.Description == "" {
		t.Fatalf("requirement %s has empty group or description", req.Code)
	}
}

// deliveryQueuesBehindPendingResult evaluates WC-DELIVERY-001. Setup
// failures fail the test loudly; only a contract violation yields a failing
// result.
func deliveryQueuesBehindPendingResult(t *testing.T) Result {
	t.Helper()

	cityPath := t.TempDir()
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	manager := sessionpkg.NewManagerWithOptions(store, sp, sessionpkg.WithCityPath(cityPath))
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
	ctx := context.Background()
	if err := handle.Start(ctx, managerActor(t, manager, sessionpkg.ActorOperator)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	state, err := handle.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	by := managerActor(t, manager, sessionpkg.ActorBackground)
	sp.SetPendingInteraction(state.SessionName, &runtime.PendingInteraction{RequestID: "req-1", Kind: "approval", Prompt: "Allow Bash?"})

	queuedResult, queuedErr := handle.Nudge(ctx, by, worker.NudgeRequest{Text: "behind the prompt"})
	_, immediateErr := handle.Nudge(ctx, by, worker.NudgeRequest{Text: "now", Delivery: worker.NudgeDeliveryImmediate})
	typed := sp.CountCalls("Nudge", state.SessionName) + sp.CountCalls("NudgeNow", state.SessionName)
	queue, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}

	evidence := map[string]string{
		"default_undelivered": string(queuedResult.Undelivered),
		"default_error":       fmt.Sprint(queuedErr),
		"immediate_error":     fmt.Sprint(immediateErr),
		"typed":               strconv.Itoa(typed),
		"queued_items":        strconv.Itoa(len(queue.Pending)),
	}
	fail := func(detail string) Result {
		return Fail(deliveryHandleProfile, RequirementDeliveryQueuesBehindPending, detail).WithEvidence(evidence)
	}
	switch {
	case queuedErr != nil || queuedResult.Undelivered != worker.NudgeQueued:
		return fail("a default nudge behind a pending interaction was not queued")
	case len(queue.Pending) != 1 || queue.Pending[0].Message != "behind the prompt":
		return fail("the queued nudge is not in the session's delivery queue")
	case !errors.Is(immediateErr, sessionpkg.ErrPendingInteraction):
		return fail("an immediate nudge behind a pending interaction was not refused with the pending-interaction error")
	case typed != 0:
		return fail("text was typed into the pending interaction")
	}
	return Pass(deliveryHandleProfile, RequirementDeliveryQueuesBehindPending,
		"default nudge queued behind the pending interaction; immediate nudge refused; nothing typed").
		WithEvidence(evidence)
}
