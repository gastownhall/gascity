package main

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// storageRepairSequenceVerb is the operator verb that inspects and raises a
// SQLite bead store's id-sequence floor. Allocation refusals in internal/beads
// name it as "gc storage repair-sequence".
const storageRepairSequenceVerb = "repair-sequence"

// newStorageRepairSequenceCmd builds `gc storage repair-sequence`.
func newStorageRepairSequenceCmd(surface storageCommandSurface, stdout, stderr io.Writer) *cobra.Command {
	var (
		dir     string
		prefix  string
		floor   string
		idFloor bool
	)
	cmd := &cobra.Command{
		Use:          storageRepairSequenceVerb,
		Short:        "Inspect or raise a SQLite bead store's id-sequence floor",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		Long: `Inspect or raise the persisted id-sequence floor of a SQLite bead store.

Without --floor this is read-only: it reports the persisted floor and the
highest auto-minted id the store still holds, and whether minting is refused
because a negative "<prefix>--<n>" id ranks above the floor.

A negative id above the floor usually means an older build wrapped the
sequence past 9223372036854775807. It can also come from a copy or import that
pinned "<prefix>--<n>" ids into a store that never wrapped. If those rows are
stray duplicates, do not raise the floor: delete every "<prefix>--<n>" row
above the floor, not only the one the report names, and reopen the store
(restart the processes serving it), and minting resumes in the positive range.
Deleting destroys those beads, so if any of them is a real bead, raise the
floor instead.

With --floor=N it persists N as the floor, so the next auto id is N+1. Pick N
at or above the highest id EVER issued under the prefix — deleted beads are no
longer in the store, so derive it from the event log and dispatcher traces. The
command refuses to lower the persisted floor and refuses N below the highest
auto id present.

Ids are ordered 1 < ... < 9223372036854775807 < -9223372036854775808 < ... < -1.
On a store an older build wrapped, the positive range is spent: pass a NEGATIVE
floor above every "<prefix>--<n>" id ever issued, and allocation continues
upward toward -1 without re-entering the positive range. That cannot be
undone. Builds without this fix refuse to open a store whose floor is negative.

Before repairing a wrapped store, stop every process serving it that runs a
build without this fix, so none keeps minting wrapped ids past the floor you
pick. After a floor raise, processes on this build need no restart: one that
refuses to mint resumes at its next mint once the floor covers the store.

The store defaults to this city's SQLite infrastructure binding; --dir names
any other store directory (the directory holding beads.sqlite).

With --id-floor it raises the city WORK store's bd id floor instead: it sets
min_hash_length one past the longest hash suffix among the ids the city's
copy manifest records as carried into the binding (those of the work store's
own prefix whose suffix bd could have minted: base36, 3 to 8 characters), and
max_hash_length with it when needed, so bd does not mint a carried id again
after the clear removed it: every id it mints has a longer suffix than any
carried one. Neither is set above 8, the longest suffix bd mints, so where a
carried suffix already has 8 characters a new 8-character id only rarely
could repeat one. It never lowers either setting, with one exception: one
above 8 (an earlier import may have set both higher) is brought down to 8,
since bd mints nothing longer and a hosted bd serve only pads ids to it. A
second run changes nothing. It holds the controller lock and refuses while
the city runs, like every writing storage verb. A file store needs no floor
and a generic exec store cannot take one; both are reported and skipped. On a
hosted bd store that refuses the write it names the value to set out of
band. --id-floor takes no --floor, --dir or --prefix.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			logPrefix := "gc " + surface.Namespace + " " + storageRepairSequenceVerb
			if idFloor {
				for _, name := range []string{"floor", "dir", "prefix"} {
					if cmd.Flags().Changed(name) {
						fmt.Fprintf(stderr, "%s: --%s raises the city work store's bd id floor and takes no --%s\n", logPrefix, storageIDFloorFlag, name) //nolint:errcheck // best-effort stderr
						return errExit
					}
				}
				request, err := resolveStorageOperatorRequest()
				if err != nil {
					fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
					return errExit
				}
				return exitForCode(doStorageRepairIDFloor(cmd.Context(), request, stdout, stderr, logPrefix))
			}
			target := dir
			if target == "" {
				request, err := resolveStorageOperatorRequest()
				if err != nil {
					fmt.Fprintf(stderr, "%s: %v (or pass --dir)\n", logPrefix, err) //nolint:errcheck // best-effort stderr
					return errExit
				}
				resolved, ok, err := resolveInfraBindingTarget(request.CityPath, request.Cfg)
				if err != nil {
					fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
					return errExit
				}
				if !ok {
					fmt.Fprintf(stderr, "%s: this city has no SQLite infrastructure binding; pass --dir <store directory>\n", logPrefix) //nolint:errcheck // best-effort stderr
					return errExit
				}
				target = resolved.Dir
			}
			return exitForCode(doStorageRepairSequence(target, prefix, floor, stdout, stderr, logPrefix))
		},
	}
	defaultPrefix, _ := config.ReservedClassPrefix(config.BeadClassGraph) // residency:allow — the default mint prefix of the store being repaired; resolves no store
	cmd.Flags().StringVar(&dir, "dir", "", "store directory holding beads.sqlite (default: this city's SQLite infrastructure binding)")
	cmd.Flags().StringVar(&prefix, "prefix", defaultPrefix, "auto-id prefix whose sequence to inspect or repair")
	cmd.Flags().StringVar(&floor, "floor", "", "new floor (int64, may be negative); omit to only report")
	cmd.Flags().BoolVar(&idFloor, storageIDFloorFlag, false, "raise the city work store's bd min_hash_length past every id the copy carried into the binding (not with --floor, --dir or --prefix)")
	return cmd
}

// storageIDFloorFlag names repair-sequence's city id-floor mode.
const storageIDFloorFlag = "id-floor"

// doStorageRepairIDFloor raises the city work store's id floor past every id
// the city's copy manifest records as carried (storage_id_floor.go), under
// the controller lock and the migration guard. It returns the exit code.
func doStorageRepairIDFloor(ctx context.Context, request storageOperatorRequest, stdout, stderr io.Writer, logPrefix string) (code int) {
	target, ok, err := resolveInfraBindingTarget(request.CityPath, request.Cfg)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !ok {
		fmt.Fprintf(stderr, "%s: this city has no SQLite infrastructure binding, so no id was carried out of its work store and none needs a floor\n", logPrefix) //nolint:errcheck // best-effort stderr
		return 1
	}
	lock, err := acquireStorageVerbExclusion(request, storageVerbScope{})
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	defer lock.Close() //nolint:errcheck // released last, after the guard
	guard, err := acquireStorageMigrationGuard(ctx, request.CityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	defer guard.Release() //nolint:errcheck // best-effort release
	manifest, recorded, err := readInfraCopyManifestRecord(target)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !recorded {
		fmt.Fprintf(stderr, "%s: %s records no copy manifest, so nothing says which ids left the work store; run `%s` first\n", logPrefix, target.ManifestPath(), storageMigrationCommand) //nolint:errcheck // best-effort stderr
		return 1
	}
	window, err := openMigrationSource(lock, request.CityPath, request.Cfg, logPrefix, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	defer window.closeReporting(logPrefix, stderr, &code)
	result, err := reserveCityIDs(request.CityPath, sortedMapKeys(manifest.IDs))
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintln(stdout, result.String()) //nolint:errcheck // best-effort stdout
	return 0
}

// doStorageRepairSequence reports, and with a floor raises, one store's
// sequence floor. It returns the process exit code.
func doStorageRepairSequence(dir, prefix, floorText string, stdout, stderr io.Writer, logPrefix string) int {
	if floorText == "" {
		state, err := beads.InspectSQLiteSequence(dir, prefix)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		writeSQLiteSequenceState(stdout, state)
		return 0
	}
	floor, err := strconv.ParseInt(floorText, 10, 64)
	if err != nil || strconv.FormatInt(floor, 10) != floorText {
		fmt.Fprintf(stderr, "%s: --floor %q is not a canonical int64\n", logPrefix, floorText) //nolint:errcheck // best-effort stderr
		return 1
	}
	before, err := beads.RaiseSQLiteSequenceFloor(dir, prefix, floor)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	after, err := beads.InspectSQLiteSequence(dir, prefix)
	if err != nil {
		fmt.Fprintf(stderr, "%s: floor raised, but re-inspecting failed: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "floor %d -> %d (%s)\n", before.Floor, after.Floor, after.FloorPath) //nolint:errcheck // best-effort stdout
	writeSQLiteSequenceState(stdout, after)
	return 0
}

func writeSQLiteSequenceState(w io.Writer, state beads.SQLiteSequenceState) {
	fmt.Fprintf(w, "store:            %s\n", state.Dir)             //nolint:errcheck // best-effort stdout
	fmt.Fprintf(w, "prefix:           %s\n", state.Prefix)          //nolint:errcheck // best-effort stdout
	fmt.Fprintf(w, "persisted floor:  %d\n", state.Floor)           //nolint:errcheck // best-effort stdout
	fmt.Fprintf(w, "highest positive: %d\n", state.HighestPositive) //nolint:errcheck // best-effort stdout
	if state.HasNegative {
		fmt.Fprintf(w, "highest wrapped:  %d\n", state.HighestNegative) //nolint:errcheck // best-effort stdout
	} else {
		fmt.Fprintln(w, "highest wrapped:  none") //nolint:errcheck // best-effort stdout
	}
	if state.Wrapped {
		fmt.Fprintf(w, "status:           WRAPPED - minting refused: %[1]s-%[2]d ranks above the persisted floor\n"+ //nolint:errcheck // best-effort stdout
			"remedy:           if a copy or import pinned %[1]s--<n> rows into a store that never wrapped and they are stray duplicates, "+
			"delete every %[1]s--<n> row above the persisted floor, not only %[1]s-%[2]d, and reopen the store; "+
			"otherwise (they are real beads, or an older build wrapped this store) raise --floor to at or above every %[1]s--<n> id ever issued (irreversible)\n",
			state.Prefix, state.HighestNegative)
	} else {
		fmt.Fprintln(w, "status:           ok") //nolint:errcheck // best-effort stdout
	}
}
