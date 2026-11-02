package main

import (
	"context"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// hookInputPurgedFailureReason is the gc.failure_reason the purge retirement
// stamps: the workflow's input work no longer exists in any store, so the
// dispatcher's hard-failure classification (internal/dispatch retry
// classification) retires it rather than scheduling another attempt.
const hookInputPurgedFailureReason = "input_purged"

// cancelPurgedWorkflow closes rootID and every open/in_progress step under it
// as a hard input_purged failure, the disposition a seat's dispose shape
// writes after burning a claim cycle proving the spec no longer exists —
// applied once here, at the claim that discovered the purge, instead of per
// seat. Terminal beads are left with their recorded outcomes intact.
func cancelPurgedWorkflow(store workflowSubtreeStore, rootID string) error {
	return closeWorkflowSubtree(store, rootID, map[string]string{
		beadmeta.OutcomeMetadataKey:       beadmeta.OutcomeFail,
		beadmeta.FailureClassMetadataKey:  beadmeta.FailureClassHard,
		beadmeta.FailureReasonMetadataKey: hookInputPurgedFailureReason,
		"close_reason":                    sourceworkflow.HookPurgedInputWorkflowCloseReason,
	})
}

// bindProveAbsent binds the ProveAbsent seam to one evaluation's context and
// work directory — the form the input evaluator takes. nil passes through as
// nil: an evaluation without proof support must stay pending, never purged.
func bindProveAbsent(ctx context.Context, dir string, prove hookProveAbsentFunc) proveAbsentFn {
	if prove == nil {
		return nil
	}
	return func(ids []string) (bool, error) {
		return prove(ctx, dir, ids)
	}
}
