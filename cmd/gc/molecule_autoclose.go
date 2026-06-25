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
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// moleculeAutocloseReason is the close_reason metadata value stamped on
// molecule roots auto-closed because all of their step children are
// terminal. Mirrors convoyAutocloseReason for the convoy path.
const moleculeAutocloseReason = "molecule autoclose: all step children closed"

// moleculeSourceAutocloseReason is the close_reason stamped on graph-workflow
// roots auto-closed because the work bead they were slung against
// (gc.source_bead_id) was closed directly by the worker. Distinct from
// moleculeAutocloseReason so an operator reading bd show can tell the
// source-bead-trigger close apart from the all-steps-terminal close.
const moleculeSourceAutocloseReason = "molecule autoclose: source work bead closed"

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
	rec := openCityRecorderAt(cityPath, stderr)

	// See doConvoyAutoclose: the bd on_close hook inherits the supervisor's
	// (city) cwd/env, so resolve the store that actually owns the bead across
	// the city and every rig, and derive the store-ref from that store, so
	// rig-store closes autoclose their molecule roots instead of silently
	// no-op'ing (#3411).
	if store, dir, ok := autocloseOwningStore(beadID, cityPath); ok {
		doMoleculeAutocloseWith(store, autocloseStoreRef(dir, cityPath), rec, beadID, stdout)
		return
	}

	store, err := openStoreAtForCity(storeRoot, cityPath)
	if err != nil {
		return
	}
	doMoleculeAutocloseWith(store, autocloseStoreRef(storeRoot, cityPath), rec, beadID, stdout)
}

// autocloseStoreRef resolves the store-ref label ("city:<name>" / "rig:<name>")
// for the store rooted at storeRoot. The source-bead reverse lookup uses it to
// scope to roots whose source actually lives in this store: in multi-store
// deployments bead IDs can collide across stores, so without the ref a close in
// one store could auto-close a root sourced from a same-ID bead in another
// store. Best-effort — returns "" when the city config cannot be loaded, which
// makes the lookup match on bead ID alone (the prior single-store behavior).
func autocloseStoreRef(storeRoot, cityPath string) string {
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		return ""
	}
	return workflowStoreRefForDir(storeRoot, cityPath, loadedCityName(cfg, cityPath), cfg)
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
func doMoleculeAutocloseWith(store beads.Store, storeRef string, rec events.Recorder, beadID string, stdout io.Writer) {
	bead, err := store.Get(beadID)
	if err != nil {
		return
	}

	// Source-bead trigger: when a graph workflow's work bead
	// (gc.source_bead_id) is closed directly by the worker — via either
	// `gc bd close` or a bare `bd update --status=closed`, both of which
	// fire this on_close hook — the workflow root is not itself a step
	// under that bead, so the step/metadata resolution below never reaches
	// it. A stepless wisp (graph.v2 root with no expanded steps) then
	// orphans and is re-routed to a fresh worker indefinitely. Reverse-
	// resolve any live workflow roots whose source bead is this bead and
	// close them once their own subtree is terminal.
	autocloseRootsForSourceBead(store, storeRef, rec, beadID, stdout)

	rootID := strings.TrimSpace(bead.Metadata[beadmeta.RootBeadIDMetadataKey])
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
	terminal, descendants := subtreeTerminalExcludingRoot(store, mol.ID)
	if !terminal {
		return
	}
	if descendants == 0 {
		// Only the root itself was returned — no descendants. The
		// molecule is either still being instantiated or already-cleaned
		// scaffolding; either way, closing here would race the
		// instantiator. Leave it. The source-bead trigger
		// (autocloseRootsForSourceBead) deliberately omits this guard: a
		// closed source/work bead is a definitive completion signal and
		// instantiation always precedes worker execution, so a stepless
		// wisp seen there is genuinely complete.
		return
	}
	announceClosedMolecule(store, rec, mol, moleculeAutocloseReason, stdout)
}

// autocloseRootsForSourceBead closes any live graph-workflow roots whose
// source/work bead (gc.source_bead_id) just closed, once each root's own
// subtree is fully terminal. Unlike autocloseMoleculeIfComplete it does not
// require the root to be issue_type "molecule" (graph.v2 wisps are
// issue_type "task") nor that it have step descendants. Best-effort: store
// errors are swallowed so a misbehaving hook never breaks the bd close path.
//
// storeRef is the store-ref of the closing bead's store; it scopes the lookup
// to roots whose source actually lives in this store (both the source-store and
// root-store arguments of ListLiveRoots). In multi-store deployments bead IDs
// can collide across stores, so a root in this store sourced from a same-ID
// bead elsewhere (a different gc.source_store_ref) must not be closed here. An
// empty storeRef falls back to matching on bead ID alone (single-store path).
func autocloseRootsForSourceBead(store beads.Store, storeRef string, rec events.Recorder, sourceBeadID string, stdout io.Writer) {
	roots, err := sourceworkflow.ListLiveRoots(store, sourceBeadID, storeRef, storeRef)
	if err != nil {
		return
	}
	for _, root := range roots {
		if terminal, _ := subtreeTerminalExcludingRoot(store, root.ID); terminal {
			if announceClosedMolecule(store, rec, root, moleculeSourceAutocloseReason, stdout) {
				_, _ = sourceworkflow.CloseSpecSidecarsForRoot(store, root.ID, sourceworkflow.WorkflowSpecSidecarClosedReason)
			}
		}
	}
}

// subtreeTerminalExcludingRoot reports whether every transitive descendant of
// rootID (the root itself excluded) is terminal, and how many descendants were
// found. It walks the full subtree — parent-child edges plus the
// gc.root_bead_id metadata link — so roots whose steps fan out into nested
// children (formula-compiler "epic" steps, gate-deferred sub-trees) are
// evaluated by descendant terminality rather than direct children. A walk
// error yields (false, 0) so the caller leaves the root open.
func subtreeTerminalExcludingRoot(store beads.Store, rootID string) (terminal bool, descendants int) {
	subtree, err := molecule.ListSubtree(store, rootID)
	if err != nil {
		return false, 0
	}
	for _, b := range subtree {
		if b.ID == rootID {
			continue
		}
		if sourceworkflow.IsGeneratedSpecSidecar(b) {
			continue
		}
		descendants++
		if !convoycore.IsTerminalStatus(b.Status) {
			return false, descendants
		}
	}
	return true, descendants
}

// announceClosedMolecule closes mol with the given close_reason, records a
// BeadClosed event, and prints the auto-close announcement to stdout. Shared
// by the step-terminal and source-bead-close triggers. Best-effort: a close
// failure aborts silently without recording or announcing.
func announceClosedMolecule(store beads.Store, rec events.Recorder, mol beads.Bead, reason string, stdout io.Writer) bool {
	if err := closeMoleculeWithReason(store, mol.ID, reason); err != nil {
		return false
	}

	rec.Record(events.Event{
		Type:    events.BeadClosed,
		Actor:   eventActor(),
		Subject: mol.ID,
	})

	fmt.Fprintf(stdout, "Auto-closed molecule %s %q\n", mol.ID, mol.Title) //nolint:errcheck // best-effort stdout
	return true
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
