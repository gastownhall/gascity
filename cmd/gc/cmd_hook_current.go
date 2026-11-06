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
	ctx, err := hookCurrentOpen()
	if err != nil {
		fmt.Fprintf(stderr, "gc hook current: %v\n", err) //nolint:errcheck
		return 1
	}
	return doHookCurrent(ctx, sessionID, idOnly, stdout, stderr)
}

// doHookCurrent prints the claim sessionID currently holds
// (resolveHookCurrent). It exits 1 — never 0 with empty output — when there is
// none it can verify: the whole point of the back-channel is that a step which
// cannot name its own bead must fail loudly rather than let a caller
// substitute an empty string, or someone else's bead, and close it.
func doHookCurrent(ctx hookCurrentContext, sessionID string, idOnly bool, stdout, stderr io.Writer) int {
	beadID, err := resolveHookCurrent(ctx, sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook current: %v\n", err) //nolint:errcheck
		return 1
	}
	if idOnly {
		fmt.Fprintln(stdout, beadID) //nolint:errcheck
		return 0
	}
	fmt.Fprintf(stdout, "%s (claimed by session %s)\n", beadID, sessionID) //nolint:errcheck
	return 0
}
