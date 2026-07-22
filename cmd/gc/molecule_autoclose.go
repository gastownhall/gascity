package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/api"
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
// doMoleculeAutocloseWith reads the just-closed bead from store (the store that
// owns it) and resolves/closes its molecule or graph-workflow root through the
// graph-class store. A closed bead can be a work/source bead in a rig store
// while the molecule root it belongs to lives in the graph store, so the
// source-bead reverse scan and the root walk run on the graph store. The graph
// store is supplied as an optional trailing argument; when omitted it collapses
// to store, so single-store CLI and test callers behave exactly as before the
// per-class seam.
func doMoleculeAutocloseWith(store beads.Store, storeRef string, rec events.Recorder, beadID string, stdout io.Writer, graphStoreOpt ...beads.Store) moleculeAutocloseCompletion {
	graphStore := store
	rootStoreRef := storeRef
	if len(graphStoreOpt) > 0 && graphStoreOpt[0] != nil {
		graphStore = graphStoreOpt[0]
		if !sameMoleculeLifecycleStore(store, graphStore) {
			// The compatibility wrapper has no authoritative physical ref for an
			// independently supplied graph store. Cross-store source autoclose must
			// use doMoleculeAutocloseWithStoreRefs and provide both refs explicitly.
			rootStoreRef = ""
		}
	}
	return doMoleculeAutocloseWithStoreRefs(store, storeRef, graphStore, rootStoreRef, rec, beadID, stdout)
}

// doMoleculeAutocloseWithStoreRefs carries the physical refs for both the
// closing source store and graph/root store. Cross-store source matching is
// disabled unless both refs resolve, because bead IDs can collide across
// independently owned stores.
func doMoleculeAutocloseWithStoreRefs(
	sourceStore beads.Store,
	sourceStoreRef string,
	rootStore beads.Store,
	rootStoreRef string,
	rec events.Recorder,
	beadID string,
	stdout io.Writer,
) moleculeAutocloseCompletion {
	bead, err := sourceStore.Get(beadID)
	if err != nil {
		return moleculeAutocloseCompletion{}
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
	completion := autocloseRootsForSourceBead(
		rootStore, sourceStore, sourceStoreRef, rootStoreRef, rec, beadID, stdout,
	)

	rootID := strings.TrimSpace(bead.Metadata[beadmeta.RootBeadIDMetadataKey])
	if rootID == "" {
		// Legacy fallback for pre-metadata beads: react only to typed
		// "step" closes with a direct molecule parent. Mirrors prior
		// behavior so molecules created before the metadata convention
		// still auto-close, and so a user closing a "task" bead
		// parented under a molecule does not trigger surprise close.
		if bead.Type != "step" || bead.ParentID == "" {
			return completion
		}
		parent, err := rootStore.Get(bead.ParentID)
		if err != nil {
			return completion
		}
		completion.add(autocloseMoleculeIfComplete(rootStore, rec, parent, stdout))
		return completion
	}
	root, err := rootStore.Get(rootID)
	if err != nil {
		return completion
	}
	completion.add(autocloseMoleculeIfComplete(rootStore, rec, root, stdout))
	return completion
}

func sameMoleculeLifecycleStore(a, b beads.Store) bool {
	if a == nil || b == nil {
		return false
	}
	aType, bType := reflect.TypeOf(a), reflect.TypeOf(b)
	return aType == bType && aType.Comparable() && a == b
}

func sameMoleculeLifecycleTransaction(a, b beads.LifecycleReadTransaction) bool {
	if a == nil || b == nil {
		return false
	}
	aType, bType := reflect.TypeOf(a), reflect.TypeOf(b)
	return aType == bType && aType.Comparable() && a == b
}

type moleculeAutocloseCompletion struct {
	lifecycleDone []<-chan struct{}
	retryNeeded   bool
	retryChecks   []func() bool
}

func (c *moleculeAutocloseCompletion) add(announcement moleculeCloseAnnouncement) {
	if announcement.lifecycleDone != nil {
		c.lifecycleDone = append(c.lifecycleDone, announcement.lifecycleDone)
	}
	c.addRetry(announcement)
}

func (c *moleculeAutocloseCompletion) addRetry(announcement moleculeCloseAnnouncement) {
	c.retryNeeded = c.retryNeeded || announcement.lifecycleRetryNeeded
	if announcement.lifecycleRetry != nil {
		c.retryChecks = append(c.retryChecks, announcement.lifecycleRetry)
	}
}

func (c *moleculeAutocloseCompletion) addLifecycle(completion moleculeLifecycleCompletion) {
	c.lifecycleDone = append(c.lifecycleDone, completion.Done())
	c.retryChecks = append(c.retryChecks, completion.Wait)
}

func (c *moleculeAutocloseCompletion) addFollowup(announcement moleculeCloseAnnouncement, fn func() moleculeLifecycleCompletion) {
	c.addRetry(announcement)
	if fn == nil {
		if announcement.lifecycleDone != nil {
			c.lifecycleDone = append(c.lifecycleDone, announcement.lifecycleDone)
		}
		return
	}
	if announcement.lifecycleDone == nil {
		c.addLifecycle(fn())
		return
	}
	select {
	case <-announcement.lifecycleDone:
		c.addLifecycle(fn())
		return
	default:
	}

	followup := newMoleculeLifecycleCompletion()
	c.addLifecycle(followup)
	go func() {
		<-announcement.lifecycleDone
		followup.finish(fn().Wait())
	}()
}

func closeSpecSidecarsForRootCompletion(store beads.Store, rootID string) moleculeLifecycleCompletion {
	result, err := sourceworkflow.CloseSpecSidecarsForRootSequenced(store, rootID, sourceworkflow.WorkflowSpecSidecarClosedReason)
	return moleculeLifecycleCompletionAfterDeliveries(result.Deliveries, err != nil)
}

func (c moleculeAutocloseCompletion) Wait() bool {
	for _, done := range c.lifecycleDone {
		<-done
	}
	retry := c.retryNeeded
	for _, check := range c.retryChecks {
		retry = check() || retry
	}
	return retry
}

func autocloseMoleculeIfComplete(store beads.Store, rec events.Recorder, mol beads.Bead, stdout io.Writer) moleculeCloseAnnouncement {
	if mol.Type != "molecule" {
		return moleculeCloseAnnouncement{}
	}
	if convoycore.IsTerminalStatus(mol.Status) {
		return moleculeCloseAnnouncement{}
	}
	terminal, descendants := subtreeTerminalExcludingRoot(store, mol.ID)
	if !terminal {
		return moleculeCloseAnnouncement{}
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
		return moleculeCloseAnnouncement{}
	}
	return announceEligibleClosedMoleculeResult(store, rec, mol, moleculeAutocloseReason, stdout)
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
func autocloseRootsForSourceBead(
	rootStore, sourceStore beads.Store,
	sourceStoreRef, rootStoreRef string,
	rec events.Recorder,
	sourceBeadID string,
	stdout io.Writer,
) moleculeAutocloseCompletion {
	var completion moleculeAutocloseCompletion
	sourceStoreRef = sourceworkflow.NormalizeSourceStoreRef(sourceStoreRef)
	rootStoreRef = sourceworkflow.NormalizeSourceStoreRef(rootStoreRef)
	requirePhysicalRefs := !sameMoleculeLifecycleStore(rootStore, sourceStore)
	roots, err := sourceworkflow.ListLiveRoots(rootStore, sourceBeadID, sourceStoreRef, rootStoreRef)
	if err != nil {
		return completion
	}
	for _, root := range roots {
		if terminal, _ := subtreeTerminalExcludingRoot(rootStore, root.ID); terminal {
			result := announceEligibleSourceClosedMoleculeResult(
				rootStore,
				sourceStore,
				sourceStoreRef,
				rootStoreRef,
				requirePhysicalRefs,
				sourceBeadID,
				rec,
				root,
				stdout,
			)
			switch {
			case result.sidecarCleanupOwned:
				completion.add(result)
			case result.closed:
				// Only the transition owner can sequence cleanup after its root
				// lifecycle delivery. An atomic loser has no receipt for the winner;
				// leave idempotent cleanup to that owner or periodic residue recovery.
				completion.addFollowup(result, func() moleculeLifecycleCompletion {
					return closeSpecSidecarsForRootCompletion(rootStore, root.ID)
				})
			default:
				completion.add(result)
			}
		}
	}
	return completion
}

// subtreeTerminalExcludingRoot reports whether every transitive descendant of
// rootID (the root itself excluded) is terminal, and how many descendants were
// found. It walks the full subtree — parent-child edges plus the
// gc.root_bead_id metadata link — so roots whose steps fan out into nested
// children (formula-compiler "epic" steps, gate-deferred sub-trees) are
// evaluated by descendant terminality rather than direct children. A walk
// error yields (false, 0) so the caller leaves the root open.
func subtreeTerminalExcludingRoot(store beads.Store, rootID string) (terminal bool, descendants int) {
	terminal, descendants, _ = subtreeTerminalExcludingRootDetailed(store, rootID)
	return terminal, descendants
}

func subtreeTerminalExcludingRootDetailed(store beads.Store, rootID string) (terminal bool, descendants int, err error) {
	subtree, err := molecule.ListSubtree(store, rootID)
	if err != nil {
		return false, 0, err
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
			return false, descendants, nil
		}
	}
	return true, descendants, nil
}

// announceClosedMolecule closes mol with the given close_reason, emits the
// lifecycle records after a successful close, and prints the auto-close
// announcement to stdout. Stores with atomic close support provide exact
// transition ownership; capability-less stores preserve the established
// at-least-once lifecycle contract. Shared by the step-terminal and
// source-bead-close triggers. Best-effort: a close failure aborts silently.
func announceClosedMolecule(store beads.Store, rec events.Recorder, mol beads.Bead, reason string, stdout io.Writer) bool {
	return announceClosedMoleculeResult(store, rec, mol, reason, stdout).closed
}

type moleculeCloseAnnouncement struct {
	closed               bool
	lifecycleDone        <-chan struct{}
	lifecycleRetryNeeded bool
	lifecycleRetry       func() bool
	sidecarCleanupOwned  bool
}

type moleculeEligibilityContext struct {
	sourceStore         beads.Store
	sourceStoreRef      string
	rootStoreRef        string
	sourceBeadID        string
	requirePhysicalRefs bool
}

func announceClosedMoleculeResult(store beads.Store, rec events.Recorder, mol beads.Bead, reason string, stdout io.Writer) moleculeCloseAnnouncement {
	return announceClosedMoleculeResultWithEligibility(store, rec, mol, reason, stdout, false, moleculeEligibilityContext{})
}

func announceEligibleClosedMoleculeResult(store beads.Store, rec events.Recorder, mol beads.Bead, reason string, stdout io.Writer) moleculeCloseAnnouncement {
	return announceClosedMoleculeResultWithEligibility(store, rec, mol, reason, stdout, true, moleculeEligibilityContext{})
}

func announceEligibleSourceClosedMoleculeResult(
	rootStore, sourceStore beads.Store,
	sourceStoreRef, rootStoreRef string,
	requirePhysicalRefs bool,
	sourceBeadID string,
	rec events.Recorder,
	root beads.Bead,
	stdout io.Writer,
) moleculeCloseAnnouncement {
	return announceClosedMoleculeResultWithEligibility(
		rootStore,
		rec,
		root,
		moleculeSourceAutocloseReason,
		stdout,
		true,
		moleculeEligibilityContext{
			sourceStore:         sourceStore,
			sourceStoreRef:      sourceworkflow.NormalizeSourceStoreRef(sourceStoreRef),
			rootStoreRef:        sourceworkflow.NormalizeSourceStoreRef(rootStoreRef),
			sourceBeadID:        sourceworkflow.NormalizeSourceBeadID(sourceBeadID),
			requirePhysicalRefs: requirePhysicalRefs,
		},
	)
}

func announceClosedMoleculeResultWithEligibility(
	store beads.Store,
	rec events.Recorder,
	mol beads.Bead,
	reason string,
	stdout io.Writer,
	requireEligibility bool,
	eligibility moleculeEligibilityContext,
) moleculeCloseAnnouncement {
	lifecycleDone := make(chan struct{})
	completed := func(result moleculeCloseAnnouncement) moleculeCloseAnnouncement {
		close(lifecycleDone)
		result.lifecycleDone = lifecycleDone
		return result
	}
	result, err := closeMoleculeWithReasonAndEvent(store, mol.ID, reason, requireEligibility, eligibility)
	if err != nil {
		return completed(moleculeCloseAnnouncement{
			lifecycleRetryNeeded: result.lifecyclePending && !errors.Is(err, errMoleculeLifecycleIneligible),
		})
	}
	transition := result.transition
	if !result.authoritativeAfter {
		// The unconditional close returned success, but the exact durable row is
		// not yet readable. The pre-close intent keeps publication recoverable;
		// never synthesize a payload from the caller's stale snapshot.
		if result.closeSucceeded {
			fmt.Fprintf(stdout, "Auto-closed molecule %s (lifecycle publication pending authoritative recovery)\n", mol.ID) //nolint:errcheck // best-effort stdout
		}
		return completed(moleculeCloseAnnouncement{
			closed:               result.closeSucceeded,
			lifecycleRetryNeeded: result.lifecyclePending,
			sidecarCleanupOwned:  result.lifecyclePending,
		})
	}
	if !transition.Transitioned {
		// Another writer can win after the live-root scan but before this
		// predicate close. The loser must not announce or emit lifecycle events,
		// but the authoritative closed snapshot is enough to run idempotent
		// generated-sidecar cleanup.
		return completed(moleculeCloseAnnouncement{
			lifecycleRetryNeeded: result.lifecyclePending,
			sidecarCleanupOwned:  result.lifecyclePending,
		})
	}
	closedMol := transition.After
	if result.lifecycleIntentID != "" {
		publication := publishPendingMoleculeLifecycle(store, rec, closedMol.ID, result.lifecycleIntentID, result.lifecycleDelivery)
		fmt.Fprintf(stdout, "Auto-closed molecule %s %q (lifecycle delivery is at-least-once: store lacks atomic close support)\n", closedMol.ID, closedMol.Title) //nolint:errcheck // best-effort stdout
		return moleculeCloseAnnouncement{
			closed:              true,
			lifecycleDone:       publication.Done(),
			lifecycleRetry:      publication.Wait,
			sidecarCleanupOwned: true,
		}
	}
	closeReason := strings.TrimSpace(closedMol.Metadata["close_reason"])

	// A decorating store reports whether it published or owns publication of
	// this exact winning transition. Bare stores leave both fields empty, so
	// publish the exact durable snapshot returned by the close operation.
	observerOwnsClosed := transition.ObserverNotified || transition.ObserverDelivery != nil
	actor := eventActor()
	recordClosed := func() {}
	if !observerOwnsClosed {
		recordClosed = func() {
			payload, err := json.Marshal(closedMol)
			if err != nil {
				return
			}
			closedEvent := events.Event{
				Type:    events.BeadClosed,
				Actor:   actor,
				Subject: closedMol.ID,
				Payload: payload,
			}
			stampBeadSnapshotCorrelation(&closedEvent, closedMol)
			rec.Record(closedEvent)
		}
	}

	// Additive attribution record: join the resolved molecule to the session
	// that produced it, read from the identity the reconciler stamped onto the
	// root (gc.session_* / gc.work_dir). A root closed before any reconcile
	// stamped it — or hand-closed by a human — degrades to empty session
	// fields rather than failing. Honesty-gate C.0 backbone for C.1/C.2/C.3.
	resolvedEvent := events.Event{
		Type:    events.MoleculeResolved,
		Actor:   actor,
		Subject: closedMol.ID,
		Payload: api.MoleculeResolvedPayloadJSON(api.MoleculeResolvedPayload{
			IssueID:     closedMol.ID,
			FromStatus:  transition.Before.Status,
			ToStatus:    "closed",
			Actor:       actor,
			SessionName: closedMol.Metadata[beadmeta.SessionNameMetadataKey],
			SessionID:   closedMol.Metadata[beadmeta.SessionIDMetadataKey],
			WorkDir:     closedMol.Metadata[beadmeta.WorkDirMetadataKey],
			CloseReason: closeReason,
			Ts:          time.Now().UTC(),
		}),
	}
	stampBeadSnapshotCorrelation(&resolvedEvent, closedMol)
	recordResolved := func() { rec.Record(resolvedEvent) }
	switch {
	case observerOwnsClosed && transition.ObserverDelivery != nil:
		// The close observer may be queued behind a callback that re-entered the
		// cache. Sequence the additive resolution record after that exact
		// bead.closed delivery without waiting inside the callback.
		transition.ObserverDelivery.AfterDelivery(func() {
			defer close(lifecycleDone)
			recordResolved()
		})
	case result.lifecycleDelivery != nil:
		// A capability-less cache suppresses its close observer because transition
		// ownership is only at-least-once. Reserve the manual lifecycle records at
		// that suppressed close's exact queue position so an earlier open
		// bead.updated snapshot cannot overtake the durable closed snapshot.
		result.lifecycleDelivery.AfterDelivery(func() {
			defer close(lifecycleDone)
			recordClosed()
			recordResolved()
		})
	default:
		func() {
			defer close(lifecycleDone)
			recordClosed()
			recordResolved()
		}()
	}

	if result.ownershipProvable {
		fmt.Fprintf(stdout, "Auto-closed molecule %s %q\n", closedMol.ID, closedMol.Title) //nolint:errcheck // best-effort stdout
	} else {
		fmt.Fprintf(stdout, "Auto-closed molecule %s %q (lifecycle delivery is at-least-once: store lacks atomic close support)\n", closedMol.ID, closedMol.Title) //nolint:errcheck // best-effort stdout
	}
	return moleculeCloseAnnouncement{closed: true, lifecycleDone: lifecycleDone}
}

type moleculeCloseResult struct {
	transition         beads.CloseTransition
	ownershipProvable  bool
	authoritativeAfter bool
	lifecycleDelivery  beads.CloseObserverDelivery
	lifecycleIntentID  string
	lifecyclePending   bool
	closeSucceeded     bool
}

const (
	moleculePostCloseReadAttempts = 3
	moleculePostCloseReadDelay    = 10 * time.Millisecond
)

// closeMoleculeWithReasonAndEvent returns whether transition ownership is
// provable. Capability-less exec stores retain the established successful-close
// behavior and publish lifecycle records at least once; their non-atomic
// protocol can duplicate those records under a racing close.
func closeMoleculeWithReasonAndEvent(
	store beads.Store,
	id, reason string,
	requireEligibility bool,
	eligibility moleculeEligibilityContext,
) (moleculeCloseResult, error) {
	// A capability-less attempt may have durably prepared ownership before the
	// store gained an atomic close capability (for example after restart). Reuse
	// that exact intent before probing the current capability; an atomic close
	// would otherwise publish once while leaving the old marker to publish again
	// during recovery.
	if live, err := beads.HandlesFor(store).Live.Get(id); err == nil &&
		strings.TrimSpace(live.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey]) != "" {
		return closeMoleculeWithPreparedLifecycle(store, id, reason, requireEligibility, "", eligibility)
	}
	if requireEligibility {
		return closeMoleculeWithPreparedLifecycle(store, id, reason, true, "", eligibility)
	}

	closer, ok := beads.CloseTransitionerFor(store)
	if ok {
		transition, err := closer.CloseWithReasonIfOpen(id, strings.TrimSpace(reason))
		if err == nil || transition.AuthoritativeClosed(id) {
			return moleculeCloseResult{
				transition:         transition,
				ownershipProvable:  true,
				authoritativeAfter: true,
			}, nil
		}
		if !errors.Is(err, beads.ErrCloseTransitionUnsupported) {
			return moleculeCloseResult{}, err
		}
	}

	return closeMoleculeWithPreparedLifecycle(store, id, reason, requireEligibility, "", eligibility)
}

func closeMoleculeWithPreparedLifecycle(
	store beads.Store,
	id, reason string,
	requireEligibility bool,
	expectedIntentID string,
	eligibility moleculeEligibilityContext,
) (moleculeCloseResult, error) {
	if store == nil {
		return moleculeCloseResult{}, errors.New("close prepared molecule lifecycle: nil store")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return moleculeCloseResult{}, errors.New("close prepared molecule lifecycle: empty bead id")
	}
	reason = strings.TrimSpace(reason)
	sourceStore := store
	sourceID := id
	if requireEligibility && reason == moleculeSourceAutocloseReason {
		if eligibility.sourceStore != nil {
			sourceStore = eligibility.sourceStore
		}
		sourceID = sourceworkflow.NormalizeSourceBeadID(eligibility.sourceBeadID)
		if sourceID == "" {
			fresh, readErr := beads.HandlesFor(store).Live.Get(id)
			if readErr != nil {
				return moleculeCloseResult{}, readErr
			}
			sourceID = sourceworkflow.NormalizeSourceBeadID(fresh.Metadata[beadmeta.SourceBeadIDMetadataKey])
			if eligibility.sourceStore == nil &&
				sourceworkflow.NormalizeSourceStoreRef(fresh.Metadata[beadmeta.SourceStoreRefMetadataKey]) != "" {
				return moleculeCloseResult{}, errMoleculeLifecycleIneligible
			}
		}
		if sourceID == "" {
			return moleculeCloseResult{}, errMoleculeLifecycleIneligible
		}
	}
	var (
		before beads.Bead
		intent moleculeLifecycleIntent
		result moleculeCloseResult
	)
	runTransaction := func(sourceTx beads.LifecycleReadTransaction, rootTx beads.LifecycleMetadataTransaction) error {
		var rootReader beads.LifecycleReadTransaction
		if requireEligibility {
			var ok bool
			rootReader, ok = rootTx.(beads.LifecycleReadTransaction)
			if !ok {
				return beads.ErrLifecycleReadUnsupported
			}
			if reason == moleculeSourceAutocloseReason {
				eligibility.requirePhysicalRefs = eligibility.requirePhysicalRefs &&
					!sameMoleculeLifecycleTransaction(sourceTx, rootReader)
			}
		}
		if expectedIntentID == "" && requireEligibility {
			fresh, readErr := rootTx.Get()
			if readErr != nil {
				return readErr
			}
			provisional := moleculeLifecycleIntent{FromStatus: fresh.Status, CloseReason: reason}
			eligible, eligibilityErr := preparedOpenMoleculeLifecycleEligible(sourceTx, rootReader, fresh, provisional, eligibility)
			if eligibilityErr != nil {
				return eligibilityErr
			}
			if !eligible {
				return errMoleculeLifecycleIneligible
			}
		}
		if expectedIntentID == "" {
			var prepareErr error
			before, intent, prepareErr = prepareMoleculeLifecycleIntentTransaction(
				rootTx,
				id,
				reason,
				eventActor(),
				time.Now().UTC(),
			)
			if prepareErr != nil {
				return prepareErr
			}
		} else {
			fresh, readErr := rootTx.Get()
			if readErr != nil {
				return readErr
			}
			before = fresh
			prepared, disposition := classifyPreparedOpenMoleculeLifecycle(fresh, id)
			if disposition == moleculeLifecycleRetry {
				return errors.New("close prepared molecule lifecycle: authoritative intent read failed")
			}
			if disposition != moleculeLifecycleReady || prepared.IntentID != expectedIntentID {
				return errMoleculeLifecycleIneligible
			}
			intent = prepared
		}

		// Re-read after the marker becomes durable. Hooks joined to the same
		// lifecycle lease may have run during metadata persistence; this exact row
		// and the live graph below are the state authorized for the close.
		fresh, readErr := rootTx.Get()
		if readErr != nil {
			return readErr
		}
		prepared, disposition := classifyPreparedOpenMoleculeLifecycle(fresh, id)
		if disposition == moleculeLifecycleRetry {
			return errors.New("close prepared molecule lifecycle: authoritative state revalidation failed")
		}
		if disposition != moleculeLifecycleReady || prepared.IntentID != intent.IntentID {
			return errMoleculeLifecycleIneligible
		}
		if requireEligibility {
			eligible, eligibilityErr := preparedOpenMoleculeLifecycleEligible(sourceTx, rootReader, fresh, prepared, eligibility)
			if eligibilityErr != nil {
				return eligibilityErr
			}
			if !eligible {
				return errMoleculeLifecycleIneligible
			}
		}
		var closeErr error
		result, closeErr = closePreparedMoleculeLifecycleTransaction(rootTx, before, prepared)
		return closeErr
	}
	var err error
	if requireEligibility {
		err = beads.WithLifecycleReadTransactions(
			sourceStore,
			sourceID,
			store,
			id,
			func(sourceTx, rootTx beads.LifecycleReadTransaction) error {
				return runTransaction(sourceTx, rootTx)
			},
		)
	} else {
		err = beads.WithLifecycleMetadataTransaction(store, id, func(rootTx beads.LifecycleMetadataTransaction) error {
			return runTransaction(nil, rootTx)
		})
	}
	if errors.Is(err, beads.ErrLifecycleMultiStoreUnsupported) || errors.Is(err, beads.ErrLifecycleReadUnsupported) {
		err = errors.Join(errMoleculeLifecycleIneligible, err)
	}
	if err != nil {
		if before.ID == id && before.Status == "closed" {
			return moleculeCloseResult{
				transition:         beads.CloseTransition{Before: before, After: before},
				authoritativeAfter: true,
				lifecyclePending:   before.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != "",
			}, nil
		}
		if result.transition.Before.ID == "" {
			result.transition.Before = before
		}
		result.lifecyclePending = true
		return result, err
	}
	return result, nil
}

// closePreparedMoleculeLifecycleTransaction resumes an exact durable intent
// without releasing tx's lifecycle mutation lease. The transaction capability
// suppresses cache observers and reserves their ordering receipt before this
// callback returns; publication occurs only from the authoritative closed row.
func closePreparedMoleculeLifecycleTransaction(
	tx beads.LifecycleMetadataTransaction,
	before beads.Bead,
	intent moleculeLifecycleIntent,
) (moleculeCloseResult, error) {
	id := before.ID
	closeResult, closeErr := beads.CloseWithinLifecycleMetadataTransaction(tx, intent.CloseReason)
	after := closeResult.After
	authoritative := pendingMoleculeLifecycleSnapshotOwned(after, id, intent.IntentID)
	// The transaction close already performed the first authoritative post-close
	// read. Spend only the remaining bounded attempts here.
	for attempt := 1; !authoritative && attempt < moleculePostCloseReadAttempts; attempt++ {
		fresh, readErr := tx.Get()
		if readErr == nil {
			after = fresh
			authoritative = pendingMoleculeLifecycleSnapshotOwned(after, id, intent.IntentID)
		}
		if authoritative || attempt+1 == moleculePostCloseReadAttempts {
			break
		}
		time.Sleep(moleculePostCloseReadDelay)
	}
	if !authoritative {
		return moleculeCloseResult{
			transition:        beads.CloseTransition{Before: before},
			lifecycleDelivery: closeResult.ObserverDelivery,
			lifecycleIntentID: intent.IntentID,
			lifecyclePending:  true,
			closeSucceeded:    closeResult.CloseSucceeded,
		}, closeErr
	}
	return moleculeCloseResult{
		transition: beads.CloseTransition{
			Before: before,
			After:  after,
			Transitioned: closeResult.Transitioned ||
				(closeResult.CloseSucceeded && closeResult.Before.ID == id && closeResult.Before.Status != "closed"),
		},
		authoritativeAfter: true,
		lifecycleDelivery:  closeResult.ObserverDelivery,
		lifecycleIntentID:  intent.IntentID,
		lifecyclePending:   true,
		closeSucceeded:     true,
	}, nil
}

func pendingMoleculeLifecycleSnapshotOwned(fresh beads.Bead, id, expectedIntentID string) bool {
	intent, disposition := classifyCurrentPendingMoleculeLifecycle(fresh, id)
	return disposition == moleculeLifecycleReady && intent.IntentID == expectedIntentID
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
