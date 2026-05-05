package beads

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"
)

// ApplyEvent updates the cache from a bd hook event. Call this when the
// event bus delivers a bead.created, bead.updated, or bead.closed event
// with the full bead JSON payload. This keeps the cache fresh without
// waiting for reconciliation.
func (c *CachingStore) ApplyEvent(eventType string, payload json.RawMessage) {
	if len(payload) == 0 {
		return
	}

	patch, fields, err := decodeCacheEvent(payload)
	if err != nil {
		c.recordProblem(fmt.Sprintf("apply %s event", eventType), err)
		return
	}
	if !c.ownsBeadID(patch.ID) {
		return
	}

	now := time.Now()
	c.mu.RLock()
	if c.state != cacheLive && c.state != cachePartial {
		c.mu.RUnlock()
		return
	}
	current, cached := c.beads[patch.ID]
	currentDeps, depsKnown := c.deps[patch.ID]
	if !depsKnown && c.depsComplete {
		depsKnown = true
	}
	currentDeps = cloneDeps(currentDeps)
	_, locallyMutated := c.beadSeq[patch.ID]
	localBeadAt := c.localBeadAt[patch.ID]
	recentlyLocal := recentLocalMutation(localBeadAt, now)
	_, locallyDeleted := c.deletedSeq[patch.ID]
	fieldConflictCached := cached && cacheEventConflictsCurrent(current, patch, fields)
	dependencyConflictCached := cached && cacheEventDependencyConflict(currentDeps, depsKnown, patch, fields)
	conflictsCached := fieldConflictCached || dependencyConflictCached
	var conflictBase Bead
	if conflictsCached {
		conflictBase = cloneBead(current)
	}
	c.mu.RUnlock()

	verifiedConflict := false
	var verifiedClosedBase Bead
	verifiedRecentLocal := false
	var verifiedRecentLocalBase Bead
	if conflictsCached && eventType == "bead.closed" {
		matchesBacking, verifyErr := c.cacheClosedEventMatchesBacking(patch.ID)
		if verifyErr != nil {
			c.recordProblem(fmt.Sprintf("verify %s event", eventType), verifyErr)
			// Drop destructive close events on verification failure; reconciliation
			// can catch up without overwriting a local reopen with a stale close.
			return
		}
		if !matchesBacking {
			return
		}
		verifiedConflict = true
		verifiedClosedBase = conflictBase
	}
	if fieldConflictCached && eventType != "bead.closed" && locallyMutated && !verifiedConflict {
		return
	}
	if dependencyConflictCached && eventType != "bead.closed" && locallyMutated && !verifiedConflict {
		return
	}
	if conflictsCached && recentlyLocal && !verifiedConflict {
		verifiedRecentLocal = true
		verifiedRecentLocalBase = conflictBase
		matchesBacking, verifyErr := c.cacheEventMatchesBacking(patch.ID, patch, fields)
		if verifyErr == nil && !matchesBacking {
			return
		}
		if verifyErr != nil {
			c.recordProblem(fmt.Sprintf("verify %s event", eventType), verifyErr)
		}
	}

	b := patch
	refreshedFromBacking := false
	if !cached {
		if fresh, err := c.backing.Get(patch.ID); err == nil {
			b = fresh
			refreshedFromBacking = true
		} else if errors.Is(err, ErrNotFound) {
			if eventType != "bead.created" && locallyDeleted {
				return
			}
		} else if !errors.Is(err, ErrNotFound) {
			c.recordProblem(fmt.Sprintf("refresh %s event", eventType), err)
		}
	}

	if c.applyEventBeforeCommitForTest != nil {
		c.applyEventBeforeCommitForTest()
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != cacheLive && c.state != cachePartial {
		return
	}
	if current, ok := c.beads[patch.ID]; ok {
		currentDeps, depsKnown := c.deps[patch.ID]
		if !depsKnown && c.depsComplete {
			depsKnown = true
		}
		fieldConflict := cacheEventConflictsCurrent(current, patch, fields)
		dependencyConflict := cacheEventDependencyConflict(currentDeps, depsKnown, patch, fields)
		if fieldConflict || dependencyConflict {
			if eventType == "bead.closed" {
				if !verifiedConflict || beadChanged(current, verifiedClosedBase) {
					return
				}
			} else {
				if _, locallyMutated := c.beadSeq[patch.ID]; fieldConflict && locallyMutated {
					return
				}
				if _, locallyMutated := c.beadSeq[patch.ID]; dependencyConflict && locallyMutated {
					return
				}
				if recentLocalMutation(c.localBeadAt[patch.ID], time.Now()) &&
					(!verifiedRecentLocal || beadChanged(current, verifiedRecentLocalBase)) {
					return
				}
			}
		}
		b = mergeCacheEventPatch(current, patch, fields)
	}

	mutated := false
	switch eventType {
	case "bead.created":
		if _, exists := c.beads[b.ID]; !exists {
			c.noteMutationLocked(b.ID)
			c.beads[b.ID] = cloneBead(b)
			c.updateEventDepsLocked(eventType, b, fields, refreshedFromBacking)
			delete(c.dirty, b.ID)
			delete(c.deletedSeq, b.ID)
		}
		c.updateStatsLocked()
		mutated = true
	case "bead.updated":
		c.noteMutationLocked(b.ID)
		c.beads[b.ID] = cloneBead(b)
		c.updateEventDepsLocked(eventType, b, fields, refreshedFromBacking)
		delete(c.dirty, b.ID)
		delete(c.deletedSeq, b.ID)
		mutated = true
	case "bead.closed":
		c.noteMutationLocked(b.ID)
		if _, exists := c.beads[b.ID]; !exists {
			c.updateStatsLocked()
		}
		c.beads[b.ID] = cloneBead(b)
		c.updateEventDepsLocked(eventType, b, fields, refreshedFromBacking)
		delete(c.dirty, b.ID)
		delete(c.deletedSeq, b.ID)
		mutated = true
	default:
		return
	}

	if mutated {
		c.markFreshLocked(time.Now())
	}
}

func (c *CachingStore) updateEventDepsLocked(eventType string, b Bead, fields map[string]json.RawMessage, refreshedFromBacking bool) {
	if hasCacheEventField(fields, "dependencies") || hasCacheEventField(fields, "needs") {
		c.deps[b.ID] = depsFromBeadFields(b)
		return
	}
	if eventType == "bead.created" && cacheEventLooksComplete(fields) {
		c.deps[b.ID] = depsFromBeadFields(b)
		return
	}
	if eventType == "bead.updated" && cacheEventLooksComplete(fields) {
		if refreshedFromBacking {
			c.deps[b.ID] = depsFromBeadFields(b)
			return
		}
		// bd dependency mutations arrive through the same on_update hook as
		// field changes, and the hook payload omits dependencies after removals.
		// Treat the bead's dependency coverage as unknown until the backing
		// store or reconciliation supplies an explicit dependency snapshot.
		delete(c.deps, b.ID)
		c.depsComplete = false
		return
	}
	if _, ok := c.deps[b.ID]; ok {
		return
	}
	if eventType == "bead.updated" && c.depsComplete {
		c.depsComplete = false
		c.recordProblemLocked("apply bead.updated event", fmt.Errorf("dependency cache marked complete but missing deps for %s", b.ID))
		return
	}
	c.depsComplete = false
}

// ApplyDepEvent updates the dep cache for callers that have an authoritative
// dependency snapshot. bd hook payloads that omit dependency fields still flow
// through ApplyEvent and fall back to reconciliation.
func (c *CachingStore) ApplyDepEvent(beadID string, deps []Dep) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != cacheLive && c.state != cachePartial {
		return
	}
	c.noteMutationLocked(beadID)
	c.deps[beadID] = cloneDeps(deps)
	delete(c.dirty, beadID)
	delete(c.deletedSeq, beadID)
	c.markFreshLocked(time.Now())
	c.updateStatsLocked()
}

func mergeCacheEventPatch(base, patch Bead, fields map[string]json.RawMessage) Bead {
	merged := cloneBead(base)
	if hasCacheEventField(fields, "title") {
		merged.Title = patch.Title
	}
	if hasCacheEventField(fields, "status") {
		merged.Status = patch.Status
	}
	if hasCacheEventField(fields, "issue_type") || hasCacheEventField(fields, "type") {
		merged.Type = patch.Type
	}
	if hasCacheEventField(fields, "priority") {
		merged.Priority = cloneIntPtr(patch.Priority)
	}
	if hasCacheEventField(fields, "created_at") {
		merged.CreatedAt = patch.CreatedAt
	}
	if hasCacheEventField(fields, "assignee") {
		merged.Assignee = patch.Assignee
	}
	if hasCacheEventField(fields, "from") {
		merged.From = patch.From
	}
	if hasCacheEventField(fields, "parent") {
		merged.ParentID = patch.ParentID
	}
	if hasCacheEventField(fields, "ref") {
		merged.Ref = patch.Ref
	}
	if hasCacheEventField(fields, "needs") {
		merged.Needs = slices.Clone(patch.Needs)
	}
	if hasCacheEventField(fields, "description") {
		merged.Description = patch.Description
	}
	if hasCacheEventField(fields, "labels") {
		merged.Labels = slices.Clone(patch.Labels)
	}
	if hasCacheEventField(fields, "metadata") {
		merged.Metadata = maps.Clone(patch.Metadata)
	}
	if hasCacheEventField(fields, "dependencies") {
		merged.Dependencies = slices.Clone(patch.Dependencies)
	}
	return merged
}

func cacheEventConflictsCurrent(current, patch Bead, fields map[string]json.RawMessage) bool {
	if hasCacheEventField(fields, "title") && current.Title != patch.Title {
		return true
	}
	if hasCacheEventField(fields, "status") && current.Status != patch.Status {
		return true
	}
	if (hasCacheEventField(fields, "issue_type") || hasCacheEventField(fields, "type")) && current.Type != patch.Type {
		return true
	}
	if hasCacheEventField(fields, "priority") {
		if (current.Priority == nil) != (patch.Priority == nil) {
			return true
		}
		if current.Priority != nil && patch.Priority != nil && *current.Priority != *patch.Priority {
			return true
		}
	}
	if hasCacheEventField(fields, "assignee") && current.Assignee != patch.Assignee {
		return true
	}
	if hasCacheEventField(fields, "description") && current.Description != patch.Description {
		return true
	}
	if hasCacheEventField(fields, "parent") && current.ParentID != patch.ParentID {
		return true
	}
	if hasCacheEventField(fields, "parent_id") && current.ParentID != patch.ParentID {
		return true
	}
	if hasCacheEventField(fields, "metadata") && !maps.Equal(current.Metadata, patch.Metadata) {
		return true
	}
	if hasCacheEventField(fields, "labels") && !slices.Equal(current.Labels, patch.Labels) {
		return true
	}
	return false
}

func cacheEventConflictsCached(current Bead, currentDeps []Dep, depsKnown bool, patch Bead, fields map[string]json.RawMessage) bool {
	if cacheEventConflictsCurrent(current, patch, fields) {
		return true
	}
	return cacheEventDependencyConflict(currentDeps, depsKnown, patch, fields)
}

func cacheEventDependencyConflict(currentDeps []Dep, depsKnown bool, patch Bead, fields map[string]json.RawMessage) bool {
	return cacheEventHasDependencyField(fields) && depsKnown && depsChanged(currentDeps, depsFromBeadFields(patch))
}

func (c *CachingStore) cacheEventMatchesBacking(id string, patch Bead, fields map[string]json.RawMessage) (bool, error) {
	fresh, err := c.backing.Get(id)
	if err != nil {
		return false, err
	}
	return cacheEventPatchMatchesBead(fresh, patch, fields), nil
}

func (c *CachingStore) cacheClosedEventMatchesBacking(id string) (bool, error) {
	fresh, err := c.backing.Get(id)
	if err != nil {
		return false, err
	}
	return fresh.Status == "closed", nil
}

func cacheEventPatchMatchesBead(current, patch Bead, fields map[string]json.RawMessage) bool {
	return !cacheEventConflictsCached(current, depsFromBeadFields(current), true, patch, fields)
}

func recentLocalMutation(mutatedAt time.Time, now time.Time) bool {
	return !mutatedAt.IsZero() && now.Sub(mutatedAt) <= 5*time.Second
}

func (c *CachingStore) recentLocalBeadConflictLocked(id string, fresh Bead, now time.Time) (Bead, bool) {
	current, ok := c.beads[id]
	if !ok {
		return Bead{}, false
	}
	if !recentLocalMutation(c.localBeadAt[id], now) {
		return Bead{}, false
	}
	if !beadChanged(current, fresh) {
		return Bead{}, false
	}
	return cloneBead(current), true
}

func (c *CachingStore) carryRecentLocalMutationLocked(id string, nextDirty map[string]struct{}, nextBeadSeq map[string]uint64, nextLocalBeadAt map[string]time.Time) {
	if _, dirty := c.dirty[id]; dirty {
		nextDirty[id] = struct{}{}
	}
	if seq, ok := c.beadSeq[id]; ok {
		nextBeadSeq[id] = seq
	}
	if mutatedAt, ok := c.localBeadAt[id]; ok {
		nextLocalBeadAt[id] = mutatedAt
	}
}

func hasCacheEventField(fields map[string]json.RawMessage, name string) bool {
	_, ok := fields[name]
	return ok
}

func cacheEventHasDependencyField(fields map[string]json.RawMessage) bool {
	return hasCacheEventField(fields, "dependencies") || hasCacheEventField(fields, "needs")
}

func cacheEventLooksComplete(fields map[string]json.RawMessage) bool {
	return hasCacheEventField(fields, "title") &&
		hasCacheEventField(fields, "status") &&
		hasCacheEventField(fields, "created_at") &&
		(hasCacheEventField(fields, "issue_type") || hasCacheEventField(fields, "type"))
}

func decodeCacheEvent(payload json.RawMessage) (Bead, map[string]json.RawMessage, error) {
	eventPayload := payload
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return Bead{}, nil, err
	}
	if beadPayload, ok := envelope["bead"]; ok {
		eventPayload = beadPayload
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(eventPayload, &fields); err != nil {
		return Bead{}, nil, err
	}
	var wire struct {
		Bead
		Metadata   StringMap `json:"metadata,omitempty"`
		TypeCompat string    `json:"type,omitempty"`
	}
	if err := json.Unmarshal(eventPayload, &wire); err != nil {
		return Bead{}, nil, err
	}
	b := wire.Bead
	if wire.Metadata != nil {
		b.Metadata = map[string]string(wire.Metadata)
	}
	if b.ID == "" {
		return Bead{}, nil, fmt.Errorf("missing bead id")
	}
	// bd hook payloads use "issue_type" while exec-style payloads may use "type".
	if b.Type == "" && wire.TypeCompat != "" {
		b.Type = wire.TypeCompat
	}
	return b, fields, nil
}

func (c *CachingStore) notifyChange(eventType string, b Bead) {
	if c.onChange == nil {
		return
	}
	payload, err := json.Marshal(b)
	if err != nil {
		c.recordProblem(fmt.Sprintf("marshal %s notification", eventType), err)
		return
	}
	c.onChange(eventType, b.ID, payload)
}

type cacheNotification struct {
	eventType string
	bead      Bead
}

func (c *CachingStore) notifyChanges(notifications []cacheNotification) {
	for _, notification := range notifications {
		c.notifyChange(notification.eventType, notification.bead)
	}
}

func beadChanged(old, fresh Bead) bool {
	if old.ID != fresh.ID ||
		old.Title != fresh.Title ||
		old.Status != fresh.Status ||
		old.Type != fresh.Type ||
		!intPtrEqual(old.Priority, fresh.Priority) ||
		!old.CreatedAt.Equal(fresh.CreatedAt) ||
		old.Assignee != fresh.Assignee ||
		old.From != fresh.From ||
		old.ParentID != fresh.ParentID ||
		old.Ref != fresh.Ref ||
		old.Description != fresh.Description {
		return true
	}
	if !maps.Equal(old.Metadata, fresh.Metadata) {
		return true
	}
	if !slices.Equal(old.Labels, fresh.Labels) {
		return true
	}
	if !slices.Equal(old.Needs, fresh.Needs) {
		return true
	}
	return !slices.Equal(old.Dependencies, fresh.Dependencies)
}

func depsChanged(old, fresh []Dep) bool {
	return !slices.Equal(old, fresh)
}

func intPtrEqual(left, right *int) bool {
	switch {
	case left == nil && right == nil:
		return true
	case left == nil || right == nil:
		return false
	default:
		return *left == *right
	}
}
