// Package beadthroughput answers the recurring "how much did we ship in
// this window" operator question: bead open/close counts over a time
// range, grouped by store, type, and label. Introduced by issue #5852 as
// the first of four `gc analyze` subcommands reading events.jsonl.
//
// The package is a pure-data layer: it parses events.Event slices into a
// grouped report. The CLI (cmd/gc/cmd_analyze_beads.go) handles IO,
// filtering, and presentation — the same split reliability
// (internal/reliability) established for #1254.
package beadthroughput

import (
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// defaultStore is the group label for a bead ID with no recognized
// mint-prefix — the common case for ordinary work beads, whose ids are
// rig-prefixed sequence numbers rather than a reserved coordination-class
// prefix (see internal/config/reserved_prefixes.go for the prefixes a
// relocated class mints under, e.g. "gcg-" for graph, "gcm-" for
// messaging). This package does not import internal/config to stay a
// pure consumer of events.Event; it reports the raw ID-prefix namespace
// rather than resolving it to a semantic class name.
const defaultStore = "default"

// Window restricts the events considered to a time range. Zero-valued
// fields disable the corresponding bound. Mirrors internal/reliability's
// Window; kept as a separate type so this package has no dependency on
// reliability and each analysis package owns its windowing independently.
type Window struct {
	Since time.Time
	Until time.Time
}

// Contains reports whether ts is within the window. A zero-valued bound
// disables that side of the check.
func (w Window) Contains(ts time.Time) bool {
	if !w.Since.IsZero() && ts.Before(w.Since) {
		return false
	}
	if !w.Until.IsZero() && ts.After(w.Until) {
		return false
	}
	return true
}

// Filter narrows the grouped report to a specific store, type, and/or
// label. Empty fields disable the corresponding filter. Matching is
// case-insensitive, consistent with reliability's Model/Rig filters.
type Filter struct {
	Store string
	Type  string
	Label string
}

func (f Filter) matches(key GroupKey) bool {
	if f.Store != "" && !strings.EqualFold(f.Store, key.Store) {
		return false
	}
	if f.Type != "" && !strings.EqualFold(f.Type, key.Type) {
		return false
	}
	if f.Label != "" && !strings.EqualFold(f.Label, key.Label) {
		return false
	}
	return true
}

// GroupKey is the (store, type, label) tuple the report groups by. A bead
// carrying N labels contributes to N label buckets (once each), so
// per-group counts summed across the Label dimension can exceed the
// number of distinct beads touched — Report.Total is computed from
// distinct bead ids, not by summing groups, precisely to keep that
// denominator honest. A bead with no labels groups under Label "".
type GroupKey struct {
	Store string `json:"store"`
	Type  string `json:"type"`
	Label string `json:"label"`
}

// Group reports open/close counts for one (store, type, label) bucket.
type Group struct {
	Key    GroupKey `json:"key"`
	Opened int      `json:"opened"`
	Closed int      `json:"closed"`
}

// Net returns Opened - Closed (positive: backlog grew; negative: shrank).
func (g Group) Net() int { return g.Opened - g.Closed }

// Report is the top-level result of an analysis pass.
type Report struct {
	Window  Window  `json:"-"`
	Filter  Filter  `json:"-"`
	Groups  []Group `json:"groups"`
	Total   Group   `json:"total"`
	Skipped int     `json:"skipped"` // bead.created/bead.closed events whose payload did not decode to a bead
}

// Analyze produces a bead-throughput report from the supplied events.
//
// Only events.BeadCreated and events.BeadClosed are considered; both carry
// a full bead snapshot (beads.BeadEventPayload's wire shape, decoded here
// via beads.DecodeBeadEventPayload to avoid an internal/api import). Events
// outside the window, or whose payload does not decode to a bead with an
// id, are dropped; the latter count toward Report.Skipped so a malformed
// or missing payload is visible rather than silently absorbed into a
// zero-value bucket.
func Analyze(es []events.Event, win Window, flt Filter) Report {
	groups := make(map[GroupKey]*Group)
	report := Report{Window: win, Filter: flt}

	// Total is computed from distinct bead ids per direction so a
	// multi-label bead is not double-counted in the denominator.
	openedIDs := make(map[string]struct{})
	closedIDs := make(map[string]struct{})

	groupFor := func(key GroupKey) *Group {
		g, ok := groups[key]
		if !ok {
			g = &Group{Key: key}
			groups[key] = g
		}
		return g
	}

	for _, e := range es {
		if e.Type != events.BeadCreated && e.Type != events.BeadClosed {
			continue
		}
		if !win.Contains(e.Ts) {
			continue
		}
		bead, ok := beads.DecodeBeadEventPayload(e.Payload)
		if !ok || strings.TrimSpace(bead.ID) == "" {
			report.Skipped++
			continue
		}

		store := storeForBeadID(bead.ID)
		labels := bead.Labels
		if len(labels) == 0 {
			labels = []string{""}
		}

		matched := false
		for _, label := range labels {
			key := GroupKey{Store: store, Type: bead.Type, Label: label}
			if !flt.matches(key) {
				continue
			}
			matched = true
			g := groupFor(key)
			switch e.Type {
			case events.BeadCreated:
				g.Opened++
			case events.BeadClosed:
				g.Closed++
			}
		}
		if !matched {
			continue
		}
		switch e.Type {
		case events.BeadCreated:
			openedIDs[bead.ID] = struct{}{}
		case events.BeadClosed:
			closedIDs[bead.ID] = struct{}{}
		}
	}

	report.Groups = sortedGroups(groups)
	report.Total = Group{Opened: len(openedIDs), Closed: len(closedIDs)}
	return report
}

// storeForBeadID returns the ID-prefix namespace before the first "-" in
// id, or defaultStore when id has no hyphen (a bare sequence number or
// single-token id) — the same "prefix, then '-', then sequence" shape
// bdRelocatedClasses/ReservedClassPrefix rely on for coordination-class
// ids (gcg-, gcm-, gcs-, gco-, gcn-), and which ordinary rig-prefixed
// work ids share.
func storeForBeadID(id string) string {
	if i := strings.IndexByte(id, '-'); i > 0 {
		return id[:i]
	}
	return defaultStore
}

// sortedGroups returns the report groups sorted deterministically:
// descending total activity (opened+closed), then ascending
// store/type/label for stable reading.
func sortedGroups(groups map[GroupKey]*Group) []Group {
	out := make([]Group, 0, len(groups))
	for _, g := range groups {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		ai, aj := out[i].Opened+out[i].Closed, out[j].Opened+out[j].Closed
		if ai != aj {
			return ai > aj
		}
		if out[i].Key.Store != out[j].Key.Store {
			return out[i].Key.Store < out[j].Key.Store
		}
		if out[i].Key.Type != out[j].Key.Type {
			return out[i].Key.Type < out[j].Key.Type
		}
		return out[i].Key.Label < out[j].Key.Label
	})
	return out
}
