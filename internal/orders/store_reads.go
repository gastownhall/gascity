package orders

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convergence"
)

// This file holds the order-class typed READ surface plus the mixed
// orders+graph reads. It confines the order-tracking / order-run label codec and
// the bead->OrderRun projection so cmd/gc and internal/api hold OrderRun values
// (or typed verdicts) rather than raw order beads.
//
// Read tiers are declared per method and preserved from the ops they replace:
//   - RecentRunsAll / OpenRuns (the dispatch cooldown/single-flight index) read
//     the LIVE tier — cache-bypass is the duplicate-dispatch guarantee.
//   - LastRun / Cursor preserve the dispatcher's pre-existing bare-List tier so
//     the migration is behavior-preserving.
//   - Get / RunDetail are the by-id detail reads (bare Get).

// The close-verify retry parameters mirror the dispatcher's original
// closeAndVerifyOrderTrackingBeads: three attempts with a short backoff between
// each, so a store that briefly reports a bead still open (Dolt write lag) is
// re-verified rather than treated as a failed close.
const (
	closeVerifyAttempts   = 3
	closeVerifyRetryDelay = 25 * time.Millisecond
)

// Get reads the tracking/run bead named by handle and projects it onto an
// OrderRun. The handle arrives WITH orders-class context (a typed order endpoint
// or list), so no class discovery is needed; it reads the bare tier (a by-id
// detail read is cache-tolerant). A bead that carries no order-run label / order:
// title still decodes (best-effort scoped name) so a caller that already holds a
// valid handle gets its fields back. Provided as the typed by-id contract; the
// API order-history-detail path (an exempt by-id federation surface that still
// emits raw labels on the wire) will migrate onto it in WI-6/7.
func (s *Store) Get(handle string) (OrderRun, error) {
	if s.store.Store == nil {
		return OrderRun{}, fmt.Errorf("orders get %q: nil store", handle)
	}
	b, err := s.store.Get(handle)
	if err != nil {
		return OrderRun{}, fmt.Errorf("orders get %q: %w", handle, err)
	}
	name, _ := NameFromTrackingBead(b)
	return decodeRun(name, b), nil
}

// RunDetail is the by-id detail projection: an OrderRun paired with the run's
// exec-gate output. It is provided as the typed by-id contract that will back the
// order-history-detail handler once that path migrates off its raw bead + inline
// convergence.gate_* crack (WI-6/7); the handler is an exempt by-id federation
// surface today and has no production caller here yet.
type RunDetail struct {
	// Run is the decoded order run.
	Run OrderRun
	// Gate is the run's captured exec-gate output (empty when the run has none).
	Gate convergence.GateOutput
}

// RunDetail reads the tracking/run bead named by handle and projects it onto a
// RunDetail (OrderRun + the run's gate output). The gate-output vocabulary stays
// owned by internal/convergence; only the typed GateOutput escapes.
func (s *Store) RunDetail(handle string) (RunDetail, error) {
	if s.store.Store == nil {
		return RunDetail{}, fmt.Errorf("orders run detail %q: nil store", handle)
	}
	b, err := s.store.Get(handle)
	if err != nil {
		return RunDetail{}, fmt.Errorf("orders run detail %q: %w", handle, err)
	}
	name, _ := NameFromTrackingBead(b)
	return RunDetail{
		Run:  decodeRun(name, b),
		Gate: convergence.GateOutputFromMetadata(b.Metadata),
	}, nil
}

// RecentRunsAll lists up to limit tracking beads across EVERY order (newest-first,
// including closed), decoded into OrderRun. It folds the dispatcher's cooldown
// history index (order_dispatch.go historyEntriesForStore) without per-handle
// Gets — a per-handle read reintroduces the cold-cache serial-query hang
// (#3201/#2893). It reads the LIVE tier (cache-bypass is the duplicate-dispatch
// guarantee); beads with no resolvable order name are skipped, exactly like the
// index fold.
func (s *Store) RecentRunsAll(limit int) ([]OrderRun, error) {
	if s.store.Store == nil {
		return nil, nil
	}
	list, err := beads.HandlesFor(s.store.Store).Live.List(beads.ListQuery{
		Label:         labelOrderTracking,
		Limit:         limit,
		IncludeClosed: true,
		Sort:          beads.SortCreatedDesc,
	})
	return decodeTrackingRuns(list), err
}

// OpenRuns lists the OPEN tracking beads across every order (newest-first),
// decoded into OrderRun. It folds the dispatcher's single-flight open-tracking
// index (order_dispatch.go entriesForStore) onto OrderRun and reads the LIVE
// tier for the same cache-bypass reason as RecentRunsAll. Beads with no
// resolvable order name are skipped.
func (s *Store) OpenRuns() ([]OrderRun, error) {
	if s.store.Store == nil {
		return nil, nil
	}
	list, err := beads.HandlesFor(s.store.Store).Live.List(beads.ListQuery{
		Label:  labelOrderTracking,
		Status: "open",
		Sort:   beads.SortCreatedDesc,
	})
	return decodeTrackingRuns(list), err
}

// StaleOpenRuns lists OPEN tracking beads whose CreatedAt is at or before cutoff,
// decoded into OrderRun (both tiers — legacy issues and wisp — like the sweep's
// ListByLabel). It is the typed read half of the stale-order-tracking sweep: the
// caller applies any order-name filter (run.Scoped), close budget, and the
// sweep-vocabulary metadata close. Names are resolved best-effort (Scoped is ""
// for a tracking bead that carries neither an order-run label nor an order:
// title), matching the sweep's "when no order filter is set, close every stale
// tracking bead" behavior.
func (s *Store) StaleOpenRuns(cutoff time.Time) ([]OrderRun, error) {
	if s.store.Store == nil {
		return nil, nil
	}
	all, err := s.store.ListByLabel(labelOrderTracking, 0, beads.WithBothTiers)
	if err != nil {
		return nil, err
	}
	out := make([]OrderRun, 0, len(all))
	for _, b := range all {
		if b.CreatedAt.IsZero() || b.CreatedAt.After(cutoff) {
			continue
		}
		name, _ := NameFromTrackingBead(b)
		out = append(out, decodeRun(name, b))
	}
	return out, nil
}

// OrphanedOpenRuns lists every OPEN tracking bead EXCEPT pre-dispatch
// trigger-env-failure markers (which the open-work gate intentionally keeps open
// until the normal stale sweep), decoded into OrderRun across both tiers. It is
// the typed read half of the orphaned-order-tracking startup sweep; the caller
// closes the returned runs via CloseRuns. Names are best-effort (the sweep closes
// by ID and does not resolve names).
func (s *Store) OrphanedOpenRuns() ([]OrderRun, error) {
	if s.store.Store == nil {
		return nil, nil
	}
	all, err := s.store.ListByLabel(labelOrderTracking, 0, beads.WithBothTiers)
	if err != nil {
		return nil, err
	}
	out := make([]OrderRun, 0, len(all))
	for _, b := range all {
		if beadLabelsContain(b.Labels, labelTriggerEnvFail) {
			continue
		}
		name, _ := NameFromTrackingBead(b)
		out = append(out, decodeRun(name, b))
	}
	return out, nil
}

// ClosedRunsForRetention lists the CLOSED tracking beads across every order
// (newest-first, both tiers), decoded into OrderRun on the LIVE tier — the read
// half of the closed-tracking retention prune. The caller buckets by order name
// (using the legacy bucket for an unresolvable name), keeps the recent-history
// floor, and deletes the aged remainder. Names are resolved best-effort so an
// unresolvable-name bead can be routed to the legacy retention bucket.
func (s *Store) ClosedRunsForRetention() ([]OrderRun, error) {
	if s.store.Store == nil {
		return nil, nil
	}
	list, err := beads.HandlesFor(s.store.Store).Live.List(beads.ListQuery{
		Status:   "closed",
		Label:    labelOrderTracking,
		Sort:     beads.SortCreatedDesc,
		TierMode: beads.TierBoth,
	})
	if err != nil {
		return nil, err
	}
	out := make([]OrderRun, 0, len(list))
	for _, b := range list {
		name, _ := NameFromTrackingBead(b)
		out = append(out, decodeRun(name, b))
	}
	return out, nil
}

// CloseRuns closes a batch of tracking beads, stamping close_reason so
// validation.on-close cities accept the close, then re-verifies that every id is
// closed — retrying a bounded number of times with a short backoff to tolerate a
// store that briefly reports a just-closed bead as still open (Dolt write lag).
// It returns the number of beads actually closed. It is the byte-identical
// replacement for the dispatcher's closeAndVerifyOrderTrackingBeads for the
// close_reason-only close sites (dispatch completion, orphaned-startup sweep).
// ctx cancels the inter-attempt backoff.
//
// DRIFT GUARD: this retry loop (attempts/backoff via closeVerifyAttempts +
// closeVerifyRetryDelay, plus uniqueNonEmptyIDs / openIDs / waitCloseRetry) is a
// deliberate twin of cmd/gc/order_dispatch.go closeAndVerifyOrderTrackingBeads,
// which survives for the stale sweep's richer sweep-vocabulary metadata close.
// Any change to the retry policy MUST land in both.
func (s *Store) CloseRuns(ctx context.Context, ids []string, reason string) (int, error) {
	ids = uniqueNonEmptyIDs(ids)
	if len(ids) == 0 {
		return 0, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if s.store.Store == nil {
		return 0, fmt.Errorf("order-tracking close: nil store")
	}
	metadata := map[string]string{"close_reason": reason}

	closed := 0
	var lastErr error
	for attempt := 1; attempt <= closeVerifyAttempts; attempt++ {
		n, err := s.store.CloseAll(ids, metadata)
		closed += n
		if closed > len(ids) {
			closed = len(ids)
		}
		if err != nil {
			lastErr = fmt.Errorf("closing order-tracking beads %s: %w", strings.Join(ids, ", "), err)
			if attempt < closeVerifyAttempts {
				if waitErr := s.waitCloseRetry(ctx); waitErr != nil {
					return closed, errors.Join(lastErr, waitErr)
				}
			}
			continue
		}
		openIDs, err := s.openIDs(ids)
		if err != nil {
			lastErr = fmt.Errorf("verifying order-tracking close for %s: %w", strings.Join(ids, ", "), err)
			if attempt < closeVerifyAttempts {
				if waitErr := s.waitCloseRetry(ctx); waitErr != nil {
					return closed, errors.Join(lastErr, waitErr)
				}
			}
			continue
		}
		if len(openIDs) == 0 {
			return closed, nil
		}
		lastErr = fmt.Errorf("verifying order-tracking close: still open: %s", strings.Join(openIDs, ", "))
		if attempt < closeVerifyAttempts {
			if waitErr := s.waitCloseRetry(ctx); waitErr != nil {
				return closed, errors.Join(lastErr, waitErr)
			}
		}
	}
	return closed, lastErr
}

func (s *Store) waitCloseRetry(ctx context.Context) error {
	timer := time.NewTimer(closeVerifyRetryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// openIDs returns the subset of ids whose tracking bead is still open. Beads that
// no longer exist are treated as closed (dropped).
func (s *Store) openIDs(ids []string) ([]string, error) {
	var openIDs []string
	for _, id := range ids {
		b, err := s.store.Get(id)
		if errors.Is(err, beads.ErrNotFound) {
			continue
		}
		if err != nil {
			return openIDs, err
		}
		if b.Status != "closed" {
			openIDs = append(openIDs, id)
		}
	}
	return openIDs, nil
}

func uniqueNonEmptyIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// MarkFailed stamps the wisp-failure outcome on a tracking bead in ONE Update,
// optionally appending the event cursor labels (order:<scoped>, seq:<N>) when the
// order is event-triggered with a non-nil cursor. Combining the outcome and
// cursor labels in a single Update is load-bearing: SetOutcome followed by
// SetCursor would be two writes and is NOT byte-equivalent to the dispatcher's
// original markTrackingFailure. cursor is nil for non-event triggers (no cursor
// labels), matching the caller's a.Trigger=="event" && headSeq>0 guard.
//
// It returns the RAW Update error unwrapped: the sole caller (the dispatcher's
// markTrackingFailure) logs it under its own "failed to mark tracking bead %s as
// failed: %v" context, so wrapping here would double the context in the operator
// log.
func (s *Store) MarkFailed(runID, scoped string, outcome RunOutcome, cursor *EventCursor) error {
	labels := outcome.Labels()
	if cursor != nil {
		labels = append(labels,
			labelOrderTitlePrefix+scoped,
			fmt.Sprintf("%s%d", labelSeqPrefix, uint64(*cursor)),
		)
	}
	return s.store.Update(runID, beads.UpdateOpts{Labels: labels})
}

// LastRun reports the most recent run time (the cooldown clock) for the named
// order, unioning the order-run:<name> evidence across the orders leg and the
// graph leg. It is a MIXED orders+graph read: the order-run label rides both
// order-tracking beads (orders class) and wisp/molecule roots (graph class), so
// reading only one class would miss the other under a graph-store split
// (cursor/cooldown regression). It preserves the dispatcher's original bare-List
// tier and its partial-tier-error tolerance (surviving rows win; the error is
// logged, not returned, once any row is in hand).
func (s *Store) LastRun(name string) (time.Time, error) {
	label := labelOrderRunPrefix + name
	var latest time.Time
	for _, store := range s.mixedLegStores() {
		results, err := store.List(beads.ListQuery{
			Label:         label,
			Limit:         1,
			IncludeClosed: true,
			Sort:          beads.SortCreatedDesc,
			TierMode:      beads.TierBoth,
		})
		if err != nil {
			if len(results) == 0 {
				return time.Time{}, err
			}
			runtimeHelpersLogf("orders: last-run lookup partially failed for %s: %v", name, err)
		}
		if len(results) == 0 {
			continue
		}
		if results[0].CreatedAt.After(latest) {
			latest = results[0].CreatedAt
		}
	}
	return latest, nil
}

// Cursor reports the max event seq (the order's event-bus high-water mark) for
// the named order, unioning across the orders leg and the graph leg. Like
// LastRun it is a MIXED orders+graph read (the seq labels ride both tracking
// beads and wisp roots) and preserves the dispatcher's original bare-List tier
// and partial-tier-error tolerance.
func (s *Store) Cursor(name string) EventCursor {
	label := labelOrderRunPrefix + name
	var latest uint64
	for _, store := range s.mixedLegStores() {
		results, err := store.List(beads.ListQuery{
			Label:         label,
			Limit:         10,
			IncludeClosed: true,
			Sort:          beads.SortCreatedDesc,
			TierMode:      beads.TierBoth,
		})
		if err != nil {
			if len(results) == 0 {
				runtimeHelpersLogf("orders: cursor lookup failed for %s: %v", name, err)
				continue
			}
			runtimeHelpersLogf("orders: cursor lookup partially failed for %s: %v", name, err)
		}
		if len(results) == 0 {
			continue
		}
		labelSets := make([][]string, 0, len(results))
		for _, b := range results {
			labelSets = append(labelSets, b.Labels)
		}
		if seq := MaxSeqFromLabels(labelSets); seq > latest {
			latest = seq
		}
	}
	return EventCursor(latest)
}

// HasOpenWork reports whether any in-flight work exists for the scoped order:
// an open order-tracking bead (orders class), or an open wisp/molecule root whose
// subtree still holds open work (graph class). It is a MIXED orders+graph read:
// it unions the order-run:<scoped> list across the orders leg and the graph leg
// (so a split store still sees both classes) on the LIVE tier, classifies
// order-tracking beads inline, and defers each wisp-root subtree verdict to
// wispHasOpenWork — the graph-walk predicate that stays graph-owned in the
// controller (the wisp-subtree traversal is graph residual). Only the boolean
// verdict escapes the edge.
func (s *Store) HasOpenWork(scoped string, wispHasOpenWork func(store beads.Store, root beads.Bead) (bool, error)) (bool, error) {
	label := labelOrderRunPrefix + scoped
	for _, store := range s.mixedLegStores() {
		results, err := beads.HandlesFor(store).Live.List(beads.ListQuery{
			Label:    label,
			Sort:     beads.SortCreatedDesc,
			TierMode: beads.TierBoth,
		})
		if err != nil {
			return false, fmt.Errorf("listing order work beads: %w", err)
		}
		for _, b := range results {
			if b.Status == "closed" {
				continue
			}
			if beadLabelsContain(b.Labels, labelOrderTracking) {
				return true, nil
			}
			if wispHasOpenWork == nil {
				continue
			}
			open, err := wispHasOpenWork(store, b)
			if err != nil {
				return false, err
			}
			if open {
				return true, nil
			}
		}
	}
	return false, nil
}
