package main

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/spf13/cobra"
)

// readySortOldest and readySortNewest are the two orders `bd ready --sort`
// accepts. Anything else is a typo, and a typo that silently fell back to a
// different order is how a bounded query ends up serving the wrong prefix.
const (
	readySortOldest = "oldest"
	readySortNewest = "newest"
)

// readyStatusInProgress is the one status whose list must read LIVE: it is the
// crash-recovery tier, where a step assigned to a worker that just died must not
// be answered from a cache that predates the claim.
const readyStatusInProgress = "in_progress"

// readyKnownStatuses is the closed set --status accepts. It is closed on
// purpose: an unrecognized status matches no bead in any store, so accepting one
// would answer a typo with an empty array — the fail-open this whole command
// exists to close, reappearing as a flag bug.
var readyKnownStatuses = []string{"open", readyStatusInProgress, "blocked", "closed"}

// readyBead is the external wire shape of one row of `gc ready` output.
//
// It is a DEDICATED type, not beads.Bead, and that is the point: beads.Bead is
// the domain model, its fields change for internal reasons, and marshaling it
// directly would publish every one of those changes as a silent break in a
// contract other programs parse. Same reason cmd_bd_store_bridge.go carries
// bdStoreBridgeBead.
//
// # The field names are bd's, not gc's
//
// The names are chosen deliberately, because gc's own JSON surfaces do NOT agree
// with each other: `gc bd dep tree --json` emits `parent_id`, `depth` and
// `truncated`, while the HTTP bead schema emits `parent` and `issue_type`. This
// command is a drop-in for `bd ready --json`, whose output the hook work-query
// path decodes straight into []beads.Bead, so the wire tags here are the ones
// that decode round-trips: `issue_type` for the type and `parent` for the
// parent. Copying bdStoreBridgeBead's `parent_id` verbatim would have produced a
// row that decodes with an empty parent on every consumer gc already has.
//
// The field SET is the same one the HTTP Bead schema publishes, so the two
// surfaces stay comparable. `parent` looks absent in a live capture only because
// it is omitempty and most beads have no parent — it is part of the contract.
type readyBead struct {
	ID                 string            `json:"id"`
	Title              string            `json:"title"`
	Status             string            `json:"status"`
	Type               string            `json:"issue_type"`
	Priority           *int              `json:"priority,omitempty"`
	CreatedAt          time.Time         `json:"created_at"`
	UpdatedAt          time.Time         `json:"updated_at,omitempty,omitzero"`
	Assignee           string            `json:"assignee,omitempty"`
	From               string            `json:"from,omitempty"`
	ParentID           string            `json:"parent,omitempty"`
	Ref                string            `json:"ref,omitempty"`
	Needs              []string          `json:"needs,omitempty"`
	Description        string            `json:"description,omitempty"`
	AcceptanceCriteria string            `json:"acceptance_criteria,omitempty"`
	ExternalRef        string            `json:"external_ref,omitempty"`
	Labels             []string          `json:"labels,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
	Dependencies       []readyBeadDep    `json:"dependencies,omitempty"`
	Ephemeral          bool              `json:"ephemeral,omitempty"`
	NoHistory          bool              `json:"no_history,omitempty"`
	DeferUntil         *time.Time        `json:"defer_until,omitempty"`
	IsBlocked          *bool             `json:"is_blocked,omitempty"`
	// BlockedBy carries the row's OPEN-or-not blocking dependencies, in bd's
	// `bd ready --json` shape. It is populated only on the --status in_progress
	// arm, which is the crash-recovery read: a resumed holder must be told
	// whether the bead it still owns is currently gated, and `dependencies`
	// cannot say — it is a list of edges with no blocker STATUS on them, so a
	// consumer cannot tell a closed blocker from an open one.
	//
	// Every other arm leaves it absent, and absent means "not computed", which
	// every consumer already reads as not-blocked (fail-open). See
	// readyBlockedByForRows for why it is not computed on the ready arm.
	BlockedBy []readyBeadBlocker `json:"blocked_by,omitempty"`
}

// readyBeadDep is the wire shape of one dependency edge, for the same reason
// readyBead exists: beads.Dep is a domain type.
type readyBeadDep struct {
	IssueID     string `json:"issue_id"`
	DependsOnID string `json:"depends_on_id"`
	Type        string `json:"type"`
}

// readyBeadBlocker is one blocking dependency, carrying the field the readiness
// decision actually needs: the blocker's STATUS.
//
// The shape is bd's, not gc's invention — `bd ready --json` emits
// `blocked_by: [{"id":..., "status":...}]` and the hook's
// isDepBlockedHookCandidate reads exactly `status` off each element. Emitting a
// bare id list here (the shape `gc graph --json` uses) would decode without
// error and be silently ignored, which is worse than omitting the field.
type readyBeadBlocker struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// toReadyBeads projects domain beads onto the wire shape, attaching each row's
// blocked_by projection when one was computed.
func toReadyBeads(items []beads.Bead, blockers map[string][]readyBeadBlocker) []readyBead {
	out := make([]readyBead, 0, len(items))
	for _, b := range items {
		row := toReadyBead(b)
		row.BlockedBy = blockers[b.ID]
		out = append(out, row)
	}
	return out
}

func toReadyBead(b beads.Bead) readyBead {
	return readyBead{
		ID:                 b.ID,
		Title:              b.Title,
		Status:             b.Status,
		Type:               b.Type,
		Priority:           b.Priority,
		CreatedAt:          b.CreatedAt,
		UpdatedAt:          b.UpdatedAt,
		Assignee:           b.Assignee,
		From:               b.From,
		ParentID:           b.ParentID,
		Ref:                b.Ref,
		Needs:              b.Needs,
		Description:        b.Description,
		AcceptanceCriteria: b.AcceptanceCriteria,
		ExternalRef:        b.ExternalRef,
		Labels:             b.Labels,
		Metadata:           b.Metadata,
		Dependencies:       toReadyBeadDeps(b.Dependencies),
		Ephemeral:          b.Ephemeral,
		NoHistory:          b.NoHistory,
		DeferUntil:         b.DeferUntil,
		IsBlocked:          b.IsBlocked,
	}
}

func toReadyBeadDeps(deps []beads.Dep) []readyBeadDep {
	if len(deps) == 0 {
		return nil
	}
	out := make([]readyBeadDep, 0, len(deps))
	for _, d := range deps {
		out = append(out, readyBeadDep{IssueID: d.IssueID, DependsOnID: d.DependsOnID, Type: d.Type})
	}
	return out
}

// readyOpts carries the parsed `gc ready` flags.
//
// --include-ephemeral is deliberately absent: the reader spans both storage
// tiers on every leg unconditionally (beads.FederatedReadTier), so the flag
// selects nothing. See newReadyCmd.
type readyOpts struct {
	assignee       string
	unassigned     bool
	metadataFields []string
	excludeTypes   []string
	excludeLabels  []string
	sortOrder      string
	limit          int
	status         string
}

// newReadyCmd builds `gc ready`: the in-process, city-wide claimable-work reader
// that federates the stores a split city spreads its work across.
//
// It is the CLI half of the split-store ready federation whose API half is
// GET /v0/beads/ready, and it federates the same legs in the same order (see
// ready_federation.go). On a city that relocates nothing it reads the one store
// it always read.
//
// The output is a bare JSON array in bd's row shape, because it is a drop-in for
// the `bd ready --json` the default work_query builds — see readyBead.
func newReadyCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts readyOpts
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
           --sort oldest --limit 20 --json

Rows are emitted in canonical ready order (priority, created_at, id) unless
--sort selects a created_at order, and --limit is applied last, so a bounded
read is the true top-N of the merged set rather than the top-N of whichever
store answered first.

Every leg is read across both storage tiers, so the wisp/ephemeral rows an
orchestration step runs as are claimable work here whether or not
--include-ephemeral is passed.`,
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
func registerReadyFlags(cmd *cobra.Command, opts *readyOpts, includeEphemeral, jsonOut *bool) {
	cmd.Flags().StringVar(&opts.assignee, "assignee", "", "only work assigned to this identity")
	cmd.Flags().BoolVar(&opts.unassigned, "unassigned", false, "only unassigned work")
	cmd.Flags().StringArrayVar(&opts.metadataFields, "metadata-field", nil, "require metadata \"key=value\", or bare \"key\" for any non-empty value (repeatable)")
	cmd.Flags().StringArrayVar(&opts.excludeTypes, "exclude-type", nil, "drop beads of this issue type (repeatable)")
	cmd.Flags().StringArrayVar(&opts.excludeLabels, "exclude-label", nil, "drop beads carrying this label (repeatable)")
	cmd.Flags().StringVar(&opts.sortOrder, "sort", "", "sort order: oldest|newest (default: canonical ready order)")
	cmd.Flags().IntVar(&opts.limit, "limit", 0, "max beads to return (0 = unlimited)")
	// --include-ephemeral is accepted for parity with `bd ready`, which the
	// generated work_query carries verbatim on a bd-1.0.5 city. It selects
	// nothing, and that is the honest spelling rather than a silent one: every
	// leg is read at beads.FederatedReadTier, which already spans the wisp tier.
	// Binding it to a discarded variable is deliberate — a readyOpts field
	// nothing consults is how a flag ends up looking honored while one leg
	// quietly narrows (ga-8lyxc).
	cmd.Flags().BoolVar(includeEphemeral, "include-ephemeral", false, "accept --include-ephemeral for bd-ready parity (every leg already spans the wisp tier)")
	cmd.Flags().StringVar(&opts.status, "status", "", "list beads in this status instead of ready work: open|in_progress|blocked|closed")
	// --json is accepted for parity with `bd ready --json`, which the generated
	// work_query carries verbatim. The payload is a JSON array either way; the
	// flag exists so the query does not have to be rewritten to drop it.
	cmd.Flags().BoolVar(jsonOut, "json", true, "accept --json for bd-ready parity (output is always a JSON array)")
}

func cmdReady(opts readyOpts, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cityStore, err := openCityStoreAt(cityPath)
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
	items, err := readyBeadsForOpts(legs, opts)
	if err != nil {
		fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := writeJSON(stdout, items); err != nil {
		fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}

// readyBeadsForOpts is the whole reader as a pure function of the legs and the
// flags: federate, filter, order, bound, project.
//
// The bound is applied LAST, after the merge and after the order, and it is
// never pushed into a leg. A limit pushed down truncates each store to N and
// then re-cuts the concatenation, so the answer is the top-N of whichever legs
// happened to fill the quota first rather than the top-N of the city — and the
// larger a leg's backlog, the further its own rows are from the merged prefix.
func readyBeadsForOpts(legs []readyLeg, opts readyOpts) ([]readyBead, error) {
	// Every flag is validated before a single store is touched: a malformed
	// query must not cost a city-wide federated read to be told it was
	// malformed.
	filters, err := parseMetadataFieldFilters(opts.metadataFields)
	if err != nil {
		return nil, err
	}
	status, err := readyStatusSelector(opts.status)
	if err != nil {
		return nil, err
	}
	order, err := readySortOrder(opts.sortOrder)
	if err != nil {
		return nil, err
	}
	items, owners, err := readReadyCandidates(legs, status)
	if err != nil {
		return nil, err
	}
	items = filterReadyBeads(items, opts, filters)
	order(items)
	if opts.limit > 0 && len(items) > opts.limit {
		items = items[:opts.limit]
	}
	// Enrich AFTER the bound, so the dependency reads are paid only for the rows
	// actually emitted. The crash-recovery tier asks for --limit=1, which makes
	// this one dependency read on the one row that matters.
	blockers, err := readyBlockedByForRows(items, owners)
	if err != nil {
		return nil, err
	}
	return toReadyBeads(items, blockers), nil
}

// readyBlockedByForRows resolves each row's blocking dependencies through the
// leg that SERVED that row.
//
// Reading through the serving leg is the whole point. The shell enrichment this
// replaces resolves blockers with `bd show` in the agent's work directory, which
// on a split city resolves nothing for a relocated id — and, failing open,
// serves the candidate as unblocked. That is the correct direction for a resume
// but it re-serves a gate-blocked step on every tick. A relocated step's
// blockers live in the same store the step does, so the leg that answered is the
// leg that can answer this too.
//
// owners is empty on every arm but in_progress, which makes this a no-op there.
// The ready arm needs nothing: the store's own Ready() already applied
// readiness, so a row it returned is by construction unblocked, and computing
// blocked_by for it would be one dependency read per row to restate that.
//
// A dependency read that FAILS is returned, not swallowed. Reporting a gated
// bead as unblocked is what re-serves work no worker can advance; the federation
// is fail-loud for the same reason.
func readyBlockedByForRows(items []beads.Bead, owners map[string]readyLeg) (map[string][]readyBeadBlocker, error) {
	if len(owners) == 0 || len(items) == 0 {
		return nil, nil
	}
	out := make(map[string][]readyBeadBlocker, len(items))
	for _, item := range items {
		leg, ok := owners[item.ID]
		if !ok || leg.store == nil {
			continue
		}
		deps, err := leg.store.DepList(item.ID, "down")
		if err != nil {
			return nil, fmt.Errorf("%s store: listing dependencies of %q: %w", leg.label, item.ID, err)
		}
		blockers := make([]readyBeadBlocker, 0, len(deps))
		for _, dep := range deps {
			if !beads.IsReadyBlockingDependencyType(dep.Type) {
				continue
			}
			blocker, err := leg.store.Get(dep.DependsOnID)
			if err != nil {
				// A blocking edge whose target this leg cannot resolve is a
				// cross-store dependency, which the split topology forbids
				// (strict residence) — so it is a real fault, not an absence, and
				// treating it as "no blocker" would serve a gated bead.
				return nil, fmt.Errorf("%s store: resolving blocker %q of %q: %w", leg.label, dep.DependsOnID, item.ID, err)
			}
			blockers = append(blockers, readyBeadBlocker{ID: blocker.ID, Status: blocker.Status})
		}
		if len(blockers) > 0 {
			out[item.ID] = blockers
		}
	}
	return out, nil
}

// readReadyCandidates reads the merged candidate set from the legs.
//
// An empty --status is the default: claimable, ready work. Any other status
// lists beads in exactly that status — the flag is a real selector, not a single
// special case with an "everything else means ready" fallthrough, which answers
// `--status closed` with open work.
//
// Both arms state beads.FederatedReadTier explicitly, and neither has a branch
// that could state anything else. A tier left at the zero value is not a neutral
// default across these legs: the work legs' bead-policy layer rewrites it to
// TierBoth and the relocated class leg has no such layer, so the merged answer
// would be one question asked of the work stores and a narrower one asked of the
// store that holds the execution DAG. See beads.FederatedReadTier.
// It also reports, for the crash-recovery arm only, which leg served each row,
// so the blocked_by enrichment can ask the store that actually holds the bead.
// Ownership is recorded at MERGE time rather than re-probed afterwards: the
// merge is first-leg-wins, and a co-resident bead that is open in one leg and
// in_progress in another would resolve to the wrong store under a fresh probe.
func readReadyCandidates(legs []readyLeg, status string) ([]beads.Bead, map[string]readyLeg, error) {
	if status == "" {
		items, err := federateReadyBeads(legs, beads.ReadyQuery{TierMode: beads.FederatedReadTier})
		return items, nil, err
	}
	query := beads.ListQuery{
		Status:   status,
		TierMode: beads.FederatedReadTier,
		// Only the crash-recovery tier pays for a live read: a claim that
		// happened seconds ago must be visible, and a cached projection of it is
		// exactly the stale answer that re-dispatches work already in flight.
		Live: status == readyStatusInProgress,
	}
	if status != readyStatusInProgress {
		items, err := federateListBeads(legs, query)
		return items, nil, err
	}
	return federateListBeadsWithOwner(legs, query)
}

// readyStatusSelector validates --status and returns the status to list, or ""
// for the default claimable-work read.
//
// An unrecognized status is REJECTED rather than passed through. No bead in any
// store carries it, so passing it through answers a typo with an empty array —
// indistinguishable from "there is no such work", which is the fail-open this
// whole command exists to close.
func readyStatusSelector(status string) (string, error) {
	status = strings.TrimSpace(status)
	if status == "" || slices.Contains(readyKnownStatuses, status) {
		return status, nil
	}
	return "", fmt.Errorf("unknown --status %q (want one of %s)", status, strings.Join(readyKnownStatuses, ", "))
}

// filterReadyBeads applies the predicates the store readers cannot express
// natively: --assignee, --unassigned, --exclude-type, --exclude-label and
// --metadata-field.
//
// They are applied in Go rather than pushed into each leg on purpose: the legs
// are different backends with different filter semantics, and a predicate
// evaluated once over the merged set gives one answer instead of one answer per
// store.
func filterReadyBeads(items []beads.Bead, opts readyOpts, metaWant []metadataFieldFilter) []beads.Bead {
	assignee := strings.TrimSpace(opts.assignee)
	exclude := make(map[string]bool, len(opts.excludeTypes))
	for _, t := range opts.excludeTypes {
		if t = strings.TrimSpace(t); t != "" {
			exclude[t] = true
		}
	}
	out := make([]beads.Bead, 0, len(items))
	for _, b := range items {
		if opts.unassigned && strings.TrimSpace(b.Assignee) != "" {
			continue
		}
		if assignee != "" && strings.TrimSpace(b.Assignee) != assignee {
			continue
		}
		if exclude[b.Type] {
			continue
		}
		if beadCarriesExcludedLabel(b, opts.excludeLabels) {
			continue
		}
		if !beadMatchesMetadata(b, metaWant) {
			continue
		}
		out = append(out, b)
	}
	return out
}

func beadCarriesExcludedLabel(b beads.Bead, excluded []string) bool {
	for _, label := range excluded {
		if label = strings.TrimSpace(label); label != "" && beadLabelsContain(b.Labels, label) {
			return true
		}
	}
	return false
}

// metadataFieldFilter is one parsed --metadata-field predicate.
type metadataFieldFilter struct {
	key string
	// value is required to match exactly when present is false.
	value string
	// present marks the bare "key" form, which requires the key to be THERE
	// carrying a non-empty value.
	//
	// The distinction is the whole reason this is a struct and not a
	// map[string]string: collapsing the bare form onto value=="" inverts it. A
	// bare key then compares Metadata[key] against "", which MATCHES every bead
	// missing the key and REJECTS every bead carrying it — the precise opposite
	// of the filter, and invisible in production only because the generated
	// work_query always passes key=value.
	present bool
}

// parseMetadataFieldFilters parses repeated --metadata-field flags.
//
//	key=value  requires Metadata[key] == value (an empty value is a real value:
//	           "key=" requires the key to be present and empty)
//	key        requires Metadata[key] to be present and non-empty
func parseMetadataFieldFilters(fields []string) ([]metadataFieldFilter, error) {
	if len(fields) == 0 {
		return nil, nil
	}
	filters := make([]metadataFieldFilter, 0, len(fields))
	for _, f := range fields {
		key, value, hasValue := strings.Cut(f, "=")
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("--metadata-field %q has no key", f)
		}
		filters = append(filters, metadataFieldFilter{key: key, value: value, present: !hasValue})
	}
	return filters, nil
}

func beadMatchesMetadata(b beads.Bead, want []metadataFieldFilter) bool {
	for _, f := range want {
		got, ok := b.Metadata[f.key]
		if f.present {
			if !ok || strings.TrimSpace(got) == "" {
				return false
			}
			continue
		}
		if !ok || got != f.value {
			return false
		}
	}
	return true
}

// readySortOrder validates --sort and returns the ordering to impose on the
// merged set.
//
// The default is the CANONICAL ready order, taken from internal/beads where the
// SQL-backed readers define it (beads.SortBeadsReadyOrder), never re-derived
// here: a second copy of a total order across a package boundary is a copy that
// drifts, and this one decides which rows a bounded read serves.
//
// Imposing it is also what makes the merged set well-ordered at all. Each leg's
// own order is deterministic but they are not the same order — a caching-wrapped
// work store emits (priority, created_at, id) while the canonical relocated
// graph binding emits (created_at, id) with no priority term — so the raw
// concatenation has no single total order for --limit to cut.
//
// An unrecognized order is rejected rather than falling back to the default,
// which is how a bounded query quietly serves the wrong prefix.
func readySortOrder(order string) (func([]beads.Bead), error) {
	switch strings.TrimSpace(order) {
	case "":
		return beads.SortBeadsReadyOrder, nil
	case readySortOldest:
		return func(items []beads.Bead) { beads.SortBeads(items, beads.SortCreatedAsc) }, nil
	case readySortNewest:
		return func(items []beads.Bead) { beads.SortBeads(items, beads.SortCreatedDesc) }, nil
	default:
		return nil, fmt.Errorf("unknown --sort %q (want %s or %s)", order, readySortOldest, readySortNewest)
	}
}
