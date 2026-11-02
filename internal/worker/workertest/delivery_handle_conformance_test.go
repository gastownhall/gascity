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

// deliveryHandleProfile labels the WC-DELIVERY-* results: handle-behavior
// contracts, not per-fixture-profile rules.
const deliveryHandleProfile ProfileID = "worker-handle"

// TestDeliveryHandleConformance emits WC-DELIVERY-001 and WC-DELIVERY-002
// through the SuiteReporter artifact path, driving a bead-backed
// SessionHandle whose session waits on a pending interaction.
func TestDeliveryHandleConformance(t *testing.T) {
	reporter := NewSuiteReporter(t, "delivery-handle", map[string]string{
		"tier": "worker-core",
	})
	reporter.Require(t, deliveryQueuesBehindPendingResult(t))
	reporter.Require(t, acceptedMessageQueuesBehindPendingResult(t))
}

func TestDeliveryHandleCatalogRegistersQueueRequirements(t *testing.T) {
	catalog := DeliveryHandleCatalog()
	want := []RequirementCode{RequirementDeliveryQueuesBehindPending, RequirementAcceptedMessageQueuesBehindPending}
	if len(catalog) != len(want) {
		t.Fatalf("DeliveryHandleCatalog() entries = %d, want %d", len(catalog), len(want))
	}
	for i, req := range catalog {
		if req.Code != want[i] {
			t.Fatalf("entry %d code = %q, want %q", i, req.Code, want[i])
		}
		if req.Group == "" || req.Description == "" {
			t.Fatalf("requirement %s has empty group or description", req.Code)
		}
	}
}

// pendingSessionHandle is a started bead-backed SessionHandle whose session
// waits on a permission prompt.
type pendingSessionHandle struct {
	handle      worker.Handle
	sp          *runtime.Fake
	sessionName string
	cityPath    string
	by          sessionpkg.Actor
}

func startPendingSessionHandle(t *testing.T) pendingSessionHandle {
	t.Helper()

	cityPath := t.TempDir()
	sp := runtime.NewFake()
	manager := sessionpkg.NewManagerWithOptions(beads.NewMemStore(), sp, sessionpkg.WithCityPath(cityPath))
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
	sp.SetPendingInteraction(state.SessionName, &runtime.PendingInteraction{RequestID: "req-1", Kind: "approval", Prompt: "Allow Bash?"})
	return pendingSessionHandle{
		handle:      handle,
		sp:          sp,
		sessionName: state.SessionName,
		cityPath:    cityPath,
		by:          managerActor(t, manager, sessionpkg.ActorBackground),
	}
}

// typed counts the texts typed into the session.
func (p pendingSessionHandle) typed() int {
	return p.sp.CountCalls("Nudge", p.sessionName) + p.sp.CountCalls("NudgeNow", p.sessionName)
}

// queued lists the session's queued deliveries.
func (p pendingSessionHandle) queued(t *testing.T) []nudgequeue.Item {
	t.Helper()
	state, err := nudgequeue.LoadState(p.cityPath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	return state.Pending
}

// deliveryQueuesBehindPendingResult evaluates WC-DELIVERY-001. Setup
// failures fail the test loudly; only a contract violation yields a failing
// result.
func deliveryQueuesBehindPendingResult(t *testing.T) Result {
	t.Helper()

	p := startPendingSessionHandle(t)
	ctx := context.Background()
	queuedResult, queuedErr := p.handle.Nudge(ctx, p.by, worker.NudgeRequest{Text: "behind the prompt"})
	_, immediateErr := p.handle.Nudge(ctx, p.by, worker.NudgeRequest{Text: "now", Delivery: worker.NudgeDeliveryImmediate})
	typed := p.typed()
	queue := p.queued(t)

	evidence := map[string]string{
		"default_undelivered": string(queuedResult.Undelivered),
		"default_error":       fmt.Sprint(queuedErr),
		"immediate_error":     fmt.Sprint(immediateErr),
		"typed":               strconv.Itoa(typed),
		"queued_items":        strconv.Itoa(len(queue)),
	}
	fail := func(detail string) Result {
		return Fail(deliveryHandleProfile, RequirementDeliveryQueuesBehindPending, detail).WithEvidence(evidence)
	}
	switch {
	case queuedErr != nil || queuedResult.Undelivered != worker.NudgeQueued:
		return fail("a default nudge behind a pending interaction was not queued")
	case len(queue) != 1 || queue[0].Message != "behind the prompt":
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

// acceptedMessageQueuesBehindPendingResult evaluates WC-DELIVERY-002.
func acceptedMessageQueuesBehindPendingResult(t *testing.T) Result {
	t.Helper()

	p := startPendingSessionHandle(t)
	ctx := context.Background()
	_, plainErr := p.handle.Message(ctx, p.by, worker.MessageRequest{Text: "refused"})
	accepted, acceptedErr := p.handle.Message(ctx, p.by, worker.MessageRequest{Text: "first prompt", QueueBehindPending: true})
	typed := p.typed()
	queue := p.queued(t)

	evidence := map[string]string{
		"plain_error":     fmt.Sprint(plainErr),
		"accepted_queued": strconv.FormatBool(accepted.Queued),
		"accepted_error":  fmt.Sprint(acceptedErr),
		"typed":           strconv.Itoa(typed),
		"queued_items":    strconv.Itoa(len(queue)),
	}
	fail := func(detail string) Result {
		return Fail(deliveryHandleProfile, RequirementAcceptedMessageQueuesBehindPending, detail).WithEvidence(evidence)
	}
	switch {
	case !errors.Is(plainErr, sessionpkg.ErrPendingInteraction):
		return fail("a plain message behind a pending interaction was not refused with the pending-interaction error")
	case acceptedErr != nil || !accepted.Queued:
		return fail("an accepted message behind a pending interaction was not queued")
	case len(queue) != 1 || queue[0].Message != "first prompt" || queue[0].Source != nudgequeue.SourceSubmit:
		return fail("the accepted message is not queued as a user turn")
	case typed != 0:
		return fail("text was typed into the pending interaction")
	}
	return Pass(deliveryHandleProfile, RequirementAcceptedMessageQueuesBehindPending,
		"plain message refused; accepted message queued as a user turn; nothing typed").
		WithEvidence(evidence)
}
