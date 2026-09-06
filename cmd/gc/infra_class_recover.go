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
// writes to the source.
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
// Nothing is deleted, from either side. The source keeps its rows verbatim, as
// it does after the migration, so a bad outcome here is a duplicate rather than
// a loss. The manifest — the record every later boot classifies absence
// against — is extended only after every moved bead has been re-read from a
// CLOSED AND REOPENED destination and proven field-, class- and dep-equal to
// the source, by the migration's own comparators.
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
// reopened destination by the same comparators the moved rows face, and folded
// into the manifest. One that cannot be proven — a row under that id that is not
// the source's row — is named and left unrecorded, because the manifest's
// meaning is "the copy was proven to deliver this id" and recording an unproven
// one would be a proof nobody made.
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
// doStorageMigrate refuses while any rig scope holds an infrastructure bead,
// because the copy's source is the city work store and a rig-scoped row would
// be silently omitted from a cutover that then declares itself proven. This
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
	)
	cmd := &cobra.Command{
		Use:          surface.Verb,
		Short:        "Copy stranded infrastructure beads from the retained work store into the converged binding",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		Long: `Copy the infrastructure beads a converged city's proven copy never carried
out of the retained work store and into the binding that is already serving.

This is the recovery the stranded-write refusal names. It is additive: it moves
only ids the binding does not hold and the proven-copy manifest does not record,
it deletes nothing from either store, and it extends the manifest only after
every moved bead has been proven equal against a closed and reopened
destination. A bead whose class it cannot state is named and left where it is.

It refuses on a city that has NOT converged — the whole copy is still owed
there, and ` + "`" + storageMigrationCommand + "`" + ` is what owes it. It is not that
command run twice: the migration is one-shot on purpose, and forcing it to
re-copy would re-import a serving binding from a source that no longer holds
what the binding does.`,
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
			return exitForCode(doStorageRecoverStranded(cmd.Context(), request, strandedRecoveryOptions{DryRun: dryRun, DumpPath: dumpPath}, stdout, stderr))
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
	return cmd
}

// strandedRecoveryOptions are the two knobs the repair carries.
type strandedRecoveryOptions struct {
	// DryRun reports and dumps without writing to the binding or the manifest.
	DryRun bool
	// DumpPath, when set, receives every stranded bead and its source dep edges
	// as JSON before any write happens.
	DumpPath string
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
}

// newSourceDepReader probes the relation read once and, if the adapter refuses
// the capability, witnesses the inline projection over the store's full
// contents.
//
// Any other probe error leaves the reader on the relation read: a transient
// failure is not evidence about the adapter, so each bead's own read decides
// that bead's fate and a bead whose read fails becomes ambiguous rather than
// edge-free.
func newSourceDepReader(source beads.Store, probeID string) (*sourceDepReader, error) {
	reader := &sourceDepReader{source: source, relationsOK: true}
	if probeID == "" {
		return reader, nil
	}
	_, err := source.DepList(probeID, "down")
	if !infraRelationCapabilityRefusal(err) {
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
func planInfraEdges(rows []beads.Bead, resident, infra, proven map[string]bool,
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
		have, err := destination.DepList(b.ID, "down")
		if err != nil {
			return infraEdgePlan{}, fmt.Errorf("listing the binding's dep edges of %s: %w", b.ID, err)
		}
		for _, d := range want {
			switch {
			case !infra[d.DependsOnID]:
				plan.crossBoundary++
			case !resident[d.DependsOnID]:
				if proven[d.DependsOnID] {
					plan.collected++
					continue
				}
				plan.dropped = append(plan.dropped, fmt.Sprintf("%s -> %s (%s): the far endpoint is an infrastructure bead this repair declined to move", b.ID, d.DependsOnID, d.Type))
			case infraDepEdgeHeld(d, have):
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
func doStorageRecoverStranded(ctx context.Context, request storageOperatorRequest, opts strandedRecoveryOptions, stdout, stderr io.Writer) int {
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
	proven, recorded, err := readInfraCopyManifest(target)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
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

	rows, err := readInfraSnapshot(source)
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
	for _, b := range rows {
		if resolvable[b.ID] {
			if !proven[b.ID] {
				unrecorded = append(unrecorded, b)
			}
			continue
		}
		if proven[b.ID] {
			removedSinceCutover++
			continue
		}
		stranded = append(stranded, b)
	}

	fmt.Fprintf(stdout, "city:     %s\nbinding:  %s\ndatabase: %s\nmanifest: %s\n", request.CityPath, target.Binding, target.Database, target.ManifestPath())             //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "source infrastructure beads: %d\nbinding beads: %d\nproven copy: %d\nremoved since cutover: %d\nstranded: %d\nin the binding, unrecorded: %d\n", //nolint:errcheck // best-effort stdout
		len(rows), len(held), len(proven), removedSinceCutover, len(stranded), len(unrecorded))

	probeID := ""
	if len(stranded) > 0 {
		probeID = stranded[0].ID
	} else if len(rows) > 0 {
		probeID = rows[0].ID
	}
	depReader, err := newSourceDepReader(source, probeID)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "%s\n", depReader.describe()) //nolint:errcheck // best-effort stdout

	// Refuse rather than guess. A bead whose owning class is outside the ones
	// this split relocates, whose class would change in the crossing, or whose
	// dependency topology this source cannot state, is one nobody can say
	// belongs in this binding in the shape it would arrive in.
	allowed := infraRecoverableClasses()
	byClass := map[string]int{}
	var movable []beads.Bead
	var ambiguous []string
	wantEdges := make(map[string][]beads.Dep, len(stranded))
	for _, b := range stranded {
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
		if err := writeStrandedDump(opts.DumpPath, target, depReader, stranded); err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		fmt.Fprintf(stdout, "dump: %s (%d bead(s) with their source dep edges)\n", opts.DumpPath, len(stranded)) //nolint:errcheck // best-effort stdout
	}

	// The topology, planned over every source bead the binding WILL hold rather
	// than over the rows this run copies. An edge from a bead that crossed at
	// cutover into one this run recovers is the dispatcher's normal wiring
	// direction, and reading only the copied rows' outbound edges drops every one
	// of them silently.
	resident := make(map[string]bool, len(resolvable)+len(movable))
	for id := range resolvable {
		resident[id] = true
	}
	for _, b := range movable {
		resident[b.ID] = true
	}
	edgesFor := func(b beads.Bead) ([]beads.Dep, bool, error) {
		if cached, ok := wantEdges[b.ID]; ok {
			return cached, true, nil
		}
		return depReader.deps(b)
	}
	plan, err := planInfraEdges(rows, resident, infraIDs, proven, edgesFor, destination)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if opts.DryRun {
		fmt.Fprintf(stdout, "dry-run: nothing was written. %d bead(s) would be copied into %s, and %d dep edge(s) restored\n", len(movable), target.Database, len(plan.missing)) //nolint:errcheck // best-effort stdout
		reportInfraEdgePlan(stdout, plan)
		reportAmbiguous(stdout, ambiguous)
		if len(ambiguous) > 0 || len(plan.dropped) > 0 || len(plan.unstatable) > 0 {
			return 1
		}
		return 0
	}

	if len(movable) == 0 && len(unrecorded) == 0 && len(plan.missing) == 0 {
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
	copied := 0
	for _, b := range movable {
		if _, err := destination.Get(b.ID); err == nil {
			// Already there — a writer that beat us to it between the list and
			// now. Idempotence, not a conflict.
			continue
		}
		if _, err := creator.CreateWithForeignID(infraMigrationRow(b)); err != nil {
			fmt.Fprintf(stderr, "%s: copying bead %s into %s: %v\n", logPrefix, b.ID, target.Database, err)                                                                                                                                                                       //nolint:errcheck // best-effort stderr
			fmt.Fprintf(stderr, "%s: %d bead(s) were copied before this failed. Nothing was deleted and the manifest was not extended; the rows already copied are recorded by the NEXT run rather than by this one, which is what makes a re-run a resume\n", logPrefix, copied) //nolint:errcheck // best-effort stderr
			return 1
		}
		copied++
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
	fmt.Fprintf(stdout, "copied: %d bead(s)\nedges: %d restored, %d already in the binding, %d cross-boundary edge(s) into work left as metadata linkage, %d into a bead the binding's own GC collected\n", //nolint:errcheck // best-effort stdout
		copied, len(plan.missing), plan.held, plan.crossBoundary, plan.collected)

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
	after, err := verifier.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		fmt.Fprintf(stderr, "%s: listing the reopened binding: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	witnessed := make(map[string]bool, len(after))
	for _, b := range after {
		witnessed[b.ID] = true
	}
	moved := make(map[string]bool, len(movable))
	for _, b := range movable {
		moved[b.ID] = true
	}

	// The equality stage, over every resident bead rather than over the copy, and
	// against a SECOND source read rather than against the one the plan was built
	// from. The first cut compared the destination to the same in-memory edge
	// list it had written from, so whatever that read lost, the proof lost too —
	// a dep proof that cannot fail. verifyInfraCopy re-reads source.DepList
	// independently and this stage now holds itself to the same standard.
	//
	// Fields are compared for the rows this run created and NOT for the rows that
	// were already resident: a bead the binding has been serving legitimately
	// diverges from the source copy it was made from, and demanding equality
	// there would refuse every healthy city. Edges are compared one-directionally
	// for those same rows, for the same reason — the binding may hold edges the
	// source never had — while a row this run created is held to both directions,
	// because anything extra on it would be fabricated.
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
		wantDeps, depsOK, _ := depReader.deps(b)
		if !depsOK {
			// The plan already named this bead as unstatable and the exit code
			// below carries it. The rows this run copied still stand: an edge
			// nobody can read is a topology this stage cannot prove, not a copy
			// it can disprove.
			continue
		}
		gotDeps, err := verifier.DepList(b.ID, "down")
		if err != nil {
			fmt.Fprintf(stderr, "%s: listing copied deps of %s: %v\n", logPrefix, b.ID, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		for _, d := range wantDeps {
			if !resident[d.DependsOnID] || infraDepEdgeHeld(d, gotDeps) {
				continue
			}
			fmt.Fprintf(stderr, "%s: dep %s -> %s is in the work store and missing from the binding after the restore. The manifest was NOT extended\n", logPrefix, b.ID, d.DependsOnID) //nolint:errcheck // best-effort stderr
			return 1
		}
		if moved[b.ID] {
			if diff := infraDepDifference(b.ID, wantDeps, gotDeps, witnessed); diff != "" {
				fmt.Fprintf(stderr, "%s: %s. The manifest was NOT extended\n", logPrefix, diff) //nolint:errcheck // best-effort stderr
				return 1
			}
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

	// The residue an interrupted predecessor left, proven by the same
	// comparators the moved rows face before it is recorded. The manifest's
	// meaning is "the copy was proven to deliver this id", so a binding row that
	// is not the source's row is named and left unrecorded rather than folded in
	// under a proof nobody made.
	var recovered []string
	var unprovable []string
	for _, b := range unrecorded {
		got, err := verifier.Get(b.ID)
		if err != nil {
			unprovable = append(unprovable, fmt.Sprintf("%s (the reopened binding cannot read it: %v)", b.ID, err))
			continue
		}
		if diff := beadCopyDifference(b, got); diff != "" {
			unprovable = append(unprovable, fmt.Sprintf("%s (%s)", b.ID, diff))
			continue
		}
		if diff := infraCopyClassDifference(b, got); diff != "" {
			unprovable = append(unprovable, fmt.Sprintf("%s (%s)", b.ID, diff))
			continue
		}
		recovered = append(recovered, b.ID)
	}
	for _, b := range movable {
		recovered = append(recovered, b.ID)
	}
	fmt.Fprintf(stdout, "verified: %d bead(s) re-read field-, class- and dep-equal from the closed and reopened %s\n", len(recovered), target.Database) //nolint:errcheck // best-effort stdout
	if len(unprovable) > 0 {
		fmt.Fprintf(stdout, "NOT RECORDED: %d bead(s) the binding holds under a source id whose row this repair cannot prove is the source's; the manifest does not claim them:\n", len(unprovable)) //nolint:errcheck // best-effort stdout
		for _, entry := range unprovable {
			fmt.Fprintf(stdout, "  %s\n", entry) //nolint:errcheck // best-effort stdout
		}
	}

	// Only now the manifest, and only as a SUPERSET: the previous proven set is
	// history the next boot still needs to classify absence against. An id
	// dropped from it turns a bead the binding's own GC legitimately collected
	// back into a strand, and the city refuses to boot over a row nothing did
	// wrong to.
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
	if err := writeInfraCopyManifest(target, extended); err != nil {
		fmt.Fprintf(stderr, "%s: %v. The previous manifest is at %s\n", logPrefix, err, backup) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "manifest: %d -> %d bead(s) (previous manifest retained at %s)\n", len(proven), len(extended), backup) //nolint:errcheck // best-effort stdout

	// The residual, read back through the same classifier the boot check uses.
	residual, err := classifyInfraContainmentGap(request.CityPath, target, setOf(extended))
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
