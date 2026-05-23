package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/events"
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

// doMoleculeAutocloseWith walks from the just-closed bead up to its
// parent molecule (if any) and closes the molecule when every child is
// terminal. Reacts to closes of typed "step" beads (the
// nonRootStepBeadType-stamped formula scaffolding) — closes of other
// child types are ignored to avoid surprising auto-close on user data.
// All errors are silently swallowed; this is called from a bd hook
// script and must not fail loudly. See gastownhall/gascity#1039.
func doMoleculeAutocloseWith(store beads.Store, rec events.Recorder, beadID string, stdout io.Writer) {
	bead, err := store.Get(beadID)
	if err != nil {
		return
	}
	// React only to formula-scaffolding step closes. Closes of "task"
	// or other types attached to a molecule shouldn't trigger an
	// auto-close (those represent real work; the user may legitimately
	// close one without intending to close the parent).
	if bead.Type != "step" {
		return
	}
	if bead.ParentID == "" {
		return
	}
	parent, err := store.Get(bead.ParentID)
	if err != nil {
		return
	}
	autocloseMoleculeIfComplete(store, rec, parent, stdout)
}

func autocloseMoleculeIfComplete(store beads.Store, rec events.Recorder, mol beads.Bead, stdout io.Writer) {
	if mol.Type != "molecule" {
		return
	}
	if convoycore.IsTerminalStatus(mol.Status) {
		return
	}

	children, err := store.Children(mol.ID, beads.IncludeClosed)
	if err != nil {
		return
	}
	if len(children) == 0 {
		// A molecule root with no step children is either still being
		// instantiated or already-cleaned scaffolding; either way,
		// closing here would race the instantiator. Leave it.
		return
	}
	for _, ch := range children {
		if !convoycore.IsTerminalStatus(ch.Status) {
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
