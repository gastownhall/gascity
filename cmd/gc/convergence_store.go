package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convergence"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/graphv2"
	"github.com/gastownhall/gascity/internal/molecule"
)

// convergenceStoreAdapter bridges beads.Store to convergence.Store.
// It maintains an in-memory index of active convergence beads (bead ID →
// target agent) to avoid O(n) scans on every tick. The index is populated
// once at startup and maintained on state transitions via SetMetadata.
// No mutex is needed — single-writer event loop. indexReady is an atomic
// flag for safe cross-goroutine reads of the ready state (e.g. test pollers).
type convergenceStoreAdapter struct {
	// store is where this scope's NEW convergence roots are created: the
	// scope store on a single-store city, the graph binding on a split one
	// (graphPlacementForScope).
	store              beads.Store
	formulaSearchPaths []string          // search paths for formula compilation in PourWisp
	relocated          bool              // store is a class binding, not this scope's work ledger
	activeIndex        map[string]string // bead ID → target agent; nil until populateIndex
	indexReady         atomic.Bool       // true once populateIndex has completed

	// scopeRef is the gc.root_store_ref this scope stamps on the roots and
	// wisps it creates and filters its lists by. Empty — every single-store
	// city — means no stamp and no filter: the store holds this scope alone.
	// On a split city the city scope and every rig scope share one binding,
	// and this is what keeps their loops apart.
	scopeRef string
	// relic is the rig's own store while it may still hold pre-S4
	// convergence loops (graphRelicLegForScope); nil otherwise. Lists union
	// it behind store, and by-id operations fall back to it, so a legacy loop
	// keeps running where it is until the rig is imported.
	relic beads.Store
}

var _ convergence.Store = (*convergenceStoreAdapter)(nil)

func newConvergenceStoreAdapter(store beads.Store, formulaSearchPaths []string, relocated bool) *convergenceStoreAdapter {
	return &convergenceStoreAdapter{store: store, formulaSearchPaths: formulaSearchPaths, relocated: relocated}
}

// inScope reports whether a bead listed from the placement store belongs to
// this scope. A rig scope owns exactly the rows stamped with its ref. The
// city scope owns every row not stamped with a rig ref, which keeps the city
// loops a split city minted before S4 (no stamp) in the city scope.
func (a *convergenceStoreAdapter) inScope(b beads.Bead) bool {
	if a.scopeRef == "" {
		return true
	}
	ref := strings.TrimSpace(b.Metadata[beadmeta.RootStoreRefMetadataKey])
	if strings.HasPrefix(a.scopeRef, "rig:") {
		return ref == a.scopeRef
	}
	return !strings.HasPrefix(ref, "rig:")
}

// list reads query from the placement store, keeps this scope's rows, and
// unions the relic leg's rows behind them (first copy of an id wins). An id
// the placement store holds for another scope still settles that id, as it
// does in get: the relic's twin of it is not this scope's loop.
func (a *convergenceStoreAdapter) list(query beads.ListQuery) ([]beads.Bead, error) {
	rows, err := a.store.List(query)
	if err != nil {
		return nil, err
	}
	out := rows[:0]
	seen := make(map[string]bool, len(rows))
	for _, b := range rows {
		seen[b.ID] = true
		if !a.inScope(b) {
			continue
		}
		out = append(out, b)
	}
	if a.relic == nil {
		return out, nil
	}
	// Relic leg (S4 debt): the rig's own store holds convergence loops only
	// from before S4a, until the rig is imported.
	var relicRows []beads.Bead
	err = beads.RelicRead(func() error {
		var listErr error
		relicRows, listErr = a.relic.List(query)
		return listErr
	})
	if err != nil {
		return nil, fmt.Errorf("listing the rig's own store for pre-S4 convergence loops: %w", err)
	}
	for _, b := range relicRows {
		if seen[b.ID] {
			continue
		}
		seen[b.ID] = true
		out = append(out, b)
	}
	return out, nil
}

// listConvergence lists this scope's convergence roots.
func (a *convergenceStoreAdapter) listConvergence() ([]beads.Bead, error) {
	return a.list(beads.ListQuery{Type: "convergence"})
}

// get reads id by residence: the placement store first, then the relic leg.
//
// The placement store is shared by every scope on a split city, so a row it
// returns for another scope reads as not found here: the city scope, or a
// second rig, must never read, write, index or pour on rig ra's loop.
func (a *convergenceStoreAdapter) get(id string) (beads.Bead, beads.Store, error) {
	var accept func(beads.Bead) bool
	if a.scopeRef != "" {
		accept = a.inScope
	}
	return graphResidenceForTree(a.store, a.relic, id, accept)
}

// storeHolding is R-S4.2 for this scope: the store a write on id, or on a
// bead extending id's tree, goes to. Without a relic leg that is always the
// placement store and nothing is read.
//
// A bead the shared binding holds for ANOTHER scope is refused (not found),
// never written: that is what keeps `gc converge ... --rig` with the wrong rig
// from driving another scope's loop. A bead no leg holds resolves to the
// placement store, whose own operation then reports the miss as before.
func (a *convergenceStoreAdapter) storeHolding(id string) (beads.Store, error) {
	if a.relic == nil && a.scopeRef == "" {
		return a.store, nil
	}
	_, store, err := a.get(id)
	switch {
	case err == nil:
		return store, nil
	case errors.Is(err, errGraphOutOfScope):
		return nil, err
	case errors.Is(err, beads.ErrNotFound):
		return a.store, nil
	default:
		return nil, err
	}
}

// populateIndex performs a one-time scan of all beads to build the
// active index. Called after startup reconciliation completes.
func (a *convergenceStoreAdapter) populateIndex() error {
	all, err := a.listConvergence()
	if err != nil {
		return err
	}
	idx := make(map[string]string)
	for _, b := range all {
		if b.Metadata == nil {
			continue
		}
		state := b.Metadata[convergence.FieldState]
		if state == convergence.StateActive || state == convergence.StateWaitingManual || state == convergence.StateWaitingTrigger {
			idx[b.ID] = b.Metadata[convergence.FieldTarget]
		}
	}
	a.activeIndex = idx
	a.indexReady.Store(true)
	return nil
}

// activeBeadIDs returns the bead IDs currently in the active index.
func (a *convergenceStoreAdapter) activeBeadIDs() []string {
	if a.activeIndex == nil {
		return nil
	}
	ids := make([]string, 0, len(a.activeIndex))
	for id := range a.activeIndex {
		ids = append(ids, id)
	}
	return ids
}

func (a *convergenceStoreAdapter) GetBead(id string) (convergence.BeadInfo, error) {
	b, _, err := a.get(id)
	if err != nil {
		return convergence.BeadInfo{}, err
	}
	return beadToInfo(b), nil
}

func (a *convergenceStoreAdapter) GetMetadata(id string) (map[string]string, error) {
	b, _, err := a.get(id)
	if err != nil {
		return nil, err
	}
	if b.Metadata == nil {
		return map[string]string{}, nil
	}
	// Return a copy to prevent callers from mutating the store's internal state.
	cp := make(map[string]string, len(b.Metadata))
	for k, v := range b.Metadata {
		cp[k] = v
	}
	return cp, nil
}

func (a *convergenceStoreAdapter) SetMetadata(id, key, value string) error {
	store, err := a.storeHolding(id)
	if err != nil {
		return err
	}
	if err := store.SetMetadata(id, key, value); err != nil {
		return err
	}
	// Maintain active index on state transitions.
	if a.activeIndex != nil && key == convergence.FieldState {
		switch value {
		case convergence.StateActive, convergence.StateWaitingManual, convergence.StateWaitingTrigger:
			// Add to index. Read target if not already indexed.
			if _, ok := a.activeIndex[id]; !ok {
				b, err := store.Get(id)
				if err != nil {
					return fmt.Errorf("reading bead %q for active index: %w", id, err)
				}
				if b.Metadata != nil {
					a.activeIndex[id] = b.Metadata[convergence.FieldTarget]
				}
			}
		case convergence.StateTerminated, convergence.StateCreating:
			delete(a.activeIndex, id)
		}
	}
	return nil
}

func (a *convergenceStoreAdapter) CloseBead(id, reason string) error {
	// Stamp close_reason before Close so validation.on-close=error sees
	// it on the close that follows. Best-effort: an error here is not
	// fatal — Close still proceeds and any pre-existing close_reason is
	// preserved.
	store, err := a.storeHolding(id)
	if err != nil {
		return err
	}
	if reason != "" {
		_ = store.SetMetadata(id, "close_reason", reason)
	}
	if err := store.Close(id); err != nil {
		return err
	}
	if a.activeIndex != nil {
		delete(a.activeIndex, id)
	}
	return nil
}

func (a *convergenceStoreAdapter) DeleteBead(id string) error {
	store, err := a.storeHolding(id)
	if err != nil {
		return err
	}
	if err := store.Delete(id); err != nil {
		return err
	}
	if a.activeIndex != nil {
		delete(a.activeIndex, id)
	}
	return nil
}

func (a *convergenceStoreAdapter) Children(parentID string) ([]convergence.BeadInfo, error) {
	store, err := a.storeHolding(parentID)
	if err != nil {
		return nil, err
	}
	children, err := store.List(beads.ListQuery{
		ParentID: parentID,
		Sort:     beads.SortCreatedAsc,
	})
	if err != nil {
		return nil, err
	}
	result := make([]convergence.BeadInfo, len(children))
	for i, b := range children {
		result[i] = beadToInfo(b)
	}
	return result, nil
}

func (a *convergenceStoreAdapter) PourWisp(parentID, formula, idempotencyKey string, vars map[string]string, evaluatePrompt string) (string, error) {
	return a.pourWisp(parentID, formula, idempotencyKey, vars, evaluatePrompt, false)
}

func (a *convergenceStoreAdapter) PourSpeculativeWisp(parentID, formula, idempotencyKey string, vars map[string]string, evaluatePrompt string) (string, error) {
	return a.pourWisp(parentID, formula, idempotencyKey, vars, evaluatePrompt, true)
}

func (a *convergenceStoreAdapter) pourWisp(parentID, formulaName, idempotencyKey string, vars map[string]string, evaluatePrompt string, deferAssignees bool) (string, error) {
	// Idempotency: check if a wisp with this key already exists (crash-retry safety).
	// Fail closed on lookup errors to prevent duplicate wisps.
	existing, found, err := a.FindByIdempotencyKey(idempotencyKey)
	if err != nil {
		return "", fmt.Errorf("idempotency check for %q: %w", idempotencyKey, err)
	}
	if found {
		return existing, nil
	}

	// Build vars map with evaluate_prompt if set.
	cookVars := make(map[string]string)
	for k, v := range vars {
		cookVars[k] = v
	}
	if evaluatePrompt != "" {
		cookVars["evaluate_prompt"] = evaluatePrompt
	}
	isGraphV2, _, err := graphv2.IsGraphV2Formula(formulaName, a.formulaSearchPaths)
	if err != nil {
		return "", fmt.Errorf("checking formulas v2 contract for convergence wisp %q: %w", formulaName, err)
	}
	if isGraphV2 {
		return "", fmt.Errorf("convergence wisps do not support v2 formula %q; use a v1 formula until convergence has an explicit input convoy target", formulaName)
	}
	opts := molecule.Options{
		Vars:           cookVars,
		ParentID:       parentID,
		IdempotencyKey: idempotencyKey,
		DeferAssignees: deferAssignees,
	}
	// molecule.Cook's body, inlined only so the compiled recipe can be classified
	// before anything is written — the same reason gc formula cook inlines it.
	// "Convergence pours a wisp" is the name of the operation, not a guarantee
	// about what the formula compiles to: a v1 POURED formula compiles to a
	// molecule whose every bead is ClassWork.
	recipe, err := formula.CompileWithoutRuntimeVarValidation(context.Background(), formulaName, a.formulaSearchPaths, cookVars)
	if err != nil {
		return "", fmt.Errorf("compiling formula %q: %w", formulaName, err)
	}
	if err := molecule.ValidateRecipeRuntimeVars(recipe, opts); err != nil {
		return "", err
	}
	target, err := a.storeHolding(parentID)
	if err != nil {
		return "", fmt.Errorf("resolving the store holding convergence root %q: %w", parentID, err)
	}
	// A loop a rig created in its own store before S4 (a relic) keeps pouring
	// where it lives: that store is the rig's work ledger, so a work-class pour
	// there strands nothing, and refusing it would stop an in-flight loop with
	// advice it cannot take. Only a pour into the binding is refused.
	if a.relocated && target == a.store && recipeCoordClass(recipe) != coordclass.ClassGraph {
		// Refuse rather than split the molecule off its parent. The convergence
		// root is graph class and lives in the graph binding; a work-class
		// molecule poured there is a strand the per-boot containment re-check
		// will make fatal, and poured anywhere else is a child orphaned from its
		// parent across a store boundary. Neither is recoverable by the loop
		// itself, so this is the one place that can still say no.
		//
		// On a split city this holds at EVERY scope (S4): a rig's convergence
		// roots live in the binding too, so a rig scope is no longer a place a
		// poured formula can go.
		return "", fmt.Errorf("convergence formula %q compiles to work-class beads (a v1 poured formula), which this city cannot pour: it serves the graph class from its own binding, where every convergence loop lives at every scope, city and rig alike, and a work-class molecule there would be a stranded write; make the formula root-only (phase = \"vapor\") so it compiles to a graph-class wisp", formulaName)
	}
	if a.scopeRef != "" && target == a.store {
		// Every graph row carries its scope (S4 §2.2): the wisp shares the
		// binding with every other scope's rows, and scope readers take the
		// scope from this key.
		for i := range recipe.Steps {
			if recipe.Steps[i].Metadata == nil {
				recipe.Steps[i].Metadata = make(map[string]string)
			}
			if strings.TrimSpace(recipe.Steps[i].Metadata[beadmeta.RootStoreRefMetadataKey]) == "" {
				recipe.Steps[i].Metadata[beadmeta.RootStoreRefMetadataKey] = a.scopeRef
			}
		}
	}
	result, err := molecule.Instantiate(context.Background(), target, recipe, opts)
	if err != nil {
		return "", err
	}
	return result.RootID, nil
}

func (a *convergenceStoreAdapter) ActivateWisp(id string) error {
	return a.activateDeferredAssignees(id)
}

func (a *convergenceStoreAdapter) activateDeferredAssignees(id string) error {
	store, err := a.storeHolding(id)
	if err != nil {
		return err
	}
	return activateDeferredAssigneesIn(store, id)
}

// activateDeferredAssigneesIn activates id and its subtree in store, the one
// store that holds the wisp's whole tree.
func activateDeferredAssigneesIn(store beads.Store, id string) error {
	b, err := store.Get(id)
	if err != nil {
		return err
	}
	update := beads.UpdateOpts{}
	if assignee := b.Metadata[molecule.DeferredAssigneeMetadataKey]; assignee != "" && b.Assignee != assignee {
		update.Assignee = &assignee
	}
	metadata := map[string]string{}
	if routedTo := b.Metadata[molecule.DeferredRoutedToMetadataKey]; routedTo != "" && b.Metadata[beadmeta.RoutedToMetadataKey] != routedTo {
		metadata[beadmeta.RoutedToMetadataKey] = routedTo
	}
	if executionRoutedTo := b.Metadata[molecule.DeferredExecutionRoutedToMetadataKey]; executionRoutedTo != "" && b.Metadata[beadmeta.ExecutionRoutedToMetadataKey] != executionRoutedTo {
		metadata[beadmeta.ExecutionRoutedToMetadataKey] = executionRoutedTo
	}
	if typ := b.Metadata[molecule.DeferredTypeMetadataKey]; typ != "" && b.Type != typ {
		update.Type = &typ
	}
	if len(metadata) > 0 {
		update.Metadata = metadata
	}
	if update.Assignee != nil || update.Type != nil || len(update.Metadata) > 0 {
		if err := store.Update(id, update); err != nil {
			return fmt.Errorf("assigning deferred bead %q: %w", id, err)
		}
	}

	children, err := store.Children(id, beads.IncludeClosed)
	if err != nil {
		return fmt.Errorf("listing children for activation %q: %w", id, err)
	}
	for _, child := range children {
		if err := activateDeferredAssigneesIn(store, child.ID); err != nil {
			return err
		}
	}
	return nil
}

func (a *convergenceStoreAdapter) FindByIdempotencyKey(key string) (string, bool, error) {
	// Extract parent bead ID from key format "converge:<bead-id>:iter:<N>".
	parentID := extractParentIDFromKey(key)
	if parentID == "" {
		// Fall back to scanning all beads.
		return a.findByKeyScan(key)
	}
	store, err := a.storeHolding(parentID)
	if err != nil {
		return "", false, fmt.Errorf("resolving the store holding %s: %w", parentID, err)
	}
	children, err := store.List(beads.ListQuery{
		ParentID: parentID,
		Sort:     beads.SortCreatedAsc,
	})
	if err != nil {
		// Children returns empty list (not error) when parent has no children,
		// so any error here is a real store failure — propagate it.
		return "", false, fmt.Errorf("listing children of %s: %w", parentID, err)
	}
	for _, b := range children {
		if b.Metadata != nil && b.Metadata["idempotency_key"] == key {
			return b.ID, true, nil
		}
	}
	return "", false, nil
}

func (a *convergenceStoreAdapter) findByKeyScan(key string) (string, bool, error) {
	all, err := a.list(beads.ListQuery{
		Metadata: map[string]string{"idempotency_key": key},
	})
	if err != nil {
		return "", false, err
	}
	for _, b := range all {
		if b.Metadata != nil && b.Metadata["idempotency_key"] == key {
			return b.ID, true, nil
		}
	}
	return "", false, nil
}

func (a *convergenceStoreAdapter) CountActiveConvergenceLoops(targetAgent string) (int, error) {
	// Use the in-memory index if populated.
	if a.activeIndex != nil {
		count := 0
		for _, target := range a.activeIndex {
			if target == targetAgent {
				count++
			}
		}
		return count, nil
	}
	// Fallback: full scan (before index is populated at startup).
	all, err := a.listConvergence()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, b := range all {
		if b.Metadata == nil {
			continue
		}
		state := b.Metadata[convergence.FieldState]
		target := b.Metadata[convergence.FieldTarget]
		if (state == convergence.StateActive || state == convergence.StateWaitingManual || state == convergence.StateWaitingTrigger) && target == targetAgent {
			count++
		}
	}
	return count, nil
}

func (a *convergenceStoreAdapter) CreateConvergenceBead(title string) (string, error) {
	bead := beads.Bead{
		Title:  title,
		Type:   "convergence",
		Status: "in_progress",
	}
	if a.scopeRef != "" {
		// R-S4.1: a new root records its scope at creation, in the same write,
		// so no window exists in which it would list under the wrong scope.
		bead.Metadata = map[string]string{beadmeta.RootStoreRefMetadataKey: a.scopeRef}
	}
	b, err := a.store.Create(bead)
	if err != nil {
		return "", err
	}
	return b.ID, nil
}

// beadToInfo converts a beads.Bead to convergence.BeadInfo.
func beadToInfo(b beads.Bead) convergence.BeadInfo {
	info := convergence.BeadInfo{
		ID:        b.ID,
		Status:    b.Status,
		ParentID:  b.ParentID,
		CreatedAt: b.CreatedAt,
	}
	if b.Metadata != nil {
		info.IdempotencyKey = b.Metadata["idempotency_key"]
		// Parse closed_at from metadata if present.
		if ca, ok := b.Metadata["closed_at"]; ok && ca != "" {
			if t, err := time.Parse(time.RFC3339Nano, ca); err == nil {
				info.ClosedAt = t
			}
		}
	}
	// If status is closed but no closed_at metadata, use CreatedAt as fallback
	// (duration will be zero, which is acceptable for v0).
	if b.Status == "closed" && info.ClosedAt.IsZero() {
		info.ClosedAt = b.CreatedAt
	}
	return info
}

// extractParentIDFromKey extracts the bead ID from an idempotency key
// of the form "converge:<bead-id>:iter:<N>".
func extractParentIDFromKey(key string) string {
	if !strings.HasPrefix(key, "converge:") {
		return ""
	}
	rest := key[len("converge:"):]
	idx := strings.Index(rest, ":iter:")
	if idx < 0 {
		return ""
	}
	return rest[:idx]
}

// convergenceEventEmitter wraps events.Recorder to implement convergence.EventEmitter.
type convergenceEventEmitter struct {
	rec events.Recorder
}

var _ convergence.EventEmitter = (*convergenceEventEmitter)(nil)

func (e *convergenceEventEmitter) Emit(eventType, eventID, beadID string, payload json.RawMessage, _ bool) {
	e.rec.Record(events.Event{
		Type:    eventType,
		Actor:   "convergence",
		Subject: beadID,
		Message: string(payload),
	})
	_ = eventID // used for deduplication by consumers, not the recorder
}
