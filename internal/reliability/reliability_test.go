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
		Type:    events.WorkerOperation,
		Ts:      ts,
		Subject: sessionID,
		Payload: mustEncode(t, workerOperationPayload{
			SessionID:     sessionID,
			SessionName:   sessionID,
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

func crashedLifecycleWithPayload(t *testing.T, seq uint64, subject, sessionID string, ts time.Time) events.Event {
	t.Helper()
	return events.Event{
		Seq:     seq,
		Type:    events.SessionCrashed,
		Ts:      ts,
		Subject: subject,
		Payload: mustEncode(t, map[string]string{"session_id": sessionID}),
	}
}

func TestWorkerOperationPayloadDecodesCanonicalJSONSubset(t *testing.T) {
	raw := []byte(`{
		"session_id": "sess-123",
		"session_name": "rig-worker-1",
		"model": "claude-opus-4-7",
		"agent_name": "rig/worker-1",
		"prompt_version": "v3",
		"provider": "anthropic",
		"operation": "start",
		"result": "succeeded"
	}`)

	var p workerOperationPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal workerOperationPayload: %v", err)
	}
	if p.SessionID != "sess-123" {
		t.Fatalf("SessionID = %q, want sess-123", p.SessionID)
	}
	if p.SessionName != "rig-worker-1" {
		t.Fatalf("SessionName = %q, want rig-worker-1", p.SessionName)
	}
	if p.Model != "claude-opus-4-7" {
		t.Fatalf("Model = %q, want claude-opus-4-7", p.Model)
	}
	if p.AgentName != "rig/worker-1" {
		t.Fatalf("AgentName = %q, want rig/worker-1", p.AgentName)
	}
	if p.PromptVersion != "v3" {
		t.Fatalf("PromptVersion = %q, want v3", p.PromptVersion)
	}
	if p.Provider != "anthropic" {
		t.Fatalf("Provider = %q, want anthropic", p.Provider)
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
		{events.SessionCrashed, LifecycleCrashed},
		{events.SessionQuarantined, LifecycleQuarantined},
		{events.SessionIdleKilled, LifecycleIdleKilled},
		{events.SessionDraining, LifecycleDraining},
		{"session.woke", LifecycleUnknown},
		{events.WorkerOperation, LifecycleUnknown},
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
		{"rig/worker-1", "rig"},
		{"myrig/worker-2", "myrig"},
		{"coordinator", ""},
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
		workerOp(t, 1, now, "sess-A", "claude-opus-4-7", "v3", "rigA/worker-1"),
		workerOp(t, 2, now, "sess-B", "claude-sonnet-4-6", "v3", "rigA/worker-2"),
		workerOp(t, 3, now, "sess-C", "claude-opus-4-7", "v2", "rigB/worker-1"),
		lifecycle(4, events.SessionCrashed, "sess-A", now),
		lifecycle(5, events.SessionQuarantined, "sess-A", now),
		lifecycle(6, events.SessionCrashed, "sess-C", now),
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
		workerOp(t, 1, now, "sess-A", "claude-opus-4-7", "v3", "rigA/worker-1"),
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
		lifecycle(4, events.SessionCrashed, "old", t0),
		lifecycle(5, events.SessionCrashed, "mid", t1),
		lifecycle(6, events.SessionCrashed, "new", t2),
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
		lifecycle(3, events.SessionCrashed, "sA", now),
		lifecycle(4, events.SessionCrashed, "sB", now),
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
		lifecycle(3, events.SessionCrashed, "sA", now),
		lifecycle(4, events.SessionCrashed, "sB", now),
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
		lifecycle(1, events.SessionCrashed, "lonely", now),
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
		lifecycle(6, events.SessionCrashed, "sA", now),
	}
	r := Analyze(es, Window{}, Filter{})
	if len(r.Groups) != 1 {
		t.Fatalf("groups: %+v", r.Groups)
	}
	if r.Groups[0].Key.PromptVersion != "v2" {
		t.Errorf("latest version should win: got %q", r.Groups[0].Key.PromptVersion)
	}
}

func TestAnalyzeLifecycleUsesLatestAttrsAtOrBeforeEvent(t *testing.T) {
	now := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	es := []events.Event{
		workerOp(t, 1, now, "sA", "opus", "v1", "rig/worker"),
		lifecycle(2, events.SessionCrashed, "sA", now.Add(time.Minute)),
		workerOp(t, 3, now.Add(2*time.Hour), "sA", "opus", "v2", "rig/worker"),
	}
	r := Analyze(es, Window{Since: now.Add(-time.Hour), Until: now.Add(time.Hour)}, Filter{})
	if len(r.Groups) != 1 {
		t.Fatalf("groups: %+v", r.Groups)
	}
	if r.Groups[0].Key.PromptVersion != "v1" {
		t.Fatalf("crash attributed to version %q, want v1", r.Groups[0].Key.PromptVersion)
	}
}

func TestAnalyzeWindowAttributionMatrix(t *testing.T) {
	base := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	win := Window{Since: base.Add(-time.Hour), Until: base.Add(time.Hour)}
	cases := []struct {
		name           string
		events         []events.Event
		wantCrashed    int
		wantSkipped    int
		wantAmbiguous  int
		wantVersion    string
		wantTotalGroup int
	}{
		{
			name: "worker operation before window still attributes lifecycle in window",
			events: []events.Event{
				workerOp(t, 1, base.Add(-2*time.Hour), "sA", "opus", "before-window", "rig/worker"),
				lifecycle(2, events.SessionCrashed, "sA", base),
			},
			wantCrashed:    1,
			wantVersion:    "before-window",
			wantTotalGroup: 1,
		},
		{
			name: "worker operation after lifecycle is skipped",
			events: []events.Event{
				lifecycle(1, events.SessionCrashed, "sA", base),
				workerOp(t, 2, base.Add(time.Minute), "sA", "opus", "after-lifecycle", "rig/worker"),
			},
			wantSkipped: 1,
		},
		{
			name: "post-window worker operation does not rebucket lifecycle",
			events: []events.Event{
				workerOp(t, 1, base.Add(-2*time.Hour), "sA", "opus", "before-window", "rig/worker"),
				lifecycle(2, events.SessionCrashed, "sA", base),
				workerOp(t, 3, base.Add(2*time.Hour), "sA", "opus", "after-window", "rig/worker"),
			},
			wantCrashed:    1,
			wantVersion:    "before-window",
			wantTotalGroup: 1,
		},
		{
			name: "cross-rig display alias is ambiguous only after second rig appears",
			events: []events.Event{
				workerOp(t, 1, base, "sA", "opus", "rigA-version", "rigA/worker-1"),
				lifecycle(2, events.SessionCrashed, "worker-1", base.Add(time.Minute)),
				workerOp(t, 3, base.Add(2*time.Minute), "sB", "sonnet", "rigB-version", "rigB/worker-1"),
				lifecycle(4, events.SessionCrashed, "worker-1", base.Add(3*time.Minute)),
			},
			wantCrashed:   1,
			wantAmbiguous: 1,
			wantVersion:   "rigA-version",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Analyze(tc.events, win, Filter{})
			if r.Total.Crashed != tc.wantCrashed {
				t.Fatalf("crashed = %d, want %d; report=%+v", r.Total.Crashed, tc.wantCrashed, r)
			}
			if r.Skipped != tc.wantSkipped {
				t.Fatalf("skipped = %d, want %d", r.Skipped, tc.wantSkipped)
			}
			if r.AmbiguousAliases != tc.wantAmbiguous {
				t.Fatalf("ambiguous aliases = %d, want %d", r.AmbiguousAliases, tc.wantAmbiguous)
			}
			if tc.wantVersion != "" {
				if len(r.Groups) == 0 {
					t.Fatalf("groups = %+v, want version %q", r.Groups, tc.wantVersion)
				}
				if r.Groups[0].Key.PromptVersion != tc.wantVersion {
					t.Fatalf("version = %q, want %q", r.Groups[0].Key.PromptVersion, tc.wantVersion)
				}
			}
			if tc.wantTotalGroup != 0 && len(r.Groups) != tc.wantTotalGroup {
				t.Fatalf("groups = %+v, want %d group(s)", r.Groups, tc.wantTotalGroup)
			}
		})
	}
}

func TestAnalyzeSkipsLifecycleBeforeFirstWorkerOperation(t *testing.T) {
	now := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	es := []events.Event{
		lifecycle(1, events.SessionCrashed, "sA", now),
		workerOp(t, 2, now.Add(time.Minute), "sA", "opus", "v1", "rig/worker"),
	}
	r := Analyze(es, Window{}, Filter{})
	if r.Total.Crashed != 0 {
		t.Fatalf("crashed = %d, want 0", r.Total.Crashed)
	}
	if r.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1", r.Skipped)
	}
}

func TestAnalyzeOutOfOrderEventsHandled(t *testing.T) {
	now := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	// Lifecycle event arrives BEFORE the worker.operation in the slice
	// but we walk in order. The two-pass design (build-attrs first, then
	// classify) makes this work regardless of iteration order.
	es := []events.Event{
		lifecycle(2, events.SessionCrashed, "sA", now),
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
		lifecycle(2, events.SessionCrashed, "sA", now),
		lifecycle(3, events.SessionQuarantined, "sA", now),
		lifecycle(4, events.SessionDraining, "sA", now),
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

func TestAnalyzeTotalSessionsDedupesSessionAcrossAttributeChanges(t *testing.T) {
	now := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	es := []events.Event{
		workerOp(t, 1, now, "sA", "opus", "v1", "rig/worker"),
		lifecycle(2, events.SessionCrashed, "sA", now.Add(time.Minute)),
		workerOp(t, 3, now.Add(2*time.Minute), "sA", "opus", "v2", "rig/worker"),
	}

	r := Analyze(es, Window{}, Filter{})

	if len(r.Groups) != 2 {
		t.Fatalf("groups = %+v, want crash group plus latest denominator group", r.Groups)
	}
	if r.Total.Sessions != 1 {
		t.Fatalf("total sessions = %d, want one unique session", r.Total.Sessions)
	}
	if r.Total.Crashed != 1 {
		t.Fatalf("total crashed = %d, want 1", r.Total.Crashed)
	}
	if got := r.Total.CrashRate(); got != 1 {
		t.Fatalf("total crash rate = %v, want 1", got)
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
		lifecycle(3, events.SessionCrashed, "sA", now),
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
		lifecycle(4, events.SessionCrashed, "s1", now),
		lifecycle(5, events.SessionCrashed, "s2", now),
		lifecycle(6, events.SessionCrashed, "s2", now),
		lifecycle(7, events.SessionCrashed, "s3", now),
		lifecycle(8, events.SessionCrashed, "s3", now),
		lifecycle(9, events.SessionCrashed, "s3", now),
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
			Type:    events.WorkerOperation,
			Ts:      now,
			Subject: "sA",
			Payload: json.RawMessage("not json"),
		},
		lifecycle(2, events.SessionCrashed, "sA", now),
	}
	r := Analyze(es, Window{}, Filter{})
	if r.Skipped != 1 {
		t.Errorf("expected to skip 1 (no attrs from malformed payload), got %d", r.Skipped)
	}
}

func TestAnalyzeCorrelatesLifecycleSubjectByWorkerOperationAlias(t *testing.T) {
	now := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	es := []events.Event{
		{
			Seq:     1,
			Type:    events.WorkerOperation,
			Ts:      now,
			Subject: "session-uuid",
			Payload: mustEncode(t, workerOperationPayload{
				SessionID:     "session-uuid",
				SessionName:   "worker-1",
				Model:         "opus",
				AgentName:     "rigA/workflows.worker",
				PromptVersion: "v1",
			}),
		},
		lifecycle(2, events.SessionCrashed, "worker-1", now),
	}

	r := Analyze(es, Window{}, Filter{})

	if r.Skipped != 0 {
		t.Fatalf("producer-shaped lifecycle subject should not be skipped: %d", r.Skipped)
	}
	if r.Total.Crashed != 1 {
		t.Fatalf("crashed = %d, want 1", r.Total.Crashed)
	}
	if len(r.Groups) != 1 || r.Groups[0].Sessions != 1 {
		t.Fatalf("groups = %+v, want one session in one group", r.Groups)
	}
}

func TestAnalyzeCorrelatesProducerDisplayNameThroughAgentAlias(t *testing.T) {
	now := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	es := []events.Event{
		{
			Seq:     1,
			Type:    events.WorkerOperation,
			Ts:      now,
			Subject: "session-uuid",
			Payload: mustEncode(t, workerOperationPayload{
				SessionID:     "session-uuid",
				SessionName:   "city-rigA-worker-1",
				Model:         "opus",
				AgentName:     "rigA/worker-1",
				PromptVersion: "v1",
			}),
		},
		lifecycle(2, events.SessionCrashed, "worker-1", now),
	}

	r := Analyze(es, Window{}, Filter{})

	if r.Skipped != 0 {
		t.Fatalf("producer display-name subject should not be skipped: %d", r.Skipped)
	}
	if r.Total.Crashed != 1 {
		t.Fatalf("crashed = %d, want 1", r.Total.Crashed)
	}
	var found bool
	for _, g := range r.Groups {
		if g.Key.Rig == "rigA" && g.Crashed == 1 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("groups = %+v, want crash attributed to rigA", r.Groups)
	}
}

func TestAnalyzePrefersLifecyclePayloadSessionIDOverAmbiguousSubject(t *testing.T) {
	now := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	es := []events.Event{
		{
			Seq:     1,
			Type:    events.WorkerOperation,
			Ts:      now,
			Subject: "session-A",
			Payload: mustEncode(t, workerOperationPayload{
				SessionID:     "session-A",
				SessionName:   "city-rigA-worker-1",
				Model:         "opus",
				AgentName:     "rigA/worker-1",
				PromptVersion: "v1",
			}),
		},
		{
			Seq:     2,
			Type:    events.WorkerOperation,
			Ts:      now,
			Subject: "session-B",
			Payload: mustEncode(t, workerOperationPayload{
				SessionID:     "session-B",
				SessionName:   "city-rigB-worker-1",
				Model:         "sonnet",
				AgentName:     "rigB/worker-1",
				PromptVersion: "v1",
			}),
		},
		crashedLifecycleWithPayload(t, 3, "worker-1", "session-A", now),
	}

	r := Analyze(es, Window{}, Filter{})

	if r.AmbiguousAliases != 0 {
		t.Fatalf("payload session_id should bypass ambiguous subject alias, ambiguous=%d", r.AmbiguousAliases)
	}
	if r.Skipped != 0 {
		t.Fatalf("payload session_id should not be skipped, skipped=%d", r.Skipped)
	}
	if r.Total.Crashed != 1 {
		t.Fatalf("crashed = %d, want 1", r.Total.Crashed)
	}
	var found bool
	for _, g := range r.Groups {
		if g.Key.Rig == "rigA" && g.Crashed == 1 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("groups = %+v, want crash attributed to rigA", r.Groups)
	}
}

func TestAnalyzeReportsAmbiguousDisplayNameAlias(t *testing.T) {
	now := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	es := []events.Event{
		{
			Seq:     1,
			Type:    events.WorkerOperation,
			Ts:      now,
			Subject: "session-A",
			Payload: mustEncode(t, workerOperationPayload{
				SessionID:     "session-A",
				SessionName:   "city-rigA-worker-1",
				Model:         "opus",
				AgentName:     "rigA/worker-1",
				PromptVersion: "v1",
			}),
		},
		{
			Seq:     2,
			Type:    events.WorkerOperation,
			Ts:      now,
			Subject: "session-B",
			Payload: mustEncode(t, workerOperationPayload{
				SessionID:     "session-B",
				SessionName:   "city-rigB-worker-1",
				Model:         "sonnet",
				AgentName:     "rigB/worker-1",
				PromptVersion: "v1",
			}),
		},
		lifecycle(3, events.SessionCrashed, "worker-1", now),
	}

	r := Analyze(es, Window{}, Filter{})

	if r.Total.Crashed != 0 {
		t.Fatalf("ambiguous display-name crash should not be attributed: total=%+v", r.Total)
	}
	if r.AmbiguousAliases != 1 {
		t.Fatalf("ambiguous aliases = %d, want 1", r.AmbiguousAliases)
	}
}

func TestAnalyzeCorrelatesRepeatedDisplayNameThroughLatestSameAgent(t *testing.T) {
	now := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	es := []events.Event{
		{
			Seq:     1,
			Type:    events.WorkerOperation,
			Ts:      now,
			Subject: "session-A",
			Payload: mustEncode(t, workerOperationPayload{
				SessionID:     "session-A",
				SessionName:   "city-rig-worker-1-a",
				Model:         "opus",
				AgentName:     "rig/worker-1",
				PromptVersion: "v1",
			}),
		},
		lifecycle(2, events.SessionCrashed, "worker-1", now.Add(time.Minute)),
		{
			Seq:     3,
			Type:    events.WorkerOperation,
			Ts:      now.Add(2 * time.Minute),
			Subject: "session-B",
			Payload: mustEncode(t, workerOperationPayload{
				SessionID:     "session-B",
				SessionName:   "city-rig-worker-1-b",
				Model:         "sonnet",
				AgentName:     "rig/worker-1",
				PromptVersion: "v2",
			}),
		},
		lifecycle(4, events.SessionCrashed, "worker-1", now.Add(3*time.Minute)),
	}

	r := Analyze(es, Window{}, Filter{})

	if r.AmbiguousAliases != 0 {
		t.Fatalf("same-agent display-name reuse should not be ambiguous: %d", r.AmbiguousAliases)
	}
	if r.Total.Crashed != 2 {
		t.Fatalf("crashed = %d, want both reused display-name crashes attributed", r.Total.Crashed)
	}
	if len(r.Groups) != 2 {
		t.Fatalf("groups = %+v, want separate groups for v1 and v2", r.Groups)
	}
}

func TestAnalyzeInstrumentationCountsMissingModelAndPromptVersion(t *testing.T) {
	now := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	es := []events.Event{
		workerOp(t, 1, now, "complete", "opus", "v1", "rig/worker-1"),
		workerOp(t, 2, now, "missing-model", "", "v1", "rig/worker-2"),
		workerOp(t, 3, now, "missing-version", "sonnet", "", "rig/worker-3"),
		workerOp(t, 4, now.Add(-48*time.Hour), "outside-window", "", "", "rig/worker-4"),
	}

	r := Analyze(es, Window{Since: now.Add(-time.Hour)}, Filter{})

	if got := r.Instrumentation.WorkerOperations; got != 3 {
		t.Fatalf("worker operations = %d, want 3", got)
	}
	if got := r.Instrumentation.MissingModel; got != 1 {
		t.Fatalf("missing model = %d, want 1", got)
	}
	if got := r.Instrumentation.MissingPromptVersion; got != 1 {
		t.Fatalf("missing prompt version = %d, want 1", got)
	}
	if got := r.Instrumentation.QuarantineSignalStatus; got != quarantineSignalStatusNotEmitted {
		t.Fatalf("quarantine signal status = %q, want %q", got, quarantineSignalStatusNotEmitted)
	}
}

func TestAnalyzeInstrumentationReportsObservedQuarantineSignal(t *testing.T) {
	now := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	es := []events.Event{
		workerOp(t, 1, now, "complete", "opus", "v1", "rig/worker-1"),
		lifecycle(2, events.SessionQuarantined, "complete", now),
	}

	r := Analyze(es, Window{Since: now.Add(-time.Hour)}, Filter{})

	if got := r.Instrumentation.QuarantineSignalStatus; got != quarantineSignalStatusObserved {
		t.Fatalf("quarantine signal status = %q, want %q", got, quarantineSignalStatusObserved)
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
