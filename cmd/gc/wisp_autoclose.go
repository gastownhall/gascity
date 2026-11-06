package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/molecule"
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
	// Attachments are ClassGraph wherever the just-closed bead lives; without
	// this the hook reads an empty graph on a migrated city and reports success.
	routeCfg, _ := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if store, _, ok := autocloseOwningStore(beadID, cityPath); ok {
		doWispAutocloseWith(store, beadID, stdout, cliGraphStore(store, routeCfg, cityPath))
		return
	}

	store, err := openStoreAtForCity(storeRoot, cityPath)
	if err != nil {
		return
	}
	doWispAutocloseWith(store, beadID, stdout, cliGraphStore(store, routeCfg, cityPath))
}

// doWispAutocloseWith closes any open attached molecule/workflow roots and
// their descendants for the given bead, once it is closed. Metadata-based
// attachments are preferred, with child traversal as a fallback for legacy
// data. Called from
// the bd on_close hook to ensure attached wisps don't outlive their parent work
// bead. All errors are swallowed — this is best-effort infrastructure — but a
// failed read is reported in the returned run so the controller can retry.
// Parent lookup and child traversal both read through the Live handle: the
// hook fires for closed and ephemeral-tier beads that cached or tier-narrow
// raw reads can miss, and an attachment missed here outlives its parent — the
// leak class this hook exists to drain.
// doWispAutocloseWith reads the just-closed bead from store (the store that owns
// it) and resolves/closes its attached molecule/workflow roots through the
// graph-class store. A closed work bead in a rig store can own graph-workflow
// attachments that live in the graph store, so the attachment collection,
// parked-checkpoint guard, subtree close, and spec-sidecar close all run on the
// graph store. It is required rather than variadic: as a variadic with a
// collapse-to-store default, both hook-path callers silently omitted it.
func doWispAutocloseWith(store beads.Store, beadID string, stdout io.Writer, graphClassStore beads.GraphStore) autocloseRun {
	var run autocloseRun
	// Unwrapped for internal use: the helpers below assert optional store
	// capabilities, which do not promote through the class wrapper.
	graphStore := graphClassStore.Store
	if graphStore == nil {
		graphStore = store
	}
	parent, err := beads.HandlesFor(store).Live.Get(beadID)
	// An open parent's attachments are its live work, finished or not: a
	// spurious bead.closed (a false scan close, a replay) must not close them.
	if err != nil || parent.Status != "closed" {
		run.note(err)
		return run
	}
	// An attachment lives where it was poured: a graph-class root (a wisp, a
	// graph.v2 workflow) in the graph store, a v1 POURED molecule (work class,
	// `gc formula cook --attach`) on the work ledger beside the bead it is
	// attached to. Each one is read, checked and closed in the store that holds
	// it. A DB-child attachment shares its parent's ledger, so children are
	// listed through the parent's own store.
	located, err := sling.CollectAttachments(parent, beads.HandlesFor(store).Live, graphStore, store)
	run.note(err)
	seen := make(map[string]bool, len(located))
	for _, a := range located {
		seen[a.Bead.ID] = true
	}
	convoyRoots, err := collectInputConvoyWorkflowRoots(store, graphStore, parent, seen)
	run.note(err)
	// The idle-retirement arm below keys on the convoy each root was
	// discovered over: which convoy, and which store owns its membership
	// edges.
	convoyLinks := make(map[string]inputConvoyWorkflowRoot, len(convoyRoots))
	for _, root := range convoyRoots {
		convoyLinks[root.Root.ID] = root
		located = append(located, sling.AttachedBead{Bead: root.Root, Store: graphStore})
	}
	if err == nil || len(located) > 0 {
		for _, a := range located {
			attached, attachedStore := a.Bead, a.Store
			if attachedStore == nil {
				attachedStore = graphStore
			}
			parked, err := attachedMoleculeIsParked(attachedStore, attached)
			run.note(err)
			if parked {
				// The idle-retirement arm of the input-convoy reap (#8729). A
				// parked subtree is preserved for its workflow-finalize
				// control — unless the workflow was dispatched over a
				// synthetic input convoy whose membership is now fully
				// terminal while every non-terminal member sits idle and
				// ungated. Such a workflow has nothing left to run: each
				// open step would dispose against input that no longer
				// exists, one burned seat poll each (the claim-time done arm
				// eventually retires it; this closes it at close time, no
				// seat poll). Source attachments keep the parked behavior,
				// and a subtree the park walk could not read is not one this
				// arm may license a close over.
				link, overConvoy := convoyLinks[attached.ID]
				if !overConvoy || err != nil {
					continue
				}
				convoyStore := graphStore
				if link.ConvoyInWork {
					convoyStore = store
				}
				members, err := convoycore.MembersIn(convoycore.MemberClasses{
					Convoy: convoyStore,
					Work:   []beads.Store{store},
					Graph:  graphStore,
				}, link.ConvoyID, true)
				run.note(err)
				if err != nil || !inputMembersAllTerminal(members) {
					continue
				}
				subtree, err := molecule.ListSubtree(attachedStore, attached.ID)
				run.note(err)
				if err != nil || !workflowSubtreeIdleAndUngated(subtree, attached.ID, link.ConvoyID) {
					continue
				}
				// Licensed: fall through to the shared rechecks and close,
				// which stamps skipped and the subtree close reason the
				// controller recognizes.
			}
			// A subtree close has no conditional form, so re-check both rows
			// it rests on live: the parent still closed, and a root decided
			// terminal not reopened since (that would need the parked check
			// again). Nothing finer is compared: the decided row may come
			// through a cache that lags, and a refusal here leaks the
			// attachment. A failed re-check read is a refusal, reported so
			// the run is retried.
			parentStill, err := autocloseStill(store, parent.ID, func(b beads.Bead) bool { return b.Status == "closed" })
			run.note(err)
			if !parentStill {
				continue
			}
			rootStill, err := autocloseStill(attachedStore, attached.ID, func(b beads.Bead) bool {
				return !convoycore.IsTerminalStatus(attached.Status) || convoycore.IsTerminalStatus(b.Status)
			})
			run.note(err)
			if !rootStill {
				continue
			}
			closed, err := closeAttachedWispSubtree(attachedStore, attached)
			run.note(err)
			if err != nil || closed == 0 {
				continue
			}
			fmt.Fprintf(stdout, "Auto-closed %s %s on %s\n", attachmentLabel(attached), attached.ID, beadID) //nolint:errcheck // best-effort stdout
		}
	}
	if !sourceworkflow.IsWorkflowRoot(parent) {
		return run
	}
	closed, err := sourceworkflow.CloseSpecSidecarsForRoot(graphStore, parent.ID, "")
	if err != nil || closed == 0 {
		return run
	}
	fmt.Fprintf(stdout, "Auto-closed %d generated spec bead(s) on %s\n", closed, beadID) //nolint:errcheck // best-effort stdout
	return run
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
// on a read error is still reaped: the walk error is returned too, and the
// controller's autoclose run reports it so the sweep retries.
func attachedMoleculeIsParked(store beads.Store, attached beads.Bead) (bool, error) {
	if convoycore.IsTerminalStatus(attached.Status) {
		return false, nil
	}
	terminal, _, err := subtreeTerminalOrErr(store, attached.ID)
	return !terminal, err
}

// inputConvoyWorkflowRoot is one open graph.v2 workflow root the close hook
// discovered over a synthetic input convoy, with the link that found it: the
// convoy's id and which of the two probed stores owns the convoy row and its
// tracks edges (the work store, or the graph store of a migrated residence).
// The owning store is carried as a flag rather than a handle so the discovery
// result stays data: the caller holds the two stores and picks by the flag.
type inputConvoyWorkflowRoot struct {
	Root beads.Bead
	// ConvoyID is the input convoy the root was discovered over — the value
	// of its gc.input_convoy_id, which the retirement arm rechecks live.
	ConvoyID string
	// ConvoyInWork reports the convoy row lives in the work store rather
	// than the graph store, so the membership read names the right owner.
	ConvoyInWork bool
}

// inputMembersAllTerminal reports whether a convoy's live membership set
// licenses retirement: non-empty (an input that resolves to nothing is the
// claim-time purge probe's question, not the close hook's), fully resolved
// (an unresolved placeholder over a dangling edge means an owner the read did
// not span may hold the member), and entirely terminal.
func inputMembersAllTerminal(members []beads.Bead) bool {
	if len(members) == 0 {
		return false
	}
	for _, m := range members {
		if convoycore.IsUnresolvedTrackedItem(m) {
			return false
		}
		if !convoycore.IsTerminalStatus(m.Status) {
			return false
		}
	}
	return true
}

// workflowSubtreeIdleAndUngated reports whether a freshly listed workflow
// subtree is idle and ungated: the root row still names the input convoy it
// was discovered over, and every non-terminal member — generated spec
// sidecars ignored, as the parked predicate ignores them — is unassigned,
// open, and carries no declared gate. gc.gate_type is the gate signal: a
// Type=gate bead without it is deferred routing (internal/molecule), not a
// checkpoint. A held member (an assignee, or any status but open) is work in
// flight whose input may be its own result, and a gated member is a
// checkpoint that outlives its input; either keeps the workflow.
func workflowSubtreeIdleAndUngated(subtree []beads.Bead, rootID, convoyID string) bool {
	for _, b := range subtree {
		if sourceworkflow.IsGeneratedSpecSidecar(b) {
			continue
		}
		if b.ID == rootID && strings.TrimSpace(b.Metadata[beadmeta.InputConvoyIDMetadataKey]) != convoyID {
			return false
		}
		if convoycore.IsTerminalStatus(b.Status) {
			continue
		}
		if strings.TrimSpace(b.Assignee) != "" {
			return false
		}
		if b.Status != "open" {
			return false
		}
		if strings.TrimSpace(b.Metadata[beadmeta.GateTypeMetadataKey]) != "" {
			return false
		}
	}
	return true
}

// collectInputConvoyWorkflowRoots finds open graph.v2 workflow roots that were
// dispatched over a synthetic input convoy tracking the just-closed bead. A
// root-only in-session wisp -- mol-focus-review and peers routed to a pool --
// runs every formula step in one worker session, so it materializes no step
// beads and no workflow-finalize control bead to close its root. sling also
// clears gc.source_bead_id for graph workflows (internal/sling/sling.go), so
// such a wisp carries no source-bead attachment for sling.CollectAttachments to
// follow. Its only durable link back to the work issue is gc.input_convoy_id ->
// the tracking convoy whose member is the issue. Without reaping the root here,
// it outlives its closed issue, re-routes to the pool, and churns a fresh worker
// (the gastownhall/gascity review-pool spawn-churn finalize-gap).
//
// Each root carries its convoy link so the caller's idle-retirement arm reads
// membership through the store that owns the edges. Discovery errors are
// returned — a probe that cannot see the convoys or the roots certifies
// nothing, and the run reports the failure so the sweep retries.
//
// This is discovery only: the caller runs each root through the shared
// attachedMoleculeIsParked guard, so a workflow whose step beads are still in
// flight stays open and closes later via its workflow-finalize control —
// unless its input is fully terminal and its subtree idle and ungated, which
// the caller's arm retires here instead.
func collectInputConvoyWorkflowRoots(workStore, graphStore beads.Store, parent beads.Bead, seen map[string]bool) ([]inputConvoyWorkflowRoot, error) {
	// The tracks edge is co-resident with the tracking convoy, and the two
	// owning candidates differ on a split city: every convoy is WORK class
	// (coordclass), minted co-resident with the issue it tracks, so a
	// post-split launch leaves the convoy + edge in the work store — while a
	// store migrated from the single-store era can hold them on the graph
	// side. TrackingConvoysForItem is single-store by contract, so probe both.
	// On a single-store city graphStore == workStore and this collapses to
	// the pre-split single read.
	convoys, err := convoycore.TrackingConvoysForItem(workStore, parent.ID)
	if err != nil {
		return nil, fmt.Errorf("listing tracking convoys over %s in the work store: %w", parent.ID, err)
	}
	// inWork maps a convoy id to the store that owns it: true for the work
	// leg above, false (zero value) for the graph leg below.
	inWork := make(map[string]bool, len(convoys))
	for _, c := range convoys {
		inWork[c.ID] = true
	}
	if graphStore != workStore {
		// The graph leg of the two-store probe above: a miss here is
		// covered by the work leg.
		var graphConvoys []beads.Bead
		gerr := beads.FederatedRead(func() error {
			var listErr error
			graphConvoys, listErr = convoycore.TrackingConvoysForItem(graphStore, parent.ID)
			return listErr
		})
		if gerr != nil {
			// Fail closed, matching the workStore posture above: on a partial
			// view we decline to force-close anything. Leaving the root for a
			// later close is the recoverable direction — the redundant close
			// paths reap it once the view is whole again — while silently
			// narrowing this read to the work leg would force-close a root we
			// could not fully see. This is also the refused-binding arm: a city
			// whose split this build must not serve delivers the refusal AS the
			// graph store (cli_storage_routes.go), so every probe errors here.
			// The refusal itself was printed once when the verdict was taken;
			// narrowing to the work leg would be exactly the
			// looks-like-success answer that file exists to close. The error
			// is returned now too, so the run reports the failure and the
			// sweep retries.
			return nil, fmt.Errorf("listing tracking convoys over %s in the graph store: %w", parent.ID, gerr)
		}
		convoys = dedupeConvoysByID(convoys, graphConvoys)
	}
	var roots []inputConvoyWorkflowRoot
	for _, convoy := range convoys {
		// Roots are graph-class, so the root lookup stays on the graph store
		// regardless of which store owned the tracking convoy.
		matches, err := graphStore.ListByMetadata(
			map[string]string{beadmeta.InputConvoyIDMetadataKey: convoy.ID},
			0, beads.WithBothTiers,
		)
		if err != nil {
			return roots, fmt.Errorf("listing workflow roots over input convoy %s: %w", convoy.ID, err)
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
			roots = append(roots, inputConvoyWorkflowRoot{Root: root, ConvoyID: convoy.ID, ConvoyInWork: inWork[convoy.ID]})
		}
	}
	return roots, nil
}

// dedupeConvoysByID merges tracking-convoy lists from the work and graph
// stores, keeping the first occurrence of each convoy id. A convoy lives in
// exactly one store, so overlap only happens when the two probes read the same
// handle — the dedupe keeps the union honest there too.
func dedupeConvoysByID(lists ...[]beads.Bead) []beads.Bead {
	seen := make(map[string]bool)
	var out []beads.Bead
	for _, list := range lists {
		for _, c := range list {
			if seen[c.ID] {
				continue
			}
			seen[c.ID] = true
			out = append(out, c)
		}
	}
	return out
}

func closeAttachedWispSubtree(store beads.Store, attached beads.Bead) (int, error) {
	return sling.CloseAttachedSubtree(store, attached)
}
