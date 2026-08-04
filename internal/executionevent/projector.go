// Package executionevent projects authoritative graph execution facts from the
// current graph and work stores.
package executionevent

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/pkg/eventexport"
)

var (
	// ErrNotGraphV2Root means the selected bead is not an authoritative graph.v2
	// workflow root.
	ErrNotGraphV2Root = errors.New("executionevent: root is not a graph.v2 workflow")
	// ErrInvalidRootReference means the selected root cannot be represented as
	// an opaque execution run reference.
	ErrInvalidRootReference = errors.New("executionevent: invalid root reference")
	// ErrInvalidConvoyReference means gc.input_convoy_id is present but cannot be
	// represented as an opaque work reference.
	ErrInvalidConvoyReference = errors.New("executionevent: invalid input convoy reference")
)

// WorkAssociation relates one physical input work bead to an execution run.
type WorkAssociation struct {
	WorkBeadID     string
	ExecutionRunID string
}

// StepDefinition describes one physical execution-step occurrence. A nil
// DependsOnStepIDs means topology is unknown; a present empty slice identifies
// an authoritative root step.
type StepDefinition struct {
	BeadID           string
	ExecutionRunID   string
	StepID           string
	DependsOnStepIDs *[]string
}

// Projection is the deterministic current-store execution projection for one
// graph.v2 workflow root.
type Projection struct {
	WorkAssociations []WorkAssociation
	Steps            []StepDefinition
}

// EmitCurrent projects and records the current execution snapshot for rootID.
// A nil recorder disables emission without reading either store.
func EmitCurrent(recorder events.Recorder, graphStore beads.GraphStore, convoyStore beads.WorkStore, rootID, actor string) error {
	if recorder == nil {
		return nil
	}
	projection, err := ProjectCurrent(graphStore, convoyStore, rootID)
	if err != nil {
		return err
	}
	for _, event := range projection.Events(actor) {
		recorder.Record(event)
	}
	return nil
}

// Events converts the projection to repeatable snapshot facts. Work
// associations precede step definitions, preserving each slice's deterministic
// order. Topology is copied so later graph reads cannot mutate emitted facts.
func (p Projection) Events(actor string) []events.Event {
	result := make([]events.Event, 0, len(p.WorkAssociations)+len(p.Steps))
	for _, association := range p.WorkAssociations {
		result = append(result, events.Event{
			Type:    events.ExecutionWorkAssociated,
			Actor:   actor,
			Subject: association.WorkBeadID,
			RunID:   association.ExecutionRunID,
		})
	}
	for _, step := range p.Steps {
		result = append(result, events.Event{
			Type:             events.ExecutionStepDefined,
			Actor:            actor,
			Subject:          step.BeadID,
			RunID:            step.ExecutionRunID,
			StepID:           step.StepID,
			DependsOnStepIDs: cloneTopology(step.DependsOnStepIDs),
		})
	}
	return result
}

// ProjectCurrent projects current execution facts for rootID. The graph store
// exclusively owns the workflow root and physical steps. When the root names an
// input convoy, the supplied work store exclusively owns that convoy's tracks
// edges. A graph run without an input convoy is valid and projects only steps.
func ProjectCurrent(graphStore beads.GraphStore, convoyStore beads.WorkStore, rootID string) (Projection, error) {
	if graphStore.Store == nil {
		return Projection{}, fmt.Errorf("%w: nil graph store", ErrNotGraphV2Root)
	}
	if !eventexport.IsOpaqueRef(rootID) {
		return Projection{}, fmt.Errorf("%w: %q", ErrInvalidRootReference, rootID)
	}
	root, err := graphStore.Get(rootID)
	if err != nil {
		return Projection{}, fmt.Errorf("loading workflow root %q: %w", rootID, err)
	}
	if root.Metadata[beadmeta.KindMetadataKey] != beadmeta.KindWorkflow ||
		root.Metadata[beadmeta.FormulaContractMetadataKey] != beadmeta.FormulaContractGraphV2 {
		return Projection{}, ErrNotGraphV2Root
	}
	if !eventexport.IsOpaqueRef(root.ID) {
		return Projection{}, fmt.Errorf("%w: %q", ErrInvalidRootReference, root.ID)
	}

	steps, err := currentSteps(graphStore, root.ID)
	if err != nil {
		return Projection{}, err
	}
	convoyID := root.Metadata[beadmeta.InputConvoyIDMetadataKey]
	if convoyID == "" {
		return Projection{Steps: steps}, nil
	}
	work, err := currentWorkAssociations(convoyStore, root.ID, convoyID)
	if err != nil {
		return Projection{}, err
	}
	return Projection{WorkAssociations: work, Steps: steps}, nil
}

func currentWorkAssociations(store beads.WorkStore, rootID, convoyID string) ([]WorkAssociation, error) {
	if !eventexport.IsOpaqueRef(convoyID) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidConvoyReference, convoyID)
	}
	if store.Store == nil {
		return nil, fmt.Errorf("listing tracks membership for convoy %q: nil work store", convoyID)
	}
	dependencies, err := store.DepList(convoyID, "down")
	if err != nil {
		return nil, fmt.Errorf("listing tracks membership for convoy %q: %w", convoyID, err)
	}
	ids := make(map[string]struct{}, len(dependencies))
	for _, dependency := range dependencies {
		if dependency.Type != convoycore.TrackingDepType || dependency.IssueID != convoyID || !eventexport.IsOpaqueRef(dependency.DependsOnID) {
			continue
		}
		ids[dependency.DependsOnID] = struct{}{}
	}
	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)
	associations := make([]WorkAssociation, 0, len(sorted))
	for _, id := range sorted {
		associations = append(associations, WorkAssociation{WorkBeadID: id, ExecutionRunID: rootID})
	}
	return associations, nil
}

func currentSteps(store beads.GraphStore, rootID string) ([]StepDefinition, error) {
	rows, err := store.ListByMetadata(
		map[string]string{beadmeta.RootBeadIDMetadataKey: rootID},
		0,
		beads.IncludeClosed,
		beads.WithBothTiers,
	)
	if err != nil {
		return nil, fmt.Errorf("listing workflow steps for root %q: %w", rootID, err)
	}
	byID := make(map[string]beads.Bead, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	steps := make([]StepDefinition, 0, len(ids))
	for _, id := range ids {
		row := byID[id]
		if row.ID == rootID || !eventexport.IsOpaqueRef(row.ID) {
			continue
		}
		stepID := row.Metadata[beadmeta.StepIDMetadataKey]
		if !validNativeStepID(stepID) {
			continue
		}
		steps = append(steps, StepDefinition{
			BeadID:           row.ID,
			ExecutionRunID:   rootID,
			StepID:           stepID,
			DependsOnStepIDs: canonicalTopology(row.Metadata[beadmeta.NativeStepDependenciesMetadataKey], stepID),
		})
	}
	return steps, nil
}

func canonicalTopology(raw, stepID string) *[]string {
	if raw == "" || !validNativeStepID(stepID) {
		return nil
	}
	var dependencies []string
	if err := json.Unmarshal([]byte(raw), &dependencies); err != nil || dependencies == nil {
		return nil
	}
	previous := ""
	for _, dependency := range dependencies {
		if !validNativeStepID(dependency) || dependency == stepID || (previous != "" && dependency <= previous) {
			return nil
		}
		previous = dependency
	}
	canonical, err := json.Marshal(dependencies)
	if err != nil || string(canonical) != raw {
		return nil
	}
	return &dependencies
}

func validNativeStepID(id string) bool {
	return strings.TrimSpace(id) != "" && len(id) <= 256 && utf8.ValidString(id)
}

func cloneTopology(dependencies *[]string) *[]string {
	if dependencies == nil {
		return nil
	}
	clone := make([]string, len(*dependencies))
	copy(clone, *dependencies)
	return &clone
}
