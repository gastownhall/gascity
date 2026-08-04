package api

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/orders"
)

func TestParseOrdersFeedLimitCapsLargeValues(t *testing.T) {
	if got := parseOrdersFeedLimit(""); got != 50 {
		t.Fatalf("default limit = %d, want 50", got)
	}
	if got := parseOrdersFeedLimit("25"); got != 25 {
		t.Fatalf("parsed limit = %d, want 25", got)
	}
	if got := parseOrdersFeedLimit("999999"); got != maxOrdersFeedLimit {
		t.Fatalf("capped limit = %d, want %d", got, maxOrdersFeedLimit)
	}
}

func TestOrderTrackingStatusTreatsWispFailedAsFailed(t *testing.T) {
	run, ok := orders.RunFromTrackingBead(beads.Bead{
		Status: "closed",
		Labels: []string{"order-tracking", "order-run:nightly", "wisp", "wisp-failed"},
	})
	if !ok {
		t.Fatal("RunFromTrackingBead ok = false")
	}
	if got := run.State(); got != "failed" {
		t.Fatalf("run.State() = %q, want failed", got)
	}
}

func TestOrderTrackingExecEnvFailedClassifiesAsFailedExec(t *testing.T) {
	run, ok := orders.RunFromTrackingBead(beads.Bead{
		Status: "closed",
		Labels: []string{"order-tracking", "order-run:nightly", "exec-env-failed"},
	})
	if !ok {
		t.Fatal("RunFromTrackingBead ok = false")
	}
	if got := run.State(); got != "failed" {
		t.Fatalf("run.State() = %q, want failed", got)
	}
	if got := orderTrackingTarget(orders.Order{}, false, run); got != "exec" {
		t.Fatalf("orderTrackingTarget = %q, want exec", got)
	}
	if got := orderTrackingType(orders.Order{}, false, run); got != "exec" {
		t.Fatalf("orderTrackingType = %q, want exec", got)
	}
}

func TestWorkflowProjectionTargetKeepsRunTargetMigrationFallback(t *testing.T) {
	root := beads.Bead{Metadata: map[string]string{
		"gc.run_target": "gascity/reviewer",
	}}
	if got := workflowProjectionTarget(root); got != "gascity/reviewer" {
		t.Fatalf("workflowProjectionTarget = %q, want gc.run_target fallback", got)
	}
}

func TestOrderTrackingTriggerEnvFailedClassifiesOpenAndClosedAsFailed(t *testing.T) {
	for _, status := range []string{"open", "closed"} {
		t.Run(status, func(t *testing.T) {
			run, ok := orders.RunFromTrackingBead(beads.Bead{
				Status: status,
				Labels: []string{"order-tracking", "order-run:nightly", "trigger-env-failed"},
			})
			if !ok {
				t.Fatal("RunFromTrackingBead ok = false")
			}
			if got := run.State(); got != "failed" {
				t.Fatalf("run.State(%s) = %q, want failed", status, got)
			}
		})
	}
}

func TestParseMonitorTimestampAcceptsRFC3339AndNano(t *testing.T) {
	base := "2026-03-26T14:06:31+01:00"
	if got := parseMonitorTimestamp(base); got.IsZero() {
		t.Fatalf("parseMonitorTimestamp(%q) = zero, want parsed timestamp", base)
	}

	nano := "2026-03-26T14:06:31.123456789+01:00"
	got := parseMonitorTimestamp(nano)
	if got.IsZero() {
		t.Fatalf("parseMonitorTimestamp(%q) = zero, want parsed timestamp", nano)
	}
	if got.Nanosecond() != 123456789 {
		t.Fatalf("nanoseconds = %d, want 123456789", got.Nanosecond())
	}
	if got.Format("2006-01-02T15:04:05.999999999Z07:00") != nano {
		t.Fatalf("formatted timestamp = %q, want %q", got.Format("2006-01-02T15:04:05.999999999Z07:00"), nano)
	}
}

func TestBuildWorkflowRunProjectionsKeepsInProgressChildrenOnHistoryFailure(t *testing.T) {
	state := newFakeState(t)
	mem := beads.NewMemStore()
	state.stores = map[string]beads.Store{
		"myrig": &workflowProjectionStore{MemStore: mem},
	}

	root, err := mem.Create(beads.Bead{
		Title: "Deploy",
		Type:  "workflow",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	child, err := mem.Create(beads.Bead{
		Title:    "Run step",
		Type:     "task",
		Assignee: "agent/alice",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	status := "in_progress"
	if err := mem.Update(child.ID, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatal(err)
	}

	got, err := buildWorkflowRunProjections(state, "rig", "myrig", "")
	if err != nil {
		t.Fatalf("buildWorkflowRunProjections: %v", err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(got.Items))
	}
	if got.Items[0].Status != "active" {
		t.Fatalf("status = %q, want active", got.Items[0].Status)
	}
	if !got.Items[0].UpdatedAt.Equal(child.CreatedAt) {
		t.Fatalf("updatedAt = %s, want %s", got.Items[0].UpdatedAt, child.CreatedAt)
	}
}

func TestBuildOrderRunFeedItemsUsesAllOrdersForDisabledExecMetadata(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	disabled := false
	state.allOrders = []orders.Order{
		{Name: "digest", Exec: "scripts/digest.sh", Trigger: "cooldown", Interval: "1h", Enabled: &disabled},
	}

	tracking, err := state.cityBeadStore.Create(beads.Bead{
		Title:  "order:digest",
		Status: "closed",
		Labels: []string{"order-tracking", "order-run:digest", "wisp"},
	})
	if err != nil {
		t.Fatalf("create tracking bead: %v", err)
	}

	got, err := buildOrderRunFeedItems(state, "city", "test-city")
	if err != nil {
		t.Fatalf("buildOrderRunFeedItems: %v", err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(got.Items))
	}
	item := got.Items[0]
	if item.BeadID != tracking.ID {
		t.Fatalf("bead_id = %q, want %q", item.BeadID, tracking.ID)
	}
	if item.Type != "exec" || item.Target != "exec" || !item.DetailAvailable || !item.RunDetailAvailable {
		t.Fatalf("item = %+v, want disabled exec order metadata", item)
	}
}

func TestOrderTrackingUpdatedAtLogsLookupFailure(t *testing.T) {
	front := orders.NewStore(beads.OrdersStore{Store: labelFailListStore{
		Store:     beads.NewMemStore(),
		failLabel: "order-run:digest",
	}})
	run := orders.OrderRun{
		Scoped:    "digest",
		CreatedAt: time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC),
	}

	var logs strings.Builder
	origLogf := orderFeedLogf
	orderFeedLogf = func(format string, args ...any) {
		logs.WriteString(strings.TrimSpace(fmt.Sprintf(format, args...)))
		logs.WriteByte('\n')
	}
	defer func() { orderFeedLogf = origLogf }()

	got := orderTrackingUpdatedAt(front, run)
	if !got.Equal(run.CreatedAt) {
		t.Fatalf("updatedAt = %s, want %s", got, run.CreatedAt)
	}
	if !strings.Contains(logs.String(), "order feed update lookup failed") {
		t.Fatalf("logs = %q, want update lookup failure warning", logs.String())
	}
}

type workflowProjectionStore struct {
	*beads.MemStore
}

type labelFailListStore struct {
	beads.Store
	failLabel string
}

func (s labelFailListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Label == s.failLabel {
		return nil, errors.New("list failed")
	}
	return s.Store.List(query)
}

func (s *workflowProjectionStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.IncludeClosed && query.Metadata["gc.root_bead_id"] != "" {
		return nil, errors.New("history unavailable")
	}
	return s.MemStore.List(query)
}

// collapsedStatusProjectionStore models the production read path for the
// workflow projection. A non-Live read (the raw scan, or any cached read)
// returns blocked and deferred beads indistinguishable from ready work:
// mapBdStatus folds bd's blocked/deferred/review/testing into Gas City's
// "open", and CachingStore.List matches on that already-collapsed status. Only
// the backing store filters on the raw status, by passing --status to bd, and
// only a Live query reaches it.
type collapsedStatusProjectionStore struct {
	beads.Store
	rawScan      []beads.Bead            // non-Live: blocked rows present, collapsed to "open"
	liveByStatus map[string][]beads.Bead // Live: bd filtered on the raw status
	liveStatuses []string
}

func (s *collapsedStatusProjectionStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if !q.Live {
		return append([]beads.Bead(nil), s.rawScan...), nil
	}
	s.liveStatuses = append(s.liveStatuses, q.Status)
	return append([]beads.Bead(nil), s.liveByStatus[q.Status]...), nil
}

// TestListActiveWorkflowProjectionBeadsExcludesBlocked covers the read side of
// gc-4zb. The workflow-root spawn path selects on gc.routed_to without
// re-checking status, so a blocked root that reaches this projection while
// still carrying a route is spawned against and burns a polecat slot on a no-op
// drain.
//
// Live reproduction (gc-nz5i, root gc-27xf, step mol-do-work.do-work):
// dolt_history_issues shows status=blocked while gc.routed_to stayed
// /home/ds/gascity/polecat from 04:00:21 to 04:08:17, and the bead's own
// reroute_observed records a second slot burned against it while blocked. It
// carries no gc.run_target, so the writer-side restore cannot re-stamp it —
// this is the reader, not the writer.
//
// Filtering the scan on b.Status cannot fix it: the blocked bead's Status is
// already the collapsed "open", so it satisfies an {open, in_progress}
// allowlist. The gate has to be a status-scoped Live read that lets bd filter
// on the raw status.
func TestListActiveWorkflowProjectionBeadsExcludesBlocked(t *testing.T) {
	const route = "/home/ds/gascity/polecat"
	// Blocked in bd, but every non-Live read decodes it as "open".
	blocked := beads.Bead{
		ID: "gc-nz5i", Title: "do-work", Type: "task", Status: "open",
		Metadata: map[string]string{"gc.routed_to": route},
	}
	ready := beads.Bead{
		ID: "gc-ready", Title: "ready", Type: "task", Status: "open",
		Metadata: map[string]string{"gc.routed_to": route},
	}
	claimed := beads.Bead{
		ID: "gc-claimed", Title: "claimed", Type: "task", Status: "in_progress",
		Assignee: route + "/th-abc", Metadata: map[string]string{"gc.run_target": route},
	}

	store := &collapsedStatusProjectionStore{
		Store:   beads.NewMemStoreFrom(0, nil, nil),
		rawScan: []beads.Bead{blocked, ready, claimed},
		liveByStatus: map[string][]beads.Bead{
			// bd's --status filter sees the raw status; gc-nz5i is blocked and absent.
			"open":        {ready},
			"in_progress": {claimed},
		},
	}

	got, err := listActiveWorkflowProjectionBeads(store)
	if err != nil {
		t.Fatalf("listActiveWorkflowProjectionBeads: %v", err)
	}
	ids := make(map[string]bool, len(got))
	for _, b := range got {
		ids[b.ID] = true
	}
	if ids["gc-nz5i"] {
		t.Errorf("blocked bead gc-nz5i reached the workflow projection; the spawn path routes on its gc.routed_to and burns a slot")
	}
	// The gate must not shrink the projection to open-only: in_progress work is
	// active and drives the running-run view.
	if !ids["gc-ready"] {
		t.Errorf("open routed bead gc-ready missing from projection")
	}
	if !ids["gc-claimed"] {
		t.Errorf("in_progress bead gc-claimed missing from projection")
	}
	if len(got) != 2 {
		t.Errorf("projection size = %d, want 2 (gc-ready, gc-claimed); got %v", len(got), ids)
	}
	// Every read must be Live and status-scoped, and in_progress must be read
	// before open: the two reads are not one snapshot, so this order confines
	// the missable flip to open->in_progress (a bead just claimed, which must
	// not be spawned against anyway).
	if want := strings.Join([]string{"in_progress", "open"}, ","); strings.Join(store.liveStatuses, ",") != want {
		t.Errorf("live status reads = %v, want [in_progress open] (status-scoped, in_progress first)", store.liveStatuses)
	}
}
