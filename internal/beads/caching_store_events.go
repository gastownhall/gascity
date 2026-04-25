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

	c.mu.RLock()
	if c.state != cacheLive {
		c.mu.RUnlock()
		return
	}
	_, cached := c.beads[patch.ID]
	c.mu.RUnlock()

	b := patch
	if !cached {
		if fresh, err := c.backing.Get(patch.ID); err == nil {
			b = fresh
		} else if !errors.Is(err, ErrNotFound) {
			c.recordProblem(fmt.Sprintf("refresh %s event", eventType), err)
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != cacheLive {
		return
	}
	if current, ok := c.beads[patch.ID]; ok {
		b = mergeCacheEventPatch(current, patch, fields)
	}

	mutated := false
	switch eventType {
	case "bead.created":
		if _, exists := c.beads[b.ID]; !exists {
			c.noteMutationLocked(b.ID)
			c.beads[b.ID] = cloneBead(b)
			delete(c.dirty, b.ID)
			delete(c.deletedSeq, b.ID)
		}
		c.updateStatsLocked()
		mutated = true
	case "bead.updated":
		c.noteMutationLocked(b.ID)
		c.beads[b.ID] = cloneBead(b)
		delete(c.dirty, b.ID)
		delete(c.deletedSeq, b.ID)
		mutated = true
	case "bead.closed":
		c.noteMutationLocked(b.ID)
		if _, exists := c.beads[b.ID]; !exists {
			c.updateStatsLocked()
		}
		c.beads[b.ID] = cloneBead(b)
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

// ApplyDepEvent updates the dep cache for a bead. Call after dep
// mutations are detected via events or write-through.
func (c *CachingStore) ApplyDepEvent(beadID string, deps []Dep) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != cacheLive {
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

func hasCacheEventField(fields map[string]json.RawMessage, name string) bool {
	_, ok := fields[name]
	return ok
}

func decodeCacheEvent(payload json.RawMessage) (Bead, map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return Bead{}, nil, err
	}
	var wire struct {
		Bead
		Metadata   StringMap `json:"metadata,omitempty"`
		TypeCompat string    `json:"type,omitempty"`
	}
	if err := json.Unmarshal(payload, &wire); err != nil {
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
