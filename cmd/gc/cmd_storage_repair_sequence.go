package main

import (
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
		dir    string
		prefix string
		floor  string
	)
	cmd := &cobra.Command{
		Use:          storageRepairSequenceVerb,
		Short:        "Inspect or raise a SQLite bead store's id-sequence floor",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		Long: `Inspect or raise the persisted id-sequence floor of a SQLite bead store.

Without --floor this is read-only: it reports the persisted floor and the
highest auto-minted id the store still holds, and whether minting is refused
because an older build wrapped the sequence past 9223372036854775807.

With --floor=N it persists N as the floor, so the next auto id is N+1. Pick N
at or above the highest id EVER issued under the prefix — deleted beads are no
longer in the store, so derive it from the event log and dispatcher traces. The
command refuses to lower the persisted floor and refuses N below the highest
auto id present.

Ids are ordered 1 < ... < 9223372036854775807 < -9223372036854775808 < ... < -1.
On a store an older build wrapped, the positive range is spent: pass a NEGATIVE
floor above every "<prefix>--<n>" id ever issued, and allocation continues
upward toward -1 without re-entering the positive range. Builds without this
fix refuse to open a store whose floor is negative.

Stop every process serving the store first when repairing a wrapped store. The
store defaults to this city's SQLite infrastructure binding; --dir names any
other store directory (the directory holding beads.sqlite).`,
		RunE: func(*cobra.Command, []string) error {
			logPrefix := "gc " + surface.Namespace + " " + storageRepairSequenceVerb
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
	return cmd
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
		fmt.Fprintf(w, "status:           WRAPPED - minting refused until the floor is at or above every %s--<n> id ever issued\n", state.Prefix) //nolint:errcheck // best-effort stdout
	} else {
		fmt.Fprintln(w, "status:           ok") //nolint:errcheck // best-effort stdout
	}
}
