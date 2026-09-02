package beads

import (
	"errors"
	"fmt"
)

// IssueGraphSnapshotReader reads issues and their DOWN dependency edges from
// one storage snapshot. It is deliberately optional: subprocess-backed stores
// cannot promise that a List followed by dependency reads observes one durable
// revision, and callers that require a CAS-ready graph must fail closed rather
// than accept a torn projection.
type IssueGraphSnapshotReader interface {
	// IssueGraphSnapshot returns the ListQuery result and every returned issue's
	// DOWN edges. The dependency map contains an entry for every returned issue,
	// including issues with no edges. On any read failure both results are nil.
	IssueGraphSnapshot(query ListQuery) ([]Bead, map[string][]Dep, error)
}

// ErrIssueGraphSnapshotUnsupported reports that a store cannot read issues and
// dependency edges through one atomic storage boundary.
var ErrIssueGraphSnapshotUnsupported = errors.New("atomic issue graph snapshot unsupported by backing store")

// IssueGraphSnapshotFor returns the atomic graph reader implemented by store.
func IssueGraphSnapshotFor(store Store) (IssueGraphSnapshotReader, bool) {
	reader, ok := store.(IssueGraphSnapshotReader)
	return reader, ok
}

// IssueGraphSnapshot reads an in-memory issue graph while holding the store's
// single mutex, so issue revisions and dependency rows cannot come from
// different mutations.
func (m *MemStore) IssueGraphSnapshot(query ListQuery) ([]Bead, map[string][]Dep, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !query.HasFilter() && !query.AllowScan {
		return nil, nil, fmt.Errorf("reading issue graph snapshot: %w", ErrQueryRequiresScan)
	}

	rows := make([]Bead, 0, len(m.beads))
	for _, bead := range m.beads {
		if query.Matches(bead) {
			rows = append(rows, cloneBead(bead))
		}
	}
	sortBeadsForQuery(rows, query.Sort)
	if query.Limit > 0 && len(rows) > query.Limit {
		rows = rows[:query.Limit]
	}

	selected := make(map[string]struct{}, len(rows))
	depsByID := make(map[string][]Dep, len(rows))
	for _, bead := range rows {
		selected[bead.ID] = struct{}{}
		depsByID[bead.ID] = []Dep{}
	}
	for _, dep := range m.deps {
		if _, ok := selected[dep.IssueID]; ok {
			depsByID[dep.IssueID] = append(depsByID[dep.IssueID], dep)
		}
	}
	return rows, depsByID, nil
}
