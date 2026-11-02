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
// Four things are deliberately NOT mirrored:
//
//   - The migration guard is not taken. It is exclusive, so a preflight holding
//     it would make a real migration started a moment later refuse with
//     "another storage migration holds this city" — the command an operator ran
//     to find out whether they could migrate would be the reason they could not.
//   - No controller lock is taken, this city's or, under the shared-store
//     attestation, a co-tenant's. A rehearsal holding one would keep that city
//     from starting while it ran.
//   - The window is reported, not refused: a live controller, the city's
//     registration with the supervisor, its running sessions, and the
//     co-tenants whose locks the migration takes. Every other check names
//     something the operator must go and fix before the window; these name the
//     window itself. Blocking on them would mean the command for planning a
//     window could only be run from inside one. They are also the checks out of
//     the migration's order — reported last, with the attestation, because both
//     describe the window rather than something to go and fix.
//   - The destination is inspected without being opened unless it already
//     exists. The migration opens it because creating it is the next thing it
//     does; a rehearsal that created it would answer its own question.
//
// The migration's --force attestations are taken, and lift here what they
// lift there, so a city that will migrate under them rehearses as it will run.
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
	var forced []string
	cmd := &cobra.Command{
		Use:          storagePreflightVerb,
		Short:        "Report what the migration would refuse, without migrating (read-only)",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		Long: `Run every check ` + storageMigrationCommand + ` runs and report what it
finds — without copying anything, creating anything, taking the migration guard
or any controller lock, or publishing any event.

This is for deciding whether the window you are about to take will be spent
migrating or spent reading a refusal. It runs against a LIVE city, so what
stopping the city ends is reported rather than refused, because stopping it is
the next thing you were going to do anyway: a controller serving this city (by
PID), the city's registration with the supervisor, its running sessions, and,
under a shared-store attestation, the other cities on the store, whose
controller locks the migration takes.

Pass the --` + storageForceFlag + ` attestations you will pass to the migration. What they
lift there is lifted here, and the command this prints for the window carries
them.

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
			force, err := parseStorageForce(forced)
			if err != nil {
				fmt.Fprintf(stderr, "gc %s %s: %v\n", surface.Namespace, storagePreflightVerb, err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			request, err := resolveStorageOperatorRequest()
			if err != nil {
				fmt.Fprintf(stderr, "gc %s %s: %v\n", surface.Namespace, storagePreflightVerb, err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			request.Force = force
			return exitForCode(doStoragePreflight(request, stdout, stderr))
		},
	}
	addStorageForceFlag(cmd, &forced)
	return cmd
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

	// 2. The same plan resolution boot performs, after the reserved-prefix
	// refusal boot and the migration make before it (R10).
	if err := config.ReservedPrefixSplitRefusal(request.Cfg); err != nil {
		return preflightBlock(stdout, "plan", err)
	}
	if _, err := resolveCityStoragePlan(request.CityPath, request.Cfg); err != nil {
		return preflightBlock(stdout, "plan", err)
	}
	preflightPass(stdout, "plan", "the binding resolves to an engine this build opens")

	// The migration's exclusion, without its locks: what it refuses before it
	// copies, under the attestations this run passes.
	window, code := preflightExclusion(request, stdout)
	if code != 0 {
		return code
	}

	// 3. The expensive one, and the reason this verb most earns its place: a bead
	// in a rig scope is refused BY NAME and no command this binary carries can
	// repair it. Finding that out inside a stopped-city window is the worst
	// possible time. It runs here, ahead of the served-binding and cutover
	// checks, because that is where the migration runs it — and, as there, it
	// gates only a copy. A city that already converged copies nothing, so the
	// migration does not census it (doStorageMigrate), and what the census
	// finds there is named without blocking.
	if code := preflightRigScopes(request, target, stdout); code != 0 {
		return code
	}

	state, stateErr := readInfraConvergenceState(target)
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
	if stateErr != nil {
		return preflightBlock(stdout, "binding root", stateErr)
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

	// 8. Informational, all of them. None names something to fix before the
	// window: the controller, the registration, the running sessions and the
	// co-tenants name the window, and the attestation names the thing no
	// process can check.
	if pid := infraMigrationForeignControllerPID(request.CityPath); pid != 0 {
		fmt.Fprintf(stdout, "controller: PID %d is live on this city. The migration refuses while it is; stop it with `%s` when you take the window.\n", pid, storageStopCommand) //nolint:errcheck // best-effort stdout
	} else {
		// What was observed, not what it implies. The probe returns 0 both when
		// nothing is listening and when the ping itself failed — a timeout, a
		// socket it could not reach — so "none is live" would be a fact this
		// line does not have. The migration collapses the same two cases in its
		// ping, and there it is safe by direction: the controller lock it takes
		// first is what proves no controller owns the city, and this rehearsal
		// does not take it. Here the operator is deciding whether their window
		// is clear, and an unreachable controller they are told is absent is the
		// one way this line could cost them the window it was printed to protect.
		fmt.Fprintf(stdout, "controller: nothing answered this city's controller socket. The migration also takes the city's controller lock, which a controller holds before its socket answers; confirm with `%s` before you rely on it.\n", storageStopCommand) //nolint:errcheck // best-effort stdout
	}
	window.report(stdout)
	fmt.Fprintf(stdout, "attestation: --%s is not checked here or anywhere. It asserts that %s.\n", storageFleetStoppedFlag, storageFleetStoppedAttestation) //nolint:errcheck // best-effort stdout

	command := append([]string{storageMigrationCommand, "--" + storageFleetStoppedFlag}, request.Force.flags()...)
	fmt.Fprintf(stdout, "\nReady. When the fleet is stopped, run: %s\n", strings.Join(command, " ")) //nolint:errcheck // best-effort stdout
	return 0
}

// preflightWindow is what the migration refuses that taking the window ends.
type preflightWindow struct {
	// registered reports that the supervisor hosts the city.
	registered bool
	// running lists the city's running sessions.
	running []string
	// coTenants lists the other cities on the shared work store, under an
	// attestation that lifted its refusal.
	coTenants []string
}

// report names each window condition and the command that ends it.
func (w preflightWindow) report(stdout io.Writer) {
	if w.registered {
		fmt.Fprintf(stdout, "supervisor: this city is registered, and the supervisor restarts it the moment its controller stops. The migration refuses while it is; `%s` stops it and unregisters it.\n", storageStopCommand) //nolint:errcheck // best-effort stdout
	}
	if len(w.running) > 0 {
		fmt.Fprintf(stdout, "sessions: %d session(s) of this city are running (%s). The migration refuses while any is; `%s` stops them.\n", len(w.running), storageSessionList(w.running), storageStopCommand) //nolint:errcheck // best-effort stdout
	}
	if len(w.coTenants) > 0 {
		fmt.Fprintf(stdout, "co-tenants: %s also use this city's work store. The migration takes each one's controller lock and holds it until it exits, and refuses while one is held: stop them with `%s`, which keeps them registered (`%s` unregisters a city, which hides it from the shared-store check).\n", strings.Join(w.coTenants, ", "), storageCoTenantStopCommand, storageStopCommand) //nolint:errcheck // best-effort stdout
	}
}

// preflightExclusion runs the checks the migration makes while it takes its
// controller lock and the shared-store check, the same functions
// (acquireStorageVerbExclusion, findSharedStore), and blocks on each that
// stopping the city does not lift. It takes no lock: a rehearsal holding this
// city's controller lock, or a co-tenant's, would keep that city from
// starting. What taking the window ends is returned for the report.
func preflightExclusion(request storageOperatorRequest, stdout io.Writer) (preflightWindow, int) {
	var window preflightWindow
	if err := storageVerbRemoteStageRefusal(request.CityPath); err != nil {
		return window, preflightBlock(stdout, "remote stage", err)
	}
	preflightPass(stdout, "remote stage", "this directory is not a remote stage of another city")

	registered, err := storageVerbCityRegistered(request.CityPath)
	if err != nil {
		return window, preflightBlock(stdout, "supervisor", err)
	}
	window.registered = registered

	running, err := storageVerbSessionRefusal(request, storageVerbScope{Splits: true})
	if err != nil {
		return window, preflightBlock(stdout, "runtime", err)
	}
	window.running = running
	if request.Force.RemoteRuntime {
		preflightPass(stdout, "runtime", fmt.Sprintf("attested --%s=%s; the migration records it", storageForceFlag, storageHazardRemoteRuntime))
	} else {
		preflightPass(stdout, "runtime", "this city's sessions run on this host, and its runtime lists every one")
	}

	finding, err := findSharedStore(request.CityPath, sharedWorkStoreLabel, request.CityPath, request.Force)
	// The rows another city's scope names refuse under the attestation, and no
	// attestation lifts them, so they are named whether or not this run
	// attests: an operator who is told to attest learns in the same report
	// whether attesting is enough.
	var rowsErr error
	if len(finding.Sharers) > 0 {
		rowsErr = sharedStoreForeignRowRefusal(request.CityPath, request.Cfg)
	}
	switch {
	case err != nil && rowsErr != nil:
		return window, preflightBlock(stdout, "shared store", fmt.Errorf("%w\nattested, the migration would still refuse: %w", err, rowsErr))
	case err != nil:
		return window, preflightBlock(stdout, "shared store", err)
	case rowsErr != nil:
		return window, preflightBlock(stdout, "shared store", rowsErr)
	case len(finding.Sharers) > 0:
		window.coTenants = finding.coTenants()
		preflightPass(stdout, "shared store", fmt.Sprintf("attested --%s=%s:%s, and no infrastructure row in it names another city's scope", storageForceFlag, storageHazardSharedStore, finding.Token))
	default:
		preflightPass(stdout, "shared store", "no other registered city uses this city's work store")
	}
	return window, 0
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
	// preflightNoteTag marks a finding the migration would not refuse over.
	preflightNoteTag = "[note]"
	// preflightTagColumn fits the widest tag plus its trailing space.
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

// preflightNote records what a check found that the migration would not
// refuse over.
func preflightNote(stdout io.Writer, check, detail string) {
	fmt.Fprintf(stdout, preflightRowFormat, preflightNoteTag, check, detail) //nolint:errcheck // best-effort stdout
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

// preflightRigScopes is the per-rig importability census (S4b). Rig history,
// closed or suspended, no longer stops the city's migration: the relic legs
// keep it readable and `gc storage migrate --from-rigs` moves it. Each rig is
// reported with what its import would do, and only a refusal — a shape the
// import cannot move without a cross-store edge, or a rig it cannot read —
// blocks.
func preflightRigScopes(request storageOperatorRequest, target infraBindingTarget, stdout io.Writer) int {
	rows := censusRigs(request.CityPath, request.Cfg, target, true)
	if len(rows) == 0 {
		preflightPass(stdout, "rig scopes", "this city declares no rig")
		return 0
	}
	var refusals []string
	for _, r := range rows {
		switch {
		case r.State == rigCensusSkipped:
			preflightPass(stdout, "rig "+r.Rig, "n/a ("+r.Why+")")
		case r.State == rigCensusUnreachable:
			refusals = append(refusals, fmt.Sprintf("rig %s: %v", r.Rig, r.Err))
		case r.Plan != nil && len(r.Plan.Refusals) > 0:
			for _, refusal := range r.Plan.Refusals {
				refusals = append(refusals, fmt.Sprintf("rig %s: %s", r.Rig, refusal))
			}
		case r.Err != nil:
			refusals = append(refusals, fmt.Sprintf("rig %s: %v", r.Rig, r.Err))
		case r.Plan != nil && (len(r.Plan.Move) > 0 || len(r.Plan.Retained) > 0):
			closed := len(r.Plan.Move) - r.Plan.Open
			preflightNote(stdout, "rig "+r.Rig, fmt.Sprintf("%s: would import %d bead(s) (%d open, %d closed), %d open routed bead(s) become visible to demand, %d scope annotation(s), %d cross-store gate(s) to mint, %d rig cop(ies) to clear (%s); run `%s`",
				r.State, len(r.Plan.Move), r.Plan.Open, closed, r.Plan.OpenRouted, len(r.Plan.Annotated), len(r.Plan.Gates), len(r.Plan.Move)+len(r.Plan.Retained), infraStrandedIDList(append(rigPlanIDs(r.Plan.Move), rigPlanIDs(r.Plan.Retained)...)), rigImportInstruction(r.Rig)))
		default:
			preflightPass(stdout, "rig "+r.Rig, "no infrastructure bead lives in this rig's store")
		}
	}
	if len(refusals) > 0 {
		lines := strings.Split(strings.Join(refusals, "\n"), "\n")
		fmt.Fprintf(stdout, preflightRowFormat, preflightBlockTag, "rig scopes", lines[0]) //nolint:errcheck // best-effort stdout
		for _, line := range lines[1:] {
			fmt.Fprintf(stdout, "%s%s\n", preflightDetailIndent, line) //nolint:errcheck // best-effort stdout
		}
		fmt.Fprintf(stdout, "\n%s: `%s` would refuse the rig(s) named above until each fix is made; each other rig imports on its own. Fix them and run this again.\n", storagePreflightLogPrefix, rigImportAllInstruction()) //nolint:errcheck // best-effort stdout
		return 1
	}
	return 0
}
