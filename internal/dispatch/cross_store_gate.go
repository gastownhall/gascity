package dispatch

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// Cross-store gates: how a drain item waits for a blocker held in another
// store without writing an edge that store cannot resolve.
//
// # The problem
//
// A drained member's out-of-convoy `blocks` dependency names a WORK bead, and
// the item workflow the drain builds for that member lives in the graph
// binding. A dependency row can only name an id its own store holds: written
// anyway, SQLite reads the absent target as never closed and strands the item
// forever, while bd/Dolt reads it as external and never blocks at all. So the
// drain used to drop the edge and let the item run at once — ahead of the work
// its member was waiting for (read-audit R3). On a single-store city the edge
// exists and the item waits.
//
// # The design: a local gate that mirrors the remote blocker
//
// For each blocker the item's store cannot hold and that still holds its
// dependents (open, or closed blocked: beads.DependencySatisfied), the drain
// mints a GATE in the item's own store — a bead of type "gate" (never ready
// work on any backend) carrying the blocker's id — and the item workflow
// depends on the gate instead of on the blocker. That edge is an ordinary
// same-store edge, so every reader of readiness agrees on it with no change of
// its own. No dependency row ever names an id its store does not hold.
//
// Gates exist only on a split city, and only in the store the drain's item
// workflow lives in — the class binding (a SQLite or workspace engine). A drain
// on a city with no binding names no member stores, never classifies a blocker
// as unprojectable, and so never mints one. A gate must never land in a bd
// work store: bd would read the gate's own edges by its prefix rules, not ours.
//
// # Release
//
// ReleaseCrossStoreGates, run by the controller OFF its tick: a gate closes
// once its blocker is closed (and not closed blocked, beads.DependencySatisfied)
// in every store that holds it, and once its drain control is closed or gone,
// so a drain that failed or was canceled leaves no open gate behind. A blocker
// no store holds keeps its gate open — an unresolvable blocker blocks, as it
// does on a single-store city — and the gate records it in
// gc.cross_store_gate_state=unresolved, which the caller reports (an event and
// a doctor finding) and an operator can override by closing the gate; a
// blocker that is restored clears the mark. A blocker a store that could not
// be read MAY hold keeps its gate exactly as it is, and the pass reports the
// store and the gates held only on it (the caller logs them, gc doctor names
// them).
//
// # A blocker that reopens
//
// On a single-store city a closed blocker that is reopened holds its
// dependents again. A gate follows it, and only on evidence that the blocker
// holds them again (it is open, or closed blocked):
//
//   - The drain reopens a controller-closed gate only when it FOUND the
//     blocker holding. Its residency frame is narrower than the release
//     pass's (it does not read every serving rig), so a blocker it cannot find
//     is not evidence: it leaves the gate as the release pass left it.
//     Otherwise a closed cross-rig blocker would be released by the pass and
//     reopened by the drain every tick, and an agent could claim in each
//     runnable window.
//   - The release pass reopens a released gate of a live drain only when some
//     store holds a copy of the blocker that does not release it. A blocker
//     that is simply gone (deleted, purged after closing) reopens nothing.
//
// A gate is controller-closed when it carries gc.cross_store_gate_state
// released or retired, or one of the exact close reasons a workflow teardown
// stamps (controllerCloseReasons). Any other closed gate — including one closed
// with an operator's reason — was closed by an operator: that close is final,
// and it is recorded as overridden the first time the drain sees it.
//
// A drain control that is reopened after its gates were closed by the
// controller meets them again — retired by the pass, or closed by a teardown
// (a run cancel, the finalizer, wisp GC) with a controller reason. A retired
// gate records the state it was retired from
// (gc.cross_store_gate_retired_from), and a drain that does not find its
// blocker hands the gate back to the release pass, which reads every store
// (classifyHandBack):
//
//   - released, or retired from released: the pass had released it, so it is
//     (or goes back to) released, and the pass reopens it if its blocker
//     opens again;
//   - anything else — retired from open, or closed by a teardown with no
//     marker or while unresolved: the pass never released it (its blocker was
//     open, unresolved, or in a store it could not read), so it is reopened
//     with its state cleared, and the pass judges it afresh. The pass lists
//     only open and released gates; left closed, the gate would let the item
//     run on a judgement the pass never made.
//
// Every state change — release, reopen, retire — is ONE write carrying the
// status and the marker together, so no failure between two writes can leave
// an "open but released" gate whose later operator close would read as a
// controller release. Each is made only while its premise still holds for the
// gate's live row, as a revision CAS where the store resolves conditional
// writes (writeGateIfStill), so an operator close that lands between the
// pass's listing and its write is never overwritten.
//
// # Released gates of finished drains
//
// A released gate whose drain control has closed can never matter again; the
// pass marks it retired (one write, once), which takes it out of the released
// listing the pass reads every time.
//
// # Identity
//
// One gate serves every item of one drain that waits on the same blocker. Its
// id is derived from (control, blocker) under the store's own mint prefix, so
// two processes that race to mint it collide on the store's duplicate-id check
// and both end up with the one gate.
//
// The id is pinned only in a store that declares a mint prefix (IDPrefix) and
// honors a pinned id: the SQLite engine and the native Dolt workspace engine a
// binding is served from, and the CachingStore over either. A store that
// declares no prefix (the in-memory test stores) mints its own id, and the
// gate is found by its metadata (owner and blocker) instead; that path is
// check-then-create and so not race-safe, which is why production stores pin.

// CrossStoreGateLabel marks a gate bead that mirrors a blocker held in another
// store.
const CrossStoreGateLabel = "gc:cross-store-gate"

// crossStoreGateType is the gate bead's type. "gate" is excluded from Ready by
// every backend and by beads.IsReadyExcludedType, so the gate is never offered
// to a worker as work.
const crossStoreGateType = "gate"

// Gate states recorded in gc.cross_store_gate_state.
const (
	// CrossStoreGateStateUnresolved: no store holds the gate's blocker.
	CrossStoreGateStateUnresolved = "unresolved"
	// CrossStoreGateStateReleased: the controller released the gate (its
	// close_reason says why). It is reopened if its blocker opens again.
	CrossStoreGateStateReleased = "released"
	// CrossStoreGateStateOverridden: an operator closed the gate. Final: the
	// drain's items run whatever the blocker does.
	CrossStoreGateStateOverridden = "overridden"
	// CrossStoreGateStateRetired: the controller closed the gate because its
	// drain control is closed or gone, recording the state it was retired
	// from. The release pass no longer reads it; a reopened drain hands it
	// back (handBackGate), and one that finds its blocker open reopens
	// it.
	CrossStoreGateStateRetired = "retired"
)

// crossStoreGateRoot is the workflow root a drain's gates belong to: the
// control's own root, or the control itself when it names none.
func crossStoreGateRoot(control beads.Bead) string {
	if root := strings.TrimSpace(control.Metadata[beadmeta.RootBeadIDMetadataKey]); root != "" {
		return root
	}
	return control.ID
}

// crossStoreGateID is the deterministic id of control's gate for blockerID in
// store, or "" when store declares no mint prefix to pin it under.
func crossStoreGateID(store beads.Store, control beads.Bead, blockerID string) string {
	prefixed, ok := store.(interface{ IDPrefix() string })
	if !ok {
		return ""
	}
	prefix := strings.Trim(strings.TrimSpace(prefixed.IDPrefix()), "-")
	if prefix == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(control.ID + "\x00" + blockerID))
	return prefix + "-xsg-" + hex.EncodeToString(sum[:6])
}

// controllerCloseReasons are the close reasons a controller stamps on every
// open bead of a workflow subtree it closes — gates included, since a gate
// carries its drain's root. A gate closed with one of these, and no state
// marker, was closed by the controller. Any other reason — `gc bd close <gate>
// --reason ...`, an API close with a reason — is an operator's.
//
// The paths that reach a gate, each pinned by
// TestEveryControllerSubtreeCloseReadsAsAControllerClose:
//   - molecule.CloseSubtree (sling attachment of a non-workflow molecule);
//   - sourceworkflow.CloseWorkflowSubtree (sling, gc formula, drain teardown,
//     sling attachment of a workflow);
//   - wisp GC's repair of closed roots and the workflow finalizer's sweep on
//     pass or fail (WorkflowSkippedCloseReason);
//   - the finalizer's sweep of a canceled run (workflowCanceledMemberCloseReason);
//   - POST /runs/{id}/cancel (sourceworkflow.RunCanceledCloseReason);
//   - gc hook --claim's skip of a workflow whose input is already done, and
//     the order-tracking sweep of stale order wisp subtrees (both close by
//     gc.root_bead_id).
//
// A scope abort (scopeAbortSkippedCloseReason) closes only the members of one
// scope, which a gate never is.
var controllerCloseReasons = map[string]bool{
	molecule.SubtreeClosedReason:                   true,
	sourceworkflow.WorkflowSubtreeClosedReason:     true,
	sourceworkflow.WorkflowSkippedCloseReason:      true,
	sourceworkflow.RunCanceledCloseReason:          true,
	workflowCanceledMemberCloseReason:              true,
	sourceworkflow.HookSkipDoneWorkflowCloseReason: true,
	sourceworkflow.StaleOrderWispCloseReason:       true,
}

// IsControllerClosedCrossStoreGate reports whether gate is a closed
// cross-store gate the controller closed (and may reopen), rather than one an
// operator closed (final). Callers outside dispatch use it to pin that their
// subtree close reads as the controller's.
func IsControllerClosedCrossStoreGate(gate beads.Bead) bool {
	return gate.Status == "closed" && controllerClosed(gate)
}

// controllerClosed reports whether a closed gate was closed by the controller
// (a release, a retire, or a workflow teardown's stamped close) rather than by
// an operator.
func controllerClosed(gate beads.Bead) bool {
	switch gate.Metadata[beadmeta.CrossStoreGateStateMetadataKey] {
	case CrossStoreGateStateReleased, CrossStoreGateStateRetired:
		return true
	case CrossStoreGateStateOverridden:
		return false
	}
	return controllerCloseReasons[strings.TrimSpace(gate.Metadata["close_reason"])]
}

// gateAnswer settles an existing gate for a blocker the drain needs held:
// foundOpen reports whether the drain FOUND the blocker holding its dependents
// (open, or closed blocked) rather than not finding it at all. An open gate
// holds. A controller-closed gate is reopened on that evidence; otherwise a
// released gate is left as the release pass left it, and any other
// controller-closed gate (retired, or closed by a teardown), whose drain is
// live again, is handed back to the release pass (handBackGate). An
// operator-closed gate is final, and its override is recorded the first time
// it is seen. It returns the gate's id when it holds the item, "" when it does
// not.
func gateAnswer(store beads.Store, gate beads.Bead, foundOpen bool) (string, error) {
	if gate.Status != "closed" {
		live, err := clearLeftoverGateMarker(store, gate)
		if err != nil {
			return "", err
		}
		if live.Status == "closed" {
			// It closed under the clearing write (an operator's close, a
			// release): settle the closed gate it now is.
			return gateAnswer(store, live, foundOpen)
		}
		return live.ID, nil
	}
	if controllerClosed(gate) {
		if !foundOpen {
			return handBackGate(store, gate)
		}
		wrote, err := reopenCrossStoreGate(store, gate.ID)
		if err != nil {
			return "", err
		}
		if !wrote {
			return reReadGateAnswer(store, gate.ID)
		}
		return gate.ID, nil
	}
	if gate.Metadata[beadmeta.CrossStoreGateStateMetadataKey] == CrossStoreGateStateOverridden {
		return "", nil
	}
	if _, err := writeGateIfStill(store, gate.ID, func(b beads.Bead) bool {
		return b.Status == "closed" && !controllerClosed(b)
	}, beads.UpdateOpts{Metadata: map[string]string{beadmeta.CrossStoreGateStateMetadataKey: CrossStoreGateStateOverridden}}, false); err != nil {
		return "", fmt.Errorf("recording the operator override of cross-store gate %s: %w", gate.ID, err)
	}
	return "", nil
}

// gateHandBack is what a drain that did not find the blocker does with a
// controller-closed gate of its live control.
type gateHandBack int

const (
	// handBackNone: the pass released the gate; leave it to the pass.
	handBackNone gateHandBack = iota
	// handBackReleased: a gate the pass had released and then retired goes
	// back to released.
	handBackReleased
	// handBackReopen: a gate whose blocker the pass never judged released —
	// retired while open, closed by a controller teardown (cancel, finalize,
	// wisp GC) with no marker or while unresolved — is reopened with its
	// state cleared, so it holds the item until the pass judges it afresh.
	handBackReopen
)

// classifyHandBack decides the hand-back of a gate by its live row. Both
// hand-back writes use it as their premise, so a gate whose classification
// moved between the drain's read and its write (another drain or a pass
// changed it) is never written on a stale judgement.
func classifyHandBack(b beads.Bead) gateHandBack {
	if b.Status != "closed" || !controllerClosed(b) {
		return handBackNone
	}
	switch b.Metadata[beadmeta.CrossStoreGateStateMetadataKey] {
	case CrossStoreGateStateReleased:
		return handBackNone
	case CrossStoreGateStateRetired:
		if b.Metadata[beadmeta.CrossStoreGateRetiredFromMetadataKey] == CrossStoreGateStateReleased {
			return handBackReleased
		}
	}
	return handBackReopen
}

// handBackGate returns a controller-closed gate of a live drain to the release
// pass in ONE write (classifyHandBack): a gate the pass had released goes back
// to released (it does not hold the item); a gate the pass never released is
// reopened with its state cleared (it holds the item until the pass judges it
// afresh). The release pass lists only open and released gates, so a gate
// left closed any other way would let the item run on a judgement nobody made.
func handBackGate(store beads.Store, gate beads.Bead) (string, error) {
	kind := classifyHandBack(gate)
	still := func(b beads.Bead) bool { return classifyHandBack(b) == kind }
	switch kind {
	case handBackReleased:
		reason := "released by the release pass; handed back to it when its drain control was reopened"
		if blocker := gateBlocker(gate); blocker != "" {
			reason = fmt.Sprintf("blocker %s closed; handed back to the release pass when its drain control was reopened", blocker)
		}
		wrote, err := writeGateIfStill(store, gate.ID, still, beads.UpdateOpts{Metadata: map[string]string{
			beadmeta.CrossStoreGateStateMetadataKey:       CrossStoreGateStateReleased,
			beadmeta.CrossStoreGateRetiredFromMetadataKey: "",
			"close_reason": reason,
		}}, false)
		if err != nil {
			return "", fmt.Errorf("handing retired cross-store gate %s back to the release pass: %w", gate.ID, err)
		}
		if !wrote {
			return reReadGateAnswer(store, gate.ID)
		}
		return "", nil
	case handBackReopen:
		wrote, err := writeGateIfStill(store, gate.ID, still, reopenGateOpts(), false)
		if err != nil {
			return "", fmt.Errorf("reopening cross-store gate %s for the release pass: %w", gate.ID, err)
		}
		if !wrote {
			return reReadGateAnswer(store, gate.ID)
		}
		return gate.ID, nil
	}
	return "", nil
}

// leftoverOpenGateMarker reports whether an OPEN gate still carries a closed
// gate's marker: an operator reopened (`bd reopen`, which keeps gc.* metadata)
// a gate that was overridden, released or retired. Left in place, a later
// controller teardown of the gate would read as that old state — an
// overridden marker as an operator's final close, so a revived drain's item
// would run unjudged.
func leftoverOpenGateMarker(b beads.Bead) bool {
	if b.Status == "closed" {
		return false
	}
	switch b.Metadata[beadmeta.CrossStoreGateStateMetadataKey] {
	case CrossStoreGateStateOverridden, CrossStoreGateStateReleased, CrossStoreGateStateRetired:
		return true
	}
	return b.Metadata[beadmeta.CrossStoreGateRetiredFromMetadataKey] != ""
}

// clearLeftoverGateMarker clears a leftover closed-gate marker from an open
// gate (leftoverOpenGateMarker) in ONE conditional write, so it reads as an
// ordinary open gate. It returns the gate as the caller should judge it: the
// cleared gate when the write landed, and otherwise the gate's LIVE row — its
// premise moved (an operator closed the gate, a pass released it or another
// caller cleared it), or both revision checks were lost and the marker stays
// for a later pass. That row may be closed, and is judged as the closed gate
// it is, never as the caller's open copy with the marker deleted.
func clearLeftoverGateMarker(store beads.Store, gate beads.Bead) (beads.Bead, error) {
	if !leftoverOpenGateMarker(gate) {
		return gate, nil
	}
	cleared := map[string]string{
		beadmeta.CrossStoreGateStateMetadataKey:       "",
		beadmeta.CrossStoreGateRetiredFromMetadataKey: "",
		beadmeta.OutcomeMetadataKey:                   "",
		"close_reason":                                "",
	}
	wrote, err := writeGateIfStill(store, gate.ID, leftoverOpenGateMarker, beads.UpdateOpts{Metadata: cleared}, false)
	if err != nil {
		return gate, fmt.Errorf("clearing the leftover marker of reopened cross-store gate %s: %w", gate.ID, err)
	}
	if !wrote {
		live, err := beads.HandlesFor(store).Live.Get(gate.ID)
		if err != nil {
			return gate, fmt.Errorf("re-reading cross-store gate %s after its leftover marker was not cleared: %w", gate.ID, err)
		}
		return live, nil
	}
	meta := make(map[string]string, len(gate.Metadata))
	for k, v := range gate.Metadata {
		meta[k] = v
	}
	for k := range cleared {
		delete(meta, k)
	}
	gate.Metadata = meta
	return gate, nil
}

// reReadGateAnswer answers from the gate's live row after a write whose
// premise moved under it: an open gate holds, a closed one does not.
func reReadGateAnswer(store beads.Store, id string) (string, error) {
	fresh, err := beads.HandlesFor(store).Live.Get(id)
	if err != nil {
		return "", fmt.Errorf("re-reading cross-store gate %s: %w", id, err)
	}
	if fresh.Status != "closed" {
		return fresh.ID, nil
	}
	return "", nil
}

// writeGateIfStill applies opts to the gate in ONE write, and only while still
// holds for its LIVE row, so a change made between the caller's read and this
// write — an operator closing the gate, another pass reopening it — is never
// overwritten. Where the store resolves conditional writes the write is a CAS
// on the re-read revision (a close uses the store's atomic close-with-metadata
// when it has one); a lost CAS re-reads and re-decides once, and a second loss
// leaves the gate for the next pass. Where conditional writes are off (the
// city's beads.conditional_writes, or a store with none) the live re-check is
// the guard, as for every other controller close (autocloseCloseIfStill). It
// reports whether it wrote.
func writeGateIfStill(store beads.Store, id string, still func(beads.Bead) bool, opts beads.UpdateOpts, closing bool) (bool, error) {
	for attempt := 0; attempt < 2; attempt++ {
		fresh, err := beads.HandlesFor(store).Live.Get(id)
		if err != nil {
			return false, err
		}
		if !still(fresh) {
			return false, nil
		}
		writer, _, err := beads.ResolveConditionalWriter(store)
		if err != nil {
			return false, err
		}
		if writer == nil {
			return true, store.Update(id, opts)
		}
		if closer, ok := beads.AtomicConditionalCloserFor(store); closing && ok {
			_, err = closer.CloseWithMetadataIfMatch(id, fresh.Revision, opts.Metadata)
		} else {
			err = writer.UpdateIfMatch(id, fresh.Revision, opts)
		}
		if beads.IsPreconditionFailed(err) {
			continue
		}
		return err == nil, err
	}
	return false, nil
}

// reopenCrossStoreGate reopens a controller-closed gate in ONE write that
// also clears its state marker and close reason (set empty: every engine
// takes that in the same write, where removing a key is refused by some), so
// it holds its items again and reads as an ordinary open gate.
func reopenCrossStoreGate(store beads.Store, id string) (bool, error) {
	wrote, err := writeGateIfStill(store, id, func(b beads.Bead) bool {
		return b.Status == "closed" && controllerClosed(b)
	}, reopenGateOpts(), false)
	if err != nil {
		return false, fmt.Errorf("reopening cross-store gate %s: %w", id, err)
	}
	return wrote, nil
}

// reopenGateOpts is the one write that reopens a gate as an ordinary open
// gate: its state marker, retired-from record and close reason are cleared.
func reopenGateOpts() beads.UpdateOpts {
	open := "open"
	return beads.UpdateOpts{
		Status: &open,
		Metadata: map[string]string{
			beadmeta.CrossStoreGateStateMetadataKey:       "",
			beadmeta.CrossStoreGateRetiredFromMetadataKey: "",
			beadmeta.OutcomeMetadataKey:                   "",
			"close_reason":                                "",
		},
	}
}

// closeCrossStoreGate closes an open gate in ONE write carrying its
// controller state (released or retired) and why. A gate retired while open
// records it, so a reopened drain hands it back to be judged afresh.
func closeCrossStoreGate(store beads.Store, id, state, reason string) (bool, error) {
	closed := "closed"
	meta := map[string]string{
		beadmeta.CrossStoreGateStateMetadataKey: state,
		"close_reason":                          reason,
	}
	if state == CrossStoreGateStateRetired {
		meta[beadmeta.CrossStoreGateRetiredFromMetadataKey] = "open"
	}
	wrote, err := writeGateIfStill(store, id, func(b beads.Bead) bool { return b.Status != "closed" }, beads.UpdateOpts{
		Status:   &closed,
		Metadata: meta,
	}, true)
	if err != nil {
		return false, fmt.Errorf("closing cross-store gate %s: %w", id, err)
	}
	return wrote, nil
}

// ensureCrossStoreGate returns the open gate in store that holds control's work
// back until blockerID closes, minting it on first use. foundOpen reports
// whether the caller FOUND the blocker holding its dependents (open, or closed
// blocked), rather than not finding it at all: only then is a controller-closed
// gate reopened (gateAnswer). It returns "" when no gate holds the item.
func ensureCrossStoreGate(store beads.Store, control beads.Bead, blockerID string, foundOpen bool) (string, error) {
	pinned := crossStoreGateID(store, control, blockerID)
	if pinned != "" {
		if gate, err := beads.HandlesFor(store).Live.Get(pinned); err == nil {
			return gateAnswer(store, gate, foundOpen)
		} else if !errors.Is(err, beads.ErrNotFound) {
			return "", fmt.Errorf("reading cross-store gate %s: %w", pinned, err)
		}
	}
	existing, err := store.List(beads.ListQuery{
		Label: CrossStoreGateLabel,
		Metadata: map[string]string{
			beadmeta.CrossStoreGateOwnerMetadataKey: control.ID,
			beadmeta.CrossStoreBlockerMetadataKey:   blockerID,
		},
		IncludeClosed: true,
		Live:          true,
		TierMode:      beads.TierBoth,
	})
	if err != nil {
		return "", fmt.Errorf("looking up the cross-store gate for blocker %s: %w", blockerID, err)
	}
	if len(existing) > 0 {
		sort.Slice(existing, func(i, j int) bool { return existing[i].ID < existing[j].ID })
		for _, gate := range existing {
			if gate.Status != "closed" {
				return gateAnswer(store, gate, foundOpen)
			}
		}
		return gateAnswer(store, existing[0], foundOpen)
	}
	gate, err := store.Create(beads.Bead{
		ID:     pinned,
		Title:  fmt.Sprintf("Waits for %s, held in another store", blockerID),
		Type:   crossStoreGateType,
		Labels: []string{CrossStoreGateLabel},
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey:          crossStoreGateRoot(control),
			beadmeta.CrossStoreGateOwnerMetadataKey: control.ID,
			beadmeta.CrossStoreBlockerMetadataKey:   blockerID,
		},
	})
	if err != nil {
		if pinned != "" {
			// A racing minter won the pinned id: settle its gate exactly as
			// one found up front.
			if raced, getErr := beads.HandlesFor(store).Live.Get(pinned); getErr == nil {
				return gateAnswer(store, raced, foundOpen)
			}
		}
		return "", fmt.Errorf("creating the cross-store gate for blocker %s: %w", blockerID, err)
	}
	return gate.ID, nil
}

// OpenCrossStoreGatesNaming returns the open cross-store gates in store that
// wait on the bead blockerID. A caller about to delete a work bead asks it
// first: deleting a bead an open gate names makes that gate's blocker
// unresolvable, which holds its drain items forever (the delete guard's half
// of the contract; the guard lives with the delete, not here).
func OpenCrossStoreGatesNaming(store beads.Store, blockerID string) ([]beads.Bead, error) {
	if store == nil || strings.TrimSpace(blockerID) == "" {
		return nil, nil
	}
	return store.List(beads.ListQuery{
		Label:    CrossStoreGateLabel,
		Status:   "open",
		Metadata: map[string]string{beadmeta.CrossStoreBlockerMetadataKey: strings.TrimSpace(blockerID)},
		Live:     true,
		TierMode: beads.TierBoth,
	})
}

// CrossStoreBlockerResolver reads every blocker in ids from the stores other
// than the gates' own, in ONE batched read per store, and returns what it read.
//
// A store it cannot read does not fail the pass: the resolver marks every id
// that store may hold undecided and names the store in Unreadable, so the pass
// still retires the gates of finished drains and judges every gate whose
// blocker is decided, and reports the rest as held on the stores it names. An
// error fails the whole pass.
type CrossStoreBlockerResolver func(ids []string) (CrossStoreBlockerResolution, error)

// CrossStoreBlockerResolution is what one resolver call read.
type CrossStoreBlockerResolution struct {
	// Copies holds every copy of a blocker the resolver found, by id.
	Copies map[string][]beads.Bead
	// Undecided names the ids a store the resolver did not read MAY hold —
	// one it could not read, or one it chose not to (a suspended or quiescent
	// one). Their gates are left exactly as they are.
	Undecided map[string]bool
	// Unreadable are the stores the resolver could not read, each error naming
	// its store and why.
	Unreadable []error
}

// CrossStoreGateReport is one release pass.
type CrossStoreGateReport struct {
	// Open is how many open gates the pass judged.
	Open int
	// Released are the gates the pass closed, by id.
	Released []string
	// Unresolved are the open gates whose blocker no store holds, as the pass
	// left them: marked unresolved, with the anchor of when each was first
	// found so (a gate whose mark failed to write carries none).
	Unresolved []beads.Bead
	// NewlyUnresolved are the gates this pass marked unresolved (a subset of
	// Unresolved): the transition a caller reports once.
	NewlyUnresolved []beads.Bead
	// Reopened are released gates of a live drain the pass reopened because
	// their blocker opened again, by id.
	Reopened []string
	// Retired are gates whose drain control the pass found closed or gone —
	// open ones it closed and released ones it re-marked — now retired and out
	// of every later pass's listing, by id.
	Retired []string
	// Undecided are the open gates the pass left exactly as they are only
	// because a store that may hold their blocker was not read — no copy it
	// did read holds them (heldUndecided) — by id.
	Undecided []string
	// Unreadable are the stores the resolver could not read this pass, each
	// error naming its store and why. While there are any, the gates in
	// Undecided may be held only by them.
	Unreadable []error
}

// ReleaseCrossStoreGates closes every open cross-store gate in store whose
// blocker is satisfied in every store that holds it, and every gate whose
// drain control is closed or gone. The blockers of all open gates are read in
// one resolver call, deduplicated by blocker.
//
// A pass with no open gate and no released gate reads nothing but the two gate
// listings (open, and released-of-a-live-drain — retired gates leave it). A
// write that fails leaves its gate as it is and is returned with the others;
// the next pass tries again. A store the resolver could not read fails
// nothing: the gates whose blocker it may hold are left as they are, and the
// report names the store (Unreadable) and the gates held only on it
// (Undecided).
func ReleaseCrossStoreGates(store beads.Store, resolve CrossStoreBlockerResolver) (CrossStoreGateReport, error) {
	var report CrossStoreGateReport
	if store == nil || resolve == nil {
		return report, nil
	}
	open, err := store.List(beads.ListQuery{
		Label:    CrossStoreGateLabel,
		Status:   "open",
		Live:     true,
		TierMode: beads.TierBoth,
	})
	if err != nil {
		return report, fmt.Errorf("listing open cross-store gates: %w", err)
	}
	released, err := store.List(beads.ListQuery{
		Label:         CrossStoreGateLabel,
		Status:        "closed",
		Metadata:      map[string]string{beadmeta.CrossStoreGateStateMetadataKey: CrossStoreGateStateReleased},
		IncludeClosed: true,
		Live:          true,
		TierMode:      beads.TierBoth,
	})
	if err != nil {
		return report, fmt.Errorf("listing released cross-store gates: %w", err)
	}
	open = slices.DeleteFunc(open, func(g beads.Bead) bool { return g.Status == "closed" })
	released = slices.DeleteFunc(released, func(g beads.Bead) bool {
		return g.Status != "closed" || g.Metadata[beadmeta.CrossStoreGateStateMetadataKey] != CrossStoreGateStateReleased
	})
	sort.Slice(open, func(i, j int) bool { return open[i].ID < open[j].ID })
	sort.Slice(released, func(i, j int) bool { return released[i].ID < released[j].ID })
	var errs []error
	// An open gate an operator reopened may still carry its closed marker;
	// clear it before judging, so a gate the pass leaves open reads as an
	// ordinary open gate. Each is judged as clearLeftoverGateMarker returns
	// it: a gate that closed under the clearing write is no longer open and is
	// not judged as one (an operator's close is final; a released gate is the
	// released listing's next pass), and one whose marker survived two lost
	// revision checks is judged with it, for a later pass to clear.
	for i, gate := range open {
		live, err := clearLeftoverGateMarker(store, gate)
		if err != nil {
			errs = append(errs, fmt.Errorf("gate %s: %w", gate.ID, err))
		}
		open[i] = live
	}
	open = slices.DeleteFunc(open, func(g beads.Bead) bool { return g.Status == "closed" })
	report.Open = len(open)
	if len(open) == 0 && len(released) == 0 {
		// Nothing gated and nothing released: the two listings are the pass.
		return report, nil
	}
	controls, err := gateControls(store, append(slices.Clone(open), released...))
	if err != nil {
		return report, errors.Join(append(errs, err)...)
	}
	// A released gate whose drain is finished can never matter again: retire
	// it (one write, once), which takes it out of the released listing.
	var reopenable []beads.Bead
	for _, gate := range released {
		if controls[strings.TrimSpace(gate.Metadata[beadmeta.CrossStoreGateOwnerMetadataKey])] == gateControlLive {
			reopenable = append(reopenable, gate)
			continue
		}
		wrote, err := writeGateIfStill(store, gate.ID, func(b beads.Bead) bool {
			return b.Status == "closed" && b.Metadata[beadmeta.CrossStoreGateStateMetadataKey] == CrossStoreGateStateReleased
		}, beads.UpdateOpts{Metadata: map[string]string{
			beadmeta.CrossStoreGateStateMetadataKey:       CrossStoreGateStateRetired,
			beadmeta.CrossStoreGateRetiredFromMetadataKey: CrossStoreGateStateReleased,
		}}, false)
		if err != nil {
			errs = append(errs, fmt.Errorf("gate %s: retiring: %w", gate.ID, err))
			continue
		}
		if wrote {
			report.Retired = append(report.Retired, gate.ID)
		}
	}
	var resolved CrossStoreBlockerResolution
	if blockers := gateBlockerIDs(append(slices.Clone(open), reopenable...)); len(blockers) > 0 {
		resolved, err = resolve(blockers)
		if err != nil {
			return report, errors.Join(append(errs, fmt.Errorf("resolving %d cross-store blocker(s): %w", len(blockers), err))...)
		}
	}
	copies, undecided := resolved.Copies, resolved.Undecided
	report.Unreadable = resolved.Unreadable
	for _, gate := range reopenable {
		// Reopen only on evidence: some store holds a copy of the blocker
		// that does not release it. A blocker that is gone (deleted, purged
		// after closing) or that a store this pass could not read may hold is
		// not evidence.
		blockerID := gateBlocker(gate)
		if blockerID == "" || undecided[blockerID] || len(copies[blockerID]) == 0 || blockerReleased(copies[blockerID]) {
			continue
		}
		wrote, err := reopenCrossStoreGate(store, gate.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("gate %s: %w", gate.ID, err))
			continue
		}
		if wrote {
			report.Reopened = append(report.Reopened, gate.ID)
		}
	}
	for _, gate := range open {
		state, reason, unresolved := judgeCrossStoreGate(gate, controls, copies, undecided)
		switch {
		case unresolved:
			if gate.Metadata[beadmeta.CrossStoreGateStateMetadataKey] != CrossStoreGateStateUnresolved {
				at := crossStoreGateNow().UTC().Format(time.RFC3339)
				if err := store.SetMetadataBatch(gate.ID, map[string]string{
					beadmeta.CrossStoreGateStateMetadataKey:        CrossStoreGateStateUnresolved,
					beadmeta.CrossStoreGateUnresolvedAtMetadataKey: at,
				}); err != nil {
					errs = append(errs, fmt.Errorf("gate %s: recording an unresolved blocker: %w", gate.ID, err))
				} else {
					if gate.Metadata == nil {
						gate.Metadata = map[string]string{}
					}
					gate.Metadata[beadmeta.CrossStoreGateStateMetadataKey] = CrossStoreGateStateUnresolved
					gate.Metadata[beadmeta.CrossStoreGateUnresolvedAtMetadataKey] = at
					report.NewlyUnresolved = append(report.NewlyUnresolved, gate)
				}
			}
			report.Unresolved = append(report.Unresolved, gate)
		case state != "":
			wrote, err := closeCrossStoreGate(store, gate.ID, state, reason)
			if err != nil {
				errs = append(errs, fmt.Errorf("gate %s: %w", gate.ID, err))
				continue
			}
			switch {
			case !wrote:
			case state == CrossStoreGateStateRetired:
				report.Retired = append(report.Retired, gate.ID)
			default:
				report.Released = append(report.Released, gate.ID)
			}
		case heldUndecided(gate, copies, undecided):
			report.Undecided = append(report.Undecided, gate.ID)
		case gate.Metadata[beadmeta.CrossStoreGateStateMetadataKey] == CrossStoreGateStateUnresolved:
			// Held by a copy of its blocker, which is back: the gate is an
			// ordinary open gate again (gc doctor stops recommending an
			// override), and a later disappearance is a new episode with its
			// own anchor. One write, mark and anchor together.
			if _, err := writeGateIfStill(store, gate.ID, func(b beads.Bead) bool {
				return b.Status != "closed" && b.Metadata[beadmeta.CrossStoreGateStateMetadataKey] == CrossStoreGateStateUnresolved
			}, beads.UpdateOpts{Metadata: map[string]string{
				beadmeta.CrossStoreGateStateMetadataKey:        "",
				beadmeta.CrossStoreGateUnresolvedAtMetadataKey: "",
			}}, false); err != nil {
				errs = append(errs, fmt.Errorf("gate %s: clearing a restored blocker's unresolved mark: %w", gate.ID, err))
			}
		}
	}
	return report, errors.Join(errs...)
}

// CrossStoreGatesHeldUndecided judges the open cross-store gates of store as a
// release pass judges them, and writes nothing: it returns the gates a pass
// leaves holding their drain items only because a store that may hold their
// blocker was not read (CrossStoreGateReport.Undecided), and the stores
// resolve could not read. gc doctor reports them.
func CrossStoreGatesHeldUndecided(store beads.Store, open []beads.Bead, resolve CrossStoreBlockerResolver) ([]beads.Bead, []error, error) {
	if store == nil || resolve == nil {
		return nil, nil, nil
	}
	open = slices.DeleteFunc(slices.Clone(open), func(g beads.Bead) bool { return g.Status == "closed" })
	blockers := gateBlockerIDs(open)
	if len(blockers) == 0 {
		return nil, nil, nil
	}
	controls, err := gateControls(store, open)
	if err != nil {
		return nil, nil, err
	}
	resolved, err := resolve(blockers)
	if err != nil {
		return nil, nil, fmt.Errorf("resolving %d cross-store blocker(s): %w", len(blockers), err)
	}
	var held []beads.Bead
	for _, gate := range open {
		state, _, unresolved := judgeCrossStoreGate(gate, controls, resolved.Copies, resolved.Undecided)
		if state == "" && !unresolved && heldUndecided(gate, resolved.Copies, resolved.Undecided) {
			held = append(held, gate)
		}
	}
	return held, resolved.Unreadable, nil
}

// gateBlocker is the bead a gate waits on, external: marker stripped; "" for a
// gate that names no bead.
func gateBlocker(gate beads.Bead) string {
	target, capability := beads.ExternalBlockerTarget(gate.Metadata[beadmeta.CrossStoreBlockerMetadataKey])
	if capability {
		return ""
	}
	return target
}

// gateBlockerIDs is every distinct bead the gates wait on, sorted: what one
// resolver call reads.
func gateBlockerIDs(gates []beads.Bead) []string {
	seen := make(map[string]bool, len(gates))
	var ids []string
	for _, gate := range gates {
		if id := gateBlocker(gate); id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// gateControlState is what a pass knows about a gate's drain control.
type gateControlState int

const (
	gateControlLive gateControlState = iota
	gateControlDone
)

// gateControls reads the drain control of every gate, one read per distinct
// control in the gates' own store (the binding, which is local).
func gateControls(store beads.Store, gates []beads.Bead) (map[string]gateControlState, error) {
	out := make(map[string]gateControlState)
	var ids []string
	for _, gate := range gates {
		owner := strings.TrimSpace(gate.Metadata[beadmeta.CrossStoreGateOwnerMetadataKey])
		if owner == "" {
			continue
		}
		if _, seen := out[owner]; seen {
			continue
		}
		out[owner] = gateControlDone
		ids = append(ids, owner)
	}
	found, err := beads.GetMany(store, ids)
	if err != nil {
		return nil, fmt.Errorf("reading the drain controls of cross-store gates: %w", err)
	}
	for _, id := range ids {
		if control, ok := found[id]; ok && !convoycore.IsTerminalStatus(control.Status) {
			out[id] = gateControlLive
		}
	}
	return out, nil
}

// judgeCrossStoreGate decides one open gate: close it into state (retired
// when its drain control is closed or gone, released when its blocker
// released) and why, report its blocker unresolved, or leave it as it is
// (state "").
func judgeCrossStoreGate(gate beads.Bead, controls map[string]gateControlState, copies map[string][]beads.Bead, undecided map[string]bool) (state, reason string, unresolved bool) {
	if owner := strings.TrimSpace(gate.Metadata[beadmeta.CrossStoreGateOwnerMetadataKey]); owner != "" {
		if controls[owner] == gateControlDone {
			return CrossStoreGateStateRetired, fmt.Sprintf("drain control %s is closed or gone", owner), false
		}
	}
	blockerID := gateBlocker(gate)
	if blockerID == "" {
		// A gate that names no bead guards nothing.
		return CrossStoreGateStateReleased, "the gate names no blocker", false
	}
	if undecided[blockerID] {
		return "", "", false
	}
	held := copies[blockerID]
	if len(held) == 0 {
		return "", "", true
	}
	if !blockerReleased(held) {
		return "", "", false
	}
	return CrossStoreGateStateReleased, fmt.Sprintf("blocker %s closed", blockerID), false
}

// heldUndecided reports whether an open gate the pass leaves as it is holds
// its item only because its blocker is undecided: a store that may hold the
// blocker was not read, and no copy the pass did read holds the gate (every
// one it read releases it, or it read none). A gate an open copy holds would
// hold whatever the unread store says.
func heldUndecided(gate beads.Bead, copies map[string][]beads.Bead, undecided map[string]bool) bool {
	blockerID := gateBlocker(gate)
	if blockerID == "" || !undecided[blockerID] {
		return false
	}
	held := copies[blockerID]
	return len(held) == 0 || blockerReleased(held)
}

// blockerReleased reports whether a blocker held as copies releases its
// dependents: held somewhere, and closed — not closed blocked — in EVERY store
// that holds it.
func blockerReleased(copies []beads.Bead) bool {
	if len(copies) == 0 {
		return false
	}
	for _, b := range copies {
		if !beads.DependencySatisfied(b.Status, beads.ReadinessWorkOutcome(b.Metadata)) {
			return false
		}
	}
	return true
}

// now is the clock gate timestamps read; a variable so a test can pin it.
var crossStoreGateNow = time.Now

// PinCrossStoreGateClockForTest makes the release pass stamp gate times from
// now, so a caller's test can observe the persisted first-unresolved anchor;
// the returned func restores the real clock. Tests only.
func PinCrossStoreGateClockForTest(now func() time.Time) (restore func()) {
	prev := crossStoreGateNow
	crossStoreGateNow = now
	return func() { crossStoreGateNow = prev }
}
