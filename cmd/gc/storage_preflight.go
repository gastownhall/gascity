package main

// `gc storage preflight` — everything the cutover would refuse, reported from
// outside the window.
//
// The migration's refusals are all correct and all arrive at the worst possible
// moment. An operator stops the city, runs `gc storage migrate --from-work
// --fleet-stopped`, and finds out there — with the fleet down — that a rig scope
// holds an infrastructure bead this binary carries no importer for. That is a
// window spent reading a message instead of migrating, and the fix is by hand.
//
// So the checks run twice: once here, against a LIVE city, and once in the
// migration where they gate the copy. This file adds no check of its own and
// reimplements none. Every step below calls the same function the migration
// calls, so a refusal that changes there changes here — and a check added there
// without a line here shows up as a preflight that clears a city the migration
// refuses, which is the one failure mode this verb cannot tolerate.
//
// That last sentence is a claim about a list, and a comment cannot keep a list
// honest: the first cut of this file asserted it while omitting three of the
// migration's refusals. TestPreflightRefusesEveryCityTheMigrationRefuses is what
// actually holds it — every fixture that makes the migration refuse for an
// operator-fixable reason is run through both, and the preflight must refuse
// too. A check added to the migration without a line here fails that test.
//
// Three things are deliberately NOT mirrored:
//
//   - The migration guard is not taken. It is exclusive, so a preflight holding
//     it would make a real migration started a moment later refuse with
//     "another storage migration holds this city" — the command an operator ran
//     to find out whether they could migrate would be the reason they could not.
//   - A live controller is reported, not refused. Every other check names
//     something the operator must go and fix before the window; the controller
//     names the window itself. Blocking on it would mean the command for
//     planning a window could only be run from inside one. It is also the one
//     check out of the migration's order — reported last, with the attestation,
//     because both describe the window rather than something to go and fix.
//   - The destination is inspected without being opened unless it already
//     exists. The migration opens it because creating it is the next thing it
//     does; a rehearsal that created it would answer its own question.
//
// It is a separate verb rather than `migrate --dry-run` for two reasons. The
// destination opener CREATES the database — that is its job — so a dry run
// sharing the migrate body would have to fork it anyway and the sharing would be
// nominal. And a mode flag on a destructive command is one typo away from the
// destructive command.

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/config"
)

// storagePreflightVerb is the read-only rehearsal of the migrate verb.
const storagePreflightVerb = "preflight"

// storagePreflightLogPrefix is how this command names itself in its own
// output, spelled the way an operator typed it.
const storagePreflightLogPrefix = "gc storage " + storagePreflightVerb

// newStoragePreflightCmd is the third read-only sibling: it reports what the
// cutover would do without doing any of it.
func newStoragePreflightCmd(surface storageCommandSurface, stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:          storagePreflightVerb,
		Short:        "Report what the migration would refuse, without migrating (read-only)",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		Long: `Run every check ` + storageMigrationCommand + ` runs and report what it
finds — without copying anything, creating anything, taking the migration guard,
or publishing any event.

This is for deciding whether the window you are about to take will be spent
migrating or spent reading a refusal. It runs against a LIVE city: a controller
serving this city is reported by PID rather than refused, because stopping it is
the next thing you were going to do anyway.

It resolves its destination from [storage.classes], so it has nothing to check
until that section names a binding. On a city with no infrastructure split it
reports exactly that and exits non-zero — author the split first.

It exits non-zero when the migration would refuse for a reason you have to go
and fix first. That is a different question from ` + storageStatusInstruction() + `,
which exits non-zero whenever the city is not yet serving from its binding — the
ordinary state of every city with a cutover still ahead of it.

One condition is never checked here, because no process can check it:
--` + storageFleetStoppedFlag + ` attests that ` + storageFleetStoppedAttestation + `.`,
		RunE: func(*cobra.Command, []string) error {
			request, err := resolveStorageOperatorRequest()
			if err != nil {
				fmt.Fprintf(stderr, "gc %s %s: %v\n", surface.Namespace, storagePreflightVerb, err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			return exitForCode(doStoragePreflight(request, stdout, stderr))
		},
	}
}

// doStoragePreflight reports what the migration would find, and changes
// nothing.
//
// It takes no event recorder and constructs none. The storage.binding.* stream
// carries serving verdicts a deploy gate reads, and this command reaches no
// verdict — it reports what a migration WOULD find. A diagnostic that published
// into that stream would let a command an operator ran to plan a window answer a
// question they did not ask. Opening the city's real recorder is itself a write,
// which is why there is nothing here to pass one to.
// TestPreflightPublishesNoEvent holds it, at the event log rather than at a
// parameter.
func doStoragePreflight(request storageOperatorRequest, stdout, stderr io.Writer) int {
	fmt.Fprintf(stdout, "city: %s\n", request.CityPath) //nolint:errcheck // best-effort stdout

	// 1. The destination, resolved the way the migration resolves it. A layout
	// this build cannot serve is refused here for the same reason it is refused
	// there: a plan boot would not serve must not be migrated toward.
	target, ok, err := resolveInfraBindingTarget(request.CityPath, request.Cfg)
	if err != nil {
		return preflightBlock(stdout, "topology", err)
	}
	if !ok {
		// Named explicitly because this is the one block an operator can reach
		// by running the rehearsal too early — before the config swap the
		// runbook opens with — where "nothing to migrate" would otherwise read
		// as a fault rather than as a step not yet taken.
		return preflightBlock(stdout, "topology", fmt.Errorf(
			"this city's [storage.classes] do not assign %s to one shared non-work binding, so there is nothing to migrate yet. %s. Author the split in city.toml first; there is nothing to rehearse until the classes name a binding",
			infraMigrationClassList(), storageSupportedTopologyStatement))
	}
	fmt.Fprintf(stdout, "binding: %s\n  database: %s\n", target.Binding, target.Database) //nolint:errcheck // best-effort stdout
	preflightPass(stdout, "topology", "this build serves the split these classes describe")

	// 2. The same plan resolution boot performs.
	if _, err := resolveCityStoragePlan(request.CityPath, request.Cfg); err != nil {
		return preflightBlock(stdout, "plan", err)
	}
	preflightPass(stdout, "plan", "the binding resolves to an engine this build opens")

	// 3. The expensive one, and the reason this verb most earns its place: a bead
	// in a rig scope is refused BY NAME and no command this binary carries can
	// repair it. Finding that out inside a stopped-city window is the worst
	// possible time. It runs here, ahead of the convergence read, because that is
	// where the migration runs it — a converged city with rig residue is refused
	// by the census, so a rehearsal that returned early on the marker would clear
	// it.
	if err := censusRigInfraResidue(request.CityPath, request.Cfg); err != nil {
		return preflightBlock(stdout, "rig scopes", err)
	}
	preflightPass(stdout, "rig scopes", "no infrastructure bead lives outside this city's work store")

	// 4. The served-binding note, which is the migration's own first check and an
	// unconditional hold: a note naming another binding means infrastructure
	// state lives somewhere this configuration does not point, and copying the
	// work store's slice would bless a destination that silently omits it.
	if blocked, held := servedBindingNoteHold(request.CityPath, target.Binding, config.StorageProviderSQLiteBeads, target.Database); held {
		return preflightBlock(stdout, "served binding", errors.New(preflightAdviceDetail(blocked)))
	}
	preflightPass(stdout, "served binding", "no earlier binding holds this city's infrastructure classes")

	// 5. Whether the cutover already happened. The marker means the migration
	// would not copy, so this clears — but clearing it silently would read as
	// "your cutover is pending and will go fine", which is the opposite of the
	// truth. Clearing here is not a claim that the migration would exit zero:
	// on a marked city it goes on to confirm convergence, which can still
	// report stranded or uncheckable. That answer lives on the destination this
	// rehearsal will not open, which is why the report hands off to
	// `gc storage status` rather than answering it.
	state, err := readInfraConvergenceState(target)
	if err != nil {
		return preflightBlock(stdout, "binding root", err)
	}
	if state == infraConvergenceMarked {
		preflightPass(stdout, "cutover", "already converged — the marker is present, so the migration would not copy")
		fmt.Fprintf(stdout, "\nNothing to migrate: this city is already converged. Run `%s` to see what it holds.\n", storageStatusInstruction()) //nolint:errcheck // best-effort stdout
		return 0
	}
	if state == infraConvergenceStale {
		preflightPass(stdout, "cutover", "a convergence marker exists with no database under it, so the copy would run again and re-converge")
	} else {
		preflightPass(stdout, "cutover", "not converged yet, which is what the migration is for")
	}

	// 6. What the copy would carry, and whether it could read it. A read that
	// failed leaves the whole clearance unfounded, so it blocks rather than
	// reporting zero — the same positive-looking absence this path refuses
	// everywhere else. The edge-payload refusal runs on the same open source and
	// the same snapshot the migration hands it, because a payload the copy cannot
	// READ is refused by edge id at exactly this point in the window. Whether it
	// could WRITE one is not asked here: that answer lives on the destination,
	// and opening the destination is the one thing this rehearsal will not do.
	source, err := openInfraMigrationSource(request.CityPath)
	if err != nil {
		return preflightBlock(stdout, "work store", fmt.Errorf("opening the work store: %w", err))
	}
	rows, listErr := readInfraSnapshot(source)
	var edgeErr error
	if listErr == nil {
		// The rehearsal opens no destination, so the carry half of the answer
		// has nowhere to be checked here; only the read half is its business.
		_, edgeErr = infraSourceEdgePayloadRefusal(source, rows)
	}
	if closeErr := closeBeadStoreHandle(source); closeErr != nil {
		fmt.Fprintf(stderr, "%s: closing the work store: %v\n", storagePreflightLogPrefix, closeErr) //nolint:errcheck // best-effort stderr
	}
	if listErr != nil {
		return preflightBlock(stdout, "work store", listErr)
	}
	preflightPass(stdout, "work store", fmt.Sprintf("would copy %d infrastructure bead(s)", len(rows)))
	if edgeErr != nil {
		return preflightBlock(stdout, "edge payloads", edgeErr)
	}
	preflightPass(stdout, "edge payloads", "the work store can report the payload on every edge the copy would re-add")

	// 7. What the destination already holds. Read-only: the database is opened
	// only if it is already there, because opening creates it.
	if err := infraDestinationPreflightRefusal(target); err != nil {
		return preflightBlock(stdout, "destination", err)
	}
	preflightPass(stdout, "destination", "nothing in the binding is in the way of the copy")

	// 8. Informational, both of them. Neither names something to fix before the
	// window; one names the window and one names the thing no process can check.
	if pid := infraMigrationForeignControllerPID(request.CityPath); pid != 0 {
		fmt.Fprintf(stdout, "controller: PID %d is live on this city. The migration refuses while it is; stop it with `%s` when you take the window.\n", pid, storageStopCommand) //nolint:errcheck // best-effort stdout
	} else {
		// What was observed, not what it implies. The probe returns 0 both when
		// nothing is listening and when the ping itself failed — a timeout, a
		// socket it could not reach — so "none is live" would be a fact this
		// line does not have. The migration collapses the same two cases, and
		// there it is safe by direction: it proceeds either way. Here the
		// operator is deciding whether their window is clear, and an unreachable
		// controller they are told is absent is the one way this line could cost
		// them the window it was printed to protect.
		fmt.Fprintf(stdout, "controller: nothing answered this city's controller socket. The migration probes the same way, so it would proceed; confirm with `%s` before you rely on it.\n", storageStopCommand) //nolint:errcheck // best-effort stdout
	}
	fmt.Fprintf(stdout, "attestation: --%s is not checked here or anywhere. It asserts that %s.\n", storageFleetStoppedFlag, storageFleetStoppedAttestation) //nolint:errcheck // best-effort stdout

	fmt.Fprintf(stdout, "\nReady. When the fleet is stopped, run: %s --%s\n", storageMigrationCommand, storageFleetStoppedFlag) //nolint:errcheck // best-effort stdout
	return 0
}

// preflightAdviceDetail renders a migration refusal for the detail column.
//
// infraMigrationOperatorAdvice leads with the command name, which is right when
// the advice is the whole of what a command says and wrong inside a report whose
// footer already names it — the operator would read the same spelling twice in
// three lines. It is asked for and then removed rather than passed empty,
// because the empty prefix still leaves behind the ": " it joins with.
func preflightAdviceDetail(report infraMigrationReport) string {
	return strings.TrimPrefix(
		infraMigrationOperatorAdvice(report, storagePreflightLogPrefix),
		storagePreflightLogPrefix+": ")
}

// The report is a fixed three-column layout: a status tag, the check name, and
// what the check found. The widths live here once rather than in each format
// string, because the continuation indent in preflightBlock has to land on the
// detail column and a literal that drifted from the one above it would put every
// wrapped refusal line half a word out of alignment.
const (
	// preflightPassTag and preflightBlockTag are the two verdicts a check has.
	preflightPassTag  = "[ok]"
	preflightBlockTag = "[BLOCK]"
	// preflightTagColumn fits the wider of the two tags plus its trailing space.
	preflightTagColumn = len(preflightBlockTag) + 1
	// preflightCheckColumn fits the longest check name this report prints,
	// which is "served binding".
	preflightCheckColumn = 14
	// preflightMargin is the indent the whole report sits at, under the
	// unindented city/binding lines that precede it.
	preflightMargin = 2
)

var (
	// preflightRowFormat lays out one check row.
	preflightRowFormat = fmt.Sprintf("%s%%-%ds%%-%ds %%s\n", strings.Repeat(" ", preflightMargin), preflightTagColumn, preflightCheckColumn)
	// preflightDetailIndent puts a wrapped line under the detail column.
	preflightDetailIndent = strings.Repeat(" ", preflightMargin+preflightTagColumn+preflightCheckColumn+1)
)

// preflightPass records a check the migration would clear.
func preflightPass(stdout io.Writer, check, detail string) {
	fmt.Fprintf(stdout, preflightRowFormat, preflightPassTag, check, detail) //nolint:errcheck // best-effort stdout
}

// preflightBlock records the check that would refuse the migration and returns
// the command's exit code.
//
// The fault goes to stdout with the checks that preceded it rather than to
// stderr, because the report is the answer: an operator reading "which of these
// would stop me" needs the failing line in the same list as the passing ones.
// The exit code carries the verdict for anything reading it instead.
// A cause carrying newlines is indented under the detail column rather than
// left at the margin, where its lines would be indistinguishable from new
// checks and a report whose whole value is "which of these would stop me" would
// appear to list checks that do not exist. Every refusal reaching here today is
// a single line; this is the layout holding for the first one that is not,
// since these messages come from the migration and are written to be read on
// their own rather than inside a column.
func preflightBlock(stdout io.Writer, check string, cause error) int {
	lines := strings.Split(strings.TrimRight(cause.Error(), "\n"), "\n")
	fmt.Fprintf(stdout, preflightRowFormat, preflightBlockTag, check, lines[0]) //nolint:errcheck // best-effort stdout
	for _, line := range lines[1:] {
		fmt.Fprintf(stdout, "%s%s\n", preflightDetailIndent, line) //nolint:errcheck // best-effort stdout
	}
	fmt.Fprintf(stdout, "\n%s: the migration would refuse. Fix the blocking check above and run this again.\n", storagePreflightLogPrefix) //nolint:errcheck // best-effort stdout
	return 1
}
