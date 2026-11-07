package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/graphroute"
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
	// graphStore may hold a graph or a v1 work molecule: a relabel until the T7 Molecule intent (typed-store T5a).
	graphMolecule := beads.GraphMolecule(beads.GraphStore{Store: graphStore})
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
	inputRoots, discoverErr := collectInputConvoyWorkflowRoots(store, graphStore, graphMolecule, parent, seen)
	run.note(discoverErr)
	inputOf := make(map[string]inputConvoyRoot, len(inputRoots))
	for _, input := range inputRoots {
		inputOf[input.root.ID] = input
		located = append(located, sling.AttachedBead{Bead: input.root, Store: graphStore})
	}
	if err == nil || len(located) > 0 {
		for _, a := range located {
			attached, attachedStore := a.Bead, a.Store
			if attachedStore == nil {
				attachedStore = graphStore
			}
			parked, err := attachedMoleculeIsParked(attachedStore, attached)
			run.note(err)
			// A parked root found through its input convoy has nothing left to
			// run once that whole input is terminal, nothing in it is held,
			// and only idle steps are left (inputConvoyWorkflowIdle): it
			// retires here, all but its teardown tail
			// (retireIdleInputWorkflow), rather than waiting for a seat to
			// claim one of those steps only to find the work done.
			input, retireIdle := inputOf[attached.ID]
			retireIdle = retireIdle && parked && err == nil
			if retireIdle {
				_, idle, err := inputConvoyWorkflowIdle(input)
				run.note(err)
				parked = !idle
			}
			if parked {
				continue
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
			var closed int
			if retireIdle {
				closed, err = retireIdleInputWorkflow(graphMolecule, input)
			} else {
				closed, err = closeAttachedWispSubtree(attachedStore, attached)
			}
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
	closed, err := sourceworkflow.CloseSpecSidecarsForRoot(graphMolecule, parent.ID, "")
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
// This is discovery only: the caller runs each root through the shared
// attachedMoleculeIsParked guard, and a parked root retires only when
// inputConvoyWorkflowIdle holds, so an orchestrated workflow whose step beads
// are still in flight stays open and closes later via its workflow-finalize
// control instead. A failed read is returned with whatever was found: the
// caller reports it so the run is retried. graphMolecule is graphStore as the
// molecule target a root's workflow members are read through.
func collectInputConvoyWorkflowRoots(workStore, graphStore beads.Store, graphMolecule beads.MoleculeTarget, parent beads.Bead, seen map[string]bool) ([]inputConvoyRoot, error) {
	// The tracks edge is co-resident with the tracking convoy, and the two
	// owning candidates differ on a split city: every convoy is WORK class
	// (coordclass), minted co-resident with the issue it tracks, so a
	// post-split launch leaves the convoy + edge in the work store — while a
	// store migrated from the single-store era can hold them on the graph
	// side. TrackingConvoysForItem is single-store by contract, so probe both.
	// On a single-store city graphStore == workStore and this collapses to
	// the pre-split single read.
	work := beads.WorkStore{Store: workStore}
	workConvoys, err := convoycore.TrackingConvoysForItem(work, parent.ID)
	if err != nil {
		return nil, err
	}
	convoys := make([]trackingConvoy, 0, len(workConvoys))
	for _, c := range workConvoys {
		convoys = append(convoys, trackingConvoy{bead: c})
	}
	var graphLeg beads.WorkStore
	if graphStore != workStore {
		// The graph leg of the two-store probe above: a miss here is
		// covered by the work leg.
		// graphStore is a federated leg, not the work class: the convoys a
		// graph store may hold, and their members, are read through it here
		// (typed-store T7).
		graphLeg = beads.WorkStore{Store: graphStore}
		var graphConvoys []beads.Bead
		gerr := beads.FederatedRead(func() error {
			var listErr error
			graphConvoys, listErr = convoycore.TrackingConvoysForItem(graphLeg, parent.ID)
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
			// looks-like-success answer that file exists to close.
			return nil, gerr
		}
		for _, c := range dedupeConvoysByID(workConvoys, graphConvoys)[len(workConvoys):] {
			convoys = append(convoys, trackingConvoy{bead: c, onGraph: true})
		}
	}
	var roots []inputConvoyRoot
	var listErr error
	for _, convoy := range convoys {
		// Roots are graph-class, so the root lookup stays on the graph store
		// regardless of which store owned the tracking convoy.
		matches, err := graphStore.ListByMetadata(
			map[string]string{beadmeta.InputConvoyIDMetadataKey: convoy.bead.ID},
			0, beads.WithBothTiers,
		)
		if err != nil {
			listErr = err
			continue
		}
		// The members are read with the convoy's own store owning its edges
		// and the just-closed bead's store as the member class.
		classes := convoycore.MemberClasses{Convoy: work, Work: []beads.WorkStore{work}}
		if convoy.onGraph {
			classes.Convoy = graphLeg
		}
		convoyID := convoy.bead.ID
		reads := inputWorkflowReads{
			root:    beads.LiveFor(graphStore).Get,
			members: func() ([]beads.Bead, error) { return convoycore.MembersInLive(classes, convoyID, true) },
			workflow: func(rootID string) ([]beads.Bead, error) {
				return sourceworkflow.ListWorkflowBeads(graphMolecule, rootID)
			},
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
			roots = append(roots, inputConvoyRoot{root: root, convoyID: convoy.bead.ID, reads: reads})
		}
	}
	return roots, listErr
}

// trackingConvoy is a convoy tracking the just-closed bead; onGraph reports
// that it and its membership edges are held by the graph store rather than
// the work store.
type trackingConvoy struct {
	bead    beads.Bead
	onGraph bool
}

// inputConvoyRoot is a graph.v2 workflow root found through the input convoy
// that tracks the just-closed bead, with the live reads its idle check needs.
type inputConvoyRoot struct {
	root     beads.Bead
	convoyID string
	reads    inputWorkflowReads
}

// inputWorkflowReads are the reads behind an input-convoy workflow's idle
// check, all of them live, so a claim, a reopen or a tracks edge a cache has
// not absorbed yet is seen: the root by id and the workflow's members, read
// from the graph store, and the convoy's members (a convoy.MembersInLive read
// over the convoy's own store and the member store). The workflow's members
// are what sourceworkflow.ListWorkflowBeads reads: the root and every bead
// naming it in gc.root_bead_id, the very set a retirement closes.
type inputWorkflowReads struct {
	root     func(id string) (beads.Bead, error)
	members  func() ([]beads.Bead, error)
	workflow func(rootID string) ([]beads.Bead, error)
}

// inputConvoyWorkflowIdle reports whether a parked graph.v2 workflow has
// nothing left to run but steps a seat would claim only to find the work done
// and, if so, returns the workflow members it judged. That holds when its root
// still names input.convoyID; that convoy's membership is non-empty, fully
// resolved and terminal; and the workflow's members leave only such idle
// steps (inputWorkflowIdleWork). Anything else keeps the workflow: a claim,
// the dispatcher and its finalizer, or a later close carries it on.
func inputConvoyWorkflowIdle(input inputConvoyRoot) ([]beads.Bead, bool, error) {
	root, err := input.reads.root(input.root.ID)
	if err != nil {
		return nil, false, err
	}
	if convoycore.IsTerminalStatus(root.Status) || strings.TrimSpace(root.Metadata[beadmeta.InputConvoyIDMetadataKey]) != input.convoyID {
		return nil, false, nil
	}
	members, err := input.reads.members()
	if err != nil {
		return nil, false, err
	}
	if len(members) == 0 {
		return nil, false, nil
	}
	for _, m := range members {
		if convoycore.IsUnresolvedTrackedItem(m) || !convoycore.IsTerminalStatus(m.Status) {
			return nil, false, nil
		}
	}
	workflow, err := input.reads.workflow(root.ID)
	if err != nil {
		return nil, false, err
	}
	if !inputWorkflowIdleWork(root, workflow) {
		return nil, false, nil
	}
	return workflow, true, nil
}

// inputWorkflowIdleWork reports whether workflow, the members of the
// input-convoy workflow rooted at root, leaves only idle work: no non-terminal
// member is held (inputWorkflowMemberHeld), and an open step a seat would
// claim is left (inputWorkflowWorkerStep), outside the teardown tail, which
// runs after the root settles by contract. A workflow whose open members are
// only its controls, scope latches and teardown tail is the dispatcher's to
// drive, and its finalizer settles the root. Spec sidecars are skipped, as
// the parked guard skips them. root is the live read of the root and stands
// in for its row in workflow.
func inputWorkflowIdleWork(root beads.Bead, workflow []beads.Bead) bool {
	teardown := molecule.TeardownTailExclusionFromMembers(workflow)
	claimable := false
	for _, b := range workflow {
		isRoot := b.ID == root.ID
		if isRoot {
			b = root
		}
		if sourceworkflow.IsGeneratedSpecSidecar(b) || convoycore.IsTerminalStatus(b.Status) {
			continue
		}
		if inputWorkflowMemberHeld(b, isRoot) {
			return false
		}
		if !isRoot && inputWorkflowWorkerStep(b) && !teardown(b) {
			claimable = true
		}
	}
	return claimable
}

// inputWorkflowMemberHeld reports whether a non-terminal member keeps its
// input-convoy workflow from retiring: an assignee, or a declared gate
// (gc.gate_type; Type=gate alone also marks deferred routing), holds any
// member; a step is held once it is past open; and so is a drain whose items
// are still running (inputWorkflowDrainInFlight). The root's status says
// nothing of a claim: gc sling promotes every graph.v2 root to in_progress
// when it launches the workflow (sling.PromoteWorkflowLaunchBead).
func inputWorkflowMemberHeld(b beads.Bead, isRoot bool) bool {
	if strings.TrimSpace(b.Assignee) != "" || strings.TrimSpace(b.Metadata[beadmeta.GateTypeMetadataKey]) != "" ||
		inputWorkflowDrainInFlight(b) {
		return true
	}
	return !isRoot && b.Status != "open"
}

// inputWorkflowDrainInFlight reports whether b is a drain control from the
// start of its expansion until it records its items' outcomes (gc.drain_state
// expanding, expanded or completing). Each item workflow it mints runs under a
// root of its own, outside the members the idle check reads, and its last
// step closes the drained input member, so that input reads terminal while
// the items still run.
func inputWorkflowDrainInFlight(b beads.Bead) bool {
	if strings.TrimSpace(b.Metadata[beadmeta.KindMetadataKey]) != beadmeta.KindDrain {
		return false
	}
	switch strings.TrimSpace(b.Metadata[beadmeta.DrainStateMetadataKey]) {
	case beadmeta.DrainStateExpanding, beadmeta.DrainStateExpanded, beadmeta.DrainStateCompleting:
		return true
	default:
		return false
	}
}

// inputWorkflowWorkerStep reports whether b is a step a seat claims: neither a
// control the dispatcher processes, open and unassigned, nor a bead that
// structures the workflow (a root, a scope latch, a spec).
func inputWorkflowWorkerStep(b beads.Bead) bool {
	kind := strings.TrimSpace(b.Metadata[beadmeta.KindMetadataKey])
	return !graphroute.IsControlDispatcherKind(kind) && !graphroute.IsWorkflowTopologyKind(kind)
}

// retireIdleInputWorkflow reads an input-convoy workflow's idle decision again
// right before its close, so a claim, a gate, a reopened step or an input
// reopened or tracked since is seen, and while the workflow is still idle
// closes its root and every open member in graph as skipped. Its teardown
// tail stays open (molecule.TeardownTailExclusionFromMembers over the members
// the decision read): teardown runs after the root settles by contract, and
// the claim's stop gate serves it then. It returns how many beads it closed.
func retireIdleInputWorkflow(graph beads.MoleculeTarget, input inputConvoyRoot) (int, error) {
	workflow, idle, err := inputConvoyWorkflowIdle(input)
	if err != nil || !idle {
		return 0, err
	}
	return sourceworkflow.CloseWorkflowSubtreeAsExcept(graph, input.root.ID, beadmeta.OutcomeSkipped,
		sourceworkflow.WorkflowSubtreeClosedReason, nil, molecule.TeardownTailExclusionFromMembers(workflow))
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
