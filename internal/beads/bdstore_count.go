package beads

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Count implements the optional Counter capability for the bd-CLI store,
// answering the closed-inclusive whole-ledger scan without hydrating rows:
// `bd count --json --include-infra`, which bd documents as matching
// `bd list --include-infra --all` cardinality.
//
// This is the path countBeadStoreRows (internal/api/store_health.go) prefers
// for the retained-row store-health denominator, because the hydrating List
// fallback cannot finish inside statusStoreReadTimeout on a long-lived city:
// the status block was permanently partial on proxied BdStore scopes for exactly
// this reason (gastownhall/gascity#7546; measured 2026-10-27 — the hydrating
// scan took 5.2-8.0s against a 1s budget on a 25.6k-row city, while bd count
// answered the same cardinality in 0.03s).
//
// Scope is deliberately the one shape whose parity is proven: an unfiltered,
// unlimited, closed-inclusive TierIssues scan. bd count's narrowing filters do
// not include the infra/wisp rows BdStore.List always adds --include-infra
// --include-gates for (measured: bd count --status open = 25 against the list
// shape's 27), and excludeTypes cannot be expressed at all, so every narrowed
// shape reports ErrCountUnsupported and callers fall back to List per the
// Counter contract.
func (s *BdStore) Count(ctx context.Context, query ListQuery, excludeTypes ...string) (int, error) {
	if err := query.Validate(); err != nil {
		return 0, err
	}
	if !query.HasFilter() && !query.AllowScan {
		return 0, fmt.Errorf("bd count: %w", ErrQueryRequiresScan)
	}
	if !bdCountSupportsQuery(query, excludeTypes) {
		return 0, fmt.Errorf("bd count: %w", ErrCountUnsupported)
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
	}
	out, err := s.runBDTransientRead("count", "--json", "--include-infra")
	if err != nil {
		if isBdCountCommandUnsupported(err) {
			return 0, fmt.Errorf("bd count: %w: %w", ErrCountUnsupported, err)
		}
		return 0, fmt.Errorf("bd count: %w", err)
	}
	var result struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(extractJSON(out), &result); err != nil {
		return 0, fmt.Errorf("bd count: parsing JSON: %w", err)
	}
	if result.Count > 0 {
		// A positive whole-ledger count is direct evidence the store holds
		// rows, the same evidence noteServerRows latches from a listed page.
		s.noteServerRows(result.Count)
	}
	return result.Count, nil
}

// bdCountSupportsQuery reports whether bd count can answer query with exactly
// List's cardinality. Live is a read-mode flag rather than a filter, so it does
// not disqualify; SkipLabels changes the payload, not the row count.
func bdCountSupportsQuery(query ListQuery, excludeTypes []string) bool {
	if len(excludeTypes) > 0 {
		return false
	}
	if !query.IncludesClosed() {
		return false
	}
	if query.HasFilter() {
		return false
	}
	if query.Limit > 0 {
		return false
	}
	return query.TierMode == TierIssues
}

// isBdCountCommandUnsupported reports whether err is bd refusing because THIS
// bd build lacks the count verb (a bd older than the command). A capability-
// absent refusal is ErrCountUnsupported — the same verdict family
// isBdBlockedUnsupported hands the blocked door — so callers fall back to List
// instead of reporting a store-health failure on an otherwise healthy ledger.
func isBdCountCommandUnsupported(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "count") {
		return false
	}
	return strings.Contains(msg, "unknown command") ||
		strings.Contains(msg, "not supported") ||
		strings.Contains(msg, "not yet supported") ||
		strings.Contains(msg, "unsupported")
}
