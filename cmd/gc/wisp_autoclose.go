package main

import (
	"fmt"
	"io"
	"os"

	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/sling"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
	"github.com/spf13/cobra"
)

func newWispCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "wisp",
		Short:  "Wisp lifecycle operations",
		Hidden: true,
	}
	cmd.AddCommand(newWispAutocloseCmd(stdout, stderr))
	return cmd
}

func newWispAutocloseCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:    "autoclose <bead-id>",
		Short:  "Auto-close open molecule descendants of a closed bead",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			doWispAutoclose(args[0], stdout, stderr)
			return nil // always succeed — best-effort infrastructure
		},
	}
}

// doWispAutoclose is the CLI entry point for wisp autoclose.
// It resolves the current store through the provider-aware resolver using the
// projected store-root environment and delegates to the testable core.
func doWispAutoclose(beadID string, stdout, _ io.Writer) {
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
	doWispAutocloseWith(store, beadID, stdout)
}

// doWispAutocloseWith closes any open attached molecule/workflow roots and
// their descendants for the given bead. Metadata-based attachments are
// preferred, with child traversal as a fallback for legacy data. Called from
// the bd on_close hook to ensure attached wisps don't outlive their parent work
// bead. All errors are silently swallowed — this is best-effort infrastructure.
// Parent lookup and child traversal both read through the Live handle: the
// hook fires for closed and ephemeral-tier beads that cached or tier-narrow
// raw reads can miss, and an attachment missed here outlives its parent — the
// leak class this hook exists to drain.
func doWispAutocloseWith(store beads.Store, beadID string, stdout io.Writer) {
	parent, err := beads.HandlesFor(store).Live.Get(beadID)
	if err != nil {
		return
	}
	attachments, err := collectAttachedBeads(parent, store, beads.HandlesFor(store).Live)
	if err == nil || len(attachments) > 0 {
		for _, attached := range attachments {
			if attachedMoleculeIsParked(store, attached) {
				continue
			}
			closed, err := closeAttachedWispSubtree(store, attached)
			if err != nil || closed == 0 {
				continue
			}
			fmt.Fprintf(stdout, "Auto-closed %s %s on %s\n", attachmentLabel(attached), attached.ID, beadID) //nolint:errcheck // best-effort stdout
		}
	}
	if parent.Status != "closed" || !sourceworkflow.IsWorkflowRoot(parent) {
		return
	}
	closed, err := sourceworkflow.CloseSpecSidecarsForRoot(store, parent.ID, "")
	if err != nil || closed == 0 {
		return
	}
	fmt.Fprintf(stdout, "Auto-closed %d generated spec bead(s) on %s\n", closed, beadID) //nolint:errcheck // best-effort stdout
}

// attachedMoleculeIsParked reports whether the attached root is a live molecule
// deliberately parked at an open descendant — e.g. a human-gate step plus the
// finalize step it blocks. Such a subtree is designed to outlive the owner
// dispatch/loop bead whose close triggered this hook, so force-closing it here
// would steamroll the human checkpoint before the maintainer acts (the #3474
// finalize defect).
//
// The predicate mirrors the terminality guard the sibling `gc molecule
// autoclose` path applies (autocloseMoleculeIfComplete leaves a non-terminal
// subtree open), so the two close-time auto-closers agree. We additionally
// require the attached root itself to still be open: an already-terminal root
// with leftover open steps is an orphan, not a parked checkpoint, and still
// reaps — preserving the descendant-cleanup intent this hook was built for.
//
// subtreeTerminalExcludingRoot returns (true, 0) for a stepless/ephemeral wisp
// (terminal, so not parked -> still reaped) and (false, 0) on a subtree-walk
// error. We deliberately treat that walk error as parked, matching the
// sibling's fail-safe `if !terminal { return }`: force-closing a possibly
// human-pending subtree because a transient store read failed is the very
// destructive behavior #3474 removes, and a genuinely complete wisp left open
// on a read error is still reaped by the redundant later close paths.
func attachedMoleculeIsParked(store beads.Store, attached beads.Bead) bool {
	if convoycore.IsTerminalStatus(attached.Status) {
		return false
	}
	terminal, _ := subtreeTerminalExcludingRoot(store, attached.ID)
	return !terminal
}

func closeAttachedWispSubtree(store beads.Store, attached beads.Bead) (int, error) {
	return sling.CloseAttachedSubtree(store, attached)
}
