package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convergence"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/molecule"
)

// convergenceStoreAdapter bridges beads.Store to convergence.Store.
// It maintains an in-memory index of active convergence beads (bead ID →
// target agent) to avoid O(n) scans on every tick. The index is populated
// once at startup and maintained on state transitions via SetMetadata.
// No mutex is needed — single-writer event loop.
type convergenceStoreAdapter struct {
	store              beads.Store
	formulaSearchPaths []string          // search paths for formula compilation in PourWisp
	activeIndex        map[string]string // bead ID → target agent; nil until populateIndex
}

var _ convergence.Store = (*convergenceStoreAdapter)(nil)

func newConvergenceStoreAdapter(store beads.Store, formulaSearchPaths []string) *convergenceStoreAdapter {
	return &convergenceStoreAdapter{store: store, formulaSearchPaths: formulaSearchPaths}
}

// populateIndex performs a one-time scan of all beads to build the
// active index. Called after startup reconciliation completes.
func (a *convergenceStoreAdapter) populateIndex() error {
	all, err := a.store.List(beads.ListQuery{Type: "convergence"})
	if err != nil {
		return err
	}
	idx := make(map[string]string)
	for _, b := range all {
		if b.Metadata == nil {
			continue
		}
		state := b.Metadata[convergence.FieldState]
		if state == convergence.StateActive || state == convergence.StateWaitingManual {
			idx[b.ID] = b.Metadata[convergence.FieldTarget]
		}
	}
	a.activeIndex = idx
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
	b, err := a.store.Get(id)
	if err != nil {
		return convergence.BeadInfo{}, err
	}
	return beadToInfo(b), nil
}

func (a *convergenceStoreAdapter) GetMetadata(id string) (map[string]string, error) {
	b, err := a.store.Get(id)
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
	if err := a.store.SetMetadata(id, key, value); err != nil {
		return err
	}
	// Maintain active index on state transitions.
	if a.activeIndex != nil && key == convergence.FieldState {
		switch value {
		case convergence.StateActive, convergence.StateWaitingManual:
			// Add to index. Read target if not already indexed.
			if _, ok := a.activeIndex[id]; !ok {
				b, err := a.store.Get(id)
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

func (a *convergenceStoreAdapter) CloseBead(id string) error {
	if err := a.store.Close(id); err != nil {
		return err
	}
	if a.activeIndex != nil {
		delete(a.activeIndex, id)
	}
	return nil
}

func (a *convergenceStoreAdapter) DeleteBead(id string) error {
	if err := a.store.Delete(id); err != nil {
		return err
	}
	if a.activeIndex != nil {
		delete(a.activeIndex, id)
	}
	return nil
}

func (a *convergenceStoreAdapter) Children(parentID string) ([]convergence.BeadInfo, error) {
	children, err := a.store.List(beads.ListQuery{
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

func (a *convergenceStoreAdapter) pourWisp(parentID, formula, idempotencyKey string, vars map[string]string, evaluatePrompt string, deferAssignees bool) (string, error) {
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
	cookVars := make(map[string]string, len(vars)+1)
	for k, v := range vars {
		cookVars[k] = v
	}
	if evaluatePrompt != "" {
		cookVars["evaluate_prompt"] = evaluatePrompt
	}
	result, err := molecule.Cook(context.Background(), a.store, formula, a.formulaSearchPaths, molecule.Options{
		Vars:           cookVars,
		ParentID:       parentID,
		IdempotencyKey: idempotencyKey,
		DeferAssignees: deferAssignees,
	})
	if err != nil {
		return "", err
	}
	return result.RootID, nil
}

func (a *convergenceStoreAdapter) ActivateWisp(id string) error {
	return a.activateDeferredAssignees(id)
}

func (a *convergenceStoreAdapter) activateDeferredAssignees(id string) error {
	b, err := a.store.Get(id)
	if err != nil {
		return err
	}
	update := beads.UpdateOpts{}
	if assignee := b.Metadata[molecule.DeferredAssigneeMetadataKey]; assignee != "" && b.Assignee != assignee {
		update.Assignee = &assignee
	}
	metadata := map[string]string{}
	if routedTo := b.Metadata[molecule.DeferredRoutedToMetadataKey]; routedTo != "" && b.Metadata["gc.routed_to"] != routedTo {
		metadata["gc.routed_to"] = routedTo
	}
	if executionRoutedTo := b.Metadata[molecule.DeferredExecutionRoutedToMetadataKey]; executionRoutedTo != "" && b.Metadata["gc.execution_routed_to"] != executionRoutedTo {
		metadata["gc.execution_routed_to"] = executionRoutedTo
	}
	if typ := b.Metadata[molecule.DeferredTypeMetadataKey]; typ != "" && b.Type != typ {
		update.Type = &typ
	}
	if len(metadata) > 0 {
		update.Metadata = metadata
	}
	if update.Assignee != nil || update.Type != nil || len(update.Metadata) > 0 {
		if err := a.store.Update(id, update); err != nil {
			return fmt.Errorf("assigning deferred bead %q: %w", id, err)
		}
	}

	children, err := a.store.Children(id, beads.IncludeClosed)
	if err != nil {
		return fmt.Errorf("listing children for activation %q: %w", id, err)
	}
	for _, child := range children {
		if err := a.activateDeferredAssignees(child.ID); err != nil {
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
	children, err := a.store.List(beads.ListQuery{
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
	all, err := a.store.List(beads.ListQuery{
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
	all, err := a.store.List(beads.ListQuery{Type: "convergence"})
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
		if (state == convergence.StateActive || state == convergence.StateWaitingManual) && target == targetAgent {
			count++
		}
	}
	return count, nil
}

func (a *convergenceStoreAdapter) CreateConvergenceBead(title string) (string, error) {
	b, err := a.store.Create(beads.Bead{
		Title:  title,
		Type:   "convergence",
		Status: "in_progress",
	})
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
