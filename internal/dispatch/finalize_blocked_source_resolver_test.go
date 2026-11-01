package dispatch

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestProcessWorkflowFinalizeDoesNotResolveStoresAboveBlockedSource pins the
// preflight half of the blocked-source stop: a blocked source's parent store
// is never resolved, so a store that cannot be opened above a blocked source
// no longer strands the finalizer pending.
func TestProcessWorkflowFinalizeDoesNotResolveStoresAboveBlockedSource(t *testing.T) {
	t.Parallel()

	f := newSourceChainFinalizeFixture(t)
	if err := f.rigStore.Close(f.rigLaunch.ID); err != nil {
		t.Fatalf("closing blocked source: %v", err)
	}
	mustStampSourceChainMetadata(t, f.rigStore, f.rigLaunch.ID, map[string]string{
		beadmeta.OutcomeMetadataKey:     beadmeta.OutcomePass,
		beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeBlocked,
	})

	var resolved []string
	resolver := func(ref string) (beads.Store, error) {
		resolved = append(resolved, ref)
		if ref == "city:test" {
			return nil, errors.New("city store unavailable")
		}
		return f.resolver(ref)
	}

	var traced strings.Builder
	result, err := ProcessControl(f.rigStore, f.finalizer, ProcessOptions{
		ResolveStoreRef: resolver,
		Tracef: func(format string, args ...any) {
			fmt.Fprintf(&traced, format, args...)
			traced.WriteByte('\n')
		},
	})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize) err = %v, want pass: a store above a blocked source is never resolved", err)
	}
	if !result.Processed || result.Action != "workflow-pass" {
		t.Fatalf("result = processed %t action %q, want processed workflow-pass", result.Processed, result.Action)
	}
	finalizerAfter := mustGetBead(t, f.rigStore, f.finalizer.ID)
	if finalizerAfter.Status != "closed" {
		t.Fatalf("finalizer status = %q, want closed", finalizerAfter.Status)
	}
	if slices.Contains(resolved, "city:test") {
		t.Fatalf("resolver asked for city:test (refs %v); the walk must stop at the blocked source before resolving its parent store", resolved)
	}
	if parentAfter := mustGetBead(t, f.cityStore, f.citySource.ID); parentAfter.Status == "closed" {
		t.Fatalf("parent of blocked source closed with gc.outcome %q", parentAfter.Metadata[beadmeta.OutcomeMetadataKey])
	}
	if !strings.Contains(traced.String(), "stop reason=blocked_source source="+f.rigLaunch.ID) {
		t.Fatalf("trace missing blocked_source stop; got:\n%s", traced.String())
	}
}
