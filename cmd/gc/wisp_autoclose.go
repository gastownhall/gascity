package main

import (
	"fmt"
	"io"
	"os"

	"github.com/gastownhall/gascity/internal/beadmeta"
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

	// See doConvoyAutoclose: the bd on_close hook inherits the supervisor's
	// (city) cwd/env, so resolve the store that actually owns the bead across
	// the city and every rig, so rig-store closes autoclose their attached
	// wisps instead of silently no-op'ing (#3411).
	if store, _, ok := autocloseOwningStore(beadID, cityPath); ok {
		doWispAutocloseWith(store, beadID, stdout)
		return
	}

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
// doWispAutocloseWith reads the just-closed bead from store (the store that owns
// it) and resolves/closes its attached molecule/workflow roots through the
// graph-class store. A closed work bead in a rig store can own graph-workflow
// attachments that live in the graph store, so the attachment collection,
// parked-checkpoint guard, subtree close, and spec-sidecar close all run on the
// graph store. The graph store is supplied as an optional trailing argument;
// when omitted it collapses to store, so single-store CLI and test callers
// behave exactly as before the per-class seam.
func doWispAutocloseWith(store beads.Store, beadID string, stdout io.Writer, graphStoreOpt ...beads.Store) {
	graphStore := store
	if len(graphStoreOpt) > 0 && graphStoreOpt[0] != nil {
		graphStore = graphStoreOpt[0]
	}
	parent, err := beads.HandlesFor(store).Live.Get(beadID)
	if err != nil {
		return
	}
	attachments, err := collectAttachedBeads(parent, graphStore, beads.HandlesFor(graphStore).Live)
	seen := make(map[string]bool, len(attachments))
	for _, attached := range attachments {
		seen[attached.ID] = true
	}
	attachments = append(attachments, collectInputConvoyWorkflowRoots(graphStore, parent, seen)...)
	if err == nil || len(attachments) > 0 {
		for _, attached := range attachments {
			if attachedMoleculeIsParked(graphStore, attached) {
				continue
			}
			closed, err := closeAttachedWispSubtree(graphStore, attached)
			if err != nil || closed == 0 {
				continue
			}
			fmt.Fprintf(stdout, "Auto-closed %s %s on %s\n", attachmentLabel(attached), attached.ID, beadID) //nolint:errcheck // best-effort stdout
		}
	}
	if parent.Status != "closed" || !sourceworkflow.IsWorkflowRoot(parent) {
		return
	}
	closed, err := sourceworkflow.CloseSpecSidecarsForRoot(graphStore, parent.ID, "")
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

// collectInputConvoyWorkflowRoots finds open graph.v2 workflow roots that were
// dispatched over a synthetic input convoy tracking the just-closed bead. A
// root-only in-session wisp -- mol-focus-review and peers routed to a pool --
// runs every formula step in one worker session, so it materializes no step
// beads and no workflow-finalize control bead to close its root. sling also
// clears gc.source_bead_id for graph workflows (internal/sling/sling.go), so
// such a wisp carries no source-bead attachment for collectAttachedBeads to
// follow. Its only durable link back to the work issue is gc.input_convoy_id ->
// the tracking convoy whose member is the issue. Without reaping the root here,
// it outlives its closed issue, re-routes to the pool, and churns a fresh worker
// (the gastownhall/gascity review-pool spawn-churn finalize-gap).
//
// This is discovery only: the caller runs each root through the shared
// attachedMoleculeIsParked guard, so an orchestrated workflow whose step beads
// are still in flight stays open and closes later via its workflow-finalize
// control instead.
func collectInputConvoyWorkflowRoots(store beads.Store, parent beads.Bead, seen map[string]bool) []beads.Bead {
	convoys, err := convoycore.TrackingConvoysForItem(store, parent.ID)
	if err != nil {
		return nil
	}
	var roots []beads.Bead
	for _, convoy := range convoys {
		matches, err := store.ListByMetadata(
			map[string]string{beadmeta.InputConvoyIDMetadataKey: convoy.ID},
			0, beads.WithBothTiers,
		)
		if err != nil {
			continue
		}
		for _, root := range matches {
			if seen[root.ID] {
				continue
			}
			if convoycore.IsTerminalStatus(root.Status) || !sourceworkflow.IsWorkflowRoot(root) {
				continue
			}
			// Restrict to graph.v2 roots, matching the documented intent and
			// formulaCookLiveInputConvoyGraphRoots: only graph workflows clear
			// gc.source_bead_id and link back solely through gc.input_convoy_id,
			// so a legacy gc.kind=workflow root is reaped via its source-bead
			// attachment instead and must not be force-closed here.
			if root.Metadata[beadmeta.FormulaContractMetadataKey] != "graph.v2" {
				continue
			}
			seen[root.ID] = true
			roots = append(roots, root)
		}
	}
	return roots
}

func closeAttachedWispSubtree(store beads.Store, attached beads.Bead) (int, error) {
	return sling.CloseAttachedSubtree(store, attached)
}
