package main

// The stranded-write repair: how the infrastructure beads a converged city's
// proven copy never carried get into the binding that is already serving.
//
// This is the recovery half of the hazard infra_class_migrate.go documents
// under "Writers, and the stranded-write window". A write that lands in the
// retained work store after the equality stage is absent from the binding, the
// per-boot containment re-check names it, and the boot refuses. The refusal
// used to tell the operator to recover the named beads into the binding's
// database and then stop — there was no command that did it, and the alarm
// named no verb. This is that command.
//
// # Why this is not `gc storage migrate` run twice
//
// The migration is one-shot BY DESIGN, and the gate is a safety property rather
// than an accident of sequencing. Two independent holds make re-running it
// wrong on a converged city:
//
//   - runInfraClassMigration reads the convergence state first and hands a
//     marked city straight to confirmInfraConvergence. It never re-copies.
//   - If the marker were removed to force the copy, prepareInfraDestination
//     would either refuse (any destination row without the migration's own
//     provenance stamp is content the migration did not write) or, on a
//     destination it did write, DELETE every stamped row and re-import from a
//     source that no longer holds them. The second arm is the loss, and it has
//     been measured rather than imagined: on the city this command was built
//     for, the binding held 51,127 rows and the retained source held 1,452, so
//     forcing a re-copy would have replaced the former with the latter. The
//     retained source's infrastructure slice shrinks over a city's life —
//     the whole point of the retained-source design is that the binding, not
//     the source, is authoritative after cutover.
//
// And the equality stage cannot pass on a live city at all: verifyInfraCopy
// demands the destination hold EXACTLY the source's infrastructure slice, while
// a serving binding legitimately grows beads the source never had.
//
// So this does not defeat that gate, and nothing here should be refactored into
// it. It is a scoped repair built from the same primitives, and everything it
// moves is additive: it copies only ids the binding does not hold and the
// manifest does not record, it never deletes a destination row, and it never
// writes to the source. The one exception is an operator's per-id answer for a
// work-store row whose id the binding holds for a different bead, which removes
// one of the two rows after backing it up (infra_class_recover_held.go).
//
// # What it will not guess at
//
// The gap it repairs is classified by exactly the rule confirmInfraConvergence
// uses, against exactly the manifest that check reads, using coordclass as the
// classification authority — because a repair that disagreed with the guard
// would move beads and leave the boot refusing anyway.
//
// A bead whose class this cannot state is reported rather than moved: an
// infrastructure classification outside the classes the split relocates, a row
// whose class would change in the crossing, or a row whose dependency topology
// the source cannot state, is a bead nobody can say belongs in this binding in
// the shape it would arrive in. Those are named and the command exits non-zero.
//
// # Copy before delete, verify before record
//
// Nothing is deleted, from either side, except by an operator's per-id choice
// for a reused id (backed up and proven first). The source keeps its rows
// verbatim, as it does after the migration, so a bad outcome here is a
// duplicate rather than a loss. The manifest — the record every later boot
// classifies absence against — is extended only after every moved bead has
// been re-read from a CLOSED AND REOPENED destination and proven field-,
// class- and dep-equal to the source, by the migration's own comparators.
//
// # The topology is repaired over the whole resident set, not over the copy
//
// An edge belongs to a PAIR of beads, and the two endpoints of a within-infra
// edge need not arrive in the binding on the same run: the dispatcher's normal
// wiring direction is a pre-existing bead blocking on a just-created one, so a
// bead that crossed at cutover routinely gains an edge into one written after
// it. So the edge pass runs over every source infrastructure bead the binding
// holds once the copy finishes — inbound and outbound, this run's rows and the
// cutover's alike — exactly as importInfraSnapshot runs its dep pass over every
// row rather than over the ones it happened to create. Only edges the binding
// lacks are written, so the pass is a diff rather than a rewrite and its count
// is a statement: "0 restored" is what a converged city reports.
//
// An edge that cannot be written is NAMED, per edge, and never reported as
// something else. There are three ways to not write one and they are three
// different facts: the far endpoint is a work bead (the destination owns no row
// for it, exactly as the migration leaves it); the far endpoint is a bead the
// manifest records and the binding's own GC has since collected (re-adding it
// would dangle a reference to a deliberate delete); or the far endpoint is an
// infrastructure bead THIS RUN REFUSED TO MOVE, which is a within-infra edge
// this repair is dropping, and is reported as a refusal rather than counted as
// linkage.
//
// # Idempotence, and what a partial failure leaves
//
// Re-running is a no-op AND a repair: the gap is recomputed from live state, a
// bead already in the binding is not re-copied, every missing within-infra edge
// between resident beads is re-driven, and the manifest is rewritten as a
// superset.
//
// A partial failure — ENOSPC, a read-only binding root, a kill — can leave rows
// in the binding that the manifest does not record, because the rows are written
// row by row and the manifest is published once at the end. Those ids are NOT
// still stranded: the binding holds them, so no containment check names them,
// and the earlier claim that the residue was self-healing was false for exactly
// the ids that got copied. Left unrecorded, the day the binding's own GC
// collects one it reads back as a strand and the city refuses to boot over a row
// the repair had correctly delivered.
//
// So the residue is a bucket of its own: every source infrastructure bead the
// binding holds that the manifest does not record is re-proven against the
// reopened destination and folded into the manifest. The proof is identity —
// the same id, created in the same second (infraHeldRowIsThisBead) — because
// the binding has served the row since and may have changed it. One that
// cannot be proven — a row under that id that is another bead — is named and
// left unrecorded, because the manifest's meaning is "the copy was proven to
// deliver this id" and recording an unproven one would be a proof nobody made.
//
// This is the asymmetry with the migration, stated so it is not re-introduced:
// an interrupted runInfraClassMigration self-heals, because the manifest is
// written before the marker and the next run's prepareInfraDestination drops the
// stamped rows and re-imports. This repair writes into an already-converged
// binding it must never clear, so its resume is recognition rather than
// rollback.
//
// # The rig census is deliberately not repeated here
//
// doStorageMigrate refuses a copy while any rig scope holds an infrastructure
// bead, because the copy's source is the city work store and a rig-scoped row
// would be silently omitted from a cutover that then declares itself proven. This
// repair makes no such declaration: it extends a manifest with ids it copied
// out of the city work store and asserts nothing about any other scope, and the
// containment re-check it feeds reads the same one store. Refusing on a rig
// stray would block the repair of a real strand over a bead that is outside
// both the repair's reach and the check's.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// infraRecoverableClasses is the closed set of classes this repair will move,
// derived from the migration's own class list so the two cannot drift.
//
// It is keyed by coordclass.Class rather than by config.StorageClass because
// coordclass is what decides which store a bead belongs in — it is the
// authority readInfraSnapshot selects the gap with and the boot guard refuses
// on. A repair that classified by any other rule could move a bead the guard
// still counts as stranded, and the city would keep refusing to boot.
func infraRecoverableClasses() map[coordclass.Class]bool {
	byName := make(map[string]bool, len(infraMigrationClasses))
	for _, class := range infraMigrationClasses {
		byName[string(class)] = true
	}
	allowed := make(map[coordclass.Class]bool, len(infraMigrationClasses))
	for _, class := range coordclass.Classes() {
		if byName[class.String()] {
			allowed[class] = true
		}
	}
	return allowed
}

// newStorageRecoverCmd is the leaf the stranded refusal names.
func newStorageRecoverCmd(surface storageCommandSurface, stdout, stderr io.Writer) *cobra.Command {
	var (
		fromWork     bool
		fleetStopped bool
		dryRun       bool
		dumpPath     string
		moveIDs      []string
		carriedIDs   []string
		replaceIDs   []string
		dropIDs      []string
	)
	cmd := &cobra.Command{
		Use:          surface.Verb,
		Short:        "Copy stranded infrastructure beads from the work store into the converged binding",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		Long: `Copy the infrastructure beads a converged city's proven copy never carried
out of the retained work store and into the binding that is already serving.

This is the recovery the stranded-write refusal names. Unless you name a row
with --replace-binding-row or --drop-work-row (below), it is additive: it
moves only ids the binding does not hold and the proven-copy manifest does not
record, it deletes nothing from either store, and it extends the manifest only
after every moved bead has been proven equal against a closed and reopened
destination. A bead whose class it cannot state is named and left where it is.

It refuses on a city that has NOT converged — the whole copy is still owed
there, and ` + "`" + storageMigrationCommand + "`" + ` is what owes it. It is not that
command run twice: the migration is one-shot on purpose, and forcing it to
re-copy would re-import a serving binding from a source that no longer holds
what the binding does.

The recovered beads stay in the work store until the migration is run again,
which clears them exactly as it clears the cutover's own copies.

A manifest written by an older build may list a work-store row by id alone,
with nothing that says whether it is the bead the copy carried: no time
bounds the entry, the row carries no created_at, or the row is younger than
the manifest, which an older recover-stranded extended with the id of every
row it carried. When the binding no longer holds such a row, a closed one is
moved. One that is not closed is refused and shown (status, type,
timestamps, title): nothing here can tell whether the binding collected the
bead the copy carried or never received it. Decide per id with --move <id>
(copy it in as it is) or --record-carried <id> (record it as carried; the
clear then deletes its work-store row, and the clear's backup keeps it).
Both also accept closed rows. --dry-run and --dump show these rows without
writing, and naming an already-resolved id again is a no-op.

A work-store row can also reuse an id the binding still holds for a
different bead: the work store minted the id again after the copy carried
the original. No copy can carry it under that id, so it is named and left
unrecorded until you decide per id: --replace-binding-row <id> (the
work-store row replaces the binding's) or --drop-work-row <id> (the
work-store row is removed and the binding's stays). These are the only
choices that remove a row, and each first backs up the row it removes, with
every edge touching it, to infra.recover-choices.jsonl at the binding root.
--dry-run shows both rows of each such id.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !fromWork {
				fmt.Fprintf(stderr, "gc %s %s: pass --%s. The source is stated explicitly rather than detected, exactly as the migration states it\n", //nolint:errcheck // best-effort stderr
					surface.Namespace, surface.Verb, surface.Flag)
				return errExit
			}
			request, err := resolveStorageOperatorRequest()
			if err != nil {
				fmt.Fprintf(stderr, "gc %s %s: %v\n", surface.Namespace, surface.Verb, err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			request.FleetStopped = fleetStopped
			return exitForCode(doStorageRecoverStranded(cmd.Context(), request, strandedRecoveryOptions{
				DryRun: dryRun, DumpPath: dumpPath,
				Move: setOf(moveIDs), RecordCarried: setOf(carriedIDs),
				ReplaceBindingRow: setOf(replaceIDs), DropWorkRow: setOf(dropIDs),
			}, stdout, stderr))
		},
	}
	cmd.Flags().BoolVar(&fromWork, surface.Flag, false,
		"recover the stranded infrastructure beads out of this city's work store")
	cmd.Flags().BoolVar(&fleetStopped, storageFleetStoppedFlag, false,
		"attest that "+storageFleetStoppedAttestation)
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"report the gap and write the dump without touching the binding or the manifest")
	cmd.Flags().StringVar(&dumpPath, "dump", "",
		"write every stranded bead and its source dep edges to this JSON file before any write")
	cmd.Flags().StringSliceVar(&moveIDs, strandedMoveFlag, nil,
		"for a work-store row the manifest lists by id alone and the binding no longer holds: copy it into the binding as it is (required for a row that is not closed; a closed one is moved by default) (repeatable, comma-separated)")
	cmd.Flags().StringSliceVar(&carriedIDs, strandedRecordCarriedFlag, nil,
		"for a work-store row the manifest lists by id alone and the binding no longer holds: record it as the bead the copy carried, so the clear deletes its work-store row (the backup keeps it); accepted for closed rows too (repeatable, comma-separated)")
	cmd.Flags().StringSliceVar(&replaceIDs, strandedReplaceBindingRowFlag, nil,
		"for a work-store row whose id the binding holds for a different bead: back up and remove the binding's row, then copy the work-store row in under the id (repeatable, comma-separated)")
	cmd.Flags().StringSliceVar(&dropIDs, strandedDropWorkRowFlag, nil,
		"for a work-store row whose id the binding holds for a different bead: back up and remove the work-store row, keeping the binding's (repeatable, comma-separated)")
	return cmd
}

// The per-id operator choices for an open row the manifest lists by id alone
// (see doStorageRecoverStranded).
const (
	strandedMoveFlag          = "move"
	strandedRecordCarriedFlag = "record-carried"
)

// strandedRecoveryOptions are the knobs the repair carries.
type strandedRecoveryOptions struct {
	// DryRun reports and dumps without writing to the binding or the manifest.
	DryRun bool
	// DumpPath, when set, receives every stranded bead and its source dep edges
	// as JSON before any write happens.
	DumpPath string
	// Move and RecordCarried are the operator's per-id answers for OPEN
	// work-store rows the manifest lists by id alone, with nothing that proves
	// whether each is the bead the copy carried, and the binding no longer
	// holds: copy it in, or record it as carried (the clear then deletes it).
	// Nothing else may be named.
	Move          map[string]bool
	RecordCarried map[string]bool
	// ReplaceBindingRow and DropWorkRow are the operator's per-id answers for
	// a work-store row whose id the binding holds for a different bead
	// (infra_class_recover_held.go): the work-store row replaces the
	// binding's, or the work-store row is removed. Nothing else may be named.
	ReplaceBindingRow map[string]bool
	DropWorkRow       map[string]bool
}

// strandedBeadDump is one stranded bead and the source dep edges it carries, as
// written to the pre-write dump.
//
// It carries beads.Bead itself rather than a projection of it. The dump is a
// local operator artifact — the input to a repair, captured before the repair
// changes anything — not a wire type, and a projection would be a second
// definition of "what a bead is" that could silently omit the field an
// investigation needed.
type strandedBeadDump struct {
	Bead      beads.Bead  `json:"bead"`
	Class     string      `json:"class"`
	Deps      []beads.Dep `json:"deps"`
	DepSource string      `json:"dep_source"`
	DepError  string      `json:"dep_error,omitempty"`
}

// infraRelationCapabilityRefusal reports whether err is an adapter saying it
// does not implement relation listing AT ALL, as opposed to a store saying this
// particular read did not work just now.
//
// The distinction is the whole safety of the fallback below. `operation
// "IssueRelations" not supported by the postgres backend` is a fact about the
// adapter: it stays true for every bead and every retry, so one probe answers
// for the store. `database is locked`, a dropped connection, a subprocess that
// could not fork are facts about a moment, and a moment is no evidence about a
// capability. Reading the second as the first swaps the relation read for the
// inline projection unconditionally — a projection this reader only trusts once
// it has WITNESSED it live below — and it does so for the whole run, under a
// zero exit code.
//
// Text matching is what is available: the refusal crosses the adapter boundary
// as a string and neither side carries a sentinel for it. It degrades in the
// safe direction — an unrecognized capability refusal is read as a transient
// error, which refuses the bead rather than moving it edge-free. The same idiom,
// for the same reason, is infraGenesisOpenFailure's "already exists".
func infraRelationCapabilityRefusal(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "not supported") || strings.Contains(text, "unsupported")
}

// sourceDepReader answers "what does this bead depend on" against a work store
// that may not implement the relation read at all.
//
// The bd/Postgres backend answers DepList with `operation "IssueRelations" not
// supported by the postgres backend`, and that is a fact about the adapter
// rather than about the city: the same rows are still carried INLINE on every
// bead the store lists, in beads.Bead.Dependencies, because bd's own list JSON
// emits a `dependencies` array beside `dependency_count`. So the reader prefers
// the explicit relation read and falls back to the inline projection — but only
// on that capability refusal, never on an error that could be a moment.
//
// (importInfraSnapshot and verifyInfraCopy both call DepList unconditionally,
// so the MIGRATION cannot run against such a source at all. That is a real gap
// on the migration path and it is tracked as its own bead rather than fixed
// here — this file is the repair, and widening the migration's source contract
// is a change to the one-shot cutover's proof.)
//
// The fallback is only trustworthy if the inline projection is actually LIVE on
// this adapter — an adapter that simply never populated the field would answer
// "no edges" for every bead and the fallback would silently drop the whole
// topology. So it is not assumed: newSourceDepReader witnesses the projection
// against the store's full contents and refuses to fall back at all unless it
// found at least one bead carrying an inline edge. An unwitnessed fallback
// makes every bead ambiguous rather than making every bead look edge-free.
type sourceDepReader struct {
	source beads.Store
	// relationsOK is false once DepList has refused; the reader stops asking.
	relationsOK bool
	// relationsErr is what DepList refused with, kept for the report.
	relationsErr error
	// inlineWitnessed is true when some bead in the source carries an inline
	// dep edge, which is what licenses reading an empty inline slice as "this
	// bead has no edges" rather than as "this adapter reports none".
	inlineWitnessed bool
	// witnessID names the bead that proved the projection, so the claim is
	// falsifiable from the report alone.
	witnessID string
	// batched holds the edges of every anchor the prefetch asked about, keyed by
	// bead. A prefetched anchor with no entry has no edges.
	batched map[string][]beads.Dep
	// prefetched names the anchors a KEPT batch answer covers, which is what
	// makes an absent entry readable as "no edges" rather than as "not asked".
	// A batch that answered nothing is not kept (see prefetch).
	prefetched map[string]bool
	// emptyBatchAnchors counts the anchors of a batch that answered nothing,
	// so the report can say why the walk went one bead at a time.
	emptyBatchAnchors int
}

// prefetch reads every anchor's edges in ONE round trip, so classifying the
// stranded rows and planning the topology walk the whole source without asking
// it again, and the equality stage's second read (see reread) is one more.
//
// THE COST IS THE POINT. This repair reads a bead's source edges three times —
// classifying the stranded rows, planning the topology over every resident bead,
// and verifying against a second independent read — so a per-bead read costs
// roughly three round trips per source bead. Against the retained work store,
// which a hosted city reaches over a WAN link that drops MySQL handshakes, a
// 231-bead city is ~460 chances to fail, each able to burn the store's whole
// read-retry budget before the walk gives up on that bead and refuses it. The
// command whose entire job is to repair a city during an incident then cannot
// finish on the network the incident is happening on (ga-50tsx).
//
// A FAILED PREFETCH FAILS THE RUN — but a store that cannot batch at all does
// not. Those are different answers: a read that was attempted and failed leaves
// the walk unable to tell an edge-free bead from an unreadable one, so
// continuing would hand every bead to the walk as edge-free, and one refusal
// naming the link is what an operator can act on. A store that never had the
// capability is simply the older, slower path, and refusing there would break
// recovery on every store type that cannot batch.
//
// ErrDepListBatchUnsupported is how a WRAPPER reports the second case. It has to
// be handled by value rather than by the type assertion alone, because a wrapper
// forwards the method precisely so the capability is not silently lost — so the
// assertion succeeds and the answer arrives as this sentinel.
//
// The anchors come from the source snapshot the run is copying FROM, so their
// existence is already established by the read that produced them; this pass
// asks only what they depend on. That is why an anchor absent from the answer
// reads as "no edges" rather than as a missing bead: the thin stores (MemStore,
// FileStore) omit edge-free anchors by convention, and a re-read could not be
// more current than the snapshot row being copied anyway.
//
// AN ANSWER WITH NOTHING IN IT IS NOT KEPT. BdStore answers a batch that bd
// refuses as "not found" — one anchor bd cannot resolve is enough — with an
// empty map and no error (a backend that refuses the read as unsupported
// answers ErrDepListBatchUnsupported, handled above), whatever edges the other anchors
// carry, and the verify pass's own batched read would answer the same nothing
// and agree with it. So an empty answer over a non-empty anchor list is set
// aside and every bead is read on its own, exactly as if the store could not
// batch. That degrade is all-or-nothing, so an answer carrying any edge came
// from a read that ran and its absent anchors keep the "no edges" reading
// above. Re-reading per ABSENT anchor instead would make every store that omits
// edge-free anchors re-read each of them one at a time — the per-bead walk this
// pass exists to remove, and on the failing link it was written for, a refused
// bead per edge-free one. The guard's price is narrower: a source with no edges
// at all, on such a store, takes the per-bead walk.
func (r *sourceDepReader) prefetch(anchors []string) error {
	batch, ok := beads.DepListBatchFor(r.source)
	if !ok || len(anchors) == 0 {
		return nil
	}
	edges, err := batch.DepListBatch(anchors)
	if errors.Is(err, beads.ErrDepListBatchUnsupported) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the source's dep edges for %d bead(s) in one batch: %w", len(anchors), err)
	}
	if len(edges) == 0 {
		r.emptyBatchAnchors = len(anchors)
		return nil
	}
	r.batched = edges
	r.prefetched = make(map[string]bool, len(anchors))
	for _, id := range anchors {
		r.prefetched[id] = true
	}
	return nil
}

// sourceAnchorIDs lists every source infrastructure bead as an anchor, not just
// the stranded ones: the topology plan and the verify pass both walk every bead
// the binding will hold, and an anchor left out of the batch is one the walk
// asks for separately.
func sourceAnchorIDs(rows []beads.Bead) []string {
	anchors := make([]string, 0, len(rows))
	for _, b := range rows {
		anchors = append(anchors, b.ID)
	}
	return anchors
}

// reread is the equality stage's SECOND read of the source's edges: a reader
// that keeps this one's verdict on what the store can answer — which leg it
// reads, and what witnessed the inline projection — and none of its answers.
//
// A proof read off the answer the plan was built from cannot fail: an edge that
// answer lost is one the plan never wrote and the proof never asks for. So the
// relation leg asks again, in one more batch where the first read was one (see
// prefetch, including when an answer is set aside), and a failed batch fails
// the run for the reason it fails the walk. It is a second answer through the
// same read, so it catches an edge a read lost rather than one the read can
// never see; re-reading one bead at a time would catch both, at the per-bead
// cost prefetch exists to remove. The inline leg has nothing to ask again: its
// edges are the snapshot row's own projection.
func (r *sourceDepReader) reread(anchors []string) (*sourceDepReader, error) {
	fresh := &sourceDepReader{
		source:          r.source,
		relationsOK:     r.relationsOK,
		relationsErr:    r.relationsErr,
		inlineWitnessed: r.inlineWitnessed,
		witnessID:       r.witnessID,
	}
	if !fresh.relationsOK {
		return fresh, nil
	}
	if err := fresh.prefetch(anchors); err != nil {
		return nil, err
	}
	return fresh, nil
}

// newSourceDepReader probes the relation read once and, if the adapter refuses
// the capability, witnesses the inline projection over the store's full
// contents.
//
// Any other probe error leaves the reader on the relation read: a transient
// failure is not evidence about the adapter, so each bead's own read decides
// that bead's fate and a bead whose read fails becomes ambiguous rather than
// edge-free. On that leg the anchors are then read in one batch (see prefetch),
// which is what keeps the walk's cost off the bead count.
func newSourceDepReader(source beads.Store, probeID string, anchors []string) (*sourceDepReader, error) {
	reader := &sourceDepReader{source: source, relationsOK: true}
	if probeID == "" {
		return reader, nil
	}
	_, err := source.DepList(probeID, "down")
	if !infraRelationCapabilityRefusal(err) {
		if prefetchErr := reader.prefetch(anchors); prefetchErr != nil {
			return nil, prefetchErr
		}
		return reader, nil
	}
	reader.relationsOK = false
	reader.relationsErr = err
	rows, listErr := source.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if listErr != nil {
		return nil, fmt.Errorf("witnessing the source's inline dependency projection: %w", listErr)
	}
	for _, b := range rows {
		if len(b.Dependencies) > 0 {
			reader.inlineWitnessed = true
			reader.witnessID = b.ID
			break
		}
	}
	return reader, nil
}

// deps returns one bead's outbound edges and whether the answer is trustworthy.
// ok=false means nothing here can state this bead's topology, and the caller
// must refuse to move it rather than move it edge-free.
func (r *sourceDepReader) deps(b beads.Bead) (edges []beads.Dep, ok bool, err error) {
	if r.relationsOK {
		if r.prefetched[b.ID] {
			return r.batched[b.ID], true, nil
		}
		listed, listErr := r.source.DepList(b.ID, "down")
		if listErr != nil {
			return nil, false, listErr
		}
		return listed, true, nil
	}
	if len(b.Dependencies) > 0 {
		// The inline projection carried edges, so it is live for this bead
		// whatever the corpus witness said.
		return b.Dependencies, true, nil
	}
	if len(b.Needs) > 0 {
		// Create-time shorthand with no materialized rows behind it that this
		// reader can resolve. Refuse rather than drop it.
		return nil, false, fmt.Errorf("carries %d unresolved needs shorthand and this source cannot list relations: %w", len(b.Needs), r.relationsErr)
	}
	if !r.inlineWitnessed {
		return nil, false, fmt.Errorf("nothing in this source carries an inline dependency projection, so an empty answer is not evidence of an empty topology, and it cannot list relations: %w", r.relationsErr)
	}
	return nil, true, nil
}

// describe states how this reader answered, for the operator report.
func (r *sourceDepReader) describe() string {
	if r.relationsOK {
		if len(r.prefetched) > 0 {
			return fmt.Sprintf("source dep edges: read through the store's relation listing, %d anchor(s) in one batched read", len(r.prefetched))
		}
		if r.emptyBatchAnchors > 0 {
			return fmt.Sprintf("source dep edges: read through the store's relation listing one bead at a time — the batched read answered nothing for %d anchor(s), which cannot be told from a degraded read", r.emptyBatchAnchors)
		}
		return "source dep edges: read through the store's relation listing"
	}
	if r.inlineWitnessed {
		return fmt.Sprintf("source dep edges: the store refuses relation listing (%v), so edges are read from the inline per-bead projection, witnessed live on %s", r.relationsErr, r.witnessID)
	}
	return fmt.Sprintf("source dep edges: UNREADABLE — the store refuses relation listing (%v) and no inline projection was witnessed", r.relationsErr)
}

// storageRecoveryLogPrefix is the recovery spelling without its source flag —
// the head every diagnostic in this file prints.
//
// It is DERIVED from storageRecoveryCommand rather than written out beside it,
// because the whole reason this command exists is that a diagnostic naming a
// command the binary does not carry is an operator instruction that fails at the
// shell. A constant would have been a second spelling to keep in step by hand,
// and TestStorageRecoveryCommandNamesTheVerbTheTreeCarries can only pin what it
// can see. An undecomposable spelling falls back to the whole command, which is
// wordy and still runnable — and newUnbuildableStorageCmd has already refused
// the build by then.
func storageRecoveryLogPrefix() string {
	surface, err := parseOperatorCommandSpelling(storageRecoveryCommand)
	if err != nil {
		return storageRecoveryCommand
	}
	return "gc " + surface.Namespace + " " + surface.Verb
}

// infraDepEdgeHeld reports whether the binding already carries want, by exactly
// infraDepDifference's rule: the far endpoint has to match, and an untyped edge
// on either side matches any type, because a store that records no dep_type is
// not recording a different one.
func infraDepEdgeHeld(want beads.Dep, have []beads.Dep) bool {
	for _, d := range have {
		if d.DependsOnID != want.DependsOnID {
			continue
		}
		if d.Type == "" || want.Type == "" || d.Type == want.Type {
			return true
		}
	}
	return false
}

// infraEdgePlan is the within-infra topology the binding owes once this run's
// copy finishes: what to write, what is already there, and — named rather than
// counted — what cannot be written at all.
//
// It is computed over every source infrastructure bead that will be resident,
// not over the rows this run copies, because an edge belongs to a PAIR and the
// two endpoints can arrive on different runs. That also makes it the repair: a
// re-run recomputes the same plan against live state, finds the edges an
// interrupted predecessor never wrote, and writes those.
type infraEdgePlan struct {
	// missing are the edges to write, in a deterministic order.
	missing []beads.Dep
	// held counts the edges the binding already carries, which is what makes a
	// re-run's "0 restored" a statement rather than a hope.
	held int
	// crossBoundary counts edges into a work bead. The destination owns no row
	// for the far endpoint, so the edge stays metadata linkage resolved by the
	// owning-store read on each side — exactly as importInfraSnapshot leaves it.
	crossBoundary int
	// collected counts edges into an infrastructure bead the manifest records
	// and the binding no longer holds. The binding's own GC took the row and the
	// edge with it; re-adding it would dangle a reference to a deliberate delete.
	collected int
	// occupied counts edges into an id the binding holds for a different bead
	// that this run leaves in place (infra_class_recover_held.go). Written,
	// the edge would point at that other bead; it waits on the operator's
	// per-id choice, which names the id.
	occupied int
	// dropped names the edges whose far endpoint is an infrastructure bead this
	// run REFUSED to move. These are within-infra edges this repair is dropping,
	// and nothing retains them — infraMigrationRow nils both Dependencies and
	// Needs — so they are named per edge rather than counted into anything.
	dropped []string
	// unstatable names resident beads whose source topology no read could state,
	// so an operator knows the plan is incomplete rather than empty.
	unstatable []string
}

// planInfraEdges builds the plan against the set of source beads the binding
// will hold when the copy finishes.
//
// A bead not yet created reads back no edges from the destination, so every edge
// it owes lands in missing — which is why the plan can be built before the copy
// and still describe the state after it. That ordering is what lets --dry-run
// report the topology it would restore without writing anything.
//
// The operator's per-id choices for reused ids (infra_class_recover_held.go)
// shape it the same way. An occupied id — one the binding holds for a
// different bead this run leaves in place — is not resident as the source's
// bead: none of its edges are planned, and an edge into it is counted, not
// written. A replacing id's binding row is removed before the copy with every
// edge touching it, so the binding is read as holding none of them.
func planInfraEdges(rows []beads.Bead, resident, infra, proven, occupied, replacing map[string]bool,
	edgesFor func(beads.Bead) ([]beads.Dep, bool, error), destination beads.Store,
) (infraEdgePlan, error) {
	plan := infraEdgePlan{}
	for _, b := range rows {
		if !resident[b.ID] {
			continue
		}
		want, ok, err := edgesFor(b)
		if !ok {
			plan.unstatable = append(plan.unstatable, fmt.Sprintf("%s (%v)", b.ID, err))
			continue
		}
		if len(want) == 0 {
			continue
		}
		var have []beads.Dep
		if !replacing[b.ID] {
			have, err = destination.DepList(b.ID, "down")
			if err != nil {
				return infraEdgePlan{}, fmt.Errorf("listing the binding's dep edges of %s: %w", b.ID, err)
			}
		}
		for _, d := range want {
			switch {
			case occupied[d.DependsOnID]:
				plan.occupied++
			case !infra[d.DependsOnID]:
				plan.crossBoundary++
			case !resident[d.DependsOnID]:
				if proven[d.DependsOnID] {
					plan.collected++
					continue
				}
				plan.dropped = append(plan.dropped, fmt.Sprintf("%s -> %s (%s): the far endpoint is an infrastructure bead this repair declined to move", b.ID, d.DependsOnID, d.Type))
			case !replacing[d.DependsOnID] && infraDepEdgeHeld(d, have):
				plan.held++
			default:
				// The near id is this bead's, not the entry's own IssueID: on the
				// inline projection the entry is a bd list-JSON row and the bead
				// it was read from is the authority for which bead it belongs to.
				plan.missing = append(plan.missing, beads.Dep{IssueID: b.ID, DependsOnID: d.DependsOnID, Type: d.Type})
			}
		}
	}
	return plan, nil
}

// reportInfraEdgePlan prints what the plan could not write. Both callers reach
// it — the run that writes and the run that finds nothing to write — because a
// dropped edge is a fact about the city rather than about this invocation.
func reportInfraEdgePlan(stdout io.Writer, plan infraEdgePlan) {
	if len(plan.dropped) > 0 {
		fmt.Fprintf(stdout, "DROPPED %d within-infra dep edge(s): the far endpoint is an infrastructure bead this repair declined to move, and nothing retains them:\n", len(plan.dropped)) //nolint:errcheck // best-effort stdout
		for _, entry := range plan.dropped {
			fmt.Fprintf(stdout, "  %s\n", entry) //nolint:errcheck // best-effort stdout
		}
	}
	if len(plan.unstatable) > 0 {
		fmt.Fprintf(stdout, "UNSTATABLE topology on %d bead(s) the binding already holds, so the edge plan is incomplete rather than empty:\n", len(plan.unstatable)) //nolint:errcheck // best-effort stdout
		for _, entry := range plan.unstatable {
			fmt.Fprintf(stdout, "  %s\n", entry) //nolint:errcheck // best-effort stdout
		}
	}
}

// doStorageRecoverStranded performs the repair, in the order the hazards demand.
//
// Structural validation first, because a plan boot would refuse must not be
// repaired toward; then the served-binding note, because a repair into a
// binding that is not the one serving is a copy into an orphan; then
// convergence, because this is a repair of a copy that happened rather than a
// substitute for one that did not; then the writers; then the guard — and only
// under the guard, the manifest, because a read-modify-write that straddles the
// lock can silently drop the ids a concurrent run published.
func doStorageRecoverStranded(ctx context.Context, request storageOperatorRequest, opts strandedRecoveryOptions, stdout, stderr io.Writer) (code int) {
	logPrefix := storageRecoveryLogPrefix()

	target, ok, err := resolveInfraBindingTarget(request.CityPath, request.Cfg)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !ok {
		reportUnresolvedInfraBinding(request, logPrefix, stdout, stderr)
		return 1
	}
	if _, err := resolveCityStoragePlan(request.CityPath, request.Cfg); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if blocked, held := servedBindingNoteHold(request.CityPath, target.Binding, config.StorageProviderSQLiteBeads, target.Database); held {
		blocked.Target = target
		fmt.Fprintln(stderr, infraMigrationOperatorAdvice(blocked, logPrefix)) //nolint:errcheck // best-effort stderr
		return 1
	}

	state, err := readInfraConvergenceState(target)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if state != infraConvergenceMarked {
		fmt.Fprintf(stderr, "%s: this city has not converged onto binding %q (%s is absent or its database is gone), so nothing here is a stranded write — the whole copy is still owed. Run:  %s\n", //nolint:errcheck // best-effort stderr
			logPrefix, target.Binding, target.MarkerPath(), storageMigrationCommand)
		return 1
	}

	if pid := infraMigrationForeignControllerPID(request.CityPath); pid != 0 {
		fmt.Fprintf(stderr, "%s: controller PID %d is live on this city and is still writing infrastructure beads to the work store; a repair proven against a moving source proves nothing. Stop it (gc stop) and run this again\n", logPrefix, pid) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !request.FleetStopped && !opts.DryRun {
		fmt.Fprintf(stderr, "%s: pass --%s to attest that %s. This command proves the controller is stopped and cannot prove anything about the rest; a write that lands after this run stays stranded and the next check names it\n", //nolint:errcheck // best-effort stderr
			logPrefix, storageFleetStoppedFlag, storageFleetStoppedAttestation)
		return 1
	}

	guard, err := storebinding.AcquireMigrationGuard(ctx, cityMigrationGuardDirectory(request.CityPath), storageMigrationGeneration)
	if err != nil {
		if errors.Is(err, storebinding.ErrMigrationGuardBusy) {
			fmt.Fprintf(stderr, "%s: another storage migration or repair holds this city. Wait for it to finish, or resolve it, and run this again\n", logPrefix) //nolint:errcheck // best-effort stderr
			return 1
		}
		fmt.Fprintf(stderr, "%s: taking the migration guard: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	defer func() {
		if releaseErr := guard.Release(); releaseErr != nil {
			fmt.Fprintf(stderr, "%s: releasing the migration guard: %v\n", logPrefix, releaseErr) //nolint:errcheck // best-effort stderr
		}
	}()

	// The manifest, under the lock that makes it a read-modify-WRITE rather than
	// two unrelated operations. runInfraClassMigration reads and writes it
	// entirely under the guard for the same reason: the guard is non-blocking, so
	// a second run of this verb can take it, publish and release inside any pause
	// above — and the controller dial above is a pause of up to three seconds on
	// a city with a stale socket, which is the normal state during the incident
	// this verb is for. A manifest read before that pause is a snapshot of bytes
	// that can have moved on, and writing a replacement built from it drops
	// whatever the other run recorded.
	manifest, recorded, err := readInfraCopyManifestRecord(target)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	proven := manifest.IDs
	if !recorded {
		fmt.Fprintf(stderr, "%s: %s converged before %s was recorded, so nothing distinguishes a bead the copy never carried from one the binding's own GC has since collected. Recovering on that basis would import rows the binding deliberately deleted. Re-converge the binding to restore the check\n", //nolint:errcheck // best-effort stderr
			logPrefix, target.Database, target.ManifestPath())
		return 1
	}

	source, err := openInfraMigrationSource(request.CityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: opening the work store: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	defer closeBeadStoreHandle(source) //nolint:errcheck // best-effort close

	// Selected under the manifest's classifier generation, exactly as the
	// containment check selects: a row the copy's classifier left as work is
	// no strand, and moving it would carry frozen pre-cutover state.
	rows, err := readInfraSnapshotAt(source, manifest.Generation)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	// One order for every pass below, so two runs over the same city report the
	// same thing in the same sequence.
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	infraIDs := make(map[string]bool, len(rows))
	for _, b := range rows {
		infraIDs[b.ID] = true
	}

	destination, err := openInfraDestination(target)
	if err != nil {
		fmt.Fprintf(stderr, "%s: opening binding %q at %s: %v\n", logPrefix, target.Binding, target.Database, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	closed := false
	defer func() {
		if !closed {
			_ = closeBeadStoreHandle(destination)
		}
	}()

	held, err := destination.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		fmt.Fprintf(stderr, "%s: listing binding %s: %v\n", logPrefix, target.Database, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	resolvable := make(map[string]bool, len(held)+len(rows))
	for _, b := range held {
		resolvable[b.ID] = true
	}

	// The gap, by exactly confirmInfraConvergence's rule: in the source, not in
	// the binding, and not in the manifest of what the copy was proven to
	// deliver. A bead the manifest records is one the binding's own lifecycle
	// removed, and importing it back would resurrect state the binding deleted.
	//
	// And the third bucket, which is neither: a bead the binding HOLDS that the
	// manifest does not record. Nothing about it is stranded, so no check names
	// it — and that is the trap. It is the residue an interrupted run leaves, and
	// left unrecorded it becomes a boot-fatal false strand the day the binding's
	// own GC collects it.
	var stranded []beads.Bead
	var unrecorded []beads.Bead
	removedSinceCutover := 0
	//
	// Both questions are asked of the manifest by (id, created_at), never by id
	// alone (infra_copy_manifest.go): a row that reuses an id the manifest
	// lists for an older bead was never carried. When the binding holds a row
	// under that id it lands in unrecorded, where the reopened binding's row is
	// proven against it — the old bead fails that proof and is named, never
	// folded in; the residue of an interrupted run of this repair passes it.
	// When the binding does not hold the id it is stranded, and moves.
	//
	// Two verdicts on a row the manifest lists by id alone say nothing proves
	// the row is or is not the bead the copy carried: an unbounded legacy
	// entry, and a presumed reused one, a bare id younger than the legacy bound
	// that an older build's recover-stranded may have appended for this very
	// row (infra_copy_manifest.go). Neither is counted as removed since
	// cutover on its own: leaving an unbounded one so made the clear refuse
	// while this command said "next: clear" — a loop with no exit. What ends it
	// depends on the row:
	//
	//   - the binding still holds the id: the unrecorded way (proven the same
	//     bead, it is recorded with its created_at);
	//   - the binding no longer holds it and the work row is CLOSED: moved like
	//     a stranded row and recorded. A closed row is terminal state, and
	//     carrying it in resurrects nothing;
	//   - the binding no longer holds it and the work row is OPEN: refused,
	//     named, and left to the operator. The work row may be a frozen
	//     snapshot of a bead the binding has since closed and collected, and
	//     moving it would bring that bead back OPEN beside whatever replaced
	//     it (#5987); or it may be the only copy of a bead the binding never
	//     received. Only the operator knows, so each such id needs --move
	//     (copy it in) or --record-carried (record it as the bead the copy
	//     carried; the clear then deletes it).
	//
	// One more answer moves such a row: a --replace-binding-row that an
	// earlier run recorded for the id, repeated. That run removed the
	// binding's row under the id and failed before its copy, so the binding
	// holds none, and moving the work-store row is what the choice asked.
	var idOnlyOpen []beads.Bead
	var recordCarried []beads.Bead
	// chosen maps each row a per-id flag answered to that flag.
	chosen := map[string]string{}
	choicesBefore, err := infraRecoverChoiceIDs(target)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	for _, b := range rows {
		match := manifest.Match(b)
		if resolvable[b.ID] {
			if match != infraManifestCarried {
				unrecorded = append(unrecorded, b)
			}
			continue
		}
		switch match {
		case infraManifestCarried:
			removedSinceCutover++
			continue
		case infraManifestLegacyUnbounded, infraManifestPresumedReused:
			switch {
			case opts.Move[b.ID] && opts.RecordCarried[b.ID]:
				fmt.Fprintf(stderr, "%s: %s is named by both --%s and --%s; choose one\n", logPrefix, b.ID, strandedMoveFlag, strandedRecordCarriedFlag) //nolint:errcheck // best-effort stderr
				return 1
			case opts.RecordCarried[b.ID]:
				chosen[b.ID] = strandedRecordCarriedFlag
				recordCarried = append(recordCarried, b)
				removedSinceCutover++
				continue
			case opts.Move[b.ID]:
				chosen[b.ID] = strandedMoveFlag
			case opts.ReplaceBindingRow[b.ID] && choicesBefore[strandedReplaceBindingRowFlag][b.ID]:
			case b.Status == "closed":
			default:
				idOnlyOpen = append(idOnlyOpen, b)
				continue
			}
		}
		stranded = append(stranded, b)
	}
	// An unrecorded row whose id the binding holds for a different bead
	// (infraHeldRowIsThisBead: created at another time, because bd minted the
	// id again after the copy, or, for a row with no created_at, written by no
	// copy). No copy can carry it under the id, so only the operator's per-id
	// answer resolves it (infra_class_recover_held.go). Without one it stays
	// unrecorded, and the proof below names it. A row the binding no longer
	// holds has no bead to choose against; the proof names it too.
	heldRows := map[string]beads.Bead{}
	var replace, drop, awaitingHeld []beads.Bead
	provable := make([]beads.Bead, 0, len(unrecorded))
	for _, b := range unrecorded {
		got, held, err := readInfraHeldRow(destination, b.ID)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v. Nothing was written\n", logPrefix, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if !held || infraHeldRowIsThisBead(b, got) {
			provable = append(provable, b)
			continue
		}
		heldRows[b.ID] = got
		switch {
		case opts.ReplaceBindingRow[b.ID] && opts.DropWorkRow[b.ID]:
			fmt.Fprintf(stderr, "%s: %s is named by both --%s and --%s; choose one\n", logPrefix, b.ID, strandedReplaceBindingRowFlag, strandedDropWorkRowFlag) //nolint:errcheck // best-effort stderr
			return 1
		case opts.ReplaceBindingRow[b.ID]:
			chosen[b.ID] = strandedReplaceBindingRowFlag
			replace = append(replace, b)
		case opts.DropWorkRow[b.ID]:
			chosen[b.ID] = strandedDropWorkRowFlag
			drop = append(drop, b)
		default:
			provable = append(provable, b)
			awaitingHeld = append(awaitingHeld, b)
		}
	}
	unrecorded = provable
	// A choice may only name a row it is the answer for: anything else is a
	// typo or a misunderstanding, and acting on it would move, record or
	// remove a bead nobody asked about. A choice for a row an earlier run
	// already resolved (now recorded, held by the binding after a copy that
	// failed before the manifest was extended, or removed by --drop-work-row)
	// is a no-op with a note, so re-running with the same flags is safe and a
	// copy failure's re-run stays a resume.
	resolved := map[string]bool{}
	inWork := map[string]bool{}
	for _, b := range rows {
		inWork[b.ID] = true
		if _, different := heldRows[b.ID]; different {
			continue
		}
		if resolvable[b.ID] || manifest.Match(b) == infraManifestCarried {
			resolved[b.ID] = true
		}
	}
	strandedSet := map[string]bool{}
	for _, b := range stranded {
		strandedSet[b.ID] = true
	}
	var misnamedLegacy, misnamedHeld, already, movedAnyway []string
	for flag, ids := range map[string]map[string]bool{strandedMoveFlag: opts.Move, strandedRecordCarriedFlag: opts.RecordCarried} {
		for id := range ids {
			switch {
			case chosen[id] == flag:
			case resolved[id]:
				already = append(already, id)
			default:
				misnamedLegacy = append(misnamedLegacy, id)
			}
		}
	}
	// A replaced or dropped row whose work-store copy is gone (the clear took
	// a replaced one; a drop took its own) is known from the backup the
	// choice wrote.
	for id := range opts.ReplaceBindingRow {
		switch {
		case chosen[id] == strandedReplaceBindingRowFlag:
		case resolved[id], choicesBefore[strandedReplaceBindingRowFlag][id] && !inWork[id]:
			already = append(already, id)
		case strandedSet[id] && chosen[id] == "":
			// The binding holds no row under the id (an earlier replace
			// removed it before its copy finished): the work-store row is
			// moved like any stranded row, which is what the choice asks.
			movedAnyway = append(movedAnyway, id)
		default:
			misnamedHeld = append(misnamedHeld, id)
		}
	}
	for id := range opts.DropWorkRow {
		switch {
		case chosen[id] == strandedDropWorkRowFlag:
		case choicesBefore[strandedDropWorkRowFlag][id] && !inWork[id]:
			already = append(already, id)
		default:
			misnamedHeld = append(misnamedHeld, id)
		}
	}
	if len(misnamedLegacy) > 0 {
		sort.Strings(misnamedLegacy)
		fmt.Fprintf(stderr, "%s: --%s/--%s name only work-store rows that the manifest lists by id alone and the binding no longer holds; these are not: %s. Nothing was written\n", logPrefix, strandedMoveFlag, strandedRecordCarriedFlag, strings.Join(misnamedLegacy, ", ")) //nolint:errcheck // best-effort stderr
		return 1
	}
	if len(misnamedHeld) > 0 {
		sort.Strings(misnamedHeld)
		fmt.Fprintf(stderr, "%s: --%s/--%s name only work-store rows whose id the binding holds for a different bead; these are not: %s. Nothing was written\n", logPrefix, strandedReplaceBindingRowFlag, strandedDropWorkRowFlag, strings.Join(misnamedHeld, ", ")) //nolint:errcheck // best-effort stderr
		return 1
	}
	if len(already) > 0 {
		sort.Strings(already)
		fmt.Fprintf(stdout, "note: already resolved by an earlier run, nothing to do: %s\n", strings.Join(already, ", ")) //nolint:errcheck // best-effort stdout
	}
	if len(movedAnyway) > 0 {
		sort.Strings(movedAnyway)
		fmt.Fprintf(stdout, "note: the binding holds no row under %s any more, so each is moved like any stranded row\n", strings.Join(movedAnyway, ", ")) //nolint:errcheck // best-effort stdout
	}
	// The evidence an operator decides each open row on. The refusal states
	// the two answers and what each does; it does not lean toward either,
	// except for a row an earlier --replace-binding-row already answered:
	// with the binding's row removed, the work-store row is the only live
	// copy of the id, and --record-carried would let the clear delete it.
	reportIDOnlyOpen := func(w io.Writer) {
		for _, b := range idOnlyOpen {
			fmt.Fprintf(w, "  %s  status=%s  type=%s  created=%s  updated=%s  title=%q\n", //nolint:errcheck // best-effort output
				b.ID, b.Status, b.Type, b.CreatedAt.UTC().Format(time.RFC3339), b.UpdatedAt.UTC().Format(time.RFC3339), b.Title)
			if choicesBefore[strandedReplaceBindingRowFlag][b.ID] {
				fmt.Fprintf(w, "    an earlier run removed the binding's row under %s by --%s (it is backed up in %s), so this row is the only live copy: finish that choice with --%s %s, not --%s, which would let the clear delete it\n", //nolint:errcheck // best-effort output
					b.ID, strandedReplaceBindingRowFlag, target.RecoverChoicesBackupPath(), strandedReplaceBindingRowFlag, b.ID, strandedRecordCarriedFlag)
			}
		}
	}
	idOnlyOpenRefusal := func() {
		fmt.Fprintf(stderr, "%s: %d work-store row(s) that are not closed are listed by the copy manifest by id alone, and the binding no longer holds them. Nothing records whether each is the bead the copy carried: no time bounds its entry, the row carries no created_at, or the row is younger than the legacy manifest, which an older build's recover-stranded extended with the id of every row it carried. Nothing here can tell whether the binding collected that bead (then the work row is a frozen snapshot) or never received it (then the work row is the only copy). Nothing was written. Decide per id and run this again with --%s <id> (copy the row into the binding as it is) or --%s <id> (record it as the bead the copy carried; the clear then deletes its work-store row, and the clear's backup keeps it). --%s and --%s show the rows without writing:\n", //nolint:errcheck // best-effort stderr
			logPrefix, len(idOnlyOpen), strandedMoveFlag, strandedRecordCarriedFlag, "dry-run", "dump")
		reportIDOnlyOpen(stderr)
	}
	// Refused now unless the operator asked to look: a dry run or a dump
	// carries on to show them, and refuses before any write.
	if len(idOnlyOpen) > 0 && !opts.DryRun && opts.DumpPath == "" {
		idOnlyOpenRefusal()
		return 1
	}

	fmt.Fprintf(stdout, "city:     %s\nbinding:  %s\ndatabase: %s\nmanifest: %s\n", request.CityPath, target.Binding, target.Database, target.ManifestPath())                                                                     //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "source infrastructure beads: %d\nbinding beads: %d\nproven copy: %d\nremoved since cutover: %d\nstranded: %d\nin the binding, unrecorded: %d\nunder an id the binding holds for a different bead: %d\n", //nolint:errcheck // best-effort stdout
		len(rows), len(held), len(proven), removedSinceCutover, len(stranded), len(unrecorded)-len(awaitingHeld), len(heldRows))

	probeID := ""
	if len(stranded) > 0 {
		probeID = stranded[0].ID
	} else if len(rows) > 0 {
		probeID = rows[0].ID
	}
	depReader, err := newSourceDepReader(source, probeID, sourceAnchorIDs(rows))
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "%s\n", depReader.describe()) //nolint:errcheck // best-effort stdout

	// Refuse rather than guess. A bead whose owning class is outside the ones
	// this split relocates, whose class would change in the crossing, or whose
	// dependency topology this source cannot state, is one nobody can say
	// belongs in this binding in the shape it would arrive in.
	//
	// A row the operator chose to put in place of the binding's row is held to
	// the same checks, before anything is removed: one that fails them keeps
	// the binding's row where it is.
	allowed := infraRecoverableClasses()
	byClass := map[string]int{}
	var movable []beads.Bead
	var ambiguous []string
	wantEdges := make(map[string][]beads.Dep, len(stranded)+len(replace))
	for _, b := range append(append([]beads.Bead(nil), stranded...), replace...) {
		class := coordclass.Classify(b)
		if !allowed[class] {
			ambiguous = append(ambiguous, fmt.Sprintf("%s (classifies as %s, which binding %q does not serve)", b.ID, class, target.Binding))
			continue
		}
		if diff := infraCopyClassDifference(b, infraMigrationRow(b)); diff != "" {
			ambiguous = append(ambiguous, fmt.Sprintf("%s (%s)", b.ID, diff))
			continue
		}
		edges, depsOK, depErr := depReader.deps(b)
		if !depsOK {
			ambiguous = append(ambiguous, fmt.Sprintf("%s (dependency topology unreadable: %v)", b.ID, depErr))
			continue
		}
		wantEdges[b.ID] = edges
		byClass[class.String()]++
		movable = append(movable, b)
	}
	for _, name := range sortedMapKeys(byClass) {
		fmt.Fprintf(stdout, "  class %-9s %d\n", name, byClass[name]) //nolint:errcheck // best-effort stdout
	}
	if len(ambiguous) > 0 {
		fmt.Fprintf(stdout, "  ambiguous (NOT moved): %d\n", len(ambiguous)) //nolint:errcheck // best-effort stdout
	}

	if opts.DumpPath != "" {
		// The rows awaiting a per-id choice are dumped too: the dump is how an
		// operator inspects them before choosing.
		dumped := append(append([]beads.Bead(nil), stranded...), idOnlyOpen...)
		dumped = append(append(append(dumped, replace...), drop...), awaitingHeld...)
		if err := writeStrandedDump(opts.DumpPath, target, depReader, dumped); err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		fmt.Fprintf(stdout, "dump: %s (%d bead(s) with their source dep edges)\n", opts.DumpPath, len(dumped)) //nolint:errcheck // best-effort stdout
	}

	// The topology, planned over every source bead the binding WILL hold rather
	// than over the rows this run copies. An edge from a bead that crossed at
	// cutover into one this run recovers is the dispatcher's normal wiring
	// direction, and reading only the copied rows' outbound edges drops every one
	// of them silently.
	//
	// An id the binding holds for a different bead is resident only once this
	// run replaces that bead with the source's: until then the binding's row
	// under it is not the source's, and no edge of the source's bead belongs
	// on it.
	resident := make(map[string]bool, len(resolvable)+len(movable))
	for id := range resolvable {
		resident[id] = true
	}
	replacing := map[string]bool{}
	var replaceMovable []beads.Bead
	for _, b := range movable {
		resident[b.ID] = true
		if _, different := heldRows[b.ID]; different {
			replacing[b.ID] = true
			replaceMovable = append(replaceMovable, b)
		}
	}
	occupied := map[string]bool{}
	for id := range heldRows {
		if !replacing[id] {
			occupied[id] = true
			delete(resident, id)
		}
	}
	edgesFor := func(b beads.Bead) ([]beads.Dep, bool, error) {
		if cached, ok := wantEdges[b.ID]; ok {
			return cached, true, nil
		}
		return depReader.deps(b)
	}
	plan, err := planInfraEdges(rows, resident, infraIDs, proven, occupied, replacing, edgesFor, destination)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if opts.DryRun {
		fmt.Fprintf(stdout, "dry-run: nothing was written. %d bead(s) would be copied into %s, and %d dep edge(s) restored\n", len(movable), target.Database, len(plan.missing)) //nolint:errcheck // best-effort stdout
		for _, b := range movable {
			if opts.Move[b.ID] {
				fmt.Fprintf(stdout, "would move: %s\n", b.ID) //nolint:errcheck // best-effort stdout
			}
		}
		for _, b := range recordCarried {
			fmt.Fprintf(stdout, "would record as carried: %s (the clear then deletes its work-store row; the backup keeps it)\n", b.ID) //nolint:errcheck // best-effort stdout
		}
		if len(idOnlyOpen) > 0 {
			fmt.Fprintf(stdout, "awaiting a choice (--%s or --%s): %d row(s)\n", strandedMoveFlag, strandedRecordCarriedFlag, len(idOnlyOpen)) //nolint:errcheck // best-effort stdout
			reportIDOnlyOpen(stdout)
		}
		for _, b := range replaceMovable {
			fmt.Fprintf(stdout, "would replace the binding's row under %s with the work-store row (the binding's row is backed up to %s first)\n", b.ID, target.RecoverChoicesBackupPath()) //nolint:errcheck // best-effort stdout
		}
		for _, b := range drop {
			fmt.Fprintf(stdout, "would remove the work-store row %s and keep the binding's (the work-store row is backed up to %s first)\n", b.ID, target.RecoverChoicesBackupPath()) //nolint:errcheck // best-effort stdout
		}
		if len(awaitingHeld) > 0 {
			fmt.Fprintf(stdout, "awaiting a choice (--%s or --%s): %d row(s) under an id the binding holds for a different bead\n", strandedReplaceBindingRowFlag, strandedDropWorkRowFlag, len(awaitingHeld)) //nolint:errcheck // best-effort stdout
			reportInfraHeldDifferent(stdout, awaitingHeld, heldRows)
		}
		reportInfraEdgePlan(stdout, plan)
		reportAmbiguous(stdout, ambiguous)
		if len(ambiguous) > 0 || len(plan.dropped) > 0 || len(plan.unstatable) > 0 || len(idOnlyOpen) > 0 || len(awaitingHeld) > 0 {
			return 1
		}
		return 0
	}
	// A dump was asked for; the rows awaiting a choice are in it, and nothing
	// is written until each has one.
	if len(idOnlyOpen) > 0 {
		idOnlyOpenRefusal()
		return 1
	}

	if len(movable) == 0 && len(unrecorded) == 0 && len(plan.missing) == 0 && len(recordCarried) == 0 && len(drop) == 0 {
		fmt.Fprintln(stdout, "nothing to recover: the binding already holds every infrastructure bead the retained work store has that the manifest does not record, with every within-infra edge between them.") //nolint:errcheck // best-effort stdout
		reportInfraEdgePlan(stdout, plan)
		reportAmbiguous(stdout, ambiguous)
		if len(ambiguous) > 0 || len(plan.dropped) > 0 || len(plan.unstatable) > 0 {
			return 1
		}
		return 0
	}

	creator, isCreator := destination.(beads.ForeignIDCreator)
	if !isCreator {
		fmt.Fprintf(stderr, "%s: binding store cannot preserve bead ids: %T does not implement ForeignIDCreator\n", logPrefix, destination) //nolint:errcheck // best-effort stderr
		return 1
	}

	// The operator's per-id answers for reused ids, before anything is copied:
	// every row they remove is backed up and proven from disk first. A replaced
	// id's work-store row is then copied with the rest of movable.
	if len(replaceMovable)+len(drop) > 0 {
		if err := applyInfraHeldChoices(target, source, destination, replaceMovable, drop, heldRows); err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		fmt.Fprintln(stdout, infraHeldChoiceSummary(target, replaceMovable, drop)) //nolint:errcheck // best-effort stdout
	}
	removedByChoice := ""
	if len(replaceMovable)+len(drop) > 0 {
		removedByChoice = fmt.Sprintf(" apart from the rows the per-id choices removed (backed up in %s)", target.RecoverChoicesBackupPath())
	}
	copied := 0
	for _, b := range movable {
		if _, err := destination.Get(b.ID); err == nil {
			// Already there — a writer that beat us to it between the list and
			// now. Idempotence, not a conflict.
			continue
		}
		if _, err := creator.CreateWithForeignID(infraMigrationRow(b)); err != nil {
			fmt.Fprintf(stderr, "%s: copying bead %s into %s: %v\n", logPrefix, b.ID, target.Database, err)                                                                                                                                                                                           //nolint:errcheck // best-effort stderr
			fmt.Fprintf(stderr, "%s: %d bead(s) were copied before this failed. Nothing was deleted%s, and the manifest was not extended; the rows already copied are recorded by the NEXT run rather than by this one, which is what makes a re-run a resume\n", logPrefix, copied, removedByChoice) //nolint:errcheck // best-effort stderr
			return 1
		}
		copied++
	}
	// A failure from here until the manifest is rewritten says only that the
	// manifest was NOT extended. The rows the per-id choices removed stay
	// removed all the same, so such a run ends by naming them, their backup,
	// and the resume.
	manifestRewritten := false
	if removedByChoice != "" {
		defer func() {
			if code != 0 && !manifestRewritten {
				fmt.Fprintf(stderr, "%s: the per-id choices took effect before this run failed, and they stand:\n%s\nRun this again with the same flags to resume: a choice that already took effect is a no-op\n", logPrefix, infraHeldChoiceSummary(target, replaceMovable, drop)) //nolint:errcheck // best-effort stderr
			}
		}()
	}

	// Dep edges, after every row exists, so an edge's far endpoint can be
	// resolved. Only edges the binding lacks are written: the pass is a diff, so
	// re-running it on a converged city writes nothing and says so. Each goes
	// through the migration's own edge writer, carrying the payload the source
	// holds for it — a repair that restored the edges and dropped their payloads
	// would report the same "edges: N restored" line while leaving the binding
	// short of what it is being repaired toward.
	for _, d := range plan.missing {
		if err := infraCopyDepEdge(destination, source, d.IssueID, d.DependsOnID, d.Type); err != nil {
			fmt.Fprintf(stderr, "%s: restoring dep %s -> %s: %v\n", logPrefix, d.IssueID, d.DependsOnID, err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	fmt.Fprintf(stdout, "copied: %d bead(s)\nedges: %d restored, %d already in the binding, %d cross-boundary edge(s) into work left as metadata linkage, %d into a bead the binding's own GC collected, %d into an id the binding holds for a different bead\n", //nolint:errcheck // best-effort stdout
		copied, len(plan.missing), plan.held, plan.crossBoundary, plan.collected, plan.occupied)

	// Proven against durable bytes, not against the handle that wrote them:
	// closed and reopened, exactly as verifyInfraCopy insists.
	if err := closeBeadStoreHandle(destination); err != nil {
		fmt.Fprintf(stderr, "%s: closing binding %q at %s after the copy: %v\n", logPrefix, target.Binding, target.Database, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	closed = true

	verifier, err := openInfraDestination(target)
	if err != nil {
		fmt.Fprintf(stderr, "%s: reopening the binding for the equality stage: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	defer closeBeadStoreHandle(verifier) //nolint:errcheck // best-effort close
	moved := make(map[string]bool, len(movable))
	for _, b := range movable {
		moved[b.ID] = true
	}

	// The equality stage, over every resident bead rather than over the copy, and
	// against a SECOND source read rather than against the one the plan was built
	// from. The first cut compared the destination to the same in-memory edge
	// list it had written from, so whatever that read lost, the proof lost too —
	// a dep proof that cannot fail. verifyInfraCopy re-reads source.DepList
	// independently and this stage now holds itself to the same standard, in one
	// batch where the walk's read was one (see reread).
	//
	// Fields are compared for the rows this run created and NOT for the rows that
	// were already resident: a bead the binding has been serving legitimately
	// diverges from the source copy it was made from, and demanding equality
	// there would refuse every healthy city. Edges are held the same way (see
	// recoveredEdgesDifference).
	proof, err := depReader.reread(sourceAnchorIDs(rows))
	if err != nil {
		fmt.Fprintf(stderr, "%s: re-reading for the equality stage: %v. The manifest was NOT extended\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	for _, b := range rows {
		if !resident[b.ID] {
			continue
		}
		got, err := verifier.Get(b.ID)
		if err != nil {
			fmt.Fprintf(stderr, "%s: bead %s missing from the reopened binding: %v. The manifest was NOT extended, so this bead is still named as stranded\n", logPrefix, b.ID, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if moved[b.ID] {
			if diff := beadCopyDifference(b, got); diff != "" {
				fmt.Fprintf(stderr, "%s: bead %s differs after the copy: %s. The manifest was NOT extended\n", logPrefix, b.ID, diff) //nolint:errcheck // best-effort stderr
				return 1
			}
			if diff := infraCopyClassDifference(b, got); diff != "" {
				fmt.Fprintf(stderr, "%s: bead %s %s. The manifest was NOT extended\n", logPrefix, b.ID, diff) //nolint:errcheck // best-effort stderr
				return 1
			}
		}
		if diff := recoveredEdgesDifference(b, proof, verifier, resident, moved[b.ID]); diff != "" {
			fmt.Fprintf(stderr, "%s: %s\n", logPrefix, diff) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	// The payload on every edge this run wrote, re-read from the reopened
	// binding and compared against the source.
	//
	// The presence loop above cannot see this: a restore that wrote all of
	// plan.missing and dropped every gate produces exactly the Dep set it
	// demands, and the run would report "edges: N restored" and extend the
	// manifest over a binding short of what it was being repaired toward. That is
	// the same blindness verifyInfraCopy carried until the migration learned to
	// witness payloads, and the repair path has no business being held to a
	// weaker standard than the copy it repairs.
	//
	// The scope is plan.missing rather than every resident edge, and deliberately:
	// an edge the binding was already serving may have diverged from the source
	// legitimately, exactly as the field comparison above declines to hold
	// already-resident rows to source equality. This run is answerable for what
	// it wrote.
	for _, d := range plan.missing {
		want, err := infraReadEdgePayload(source, d.IssueID, d.DependsOnID)
		if err != nil {
			fmt.Fprintf(stderr, "%s: re-reading the source payload on dep %s -> %s: %v. The manifest was NOT extended\n", logPrefix, d.IssueID, d.DependsOnID, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		got, err := infraReadEdgePayload(verifier, d.IssueID, d.DependsOnID)
		if err != nil {
			fmt.Fprintf(stderr, "%s: reading the restored payload on dep %s -> %s: %v. The manifest was NOT extended\n", logPrefix, d.IssueID, d.DependsOnID, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if want != got {
			fmt.Fprintf(stderr, "%s: dep %s -> %s was restored carrying %s, and the work store holds %s. The manifest was NOT extended\n", //nolint:errcheck // best-effort stderr
				logPrefix, d.IssueID, d.DependsOnID, infraFormatEdgePayload(got), infraFormatEdgePayload(want))
			return 1
		}
	}

	// The rows the binding already holds that the manifest does not record — an
	// interrupted predecessor's residue, or a row an older build's
	// recover-stranded carried — proven before they are recorded. The
	// manifest's meaning is "the copy was proven to deliver this id", so a
	// binding row that is not the source's bead is named and left unrecorded
	// rather than folded in under a proof nobody made. The proof is identity
	// (infraHeldRowDifference), not the moved rows' field equality: the binding
	// has been serving these rows, and a row it has since changed is still the
	// bead it carried.
	var recovered []string
	var unprovable []string
	for _, b := range unrecorded {
		got, err := verifier.Get(b.ID)
		if err != nil {
			unprovable = append(unprovable, fmt.Sprintf("%s (the reopened binding cannot read it: %v)", b.ID, err))
			continue
		}
		if diff := infraHeldRowDifference(b, got); diff != "" {
			unprovable = append(unprovable, fmt.Sprintf("%s (%s)", b.ID, diff))
			continue
		}
		recovered = append(recovered, b.ID)
	}
	for _, b := range movable {
		recovered = append(recovered, b.ID)
	}
	fmt.Fprintf(stdout, "verified: %d bead(s) re-read from the closed and reopened %s (%d copied, field-, class- and dep-equal; %d it already held, the same bead)\n", //nolint:errcheck // best-effort stdout
		len(recovered), target.Database, len(movable), len(recovered)-len(movable))
	if len(unprovable) > 0 {
		fmt.Fprintf(stdout, "NOT RECORDED: %d bead(s) the binding holds under a source id whose row this repair cannot prove is the source's; the manifest does not claim them:\n", len(unprovable)) //nolint:errcheck // best-effort stdout
		for _, entry := range unprovable {
			fmt.Fprintf(stdout, "  %s\n", entry) //nolint:errcheck // best-effort stdout
		}
	}
	if len(awaitingHeld) > 0 {
		fmt.Fprintf(stdout, "Under %d of those ids the binding holds a different bead, and only one row can keep the id. Decide per id and run this again with --%s <id> (the work-store row replaces the binding's) or --%s <id> (the work-store row is removed and the binding's stays); each first backs up the row it removes, with every edge touching it, to %s:\n", //nolint:errcheck // best-effort stdout
			len(awaitingHeld), strandedReplaceBindingRowFlag, strandedDropWorkRowFlag, target.RecoverChoicesBackupPath())
		reportInfraHeldDifferent(stdout, awaitingHeld, heldRows)
	}

	// Only now the manifest, and only as a SUPERSET: the previous proven set is
	// history the next boot still needs to classify absence against. An id
	// dropped from it turns a bead the binding's own GC legitimately collected
	// back into a strand, and the city refuses to boot over a row nothing did
	// wrong to.
	// The operator's --record-carried answers: recorded with their created_at
	// as beads the copy carried (the binding has since collected them), so the
	// clear deletes the frozen work rows.
	for _, b := range recordCarried {
		recovered = append(recovered, b.ID)
	}
	if len(recordCarried) > 0 {
		fmt.Fprintf(stdout, "recorded as carried (by --%s): %d bead(s)\n", strandedRecordCarriedFlag, len(recordCarried)) //nolint:errcheck // best-effort stdout
	}
	backup, err := backupInfraCopyManifest(target)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	extended := make([]string, 0, len(proven)+len(recovered))
	for id := range proven {
		extended = append(extended, id)
	}
	for _, id := range recovered {
		if !proven[id] {
			extended = append(extended, id)
		}
	}
	sort.Strings(extended)
	// The superset check is sound because proven was read UNDER the guard, a few
	// hundred lines up: these are the bytes on disk, not a snapshot that could
	// have moved on while this run was dialing a socket.
	if dropped := manifestIDsDropped(proven, extended); len(dropped) > 0 {
		fmt.Fprintf(stderr, "%s: the extended manifest would drop %d id(s) the previous one recorded (%s). It is written as a superset or not at all; nothing was written and the previous manifest at %s still stands\n", //nolint:errcheck // best-effort stderr
			logPrefix, len(dropped), strings.Join(dropped, ", "), target.ManifestPath())
		return 1
	}
	// created_at carries over for every id already recorded, and a recovered
	// row's own created_at replaces it (infraManifestRecordCreated).
	extendedCreated := infraManifestRecordCreated(manifest.Created, rows, recovered)
	// The bounds pass through unchanged. The legacy bound must not move, or
	// every id reused since the legacy manifest was written would read as the
	// bead it copied; #gc:written must not either, because it bounds the bare
	// ids an older build appends later (infra_copy_manifest.go, "The write
	// time"). The classifier generation passes through too: this run moved
	// only what that generation selects.
	if err := rewriteInfraCopyManifest(target, manifest, extended, extendedCreated); err != nil {
		fmt.Fprintf(stderr, "%s: %v. The previous manifest is at %s\n", logPrefix, err, backup) //nolint:errcheck // best-effort stderr
		return 1
	}
	manifestRewritten = true
	extendedManifest, _, err := readInfraCopyManifestRecord(target)
	if err != nil {
		fmt.Fprintf(stderr, "%s: re-reading the extended manifest: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "manifest: %d -> %d bead(s) (previous manifest retained at %s)\n", len(proven), len(extended), backup) //nolint:errcheck // best-effort stdout

	// The residual, read back through the same classifier the boot check uses.
	residual, err := classifyInfraContainmentGap(request.CityPath, target, extendedManifest)
	if err != nil {
		fmt.Fprintf(stderr, "%s: re-reading the containment gap: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "residual stranded: %d\nresidual removed-since-cutover: %d\n", len(residual.Stranded), residual.RemovedSinceCutover) //nolint:errcheck // best-effort stdout
	if len(residual.Stranded) > 0 {
		fmt.Fprintf(stdout, "residual ids: %s\n", strings.Join(residual.Stranded, ", ")) //nolint:errcheck // best-effort stdout
	}
	reportInfraEdgePlan(stdout, plan)
	reportAmbiguous(stdout, ambiguous)
	if len(residual.Stranded) > 0 || len(ambiguous) > 0 || len(plan.dropped) > 0 || len(plan.unstatable) > 0 || len(unprovable) > 0 {
		return 1
	}
	// The recovered rows are now proven into the binding and still in the work
	// store: second rows under ids the binding owns, which boot refuses until
	// they are cleared. This command only ever adds, so it names the one that
	// clears rather than doing it.
	if len(residual.Retained) > 0 {
		// Retained counts every carried row, including the ones whose binding
		// copy the binding's own GC has since collected; name the two apart.
		held := len(residual.Retained) - residual.RemovedSinceCutover
		fmt.Fprintf(stdout, "next: the work store still holds %d retained cop(ies) (%d of beads the binding holds, %d the binding has since collected); clear them with `%s`\n", len(residual.Retained), held, residual.RemovedSinceCutover, storageClearInstruction()) //nolint:errcheck // best-effort stdout
	}
	return 0
}

// reportUnresolvedInfraBinding explains a city whose infrastructure classes do
// not resolve to a binding this build can repair into.
//
// Two very different cities reach it, and one refusal cannot be true of both. A
// legacy single-store city really has nothing stranded: every class is served by
// the work store and no bead is anywhere the readers cannot see. A BORN-SPLIT
// city has the whole split, served by a provider this build carries no migration
// discipline for — and the infrastructure beads sitting in its work store are
// exactly what the boot guard refuses over and what `gc storage status` prints
// as `born-split: BLOCKED`. Telling that operator "nothing here is stranded"
// contradicted two other surfaces of the same binary about the same bead id,
// during an incident, one step from the born-split advice's own second clause:
// "then delete them from the work store".
//
// So the born-split arm defers to the authority that CAN answer, and reports the
// same discipline doStorageStatus reports rather than asserting a fact about
// data this verb never read.
func reportUnresolvedInfraBinding(request storageOperatorRequest, logPrefix string, stdout, stderr io.Writer) {
	storage := request.Cfg.EffectiveStorage()
	if shape, binding := storageSplitShapeOf(storage); shape == storageSplitWhole {
		provider := storage.Bindings[binding].Provider
		fmt.Fprintf(stderr, "%s: binding %q is served by provider %q, which this build carries no repair for — the one it carries serves only a binding backed by its own bead engine. This city serves under the born-split discipline instead, and this command cannot state anything about it.\n", //nolint:errcheck // best-effort stderr
			logPrefix, binding, provider)
		report := checkBornSplitDiscipline(request.CityPath, logPrefix, stderr)
		switch report.Outcome {
		case infraMigrationConverged:
			fmt.Fprintf(stdout, "born-split: clean — the work store holds no infrastructure bead, so nothing here is stranded and the binding may serve.\n") //nolint:errcheck // best-effort stdout
		case infraMigrationBornSplitBlocked:
			report.Target = infraBindingTarget{Binding: binding}
			fmt.Fprintf(stdout, "born-split: BLOCKED — the work store holds %d infrastructure bead(s) the binding cannot read: %s\n", //nolint:errcheck // best-effort stdout
				len(report.Stranded), strings.Join(report.Stranded, ", "))
			fmt.Fprintln(stderr, infraMigrationOperatorAdvice(report, logPrefix)) //nolint:errcheck // best-effort stderr
		default:
			fmt.Fprintf(stdout, "born-split: could not be verified (%s); reason on stderr\n", report.Outcome) //nolint:errcheck // best-effort stdout
		}
		return
	}
	fmt.Fprintf(stderr, "%s: this city's [storage.classes] do not assign %s to one shared non-work binding this build can serve, so there is no binding to recover into and nothing here is stranded. %s\n", //nolint:errcheck // best-effort stderr
		logPrefix, infraMigrationClassList(), storageSupportedTopologyStatement)
}

// manifestIDsDropped returns the sorted ids the previous proven set recorded
// that the replacement does not.
//
// The manifest is the only record that tells a bead the copy never carried
// apart from one the binding's own GC collected, so it is append-only in
// practice: every id it has ever recorded stays recorded. Checking rather than
// asserting is the point — the extension is built by hand a few lines above,
// and a superset is exactly the kind of property that stays true until someone
// makes it a set intersection by accident.
func manifestIDsDropped(previous map[string]bool, replacement []string) []string {
	keep := setOf(replacement)
	var dropped []string
	for id := range previous {
		if !keep[id] {
			dropped = append(dropped, id)
		}
	}
	sort.Strings(dropped)
	return dropped
}

// reportAmbiguous names the beads this repair declined to move, one per line,
// because a count alone is not a starting point for resolving them.
func reportAmbiguous(stdout io.Writer, ambiguous []string) {
	if len(ambiguous) == 0 {
		return
	}
	fmt.Fprintf(stdout, "REFUSED to move %d bead(s) whose class this repair cannot state; they are intact in the work store:\n", len(ambiguous)) //nolint:errcheck // best-effort stdout
	for _, entry := range ambiguous {
		fmt.Fprintf(stdout, "  %s\n", entry) //nolint:errcheck // best-effort stdout
	}
}

// infraPathInside reports whether path resolves inside root.
//
// Both sides are made absolute and lexically cleaned before they are compared,
// so a relative spelling, a trailing slash or a `..` cannot walk past the check.
// The path itself need not exist — that is the point, since the refusal has to
// fire BEFORE anything is created there.
func infraPathInside(root, path string) (bool, error) {
	if root == "" {
		return false, nil
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false, err
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		// Different volumes on Windows; nothing under the root by construction.
		return false, nil //nolint:nilerr // an unrelatable path is not inside the root
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)), nil
}

// setOf renders an id slice as the membership map the containment classifier
// takes.
func setOf(ids []string) map[string]bool {
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set
}

// recoveredEdgesDifference holds one resident bead's edges in the reopened
// binding to the source's second read of them, and names the first way they
// disagree, or "" when they do not.
//
// Every resident bead must hold each edge the source gives it into another
// resident bead. Only a bead this run created is held to the other direction
// too, because anything extra on it would be fabricated; a bead that was
// already resident may hold edges the source never had, legitimately.
//
// Resident is the plan's set (planInfraEdges), in both directions: an id the
// binding holds for a different bead is not resident, so an edge into it,
// which the plan leaves for the operator's per-id choice, is not demanded.
//
// A bead whose edges the second read cannot state is no disagreement: an edge
// nobody can read is a topology this stage cannot prove, not a copy it can
// disprove, so the rows this run copied still stand. A bead the plan's own read
// could not state either is named unstatable there, and the exit code carries
// it.
func recoveredEdgesDifference(b beads.Bead, proof *sourceDepReader, verifier beads.Store, resident map[string]bool, moved bool) string {
	wantDeps, depsOK, _ := proof.deps(b)
	if !depsOK {
		return ""
	}
	gotDeps, err := verifier.DepList(b.ID, "down")
	if err != nil {
		return fmt.Sprintf("listing copied deps of %s: %v", b.ID, err)
	}
	for _, d := range wantDeps {
		if !resident[d.DependsOnID] || infraDepEdgeHeld(d, gotDeps) {
			continue
		}
		return fmt.Sprintf("dep %s -> %s is in the work store and missing from the binding after the restore. The manifest was NOT extended", b.ID, d.DependsOnID)
	}
	if !moved {
		return ""
	}
	if diff := infraDepDifference(b.ID, wantDeps, gotDeps, resident); diff != "" {
		return diff + ". The manifest was NOT extended"
	}
	return ""
}

// writeStrandedDump records every stranded bead and its source dep edges before
// anything is written, so the input to the repair is reproducible from a file
// rather than from a store that has since changed.
//
// It refuses two paths rather than truncating them, because this is the one
// write in this file an operator TYPES, and it happens before the dry-run branch
// and long before any backup exists:
//
//   - anything inside the binding root. The three completions available there
//     are the manifest, the marker and the database directory, so every
//     completion in the directory an operator is investigating is fatal: the
//     manifest's loss turns stranded-write detection off for the life of the
//     city, whose only documented remedy is a re-converge this file measures as
//     a 51,127 -> 1,452 loss.
//   - anything that already exists. A dump is a new artifact; an existing path
//     is a typo rather than an instruction, and `--dry-run` — the first thing a
//     cautious operator reaches for, and the one mode that requires no
//     attestation — would otherwise print "nothing was written" over a file it
//     had just emptied.
func writeStrandedDump(path string, target infraBindingTarget, depReader *sourceDepReader, stranded []beads.Bead) error {
	if inside, err := infraPathInside(target.Root, path); err != nil {
		return fmt.Errorf("resolving the stranded-bead dump path %s: %w", path, err)
	} else if inside {
		return fmt.Errorf("refusing to write the stranded-bead dump to %s: it is inside the binding root %s, which holds the proven-copy manifest, the convergence marker and the database this repair reads. Write the dump somewhere else", path, target.Root)
	}
	entries := make([]strandedBeadDump, 0, len(stranded))
	for _, b := range stranded {
		entry := strandedBeadDump{Bead: b, Class: coordclass.Classify(b).String(), DepSource: "relations"}
		if !depReader.relationsOK {
			entry.DepSource = "inline"
		}
		deps, ok, err := depReader.deps(b)
		if !ok {
			entry.DepSource = "unreadable"
			entry.DepError = fmt.Sprint(err)
		}
		entry.Deps = deps
		entries = append(entries, entry)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("refusing to write the stranded-bead dump to %s: it already exists, and a dump is written to a fresh path so a mistyped one cannot truncate the file it names", path)
		}
		return fmt.Errorf("writing the stranded-bead dump: %w", err)
	}
	writer := bufio.NewWriter(file)
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(entries); err != nil {
		_ = file.Close()
		return fmt.Errorf("encoding the stranded-bead dump: %w", err)
	}
	if err := writer.Flush(); err != nil {
		_ = file.Close()
		return fmt.Errorf("flushing the stranded-bead dump: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing the stranded-bead dump: %w", err)
	}
	return nil
}

// backupInfraCopyManifest copies the current manifest beside itself before the
// repair rewrites it. The rewrite is atomic and a proven superset, so this is
// belt and braces — but the manifest is the only record that tells a strand
// apart from a GC delete, and a file that cannot be reconstructed is one worth
// keeping.
func backupInfraCopyManifest(target infraBindingTarget) (string, error) {
	contents, err := os.ReadFile(target.ManifestPath())
	if err != nil {
		return "", fmt.Errorf("reading the manifest to back it up: %w", err)
	}
	path := fmt.Sprintf("%s.bak-%s", target.ManifestPath(), time.Now().UTC().Format("20060102T150405Z"))
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		return "", fmt.Errorf("backing the manifest up to %s: %w", path, err)
	}
	return path, nil
}
