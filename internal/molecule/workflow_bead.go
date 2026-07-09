package molecule

import (
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// WorkflowBead is the typed projection of a workflow root or step bead: the
// derived status/kind/attempt plus the gc.* metadata a snapshot presentation
// consumes, read once through a confined codec instead of cracking the raw
// bead inline at every call site.
//
// It is the workflow-domain analog of session.InfoFromPersistedBead's Info:
// molecule is the package that materializes a formula run as a root bead plus
// child step beads, so it owns what a workflow bead means. WorkflowBeadFromBead
// is pure, side-effect-free, and backend-invariant — it reads only stored bead
// fields, so a bead round-trips to the same WorkflowBead whether it was persisted
// to bd, sqlite, or postgres.
//
// Not to be confused with internal/runproj.toRunSnapshotBead (detail.go) and
// internal/runproj.fromBead (summary.go). Those are a deliberately DIFFERENT
// projection: a byte-parity port of the TS golden run-view generator that reads
// the raw bead status (not this file's derived pending/active/completed/failed/
// skipped vocabulary), uses b.Ref for the step ref (not gc.step_ref), and honors
// gc.original_kind over b.Type (not gc.kind). Those semantics are locked by
// detail_golden_test.go / detail_parity_test.go. Do NOT merge the two codecs:
// unifying them would break golden parity or force a dual-mode codec.
type WorkflowBead struct {
	// ID and Title mirror the bead's identity fields verbatim.
	ID    string
	Title string
	// Status is the derived presentation status (see WorkflowStatus).
	Status string
	// Kind is the workflow kind (see WorkflowKind).
	Kind string
	// StepRef is the trimmed gc.step_ref metadata.
	StepRef string
	// Attempt is the parsed gc.attempt metadata (0 when unset or unparseable).
	Attempt int
	// LogicalBeadID is the trimmed gc.logical_bead_id metadata.
	LogicalBeadID string
	// ScopeRef is the trimmed gc.scope_ref metadata.
	ScopeRef string
	// Assignee is the trimmed bead assignee.
	Assignee string
	// Metadata is an independent clone of the bead metadata. A nil source map
	// stays nil so the wire keeps emitting "metadata": null for nil-metadata
	// beads.
	Metadata map[string]string
}

// WorkflowBeadFromBead projects a workflow root or step bead onto WorkflowBead.
// It composes WorkflowStatus/WorkflowKind/WorkflowAttempt and trims the gc.*
// metadata scalars, cloning the metadata map so callers never share the bead's
// backing storage. See WorkflowBead for the purity and runproj-divergence notes.
func WorkflowBeadFromBead(b beads.Bead) WorkflowBead {
	return WorkflowBead{
		ID:            b.ID,
		Title:         b.Title,
		Status:        WorkflowStatus(b),
		Kind:          WorkflowKind(b),
		StepRef:       strings.TrimSpace(b.Metadata[beadmeta.StepRefMetadataKey]),
		Attempt:       WorkflowAttempt(b),
		LogicalBeadID: strings.TrimSpace(b.Metadata[beadmeta.LogicalBeadIDMetadataKey]),
		ScopeRef:      strings.TrimSpace(b.Metadata[beadmeta.ScopeRefMetadataKey]),
		Assignee:      strings.TrimSpace(b.Assignee),
		Metadata:      cloneMetadata(b.Metadata),
	}
}

// WorkflowStatus derives a workflow bead's presentation status from its bead
// status and gc.outcome metadata: closed+fail -> "failed", closed+skipped ->
// "skipped", closed -> "completed", in_progress with an assignee -> "active",
// in_progress or open -> "pending". Any other raw status honors gc.outcome
// (fail/skipped) and otherwise passes through trimmed. It is exported separately
// so hot loops can derive status without paying the full-projection metadata
// clone.
func WorkflowStatus(b beads.Bead) string {
	outcome := strings.TrimSpace(b.Metadata[beadmeta.OutcomeMetadataKey])
	hasAssignment := strings.TrimSpace(b.Assignee) != ""
	switch strings.TrimSpace(b.Status) {
	case "closed":
		switch outcome {
		case beadmeta.OutcomeFail:
			return "failed"
		case beadmeta.OutcomeSkipped:
			return "skipped"
		}
		return "completed"
	case "in_progress":
		if hasAssignment {
			return "active"
		}
		return "pending"
	case "open":
		return "pending"
	default:
		switch outcome {
		case beadmeta.OutcomeFail:
			return "failed"
		case beadmeta.OutcomeSkipped:
			return "skipped"
		}
		return strings.TrimSpace(b.Status)
	}
}

// WorkflowKind returns the workflow kind: the trimmed gc.kind metadata when
// present, falling back to the trimmed bead Type.
func WorkflowKind(b beads.Bead) string {
	if b.Metadata != nil {
		if kind := strings.TrimSpace(b.Metadata[beadmeta.KindMetadataKey]); kind != "" {
			return kind
		}
	}
	return strings.TrimSpace(b.Type)
}

// WorkflowAttempt returns the parsed gc.attempt metadata as an int, or 0 when
// the metadata is empty or non-numeric. The API mapper converts 0 to the wire's
// omitted *int.
func WorkflowAttempt(b beads.Bead) int {
	raw := strings.TrimSpace(b.Metadata[beadmeta.AttemptMetadataKey])
	if raw == "" {
		return 0
	}
	v, _ := strconv.Atoi(raw)
	return v
}

// cloneMetadata returns an independent copy of a metadata map, preserving
// nil -> nil so a nil-metadata bead projects to nil metadata (and the wire keeps
// emitting "metadata": null). Port of api.cloneStringMap.
func cloneMetadata(src map[string]string) map[string]string {
	if src == nil {
		return nil
	}
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
