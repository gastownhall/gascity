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
// calls, in the order the migration calls it, so a refusal that changes there
// changes here — and a check added there without a line here shows up as a
// preflight that clears a city the migration refuses, which is the one failure
// mode this verb cannot tolerate.
//
// Two things are deliberately NOT mirrored:
//
//   - The migration guard is not taken. It is exclusive, so a preflight holding
//     it would make a real migration started a moment later refuse with
//     "another storage migration holds this city" — the command an operator ran
//     to find out whether they could migrate would be the reason they could not.
//   - A live controller is reported, not refused. Every other check names
//     something the operator must go and fix before the window; the controller
//     names the window itself. Blocking on it would mean the command for
//     planning a window could only be run from inside one.
//
// It is a separate verb rather than `migrate --dry-run` for two reasons. The
// destination opener CREATES the database — that is its job — so a dry run
// sharing the migrate body would have to fork it anyway and the sharing would be
// nominal. And a mode flag on a destructive command is one typo away from the
// destructive command.

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/events"
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
		Long: `Run every check ` + storageMigrationCommand + ` runs, in the same order, and
report what it finds — without copying anything, creating anything, taking the
migration guard, or publishing any event.

This is for deciding whether the window you are about to take will be spent
migrating or spent reading a refusal. It runs against a LIVE city: a controller
serving this city is reported by PID rather than refused, because stopping it is
the next thing you were going to do anyway.

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
func doStoragePreflight(request storageOperatorRequest, stdout, stderr io.Writer) int {
	return doStoragePreflightWithRecorder(request, nil, stdout, stderr)
}

// doStoragePreflightWithRecorder is the body, with the event recorder passed in
// so a test can prove nothing is published.
//
// The recorder is threaded through and never used, which is the point: the
// storage.binding.* stream carries serving verdicts a deploy gate reads, and
// this command reaches no verdict — it reports what a migration WOULD find. A
// diagnostic that published into that stream would let a command an operator ran
// to plan a window answer a question they did not ask. Opening the city's real
// recorder is itself a write, so the read-only path never constructs one.
func doStoragePreflightWithRecorder(request storageOperatorRequest, _ events.Recorder, stdout, stderr io.Writer) int {
	fmt.Fprintf(stdout, "city: %s\n", request.CityPath) //nolint:errcheck // best-effort stdout

	// 1. The destination, resolved the way the migration resolves it. A layout
	// this build cannot serve is refused here for the same reason it is refused
	// there: a plan boot would not serve must not be migrated toward.
	target, ok, err := resolveInfraBindingTarget(request.CityPath, request.Cfg)
	if err != nil {
		return preflightBlock(stdout, "topology", err)
	}
	if !ok {
		return preflightBlock(stdout, "topology", fmt.Errorf(
			"this city's [storage.classes] do not assign %s to one shared non-work binding, so there is nothing to migrate. %s",
			infraMigrationClassList(), storageSupportedTopologyStatement))
	}
	fmt.Fprintf(stdout, "binding: %s\n  database: %s\n", target.Binding, target.Database) //nolint:errcheck // best-effort stdout
	preflightPass(stdout, "topology", "this build serves the split these classes describe")

	// 2. The same plan resolution boot performs.
	if _, err := resolveCityStoragePlan(request.CityPath, request.Cfg); err != nil {
		return preflightBlock(stdout, "plan", err)
	}
	preflightPass(stdout, "plan", "the binding resolves to an engine this build opens")

	// 3. Whether the cutover already happened. A converged city's migration is a
	// no-op that exits zero, so this clears — but clearing it silently would read
	// as "your cutover is pending and will go fine", which is the opposite of the
	// truth.
	state, err := preflightConvergenceState(target)
	if err != nil {
		return preflightBlock(stdout, "binding root", err)
	}
	if state == infraConvergenceMarked {
		preflightPass(stdout, "cutover", "already converged — the migration would find the marker and do nothing")
		fmt.Fprintf(stdout, "\nNothing to migrate: this city is already converged. Run `%s` to see what it holds.\n", storageStatusInstruction()) //nolint:errcheck // best-effort stdout
		return 0
	}
	if state == infraConvergenceStale {
		preflightPass(stdout, "cutover", "a convergence marker exists with no database under it, so the copy would run again and re-converge")
	} else {
		preflightPass(stdout, "cutover", "not converged yet, which is what the migration is for")
	}

	// 4. The expensive one, and the reason this verb most earns its place: a bead
	// in a rig scope is refused BY NAME and no command this binary carries can
	// repair it. Finding that out inside a stopped-city window is the worst
	// possible time.
	if err := censusRigInfraResidue(request.CityPath, request.Cfg); err != nil {
		return preflightBlock(stdout, "rig scopes", err)
	}
	preflightPass(stdout, "rig scopes", "no infrastructure bead lives outside this city's work store")

	// 5. What the copy would carry. A read that failed leaves the whole clearance
	// unfounded, so it blocks rather than reporting zero — the same
	// positive-looking absence this path refuses everywhere else.
	source, err := openInfraMigrationSource(request.CityPath)
	if err != nil {
		return preflightBlock(stdout, "work store", fmt.Errorf("opening the work store: %w", err))
	}
	rows, listErr := readInfraSnapshot(source)
	if closeErr := closeBeadStoreHandle(source); closeErr != nil {
		fmt.Fprintf(stderr, "%s: closing the work store: %v\n", storagePreflightLogPrefix, closeErr) //nolint:errcheck // best-effort stderr
	}
	if listErr != nil {
		return preflightBlock(stdout, "work store", listErr)
	}
	preflightPass(stdout, "work store", fmt.Sprintf("would copy %d infrastructure bead(s)", len(rows)))

	// 6. Informational, both of them. Neither names something to fix before the
	// window; one names the window and one names the thing no process can check.
	if pid := infraMigrationForeignControllerPID(request.CityPath); pid != 0 {
		fmt.Fprintf(stdout, "controller: PID %d is live on this city. The migration refuses while it is; stop it with `%s` when you take the window.\n", pid, storageStopCommand) //nolint:errcheck // best-effort stdout
	} else {
		fmt.Fprintln(stdout, "controller: none is live on this city.") //nolint:errcheck // best-effort stdout
	}
	fmt.Fprintf(stdout, "attestation: --%s is not checked here or anywhere. It asserts that %s.\n", storageFleetStoppedFlag, storageFleetStoppedAttestation) //nolint:errcheck // best-effort stdout

	fmt.Fprintf(stdout, "\nReady. When the fleet is stopped, run: %s --%s\n", storageMigrationCommand, storageFleetStoppedFlag) //nolint:errcheck // best-effort stdout
	return 0
}

// preflightConvergenceState reads whether this city already cut over, and
// refuses to answer from a binding root it could not look inside.
//
// An absent root is the ORDINARY pre-cutover state — openInfraDestination is the
// only thing that creates it and has never run — so it is not a fault here, the
// way it is on a city whose evidence is being weighed. A root that is present
// but unreadable is a different matter: every path under it reads absent, so the
// marker would report "never converged" for a city that may well have.
func preflightConvergenceState(target infraBindingTarget) (infraConvergenceState, error) {
	present, err := infraPathExists(target.Root)
	if err != nil {
		return infraConvergenceAbsent, err
	}
	if !present {
		return infraConvergenceAbsent, nil
	}
	if err := infraBindingRootEnumerable(target.Root); err != nil {
		return infraConvergenceAbsent, err
	}
	return readInfraConvergenceState(target)
}

// preflightPass records a check the migration would clear.
func preflightPass(stdout io.Writer, check, detail string) {
	fmt.Fprintf(stdout, "  [ok]    %-11s %s\n", check, detail) //nolint:errcheck // best-effort stdout
}

// preflightBlock records the check that would refuse the migration and returns
// the command's exit code.
//
// The fault goes to stdout with the checks that preceded it rather than to
// stderr, because the report is the answer: an operator reading "which of these
// would stop me" needs the failing line in the same list as the passing ones.
// The exit code carries the verdict for anything reading it instead.
func preflightBlock(stdout io.Writer, check string, cause error) int {
	fmt.Fprintf(stdout, "  [BLOCK] %-11s %v\n", check, cause)                                                                              //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "\n%s: the migration would refuse. Fix the blocking check above and run this again.\n", storagePreflightLogPrefix) //nolint:errcheck // best-effort stdout
	return 1
}
