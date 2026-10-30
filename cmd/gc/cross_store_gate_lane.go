package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/storeref"
)

// The cross-store gate lane: the controller half of how a drain item waits for
// a blocker held in another store (internal/dispatch cross_store_gate.go).
//
// A drain on a split city cannot write an edge from its graph-resident item
// workflow to a work-resident blocker, so it holds the item behind a GATE in the
// graph binding that mirrors the blocker. This lane closes a gate once its
// blocker is closed in every work store that holds it (and closes the gates of
// a drain control that is closed or gone), which makes the item ready through
// an ordinary same-store edge every readiness reader already honors.
//
// # Off the tick
//
// The lane reads work stores, and the tick never reads the work ledger
// (ga-l7jdg; TestSteadyTickLedgerRoundTripBudgetIsZero). So it runs on its own
// goroutine, the shape of the route-recovery backstop poller, once per
// crossStoreGateInterval.
//
// # Cost
//
// A city that relocates nothing never mints a gate, and the loop does not even
// start reading: it checks the routes and returns. On a split city a pass reads
// the open gates from the binding (one local query) and, only when there are
// some, every distinct blocker in ONE batched read per work store. A store that
// is suspended or quiescent is not read, and neither is any store while the
// city is quiescent.
//
// # A store the pass could not read
//
// A blocker a store the pass did not read may hold is UNDECIDED: its gate stays
// exactly as it is — still holding its item, not released and not marked
// unresolved — until a pass can read every store that may hold it. The hold is
// never silent when a read FAILED: every pass names the store and its error on
// stderr with the count of gates it holds, and `gc doctor` (cross-store-gates)
// names each gate held only because of it. A suspended rig is not read by the
// operator's own choice, and is not reported.

// crossStoreGateInterval bounds how often the lane runs on a split city. It is
// the latency between a work blocker closing and its drain item becoming ready.
const crossStoreGateInterval = 30 * time.Second

// runCrossStoreGateLoop polls the gate lane off the tick until ctx ends.
func (cr *CityRuntime) runCrossStoreGateLoop(ctx context.Context) {
	ticker := time.NewTicker(crossStoreGateInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		cr.safeTick(func() { cr.releaseCrossStoreGates() }, "cross-store-gates")
	}
}

// releaseCrossStoreGates runs one release pass and reports it.
func (cr *CityRuntime) releaseCrossStoreGates() dispatch.CrossStoreGateReport {
	// The config a reload last published, read under its lock: this lane runs
	// off the controller goroutine, which writes cr.cfg on reload.
	cfg := cr.serviceConfigSnapshot()
	city := cr.cityBeadStore()
	graph := resolveGraphStore(cr.storageRoutes, city, cfg, cr.cityPath, cr.rec)
	if graph == nil || city == nil || graph == city {
		// Nothing relocated: no drain ever needed a gate.
		return dispatch.CrossStoreGateReport{}
	}
	if cr.beadsQuiescent.Load() || cr.cs.storesQuiescent() {
		// A quiescent city's stores are not touched; the gates wait for it.
		return dispatch.CrossStoreGateReport{}
	}
	report, err := dispatch.ReleaseCrossStoreGates(graph, crossStoreBlockerResolver(graph, city, cfg, cr.rigBeadStores(), buildSuspendedRigPathsForCity(cfg, cr.cityPath))) // residency:allow — the work legs a gate's blocker may live in; see crossStoreBlockerResolver
	if len(report.Released) > 0 {
		fmt.Fprintf(cr.stdout, "Cross-store gates: released %d (%s); their drain items may run\n", len(report.Released), strings.Join(report.Released, " ")) //nolint:errcheck // best-effort stdout
	}
	if len(report.Reopened) > 0 {
		fmt.Fprintf(cr.stdout, "Cross-store gates: reopened %d (%s); their blockers opened again\n", len(report.Reopened), strings.Join(report.Reopened, " ")) //nolint:errcheck // best-effort stdout
	}
	if len(report.Unreadable) > 0 {
		// Every pass, for as long as it lasts: a store that cannot be read
		// holds the gates it may decide, and that hold must not be silent.
		fmt.Fprintf(cr.stderr, "%s: cross-store gates: %d open gate(s) held undecided while a store could not be read (%s); the release pass judges them once it can\n", cr.logPrefix, len(report.Undecided), crossStoreUnreadableText(report.Unreadable)) //nolint:errcheck // best-effort stderr
	}
	cr.recordUnresolvedCrossStoreGates(report)
	if err != nil {
		fmt.Fprintf(cr.stderr, "%s: cross-store gates: %v\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
	}
	return report
}

// recordUnresolvedCrossStoreGates makes gates whose blocker no store holds
// visible: once per drain control per pass that newly found any, as one
// control.stalled event on that control listing EVERY unresolved gate it waits
// on (report.Unresolved), not only the ones new this pass, and on stderr.
// `gc doctor` reports every such gate for as long as it is open
// (cross-store-gates).
//
// The event is the one the control dispatcher emits for a control whose
// pending wait outlived its horizon (emitControlStalled), field for field:
// actor "controller", error_class "pending" (the drain is open and an operator
// can heal it — restore the blocker, or close the gate), first_seen the
// persisted anchor (the earliest gc.cross_store_gate_unresolved_at among the
// control's gates), attempts the count of gates it waits on, the message and
// the run id. A dedicated event type would need a new API schema entry for the
// same operator question — "which control is stuck, since when, on what" —
// that control.stalled already answers on the surface operators triage from.
func (cr *CityRuntime) recordUnresolvedCrossStoreGates(report dispatch.CrossStoreGateReport) {
	announced := make(map[string]bool)
	for _, gate := range report.NewlyUnresolved {
		announced[gate.Metadata[beadmeta.CrossStoreGateOwnerMetadataKey]] = true
	}
	if len(announced) == 0 {
		return
	}
	byControl := make(map[string][]beads.Bead)
	var controls []string
	for _, gate := range report.Unresolved {
		control := gate.Metadata[beadmeta.CrossStoreGateOwnerMetadataKey]
		if !announced[control] {
			continue
		}
		if _, seen := byControl[control]; !seen {
			controls = append(controls, control)
		}
		byControl[control] = append(byControl[control], gate)
	}
	sort.Strings(controls)
	for _, control := range controls {
		group := byControl[control]
		firstSeen := ""
		var parts []string
		for _, gate := range group {
			if at := gate.Metadata[beadmeta.CrossStoreGateUnresolvedAtMetadataKey]; at != "" && (firstSeen == "" || at < firstSeen) {
				firstSeen = at
			}
			parts = append(parts, fmt.Sprintf("%s (blocker %s)", gate.ID, gate.Metadata[beadmeta.CrossStoreBlockerMetadataKey]))
		}
		rootID := group[0].Metadata[beadmeta.RootBeadIDMetadataKey]
		msg := fmt.Sprintf("drain %s waits on cross-store gate(s) %s whose blocker no store this city federates holds; restore the blocker, or close the gate to release its items", control, strings.Join(parts, ", "))
		fmt.Fprintf(cr.stderr, "%s: cross-store gates: %s\n", cr.logPrefix, msg) //nolint:errcheck // best-effort stderr
		if cr.rec == nil {
			continue
		}
		payload := events.ControlStalledPayload{
			BeadID:     control,
			Kind:       "drain",
			RootBeadID: rootID,
			ErrorClass: "pending",
			FirstSeen:  firstSeen,
			Attempts:   len(group),
			Error:      msg,
		}
		cr.rec.Record(events.Event{
			Type:    events.ControlStalled,
			Actor:   "controller",
			Subject: control,
			Message: fmt.Sprintf("control bead %s stalled since %s: %s", control, firstSeen, msg),
			Payload: events.ControlStalledPayloadJSON(payload),
			RunID:   rootID,
		})
	}
}

// crossStoreBlockerLeg is one work store a gate's blocker may live in, by the
// label an operator knows it by ("city", "rig <name>").
type crossStoreBlockerLeg struct {
	label string
	store beads.Store
}

// crossStoreBlockerResolver reads every gate blocker from every work store a
// blocker may live in — the city store, then each serving rig store — in ONE
// batched read per store, and returns every copy it finds. The binding the
// gates live in is skipped: a blocker it held would have been an ordinary edge,
// never a gate. A blocker in a SUSPENDED rig's namespace is undecided (the
// rig's store is not read). A store whose read fails is reported, not
// returned (crossStoreBlockerResolverOver).
func crossStoreBlockerResolver(binding, city beads.Store, cfg *config.City, rigs map[string]beads.Store, suspended map[string]bool) dispatch.CrossStoreBlockerResolver {
	serving := servingRigStores(cfg, rigs, suspended)
	// A configured rig servingRigStores dropped is suspended (or has no store
	// this runtime holds): its store is not read, so its namespace is undecided.
	var suspendedPrefixes []string
	if cfg != nil {
		for _, rig := range cfg.Rigs {
			if _, live := serving[rig.Name]; live {
				continue
			}
			if prefix := strings.ToLower(strings.Trim(rig.EffectivePrefix(), "-")); prefix != "" {
				suspendedPrefixes = append(suspendedPrefixes, prefix)
			}
		}
	}
	// Every work store is a leg, not just the first owner: a gate releases only
	// once its blocker is closed in EVERY work store that holds it.
	legs := []crossStoreBlockerLeg{{label: "city", store: city}}
	names := make([]string, 0, len(serving))
	for name := range serving {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		leg := serving[name]
		if leg == nil || leg == binding || leg == city {
			continue
		}
		dup := false
		for _, have := range legs {
			if have.store == leg {
				dup = true
			}
		}
		if !dup {
			legs = append(legs, crossStoreBlockerLeg{label: "rig " + name, store: leg})
		}
	}
	return crossStoreBlockerResolverOver(binding, legs, suspendedPrefixes)
}

// crossStoreBlockerResolverOver reads every blocker from legs in ONE batched
// read per leg, skipping the gates' own binding, and returns every copy it
// finds. An id in one of the unread namespaces (a store that is not read) is
// undecided. A leg whose read fails might hold an open copy of any blocker, so
// it makes every blocker undecided and is named in Unreadable with its error:
// the pass holds those gates as they are, and its caller says why.
func crossStoreBlockerResolverOver(binding beads.Store, legs []crossStoreBlockerLeg, unreadNamespaces []string) dispatch.CrossStoreBlockerResolver {
	return func(ids []string) (dispatch.CrossStoreBlockerResolution, error) {
		resolved := dispatch.CrossStoreBlockerResolution{
			Copies:    make(map[string][]beads.Bead, len(ids)),
			Undecided: make(map[string]bool),
		}
		for _, id := range ids {
			for _, prefix := range unreadNamespaces {
				// Whole-namespace match, so a hyphenated rig prefix such as
				// "gc-ops" owns "gc-ops-12" and not "gc-12".
				if storeref.IDInNamespace(strings.ToLower(id), prefix) { // residency:allow — decides only whether an unread store might hold id; reads nothing
					resolved.Undecided[id] = true
				}
			}
		}
		for _, leg := range legs {
			if leg.store == nil || leg.store == binding {
				continue
			}
			found, err := beads.GetMany(leg.store, ids)
			if err != nil {
				for _, id := range ids {
					resolved.Undecided[id] = true
				}
				resolved.Unreadable = append(resolved.Unreadable, fmt.Errorf("%s: reading %d cross-store blocker(s): %w", leg.label, len(ids), err))
				continue
			}
			for _, id := range ids {
				if b, ok := found[id]; ok {
					resolved.Copies[id] = append(resolved.Copies[id], b)
				}
			}
		}
		return resolved, nil
	}
}

// crossStoreUnreadableText lists the stores a pass could not read for an
// operator's line, each error already naming its store and why.
func crossStoreUnreadableText(unreadable []error) string {
	parts := make([]string, 0, len(unreadable))
	for _, err := range unreadable {
		parts = append(parts, err.Error())
	}
	return strings.Join(parts, "; ")
}
