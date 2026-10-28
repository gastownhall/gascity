package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/storeref"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// Doctor checks for the split-city cross-store blocker rule (internal/readywork
// crossstore.go, internal/dispatch cross_store_gate.go). Both report nothing on
// a city with no class binding: there every store keeps its own ready answer.

// doctorScope is one store a cross-store check reads, by its operator label.
type doctorScope struct {
	label string
	store beads.Store
}

// splitCityDoctorScopes returns the stores a split city's cross-store checks
// read — the city work store, every rig store that is not suspended and the
// class binding — or nil when the city relocates nothing.
//
// A rig is suspended by the same rule the rest of doctor uses (the runtime
// suspension state, then the rig's suspended / suspended_on_start fields). A
// suspended rig, and a rig whose store would not open, is not read; its id
// namespace is returned in unread so a caller treats every id in it as
// UNDECIDED rather than absent. skipped names the stores that would not open.
func splitCityDoctorScopes(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) (scopes []doctorScope, unread []string, skipped []string) {
	graph, relocated := cliStorageRoutes(cityPath).storeFor(coordclass.ClassGraph) // residency:allow — a doctor scan of every store a split city's blockers may live in
	if !relocated || graph == nil || newStore == nil {
		return nil, nil, nil
	}
	if store, err := newStore(cityPath); err != nil {
		skipped = append(skipped, fmt.Sprintf("city skipped: opening bead store: %v", err))
	} else {
		scopes = append(scopes, doctorScope{label: "city", store: store})
	}
	if cfg != nil {
		suspState, _ := loadSuspensionState(fsys.OSFS{}, cityPath)
		for i := range cfg.Rigs {
			rig := cfg.Rigs[i]
			prefix := strings.ToLower(strings.Trim(strings.TrimSpace(rig.EffectivePrefix()), "-"))
			if suspensionstate.EffectiveRigSuspended(suspState, rig.Name, rig.EffectiveSuspendedOnStart()) || strings.TrimSpace(rig.Path) == "" {
				if prefix != "" {
					unread = append(unread, prefix)
				}
				continue
			}
			store, err := newStore(rig.Path)
			if err != nil {
				skipped = append(skipped, fmt.Sprintf("rig %s skipped: opening bead store: %v", rig.Name, err))
				if prefix != "" {
					unread = append(unread, prefix)
				}
				continue
			}
			scopes = append(scopes, doctorScope{label: "rig " + rig.Name, store: store})
		}
	}
	scopes = append(scopes, doctorScope{label: "graph binding", store: graph})
	return scopes, unread, skipped
}

// inUnreadNamespace reports whether id is in one of the unread rigs' id
// namespaces. Namespaces are matched whole (storeref.IDInNamespace), so a
// hyphenated rig prefix such as "gc-ops" owns "gc-ops-12" and not "gc-12".
func inUnreadNamespace(id string, unread []string) bool {
	for _, prefix := range unread {
		if storeref.IDInNamespace(strings.ToLower(id), prefix) { // residency:allow — decides only whether an unread rig might hold id; reads nothing
			return true
		}
	}
	return false
}

// crossStoreGatesCheck reports the open cross-store gates whose blocker no
// store holds (gc.cross_store_gate_state=unresolved). Each holds a drain's
// items forever: an unresolvable blocker blocks. The operator decides — close
// the gate to release the items, or restore the blocker.
type crossStoreGatesCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
}

func newCrossStoreGatesCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *crossStoreGatesCheck {
	return &crossStoreGatesCheck{cfg: cfg, cityPath: cityPath, newStore: newStore}
}

func (c *crossStoreGatesCheck) Name() string         { return "cross-store-gates" }
func (c *crossStoreGatesCheck) CanFix() bool         { return false }
func (c *crossStoreGatesCheck) WarmupEligible() bool { return false }
func (c *crossStoreGatesCheck) Fix(*doctor.CheckContext) error {
	return nil
}

func (c *crossStoreGatesCheck) Run(*doctor.CheckContext) *doctor.CheckResult {
	graph, relocated := cliStorageRoutes(c.cityPath).storeFor(coordclass.ClassGraph) // residency:allow — the gates live in the binding, by construction
	if !relocated || graph == nil {
		return okCheck(c.Name(), "no class binding; drains need no cross-store gates")
	}
	gates, err := graph.List(beads.ListQuery{
		Label:    dispatch.CrossStoreGateLabel,
		Status:   "open",
		Metadata: map[string]string{beadmeta.CrossStoreGateStateMetadataKey: dispatch.CrossStoreGateStateUnresolved},
		Live:     true,
		TierMode: beads.TierBoth,
	})
	if err != nil {
		return warnCheck(c.Name(), "could not list cross-store gates", "fix the class binding, then rerun gc doctor", []string{err.Error()})
	}
	if len(gates) == 0 {
		return okCheck(c.Name(), "no cross-store gate waits on a blocker no store holds")
	}
	details := make([]string, 0, len(gates))
	for _, gate := range gates {
		details = append(details, fmt.Sprintf("gate %s (drain %s) waits on %s, which no store holds", gate.ID, gate.Metadata[beadmeta.CrossStoreGateOwnerMetadataKey], gate.Metadata[beadmeta.CrossStoreBlockerMetadataKey]))
	}
	sort.Strings(details)
	return warnCheck(c.Name(),
		fmt.Sprintf("%d cross-store gate(s) hold drain items on a blocker no store holds", len(gates)),
		"restore the blocker, or close the gate (gc bd close <gate>) to release its drain items",
		details)
}

// danglingBlockingEdgesCheck reports open and in-progress rows whose blocking
// edge names a bead no store holds. On a split city gc ready holds such a row
// forever (an unresolvable blocker blocks), where bd on a single-store city
// would have served it.
//
// --fix acts only on what it proved, and never while any store could not be
// read:
//
//   - A blocking edge to a malformed "<type>:<id>" literal — the
//     maintainer-city shape is "discovered-from:<id>", a typed edge written as
//     a blocks target — is REWRITTEN as the real <type> edge to <id> when the
//     row's own store holds <id>. When only another store holds <id> it is
//     reported for the operator and left alone: a rewrite there would be a
//     cross-store edge, which gc never writes.
//   - Any other blocking edge whose target no store holds is removed from the
//     row's own store. Nothing is written in its place.
//   - A target in the namespace of a suspended rig, or of a rig whose store
//     would not open, is undecided: never reported, never touched.
type danglingBlockingEdgesCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
	// scopes are the stores the last collect read; Fix re-asks them whether a
	// target is still absent right before it removes an edge to it.
	scopes []doctorScope
}

func newDanglingBlockingEdgesCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *danglingBlockingEdgesCheck {
	return &danglingBlockingEdgesCheck{cfg: cfg, cityPath: cityPath, newStore: newStore}
}

func (c *danglingBlockingEdgesCheck) Name() string         { return "dangling-blocking-edges" }
func (c *danglingBlockingEdgesCheck) CanFix() bool         { return true }
func (c *danglingBlockingEdgesCheck) WarmupEligible() bool { return false }

// danglingAction is what --fix does with a dangling edge.
type danglingAction int

const (
	// danglingRemove: no store holds the target; the edge is removed.
	danglingRemove danglingAction = iota
	// danglingRewrite: a "<type>:<id>" literal whose <id> the row's own store
	// holds; the edge is rewritten as the real <type> edge.
	danglingRewrite
	// danglingOperator: a "<type>:<id>" literal whose <id> only another store
	// holds; left for the operator.
	danglingOperator
)

// danglingEdge is one blocking edge no store resolves as written.
type danglingEdge struct {
	label   string
	store   beads.Store
	rowID   string
	target  string // the edge's DependsOnID as written
	action  danglingAction
	depType string // danglingRewrite / danglingOperator: the literal's type
	realID  string // danglingRewrite / danglingOperator: the literal's id
}

// typedDepLiteralTypes are the edge types a malformed "<type>:<id>" blocks
// target is read as. Only non-blocking types: rewriting must never make a
// row wait on something it did not wait on.
var typedDepLiteralTypes = map[string]bool{"discovered-from": true, "related": true}

// typedDepLiteral splits a "<type>:<id>" target for a known non-blocking type.
func typedDepLiteral(target string) (depType, id string, ok bool) {
	depType, id, found := strings.Cut(target, ":")
	if !found || !typedDepLiteralTypes[depType] || strings.TrimSpace(id) == "" || strings.Contains(id, ":") {
		return "", "", false
	}
	return depType, strings.TrimSpace(id), true
}

// collect reads, per store, the open and in-progress rows (two lists), their
// edges (one batched read) and then every distinct target from every store
// (one batched read per store).
func (c *danglingBlockingEdgesCheck) collect() (edges []danglingEdge, skipped []string) {
	scopes, unread, skipped := splitCityDoctorScopes(c.cfg, c.cityPath, c.newStore)
	c.scopes = scopes
	if len(scopes) == 0 {
		return nil, skipped
	}
	type candidate struct {
		scope  doctorScope
		rowID  string
		raw    string
		target string
	}
	var candidates []candidate
	targetSet := make(map[string]bool)
	for _, sc := range scopes {
		var rows []beads.Bead
		failed := false
		for _, status := range []string{"open", "in_progress"} {
			got, err := sc.store.List(beads.ListQuery{Status: status, AllowScan: true, Live: true, TierMode: beads.TierBoth})
			if err != nil {
				skipped = append(skipped, fmt.Sprintf("%s skipped: listing %s rows: %v", sc.label, status, err))
				failed = true
				break
			}
			rows = append(rows, got...)
		}
		if failed || len(rows) == 0 {
			continue
		}
		ids := make([]string, 0, len(rows))
		for _, b := range rows {
			ids = append(ids, b.ID)
		}
		deps, err := doctorDepsFor(sc.store, ids)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: listing dependencies: %v", sc.label, err))
			continue
		}
		for _, id := range ids {
			for _, dep := range deps[id] {
				if !beads.IsReadyBlockingDependencyType(dep.Type) {
					continue
				}
				target, capability := beads.ExternalBlockerTarget(dep.DependsOnID)
				if capability || target == "" {
					continue
				}
				candidates = append(candidates, candidate{scope: sc, rowID: id, raw: dep.DependsOnID, target: target})
				targetSet[target] = true
				if _, realID, ok := typedDepLiteral(target); ok {
					targetSet[realID] = true
				}
			}
		}
	}
	if len(candidates) == 0 {
		return nil, skipped
	}
	targets := make([]string, 0, len(targetSet))
	for t := range targetSet {
		targets = append(targets, t)
	}
	sort.Strings(targets)
	heldBy := make(map[string]map[beads.Store]bool, len(targets))
	for _, sc := range scopes {
		found, err := beads.GetMany(sc.store, targets)
		if err != nil {
			// A store that cannot be read may hold any target: report nothing
			// rather than call a held blocker dangling.
			return nil, append(skipped, fmt.Sprintf("%s: reading blockers: %v (no edge reported dangling while a store is unreadable)", sc.label, err))
		}
		for id := range found {
			if heldBy[id] == nil {
				heldBy[id] = map[beads.Store]bool{}
			}
			heldBy[id][sc.store] = true
		}
	}
	for _, cand := range candidates {
		if len(heldBy[cand.target]) > 0 || inUnreadNamespace(cand.target, unread) {
			continue
		}
		edge := danglingEdge{label: cand.scope.label, store: cand.scope.store, rowID: cand.rowID, target: cand.raw, action: danglingRemove}
		if depType, realID, ok := typedDepLiteral(cand.target); ok {
			edge.depType, edge.realID = depType, realID
			switch {
			case heldBy[realID][cand.scope.store]:
				edge.action = danglingRewrite
			case len(heldBy[realID]) > 0 || inUnreadNamespace(realID, unread):
				edge.action = danglingOperator
			}
		}
		edges = append(edges, edge)
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].rowID != edges[j].rowID {
			return edges[i].rowID < edges[j].rowID
		}
		return edges[i].target < edges[j].target
	})
	return edges, skipped
}

// doctorDepsFor reads ids' down edges in one batched read where the store has
// one, otherwise one DepList per row.
func doctorDepsFor(store beads.Store, ids []string) (map[string][]beads.Dep, error) {
	if batch, ok := beads.DepListBatchFor(store); ok {
		if deps, err := batch.DepListBatch(ids); err == nil {
			return deps, nil
		}
	}
	out := make(map[string][]beads.Dep, len(ids))
	for _, id := range ids {
		deps, err := store.DepList(id, "down")
		if err != nil {
			return nil, err
		}
		out[id] = deps
	}
	return out, nil
}

func (e danglingEdge) describe() string {
	switch e.action {
	case danglingRewrite:
		return fmt.Sprintf("%s row %s blocks on the malformed literal %q; --fix rewrites it as a %s edge to %s", e.label, e.rowID, e.target, e.depType, e.realID)
	case danglingOperator:
		return fmt.Sprintf("%s row %s blocks on the malformed literal %q; %s is held by another store, so it is left for the operator (gc never writes a cross-store edge)", e.label, e.rowID, e.target, e.realID)
	default:
		return fmt.Sprintf("%s row %s blocks on %s, which no store holds; --fix removes the edge", e.label, e.rowID, e.target)
	}
}

func (c *danglingBlockingEdgesCheck) Run(*doctor.CheckContext) *doctor.CheckResult {
	edges, skipped := c.collect()
	if len(edges) == 0 && len(skipped) == 0 {
		return okCheck(c.Name(), "no open row is held by a blocking edge no store resolves")
	}
	details := make([]string, 0, len(edges)+len(skipped))
	for _, e := range edges {
		details = append(details, e.describe())
	}
	details = append(details, skipped...)
	if len(edges) == 0 {
		return warnCheck(c.Name(), fmt.Sprintf("dangling-edge scan skipped %d scope(s)", len(skipped)), "fix bead store access, then rerun gc doctor", details)
	}
	hint := "run gc doctor --fix to rewrite or remove the dangling edges (no cross-store edge is written)"
	if len(skipped) > 0 {
		hint = "fix bead store access first: --fix acts on nothing while a store could not be read"
	}
	return warnCheck(c.Name(),
		fmt.Sprintf("%d blocking edge(s) name a bead no store holds; gc ready holds their rows forever on a split city", len(edges)),
		hint, details)
}

func (c *danglingBlockingEdgesCheck) Fix(*doctor.CheckContext) error {
	edges, skipped := c.collect()
	if len(skipped) > 0 {
		// A store that was not read may hold a target this scan called
		// dangling: act on nothing.
		return fmt.Errorf("dangling-blocking-edges: not fixing while %d scope(s) could not be read: %s", len(skipped), strings.Join(skipped, "; "))
	}
	var errs []error
	for _, e := range edges {
		if e.action == danglingOperator {
			continue
		}
		if err := fixDanglingEdge(e, c.stillAbsent); err != nil {
			// One row's failure does not abandon the others.
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// stillAbsent re-asks every store the scan read whether id is held now.
func (c *danglingBlockingEdgesCheck) stillAbsent(id string) (bool, error) {
	for _, sc := range c.scopes {
		found, err := beads.GetMany(sc.store, []string{id})
		if err != nil {
			return false, fmt.Errorf("%s: re-reading %s: %w", sc.label, id, err)
		}
		if len(found) > 0 {
			return false, nil
		}
	}
	return true, nil
}

// fixDanglingEdge applies one edge's fix, re-reading right before it writes
// (an agent may have changed things since the scan): the row's edges, and —
// for a removal — that no store holds the target now, or — for a rewrite —
// that the row's own store still holds the real id. A premise that moved
// leaves the edge for the next run.
//
// A rewrite never retypes an edge the row already has: on SQLite and the
// in-memory stores adding an edge to a target the row already points at
// RETYPES that edge in place, which would turn a real blocks edge into a
// discovered-from one and let the row stop waiting on an open blocker (and on
// bd the add fails). So when the row already has any edge to the real id, the
// literal is only removed. Otherwise the typed edge is ADDED FIRST — carrying
// the literal edge's payload where the store can say what it is — and the
// literal is removed only once the add succeeded, so a failed add loses
// nothing. (A creator is not carried: the store interface does not expose it.)
func fixDanglingEdge(e danglingEdge, stillAbsent func(string) (bool, error)) error {
	deps, err := e.store.DepList(e.rowID, "down")
	if err != nil {
		return fmt.Errorf("%s row %s: re-reading edges before the fix: %w", e.label, e.rowID, err)
	}
	literalPresent, realPresent := false, false
	for _, d := range deps {
		switch d.DependsOnID {
		case e.target:
			literalPresent = literalPresent || beads.IsReadyBlockingDependencyType(d.Type)
		case e.realID:
			if e.realID != "" {
				realPresent = true
			}
		}
	}
	if !literalPresent {
		return nil
	}
	action := e.action
	switch action {
	case danglingRewrite:
		if _, err := beads.HandlesFor(e.store).Live.Get(e.realID); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				return nil
			}
			return fmt.Errorf("%s row %s: re-reading %s before the rewrite: %w", e.label, e.rowID, e.realID, err)
		}
	default:
		target, _ := beads.ExternalBlockerTarget(e.target)
		if stillAbsent != nil {
			absent, err := stillAbsent(target)
			if err != nil {
				return fmt.Errorf("%s row %s: %w", e.label, e.rowID, err)
			}
			if !absent {
				return nil
			}
		}
		if e.realID != "" {
			// A typed literal whose bead no store held at the scan: if the
			// row's own store holds it now, rewrite rather than remove; if
			// another store does, it is the operator's (no cross-store edge).
			_, err := beads.HandlesFor(e.store).Live.Get(e.realID)
			switch {
			case err == nil:
				action = danglingRewrite
			case !errors.Is(err, beads.ErrNotFound):
				return fmt.Errorf("%s row %s: re-reading %s before the removal: %w", e.label, e.rowID, e.realID, err)
			case stillAbsent != nil:
				absent, err := stillAbsent(e.realID)
				if err != nil {
					return fmt.Errorf("%s row %s: %w", e.label, e.rowID, err)
				}
				if !absent {
					return nil
				}
			}
		}
	}
	if action == danglingRewrite && !realPresent {
		if err := addTypedEdgeLike(e); err != nil {
			return fmt.Errorf("%s row %s: writing the %s edge to %s (the literal %q is kept): %w", e.label, e.rowID, e.depType, e.realID, e.target, err)
		}
	}
	if err := e.store.DepRemove(e.rowID, e.target); err != nil {
		return fmt.Errorf("%s row %s: removing edge %q: %w", e.label, e.rowID, e.target, err)
	}
	return nil
}

// addTypedEdgeLike writes the rewrite's typed edge, with the literal edge's
// payload when the store can read and write one.
func addTypedEdgeLike(e danglingEdge) error {
	reader, canRead := e.store.(beads.DepMetadataReader)
	writer, canWrite := e.store.(beads.DepMetadataWriter)
	if canRead && canWrite {
		if payload, carries, err := reader.DepMetadata(e.rowID, e.target); err == nil && carries {
			return writer.DepAddWithMetadata(e.rowID, e.realID, e.depType, payload)
		}
	}
	return e.store.DepAdd(e.rowID, e.realID, e.depType)
}
