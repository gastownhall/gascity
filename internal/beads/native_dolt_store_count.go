package beads

import (
	"context"
	"errors"
	"fmt"

	beadslib "github.com/steveyegge/beads"
)

// Count implements the optional Counter capability with the pinned beads
// library's CountIssues: a hydration-free SELECT COUNT(*) over the durable
// issues table merged with the wisps tier, mirroring SearchIssues' merge
// semantics (upstream GH#4387 — the wisps undercount that previously kept
// this store Counter-less is fixed there). This answers the closed-inclusive
// shapes the CachingStore can never serve from its open-bead cache — most
// importantly the store-health denominator, whose hydrating List fallback
// cannot finish inside the status read timeout on a long-lived city (#1896
// follow-up).
//
// Count answers only shapes whose ListQuery→IssueFilter translation is
// exact, so the backend count equals List's post-ApplyListQuery cardinality;
// everything else reports ErrCountUnsupported and callers fall back to the
// hydrating List, exactly as the Counter contract specifies. See
// nativeDoltCountSupported for the shape-by-shape rationale. Known parity
// gap: rows whose metadata JSON fails to parse are dropped by List but
// counted here — that state is store corruption, and counting such a row
// beats reporting no count at all.
//
// A backend whose raw CountIssues refuses before entry — httpstore's
// generated unsupported_gen shell, via a typed *beadslib.ErrUnsupported —
// ALWAYS reports ErrCountUnsupported here, for every shape, with no
// server-side substitute attempted. An earlier version of this method
// answered such a refusal through the wire-served Counter role
// (issueops.Counter, bd-enterprise's httpstore accessors.go) for the subset
// of shapes issueops.CountRequest could express. A real-bd-serve review at
// the enterprise pin found that fast path unsound on three counts: (1)
// CountRequest.IncludeInfra is a single bool that also drops the wisps
// tier, so a TierIssues count silently UNDERCOUNTED no-history rows
// (measured Count=4 vs List=5 against a real server); (2) Status:"all" was
// translated as "no status filter" by the role but OVERCOUNTED against the
// real server's semantics (Count=4 vs List=0); and (3) the fast path was
// unreachable in production regardless, because
// wrapStoreWithBeadPolicies/expandPolicyReadTier
// (cmd/gc/bead_policy_store.go) promotes every TierIssues read to TierBoth
// before it reaches here, and the gate only ever accepted TierIssues. There
// is currently no CountRequest shape — and no other served role — that
// reproduces List cardinality against a real server, so the fast path was
// removed rather than narrowed further. Counts over http hydrate via the
// List fallback until a server-side TierBoth-aware count exists (see
// /data/tmp/bdserve-pg-design/S5b-DESIGN.md §3).
//
// Reporting ErrCountUnsupported (rather than leaking the raw
// *beadslib.ErrUnsupported) for every refusal is the load-bearing part:
// every existing caller (store_health's countBeadStoreRows, the status
// handler) keys its hydrating List fallback off errors.Is(err,
// ErrCountUnsupported) and treats any OTHER error as a hard failure — a raw
// refusal escaping unclassified would read as store-health going dark
// instead of falling back to List. A non-ErrUnsupported failure (a real
// backend error, not a refusal) is unaffected: it keeps returning
// unchanged, exactly as before this classification existed. The same rule
// applies uniformly regardless of shape, so there is no longer a
// shape-dependent branch to get wrong here.
func (s *NativeDoltStore) Count(ctx context.Context, query ListQuery, excludeTypes ...string) (int, error) {
	if err := query.Validate(); err != nil {
		return 0, err
	}
	if !query.HasFilter() && !query.AllowScan {
		return 0, fmt.Errorf("counting beads: %w", ErrQueryRequiresScan)
	}
	if !nativeDoltCountSupported(query, excludeTypes) {
		return 0, fmt.Errorf("counting beads: %w", ErrCountUnsupported)
	}
	var n int
	err := s.withReadRetry(func(readCtx context.Context, storage beadslib.Storage) error {
		// The Counter contract promises the caller's ctx cancels the backing
		// query, but withReadRetry runs fn under its own retry-budget context.
		// Derive a context canceled by either, and surface the caller's
		// cancellation as the context error itself so the retry loop treats
		// it as terminal instead of reconnect-worthy.
		if err := ctx.Err(); err != nil {
			return err
		}
		countCtx, cancel := context.WithCancel(readCtx)
		defer cancel()
		stop := context.AfterFunc(ctx, cancel)
		defer stop()
		total, err := storage.CountIssues(countCtx, "", nativeIssueFilterFromListQuery(query))
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			// Ask-don't-handshake, the same pattern Tx and the ready-veto
			// use: a backend with no raw CountIssues (httpstore's
			// unsupported_gen shell) refuses before touching the database
			// with a typed *beadslib.ErrUnsupported, so nothing was
			// double-counted or left half-done. See the doc comment above
			// for why this ALWAYS classifies as ErrCountUnsupported rather
			// than attempting a server-side substitute.
			var unsupported *beadslib.ErrUnsupported
			if errors.As(err, &unsupported) {
				return fmt.Errorf("%w: %w", ErrCountUnsupported, err)
			}
			return err
		}
		n = int(total)
		s.noteRows(n)
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("counting beads: %w", err)
	}
	return n, nil
}

// nativeDoltCountSupported reports whether Count can answer query with
// List-cardinality parity. Rejected shapes and why:
//   - excludeTypes: the upstream IssueFilter has no type-exclusion
//     predicate; List's callers apply it post-hydration.
//   - non-default tiers: nativeIssueFilterFromListQuery defers the wisp
//     tier filter to ApplyListQuery, so the backend result is a superset.
//   - Status "open": translated to an exclude-list (closed, in_progress)
//     while Matches requires status == "open" exactly, so beads in any
//     other non-excluded status would be overcounted.
//   - Assignees / ParentIDs / IDs / SeekAfter / UpdatedBefore: not translated
//     into the filter at all; List narrows them Go-side.
//   - Metadata / CreatedBefore / ParentID: translated, but List re-applies
//     them through Matches with Go-side semantics (exact metadata match vs
//     the backend's own predicate, Before() precision, the parent
//     projection) a bare COUNT cannot be proven to reproduce — the same
//     over-conservative gate the DoltLite Counter applies.
//   - Limit: the Counter contract is List cardinality, including List's
//     post-sort limit cap.
//
// TierBoth is supported alongside TierIssues: it is the one tier with no
// narrowing anywhere — no SQL ephemeral predicate (nativeIssueFilterFromListQuery
// emits no tier filter) and no Go-side re-filter (matchesTier returns true
// unconditionally) — so CountIssues' SearchIssues-mirroring merge is already
// the exact List cardinality. This matters in practice: beadPolicyStore
// expands every TierIssues read to TierBoth, so a TierIssues-only gate makes
// policy-wrapped cities (any city with bead policies, e.g. order_tracking
// retention) silently lose the Counter fast path and fall back to hydration.
// TierWisps stays unsupported: its ephemeral||no-history membership is
// resolved Go-side and has no exact filter translation.
//
// This gate is unchanged by the removal of the Counter-role http fallback
// above: it governs only when the RAW CountIssues answer (a real backend
// query) can be trusted, which remains true regardless of how a refusal is
// classified.
func nativeDoltCountSupported(query ListQuery, excludeTypes []string) bool {
	return len(excludeTypes) == 0 &&
		(query.TierMode == TierIssues || query.TierMode == TierBoth) &&
		query.Status != "open" &&
		len(query.Assignees) == 0 &&
		len(query.ParentIDs) == 0 &&
		len(query.IDs) == 0 &&
		query.SeekAfter == nil &&
		query.UpdatedBefore.IsZero() &&
		len(query.Metadata) == 0 &&
		query.CreatedBefore.IsZero() &&
		query.ParentID == "" &&
		query.Limit == 0
}
