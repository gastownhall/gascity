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

// LifecycleEvent constructs a lifecycle fact only for a physical native step
// of the supplied authoritative graph.v2 root. It is shared by claim and close
// notification producers so the event contract cannot drift between them.
func LifecycleEvent(eventType string, root, step beads.Bead, actor string) (events.Event, bool) {
	if eventType != events.ExecutionStepStarted && eventType != events.ExecutionStepCompleted {
		return events.Event{}, false
	}
	if root.Metadata[beadmeta.KindMetadataKey] != beadmeta.KindWorkflow ||
		root.Metadata[beadmeta.FormulaContractMetadataKey] != beadmeta.FormulaContractGraphV2 ||
		!eventexport.IsOpaqueRef(root.ID) || !eventexport.IsOpaqueRef(step.ID) ||
		step.Metadata[beadmeta.RootBeadIDMetadataKey] != root.ID ||
		beadmeta.IsControlKind(strings.TrimSpace(step.Metadata[beadmeta.KindMetadataKey])) {
		return events.Event{}, false
	}
	stepID := step.Metadata[beadmeta.StepIDMetadataKey]
	sessionID := step.Metadata[beadmeta.SessionIDMetadataKey]
	if !validNativeStepID(stepID) || !eventexport.IsOpaqueRef(sessionID) {
		return events.Event{}, false
	}
	return events.Event{
		Type: eventType, Actor: actor, Subject: step.ID, RunID: root.ID,
		SessionID: sessionID, StepID: stepID,
		DependsOnStepIDs: canonicalTopology(step.Metadata[beadmeta.NativeStepDependenciesMetadataKey], stepID),
	}, true
}

// EmitLifecycle records a validated lifecycle fact for a graph.v2 step. The
// root is loaded from graphStore so a v1 or unrelated parent can never produce
// a lifecycle event by metadata resemblance alone.
func EmitLifecycle(recorder events.Recorder, graphStore beads.Store, eventType string, step beads.Bead, actor string) bool {
	if recorder == nil || graphStore == nil {
		return false
	}
	rootID := step.Metadata[beadmeta.RootBeadIDMetadataKey]
	if !eventexport.IsOpaqueRef(rootID) {
		return false
	}
	root, err := graphStore.Get(rootID)
	if err != nil {
		return false
	}
	event, ok := LifecycleEvent(eventType, root, step, actor)
	if !ok {
		return false
	}
	recorder.Record(event)
	return true
}

// EmitCompletedFromClosedNotification is the sole close-side lifecycle entry
// point. It consumes the physical bead snapshot carried by the authoritative
// bead.closed notification rather than inferring completion from dependencies
// or re-projecting current graph state.
func EmitCompletedFromClosedNotification(recorder events.Recorder, graphStore beads.Store, payload json.RawMessage, actor string) bool {
	step, ok := beads.DecodeBeadEventPayload(payload)
	if !ok || !strings.EqualFold(strings.TrimSpace(step.Status), "closed") {
		return false
	}
	return EmitLifecycle(recorder, graphStore, events.ExecutionStepCompleted, step, actor)
}

// ReconcileCompleted repairs completed facts that were stranded between a
// durable graph-step close and the best-effort event append. It projects only
// closed physical steps of authoritative graph.v2 roots, and uses the event
// journal as the durable idempotency record: an exact lifecycle fact is not
// repeated, while a conflicting historical fact remains visible alongside the
// newly projected correction.
func ReconcileCompleted(recorder events.Provider, graphStore beads.GraphStore, actor string) int {
	return ReconcileCompletedStores(recorder, []beads.GraphStore{graphStore}, actor)
}

// ReconcileCompletedStores repairs completion facts across graph stores with
// one journal read. The completed-fact index is updated after each append so
// the pass remains idempotent even when more than one source is scanned.
func ReconcileCompletedStores(recorder events.Provider, graphStores []beads.GraphStore, actor string) int {
	if recorder == nil {
		return 0
	}
	hasStore := false
	for _, graphStore := range graphStores {
		if graphStore.Store != nil {
			hasStore = true
			break
		}
	}
	if !hasStore {
		return 0
	}

	existing, err := completedFacts(recorder)
	if err != nil {
		// If the journal cannot be read, avoid generating duplicate recovery
		// facts. A later reconciliation pass can safely retry.
		return 0
	}
	completed := make(map[completedFactKey]struct{}, len(existing))
	for _, event := range existing {
		if event.Type == events.ExecutionStepCompleted {
			completed[completedFactKeyFor(event)] = struct{}{}
		}
	}

	emitted := 0
	for _, graphStore := range graphStores {
		if graphStore.Store == nil {
			continue
		}
		roots, err := graphStore.ListByMetadata(
			map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
			0,
			beads.IncludeClosed,
			beads.WithBothTiers,
		)
		if err != nil {
			continue
		}
		sort.Slice(roots, func(i, j int) bool { return roots[i].ID < roots[j].ID })
		for _, root := range roots {
			if root.Metadata[beadmeta.FormulaContractMetadataKey] != beadmeta.FormulaContractGraphV2 {
				continue
			}
			definitions, err := currentSteps(graphStore, root.ID)
			if err != nil {
				continue
			}
			for _, definition := range definitions {
				step, err := graphStore.Get(definition.BeadID)
				if err != nil || !strings.EqualFold(strings.TrimSpace(step.Status), "closed") {
					continue
				}
				event, ok := LifecycleEvent(events.ExecutionStepCompleted, root, step, actor)
				if !ok {
					continue
				}
				key := completedFactKeyFor(event)
				if _, exists := completed[key]; exists {
					continue
				}
				recorder.Record(event)
				completed[key] = struct{}{}
				emitted++
			}
		}
	}
	return emitted
}

// completedFacts returns the retained completion journal, including a
// FileRecorder segment that is temporarily awaiting archive compression. A
// reconciliation pass must see that segment before deciding a close needs a
// recovery fact; otherwise an event rotation can create a duplicate fact.
func completedFacts(recorder events.Provider) ([]events.Event, error) {
	filter := events.Filter{Type: events.ExecutionStepCompleted}
	if inFlight, ok := recorder.(events.InFlightProvider); ok {
		return inFlight.ListInFlight(filter)
	}
	return recorder.List(filter)
}

type completedFactKey struct {
	subject           string
	runID             string
	sessionID         string
	stepID            string
	topologyKnown     bool
	topologyCanonical string
}

func completedFactKeyFor(event events.Event) completedFactKey {
	key := completedFactKey{
		subject:   event.Subject,
		runID:     event.RunID,
		sessionID: event.SessionID,
		stepID:    event.StepID,
	}
	if event.DependsOnStepIDs != nil {
		key.topologyKnown = true
		if len(*event.DependsOnStepIDs) == 0 {
			key.topologyCanonical = "[]"
			return key
		}
		topology, _ := json.Marshal(*event.DependsOnStepIDs)
		key.topologyCanonical = string(topology)
	}
	return key
}
