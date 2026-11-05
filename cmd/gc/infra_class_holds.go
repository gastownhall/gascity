package main

// Holds the copy must carry (ga-1hncv0).
//
// The copy re-adds an infrastructure bead's edges to other infrastructure beads
// and nothing else, so two things that keep a bead from being ready in the work
// store do not reach the binding on their own:
//
//   - a blocking edge onto an unfinished WORK bead. A dependency row can only
//     name an id its own store holds, and the work bead stays in the work
//     store. Copied as a bare edge, SQLite would strand the bead forever and
//     bd would read the edge as external and never block.
//   - bd's indefinite deferral (status=deferred with no defer_until, or one
//     already past that bd's sweep has not lifted). Gas City reads it as
//     IndefinitelyDeferred, which no class binding can record.
//   - any other status bd parks a bead with: blocked (the sanctioned "I cannot
//     proceed" mark), pinned, hooked, or a custom status. A bd-backed read folds
//     each to "open" and keeps the raw status beside it (Bead.SourceStatus);
//     Gas City's demand reads and gc hook honor the raw status, so the bead is
//     parked in the work store, while the binding stores the folded "open".
//
// Either way the bead an operator or agent parked arrives open and ready in the
// binding, and is dispatched. The rehearsals found the first two on every
// unsplit city.
//
// A `blocks` edge onto unfinished work crosses as a cross-store gate in the
// binding (internal/dispatch cross_store_gate.go), exactly as the rig importer
// and the drain carry the same hold: the bead waits on the gate through an
// ordinary same-store edge every readiness reader honors, and the controller's
// gate lane releases the gate once the work bead closes. The holds a gate
// cannot encode — another blocking type, an undated deferral, or a parking
// status — are refused by id, before anything is written, with what to do about
// each, and preflight names the same refusal so the operator fixes it before
// taking the window. A deferral with a future defer_until crosses as that date,
// which the binding honors.

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
)

// infraCopyGate is one cross-store gate the copy mints in the binding: Owner
// waits on it until Blocker, a work bead, is satisfied. ID is the gate's id
// once minted.
type infraCopyGate struct {
	Owner   beads.Bead
	Blocker string
	ID      string
}

// infraUncarriedHolds is the migration's refusal for holds the binding cannot
// express. Each entry names the bead, the hold, and the fix.
type infraUncarriedHolds struct {
	Holds []string
}

func (e *infraUncarriedHolds) Error() string {
	return fmt.Sprintf("%d infrastructure bead(s) are held in the work store by something the binding cannot express, so the copy would make them ready; nothing was copied. Resolve each, then run this again:\n  %s",
		len(e.Holds), strings.Join(e.Holds, "\n  "))
}

// planInfraCopyHolds decides, for every open row of the copy's plan, how each
// hold on it crosses: the gates to mint, or a refusal naming every hold that
// cannot cross. It reads the edges the plan already listed; blockers are read
// by id in work, the store the blocking work beads stay in. It reads only.
//
// A blocking edge onto another row of the copy is the copy's own edge and
// needs nothing. One onto an "external:" reference, or onto an id the work
// store does not hold, is left as the copy has always left it: it holds
// nothing the work store can say is unfinished.
func planInfraCopyHolds(plan infraCopyPlan, work beads.Store, now time.Time) ([]infraCopyGate, error) {
	var gates []infraCopyGate
	var uncarried []string
	for _, b := range plan.rows {
		if b.Status == "closed" {
			continue
		}
		if why := infraParkedStatusRefusal(b, now); why != "" {
			uncarried = append(uncarried, why)
		}
		for _, d := range plan.down[b.ID] {
			to := d.DependsOnID
			if plan.infraIDs[to] || strings.HasPrefix(to, "external:") || !beads.IsReadyBlockingDependencyType(d.Type) {
				continue
			}
			blocker, err := work.Get(to)
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("reading %s, which %s waits on (%s): %w", to, b.ID, d.Type, err)
			}
			if beads.DependencySatisfied(blocker.Status, beads.ReadinessWorkOutcome(blocker.Metadata)) {
				continue
			}
			if d.Type != "blocks" {
				uncarried = append(uncarried, fmt.Sprintf("%s waits (%s) on unfinished work %s, and a cross-store gate encodes only blocks: finish %s, or close %s, or replace the edge with a blocks edge", b.ID, d.Type, to, to, b.ID))
				continue
			}
			gates = append(gates, infraCopyGate{Owner: b, Blocker: to})
		}
	}
	if len(uncarried) > 0 {
		return nil, &infraUncarriedHolds{Holds: uncarried}
	}
	return gates, nil
}

// infraParkedStatusRefusal names the hold bd's raw status puts on b when the
// binding cannot carry it, or returns "". The binding stores Gas City's three
// statuses only, so a bead bd parks by status reaches it as the "open" a
// bd-backed read folds the status to, and is ready there. A deferral is carried
// only as a future defer_until; deferred is read from either signal a store
// reports it by (IndefinitelyDeferred, or the raw status alone).
func infraParkedStatusRefusal(b beads.Bead, now time.Time) string {
	if b.IndefinitelyDeferred || b.SourceStatus == "deferred" {
		if b.DeferUntil != nil && b.DeferUntil.After(now) {
			return ""
		}
		return fmt.Sprintf("%s is deferred in the work store with no future defer_until, which the binding cannot hold: give it a dated deferral (bd defer %s --until <date>), which the copy carries, or undefer it (bd undefer %s)", b.ID, b.ID, b.ID)
	}
	switch b.SourceStatus {
	case "", "open", "in_progress", "closed":
		return ""
	}
	return fmt.Sprintf("%s has bd status %q in the work store, which parks it there and which the binding cannot hold (it stores open, in_progress and closed), so it would arrive ready: keep it parked with a dated deferral (bd defer %s --until <date>), which the copy carries, release it (bd update %s --status open), or close it", b.ID, b.SourceStatus, b.ID, b.ID)
}

// mintInfraCopyGates creates each planned gate in the binding, stamped as this
// migration's so an interrupted attempt's gates are cleared with its rows, and
// holds its owner on it. It returns the gates with their ids.
func mintInfraCopyGates(destination beads.Store, gates []infraCopyGate) ([]infraCopyGate, error) {
	minted := make([]infraCopyGate, 0, len(gates))
	for _, g := range gates {
		gate := dispatch.NewCrossStoreGate(beads.GraphStore{Store: destination}, g.Owner, g.Blocker)
		gate.Metadata[infraMigrationStampKey] = config.StorageWorkBinding
		if scope := strings.TrimSpace(g.Owner.Metadata[beadmeta.RootStoreRefMetadataKey]); scope != "" {
			gate.Metadata[beadmeta.RootStoreRefMetadataKey] = scope
		}
		created, err := destination.Create(gate)
		if err != nil {
			return minted, fmt.Errorf("minting the cross-store gate for %s on %s: %w", g.Owner.ID, g.Blocker, err)
		}
		if err := destination.DepAdd(g.Owner.ID, created.ID, "blocks"); err != nil {
			return minted, fmt.Errorf("holding %s on its cross-store gate %s: %w", g.Owner.ID, created.ID, err)
		}
		g.ID = created.ID
		minted = append(minted, g)
	}
	return minted, nil
}

// verifyInfraCopyGate proves one minted gate against the row the reopened
// binding holds under its id (the zero bead when it holds none): an open gate
// naming its owner and blocker.
func verifyInfraCopyGate(got beads.Bead, g infraCopyGate) error {
	switch {
	case got.ID != g.ID:
		return fmt.Errorf("cross-store gate %s for %s missing from binding", g.ID, g.Owner.ID)
	case got.Status != "open":
		return fmt.Errorf("cross-store gate %s for %s is %s in the binding, want open", g.ID, g.Owner.ID, got.Status)
	case !hasLabel(got.Labels, dispatch.CrossStoreGateLabel):
		return fmt.Errorf("cross-store gate %s for %s does not carry %s", g.ID, g.Owner.ID, dispatch.CrossStoreGateLabel)
	case got.Metadata[beadmeta.CrossStoreGateOwnerMetadataKey] != g.Owner.ID || got.Metadata[beadmeta.CrossStoreBlockerMetadataKey] != g.Blocker:
		return fmt.Errorf("cross-store gate %s names owner %q and blocker %q, want %s waiting on %s", g.ID, got.Metadata[beadmeta.CrossStoreGateOwnerMetadataKey], got.Metadata[beadmeta.CrossStoreBlockerMetadataKey], g.Owner.ID, g.Blocker)
	}
	return nil
}

// infraCopyGatesByOwner indexes minted gate ids by the bead each holds.
func infraCopyGatesByOwner(gates []infraCopyGate) map[string]map[string]bool {
	byOwner := make(map[string]map[string]bool, len(gates))
	for _, g := range gates {
		if byOwner[g.Owner.ID] == nil {
			byOwner[g.Owner.ID] = map[string]bool{}
		}
		byOwner[g.Owner.ID][g.ID] = true
	}
	return byOwner
}

// withoutInfraCopyGateEdges returns deps without the edges onto the owner's
// gates, failing when one of them is missing or is not a blocks edge.
func withoutInfraCopyGateEdges(owner string, deps []beads.Dep, gateIDs map[string]bool) ([]beads.Dep, error) {
	if len(gateIDs) == 0 {
		return deps, nil
	}
	held := map[string]bool{}
	kept := make([]beads.Dep, 0, len(deps))
	for _, d := range deps {
		if gateIDs[d.DependsOnID] {
			if d.Type != "blocks" {
				return nil, fmt.Errorf("dep %s -> %s onto its cross-store gate crossed as %q, want blocks", owner, d.DependsOnID, d.Type)
			}
			held[d.DependsOnID] = true
			continue
		}
		kept = append(kept, d)
	}
	for _, id := range sortedMapKeys(gateIDs) {
		if !held[id] {
			return nil, fmt.Errorf("dep %s -> %s onto its cross-store gate missing from binding", owner, id)
		}
	}
	return kept, nil
}
