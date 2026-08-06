package main

// `gc storage` — the operator surface for a city's storage-class layout.
//
// The whole tree is built from storageMigrationCommand, the one constant this
// program spells the operator command in. The boot refusal prints that same
// constant (infra_class_migrate.go), so building the command from it is what
// stops the refusal and the command from drifting apart in the one direction
// that matters: a refusal naming a command the binary does not carry is an
// operator instruction that fails at the shell.
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

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storebinding"
)

const (
	// storageMigrationCommand is the one spelling of the command that performs
	// the move. The boot refusal prints it and this file parses it into the
	// cobra tree, so the two cannot disagree.
	storageMigrationCommand = "gc storage migrate --from-work"

	// storageStatusVerb is the read-only sibling of the migrate verb.
	storageStatusVerb = "status"

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

// newStorageCmd constructs the `gc storage` tree named by the operator
// spelling.
func newStorageCmd(stdout, stderr io.Writer) *cobra.Command {
	return newStorageCmdFromSpelling(storageMigrationCommand, stdout, stderr)
}

// newStorageCmdFromSpelling is the testable body of newStorageCmd.
func newStorageCmdFromSpelling(spelling string, stdout, stderr io.Writer) *cobra.Command {
	surface, err := parseOperatorCommandSpelling(spelling)
	if err != nil {
		return newUnbuildableStorageCmd(err, stderr)
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
		newStorageStatusCmd(surface, stdout, stderr),
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
		fleetStopped bool
	)
	cmd := &cobra.Command{
		Use:          surface.Verb,
		Short:        "Migrate this city's infrastructure classes onto their configured binding",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		Long: `Copy this city's infrastructure-class beads out of the work store and into
the binding [storage.classes] assigns them to.

Every bead is copied with its id and its within-class dependency topology
preserved, proven field-equal against a closed and reopened destination, and
then recorded in a proven-copy manifest and a convergence marker. The source is
RETAINED verbatim: nothing here writes to, moves or prunes the work store, so a
rollback before cutover is a config edit with no data recovery step.

The move refuses while a writer can reach the source. This binary can prove the
absence of a controller and cannot prove the absence of anything else, so that
half is an explicit operator attestation.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !fromWork {
				fmt.Fprintf(stderr, "gc %s %s: pass --%s. This build carries one source — the city's work store — and states it explicitly rather than detecting it, so a source added later cannot inherit this one's behavior by silence\n", //nolint:errcheck // best-effort stderr
					surface.Namespace, surface.Verb, surface.Flag)
				return errExit
			}
			request, err := resolveStorageOperatorRequest()
			if err != nil {
				fmt.Fprintf(stderr, "gc %s %s: %v\n", surface.Namespace, surface.Verb, err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			request.FleetStopped = fleetStopped
			return exitForCode(doStorageMigrate(cmd.Context(), request, stdout, stderr))
		},
	}
	cmd.Flags().BoolVar(&fromWork, surface.Flag, false,
		"migrate the infrastructure classes out of this city's work store")
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
converged on, so a deployment script can gate on it.`,
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
// nothing; then the guard, because two migrators are worse than one; then the
// rig census, because a bead this city's own scopes hold outside the work store
// is one the copy will never see; and only then the copy itself.
func doStorageMigrate(ctx context.Context, request storageOperatorRequest, stdout, stderr io.Writer) int {
	const logPrefix = "gc storage migrate"

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
	if _, err := resolveCityStoragePlan(request.CityPath, request.Cfg); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if pid := infraMigrationForeignControllerPID(request.CityPath); pid != 0 {
		fmt.Fprintf(stderr, "%s: controller PID %d is live on this city and is still writing to the work store. Stop it (gc stop) and run this again\n", logPrefix, pid) //nolint:errcheck // best-effort stderr
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

	if err := censusRigInfraResidue(request.CityPath, request.Cfg); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}

	report := runInfraClassMigration(request.CityPath, target, logPrefix, stderr)
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
	fmt.Fprintf(stdout, "Migrated. %s now serves %s from %s; the work store keeps its rows. Start the city with `gc start`.\n", //nolint:errcheck // best-effort stdout
		target.Binding, infraMigrationClassList(), target.Database)
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

	target, ok, err := resolveInfraBindingTarget(request.CityPath, request.Cfg)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !ok {
		fmt.Fprintln(stdout, "binding: none — every class is served by the work store, and nothing migrates.") //nolint:errcheck // best-effort stdout
		return 0
	}
	fmt.Fprintf(stdout, "binding: %s\n  database: %s\n  marker:   %s\n  manifest: %s\n", //nolint:errcheck // best-effort stdout
		target.Binding, target.Database, target.MarkerPath(), target.ManifestPath())

	state, err := readInfraConvergenceState(target)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
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
	fmt.Fprintf(stdout, "source: %d infrastructure bead(s) retained in the work store\n", len(rows)) //nolint:errcheck // best-effort stdout

	if state != infraConvergenceMarked {
		fmt.Fprintf(stdout, "converged: no\nblocking invariant: boot never migrates; run `%s`\n", storageMigrationCommand) //nolint:errcheck // best-effort stdout
		return 1
	}

	proven, recorded, err := readInfraCopyManifest(target)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !recorded {
		fmt.Fprintln(stdout, "converged: yes (no proven-copy manifest, so stranded-write detection is off for this city)") //nolint:errcheck // best-effort stdout
		return 0
	}
	gap, err := classifyInfraContainmentGap(request.CityPath, target, proven)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "converged: yes\n  proven copy: %d bead(s)\n  stranded:    %d\n  removed since cutover: %d\n", //nolint:errcheck // best-effort stdout
		len(proven), len(gap.Stranded), gap.RemovedSinceCutover)
	if len(gap.Stranded) > 0 {
		fmt.Fprintf(stdout, "  stranded ids: %s\n", strings.Join(gap.Stranded, ", ")) //nolint:errcheck // best-effort stdout
		return 1
	}
	return 0
}

// cityMigrationGuardDirectory returns the city .gc directory the migration
// guard locks.
func cityMigrationGuardDirectory(cityPath string) string {
	return filepath.Join(cityPath, ".gc")
}

// openStorageScopeStore opens one scope's bead store for the rig-residue
// census. Overridden by tests, which have no real rig workspaces to open.
var openStorageScopeStore = openStoreAtForCity

// censusRigInfraResidue refuses a migration while any rig scope holds a bead
// this migration classifies as infrastructure.
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
func censusRigInfraResidue(cityPath string, cfg *config.City) error {
	if cfg == nil {
		return nil
	}
	var found []string
	for _, rig := range cfg.Rigs {
		root := strings.TrimSpace(rig.Path)
		if root == "" {
			// An unbound rig has no workspace of its own; openStoreAtForCity
			// would fall back to the city scope and census it twice.
			continue
		}
		store, err := openStorageScopeStore(root, cityPath)
		if err != nil {
			return fmt.Errorf("censusing rig %q for infrastructure beads: %w (the census must complete before a cutover, so this is a refusal rather than a skip)", rig.Name, err)
		}
		rows, listErr := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
		closeErr := closeBeadStoreHandle(store)
		if listErr != nil {
			return fmt.Errorf("listing rig %q: %w", rig.Name, listErr)
		}
		if closeErr != nil {
			return fmt.Errorf("closing rig %q after the census: %w", rig.Name, closeErr)
		}
		for _, bead := range rows {
			if coordclass.Classify(bead) == coordclass.ClassWork {
				continue
			}
			found = append(found, fmt.Sprintf("%s (rig %s)", bead.ID, rig.Name))
		}
	}
	if len(found) == 0 {
		return nil
	}
	sort.Strings(found)
	return fmt.Errorf("%d infrastructure bead(s) live in rig scopes rather than in this city's work store, and the copy reads only the work store, so a cutover would leave them unreachable: %s. Move them into the city work store before migrating",
		len(found), strings.Join(found, ", "))
}
