package reliability

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

func mustEncode(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// workerOp builds a worker.operation event for the supplied session with
// the given attributes. Used as test data for the reliability analyzer.
func workerOp(t *testing.T, seq uint64, ts time.Time, sessionID, model, version, agentName string) events.Event {
	t.Helper()
	return events.Event{
		Seq:     seq,
		Type:    eventWorkerOperation,
		Ts:      ts,
		Subject: sessionID,
		Payload: mustEncode(t, workerOperationPayload{
			SessionID:     sessionID,
			Model:         model,
			AgentName:     agentName,
			PromptVersion: version,
		}),
	}
}

func lifecycle(seq uint64, eventType, sessionID string, ts time.Time) events.Event {
	return events.Event{
		Seq:     seq,
		Type:    eventType,
		Ts:      ts,
		Subject: sessionID,
	}
}

func TestLifecycleKindString(t *testing.T) {
	cases := []struct {
		k    LifecycleKind
		want string
	}{
		{LifecycleCrashed, "crashed"},
		{LifecycleQuarantined, "quarantined"},
		{LifecycleIdleKilled, "idle_killed"},
		{LifecycleDraining, "draining"},
		{LifecycleUnknown, "unknown"},
		{LifecycleKind(99), "unknown"},
	}
	for _, tc := range cases {
		if got := tc.k.String(); got != tc.want {
			t.Errorf("LifecycleKind(%d).String() = %q, want %q", tc.k, got, tc.want)
		}
	}
}

func TestClassifyType(t *testing.T) {
	cases := []struct {
		in   string
		want LifecycleKind
	}{
		{"session.crashed", LifecycleCrashed},
		{"session.quarantined", LifecycleQuarantined},
		{"session.idle_killed", LifecycleIdleKilled},
		{"session.draining", LifecycleDraining},
		{"session.woke", LifecycleUnknown},
		{"worker.operation", LifecycleUnknown},
		{"", LifecycleUnknown},
	}
	for _, tc := range cases {
		if got := classifyType(tc.in); got != tc.want {
			t.Errorf("classifyType(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestSessionAttrsRig(t *testing.T) {
	cases := []struct {
		agentName string
		want      string
	}{
		{"rig/polecat-1", "rig"},
		{"myrig/polecat-2", "myrig"},
		{"mayor", ""},
		{"", ""},
		{"/orphan", ""}, // leading slash → no rig
	}
	for _, tc := range cases {
		got := SessionAttrs{AgentName: tc.agentName}.Rig()
		if got != tc.want {
			t.Errorf("Rig(%q) = %q, want %q", tc.agentName, got, tc.want)
		}
	}
}

func TestGroupRates(t *testing.T) {
	g := Group{Sessions: 100, Crashed: 5, UnhealthyTotal: 12}
	if got := g.CrashRate(); got != 0.05 {
		t.Errorf("CrashRate = %v, want 0.05", got)
	}
	if got := g.UnhealthyRate(); got != 0.12 {
		t.Errorf("UnhealthyRate = %v, want 0.12", got)
	}
	zero := Group{}
	if got := zero.CrashRate(); got != 0 {
		t.Errorf("CrashRate on empty group = %v, want 0", got)
	}
	if got := zero.UnhealthyRate(); got != 0 {
		t.Errorf("UnhealthyRate on empty group = %v, want 0", got)
	}
}

func TestWindowContains(t *testing.T) {
	t1 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)
	w := Window{Since: t1, Until: t3}
	if !w.Contains(t2) {
		t.Error("midpoint should be in window")
	}
	if w.Contains(t1.Add(-time.Second)) {
		t.Error("before-since should not be in window")
	}
	if w.Contains(t3.Add(time.Second)) {
		t.Error("after-until should not be in window")
	}

	// Zero bounds disable the check.
	open := Window{}
	if !open.Contains(t1) || !open.Contains(t3) {
		t.Error("zero window must accept everything")
	}
}

func TestAnalyzeBasicCorrelation(t *testing.T) {
	now := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	es := []events.Event{
		workerOp(t, 1, now, "sess-A", "claude-opus-4-7", "v3", "rigA/polecat-1"),
		workerOp(t, 2, now, "sess-B", "claude-sonnet-4-6", "v3", "rigA/polecat-2"),
		workerOp(t, 3, now, "sess-C", "claude-opus-4-7", "v2", "rigB/polecat-1"),
		lifecycle(4, eventSessionCrashed, "sess-A", now),
		lifecycle(5, eventSessionQuarantined, "sess-A", now),
		lifecycle(6, eventSessionCrashed, "sess-C", now),
	}
	r := Analyze(es, Window{}, Filter{})
	if len(r.Groups) != 3 {
		t.Fatalf("got %d groups, want 3", len(r.Groups))
	}
	// First group (sorted by unhealthy total desc): sess-A's group has 2 events.
	if r.Groups[0].UnhealthyTotal != 2 {
		t.Errorf("top group unhealthy = %d, want 2", r.Groups[0].UnhealthyTotal)
	}
	if r.Groups[0].Crashed != 1 || r.Groups[0].Quarantined != 1 {
		t.Errorf("top group counts: %+v", r.Groups[0])
	}
	if r.Total.UnhealthyTotal != 3 {
		t.Errorf("total unhealthy = %d, want 3", r.Total.UnhealthyTotal)
	}
}

func TestAnalyzeIgnoresUnknownEventTypes(t *testing.T) {
	now := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	es := []events.Event{
		workerOp(t, 1, now, "sess-A", "claude-opus-4-7", "v3", "rigA/polecat-1"),
		{Seq: 2, Type: "session.woke", Ts: now, Subject: "sess-A"},
		{Seq: 3, Type: "controller.started", Ts: now, Subject: "sess-A"},
		{Seq: 4, Type: "bead.created", Ts: now, Subject: "rigA-1"},
	}
	r := Analyze(es, Window{}, Filter{})
	if r.Total.UnhealthyTotal != 0 {
		t.Errorf("non-tracked types should not contribute: total=%d", r.Total.UnhealthyTotal)
	}
	// One worker.operation creates one session record.
	if len(r.Groups) != 1 || r.Groups[0].Sessions != 1 {
		t.Errorf("expected one group with 1 session, got %+v", r.Groups)
	}
}

func TestAnalyzeWindowBounds(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)
	es := []events.Event{
		workerOp(t, 1, t0, "old", "m", "v1", "rig/p"),
		workerOp(t, 2, t1, "mid", "m", "v1", "rig/p"),
		workerOp(t, 3, t2, "new", "m", "v1", "rig/p"),
		lifecycle(4, eventSessionCrashed, "old", t0),
		lifecycle(5, eventSessionCrashed, "mid", t1),
		lifecycle(6, eventSessionCrashed, "new", t2),
	}
	win := Window{
		Since: t1.Add(-time.Hour),
		Until: t2.Add(-time.Hour),
	}
	r := Analyze(es, win, Filter{})
	if r.Total.Crashed != 1 {
		t.Errorf("window should keep only 'mid', got %d crashes", r.Total.Crashed)
	}
}

func TestAnalyzeFilterByModel(t *testing.T) {
	now := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	es := []events.Event{
		workerOp(t, 1, now, "sA", "opus", "v1", "rig/a"),
		workerOp(t, 2, now, "sB", "sonnet", "v1", "rig/b"),
		lifecycle(3, eventSessionCrashed, "sA", now),
		lifecycle(4, eventSessionCrashed, "sB", now),
	}
	r := Analyze(es, Window{}, Filter{Model: "opus"})
	if r.Total.Crashed != 1 {
		t.Errorf("filter by model: total crashed = %d, want 1", r.Total.Crashed)
	}
	for _, g := range r.Groups {
		if g.Key.Model != "opus" {
			t.Errorf("filtered group has wrong model: %+v", g.Key)
		}
	}
}

func TestAnalyzeFilterByRig(t *testing.T) {
	now := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	es := []events.Event{
		workerOp(t, 1, now, "sA", "opus", "v1", "rigA/p1"),
		workerOp(t, 2, now, "sB", "opus", "v1", "rigB/p1"),
		lifecycle(3, eventSessionCrashed, "sA", now),
		lifecycle(4, eventSessionCrashed, "sB", now),
	}
	r := Analyze(es, Window{}, Filter{Rig: "rigB"})
	if r.Total.Crashed != 1 {
		t.Errorf("filter by rig: total crashed = %d, want 1", r.Total.Crashed)
	}
	for _, g := range r.Groups {
		if g.Key.Rig != "rigB" {
			t.Errorf("filtered group has wrong rig: %+v", g.Key)
		}
	}
}

func TestAnalyzeSkippedWhenNoAttrs(t *testing.T) {
	now := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	// Lifecycle event with no preceding worker.operation — no attributes
	// to group by. Shouldn't be silently bucketed under empty key.
	es := []events.Event{
		lifecycle(1, eventSessionCrashed, "lonely", now),
	}
	r := Analyze(es, Window{}, Filter{})
	if r.Skipped != 1 {
		t.Errorf("expected 1 skipped, got %d", r.Skipped)
	}
	if len(r.Groups) != 0 {
		t.Errorf("expected no groups, got %+v", r.Groups)
	}
}

func TestAnalyzeLatestAttrsWin(t *testing.T) {
	now := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	es := []events.Event{
		// Earlier op records old version.
		workerOp(t, 1, now, "sA", "opus", "v1", "rig/p"),
		// Later op (higher seq) records new version.
		workerOp(t, 5, now, "sA", "opus", "v2", "rig/p"),
		// Lifecycle event after — should attribute to v2.
		lifecycle(6, eventSessionCrashed, "sA", now),
	}
	r := Analyze(es, Window{}, Filter{})
	if len(r.Groups) != 1 {
		t.Fatalf("groups: %+v", r.Groups)
	}
	if r.Groups[0].Key.PromptVersion != "v2" {
		t.Errorf("latest version should win: got %q", r.Groups[0].Key.PromptVersion)
	}
}

func TestAnalyzeOutOfOrderEventsHandled(t *testing.T) {
	now := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	// Lifecycle event arrives BEFORE the worker.operation in the slice
	// but we walk in order. The two-pass design (build-attrs first, then
	// classify) makes this work regardless of iteration order.
	es := []events.Event{
		lifecycle(2, eventSessionCrashed, "sA", now),
		workerOp(t, 1, now, "sA", "opus", "v1", "rig/p"),
	}
	r := Analyze(es, Window{}, Filter{})
	if r.Total.Crashed != 1 {
		t.Errorf("out-of-order should still attribute: got %d", r.Total.Crashed)
	}
	if r.Skipped != 0 {
		t.Errorf("should not skip when attrs exist anywhere in stream: skipped=%d", r.Skipped)
	}
}

func TestAnalyzeSessionsCountedOnce(t *testing.T) {
	now := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	es := []events.Event{
		workerOp(t, 1, now, "sA", "opus", "v1", "rig/p"),
		// Same session, three lifecycle events.
		lifecycle(2, eventSessionCrashed, "sA", now),
		lifecycle(3, eventSessionQuarantined, "sA", now),
		lifecycle(4, eventSessionDraining, "sA", now),
	}
	r := Analyze(es, Window{}, Filter{})
	if len(r.Groups) != 1 {
		t.Fatalf("groups: %+v", r.Groups)
	}
	if r.Groups[0].Sessions != 1 {
		t.Errorf("Sessions should count distinct sessions: got %d", r.Groups[0].Sessions)
	}
	if r.Groups[0].UnhealthyTotal != 3 {
		t.Errorf("UnhealthyTotal counts events not sessions: got %d", r.Groups[0].UnhealthyTotal)
	}
}

func TestAnalyzeSessionsIncludeBenign(t *testing.T) {
	// A session that had a worker.operation but no lifecycle events
	// should still count toward total Sessions for its group — this is
	// the denominator side of crash-rate calculations.
	now := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	es := []events.Event{
		workerOp(t, 1, now, "sA", "opus", "v1", "rig/p"),
		workerOp(t, 2, now, "sB", "opus", "v1", "rig/p"),
		lifecycle(3, eventSessionCrashed, "sA", now),
	}
	r := Analyze(es, Window{}, Filter{})
	if len(r.Groups) != 1 {
		t.Fatalf("groups: %+v", r.Groups)
	}
	g := r.Groups[0]
	if g.Sessions != 2 {
		t.Errorf("Sessions = %d, want 2 (benign session counts)", g.Sessions)
	}
	if g.Crashed != 1 {
		t.Errorf("Crashed = %d, want 1", g.Crashed)
	}
	if got := g.CrashRate(); got != 0.5 {
		t.Errorf("CrashRate = %v, want 0.5", got)
	}
}

func TestAnalyzeSortStability(t *testing.T) {
	now := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	// Three groups with varying unhealthy totals.
	es := []events.Event{
		workerOp(t, 1, now, "s1", "modelA", "v1", "rigA/p"),
		workerOp(t, 2, now, "s2", "modelB", "v1", "rigB/p"),
		workerOp(t, 3, now, "s3", "modelC", "v1", "rigC/p"),
		lifecycle(4, eventSessionCrashed, "s1", now),
		lifecycle(5, eventSessionCrashed, "s2", now),
		lifecycle(6, eventSessionCrashed, "s2", now),
		lifecycle(7, eventSessionCrashed, "s3", now),
		lifecycle(8, eventSessionCrashed, "s3", now),
		lifecycle(9, eventSessionCrashed, "s3", now),
	}
	r := Analyze(es, Window{}, Filter{})
	if len(r.Groups) != 3 {
		t.Fatalf("expected 3 groups, got %d", len(r.Groups))
	}
	// Highest unhealthy comes first.
	if r.Groups[0].Key.Model != "modelC" {
		t.Errorf("expected modelC first, got %q", r.Groups[0].Key.Model)
	}
	if r.Groups[2].Key.Model != "modelA" {
		t.Errorf("expected modelA last, got %q", r.Groups[2].Key.Model)
	}
}

func TestAnalyzeMalformedPayloadIgnored(t *testing.T) {
	now := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	es := []events.Event{
		{
			Seq:     1,
			Type:    eventWorkerOperation,
			Ts:      now,
			Subject: "sA",
			Payload: json.RawMessage("not json"),
		},
		lifecycle(2, eventSessionCrashed, "sA", now),
	}
	r := Analyze(es, Window{}, Filter{})
	if r.Skipped != 1 {
		t.Errorf("expected to skip 1 (no attrs from malformed payload), got %d", r.Skipped)
	}
}

func TestAnalyzeEmptyInput(t *testing.T) {
	r := Analyze(nil, Window{}, Filter{})
	if len(r.Groups) != 0 {
		t.Error("empty input should produce zero groups")
	}
	if r.Total.Sessions != 0 {
		t.Error("empty input should produce zero total")
	}
}
