package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/molecule"
)

// moleculeAutocloseReason is the close_reason metadata value stamped on
// molecule roots auto-closed because all of their step children are
// terminal. Mirrors convoyAutocloseReason for the convoy path.
const moleculeAutocloseReason = "molecule autoclose: all step children closed"

// newMoleculeCmd is the parent for molecule lifecycle operations.
// Hidden — exposed only so the bd close hook can dispatch into it.
func newMoleculeCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "molecule",
		Short:  "Molecule lifecycle operations",
		Hidden: true,
	}
	cmd.AddCommand(newMoleculeAutocloseCmd(stdout, stderr))
	return cmd
}

// newMoleculeAutocloseCmd is the bd-hook entry point. Best-effort; never
// returns an error so a misbehaving hook does not break the bd close
// path itself.
func newMoleculeAutocloseCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:    "autoclose <bead-id>",
		Short:  "Auto-close molecule root when all step children are terminal",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			doMoleculeAutoclose(args[0], stdout, stderr)
			return nil // always succeed — best-effort infrastructure
		},
	}
}

// doMoleculeAutoclose is the CLI entry point. It opens the cwd-rooted
// store through the provider-aware resolver and delegates to the
// testable core. Mirrors doConvoyAutoclose so the on_close hook chain
// has consistent failure semantics across the three auto-closers.
func doMoleculeAutoclose(beadID string, stdout, stderr io.Writer) {
	cwd, err := os.Getwd()
	if err != nil {
		return
	}
	storeRoot := convoyAutocloseStoreRoot(cwd)
	cityPath := autocloseCityPathForStoreRoot(storeRoot)
	store, err := openStoreAtForCity(storeRoot, cityPath)
	if err != nil {
		return
	}
	rec := openCityRecorderAt(cityPath, stderr)
	doMoleculeAutocloseWith(store, rec, beadID, stdout)
}

// doMoleculeAutocloseWith finds the molecule root the just-closed bead
// belongs to and closes that root when every transitive descendant is
// terminal. Reacts to formula-scaffolded members (steps, gates, epics,
// nested steps) — identified by the gc.root_bead_id metadata that
// molecule.Instantiate stamps onto every member — and falls back to a
// type=="step" + direct-molecule-parent check for legacy beads that
// predate the metadata convention. All errors are silently swallowed;
// this is called from a bd hook script and must not fail loudly. See
// gastownhall/gascity#1039.
func doMoleculeAutocloseWith(store beads.Store, rec events.Recorder, beadID string, stdout io.Writer) {
	bead, err := store.Get(beadID)
	if err != nil {
		return
	}
	rootID := strings.TrimSpace(bead.Metadata["gc.root_bead_id"])
	if rootID == "" {
		// Legacy fallback for pre-metadata beads: react only to typed
		// "step" closes with a direct molecule parent. Mirrors prior
		// behavior so molecules created before the metadata convention
		// still auto-close, and so a user closing a "task" bead
		// parented under a molecule does not trigger surprise close.
		if bead.Type != "step" || bead.ParentID == "" {
			return
		}
		parent, err := store.Get(bead.ParentID)
		if err != nil {
			return
		}
		autocloseMoleculeIfComplete(store, rec, parent, stdout)
		return
	}
	root, err := store.Get(rootID)
	if err != nil {
		return
	}
	autocloseMoleculeIfComplete(store, rec, root, stdout)
}

func autocloseMoleculeIfComplete(store beads.Store, rec events.Recorder, mol beads.Bead, stdout io.Writer) {
	if mol.Type != "molecule" {
		return
	}
	if convoycore.IsTerminalStatus(mol.Status) {
		return
	}

	// Walk the full transitive subtree (parent-child edges plus the
	// gc.root_bead_id metadata link) so molecules whose steps fan out
	// into nested children — formula compiler "epic" steps,
	// gate-deferred sub-trees — are evaluated by descendant terminality,
	// not just direct children. Closing on direct-children-only would
	// either fire too early (descendants still open under a closed
	// intermediate) or never (nested open child under a closed
	// intermediate but recorded children look terminal).
	subtree, err := molecule.ListSubtree(store, mol.ID)
	if err != nil {
		return
	}
	if len(subtree) <= 1 {
		// Only the root itself was returned — no descendants. The
		// molecule is either still being instantiated or already-cleaned
		// scaffolding; either way, closing here would race the
		// instantiator. Leave it.
		return
	}
	for _, b := range subtree {
		if b.ID == mol.ID {
			continue
		}
		if !convoycore.IsTerminalStatus(b.Status) {
			return
		}
	}

	if err := closeMoleculeWithReason(store, mol.ID, moleculeAutocloseReason); err != nil {
		return
	}

	rec.Record(events.Event{
		Type:    events.BeadClosed,
		Actor:   eventActor(),
		Subject: mol.ID,
	})

	fmt.Fprintf(stdout, "Auto-closed molecule %s %q\n", mol.ID, mol.Title) //nolint:errcheck // best-effort stdout
}

// closeMoleculeWithReason mirrors closeConvoyWithReason: stamps a
// close_reason metadata value before invoking the store's close so the
// reason is auditable via bd show. Falls back to a plain Close when
// the store has no explicit-reason close path.
func closeMoleculeWithReason(store beads.Store, id, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return store.Close(id)
	}
	if err := store.SetMetadata(id, "close_reason", reason); err != nil {
		return fmt.Errorf("stamping molecule %s close reason: %w", id, err)
	}
	if closer, ok := store.(explicitReasonCloser); ok {
		return closer.CloseWithReason(id, reason)
	}
	return store.Close(id)
}
