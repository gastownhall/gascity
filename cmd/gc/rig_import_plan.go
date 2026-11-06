package main

// The rig importer's plan (S4b): which rows of one rig store move into the
// city binding, which edges move with them, and which shapes refuse.
//
// On a split city graph and infrastructure beads live in the city's binding,
// at every scope; a rig store holds only work. Rigs written before that rule
// (S4a) still hold graph rows, and `gc storage migrate --from-rigs` moves them.
// Nothing here writes: planRigImport is a pure function of one rig's rows and
// edges, the rig's manifest, and what the binding holds under the same ids, so
// `gc storage preflight` runs exactly the plan the import runs.
//
// # The edge policy
//
// No cross-store edge is ever written. The edges are planned by the shared
// crossBoundaryPlan (cross_boundary_plan.go), which the city copy runs too.
//
// Each refusal names the beads and the fix.

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// rigImportStamp is the gc.infra_migrated_from value a rig import stamps on
// every row and gate it writes into the binding.
func rigImportStamp(rig string) string { return "rig:" + rig }

// rigScopeRef is the gc.root_store_ref value of rig's scope.
func rigScopeRef(rig string) string { return rigGraphScope(rig).rootStoreRef("") }

// rigSnapshot is one read of a rig store: every row of both tiers, closed
// included, and the DOWN edges of every row.
type rigSnapshot struct {
	rows []beads.Bead
	byID map[string]beads.Bead
	down map[string][]beads.Dep
}

// readRigSnapshot lists the rig store once and reads every row's edges in one
// batched read where the store offers one.
func readRigSnapshot(store beads.Store) (rigSnapshot, error) {
	rows, err := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return rigSnapshot{}, fmt.Errorf("listing the rig store: %w", err)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	snap := rigSnapshot{rows: rows, byID: make(map[string]beads.Bead, len(rows)), down: make(map[string][]beads.Dep, len(rows))}
	ids := make([]string, 0, len(rows))
	for _, b := range rows {
		snap.byID[b.ID] = b
		ids = append(ids, b.ID)
	}
	if len(ids) == 0 {
		return snap, nil
	}
	if batch, ok := beads.DepListBatchFor(store); ok {
		edges, err := batch.DepListBatch(ids)
		switch {
		case err == nil:
			for _, id := range ids {
				snap.down[id] = edges[id]
			}
			return snap, nil
		case !isDepListBatchUnsupported(err):
			return rigSnapshot{}, fmt.Errorf("reading the rig store's edges: %w", err)
		}
	}
	for _, id := range ids {
		deps, err := store.DepList(id, "down")
		if err != nil {
			return rigSnapshot{}, fmt.Errorf("listing deps of %s: %w", id, err)
		}
		snap.down[id] = deps
	}
	return snap, nil
}

func isDepListBatchUnsupported(err error) bool {
	return errors.Is(err, beads.ErrDepListBatchUnsupported)
}

// rigImportGate is one S3b gate the import mints: Owner (a moved row) waits
// on Blocker (open rig work) through a same-store gate in the binding.
type rigImportGate struct {
	Owner   beads.Bead
	Blocker string
}

// rigCrossEdgeRecord is one boundary edge the import did not copy, as it is
// recorded on the moved row (gc.migrated_cross_edges).
type rigCrossEdgeRecord struct {
	// Dir is "out" when the moved row depends on Other, "in" when Other
	// depends on the moved row.
	Dir         string `json:"dir"`
	Type        string `json:"type"`
	Other       string `json:"other"`
	OtherStatus string `json:"other_status_at_import,omitempty"`
	Payload     string `json:"payload,omitempty"`
	// Action is what the import did: dropped, gated (an S3b gate encodes it)
	// or copied-external (an external: reference, copied as it is).
	Action string `json:"action"`
}

const (
	rigEdgeDropped  = "dropped"
	rigEdgeGated    = "gated"
	rigEdgeExternal = "copied-external"
	rigEdgeCopied   = "copied"
)

// rigImportRefusal is one shape the import cannot move, by bead, with the fix.
type rigImportRefusal struct {
	IDs    []string
	Reason string
	Remedy string
}

func (r rigImportRefusal) String() string {
	return fmt.Sprintf("%s: %s. Fix: %s", strings.Join(r.IDs, ", "), r.Reason, r.Remedy)
}

// rigImportPlan is what one import of one rig does.
type rigImportPlan struct {
	Rig        string
	Generation coordclass.Generation
	// Infra is every infrastructure row the rig holds.
	Infra int
	// Move are the rows to copy into the binding, sorted by id.
	Move []beads.Bead
	// Retained are rows the rig's manifest records as carried that the rig
	// still holds: the clear removes them.
	Retained []beads.Bead
	// Shared are rows the binding already holds field-equal under another
	// import's stamp (a rig on the city's database): neither copied nor
	// cleared here.
	Shared []string
	// Copy are the edges copied, both ends in the binding after the import.
	Copy []beads.Dep
	// Gates are the S3b gates minted for open blocks edges onto rig work.
	Gates []rigImportGate
	// Records are the boundary edges not copied, keyed by moved row.
	Records map[string][]rigCrossEdgeRecord
	// RetainedRecords are the boundary edges an earlier run of the import did
	// not copy, keyed by retained row, as that run recorded them on the
	// binding copy (gc.migrated_cross_edges): the clear's backup marks those
	// edges with what was done with them, not as copied.
	RetainedRecords map[string][]rigCrossEdgeRecord
	// Annotated names the moved rows stamped with gc.root_store_ref because
	// they carried none.
	Annotated []string
	// OpenRouted counts moved rows that are open and routed: they become
	// visible to demand in the binding.
	OpenRouted int
	// Open counts moved rows that are open.
	Open     int
	Refusals []rigImportRefusal
}

// rigRowOpen reports whether b is open for the edge policy: anything not
// closed.
func rigRowOpen(b beads.Bead) bool { return b.Status != "closed" }

// rigRowSatisfied reports whether a blocker releases its dependents.
func rigRowSatisfied(b beads.Bead) bool {
	return beads.DependencySatisfied(b.Status, beads.ReadinessWorkOutcome(b.Metadata))
}

func rigMembershipEdge(depType string) bool {
	return depType == "tracks" || depType == "parent-child"
}

// rigPlanContext is everything planRigImport reads. Nothing in it writes.
type rigPlanContext struct {
	Rig      string
	CityName string
	Snap     rigSnapshot
	// Manifest is the rig's manifest when its import marker exists (nil
	// before the first import completes: an interrupted attempt is re-copied,
	// never trusted).
	Manifest *infraCopyManifest
	// Held is what the binding holds under every id the plan asks about
	// (bindingIDsToProbe).
	Held map[string]beads.Bead
	// Elsewhere holds every copy, in a store other than the rig and the
	// binding (the city work store, the other rigs), of a blocker an open
	// moved row waits on that neither the rig nor the binding holds.
	Elsewhere map[string][]beads.Bead
	// ElsewhereUnread names the stores that could not be read for those
	// blockers: a blocker none of the read stores holds may be in one.
	ElsewhereUnread []string
	// ForeignClaimant reports an assignee this city could not have minted
	// and holds no session record for (#7895's rule): an open row it claims
	// belongs to another city sharing the rig store.
	ForeignClaimant func(assignee string) bool
}

// planRigImport plans one rig's import. A row whose scope names another city
// or another rig, or that another city's agent claims, refuses.
func planRigImport(ctx rigPlanContext) rigImportPlan {
	rig, cityName, snap, manifest, held := ctx.Rig, ctx.CityName, ctx.Snap, ctx.Manifest, ctx.Held
	plan := rigImportPlan{Rig: rig, Generation: coordclass.CurrentGeneration, RetainedRecords: map[string][]rigCrossEdgeRecord{}}
	if manifest != nil {
		plan.Generation = manifest.Generation
	}
	stamp := rigImportStamp(rig)
	moves := infraClassMoves(plan.Generation)
	moved := map[string]bool{}
	inBinding := map[string]bool{} // rows that are (or will be) binding rows
	var collisions, reused, foreign, claimed, unheld, unreadable []string
	for _, b := range snap.rows {
		if !moves(b) {
			continue
		}
		plan.Infra++
		h, holds := held[b.ID]
		if manifest != nil && manifest.Carried(b) {
			switch {
			case !holds:
				// The rig holds the only copy. It is never read as absent
				// (an edge onto it recorded as onto a bead no store holds).
				unheld = append(unheld, b.ID)
				continue
			case h.Metadata[infraMigrationStampKey] != stamp:
				collisions = append(collisions, b.ID)
				continue
			}
			if raw := strings.TrimSpace(h.Metadata[beadmeta.MigratedCrossEdgesMetadataKey]); raw != "" {
				var recs []rigCrossEdgeRecord
				if err := json.Unmarshal([]byte(raw), &recs); err != nil {
					unreadable = append(unreadable, fmt.Sprintf("%s (%v)", b.ID, err))
					continue
				}
				plan.RetainedRecords[b.ID] = recs
			}
			plan.Retained = append(plan.Retained, b)
			inBinding[b.ID] = true
			continue
		}
		if ref := strings.TrimSpace(b.Metadata[beadmeta.RootStoreRefMetadataKey]); rigScopeIsForeign(ref, rig, cityName) {
			foreign = append(foreign, fmt.Sprintf("%s (%s)", b.ID, ref))
			continue
		}
		if a := strings.TrimSpace(b.Assignee); a != "" && rigRowOpen(b) && ctx.ForeignClaimant != nil && ctx.ForeignClaimant(a) {
			claimed = append(claimed, fmt.Sprintf("%s (claimed by %s)", b.ID, a))
			continue
		}
		if holds {
			switch {
			case h.Metadata[infraMigrationStampKey] == stamp && (manifest == nil || !manifest.IDs[b.ID]):
				// An interrupted earlier attempt's copy: prepare deletes it.
			case h.Metadata[infraMigrationStampKey] == stamp:
				// The manifest records this id for a different bead: the
				// rig minted it again after the import.
				reused = append(reused, b.ID)
				continue
			case h.Metadata[infraMigrationStampKey] != "" && beadCopyDifference(b, h) == "":
				plan.Shared = append(plan.Shared, b.ID)
				inBinding[b.ID] = true
				continue
			default:
				collisions = append(collisions, b.ID)
				continue
			}
		}
		plan.Move = append(plan.Move, b)
		moved[b.ID] = true
		inBinding[b.ID] = true
		if rigRowOpen(b) {
			plan.Open++
			if strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]) != "" {
				plan.OpenRouted++
			}
		}
		if strings.TrimSpace(b.Metadata[beadmeta.RootStoreRefMetadataKey]) == "" {
			plan.Annotated = append(plan.Annotated, b.ID)
		}
	}
	if len(unheld) > 0 {
		sort.Strings(unheld)
		plan.Refusals = append(plan.Refusals, rigImportRefusal{
			IDs:    unheld,
			Reason: "the rig's manifest records each of these rows as carried into the city binding, but the binding no longer holds it, so the rig row may be the only copy",
			Remedy: "restore the binding database from its backup, or, if the binding removed a row on purpose since the import, remove the rig row too, then re-run",
		})
	}
	if len(unreadable) > 0 {
		sort.Strings(unreadable)
		plan.Refusals = append(plan.Refusals, rigImportRefusal{
			IDs:    unreadable,
			Reason: "the binding copy of each of these rows carries a gc.migrated_cross_edges record that does not parse, so the clear's backup cannot say what the import did with the row's boundary edges",
			Remedy: "restore each binding row's gc.migrated_cross_edges from the binding database's backup, then re-run",
		})
	}
	if len(collisions) > 0 {
		sort.Strings(collisions)
		plan.Refusals = append(plan.Refusals, rigImportRefusal{
			IDs:    collisions,
			Reason: "the city binding already holds each of these ids as a different bead",
			Remedy: "compare each binding row with the rig row (`gc bd show <id>` reads the binding first) and remove the one that is not wanted, then re-run",
		})
	}
	if len(reused) > 0 {
		sort.Strings(reused)
		plan.Refusals = append(plan.Refusals, rigImportRefusal{
			IDs:    reused,
			Reason: "the rig minted each of these ids again after an earlier import moved the bead that first held it",
			Remedy: "re-create each rig row under a new id (the import raised the rig's minimum id length so this does not recur), then re-run",
		})
	}
	if len(foreign) > 0 {
		sort.Strings(foreign)
		plan.Refusals = append(plan.Refusals, rigImportRefusal{
			IDs:    foreign,
			Reason: "these rows' gc.root_store_ref names another city or another rig, so they are not this rig's to move: the store is shared (or holds another rig's rows)",
			Remedy: "give this rig a store of its own, or move the rows to the scope that owns them, then re-run; a shared store is refused until the single-writer attestation exists",
		})
	}
	if len(claimed) > 0 {
		sort.Strings(claimed)
		plan.Refusals = append(plan.Refusals, rigImportRefusal{
			IDs:    claimed,
			Reason: "another city's agent holds these open rows (this city could not have minted the claimant and holds no session record for it), so the rig store is shared",
			Remedy: "let the other city finish or release them, or give this rig a store of its own, then re-run",
		})
	}

	boundary := crossBoundaryPlan(crossBoundaryInput{
		Snap:            snap,
		Moves:           moves,
		Moved:           moved,
		InBinding:       inBinding,
		Held:            held,
		Elsewhere:       ctx.Elsewhere,
		ElsewhereUnread: ctx.ElsewhereUnread,
		WorkNoun:        "rig work",
		SourceNoun:      "the rig",
	})
	plan.Copy, plan.Gates, plan.Records = boundary.Copy, boundary.Gates, boundary.Records
	plan.Refusals = append(plan.Refusals, boundary.Refusals...)
	sort.Strings(plan.Annotated)
	return plan
}

// rigScopeIsForeign reports whether a row's gc.root_store_ref names a scope
// other than this rig's: another city, or another rig.
func rigScopeIsForeign(ref, rig, cityName string) bool {
	switch {
	case ref == "":
		return false
	case strings.HasPrefix(ref, "rig:"):
		return ref != rigScopeRef(rig)
	case strings.HasPrefix(ref, "city:"):
		return cityName != "" && ref != "city:"+cityName
	default:
		return false
	}
}

// elsewhereCandidates names the blockers the plan must look for outside the
// rig and the binding: targets of an open infrastructure row's blocking or
// membership edge that neither holds.
func elsewhereCandidates(snap rigSnapshot, gen coordclass.Generation, held map[string]beads.Bead) []string {
	want := map[string]bool{}
	moves := infraClassMoves(gen)
	for _, b := range snap.rows {
		if !rigRowOpen(b) || !moves(b) {
			continue
		}
		for _, d := range snap.down[b.ID] {
			to := d.DependsOnID
			if !beads.IsReadyBlockingDependencyType(d.Type) && !rigMembershipEdge(d.Type) {
				continue
			}
			if _, ok := snap.byID[to]; ok || strings.HasPrefix(to, "external:") {
				continue
			}
			if _, ok := held[to]; ok {
				continue
			}
			want[to] = true
		}
	}
	return sortedMapKeys(want)
}

// bindingIDsToProbe is every id the plan asks the binding about: the rig's
// infrastructure rows, and every edge endpoint the rig does not hold.
func bindingIDsToProbe(snap rigSnapshot, gen coordclass.Generation) []string {
	want := map[string]bool{}
	moves := infraClassMoves(gen)
	for _, b := range snap.rows {
		if moves(b) {
			want[b.ID] = true
		}
		for _, d := range snap.down[b.ID] {
			if _, ok := snap.byID[d.DependsOnID]; !ok && !strings.HasPrefix(d.DependsOnID, "external:") {
				want[d.DependsOnID] = true
			}
		}
	}
	return sortedMapKeys(want)
}

// rigImportRow returns the form of b the import creates in the binding: the
// import stamp, the scope when b carries none, the record of the boundary
// edges not copied, and the create-time dependency shorthands stripped (as
// infraMigrationRow strips them).
func rigImportRow(b beads.Bead, rig string, records []rigCrossEdgeRecord) beads.Bead {
	metadata := make(beads.StringMap, len(b.Metadata)+3)
	for key, value := range b.Metadata {
		metadata[key] = value
	}
	metadata[infraMigrationStampKey] = rigImportStamp(rig)
	if strings.TrimSpace(metadata[beadmeta.RootStoreRefMetadataKey]) == "" {
		metadata[beadmeta.RootStoreRefMetadataKey] = rigScopeRef(rig)
	}
	if len(records) > 0 {
		if raw, err := json.Marshal(records); err == nil {
			metadata[beadmeta.MigratedCrossEdgesMetadataKey] = string(raw)
		}
	}
	b.Metadata = metadata
	b.Dependencies = nil
	b.Needs = nil
	b.IsBlocked = nil
	return b
}

// rigImportHashFloor is the bd min_hash_length the rig needs once the
// imported ids leave it: one longer than the longest hash suffix among them,
// wisp ids included (bd mints wisp suffixes under the same length bounds), so
// a short id the rig mints later cannot re-mint an imported one (the
// mc-wisp-jkl id-reuse defect, moved to rigs); at bd's cap of 8 (below) that
// holds only probabilistically. Zero means no id needs it.
//
// Only a suffix bd could have minted counts (bdHashShaped): a cross-store
// gate's "xsg-<12 hex>" or a hand-named id is not one bd will mint again.
// The floor never passes bdMaxMintedHashLength, the longest suffix bd mints:
// bd's CLI and native minting ignore a higher one, and a hosted `bd serve`
// pads every id it mints to it with zeros.
func rigImportHashFloor(ids []string) int {
	longest := 0
	for _, id := range ids {
		suffix := rigIDHashSuffix(id)
		if !bdHashShaped(suffix) {
			continue
		}
		if n := len(suffix); n > longest {
			longest = n
		}
	}
	if longest == 0 {
		return 0
	}
	return min(longest+1, bdMaxMintedHashLength)
}

// bdMaxMintedHashLength is the longest hash suffix bd mints, and so the
// highest id floor worth setting.
const bdMaxMintedHashLength = 8

// bdHashShaped reports whether suffix is one bd mints: base36, 3 to
// bdMaxMintedHashLength characters.
func bdHashShaped(suffix string) bool {
	if len(suffix) < 3 || len(suffix) > bdMaxMintedHashLength {
		return false
	}
	for _, r := range suffix {
		if (r < '0' || r > '9') && (r < 'a' || r > 'z') {
			return false
		}
	}
	return true
}

// rigIDHashSuffix returns the hash segment of a bd id ("gp-k0nvi" -> "k0nvi",
// "gp-wisp-4m43" -> "4m43", "gp-k0nvi.2" -> "k0nvi"), or "" for an id with no
// hash segment.
func rigIDHashSuffix(id string) string {
	cut := strings.LastIndex(id, "-")
	if cut < 0 || cut == len(id)-1 {
		return ""
	}
	suffix := id[cut+1:]
	if dot := strings.Index(suffix, "."); dot >= 0 {
		suffix = suffix[:dot]
	}
	return suffix
}

// rigPlanIDs returns the ids of rows, in order.
func rigPlanIDs(rows []beads.Bead) []string {
	out := make([]string, 0, len(rows))
	for _, b := range rows {
		out = append(out, b.ID)
	}
	return out
}

// censusEdges renders a row's dependency edges as set members, so two rows'
// edges compare without regard to the order a store lists them in.
func censusEdges(deps []beads.Dep) []string {
	edges := make([]string, 0, len(deps))
	for _, dep := range deps {
		edges = append(edges, fmt.Sprintf("%s (%s)", dep.DependsOnID, dep.Type))
	}
	return edges
}
