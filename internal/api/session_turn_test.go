package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

// The session turn read (GET /v0/city/{cityName}/session/{id}/turn) projects
// the worker's TurnHandle: the session's turn source and capabilities, and
// what its current read of turns observed. It never 5xxs a session whose
// turns cannot be told; the observation says so.

const (
	turnTestEnded = "7f3e9a10-1111-4aaa-8000-000000000001"
	turnTestOpen  = "7f3e9a10-2222-4aaa-8000-000000000002"
)

// sessionTurnWire is the response as a client decodes it.
type sessionTurnWire struct {
	SessionID    string `json:"session_id"`
	Source       string `json:"source"`
	Capabilities struct {
		Start     string `json:"start"`
		End       string `json:"end"`
		Fence     string `json:"fence"`
		Link      string `json:"link"`
		Lossless  bool   `json:"lossless"`
		Interrupt string `json:"interrupt"`
	} `json:"capabilities"`
	Observation string     `json:"observation"`
	Detail      string     `json:"detail"`
	ObservedAt  *time.Time `json:"observed_at"`
	Current     *struct {
		ID        string     `json:"id"`
		StartedAt *time.Time `json:"started_at"`
	} `json:"current"`
	Recent []struct {
		ID        string     `json:"id"`
		StartedAt *time.Time `json:"started_at"`
		EndedAt   *time.Time `json:"ended_at"`
		Status    string     `json:"status"`
		Inferred  bool       `json:"inferred"`
	} `json:"recent"`
}

// busyBackend is a runtime backend whose idle snapshot reads busy, as an
// agent mid-turn does, so the idle overlay never ends an open turn however
// slowly a test runs.
type busyBackend struct{ runtime.FullFake }

func (busyBackend) SnapshotIdle(string) (bool, error) { return false, nil }

// newTurnTestClaudeSession creates a Claude session under a generated key, as
// production launches it with --session-id, on a fake state whose session
// provider is profiled as backend p. A nil backend is a FullFake, whose idle
// snapshot reads a running session idle.
func newTurnTestClaudeSession(t *testing.T, p runtime.Profile, backend func(*runtime.Fake) runtime.ProfileBackend) (*fakeState, *Server, session.Info, string) {
	t.Helper()
	fs := newSessionFakeState(t)
	var b runtime.ProfileBackend = runtime.FullFake{Fake: fs.sp}
	if backend != nil {
		b = backend(fs.sp)
	}
	fs.sessionProvider = runtime.NewFakeProfile(p, b)
	srv := New(fs)
	searchBase := t.TempDir()
	srv.sessionLogSearchPaths = []string{searchBase}
	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sessionProvider, session.WithCityPath(fs.cityPath),
		session.WithStaleKeyDetectionWaiter(func(context.Context, string) error { return nil }))
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{
		Template: "myrig/worker", Title: "Turns", Command: "claude", WorkDir: t.TempDir(), Provider: "claude",
		Resume:    session.ProviderResume{ResumeFlag: "--resume", ResumeStyle: "flag", SessionIDFlag: "--session-id"},
		ExtraMeta: map[string]string{"session_origin": "manual"},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if info.SessionKey == "" {
		t.Fatalf("session %s created without a session key", info.ID)
	}
	return fs, srv, info, searchBase
}

func getSessionTurn(t *testing.T, fs *fakeState, srv *Server, id string) *httptest.ResponseRecorder {
	t.Helper()
	h := newTestCityHandlerWith(t, fs, srv)
	req := httptest.NewRequest(http.MethodGet, cityURL(fs, "/session/"+id+"/turn"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeSessionTurn(t *testing.T, rec *httptest.ResponseRecorder) sessionTurnWire {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("GET session turn = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got sessionTurnWire
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode session turn: %v (%s)", err, rec.Body.String())
	}
	return got
}

func claudeTurnPrompt(id, ts string) string {
	return fmt.Sprintf(`{"type":"user","isSidechain":false,"timestamp":%q,"message":{"role":"user","content":"a prompt"},"promptId":%q,"origin":"user","promptSource":"prompt"}`, ts, id)
}

func claudeTurnReply(ts string) string {
	return fmt.Sprintf(`{"type":"assistant","isSidechain":false,"timestamp":%q,"message":{"role":"assistant","content":[{"type":"text","text":"a reply"}],"stop_reason":"end_turn"}}`, ts)
}

func claudeTurnDurationRecord(ts string) string {
	return fmt.Sprintf(`{"type":"system","subtype":"turn_duration","isSidechain":false,"timestamp":%q,"durationMs":1000}`, ts)
}

func busyTurnBackend(f *runtime.Fake) runtime.ProfileBackend {
	return busyBackend{runtime.FullFake{Fake: f}}
}

func TestSessionTurnTranscriptTierReportsCurrentAndRecentTurns(t *testing.T) {
	fs, srv, info, searchBase := newTurnTestClaudeSession(t, runtime.ProfileTmux, busyTurnBackend)
	writeNamedSessionJSONL(t, searchBase, info.WorkDir, info.SessionKey+".jsonl",
		claudeTurnPrompt(turnTestEnded, "2026-10-13T05:00:00.123Z"),
		claudeTurnReply("2026-10-13T05:00:04.000Z"),
		claudeTurnDurationRecord("2026-10-13T05:00:05.456Z"),
		claudeTurnPrompt(turnTestOpen, "2026-10-13T05:01:00.250Z"),
	)

	got := decodeSessionTurn(t, getSessionTurn(t, fs, srv, info.ID))

	if got.SessionID != info.ID || got.Source != "transcript" {
		t.Fatalf("session_id/source = %q/%q, want %q/transcript", got.SessionID, got.Source, info.ID)
	}
	c := got.Capabilities
	if c.Start != "recorded" || c.End != "inferred" || c.Fence != "checked" || c.Link != "correlated" || c.Lossless || c.Interrupt != "keystroke" {
		t.Fatalf("capabilities = %+v, want recorded/inferred/checked/correlated, not lossless, keystroke", c)
	}
	if got.Observation != "observed" || got.ObservedAt == nil {
		t.Fatalf("observation = %q (observed_at %v, detail %q), want observed", got.Observation, got.ObservedAt, got.Detail)
	}
	if got.Current == nil || got.Current.ID != turnTestOpen {
		t.Fatalf("current = %+v, want the open turn %s", got.Current, turnTestOpen)
	}
	if want := time.Date(2026, 10, 13, 5, 1, 0, 250_000_000, time.UTC); got.Current.StartedAt == nil || !got.Current.StartedAt.Equal(want) {
		t.Fatalf("current.started_at = %v, want %v", got.Current.StartedAt, want)
	}
	if len(got.Recent) != 1 {
		t.Fatalf("recent = %+v, want the one ended turn", got.Recent)
	}
	r := got.Recent[0]
	if r.ID != turnTestEnded || r.Status != "completed" || r.Inferred {
		t.Fatalf("recent[0] = %+v, want %s completed, recorded", r, turnTestEnded)
	}
	if want := time.Date(2026, 10, 13, 5, 0, 5, 456_000_000, time.UTC); r.EndedAt == nil || !r.EndedAt.Equal(want) {
		t.Fatalf("recent[0].ended_at = %v, want %v", r.EndedAt, want)
	}
}

func TestSessionTurnTranscriptTierIdleSessionHasNoCurrentTurn(t *testing.T) {
	fs, srv, info, searchBase := newTurnTestClaudeSession(t, runtime.ProfileTmux, nil)
	writeNamedSessionJSONL(t, searchBase, info.WorkDir, info.SessionKey+".jsonl",
		claudeTurnPrompt(turnTestEnded, "2026-10-13T05:00:00.123Z"),
		claudeTurnReply("2026-10-13T05:00:04.000Z"),
		claudeTurnDurationRecord("2026-10-13T05:00:05.456Z"),
	)

	got := decodeSessionTurn(t, getSessionTurn(t, fs, srv, info.ID))

	if got.Source != "transcript" || got.Observation != "observed" {
		t.Fatalf("source/observation = %q/%q (%s), want transcript/observed", got.Source, got.Observation, got.Detail)
	}
	if got.Current != nil {
		t.Fatalf("current = %+v, want none: the session is idle", got.Current)
	}
	if len(got.Recent) != 1 || got.Recent[0].ID != turnTestEnded {
		t.Fatalf("recent = %+v, want %s", got.Recent, turnTestEnded)
	}
}

// An open turn whose transcript has been quiet while two idle snapshots read
// idle reads ended, its end inferred (the worker's idle overlay).
func TestSessionTurnTranscriptTierInfersAQuietTurnsEnd(t *testing.T) {
	fs, srv, info, searchBase := newTurnTestClaudeSession(t, runtime.ProfileTmux, nil)
	writeNamedSessionJSONL(t, searchBase, info.WorkDir, info.SessionKey+".jsonl",
		claudeTurnPrompt(turnTestOpen, "2026-10-13T05:01:00.250Z"),
		claudeTurnReply("2026-10-13T05:01:04.000Z"),
	)
	quiet := time.Now().Add(-time.Minute)
	path := filepath.Join(searchBase, sessionlog.ProjectSlug(info.WorkDir), info.SessionKey+".jsonl")
	if err := os.Chtimes(path, quiet, quiet); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	got := decodeSessionTurn(t, getSessionTurn(t, fs, srv, info.ID))

	if got.Observation != "observed" || got.Current != nil {
		t.Fatalf("observation %q current %+v, want observed with no current turn", got.Observation, got.Current)
	}
	if len(got.Recent) != 1 || got.Recent[0].ID != turnTestOpen || !got.Recent[0].Inferred || got.Recent[0].Status != "" {
		t.Fatalf("recent = %+v, want %s ended by inference, with no status", got.Recent, turnTestOpen)
	}
}

// A transcript-tier session whose transcript is not written yet keeps its
// capabilities and reads unavailable: its turns are unknown, never idle.
func TestSessionTurnTranscriptNotWrittenIsUnavailable(t *testing.T) {
	fs, srv, info, _ := newTurnTestClaudeSession(t, runtime.ProfileTmux, nil)

	rec := getSessionTurn(t, fs, srv, info.ID)
	got := decodeSessionTurn(t, rec)
	if strings.Contains(rec.Body.String(), info.SessionKey) {
		t.Fatalf("turn read exposes the session key: %s", rec.Body.String())
	}

	if got.Source != "transcript" || got.Capabilities.Fence != "checked" {
		t.Fatalf("source/fence = %q/%q, want transcript/checked", got.Source, got.Capabilities.Fence)
	}
	if got.Observation != "unavailable" || got.Detail == "" {
		t.Fatalf("observation = %q detail %q, want unavailable with a detail", got.Observation, got.Detail)
	}
	if got.Current != nil || len(got.Recent) != 0 || got.ObservedAt != nil {
		t.Fatalf("unavailable read reported turns: current %+v recent %+v observed_at %v", got.Current, got.Recent, got.ObservedAt)
	}
}

// A session whose backend runs the agent elsewhere has no turn source: the
// read answers 200 unsupported, not a 5xx.
func TestSessionTurnWithoutSourceIsUnsupported(t *testing.T) {
	fs, srv, info, searchBase := newTurnTestClaudeSession(t, runtime.ProfileK8s, nil)
	writeNamedSessionJSONL(t, searchBase, info.WorkDir, info.SessionKey+".jsonl",
		claudeTurnPrompt(turnTestOpen, "2026-10-13T05:01:00.250Z"),
	)

	got := decodeSessionTurn(t, getSessionTurn(t, fs, srv, info.ID))

	if got.Source != "none" || got.Observation != "unsupported" {
		t.Fatalf("source/observation = %q/%q, want none/unsupported", got.Source, got.Observation)
	}
	c := got.Capabilities
	if c.Start != "none" || c.End != "none" || c.Fence != "none" || c.Link != "none" || c.Lossless {
		t.Fatalf("capabilities = %+v, want none on every axis", c)
	}
	if got.Current != nil || len(got.Recent) != 0 {
		t.Fatalf("unsupported read reported turns: current %+v recent %+v", got.Current, got.Recent)
	}
}

// newTurnTestProtocolSession creates a session whose runtime observes its
// turns itself, as ACP and t3bridge do: its turn reads answer from fs.sp's
// TurnHistoryScripts and TurnHistoryErrors.
func newTurnTestProtocolSession(t *testing.T) (*fakeState, *Server, session.Info) {
	t.Helper()
	fs := newSessionFakeState(t)
	fs.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
	fs.sessionProvider = runtime.FullFake{Fake: fs.sp}
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Protocol turns")
	fs.sp.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{info.SessionName: {{
		Start: runtime.TurnStartRecorded, End: runtime.TurnEndRecorded, Fence: runtime.TurnFenceAtomic, Link: runtime.TurnLinkExact,
	}}}
	fs.sp.TurnHistoryScripts = map[string][]runtime.TurnHistory{}
	fs.sp.TurnHistoryErrors = map[string][]error{}
	return fs, New(fs), info
}

// An open session that no runtime is bound to, on a protocol turn source (an
// ACP session with no control socket or a closed connection, a t3bridge
// session with no thread), runs no turn: its read answers 200 observed with no
// current turn, as /stop reads it, never the 404 a closed session gets, which
// would tell a client fencing a stop on this read that the session is gone.
func TestSessionTurnProtocolSourceWithNoRuntimeBoundRunsNoTurn(t *testing.T) {
	for name, err := range map[string]error{
		"acp with no control socket": fmt.Errorf("%w: no ACP control socket for %q", runtime.ErrSessionNotFound, "s"),
		"acp connection closed":      fmt.Errorf("%w: ACP session %q connection closed", runtime.ErrSessionNotFound, "s"),
		"t3bridge with no thread":    fmt.Errorf("%w: no t3 thread is bound to %q", runtime.ErrSessionNotFound, "s"),
	} {
		t.Run(name, func(t *testing.T) {
			fs, srv, info := newTurnTestProtocolSession(t)
			fs.sp.TurnHistoryErrors[info.SessionName] = []error{err}

			got := decodeSessionTurn(t, getSessionTurn(t, fs, srv, info.ID))

			if got.SessionID != info.ID || got.Source != "protocol" || got.Observation != "observed" || got.ObservedAt == nil {
				t.Fatalf("session_id/source/observation = %q/%q/%q (observed_at %v, detail %q), want %s/protocol/observed", got.SessionID, got.Source, got.Observation, got.ObservedAt, got.Detail, info.ID)
			}
			if got.Current != nil || len(got.Recent) != 0 {
				t.Fatalf("turns = current %+v recent %+v, want none: no runtime runs the session", got.Current, got.Recent)
			}
		})
	}
}

// A closed session is 404, also on a protocol turn source, whose runtime
// reports a session it does not run in the same terms.
func TestSessionTurnClosedSessionIsNotFound(t *testing.T) {
	fs, srv, info := newTurnTestProtocolSession(t)
	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp, session.WithCityPath(fs.cityPath))
	if err := mgr.Close(context.Background(), testOperatorIn(t, fs.cityPath), info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rec := getSessionTurn(t, fs, srv, info.ID)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET closed session turn = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	var problem apierr.ErrorModel
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v (%s)", err, rec.Body.String())
	}
	if problem.Code != apierr.SessionNotFound.Code {
		t.Fatalf("problem code = %q, want %q: %s", problem.Code, apierr.SessionNotFound.Code, rec.Body.String())
	}
}

// A turn the source saw end without an end time carries no ended_at, as one
// without a start time carries no started_at, never the zero time.
func TestSessionTurnOmitsAnUnknownEndTime(t *testing.T) {
	fs, srv, info := newTurnTestProtocolSession(t)
	fs.sp.TurnHistoryScripts[info.SessionName] = []runtime.TurnHistory{{
		Recent: []runtime.EndedTurn{{TurnID: turnTestEnded, Status: runtime.TurnStatusCompleted}},
	}}

	rec := getSessionTurn(t, fs, srv, info.ID)
	got := decodeSessionTurn(t, rec)

	if len(got.Recent) != 1 || got.Recent[0].ID != turnTestEnded || got.Recent[0].Status != "completed" {
		t.Fatalf("recent = %+v, want %s completed", got.Recent, turnTestEnded)
	}
	if strings.Contains(rec.Body.String(), "ended_at") {
		t.Fatalf("turn with no end time carries ended_at: %s", rec.Body.String())
	}
}

func TestSessionTurnUnknownSessionIsNotFound(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{filepath.Join(t.TempDir(), "logs")}

	rec := getSessionTurn(t, fs, srv, "gc-no-such-session")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET unknown session turn = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	var problem apierr.ErrorModel
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v (%s)", err, rec.Body.String())
	}
	if problem.Code != apierr.SessionNotFound.Code {
		t.Fatalf("problem code = %q, want %q: %s", problem.Code, apierr.SessionNotFound.Code, rec.Body.String())
	}
}
