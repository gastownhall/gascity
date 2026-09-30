package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// The blocked-outcome veto (ga-id9fqz, ruling ga-mmmczu option A).
//
// The controller's demand side reads Store.Ready(), which refuses a candidate
// whose ready-blocking dependency closed with gc.work_outcome=blocked — the
// work-record-close protocol's outcome for "committed, not merged". The hook's
// serve side reads `bd ready`, which checks blocker STATUS only, so a worker
// could be served — and could claim — a row the controller never counted, and no
// seat was ever woken for it. Both sides now refuse it.
//
// The veto runs in the leg runner every serving path reads through
// (hookServeRunner) rather than as a chained call at each consumer, because it
// needs the store the rows came from: the discovery door and the cross-store
// resume check only ever see a leg's output, never which leg produced it.

// hookBlockerReadTimeout bounds the veto's one batched bd read. A healthy read
// is a single `bd show` (0.4 s measured on a 25k-closed-bead store); the bound
// only stops a wedged store from holding a hook call past the work query it
// already survived. On expiry the veto fails open, like any other read failure.
const hookBlockerReadTimeout = 30 * time.Second

// hookBlockerStoreOpener opens the bd workspace a work query just read, so the
// veto reads a candidate's blockers from the store that holds them. Blockers
// live in their dependent's store; a cross-store blocking edge is not a
// supported shape.
type hookBlockerStoreOpener func(ctx context.Context, dir string, env []string) beads.ExactBatchGetter

// openHookBlockerStore is the production hookBlockerStoreOpener: the leg's own
// bd workspace, with its bd children bound to ctx.
func openHookBlockerStore(ctx context.Context, dir string, env []string) beads.ExactBatchGetter {
	return hookClaimBdStoreContext(ctx, dir, env, "")
}

// hookServeRunner is the work-query runner every serving path reads through:
// shellWorkQueryWithEnv with the blocked-outcome veto applied to each leg's
// rows. Discovery (cmdHookWithOptions) and claim (claimHookWork) both take it,
// and TestNoHookServeEntryPointBypassesTheBlockedOutcomeVeto fails the build if
// a serving entry point hands bestStoreWithWork or claimHookWorkWithRunner the
// bare shell runner instead.
func hookServeRunner(stderr io.Writer) hookStoreRunner {
	return withHookBlockedOutcomeVeto(shellWorkQueryWithEnv, openHookBlockerStore, stderr)
}

// withHookBlockedOutcomeVeto wraps run so the rows it returns for a leg have the
// blocked-outcome veto applied against that leg's own store (dir, env). Every
// consumer downstream — leg selection and ranking (bestStoreWithWork), claim-time
// re-validation (claimStoreWithFallback), the claim itself and plain `gc hook`
// display — therefore sees only servable rows.
//
// It is a strict no-op unless a row is actually vetoed: an output with nothing to
// veto comes back byte-for-byte as run produced it, and a failed work query is
// passed through untouched. A veto that cannot run — the store read failed or
// timed out — fails OPEN: the rows are served as before and the failure is
// reported on stderr. Failing closed would idle every seat in the city on a
// store hiccup, which is a worse outage than the over-serving it prevents.
func withHookBlockedOutcomeVeto(run hookStoreRunner, open hookBlockerStoreOpener, stderr io.Writer) hookStoreRunner {
	return func(command, dir string, env []string) (string, error) {
		out, err := run(command, dir, env)
		if err != nil {
			return out, err
		}
		candidates := filterUnreadyHookCandidates(normalizeWorkQueryOutput(strings.TrimSpace(out)), time.Now())
		ctx, cancel := context.WithTimeout(context.Background(), hookBlockerReadTimeout)
		defer cancel()
		vetoed, vetoErr := filterBlockedOutcomeHookCandidates(candidates, open(ctx, dir, env))
		if vetoErr != nil {
			fmt.Fprintf(stderr, "gc hook: blocked-outcome veto unavailable: %v; serving candidates without it\n", vetoErr) //nolint:errcheck // best-effort stderr
			return out, nil
		}
		if vetoed == candidates {
			return out, nil
		}
		return vetoed, nil
	}
}

// filterBlockedOutcomeHookCandidates drops the candidates whose ready-blocking
// dependency is closed with a work outcome that does not satisfy it
// (beads.DependencySatisfied): a blocker closed gc.work_outcome=blocked has
// committed its work without landing it, so the dependent must not start yet.
// candidates is the already-filtered work-query JSON (filterUnreadyHookCandidates
// has run); the survivors come back in their original order.
//
// The veto is deliberately narrow, mirroring the store-side veto it agrees with
// (BdStore.filterReadyByWorkOutcome):
//   - only OPEN candidates are considered. An in_progress row is a resume, and
//     the demand side never gates a claimed row, so vetoing it here would strand
//     work a session is already doing;
//   - only ready-blocking edge types count (beads.IsReadyBlockingDependencyType);
//   - only a blocker bd answered for AND that is closed can veto. A blocker bd
//     could not resolve, or that is still open, is bd's own verdict to make — the
//     veto never re-blocks a row bd already offered on a dependency it could not
//     read.
//
// Edges come from each row's inline typed `dependencies`, the shape live
// `bd ready --json` emits. Its `blocked_by` is null on every ready row and so
// cannot carry a closed blocker. Every blocker is read with ONE batched
// GetExactBatch (`bd show --json <ids...>`) keyed by distinct blocker id, and
// only when some open candidate has a ready-blocking edge. The store-side
// shape, List{IDs, Status: closed}, would scan the whole closed population on
// every hook call — 118 MB, 3.3 s and 730 MB RSS on a 25k-closed-bead store,
// against 80 KB and 0.4 s for the batched read.
//
// Input that is not a JSON array passes through unchanged, and so does every
// row that fails to decode, as in the sibling filters. When the blocker read
// fails the input is returned unchanged with the error, and the caller reports
// it and serves the rows anyway.
func filterBlockedOutcomeHookCandidates(candidates string, blockers beads.ExactBatchGetter) (string, error) {
	var decoded any
	if err := json.Unmarshal([]byte(candidates), &decoded); err != nil {
		return candidates, nil
	}
	rows, ok := decoded.([]any)
	if !ok {
		return candidates, nil
	}

	blockerIDsByRow := make([][]string, len(rows))
	var distinct []string
	seen := make(map[string]bool)
	for i, item := range rows {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		candidate, ok := decodeHookCandidateBead(obj)
		if !ok || !strings.EqualFold(strings.TrimSpace(candidate.Status), "open") {
			continue
		}
		for _, dep := range candidate.Dependencies {
			if !beads.IsReadyBlockingDependencyType(dep.Type) {
				continue
			}
			blockerIDsByRow[i] = append(blockerIDsByRow[i], dep.DependsOnID)
			if !seen[dep.DependsOnID] {
				seen[dep.DependsOnID] = true
				distinct = append(distinct, dep.DependsOnID)
			}
		}
	}
	if len(distinct) == 0 {
		return candidates, nil
	}
	if blockers == nil {
		return candidates, errors.New("no store to read blocking dependencies from")
	}

	found, _, err := blockers.GetExactBatch(distinct)
	if err != nil {
		return candidates, fmt.Errorf("reading %d blocking dependencies: %w", len(distinct), err)
	}

	kept := make([]any, 0, len(rows))
	for i, item := range rows {
		if !hookBlockerHandedOff(blockerIDsByRow[i], found) {
			kept = append(kept, item)
		}
	}
	if len(kept) == len(rows) {
		return candidates, nil
	}
	reencoded, err := json.Marshal(kept)
	if err != nil {
		return candidates, fmt.Errorf("re-encoding vetoed candidates: %w", err)
	}
	return string(reencoded), nil
}

// hookBlockerHandedOff reports whether any of ids names a blocker that is closed
// with an outcome that does not satisfy the dependency. An id absent from found
// is a blocker bd could not answer for, which is no evidence either way.
func hookBlockerHandedOff(ids []string, found map[string]beads.Bead) bool {
	for _, id := range ids {
		blocker, ok := found[id]
		if !ok || blocker.Status != "closed" {
			continue
		}
		if !beads.DependencySatisfied(blocker.Status, blocker.Metadata[beadmeta.WorkOutcomeMetadataKey]) {
			return true
		}
	}
	return false
}
