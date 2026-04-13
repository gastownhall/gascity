package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// isRigAgent reports whether the current process is running as a rig agent.
// Rig agents have a "/" in GC_AGENT (e.g. "gascity/polecat"), whereas
// city-level agents do not (e.g. "mayor", "deputy").
func isRigAgent() bool {
	return strings.Contains(os.Getenv("GC_AGENT"), "/")
}

// withRigAgentGuard wraps cmd so that city-level operations are blocked when
// the caller is a rig agent. Rig agents are identified by a "/" in GC_AGENT.
// Any existing PersistentPreRunE is chained after the guard check.
func withRigAgentGuard(cmd *cobra.Command) *cobra.Command {
	prior := cmd.PersistentPreRunE
	cmd.PersistentPreRunE = func(c *cobra.Command, args []string) error {
		if isRigAgent() {
			fmt.Fprintf(c.ErrOrStderr(), //nolint:errcheck // best-effort stderr
				"gc: city-level commands are not permitted from rig agents (GC_AGENT=%s).\n"+
					"    Use 'gc mail send deputy' to request city-level changes.\n",
				os.Getenv("GC_AGENT"),
			)
			return errExit
		}
		if prior != nil {
			return prior(c, args)
		}
		return nil
	}
	return cmd
}
