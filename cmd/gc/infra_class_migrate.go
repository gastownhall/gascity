package main

// The storage-class migration: how a city's infrastructure classes move out of
// the work store and into a binding of their own.
//
// A city whose [storage.classes] point graph/sessions/messaging/orders/nudges
// at a non-work binding, while work stays on the reserved `work` binding, still
// holds every one of those beads in the work store until this runs. It copies
// that slice into the binding's bead engine — ids preserved, within-class dep
// topology re-added — then TESTS EQUALITY between source and destination, and
// only then records convergence.
//
// # Destination
//
// The copy lands in the exact database the deployed provider opens, under the
// exact id prefix it opens it with: the provider's own resolver supplies
// <binding root>/graph/beads.sqlite, and config.ReservedClassPrefix supplies
// the reserved namespace it mints in. Neither is spelled here. A migration that
// writes its own idea of the path reports success into a file no runtime
// binding ever opens, which is a worse failure than not running at all — so the
// path and the prefix come from the provider's helper and the shared prefix
// registry rather than from a local join, and they cannot drift apart from what
// the runtime reads.
//
// # Trigger and ordering
//
// The migration is triggered BY the config swap, not the other way around: it
// resolves its destination from [storage.classes] and [storage.bindings], so by
// the time it runs the operator has already pointed the infrastructure classes
// at the binding. The real order is
//
//	operator swaps [storage.classes] → operator runs the command → equality → marker
//
// and not the copy → verify → swap shape a migration usually has. Nothing here
// can own the swap: it observes only post-swap config, so there is no pre-swap
// state left to swap from; gc does not author city.toml; and a process that
// rewrote the operator's storage assignments would be racing config reload for
// ownership of the same file. What this can own — and does — is refusing to
// record convergence for a copy it could not prove.
//
// The consequence is an operator-visible window: between the swap and a proven
// copy, the city is configured to read a binding that does not yet hold its
// infrastructure state. A boot inside that window refuses to start and names
// the command, so no read is ever served from the wrong side of it.
//
// # Boot never migrates
//
// This file performs the move only from the operator command. Boot calls the
// check half — target resolution, convergence read, per-boot containment
// re-check, genesis — and maps what it finds to a verdict; storage_boot.go
// turns a non-serving verdict into a refusal to start.
//
// That split is the whole point. A boot that migrated unattended served
// infrastructure reads AND writes from a binding nothing had proven held the
// city's state, and the first such write made the binding non-empty, which made
// any later revert lossy and left an unstamped row that prepareInfraDestination
// would refuse on every subsequent boot. Detection cannot have that side
// effect, so the copy is not on the boot path at all.
//
// # How the revert instruction is licensed
//
// Pointing [storage.classes] back at "work" is correct on a city that never cut
// over and destructive on one that did.
//
// So the instruction is NOT derived from what a check concluded. Outcomes are
// reachable through many chains — a failed copy, a failed equality check, a
// manifest that would not rename, a marker that would not rename after a copy
// that fully succeeded — and each new chain into an outcome that names the
// revert is a new way to print a destructive instruction. Three rounds of that
// found three chains.
//
// It is derived instead from POSITIVE EVIDENCE about the binding, read after
// the fact by infraBindingHoldsNothing: a binding root this process could
// actually enumerate, holding no marker, no manifest, and a database that
// either does not exist or enumerates empty. That is the condition under which
// reverting can lose nothing, stated as a property of the binding rather than
// of any path. infraMigrationOperatorAdvice appends the revert on that evidence
// alone, so an outcome can suppress the sentence and can never grant it, and a
// chain that has not been thought of yet cannot reach it at all.
//
// The root read is the load-bearing one, and it is what the fourth round of
// this hazard turned on. The other three facts are all absences, and an absence
// only means "nothing is there" when the directory that would hold it was seen.
// A binding root that vanishes wholesale — an unmounted volume taking the
// marker, the manifest and the database with it — makes all three absences read
// true at once, which is how a converged city was offered an instruction that
// would abandon everything on that volume. Absence of everything is not proof
// of emptiness; it is usually proof that nobody looked.
//
// # Retained source
//
// The source is never mutated. There is no residue sweep and no delete-back:
// the work store keeps its infrastructure rows verbatim, so rolling back is a
// config swap with no data recovery step. That is the whole reason equality is
// a separate, fail-closed stage rather than a side effect of the copy.
//
// # Writers, and the stranded-write window
//
// A copy is only as good as the source holding still. Two mechanisms bound
// that, and neither is a byte-lock over the source, because nothing here can
// take one: the source is a store any `bd` or `gc` process can open, so "no
// writer exists" is not a decidable question from inside this file.
//
// The first mechanism is exclusion of the one writer that IS provable. A
// controller serves .gc/controller.sock for its whole life, so a live one
// answers a ping with its PID. infraMigrationForeignControllerPID asks, and a
// foreign answer is a refusal — the copy does not run against a source another
// controller is still writing.
//
// The second mechanism is detection, because the first cannot be complete.
// Any other process — `gc sling`, a mail write, a rig-side `bd` — can add an
// infrastructure bead to the work store without holding anything this migration
// can see. verifyInfraCopy's re-read turns a write landing DURING the copy into
// a refusal, but a write landing between that re-read and the marker rename is
// stranded in the retained source, and the marker would otherwise bless the
// binding as converged and make it invisible forever. So convergence is not
// re-asserted from the marker alone: confirmInfraConvergence re-checks, on
// every later boot, that every infrastructure bead the source still holds is
// readable from the binding. A strand does not become silent — it becomes a
// blocked boot naming the ids, and the beads are still intact in the retained
// source.
//
// The window is therefore not narrowed, which would only make the loss rarer.
// It is closed for the writer that can be excluded, and made loud for the
// writers that cannot.
//
// # What absence from the binding actually means
//
// "The source holds it and the binding does not" is not by itself a strand,
// and reading it as one would defeat the detector. The binding is the live
// infrastructure store after cutover, and its own lifecycle DELETES rows: wisp
// GC hard-deletes the ownership closure of every expired closed workflow root,
// and the mail retention sweep hard-deletes read message wisps. The retained
// source keeps those same rows verbatim forever by design. So on any healthy
// city the first wisp GC after cutover leaves the source holding beads the
// binding will never hold again — a permanent, entirely correct divergence. A
// check that called that a strand would fire on every healthy city, and an
// alarm that fires on every healthy city is muted, which is how the real strand
// it exists to catch goes unseen.
//
// The distinction the check needs is "the binding never received this bead"
// versus "the binding received it and its own lifecycle removed it". Neither
// the binding nor the source carries that after the fact — a deleted row leaves
// nothing behind, the provenance stamp goes with it, and the copy's own row is
// gone — so the migration RECORDS it: writeInfraCopyManifest writes the exact
// id set the equality stage proved, beside the marker and before it. Later
// boots read the manifest rather than infer.
//
// Recorded evidence rather than a timestamp cut, deliberately. A creation-time
// cut looks equivalent and is not: the work store truncates CreatedAt to second
// granularity, because its backend persists timestamps at second granularity.
// Cutting on a time therefore cannot separate a bead written just before the
// proof from one written just after, and both misreadings are harmful — one
// invents a strand on a healthy city, the other drops the exact write this
// check exists to name.
//
// # What this cannot see, stated rather than papered over
//
// One residue survives, and it is bounded rather than reported as a strand: a
// bead the copy DID deliver and the binding has since lost to something other
// than its own GC — a wipe, a bad restore, a store bug — is indistinguishable
// from a legitimate GC delete, because both leave the same evidence, which is
// none. Those are counted, not named, and surfaced only as context alongside a
// real strand. Separating them would need the binding to keep tombstones for
// rows its GC deletes, which it does not and should not.
//
// A city converged before the manifest existed has a marker and no manifest.
// That city gets no strand detection — there is nothing to check against — and
// says so rather than pretending either answer.
//
// # Cannot check is not did not converge
//
// Every read this check makes can fail on a perfectly healthy city: a busy
// database, a momentary permission fault, a manifest on a filesystem that
// hiccuped. None of those is evidence about the city. A failed re-check
// therefore reports infraMigrationUncheckable rather than non-convergence, and
// the boot refusal names the failed read instead of claiming the city never
// converged.
//
// What licenses that distinction is the marker. It is durable evidence that a
// copy completed and an equality stage passed, and a store that will not open
// right now does not retract it. Absence of proof is not proof of absence.
//
// This distinction is worth keeping for the diagnosis it gives an operator, but
// it is no longer load-bearing for their data: the revert is licensed by the
// binding's contents, not by which of these two an outcome landed on.
//
// # Genesis cities
//
// A genesis city never migrates, and two independent gates keep it dark. A city
// born on its own infrastructure store — one on an out-of-tree provider, or any
// city with no [storage] at all — does not resolve a migration target and this
// opens nothing. A city whose destination already holds rows this migration did
// not stamp is refused at the destination gate rather than overwritten.
//
// "No [storage] at all" means never configured, not un-configured: a city that
// HAS served a split and then had its section deleted is held by the served-
// binding note at the top of the boot gate, because its infrastructure state is
// in a binding no work-store reader will ever look at.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	sqlitebinding "github.com/gastownhall/gascity/internal/storebinding/sqlite"
)

// infraMigratedMarkerName is the convergence record written at the binding
// root, beside the component directory holding the database. Its presence is
// necessary but not sufficient: see readInfraConvergenceState, which refuses to
// read a marker whose database has since been removed as convergence.
const infraMigratedMarkerName = "infra.migrated"

// infraCopyManifestName is the record of exactly which beads the equality stage
// proved the binding held, one id per line, beside the marker. It is what the
// per-boot containment re-check tests absence against: without it, "the source
// has it and the binding does not" cannot be told apart from the binding's own
// GC doing its job. See "What absence from the binding actually means".
const infraCopyManifestName = "infra.migrated.beads"

// infraMigrationStampKey marks a destination row as one this migration wrote.
// The stamp — not id equality — is what tells a resumable partial attempt apart
// from content the destination owns: the source and destination mint from
// independent id sequences, so a coincidental id collision would otherwise read
// as "ours" and be deleted. Rows the source has are additionally identified by
// id, but the stamp is the necessary condition.
var infraMigrationStampKey = beadmeta.InfraMigratedFromMetadataKey

// infraMigrationClasses is the closed set of classes this migration moves. Work
// is excluded by construction — it stays on the reserved work binding.
var infraMigrationClasses = []config.StorageClass{
	config.StorageClassGraph,
	config.StorageClassSessions,
	config.StorageClassMessaging,
	config.StorageClassOrders,
	config.StorageClassNudges,
}

// infraMigrationOutcome is what one check or one migration attempt concluded.
// The distinction the caller acts on is between a city that was never asked to
// migrate and one that was asked and has not: the first is the normal state of
// every city with no split, the second means the city is configured to read a
// binding that does not hold its infrastructure state.
//
// What an outcome is NOT is the thing that licenses the revert. That is
// derived from the binding — see infraMigrationReport — because an outcome is
// reachable through many chains and every chain is a chance to reach the wrong
// instruction, while the binding's contents are the fact the instruction is
// actually about.
type infraMigrationOutcome int

const (
	// infraMigrationNotConfigured reports a city with no infrastructure split.
	// Nothing was opened, read, or written.
	infraMigrationNotConfigured infraMigrationOutcome = iota
	// infraMigrationConverged reports a city whose infrastructure classes are
	// proven to be readable from the binding — either by this attempt or an
	// earlier one.
	infraMigrationConverged
	// infraMigrationUnconverged reports a configured split this city has not
	// converged: its infrastructure state is still in the work store and no
	// marker records a proven copy.
	//
	// On the boot path it is a refusal that names the operator command. On the
	// command path it is what the attempt failed to achieve, with the reason on
	// stderr.
	infraMigrationUnconverged
	// infraMigrationStranded reports a city that DID converge and has since
	// lost that property: the binding is authoritative, and the retained source
	// holds infrastructure beads it cannot read.
	infraMigrationStranded
	// infraMigrationUncheckable reports a city this check could not decide
	// about: it may hold a proven copy right now, and nothing here proved it
	// does not.
	//
	// It exists because the other two bad outcomes are claims about the city
	// while a failed check is a fact about the check. A busy database, a
	// momentary permission fault, a manifest that could not be read — none of
	// those say the copy never happened.
	infraMigrationUncheckable
	// infraMigrationGenesis reports a configured city with nothing to move: no
	// marker, no infrastructure bead in the source, and a destination this
	// migration may create. Boot creates the binding, records an empty proven
	// copy, and serves from it.
	infraMigrationGenesis
	// infraMigrationBornSplitBlocked reports a city whose binding is served by
	// a provider this build carries no migration discipline for, and whose
	// work store holds infrastructure beads that binding cannot read. Such a
	// binding serves only under the born-split invariant — the work store
	// holds no infrastructure bead at all — and this outcome is that invariant
	// failing, with the ids named.
	infraMigrationBornSplitBlocked
	// infraMigrationGenesisBlocked reports a city whose configuration points
	// the infrastructure classes somewhere other than the binding its served
	// note records. Nothing on this path proves the configured target holds
	// what the served binding holds — genesis's premise that no marker and an
	// empty work store mean nothing exists anywhere is false for such a city —
	// so serving, genesis and migration all refuse until the operator attests
	// by removing the note.
	infraMigrationGenesisBlocked
)

// String names the outcome for operator-visible diagnostics and test failures.
func (o infraMigrationOutcome) String() string {
	switch o {
	case infraMigrationNotConfigured:
		return "not-configured"
	case infraMigrationConverged:
		return "converged"
	case infraMigrationUnconverged:
		return "unconverged"
	case infraMigrationStranded:
		return "stranded"
	case infraMigrationUncheckable:
		return "uncheckable"
	case infraMigrationGenesis:
		return "genesis"
	case infraMigrationBornSplitBlocked:
		return "born-split-blocked"
	case infraMigrationGenesisBlocked:
		return "genesis-blocked"
	}
	return fmt.Sprintf("infraMigrationOutcome(%d)", int(o))
}

// infraMigrationReport is what one check or attempt concluded AND the evidence
// the operator instruction is derived from. They are separate fields because
// they answer different questions, and three rounds of this hazard came from
// answering the second with the first.
//
// The outcome says what happened. BindingProvenEmpty says whether reverting
// [storage.classes] can lose anything — which is a property of the binding, not
// of any code path that led here. Keying the revert to an outcome made every
// new error chain into that outcome a new way to print a destructive
// instruction; keying it to the binding means a chain cannot reach it at all,
// because the chain is no longer what decides.
//
// It is filled in two halves. The inspection that reaches the verdict fills
// what only it can know — the outcome, and the stranded ids when there are
// any — and its caller completes the target and the binding evidence.
type infraMigrationReport struct {
	// Outcome is what was observed.
	Outcome infraMigrationOutcome
	// Stranded holds the sorted ids the stranded outcome found, and is empty
	// for every other outcome. It travels on the report because the refusal
	// has to NAME them: a supervisor records the refusal string and nothing
	// else, so ids carried anywhere but in the message are ids the operator
	// never sees.
	Stranded []string
	// ServedBinding, ServedProvider and ServedNotePath carry the genesis-blocked
	// evidence: which binding this city previously served its infrastructure
	// classes from, under which provider, and the note file whose removal is
	// the operator's attestation. They travel on the report for the same
	// reason Stranded does — the refusal string is the only output recorded.
	ServedBinding  string
	ServedProvider string
	ServedNotePath string
	// ProvenBeads is the size of the proven-copy manifest a serving verdict
	// rests on, and is set only by the paths that read that manifest to reach
	// their verdict. Zero on every other outcome means the size was not
	// established here, not that the copy is empty — a genesis city's zero is
	// the real answer, and it reaches this field the same way.
	ProvenBeads int
	// Fault is the read that stopped this check from reaching a verdict, and is
	// set only by the uncheckable outcome. It travels on the report for the same
	// reason Stranded does: the refusal string is the only output a supervisor
	// records and the only thing an event subscriber receives, so a fault
	// written to stderr and nowhere else is a fault they cannot see. "The reason
	// is above" is not an answer to a reader holding one line.
	Fault error
	// Target is the resolved destination the outcome is about. Its zero value
	// belongs to the outcomes that resolved nothing.
	Target infraBindingTarget
	// BindingProvenEmpty is positive evidence, read from the binding itself,
	// that the binding holds nothing a revert would abandon. Its zero value is
	// "not proven", so a path that never establishes it can only ever withhold
	// the revert. Only infraBindingHoldsNothing sets it.
	BindingProvenEmpty bool
	// BindingProbe is the fault that stopped that evidence from being
	// established, or nil. A probe that could not run is not proof of anything,
	// so it withholds the revert — and says why rather than going quiet.
	BindingProbe error
}

// serving reports whether this outcome leaves the binding safe to route reads
// and writes at. Only two do: a proven copy, and a genesis that had nothing to
// copy and recorded that fact.
func (r infraMigrationReport) serving() bool {
	return r.Outcome == infraMigrationConverged || r.Outcome == infraMigrationGenesis
}

// infraRevertInstruction is the one sentence that tells an operator to point
// [storage.classes] back at the work binding, and the only place it is spelled.
// It names the evidence in the same breath as the instruction because the
// evidence is the only thing that makes it safe: on a binding holding anything
// at all, this sentence abandons it.
const infraRevertInstruction = "The binding holds no beads, so reverting loses nothing: point [storage.classes] back at %q in city.toml and start again, or resolve the reason above."

// infraMigrationOperatorAdvice returns the operator instruction for one
// migration report, or "" when there is nothing to add.
//
// The outcome selects the SITUATION sentence. It does not select the
// instruction: every bad outcome ends with the same single decision, taken on
// report.BindingProvenEmpty, so there is exactly one place in this program that
// can emit the revert and exactly one fact that licenses it. An outcome can
// suppress the revert (by rendering no advice at all) and can never grant it.
func infraMigrationOperatorAdvice(report infraMigrationReport, logPrefix string) string {
	situation := ""
	switch report.Outcome {
	case infraMigrationUnconverged:
		if report.BindingProbe != nil {
			// The binding could not be read, so "no marker" is not a fact about
			// this city — it is a fact about a directory nobody could look
			// inside, and an unmounted volume takes the marker away exactly as
			// a city that never cut over never wrote one. So this arm asserts
			// nothing about the city, and it puts the hazard ahead of the
			// remedy: run against a bare mountpoint and the copy lands on the
			// volume that is missing, leaving two divergent infrastructure
			// stores. The command is still named, because the ordinary first
			// cutover reaches exactly this state — a binding root that nothing
			// has created yet — and an operator who knows this city never
			// migrated needs the spelling.
			situation = fmt.Sprintf("%s: [storage.classes] assign %s to binding %q, and this boot could not read that binding: %v. Nothing here says whether this city ever migrated: no convergence marker was found at %s, but an absence read from a directory this boot could not look inside is not evidence. Resolve that first — if %s is a volume that is not mounted, mount it and start again, because copying the retained work store onto a bare mountpoint leaves two divergent infrastructure stores. Boot never migrates. Once this city is known never to have cut over, the copy is:  %s",
				logPrefix, infraMigrationClassList(), report.Target.Binding, report.BindingProbe, report.Target.MarkerPath(), report.Target.Root, storageMigrationCommand)
		} else {
			situation = fmt.Sprintf("%s: [storage.classes] assign %s to binding %q, but this city has not migrated onto it: that state still lives in the work store and no convergence marker exists at %s. Boot never migrates. Run:  %s",
				logPrefix, infraMigrationClassList(), report.Target.Binding, report.Target.MarkerPath(), storageMigrationCommand)
		}
	case infraMigrationStranded:
		// The remedy names a command rather than describing one. It used to
		// describe one — "recover them into the binding's database" — and a
		// city that acquired strands was then permanently alarmed with no
		// documented way out, because the migration refuses to re-run and
		// nothing else moved a bead. See infra_class_recover.go for why the
		// repair is a separate, additive verb and not the migration again.
		situation = fmt.Sprintf("%s: this city converged on binding %q, and the retained work store holds %d infrastructure bead(s) the binding cannot read: %s. The named beads are intact in the retained work store. Stop every writer and copy them into the binding with:  %s. Re-check with `gc storage status`, which exits zero once the binding contains them.",
			logPrefix, report.Target.Binding, len(report.Stranded), infraStrandedIDList(report.Stranded), storageRecoveryInstruction())
	case infraMigrationUncheckable:
		// The fault is repeated here rather than left on stderr because this
		// sentence is what a supervisor records and what the event carries, and
		// neither of those readers has an "above" to look at. Every producer of
		// this outcome sets it today; the other arm is what a future one that
		// forgets degrades to, and it degrades to the old sentence rather than
		// to a rendered nil.
		if report.Fault != nil {
			situation = fmt.Sprintf("%s: this city's infrastructure binding %q could NOT be verified: %v. Nothing here proved it is safe to serve from.", logPrefix, report.Target.Binding, report.Fault)
		} else {
			situation = fmt.Sprintf("%s: this city's infrastructure binding %q could NOT be verified (reason above), so nothing here proved it is safe to serve from.", logPrefix, report.Target.Binding)
		}
	case infraMigrationBornSplitBlocked:
		// This arm deliberately does NOT name the recovery command. That verb
		// resolves its destination through resolveInfraBindingTarget, which
		// answers only for a binding backed by this build's own bead engine —
		// and this outcome exists precisely because the binding is served by a
		// provider this build carries no migration discipline for. Naming it
		// here would send the operator to a command that refuses.
		// TestBornSplitAdviceDoesNotNameARepairItCannotRun pins that.
		situation = fmt.Sprintf("%s: binding %q is served by a provider this build cannot migrate onto, so it serves only while the work store holds no infrastructure bead — and the work store holds %d: %s. Either an earlier configuration wrote them before this city moved to the split, or a writer without this [storage] configuration is still writing. The named beads are intact in the work store. Recover them into the binding's database with every writer stopped, then delete them from the work store — the work store was never this split's infrastructure source, and the next boot serves once it holds none. This build carries no repair command for that provider; the one it does carry serves only a binding backed by its own bead engine.",
			logPrefix, report.Target.Binding, len(report.Stranded), infraStrandedIDList(report.Stranded))
	case infraMigrationGenesisBlocked:
		if report.ServedProvider == "" {
			// The note exists but could not be read. It is still evidence
			// that some binding served this city's infrastructure classes,
			// so it holds exactly as a readable note would.
			situation = fmt.Sprintf("%s: this city's served-binding note (%s) exists but cannot be read, and an unreadable note must hold exactly as a readable one would: some binding served this city's infrastructure classes, and re-pointing them elsewhere would make every bead written there permanently invisible. Repair or inspect the note; removing it is the operator's attestation that the previously served binding's contents are recovered or deliberately abandoned.",
				logPrefix, report.ServedNotePath)
		} else {
			situation = fmt.Sprintf("%s: this city's infrastructure classes are served from binding %q (provider %q), and this configuration points them somewhere else. Nothing here proves the configured target holds what the served binding holds, so this refuses rather than risk making every bead written there invisible. Verify or recover the served binding's contents first; removing %s is the operator's attestation that they are recovered, still reachable through the new configuration, or deliberately abandoned.",
				logPrefix, report.ServedBinding, report.ServedProvider, report.ServedNotePath)
		}
		// Neither canned tail applies: the revert grant is evidence about the
		// NEW target and says nothing about the served binding this outcome
		// is protecting, and the do-not-revert tail names the wrong hazard.
		// The withholding is explicit instead.
		return situation + fmt.Sprintf(" Do NOT revert [storage.classes] to %q either: new infrastructure writes would land in the work store while the served binding's contents stay unrecovered.", config.StorageWorkBinding)
	default:
		return ""
	}
	if report.BindingProvenEmpty {
		return situation + " " + fmt.Sprintf(infraRevertInstruction, config.StorageWorkBinding)
	}
	because := ""
	if report.BindingProbe != nil {
		because = fmt.Sprintf(" (%v)", report.BindingProbe)
	}
	return situation + fmt.Sprintf(" Do NOT revert [storage.classes] to %q: the binding is not provably empty%s, so the revert would abandon every infrastructure bead that exists only there. Resolve the reason above and start again.",
		config.StorageWorkBinding, because)
}

// infraMigrationClassList names the classes this migration moves, in the order
// they are declared, for operator-facing text.
func infraMigrationClassList() string {
	names := make([]string, 0, len(infraMigrationClasses))
	for _, class := range infraMigrationClasses {
		names = append(names, class.String())
	}
	return strings.Join(names, "/")
}

// openInfraMigrationSource opens the work store the infrastructure beads are
// copied out of. Overridden by tests.
var openInfraMigrationSource = func(cityPath string) (beads.Store, error) {
	return openStoreAtForCity(cityPath, cityPath)
}

// openInfraDestination opens the binding's Beads engine at the component path
// and under the reserved id prefix the deployed provider uses. It is not a test
// seam: a stubbed destination would let the migration pass while writing
// somewhere no runtime binding reads, which is the exact defect this opener
// exists to prevent, so tests exercise the production opener at a temporary
// binding root.
func openInfraDestination(target infraBindingTarget) (beads.Store, error) {
	prefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok || prefix == "" {
		return nil, fmt.Errorf("no reserved id prefix is registered for the %q class", config.BeadClassGraph)
	}
	return beads.OpenSQLiteStore(target.Dir, beads.WithSQLiteStoreIDPrefix(prefix))
}

// infraMigrationRename is the atomic publish step shared by the copy manifest
// and the convergence marker. Overridden by tests.
//
// It is a seam rather than a direct os.Rename call because the failure it can
// have — a rename that does not land after the copy AND the equality stage have
// both passed, leaving a fully populated binding with no record of it — is the
// state that produced the third round of the revert hazard, and no arrangement
// of the filesystem reproduces it: every way to make a rename onto the marker
// path fail also makes that path stat as present, which sends the boot down the
// already-converged branch instead of the one under test.
var infraMigrationRename = os.Rename

// infraMigrationForeignControllerPID reports the PID of a controller other than
// this process that is live on the city, or 0 when none answers.
//
// This process is excluded by PID rather than trusted to be absent: the
// standalone path starts its own controller socket before it calls
// newCityRuntime, so an unfiltered ping would return our own PID and the
// migration would refuse on its own liveness. A PID that is not ours is
// another controller, and it is writing.
//
// The ping is the seam rather than the whole answer, so the self-exclusion —
// the part that is easy to get backwards and impossible to notice, because
// getting it wrong makes every standalone boot refuse — is itself under test.
func infraMigrationForeignControllerPID(cityPath string) int {
	pid := infraMigrationControllerPing(cityPath)
	if pid == os.Getpid() {
		return 0
	}
	return pid
}

// infraMigrationControllerPing reports the PID serving the city's controller
// socket, or 0 when nothing answers. Overridden by tests.
var infraMigrationControllerPing = controllerAlive

// infraStrandedIDsReported caps how many stranded ids the operator message
// names before it summarizes the rest. The count is the alarm; the ids are the
// starting point for recovering them by hand.
const infraStrandedIDsReported = 5

// infraStrandedIDList renders stranded ids for an operator message: the first
// infraStrandedIDsReported by id, then a count of the rest. One spelling, so
// the stderr report and the refusal that carries the ids cannot drift apart.
func infraStrandedIDList(ids []string) string {
	if len(ids) <= infraStrandedIDsReported {
		return strings.Join(ids, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(ids[:infraStrandedIDsReported], ", "), len(ids)-infraStrandedIDsReported)
}

// infraBindingTarget names the resolved destination of an infra split.
type infraBindingTarget struct {
	// Binding is the [storage.bindings.<name>] key every infrastructure class selects.
	Binding string
	// Root is the canonical binding root, the configured path with every
	// symlink resolved.
	Root string
	// Dir is the canonical component directory the database lives in — what
	// beads.OpenSQLiteStore is handed.
	Dir string
	// Database is the canonical database file the runtime binding opens.
	Database string
}

// MarkerPath returns the convergence marker for this target.
func (t infraBindingTarget) MarkerPath() string {
	return filepath.Join(t.Root, infraMigratedMarkerName)
}

// ManifestPath returns the proven-copy manifest for this target.
func (t infraBindingTarget) ManifestPath() string {
	return filepath.Join(t.Root, infraCopyManifestName)
}

// resolveInfraBindingTarget reports the binding that owns all five
// infrastructure classes. ok=false with no error means the city is not
// configured for the split; an error means it is configured and the destination
// could not be resolved, which is a refusal rather than a reason to run dark.
//
// The split is recognized only in its whole form: work on the reserved work
// binding, and all five infrastructure classes on one shared non-work binding
// backed by the built-in bead engine. A partial or per-class fan-out is not a
// shape this migration can converge — the five classes are one bead scope
// reached through one set of adapters, not five — so it is reported as
// not-configured rather than half-migrated, and storage_boot.go refuses to
// serve it rather than routing part of it.
func resolveInfraBindingTarget(cityPath string, cfg *config.City) (infraBindingTarget, bool, error) {
	if cfg == nil || cityPath == "" {
		return infraBindingTarget{}, false, nil
	}
	storage := cfg.EffectiveStorage()
	if storage.Classes.Work != config.StorageWorkBinding {
		return infraBindingTarget{}, false, nil
	}
	name := storage.Classes.BindingFor(infraMigrationClasses[0])
	if name == "" || name == config.StorageWorkBinding {
		return infraBindingTarget{}, false, nil
	}
	for _, class := range infraMigrationClasses[1:] {
		if storage.Classes.BindingFor(class) != name {
			return infraBindingTarget{}, false, nil
		}
	}
	binding, ok := storage.Bindings[name]
	if !ok || binding.Provider != config.StorageProviderSQLiteBeads {
		return infraBindingTarget{}, false, nil
	}
	path := binding.Path
	if path == "" {
		path = config.DefaultSQLiteStoragePath
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cityPath, path)
	}
	// The provider's own resolver, not a local join: the destination has to be
	// the file the runtime binding opens, canonicalized the same way, or the
	// copy lands in an orphan database.
	database, err := sqlitebinding.GraphPath(path)
	if err != nil {
		return infraBindingTarget{}, false, fmt.Errorf("resolving the destination of binding %q: %w", name, err)
	}
	dir := filepath.Dir(database)
	return infraBindingTarget{
		Binding:  name,
		Root:     filepath.Dir(dir),
		Dir:      dir,
		Database: database,
	}, true, nil
}

// checkInfraClassConvergence decides, without moving a single bead, whether a
// city may serve its infrastructure classes from the binding its config names.
// It returns both what it concluded and the evidence the operator instruction
// is derived from.
//
// The two are computed by different code deliberately. The verdict can be
// reached through many chains; the evidence is then read from the binding
// afterwards, by one function that looks at nothing else. So no chain — present
// or future — can talk the boot path into naming the revert: the chain decides
// what happened, and the binding decides what the operator is told to do about
// it.
//
// Nothing here writes to the source, and the only thing it may write to the
// destination is a genesis city's empty proven copy and marker, which record
// that a city with nothing to move has nothing to move. That is the zero-row
// degenerate of the copy the operator command performs, not a copy of its own.
func checkInfraClassConvergence(cityPath string, cfg *config.City, logPrefix string, stderr io.Writer) infraMigrationReport {
	target, ok, err := resolveInfraBindingTarget(cityPath, cfg)
	if err != nil {
		// The destination did not resolve, so there is no binding to read
		// anything off — neither the marker that says whether this city cut
		// over nor the rows that say whether a revert would lose them. The
		// zero-value report stands, and it withholds the revert.
		fmt.Fprintf(stderr, "%s: storage class migration: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return infraMigrationReport{Outcome: infraMigrationUncheckable, Fault: err}
	}
	if !ok {
		return infraMigrationReport{Outcome: infraMigrationNotConfigured}
	}
	report := inspectInfraConvergence(cityPath, target, logPrefix, stderr)
	report.Target = target
	report.BindingProvenEmpty, report.BindingProbe = infraBindingHoldsNothing(target)
	return report
}

// inspectInfraConvergence maps what the binding and the source say to a verdict,
// as the inspection half of a report: the outcome and, when the binding has
// stranded writes, the ids the refusal must name.
//
// The marker is read first, once, because it decides which instruction a later
// failure is allowed to carry: the revert is safe on a city that never
// converged and destroys a city that did. A city with a marker gets its
// convergence re-proved; a city without one is either a genesis (nothing to
// move, so it is admitted and recorded) or a city whose infrastructure state is
// still in the work store, which is the refusal that names the command.
func inspectInfraConvergence(cityPath string, target infraBindingTarget, logPrefix string, stderr io.Writer) infraMigrationReport {
	say := func(outcome infraMigrationOutcome, err error) infraMigrationReport {
		fmt.Fprintf(stderr, "%s: storage class migration: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		// The fault rides along so the outcome that could not decide can say what
		// stopped it, wherever that outcome is later rendered.
		return infraMigrationReport{Outcome: outcome, Fault: err}
	}

	if blocked, ok := servedBindingNoteHold(cityPath, target.Binding, config.StorageProviderSQLiteBeads, target.Database); ok {
		return blocked
	}

	state, err := readInfraConvergenceState(target)
	if err != nil {
		return say(infraMigrationUncheckable, err)
	}
	switch state {
	case infraConvergenceMarked:
		return confirmInfraConvergence(cityPath, target, logPrefix, stderr)
	case infraConvergenceStale:
		// The marker's claim about the past still holds, so this is not a city
		// that never converged and the revert stays withheld by the marker the
		// evidence probe can still see. Its claim about the present does not
		// hold, and only the operator command may act on that.
		return say(infraMigrationUncheckable, fmt.Errorf("%s claims convergence but %s is gone; the binding cannot serve until the migration runs again",
			target.MarkerPath(), target.Database))
	}

	// No marker. Either this city has infrastructure beads in the work store —
	// in which case only the operator command may move them — or it has none,
	// which is a genesis: the copy would move zero rows, prove equality
	// vacuously, and record it. Doing exactly that here costs nothing and is
	// what lets a brand-new city with a [storage] section start.
	source, err := openInfraMigrationSource(cityPath)
	if err != nil {
		return say(infraMigrationUnconverged, fmt.Errorf("opening the work store to census infrastructure beads: %w", err))
	}
	defer closeBeadStoreHandle(source) //nolint:errcheck // best-effort close
	rows, err := readInfraSnapshot(source)
	if err != nil {
		return say(infraMigrationUnconverged, err)
	}
	if len(rows) > 0 {
		return infraMigrationReport{Outcome: infraMigrationUnconverged}
	}
	if err := recordInfraGenesis(target); err != nil {
		return say(infraMigrationUnconverged, err)
	}
	return infraMigrationReport{Outcome: infraMigrationGenesis}
}

// recordInfraGenesis admits and records a city that has nothing to migrate: it
// creates the destination, refuses it if anything else already owns rows there,
// and writes the empty proven copy and the marker in that order.
//
// The manifest is empty rather than absent on purpose. An absent manifest turns
// the per-boot containment re-check off for the city's whole life; an empty one
// says "the copy delivered nothing, because there was nothing", which is the
// truth and keeps the strand detector armed from the first boot.
func recordInfraGenesis(target infraBindingTarget) error {
	destination, err := openInfraDestination(target)
	if err != nil {
		return infraGenesisOpenFailure(target, err)
	}
	if err := prepareInfraDestination(destination); err != nil {
		_ = closeBeadStoreHandle(destination)
		return err
	}
	if err := closeBeadStoreHandle(destination); err != nil {
		return fmt.Errorf("closing binding %q at %s: %w", target.Binding, target.Database, err)
	}
	if err := writeInfraCopyManifest(target, nil); err != nil {
		return err
	}
	return writeInfraMigratedMarker(target)
}

// infraGenesisOpenFailure explains a destination that would not open during
// genesis.
//
// One of its shapes is a race rather than a fault: two processes boot the same
// genesis city at the same moment, both find no marker and nothing to move, and
// both create the same database. The loser's driver reports a table that
// already exists, which as a bare error tells an operator nothing about what
// happened or what to do — and what to do is nothing, because the winner
// records the empty proven copy the loser was about to record.
//
// The race is recognized by the driver's own "already exists" text: the store's
// opener wraps a driver error and neither side of that boundary carries a
// sentinel for it. Text matching degrades safely here — an unrecognized race
// falls through to the plain open failure, which is loud rather than silent.
func infraGenesisOpenFailure(target infraBindingTarget, err error) error {
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "already exists") {
		return fmt.Errorf("another process created binding %q at %s at the same moment as this one, and this boot lost the race: %w. Nothing was lost — the process that won records the empty proven copy this city needs. Start the city again and it will serve from the binding the winner created", target.Binding, target.Database, err)
	}
	return fmt.Errorf("creating binding %q at %s: %w", target.Binding, target.Database, err)
}

// infraBindingHoldsNothing reports POSITIVE evidence that reverting
// [storage.classes] to the work binding would abandon nothing, by reading the
// binding rather than by reasoning about how this boot got here.
//
// Four facts, all read from the binding root, and all four required:
//
//   - The binding root exists, is a directory, and this process can enumerate
//     it. This one comes first because it is what makes the other three mean
//     anything: they are all statements about absence, and absence is only
//     evidence when the place the thing would be is somewhere this boot could
//     look. See infraBindingRootEnumerable.
//   - No convergence marker. A marker is the binding's own record that it has
//     been in service, and a binding that has been in service may hold writes
//     the retained source never saw — including on a volume that is not
//     mounted right now, which is why a missing database does not overrule a
//     present marker.
//   - No copy manifest. The manifest is written before the marker, so a
//     manifest with no marker is a cutover that got as far as a proven copy.
//     The rows are in the binding even though nothing recorded convergence.
//   - The database holds no beads: either the file does not exist within that
//     readable root, so nothing can be reading or writing it, or it opens and
//     enumerates empty.
//
// Any error is not proof and is returned rather than swallowed: false with a
// cause withholds the revert AND tells the operator which read failed.
func infraBindingHoldsNothing(target infraBindingTarget) (bool, error) {
	if err := infraBindingRootEnumerable(target.Root); err != nil {
		return false, err
	}
	for _, record := range []struct{ what, path string }{
		{"convergence marker", target.MarkerPath()},
		{"copy manifest", target.ManifestPath()},
	} {
		present, err := infraPathExists(record.path)
		if err != nil {
			return false, fmt.Errorf("reading the binding %s %s: %w", record.what, record.path, err)
		}
		if present {
			return false, nil
		}
	}
	present, err := infraPathExists(target.Database)
	if err != nil {
		return false, fmt.Errorf("reading the binding database %s: %w", target.Database, err)
	}
	if !present {
		return true, nil
	}
	// Opened only when the file is already there. Opening would create it, and
	// a probe that creates the database it is asked about would be answering
	// its own question.
	store, err := openInfraDestination(target)
	if err != nil {
		return false, fmt.Errorf("opening the binding %q at %s: %w", target.Binding, target.Database, err)
	}
	defer closeBeadStoreHandle(store) //nolint:errcheck // best-effort close
	rows, err := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return false, fmt.Errorf("listing the binding %s: %w", target.Database, err)
	}
	return len(rows) == 0, nil
}

// infraBindingCensus counts what the binding holds right now, without creating
// it.
//
// The retained source cannot answer whether a cutover landed: the migration
// copies and keeps, so the source census reads the same before and after a
// successful one. The manifest cannot answer it either — it records what the
// copy was proven to deliver at cutover, which is the past. Only the binding's
// own count says what is being served today, and the two diverge the moment
// anything writes to or reaps from the binding.
//
// The root read comes first, exactly as it does in the evidence probe, and for
// the same reason. "The database file is not there" is only a count of zero if
// the directory that would hold it was observed; an unmounted volume takes the
// database away with the whole root, so a confident "binding: 0 held now" would
// be the same positive-looking absence the probe exists to refuse — handed this
// time to a human deciding whether a cutover landed. The caller renders the
// fault in place of the number rather than a number nobody could take.
//
// Below a readable root, an absent database is zero rather than an error, for
// the same reason infraBindingHoldsNothing refuses to open one: opening creates
// it, and a report that creates the database it is asked about would leave a
// store behind on a city that never cut over.
func infraBindingCensus(target infraBindingTarget) (int, error) {
	if err := infraBindingRootEnumerable(target.Root); err != nil {
		return 0, err
	}
	present, err := infraPathExists(target.Database)
	if err != nil {
		return 0, fmt.Errorf("reading the binding database %s: %w", target.Database, err)
	}
	if !present {
		return 0, nil
	}
	store, err := openInfraDestination(target)
	if err != nil {
		return 0, fmt.Errorf("opening the binding %q at %s: %w", target.Binding, target.Database, err)
	}
	defer closeBeadStoreHandle(store) //nolint:errcheck // best-effort close
	rows, err := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return 0, fmt.Errorf("listing the binding %s: %w", target.Database, err)
	}
	return len(rows), nil
}

// infraBindingRootEnumerable reports whether this boot could look inside the
// binding root at all, and is the precondition on every absence read under it.
//
// The distinction it draws is "I looked and there is nothing there" versus "I
// could not look". Every other fact the evidence probe reads is an absence — no
// marker, no manifest, no database — and an absence is only evidence when the
// directory that would hold it was itself observed. If the root vanishes
// wholesale, an unmounted volume takes the marker, the manifest AND the
// database away together, so all three absences read true at once and a
// converged city looks indistinguishable from one that never cut over. That is
// the exact shape of the fourth round of the revert hazard: three positive-
// looking facts, all of them artifacts of the same missing directory.
//
// So a root that is missing, is not a directory, or cannot be enumerated is
// reported as a fault. The evidence stays unproven, the revert is withheld, and
// the operator is told which read failed rather than being handed an
// instruction derived from a directory nobody could see.
//
// # What this does and does not separate
//
// It separates "the binding is not here right now" from "the binding is here
// and holds nothing". It does NOT separate every genesis city from every
// unmounted one, and the residue is stated rather than papered over:
//
//   - A city that never cut over and never opened its destination has no root,
//     because openInfraDestination — the only thing that creates it — never
//     ran. That city now gets "cannot check" instead of the revert. It is the
//     safe answer and it is recoverable: resolving the named reason lets the
//     next boot open the destination, which creates the root, which makes the
//     evidence readable.
//   - A bare mountpoint that is present, world-readable and empty is
//     indistinguishable from a genesis root that was created and never written.
//     Both are a readable empty directory, and nothing on either says which. A
//     mountpoint whose permissions keep this process out is caught here; one
//     that does not is not. Closing that residue needs evidence outside the
//     binding — a fact about the city rather than about the volume — which this
//     migration deliberately does not keep.
func infraBindingRootEnumerable(root string) error {
	info, err := os.Stat(root)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("the binding root %s does not exist, which reads the same whether this city never cut over or its binding is not mounted right now", root)
	}
	if err != nil {
		return fmt.Errorf("reading the binding root %s: %w", root, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("the binding root %s is not a directory, so nothing under it could be read", root)
	}
	// Stat answers whether the root is there; only a read answers whether this
	// process can see what is in it. A directory it cannot enumerate reports
	// every path under it as absent.
	if _, err := os.ReadDir(root); err != nil {
		return fmt.Errorf("listing the binding root %s: %w", root, err)
	}
	return nil
}

// runInfraClassMigration is the migration proper, run from the operator command
// against an already resolved destination: it converges the binding or refuses,
// and reports which as the inspection half of a report.
//
// The marker is written only after copy AND equality both succeed, so a city
// that has never carried one has not proven convergence — which is the whole
// content of infraMigrationUnconverged. A failure on a city that DOES carry a
// marker says nothing of the kind, so it reports infraMigrationUncheckable
// instead. The marker is read up front, once, for exactly that reason.
//
// Nothing here decides what the operator is told to DO. This function's job is
// to say what happened and why, on stderr; the revert is decided from the
// binding afterwards, by infraBindingHoldsNothing.
func runInfraClassMigration(cityPath string, target infraBindingTarget, logPrefix string, stderr io.Writer) infraMigrationReport {
	say := func(outcome infraMigrationOutcome, err error) infraMigrationReport {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		// The fault rides along so the outcome that could not decide can say what
		// stopped it, wherever that outcome is later rendered.
		return infraMigrationReport{Outcome: outcome, Fault: err}
	}

	// A served-binding note naming any other binding is a hold on this whole
	// command: a copy of the work store's slice would bless a destination
	// that silently omits everything the served binding holds. The note's
	// removal is the operator's attestation, checked before anything opens.
	if blocked, ok := servedBindingNoteHold(cityPath, target.Binding, config.StorageProviderSQLiteBeads, target.Database); ok {
		return blocked
	}

	state, err := readInfraConvergenceState(target)
	if err != nil {
		return say(infraMigrationUncheckable, err)
	}

	// A city with no marker has not proven it ever served from the binding, so
	// a failure here is a claim about the city: it is not converged. A city
	// that has one converged at some point — that fact is durable, and it does
	// not become false because this attempt could not re-read a store — so its
	// failures are reported as facts about the check instead.
	fail := func(err error) infraMigrationReport {
		if state == infraConvergenceAbsent {
			return say(infraMigrationUnconverged, err)
		}
		return say(infraMigrationUncheckable, err)
	}

	switch state {
	case infraConvergenceMarked:
		return confirmInfraConvergence(cityPath, target, logPrefix, stderr)
	case infraConvergenceStale:
		fmt.Fprintf(stderr, "%s: %s claims convergence but %s is gone; re-running the copy\n", //nolint:errcheck // best-effort stderr
			logPrefix, target.MarkerPath(), target.Database)
	}

	// The one writer exclusion this can prove. A source another controller is
	// still writing cannot be copied provably, so a foreign controller is a
	// refusal before anything opens.
	if pid := infraMigrationForeignControllerPID(cityPath); pid != 0 {
		return fail(fmt.Errorf("controller PID %d is live on this city and is still writing infrastructure beads to the work store; the copy cannot be proven against a source under mutation. Stop it (gc stop) and start again", pid))
	}

	source, err := openInfraMigrationSource(cityPath)
	if err != nil {
		return fail(fmt.Errorf("opening work store: %w", err))
	}
	defer closeBeadStoreHandle(source) //nolint:errcheck // best-effort close

	writer, err := openInfraDestination(target)
	if err != nil {
		return fail(fmt.Errorf("opening binding %q at %s: %w", target.Binding, target.Database, err))
	}
	// The abort path. The success path closes the writer explicitly, before the
	// equality stage, and clears it so this does not run twice.
	defer func() { _ = closeBeadStoreHandle(writer) }()

	rows, err := readInfraSnapshot(source)
	if err != nil {
		return fail(err)
	}
	// Before the destination is touched, so a refused city can re-run from
	// empty once the payload can be carried.
	if err := infraSourceEdgePayloadRefusal(source, rows); err != nil {
		return fail(err)
	}
	if err := prepareInfraDestination(writer); err != nil {
		return fail(err)
	}
	imported, err := importInfraSnapshot(writer, source, rows)
	if err != nil {
		return fail(err)
	}
	// Equality is proven against the database, not against the connection that
	// just wrote it. Closing the writer first is what makes the proof about
	// durable bytes: a handle that is still open can answer out of state the
	// file does not carry, and the runtime binding opens the file.
	if err := closeBeadStoreHandle(writer); err != nil {
		return fail(fmt.Errorf("closing binding %q at %s after the copy: %w", target.Binding, target.Database, err))
	}
	writer = nil
	proven, err := verifyInfraCopy(func() (beads.Store, error) { return openInfraDestination(target) }, source)
	if err != nil {
		return fail(fmt.Errorf("equality check: %w", err))
	}
	// Before the marker, so a marker never exists without the manifest that
	// makes its convergence claim re-checkable.
	if err := writeInfraCopyManifest(target, proven); err != nil {
		return fail(err)
	}
	if err := writeInfraMigratedMarker(target); err != nil {
		return fail(err)
	}
	fmt.Fprintf(stderr, "%s: infrastructure classes migrated to %s (%d beads copied, source retained)\n", //nolint:errcheck // best-effort stderr
		logPrefix, target.Database, imported)
	// The manifest's size rather than the import count: the manifest is what
	// every later verdict re-reads, so reporting anything else here would make
	// the cutover's own event disagree with every boot that follows it.
	return infraMigrationReport{Outcome: infraMigrationConverged, ProvenBeads: len(proven)}
}

// infraConvergenceState is what the binding root says about whether this city
// has ever cut over. It is read before anything else runs, because it decides
// which instruction a later failure is allowed to carry: the revert is safe on
// a city that never converged and destroys a city that did.
type infraConvergenceState int

const (
	// infraConvergenceAbsent reports no marker: this city has never converged,
	// and its infrastructure state is still readable from the retained source.
	infraConvergenceAbsent infraConvergenceState = iota
	// infraConvergenceMarked reports a marker whose database is present — a
	// city that converged and is being served from the binding.
	infraConvergenceMarked
	// infraConvergenceStale reports a marker whose database is gone. The city
	// converged at some point, so the marker's claim about the past still
	// holds; only its claim about the present does not.
	infraConvergenceStale
)

// readInfraConvergenceState reports whether an earlier boot already converged
// this city, and whether the marker it found is stale.
//
// The marker alone is not the answer. It sits at the binding root while the
// data sits in the component directory below it, so removing the database — a
// wipe, a restore, a hand-edit — leaves a marker that would otherwise bless an
// empty destination as migrated. A marker with no database is reported stale
// and the copy runs again; the destination is empty, so it simply re-converges.
//
// An error is never a state. Neither a marker that could not be read nor a
// database that could not be stat'ed says the city failed to converge, and the
// caller treats both as undecided rather than as absence.
func readInfraConvergenceState(target infraBindingTarget) (infraConvergenceState, error) {
	marked, err := infraPathExists(target.MarkerPath())
	if err != nil {
		return infraConvergenceAbsent, fmt.Errorf("reading the convergence marker: %w", err)
	}
	if !marked {
		return infraConvergenceAbsent, nil
	}
	present, err := infraPathExists(target.Database)
	if err != nil {
		return infraConvergenceAbsent, fmt.Errorf("reading the binding database %s: %w", target.Database, err)
	}
	if present {
		return infraConvergenceMarked, nil
	}
	return infraConvergenceStale, nil
}

// confirmInfraConvergence re-proves a city an earlier boot already converged.
//
// The marker records that the binding once held the source's infrastructure
// slice. It cannot record that it still does, and the gap matters in exactly
// the case the copy could not fence: a write that landed in the retained source
// between the equality re-read and the marker rename is absent from the
// binding, and every read after cutover routes past it. Left at the marker,
// that bead is invisible forever. Re-checking containment on each boot is what
// makes it loud instead.
//
// Containment, not equality: the binding legitimately grows after cutover with
// beads the source never had, so only the source-to-binding direction is a
// defect. And within that direction, only the beads the proven copy never
// carried are a defect — the binding's own GC hard-deletes rows the retained
// source keeps forever, so absence is classified against the recorded copy
// manifest rather than taken at face value. What that can and cannot separate
// is set out at the top of this file.
//
// Every way this re-check can fail is a fact about the check, not about the
// city. The city on this path converged: its marker and its database are both
// present and it is being served from the binding. So a re-check that cannot
// run degrades — infraMigrationUncheckable, naming the fault — instead of
// reporting the city unconverged and handing it the revert, which is the one
// instruction that would abandon everything written since cutover.
func confirmInfraConvergence(cityPath string, target infraBindingTarget, logPrefix string, stderr io.Writer) infraMigrationReport {
	proven, recorded, err := readInfraCopyManifest(target)
	if err != nil {
		return reportUncheckableConvergence(target, logPrefix, stderr, err)
	}
	if !recorded {
		// No manifest, no check. Every absence from the binding would have to be
		// guessed at, and both guesses are wrong: calling them strands blocks a
		// healthy garbage-collected city forever, calling them GC hides the one
		// write this exists to name. Say which it is instead.
		fmt.Fprintf(stderr, "%s: %s converged before %s was recorded, so stranded-write detection is OFF for this city. Nothing distinguishes a bead the copy never carried from one the binding's own GC has since collected. Re-converge the binding to restore the check.\n", //nolint:errcheck // best-effort stderr
			logPrefix, target.Database, target.ManifestPath())
		return infraMigrationReport{
			Outcome: infraMigrationUncheckable,
			Fault: fmt.Errorf("%s converged before %s was recorded, so stranded-write detection is off for this city; re-converge the binding to restore the check",
				target.Database, target.ManifestPath()),
		}
	}
	gap, err := classifyInfraContainmentGap(cityPath, target, proven)
	if err != nil {
		return reportUncheckableConvergence(target, logPrefix, stderr, err)
	}
	if len(gap.Stranded) == 0 {
		return infraMigrationReport{Outcome: infraMigrationConverged, ProvenBeads: len(proven)}
	}
	// The removed-since count is context for reading the strand, not a second
	// alarm: on a city whose wisp GC has run it is large and entirely expected,
	// which is why it is never reported on its own.
	residue := ""
	if gap.RemovedSinceCutover > 0 {
		residue = fmt.Sprintf(" (a further %d bead(s) the copy did deliver are no longer in the binding, which is what the binding's own GC does to expired closed wisps and read mail; they are not counted as stranded)", gap.RemovedSinceCutover)
	}
	// What to DO about it is not decided here. This names the defect; the single
	// revert decision (infraMigrationOperatorAdvice, on evidence read from the
	// binding) is what follows it.
	fmt.Fprintf(stderr, "%s: %d infrastructure bead(s) in the retained work store were never carried by the proven copy and are NOT in the converged binding %s: %s%s. A writer this migration could not fence wrote them to the source during or after the cutover. They are intact in the work store. Stop every writer and copy the listed beads into the binding with:  %s\n", //nolint:errcheck // best-effort stderr
		logPrefix, len(gap.Stranded), target.Database, infraStrandedIDList(gap.Stranded), residue, storageRecoveryInstruction())
	return infraMigrationReport{Outcome: infraMigrationStranded, Stranded: gap.Stranded}
}

// reportUncheckableConvergence reports a converged city whose convergence could
// not be re-proved, and says which of the two things the operator is looking
// at: the city is fine as far as anything here knows, and the CHECK is what
// failed.
//
// The marker is what makes that assertable. It is durable evidence that the
// copy completed and the equality stage passed, and a store that would not
// open a minute ago does not retract it. So the message names the fault rather
// than claiming the copy never happened — and the boot still refuses, because
// a check that could not run is not permission to serve.
func reportUncheckableConvergence(target infraBindingTarget, logPrefix string, stderr io.Writer, cause error) infraMigrationReport {
	fmt.Fprintf(stderr, "%s: %s converged (%s records it) and this could NOT be re-checked for stranded writes: %v. That is a failure of the check, not evidence the copy never happened. Resolve the fault and start again.\n", //nolint:errcheck // best-effort stderr
		logPrefix, target.Database, target.MarkerPath(), cause)
	return infraMigrationReport{
		Outcome: infraMigrationUncheckable,
		Fault: fmt.Errorf("%s converged (%s records it) and the stranded-write re-check could not run: %w",
			target.Database, target.MarkerPath(), cause),
	}
}

// infraContainmentGap is what one containment re-check found: the source infra
// beads the binding cannot read, split by whether the copy is known to have
// delivered them.
type infraContainmentGap struct {
	// Stranded holds the sorted ids of source beads the proven copy never
	// carried: they are absent from the binding AND absent from the manifest of
	// what the equality stage proved. The binding never received them, so no
	// lifecycle of its own could have removed them. These are the defect.
	Stranded []string
	// RemovedSinceCutover counts source beads the manifest records as delivered
	// that the binding no longer holds. The copy demonstrably put them there, so
	// their absence is a binding-side removal — overwhelmingly wisp GC and mail
	// retention, which hard-delete rows the retained source keeps forever. A
	// removal from any other cause leaves identical evidence and is therefore
	// inside this count rather than distinguished from it.
	RemovedSinceCutover int
}

// classifyInfraContainmentGap classifies every source infrastructure bead the binding
// cannot read against the manifest of what the copy was proven to deliver. It
// opens both stores read-only and creates nothing: a converged city must not be
// mutated by its own convergence check.
func classifyInfraContainmentGap(cityPath string, target infraBindingTarget, proven map[string]bool) (infraContainmentGap, error) {
	source, err := openInfraMigrationSource(cityPath)
	if err != nil {
		return infraContainmentGap{}, fmt.Errorf("opening work store: %w", err)
	}
	defer closeBeadStoreHandle(source) //nolint:errcheck // best-effort close

	rows, err := readInfraSnapshot(source)
	if err != nil {
		return infraContainmentGap{}, err
	}
	if len(rows) == 0 {
		return infraContainmentGap{}, nil
	}

	destination, err := openInfraDestination(target)
	if err != nil {
		return infraContainmentGap{}, fmt.Errorf("opening binding %q at %s: %w", target.Binding, target.Database, err)
	}
	defer closeBeadStoreHandle(destination) //nolint:errcheck // best-effort close

	copied, err := destination.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return infraContainmentGap{}, fmt.Errorf("listing binding: %w", err)
	}
	have := make(map[string]bool, len(copied))
	for _, b := range copied {
		have[b.ID] = true
	}
	gap := infraContainmentGap{}
	for _, b := range rows {
		if have[b.ID] {
			continue
		}
		if proven[b.ID] {
			gap.RemovedSinceCutover++
			continue
		}
		gap.Stranded = append(gap.Stranded, b.ID)
	}
	sort.Strings(gap.Stranded)
	return gap, nil
}

// writeInfraCopyManifest records the ids the equality stage proved the binding
// held. It is written atomically (temp + rename) and before the marker, so a
// marker never claims convergence the manifest cannot substantiate and a crash
// mid-write cannot leave a truncated manifest that reads as complete.
func writeInfraCopyManifest(target infraBindingTarget, ids []string) error {
	if err := os.MkdirAll(target.Root, 0o755); err != nil {
		return fmt.Errorf("writing the infra copy manifest: %w", err)
	}
	tmp, err := os.CreateTemp(target.Root, infraCopyManifestName+".tmp-*")
	if err != nil {
		return fmt.Errorf("writing the infra copy manifest: %w", err)
	}
	name := tmp.Name()
	cleanup := func(cause error) error {
		_ = os.Remove(name)
		return fmt.Errorf("writing the infra copy manifest: %w", cause)
	}
	writer := bufio.NewWriter(tmp)
	for _, id := range ids {
		if _, err := fmt.Fprintln(writer, id); err != nil {
			_ = tmp.Close()
			return cleanup(err)
		}
	}
	if err := writer.Flush(); err != nil {
		_ = tmp.Close()
		return cleanup(err)
	}
	if err := tmp.Close(); err != nil {
		return cleanup(err)
	}
	if err := infraMigrationRename(name, target.ManifestPath()); err != nil {
		return cleanup(err)
	}
	return nil
}

// readInfraCopyManifest returns the id set the copy was proven to deliver.
// recorded=false with no error means the city converged before the manifest
// existed, which is the one state in which absence cannot be classified at all.
func readInfraCopyManifest(target infraBindingTarget) (proven map[string]bool, recorded bool, err error) {
	contents, err := os.ReadFile(target.ManifestPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("reading the infra copy manifest %s: %w", target.ManifestPath(), err)
	}
	proven = make(map[string]bool)
	for _, line := range strings.Split(string(contents), "\n") {
		if id := strings.TrimSpace(line); id != "" {
			proven[id] = true
		}
	}
	return proven, true, nil
}

// infraPathExists reports whether path exists, distinguishing absence from an
// unreadable parent so a permission fault is never read as "not migrated".
func infraPathExists(path string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// readInfraSnapshot returns every infra-class bead in the work store, both
// tiers and including closed rows. Closed rows cross deliberately: an order's
// finalize votes, a delivered message, and a completed session lifecycle bead
// are all terminal yet still read after the cutover, and the destination's
// retention sweeper is off by default so history survives the rollback window.
func readInfraSnapshot(source beads.Store) ([]beads.Bead, error) {
	rows, err := source.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return nil, fmt.Errorf("listing work store: %w", err)
	}
	infra := make([]beads.Bead, 0, len(rows))
	for _, b := range rows {
		if coordclass.Classify(b) == coordclass.ClassWork {
			continue
		}
		infra = append(infra, b)
	}
	return infra, nil
}

// infraSourceEdgePayloadRefusal returns the refusal for a source whose
// dependency edges carry payloads this copy cannot carry, or nil when the copy
// is safe.
//
// The copy re-adds edges through beads.Dep, which holds the pair and the type
// and nothing else, so a payload on the source has no way across. On the
// destination the empty carry is worse than absent: setGraphEdgeMetadataTx
// clears the pair's sidecar before deciding it has nothing to store. Until the
// carry exists (ga-o34dj.4.9), refusing is strictly better than dropping —
// the source is retained and unchanged, so a refused city is exactly where it
// was, and the operator learns which edge is in the way.
//
// A source this build cannot ask is refused too. "No reader" and "no payloads"
// are different answers, and treating the first as the second is precisely the
// conflation that let the drop go unnoticed. Every store on the deployed
// migration path answers: the SQLite and native-Dolt leaves read their own
// column, MemStore and the file store report honestly that they have no way to
// hold a payload, and the caching, policy, and strict wrappers forward. What
// remains mute is beads.BdStore and the exec store, whose payloads live behind
// a bd process that offers no dependency-metadata read today (bd dep add takes
// no metadata flag; a payload reaches bd only through a create --graph plan,
// and bd dep list --json does not report it). A city on either of those is
// refused by name until that read exists — tracked as ga-qcpgy.
//
// The scope is edges whose BOTH endpoints are infra, matching what the copy
// actually re-adds. A cross-boundary edge into work is not carried at all — it
// stays metadata linkage resolved by the owning-store read on each side — so
// its payload is not something this copy drops, and refusing on it would block
// cities over an edge the migration never touches.
func infraSourceEdgePayloadRefusal(source beads.Store, rows []beads.Bead) error {
	reader, ok := source.(beads.DepMetadataReader)
	if !ok {
		return fmt.Errorf("work store %T cannot report whether its dependency edges carry payloads, and an unanswerable source is not an empty one: refusing to copy edges whose payloads would be dropped without a trace", source)
	}
	infraIDs := make(map[string]bool, len(rows))
	for _, b := range rows {
		infraIDs[b.ID] = true
	}
	for _, b := range rows {
		deps, err := source.DepList(b.ID, "down")
		if err != nil {
			return fmt.Errorf("listing deps of %s to check for edge payloads: %w", b.ID, err)
		}
		for _, dep := range deps {
			if !infraIDs[dep.DependsOnID] {
				continue
			}
			payload, carried, err := reader.DepMetadata(dep.IssueID, dep.DependsOnID)
			if err != nil {
				return fmt.Errorf("reading the payload on dep %s -> %s: %w", dep.IssueID, dep.DependsOnID, err)
			}
			if !carried || !beads.DepMetadataCarries(payload) {
				continue
			}
			return fmt.Errorf("dep %s -> %s carries an edge payload this copy cannot carry, and copying it would drop the payload silently: refusing. The work store is unchanged; carrying edge payloads is tracked as ga-o34dj.4.9", dep.IssueID, dep.DependsOnID)
		}
	}
	return nil
}

// prepareInfraDestination makes the destination safe to import into.
//
// An empty destination is ready as-is. A destination holding only rows this
// migration stamped is an interrupted earlier attempt: those rows are dropped
// so the import cannot resurrect state the still-authoritative source has since
// changed. Any unstamped row is content this migration did not put there — a
// genesis city, or a store already in service — and the migration refuses
// rather than overwriting it.
func prepareInfraDestination(destination beads.Store) error {
	existing, err := destination.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return fmt.Errorf("listing binding: %w", err)
	}
	if err := infraDestinationPopulatedRefusal(existing); err != nil {
		return err
	}
	for _, b := range existing {
		if err := destination.Delete(b.ID); err != nil {
			return fmt.Errorf("clearing partial attempt row %s: %w", b.ID, err)
		}
	}
	return nil
}

// infraDestinationPopulatedRefusal returns the refusal for a destination
// holding content this migration did not write, or nil when there is nothing in
// the way.
//
// Split out of prepareInfraDestination so the preflight rehearsal can run this
// exact predicate instead of a second copy of it. The preparer goes on to
// DELETE the stamped rows it clears, which a read-only path must not do, but
// the refusal itself is a pure function of what the destination holds — and a
// refusal that drifted between the rehearsal and the migration would clear a
// city inside the window that the migration then refuses.
func infraDestinationPopulatedRefusal(existing []beads.Bead) error {
	for _, b := range existing {
		if b.Metadata[infraMigrationStampKey] == "" {
			return fmt.Errorf("binding already holds %d beads including %s, which this migration did not write: refusing to overwrite a populated destination", len(existing), b.ID)
		}
	}
	return nil
}

// infraDestinationPreflightRefusal reports what prepareInfraDestination would
// refuse, without creating anything.
//
// The database is opened only when it is already on disk, for the reason
// infraBindingHoldsNothing gives: openInfraDestination CREATES it, and a
// rehearsal that created the database it was asked about would leave a store
// behind on a city that never cut over — and would answer its own question,
// since the store it just made is empty and clears every time.
func infraDestinationPreflightRefusal(target infraBindingTarget) error {
	present, err := infraPathExists(target.Database)
	if err != nil {
		return fmt.Errorf("reading the binding database %s: %w", target.Database, err)
	}
	if !present {
		return nil
	}
	store, err := openInfraDestination(target)
	if err != nil {
		return fmt.Errorf("opening the binding %q at %s: %w", target.Binding, target.Database, err)
	}
	defer closeBeadStoreHandle(store) //nolint:errcheck // best-effort close
	existing, err := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return fmt.Errorf("listing the binding %s: %w", target.Database, err)
	}
	return infraDestinationPopulatedRefusal(existing)
}

// importInfraSnapshot copies rows into the destination with their ids preserved
// and re-adds the dep edges whose BOTH endpoints are infra. Cross-boundary
// edges into work stay metadata linkage, resolved by the owning-store read on
// each side — re-adding them here would need a work-store row the destination
// does not own.
func importInfraSnapshot(destination beads.Store, source beads.Store, rows []beads.Bead) (int, error) {
	creator, ok := destination.(beads.ForeignIDCreator)
	if !ok {
		return 0, fmt.Errorf("binding store cannot preserve bead ids: %T does not implement ForeignIDCreator", destination)
	}
	infraIDs := make(map[string]bool, len(rows))
	for _, b := range rows {
		infraIDs[b.ID] = true
	}
	imported := 0
	for _, b := range rows {
		if _, err := destination.Get(b.ID); err == nil {
			continue
		}
		if _, err := creator.CreateWithForeignID(infraMigrationRow(b)); err != nil {
			return imported, fmt.Errorf("importing bead %s: %w", b.ID, err)
		}
		imported++
	}
	for _, b := range rows {
		deps, err := source.DepList(b.ID, "down")
		if err != nil {
			return imported, fmt.Errorf("listing deps of %s: %w", b.ID, err)
		}
		for _, d := range deps {
			if !infraIDs[d.DependsOnID] {
				continue
			}
			if err := destination.DepAdd(b.ID, d.DependsOnID, d.Type); err != nil {
				return imported, fmt.Errorf("importing dep %s -> %s: %w", b.ID, d.DependsOnID, err)
			}
		}
	}
	return imported, nil
}

// infraMigrationRow returns the form of b that is safe to create in the
// destination: the provenance stamp added, and the create-time dependency
// shorthands stripped, without mutating the caller's bead or its maps.
//
// Stripping matters. A store's Create derives dep rows from Dependencies (or,
// failing that, Needs), and a source bead carries edges pointing at work beads
// the destination does not own — creating them here would leave dangling
// cross-boundary rows. The explicit within-infra pass in importInfraSnapshot is
// the only thing that writes edges, and the source's own materialized dep rows
// are what it reads, so nothing is lost.
func infraMigrationRow(b beads.Bead) beads.Bead {
	metadata := make(beads.StringMap, len(b.Metadata)+1)
	for key, value := range b.Metadata {
		metadata[key] = value
	}
	metadata[infraMigrationStampKey] = config.StorageWorkBinding
	b.Metadata = metadata
	b.Dependencies = nil
	b.Needs = nil
	// IsBlocked is stripped, not copied. It is bd's DENORMALIZED readiness
	// projection, and the destination does not recompute it — SQLiteStore
	// round-trips the field through bead_json and serves back whatever was
	// written, while CachingStore.cachedBeadReady PREFERS a non-nil IsBlocked
	// over dependency-derived readiness. So a copied value would be authoritative
	// and permanent.
	//
	// Copying it faithfully would still be wrong. The source's value was computed
	// by bd over the WHOLE graph, including edges to work beads; the binding holds
	// only the within-infra edges. A session bead blocked at copy time by a work
	// bead would be frozen blocked in the binding forever, with nothing to unblock
	// it. Nil is the documented "store did not provide the projection, fall back
	// to dependency-derived readiness" (beads.Bead.IsBlocked), and the dep rows
	// that fallback reads ARE witnessed bidirectionally by verifyInfraCopy.
	b.IsBlocked = nil
	return b
}

// verifyInfraCopy is the equality stage: the destination must hold exactly the
// source's infrastructure slice — every row readable with its identity, lifecycle, and
// routing fields intact, no row the copy invented, and every within-infra dep
// edge present. It runs before the marker, so a mismatch leaves the city on the
// retained source rather than routing reads at a store that lost something.
//
// On success it returns the sorted ids it proved present, which is the content
// of the copy manifest. Returning them rather than re-deriving them elsewhere
// is what keeps the manifest a record of the PROVEN set: any later re-read
// could see a source that has moved on.
//
// The source is re-read here rather than compared against the snapshot the copy
// was derived from. A bead written to the work store while the copy was running
// is in the source and not in the destination; comparing against the snapshot
// would not see it, and the marker would then bless a copy that stranded it —
// the same silent loss the equality stage exists to catch. Re-reading turns
// that into a refusal, and the next boot converges against a quiet source.
//
// The destination arrives as an OPENER rather than as a store, and that is the
// point. The copy's writing handle is closed before this runs and the database
// is opened again here, so what the marker attests to is the bytes on disk — the
// same bytes the runtime binding will open — and not a warm connection that
// could answer out of state the file does not carry. Taking a beads.Store would
// make handing the writer back in a one-line refactor away, and the resulting
// proof would look identical while proving something weaker.
func verifyInfraCopy(openDestination func() (beads.Store, error), source beads.Store) ([]string, error) {
	rows, err := readInfraSnapshot(source)
	if err != nil {
		return nil, err
	}
	infraIDs := make(map[string]bool, len(rows))
	proven := make([]string, 0, len(rows))
	for _, b := range rows {
		infraIDs[b.ID] = true
		proven = append(proven, b.ID)
	}
	destination, err := openDestination()
	if err != nil {
		return nil, fmt.Errorf("reopening the binding for the equality stage: %w", err)
	}
	defer func() { _ = closeBeadStoreHandle(destination) }()
	// Equality is bidirectional: the destination must hold the source's infra
	// slice and nothing else, so a row the copy invented is caught too.
	copied, err := destination.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return nil, fmt.Errorf("listing binding: %w", err)
	}
	for _, b := range copied {
		if !infraIDs[b.ID] {
			return nil, fmt.Errorf("binding holds %s, which the work store's infrastructure slice does not", b.ID)
		}
	}
	if len(copied) != len(rows) {
		return nil, fmt.Errorf("binding holds %d beads, want %d", len(copied), len(rows))
	}
	for _, want := range rows {
		got, err := destination.Get(want.ID)
		if err != nil {
			return nil, fmt.Errorf("bead %s missing from binding: %w", want.ID, err)
		}
		if diff := beadCopyDifference(want, got); diff != "" {
			return nil, fmt.Errorf("bead %s differs after copy: %s", want.ID, diff)
		}
		if diff := infraCopyClassDifference(want, got); diff != "" {
			return nil, fmt.Errorf("bead %s %s", want.ID, diff)
		}
		wantDeps, err := source.DepList(want.ID, "down")
		if err != nil {
			return nil, fmt.Errorf("re-listing deps of %s: %w", want.ID, err)
		}
		gotDeps, err := destination.DepList(want.ID, "down")
		if err != nil {
			return nil, fmt.Errorf("listing copied deps of %s: %w", want.ID, err)
		}
		if diff := infraDepDifference(want.ID, wantDeps, gotDeps, infraIDs); diff != "" {
			return nil, errors.New(diff)
		}
	}
	sort.Strings(proven)
	return proven, nil
}

// beadCopyDifference returns a human-readable description of the first field
// that did not survive the copy, or "" when the copy is faithful.
//
// It witnesses every durable field of beads.Bead. The five it does not are named
// individually, with the reason each cannot be a witness, in
// beadCopyExemptFields, and TestBeadCopyDifferenceWitnessesEveryDurableField
// guards that list against the struct by reflection so a field added to
// beads.Bead is either compared here or exempted there — never silently
// unwitnessed. The guard exists because the failure mode is the worst one this
// migration has available: a copy that dropped a field nothing compares PASSES
// the equality stage, gets a convergence marker, and the city then routes every
// infra read at a binding missing state the retained source still holds. Three
// of the six fields this stage used to ignore are load-bearing on their own —
// DeferUntil gates readiness, Priority orders ready work, NoHistory routes the
// storage tier — and Description is simply the bead's body.
//
// Timestamps compare at second resolution because the source truncates there:
// the work store persists at second granularity, so exact equality is not a
// property any copy between these two stores can promise. A zero source
// timestamp is exempt for the mirror-image reason — the destination's
// normalizeCreate backfills a zero CreatedAt from the clock and a zero UpdatedAt
// from CreatedAt, so a legacy bead's absent timestamp cannot survive as absent
// and comparing it would refuse every copy that carried one.
func beadCopyDifference(want, got beads.Bead) string {
	switch {
	// Asserted on the destination alone, whatever the source carried:
	// infraMigrationRow strips IsBlocked precisely because the destination does
	// not recompute it and CachingStore prefers it over dependency-derived
	// readiness. A non-nil value here is a projection the binding would serve
	// forever and nothing would ever correct.
	case got.IsBlocked != nil:
		return fmt.Sprintf("binding carries is_blocked=%v; the copy strips it so readiness falls back to the dep rows this stage witnesses", *got.IsBlocked)
	case want.ID != got.ID:
		return fmt.Sprintf("id %q != %q", want.ID, got.ID)
	case want.Title != got.Title:
		return fmt.Sprintf("title %q != %q", want.Title, got.Title)
	case want.Status != got.Status:
		return fmt.Sprintf("status %q != %q", want.Status, got.Status)
	case want.Type != got.Type:
		return fmt.Sprintf("type %q != %q", want.Type, got.Type)
	case want.Description != got.Description:
		return fmt.Sprintf("description %q != %q", want.Description, got.Description)
	case !beadCopyEqualInt(want.Priority, got.Priority):
		return fmt.Sprintf("priority %s != %s", beadCopyFormatInt(want.Priority), beadCopyFormatInt(got.Priority))
	case want.Assignee != got.Assignee:
		return fmt.Sprintf("assignee %q != %q", want.Assignee, got.Assignee)
	case want.From != got.From:
		return fmt.Sprintf("from %q != %q", want.From, got.From)
	case want.ParentID != got.ParentID:
		return fmt.Sprintf("parent %q != %q", want.ParentID, got.ParentID)
	case want.Ref != got.Ref:
		return fmt.Sprintf("ref %q != %q", want.Ref, got.Ref)
	// Ephemeral and NoHistory are the two tier-routing bits. A copy that lost
	// either lands the row in a tier its readers do not query.
	case want.Ephemeral != got.Ephemeral:
		return fmt.Sprintf("ephemeral %v != %v", want.Ephemeral, got.Ephemeral)
	case want.NoHistory != got.NoHistory:
		return fmt.Sprintf("no_history %v != %v", want.NoHistory, got.NoHistory)
	// Creation time drives retention and every age-based gate downstream, so a
	// copy that re-stamped it is a failed copy.
	case !want.CreatedAt.IsZero() && want.CreatedAt.Unix() != got.CreatedAt.Unix():
		return fmt.Sprintf("created_at %s != %s", want.CreatedAt.UTC(), got.CreatedAt.UTC())
	case !want.UpdatedAt.IsZero() && want.UpdatedAt.Unix() != got.UpdatedAt.Unix():
		return fmt.Sprintf("updated_at %s != %s", want.UpdatedAt.UTC(), got.UpdatedAt.UTC())
	// DeferUntil hides a bead from every ready and claimable view until it
	// passes. A copy that dropped it makes deferred work instantly claimable;
	// one that invented it hides ready work indefinitely.
	case !beadCopyEqualInstant(want.DeferUntil, got.DeferUntil):
		return fmt.Sprintf("defer_until %s != %s", beadCopyFormatInstant(want.DeferUntil), beadCopyFormatInstant(got.DeferUntil))
	}
	if diff := stringSetDifference("label", want.Labels, got.Labels); diff != "" {
		return diff
	}
	return stringMapDifference("metadata", want.Metadata, got.Metadata, infraMigrationStampKey)
}

// infraCopyClassDifference reports a copied row whose coordclass routing changed
// in the crossing, or "" when it did not.
//
// Every input coordclass.Classify reads — type, labels, metadata — is also a
// field beadCopyDifference witnesses, so on today's equality stage no real copy
// can reach this check. It is kept anyway because the property is worth stating
// in its own terms rather than as a side effect of another comparison: the infra
// binding is the store five classes of reader route to, and a row that arrives
// answering to a different class than it left with is unreachable to the
// subsystem that owns it. The work arm is the one that must never be wrong —
// work does not live in this binding at all.
func infraCopyClassDifference(want, got beads.Bead) string {
	wantClass, gotClass := coordclass.Classify(want), coordclass.Classify(got)
	if gotClass == coordclass.ClassWork {
		return fmt.Sprintf("classifies as %s on the binding; work never crosses", gotClass)
	}
	if wantClass != gotClass {
		return fmt.Sprintf("classifies as %s on the binding but %s in the work store", gotClass, wantClass)
	}
	return ""
}

// infraDepDifference compares one bead's outbound dep edges in both directions,
// returning the first defect or "".
//
// The forward direction is the copy's own contract: every within-infra edge the
// source has must be on the destination. The reverse direction is the one a
// one-directional check misses, and it is not a theoretical worry — a fabricated
// blocks edge changes what the destination reports ready, so a copy that
// invented one silently changes the city's scheduling.
//
// Two asymmetries are deliberate. Cross-boundary edges into work are skipped in
// the forward direction because importInfraSnapshot does not re-add them (the
// destination owns no row for the far endpoint); they are NOT skipped in the
// reverse direction, because nothing in this migration writes one, so a
// destination that has one did not get it from here. And a dep type is compared
// only when both sides carry one: the destination normalizes an empty type to
// its own default, so an empty source type is evidence of nothing.
//
// # The edge payload is still not witnessed HERE — it is refused upstream
//
// The source's dependency rows carry a metadata JSON column, written in
// production by every formula step with a waits_for gate. The copy cannot carry
// it: beads.Dep exposes only IssueID, DependsOnID and Type, so
// importInfraSnapshot's DepAdd has nothing to pass — and on the destination
// that empty payload is not merely absent but DESTRUCTIVE, because
// setGraphEdgeMetadataTx clears the pair's sidecar before deciding it has
// nothing to store.
//
// This stage therefore compares no payload, and deliberately does not try to:
// by the time it runs, infraSourceEdgePayloadRefusal has already refused any
// source holding one, so an edge that reaches here is known to have nothing to
// compare. That refusal is what closed the detection half of the gap —
// NativeDoltStore.DepMetadata gave a Dolt-backed source a payload read, and a
// source that cannot answer at all is refused rather than assumed clean.
//
// What remains open is CARRIAGE. The binding's own adoption witness
// (internal/storebinding/sqlite/graph_witness.go) insists that "a destination
// that moved the edges but dropped their payloads must not hash equal"; this
// stage still cannot move one. So a city whose formulas use waits_for gates
// cannot cut over — it is refused, loudly, naming the edge — until the carry
// lands. That is a strictly better failure than the silent drop it replaced,
// and it is not the finished job.
func infraDepDifference(id string, wantDeps, gotDeps []beads.Dep, infraIDs map[string]bool) string {
	wantTypes := make(map[string][]string, len(wantDeps))
	for _, d := range wantDeps {
		if !infraIDs[d.DependsOnID] {
			continue
		}
		wantTypes[d.DependsOnID] = append(wantTypes[d.DependsOnID], d.Type)
	}
	gotTypes := make(map[string][]string, len(gotDeps))
	for _, d := range gotDeps {
		gotTypes[d.DependsOnID] = append(gotTypes[d.DependsOnID], d.Type)
	}
	for _, dependsOn := range sortedMapKeys(wantTypes) {
		if _, ok := gotTypes[dependsOn]; !ok {
			return fmt.Sprintf("dep %s -> %s missing from binding", id, dependsOn)
		}
	}
	for _, dependsOn := range sortedMapKeys(gotTypes) {
		want, ok := wantTypes[dependsOn]
		if !ok {
			return fmt.Sprintf("binding holds dep %s -> %s, which the work store does not", id, dependsOn)
		}
		for _, kind := range gotTypes[dependsOn] {
			if kind == "" || slices.Contains(want, kind) || slices.Contains(want, "") {
				continue
			}
			return fmt.Sprintf("dep %s -> %s crossed as %q, want one of %q", id, dependsOn, kind, want)
		}
	}
	return ""
}

// sortedMapKeys returns m's keys in order, so a difference message names the
// same field on every run rather than whichever one map iteration reached first.
func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// beadCopyEqualInt reports whether two optional ints hold the same value,
// treating unset and set-to-zero as different: bd's priority column is nullable
// and an unset priority is not priority 0.
func beadCopyEqualInt(want, got *int) bool {
	if want == nil || got == nil {
		return want == got
	}
	return *want == *got
}

// beadCopyFormatInt renders an optional int for a difference message.
func beadCopyFormatInt(v *int) string {
	if v == nil {
		return "unset"
	}
	return strconv.Itoa(*v)
}

// beadCopyEqualInstant reports whether two optional timestamps name the same
// second. Second resolution for the same reason CreatedAt uses it: the source
// persists at second granularity.
func beadCopyEqualInstant(want, got *time.Time) bool {
	if want == nil || got == nil {
		return want == got
	}
	return want.Unix() == got.Unix()
}

// beadCopyFormatInstant renders an optional timestamp for a difference message.
func beadCopyFormatInstant(v *time.Time) string {
	if v == nil {
		return "unset"
	}
	return v.UTC().String()
}

// stringSetDifference reports the first difference between two string sets in
// EITHER direction — order is not part of the copy contract, but membership is
// both ways. A subset-only comparison would let the copy invent a label, and an
// invented label is state no reader can attribute to the source.
func stringSetDifference(kind string, want, got []string) string {
	have := make(map[string]bool, len(got))
	for _, v := range got {
		have[v] = true
	}
	wanted := make(map[string]bool, len(want))
	for _, v := range want {
		wanted[v] = true
	}
	for _, v := range want {
		if !have[v] {
			return fmt.Sprintf("%s %q missing", kind, v)
		}
	}
	for _, v := range got {
		if !wanted[v] {
			return fmt.Sprintf("%s %q was invented by the copy", kind, v)
		}
	}
	return ""
}

// stringMapDifference reports the first difference between two string maps in
// EITHER direction, ignoring one exempt key.
//
// Both directions, and on key PRESENCE rather than value alone: absent and
// present-but-empty are different states, so a value-only comparison would let
// the copy invent a key as long as it left the value empty.
//
// The exemption is the migration's own provenance stamp, which is on every
// destination row by construction and on no source row that was not itself
// migrated. It is exempt in the forward direction too: infraMigrationRow
// overwrites a stamp the source already carries, so a re-migrated city would
// otherwise fail equality on the one key the migration itself controls.
func stringMapDifference(kind string, want, got beads.StringMap, exempt string) string {
	for _, key := range sortedMapKeys(want) {
		if key == exempt {
			continue
		}
		value, ok := got[key]
		if !ok {
			return fmt.Sprintf("%s %q missing", kind, key)
		}
		if value != want[key] {
			return fmt.Sprintf("%s %q %q != %q", kind, key, want[key], value)
		}
	}
	for _, key := range sortedMapKeys(got) {
		if key == exempt {
			continue
		}
		if _, ok := want[key]; !ok {
			return fmt.Sprintf("%s %q was invented by the copy", kind, key)
		}
	}
	return ""
}

// writeInfraMigratedMarker records convergence atomically (temp + rename) so a
// crash mid-write can never leave a truncated marker that reads as present.
//
// It is not self-certifying: writeInfraCopyManifest has already recorded what
// the copy was proven to deliver, and every later boot re-checks containment
// against that record rather than reading convergence off this file.
func writeInfraMigratedMarker(target infraBindingTarget) error {
	if err := os.MkdirAll(target.Root, 0o755); err != nil {
		return fmt.Errorf("writing infra migrated marker: %w", err)
	}
	tmp, err := os.CreateTemp(target.Root, infraMigratedMarkerName+".tmp-*")
	if err != nil {
		return fmt.Errorf("writing infra migrated marker: %w", err)
	}
	name := tmp.Name()
	cleanup := func(cause error) error {
		_ = os.Remove(name)
		return fmt.Errorf("writing infra migrated marker: %w", cause)
	}
	if _, err := fmt.Fprintf(tmp, "infrastructure classes migrated to binding %s (%s) %s\n", target.Binding, target.Database, time.Now().UTC().Format(time.RFC3339)); err != nil {
		_ = tmp.Close()
		return cleanup(err)
	}
	if err := tmp.Close(); err != nil {
		return cleanup(err)
	}
	if err := infraMigrationRename(name, target.MarkerPath()); err != nil {
		return cleanup(err)
	}
	return nil
}
