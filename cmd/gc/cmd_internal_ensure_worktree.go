package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/git"
	"github.com/spf13/cobra"
)

// newInternalEnsureWorktreeCmd provisions an isolated git worktree for one
// pool-agent session. Invoked from implicit-pool PreStart hooks so parallel
// dispatches never share the rig root working tree.
func newInternalEnsureWorktreeCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "ensure-worktree",
		Short:  "Ensure an isolated git worktree for one agent session",
		Hidden: true,
		Args:   cobra.ExactArgs(3),
		RunE: func(_ *cobra.Command, args []string) error {
			rigRoot := strings.TrimSpace(args[0])
			workDir := strings.TrimSpace(args[1])
			agentName := strings.TrimSpace(args[2])
			if err := git.EnsureAgentWorktree(rigRoot, workDir, agentName); err != nil {
				fmt.Fprintf(stderr, "gc internal ensure-worktree: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			fmt.Fprintln(stdout, workDir) //nolint:errcheck // best-effort stdout
			return nil
		},
	}
	return cmd
}
