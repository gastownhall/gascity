package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

func TestHandleSessionResetRequestsFreshRestart(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "reset-test")
	pokesBefore := fs.pokeCount

	rec := httptest.NewRecorder()
	req := newPostRequest(cityURL(fs, "/session/")+info.ID+"/reset", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("reset status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body struct {
		Status string `json:"status"`
		ID     string `json:"id"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" || body.ID != info.ID {
		t.Fatalf("reset response = %+v, want status ok id %q", body, info.ID)
	}

	b, err := fs.cityBeadStore.Get(info.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", info.ID, err)
	}
	if got := b.Metadata["restart_requested"]; got != "true" {
		t.Errorf("restart_requested = %q, want true", got)
	}
	if got := b.Metadata["continuation_reset_pending"]; got != "true" {
		t.Errorf("continuation_reset_pending = %q, want true", got)
	}
	if fs.pokeCount != pokesBefore+1 {
		t.Errorf("pokeCount = %d, want %d", fs.pokeCount, pokesBefore+1)
	}

	evs, err := fs.eventProv.List(events.Filter{Type: events.WorkerOperation})
	if err != nil {
		t.Fatalf("List events: %v", err)
	}
	var found bool
	for _, ev := range evs {
		var p WorkerOperationEventPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatalf("decode worker.operation payload: %v", err)
		}
		if p.Operation == "reset" && p.SessionID == info.ID {
			found = true
			if p.Result != "succeeded" {
				t.Errorf("reset operation result = %q, want succeeded", p.Result)
			}
		}
	}
	if !found {
		t.Errorf("no worker.operation reset event recorded for %s", info.ID)
	}
}

func TestHandleSessionResetNotFound(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)

	rec := httptest.NewRecorder()
	req := newPostRequest(cityURL(fs, "/session/nonexistent/reset"), nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("reset nonexistent status = %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	if fs.pokeCount != 0 {
		t.Errorf("pokeCount = %d, want 0 for an unknown session", fs.pokeCount)
	}
}

func TestHandleSessionResetClosedSessionConflicts(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "reset-closed-test")
	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp, session.WithCityPath(fs.cityPath))
	if err := mgr.Close(context.Background(), testOperatorIn(t, fs.cityPath), info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	pokesBefore := fs.pokeCount

	rec := httptest.NewRecorder()
	req := newPostRequest(cityURL(fs, "/session/")+info.ID+"/reset", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("reset closed status = %d, want %d; body: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}
	b, err := fs.cityBeadStore.Get(info.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", info.ID, err)
	}
	if got := b.Metadata["continuation_reset_pending"]; got != "" {
		t.Errorf("continuation_reset_pending = %q on a closed session, want unset", got)
	}
	if fs.pokeCount != pokesBefore {
		t.Errorf("pokeCount = %d, want %d (no poke on a rejected reset)", fs.pokeCount, pokesBefore)
	}
}

// TestHandleSessionResetLeavesBreakerClearToTheController pins the half of the
// published contract that says what this endpoint deliberately does NOT do: it
// records the restart markers and moves no key of the respawn breaker cluster,
// because the clear is the controller's to make when it consumes the request.
// The handler consults no named-session metadata, so the identity key below
// sets the scenario rather than constraining this assertion; the named-session
// scoping of the clear is owned by cmd/gc/session_reconciler_restart_request_test.go
// instead. `gc session reset` clears the breaker synchronously first
// (cmd/gc/cmd_session_reset.go, resetSessionCircuitBreakerOnController);
// the reconciler's restart-requested block does it for this path instead
// (cmd/gc/session_reconciler.go, resetSessionCircuitBreakerState). Without this
// test the asymmetry lives only in the `describes(...)` prose, which is exactly
// how that prose drifted out of step with the code once already.
func TestHandleSessionResetLeavesBreakerClearToTheController(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "reset-breaker-test")
	// A tripped named-session respawn breaker, in the shape the reconciler
	// persists it (cmd/gc/session_circuit_breaker.go).
	if err := fs.cityBeadStore.SetMetadataBatch(info.ID, map[string]string{
		session.NamedSessionIdentityMetadata:              "myrig/worker",
		session.SessionCircuitStateMetadataKey:            "open",
		session.SessionCircuitOpenedAtMetadataKey:         "2026-01-01T00:00:00Z",
		session.SessionCircuitOpenRestartCountMetadataKey: "5",
		session.SessionCircuitResetGenerationMetadataKey:  "3",
	}); err != nil {
		t.Fatalf("SetMetadataBatch(breaker state): %v", err)
	}
	before, err := fs.cityBeadStore.Get(info.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", info.ID, err)
	}
	trippedBefore := session.CircuitStateFromMetadata(before.Metadata)
	if trippedBefore.State != "open" {
		t.Fatalf("precondition: circuit state = %q, want open", trippedBefore.State)
	}

	rec := httptest.NewRecorder()
	req := newPostRequest(cityURL(fs, "/session/")+info.ID+"/reset", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("reset status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	after, err := fs.cityBeadStore.Get(info.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", info.ID, err)
	}
	// The request is recorded: the controller needs the markers to reach its
	// restart-requested block, which is where it clears the breaker.
	if got := after.Metadata["restart_requested"]; got != "true" {
		t.Errorf("restart_requested = %q, want true", got)
	}
	if got := after.Metadata["continuation_reset_pending"]; got != "true" {
		t.Errorf("continuation_reset_pending = %q, want true", got)
	}
	// ...and not one byte of the breaker cluster moved synchronously.
	if got := session.CircuitStateFromMetadata(after.Metadata); got != trippedBefore {
		t.Errorf("circuit breaker state changed synchronously:\n got %+v\nwant %+v", got, trippedBefore)
	}
}

func TestHandleSessionResetRequiresCSRFHeader(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "reset-csrf-test")

	req := httptest.NewRequest(http.MethodPost, cityURL(fs, "/session/")+info.ID+"/reset", strings.NewReader(""))
	// No X-GC-Request header.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("reset without CSRF status = %d, want %d; body: %s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
	b, err := fs.cityBeadStore.Get(info.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", info.ID, err)
	}
	if got := b.Metadata["restart_requested"]; got != "" {
		t.Errorf("restart_requested = %q after CSRF rejection, want unset", got)
	}
}

// resetRefusingState is a controller that names why it will not act on a
// recorded reset.
type resetRefusingState struct {
	*fakeState
	refusal string
	seen    session.Info
}

func (s *resetRefusingState) ResetRefusal(info session.Info) string {
	s.seen = info
	return s.refusal
}

// TestHandleSessionResetRefusedByTheController: when the controller names why
// it will not act on a reset of the row (a state it skips), the API reset
// answers 409 session-conflict with that reason, judged on the stored row,
// and writes nothing: no fresh-restart request, no restart op, no reconcile
// poke. Kills a reset that queues a restart the controller never applies.
func TestHandleSessionResetRefusedByTheController(t *testing.T) {
	e := newControlOpsEnv(t, "auto", "auto")
	info := e.session(t, "Skipped")
	refuser := &resetRefusingState{fakeState: e.fs, refusal: "its state \"future-state\" is not one this gc version's controller manages, so the controller skips it; `gc session close " + info.ID + "` releases it"}
	e.resolver.state = refuser
	pokesBefore := e.fs.pokeCount

	rec := e.post(t, "/session/"+info.ID+"/reset", "", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("reset status = %d, want %d; body: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}
	for _, want := range []string{"session-conflict", "session " + info.ID + " cannot be reset: ", `\"future-state\"`, "gc session close " + info.ID} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("body missing %q: %s", want, rec.Body.String())
		}
	}
	if refuser.seen.ID != info.ID {
		t.Fatalf("refuser read %+v, want the stored row %s", refuser.seen, info.ID)
	}
	b, err := e.fs.cityBeadStore.Get(info.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", info.ID, err)
	}
	if b.Metadata["restart_requested"] != "" || b.Metadata["continuation_reset_pending"] != "" {
		t.Fatalf("metadata = %v, want no fresh restart requested", b.Metadata)
	}
	if rows := e.opRows(t); len(rows) != 0 {
		t.Fatalf("op rows = %d, want none", len(rows))
	}
	if e.fs.pokeCount != pokesBefore {
		t.Fatalf("pokeCount = %d, want %d", e.fs.pokeCount, pokesBefore)
	}
}
