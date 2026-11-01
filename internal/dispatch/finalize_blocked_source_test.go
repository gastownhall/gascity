package dispatch

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// mustStampSourceChainMetadata stamps metadata onto a source-chain bead,
// failing the test on the first write error.
func mustStampSourceChainMetadata(t *testing.T, store beads.Store, beadID string, metadata map[string]string) {
	t.Helper()
	if err := store.SetMetadataBatch(beadID, metadata); err != nil {
		t.Fatalf("stamping metadata on %s: %v", beadID, err)
	}
}

// sourceChainTraceOptions returns finalize options that resolve the fixture's
// cross-store refs and record every trace line into the returned buffer.
func sourceChainTraceOptions(f sourceChainFinalizeFixture) (ProcessOptions, *bytes.Buffer) {
	var traceBuf bytes.Buffer
	return ProcessOptions{
		ResolveStoreRef: f.resolver,
		Tracef: func(format string, args ...any) {
			fmt.Fprintf(&traceBuf, format, args...)
			traceBuf.WriteByte('\n')
		},
	}, &traceBuf
}

// assertBlockedSourceHoldsChain checks the post-finalize state every blocked
// source must reach: the workflow root and finalizer closed as passed, the
// source itself closed in its blocked form with the root's domain metadata
// propagated onto it, and the source's own parent untouched above it.
func assertBlockedSourceHoldsChain(t *testing.T, f sourceChainFinalizeFixture, traced string) {
	t.Helper()
	rootAfter := mustGetBead(t, f.rigStore, f.workflow.ID)
	if rootAfter.Status != "closed" || rootAfter.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
		t.Fatalf("workflow root = status %q gc.outcome %q, want closed pass", rootAfter.Status, rootAfter.Metadata[beadmeta.OutcomeMetadataKey])
	}
	finalizerAfter := mustGetBead(t, f.rigStore, f.finalizer.ID)
	if finalizerAfter.Status != "closed" {
		t.Fatalf("finalizer status = %q, want closed", finalizerAfter.Status)
	}
	sourceAfter := mustGetBead(t, f.rigStore, f.rigLaunch.ID)
	if sourceAfter.Status != "closed" {
		t.Fatalf("blocked source status = %q, want closed in its blocked form", sourceAfter.Status)
	}
	if got := sourceAfter.Metadata[beadmeta.WorkOutcomeMetadataKey]; got != beadmeta.WorkOutcomeBlocked {
		t.Fatalf("blocked source gc.work_outcome = %q, want %q", got, beadmeta.WorkOutcomeBlocked)
	}
	if got := sourceAfter.Metadata[beadmeta.OutcomeMetadataKey]; got != beadmeta.OutcomePass {
		t.Fatalf("blocked source gc.outcome = %q, want pass", got)
	}
	if got := sourceAfter.Metadata["pr_review.verdict"]; got != "approve" {
		t.Fatalf("blocked source pr_review.verdict = %q, want the root's propagated approve", got)
	}
	parentAfter := mustGetBead(t, f.cityStore, f.citySource.ID)
	if parentAfter.Status == "closed" {
		t.Fatalf("parent of blocked source closed with gc.outcome %q; the chain must stop at a blocked source", parentAfter.Metadata[beadmeta.OutcomeMetadataKey])
	}
	if got := parentAfter.Metadata[beadmeta.OutcomeMetadataKey]; got != "" {
		t.Fatalf("parent gc.outcome = %q, want none", got)
	}
	if got := parentAfter.Metadata["pr_review.verdict"]; got != "" {
		t.Fatalf("parent pr_review.verdict = %q, want none", got)
	}
	if !strings.Contains(traced, "stop reason=blocked_source source="+f.rigLaunch.ID) {
		t.Fatalf("trace missing blocked_source stop; got:\n%s", traced)
	}
}

// TestProcessWorkflowFinalizeStopsSourceChainAtBlockedSourceStates pins the
// blocked-source stop of the finalize source-chain walk across both states a
// blocked source can be reached in — already closed by mol-do-work's
// typed-outcome close (gc.outcome=pass + gc.work_outcome=blocked) or still
// open with gc.work_outcome=blocked — and both root formulas the claim names
// (the walk never reads gc.formula_name, so both must behave identically).
// Either way the source settles closed in its blocked form with the root's
// domain metadata on it, and the chain above it stays open: blocked work did
// not complete the parent, the same way beads.DependencySatisfied withholds
// dependents of a closed blocked bead.
func TestProcessWorkflowFinalizeStopsSourceChainAtBlockedSourceStates(t *testing.T) {
	t.Parallel()

	rows := []struct {
		name          string
		sourceClosed  bool
		sourceOutcome string
	}{
		{"source_closed_by_typed_outcome", true, beadmeta.OutcomePass},
		{"source_open_blocked", false, ""},
	}
	for _, row := range rows {
		for _, formulaName := range []string{"mol-do-work", "mol-polecat-report"} {
			t.Run(row.name+"/formula="+formulaName, func(t *testing.T) {
				t.Parallel()

				f := newSourceChainFinalizeFixture(t)
				if row.sourceClosed {
					if err := f.rigStore.Close(f.rigLaunch.ID); err != nil {
						t.Fatalf("closing blocked source: %v", err)
					}
				}
				stamp := map[string]string{beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeBlocked}
				if row.sourceOutcome != "" {
					stamp[beadmeta.OutcomeMetadataKey] = row.sourceOutcome
				}
				mustStampSourceChainMetadata(t, f.rigStore, f.rigLaunch.ID, stamp)
				mustStampSourceChainMetadata(t, f.rigStore, f.workflow.ID, map[string]string{
					beadmeta.FormulaNameMetadataKey: formulaName,
					"pr_review.verdict":             "approve",
				})

				opts, traceBuf := sourceChainTraceOptions(f)
				result, err := ProcessControl(f.rigStore, f.finalizer, opts)
				if err != nil {
					t.Fatalf("ProcessControl(workflow-finalize): %v", err)
				}
				if !result.Processed || result.Action != "workflow-pass" {
					t.Fatalf("result = processed %t action %q, want processed workflow-pass", result.Processed, result.Action)
				}
				assertBlockedSourceHoldsChain(t, f, traceBuf.String())
			})
		}
	}
}

// TestProcessWorkflowFinalizeClearsStaleFailureStampOnBlockedSource pins the
// already-closed treatment a blocked source still receives: the pass path's
// propagate clears a stale failure stamp from an earlier failed attempt while
// the chain above stays open.
func TestProcessWorkflowFinalizeClearsStaleFailureStampOnBlockedSource(t *testing.T) {
	t.Parallel()

	f := newSourceChainFinalizeFixture(t)
	if err := f.rigStore.Close(f.rigLaunch.ID); err != nil {
		t.Fatalf("closing blocked source: %v", err)
	}
	mustStampSourceChainMetadata(t, f.rigStore, f.rigLaunch.ID, map[string]string{
		beadmeta.OutcomeMetadataKey:        beadmeta.OutcomePass,
		beadmeta.WorkOutcomeMetadataKey:    beadmeta.WorkOutcomeBlocked,
		beadmeta.FailureReasonMetadataKey:  "earlier attempt failed",
		beadmeta.FailureClassMetadataKey:   "transient",
		beadmeta.FailureSubjectMetadataKey: f.rigLaunch.ID,
	})
	mustStampSourceChainMetadata(t, f.rigStore, f.workflow.ID, map[string]string{"pr_review.verdict": "approve"})

	opts, traceBuf := sourceChainTraceOptions(f)
	result, err := ProcessControl(f.rigStore, f.finalizer, opts)
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if !result.Processed || result.Action != "workflow-pass" {
		t.Fatalf("result = processed %t action %q, want processed workflow-pass", result.Processed, result.Action)
	}
	sourceAfter := mustGetBead(t, f.rigStore, f.rigLaunch.ID)
	for _, key := range []string{beadmeta.FailureReasonMetadataKey, beadmeta.FailureClassMetadataKey, beadmeta.FailureSubjectMetadataKey} {
		if got := strings.TrimSpace(sourceAfter.Metadata[key]); got != "" {
			t.Fatalf("blocked source %s = %q, want cleared", key, got)
		}
	}
	parentAfter := mustGetBead(t, f.cityStore, f.citySource.ID)
	if parentAfter.Status == "closed" {
		t.Fatalf("parent of blocked source closed with gc.outcome %q", parentAfter.Metadata[beadmeta.OutcomeMetadataKey])
	}
	if !strings.Contains(traceBuf.String(), "stop reason=blocked_source source="+f.rigLaunch.ID) {
		t.Fatalf("trace missing blocked_source stop; got:\n%s", traceBuf.String())
	}
}

// TestProcessWorkflowFinalizeKeepsLiveChildPrecedenceOverBlockedSource pins
// that a live child workflow on an open blocked source keeps its precedence
// over the blocked stop: the source is neither closed nor walked past.
func TestProcessWorkflowFinalizeKeepsLiveChildPrecedenceOverBlockedSource(t *testing.T) {
	t.Parallel()

	f := newSourceChainFinalizeFixture(t)
	mustStampSourceChainMetadata(t, f.rigStore, f.rigLaunch.ID, map[string]string{
		beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeBlocked,
	})
	mustCreateWorkflowBead(t, f.rigStore, beads.Bead{
		Title: "Live sibling workflow",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:         beadmeta.KindWorkflow,
			beadmeta.SourceBeadIDMetadataKey: f.rigLaunch.ID,
		},
	})

	opts, traceBuf := sourceChainTraceOptions(f)
	result, err := ProcessControl(f.rigStore, f.finalizer, opts)
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if !result.Processed || result.Action != "workflow-pass" {
		t.Fatalf("result = processed %t action %q, want processed workflow-pass", result.Processed, result.Action)
	}
	sourceAfter := mustGetBead(t, f.rigStore, f.rigLaunch.ID)
	if sourceAfter.Status == "closed" {
		t.Fatal("blocked source with a live child workflow closed; live-child precedence keeps it open")
	}
	parentAfter := mustGetBead(t, f.cityStore, f.citySource.ID)
	if parentAfter.Status == "closed" {
		t.Fatalf("parent of source with a live child closed with gc.outcome %q", parentAfter.Metadata[beadmeta.OutcomeMetadataKey])
	}
	traced := traceBuf.String()
	if !strings.Contains(traced, "stop reason=live_child_workflow source="+f.rigLaunch.ID) {
		t.Fatalf("trace missing live_child_workflow stop; got:\n%s", traced)
	}
	if strings.Contains(traced, "blocked_source") {
		t.Fatalf("trace shows a blocked_source stop for a source held by a live child; got:\n%s", traced)
	}
}
