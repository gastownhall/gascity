package beads

import (
	"context"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

func controlReadyIDs(bs []Bead) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.ID)
	}
	return out
}

func TestControlReadyFilterActive(t *testing.T) {
	if (ControlReadyFilter{}).Active() {
		t.Error("empty filter Active() = true, want false")
	}
	if !(ControlReadyFilter{Assignees: []string{"a"}}).Active() {
		t.Error("assignee filter Active() = false, want true")
	}
	if !(ControlReadyFilter{Routes: []string{"r"}}).Active() {
		t.Error("route filter Active() = false, want true")
	}
	// A limit alone is not a predicate: with nothing to match it must stay a
	// no-op so generic /beads/ready callers keep the full set.
	if (ControlReadyFilter{PerGroupLimit: 5}).Active() {
		t.Error("limit-only filter Active() = true, want false")
	}
}

func TestControlReadyFilterApplyInactiveReturnsInputUnchanged(t *testing.T) {
	in := []Bead{{ID: "a"}, {ID: "b"}}
	got := (ControlReadyFilter{}).Apply(in)
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("inactive Apply = %v, want input unchanged %v", controlReadyIDs(got), controlReadyIDs(in))
	}
}

func TestControlReadyFilterApplyMatchesAssigneeAndRoutes(t *testing.T) {
	in := []Bead{
		{ID: "assigned-me", Assignee: "session-1"},
		{ID: "assigned-other", Assignee: "session-2"},
		{ID: "route-run-target", Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "ctl"}},
		{ID: "route-routed-to", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "ctl"}},
		{ID: "route-exec-routed", Metadata: map[string]string{beadmeta.ExecutionRoutedToMetadataKey: "ctl"}},
		{ID: "route-but-assigned", Assignee: "someone", Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "ctl"}},
		{ID: "route-other", Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "different"}},
	}
	f := ControlReadyFilter{Assignees: []string{"session-1"}, Routes: []string{"ctl"}}
	got := controlReadyIDs(f.Apply(in))
	want := []string{"assigned-me", "route-run-target", "route-routed-to", "route-exec-routed"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Apply = %v, want %v (assigned-other/route-but-assigned/route-other must be excluded)", got, want)
	}
}

func TestControlReadyFilterApplyExcludesIneligible(t *testing.T) {
	in := []Bead{
		{ID: "", Assignee: "s"},                   // empty ID
		{ID: "epic", Type: "epic", Assignee: "s"}, // epic
		{ID: "instantiating", Assignee: "s", Metadata: map[string]string{beadmeta.InstantiatingMetadataKey: "1"}}, // instantiating
		{ID: "ok", Assignee: "s"},
	}
	got := controlReadyIDs(ControlReadyFilter{Assignees: []string{"s"}}.Apply(in))
	want := []string{"ok"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Apply = %v, want %v (empty-id/epic/instantiating excluded)", got, want)
	}
}

func TestControlReadyFilterApplyPerGroupLimitAndDedup(t *testing.T) {
	in := []Bead{
		{ID: "a1", Assignee: "s"},
		{ID: "a2", Assignee: "s"},
		{ID: "a3", Assignee: "s"},
		// Matches both routes "r1" and "r2"; dedup must keep it once, under the
		// first route group it lands in.
		{ID: "shared", Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "r1", beadmeta.RoutedToMetadataKey: "r2"}},
	}
	f := ControlReadyFilter{Assignees: []string{"s"}, Routes: []string{"r1", "r2"}, PerGroupLimit: 2}
	got := controlReadyIDs(f.Apply(in))
	want := []string{"a1", "a2", "shared"} // assignee group capped at 2; shared deduped to route r1
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Apply = %v, want %v", got, want)
	}
}

// TestControlReadyFilterApplyIdempotent pins the property the split server/client
// filtering relies on: the supervisor pre-filters server-side and the dispatcher
// re-filters client-side, so Apply over an already-filtered set must reselect the
// same beads.
func TestControlReadyFilterApplyIdempotent(t *testing.T) {
	in := []Bead{
		{ID: "a1", Assignee: "s"},
		{ID: "a2", Assignee: "s"},
		{ID: "a3", Assignee: "s"},
		{ID: "r1", Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "ctl"}},
		{ID: "r2", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "ctl"}},
		{ID: "noise", Assignee: "other"},
	}
	f := ControlReadyFilter{Assignees: []string{"s"}, Routes: []string{"ctl"}, PerGroupLimit: 2}
	once := f.Apply(in)
	twice := f.Apply(once)
	if !reflect.DeepEqual(controlReadyIDs(once), controlReadyIDs(twice)) {
		t.Fatalf("Apply not idempotent: once=%v twice=%v", controlReadyIDs(once), controlReadyIDs(twice))
	}
}

// TestCachedReadyCacheOnlyDefaultTierExcludesEphemeral pins the storage-tier
// invariant the control dispatcher's cache-only ready scan relies on: it reads
// the default tier (TierIssues), which surfaces durable beads and excludes
// ephemeral (wisp) rows. Graph.v2 control beads are instantiated durable
// (ApplyGraphPlan → StorageDefault), so the dispatcher sees them; if a future
// change ever materialized a control bead ephemeral it would be silently dropped
// from this scan — a silent orchestration stall. This guard turns that into a
// loud failure. The cache primes both tiers, so the ephemeral bead is present in
// the projection and genuinely tier-filtered here, not merely absent.
func TestCachedReadyCacheOnlyDefaultTierExcludesEphemeral(t *testing.T) {
	backing := NewMemStore()
	durable, err := backing.Create(Bead{Type: "task", Title: "durable-control"})
	if err != nil {
		t.Fatalf("create durable: %v", err)
	}
	ephemeral, err := backing.Create(Bead{Type: "task", Title: "ephemeral-control", Ephemeral: true})
	if err != nil {
		t.Fatalf("create ephemeral: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}

	rows, err := HandlesFor(cache).Cached.ReadyCacheOnly()
	if err != nil {
		t.Fatalf("ReadyCacheOnly: %v", err)
	}
	got := make(map[string]bool, len(rows))
	for _, b := range rows {
		got[b.ID] = true
	}
	if !got[durable.ID] {
		t.Errorf("durable control bead %s missing from the default-tier ready scan", durable.ID)
	}
	if got[ephemeral.ID] {
		t.Errorf("ephemeral bead %s surfaced; the default-tier control-ready scan must exclude it, so control beads must be instantiated durable", ephemeral.ID)
	}
}
