package t3bridge

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// turnWithLatest is a snapshot in which turnFenceSession's thread has session
// and latestTurn set verbatim; a nil latest leaves latestTurn null.
func turnWithLatest(session, latest map[string]interface{}) map[string]interface{} {
	snapshot := turnSnapshot(session)
	thread := snapshot["threads"].([]interface{})[0].(map[string]interface{})
	thread["latestTurn"] = nil
	if latest != nil {
		thread["latestTurn"] = latest
	}
	return snapshot
}

// latestTurn is a T3 V1 latestTurn of id in state, requested at requested and
// completed at completed (null when empty).
func latestTurn(id, state, requested, completed string) map[string]interface{} {
	turn := map[string]interface{}{"turnId": id, "state": state, "requestedAt": requested, "completedAt": nil}
	if completed != "" {
		turn["completedAt"] = completed
	}
	return turn
}

func mustParseT3Time(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// Kills a declaration that drifts from X11 §5.1's T3 V1 row (End inferred,
// as ruled on 2026-10-20), varies by session, or is lost through the
// seam-backed provider.
func TestTurnCapabilitiesAreV1sStaticRow(t *testing.T) {
	want := runtime.TurnCapabilities{
		Start:    runtime.TurnStartRecorded,
		End:      runtime.TurnEndInferred,
		Fence:    runtime.TurnFenceChecked,
		Link:     runtime.TurnLinkCorrelated,
		Lossless: false,
	}
	p := NewProvider()
	for _, name := range []string{turnFenceSession, "unbound", ""} {
		if got := p.TurnCapabilities(name); got != want {
			t.Errorf("TurnCapabilities(%q) = %+v, want %+v", name, got, want)
		}
	}
	if got := runtime.TurnCapabilitiesFor(NewSeamBacked(), turnFenceSession); got != want.Normalized() {
		t.Fatalf("TurnCapabilitiesFor(seam-backed) = %+v, want %+v", got, want.Normalized())
	}
}

// Kills a history that reads the wrong turn as current, drops its start,
// reports a running or absent latestTurn as ended, maps an end to the wrong
// status, or names a turn differently from ObserveTurn.
func TestTurnHistoryReadsTheFreshSnapshot(t *testing.T) {
	const (
		requested = "2026-10-16T12:00:00.123Z"
		completed = "2026-10-16T12:00:05.456Z"
	)
	running := func(id string) map[string]interface{} {
		return map[string]interface{}{"status": "running", "activeTurnId": id}
	}
	ready := map[string]interface{}{"status": "ready", "activeTurnId": nil}
	ended := func(status runtime.TurnStatus, id string) []runtime.EndedTurn {
		return []runtime.EndedTurn{{
			TurnID:    "thread-agent-a/" + id,
			StartedAt: mustParseT3Time(t, requested),
			EndedAt:   mustParseT3Time(t, completed),
			Status:    status,
		}}
	}
	cases := []struct {
		name      string
		session   map[string]interface{}
		latest    map[string]interface{}
		current   string
		start     string
		recent    []runtime.EndedTurn
		noLatestK bool
	}{
		{name: "no provider session"},
		{name: "no provider session or latestTurn key", noLatestK: true},
		{name: "absent status with a stale id", session: map[string]interface{}{"status": "stopped", "activeTurnId": "t1"}},
		{name: "ready with a null id", session: ready},
		{name: "running the latest turn", session: running("t2"), latest: latestTurn("t2", "running", requested, ""), current: "thread-agent-a/t2", start: requested},
		{name: "running with no latestTurn", session: running("t2"), current: "thread-agent-a/t2"},
		// T3 marks the active turn completed at its first finalized assistant
		// message while the session keeps running it (fork F,
		// ProjectionPipeline.ts thread.message-sent).
		{name: "running a turn T3 already marks completed", session: running("t2"), latest: latestTurn("t2", "completed", requested, completed), current: "thread-agent-a/t2", start: requested},
		// T3 marks the active turn interrupted when a turn-scoped interrupt is
		// requested, before the session stops running it.
		{name: "running a turn T3 already marks interrupted", session: running("t2"), latest: latestTurn("t2", "interrupted", requested, ""), current: "thread-agent-a/t2", start: requested},
		{name: "running a turn T3 already marks error", session: running("t2"), latest: latestTurn("t2", "error", requested, completed), current: "thread-agent-a/t2", start: requested},
		// runtime.error with a turn id leaves an error session holding a stale
		// activeTurnId that latestTurn names.
		{name: "error status with a stale id naming an ended latestTurn", session: map[string]interface{}{"status": "error", "activeTurnId": "t1"}, latest: latestTurn("t1", "completed", requested, completed), recent: ended(runtime.TurnStatusCompleted, "t1")},
		{name: "running past an older ended turn", session: running("t2"), latest: latestTurn("t1", "completed", requested, completed), current: "thread-agent-a/t2", recent: ended(runtime.TurnStatusCompleted, "t1")},
		{name: "idle after completed", session: ready, latest: latestTurn("t1", "completed", requested, completed), recent: ended(runtime.TurnStatusCompleted, "t1")},
		{name: "idle after interrupted", session: ready, latest: latestTurn("t1", "interrupted", requested, completed), recent: ended(runtime.TurnStatusCancelled, "t1")},
		{name: "idle after error", session: ready, latest: latestTurn("t1", "error", requested, completed), recent: ended(runtime.TurnStatusFailed, "t1")},
		{
			name: "idle after an end with no completedAt", session: ready, latest: latestTurn("t1", "completed", requested, ""),
			recent: []runtime.EndedTurn{{TurnID: "thread-agent-a/t1", StartedAt: mustParseT3Time(t, requested), Status: runtime.TurnStatusCompleted}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := turnWithLatest(tc.session, tc.latest)
			if tc.noLatestK {
				delete(snapshot["threads"].([]interface{})[0].(map[string]interface{}), "latestTurn")
			}
			server := newTurnFenceServer(t, snapshot)
			p := newTurnFenceTestProvider(t, server)
			before := time.Now()
			h, err := p.TurnHistory(t.Context(), turnFenceSession)
			if err != nil {
				t.Fatalf("TurnHistory: %v", err)
			}
			if h.Current.TurnID != tc.current {
				t.Errorf("Current = %q, want %q", h.Current.TurnID, tc.current)
			}
			var wantStart time.Time
			if tc.start != "" {
				wantStart = mustParseT3Time(t, tc.start)
			}
			if !h.CurrentStart.Equal(wantStart) {
				t.Errorf("CurrentStart = %v, want %v", h.CurrentStart, wantStart)
			}
			if len(h.CurrentKeys) != 0 {
				t.Errorf("CurrentKeys = %v, want none: V1 never fills a message's turnId", h.CurrentKeys)
			}
			if !reflect.DeepEqual(h.Recent, tc.recent) {
				t.Errorf("Recent = %+v, want %+v", h.Recent, tc.recent)
			}
			if h.ObservedAt.Before(before) {
				t.Errorf("ObservedAt = %v, before the read began at %v", h.ObservedAt, before)
			}
			st, err := p.ObserveTurn(t.Context(), turnFenceSession)
			if err != nil || st.TurnID != h.Current.TurnID {
				t.Errorf("ObserveTurn = %+v, %v; want the history's current %q", st, err, h.Current.TurnID)
			}
		})
	}
}

// Kills a history read that calls an indeterminate snapshot idle (I8):
// every case must fail with ErrRuntimeUnavailable.
func TestTurnHistoryIndeterminateIsUnavailable(t *testing.T) {
	const requested = "2026-10-16T12:00:00.123Z"
	running := map[string]interface{}{"status": "running", "activeTurnId": "t2"}
	ready := map[string]interface{}{"status": "ready", "activeTurnId": nil}
	longID := strings.Repeat("x", runtime.MaxTurnIDLen)
	cases := []struct {
		name    string
		session map[string]interface{}
		latest  interface{}
	}{
		{name: "running latestTurn while idle", session: ready, latest: latestTurn("t1", "running", requested, "")},
		{name: "running latestTurn that is not the active turn", session: running, latest: latestTurn("t1", "running", requested, "")},
		// F's runtime.error shape: an error session keeps a stale activeTurnId
		// whose row stays running until the next session-set.
		{name: "error status with a stale id naming a running latestTurn", session: map[string]interface{}{"status": "error", "activeTurnId": "t1"}, latest: latestTurn("t1", "running", requested, "")},
		{name: "unknown latestTurn state", session: ready, latest: latestTurn("t1", "future-state", requested, "")},
		{name: "latestTurn not an object", session: ready, latest: "t1"},
		{name: "latestTurn without a turnId", session: ready, latest: map[string]interface{}{"state": "completed", "requestedAt": requested}},
		{name: "latestTurn with a numeric turnId", session: ready, latest: map[string]interface{}{"turnId": 7, "state": "completed", "requestedAt": requested}},
		{name: "latestTurn without requestedAt", session: ready, latest: map[string]interface{}{"turnId": "t1", "state": "completed"}},
		{name: "unparseable requestedAt", session: ready, latest: latestTurn("t1", "completed", "yesterday", "")},
		{name: "unparseable completedAt", session: ready, latest: latestTurn("t1", "completed", requested, "later")},
		{name: "numeric completedAt", session: ready, latest: map[string]interface{}{"turnId": "t1", "state": "completed", "requestedAt": requested, "completedAt": 5}},
		{name: "over-long current id", session: map[string]interface{}{"status": "running", "activeTurnId": longID}},
		{name: "over-long ended id", session: ready, latest: latestTurn(longID, "completed", requested, "")},
		{name: "control character in the ended id", session: ready, latest: latestTurn("t\x001", "completed", requested, "")},
		{name: "running without an active id", session: map[string]interface{}{"status": "running", "activeTurnId": nil}},
		{name: "unknown session status", session: map[string]interface{}{"status": "future-state", "activeTurnId": "t1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := turnSnapshot(tc.session)
			snapshot["threads"].([]interface{})[0].(map[string]interface{})["latestTurn"] = tc.latest
			server := newTurnFenceServer(t, snapshot)
			p := newTurnFenceTestProvider(t, server)
			h, err := p.TurnHistory(t.Context(), turnFenceSession)
			if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("TurnHistory = %+v, %v; want ErrRuntimeUnavailable", h, err)
			}
			if !reflect.DeepEqual(h, runtime.TurnHistory{}) {
				t.Fatalf("TurnHistory = %+v alongside %v; want the zero history", h, err)
			}
		})
	}
}

// Kills a history read that reports a missing, unreachable or still-starting
// session as idle, or calls a session not yet visible gone.
func TestTurnHistoryErrorsPassThrough(t *testing.T) {
	t.Run("unbound", func(t *testing.T) {
		server := newTurnFenceServer(t, map[string]interface{}{"threads": []interface{}{}})
		p := newTurnFenceTestProvider(t, server)
		if _, err := p.TurnHistory(t.Context(), turnFenceSession); !errors.Is(err, runtime.ErrSessionNotFound) {
			t.Fatalf("unbound = %v, want ErrSessionNotFound", err)
		}
	})
	t.Run("inside the start grace", func(t *testing.T) {
		server := newTurnFenceServer(t, map[string]interface{}{"threads": []interface{}{}})
		p := newTurnFenceTestProvider(t, server)
		p.setRecentStart(turnFenceSession, time.Now())
		_, err := p.TurnHistory(t.Context(), turnFenceSession)
		if !errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, runtime.ErrSessionNotFound) {
			t.Fatalf("inside the start grace = %v, want only ErrRuntimeUnavailable", err)
		}
	})
	t.Run("no snapshot", func(t *testing.T) {
		server := newTurnFenceServer(t, nil)
		p := newTurnFenceTestProvider(t, server)
		if _, err := p.TurnHistory(t.Context(), turnFenceSession); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("snapshot outage = %v, want ErrRuntimeUnavailable", err)
		}
	})
	t.Run("ctx ended", func(t *testing.T) {
		server := newTurnFenceServer(t)
		server.setStalled()
		p := newTurnFenceTestProvider(t, server)
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()
		_, err := p.TurnHistory(ctx, turnFenceSession)
		if !errors.Is(err, runtime.ErrRuntimeUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("stalled = %v, want ErrRuntimeUnavailable wrapping the deadline", err)
		}
	})
}

// Kills a history read served from the 10 s cache.
func TestTurnHistoryIsUncached(t *testing.T) {
	server := newTurnFenceServer(t, turnIdle(), turnRunning("t1"), turnRunning("t2"))
	p := newTurnFenceTestProvider(t, server)
	if _, err := p.rpcSnapshot(); err != nil {
		t.Fatalf("warming the cache: %v", err)
	}
	for i, want := range []string{"thread-agent-a/t1", "thread-agent-a/t2"} {
		h, err := p.TurnHistory(t.Context(), turnFenceSession)
		if err != nil || h.Current.TurnID != want {
			t.Fatalf("read %d = %+v, %v; want current %q", i+1, h, err, want)
		}
	}
	if calls := server.snapshotCalls(); calls != 3 {
		t.Fatalf("snapshot reads = %d, want 3: each history read takes its own", calls)
	}
}

// Kills a start time that loses the precision CorrelateTurn's requestedAt =
// createdAt rule (DES-O1 §4.8 row 3) depends on: a requestedAt equal to the
// createdAt the bridge stamped on thread.turn.start must read as that same
// instant.
func TestTurnHistoryStartEqualsTheTurnStartsCreatedAt(t *testing.T) {
	server := newTurnFenceServer(t, turnIdle())
	p := newTurnFenceTestProvider(t, server)
	if err := p.dispatchTurnStart("thread-agent-a", "hello", "codex", "gpt-5", p.freshTurnStartIDs()); err != nil {
		t.Fatal(err)
	}
	payloads := server.commandPayloads("thread.turn.start")
	if len(payloads) != 1 {
		t.Fatalf("turn starts = %+v, want one", payloads)
	}
	created, _ := payloads[0]["createdAt"].(string)
	if created == "" {
		t.Fatal("the bridge stamped no createdAt")
	}
	at, err := time.Parse(t3TimeLayout, created)
	if err != nil {
		t.Fatalf("createdAt %q does not parse as %q: %v", created, t3TimeLayout, err)
	}
	if at.Nanosecond()%int(time.Millisecond) != 0 || at.UTC().Format(t3TimeLayout) != created {
		t.Fatalf("createdAt %q is not stamped with the bridge's layout %q", created, t3TimeLayout)
	}
	server.setSnapshots(turnWithLatest(
		map[string]interface{}{"status": "running", "activeTurnId": "t2"},
		latestTurn("t2", "running", created, ""),
	))
	h, err := p.TurnHistory(t.Context(), turnFenceSession)
	if err != nil {
		t.Fatal(err)
	}
	if !h.CurrentStart.Equal(at) {
		t.Fatalf("CurrentStart = %v, want the stamped %v", h.CurrentStart, at)
	}
	if got := h.CurrentStart.UTC().Format(t3TimeLayout); got != created {
		t.Fatalf("CurrentStart formats as %q, want the stamped %q", got, created)
	}
}

// Kills a seam-backed provider that drops the history passthrough.
func TestSeamBackedForwardsTurnHistory(t *testing.T) {
	server := newTurnFenceServer(t, turnRunning("t1"))
	raw := newTurnFenceTestProvider(t, server)
	seam := &seamBackedProvider{raw: raw}
	h, err := seam.TurnHistory(t.Context(), turnFenceSession)
	if err != nil || h.Current.TurnID != "thread-agent-a/t1" {
		t.Fatalf("seam-backed TurnHistory = %+v, %v; want current thread-agent-a/t1", h, err)
	}
}
