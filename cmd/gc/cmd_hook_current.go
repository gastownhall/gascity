package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// hookCurrentSessionFrontDoor is the session-front-door seam `gc hook current`
// reads through, overridable in tests.
var hookCurrentSessionFrontDoor = sessionCurrentClaimFrontDoor

func newHookCurrentCmd(stdout, stderr io.Writer) *cobra.Command {
	var idOnly bool
	cmd := &cobra.Command{
		Use:   "current",
		Short: "Print the work bead this session most recently claimed",
		Long: `Prints the work bead this session most recently claimed with gc hook --claim.

The claim protocol stamps the claimed bead id onto the calling session's own
bead, because the environment cannot name it: $GC_BEAD_ID exists only in the
controller's dispatch condition environment, never in a session shell, and no
session shell carries the id of the bead that triggered its spawn. A
demand-spawned session sees only $GC_SPAWN_ORIGIN=demand, a presence marker
that names no bead; the pool is pull. A formula step that must close the bead
it is running reads the stamp back here:

    BEAD_ID="${GC_BEAD_ID:-$(gc hook current --id-only)}"

The stamp is verified against the live work ledger before it prints: the
stamped bead must still be in_progress, still assigned to this session's
actor, and still stamped to this session. When it is not — the step was
closed, released, or claimed by another session — the command resolves the
session's live claim from the ledger instead (the rows 'gc bd list
--assignee=<actor> --status=in_progress' answers, filtered to this session)
and prints it when exactly one matches. It never re-stamps: repairing the
pointer is the claim path's job, not a read's side effect. A transient ledger
read failure prints the stamp unverified with a warning — a working session
must not be failed by a store hiccup — while an empty or ambiguous ledger
(Zero or several live claims) prints nothing and exits 1: guessing at a
claim is how one session closes another's bead.

The calling session is taken from $GC_SESSION_ID. Exits 1 when there is no
session identity and when the session has claimed nothing, so a caller that
cannot name its bead fails loudly instead of skipping its own work.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return exitForCode(cmdHookCurrent(idOnly, stdout, stderr))
		},
	}
	cmd.Flags().BoolVar(&idOnly, "id-only", false, "print only the bead id, with no surrounding context")
	return cmd
}

// cmdHookCurrent resolves the calling session from the environment and prints
// its current claim. It is the thin env+front-door root over doHookCurrent, which
// holds the whole decision so it can be exercised without a city on disk.
func cmdHookCurrent(idOnly bool, stdout, stderr io.Writer) int {
	sessionID := strings.TrimSpace(os.Getenv("GC_SESSION_ID"))
	if sessionID == "" {
		fmt.Fprintln(stderr, "gc hook current: no session identity (set $GC_SESSION_ID); only a session that claimed work has a current bead") //nolint:errcheck
		return 1
	}
	sessFront, err := hookCurrentSessionFrontDoor()
	if err != nil {
		fmt.Fprintf(stderr, "gc hook current: %v\n", err) //nolint:errcheck
		return 1
	}
	return doHookCurrent(sessFront, sessionID, idOnly, stdout, stderr)
}

// doHookCurrent prints the bead id stamped on sessionID's bead by the claim
// protocol, once the live work ledger agrees it is still this session's claim
// (or once the ledger names the claim the stamp forgot). It exits 1 — never 0
// with empty output — when nothing is stamped and nothing is live: the whole
// point of the back-channel is that a step which cannot name its own bead must
// fail loudly rather than let a caller substitute an empty string and skip the
// close it owes.
func doHookCurrent(sessFront *session.Store, sessionID string, idOnly bool, stdout, stderr io.Writer) int {
	return resolveHookCurrentClaim(sessFront, sessionID, hookCurrentActorFromEnv(), hookCurrentLedgerProbe(), idOnly, stdout, stderr)
}

// resolveHookCurrentClaim is the whole decision over an injected actor and
// ledger, so every branch is exercisable without a city on disk. A nil ledger
// is the legacy behavior: print the stamp, unverified.
func resolveHookCurrentClaim(sessFront *session.Store, sessionID, actor string, ledger *hookCurrentLedger, idOnly bool, stdout, stderr io.Writer) int {
	stamp, err := sessFront.CurrentClaimBeadID(sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook current: %v\n", err) //nolint:errcheck
		return 1
	}
	if stamp == "" {
		if ledger != nil && actor != "" {
			return hookCurrentPrintResolved(ledger, actor, sessionID, "", idOnly, stdout, stderr)
		}
		fmt.Fprintf(stderr, "gc hook current: session %s has no current claim (nothing claimed through gc hook --claim)\n", sessionID) //nolint:errcheck
		return 1
	}
	if ledger == nil {
		return hookCurrentPrint(stamp, sessionID, idOnly, stdout)
	}
	work, err := ledger.readClaim(stamp)
	switch {
	case err == nil && hookCurrentStampLive(work, sessionID, actor):
		return hookCurrentPrint(stamp, sessionID, idOnly, stdout)
	case err == nil, errors.Is(err, beads.ErrNotFound):
		// Stale or absent stamp — resolve from the ledger below.
	default:
		// A transient read failure must neither print a guess nor fail a
		// working session: report the uncertainty and print the stamp, the
		// same answer the command gave before it could verify anything.
		fmt.Fprintf(stderr, "gc hook current: warning: cannot verify stamped claim %s against the work ledger: %v; printing it unverified\n", stamp, err) //nolint:errcheck
		return hookCurrentPrint(stamp, sessionID, idOnly, stdout)
	}
	if actor == "" {
		fmt.Fprintf(stderr, "gc hook current: stamped claim %s is no longer this session's live claim, and no actor identity is available to resolve one from the ledger (set $BEADS_ACTOR or $GC_AGENT)\n", stamp) //nolint:errcheck
		return 1
	}
	return hookCurrentPrintResolved(ledger, actor, sessionID, stamp, idOnly, stdout, stderr)
}

// hookCurrentPrintResolved answers from the ledger's live rows. stamp is the
// stale id when one is being corrected, "" when nothing was ever stamped.
func hookCurrentPrintResolved(ledger *hookCurrentLedger, actor, sessionID, stamp string, idOnly bool, stdout, stderr io.Writer) int {
	live, err := ledger.listLiveClaims(actor)
	if err != nil {
		if stamp == "" {
			fmt.Fprintf(stderr, "gc hook current: session %s has no stamped claim, and the work ledger could not be read to resolve one: %v\n", sessionID, err) //nolint:errcheck
		} else {
			fmt.Fprintf(stderr, "gc hook current: stamped claim %s is stale, and the work ledger could not be read to resolve this session's live claim: %v\n", stamp, err) //nolint:errcheck
		}
		return 1
	}
	candidates := hookCurrentCandidates(live, sessionID)
	switch len(candidates) {
	case 1:
		if stamp != "" {
			fmt.Fprintf(stderr, "gc hook current: stamped claim %s is stale; resolved this session's live claim %s from the work ledger\n", stamp, candidates[0]) //nolint:errcheck
		} else {
			fmt.Fprintf(stderr, "gc hook current: resolved this session's live claim %s from the work ledger (no claim was stamped)\n", candidates[0]) //nolint:errcheck
		}
		return hookCurrentPrint(candidates[0], sessionID, idOnly, stdout)
	case 0:
		if stamp == "" {
			fmt.Fprintf(stderr, "gc hook current: session %s has no current claim (nothing claimed through gc hook --claim, and nothing in_progress is assigned to %s)\n", sessionID, actor) //nolint:errcheck
		} else {
			fmt.Fprintf(stderr, "gc hook current: stamped claim %s is stale, and session %s holds no live in_progress claim in the work ledger\n", stamp, sessionID) //nolint:errcheck
		}
		return 1
	default:
		fmt.Fprintf(stderr, "gc hook current: %d live in_progress claims are assigned to %s (%s); refusing to pick one — gc hook --claim records exactly one current claim, so this session's ledger state is ambiguous\n", len(candidates), actor, strings.Join(candidates, ", ")) //nolint:errcheck
		return 1
	}
}

// hookCurrentCandidates filters the ledger's live rows to claims this session
// may answer for: in_progress, and either unstamped or stamped to this
// session. Assignee and status were already filtered by the list itself; the
// session fence is what keeps a same-agent-name sibling session's claim from
// being read as this session's.
func hookCurrentCandidates(live []beads.Bead, sessionID string) []string {
	var ids []string
	for _, b := range live {
		if b.Status != "in_progress" {
			continue
		}
		if owner := strings.TrimSpace(b.Metadata[beadmeta.SessionIDMetadataKey]); owner != "" && owner != sessionID {
			continue
		}
		ids = append(ids, b.ID)
	}
	return ids
}

// hookCurrentStampLive reports whether the ledger still holds the stamped
// bead as this session's claim: in_progress, not released or re-claimed
// elsewhere (the shared claimedWorkStillHeldBy fence), and — when this shell
// knows its actor identity — still assigned to it.
func hookCurrentStampLive(work beads.Bead, sessionID, actor string) bool {
	if work.Status != "in_progress" {
		return false
	}
	if !claimedWorkStillHeldBy(work, sessionID) {
		return false
	}
	return actor == "" || strings.TrimSpace(work.Assignee) == actor
}

// hookCurrentPrint is the command's only stdout contract: --id-only prints
// exactly the bare id (a substituted value must carry no context), the full
// form prints the id with the session that claimed it.
func hookCurrentPrint(beadID, sessionID string, idOnly bool, stdout io.Writer) int {
	if idOnly {
		fmt.Fprintln(stdout, beadID) //nolint:errcheck
		return 0
	}
	fmt.Fprintf(stdout, "%s (claimed by session %s)\n", beadID, sessionID) //nolint:errcheck
	return 0
}
