package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

// apiKillRoutesFor is apiKillRoutes over any State, with an optional query
// string (e.g. "?force=true").
func apiKillRoutesFor(t *testing.T, state State) map[string]func(id, query string) *httptest.ResponseRecorder {
	return map[string]func(id, query string) *httptest.ResponseRecorder{
		"city route": func(id, query string) *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			newTestCityHandler(t, state).ServeHTTP(rec, newPostRequest(cityURL(state, "/session/")+id+"/kill"+query, nil))
			return rec
		},
		"legacy mux": func(id, query string) *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			New(state).legacySessionHandler().ServeHTTP(rec, newPostRequest("/v0/session/"+id+"/kill"+query, nil))
			return rec
		},
	}
}

func eventsOfType(ep events.Provider, typ, subject string) []events.Event {
	var out []events.Event
	for _, e := range ep.(*events.Fake).Events {
		if e.Type == typ && e.Subject == subject {
			out = append(out, e)
		}
	}
	return out
}

func workerKillOperations(t *testing.T, ep events.Provider, sessionID string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, e := range ep.(*events.Fake).Events {
		if e.Type != events.WorkerOperation {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatalf("decoding worker.operation payload %s: %v", e.Payload, err)
		}
		if payload["operation"] == "kill" && payload["session_id"] == sessionID {
			out = append(out, payload)
		}
	}
	return out
}

// An operator kill must leave the same outcome whether it comes from `gc
// session kill` or the API (#7645): the row asleep/killed with the fence
// lifted (#3629), session.stopped recorded with the CLI's message and payload,
// the worker's kill operation recorded, and the controller poked for the
// session so it observes the kill at once (#3812).
func TestAPISessionKillMatchesCLIKillOutcome(t *testing.T) {
	for route := range apiKillRoutesFor(t, nil) {
		t.Run(route, func(t *testing.T) {
			fs := newSessionFakeState(t)
			info := createTestSession(t, fs.cityBeadStore, fs.sp, "kill-parity")
			if !fs.sp.IsRunning(info.SessionName) {
				t.Fatalf("precondition: runtime %q not running", info.SessionName)
			}

			rec := apiKillRoutesFor(t, fs)[route](info.ID, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("kill status = %d, want 200; body: %s", rec.Code, rec.Body.String())
			}
			if fs.sp.IsRunning(info.SessionName) {
				t.Fatal("runtime still running after kill")
			}
			b, err := fs.cityBeadStore.Get(info.ID)
			if err != nil {
				t.Fatalf("Get(%s): %v", info.ID, err)
			}
			if got := b.Metadata["state"]; got != string(session.StateAsleep) {
				t.Errorf("state after API kill = %q, want %q", got, session.StateAsleep)
			}
			if got := b.Metadata["sleep_reason"]; got != string(session.SleepReasonKilled) {
				t.Errorf("sleep_reason after API kill = %q, want %q", got, session.SleepReasonKilled)
			}

			stopped := eventsOfType(fs.eventProv, events.SessionStopped, info.ID)
			if len(stopped) != 1 {
				t.Fatalf("session.stopped events for %s = %d, want 1; all events: %+v", info.ID, len(stopped), fs.eventProv.(*events.Fake).Events)
			}
			if stopped[0].Message != "killed" {
				t.Errorf("session.stopped message = %q, want %q (gc session kill's)", stopped[0].Message, "killed")
			}
			if got, want := string(stopped[0].Payload), string(SessionLifecyclePayloadJSON(info.ID, "", "killed")); got != want {
				t.Errorf("session.stopped payload = %s, want %s (gc session kill's)", got, want)
			}

			ops := workerKillOperations(t, fs.eventProv, info.ID)
			if len(ops) != 1 {
				t.Fatalf("worker.operation kill events for %s = %d, want 1", info.ID, len(ops))
			}
			if ops[0]["result"] != "succeeded" {
				t.Errorf("worker.operation kill result = %v, want succeeded", ops[0]["result"])
			}

			fs.enqueueMu.Lock()
			enqueued := slices.Clone(fs.enqueued)
			fs.enqueueMu.Unlock()
			if !slices.Contains(enqueued, reconcilekey.Session(info.ID).Normalize()) {
				t.Errorf("controller pokes = %v, want the session's reconcile key %v", enqueued, reconcilekey.Session(info.ID).Normalize())
			}
		})
	}
}

// writeLiveSubagentTranscript gives info a parent transcript with a
// background Agent spawn that has no terminal notification, and the subagent
// file mapping it: the work `gc session kill` refuses to destroy without
// --force.
func writeLiveSubagentTranscript(t *testing.T, searchBase, workDir string, info session.Info) {
	t.Helper()
	slugDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	subagentsDir := filepath.Join(slugDir, info.SessionKey, "subagents")
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatalf("mkdir subagents: %v", err)
	}
	parent := strings.Join([]string{
		`{"uuid":"u1","type":"user","message":{"role":"user","content":"hello"}}`,
		`{"uuid":"u2","parentUuid":"u1","type":"assistant","timestamp":"2026-10-17T09:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_live","name":"Agent","input":{"description":"Investigate the slow check","prompt":"Inspect it"}}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(slugDir, info.SessionKey+".jsonl"), []byte(parent), 0o644); err != nil {
		t.Fatalf("write parent transcript: %v", err)
	}
	agent := `{"uuid":"a1","type":"system","parentToolUseId":"toolu_live"}` + "\n"
	if err := os.WriteFile(filepath.Join(subagentsDir, "agent-helper.jsonl"), []byte(agent), 0o644); err != nil {
		t.Fatalf("write agent transcript: %v", err)
	}
}

// The API kill respects `gc session kill`'s live-subagent guard: a kill that
// would destroy live background subagents is refused (409) and leaves the
// runtime and the row alone, and force overrides it, as --force does.
func TestAPISessionKillRefusesLiveSubagentsUnlessForced(t *testing.T) {
	for route := range apiKillRoutesFor(t, nil) {
		t.Run(route, func(t *testing.T) {
			fs := newSessionFakeState(t)
			searchBase := t.TempDir()
			fs.cfg.Daemon.ObservePaths = []string{searchBase}
			workDir := filepath.Join(t.TempDir(), "claude-project")
			info := createTranscriptBackedSession(t, fs.cityBeadStore, fs.sp, workDir)
			writeLiveSubagentTranscript(t, searchBase, workDir, info)
			kill := apiKillRoutesFor(t, fs)[route]

			rec := kill(info.ID, "")
			if rec.Code != http.StatusConflict {
				t.Fatalf("kill over a live subagent: status = %d, want 409; body: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "Investigate the slow check") {
				t.Errorf("refusal body does not name the subagent it protects: %s", rec.Body.String())
			}
			if !fs.sp.IsRunning(info.SessionName) {
				t.Fatal("refused kill stopped the runtime")
			}
			if b, _ := fs.cityBeadStore.Get(info.ID); b.Metadata["state"] == string(session.StateAsleep) {
				t.Errorf("refused kill wrote the row asleep: %v", b.Metadata)
			}
			if n := len(eventsOfType(fs.eventProv, events.SessionStopped, info.ID)); n != 0 {
				t.Errorf("refused kill recorded %d session.stopped events, want 0", n)
			}

			rec = kill(info.ID, "?force=true")
			if rec.Code != http.StatusOK {
				t.Fatalf("forced kill: status = %d, want 200; body: %s", rec.Code, rec.Body.String())
			}
			if fs.sp.IsRunning(info.SessionName) {
				t.Fatal("forced kill left the runtime running")
			}
			if n := len(eventsOfType(fs.eventProv, events.SessionStopped, info.ID)); n != 1 {
				t.Errorf("forced kill recorded %d session.stopped events, want 1", n)
			}
		})
	}
}

// circuitResetState is a fakeState with the kill hooks of the process that
// runs the controller (SessionKillHooks).
type circuitResetState struct {
	*fakeState
	mu     sync.Mutex
	resets [][2]string
	stops  []string
}

func (s *circuitResetState) ResetSessionCircuitBreaker(sessionID, identity string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resets = append(s.resets, [2]string{sessionID, identity})
	return nil
}

func (s *circuitResetState) RecordSessionKillStop(info session.Info) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stops = append(s.stops, info.ID)
}

// An explicit kill of a configured named session clears its respawn circuit
// breaker, as `gc session kill` does, so the controller can revive it, and
// records the kill's stop metric.
func TestAPISessionKillClearsNamedSessionCircuitBreaker(t *testing.T) {
	for route := range apiKillRoutesFor(t, nil) {
		t.Run(route, func(t *testing.T) {
			fs := newSessionFakeState(t)
			state := &circuitResetState{fakeState: fs}
			info := createTestSession(t, fs.cityBeadStore, fs.sp, "named-kill")
			if err := fs.cityBeadStore.SetMetadata(info.ID, session.NamedSessionIdentityMetadata, "mayor"); err != nil {
				t.Fatalf("SetMetadata: %v", err)
			}

			rec := apiKillRoutesFor(t, state)[route](info.ID, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("kill status = %d, want 200; body: %s", rec.Code, rec.Body.String())
			}
			state.mu.Lock()
			defer state.mu.Unlock()
			if want := [][2]string{{info.ID, "mayor"}}; !slices.Equal(state.resets, want) {
				t.Errorf("circuit breaker resets = %v, want %v", state.resets, want)
			}
			if want := []string{info.ID}; !slices.Equal(state.stops, want) {
				t.Errorf("stop metrics recorded = %v, want %v", state.stops, want)
			}
		})
	}
}
