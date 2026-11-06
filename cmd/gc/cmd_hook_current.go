package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/session"
)

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

The stamp is written best-effort, so it is verified against the live ledger
before it is printed: the stamped bead must still be in progress, held by this
session, and assigned to one of its identities. A stamp the ledger does not
back is not printed — the id this command hands out is a close target, and an
unreadable claim refuses instead of printing an id the caller might close. The
command never repairs the stamp — that is the claim path's job.

When the stamp no longer holds — stale, or never written — the session's
recorded hook deliveries are verified the same way, and the one live claim
they still name is printed, with the substitution disclosed on stderr only.
Zero or several live claims refuse: the command never guesses.

The calling session is taken from $GC_SESSION_ID. Exits 1 when there is no
session identity, when the session has claimed nothing it can prove, and when
the claim cannot be verified, so a caller that cannot name its bead fails
loudly instead of skipping its own work.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return exitForCode(cmdHookCurrent(idOnly, stdout, stderr))
		},
	}
	cmd.Flags().BoolVar(&idOnly, "id-only", false, "print only the bead id, with no surrounding context")
	return cmd
}

// cmdHookCurrent resolves the calling session from the environment and prints
// its current claim. It is the thin env+context root over doHookCurrent, which
// holds the whole decision so it can be exercised without a city on disk.
func cmdHookCurrent(idOnly bool, stdout, stderr io.Writer) int {
	sessionID := strings.TrimSpace(os.Getenv("GC_SESSION_ID"))
	if sessionID == "" {
		fmt.Fprintln(stderr, "gc hook current: no session identity (set $GC_SESSION_ID); only a session that claimed work has a current bead") //nolint:errcheck
		return 1
	}
	ctx, err := hookCurrentReadContext()
	if err != nil {
		fmt.Fprintf(stderr, "gc hook current: %v\n", err) //nolint:errcheck
		return 1
	}
	return doHookCurrent(ctx, sessionID, idOnly, stdout, stderr)
}

// doHookCurrent prints the session's verified current claim: the stamped id
// when the live ledger still backs it, else the one live claim the session's
// recorded deliveries still name. It exits 1 — never 0 with empty output —
// when no claim can be proven: the whole point of the back-channel is that a
// step which cannot name its own bead must fail loudly rather than let a
// caller substitute an empty string and skip the close it owes. The stamp is
// written best-effort by the claim, so its word alone is not evidence; a
// recovered claim is disclosed on stderr, stdout stays the id alone.
func doHookCurrent(ctx hookCurrentContext, sessionID string, idOnly bool, stdout, stderr io.Writer) int {
	info, err := ctx.sessFront.Get(sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook current: %v\n", err) //nolint:errcheck
		return 1
	}
	cell, err := session.DecodeWorkStarted(info.WorkStartedRaw)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook current: session %s: %v\n", sessionID, err) //nolint:errcheck
		return 1
	}
	stamp := strings.TrimSpace(info.CurrentClaimBeadID)
	read := newHookCurrentReader(ctx, sessionID, releaseScope(info, ctx.cfg))
	if stamp != "" {
		v := read.verify(stamp)
		if v.err != nil {
			fmt.Fprintf(stderr, "gc hook current: %v\n", v.err) //nolint:errcheck
			return 1
		}
		if v.held {
			newer, err := hookCurrentNewerQualifying(read, cell, stamp)
			if err != nil {
				fmt.Fprintf(stderr, "gc hook current: %v\n", err) //nolint:errcheck
				return 1
			}
			if newer != "" {
				fmt.Fprintf(stderr, "gc hook current: session %s's stamped claim %s is contradicted by a newer live recorded claim %s; refusing to pick one\n", sessionID, stamp, newer) //nolint:errcheck
				return 1
			}
			return hookCurrentPrint(stdout, sessionID, stamp, idOnly)
		}
		live, err := hookCurrentQualifyingIDs(read, cell)
		if err != nil {
			fmt.Fprintf(stderr, "gc hook current: %v\n", err) //nolint:errcheck
			return 1
		}
		switch len(live) {
		case 1:
			fmt.Fprintf(stderr, "gc hook current: session %s's stamped claim %s is no longer held (%s); printing its live recorded claim %s instead\n", sessionID, stamp, v.reason, live[0]) //nolint:errcheck
			return hookCurrentPrint(stdout, sessionID, live[0], idOnly)
		case 0:
			fmt.Fprintf(stderr, "gc hook current: session %s's stamped claim %s is no longer held (%s), and no live claim is recorded for the session\n", sessionID, stamp, v.reason) //nolint:errcheck
		default:
			fmt.Fprintf(stderr, "gc hook current: session %s holds several live claims (%s); refusing to pick one\n", sessionID, strings.Join(live, ", ")) //nolint:errcheck
		}
		return 1
	}
	live, err := hookCurrentQualifyingIDs(read, cell)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook current: %v\n", err) //nolint:errcheck
		return 1
	}
	switch len(live) {
	case 1:
		fmt.Fprintf(stderr, "gc hook current: session %s has no claim stamped; printing its one live recorded claim %s\n", sessionID, live[0]) //nolint:errcheck
		return hookCurrentPrint(stdout, sessionID, live[0], idOnly)
	case 0:
		fmt.Fprintf(stderr, "gc hook current: session %s has no current claim (nothing claimed through gc hook --claim)\n", sessionID) //nolint:errcheck
	default:
		fmt.Fprintf(stderr, "gc hook current: session %s holds several live claims (%s); refusing to pick one\n", sessionID, strings.Join(live, ", ")) //nolint:errcheck
	}
	return 1
}

// hookCurrentPrint prints one verified claim in the command's two shapes:
// bare for --id-only, else with the session that claimed it.
func hookCurrentPrint(stdout io.Writer, sessionID, beadID string, idOnly bool) int {
	if idOnly {
		fmt.Fprintln(stdout, beadID) //nolint:errcheck
		return 0
	}
	fmt.Fprintf(stdout, "%s (claimed by session %s)\n", beadID, sessionID) //nolint:errcheck
	return 0
}
