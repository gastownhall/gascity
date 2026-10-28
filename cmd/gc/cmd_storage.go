package main

// `gc storage` — the operator surface for a city's storage-class layout.
//
// The whole tree is built from the two constants this program spells its
// operator commands in: storageMigrationCommand for the cutover and
// storageRecoveryCommand for the stranded-write repair. The refusals print
// those same constants (infra_class_migrate.go), so building the commands from
// them is what stops a refusal and a command from drifting apart in the one
// direction that matters: a refusal naming a command the binary does not carry
// is an operator instruction that fails at the shell — and a refusal naming no
// command at all is the defect the repair verb was added to close.
//
// The source is stated explicitly rather than detected. `migrate` carries one
// source shape — the work store this city is running on — and refuses if the
// operator does not name it, so a second source added later cannot inherit this
// one's behavior by silence. There is no dispatch table for that second source
// yet, on purpose: with one arm a table is shape without content, and an arm
// added later owns its own preflight, copy, proof and markers end to end,
// sharing nothing implicitly except the guard and the refusal grammar.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/storebinding"
	"github.com/gastownhall/gascity/internal/storeref"
)

const (
	// storageMigrationCommand is the one spelling of the command that performs
	// the move. The boot refusal prints it and this file parses it into the
	// cobra tree, so the two cannot disagree.
	storageMigrationCommand = "gc storage migrate --from-work"

	// storageRecoveryCommand is the one spelling of the command that repairs a
	// stranded write. The stranded refusal prints it and this file parses it
	// into the cobra tree, for the same reason the migration spelling is
	// handled that way — and because the defect this command closes was
	// precisely an alarm that named NO verb: a city acquired strands, every
	// later command carried a permanent refusal, and the refusal's remedy was
	// a sentence rather than something an operator could run.
	storageRecoveryCommand = "gc storage recover-stranded --from-work"

	// storageStatusVerb is the read-only sibling of the migrate verb.
	storageStatusVerb = "status"

	// storageStopCommand is the one spelling of the command that clears the
	// controller a migration refuses under. Both the migration's refusal and the
	// preflight's advisory print it, for the reason the file header gives: an
	// instruction naming a command this binary does not carry fails at the shell.
	storageStopCommand = "gc stop"

	// storageFromBackupFlag names the second source the migrate verb takes:
	// the retained-source backup a clear wrote, for re-copying a binding whose
	// database is gone after the work store's copies were cleared.
	storageFromBackupFlag = "from-backup"

	// storageFleetStoppedFlag is the operator's attestation that nothing this
	// process cannot see is still writing to the source.
	storageFleetStoppedFlag = "fleet-stopped"

	// storageFleetStoppedAttestation states what the operator is attesting to.
	// It is a sentence rather than a word because an attestation nobody can
	// read is an attestation nobody made.
	storageFleetStoppedAttestation = "every writer that can reach this city's work store is stopped — not just its controller, which this command proves on its own"

	// storageMigrationGeneration is the durable generation this build's layout
	// belongs to. The guard needs a valid generation and this build has exactly
	// one, so it is a constant rather than a knob nobody could set correctly.
	storageMigrationGeneration = storebinding.Generation(1)
)

// storageCommandSurface is the cobra tree an operator-command spelling names.
type storageCommandSurface struct {
	// Namespace is the parent command, `storage` in `gc storage migrate`.
	Namespace string
	// Verb is the leaf command that performs the move.
	Verb string
	// Flag is the source flag the leaf requires, without its dashes.
	Flag string
}

// parseOperatorCommandSpelling decomposes an operator-command spelling into the
// cobra tree that has to serve it.
//
// It takes the spelling as an argument rather than reading the constant so the
// rejections are reachable from a test. A spelling this cannot decompose is a
// build-time defect, and the only honest thing to do with it at runtime is to
// say so — which is what newStorageCmdFromSpelling returns a command for.
func parseOperatorCommandSpelling(spelling string) (storageCommandSurface, error) {
	fields := strings.Fields(spelling)
	if len(fields) != 4 {
		return storageCommandSurface{}, fmt.Errorf("the operator command %q is not `gc <namespace> <verb> --<flag>`: it has %d word(s), want 4", spelling, len(fields))
	}
	if fields[0] != "gc" {
		return storageCommandSurface{}, fmt.Errorf("the operator command %q does not start with the binary name", spelling)
	}
	flag := strings.TrimPrefix(fields[3], "--")
	if flag == fields[3] || flag == "" {
		return storageCommandSurface{}, fmt.Errorf("the operator command %q does not end in a --flag", spelling)
	}
	for _, word := range []string{fields[1], fields[2]} {
		if strings.HasPrefix(word, "-") {
			return storageCommandSurface{}, fmt.Errorf("the operator command %q names a flag where a command belongs", spelling)
		}
	}
	return storageCommandSurface{Namespace: fields[1], Verb: fields[2], Flag: flag}, nil
}

// storageRecoveryInstruction renders the recovery command the way an operator
// types it: the spelling the tree is built from, plus the attestation the
// repair requires before it writes anything.
//
// The attestation is appended here rather than folded into the constant because
// parseOperatorCommandSpelling decomposes exactly four words into a namespace,
// a verb and a source flag. --fleet-stopped is a flag on the leaf, not part of
// the tree, so putting it in the constant would make the spelling
// undecomposable and the whole tree unbuildable.
func storageRecoveryInstruction() string {
	return storageRecoveryCommand + " --" + storageFleetStoppedFlag
}

// storageClearInstruction is the operator command that finishes a converged
// city's cutover by clearing the retained work-store copies: the migration
// itself, re-run. On a converged city it re-proves convergence and then clears,
// so the command that repairs a city migrated by an earlier build is the same
// one an operator already ran.
func storageClearInstruction() string {
	return storageMigrationCommand + " --" + storageFleetStoppedFlag
}

// storageRecopyFromBackupInstruction re-copies a stale binding from the
// retained-source backup, for a city whose work store no longer holds the
// slice.
func storageRecopyFromBackupInstruction() string {
	surface, err := parseOperatorCommandSpelling(storageMigrationCommand)
	if err != nil {
		return "gc storage migrate --" + storageFromBackupFlag + " --" + storageFleetStoppedFlag
	}
	return "gc " + surface.Namespace + " " + surface.Verb + " --" + storageFromBackupFlag + " --" + storageFleetStoppedFlag
}

// storageStatusInstruction renders the read-only report the way an operator
// types it. The read verb has no operator-command spelling of its own — it
// takes no source flag — so its namespace is derived from the one the migrate
// spelling names, for the reason the file header gives: a message pointing at a
// command this binary does not carry is an instruction that fails at the shell.
// An unparseable spelling is already reported by newUnbuildableStorageCmd, so
// here it degrades to the default namespace rather than swallowing the message
// that carries it.
func storageStatusInstruction() string {
	surface, err := parseOperatorCommandSpelling(storageMigrationCommand)
	if err != nil {
		return "gc storage " + storageStatusVerb
	}
	return "gc " + surface.Namespace + " " + storageStatusVerb
}

// newStorageCmd constructs the `gc storage` tree named by the operator
// spellings.
func newStorageCmd(stdout, stderr io.Writer) *cobra.Command {
	return newStorageCmdFromSpellings(storageMigrationCommand, storageRecoveryCommand, stdout, stderr)
}

// newStorageCmdFromSpellings is the testable body of newStorageCmd.
//
// Both spellings are arguments rather than reads of the constants, for the
// reason parseOperatorCommandSpelling states: the rejections have to be
// reachable from a test. The namespaces must agree — two operator commands that
// named different parents would build one tree and leave the other spelling
// naming a command that does not resolve, which is the exact failure the
// spelling-derived tree exists to prevent.
func newStorageCmdFromSpellings(migration, recovery string, stdout, stderr io.Writer) *cobra.Command {
	surface, err := parseOperatorCommandSpelling(migration)
	if err != nil {
		return newUnbuildableStorageCmd(err, stderr)
	}
	repair, err := parseOperatorCommandSpelling(recovery)
	if err != nil {
		return newUnbuildableStorageCmd(err, stderr)
	}
	if repair.Namespace != surface.Namespace {
		return newUnbuildableStorageCmd(fmt.Errorf("the operator commands %q and %q name different parents (%q and %q); one tree cannot serve both",
			migration, recovery, surface.Namespace, repair.Namespace), stderr)
	}
	cmd := &cobra.Command{
		Use:   surface.Namespace,
		Short: "Inspect and migrate this city's storage-class layout",
		Long: `Move this city's storage-class layout, and report on it.

These are operator commands. Nothing here runs on boot: a city whose
[storage.classes] name a binding it has not converged on refuses to start and
names the migrate command, because the source's writer set is something an
operator arranges rather than something a program can observe.`,
	}
	cmd.AddCommand(
		newStorageMigrateCmd(surface, stdout, stderr),
		newStoragePreflightCmd(surface, stdout, stderr),
		newStorageStatusCmd(surface, stdout, stderr),
		newStorageRecoverCmd(repair, stdout, stderr),
		newStorageRepairSequenceCmd(surface, stdout, stderr),
		newStorageVouchBdCmd(surface, stdout, stderr),
	)
	return cmd
}

// newUnbuildableStorageCmd is what an operator-command spelling this program
// cannot decompose becomes.
//
// Command construction has no error path and a panic would take the whole
// binary down over a string, so the fault is carried into a command that
// reports it. The name is a fallback because there is no spelling to take one
// from — that is the defect being reported.
func newUnbuildableStorageCmd(cause error, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:          "storage",
		Short:        "Storage-class layout (unavailable in this build)",
		SilenceUsage: true,
		RunE: func(*cobra.Command, []string) error {
			fmt.Fprintf(stderr, "gc storage: this build cannot serve its own operator command: %v\n", cause) //nolint:errcheck // best-effort stderr
			return errExit
		},
	}
}

// newStorageMigrateCmd is the command the boot refusal names.
func newStorageMigrateCmd(surface storageCommandSurface, stdout, stderr io.Writer) *cobra.Command {
	var (
		fromWork     bool
		fromBackup   bool
		fleetStopped bool
	)
	cmd := &cobra.Command{
		Use:          surface.Verb,
		Short:        "Migrate this city's infrastructure classes onto their configured binding",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		Long: `Copy this city's infrastructure-class beads out of the work store and into
the binding [storage.classes] assigns them to, then clear the work store's
copies.

Every bead is copied with its id and its within-class dependency topology
preserved, proven field-equal against a closed and reopened destination, and
then recorded in a proven-copy manifest and a convergence marker. Nothing in
the work store changes before the marker, so a rollback before cutover is a
config edit with no data recovery step.

Past the marker the binding is the only authoritative copy, so the work
store's now-stale copies are cleared: each is written to a backup beside the
manifest (infra.retained-source.jsonl), the backup is re-read and proven equal
to the rows it records, and only then are the rows removed. A work bead whose
blocking dependency the binding has already satisfied is released; every other
cross-store edge is kept.

Run it again on a converged city to repair one that still holds its copies —
a city migrated by an earlier build, or a clear that was interrupted. Boot
refuses such a city and names this command.

--from-backup re-copies a binding whose database is gone from that backup
instead of from the work store, which no longer holds the slice.

Last, on the converged binding, each open deliver-now unit the copy carried in
is re-lowered under the binding's id prefix: its derived interrupt is re-created
at the id a lowering there gives it, with its state, and the old row is closed
as a pointer to it, one all-or-nothing write per unit. A unit in flight or that
may have run, one with more than one open derived interrupt, and one whose id
there another row already holds are left as they are and named; their lanes run
each one as a unit by identity. A message not yet lowered, or whose unit is
over, is not a unit to move, and is not named. A city whose binding holds no
lowered deliver-now message skips the step and prints nothing for it. A step
that did not finish exits non-zero, and running this command again finishes it.

The move refuses while a writer can reach the source. This binary can prove the
absence of a controller and cannot prove the absence of anything else, so that
half is an explicit operator attestation.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if fromWork == fromBackup {
				fmt.Fprintf(stderr, "gc %s %s: pass exactly one of --%s or --%s. The source is stated explicitly rather than detected, so a source added later cannot inherit another's behavior by silence\n", //nolint:errcheck // best-effort stderr
					surface.Namespace, surface.Verb, surface.Flag, storageFromBackupFlag)
				return errExit
			}
			request, err := resolveStorageOperatorRequest()
			if err != nil {
				fmt.Fprintf(stderr, "gc %s %s: %v\n", surface.Namespace, surface.Verb, err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			request.FleetStopped = fleetStopped
			request.FromBackup = fromBackup
			return exitForCode(doStorageMigrate(cmd.Context(), request, stdout, stderr))
		},
	}
	cmd.Flags().BoolVar(&fromWork, surface.Flag, false,
		"migrate the infrastructure classes out of this city's work store")
	cmd.Flags().BoolVar(&fromBackup, storageFromBackupFlag, false,
		"re-copy a binding whose database is gone from the retained-source backup")
	cmd.Flags().BoolVar(&fleetStopped, storageFleetStoppedFlag, false,
		"attest that "+storageFleetStoppedAttestation)
	return cmd
}

// newStorageStatusCmd is the read-only sibling: it describes the layout and
// creates nothing.
func newStorageStatusCmd(surface storageCommandSurface, stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:          storageStatusVerb,
		Short:        "Report this city's storage-class layout (read-only)",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		Long: `Report which binding serves each storage class, whether this city has
converged onto it, and what the retained source and the binding each hold.

This path is read-only against a LIVE city: it creates no directory, database,
write-ahead log, shared-memory index, schema, marker or manifest. It does not
open the binding's engine unless that database already exists, because opening
it would create the very database the report is being asked about.

It exits non-zero when the city is configured for a binding it has not
converged on, so a deployment script can gate on it. That is the ordinary state
of every city with a cutover still ahead of it, and it is NOT a fault report: a
non-zero status here says the migration has not run, not that it would fail. To
find out whether it would fail, run ` + "`gc storage " + storagePreflightVerb + "`" + `, which rehearses
every check the migration makes without migrating.`,
		RunE: func(*cobra.Command, []string) error {
			request, err := resolveStorageOperatorRequest()
			if err != nil {
				fmt.Fprintf(stderr, "gc %s %s: %v\n", surface.Namespace, storageStatusVerb, err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			return exitForCode(doStorageStatus(request, stdout, stderr))
		},
	}
	return cmd
}

// storageOperatorRequest is one resolved city and its configuration.
type storageOperatorRequest struct {
	CityPath string
	Cfg      *config.City
	// FleetStopped carries the operator's attestation about the writers this
	// command cannot probe.
	FleetStopped bool
	// FromBackup selects the retained-source backup as the migration's source.
	FromBackup bool
}

// resolveStorageOperatorRequest resolves the city and its configuration.
//
// It loads the configuration WITHOUT refreshing builtin packs, and that is not
// an optimization: the refresh materializes generated packs into the city, and
// the read-only report shares this path. A status command that rewrote part of
// the city it was asked to describe would falsify its own claim.
func resolveStorageOperatorRequest() (storageOperatorRequest, error) {
	cityPath, err := resolveCity()
	if err != nil {
		return storageOperatorRequest{}, err
	}
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil {
		return storageOperatorRequest{}, fmt.Errorf("loading %s: %w", cityPath, err)
	}
	if cfg == nil {
		return storageOperatorRequest{}, errors.New("this city loaded no configuration")
	}
	return storageOperatorRequest{CityPath: cityPath, Cfg: cfg}, nil
}

// doStorageMigrate performs the move, in the order the hazards demand.
//
// Structural validation first, because a plan boot would refuse must not be
// migrated toward; then the writers, because a copy of a moving source proves
// nothing; then the guard, because two migrators are worse than one; then,
// unless the city already converged and nothing is copied, the rig census,
// because a bead this city's own scopes hold outside the work store is one the
// copy will never see; and only then the copy itself.
func doStorageMigrate(ctx context.Context, request storageOperatorRequest, stdout, stderr io.Writer) int {
	const logPrefix = "gc storage migrate"

	// A split the boot gate would refuse to serve (a reserved HQ or rig
	// prefix, R10) is not created either.
	if err := config.ReservedPrefixSplitRefusal(request.Cfg); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}

	target, ok, err := resolveInfraBindingTarget(request.CityPath, request.Cfg)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !ok {
		fmt.Fprintf(stderr, "%s: this city's [storage.classes] do not assign %s to one shared non-work binding, so there is nothing to migrate. %s\n", //nolint:errcheck // best-effort stderr
			logPrefix, infraMigrationClassList(), storageSupportedTopologyStatement)
		return 1
	}
	// The same resolution boot performs, performed first: a plan the runtime
	// would refuse to serve must not be migrated toward, or the operator ends
	// up with a proven copy in a binding that still cannot start.
	plan, err := resolveCityStoragePlan(request.CityPath, request.Cfg)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if pid := infraMigrationForeignControllerPID(request.CityPath); pid != 0 {
		fmt.Fprintf(stderr, "%s: controller PID %d is live on this city and is still writing to the work store. Stop it (%s) and run this again\n", logPrefix, pid, storageStopCommand) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !request.FleetStopped {
		fmt.Fprintf(stderr, "%s: pass --%s to attest that %s. This command proves the controller is stopped and cannot prove anything about the rest; a write that lands in the source after the copy is proven stays stranded there, and every later boot names it rather than losing it\n", //nolint:errcheck // best-effort stderr
			logPrefix, storageFleetStoppedFlag, storageFleetStoppedAttestation)
		return 1
	}

	guard, err := storebinding.AcquireMigrationGuard(ctx, cityMigrationGuardDirectory(request.CityPath), storageMigrationGeneration)
	if err != nil {
		if errors.Is(err, storebinding.ErrMigrationGuardBusy) {
			fmt.Fprintf(stderr, "%s: another storage migration holds this city. Wait for it to finish, or resolve it, and run this again\n", logPrefix) //nolint:errcheck // best-effort stderr
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

	// The rig census guards a COPY: a bead a rig scope holds is one the copy
	// never sees. A city that already converged copies nothing on this run —
	// it re-checks containment and clears the retained copies — so a rig row
	// is exactly as unreachable after it as before, and refusing over one
	// would put the clear, the only step that ends the retained copies boot
	// refuses over, out of reach. `gc storage preflight` still names those
	// rows. A state that cannot be read is censused, and the migration below
	// reports the read.
	if state, err := readInfraConvergenceState(target); err != nil || state != infraConvergenceMarked {
		if err := censusRigInfraResidue(request.CityPath, request.Cfg); err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	from := infraMigrationFromWork
	if request.FromBackup {
		from = infraMigrationFromBackup
	}
	report := runInfraClassMigrationFrom(request.CityPath, target, from, logPrefix, stderr)
	report.Target = target
	report.BindingProvenEmpty, report.BindingProbe = infraBindingHoldsNothing(target)
	rec := openCityRecorderAt(request.CityPath, stderr)
	if !report.serving() {
		advice := infraMigrationOperatorAdvice(report, logPrefix)
		if advice != "" {
			fmt.Fprintln(stderr, advice) //nolint:errcheck // best-effort stderr
		}
		recordStorageBindingOutcome(rec, report, advice)
		return 1
	}
	recordStorageBindingOutcome(rec, report, "")
	// The served-binding note, now rather than at the first boot: the binding
	// holds this city's infrastructure state from this moment. Without it a
	// city migrated but not yet booted carried no evidence outside its config
	// that it is split, so a truncated city.toml routed every class to the
	// work store (split-default R1, re-review probe P3). The note is the one
	// the boot gate writes (recordServedBinding), so the first boot finds it
	// already current.
	if err := recordServedBinding(plan, request.CityPath, request.Cfg.EffectiveStorage(), target); err != nil {
		fmt.Fprintf(stderr, "%s: recording the served binding: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	// The last step, on a converged binding only: every other binding handle
	// of this command is closed, and the step opens the binding as the
	// runtime serves it.
	var relowered opjournal.RelowerReport
	if converged, ok := report.convergedBinding(plan, request.CityPath, request.Cfg); ok {
		relowered, err = relowerConvergedNowUnits(ctx, converged, rec, logPrefix, stderr)
		if err != nil {
			fmt.Fprintln(stderr, relowerFailureAdvice(logPrefix, target.Binding, err)) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	fmt.Fprintf(stdout, "Migrated. %s now serves %s from %s; %s%s. Start the city with `gc start`.\n", //nolint:errcheck // best-effort stdout
		target.Binding, infraMigrationClassList(), target.Database, describeInfraClear(report.Clear), relowerSuccessClause(relowered))
	return 0
}

// doStorageStatus reports the layout without changing it.
func doStorageStatus(request storageOperatorRequest, stdout, stderr io.Writer) int {
	const logPrefix = "gc storage status"

	storage := request.Cfg.EffectiveStorage()
	fmt.Fprintf(stdout, "city: %s\n", request.CityPath)                           //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "[storage] configured: %t\n", request.Cfg.Storage != nil) //nolint:errcheck // best-effort stdout
	for _, class := range storageClassOrder() {
		fmt.Fprintf(stdout, "  %-9s -> %s\n", class, storage.Classes.BindingFor(class)) //nolint:errcheck // best-effort stdout
	}

	// Resolve the boot plan once on every configured path. This command's exit
	// code is a deploy gate — "a city boot refuses must not report may-serve
	// here" — and that contract used to hold only on the born-split path below,
	// the one path that resolved the plan. The configured served and all-work
	// paths returned 0 without ever asking.
	//
	// The no-[storage] compatibility path deliberately reaches no registry or
	// plan, matching storageBootGate. A legacy city must not acquire a new boot
	// refusal merely because the compiled provider registry cannot be built.
	//
	// A refusal is reported and carried into the exit code rather than
	// returned on, because it is the moment an operator most needs the rest of
	// the readout.
	var plan *storebinding.StoragePlan
	var planErr error
	exitCode := 0
	if request.Cfg.Storage != nil {
		plan, planErr = resolveCityStoragePlan(request.CityPath, request.Cfg)
		if planErr != nil {
			fmt.Fprintf(stdout, "boot plan: REFUSED — a city boot would not serve this configuration: %v\n", planErr) //nolint:errcheck // best-effort stdout
			exitCode = 1
		}
		// Boot refuses a split city whose HQ or rig prefix is a reserved
		// class prefix (R10) before it resolves a plan.
		if err := config.ReservedPrefixSplitRefusal(request.Cfg); err != nil {
			fmt.Fprintf(stdout, "boot plan: REFUSED — a city boot would not serve this configuration: %v\n", err) //nolint:errcheck // best-effort stdout
			exitCode = 1
		}
	}

	target, ok, err := resolveInfraBindingTarget(request.CityPath, request.Cfg)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !ok {
		if shape, binding := storageSplitShapeOf(storage); shape == storageSplitWhole {
			// The split shape is supported and only the provider refused the
			// target: a binding this build cannot migrate onto, serving under
			// the born-split discipline. Report that discipline's state — the
			// same one boot enforces — rather than claiming the work store
			// serves classes it does not.
			provider := storage.Bindings[binding].Provider
			fmt.Fprintf(stdout, "binding: %s\n  provider: %s (not this build's engine; serves under the born-split discipline)\n", binding, provider) //nolint:errcheck // best-effort stdout
			// The seam check boot performs, against the plan resolved above.
			// A refusal has already been reported in the body; there is no
			// plan to check the seam against, so the readout stops here.
			if planErr != nil {
				fmt.Fprintf(stderr, "%s: %v\n", logPrefix, planErr) //nolint:errcheck // best-effort stderr
				return 1
			}
			if plannedBindingOpener(plan, binding) == nil {
				fmt.Fprintf(stdout, "born-split: BLOCKED — provider %s does not open a bead engine, so the classes assigned to binding %s cannot be served\n", provider, binding) //nolint:errcheck // best-effort stdout
				return 1
			}
			location, locErr := servedBindingLocation(plan, binding, storage.Bindings[binding])
			if locErr != nil {
				fmt.Fprintf(stderr, "%s: %v\n", logPrefix, locErr) //nolint:errcheck // best-effort stderr
				return 1
			}
			if blocked, held := servedBindingNoteHold(request.CityPath, binding, provider, location); held {
				fmt.Fprintf(stdout, "born-split: BLOCKED — %s\n", infraMigrationOperatorAdvice(blocked, logPrefix)) //nolint:errcheck // best-effort stdout
				return 1
			}
			report := checkBornSplitDiscipline(request.CityPath, logPrefix, stderr)
			switch report.Outcome {
			case infraMigrationConverged:
				fmt.Fprintln(stdout, "born-split: clean — the work store holds no infrastructure bead, so the binding may serve.") //nolint:errcheck // best-effort stdout
				return exitCode
			case infraMigrationBornSplitBlocked:
				fmt.Fprintf(stdout, "born-split: BLOCKED — the work store holds %d infrastructure bead(s) the binding cannot read: %s\n", //nolint:errcheck // best-effort stdout
					len(report.Stranded), strings.Join(report.Stranded, ", "))
				return 1
			default:
				fmt.Fprintf(stdout, "born-split: could not be verified (%s); reason on stderr\n", report.Outcome) //nolint:errcheck // best-effort stdout
				return 1
			}
		}
		if blocked, held := revertHoldingNote(storageSplitNone, request.CityPath); held {
			fmt.Fprintf(stdout, "binding: none configured, and BLOCKED — %s\n", infraMigrationOperatorAdvice(blocked, logPrefix)) //nolint:errcheck // best-effort stdout
			return 1
		}
		fmt.Fprintln(stdout, "binding: none — every class is served by the work store, and nothing migrates.") //nolint:errcheck // best-effort stdout
		return exitCode
	}
	fmt.Fprintf(stdout, "binding: %s\n  database: %s\n  marker:   %s\n  manifest: %s\n", //nolint:errcheck // best-effort stdout
		target.Binding, target.Database, target.MarkerPath(), target.ManifestPath())

	if blocked, held := servedBindingNoteHold(request.CityPath, target.Binding, config.StorageProviderSQLiteBeads, target.Database); held {
		blocked.Target = target
		fmt.Fprintf(stdout, "served-binding hold: %s\n", infraMigrationOperatorAdvice(blocked, logPrefix)) //nolint:errcheck // best-effort stdout
		return 1
	}

	state, err := readInfraConvergenceState(target)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	// A converged city's work store is counted under its manifest's classifier
	// generation, the one every verdict below judges it by: rows the copy's
	// classifier left as work are not infrastructure that city retains.
	var manifest infraCopyManifest
	recorded := false
	if state == infraConvergenceMarked {
		manifest, recorded, err = readInfraCopyManifestRecord(target)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	generation := coordclass.CurrentGeneration
	if recorded {
		generation = manifest.Generation
	}
	source, err := openInfraMigrationSource(request.CityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: opening the work store: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	defer closeBeadStoreHandle(source) //nolint:errcheck // best-effort close
	rows, err := readInfraSnapshotAt(source, generation)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "source: %d infrastructure bead(s) retained in the work store\n", len(rows)) //nolint:errcheck // best-effort stdout
	if backup, present, err := readInfraRetainedBackup(target); err != nil {
		fmt.Fprintf(stdout, "retained-source backup: unreadable — %v\n", err) //nolint:errcheck // best-effort stdout
	} else if present {
		fmt.Fprintf(stdout, "retained-source backup: %s (%d cleared bead(s))\n", target.RetainedBackupPath(), len(backup)) //nolint:errcheck // best-effort stdout
	}
	// Both sides, always — including on the unconverged arm below, where the
	// binding's zero is the whole point. The source count alone cannot tell an
	// operator whether a cutover landed: it reads zero both on a cleared city
	// and on one that never held an infrastructure bead.
	//
	// A census that could not run prints its reason in place of the number
	// instead of a confident zero. The unreadable case is a whole missing binding
	// root, which takes the marker and the manifest with it, so this line would
	// otherwise sit next to "converged: no" as a second piece of positive-looking
	// evidence for a city that may well have cut over.
	if held, err := infraBindingCensus(target); err != nil {
		fmt.Fprintf(stdout, "binding: unknown — %v\n", err) //nolint:errcheck // best-effort stdout
	} else {
		fmt.Fprintf(stdout, "binding: %d infrastructure bead(s) held now\n", held) //nolint:errcheck // best-effort stdout
	}

	if state == infraConvergenceStale {
		remedy := storageClearInstruction()
		if present, err := infraPathExists(target.RetainedBackupPath()); err == nil && present {
			remedy = storageRecopyFromBackupInstruction()
		}
		fmt.Fprintf(stdout, "converged: no (the marker is present and the database is gone)\nblocking invariant: re-copy the binding with `%s`\n", remedy) //nolint:errcheck // best-effort stdout
		return 1
	}
	if state != infraConvergenceMarked {
		fmt.Fprintf(stdout, "converged: no\nblocking invariant: boot never migrates; run `%s`\n", storageMigrationCommand) //nolint:errcheck // best-effort stdout
		return 1
	}
	reportBindingRelics(target, logPrefix, stdout, stderr)

	if !recorded {
		fmt.Fprintln(stdout, "converged: yes (no proven-copy manifest, so stranded-write detection is off for this city)") //nolint:errcheck // best-effort stdout
		return exitCode
	}
	gap, err := classifyInfraContainmentGap(request.CityPath, target, manifest)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "converged: yes\n  proven copy: %d bead(s)\n  stranded:    %d\n  retained copies: %d\n", //nolint:errcheck // best-effort stdout
		manifest.Len(), len(gap.Stranded), len(gap.Retained))
	if len(gap.Stranded) > 0 {
		fmt.Fprintf(stdout, "  stranded ids: %s\n", strings.Join(gap.Stranded, ", ")) //nolint:errcheck // best-effort stdout
		// The exit code is the deploy gate; the remedy is what an operator does
		// about it. This report used to carry only the ids, so the one command
		// an operator would reach for after a non-zero `gc storage status` said
		// nothing about how to make it zero again.
		if len(gap.HeldDifferent) > 0 {
			fmt.Fprintf(stdout, "  held for a different bead: %s\n", strings.Join(gap.HeldDifferent, ", ")) //nolint:errcheck // best-effort stdout
		}
		if len(gap.PresumedReused) > 0 {
			fmt.Fprintf(stdout, "  presumed reused (bare id, younger than the legacy manifest): %s\n", strings.Join(gap.PresumedReused, ", ")) //nolint:errcheck // best-effort stdout
		}
		// Split by why each id is stranded (infraStrandedAdvice): a held
		// different bead and a presumed-reused row need a per-id choice, and
		// only an absent row is told to copy it in.
		fmt.Fprintf(stdout, "blocking invariant: %s\n", infraStrandedAdvice(gap.Stranded, gap.HeldDifferent, gap.PresumedReused)) //nolint:errcheck // best-effort stdout
		return 1
	}
	note, notePresent, err := readInfraClearedNote(request.CityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if len(note.LostCrossEdges) > 0 {
		fmt.Fprintf(stdout, "  lost cross-store edges: %d (the work store could not keep these edges from work beads into beads the migration moved; each is recorded in the retained-source backup)\n", len(note.LostCrossEdges)) //nolint:errcheck // best-effort stdout
		for _, edge := range note.LostCrossEdges {
			fmt.Fprintf(stdout, "    %s\n", edge) //nolint:errcheck // best-effort stdout
		}
	}
	if notePresent && !note.Complete {
		fmt.Fprintf(stdout, "  clear: INTERRUPTED (%d bead(s) in the unfinished session)\nblocking invariant: an earlier clear of the retained copies did not finish. Stop the city and finish it with `%s`\n", len(note.Pending), storageClearInstruction()) //nolint:errcheck // best-effort stdout
		return 1
	}
	if len(gap.Retained) > 0 {
		fmt.Fprintf(stdout, "  retained ids: %s\n", infraStrandedIDList(gap.Retained)) //nolint:errcheck // best-effort stdout
		// The same refusal boot makes: a second row under an id the binding
		// owns is served to every reader that reaches the work store first.
		// The clear removes only the copies the manifest records, so the
		// others are named with the command that records them.
		if len(gap.Unrecorded) > 0 {
			fmt.Fprintf(stdout, "  unrecorded ids: %s\nblocking invariant: the work store still holds copies of beads the binding owns, and readers that reach it first are served the frozen copy. The copy manifest does not record %d of them as beads the copy carried, and the clear removes only the copies it records. Stop the city, record them with `%s`, then clear them with `%s`\n", infraStrandedIDList(gap.Unrecorded), len(gap.Unrecorded), storageRecoveryInstruction(), storageClearInstruction()) //nolint:errcheck // best-effort stdout
			return 1
		}
		fmt.Fprintf(stdout, "blocking invariant: the work store still holds copies of beads the binding owns, and readers that reach it first are served the frozen copy. Stop the city and clear them with `%s`\n", storageClearInstruction()) //nolint:errcheck // best-effort stdout
		return 1
	}
	return exitCode
}

// reportBindingRelics prints how many beads the binding still holds under ids
// no reader can route to it from — the rows the cutover carried across under
// their original work-shaped ids.
//
// This is a DRAIN gauge, not the residence probe's retirement condition — the
// two parted company in ga-qdt5y.19. Retirement asks whether the binding can
// hold an id at all, and a closed relic still answers yes: the migration never
// deleted the work store's frozen pre-migration copy, so retiring on the last
// CLOSE would serve that id from the frozen copy forever. So the verdict counts
// closed residents (storeref.LegacyResidents) and this count deliberately does
// not — an operator draining a migrated city needs a number that reaches zero,
// and the widened one never can.
//
// What reaching zero means, then, is that the carried-across work is finished,
// not that the per-read cost is gone. A city that migrated any beads keeps its
// probe for good.
//
// It never changes the exit code. A relic is the migration working as designed,
// so a status command that failed over one would report every city that ever
// migrated as broken. A binding that cannot be read is reported and skipped for
// the same reason: the layout facts above it stand on their own.
//
// It reads through openInfraBindingReadOnly for the reason that opener states:
// its only caller runs past the convergence gate in doStorageStatus, which is
// the arm a controller can be serving, and a read-write connection there could
// checkpoint the WAL of a live binding on close. The gate is also the
// precondition read-only needs — infraConvergenceMarked means the database is
// present — so no extra stat is required here.
func reportBindingRelics(target infraBindingTarget, logPrefix string, stdout, stderr io.Writer) {
	binding, err := openInfraBindingReadOnly(target)
	if err != nil {
		fmt.Fprintf(stderr, "%s: counting open relics: opening the binding: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return
	}
	defer closeBeadStoreHandle(binding) //nolint:errcheck // best-effort close

	relics, err := storeref.OpenLegacyResidents(binding, config.AllReservedClassPrefixes())
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return
	}
	fmt.Fprintf(stdout, "open relics: %d (carried across under their original ids; closed relics stay readable by id, so a binding that ever held one keeps its residence probe)\n", len(relics)) //nolint:errcheck // best-effort stdout
}

// cityWorkStoreRows lists the copy's source — the city work store, opened
// the way the migration opens it — keyed by id, for the rig census.
func cityWorkStoreRows(cityPath string) (map[string]beads.Bead, error) {
	source, err := openInfraMigrationSource(cityPath)
	if err != nil {
		return nil, fmt.Errorf("opening the city work store for the rig census: %w (the census must complete before a cutover, so this is a refusal rather than a skip)", err)
	}
	rows, listErr := source.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	closeErr := closeBeadStoreHandle(source)
	if listErr != nil {
		return nil, fmt.Errorf("listing the city work store for the rig census: %w", listErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("closing the city work store after the rig census: %w", closeErr)
	}
	byID := make(map[string]beads.Bead, len(rows))
	for _, bead := range rows {
		byID[bead.ID] = bead
	}
	return byID, nil
}

// censusRowDiffers reports whether the city work store holds the rig's row in
// another state than the rig scope shows: state the copy would not carry, so a
// cutover would keep the city's version and drop the rig's.
//
// The same state is what the migration's own equality stage means by a
// faithful copy: beadCopyDifference, which witnesses every durable field —
// title, status, type, description, priority, assignee, sender, parent, ref,
// the two tier bits, the timestamps, defer_until, close_reason, labels as a set
// and metadata as a map — under a reflection guard that makes a field added to
// beads.Bead either compared there, and so here, or exempted with its reason.
// The payload is compared rather than trusted to a version stamp because no
// stamp is comparable across the two stores: a revision is each store's own
// token (a native row's is a random row_lock per database), bd lists every
// row without one and keeps updated_at when it adds a label or an edge, and
// the native store never fills UpdatedAt.
//
// Two list rows are not a source row and its copy, so four things differ from
// that stage:
//   - Dependencies are compared as a set of (target, type) edges; the copy's
//     stage reads them through DepList instead. A store whose list rows carry
//     no edges shows both sides empty, so there a divergence in edges alone is
//     not seen. Needs is create-time shorthand for edges, which the copy
//     strips rather than carries, and is not compared.
//   - Revision is not compared, as beadCopyDifference does not compare it:
//     it is a token of the store that minted it, so two stores' revisions for
//     one id are unrelated whether they agree or not, and 0 is either side's
//     real revision of a legacy row, not an absent one.
//   - A timestamp is compared only when both rows carry it: a listing that
//     publishes none is neither a match nor a mismatch.
//   - IsBlocked is not compared: it is bd's readiness projection, not stored
//     state, and the copy strips it.
func censusRowDiffers(city, rig beads.Bead) bool {
	if stringSetDifference("dependency", censusEdges(rig.Dependencies), censusEdges(city.Dependencies)) != "" {
		return true
	}
	rig, city = censusComparable(rig, city)
	return beadCopyDifference(rig, city) != ""
}

// censusComparable clears what censusRowDiffers does not compare from both
// rows: the readiness projection, and each timestamp either row lacks.
func censusComparable(a, b beads.Bead) (beads.Bead, beads.Bead) {
	a.IsBlocked, b.IsBlocked = nil, nil
	if a.CreatedAt.IsZero() || b.CreatedAt.IsZero() {
		a.CreatedAt, b.CreatedAt = time.Time{}, time.Time{}
	}
	if a.UpdatedAt.IsZero() || b.UpdatedAt.IsZero() {
		a.UpdatedAt, b.UpdatedAt = time.Time{}, time.Time{}
	}
	return a, b
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

// cityMigrationGuardDirectory returns the city .gc directory the migration
// guard locks.
func cityMigrationGuardDirectory(cityPath string) string {
	return filepath.Join(cityPath, ".gc")
}

// openStorageScopeStore opens one scope's bead store for the rig-residue
// census. Overridden by tests, which have no real rig workspaces to open.
var openStorageScopeStore = openStoreAtForCity

// censusRigInfraResidue refuses a copy while any rig scope holds a bead this
// migration classifies as infrastructure. A city that already converged copies
// nothing and is not censused (doStorageMigrate).
//
// The copy's source is the CITY work store, and every later boot re-checks
// containment against that same store. A rig scope is neither: an
// infrastructure bead sitting in a rig's own bead workspace is invisible to the
// copy and invisible to the strand detector, so cutting over would route every
// reader past it permanently and nothing would ever say so.
//
// So it is refused by name here, before the guard is doing any good, rather
// than counted or repaired. Repairing it would mean importing rig rows into the
// city work store, and this binary carries no such importer — naming one in the
// remedy would send the operator to a command that does not exist.
//
// A rig whose binding IS the city work store is not residue. A unified city's
// rig .beads can carry a copy of the city's own remote binding, or resolve the
// same database, and its rows are then the copy's source rows: listing it
// lists the city. Which binding a scope resolves to is bd's to know, not gc's
// (see work_topology_binding.go), so the census asks the question it actually
// cares about instead: does the copy's source hold this row in the same state
// (censusRowDiffers)? Such a row is carried by the copy; anything else —
// absent, or the same id in another state — would be stranded and is refused
// by name, with the remedy each needs.
func censusRigInfraResidue(cityPath string, cfg *config.City) error {
	if cfg == nil {
		return nil
	}
	candidates, err := listRigInfraRows(cityPath, cfg.Rigs)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		return nil
	}
	carried, err := cityWorkStoreRows(cityPath)
	if err != nil {
		return err
	}
	var absent, diverged []string
	for _, row := range candidates {
		name := fmt.Sprintf("%s (rig %s)", row.bead.ID, row.rig)
		city, ok := carried[row.bead.ID]
		switch {
		case !ok:
			absent = append(absent, name)
		case censusRowDiffers(city, row.bead):
			diverged = append(diverged, name)
		}
	}
	return rigResidueRefusal(absent, diverged)
}

// rigInfraRow is one infrastructure row a rig scope lists, with the rig that
// listed it.
type rigInfraRow struct {
	bead beads.Bead
	rig  string
}

// listRigInfraRows lists the infrastructure rows every bound rig scope holds.
// A rig that cannot be opened, listed or closed fails the census.
func listRigInfraRows(cityPath string, rigs []config.Rig) ([]rigInfraRow, error) {
	var found []rigInfraRow
	for _, rig := range rigs {
		root := strings.TrimSpace(rig.Path)
		if root == "" {
			// An unbound rig has no workspace of its own; openStoreAtForCity
			// would fall back to the city scope and census it twice.
			continue
		}
		store, err := openStorageScopeStore(root, cityPath)
		if err != nil {
			return nil, fmt.Errorf("censusing rig %q for infrastructure beads: %w (the census must complete before a cutover, so this is a refusal rather than a skip)", rig.Name, err)
		}
		rows, listErr := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
		closeErr := closeBeadStoreHandle(store)
		if listErr != nil {
			return nil, fmt.Errorf("listing rig %q: %w", rig.Name, listErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("closing rig %q after the census: %w", rig.Name, closeErr)
		}
		for _, bead := range rows {
			if coordclass.Classify(bead) == coordclass.ClassWork {
				continue
			}
			found = append(found, rigInfraRow{bead: bead, rig: rig.Name})
		}
	}
	return found, nil
}

// rigResidueRefusal names every rig row a cutover would strand, split by the
// remedy each needs. A row the city work store lacks has to be moved into it.
// A row it holds in another state cannot be moved — the id is taken — so the
// two have to be brought into agreement; and on a running city the census may
// only have read the row on either side of a write, which a stopped city rules
// out.
func rigResidueRefusal(absent, diverged []string) error {
	var reasons []string
	if len(absent) > 0 {
		sort.Strings(absent)
		reasons = append(reasons, fmt.Sprintf("%d infrastructure bead(s) live in rig scopes rather than in this city's work store, and the copy reads only the work store, so a cutover would leave them unreachable: %s. Move them into the city work store before migrating",
			len(absent), strings.Join(absent, ", ")))
	}
	if len(diverged) > 0 {
		sort.Strings(diverged)
		reasons = append(reasons, fmt.Sprintf("%d infrastructure bead(s) in rig scopes are held by this city's work store in another state, and the copy carries only the work store's state, so a cutover would drop the rig's: %s. Bring each rig row and the work store's row into agreement before migrating; if the city is running, a write may have landed between the two reads, so stop it (%s) and check again first",
			len(diverged), strings.Join(diverged, ", "), storageStopCommand))
	}
	if len(reasons) == 0 {
		return nil
	}
	return errors.New(strings.Join(reasons, ". "))
}
