package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// hookCurrentOpen opens the context `gc hook current` verifies the claim
// stamp against, overridable in tests.
var hookCurrentOpen = openHookCurrentContext

func newHookCurrentCmd(stdout, stderr io.Writer) *cobra.Command {
	var idOnly bool
	cmd := &cobra.Command{
		Use:   "current",
		Short: "Print the work bead this session currently holds",
		Long: `Prints the work bead this session currently holds, as claimed with gc hook --claim.

The claim protocol stamps the claimed bead id onto the calling session's own
bead, because the environment cannot name it: $GC_BEAD_ID exists only in the
controller's dispatch condition environment, never in a session shell, and no
session shell carries the id of the bead that triggered its spawn. A
demand-spawned session sees only $GC_SPAWN_ORIGIN=demand, a presence marker
that names no bead; the pool is pull. A formula step that must close the bead
it is running reads the stamp back here:

    BEAD_ID="${GC_BEAD_ID:-$(gc hook current --id-only)}"

The stamp is written best-effort, so it is printed only when the live ledger
says this session still holds that bead: in progress, not claimed since by
another session, and assigned to one of this session's identities. When the
stamp is stale or missing, the one recorded hook delivery the session still
holds is printed instead, with a note on stderr. The stamp is never rewritten.

The calling session is taken from $GC_SESSION_ID. Exits 1, printing nothing,
when there is no session identity, when the session holds no verified claim,
and when it cannot decide: several held deliveries, a stamp a newer held
delivery contradicts, or a bead or rig store it could not read. A caller that
cannot name its bead fails loudly instead of skipping its own work or closing
someone else's.`,
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
	ctx, err := hookCurrentOpen()
	if err != nil {
		fmt.Fprintf(stderr, "gc hook current: %v\n", err) //nolint:errcheck
		return 1
	}
	return doHookCurrent(ctx, sessionID, idOnly, stdout, stderr)
}

// doHookCurrent prints the claim sessionID currently holds
// (resolveHookCurrent), disclosing on stderr a claim that is not the session's
// stamp. It exits 1 — never 0 with empty output — when there is
// none it can verify: the whole point of the back-channel is that a step which
// cannot name its own bead must fail loudly rather than let a caller
// substitute an empty string, or someone else's bead, and close it.
func doHookCurrent(ctx hookCurrentContext, sessionID string, idOnly bool, stdout, stderr io.Writer) int {
	beadID, note, err := resolveHookCurrent(ctx, sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook current: %v\n", err) //nolint:errcheck
		return 1
	}
	if note != "" {
		fmt.Fprintf(stderr, "gc hook current: %s\n", note) //nolint:errcheck
	}
	if idOnly {
		fmt.Fprintln(stdout, beadID) //nolint:errcheck
		return 0
	}
	fmt.Fprintf(stdout, "%s (claimed by session %s)\n", beadID, sessionID) //nolint:errcheck
	return 0
}
