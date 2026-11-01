package config

import (
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// The default work query filters gc.exclude_claimants BEFORE it picks the head
// it serves, so an excluded bead at the front of a bounded read cannot starve
// the allowed work behind it. `gc hook --claim` enforces the same exclusion in
// Go on every tier (cmd/gc/cmd_hook_claim_exclude_claimants_test.go); these
// tests EXECUTE the generated shell against a fake bd to pin the query half.

const (
	excludeQueryPool     = "myrig/review"
	excludeQueryReviewer = "myrig/review.one"
	excludeQueryOther    = "myrig/review.two"
)

func excludeQueryRow(id, status, assignee, exclude string) string {
	md := `"gc.routed_to":"` + excludeQueryPool + `"`
	if exclude != "" {
		md += `,"` + beadmeta.ExcludeClaimantsMetadataKey + `":"` + exclude + `"`
	}
	return `{"id":"` + id + `","status":"` + status + `","assignee":"` + assignee + `","metadata":{` + md + `}}`
}

func excludeQueryAgent() Agent {
	return Agent{Name: "review", Dir: "myrig"}
}

// excludeQueryFakeBD serves window to the bounded routed read and full to an
// unbounded one; every other read is empty.
func excludeQueryFakeBD(window, full string) string {
	return `#!/bin/sh
case "$1" in
  ready)
    case "$*" in
      *"--assignee"*) printf '[]' ;;
      *"gc.run_target"*) printf '[]' ;;
      *"--limit 0"*|*"--limit=0"*) printf '%s' '` + full + `' ;;
      *"gc.routed_to=` + excludeQueryPool + `"*) printf '%s' '` + window + `' ;;
      *) printf '[]' ;;
    esac ;;
  *) printf '[]' ;;
esac
`
}

func TestEffectiveWorkQuerySkipsExcludedRoutedHead(t *testing.T) {
	requireJQ(t)
	rows := `[` + excludeQueryRow("recheck-1", "open", "", excludeQueryReviewer) + `,` +
		excludeQueryRow("fresh-2", "open", "", "") + `]`
	for _, key := range []string{"GC_ALIAS", "GC_AGENT", "GC_SESSION_ID"} {
		t.Run(key, func(t *testing.T) {
			out := runEffectiveWorkQuery(t, excludeQueryAgent(), map[string]string{
				"GC_SESSION_ORIGIN": "ephemeral",
				key:                 excludeQueryReviewer,
			}, excludeQueryFakeBD(rows, rows))
			ids := workQueryOutputIDOrder(t, strings.TrimSpace(out))
			if len(ids) != 1 || ids[0] != "fresh-2" {
				t.Fatalf("EffectiveWorkQuery() ids = %v, want [fresh-2]: recheck-1 excludes %s", ids, excludeQueryReviewer)
			}
		})
	}
}

func TestEffectiveWorkQueryServesExcludedBeadToEligibleClaimant(t *testing.T) {
	requireJQ(t)
	rows := `[` + excludeQueryRow("recheck-1", "open", "", excludeQueryReviewer) + `]`
	out := runEffectiveWorkQuery(t, excludeQueryAgent(), map[string]string{
		"GC_SESSION_ORIGIN": "ephemeral",
		"GC_ALIAS":          excludeQueryOther,
	}, excludeQueryFakeBD(rows, rows))
	if ids := workQueryOutputIDOrder(t, strings.TrimSpace(out)); len(ids) != 1 || ids[0] != "recheck-1" {
		t.Fatalf("EffectiveWorkQuery() ids = %v, want [recheck-1] for an identity the bead does not exclude", ids)
	}
}

// A full bounded window of excluded rows widens to an unbounded read, so the
// allowed bead past the window is still found.
func TestEffectiveWorkQueryWidensPastAFullyExcludedWindow(t *testing.T) {
	requireJQ(t)
	excluded := make([]string, 0, 20)
	for i := range 20 {
		excluded = append(excluded, excludeQueryRow("recheck-"+strconv.Itoa(i), "open", "", excludeQueryReviewer))
	}
	window := `[` + strings.Join(excluded, ",") + `]`
	full := `[` + strings.Join(append(excluded, excludeQueryRow("fresh-21", "open", "", "")), ",") + `]`
	out := runEffectiveWorkQuery(t, excludeQueryAgent(), map[string]string{
		"GC_SESSION_ORIGIN": "ephemeral",
		"GC_ALIAS":          excludeQueryReviewer,
	}, excludeQueryFakeBD(window, full))
	if ids := workQueryOutputIDOrder(t, strings.TrimSpace(out)); len(ids) != 1 || ids[0] != "fresh-21" {
		t.Fatalf("EffectiveWorkQuery() ids = %v, want [fresh-21] past a window of 20 excluded rows", ids)
	}
}

// A short window that filters to nothing is the whole answer: no widening read.
func TestEffectiveWorkQueryDoesNotWidenAShortExcludedWindow(t *testing.T) {
	requireJQ(t)
	window := `[` + excludeQueryRow("recheck-1", "open", "", excludeQueryReviewer) + `]`
	full := `[` + excludeQueryRow("unbounded-only", "open", "", "") + `]`
	out := runEffectiveWorkQuery(t, excludeQueryAgent(), map[string]string{
		"GC_SESSION_ORIGIN": "ephemeral",
		"GC_ALIAS":          excludeQueryReviewer,
	}, excludeQueryFakeBD(window, full))
	if strings.TrimSpace(out) != "[]" {
		t.Fatalf("EffectiveWorkQuery() = %q, want [] (a short window is complete; no widened read)", out)
	}
}

// An excluded in_progress assignment falls through to the session's other
// work instead of being served and refused on every tick.
func TestEffectiveWorkQueryInProgressTierFallsThroughWhenExcluded(t *testing.T) {
	requireJQ(t)
	bd := `#!/bin/sh
case "$1" in
  list) printf '%s' '[` + excludeQueryRow("held-1", "in_progress", "sess-1", excludeQueryReviewer) + `]' ;;
  show) printf '%s' '[{"id":"held-1","status":"in_progress","dependencies":[]}]' ;;
  ready)
    case "$*" in
      *"--assignee"*) printf '%s' '[` + excludeQueryRow("pinned-2", "open", "sess-1", "") + `]' ;;
      *) printf '[]' ;;
    esac ;;
  *) printf '[]' ;;
esac
`
	out := runEffectiveWorkQuery(t, excludeQueryAgent(), map[string]string{
		"GC_SESSION_ORIGIN": "ephemeral",
		"GC_SESSION_ID":     "sess-1",
		"GC_ALIAS":          excludeQueryReviewer,
	}, bd)
	if ids := workQueryOutputIDOrder(t, strings.TrimSpace(out)); len(ids) != 1 || ids[0] != "pinned-2" {
		t.Fatalf("EffectiveWorkQuery() ids = %v, want [pinned-2] behind the excluded in_progress bead", ids)
	}
}

// An excluded ready assignment falls through to routed work.
func TestEffectiveWorkQueryAssignedReadyTierFallsThroughWhenExcluded(t *testing.T) {
	requireJQ(t)
	bd := `#!/bin/sh
case "$1" in
  ready)
    case "$*" in
      *"--assignee"*) printf '%s' '[` + excludeQueryRow("pinned-1", "open", "sess-1", excludeQueryReviewer) + `]' ;;
      *"gc.run_target"*) printf '[]' ;;
      *"gc.routed_to=` + excludeQueryPool + `"*) printf '%s' '[` + excludeQueryRow("fresh-2", "open", "", "") + `]' ;;
      *) printf '[]' ;;
    esac ;;
  *) printf '[]' ;;
esac
`
	out := runEffectiveWorkQuery(t, excludeQueryAgent(), map[string]string{
		"GC_SESSION_ORIGIN": "ephemeral",
		"GC_SESSION_ID":     "sess-1",
		"GC_ALIAS":          excludeQueryReviewer,
	}, bd)
	if ids := workQueryOutputIDOrder(t, strings.TrimSpace(out)); len(ids) != 1 || ids[0] != "fresh-2" {
		t.Fatalf("EffectiveWorkQuery() ids = %v, want [fresh-2] behind the excluded ready assignment", ids)
	}
}

// A reader payload the filter cannot parse is served unchanged, like every
// other jq gate in the query: the hook's decode decides what to do with it.
func TestEffectiveWorkQueryExclusionFailsOpenOnUnparseableRows(t *testing.T) {
	requireJQ(t)
	const garbage = `not-json`
	out := runEffectiveWorkQuery(t, excludeQueryAgent(), map[string]string{
		"GC_SESSION_ORIGIN": "ephemeral",
		"GC_ALIAS":          excludeQueryReviewer,
	}, excludeQueryFakeBD(garbage, garbage))
	if strings.TrimSpace(out) != garbage {
		t.Fatalf("EffectiveWorkQuery() = %q, want the unparseable payload passed through", out)
	}
}

// The reconciler's count-form runs with no session identity, so an excluded
// bead is still demand for its pool: some other seat can claim it.
func TestPoolDemandCountStillCountsExcludedBeads(t *testing.T) {
	requireJQ(t)
	rows := `[` + excludeQueryRow("recheck-1", "open", "", excludeQueryReviewer) + `]`
	a := excludeQueryAgent()
	out := runShellWithFakeBd(t, a.EffectivePoolDemandQuery(), map[string]string{
		"GC_ALIAS": excludeQueryReviewer,
	}, excludeQueryFakeBD(rows, rows))
	if strings.TrimSpace(out) != "1" {
		t.Fatalf("EffectivePoolDemandQuery() = %q, want 1: exclusion is a per-claimant filter, not a demand filter", out)
	}
}
