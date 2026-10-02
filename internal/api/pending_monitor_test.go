package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// pendingTransitions returns the session.pending / session.pending_cleared
// events recorded so far, in seq order.
func pendingTransitions(t *testing.T, ep events.Provider) []events.Event {
	t.Helper()
	all, err := ep.List(events.Filter{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var out []events.Event
	for _, e := range all {
		if e.Type == events.SessionPending || e.Type == events.SessionPendingCleared {
			out = append(out, e)
		}
	}
	return out
}

func decodePendingPayload(t *testing.T, e events.Event) SessionPendingPayload {
	t.Helper()
	if e.Type != events.SessionPending {
		t.Fatalf("event type = %q, want %q", e.Type, events.SessionPending)
	}
	var p SessionPendingPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatalf("decode %s payload: %v", e.Type, err)
	}
	return p
}

func decodeClearedPayload(t *testing.T, e events.Event) SessionPendingClearedPayload {
	t.Helper()
	if e.Type != events.SessionPendingCleared {
		t.Fatalf("event type = %q, want %q", e.Type, events.SessionPendingCleared)
	}
	var p SessionPendingClearedPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatalf("decode %s payload: %v", e.Type, err)
	}
	return p
}

func approvalFor(requestID string) *runtime.PendingInteraction {
	return &runtime.PendingInteraction{
		RequestID: requestID,
		Kind:      "approval",
		Prompt:    "Bash: rm -rf build",
		Options:   []string{"Yes", "Yes, and don't ask again for rm commands", "No"},
		Metadata:  map[string]string{"tool_name": "Bash", "source": "tmux"},
	}
}

func TestPendingEventTypesAreRegistered(t *testing.T) {
	for _, typ := range []string{events.SessionPending, events.SessionPendingCleared} {
		found := false
		for _, known := range events.KnownEventTypes {
			if known == typ {
				found = true
			}
		}
		if !found {
			t.Errorf("%q missing from events.KnownEventTypes", typ)
		}
		if _, ok := events.LookupPayload(typ); !ok {
			t.Errorf("%q has no registered payload", typ)
		}
	}
}

// A pending interaction is announced once, with everything a client needs to
// answer it, no matter how many detection polls see it.
func TestPendingMonitorEmitsPendingOnceAcrossPolls(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	waiting := createTestSession(t, fs.cityBeadStore, fs.sp, "Waiting")
	_ = createTestSession(t, fs.cityBeadStore, fs.sp, "Idle")
	fs.sp.SetPendingInteraction(waiting.SessionName, approvalFor("req-1"))

	m := newPendingMonitor(fs.cityPath)
	for i := 0; i < 3; i++ {
		m.cycle(srv)
	}

	got := pendingTransitions(t, fs.eventProv)
	if len(got) != 1 {
		t.Fatalf("got %d pending transitions after 3 polls, want exactly 1: %#v", len(got), got)
	}
	e := got[0]
	if e.Subject != waiting.ID || e.SessionID != waiting.ID {
		t.Fatalf("envelope subject=%q session_id=%q, want both %q", e.Subject, e.SessionID, waiting.ID)
	}
	p := decodePendingPayload(t, e)
	want := approvalFor("req-1")
	if p.SessionID != waiting.ID || p.RequestID != want.RequestID || p.Kind != want.Kind ||
		p.Prompt != want.Prompt || strings.Join(p.Options, "|") != strings.Join(want.Options, "|") ||
		p.Metadata["tool_name"] != "Bash" || p.Template != waiting.Template {
		t.Fatalf("payload = %#v, want the full interaction for session %s", p, waiting.ID)
	}
}

// Answering the interaction clears it exactly once, with the same ids.
func TestPendingMonitorEmitsClearedOnceAfterRespond(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Waiting")
	fs.sp.SetPendingInteraction(info.SessionName, approvalFor("req-1"))

	m := newPendingMonitor(fs.cityPath)
	m.cycle(srv)

	req := newPostRequest(cityURL(fs, "/session/")+info.ID+"/respond", strings.NewReader(`{"action":"approve","request_id":"req-1"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("respond status = %d, want 202; body: %s", rec.Code, rec.Body.String())
	}

	m.cycle(srv)
	m.cycle(srv)

	got := pendingTransitions(t, fs.eventProv)
	if len(got) != 2 {
		t.Fatalf("got %d transitions, want pending then cleared: %#v", len(got), got)
	}
	cleared := decodeClearedPayload(t, got[1])
	if cleared.SessionID != info.ID || cleared.RequestID != "req-1" || cleared.Kind != "approval" || cleared.Reason != PendingClearedResolved {
		t.Fatalf("cleared payload = %#v, want session %s req-1 approval resolved", cleared, info.ID)
	}
	if got[1].Subject != info.ID || got[1].SessionID != info.ID {
		t.Fatalf("cleared envelope subject=%q session_id=%q, want %q", got[1].Subject, got[1].SessionID, info.ID)
	}
}

// A different interaction on the same session is a clear of the old one
// followed by a pending for the new one.
func TestPendingMonitorReplacedInteraction(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Waiting")
	fs.sp.SetPendingInteraction(info.SessionName, approvalFor("req-1"))

	m := newPendingMonitor(fs.cityPath)
	m.cycle(srv)
	fs.sp.SetPendingInteraction(info.SessionName, approvalFor("req-2"))
	m.cycle(srv)
	m.cycle(srv)

	got := pendingTransitions(t, fs.eventProv)
	if len(got) != 3 {
		t.Fatalf("got %d transitions, want pending/cleared/pending: %#v", len(got), got)
	}
	if p := decodePendingPayload(t, got[0]); p.RequestID != "req-1" {
		t.Fatalf("first pending = %#v, want req-1", p)
	}
	if c := decodeClearedPayload(t, got[1]); c.RequestID != "req-1" || c.Reason != PendingClearedReplaced {
		t.Fatalf("cleared = %#v, want req-1 replaced", c)
	}
	if p := decodePendingPayload(t, got[2]); p.RequestID != "req-2" {
		t.Fatalf("second pending = %#v, want req-2", p)
	}
}

// A session that leaves the probed set clears its interaction.
func TestPendingMonitorClearsWhenSessionGone(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Waiting")
	fs.sp.SetPendingInteraction(info.SessionName, approvalFor("req-1"))

	m := newPendingMonitor(fs.cityPath)
	m.cycle(srv)
	suspendSessionForPermissionModeTest(t, fs, info.ID)
	m.cycle(srv)
	m.cycle(srv)

	got := pendingTransitions(t, fs.eventProv)
	if len(got) != 2 {
		t.Fatalf("got %d transitions, want pending then cleared: %#v", len(got), got)
	}
	if c := decodeClearedPayload(t, got[1]); c.SessionID != info.ID || c.RequestID != "req-1" || c.Reason != PendingClearedSessionGone {
		t.Fatalf("cleared = %#v, want %s req-1 session_gone", c, info.ID)
	}
}

// A failed probe says nothing about the interaction, so it must not clear it.
func TestPendingMonitorProbeErrorDoesNotClear(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Waiting")
	fs.sp.SetPendingInteraction(info.SessionName, approvalFor("req-1"))
	failing := &pendingPerSessionErrorProvider{Fake: fs.sp, failErr: errors.New("capturing pane: probe blew up")}
	state := &stateWithSessionProvider{fakeState: fs, provider: failing}
	srv := New(state)

	m := newPendingMonitor(fs.cityPath)
	m.cycle(srv)
	failing.failName = info.SessionName
	m.cycle(srv)
	failing.failName = ""
	m.cycle(srv)

	got := pendingTransitions(t, fs.eventProv)
	if len(got) != 1 || got[0].Type != events.SessionPending {
		t.Fatalf("transitions = %#v, want only the original session.pending", got)
	}
}

// A restarted monitor (new process) picks up what was already published, so
// it emits only the transitions that happened while it was down.
func TestPendingMonitorRestartEmitsOnlyMissedTransitions(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	answered := createTestSession(t, fs.cityBeadStore, fs.sp, "Answered")
	still := createTestSession(t, fs.cityBeadStore, fs.sp, "Still")
	fs.sp.SetPendingInteraction(answered.SessionName, approvalFor("req-a"))
	fs.sp.SetPendingInteraction(still.SessionName, approvalFor("req-b"))

	newPendingMonitor(fs.cityPath).cycle(srv)
	if got := pendingTransitions(t, fs.eventProv); len(got) != 2 {
		t.Fatalf("before restart: %d transitions, want 2 pending: %#v", len(got), got)
	}

	// Down: the first interaction is answered at the terminal.
	fs.sp.SetPendingInteraction(answered.SessionName, nil)

	restarted := newPendingMonitor(fs.cityPath)
	restarted.cycle(New(fs))
	restarted.cycle(New(fs))

	got := pendingTransitions(t, fs.eventProv)
	if len(got) != 3 {
		t.Fatalf("after restart: %d transitions, want 2 pending + 1 cleared: %#v", len(got), got)
	}
	if c := decodeClearedPayload(t, got[2]); c.SessionID != answered.ID || c.RequestID != "req-a" || c.Reason != PendingClearedResolved {
		t.Fatalf("cleared after restart = %#v, want %s req-a resolved", c, answered.ID)
	}
}

// failingAckProvider drops the first n RecordAck calls.
type failingAckProvider struct {
	*events.Fake
	drops int
}

func (p *failingAckProvider) RecordAck(e events.Event) error {
	if p.drops > 0 {
		p.drops--
		return errors.New("events: append dropped")
	}
	return p.Fake.RecordAck(e)
}

// A transition whose event was dropped is retried on the next poll instead of
// being marked published.
func TestPendingMonitorRetriesDroppedEvent(t *testing.T) {
	fs := newSessionFakeState(t)
	ep := &failingAckProvider{Fake: events.NewFake(), drops: 1}
	fs.eventProv = ep
	srv := New(fs)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Waiting")
	fs.sp.SetPendingInteraction(info.SessionName, approvalFor("req-1"))

	m := newPendingMonitor(fs.cityPath)
	m.cycle(srv)
	if got := pendingTransitions(t, ep); len(got) != 0 {
		t.Fatalf("dropped append still produced %d events", len(got))
	}
	m.cycle(srv)
	m.cycle(srv)
	if got := pendingTransitions(t, ep); len(got) != 1 {
		t.Fatalf("got %d transitions after retry, want exactly 1: %#v", len(got), got)
	}
}

// sseFrame is one parsed SSE frame.
type sseFrame struct {
	event string
	id    string
	data  string
}

// openEventStream opens the city event stream and returns a channel of frames.
func openEventStream(ctx context.Context, t *testing.T, baseURL string, state State, lastEventID string) <-chan sseFrame {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+cityURL(state, "/events/stream"), nil)
	if err != nil {
		t.Fatalf("build stream request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open event stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("event stream status = %d", resp.StatusCode)
	}
	frames := make(chan sseFrame, 64)
	go func() {
		defer close(frames)
		defer resp.Body.Close() //nolint:errcheck
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		var cur sseFrame
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if cur.data != "" {
					frames <- cur
				}
				cur = sseFrame{}
			case strings.HasPrefix(line, "event:"):
				cur.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "id:"):
				cur.id = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
			case strings.HasPrefix(line, "data:"):
				cur.data += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
	}()
	return frames
}

// waitForFrame returns the first frame whose JSON type is eventType.
func waitForFrame(t *testing.T, frames <-chan sseFrame, eventType string) sseFrame {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatalf("stream closed before a %s frame", eventType)
			}
			var env struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal([]byte(f.data), &env); err != nil {
				continue
			}
			if env.Type == eventType {
				return f
			}
		case <-deadline:
			t.Fatalf("no %s frame within 5s", eventType)
		}
	}
}

func fastPendingMonitor(t *testing.T) {
	t.Helper()
	prev := pendingMonitorInterval
	pendingMonitorInterval = 20 * time.Millisecond
	t.Cleanup(func() { pendingMonitorInterval = prev })
}

// The city event stream carries the pending and cleared transitions to a
// client that only watches the stream.
func TestCityEventStreamCarriesPendingTransitions(t *testing.T) {
	fastPendingMonitor(t)
	fs := newSessionFakeState(t)
	srv := New(fs)
	ts := httptest.NewServer(newTestCityHandlerWith(t, fs, srv))
	defer ts.Close()
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Waiting")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames := openEventStream(ctx, t, ts.URL, fs, "")

	fs.sp.SetPendingInteraction(info.SessionName, approvalFor("req-1"))
	pending := waitForFrame(t, frames, events.SessionPending)
	if !strings.Contains(pending.data, `"request_id":"req-1"`) || !strings.Contains(pending.data, `"prompt":"Bash: rm -rf build"`) {
		t.Fatalf("session.pending frame lacks the interaction: %s", pending.data)
	}

	req, err := http.NewRequest(http.MethodPost, ts.URL+cityURL(fs, "/session/")+info.ID+"/respond", strings.NewReader(`{"action":"approve","request_id":"req-1"}`))
	if err != nil {
		t.Fatalf("build respond request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GC-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("respond: %v", err)
	}
	resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("respond status = %d, want 202", resp.StatusCode)
	}
	cleared := waitForFrame(t, frames, events.SessionPendingCleared)
	if !strings.Contains(cleared.data, `"request_id":"req-1"`) || !strings.Contains(cleared.data, `"reason":"resolved"`) {
		t.Fatalf("session.pending_cleared frame = %s", cleared.data)
	}

	// Many more polls: still exactly one of each.
	time.Sleep(10 * pendingMonitorInterval)
	if got := pendingTransitions(t, fs.eventProv); len(got) != 2 {
		t.Fatalf("got %d transitions, want exactly 2: %#v", len(got), got)
	}
}

// Detection costs nothing while no event stream is open for the city.
func TestPendingMonitorProbesOnlyWhileStreamOpen(t *testing.T) {
	fastPendingMonitor(t)
	fs := newSessionFakeState(t)
	srv := New(fs)
	ts := httptest.NewServer(newTestCityHandlerWith(t, fs, srv))
	defer ts.Close()
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Waiting")
	fs.sp.SetPendingInteraction(info.SessionName, approvalFor("req-1"))

	time.Sleep(10 * pendingMonitorInterval)
	if n := fs.sp.CountCalls("Pending", info.SessionName); n != 0 {
		t.Fatalf("probed %d times with no stream open, want 0", n)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames := openEventStream(ctx, t, ts.URL, fs, "")
	waitForFrame(t, frames, events.SessionPending)
	cancel()
	for range frames { //nolint:revive // drain until the stream goroutine exits
	}

	if n := fs.sp.CountCalls("Pending", info.SessionName); n == 0 {
		t.Fatal("no probes while the stream was open")
	}
	waitForMonitorIdle(t, fs, info.SessionName)
	idle := fs.sp.CountCalls("Pending", info.SessionName)
	time.Sleep(10 * pendingMonitorInterval)
	if n := fs.sp.CountCalls("Pending", info.SessionName); n != idle {
		t.Fatalf("probed %d more times after the last stream closed, want 0", n-idle)
	}
}

// A client that resumes with Last-Event-ID receives the clear for an
// interaction answered while it (and so the monitor) was away.
func TestEventStreamResumeDeliversClearMissedWhileDisconnected(t *testing.T) {
	fastPendingMonitor(t)
	fs := newSessionFakeState(t)
	srv := New(fs)
	ts := httptest.NewServer(newTestCityHandlerWith(t, fs, srv))
	defer ts.Close()
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Waiting")
	fs.sp.SetPendingInteraction(info.SessionName, approvalFor("req-1"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames := openEventStream(ctx, t, ts.URL, fs, "")
	pending := waitForFrame(t, frames, events.SessionPending)
	cancel()
	for range frames { //nolint:revive // drain until the stream goroutine exits
	}
	waitForMonitorIdle(t, fs, info.SessionName)

	// Answered at the terminal while nobody watches.
	fs.sp.SetPendingInteraction(info.SessionName, nil)
	time.Sleep(5 * pendingMonitorInterval)
	if got := pendingTransitions(t, fs.eventProv); len(got) != 1 {
		t.Fatalf("got %d transitions while disconnected, want only the pending: %#v", len(got), got)
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	resumed := openEventStream(ctx2, t, ts.URL, fs, pending.id)
	cleared := waitForFrame(t, resumed, events.SessionPendingCleared)
	if !strings.Contains(cleared.data, `"request_id":"req-1"`) {
		t.Fatalf("resumed clear = %s, want req-1", cleared.data)
	}
}

// waitForMonitorIdle waits until the monitor stops probing sessionName.
func waitForMonitorIdle(t *testing.T, fs *fakeState, sessionName string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		before := fs.sp.CountCalls("Pending", sessionName)
		time.Sleep(5 * pendingMonitorInterval)
		if fs.sp.CountCalls("Pending", sessionName) == before {
			return
		}
	}
	t.Fatal("monitor kept probing after the last stream closed")
}

// The supervisor-scope stream carries pending transitions for every city it
// watches, including a city that starts after the client connected.
func TestSupervisorEventStreamCarriesPendingTransitions(t *testing.T) {
	fastPendingMonitor(t)
	alpha := newSessionFakeState(t)
	alpha.cityName = "alpha"
	alphaInfo := createTestSession(t, alpha.cityBeadStore, alpha.sp, "Waiting")
	alpha.sp.SetPendingInteraction(alphaInfo.SessionName, approvalFor("req-alpha"))
	resolver := newDynamicCityResolver(map[string]*fakeState{"alpha": alpha})
	sm := NewSupervisorMux(resolver, nil, false, "test", "", time.Now())
	stream := openLiveGlobalStream(t, sm, "", "")

	frame, data := stream.nextTaggedEvent(t)
	if data["type"] != events.SessionPending || data["city"] != "alpha" || data["subject"] != alphaInfo.ID {
		t.Fatalf("first tagged_event = %s, want alpha session.pending for %s", frame.Data, alphaInfo.ID)
	}

	beta := newSessionFakeState(t)
	beta.cityName = "beta"
	betaInfo := createTestSession(t, beta.cityBeadStore, beta.sp, "Waiting")
	beta.sp.SetPendingInteraction(betaInfo.SessionName, approvalFor("req-beta"))
	resolver.addCity(beta)

	frame, data = stream.nextTaggedEvent(t)
	if data["type"] != events.SessionPending || data["city"] != "beta" || data["subject"] != betaInfo.ID {
		t.Fatalf("second tagged_event = %s, want beta session.pending for %s", frame.Data, betaInfo.ID)
	}
}
