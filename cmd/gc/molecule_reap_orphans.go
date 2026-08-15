package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/molecule"
)

// reapOrphanCloseReason is the close_reason metadata value stamped on
// molecule subtrees closed by reapOrphanMolecules, distinct from
// moleculeAutocloseReason/moleculeSourceAutocloseReason so an operator
// reading bd show can tell a crn-vbsr0 orphan reap apart from the ordinary
// step-terminal or source-bead-close autoclose triggers.
const reapOrphanCloseReason = "molecule reap-orphans: root non-terminal, target(s) terminal, all steps never entered (crn-vbsr0)"

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
// closes their subtrees (crn-vbsr0). A root qualifies when: (1) it is a
// non-terminal "molecule" bead; (2) it has at least one attached target bead
// (metadata.molecule_id == root.ID) and every such target is terminal —
// exactly the shape attachedMoleculeIsParked already protects, since #3474 is
// this function's counterpart, not a competing check; and (3) every
// descendant in its subtree has never been touched since creation, which is
// the discriminator that makes closing safe here: a molecule with any
// touched-but-open step is genuinely in flight (parked, not orphaned) and
// must be left alone. Molecules with zero descendants are skipped too, since
// that shape means instantiation has not finished yet rather than that the
// molecule is orphaned.
func reapOrphanMolecules(store beads.Store, opts reapOrphansOptions, stdout io.Writer) ([]reapOrphanResult, error) {
	candidates, err := store.List(beads.ListQuery{Type: "molecule"})
	if err != nil {
		return nil, fmt.Errorf("listing open molecules: %w", err)
	}

	var results []reapOrphanResult
	for _, root := range candidates {
		orphan, err := isOrphanMolecule(store, root)
		if err != nil {
			return nil, fmt.Errorf("checking molecule %s: %w", root.ID, err)
		}
		if !orphan {
			continue
		}

		if opts.DryRun {
			results = append(results, reapOrphanResult{RootID: root.ID, Title: root.Title, DryRun: true})
			fmt.Fprintf(stdout, "would reap orphan molecule %s %q\n", root.ID, root.Title) //nolint:errcheck // best-effort stdout
			continue
		}

		closed, err := molecule.CloseSubtreeWithMetadata(store, root.ID, map[string]string{
			"close_reason": reapOrphanCloseReason,
		})
		if err != nil {
			return nil, fmt.Errorf("closing orphan molecule %s: %w", root.ID, err)
		}
		results = append(results, reapOrphanResult{RootID: root.ID, Title: root.Title, Closed: closed})
		fmt.Fprintf(stdout, "reaped orphan molecule %s %q (%d beads closed)\n", root.ID, root.Title, closed) //nolint:errcheck // best-effort stdout
	}
	return results, nil
}

// isOrphanMolecule applies the crn-vbsr0 reap predicate to a single
// candidate root. See reapOrphanMolecules for the full rationale.
func isOrphanMolecule(store beads.Store, root beads.Bead) (bool, error) {
	if root.Type != "molecule" || convoycore.IsTerminalStatus(root.Status) {
		return false, nil
	}

	targets, err := store.ListByMetadata(map[string]string{beadmeta.MoleculeIDMetadataKey: root.ID}, 0, beads.IncludeClosed, beads.WithBothTiers)
	if err != nil {
		return false, fmt.Errorf("listing target beads for molecule %s: %w", root.ID, err)
	}
	if len(targets) == 0 {
		// Vacuous truth guard: a molecule never slung to a target has not
		// started yet, not orphaned.
		return false, nil
	}
	for _, target := range targets {
		if !convoycore.IsTerminalStatus(target.Status) {
			return false, nil
		}
	}

	subtree, err := molecule.ListSubtree(store, root.ID)
	if err != nil {
		return false, fmt.Errorf("listing subtree for molecule %s: %w", root.ID, err)
	}
	descendants := 0
	for _, b := range subtree {
		if b.ID == root.ID {
			continue
		}
		descendants++
		if beadWasEntered(b) {
			// Touched-but-open descendant: this is the #3474 parked-molecule
			// shape, not an orphan. Veto the whole subtree.
			return false, nil
		}
	}
	if descendants == 0 {
		// Root only — instantiation in progress or already-cleaned
		// scaffolding. Leave it; closing here would race the instantiator.
		return false, nil
	}
	return true, nil
}

// beadWasEntered reports whether b has been touched since creation. Legacy
// beads with a zero UpdatedAt are treated as untouched, matching
// UpdatedBefore's documented CreatedAt fallback elsewhere in this package.
func beadWasEntered(b beads.Bead) bool {
	return !b.UpdatedAt.IsZero() && !b.UpdatedAt.Equal(b.CreatedAt)
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
