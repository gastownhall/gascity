package main

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/readywork"
	"github.com/spf13/cobra"
)

// newReadyCmd builds `gc ready`: the in-process, city-wide claimable-work reader
// that federates the stores a split city spreads its work across.
//
// The reader itself is internal/readywork, shared with the supervisor's
// GET /v0/city/{cityName}/work/ready; this file is its CLI projection. On a city
// that relocates nothing it reads the stores it always read.
//
// The output is a bare JSON array in bd's row shape, because it is a drop-in for
// the `bd ready --json` the default work_query builds; see readywork.ReadyBead.
func newReadyCmd(stdout, stderr io.Writer) *cobra.Command {
	var flags readyFlags
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
class there is no graph store to add: it reads the city store and the rig
stores.

The flags mirror the "bd ready" contract the default work_query builds:
  gc ready --metadata-field "gc.routed_to=$target" --unassigned \
           --exclude-type=epic --exclude-label "hold:mayor" \
           --exclude-label "hold:external" --exclude-label "human" \
           --json --limit=20

Rows are emitted in canonical ready order (priority, created_at, id) unless
--sort selects a created_at order, and --limit is applied last, so a bounded
read is the true top-N of the merged set rather than the top-N of whichever
store answered first.

The rows of --status in_progress carry their blockers (blocked_by), and a row
whose blockers cannot be resolved fails the read. --until-unblocked ends that
answer at its first unblocked row, the one a crash-recovery read resumes, and
resolves no row past it, so a later row's unresolvable blocker does not fail
the read.

Every leg is read across both storage tiers, so the wisp/ephemeral rows an
orchestration step runs as are claimable work here whether or not
--include-ephemeral is passed.

A row being ready does not mean a worker could be handed it. Synthetic input
convoys (one minted per dispatch), molecule step beads, deliberately parked
beads (deploy-handoff, hold:*, owner-action, human-decision, human), already
routed and already-assigned beads are all ready by the store's definition and
none of them is supply. --dispatchable keeps only the rows that are, and
--census prints the count instead of the rows along with the per-class breakdown
that complements it. Neither changes what a plain "gc ready" returns: the raw
list stays complete for the callers that legitimately want convoy rows.

Read the queue-health floor off --census's "dispatchable" number, not off the
row count: a city's raw ready count grows with throughput because carriers are
minted as work is dispatched, so the raw number reads healthy exactly when the
pool is being consumed.

Suspended rigs are not read: rigs suspended with "gc rig suspend", or declared
suspended_on_start and not resumed. Their agents claim nothing and the
controller counts no demand in them, so this reads the same rigs the controller
does; their work comes back when the rig is resumed. The city store and a
relocated graph store are always read.

When the city's controller is running, it answers the query from the stores it
already holds open (GET /v0/city/{cityName}/work/ready), with the reader this
command runs, reading every store live, never from a cache. It reads the rigs
of the config the controller has loaded: a rig added, removed or rebound in the
city's config takes effect in its answer at the controller's next config
reload. It refuses while a config reload or an API config write is in
progress, and after a failed reload until one succeeds; a [storage] change
fails every reload until it is reverted or the city restarts. If the
controller is not running, refuses, or does not answer within 10s, this command
loads the config and reads the stores itself. GC_NO_API=1 forces the local
read.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdReady(flags, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	registerReadyFlags(cmd, &flags, &includeEphemeral, &jsonOut)
	return cmd
}

// readyFlags is the `gc ready` flag surface: the reader's query, which is also
// the query GET /v0/city/{cityName}/work/ready takes, and untilUnblocked, the
// cut --until-unblocked makes of an in_progress answer
// (readywork.ReadUntilUnblocked). The cut is not part of the query because the
// endpoint does not take it: on that route the command cuts the controller's
// whole answer itself.
//
// dispatchable and census are the DISPATCHABILITY surface (dispatchable.go in
// internal/readywork): they are applied to the rows after the read, are not
// part of the query, and therefore need no wire change. That is deliberate —
// the classification is a pure function of a row, so the API and the CLI
// cannot disagree about it.
//
// Neither flag name is bd's, and that is a constraint rather than an accident.
// `bd ready` already spends --explain on "show dependency-aware reasoning" and
// --claim on taking the first row; reusing either spelling here would answer a
// bd-shaped work query with a census object instead of a row array, which every
// consumer that decodes the array would report as an empty queue. The names are
// new precisely so a copied bd invocation cannot mean something else here.
type readyFlags struct {
	query          readywork.Query
	untilUnblocked bool
	// dispatchable drops every row no worker could be handed now (synthetic
	// convoy carriers, molecule step beads, parked labels, already-routed and
	// already-assigned beads), leaving supply only.
	dispatchable bool
	// census replaces the row array with the census: the dispatchable count
	// and the per-class breakdown that complements it.
	census bool
}

// validate checks every flag without reading anything.
func (f readyFlags) validate() error {
	if f.census || f.dispatchable {
		// The dispatchability surface measures READY work. --status lists beads
		// in another status, and most of those (closed, blocked, already
		// in_progress) are not claimable by definition, so classifying them as
		// "dispatchable" would be a well-formed answer to a question nobody
		// asked and a wrong one to the question they did.
		if status := strings.TrimSpace(f.query.Status); status != "" && status != readywork.StatusOpen {
			return fmt.Errorf("--dispatchable and --census measure ready work and do not take --status %s", f.query.Status)
		}
	}
	if f.census {
		// Both answers are about the same population, and they are different
		// payloads: --dispatchable emits the rows, --census emits the count.
		// Accepting both would mean silently choosing one, which is the
		// accepted-looking-flag failure this package keeps naming (ga-8lyxc).
		if f.dispatchable {
			return fmt.Errorf("--census and --dispatchable are two answers to one question; pass one (--census counts, --dispatchable lists)")
		}
		// --limit bounds a ROW list, and --census returns no rows. Silently
		// ignoring it would produce a document that looks limited and is not.
		if f.query.Limit != 0 {
			return fmt.Errorf("--census reports a count and takes no --limit (got %d); use --dispatchable --limit to list rows", f.query.Limit)
		}
	}
	if f.untilUnblocked {
		return readywork.ValidateUntilUnblocked(f.query)
	}
	return f.query.Validate()
}

// read answers an explicit query over legs. The query can differ from the
// flags' own when the dispatchability surface is in play: it reads unbounded and
// cuts afterwards, while the flags keep the caller's limit for that cut.
func (f readyFlags) read(legs []readywork.Leg, query readywork.Query) ([]readywork.ReadyBead, error) {
	if f.untilUnblocked {
		return readywork.ReadUntilUnblocked(context.Background(), legs, query)
	}
	return readywork.Read(legs, query)
}

// postProcess applies the dispatchability surface to rows the reader returned.
// It returns either the filtered rows (the --dispatchable arm) or, for
// --census, no rows and the census document to print instead.
//
// The limit is the subtle part. The reader applies --limit LAST, but
// --dispatchable drops rows AFTER the read, so a bounded read followed by a
// filter can answer with fewer rows than the caller asked for even though more
// dispatchable rows exist further down. cmdReady therefore reads UNBOUNDED when
// --dispatchable or --census is set and cuts here, after the filter — the same
// "the limit is the top N of the answer" rule, applied to the answer this
// surface actually has. The cost is one whole-city read, which is what an
// unbounded `gc ready` already costs and what an honest census needs anyway.
func (f readyFlags) postProcess(rows []readywork.ReadyBead) (kept []readywork.ReadyBead, census *readyCensusDoc) {
	if !f.dispatchable && !f.census {
		return rows, nil
	}
	summary := readywork.Summarize(rows)
	if f.census {
		return nil, &readyCensusDoc{
			Dispatchable: summary.Dispatchable,
			Total:        summary.Total,
			InFlight:     summary.InFlight(),
			ByClass:      summary.ByClass,
		}
	}
	kept = summary.Rows
	if f.query.Limit > 0 && len(kept) > f.query.Limit {
		kept = kept[:f.query.Limit]
	}
	return kept, nil
}

// readyCensusDoc is the --census payload: the dispatchable count, the total
// it was drawn from, how many of the rest are work in flight, and the full
// per-class breakdown — every class present, zero included, so a consumer
// reading the JSON never has to tell "absent" from "zero".
//
// in_flight is in the document rather than left to the caller as (total -
// dispatchable - synthetic - parked) because that subtraction would count
// parked beads as in-flight, and the charter's scheduling order escalates on
// exactly that distinction: busy (work in flight) is healthy, starved (nothing
// in flight) is not.
type readyCensusDoc struct {
	Dispatchable int                             `json:"dispatchable"`
	Total        int                             `json:"total"`
	InFlight     int                             `json:"in_flight"`
	ByClass      map[readywork.DispatchClass]int `json:"by_class"`
}

// registerReadyFlags binds the `gc ready` flag surface onto cmd.
//
// It is a function rather than an inline block so the SAME flag surface can be
// parsed outside the command — specifically by the reader-agreement conformance
// test, which takes the routed-demand tier straight out of the generated work
// query, parses it here, and runs the real filterReadyBeads over a row corpus.
// A test that re-declared these flags would be testing its own copy of the
// contract, which is the drift this whole lane exists to remove.
func registerReadyFlags(cmd *cobra.Command, flags *readyFlags, includeEphemeral, jsonOut *bool) {
	opts := &flags.query
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
	cmd.Flags().BoolVar(&flags.untilUnblocked, "until-unblocked", false, "with --status in_progress, end the answer at its first unblocked row, resolving no row past it")
	cmd.Flags().BoolVar(&flags.dispatchable, "dispatchable", false, "keep only rows a worker could be handed now: drop synthetic convoy carriers, molecule step beads, parked labels, already-routed and already-assigned beads")
	cmd.Flags().BoolVar(&flags.census, "census", false, "print the dispatchable census instead of the rows: the count the queue-health floor is measured against, with the per-class breakdown that complements it")
	// --json is accepted for parity with `bd ready --json`, which the generated
	// work_query carries verbatim. The payload is a JSON array either way; the
	// flag exists so the query does not have to be rewritten to drop it.
	cmd.Flags().BoolVar(jsonOut, "json", true, "accept --json for bd-ready parity (output is always a JSON array)")
}

func cmdReady(flags readyFlags, stdout, stderr io.Writer) int {
	// Every flag is validated before anything is resolved, loaded or opened: a
	// malformed query must not cost a city-wide federated read to be told it was
	// malformed.
	if err := flags.validate(); err != nil {
		fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	// --dispatchable and --census filter AFTER the read, so a limit pushed into
	// the read would cut the wrong population: the top N ready rows are not the
	// top N dispatchable ones. Read unbounded and let postProcess cut with the
	// caller's own limit, which is also what an honest census needs.
	//
	// The zeroing is on a COPY. Clearing flags.query.Limit instead would erase
	// the number postProcess cuts with, and the answer would silently be
	// unbounded — an over-delivery, not the requested top N.
	query := flags.query
	if flags.dispatchable || flags.census {
		query.Limit = 0
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
	if rows, ok := readyThroughController(cityPath, query, stderr); ok {
		if flags.untilUnblocked {
			// The controller answers the whole window with every row resolved, and
			// its cut is the in-process read's answer. A row past the cut that cannot
			// be resolved fails the controller's read instead, and the in-process
			// read below answers.
			rows = readywork.CutUntilUnblocked(rows)
		}
		return writeReadyAnswer(flags, rows, stdout, stderr)
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
	rows, err := flags.read(legs, query)
	if err != nil {
		fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return writeReadyAnswer(flags, rows, stdout, stderr)
}

// writeReadyAnswer applies the dispatchability surface to rows and prints the
// answer: the census document for --census, the (possibly filtered) row array
// otherwise.
func writeReadyAnswer(flags readyFlags, rows []readywork.ReadyBead, stdout, stderr io.Writer) int {
	rows, census := flags.postProcess(rows)
	var err error
	if census != nil {
		err = writeJSON(stdout, census)
	} else {
		err = writeJSON(stdout, rows)
	}
	if err != nil {
		fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}

// readyAPITimeout bounds how long `gc ready` waits for the controller before it
// reads in-process instead. The controller answers a claim tier in tens to
// hundreds of milliseconds and a whole unbounded ready set (eleven thousand rows
// on a large city) in a few seconds; past this, the request is hung or the
// controller is overloaded, and the in-process read is the faster route. A
// variable so tests can stand up a hung controller without waiting this long.
var readyAPITimeout = 10 * time.Second

// readyAPIClient resolves the controller `gc ready` asks, or nil with the reason
// it reads in-process instead. supervisorFallthroughAPIClient is the routing for
// a read whose local path is expensive: a supervisor-managed city answers on the
// supervisor's port even though its per-city controller socket is alive, where
// apiClient would send it to the local path. A variable so tests can stand a
// controller up, or take it away.
var readyAPIClient = supervisorFallthroughAPIClient

// readyThroughController asks the controller holding this city's stores open
// (GET /v0/city/{cityName}/work/ready) for the answer. That answer is the
// in-process reader's over the config the controller has published and the
// stores it holds: the same rows in the same order with the same fields, read
// live (see controllerState.ReadyWork). The process then opens no store at all.
// It is not a fresh read of the config on disk: a rig added, removed or rebound
// there takes effect in the controller's answer at its next config reload, and
// in the in-process read at once.
//
// It answers false on ANY failure, and the caller reads in-process exactly as it
// always has: no controller, a controller from before the endpoint existed, a
// refused request (a reload or API config write in progress, or a failed
// reload), a store the controller could not read, a transport error or a
// timeout. That read is the one `gc ready` made before the endpoint existed, so
// falling back to it is always safe. GC_NO_API forces it.
func readyThroughController(cityPath string, q readywork.Query, stderr io.Writer) ([]readywork.ReadyBead, bool) {
	client, reason := readyAPIClient(cityPath)
	if client == nil {
		logRoute(stderr, "ready", "fallback", reason)
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), readyAPITimeout)
	defer cancel()
	rows, err := client.ReadyWork(ctx, q)
	if err != nil {
		logRoute(stderr, "ready", "fallback", "api-error", "error", strconv.Quote(err.Error()))
		return nil, false
	}
	logRoute(stderr, "ready", "api", "")
	return rows, true
}
