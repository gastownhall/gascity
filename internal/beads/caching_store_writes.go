package beads

import (
	"errors"
	"fmt"
	"time"
)

// Create passes through to the backing store and updates the cache.
func (c *CachingStore) Create(b Bead) (Bead, error) {
	created, err := c.backing.Create(b)
	if err != nil {
		return created, err
	}

	if fresh, err := c.backing.Get(created.ID); err == nil {
		created = fresh
	} else if !errors.Is(err, ErrNotFound) {
		c.recordProblem("refresh bead after create", fmt.Errorf("%s: %w", created.ID, err))
	}

	c.mu.Lock()
	c.noteLocalMutationLocked(created.ID)
	c.beads[created.ID] = cloneBead(created)
	delete(c.dirty, created.ID)
	delete(c.deletedSeq, created.ID)
	c.markFreshLocked(time.Now())
	c.updateStatsLocked()
	c.mu.Unlock()

	c.notifyChange("bead.created", created)
	return created, nil
}

// Update passes through to the backing store and refreshes the cache.
func (c *CachingStore) Update(id string, opts UpdateOpts) error {
	if err := c.backing.Update(id, opts); err != nil {
		return err
	}

	// Re-fetch from backing to get the authoritative state.
	fresh, err := c.backing.Get(id)
	if err != nil {
		c.mu.Lock()
		c.dirty[id] = struct{}{}
		c.mu.Unlock()
		c.recordProblem("refresh bead after update", fmt.Errorf("%s: %w", id, err))
		return nil
	}
	fresh = applyUpdateOptsToBead(fresh, opts)

	c.mu.Lock()
	c.noteLocalMutationLocked(id)
	c.beads[id] = cloneBead(fresh)
	delete(c.dirty, id)
	delete(c.deletedSeq, id)
	c.markFreshLocked(time.Now())
	c.updateStatsLocked()
	c.mu.Unlock()

	c.notifyChange("bead.updated", fresh)
	return nil
}

// Close marks a bead as closed in the backing store and cache.
func (c *CachingStore) Close(id string) error {
	if err := c.backing.Close(id); err != nil {
		return err
	}

	var closed Bead
	var found bool
	if fresh, err := c.backing.Get(id); err == nil {
		closed = fresh
		closed.Status = "closed"
		found = true
	} else if !errors.Is(err, ErrNotFound) {
		c.recordProblem("refresh bead after close", fmt.Errorf("%s: %w", id, err))
	}

	c.mu.Lock()
	c.noteLocalMutationLocked(id)
	if b, ok := c.beads[id]; ok {
		b.Status = "closed"
		c.beads[id] = b
		delete(c.dirty, id)
		delete(c.deletedSeq, id)
		closed = cloneBead(b)
		found = true
		c.markFreshLocked(time.Now())
		c.updateStatsLocked()
	} else if found {
		c.beads[id] = cloneBead(closed)
		delete(c.dirty, id)
		delete(c.deletedSeq, id)
		c.markFreshLocked(time.Now())
		c.updateStatsLocked()
	}
	c.mu.Unlock()

	if found {
		c.notifyChange("bead.closed", closed)
	}
	return nil
}

// Reopen marks a bead as open in the backing store and cache.
func (c *CachingStore) Reopen(id string) error {
	if err := c.backing.Reopen(id); err != nil {
		return err
	}

	var reopened Bead
	var found bool
	if fresh, err := c.backing.Get(id); err == nil {
		reopened = fresh
		reopened.Status = "open"
		found = true
	} else if !errors.Is(err, ErrNotFound) {
		c.recordProblem("refresh bead after reopen", fmt.Errorf("%s: %w", id, err))
	}

	c.mu.Lock()
	c.noteLocalMutationLocked(id)
	if b, ok := c.beads[id]; ok {
		b.Status = "open"
		c.beads[id] = b
		delete(c.dirty, id)
		delete(c.deletedSeq, id)
		reopened = cloneBead(b)
		found = true
		c.markFreshLocked(time.Now())
		c.updateStatsLocked()
	} else if found {
		c.beads[id] = cloneBead(reopened)
		delete(c.dirty, id)
		delete(c.deletedSeq, id)
		c.markFreshLocked(time.Now())
		c.updateStatsLocked()
	}
	c.mu.Unlock()

	if found {
		c.notifyChange("bead.updated", reopened)
	}
	return nil
}

// CloseAll closes multiple beads and sets metadata on each.
func (c *CachingStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	n, err := c.backing.CloseAll(ids, metadata)
	if err != nil && n == 0 {
		return n, err
	}

	type refreshedBead struct {
		id   string
		bead Bead
	}
	refreshed := make([]refreshedBead, 0, len(ids))
	var refreshErr error
	refreshFailed := make(map[string]struct{})
	for _, id := range ids {
		fresh, getErr := c.backing.Get(id)
		if getErr != nil {
			refreshFailed[id] = struct{}{}
			refreshErr = errors.Join(refreshErr, fmt.Errorf("refresh bead after close-all %s: %w", id, getErr))
			continue
		}
		refreshed = append(refreshed, refreshedBead{id: id, bead: fresh})
	}

	notifications := make([]cacheNotification, 0, len(refreshed))
	c.mu.Lock()
	c.noteLocalMutationLocked(ids...)
	if refreshErr != nil {
		c.recordProblemLocked("close-all refresh", refreshErr)
	}
	for id := range refreshFailed {
		c.dirty[id] = struct{}{}
	}
	for _, item := range refreshed {
		previous, hadPrevious := c.beads[item.id]
		c.beads[item.id] = cloneBead(item.bead)
		delete(c.dirty, item.id)
		delete(c.deletedSeq, item.id)
		if item.bead.Status == "closed" {
			delete(c.deps, item.id)
		}
		if hadPrevious && previous.Status != "closed" && item.bead.Status == "closed" {
			notifications = append(notifications, cacheNotification{
				eventType: "bead.closed",
				bead:      cloneBead(item.bead),
			})
		}
	}
	c.markFreshLocked(time.Now())
	c.updateStatsLocked()
	c.mu.Unlock()
	c.notifyChanges(notifications)
	return n, errors.Join(err, refreshErr)
}

// SetMetadata sets a single metadata key-value on a bead.
func (c *CachingStore) SetMetadata(id, key, value string) error {
	if err := c.backing.SetMetadata(id, key, value); err != nil {
		return err
	}

	c.mu.Lock()
	c.noteLocalMutationLocked(id)
	if b, ok := c.beads[id]; ok {
		if b.Metadata == nil {
			b.Metadata = make(map[string]string)
		}
		b.Metadata[key] = value
		c.beads[id] = b
		delete(c.dirty, id)
		delete(c.deletedSeq, id)
	}
	c.markFreshLocked(time.Now())
	c.updateStatsLocked()
	c.mu.Unlock()
	return nil
}

// SetMetadataBatch sets multiple metadata key-values on a bead.
func (c *CachingStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if err := c.backing.SetMetadataBatch(id, kvs); err != nil {
		return err
	}

	c.mu.Lock()
	c.noteLocalMutationLocked(id)
	if b, ok := c.beads[id]; ok {
		if b.Metadata == nil {
			b.Metadata = make(map[string]string, len(kvs))
		}
		for k, v := range kvs {
			b.Metadata[k] = v
		}
		c.beads[id] = b
		delete(c.dirty, id)
		delete(c.deletedSeq, id)
	}
	c.markFreshLocked(time.Now())
	c.updateStatsLocked()
	c.mu.Unlock()
	return nil
}

// DepAdd adds a dependency and updates the cache.
func (c *CachingStore) DepAdd(issueID, dependsOnID, depType string) error {
	if err := c.backing.DepAdd(issueID, dependsOnID, depType); err != nil {
		return err
	}

	c.mu.Lock()
	c.noteLocalMutationLocked(issueID)
	if !c.depsComplete {
		delete(c.deps, issueID)
		delete(c.dirty, issueID)
		delete(c.deletedSeq, issueID)
		c.markFreshLocked(time.Now())
		c.updateStatsLocked()
		c.mu.Unlock()
		return nil
	}
	deps := c.deps[issueID]
	for i, d := range deps {
		if d.DependsOnID == dependsOnID {
			deps[i].Type = depType
			c.deps[issueID] = deps
			delete(c.dirty, issueID)
			delete(c.deletedSeq, issueID)
			c.markFreshLocked(time.Now())
			c.updateStatsLocked()
			c.mu.Unlock()
			return nil
		}
	}
	c.deps[issueID] = append(deps, Dep{IssueID: issueID, DependsOnID: dependsOnID, Type: depType})
	delete(c.dirty, issueID)
	delete(c.deletedSeq, issueID)
	c.markFreshLocked(time.Now())
	c.updateStatsLocked()
	c.mu.Unlock()
	return nil
}

// DepRemove removes a dependency and updates the cache.
func (c *CachingStore) DepRemove(issueID, dependsOnID string) error {
	if err := c.backing.DepRemove(issueID, dependsOnID); err != nil {
		return err
	}

	c.mu.Lock()
	c.noteLocalMutationLocked(issueID)
	if !c.depsComplete {
		delete(c.deps, issueID)
		delete(c.dirty, issueID)
		delete(c.deletedSeq, issueID)
		c.markFreshLocked(time.Now())
		c.updateStatsLocked()
		c.mu.Unlock()
		return nil
	}
	deps := c.deps[issueID]
	for i, d := range deps {
		if d.DependsOnID == dependsOnID {
			c.deps[issueID] = append(deps[:i], deps[i+1:]...)
			delete(c.dirty, issueID)
			delete(c.deletedSeq, issueID)
			break
		}
	}
	c.markFreshLocked(time.Now())
	c.updateStatsLocked()
	c.mu.Unlock()
	return nil
}

// Delete passes through to the backing store and removes from cache.
func (c *CachingStore) Delete(id string) error {
	if err := c.backing.Delete(id); err != nil {
		return err
	}

	c.mu.Lock()
	seq := c.noteLocalMutationLocked(id)
	delete(c.beads, id)
	delete(c.deps, id)
	delete(c.dirty, id)
	delete(c.beadSeq, id)
	c.deletedSeq[id] = seq
	c.markFreshLocked(time.Now())
	c.updateStatsLocked()
	c.mu.Unlock()
	return nil
}

func applyUpdateOptsToBead(bead Bead, opts UpdateOpts) Bead {
	if opts.Title != nil {
		bead.Title = *opts.Title
	}
	if opts.Status != nil {
		bead.Status = *opts.Status
	}
	if opts.Type != nil {
		bead.Type = *opts.Type
	}
	if opts.Priority != nil {
		bead.Priority = cloneIntPtr(opts.Priority)
	}
	if opts.Description != nil {
		bead.Description = *opts.Description
	}
	if opts.ParentID != nil {
		bead.ParentID = *opts.ParentID
	}
	if opts.Assignee != nil {
		bead.Assignee = *opts.Assignee
	}
	if len(opts.Metadata) > 0 {
		if bead.Metadata == nil {
			bead.Metadata = make(map[string]string, len(opts.Metadata))
		}
		for key, value := range opts.Metadata {
			bead.Metadata[key] = value
		}
	}
	if len(opts.Labels) > 0 || len(opts.RemoveLabels) > 0 {
		remove := make(map[string]struct{}, len(opts.RemoveLabels))
		for _, label := range opts.RemoveLabels {
			remove[label] = struct{}{}
		}

		labels := make([]string, 0, len(bead.Labels)+len(opts.Labels))
		seen := make(map[string]struct{}, len(bead.Labels)+len(opts.Labels))
		for _, label := range bead.Labels {
			if _, drop := remove[label]; drop {
				continue
			}
			if _, exists := seen[label]; exists {
				continue
			}
			labels = append(labels, label)
			seen[label] = struct{}{}
		}
		for _, label := range opts.Labels {
			if _, drop := remove[label]; drop {
				continue
			}
			if _, exists := seen[label]; exists {
				continue
			}
			labels = append(labels, label)
			seen[label] = struct{}{}
		}
		bead.Labels = labels
	}
	return bead
}
