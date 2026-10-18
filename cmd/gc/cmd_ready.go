package main

import (
	"fmt"
	"io"

	"github.com/gastownhall/gascity/internal/readywork"
	"github.com/spf13/cobra"
)

// newReadyCmd builds `gc ready`: the in-process, city-wide claimable-work reader
// that federates the stores a split city spreads its work across.
//
// The reader itself is internal/readywork; this file is its CLI projection.
// On a city that relocates nothing it reads the stores it always read.
//
// The output is a bare JSON array in bd's row shape, because it is a drop-in for
// the `bd ready --json` the default work_query builds; see readywork.Row.
func newReadyCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts readywork.Query
	var jsonOut bool
	var includeEphemeral bool
	cmd := &cobra.Command{
		Use:   "ready",
		Short: "List ready (claimable) work across every store in the city",
		Long: `List ready, claimable work as a JSON array, federating the city store, the
rig stores, and — on a split city — the relocated graph store where molecule
roots, step beads and control beads live.

It is the in-process, city-wide drop-in for the single-store "bd ready" work
query, so workers and the control plane see graph-class work instead of an
authoritative-looking short answer. On a city that relocates no coordination
class it reads the one store, unchanged.

The flags mirror the "bd ready" contract the default work_query builds:
  gc ready --metadata-field "gc.routed_to=$target" --unassigned \
           --exclude-type=epic --exclude-label "hold:mayor" \
           --exclude-label "hold:external" --exclude-label "human" \
           --json --limit=20

Rows are emitted in canonical ready order (priority, created_at, id) unless
--sort selects a created_at order, and --limit is applied last, so a bounded
read is the true top-N of the merged set rather than the top-N of whichever
store answered first.

Every leg is read across both storage tiers, so the wisp/ephemeral rows an
orchestration step runs as are claimable work here whether or not
--include-ephemeral is passed.

Suspended rigs are not read: rigs suspended with "gc rig suspend", or declared
suspended_on_start and not resumed. Their agents claim nothing and the
controller counts no demand in them, so this reads the same rigs the controller
does; their work comes back when the rig is resumed. The city store and a
relocated graph store are always read.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdReady(opts, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	registerReadyFlags(cmd, &opts, &includeEphemeral, &jsonOut)
	return cmd
}

// registerReadyFlags binds the `gc ready` flag surface onto cmd.
//
// It is a function rather than an inline block so the SAME flag surface can be
// parsed outside the command — specifically by the reader-agreement conformance
// test, which takes the routed-demand tier straight out of the generated work
// query, parses it here, and runs the real filterReadyBeads over a row corpus.
// A test that re-declared these flags would be testing its own copy of the
// contract, which is the drift this whole lane exists to remove.
func registerReadyFlags(cmd *cobra.Command, opts *readywork.Query, includeEphemeral, jsonOut *bool) {
	cmd.Flags().StringVar(&opts.Assignee, "assignee", "", "only work assigned to exactly this identity")
	cmd.Flags().BoolVar(&opts.Unassigned, "unassigned", false, "only unassigned work")
	cmd.Flags().StringArrayVar(&opts.MetadataFields, "metadata-field", nil, "require metadata \"key=value\", or bare \"key\" for any non-empty value (repeatable)")
	cmd.Flags().StringArrayVar(&opts.ExcludeTypes, "exclude-type", nil, "drop beads of this issue type (repeatable)")
	cmd.Flags().StringArrayVar(&opts.ExcludeLabels, "exclude-label", nil, "drop beads carrying this label (repeatable)")
	cmd.Flags().StringVar(&opts.Sort, "sort", "", "sort order: oldest|newest (default: canonical ready order)")
	cmd.Flags().IntVar(&opts.Limit, "limit", 0, "max beads to return (0 = unlimited)")
	// --include-ephemeral is accepted for parity with `bd ready`, which the
	// generated work_query carries verbatim on a bd-1.0.5 city. It selects
	// nothing, and that is the honest spelling rather than a silent one: every
	// leg is read at beads.FederatedReadTier, which already spans the wisp tier.
	// Binding it to a discarded variable is deliberate — a readywork.Query field
	// nothing consults is how a flag ends up looking honored while one leg
	// quietly narrows (ga-8lyxc).
	cmd.Flags().BoolVar(includeEphemeral, "include-ephemeral", false, "accept --include-ephemeral for bd-ready parity (every leg already spans the wisp tier)")
	cmd.Flags().StringVar(&opts.Status, "status", "", "list beads in this status instead of ready work: open|in_progress|blocked|closed")
	// --json is accepted for parity with `bd ready --json`, which the generated
	// work_query carries verbatim. The payload is a JSON array either way; the
	// flag exists so the query does not have to be rewritten to drop it.
	cmd.Flags().BoolVar(jsonOut, "json", true, "accept --json for bd-ready parity (output is always a JSON array)")
}

func cmdReady(opts readywork.Query, stdout, stderr io.Writer) int {
	// Every flag is validated before anything is resolved, loaded or opened: a
	// malformed query must not cost a city-wide federated read to be told it was
	// malformed.
	if err := opts.Validate(); err != nil {
		fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	// One composed city-config load for the whole invocation. Every store open
	// below resolves its own inputs — the pinned bd, the storage binding env —
	// and each of those composed the city config again, once per rig: 22 loads
	// for one `gc ready` on a six-rig bd-backed city. They are all answering for
	// the config this command is about to load, so the memo serves them from it
	// (see invocationConfigMemo for what callers may and may not touch).
	defer beginInvocationConfigMemo()()
	// The reader is city-wide: no rig context selects anything, so the
	// resolution skips the full config load a rig name costs.
	cityPath, err := resolveCityPathOnly()
	if err != nil {
		fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cityStore, err := openCityStoreAtWithConfig(cityPath, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	rigStores, err := readyRigLegStores(cfg, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	legs, err := readyFederationLegs(cityPath, loadedCityName(cfg, cityPath), cfg, cityStore, rigStores)
	if err != nil {
		// A refused city: the plan carries the refusal that names the remedy, and
		// a work-only answer here is a short array indistinguishable from "no
		// work" for every work query this reader backs.
		fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	rows, err := readywork.Read(legs, opts)
	if err != nil {
		fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := writeJSON(stdout, rows); err != nil {
		fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}
