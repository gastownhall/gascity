package beads

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// AbsentBlockedReader lists the beads a store's own Ready withholds because a
// ready-blocking edge names an id the store does not hold.
//
// # Why a reader needs this
//
// A split city keeps its work and its coordination classes in different
// stores, so a dependency edge can name a blocker that lives in another store.
// The backends disagree about what such an edge means. SQLite and the in-memory
// stores read the absent target as a status that is never "closed", so the
// dependent is blocked FOREVER: closing the real blocker in its own store
// cannot release it, because the store holding the edge never sees that row.
// bd/Dolt classifies a cross-prefix target as external and does not block on it
// at all, so the dependent is ready AT ONCE. The owner rule is neither: a
// blocker held in another store blocks until it is closed there.
//
// A federated ready reader (internal/readywork) implements that rule over the
// stores' own answers. The premature half it can see directly, because the
// dependent is in the store's ready set and its edges can be read. The
// stranded half it cannot: the dependent is not in any ready set, and nothing
// in the Store interface lists it. This reader is that listing.
//
// # The contract
//
// StrandsAbsentTargets reports whether the store's Ready reads an absent target
// as a blocker. The engines whose Ready does (MemStore, FileStore, SQLiteStore)
// answer true. A wrapper has to declare both methods to keep the listing
// reachable, because the embedded Store interface does not promote it, and it
// answers for the store it wraps. A wrapper over bd/Dolt therefore answers
// false: a reader that took the method's mere presence as "this store strands"
// would trust every row bd served as already judged, and serve a dependent
// whose blocker is open in another store.
//
// ReadyBlockedByAbsent returns the open beads that pass Ready's own candidate
// filters for query (status open, an actionable type, not deferred, the
// requested tier and assignee) and that carry at least one ready-blocking edge
// (blocks, waits-for, conditional-blocks) whose target this store does not
// hold. It does NOT judge the edges whose targets it does hold: a row it
// returns may still be blocked by a resident blocker, and the caller re-judges
// every edge before it serves the row. Limit is ignored, because the caller
// re-judges and so cannot know which rows survive a cut.
//
// A store whose backend already treats an absent target as non-blocking
// (bd/Dolt) has no such rows and does not implement this: its dependents are
// already in its ready set, where the caller finds them.
type AbsentBlockedReader interface {
	StrandsAbsentTargets() bool
	ReadyBlockedByAbsent(query ReadyQuery) ([]Bead, error)
}

// ReadyBlockedByAbsentFor returns store's absent-blocker listing, or false
// when the store strands nothing: it has no listing, or it wraps a store that
// has none (StrandsAbsentTargets).
func ReadyBlockedByAbsentFor(store Store) (AbsentBlockedReader, bool) {
	reader, ok := store.(AbsentBlockedReader)
	if !ok || !reader.StrandsAbsentTargets() {
		return nil, false
	}
	return reader, true
}

var (
	_ AbsentBlockedReader = (*MemStore)(nil)
	_ AbsentBlockedReader = (*FileStore)(nil)
	_ AbsentBlockedReader = (*SQLiteStore)(nil)
	_ AbsentBlockedReader = (*CachingStore)(nil)
)

// StrandsAbsentTargets reports true: the in-memory store's Ready reads an
// absent blocker as never closed. FileStore inherits it.
func (m *MemStore) StrandsAbsentTargets() bool { return true }

// ReadyBlockedByAbsent implements AbsentBlockedReader for the in-memory store,
// whose Ready reads an absent blocker as never closed.
func (m *MemStore) ReadyBlockedByAbsent(query ReadyQuery) ([]Bead, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	held := make(map[string]bool, len(m.beads))
	for _, b := range m.beads {
		held[b.ID] = true
	}
	absentFor := make(map[string]bool)
	for _, dep := range m.deps {
		if !IsReadyBlockingDependencyType(dep.Type) || held[dep.DependsOnID] {
			continue
		}
		absentFor[dep.IssueID] = true
	}
	if len(absentFor) == 0 {
		return nil, nil
	}
	now := time.Now().UTC()
	var out []Bead
	for _, b := range m.beads {
		if !absentFor[b.ID] || !IsReadyCandidateForTier(b, now, query.TierMode) {
			continue
		}
		if query.Assignee != "" && b.Assignee != query.Assignee {
			continue
		}
		if query.Unassigned && strings.TrimSpace(b.Assignee) != "" {
			continue
		}
		out = append(out, cloneBead(b))
	}
	return out, nil
}

// ReadyBlockedByAbsent reloads the on-disk store before listing, as Ready does.
func (fs *FileStore) ReadyBlockedByAbsent(query ReadyQuery) ([]Bead, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.refreshReadStateLocked(); err != nil {
		return nil, err
	}
	return fs.MemStore.ReadyBlockedByAbsent(query)
}

// StrandsAbsentTargets answers for the backing store, whose listing this cache
// forwards: over bd/Dolt it is false.
//
// It describes the backing store's Ready, which every live read here reaches.
// The cache's own ready projection reads a target it does not hold as no
// blocker, so a row served from that projection can carry an absent target; the
// listing below names every such row, which is how a reader gating cached rows
// finds them.
func (c *CachingStore) StrandsAbsentTargets() bool {
	_, ok := ReadyBlockedByAbsentFor(c.backing)
	return ok
}

// ReadyBlockedByAbsent reads the backing store. The listing is an
// authoritative read for a readiness decision, like Live.Ready, so it never
// answers from the cache; a backing store without the listing has no such rows.
func (c *CachingStore) ReadyBlockedByAbsent(query ReadyQuery) ([]Bead, error) {
	reader, ok := ReadyBlockedByAbsentFor(c.backing)
	if !ok {
		return nil, nil
	}
	return reader.ReadyBlockedByAbsent(query)
}

// StrandsAbsentTargets reports true: SQLite's ready projection reads an absent
// blocker as never closed (sqliteReadyBlockerExists).
func (s *SQLiteStore) StrandsAbsentTargets() bool { return true }

// ReadyBlockedByAbsent implements AbsentBlockedReader for SQLite, whose ready
// projection LEFT JOINs the blocker and so reads an absent one as never closed
// (sqliteReadyBlockerExists).
func (s *SQLiteStore) ReadyBlockedByAbsent(query ReadyQuery) ([]Bead, error) {
	if err := s.ensureOpen(); err != nil {
		return nil, err
	}
	ctx := context.Background()
	sqlText, args := sqliteReadyCandidateSQL(query, s.sqliteBeadProjection(), sqliteAbsentBlockerExists("b.id"))
	rows, err := s.readDB.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("listing sqlite beads blocked by absent targets: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var out []Bead
	now := time.Now().UTC()
	for rows.Next() {
		b, err := scanSQLiteBead(rows)
		if err != nil {
			return nil, err
		}
		if !IsReadyCandidateForTier(b, now, query.TierMode) {
			continue
		}
		if query.Unassigned && strings.TrimSpace(b.Assignee) != "" {
			continue
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing sqlite beads blocked by absent targets: %w", err)
	}
	if err := hydrateSQLiteDeps(ctx, s.readDB, out); err != nil {
		return nil, fmt.Errorf("listing sqlite beads blocked by absent targets: %w", err)
	}
	return out, nil
}

// sqliteAbsentBlockerExists is the EXISTS over issueCol's ready-blocking edges
// whose target this database does not hold — exactly the edges
// sqliteReadyBlockerExists reads as a NULL status that is never "closed".
func sqliteAbsentBlockerExists(issueCol string) string {
	return fmt.Sprintf(`EXISTS (
			SELECT 1 FROM deps d
			LEFT JOIN beads blocker ON blocker.id=d.depends_on_id
			WHERE d.issue_id=%s
			  AND d.dep_type IN ('blocks','waits-for','conditional-blocks')
			  AND blocker.id IS NULL
		  )`, issueCol)
}
