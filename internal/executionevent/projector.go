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

// stepRow pairs a projected step definition with the physical row it was
// decided from. Callers that need the step's Status or its full metadata read
// them here instead of re-Getting the bead: the ListByMetadata below already
// carried both, and a per-step Get made the completions reconcile cost
// O(roots x steps) sequential round trips against stores whose remote leg
// answers in seconds.
type stepRow struct {
	definition StepDefinition
	bead       beads.Bead
}

func currentSteps(store beads.GraphStore, rootID string) ([]StepDefinition, error) {
	rows, err := currentStepRows(store, rootID)
	if err != nil {
		return nil, err
	}
	steps := make([]StepDefinition, 0, len(rows))
	for _, row := range rows {
		steps = append(steps, row.definition)
	}
	return steps, nil
}

func currentStepRows(store beads.GraphStore, rootID string) ([]stepRow, error) {
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
	steps := make([]stepRow, 0, len(ids))
	for _, id := range ids {
		row := byID[id]
		if row.ID == rootID || !eventexport.IsOpaqueRef(row.ID) {
			continue
		}
		stepID := row.Metadata[beadmeta.StepIDMetadataKey]
		if !validNativeStepID(stepID) {
			continue
		}
		steps = append(steps, stepRow{
			definition: StepDefinition{
				BeadID:           row.ID,
				ExecutionRunID:   rootID,
				StepID:           stepID,
				DependsOnStepIDs: canonicalTopology(row.Metadata[beadmeta.NativeStepDependenciesMetadataKey], stepID),
			},
			bead: row,
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

	// One unbounded chunk IS the whole sweep, so the startup pass and the
	// background lane's chunks cannot drift apart in what they visit or how they
	// decide it.
	return (&CompletionBackstop{}).Pass(recorder, graphStores, actor).Emitted
}

// ReconcileCompletedRoots is the DELTA form of ReconcileCompletedStores: it
// repairs completion facts for the named roots only.
//
// The full pass walks every workflow root ever created, closed ones included,
// against a corpus that only grows — 72.4s +/- 0.9s of a ~360s controller tick
// on maintainer-city (ga-l7jdg). Only a root something happened to since the
// last pass can have a stranded close, and the journal already names those: the
// RunID of an execution.step_* fact, and the gc.root_bead_id of a bead.closed
// step snapshot. The caller decodes them; this projects them.
//
// With no named roots it reads NOTHING — not the stores, not the journal. That
// is the steady tick, and it is the whole point: the journal read alone is
// O(retained history).
//
// It is the delta half of a two-lane doctrine, never a replacement. A close can
// exist with no event naming it — a controller can crash between the durable
// step close and the best-effort append, and graph stores emit no bead.closed by
// design — so the full pass remains the convergence backstop.
func ReconcileCompletedRoots(recorder events.Provider, graphStores []beads.GraphStore, rootIDs []string, actor string) int {
	if recorder == nil || len(rootIDs) == 0 {
		return 0
	}
	completed, ok := loadCompletedFactIndex(recorder)
	if !ok {
		return 0
	}
	emitted := 0
	for _, graphStore := range graphStores {
		if graphStore.Store == nil {
			continue
		}
		// One batched read for every named root this store might hold, rather
		// than one Get per root per store.
		roots, err := graphStore.List(beads.ListQuery{
			IDs:           rootIDs,
			IncludeClosed: true,
			TierMode:      beads.TierBoth,
		})
		if err != nil {
			continue
		}
		sort.Slice(roots, func(i, j int) bool { return roots[i].ID < roots[j].ID })
		emitted += reconcileRoots(recorder, graphStore, roots, completed, actor)
	}
	return emitted
}

// CompletionBackstop is the chunked, resumable form of the full pass.
//
// The full sweep is minutes of sequential reads on a large city, so the
// background lane runs it a chunk at a time and RESUMES: a pass that is cut
// short leaves a cursor, and the next one continues from it instead of
// restarting at the first root. Without that, a corpus larger than one budget
// starves its own convergence — it would forever re-walk the same prefix.
//
// The cursor is free because the pass already sorts roots. A sweep ends when the
// last store's last root is visited, and the next Pass starts a fresh one: this
// is a convergence scan, not a one-shot migration.
type CompletionBackstop struct {
	// ChunkSize caps the roots one Pass visits. Zero means the whole sweep.
	ChunkSize int

	storeIndex  int
	afterRootID string
}

// CompletionBackstopResult is one chunk's outcome.
type CompletionBackstopResult struct {
	Emitted      int
	RootsVisited int
	// SweepComplete reports that this Pass finished a full traversal, so the
	// cursor has wrapped and the next Pass begins a new sweep.
	SweepComplete bool
	// ListErrors names the stores whose root list this chunk could not read. A
	// store that cannot be listed is silently skipped by the traversal so one
	// dark store does not stall the sweep — which is correct, and is exactly why
	// it has to be REPORTED: a convergence lane that quietly converges nothing
	// is indistinguishable from one with nothing to do.
	ListErrors []error
}

// Pass visits at most ChunkSize roots, resuming from the last Pass's cursor.
func (b *CompletionBackstop) Pass(recorder events.Provider, graphStores []beads.GraphStore, actor string) CompletionBackstopResult {
	var result CompletionBackstopResult
	if recorder == nil || len(graphStores) == 0 {
		result.SweepComplete = true
		return result
	}
	completed, ok := loadCompletedFactIndex(recorder)
	if !ok {
		return result
	}
	for b.storeIndex < len(graphStores) {
		if b.ChunkSize > 0 && result.RootsVisited >= b.ChunkSize {
			return result
		}
		graphStore := graphStores[b.storeIndex]
		if graphStore.Store == nil {
			b.storeIndex++
			b.afterRootID = ""
			continue
		}
		roots, err := graphStore.ListByMetadata(
			map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
			0,
			beads.IncludeClosed,
			beads.WithBothTiers,
		)
		if err != nil {
			// A store that cannot be listed does not stall the sweep; the next
			// sweep retries it. The caller is told, so a lane converging nothing
			// cannot look like a lane with nothing to converge.
			result.ListErrors = append(result.ListErrors, fmt.Errorf("listing workflow roots in graph store %d: %w", b.storeIndex, err))
			b.storeIndex++
			b.afterRootID = ""
			continue
		}
		sort.Slice(roots, func(i, j int) bool { return roots[i].ID < roots[j].ID })
		// Resume strictly after the last root this cursor visited. The list is
		// re-read each Pass, so a root created mid-sweep before the cursor is
		// picked up by the NEXT sweep rather than being skipped forever.
		remaining := roots
		for len(remaining) > 0 && b.afterRootID != "" && remaining[0].ID <= b.afterRootID {
			remaining = remaining[1:]
		}
		budget := len(remaining)
		if b.ChunkSize > 0 {
			if left := b.ChunkSize - result.RootsVisited; left < budget {
				budget = left
			}
		}
		chunk := remaining[:budget]
		result.Emitted += reconcileRoots(recorder, graphStore, chunk, completed, actor)
		result.RootsVisited += len(chunk)
		if len(chunk) > 0 {
			b.afterRootID = chunk[len(chunk)-1].ID
		}
		if len(chunk) == len(remaining) {
			b.storeIndex++
			b.afterRootID = ""
		}
	}
	b.storeIndex = 0
	b.afterRootID = ""
	result.SweepComplete = true
	return result
}

// reconcileRoots projects the closed steps of the supplied roots and records the
// completion facts the journal is missing. completed is updated as it goes so
// one pass cannot emit the same fact twice across stores.
func reconcileRoots(recorder events.Recorder, graphStore beads.GraphStore, roots []beads.Bead, completed map[completedFactKey]struct{}, actor string) int {
	emitted := 0
	for _, root := range roots {
		if root.Metadata[beadmeta.KindMetadataKey] != beadmeta.KindWorkflow ||
			root.Metadata[beadmeta.FormulaContractMetadataKey] != beadmeta.FormulaContractGraphV2 {
			continue
		}
		rows, err := currentStepRows(graphStore, root.ID)
		if err != nil {
			continue
		}
		for _, row := range rows {
			// The row the steps List already returned decides the status.
			// Re-Getting it would only narrow a window the journal-keyed
			// idempotency record already covers: a step that closes between
			// the List and the write is repaired by the next pass.
			step := row.bead
			if !strings.EqualFold(strings.TrimSpace(step.Status), "closed") {
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
	return emitted
}

// loadCompletedFactIndex reads the retained completion journal into the exact-fact
// idempotency set. A journal that cannot be read reports !ok: emitting without it
// would duplicate recovery facts, and a later pass can safely retry.
func loadCompletedFactIndex(recorder events.Provider) (map[completedFactKey]struct{}, bool) {
	existing, err := completedFacts(recorder)
	if err != nil {
		return nil, false
	}
	completed := make(map[completedFactKey]struct{}, len(existing))
	for _, event := range existing {
		if event.Type == events.ExecutionStepCompleted {
			completed[completedFactKeyFor(event)] = struct{}{}
		}
	}
	return completed, true
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
