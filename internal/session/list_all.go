package session

import (
	"fmt"
	"hash/fnv"
	"io"
	"sort"

	"github.com/gastownhall/gascity/internal/beads"
)

// ListAllSessionBeads returns every session bead from the store using a
// type+label union so canonical session beads that have lost their
// gc:session label (after a crash, partial write, or schema migration)
// still surface alongside legacy records that retain the label but have
// an empty type.
//
// Two indexed store.List queries are issued:
//   - one with Type=BeadType — the authoritative source for session beads
//   - one with Label=LabelSession — catches repairable Type="" beads
//
// Results are unioned, deduped by bead ID, and filtered through
// IsSessionBeadOrRepairable so the returned slice is exactly the set of
// beads downstream code treats as sessions.
//
// base is preserved for any filter fields the caller cares about
// (IncludeClosed, Sort, Status, Assignee, Metadata, Limit, Live, etc.).
// base.Type and base.Label are overridden by the union queries
// internally — callers should not set them.
//
// PartialResultError semantics: if either underlying List returns a
// PartialResultError, its (partial) rows are still folded into the
// union, and a PartialResultError is returned alongside the merged
// result so callers can surface degraded-but-non-empty output. Any
// other (hard) error short-circuits and returns nil rows. The hard
// error is wrapped with context naming which leg failed so logs are
// diagnosable.
func ListAllSessionBeads(store beads.Store, base beads.ListQuery) ([]beads.Bead, error) {
	if store == nil {
		return nil, nil
	}

	// Limit is applied globally after the union (see below); passing
	// base.Limit into each leg independently could return up to 2× the
	// requested rows or drop the correct top-N when the union spans
	// both legs.
	byTypeQuery := base
	byTypeQuery.Type = BeadType
	byTypeQuery.Label = ""
	byTypeQuery.Limit = 0
	byType, typeErr := store.List(byTypeQuery)
	if typeErr != nil && !beads.IsPartialResult(typeErr) {
		return nil, fmt.Errorf("listing session beads by type: %w", typeErr)
	}

	byLabelQuery := base
	byLabelQuery.Type = ""
	byLabelQuery.Label = LabelSession
	byLabelQuery.Limit = 0
	byLabel, labelErr := store.List(byLabelQuery)
	if labelErr != nil && !beads.IsPartialResult(labelErr) {
		return nil, fmt.Errorf("listing session beads by label: %w", labelErr)
	}

	seen := make(map[string]struct{}, len(byType)+len(byLabel))
	out := make([]beads.Bead, 0, len(byType)+len(byLabel))
	for _, b := range byType {
		if _, dup := seen[b.ID]; dup {
			continue
		}
		if !IsSessionBeadOrRepairable(b) {
			continue
		}
		seen[b.ID] = struct{}{}
		out = append(out, b)
	}
	for _, b := range byLabel {
		if _, dup := seen[b.ID]; dup {
			continue
		}
		if !IsSessionBeadOrRepairable(b) {
			continue
		}
		seen[b.ID] = struct{}{}
		out = append(out, b)
	}

	// Each leg's store.List honored base.Sort within its result set, but
	// the union concatenates them — sort globally so mixed-shape rows
	// interleave correctly. Unknown Sort values are left alone for
	// forward-compat with future sort modes.
	switch base.Sort {
	case beads.SortCreatedAsc:
		sort.SliceStable(out, func(i, j int) bool {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		})
	case beads.SortCreatedDesc:
		sort.SliceStable(out, func(i, j int) bool {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		})
	}

	if base.Limit > 0 && len(out) > base.Limit {
		out = out[:base.Limit]
	}

	// Surface the first partial-result error encountered. Either leg
	// being partial means the merged set may be missing rows; callers
	// already handle PartialResultError to render a degraded view.
	if typeErr != nil {
		return out, typeErr
	}
	if labelErr != nil {
		return out, labelErr
	}
	return out, nil
}

// ListAllOptions mirrors the beads.ListQuery fields real ListAllSessionBeads
// callers set. The zero value is exactly today's
// ListAllSessionBeads(store, beads.ListQuery{}) — the default direct union.
//
// (The design named Sort as beads.Sort; the actual type in this tree is
// beads.SortOrder.)
type ListAllOptions struct {
	// IncludeClosed keeps closed session beads in the result (both legs).
	IncludeClosed bool
	// Sort orders the merged union globally (the union is re-sorted after the
	// two legs concatenate; per-leg order is not enough).
	Sort beads.SortOrder
	// Limit caps the merged union AFTER the two legs are unioned and sorted —
	// never per leg (a per-leg limit could return up to 2× the requested rows).
	Limit int
	// Live sets query.Live on each leg, bypassing any CachingStore so the read
	// observes external mutations immediately.
	Live bool
	// CacheFirst peeks the read-model cache for both leg shapes and merges them
	// locally when both hit (the #3939/#3941 dashboard read-model tier), falling
	// back to the direct union on either-leg miss. Live and CacheFirst are
	// mutually exclusive: when both are set, Live wins (the cache peek is
	// skipped) so the caller's demand for immediate freshness is honored.
	//
	// CacheFirst REQUIRES an explicit Sort: with SortDefault the cache peek falls
	// through to the direct union, because the cache serves rows in map-iteration
	// order that a no-op sort would not stabilize (the cold path returns store
	// order — the two would disagree). Every CacheFirst caller sets SortCreatedDesc.
	CacheFirst bool
}

// ListedSession pairs the scalar Info projection with the persisted-response
// projection for one session bead: one row read, both views, no bead escapes.
// It is the API read model's row type — the response builder gets Info for the
// scalar/runtime fields and PersistedResponse for the status/metadata-derived
// fields without a *beads.Bead crossing the boundary.
type ListedSession struct {
	Info     Info
	Response PersistedResponse
}

// cachedListStore is the optional read-model cache capability: it answers a
// ListQuery from an in-memory cache, reporting whether the cache was clean
// enough to serve it. It is the same seam internal/api/cache_read_model.go
// peeks; the CacheFirst tier asserts it on the embedded raw store (optional
// capabilities are not promoted through the SessionStore wrapper).
type cachedListStore interface {
	CachedList(beads.ListQuery) ([]beads.Bead, bool)
}

// ListAll returns every session bead projected to session.Info, using the same
// type+label union, dedupe, IsSessionBeadOrRepairable filter, global re-sort,
// post-union Limit, and PartialResultError fold-through as ListAllSessionBeads
// — it wraps that body and projects each surviving row via InfoFromPersistedBead.
// TestListAllMatchesListAllSessionBeads is the row-set/order/error equivalence
// oracle that pins this against a naive Store.List substitution (which would
// silently drop the type-only label-lost beads and the label-only repairable
// beads).
//
// On a hard error nil rows are returned with the wrapped error; on a partial
// result the projected partial rows are returned alongside the PartialResultError.
func (s *Store) ListAll(opts ListAllOptions) ([]Info, error) {
	rows, err := s.listAllBeads(opts)
	if rows == nil {
		return nil, err
	}
	out := make([]Info, 0, len(rows))
	for _, b := range rows {
		out = append(out, infoFromPersistedBead(b))
	}
	return out, err
}

// ReconcileSession is one row of the reconciler tick feed: the session's domain
// projection paired with its persisted circuit-breaker cluster. The pair exists
// because the breaker cluster (the session_circuit_* keys) is deliberately NOT
// on Info (a separate concern from lifecycle-decision facts); the reconciler is
// the one consumer that needs both, read once per tick from the same bead. A
// per-id Store.CircuitState Get would break the pinned 0-Get tick budget, and a
// parallel map[id]CircuitState would break the row-lockstep the dedup pass needs
// (a retired row must carry its circuit with it), so the row carries both. This
// mirrors the ListedSession{Info, Response} precedent.
type ReconcileSession struct {
	Info    Info
	Circuit CircuitState
}

// ListAllForReconcile returns every session bead projected to a ReconcileSession,
// using the identical type+label union, dedupe, IsSessionBeadOrRepairable filter,
// global re-sort, post-union Limit, and PartialResultError fold-through as
// ListAllSessionBeads / ListAll — it wraps the shared listAllBeads body and
// projects each surviving row via InfoFromPersistedBead + CircuitStateFromMetadata.
// Both projections are pure and in-package; no bead escapes.
//
// TestListAllForReconcileMatchesListAllSessionBeads is the row-set/order/error
// equivalence oracle. Error semantics match ListAll (hard error → nil rows +
// wrapped error; partial → projected partial rows + PartialResultError).
func (s *Store) ListAllForReconcile(opts ListAllOptions) ([]ReconcileSession, error) {
	rows, err := s.listAllBeads(opts)
	if rows == nil {
		return nil, err
	}
	out := make([]ReconcileSession, 0, len(rows))
	for _, b := range rows {
		out = append(out, ReconcileSession{
			Info:    infoFromPersistedBead(b),
			Circuit: CircuitStateFromMetadata(b.Metadata),
		})
	}
	return out, err
}

// ReconcileRowsFromBeads projects an in-memory slice of raw session beads to the
// reconcile row feed (Info + circuit cluster per bead), the same per-row projection
// ListAllForReconcile applies to store rows. It exists for the callers that hold raw
// beads directly rather than reading them from the store — the reconciler test/compat
// wrappers and the sync-tail fallback — so the InfoFromPersistedBead +
// CircuitStateFromMetadata codec stays confined to this package. Unlike
// ListAllForReconcile it applies NO union/dedupe/filter: the input is taken as-is,
// row for row, order preserved (closed beads included; the snapshot constructor drops
// them).
func ReconcileRowsFromBeads(beadsIn []beads.Bead) []ReconcileSession {
	out := make([]ReconcileSession, 0, len(beadsIn))
	for _, b := range beadsIn {
		out = append(out, ReconcileSession{
			Info:    infoFromPersistedBead(b),
			Circuit: CircuitStateFromMetadata(b.Metadata),
		})
	}
	return out
}

// SetFingerprint hashes the identity-affecting shape of a raw session-bead
// set: each bead's ID + Status + Assignee + every metadata key/value, order-
// independent (beads sorted by ID, keys sorted per bead). It is the config-change
// detector's cache key and MUST reflect ALL metadata keys — session.Info deliberately
// drops keys it does not project (info_apply_patch.go), so this fingerprint CANNOT be
// derived from Info. It is computed at the store edge (ListAllForReconcileWithFingerprint)
// where the raw beads are still in hand and carried onto the snapshot as a field. The
// byte layout is the reference the config-change caching depends on — a drift re-runs or
// skips demand rebuilds — so it is pinned byte-for-byte against the pre-migration inline
// hash by TestSetFingerprintMatchesInlineHash.
func SetFingerprint(beadsIn []beads.Bead) string {
	sorted := make([]beads.Bead, len(beadsIn))
	copy(sorted, beadsIn)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].ID < sorted[j].ID
	})
	h := fnv.New64a()
	for _, bead := range sorted {
		_, _ = io.WriteString(h, bead.ID)
		_, _ = io.WriteString(h, "\x00")
		_, _ = io.WriteString(h, bead.Status)
		_, _ = io.WriteString(h, "\x00")
		_, _ = io.WriteString(h, bead.Assignee)
		_, _ = io.WriteString(h, "\x00")
		keys := make([]string, 0, len(bead.Metadata))
		for key := range bead.Metadata {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			_, _ = io.WriteString(h, key)
			_, _ = io.WriteString(h, "\x00")
			_, _ = io.WriteString(h, bead.Metadata[key])
			_, _ = io.WriteString(h, "\x00")
		}
	}
	return fmt.Sprintf("%x", h.Sum64())
}

// ListAllForReconcileWithFingerprint is ListAllForReconcile paired with the
// SetFingerprint of the same raw bead set, computed in a single list so the
// snapshot can carry the config-change fingerprint without a second store scan or a
// raw bead escaping the package. The fingerprint is over the surviving union rows
// (post-dedupe/filter), matching the set the snapshot projects. Error semantics match
// ListAllForReconcile (hard error → nil rows + empty fingerprint + wrapped error;
// partial → projected partial rows + their fingerprint + PartialResultError).
func (s *Store) ListAllForReconcileWithFingerprint(opts ListAllOptions) ([]ReconcileSession, string, error) {
	rows, err := s.listAllBeads(opts)
	if rows == nil {
		return nil, "", err
	}
	fingerprint := SetFingerprint(rows)
	out := make([]ReconcileSession, 0, len(rows))
	for _, b := range rows {
		out = append(out, ReconcileSession{
			Info:    infoFromPersistedBead(b),
			Circuit: CircuitStateFromMetadata(b.Metadata),
		})
	}
	return out, fingerprint, err
}

// ListAllWithResponses is ListAll paired with the persisted-response projection:
// each row is read once and projected to both Info and PersistedResponse. It is
// the API read model's typed feed. Error semantics match ListAll (hard error →
// nil rows + wrapped error; partial → projected partial rows + PartialResultError).
func (s *Store) ListAllWithResponses(opts ListAllOptions) ([]ListedSession, error) {
	rows, err := s.listAllBeads(opts)
	if rows == nil {
		return nil, err
	}
	out := make([]ListedSession, 0, len(rows))
	for _, b := range rows {
		out = append(out, ListedSession{
			Info:     infoFromPersistedBead(b),
			Response: PersistedResponseFromBead(b),
		})
	}
	return out, err
}

// listAllBeads is the shared union body behind ListAll / ListAllWithResponses:
// the CacheFirst peek tier when eligible, else the direct ListAllSessionBeads
// union. It returns the raw beads so the two public methods can pick their
// projection; the beads never escape the package.
func (s *Store) listAllBeads(opts ListAllOptions) ([]beads.Bead, error) {
	if s == nil || s.store.Store == nil {
		return nil, nil
	}
	base := beads.ListQuery{
		IncludeClosed: opts.IncludeClosed,
		Sort:          opts.Sort,
		Limit:         opts.Limit,
		Live:          opts.Live,
	}
	// CacheFirst peek — skipped when Live is set (Live wins; the caller demands
	// a store read, not a cache peek).
	if opts.CacheFirst && !opts.Live {
		if merged, ok := s.cachedListUnion(opts); ok {
			return merged, nil
		}
	}
	return ListAllSessionBeads(s.store.Store, base)
}

// cachedListUnion ports the internal/api/cache_read_model.go peek-union: it asks
// the read-model cache for both the type and label leg shapes and merges them
// locally when BOTH hit, so a warm dashboard read serves the whole session list
// without touching the backing store. It reports ok=false (fall through to the
// direct union) when the store has no cache capability or either leg misses.
//
// The merge mirrors ListAllSessionBeads exactly: dedupe by ID, filter through
// IsSessionBeadOrRepairable, global re-sort by opts.Sort, and post-union Limit.
// IncludeClosed is threaded onto the leg queries so an include-closed read falls
// through (CachedList refuses closed queries) rather than silently dropping
// closed rows.
//
// SortDefault falls through: the cache serves rows in map-iteration order and the
// SortDefault sort switch is a no-op, so a warm read would be nondeterministic and
// disagree with the cold path's store order. Requiring an explicit Sort keeps the
// two tiers row-equivalent (pinned by the CacheFirst row-equivalence test).
func (s *Store) cachedListUnion(opts ListAllOptions) ([]beads.Bead, bool) {
	if opts.Sort == beads.SortDefault {
		return nil, false
	}
	cached, ok := s.store.Store.(cachedListStore)
	if !ok {
		return nil, false
	}
	typeQuery := beads.ListQuery{Type: BeadType, Sort: opts.Sort, IncludeClosed: opts.IncludeClosed}
	labelQuery := beads.ListQuery{Label: LabelSession, Sort: opts.Sort, IncludeClosed: opts.IncludeClosed}
	typeRows, typeOK := cached.CachedList(typeQuery)
	labelRows, labelOK := cached.CachedList(labelQuery)
	if !typeOK || !labelOK {
		return nil, false
	}

	seen := make(map[string]struct{}, len(typeRows)+len(labelRows))
	merged := make([]beads.Bead, 0, len(typeRows)+len(labelRows))
	add := func(rows []beads.Bead) {
		for _, b := range rows {
			if _, dup := seen[b.ID]; dup {
				continue
			}
			if !IsSessionBeadOrRepairable(b) {
				continue
			}
			seen[b.ID] = struct{}{}
			merged = append(merged, b)
		}
	}
	add(typeRows)
	add(labelRows)

	switch opts.Sort {
	case beads.SortCreatedAsc:
		sort.SliceStable(merged, func(i, j int) bool {
			return merged[i].CreatedAt.Before(merged[j].CreatedAt)
		})
	case beads.SortCreatedDesc:
		sort.SliceStable(merged, func(i, j int) bool {
			return merged[i].CreatedAt.After(merged[j].CreatedAt)
		})
	}

	if opts.Limit > 0 && len(merged) > opts.Limit {
		merged = merged[:opts.Limit]
	}
	return merged, true
}

// HasOpenSessionNamed reports whether an OPEN session bead exists carrying the
// given runtime session_name. It is the Live-tier existence probe: a
// session_name-filtered, Live union scan (bypassing any CachingStore) so the
// adoption barrier observes just-created beads immediately. It is the front
// door for adoption_barrier.go's openSessionBeadExists — the one ListAll
// consumer with a Metadata filter that does not fit ListAllOptions.
//
// Any error (including a PartialResultError) is returned as (false, err),
// matching the raw probe it replaces: a degraded list cannot prove absence.
func (s *Store) HasOpenSessionNamed(sessionName string) (bool, error) {
	if s == nil || s.store.Store == nil {
		return false, nil
	}
	existing, err := ListAllSessionBeads(s.store.Store, beads.ListQuery{
		Metadata: map[string]string{"session_name": sessionName},
		Live:     true,
	})
	if err != nil {
		return false, fmt.Errorf("listing session beads for %q: %w", sessionName, err)
	}
	for _, b := range existing {
		if b.Status == "closed" {
			continue
		}
		// ListAllSessionBeads already filters via IsSessionBeadOrRepairable.
		return true, nil
	}
	return false, nil
}
