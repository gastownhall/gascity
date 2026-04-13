package formula

import (
	"encoding/json"
	"fmt"
)

const sourceSpecKind = "spec"

func newSourceSpecStep(step *Step) (*Step, error) {
	if step == nil {
		return nil, fmt.Errorf("serializing step spec: missing step")
	}
	specJSON, err := json.Marshal(step)
	if err != nil {
		return nil, fmt.Errorf("serializing step spec for %q: %w", step.ID, err)
	}
	return &Step{
		ID:          step.ID + ".spec",
		Title:       "Step spec for " + step.Title,
		Type:        "spec",
		Description: string(specJSON),
		Metadata: map[string]string{
			"gc.kind":         sourceSpecKind,
			"gc.spec_for":     step.ID,
			"gc.spec_for_ref": step.ID,
		},
	}, nil
}

func isSourceSpecKind(kind string) bool {
	return kind == sourceSpecKind
}

func isSourceSpecStep(step *Step) bool {
	if step == nil {
		return false
	}
	return isSourceSpecKind(step.Metadata["gc.kind"])
}

func namespaceSourceSpecStep(step *Step, iterationID string) *Step {
	clone := cloneStep(step)
	clone.ID = iterationID + "." + step.ID
	clone.Children = nil
	clone.Ralph = nil
	clone.Retry = nil
	clone.DependsOn = nil
	clone.Needs = nil
	clone.WaitsFor = ""
	clone.Assignee = ""
	clone.Metadata = withMetadata(clone.Metadata, nil)
	for _, key := range []string{"gc.scope_ref", "gc.scope_role", "gc.on_fail", "gc.step_id", "gc.ralph_step_id", "gc.attempt", "gc.step_ref"} {
		delete(clone.Metadata, key)
	}
	if specForRef := step.Metadata["gc.spec_for_ref"]; specForRef != "" {
		clone.Metadata["gc.spec_for_ref"] = iterationID + "." + specForRef
	} else if specFor := step.Metadata["gc.spec_for"]; specFor != "" {
		clone.Metadata["gc.spec_for_ref"] = iterationID + "." + specFor
	}
	return clone
}
