package beads

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// ApplyEvent updates the cache from a bd hook event. Call this when the
// event bus delivers a bead.created, bead.updated, bead.closed, or bead.deleted event
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
	unlock, serializedAfterMutation := c.lockCloseStateContended(patch.ID)
	defer unlock()

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
	seqBase, locallyMutated := c.beadSeq[patch.ID]
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
	var verifiedClosedFresh Bead
	verifiedClosedFromBacking := false
	verifiedRecentLocal := false
	var verifiedRecentLocalBase Bead
	if conflictsCached && eventType == "bead.closed" {
		fresh, matchesBacking, verifyErr := c.cacheClosedEventMatchesBacking(patch.ID)
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
		if closedEventPayloadNeedsBackingRefresh(patch, fresh) {
			verifiedClosedFresh = fresh
			verifiedClosedFromBacking = true
		}
	}
	if conflictsCached && eventType != "bead.closed" && locallyMutated && !recentlyLocal && !verifiedConflict {
		// The bead is flagged locally mutated only because a prior applied
		// event set its mutation seq (noteMutationLocked sets beadSeq on every
		// applied event), or because of a local write older than the recency
		// window. Backing reads are reliable here (no in-flight write-through),
		// so verify the conflicting event against the backing store instead of
		// dropping it outright: drop only genuinely stale events (which would
		// clobber an unflushed local write); apply when the backing store
		// already reflects the event — e.g. a gc.routed_to stamp written by
		// `gc sling` in another process. Dropping unconditionally here stranded
		// pool demand until an unrelated later event arrived after a reconcile
		// cleared the mutation seq (gastownhall/gascity#2210).
		matchesBacking, verifyErr := c.cacheEventMatchesBacking(patch.ID, patch, fields)
		if verifyErr != nil {
			c.recordProblem(fmt.Sprintf("verify %s event", eventType), verifyErr)
			return
		}
		if !matchesBacking {
			// A field-changing event that could not be confirmed against the
			// backing store is either genuinely stale, or real but not yet
			// visible to this process's backing read — a write-through race
			// after a cross-process gc sling/kickoff stamps gc.routed_to or
			// claims the bead. Dropping it outright leaves a stale cached row
			// that CachedReady still serves with ok=true, so the demand path
			// counts the bead off the stale row and strands it (no routed_to /
			// wrong status) until the next full reconcile
			// (gastownhall/gascity#2927). Mark the bead dirty so the cached
			// ready model declines for it and the demand path falls back to the
			// authoritative ReadyLive query; reconciliation clears the flag once
			// cache and backing reconverge. A dependency-only conflict is left
			// untouched: dependency snapshots routinely arrive ahead of the
			// backing and are intentionally tolerated without declining.
			if fieldConflictCached {
				c.mu.Lock()
				c.markDirtyLocked(patch.ID)
				c.mu.Unlock()
			}
			return
		}
		verifiedRecentLocal = true
		verifiedRecentLocalBase = conflictBase
	} else if !recentlyLocal || !serializedAfterMutation {
		if fieldConflictCached && eventType != "bead.closed" && locallyMutated && !verifiedConflict {
			return
		}
		if dependencyConflictCached && eventType != "bead.closed" && locallyMutated && !verifiedConflict {
			return
		}
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
			if serializedAfterMutation && locallyMutated {
				return
			}
		}
	}

	b := patch
	refreshedFromBacking := false
	if verifiedClosedFromBacking {
		b = verifiedClosedFresh
		refreshedFromBacking = true
	} else if !cached {
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
				if !verifiedConflict || beadChanged(current, verifiedClosedBase, false) {
					return
				}
			} else {
				_, locallyMutated := c.beadSeq[patch.ID]
				// A concurrent local write can land in the RUnlock->Lock window.
				// beadChanged compares only the cached Bead, but DepAdd/DepRemove
				// mutate c.deps and bump the mutation seq without touching
				// c.beads[id], so a dep-only write slips that guard. The mutation
				// seq advancing past the read-phase snapshot is the reliable
				// signal that some local write intervened since the backing
				// verification (gastownhall/gascity#2210).
				changedSinceVerify := beadChanged(current, verifiedRecentLocalBase, false) ||
					c.beadSeq[patch.ID] != seqBase
				// Re-check a genuine recent local write under the write lock to
				// catch a write that landed between the read-lock verification
				// and here; it wins unconditionally.
				if recentLocalMutation(c.localBeadAt[patch.ID], time.Now()) &&
					(!verifiedRecentLocal || changedSinceVerify) {
					return
				}
				// For a bead flagged locally mutated only by a prior event,
				// apply the conflict only if it was verified against the
				// backing store under the read lock and nothing changed since
				// (no concurrent local write); otherwise drop and let
				// reconciliation reconverge (gastownhall/gascity#2210).
				if locallyMutated &&
					(!verifiedRecentLocal || changedSinceVerify) {
					return
				}
			}
		}
		if eventType != "bead.closed" || !verifiedClosedFromBacking {
			b = mergeCacheEventPatch(current, patch, fields)
		}
	}

	mutated := false
	switch eventType {
	case "bead.created":
		if _, exists := c.beads[b.ID]; !exists {
			c.noteMutationLocked(b.ID)
			// OC-3: absorb installs the row before updateEventDepsLocked, whose
			// clearReadyProjectionLocked must observe the newly absorbed row.
			c.absorbFreshLocked(b.ID, b, time.Now(), absorbOpts{
				depsMode:   depsKeepCached,
				seqMode:    seqKeep,
				clearDirty: true,
			})
			c.updateEventDepsLocked(eventType, b, fields, refreshedFromBacking)
		}
		c.updateStatsLocked()
		mutated = true
		if c.clearDependentReadyProjectionsLocked(b.ID) {
			mutated = true
		}
	case "bead.updated":
		existing, cached := c.beads[b.ID]
		if !cached || beadChanged(existing, b, false) {
			c.noteMutationLocked(b.ID)
			c.absorbFreshLocked(b.ID, b, time.Now(), absorbOpts{
				depsMode:   depsKeepCached,
				seqMode:    seqKeep,
				clearDirty: true,
			})
			mutated = true
		}
		if depsMutated := c.updateEventDepsLocked(eventType, b, fields, refreshedFromBacking); depsMutated && !mutated {
			c.noteMutationLocked(b.ID)
			mutated = true
		}
		if hasCacheEventField(fields, "status") && c.clearDependentReadyProjectionsLocked(b.ID) {
			mutated = true
		}
	case "bead.closed":
		c.noteMutationLocked(b.ID)
		if _, exists := c.beads[b.ID]; !exists {
			c.updateStatsLocked()
		}
		// OC-3: absorb before updateEventDepsLocked (see bead.created).
		c.absorbFreshLocked(b.ID, b, time.Now(), absorbOpts{
			depsMode:   depsKeepCached,
			seqMode:    seqKeep,
			clearDirty: true,
		})
		c.updateEventDepsLocked(eventType, b, fields, refreshedFromBacking)
		mutated = true
		if c.clearDependentReadyProjectionsLocked(b.ID) {
			mutated = true
		}
	case "bead.deleted":
		c.noteMutationLocked(b.ID)
		c.tombstoneLocked(b.ID, c.mutationSeq)
		c.updateStatsLocked()
		mutated = true
		if c.clearDependentReadyProjectionsLocked(b.ID) {
			mutated = true
		}
	default:
		return
	}

	if mutated {
		c.markFreshLocked(time.Now())
	}
}

func (c *CachingStore) updateEventDepsLocked(eventType string, b Bead, fields map[string]json.RawMessage, refreshedFromBacking bool) bool {
	if hasCacheEventField(fields, "dependencies") || hasCacheEventField(fields, "needs") {
		return c.setEventDepsLocked(b.ID, depsFromBeadFields(b))
	}
	if eventType == "bead.created" && cacheEventLooksComplete(fields) {
		return c.setEventDepsLocked(b.ID, depsFromBeadFields(b))
	}
	if eventType == "bead.updated" && cacheEventLooksComplete(fields) {
		if refreshedFromBacking {
			return c.setEventDepsLocked(b.ID, depsFromBeadFields(b))
		}
		// bd dependency mutations arrive through the same on_update hook as
		// field changes, and the hook payload omits dependencies after removals.
		// Treat the bead's dependency coverage as unknown until the backing
		// store or reconciliation supplies an explicit dependency snapshot.
		mutated := false
		if _, ok := c.deps[b.ID]; ok {
			delete(c.deps, b.ID)
			mutated = true
		}
		if c.clearReadyProjectionLocked(b.ID) {
			mutated = true
		}
		if c.depsComplete {
			c.depsComplete = false
			mutated = true
		}
		return mutated
	}
	if _, ok := c.deps[b.ID]; ok {
		return false
	}
	if eventType == "bead.updated" && c.depsComplete {
		c.depsComplete = false
		c.recordProblemLocked("apply bead.updated event", fmt.Errorf("dependency cache marked complete but missing deps for %s", b.ID))
		return true
	}
	if !c.depsComplete {
		return false
	}
	c.depsComplete = false
	return true
}

func (c *CachingStore) setEventDepsLocked(id string, deps []Dep) bool {
	if existing, ok := c.deps[id]; ok {
		if !depsChanged(existing, deps) {
			return false
		}
		c.deps[id] = cloneDeps(deps)
		c.clearReadyProjectionLocked(id)
		return true
	}
	if c.depsComplete && len(deps) == 0 {
		return c.clearReadyProjectionLocked(id)
	}
	c.deps[id] = cloneDeps(deps)
	c.clearReadyProjectionLocked(id)
	return true
}

// ApplyDepEvent updates the dep cache for callers that have an authoritative
// dependency snapshot. bd hook payloads that omit dependency fields still flow
// through ApplyEvent and fall back to reconciliation.
func (c *CachingStore) ApplyDepEvent(beadID string, deps []Dep) {
	unlock := c.lockCloseState(beadID)
	defer unlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != cacheLive && c.state != cachePartial {
		return
	}
	c.noteMutationLocked(beadID)
	c.deps[beadID] = cloneDeps(deps)
	c.clearReadyProjectionLocked(beadID)
	c.clearStalenessMarksLocked(beadID)
	c.markFreshLocked(time.Now())
	c.updateStatsLocked()
}

func (c *CachingStore) clearReadyProjectionLocked(id string) bool {
	b, ok := c.beads[id]
	if !ok || b.IsBlocked == nil {
		return false
	}
	b.IsBlocked = nil
	c.beads[id] = b
	return true
}

func (c *CachingStore) clearAllReadyProjectionsLocked() bool {
	cleared := make([]string, 0)
	for id := range c.beads {
		if c.clearReadyProjectionLocked(id) {
			cleared = append(cleared, id)
		}
	}
	if len(cleared) == 0 {
		return false
	}
	c.noteMutationLocked(cleared...)
	return true
}

func (c *CachingStore) clearDependentReadyProjectionsLocked(dependsOnID string) bool {
	if dependsOnID == "" {
		return false
	}
	if !c.depsComplete {
		return c.clearAllReadyProjectionsLocked()
	}
	cleared := make([]string, 0)
	for id, deps := range c.deps {
		if _, ok := c.beads[id]; !ok {
			continue
		}
		for _, dep := range deps {
			if dep.DependsOnID != dependsOnID || !isReadyBlockingDependencyType(dep.Type) {
				continue
			}
			if c.clearReadyProjectionLocked(id) {
				cleared = append(cleared, id)
			}
			break
		}
	}
	if len(cleared) == 0 {
		return false
	}
	c.noteMutationLocked(cleared...)
	return true
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
	if hasCacheEventField(fields, "ephemeral") {
		merged.Ephemeral = patch.Ephemeral
	}
	if hasCacheEventField(fields, "defer_until") {
		merged.DeferUntil = cloneTimePtr(patch.DeferUntil)
	}
	if hasCacheEventField(fields, "is_blocked") {
		merged.IsBlocked = cloneBoolPtr(patch.IsBlocked)
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
	if hasCacheEventField(fields, "labels") && !stringSetEqual(current.Labels, patch.Labels) {
		return true
	}
	if hasCacheEventField(fields, "ephemeral") && current.Ephemeral != patch.Ephemeral {
		return true
	}
	if hasCacheEventField(fields, "defer_until") && !timePtrEqual(current.DeferUntil, patch.DeferUntil) {
		return true
	}
	if hasCacheEventField(fields, "is_blocked") && !boolPtrEqual(current.IsBlocked, patch.IsBlocked) {
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

func (c *CachingStore) cacheClosedEventMatchesBacking(id string) (Bead, bool, error) {
	fresh, err := c.backing.Get(id)
	if err != nil {
		return Bead{}, false, err
	}
	return fresh, fresh.Status == "closed", nil
}

func closedEventPayloadNeedsBackingRefresh(patch Bead, fresh Bead) bool {
	// Verified close events only need the backing row when the hook payload is
	// partial and the timestamp is unusable or not newer. Rich close snapshots
	// should still flow through the normal merge path so they can replace stale
	// cached fields that the backing row still carries.
	if patch.UpdatedAt.IsZero() || fresh.UpdatedAt.IsZero() || !patch.UpdatedAt.After(fresh.UpdatedAt) {
		return !closedEventCarriesRichCloseSnapshot(patch)
	}
	return false
}

func closedEventCarriesRichCloseSnapshot(patch Bead) bool {
	return patch.Title != "" ||
		len(patch.Labels) > 0 ||
		patch.Description != "" ||
		patch.Assignee != "" ||
		patch.ParentID != "" ||
		patch.Ref != "" ||
		len(patch.Needs) > 0 ||
		patch.Type != "" ||
		patch.Priority != nil ||
		patch.Ephemeral ||
		patch.NoHistory ||
		patch.DeferUntil != nil
}

func cacheEventPatchMatchesBead(current, patch Bead, fields map[string]json.RawMessage) bool {
	return !cacheEventConflictsCached(current, depsFromBeadFields(current), true, patch, fields)
}

func recentLocalMutation(mutatedAt time.Time, now time.Time) bool {
	return !mutatedAt.IsZero() && now.Sub(mutatedAt) <= 5*time.Second
}

func (c *CachingStore) recentLocalBeadConflictLocked(id string, fresh Bead, now time.Time, skipLabels bool) (Bead, bool) {
	current, ok := c.beads[id]
	if !ok {
		return Bead{}, false
	}
	if !recentLocalMutation(c.localBeadAt[id], now) {
		return Bead{}, false
	}
	if !beadChanged(current, fresh, skipLabels) {
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

// decodeCacheEvent decodes a bead.* event payload into a bead patch AND the raw
// top-level field set the cache uses for change-detection (hasCacheEventField).
// It unwraps the tolerant {"bead": ...} envelope for the fields map, then routes
// the bead itself through the shared canonical decoder so the cache and the
// run-view projection can never drift apart on the wire shape or the
// issue_type/type compat. An empty id is a decode miss (error), matching the
// prior contract.
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
	b, ok := DecodeBeadEventPayload(eventPayload)
	if !ok {
		return Bead{}, nil, fmt.Errorf("missing bead id")
	}
	return b, fields, nil
}

type cacheObserverNotification struct {
	eventType   string
	beadID      string
	runID       string
	sessionID   string
	stepID      string
	payload     json.RawMessage
	occurredAt  time.Time
	pendingGate uint64
	delivery    *cacheObserverDelivery
	deliver     func(cacheObserverNotification)
}

// cacheObserverDelivery is the nonblocking completion receipt attached to an
// ordered observer notification. Follow-up callbacks are retained until the
// observer returns, then invoked without holding either the receipt mutex or
// the notification-queue mutex.
type cacheObserverDelivery struct {
	mu        sync.Mutex
	delivered bool
	after     []func()
}

func (d *cacheObserverDelivery) AfterDelivery(fn func()) {
	if d == nil || fn == nil {
		return
	}
	d.mu.Lock()
	if !d.delivered {
		d.after = append(d.after, fn)
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()
	fn()
}

func (d *cacheObserverDelivery) markDelivered() {
	if d == nil {
		return
	}
	d.mu.Lock()
	if d.delivered {
		d.mu.Unlock()
		return
	}
	d.delivered = true
	after := d.after
	d.after = nil
	d.mu.Unlock()

	for _, fn := range after {
		fn()
	}
}

func (d *cacheObserverDelivery) isDelivered() bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.delivered
}

func (c *CachingStore) prepareObserverNotification(eventType string, b Bead) (cacheObserverNotification, bool) {
	return c.prepareObserverNotificationAt(eventType, b, time.Time{})
}

func (c *CachingStore) prepareObserverNotificationAt(eventType string, b Bead, occurredAt time.Time) (cacheObserverNotification, bool) {
	if c.onChange == nil {
		return cacheObserverNotification{}, false
	}
	payload, err := json.Marshal(b)
	if err != nil {
		c.recordProblem(fmt.Sprintf("marshal %s notification", eventType), err)
		return cacheObserverNotification{}, false
	}
	// Resolve the opaque run/session correlation ids from the bead's metadata at
	// the record site and pass ONLY those two ids to onChange — never the
	// free-form metadata map. The run-chain (workflow_id || molecule_id ||
	// gc.root_bead_id || bead.ID) always resolves to a non-empty id since b.ID is
	// non-empty; session id is a direct, optional metadata read. Both are
	// safeRef-gated again at the export boundary.
	runID := beadmeta.ResolveRunID(b.Metadata, b.ID, "")
	sessionID := b.Metadata[beadmeta.SessionIDMetadataKey]
	// step_id is the acting work bead the lifecycle event is about: a work/dispatch
	// bead carries its own gc.step_id, so a bead.created/closed on one stamps that
	// step. Non-work beads (sessions, mail, …) carry none → empty, omitted at export.
	stepID := b.Metadata[beadmeta.StepIDMetadataKey]
	if occurredAt.IsZero() {
		occurredAt = beadEventOccurrenceTime(eventType, b)
	}
	return cacheObserverNotification{
		eventType:  eventType,
		beadID:     b.ID,
		runID:      runID,
		sessionID:  sessionID,
		stepID:     stepID,
		payload:    payload,
		occurredAt: occurredAt.UTC(),
		deliver:    c.deliverObserverNotification,
	}, true
}

func beadEventOccurrenceTime(eventType string, b Bead) time.Time {
	switch eventType {
	case "bead.created":
		if !b.CreatedAt.IsZero() {
			return b.CreatedAt.UTC()
		}
	case "bead.updated", "bead.closed":
		if !b.UpdatedAt.IsZero() {
			return b.UpdatedAt.UTC()
		}
	}
	return time.Now().UTC()
}

func (c *CachingStore) deliverObserverNotification(notification cacheObserverNotification) {
	c.onChange(
		notification.eventType,
		notification.beadID,
		notification.runID,
		notification.sessionID,
		notification.stepID,
		notification.payload,
		notification.occurredAt,
	)
}

func (c *CachingStore) notifyChange(eventType string, b Bead) {
	notification, ok := c.prepareObserverNotification(eventType, b)
	if !ok {
		return
	}
	c.deliverObserverNotification(notification)
}

// enqueueOrderedChange reserves an ordinary notification without consuming an
// older failed-refresh intent. The returned drain function must run only after
// the caller releases its mutation lock. A nil drain means another callback is
// already draining this ID; it will deliver the queued notification before
// finishing.
func (c *CachingStore) enqueueOrderedChange(eventType string, b Bead) (drain func(), delivery *cacheObserverDelivery) {
	return c.enqueueOrderedChangeAt(eventType, b, time.Time{})
}

func (c *CachingStore) enqueueOrderedChangeAt(eventType string, b Bead, occurredAt time.Time) (drain func(), delivery *cacheObserverDelivery) {
	return c.enqueueOrderedChangeResolvingPending(eventType, b, occurredAt, false)
}

// enqueueOrderedCompleteChange may resolve an older pending intent because b is
// an authoritative, dependency-complete snapshot obtained from
// readBeadWithDeps or an equivalent complete transition contract.
func (c *CachingStore) enqueueOrderedCompleteChange(eventType string, b Bead) (drain func(), delivery *cacheObserverDelivery) {
	return c.enqueueOrderedCompleteChangeAt(eventType, b, time.Time{})
}

func (c *CachingStore) enqueueOrderedCompleteChangeAt(eventType string, b Bead, occurredAt time.Time) (drain func(), delivery *cacheObserverDelivery) {
	return c.enqueueOrderedChangeResolvingPending(eventType, b, occurredAt, true)
}

// enqueueOrderedChangeResolvingPending reserves an exact observer snapshot and,
// when requested, delivers any older failed-refresh publication for the same
// bead. The resolving mutation's OWN current event is the newest snapshot for
// the bead, so it must deliver after everything already queued: the retained
// pending events take the gate's reserved position, and the current event goes
// to the queue tail unless it can coalesce onto the final pending event with no
// intervening notification for that ID. Callers that resolve pending state hold
// the shared mutation scope and per-ID ordering lock through this reservation.
func (c *CachingStore) enqueueOrderedChangeResolvingPending(eventType string, b Bead, occurredAt time.Time, resolvePending bool) (drain func(), delivery *cacheObserverDelivery) {
	pending, hasPending := cachePendingPublication{}, false
	if resolvePending && c.pendingPublications != nil {
		pending, hasPending = c.pendingPublications.current(b.ID)
	}
	if occurredAt.IsZero() {
		occurredAt = beadEventOccurrenceTime(eventType, b)
	}

	currentNotification, ok := c.prepareObserverNotificationAt(eventType, b, occurredAt)
	if !ok {
		return nil, nil
	}
	if !hasPending {
		return c.enqueueOrderedNotification(currentNotification)
	}

	pendingEvents := pending.events()
	if len(pendingEvents) == 0 {
		// A gate with no recorded events cannot happen through retain (it always
		// merges at least one event), but guard defensively: release the gate,
		// clear the intent, and deliver only the current event at the tail.
		gateDrain := c.releaseOrderedPendingGate(b.ID, pending.gate)
		c.pendingPublications.clearIfToken(b.ID, pending.token)
		tailDrain, tailDelivery := c.enqueueOrderedNotification(currentNotification)
		if gateDrain != nil {
			return gateDrain, tailDelivery
		}
		return tailDrain, tailDelivery
	}

	last := len(pendingEvents) - 1
	sameTypeCoalesce := pendingEvents[last].eventType == eventType
	createdSubsumesUpdate := len(pendingEvents) == 1 &&
		pendingEvents[0].eventType == "bead.created" && eventType == "bead.updated"
	currentCoalesces := sameTypeCoalesce || createdSubsumesUpdate

	// The current event may coalesce onto the gate position ONLY when nothing was
	// enqueued for this bead between the gate and the tail; an intervening
	// notification means the current (newest) snapshot must deliver last, so it is
	// split off to the tail. Intermediate notifications can only be drained (never
	// added) while the caller holds the per-ID scope, so a stale "true" at most
	// forces a harmless extra delivery — it never lets the current overtake a live
	// intermediate.
	coalesceAtGate := currentCoalesces && !c.pendingGateHasIntermediateNotification(b.ID, pending.gate)

	var gateNotifications []cacheObserverNotification
	for i, pendingEvent := range pendingEvents {
		if coalesceAtGate && sameTypeCoalesce && i == last {
			// The current snapshot satisfies the final pending publication; it takes
			// the gate position and carries the current occurrence time.
			gateNotifications = append(gateNotifications, currentNotification)
			continue
		}
		notification, ok := c.prepareObserverNotificationAt(pendingEvent.eventType, b, pendingEvent.occurredAt)
		if !ok {
			return nil, nil
		}
		gateNotifications = append(gateNotifications, notification)
	}

	gateDrain, deliveries, reserved := c.resolveOrderedPendingGate(b.ID, pending.gate, gateNotifications)
	if !reserved {
		c.recordProblem("resolve pending observer gate", fmt.Errorf("%s: gate %d missing", b.ID, pending.gate))
		return nil, nil
	}
	drain = gateDrain
	if coalesceAtGate {
		// The final gate-position notification carries the current event (same type
		// took the current snapshot; created-subsumes-update publishes the one
		// complete creation), so its receipt is the current event's receipt.
		delivery = deliveries[len(deliveries)-1]
	} else {
		tailDrain, tailDelivery := c.enqueueOrderedNotification(currentNotification)
		if drain == nil {
			drain = tailDrain
		}
		delivery = tailDelivery
	}

	if !c.pendingPublications.clearIfToken(b.ID, pending.token) {
		// The intent advanced concurrently (believed unreachable under the
		// mutation-scope locking; defense in depth). The surviving intent carries
		// the same gate number, which this call just consumed, so re-reserve a
		// placeholder for it — an end-of-queue position is acceptable degraded
		// ordering — otherwise its later recovery would hit "gate missing" and drop
		// the publication.
		c.reserveOrderedPendingGate(b.ID, pending.gate)
		c.recordProblem("clear pending observer publication", fmt.Errorf("%s: intent %d changed during reservation", b.ID, pending.token))
	}
	return drain, delivery
}

// pendingGateHasIntermediateNotification reports whether a deliverable
// notification for id sits behind its reserved gate in the ordered queue. It is
// how gate resolution decides whether the resolving mutation's current event can
// coalesce onto the gate position or must be split off to the tail so it does
// not overtake a strictly older queued snapshot.
func (c *CachingStore) pendingGateHasIntermediateNotification(id string, gate uint64) bool {
	c.orderedNotificationMu.Lock()
	defer c.orderedNotificationMu.Unlock()
	queue := c.orderedNotificationQueues[id]
	if queue == nil {
		return false
	}
	gateIndex := -1
	for i := range queue.pending {
		if queue.pending[i].pendingGate == gate {
			gateIndex = i
			break
		}
	}
	if gateIndex < 0 {
		return false
	}
	for i := gateIndex + 1; i < len(queue.pending); i++ {
		// Only a deliverable snapshot notification can be overtaken. Barriers
		// (empty eventType receipts) and other gate placeholders carry no snapshot,
		// so they do not force the current event off the gate position.
		if queue.pending[i].pendingGate == 0 && queue.pending[i].eventType != "" {
			return true
		}
	}
	return false
}

// reserveOrderedPendingGate puts a non-delivering placeholder at the durable
// mutation's original observer position. Draining stops at an unresolved gate,
// so later notifications and barriers cannot overtake a failed-refresh event.
func (c *CachingStore) reserveOrderedPendingGate(id string, gate uint64) {
	if id == "" || gate == 0 {
		return
	}
	c.orderedNotificationMu.Lock()
	defer c.orderedNotificationMu.Unlock()
	if c.orderedNotificationQueues == nil {
		c.orderedNotificationQueues = make(map[string]*cacheOrderedNotificationQueue)
	}
	queue := c.orderedNotificationQueues[id]
	if queue == nil {
		queue = &cacheOrderedNotificationQueue{}
		c.orderedNotificationQueues[id] = queue
	}
	for _, notification := range queue.pending {
		if notification.pendingGate == gate {
			return
		}
	}
	queue.pending = append(queue.pending, cacheObserverNotification{beadID: id, pendingGate: gate})
}

// resolveOrderedPendingGate atomically replaces an unresolved placeholder with
// exact prepared notifications at the same queue position.
func (c *CachingStore) resolveOrderedPendingGate(id string, gate uint64, notifications []cacheObserverNotification) (drain func(), deliveries []*cacheObserverDelivery, ok bool) {
	if id == "" || gate == 0 || len(notifications) == 0 {
		return nil, nil, false
	}
	c.orderedNotificationMu.Lock()
	queue := c.orderedNotificationQueues[id]
	if queue == nil {
		c.orderedNotificationMu.Unlock()
		return nil, nil, false
	}
	gateIndex := -1
	for i := range queue.pending {
		if queue.pending[i].pendingGate == gate {
			gateIndex = i
			break
		}
	}
	if gateIndex < 0 {
		c.orderedNotificationMu.Unlock()
		return nil, nil, false
	}

	prepared := make([]cacheObserverNotification, len(notifications))
	deliveries = make([]*cacheObserverDelivery, len(notifications))
	for i, notification := range notifications {
		delivery := &cacheObserverDelivery{}
		notification.delivery = delivery
		prepared[i] = notification
		deliveries[i] = delivery
	}
	replacement := make([]cacheObserverNotification, 0, len(queue.pending)-1+len(prepared))
	replacement = append(replacement, queue.pending[:gateIndex]...)
	replacement = append(replacement, prepared...)
	replacement = append(replacement, queue.pending[gateIndex+1:]...)
	queue.pending = replacement
	if queue.draining {
		c.orderedNotificationMu.Unlock()
		return nil, deliveries, true
	}
	queue.draining = true
	c.orderedNotificationMu.Unlock()
	return func() { c.drainOrderedChanges(id, queue) }, deliveries, true
}

// releaseOrderedPendingGate removes an unresolved gate placeholder for an
// abandoned pending publication (its bead was deleted out-of-band, so no
// replacement notification will ever resolve it). If deliverable notifications
// remain queued and no callback is draining, it claims the drain and returns a
// drain func with the same contract as resolveOrderedPendingGate; the caller
// runs it after releasing its locks so the blocked notifications flow. An
// emptied queue is deleted.
func (c *CachingStore) releaseOrderedPendingGate(id string, gate uint64) func() {
	if id == "" || gate == 0 {
		return nil
	}
	c.orderedNotificationMu.Lock()
	queue := c.orderedNotificationQueues[id]
	if queue == nil {
		c.orderedNotificationMu.Unlock()
		return nil
	}
	gateIndex := -1
	for i := range queue.pending {
		if queue.pending[i].pendingGate == gate {
			gateIndex = i
			break
		}
	}
	if gateIndex < 0 {
		c.orderedNotificationMu.Unlock()
		return nil
	}
	queue.pending = slices.Delete(queue.pending, gateIndex, gateIndex+1)
	if len(queue.pending) == 0 {
		if c.orderedNotificationQueues[id] == queue {
			delete(c.orderedNotificationQueues, id)
		}
		c.orderedNotificationMu.Unlock()
		return nil
	}
	if queue.draining {
		c.orderedNotificationMu.Unlock()
		return nil
	}
	queue.draining = true
	c.orderedNotificationMu.Unlock()
	return func() { c.drainOrderedChanges(id, queue) }
}

// enqueueOrderedBarrier reserves a receipt in the same per-ID queue as
// observer notifications without invoking the observer itself. It lets a
// caller that deliberately suppressed a notification publish its replacement
// only after every earlier snapshot for the ID has been delivered.
func (c *CachingStore) enqueueOrderedBarrier(id string) (drain func(), delivery *cacheObserverDelivery) {
	return c.enqueueOrderedNotification(cacheObserverNotification{beadID: id})
}

func (c *CachingStore) enqueueOrderedNotification(notification cacheObserverNotification) (drain func(), delivery *cacheObserverDelivery) {
	delivery = &cacheObserverDelivery{}
	notification.delivery = delivery

	c.orderedNotificationMu.Lock()
	if c.orderedNotificationQueues == nil {
		c.orderedNotificationQueues = make(map[string]*cacheOrderedNotificationQueue)
	}
	queue := c.orderedNotificationQueues[notification.beadID]
	if queue == nil {
		queue = &cacheOrderedNotificationQueue{}
		c.orderedNotificationQueues[notification.beadID] = queue
	}
	queue.pending = append(queue.pending, notification)
	if queue.draining {
		c.orderedNotificationMu.Unlock()
		return nil, delivery
	}
	queue.draining = true
	c.orderedNotificationMu.Unlock()

	return func() { c.drainOrderedChanges(notification.beadID, queue) }, delivery
}

func (c *CachingStore) drainOrderedChanges(id string, queue *cacheOrderedNotificationQueue) {
	for {
		c.orderedNotificationMu.Lock()
		if len(queue.pending) == 0 {
			if c.orderedNotificationQueues[id] == queue {
				delete(c.orderedNotificationQueues, id)
			}
			queue.draining = false
			c.orderedNotificationMu.Unlock()
			return
		}
		notification := queue.pending[0]
		if notification.pendingGate != 0 {
			queue.draining = false
			c.orderedNotificationMu.Unlock()
			return
		}
		queue.pending[0] = cacheObserverNotification{}
		queue.pending = queue.pending[1:]
		c.orderedNotificationMu.Unlock()

		if notification.eventType != "" && notification.deliver != nil {
			notification.deliver(notification)
		}
		notification.delivery.markDelivered()
	}
}

type cacheNotification struct {
	eventType      string
	bead           Bead
	occurredAt     time.Time
	resolvePending bool
}

// enqueueOrderedChanges reserves a batch of per-ID notifications in mutation
// order. Callers reserve while holding their mutation scope, release that
// scope, and only then invoke the returned drain functions.
func (c *CachingStore) enqueueOrderedChanges(notifications []cacheNotification) []func() {
	drains := make([]func(), 0, len(notifications))
	for _, notification := range notifications {
		var drain func()
		if notification.resolvePending {
			drain, _ = c.enqueueOrderedCompleteChangeAt(notification.eventType, notification.bead, notification.occurredAt)
		} else {
			drain, _ = c.enqueueOrderedChangeAt(notification.eventType, notification.bead, notification.occurredAt)
		}
		if drain != nil {
			drains = append(drains, drain)
		}
	}
	return drains
}

// enqueueReconcileChanges reserves reconciliation notifications without
// allowing an incomplete ordinary diff to consume a failed-refresh intent.
// Only a notification built from a targeted complete recovery opts in.
func (c *CachingStore) enqueueReconcileChanges(notifications []cacheNotification) []func() {
	drains := make([]func(), 0, len(notifications))
	for _, notification := range notifications {
		var drain func()
		if notification.resolvePending {
			drain, _ = c.enqueueOrderedCompleteChangeAt(notification.eventType, notification.bead, notification.occurredAt)
		} else {
			drain, _ = c.enqueueOrderedChangeAt(notification.eventType, notification.bead, notification.occurredAt)
		}
		if drain != nil {
			drains = append(drains, drain)
		}
	}
	return drains
}

func beadChanged(old, fresh Bead, skipLabels bool) bool {
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
		old.Description != fresh.Description ||
		old.Ephemeral != fresh.Ephemeral ||
		!timePtrEqual(old.DeferUntil, fresh.DeferUntil) ||
		!boolPtrEqual(old.IsBlocked, fresh.IsBlocked) {
		return true
	}
	if !maps.Equal(old.Metadata, fresh.Metadata) {
		return true
	}
	// Labels, needs, and dependencies are SETS: their order carries no meaning.
	// Compare them order-insensitively. A backing store that returns these in a
	// different order than the cache holds (the Dolt gcg rig store does not
	// guarantee a stable order across scans) would otherwise register as a
	// spurious change. For needs and dependencies that misfires on every
	// reconcile pass — the cache-reconcile re-absorb churn that needlessly
	// re-touched live molecule wisps (ga-ocypq2). Labels are skipped during
	// reconcile (skipLabels: true) and so matter only for the skipLabels:false
	// change checks.
	if !skipLabels && !stringSetEqual(old.Labels, fresh.Labels) {
		return true
	}
	if !stringSetEqual(old.Needs, fresh.Needs) {
		return true
	}
	return !depSetEqual(old.Dependencies, fresh.Dependencies)
}

func depsChanged(old, fresh []Dep) bool {
	return !depSetEqual(old, fresh)
}

// stringSetEqual reports whether two string slices hold the same multiset of
// values regardless of order. Used for order-insensitive label/needs change
// detection so a store returning a set in a different order than the cache is
// not mistaken for a change (ga-ocypq2).
func stringSetEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, s := range a {
		counts[s]++
	}
	for _, s := range b {
		counts[s]--
		if counts[s] < 0 {
			return false
		}
	}
	return true
}

// depSetEqual reports whether two dependency slices hold the same multiset of
// dependencies regardless of order. Dep is a comparable struct, so it is a
// valid map key for the multiset count.
func depSetEqual(a, b []Dep) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[Dep]int, len(a))
	for _, d := range a {
		counts[d]++
	}
	for _, d := range b {
		counts[d]--
		if counts[d] < 0 {
			return false
		}
	}
	return true
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

func boolPtrEqual(left, right *bool) bool {
	switch {
	case left == nil && right == nil:
		return true
	case left == nil || right == nil:
		return false
	default:
		return *left == *right
	}
}

func timePtrEqual(left, right *time.Time) bool {
	switch {
	case left == nil && right == nil:
		return true
	case left == nil || right == nil:
		return false
	default:
		return left.Equal(*right)
	}
}
