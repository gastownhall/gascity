package readywork

import (
	"fmt"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// The two orders --sort accepts, as `bd ready --sort` spells them. Anything
// else is a typo, and a typo that silently fell back to a different order is
// how a bounded query ends up serving the wrong prefix.
const (
	SortOldest = "oldest"
	SortNewest = "newest"
)

// The statuses --status accepts.
const (
	// StatusOpen is Gas City's open status: every non-closed bead that is not
	// in_progress (see readOpenStatusCandidates).
	StatusOpen = "open"
	// StatusInProgress is the crash-recovery tier, the one status whose rows
	// carry their blockers (blocked_by): a step assigned to a worker that just
	// died must be resumable, and a resumed holder must be told whether the bead
	// it still owns is gated.
	StatusInProgress = "in_progress"
	// StatusBlocked and StatusClosed list beads in exactly that status.
	StatusBlocked = "blocked"
	StatusClosed  = "closed"
)

// knownStatuses is the closed set --status accepts. It is closed on purpose: an
// unrecognized status matches no bead in any store, so accepting one would
// answer a typo with an empty array, the fail-open this reader exists to close.
var knownStatuses = []string{StatusOpen, StatusInProgress, StatusBlocked, StatusClosed}

// KnownStatuses is the closed set of statuses --status accepts.
func KnownStatuses() []string { return slices.Clone(knownStatuses) }

// Query is one ready-work question: the `gc ready` flags, and the parameters of
// GET /v0/city/{cityName}/work/ready.
//
// --include-ephemeral and --json have no field. Every leg is read across both
// storage tiers (beads.FederatedReadTier) and the answer is always a JSON array,
// so neither selects anything; a field nothing consults is how a flag ends up
// looking honored while one leg quietly narrows (ga-8lyxc).
type Query struct {
	// Assignee keeps only beads whose assignee is exactly this identity, as
	// `bd ready --assignee` matches. Surrounding whitespace in the flag is
	// trimmed; the stored assignee is compared as stored.
	Assignee string
	// Unassigned keeps only beads with no assignee (an empty assignee), as
	// `bd ready --unassigned` matches.
	Unassigned bool
	// MetadataFields are "key=value" (the value must match exactly, and an empty
	// value is a real value) or bare "key" (the key must be present with a
	// non-empty value) predicates, all of which must hold.
	MetadataFields []string
	// ExcludeTypes drops beads of these issue types.
	ExcludeTypes []string
	// ExcludeLabels drops beads carrying any of these labels.
	ExcludeLabels []string
	// Sort is "" for the canonical ready order (priority, created_at, id),
	// SortOldest, or SortNewest.
	Sort string
	// Limit bounds the answer to its first Limit rows; zero or less is
	// unlimited. It is applied after the merge and after the order, so the
	// answer is the city's top N rather than the top N of whichever store
	// answered first.
	Limit int
	// Status is "" for ready work, or one of the statuses in knownStatuses to
	// list beads in that status instead.
	Status string
}

// compiled is a validated Query: every flag parsed once, before any store is
// touched. A malformed query must not cost a city-wide federated read to be told
// it was malformed.
type compiled struct {
	query         Query
	assignee      string
	excludeTypes  map[string]bool
	excludeLabels []string
	metadata      []metadataFieldFilter
	status        string
	sort          string
}

// Validate reports whether q is well formed, without reading anything.
func (q Query) Validate() error {
	_, err := compile(q)
	return err
}

func compile(q Query) (compiled, error) {
	metadata, err := parseMetadataFieldFilters(q.MetadataFields)
	if err != nil {
		return compiled{}, err
	}
	status, err := statusSelector(q.Status)
	if err != nil {
		return compiled{}, err
	}
	sortOrder, err := sortSelector(q.Sort)
	if err != nil {
		return compiled{}, err
	}
	c := compiled{
		query:    q,
		assignee: strings.TrimSpace(q.Assignee),
		metadata: metadata,
		status:   status,
		sort:     sortOrder,
	}
	for _, t := range q.ExcludeTypes {
		if t = strings.TrimSpace(t); t != "" {
			if c.excludeTypes == nil {
				c.excludeTypes = make(map[string]bool, len(q.ExcludeTypes))
			}
			c.excludeTypes[t] = true
		}
	}
	for _, label := range q.ExcludeLabels {
		if label = strings.TrimSpace(label); label != "" {
			c.excludeLabels = append(c.excludeLabels, label)
		}
	}
	return c, nil
}

// Matcher returns the row predicate of q: --assignee, --unassigned,
// --exclude-type, --exclude-label and --metadata-field, the predicates a work
// query's tier applies to every candidate. It is what the merged set is filtered
// by, and it is exported for the reader-agreement checks that hold the
// controller's demand census to the same predicate a seat claims through.
func (q Query) Matcher() (func(beads.Bead) bool, error) {
	c, err := compile(q)
	if err != nil {
		return nil, err
	}
	return c.matches, nil
}

// matches applies the row predicates to one bead.
//
// --assignee and --unassigned compare the stored assignee exactly, as `bd ready`
// does and as every store's own assignee filter does, so `gc ready` stays the
// drop-in for the `bd ready` the generated work query was built on.
func (c compiled) matches(b beads.Bead) bool {
	if c.query.Unassigned && b.Assignee != "" {
		return false
	}
	if c.assignee != "" && b.Assignee != c.assignee {
		return false
	}
	if c.excludeTypes[b.Type] {
		return false
	}
	for _, label := range c.excludeLabels {
		if slices.Contains(b.Labels, label) {
			return false
		}
	}
	return beadMatchesMetadata(b, c.metadata)
}

// filter keeps the beads that pass every row predicate, in order.
func (c compiled) filter(items []beads.Bead) []beads.Bead {
	out := make([]beads.Bead, 0, len(items))
	for _, b := range items {
		if c.matches(b) {
			out = append(out, b)
		}
	}
	return out
}

// order imposes the query's order on the merged set.
//
// The default is the CANONICAL ready order, taken from internal/beads where the
// SQL-backed readers define it (beads.SortBeadsReadyOrder), never re-derived
// here: a second copy of a total order across a package boundary is a copy that
// drifts, and this one decides which rows a bounded read serves. Imposing it is
// also what makes the merged set well-ordered at all: each leg emits its own rows
// in its own order, and a concatenation of sorted legs is not sorted.
func (c compiled) order(items []beads.Bead) {
	switch c.sort {
	case SortOldest:
		beads.SortBeads(items, beads.SortCreatedAsc)
	case SortNewest:
		beads.SortBeads(items, beads.SortCreatedDesc)
	default:
		beads.SortBeadsReadyOrder(items)
	}
}

// statusSelector validates --status and returns the status to list, or "" for
// the default claimable-work read.
//
// An unrecognized status is REJECTED rather than passed through. No bead in any
// store carries it, so passing it through answers a typo with an empty array,
// indistinguishable from "there is no such work".
func statusSelector(status string) (string, error) {
	status = strings.TrimSpace(status)
	if status == "" || slices.Contains(knownStatuses, status) {
		return status, nil
	}
	return "", fmt.Errorf("unknown --status %q (want one of %s)", status, strings.Join(knownStatuses, ", "))
}

// sortSelector validates --sort. An unrecognized order is rejected rather than
// falling back to the default, which is how a bounded query quietly serves the
// wrong prefix.
func sortSelector(order string) (string, error) {
	switch order = strings.TrimSpace(order); order {
	case "", SortOldest, SortNewest:
		return order, nil
	default:
		return "", fmt.Errorf("unknown --sort %q (want %s or %s)", order, SortOldest, SortNewest)
	}
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
	// missing the key and REJECTS every bead carrying it, the precise opposite of
	// the filter.
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
