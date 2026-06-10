package formula

import (
	"encoding/json"
	"fmt"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

const sourceSpecKind = "spec"

func newSourceSpecStep(step *Step) (*Step, error) {
	if step == nil {
		return nil, fmt.Errorf("serializing step spec: missing step")
	}
	// Stored step snapshots intentionally preserve legacy JSON field names so
	// in-flight beads remain readable across mixed-version rollouts.
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
			beadmeta.KindMetadataKey:       sourceSpecKind,
			beadmeta.SpecForMetadataKey:    step.ID,
			beadmeta.SpecForRefMetadataKey: step.ID,
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
	return isSourceSpecKind(step.Metadata[beadmeta.KindMetadataKey])
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
	for _, key := range []string{beadmeta.ScopeRefMetadataKey, beadmeta.ScopeRoleMetadataKey, beadmeta.OnFailMetadataKey, beadmeta.StepIDMetadataKey, beadmeta.RalphStepIDMetadataKey, beadmeta.AttemptMetadataKey, beadmeta.StepRefMetadataKey} {
		delete(clone.Metadata, key)
	}
	if specForRef := step.Metadata[beadmeta.SpecForRefMetadataKey]; specForRef != "" {
		clone.Metadata[beadmeta.SpecForRefMetadataKey] = iterationID + "." + specForRef
	} else if specFor := step.Metadata[beadmeta.SpecForMetadataKey]; specFor != "" {
		clone.Metadata[beadmeta.SpecForRefMetadataKey] = iterationID + "." + specFor
	}
	return clone
}
