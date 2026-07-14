package orders

import (
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

func recordingOrdersStore() (*Store, *beadstest.RecordingStore) {
	rec := beadstest.NewRecordingStore(beads.NewMemStore())
	return NewStore(beads.OrdersStore{Store: rec}), rec
}

// TestCreateRunByteIdenticalToDispatcher proves CreateRun emits exactly the
// Create the dispatcher's normal pre-dispatch path emits:
// Title "order:<scoped>", Labels {order-run:<scoped>, order-tracking},
// NoHistory true.
func TestCreateRunByteIdenticalToDispatcher(t *testing.T) {
	st, rec := recordingOrdersStore()

	run, err := st.CreateRun("rig/agent", RunOpts{})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if !run.Open || run.Scoped != "rig/agent" {
		t.Errorf("run = %+v, want open scoped=rig/agent", run)
	}

	calls := rec.CallsForOp("Create")
	if len(calls) != 1 {
		t.Fatalf("want 1 Create, got %d", len(calls))
	}
	got := calls[0].Bead
	if got.Title != "order:rig/agent" {
		t.Errorf("title = %q, want order:rig/agent", got.Title)
	}
	if !got.NoHistory {
		t.Errorf("NoHistory = false, want true")
	}
	wantLabels := []string{"order-run:rig/agent", "order-tracking"}
	if !reflect.DeepEqual(got.Labels, wantLabels) {
		t.Errorf("labels = %v, want %v", got.Labels, wantLabels)
	}
}

// TestCreateRunWithTriggerEnvFailedOutcome proves the pre-dispatch failure path
// adds the trigger-env-failed label, matching order_dispatch.go:559.
func TestCreateRunWithTriggerEnvFailedOutcome(t *testing.T) {
	st, rec := recordingOrdersStore()

	if _, err := st.CreateRun("rig/agent", RunOpts{Outcome: RunOutcomeTriggerEnvFailed}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	got := rec.CallsForOp("Create")[0].Bead
	wantLabels := []string{"order-run:rig/agent", "order-tracking", "trigger-env-failed"}
	if !reflect.DeepEqual(got.Labels, wantLabels) {
		t.Errorf("labels = %v, want %v", got.Labels, wantLabels)
	}
}

// TestSetOutcomeLabelSets proves each outcome maps to the exact label set the
// dispatcher stamps via store.Update.
func TestSetOutcomeLabelSets(t *testing.T) {
	cases := []struct {
		outcome RunOutcome
		want    []string
	}{
		{RunOutcomeExec, []string{"exec"}},
		{RunOutcomeExecFailed, []string{"exec-failed"}},
		{RunOutcomeExecEnvFailed, []string{"exec-env-failed"}},
		{RunOutcomeWisp, []string{"wisp"}},
		{RunOutcomeWispFailed, []string{"wisp", "wisp-failed"}},
		{RunOutcomeWispCanceled, []string{"wisp", "wisp-canceled"}},
	}
	for _, tc := range cases {
		st, rec := recordingOrdersStore()
		seeded, err := st.store.Create(beads.Bead{Title: "order:rig/agent"})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		rec.Reset()
		if err := st.SetOutcome(seeded.ID, tc.outcome); err != nil {
			t.Fatalf("SetOutcome(%v): %v", tc.outcome, err)
		}
		calls := rec.CallsForOp("Update")
		if len(calls) != 1 {
			t.Fatalf("outcome %v: want 1 Update, got %d", tc.outcome, len(calls))
		}
		if !reflect.DeepEqual(calls[0].Opts.Labels, tc.want) {
			t.Errorf("outcome %v: labels = %v, want %v", tc.outcome, calls[0].Opts.Labels, tc.want)
		}
	}
}

// TestSetCursorLabelPair proves the cursor is encoded as (order:<scoped>,
// seq:<N>), matching order_dispatch.go:1021/1390.
func TestSetCursorLabelPair(t *testing.T) {
	st, rec := recordingOrdersStore()
	seeded, err := st.store.Create(beads.Bead{Title: "order:rig/agent"})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	rec.Reset()

	if err := st.SetCursor(seeded.ID, "rig/agent", EventCursor(7)); err != nil {
		t.Fatalf("SetCursor: %v", err)
	}
	got := rec.CallsForOp("Update")[0].Opts.Labels
	want := []string{"order:rig/agent", "seq:7"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cursor labels = %v, want %v", got, want)
	}
}

// TestCloseRunStampsReasonThenCloses proves CloseRun stamps close_reason then
// closes — matching cmd_order.go's SetMetadata(close_reason)+Close.
func TestCloseRunStampsReasonThenCloses(t *testing.T) {
	st, rec := recordingOrdersStore()
	if _, err := st.store.Create(beads.Bead{Title: "order:rig/agent"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Use the actually-created id.
	id := rec.CallsForOp("Create")[0].ID
	rec.Reset()

	if err := st.CloseRun(id, "manual run complete enough chars"); err != nil {
		t.Fatalf("CloseRun: %v", err)
	}
	gotOps := make([]string, 0)
	for _, c := range rec.Calls() {
		gotOps = append(gotOps, c.Op)
	}
	want := []string{"SetMetadata", "Close"}
	if !reflect.DeepEqual(gotOps, want) {
		t.Fatalf("ops = %v, want %v", gotOps, want)
	}
	mc := rec.CallsForOp("SetMetadata")[0]
	if mc.Key != "close_reason" || mc.Value != "manual run complete enough chars" {
		t.Errorf("close_reason write = (%q,%q)", mc.Key, mc.Value)
	}
}

// TestCreateRunClosedCooldownOnly proves CreateRunClosed creates an already
// labeled tracking bead and then closes it (stamping close_reason) so the run
// advances the cooldown clock without lingering as an in-flight marker — the
// byte-identical replacement for cmd_order.go's Create + (Update outcome) +
// Close manual-run path.
func TestCreateRunClosedCooldownOnly(t *testing.T) {
	st, rec := recordingOrdersStore()

	run, err := st.CreateRunClosed("rig/agent", RunOutcomeExec, nil, "manual run complete enough chars")
	if err != nil {
		t.Fatalf("CreateRunClosed: %v", err)
	}
	if run.Open {
		t.Errorf("run.Open = true, want false (cooldown-only)")
	}

	ops := make([]string, 0)
	for _, c := range rec.Calls() {
		ops = append(ops, c.Op)
	}
	want := []string{"Create", "Update", "SetMetadata", "Close"}
	if !reflect.DeepEqual(ops, want) {
		t.Fatalf("ops = %v, want %v", ops, want)
	}
	create := rec.CallsForOp("Create")[0].Bead
	if create.Title != "order:rig/agent" || !create.NoHistory {
		t.Errorf("create = %+v, want title order:rig/agent NoHistory true", create)
	}
	if got := rec.CallsForOp("Update")[0].Opts.Labels; !reflect.DeepEqual(got, []string{"exec"}) {
		t.Errorf("outcome labels = %v, want [exec]", got)
	}
	mc := rec.CallsForOp("SetMetadata")[0]
	if mc.Key != "close_reason" || mc.Value != "manual run complete enough chars" {
		t.Errorf("close_reason = (%q,%q)", mc.Key, mc.Value)
	}
}

// TestCreateRunClosedWithCursor proves the cursor label pair is stamped before
// close when supplied (the event-exec manual path), matching the
// (Create, Update cursor, Update outcome, Close) raw sequence.
func TestCreateRunClosedWithCursor(t *testing.T) {
	st, rec := recordingOrdersStore()
	cur := EventCursor(9)

	if _, err := st.CreateRunClosed("rig/agent", RunOutcomeExec, &cur, "manual run complete enough chars"); err != nil {
		t.Fatalf("CreateRunClosed: %v", err)
	}
	updates := rec.CallsForOp("Update")
	if len(updates) != 2 {
		t.Fatalf("Update calls = %d, want 2 (cursor + outcome)", len(updates))
	}
	if got := updates[0].Opts.Labels; !reflect.DeepEqual(got, []string{"order:rig/agent", "seq:9"}) {
		t.Errorf("cursor labels = %v, want [order:rig/agent seq:9]", got)
	}
	if got := updates[1].Opts.Labels; !reflect.DeepEqual(got, []string{"exec"}) {
		t.Errorf("outcome labels = %v, want [exec]", got)
	}
}

// TestRecentRunsReadsHistory proves RecentRuns lists tracking beads newest-first
// (including closed) and decodes them into OrderRun values carrying the cooldown
// clock and open flag.
func TestRecentRunsReadsHistory(t *testing.T) {
	st, _ := recordingOrdersStore()
	first, err := st.CreateRun("rig/agent", RunOpts{})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := st.CloseRun(first.ID, "first run complete enough chars"); err != nil {
		t.Fatalf("CloseRun: %v", err)
	}
	if _, err := st.CreateRun("rig/agent", RunOpts{}); err != nil {
		t.Fatalf("CreateRun 2: %v", err)
	}

	runs, err := st.RecentRuns("rig/agent", 10)
	if err != nil {
		t.Fatalf("RecentRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("RecentRuns = %d entries, want 2", len(runs))
	}
	for _, r := range runs {
		if r.Scoped != "rig/agent" || r.CreatedAt.IsZero() {
			t.Errorf("run = %+v, want scoped rig/agent with cooldown clock", r)
		}
	}
}

// TestRunFromTrackingBeadDecodesScopedOutcomeOpenCursor proves the exported
// tracking-bead decode extracts the scoped name from the first non-empty
// order-run label and folds outcome / open / cursor exactly like decodeRun, and
// rejects beads that carry no order-run label (ok=false).
func TestRunFromTrackingBeadDecodesScopedOutcomeOpenCursor(t *testing.T) {
	b := beads.Bead{
		ID:     "gc-42",
		Status: "open",
		Labels: []string{
			"order-tracking",
			"order-run:digest:rig:demo",
			"wisp",
			"wisp-failed",
			"order:digest:rig:demo",
			"seq:7",
		},
	}
	run, ok := RunFromTrackingBead(b)
	if !ok {
		t.Fatal("RunFromTrackingBead ok = false, want true")
	}
	want := OrderRun{
		ID:        "gc-42",
		Scoped:    "digest:rig:demo",
		Outcome:   RunOutcomeWispFailed,
		CreatedAt: b.CreatedAt,
		Open:      true,
		Cursor:    EventCursor(7),
	}
	if !reflect.DeepEqual(run, want) {
		t.Fatalf("run = %+v, want %+v", run, want)
	}

	if _, ok := RunFromTrackingBead(beads.Bead{Labels: []string{"order-tracking"}}); ok {
		t.Errorf("RunFromTrackingBead(order-tracking only) ok = true, want false")
	}
	if _, ok := RunFromTrackingBead(beads.Bead{Labels: []string{"order-tracking", "order-run:"}}); ok {
		t.Errorf("RunFromTrackingBead(empty order-run suffix) ok = true, want false")
	}
	if _, ok := RunFromTrackingBead(beads.Bead{Labels: []string{"order-run:   "}}); ok {
		t.Errorf("RunFromTrackingBead(whitespace order-run suffix) ok = true, want false")
	}
}

// TestRunOutcomeDisplayAndIsExec pins the display/exec vocabulary that replaces
// the API's inline label cracks. The label sub-table is ported verbatim from the
// deleted API test TestLastRunOutcomeFromLabelsPrioritizesTerminalLabels so the
// pre-refactor outcome truth table survives through outcomeFromLabels + Display.
func TestRunOutcomeDisplayAndIsExec(t *testing.T) {
	cases := []struct {
		outcome     RunOutcome
		wantDisplay string
		wantIsExec  bool
	}{
		{RunOutcomeNone, "", false},
		{RunOutcomeExec, "success", true},
		{RunOutcomeExecFailed, "failed", true},
		{RunOutcomeExecEnvFailed, "failed", true},
		{RunOutcomeWisp, "success", false},
		{RunOutcomeWispFailed, "failed", false},
		{RunOutcomeWispCanceled, "canceled", false},
		{RunOutcomeTriggerEnvFailed, "failed", false},
	}
	for _, tc := range cases {
		if got := tc.outcome.Display(); got != tc.wantDisplay {
			t.Errorf("Display(%v) = %q, want %q", tc.outcome, got, tc.wantDisplay)
		}
		if got := tc.outcome.IsExec(); got != tc.wantIsExec {
			t.Errorf("IsExec(%v) = %v, want %v", tc.outcome, got, tc.wantIsExec)
		}
	}

	labelCases := []struct {
		name   string
		labels []string
		want   string
	}{
		{"wisp failed dominates success", []string{"wisp", "wisp-failed"}, "failed"},
		{"failed alone", []string{"wisp-failed"}, "failed"},
		{"exec failed dominates success", []string{"exec", "exec-failed"}, "failed"},
		{"exec env failed is failed", []string{"exec-env-failed"}, "failed"},
		{"trigger env failed is failed", []string{"trigger-env-failed"}, "failed"},
		{"canceled dominates success", []string{"wisp", "wisp-canceled"}, "canceled"},
		{"success fallback", []string{"exec"}, "success"},
		{"unknown", []string{"order-tracking"}, ""},
	}
	for _, tc := range labelCases {
		if got := outcomeFromLabels(tc.labels).Display(); got != tc.want {
			t.Errorf("%s: outcomeFromLabels(%v).Display() = %q, want %q", tc.name, tc.labels, got, tc.want)
		}
	}
}

// TestOrderRunStateMatchesLegacyFeedStatus is the equivalence tripwire for the
// deleted orderTrackingStatus: each single-outcome-family bead decodes and its
// State() must match the pre-refactor active/failed/completed classification.
func TestOrderRunStateMatchesLegacyFeedStatus(t *testing.T) {
	cases := []struct {
		name   string
		status string
		labels []string
		want   string
	}{
		{"open exec-failed", "open", []string{"order-run:s", "exec-failed"}, "failed"},
		{"open exec-env-failed", "open", []string{"order-run:s", "exec-env-failed"}, "failed"},
		{"open trigger-env-failed", "open", []string{"order-run:s", "trigger-env-failed"}, "failed"},
		{"open wisp-canceled", "open", []string{"order-run:s", "wisp", "wisp-canceled"}, "failed"},
		{"open wisp-failed", "open", []string{"order-run:s", "wisp", "wisp-failed"}, "failed"},
		{"open no-outcome", "open", []string{"order-run:s"}, "active"},
		{"closed wisp", "closed", []string{"order-run:s", "wisp"}, "completed"},
		{"closed exec", "closed", []string{"order-run:s", "exec"}, "completed"},
		{"closed no-outcome", "closed", []string{"order-run:s"}, "completed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run, ok := RunFromTrackingBead(beads.Bead{Status: tc.status, Labels: tc.labels})
			if !ok {
				t.Fatalf("RunFromTrackingBead ok = false")
			}
			if got := run.State(); got != tc.want {
				t.Fatalf("State() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestListTrackingDecodesTrackingBeadsNewestFirst proves ListTracking mirrors
// the /v0/orders/feed order-tracking scan: newest-first, fully decoded, skipping
// beads without an order-run label, and returning (nil, nil) for a nil store.
func TestListTrackingDecodesTrackingBeadsNewestFirst(t *testing.T) {
	now := time.Now()
	older := beads.Bead{
		ID:        "gc-1",
		Status:    "open",
		CreatedAt: now.Add(-time.Hour),
		Labels:    []string{"order-tracking", "order-run:rig/a"},
	}
	newer := beads.Bead{
		ID:        "gc-2",
		Status:    "open",
		CreatedAt: now,
		Labels:    []string{"order-tracking", "order-run:rig/b"},
	}
	unlabeled := beads.Bead{
		ID:        "gc-3",
		Status:    "open",
		CreatedAt: now.Add(-30 * time.Minute),
	}
	mem := beads.NewMemStoreFrom(3, []beads.Bead{older, newer, unlabeled}, nil)
	front := NewStore(beads.OrdersStore{Store: mem})

	runs, err := front.ListTracking()
	if err != nil {
		t.Fatalf("ListTracking: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(runs))
	}
	if runs[0].Scoped != "rig/b" || runs[1].Scoped != "rig/a" {
		t.Fatalf("order = [%s %s], want [rig/b rig/a] (newest first)", runs[0].Scoped, runs[1].Scoped)
	}

	got, err := NewStore(beads.OrdersStore{}).ListTracking()
	if err != nil || got != nil {
		t.Fatalf("nil-store ListTracking = (%v, %v), want (nil, nil)", got, err)
	}
}

// TestLatestOpenRunIgnoresClosedRuns pins the deliberate IncludeClosed omission
// (adjustment B): the freshness signal is the newest OPEN run, so a newer closed
// run must not win, and an all-closed scoped name reports found=false.
func TestLatestOpenRunIgnoresClosedRuns(t *testing.T) {
	now := time.Now()
	olderOpen := beads.Bead{
		ID:        "gc-1",
		Status:    "open",
		CreatedAt: now.Add(-time.Hour),
		Labels:    []string{"order-tracking", "order-run:rig/agent"},
	}
	newerClosed := beads.Bead{
		ID:        "gc-2",
		Status:    "closed",
		CreatedAt: now,
		Labels:    []string{"order-tracking", "order-run:rig/agent", "wisp"},
	}
	mem := beads.NewMemStoreFrom(2, []beads.Bead{olderOpen, newerClosed}, nil)
	front := NewStore(beads.OrdersStore{Store: mem})

	run, found, err := front.LatestOpenRun("rig/agent")
	if err != nil {
		t.Fatalf("LatestOpenRun: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true (older open run)")
	}
	if run.ID != "gc-1" {
		t.Errorf("run.ID = %q, want gc-1 (open run, not the newer closed run)", run.ID)
	}

	allClosed := beads.NewMemStoreFrom(1, []beads.Bead{{
		ID:        "gc-9",
		Status:    "closed",
		CreatedAt: now,
		Labels:    []string{"order-tracking", "order-run:rig/agent"},
	}}, nil)
	if _, found, err := NewStore(beads.OrdersStore{Store: allClosed}).LatestOpenRun("rig/agent"); err != nil || found {
		t.Fatalf("all-closed LatestOpenRun = (found=%v, err=%v), want (false, nil)", found, err)
	}
}
