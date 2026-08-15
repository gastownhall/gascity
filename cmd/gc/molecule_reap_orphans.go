package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/beads"
)

// reapOrphansOptions configures reapOrphanMolecules.
type reapOrphansOptions struct {
	// DryRun reports what would be reaped without closing anything.
	DryRun bool
}

// reapOrphanResult describes one molecule subtree reapOrphanMolecules closed,
// or would close under DryRun.
type reapOrphanResult struct {
	RootID string
	Title  string
	Closed int
	DryRun bool
}

// reapOrphanMolecules finds molecule roots left open with no runnable work and
// closes their subtrees (crn-vbsr0). Not yet implemented — TDD RED.
func reapOrphanMolecules(_ beads.Store, _ reapOrphansOptions, _ io.Writer) ([]reapOrphanResult, error) {
	return nil, nil
}

// newMoleculeReapOrphansCmd is a manually-invoked maintenance command: an
// operator cds into a city or rig and runs it to clear molecule roots
// stranded by crn-vbsr0. Unlike the autoclose hook commands it is not
// best-effort — failures are surfaced to the caller.
func newMoleculeReapOrphansCmd(stdout io.Writer) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "reap-orphans",
		Short: "Close molecule roots left open with no runnable work",
		Long: strings.TrimSpace(`
Finds molecule roots whose attached target beads have all reached a terminal
status and whose step descendants were never entered, then closes each
orphaned subtree. Use --dry-run to preview without closing anything.
`),
		RunE: func(*cobra.Command, []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("getting working directory: %w", err)
			}
			storeRoot := convoyAutocloseStoreRoot(cwd)
			cityPath := autocloseCityPathForStoreRoot(storeRoot)
			store, err := openStoreAtForCity(storeRoot, cityPath)
			if err != nil {
				return fmt.Errorf("opening store: %w", err)
			}
			results, err := reapOrphanMolecules(store, reapOrphansOptions{DryRun: dryRun}, stdout)
			if err != nil {
				return err
			}
			if len(results) == 0 {
				fmt.Fprintln(stdout, "no orphan molecules found") //nolint:errcheck // best-effort stdout
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview what would be reaped without closing anything")
	return cmd
}
