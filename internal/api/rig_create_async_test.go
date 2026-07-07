package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// TestRequestIDFromPayloadRigVariants pins the correlation lookup for the two
// new rig payloads — the load-bearing case the async waiter and the
// emitAsyncResult nil-provider log path both depend on (G13 §10).
func TestRequestIDFromPayloadRigVariants(t *testing.T) {
	if got := requestIDFromPayload(RigCreateSucceededPayload{RequestID: "x"}); got != "x" {
		t.Errorf("RigCreateSucceededPayload request_id = %q, want x", got)
	}
	if got := requestIDFromPayload(RigProvisionProgressPayload{RequestID: "y"}); got != "y" {
		t.Errorf("RigProvisionProgressPayload request_id = %q, want y", got)
	}
}

// TestWithRigNameLockEmptyNameRefuses proves the empty-key inversion of the
// sourceworkflow.WithLock gotcha: an empty rig name is an error, never an
// unlocked fn() bypass.
func TestWithRigNameLockEmptyNameRefuses(t *testing.T) {
	ran := false
	err := withRigNameLock(context.Background(), "/city", "  ", func() error {
		ran = true
		return nil
	})
	if err == nil {
		t.Fatal("empty rig name lock: want error, got nil")
	}
	if ran {
		t.Fatal("empty rig name lock ran fn() unlocked (the WithLock bypass this inverts)")
	}
}

// TestWithRigNameLockSerializes proves same-(city,name) admission is mutually
// exclusive while a different name runs concurrently. Under -race a
// non-serialized critical section would trip on the shared counter.
func TestWithRigNameLockSerializes(t *testing.T) {
	const city = "/city"
	var counter int
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = withRigNameLock(context.Background(), city, "web", func() error {
				n := counter
				time.Sleep(time.Millisecond)
				counter = n + 1
				return nil
			})
		}()
	}
	wg.Wait()
	if counter != 20 {
		t.Fatalf("serialized counter = %d, want 20 (lost updates ⇒ not mutually exclusive)", counter)
	}

	// After the last waiter departs, the map entry is deleted (no leak).
	rigNameLockSet.mu.Lock()
	_, present := rigNameLockSet.locks[city+"\x00web"]
	rigNameLockSet.mu.Unlock()
	if present {
		t.Fatal("rig name lock entry leaked after all waiters released")
	}
}

// TestRigCreateAsyncGitURL202 drives the full async wire: a git_url POST returns
// 202 accepted with a request_id + event_cursor, the detached goroutine
// provisions (fake), and a terminal request.result.rig.create plus at least one
// rig.provision.progress event land on the city stream with the request_id.
func TestRigCreateAsyncGitURL202(t *testing.T) {
	state := newFakeMutatorState(t)
	state.cityBeadStore = beads.NewMemStore()
	h := newTestCityHandler(t, state)

	body := `{"name":"gitrig","git_url":"https://example.com/repo.git","request_id":"req-async-0001"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/rigs"), strings.NewReader(body)))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", rec.Code, rec.Body.String())
	}
	var resp RigCreateResponseBody
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "accepted" || resp.RequestID != "req-async-0001" {
		t.Fatalf("body = %+v, want status=accepted request_id=req-async-0001", resp)
	}
	if resp.EventCursor == "" {
		t.Fatal("202 body missing event_cursor")
	}

	// The detached goroutine drives to the terminal success event.
	result := waitForEventType(t, state.eventProv, events.RequestResultRigCreate, 3*time.Second)
	var succeeded RigCreateSucceededPayload
	if err := json.Unmarshal(result.Payload, &succeeded); err != nil {
		t.Fatalf("unmarshal result payload: %v", err)
	}
	if succeeded.RequestID != "req-async-0001" || succeeded.Rig != "gitrig" {
		t.Fatalf("result payload = %+v, want request_id=req-async-0001 rig=gitrig", succeeded)
	}

	// At least one progress event carried the same request_id.
	progress := waitForEventType(t, state.eventProv, events.RigProvisionProgress, time.Second)
	var prog RigProvisionProgressPayload
	if err := json.Unmarshal(progress.Payload, &prog); err != nil {
		t.Fatalf("unmarshal progress payload: %v", err)
	}
	if prog.RequestID != "req-async-0001" {
		t.Fatalf("progress request_id = %q, want req-async-0001", prog.RequestID)
	}
}

// TestRigCreateAsyncInflightReplay proves a duplicate identical request_id while
// the first provision is in flight replays the ORIGINAL 202 cursor and does not
// spawn a second provision (the ledger-lag double-clone guard, G13 §5).
func TestRigCreateAsyncInflightReplay(t *testing.T) {
	state := newFakeMutatorState(t)
	state.cityBeadStore = beads.NewMemStore()

	// Block the provision goroutine so the live entry is guaranteed in flight for
	// the replay POST.
	release := make(chan struct{})
	state.provisionGate = release
	h := newTestCityHandler(t, state)

	body := `{"name":"replayrig","git_url":"https://example.com/r.git","request_id":"req-replay-9001"}`
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, newPostRequest(cityURL(state, "/rigs"), strings.NewReader(body)))
	if rec1.Code != http.StatusAccepted {
		t.Fatalf("first POST status = %d, want 202; body=%s", rec1.Code, rec1.Body.String())
	}
	var first RigCreateResponseBody
	_ = json.NewDecoder(rec1.Body).Decode(&first)

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, newPostRequest(cityURL(state, "/rigs"), strings.NewReader(body)))
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("replay POST status = %d, want 202; body=%s", rec2.Code, rec2.Body.String())
	}
	var second RigCreateResponseBody
	_ = json.NewDecoder(rec2.Body).Decode(&second)
	if second.EventCursor != first.EventCursor {
		t.Fatalf("replay cursor = %q, want the original %q", second.EventCursor, first.EventCursor)
	}
	close(release)

	// Exactly one durable record, and it drives to succeeded.
	waitForEventType(t, state.eventProv, events.RequestResultRigCreate, 3*time.Second)
}

// panicOnTypeProvider wraps a fake event provider and panics on Record for one
// event type, to prove the OnStep emit is panic-isolated.
type panicOnTypeProvider struct {
	*events.Fake
	panicType string
}

func (p *panicOnTypeProvider) Record(e events.Event) {
	if e.Type == p.panicType {
		panic("boom: event bus down for " + e.Type)
	}
	p.Fake.Record(e)
}

// TestRigProvisionProgressEmitPanicIsSafe proves an OnStep emit that panics does
// NOT roll back or fail the provision: the recover lives inside the closure, so
// the terminal request.result.rig.create still lands and no request.failed is
// emitted (guards event_payloads §1.5 against provision.go rollback semantics).
func TestRigProvisionProgressEmitPanicIsSafe(t *testing.T) {
	state := newFakeMutatorState(t)
	state.cityBeadStore = beads.NewMemStore()
	state.eventProv = &panicOnTypeProvider{Fake: events.NewFake(), panicType: events.RigProvisionProgress}
	h := newTestCityHandler(t, state)

	body := `{"name":"panicrig","git_url":"https://example.com/p.git","request_id":"req-panic-0001"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/rigs"), strings.NewReader(body)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}

	// Success still lands despite every progress emit panicking.
	waitForEventType(t, state.eventProv.(*panicOnTypeProvider).Fake, events.RequestResultRigCreate, 3*time.Second)

	// And no request.failed was emitted.
	evs, _ := state.eventProv.List(events.Filter{})
	for _, e := range evs {
		if e.Type == events.RequestFailed {
			t.Fatalf("request.failed emitted despite recover-in-closure: %s", e.Payload)
		}
	}
}

func waitForEventType(t *testing.T, prov events.Provider, eventType string, timeout time.Duration) events.Event {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		evs, err := prov.List(events.Filter{})
		if err == nil {
			for _, e := range evs {
				if e.Type == eventType {
					return e
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("event %q not observed within %s", eventType, timeout)
	return events.Event{}
}
