package beads

import (
	"encoding/json"
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

	b, err := decodeCacheEvent(payload)
	if err != nil {
		c.recordProblem(fmt.Sprintf("apply %s event", eventType), err)
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != cacheLive {
		return
	}

	switch eventType {
	case "bead.created":
		if _, exists := c.beads[b.ID]; !exists {
			c.beads[b.ID] = cloneBead(b)
			delete(c.dirty, b.ID)
			c.updateStatsLocked()
		}
	case "bead.updated":
		c.beads[b.ID] = cloneBead(b)
		delete(c.dirty, b.ID)
	case "bead.closed":
		if _, exists := c.beads[b.ID]; !exists {
			c.updateStatsLocked()
		}
		c.beads[b.ID] = cloneBead(b)
		delete(c.dirty, b.ID)
	default:
		return
	}

	c.markFreshLocked(time.Now())
}

// ApplyDepEvent updates the dep cache for a bead. Call after dep
// mutations are detected via events or write-through.
func (c *CachingStore) ApplyDepEvent(beadID string, deps []Dep) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != cacheLive {
		return
	}
	c.deps[beadID] = cloneDeps(deps)
	delete(c.dirty, beadID)
	c.markFreshLocked(time.Now())
	c.updateStatsLocked()
}

func decodeCacheEvent(payload json.RawMessage) (Bead, error) {
	var wire struct {
		Bead
		Metadata   StringMap `json:"metadata,omitempty"`
		TypeCompat string    `json:"type,omitempty"`
	}
	if err := json.Unmarshal(payload, &wire); err != nil {
		return Bead{}, err
	}
	b := wire.Bead
	if wire.Metadata != nil {
		b.Metadata = map[string]string(wire.Metadata)
	}
	if b.ID == "" {
		return Bead{}, fmt.Errorf("missing bead id")
	}
	// bd hook payloads use "issue_type" while exec-style payloads may use "type".
	if b.Type == "" && wire.TypeCompat != "" {
		b.Type = wire.TypeCompat
	}
	return b, nil
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
