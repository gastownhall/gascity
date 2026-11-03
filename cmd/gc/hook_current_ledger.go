package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// hookCurrentLedger is the read-only work-ledger view `gc hook current` uses
// to validate its claim stamp and, when the stamp is stale or missing, to
// resolve the calling session's live claim. Both failure directions of the
// stale-pointer bug are covered on the read side, whatever write-side race
// produced the stale stamp:
//
//   - a stamp naming a CLOSED or released bead would print as the session's
//     claim, and a formula guard that trusts it refuses to drain a session
//     that has finished its work (the completed session that could not exit);
//   - a stamp naming a still-live but not-claiming bead would print, and a
//     step that closes "its" bead closes the wrong work.
//
// The command never writes. Repairing the stamp is the claim path's job
// (`gc hook --claim`); a read that re-stamped would make the pointer's
// authority depend on who last happened to look at it.

type hookCurrentLedger struct {
	// readClaim reads one bead by exact id from the store that owns its
	// prefix — the routing `gc bd <verb> <id>` applies to an addressed
	// subject. beads.ErrNotFound answers for a bead the ledger does not
	// hold; any other error is a transient read failure, which the caller
	// must treat as unknown, not as stale.
	readClaim func(beadID string) (beads.Bead, error)

	// listLiveClaims answers the rows `gc bd list --assignee=<actor>
	// --status=in_progress` would answer for the same shell — the parity
	// the read-back is held to. Newest-first, capped at
	// hookCurrentClaimLimit.
	listLiveClaims func(actor string) ([]beads.Bead, error)
}

// hookCurrentClaimLimit bounds the fallback list. A session with exactly one
// live claim is healthy; a third row means the state is ambiguous and the
// command refuses to pick — so 3 is enough to tell "one", "none-or-two", and
// "many" apart without scanning the ledger.
const hookCurrentClaimLimit = 3

// bdAssigneeStatusLister is the store capability listLiveClaims needs. It is
// probed with a type assertion the way `gc bd release-if-current` probes its
// conditional capability, so a store without it is an error, not a silent
// narrower answer.
type bdAssigneeStatusLister interface {
	ListByAssignee(assignee, status string, limit int) ([]beads.Bead, error)
}

// hookCurrentLedgerProbe is the seam resolveHookCurrentClaim reads its ledger
// through. Tests pass an explicit *hookCurrentLedger to the resolver, and
// stub this to nil where they exercise doHookCurrent's env wiring — the
// command must never reach a real ledger from a unit test.
var hookCurrentLedgerProbe = func() *hookCurrentLedger { return hookCurrentLedgerFor() }

// hookCurrentActorFromEnv names the identity the claim path writes into a
// claimed bead's assignee: BEADS_ACTOR first (the actor bd records claims
// under), GC_AGENT as the session's own fallback. "" means no actor identity
// is available: stamp validation then skips the assignee fence, and the
// ledger fallback cannot run at all.
func hookCurrentActorFromEnv() string {
	if actor := strings.TrimSpace(os.Getenv("BEADS_ACTOR")); actor != "" {
		return actor
	}
	return strings.TrimSpace(os.Getenv("GC_AGENT"))
}

// hookCurrentLedgerFor resolves the probe against the same city, scope, and
// stores a `gc bd` invocation from this shell resolves, and returns nil —
// never an error — when any part of that resolution fails. A session whose
// ledger cannot be located at all keeps the legacy stamp-only behavior; the
// probe is an improvement on the read, not a new dependency the read fails
// closed on.
func hookCurrentLedgerFor() *hookCurrentLedger {
	cityPath, err := resolveBdCity("")
	if err != nil {
		return nil
	}
	cfg, err := loadBdPassthroughConfig(cityPath, io.Discard)
	if err != nil {
		return nil
	}
	cityTarget := bdCityScopeTarget(cityPath, cfg)
	// The list target resolves exactly as a bare `gc bd list` resolves its
	// scope from this shell (GC_RIG env, enclosing rig, city root), which is
	// what keeps the fallback agreeing with the command an operator would
	// type to check the same claim. Diagnostics go to io.Discard: the
	// probe's failure answers as nil, and the command's own stderr stays
	// reserved for verdicts about the claim.
	listTarget, err := resolveBdScopeTarget(cfg, cityPath, "", []string{"list"}, false, io.Discard)
	if err != nil {
		return nil
	}
	return &hookCurrentLedger{
		readClaim: func(beadID string) (beads.Bead, error) {
			target := listTarget
			if owner, ok := bdPrefixOwner(cfg, cityTarget, cityPath, beadID); ok {
				target = owner
			}
			store, err := openStoreAtForCityWithConfig(target.ScopeRoot, cityPath, cfg)
			if err != nil {
				return beads.Bead{}, err
			}
			return store.Get(beadID)
		},
		listLiveClaims: func(actor string) ([]beads.Bead, error) {
			store, err := openStoreAtForCityWithConfig(listTarget.ScopeRoot, cityPath, cfg)
			if err != nil {
				return nil, err
			}
			lister, ok := store.(bdAssigneeStatusLister)
			if !ok {
				return nil, fmt.Errorf("the work ledger at scope %q cannot list by assignee", listTarget.ScopeRoot)
			}
			return lister.ListByAssignee(actor, "in_progress", hookCurrentClaimLimit)
		},
	}
}
