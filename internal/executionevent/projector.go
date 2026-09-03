// Package executionevent projects authoritative graph execution facts from the
// current graph and work stores.
package executionevent

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
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

// RunAnchor relates a source work bead to an execution run through the
// authoritative generic source chain. It is distinct from WorkAssociation:
// the latter continues to identify the physical rig launch that entered the
// input convoy.
type RunAnchor struct {
	SourceBeadID   string
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
	RunAnchors       []RunAnchor
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
// associations precede source anchors and step definitions, preserving each
// slice's deterministic order. Topology is copied so later graph reads cannot
// mutate emitted facts.
func (p Projection) Events(actor string) []events.Event {
	result := make([]events.Event, 0, len(p.WorkAssociations)+len(p.RunAnchors)+len(p.Steps))
	for _, association := range p.WorkAssociations {
		result = append(result, events.Event{
			Type:    events.ExecutionWorkAssociated,
			Actor:   actor,
			Subject: association.WorkBeadID,
			RunID:   association.ExecutionRunID,
		})
	}
	for _, anchor := range p.RunAnchors {
		result = append(result, events.Event{
			Type:    events.ExecutionRunAnchored,
			Actor:   actor,
			Subject: anchor.SourceBeadID,
			RunID:   anchor.ExecutionRunID,
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
	anchors := currentRunAnchors(convoyStore, root, work)
	return Projection{WorkAssociations: work, RunAnchors: anchors, Steps: steps}, nil
}

// currentRunAnchors follows the exact generic source chain from each
// authoritative input work association to its launch bead and then to that
// launch's source work bead. A root source link, when present, must identify
// one of those launches. It deliberately does not infer identity from other
// work, sessions, provider identifiers, or wrapper metadata. Missing,
// unreadable, non-exact, or ambiguous links produce no anchor while preserving
// the rest of the execution snapshot.
func currentRunAnchors(store beads.WorkStore, root beads.Bead, work []WorkAssociation) []RunAnchor {
	if len(work) == 0 || store.Store == nil {
		return nil
	}
	declaredLaunchID := root.Metadata[beadmeta.SourceBeadIDMetadataKey]
	if declaredLaunchID != "" && !eventexport.IsOpaqueRef(declaredLaunchID) {
		return nil
	}
	anchors := make([]RunAnchor, 0, len(work))
	seenSources := make(map[string]string, len(work))
	declaredMatched := false
	for _, association := range work {
		launchID := association.WorkBeadID
		if !eventexport.IsOpaqueRef(launchID) {
			continue
		}
		launch, err := store.Get(launchID)
		if err != nil || launch.ID != launchID {
			continue
		}
		if launchID == declaredLaunchID {
			declaredMatched = true
		}
		sourceID := launch.Metadata[beadmeta.SourceBeadIDMetadataKey]
		if !eventexport.IsOpaqueRef(sourceID) {
			continue
		}
		if prior, ok := seenSources[sourceID]; ok && prior != launchID {
			return nil
		}
		seenSources[sourceID] = launchID
		anchors = append(anchors, RunAnchor{SourceBeadID: sourceID, ExecutionRunID: root.ID})
	}
	if (declaredLaunchID != "" && !declaredMatched) || len(anchors) == 0 {
		return nil
	}
	sort.Slice(anchors, func(i, j int) bool { return anchors[i].SourceBeadID < anchors[j].SourceBeadID })
	return anchors
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

// ReconcileCompletedRoots is the DELTA form of ReconcileCompletedStores over a
// COLD fact index: it repairs completion facts for the named roots only, and
// pays the full journal read to build its idempotency record.
//
// It is the one-shot entry point. A caller on the tick holds a
// [CompletedFactIndex] and calls [CompletedFactIndex.ReconcileRoots] instead, so
// the journal read is paid once per process rather than once per pass.
func ReconcileCompletedRoots(recorder events.Provider, graphStores []beads.GraphStore, rootIDs []string, actor string) int {
	return (&CompletedFactIndex{}).ReconcileRoots(recorder, graphStores, rootIDs, actor)
}

// completedFactIndexGrowthCap bounds how far the set may grow BEYOND the size
// its last journal load produced.
//
// The index holds one key per completion fact it has seen and a controller runs
// for weeks, so unbounded it is a leak rather than a cache. The bound is on
// GROWTH, not on absolute size, and that distinction is load-bearing: a city
// whose journal already retains more facts than any absolute cap would rebuild
// on every single pass, which is exactly the O(retained-history) read per tick
// this type exists to delete. Rebuilding resets the baseline, so each rebuild
// buys another cap's worth of headroom.
//
// A rebuild sees exactly what the retained journal holds — the same set every
// pass saw before this index existed — so anything it forgets is at worst one
// restated recovery fact, never a lost repair.
const completedFactIndexGrowthCap = 50000

// CompletedFactIndex is the journal-derived idempotency record for completion
// facts, held ACROSS passes instead of rebuilt on each one.
//
// # Why it exists
//
// ReconcileCompletedRoots was written to read nothing on a steady tick, and it
// does — but only when the journal names NO root. Maintainer-city names 1-2 on
// every tick, so the pass cleared its early return and rebuilt this set from the
// journal every time: 69.7s of a 373s tick, flat, and independent of how many
// roots were named (ga-l7jdg). The cost is not the roots, it is the read behind
// them — [events.Provider.List] gunzips and scans every retained archive, and no
// seq filter avoids that on the active log.
//
// # How it stays warm
//
// Load once, then two feeds keep it current and neither of them reads:
//
//   - facts the pass itself records are added as it records them, exactly as the
//     per-pass map already did, so one pass cannot repeat its own fact;
//   - facts appended by anything else arrive through [CompletedFactIndex.Absorb],
//     called from the journal feed the delta lane already tails. That feed IS the
//     cursor: it delivers every event in seq order and calls its gap hook on every
//     way it can stop being able to promise that.
//
// # What a gap means here
//
// [CompletedFactIndex.Invalidate] drops the set so the next pass reloads. It is
// wired to the same gap hook that forces the convergence sweep, because a missed
// fact has the same cause and the opposite cost: it does not strand a repair, it
// risks a DUPLICATE recovery fact. Reloading is cheap insurance against that and
// happens only when the feed says it cannot promise completeness.
//
// Rotation is deliberately not a gap. It only removes history from the read path,
// so reloading on it would FORGET facts and start re-emitting them; the watcher
// spans rotation and keeps delivering.
//
// # Ownership
//
// One index belongs to one lane, and the lock is what lets the journal feed write
// to it from its own goroutine while the tick reads. It is held per key lookup,
// never across a store read.
type CompletedFactIndex struct {
	mu sync.Mutex
	// facts maps each seen completion fact to whether a JOURNAL READ has
	// confirmed it (true) or this process only recorded it itself and has not
	// yet read it back (false, "unconfirmed"). The distinction is load-bearing
	// for the convergence stamp: [events.Recorder.Record] is best-effort and can
	// drop silently, so a key this process add()ed is proof it TRIED to emit, not
	// proof the journal holds the fact. Dedup keys on PRESENCE (either state);
	// only a confirmed key witnesses convergence. warm() drops the unconfirmed
	// ones on every re-derivation, so a dropped Record is re-emitted next pass
	// rather than masked as converged forever.
	facts  map[completedFactKey]bool
	loaded bool
	// baseline is len(facts) as the last FULL journal load (a cold read or a
	// rebuild) left it. Growth past it by completedFactIndexGrowthCap forces the
	// next rebuild; a tail merge extends the set without moving the baseline, so
	// growth accumulates toward the cap instead of re-flooring every pass. See
	// that const for why the bound is relative and not absolute.
	baseline int
	// maxSeq is the journal high-water of the last successful load. Journal
	// seqs are monotonic and a fact once read cannot change, so every re-read
	// resumes at this floor instead of byte 0 — the full-history form of the
	// re-read (every archive gunzipped plus the whole active journal, per
	// retry, per city) was the primary supervisor I/O burn (ga-ftgyl). It
	// survives Invalidate and warm errors by design: only what was READ is
	// behind it, so resuming from it can never skip an unseen fact.
	maxSeq uint64
}

// Absorb records a completion fact the journal named, so the next pass does not
// re-emit it. Events of any other type are ignored, and an index that has not
// loaded yet ignores everything: its load reads the journal these events are in.
func (idx *CompletedFactIndex) Absorb(event events.Event) {
	if event.Type != events.ExecutionStepCompleted {
		return
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if !idx.loaded {
		return
	}
	// The feed reads from the journal these facts were appended to, so an
	// absorbed fact is journal-confirmed: it witnesses convergence and upgrades
	// an earlier unconfirmed add() of the same key.
	idx.facts[completedFactKeyFor(event)] = true
}

// Invalidate marks the set stale so the next pass re-derives from the journal
// what other writers recorded meanwhile. Call it when the event feed can no
// longer promise to name every fact.
//
// It keeps the facts and the high-water: everything below maxSeq was READ from
// the journal and cannot change (seqs are monotonic, facts are appends), so the
// re-derivation only needs the tail. Dropping the set here is what made every
// sweep start a full-history read (ga-ftgyl).
func (idx *CompletedFactIndex) Invalidate() {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.loaded = false
}

// ReconcileRoots repairs completion facts for the named roots, reusing this
// index's warm idempotency record.
//
// With no named roots it reads NOTHING — not the stores, not the journal. That is
// the steady tick. With roots named it reads the graph stores for those roots and
// the journal not at all, because the feed has kept the record current.
//
// It is the delta half of a two-lane doctrine, never a replacement. A close can
// exist with no event naming it — a controller can crash between the durable step
// close and the best-effort append, and graph stores emit no bead.closed by
// design — so the full pass remains the convergence backstop.
func (idx *CompletedFactIndex) ReconcileRoots(recorder events.Provider, graphStores []beads.GraphStore, rootIDs []string, actor string) int {
	if recorder == nil || len(rootIDs) == 0 {
		return 0
	}
	if !idx.warm(recorder) {
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
		emitted += reconcileRoots(recorder, graphStore, roots, idx, actor)
	}
	return emitted
}

// warm loads the set from the journal when it is cold and reports whether the
// index can be used. A journal that cannot be read reports false: emitting
// without the record would duplicate recovery facts, and a later pass retries.
func (idx *CompletedFactIndex) warm(recorder events.Provider) bool {
	idx.mu.Lock()
	// Growth past the load baseline forces a REBUILD, not another tail merge: the
	// set has accumulated more keys than the journal still backs, so re-read it
	// whole and let it shrink to what retention holds. See the cap const for why
	// the bound is on growth and not absolute size.
	//
	// The cap is checked on GROWTH alone, NOT gated on loaded: the convergence
	// sweep Invalidate()s this index (loaded=false) immediately before every warm,
	// and a city whose sweep fits in one chunk reaches warm() only in that cold
	// state. Gating the rebuild on loaded there makes it structurally unreachable,
	// so the set grows without bound over the controller's lifetime — the very
	// leak the cap exists to stop. Below the cap, a cold index still just
	// tail-merges (the early return only skips work for a loaded one).
	rebuild := len(idx.facts) > idx.baseline+completedFactIndexGrowthCap
	if idx.loaded && !rebuild {
		idx.mu.Unlock()
		return true
	}
	afterSeq := idx.maxSeq
	if rebuild {
		afterSeq = 0
	}
	// A full load — a cold first read or a rebuild — reads the whole retained
	// journal (AfterSeq 0) and so redefines the baseline the growth cap measures
	// from. A tail merge (AfterSeq at the high-water) only extends the set, so it
	// must NOT re-floor the baseline, or growth never accumulates toward the cap
	// and the rebuild never fires on a lane that only ever tail-merges.
	fullLoad := afterSeq == 0
	idx.mu.Unlock()

	// Read outside the lock: this is the expensive call, and holding the lock
	// across it would block the journal feed for the length of a full archive
	// walk. A fact appended during the read can therefore miss both this read and
	// the concurrent Absorb, and be re-emitted once as a duplicate recovery fact.
	// That window is the pre-index behavior exactly — every pass read the journal
	// and then decided — so it is not new, and the emitted fact is a correct
	// restatement of a real close rather than a wrong one.
	//
	// The read resumes at the high-water: a cold index reads everything once, a
	// warm or invalidated one reads only the tail, and the merge below is what
	// keeps the earlier reads' facts. AfterSeq also lets the reader skip whole
	// archives by their seq window instead of gunzipping them. A rebuild resets
	// the high-water to 0 to re-read the retained journal in full.
	existing, err := completedFacts(recorder, events.Filter{Type: events.ExecutionStepCompleted, AfterSeq: afterSeq})
	if err != nil {
		return false
	}
	idx.mergeFromJournal(existing, rebuild, fullLoad)
	return true
}

// mergeFromJournal folds a journal read into the fact set under the index lock.
// A rebuild re-floors the set to exactly what the read returned; otherwise the
// read is a tail merge that extends the set from the high-water. fullLoad (a
// cold first read or a rebuild) redefines the growth-cap baseline.
func (idx *CompletedFactIndex) mergeFromJournal(existing []events.Event, rebuild, fullLoad bool) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	switch {
	case rebuild:
		// The full re-read below is the new set of record: drop everything the
		// journal no longer backs — keys for facts rotated out of retention and
		// any unconfirmed phantom a dropped Record left behind.
		idx.facts = make(map[completedFactKey]bool, len(existing))
		idx.maxSeq = 0
	case idx.facts == nil:
		idx.facts = make(map[completedFactKey]bool, len(existing))
	default:
		// Re-derivation from the tail. Drop UNCONFIRMED keys first: a key this
		// process add()ed that no journal read has returned is either a fact that
		// did land (its seq is above the high-water, so the tail read below
		// re-adds it as confirmed) or a dropped best-effort Record that never
		// reached the journal — gone now, so this pass re-emits it instead of
		// carrying it as a converged witness forever. A confirmed key is kept: it
		// sits below the high-water the tail read resumes from and cannot be
		// re-derived, which is the ga-ftgyl read this index exists to delete.
		idx.dropUnconfirmedLocked()
	}
	maxSeq := idx.maxSeq
	for _, event := range existing {
		if event.Type == events.ExecutionStepCompleted {
			// Read back from the journal: journal-confirmed, a convergence witness.
			idx.facts[completedFactKeyFor(event)] = true
		}
		if event.Seq > maxSeq {
			maxSeq = event.Seq
		}
	}
	idx.maxSeq = maxSeq
	if fullLoad {
		// Only a whole-journal read redefines the baseline; a tail merge extends
		// the set without re-flooring it, so growth toward the cap accumulates.
		idx.baseline = len(idx.facts)
	}
	idx.loaded = true
}

// dropUnconfirmedLocked removes every key no journal read has confirmed. The
// caller holds idx.mu.
func (idx *CompletedFactIndex) dropUnconfirmedLocked() {
	for key, confirmed := range idx.facts {
		if !confirmed {
			delete(idx.facts, key)
		}
	}
}

// lookup reports whether this exact fact is already recorded (present) and, if
// so, whether a journal read confirmed it (confirmed). Dedup skips on presence;
// only a confirmed key witnesses convergence. Both are read under one lock so a
// concurrent warm cannot flip confirmed between two calls.
func (idx *CompletedFactIndex) lookup(key completedFactKey) (present, confirmed bool) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	confirmed, present = idx.facts[key]
	return present, confirmed
}

// add records a fact this process just emitted, PROVISIONALLY: the emit is a
// best-effort recorder.Record that can be silently dropped, so the key dedups
// re-emission within the process but does not yet witness convergence. It stays
// unconfirmed (false) until a journal read returns it (warm sets it true); a
// later confirmed add must NOT downgrade it, so only a missing key is inserted.
func (idx *CompletedFactIndex) add(key completedFactKey) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.facts == nil {
		idx.facts = map[completedFactKey]bool{}
		idx.loaded = true
	}
	if _, ok := idx.facts[key]; !ok {
		idx.facts[key] = false
	}
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
	// storeRoots holds the current store's UNSTAMPED roots, listed and sorted
	// once when the cursor enters the store and reused by every chunk of the
	// same sweep. The old shape re-listed and re-sorted the store's whole root
	// set on every chunk — O(roots^2) listing per sweep — and a mid-sweep list
	// change could shift the cursor's ground. Roots created mid-sweep are
	// picked up by the NEXT sweep, which was already the documented contract.
	storeRoots       []beads.Bead
	storeRootsLoaded bool
	// skippedConverged accumulates the stamped-root skips of the store
	// currently being traversed so each chunk reports only its own share.
	skippedConverged int
	// VisitStamped makes the NEXT sweep traverse stamped roots too. The
	// completions lane sets it for startup sweeps: converged stamps are a
	// cadence optimization, and the per-boot full traversal is what re-examines
	// them (clearing stale ones — a hand-reopened step, a vacuous stamp from a
	// past store wedge) so a wrong stamp is bounded by one boot, not forever.
	VisitStamped bool
	// sweepVisitsStamped latches VisitStamped for the life of one sweep.
	sweepVisitsStamped bool
	// sweepStoreSignature latches an ORDER-SENSITIVE identity of the store fan at
	// sweep start. The caller re-resolves its store set every chunk, and the
	// cursor is an INDEX into it: if any position's store changes mid-sweep — a
	// store added, removed, replaced, or reordered, even when the total COUNT is
	// unchanged (completionReconcileInputs sorts the rig tail, so a rig add+remove
	// reorders it at equal length) — the held roots no longer correspond to the
	// store at the cursor, so the sweep restarts rather than reconciling one
	// store's roots against another store. A length-only latch missed the
	// equal-length reorder; see storeFanSignature.
	sweepStoreSignature string
	// index is this sweep's idempotency record, loaded once per SWEEP rather
	// than once per chunk. Re-reading the whole journal per chunk would make the
	// chunking that keeps the sweep bounded cost more than the sweep it bounds.
	index CompletedFactIndex
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
	// RootsSkippedConverged counts roots this chunk skipped because they carry
	// the converged stamp: closed roots whose facts a previous pass proved
	// fully journaled. They cost the chunk a map lookup, not a step listing.
	RootsSkippedConverged int
	// WarmFailed reports that the chunk could not load its idempotency record
	// and therefore visited nothing. It is otherwise indistinguishable from a
	// budget-bounded chunk, and the two need different follow-ups: a budget
	// chunk is re-polled immediately, a warm failure must be backed off or the
	// caller re-issues the journal read every poll interval forever (ga-ftgyl).
	WarmFailed bool
}

// Pass visits at most ChunkSize roots, resuming from the last Pass's cursor.
func (b *CompletionBackstop) Pass(recorder events.Provider, graphStores []beads.GraphStore, actor string) CompletionBackstopResult {
	var result CompletionBackstopResult
	if recorder == nil || len(graphStores) == 0 {
		result.SweepComplete = true
		return result
	}
	b.prepareSweep(storeFanSignature(graphStores))
	if !b.index.warm(recorder) {
		result.WarmFailed = true
		return result
	}
	for b.storeIndex < len(graphStores) {
		if b.ChunkSize > 0 && result.RootsVisited >= b.ChunkSize {
			return result
		}
		graphStore := graphStores[b.storeIndex]
		if graphStore.Store == nil {
			b.advanceStore()
			continue
		}
		if err := b.ensureStoreRoots(graphStore); err != nil {
			// A store that cannot be listed does not stall the sweep; the next
			// sweep retries it. The caller is told, so a lane converging nothing
			// cannot look like a lane with nothing to converge.
			result.ListErrors = append(result.ListErrors, err)
			b.advanceStore()
			continue
		}
		// Report the converged roots the load filtered out once, when the store's
		// roots are first held; skippedConverged is 0 on the store's later chunks.
		result.RootsSkippedConverged += b.skippedConverged
		b.skippedConverged = 0
		// Resume strictly after the last root this cursor visited, over the
		// list held for this sweep.
		remaining := b.storeRoots
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
		result.Emitted += reconcileRoots(recorder, graphStore, chunk, &b.index, actor)
		result.RootsVisited += len(chunk)
		if len(chunk) > 0 {
			b.afterRootID = chunk[len(chunk)-1].ID
		}
		if len(chunk) == len(remaining) {
			b.advanceStore()
		}
	}
	b.storeIndex = 0
	b.afterRootID = ""
	result.SweepComplete = true
	return result
}

// prepareSweep readies the sweep for this Pass. At a fresh sweep start it
// re-derives the completion record — nothing feeds this index between sweeps, so
// a warm one would only know the facts it emitted itself and would re-emit
// everything the delta lane or the close path recorded — and latches the sweep's
// VisitStamped choice and store-fan signature. Mid-sweep, a changed store fan (a
// store added, removed, replaced, or reordered, even at unchanged length) means
// the cursor's store index no longer names the store its held roots came from, so
// the sweep restarts against the current fan.
func (b *CompletionBackstop) prepareSweep(storeSignature string) {
	if b.storeIndex == 0 && b.afterRootID == "" && !b.storeRootsLoaded {
		b.index.Invalidate()
		b.sweepVisitsStamped = b.VisitStamped
		b.sweepStoreSignature = storeSignature
		return
	}
	if b.sweepStoreSignature == storeSignature {
		return
	}
	b.storeIndex = 0
	b.afterRootID = ""
	b.storeRoots = nil
	b.storeRootsLoaded = false
	b.skippedConverged = 0
	b.sweepVisitsStamped = b.VisitStamped
	b.sweepStoreSignature = storeSignature
}

// ensureStoreRoots lists graphStore's graph.v2 workflow roots and holds the ones
// still owing a completion pass, sorted by ID, for the rest of the sweep. It is a
// no-op once those roots are held, so the store is listed once per sweep and its
// later chunks reuse the held list (ga-wevcl). The count of converged roots it
// filtered out is recorded on the cursor so the caller can report it.
func (b *CompletionBackstop) ensureStoreRoots(graphStore beads.GraphStore) error {
	if b.storeRootsLoaded {
		return nil
	}
	roots, err := graphStore.ListByMetadata(
		map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
		0,
		beads.IncludeClosed,
		beads.WithBothTiers,
	)
	if err != nil {
		return fmt.Errorf("listing workflow roots in graph store %d: %w", b.storeIndex, err)
	}
	// Converged roots leave the traversal here: the stamp rides the listing
	// already in hand, so skipping one costs a map lookup where visiting it costs
	// a per-root step listing (ga-wevcl).
	unstamped, skipped := partitionUnstampedRoots(roots, b.sweepVisitsStamped)
	sort.Slice(unstamped, func(i, j int) bool { return unstamped[i].ID < unstamped[j].ID })
	b.storeRoots = unstamped
	b.storeRootsLoaded = true
	b.skippedConverged = skipped
	return nil
}

// partitionUnstampedRoots splits roots into the ones a sweep must still visit
// (returned, filtered in place) and a count of the converged ones it may skip. A
// root is skipped only when it is STILL converged: closed AND stamped, and only
// when this is not a visitStamped (per-boot full re-examination) sweep. A stamped
// root that has REOPENED (Status != closed) is no longer converged — it can emit
// again — so it stays in the traversal to be revisited and to have its stale
// stamp cleared, exactly like any other open root. Skipping a reopened-but-stamped
// root would leave the stamp uncleared forever (the clear branch only runs for
// visited roots), permanently voiding the backstop's recovery guarantee for it.
func partitionUnstampedRoots(roots []beads.Bead, visitStamped bool) (unstamped []beads.Bead, skipped int) {
	unstamped = roots[:0]
	for _, root := range roots {
		if !visitStamped &&
			strings.TrimSpace(root.Metadata[beadmeta.CompletionFactsConvergedMetadataKey]) != "" &&
			strings.EqualFold(strings.TrimSpace(root.Status), "closed") {
			skipped++
			continue
		}
		unstamped = append(unstamped, root)
	}
	return unstamped, skipped
}

// advanceStore moves the sweep cursor to the next store and drops the held
// root list so the next store lists afresh.
func (b *CompletionBackstop) advanceStore() {
	b.storeIndex++
	b.afterRootID = ""
	b.storeRoots = nil
	b.storeRootsLoaded = false
	b.skippedConverged = 0
}

// storeFanSignature builds an order-sensitive identity for a resolved store fan.
// The sweep cursor is an INDEX into this slice, so a mid-sweep change to which
// store sits at any position — one added, removed, or replaced, even when the
// total count is unchanged — must restart the sweep. Pointer identity of each
// embedded store, taken in slice order, captures exactly that: two fans with the
// same stores in the same order share a signature, and any reorder or
// replacement diverges. This identity guarantee holds for pointer-backed stores,
// which every production Store is (*BdStore, *CachingStore, *MemStore). A nil
// store and a store whose concrete value is not a pointer (no stable address to
// compare) get fixed markers ("nil"/"np"), so such entries are compared by shape
// and position only: two DISTINCT non-pointer stores at the same index share the
// "np" marker and would NOT diverge — acceptable only because no production fan
// holds one.
func storeFanSignature(graphStores []beads.GraphStore) string {
	var b strings.Builder
	for i, graphStore := range graphStores {
		if i > 0 {
			b.WriteByte('|')
		}
		if graphStore.Store == nil {
			b.WriteString("nil")
			continue
		}
		v := reflect.ValueOf(graphStore.Store)
		if v.Kind() == reflect.Pointer && !v.IsNil() {
			fmt.Fprintf(&b, "%d", v.Pointer())
			continue
		}
		b.WriteString("np")
	}
	return b.String()
}

// reconcileRoots projects the closed steps of the supplied roots and records the
// completion facts the journal is missing. The index is updated as it goes so
// one pass cannot emit the same fact twice across stores. Each root's converged
// stamp is written or cleared from the signals its step pass gathered.
func reconcileRoots(recorder events.Recorder, graphStore beads.GraphStore, roots []beads.Bead, completed *CompletedFactIndex, actor string) int {
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
		emittedForRoot, unconfirmedForRoot, everyStepClosed := reconcileRootSteps(recorder, root, rows, completed, actor)
		emitted += emittedForRoot
		applyConvergenceStamp(graphStore, root, len(rows), emittedForRoot, unconfirmedForRoot, everyStepClosed)
	}
	return emitted
}

// reconcileRootSteps records the completion facts the journal is missing for one
// root's closed steps, updating the index as it goes. It reports how many facts
// this pass emitted, how many present-but-UNCONFIRMED witnesses it saw (a
// best-effort add this process made that no journal read has returned — possibly
// a dropped Record), and whether every step row was closed. applyConvergenceStamp
// turns those three signals into the root's converged stamp.
func reconcileRootSteps(recorder events.Recorder, root beads.Bead, rows []stepRow, completed *CompletedFactIndex, actor string) (emitted, unconfirmed int, everyStepClosed bool) {
	everyStepClosed = true
	for _, row := range rows {
		// The row the steps List already returned decides the status.
		// Re-Getting it would only narrow a window the journal-keyed
		// idempotency record already covers: a step that closes between
		// the List and the write is repaired by the next pass.
		step := row.bead
		if !strings.EqualFold(strings.TrimSpace(step.Status), "closed") {
			everyStepClosed = false
			continue
		}
		event, ok := LifecycleEvent(events.ExecutionStepCompleted, root, step, actor)
		if !ok {
			continue
		}
		key := completedFactKeyFor(event)
		if present, confirmed := completed.lookup(key); present {
			// Already recorded: dedup on presence. But an UNconfirmed key
			// (this process's own best-effort add that no journal read has
			// returned) is not a convergence witness — it may be a dropped
			// Record that never reached the journal. Count it so the stamp
			// withholds until a journal read confirms it.
			if !confirmed {
				unconfirmed++
			}
			continue
		}
		recorder.Record(event)
		completed.add(key)
		emitted++
	}
	return emitted, unconfirmed, everyStepClosed
}

// applyConvergenceStamp writes or clears root's converged stamp from the signals
// reconcileRootSteps gathered. Both writes are best-effort: a failed stamp
// re-proves and a failed clear re-heals on the next sweep.
func applyConvergenceStamp(graphStore beads.GraphStore, root beads.Bead, rowCount, emittedForRoot, unconfirmedForRoot int, everyStepClosed bool) {
	stamped := strings.TrimSpace(root.Metadata[beadmeta.CompletionFactsConvergedMetadataKey]) != ""
	// VERIFIED convergence only: this pass emitted NOTHING for the root AND every
	// surviving witness was JOURNAL-CONFIRMED (read back by the warm index), not a
	// self-added phantom. A key this process merely add()ed for a best-effort
	// Record that may have been silently dropped is unconfirmed and blocks the
	// stamp (unconfirmedForRoot > 0) until a later journal read confirms the fact
	// really landed — otherwise a dropped append would stamp a PERMANENT fact loss
	// (the review's critical). A pass that emitted stamps nothing either: its
	// Record calls are fire-and-forget, so the root converges a sweep later,
	// forever after. rowCount > 0 keeps a transiently empty step listing (a store
	// wedge answering empty-with-nil) from vacuously proving convergence. The stamp
	// only ever applies to a closed root.
	converged := emittedForRoot == 0 && unconfirmedForRoot == 0 && everyStepClosed && rowCount > 0 &&
		strings.EqualFold(strings.TrimSpace(root.Status), "closed")
	switch {
	case converged && !stamped:
		_ = graphStore.SetMetadata(root.ID, beadmeta.CompletionFactsConvergedMetadataKey, time.Now().UTC().Format(time.RFC3339))
	case stamped && !converged:
		// A stamped root this pass could still emit for — one with an unconfirmed
		// witness, open steps, or an invisible listing — carries a STALE stamp (a
		// hand-reopened step, a vacuous stamp from a past wedge, or a false stamp a
		// since-fixed phantom left behind), OR a root that has REOPENED
		// (root.Status != closed) while its step rows stayed closed (a converged
		// root re-driven/retried). The stamp only ever applies to a closed root, so
		// a stamped non-closed root is always stale. Clear it so the sweeps resume
		// visiting: the cadence filter keeps a reopened root in the traversal, and a
		// startup sweep re-examines every stamped root, so both paths reach here and
		// heal it.
		_ = graphStore.SetMetadata(root.ID, beadmeta.CompletionFactsConvergedMetadataKey, "")
	}
}

// completedFacts returns the matching completion journal, including a
// FileRecorder segment that is temporarily awaiting archive compression. A
// reconciliation pass must see that segment before deciding a close needs a
// recovery fact; otherwise an event rotation can create a duplicate fact.
func completedFacts(recorder events.Provider, filter events.Filter) ([]events.Event, error) {
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
