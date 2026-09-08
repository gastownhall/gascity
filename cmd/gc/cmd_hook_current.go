package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

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
bead, because the environment alone cannot reliably name it: $GC_BEAD_ID exists
only in the controller's dispatch condition environment, never in a session
shell, and $GC_TRIGGER_BEAD_ID — exported to demand-spawned pool seats as a
pool-level spawn marker — is absent on other seats (e.g. a warm seat bound
after start) and never decides what a session claims; the pool is pull. It
appears in the chain below only as a name fallback for work already claimed:
for a vapor wisp the trigger IS the work bead. A formula step that must close
the bead it is running reads the stamp back here:

    BEAD_ID="${GC_BEAD_ID:-${GC_TRIGGER_BEAD_ID:-$(gc hook current --id-only)}}"

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
// protocol. It exits 1 — never 0 with empty output — when nothing is stamped:
// the whole point of the back-channel is that a step which cannot name its own
// bead must fail loudly rather than let a caller substitute an empty string and
// skip the close it owes.
func doHookCurrent(sessFront *session.Store, sessionID string, idOnly bool, stdout, stderr io.Writer) int {
	beadID, err := sessFront.CurrentClaimBeadID(sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook current: %v\n", err) //nolint:errcheck
		return 1
	}
	if beadID == "" {
		fmt.Fprintf(stderr, "gc hook current: session %s has no current claim (nothing claimed through gc hook --claim)\n", sessionID) //nolint:errcheck
		return 1
	}
	if idOnly {
		fmt.Fprintln(stdout, beadID) //nolint:errcheck
		return 0
	}
	fmt.Fprintf(stdout, "%s (claimed by session %s)\n", beadID, sessionID) //nolint:errcheck
	return 0
}
