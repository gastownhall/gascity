package beads

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
)

// MemStore is an in-memory Store implementation backed by a slice. It is
// exported for use as a test double in cross-package tests. It is safe for
// concurrent use.
type MemStore struct {
	mu    sync.Mutex
	beads []Bead
	deps  []Dep
	seq   int
}

// NewMemStore returns a new empty MemStore.
func NewMemStore() *MemStore {
	return &MemStore{}
}

// NewMemStoreFrom returns a MemStore seeded with existing beads, deps, and
// sequence counter. Used by FileStore to restore state from disk.
func NewMemStoreFrom(seq int, existing []Bead, deps []Dep) *MemStore {
	b := make([]Bead, len(existing))
	copy(b, existing)
	d := make([]Dep, len(deps))
	copy(d, deps)
	return &MemStore{seq: seq, beads: b, deps: d}
}

// restoreFrom replaces the in-memory state with the given snapshot.
// Used by FileStore to roll back mutations when a disk flush fails.
func (m *MemStore) restoreFrom(seq int, beads []Bead, deps []Dep) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq = seq
	m.beads = beads
	m.deps = deps
}

// snapshot returns the current sequence counter, a deep copy of all beads, and
// a copy of all deps. Used by FileStore for serialization. Caller must hold m.mu.
func (m *MemStore) snapshot() (int, []Bead, []Dep) {
	b := make([]Bead, len(m.beads))
	for i, bead := range m.beads {
		b[i] = cloneBead(bead)
	}
	d := make([]Dep, len(m.deps))
	copy(d, m.deps)
	return m.seq, b, d
}

// cloneBead returns a deep copy of a bead, cloning reference fields
// (Metadata, Labels, Needs) to prevent shared-state races between callers
// and the store.
func cloneBead(b Bead) Bead {
	b.Priority = cloneIntPtr(b.Priority)
	b.Metadata = maps.Clone(b.Metadata)
	b.Labels = slices.Clone(b.Labels)
	b.Needs = slices.Clone(b.Needs)
	return b
}

// Create persists a new bead in memory with a sequential ID.
func (m *MemStore) Create(b Bead) (Bead, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.seq++
	b.ID = fmt.Sprintf("gc-%d", m.seq)
	b.Status = "open"
	if b.Type == "" {
		b.Type = "task"
	}
	b.CreatedAt = time.Now()

	stored := cloneBead(b)
	m.beads = append(m.beads, stored)
	for _, need := range stored.Needs {
		depType := "blocks"
		dependsOnID := need
		if strings.Contains(need, ":") {
			parts := strings.SplitN(need, ":", 2)
			if parts[0] != "" && parts[1] != "" {
				depType = parts[0]
				dependsOnID = parts[1]
			}
		}
		m.deps = append(m.deps, Dep{
			IssueID:     stored.ID,
			DependsOnID: dependsOnID,
			Type:        depType,
		})
	}
	return cloneBead(stored), nil
}

// Update modifies fields of an existing bead. Only non-nil fields in opts
// are applied. Returns a wrapped ErrNotFound if the ID does not exist.
func (m *MemStore) Update(id string, opts UpdateOpts) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.beads {
		if m.beads[i].ID == id {
			if opts.Title != nil {
				m.beads[i].Title = *opts.Title
			}
			if opts.Status != nil {
				m.beads[i].Status = *opts.Status
			}
			if opts.Description != nil {
				m.beads[i].Description = *opts.Description
			}
			if opts.Priority != nil {
				m.beads[i].Priority = cloneIntPtr(opts.Priority)
			}
			if opts.ParentID != nil {
				m.beads[i].ParentID = *opts.ParentID
			}
			if opts.Assignee != nil {
				m.beads[i].Assignee = *opts.Assignee
			}
			if len(opts.Metadata) > 0 {
				if m.beads[i].Metadata == nil {
					m.beads[i].Metadata = make(map[string]string, len(opts.Metadata))
				}
				for k, v := range opts.Metadata {
					m.beads[i].Metadata[k] = v
				}
			}
			if len(opts.Labels) > 0 {
				m.beads[i].Labels = append(m.beads[i].Labels, opts.Labels...)
			}
			if len(opts.RemoveLabels) > 0 {
				remove := make(map[string]bool, len(opts.RemoveLabels))
				for _, rl := range opts.RemoveLabels {
					remove[rl] = true
				}
				filtered := m.beads[i].Labels[:0]
				for _, l := range m.beads[i].Labels {
					if !remove[l] {
						filtered = append(filtered, l)
					}
				}
				m.beads[i].Labels = filtered
			}
			return nil
		}
	}
	return fmt.Errorf("updating bead %q: %w", id, ErrNotFound)
}

// Close sets a bead's status to "closed". Returns a wrapped ErrNotFound if
// the ID does not exist. Closing an already-closed bead is a no-op.
func (m *MemStore) Close(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.beads {
		if m.beads[i].ID == id {
			m.beads[i].Status = "closed"
			return nil
		}
	}
	return fmt.Errorf("closing bead %q: %w", id, ErrNotFound)
}

// CloseAll closes multiple beads in a single batch and sets metadata on each.
func (m *MemStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	idSet := make(map[string]bool, len(ids))
	for _, id := range ids {
		idSet[id] = true
	}
	closed := 0
	for i := range m.beads {
		if !idSet[m.beads[i].ID] || m.beads[i].Status == "closed" {
			continue
		}
		m.beads[i].Status = "closed"
		if m.beads[i].Metadata == nil {
			m.beads[i].Metadata = make(map[string]string, len(metadata))
		}
		for k, v := range metadata {
			m.beads[i].Metadata[k] = v
		}
		closed++
	}
	return closed, nil
}

// ListOpen returns all beads in creation order.
func (m *MemStore) ListOpen(status ...string) ([]Bead, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	filterStatus := ""
	if len(status) > 0 {
		filterStatus = status[0]
	}
	var result []Bead
	for _, b := range m.beads {
		if filterStatus != "" && b.Status != filterStatus {
			continue
		}
		result = append(result, cloneBead(b))
	}
	return result, nil
}

// Ready returns all open beads with no open blocking dependencies, in
// creation order.
func (m *MemStore) Ready() ([]Bead, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	statusByID := make(map[string]string, len(m.beads))
	for _, bead := range m.beads {
		statusByID[bead.ID] = bead.Status
	}

	var result []Bead
	for _, b := range m.beads {
		if b.Status != "open" {
			continue
		}
		blocked := false
		for _, dep := range m.deps {
			if dep.IssueID != b.ID {
				continue
			}
			switch dep.Type {
			case "blocks", "waits-for", "conditional-blocks":
			default:
				continue
			}
			if statusByID[dep.DependsOnID] != "closed" {
				blocked = true
				break
			}
		}
		if !blocked {
			result = append(result, cloneBead(b))
		}
	}
	return result, nil
}

// Get retrieves a bead by ID. Returns a wrapped ErrNotFound if the ID does
// not exist.
func (m *MemStore) Get(id string) (Bead, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, b := range m.beads {
		if b.ID == id {
			return cloneBead(b), nil
		}
	}
	return Bead{}, fmt.Errorf("getting bead %q: %w", id, ErrNotFound)
}

// Children returns all beads whose ParentID matches the given ID, in creation
// order.
func (m *MemStore) Children(parentID string, _ ...QueryOpt) ([]Bead, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var result []Bead
	for _, b := range m.beads {
		if b.ParentID == parentID {
			result = append(result, cloneBead(b))
		}
	}
	return result, nil
}

// ListByLabel returns beads matching an exact label string. Results are
// returned in reverse creation order (newest first). Limit controls max
// results (0 = unlimited).
func (m *MemStore) ListByLabel(label string, limit int, _ ...QueryOpt) ([]Bead, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var result []Bead
	for i := len(m.beads) - 1; i >= 0; i-- {
		for _, l := range m.beads[i].Labels {
			if l == label {
				result = append(result, cloneBead(m.beads[i]))
				if limit > 0 && len(result) >= limit {
					return result, nil
				}
				break
			}
		}
	}
	return result, nil
}

// ListByAssignee returns beads assigned to the given agent with the specified
// status. Limit controls max results (0 = unlimited).
func (m *MemStore) ListByAssignee(assignee, status string, limit int) ([]Bead, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var result []Bead
	for i := len(m.beads) - 1; i >= 0; i-- {
		b := m.beads[i]
		if b.Assignee == assignee && b.Status == status {
			result = append(result, cloneBead(b))
			if limit > 0 && len(result) >= limit {
				return result, nil
			}
		}
	}
	return result, nil
}

// ListByMetadata returns beads whose metadata contains all key-value pairs
// in filters. Limit controls max results (0 = unlimited).
func (m *MemStore) ListByMetadata(filters map[string]string, limit int) ([]Bead, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var result []Bead
	for i := len(m.beads) - 1; i >= 0; i-- {
		if matchesMetadata(m.beads[i], filters) {
			result = append(result, cloneBead(m.beads[i]))
			if limit > 0 && len(result) >= limit {
				return result, nil
			}
		}
	}
	return result, nil
}

// SetMetadata sets a key-value metadata pair on a bead. Returns a wrapped
// ErrNotFound if the bead does not exist.
func (m *MemStore) SetMetadata(id, key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, b := range m.beads {
		if b.ID == id {
			if b.Metadata == nil {
				m.beads[i].Metadata = make(map[string]string)
			}
			m.beads[i].Metadata[key] = value
			return nil
		}
	}
	return fmt.Errorf("setting metadata on %q: %w", id, ErrNotFound)
}

// SetMetadataBatch atomically sets multiple key-value metadata pairs on a bead.
func (m *MemStore) SetMetadataBatch(id string, kvs map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, b := range m.beads {
		if b.ID == id {
			if b.Metadata == nil {
				m.beads[i].Metadata = make(map[string]string)
			}
			for k, v := range kvs {
				m.beads[i].Metadata[k] = v
			}
			return nil
		}
	}
	return fmt.Errorf("setting metadata batch on %q: %w", id, ErrNotFound)
}

// Delete removes a bead from the in-memory store.
func (m *MemStore) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, b := range m.beads {
		if b.ID == id {
			m.beads = append(m.beads[:i], m.beads[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("deleting bead %q: %w", id, ErrNotFound)
}

// Ping always succeeds for MemStore (in-memory, always available).
func (m *MemStore) Ping() error {
	return nil
}

// DepAdd records a dependency: issueID depends on dependsOnID.
func (m *MemStore) DepAdd(issueID, dependsOnID, depType string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, d := range m.deps {
		if d.IssueID == issueID && d.DependsOnID == dependsOnID {
			m.deps[i].Type = depType // update type on re-add
			return nil
		}
	}
	m.deps = append(m.deps, Dep{
		IssueID:     issueID,
		DependsOnID: dependsOnID,
		Type:        depType,
	})
	return nil
}

// DepRemove removes a dependency between two beads.
func (m *MemStore) DepRemove(issueID, dependsOnID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, d := range m.deps {
		if d.IssueID == issueID && d.DependsOnID == dependsOnID {
			m.deps = append(m.deps[:i], m.deps[i+1:]...)
			return nil
		}
	}
	return nil // removing nonexistent dep is a no-op
}

// DepList returns dependencies for a bead. Direction "down" (default)
// returns what this bead depends on; "up" returns what depends on this bead.
func (m *MemStore) DepList(id, direction string) ([]Dep, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []Dep
	for _, d := range m.deps {
		switch direction {
		case "up":
			if d.DependsOnID == id {
				result = append(result, d)
			}
		default: // "down" or empty
			if d.IssueID == id {
				result = append(result, d)
			}
		}
	}
	return result, nil
}
