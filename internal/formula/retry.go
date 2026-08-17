package formula

import (
	"fmt"
	"strconv"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// ApplyRetries expands inline retry-managed steps into control + attempt beads.
//
// A retry-managed step:
//   - keeps its original step ID as the control bead (gc.kind=retry)
//   - emits a first attempt: <step>.attempt.1
//
// The control bead blocks on the attempt. When the attempt closes, the
// controller re-activates the control bead to classify the outcome and
// optionally spawn the next attempt via molecule.Attach.
//
// Downstream steps continue to depend on the original logical step ID.
func ApplyRetries(steps []*Step) ([]*Step, error) {
	result := make([]*Step, 0, len(steps))

	for _, step := range steps {
		if step.Retry == nil {
			clone := cloneStep(step)
			if len(step.Children) > 0 {
				children, err := ApplyRetries(step.Children)
				if err != nil {
					return nil, err
				}
				clone.Children = children
			}
			result = append(result, clone)
			continue
		}

		expanded, err := expandRetry(step)
		if err != nil {
			return nil, err
		}
		result = append(result, expanded...)
	}

	return result, nil
}

func expandRetry(step *Step) ([]*Step, error) {
	if step.Retry == nil {
		return nil, fmt.Errorf("expanding retry: step %q missing retry spec", step.ID)
	}

	attempt := 1
	attemptID := fmt.Sprintf("%s.attempt.%d", step.ID, attempt)
	onExhausted := step.Retry.OnExhausted
	if onExhausted == "" {
		onExhausted = "hard_fail"
	}

	specStep, err := newSourceSpecStep(step)
	if err != nil {
		return nil, err
	}

	// Control bead — orchestrates retry attempts.
	control := cloneStep(step)
	control.Retry = nil
	control.Children = nil
	control.Assignee = ""
	control.Metadata = withMetadata(control.Metadata, map[string]string{
		beadmeta.KindMetadataKey:         beadmeta.KindRetry,
		beadmeta.StepIDMetadataKey:       step.ID,
		beadmeta.MaxAttemptsMetadataKey:  strconv.Itoa(step.Retry.MaxAttempts),
		beadmeta.OnExhaustedMetadataKey:  onExhausted,
		beadmeta.ControlEpochMetadataKey: "1",
	})
	if kind := step.Metadata[beadmeta.KindMetadataKey]; kind != "" {
		control.Metadata[beadmeta.OriginalKindMetadataKey] = kind
	}
	control.Needs = appendUniqueCopy(control.Needs, attemptID)
	control.WaitsFor = ""

	// First attempt — the actual work bead, tagged as attempt 1.
	run := cloneStep(step)
	run.ID = attemptID
	run.Retry = nil
	run.OnComplete = nil
	run.Children = nil
	run.Metadata = withMetadata(run.Metadata, map[string]string{
		beadmeta.AttemptMetadataKey: strconv.Itoa(attempt),
		beadmeta.StepIDMetadataKey:  step.ID,
		// gc.control_for records the durable lineage pointer back to the retry
		// control. At compile time no store bead ID exists yet, so the value is
		// the control's identity as known now (step.ID, which the control also
		// carries as gc.step_id). findLatestAttempt matches on this metadata
		// instead of parsing ref strings.
		beadmeta.ControlForMetadataKey: step.ID,
		// gc.step_ref is NOT set here — molecule.Instantiate fills it from
		// step.ID which includes the formula prefix (e.g., "mol.finalize.attempt.1"
		// instead of the bare "finalize.attempt.1").
	})
	if kind := step.Metadata[beadmeta.KindMetadataKey]; kind != "" {
		run.Metadata[beadmeta.OriginalKindMetadataKey] = kind
	}
	delete(run.Metadata, beadmeta.ScopeRefMetadataKey)
	delete(run.Metadata, beadmeta.ScopeRoleMetadataKey)
	delete(run.Metadata, beadmeta.OnFailMetadataKey)
	run.SourceLocation = fmt.Sprintf("%s.retry.attempt.%d", step.SourceLocation, attempt)

	return []*Step{control, specStep, run}, nil
}
