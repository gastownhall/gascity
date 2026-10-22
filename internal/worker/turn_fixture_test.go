package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

// Fixtures shared by the turn tests (turn_compose_test.go,
// turn_current_test.go, turn_watch_test.go).

// Turn ids in the record shapes TI-2a's scanners read.
const (
	claudeTurn1 = "7f3e9a10-1111-4aaa-8000-000000000001"
	claudeTurn2 = "7f3e9a10-2222-4aaa-8000-000000000002"
	codexTurn1  = "019a1111-0000-7000-8000-000000000001"
	codexTurn2  = "019a2222-0000-7000-8000-000000000002"
	geminiTurn1 = "9d1e0000-aaaa-4000-8000-000000000001"
	// codexConversation is the Codex session's conversation id: its
	// session_key, as the SessionStart hook stamps it, and its rollout's.
	codexConversation = "019a0000-0000-7000-8000-000000000000"
	// otherConversation is a conversation no fixture session runs.
	otherConversation = "019a9999-0000-7000-8000-000000000009"
)

var (
	fullTurnCaps = runtime.TurnCapabilities{Start: runtime.TurnStartRecorded, End: runtime.TurnEndRecorded, Fence: runtime.TurnFenceAtomic, Link: runtime.TurnLinkExact, Lossless: true}
	v1TurnCaps   = runtime.TurnCapabilities{Start: runtime.TurnStartRecorded, End: runtime.TurnEndInferred, Fence: runtime.TurnFenceChecked, Link: runtime.TurnLinkCorrelated}
)

func mustTurnTime(t *testing.T, raw string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return at
}

// Claude records.
func claudePrompt(id, ts string) string {
	return fmt.Sprintf(`{"type":"user","isSidechain":false,"timestamp":%q,"message":{"role":"user","content":"a prompt"},"promptId":%q,"origin":"user","promptSource":"prompt"}`, ts, id)
}

// claudePromptWithoutID is a prompt as a Claude CLI that predates promptId
// records it.
func claudePromptWithoutID(ts string) string {
	return fmt.Sprintf(`{"type":"user","isSidechain":false,"timestamp":%q,"message":{"role":"user","content":"a prompt"}}`, ts)
}

func claudeReply(ts string) string {
	return fmt.Sprintf(`{"type":"assistant","isSidechain":false,"timestamp":%q,"message":{"role":"assistant","content":[{"type":"text","text":"a reply"}],"stop_reason":"end_turn"}}`, ts)
}

func claudeTurnDuration(ts string) string {
	return fmt.Sprintf(`{"type":"system","subtype":"turn_duration","isSidechain":false,"timestamp":%q,"durationMs":1000}`, ts)
}

// claudeQueueOp is a queue-operation record: Claude Code 2.1.299 and later
// write an enqueue and a dequeue around input it runs from its queue, and can
// open a conversation with a turn that ran from queued input, with no user
// entry before its reply and turn_duration.
func claudeQueueOp(op, ts string) string {
	return fmt.Sprintf(`{"type":"queue-operation","operation":%q,"timestamp":%q,"sessionId":"5b1c0d2e-0000-4000-8000-000000000001","content":"queued text"}`, op, ts)
}

// claudeLongReply is a reply of about size bytes.
func claudeLongReply(ts string, size int) string {
	return fmt.Sprintf(`{"type":"assistant","isSidechain":false,"timestamp":%q,"message":{"role":"assistant","content":[{"type":"text","text":%q}]}}`, ts, strings.Repeat("x", size))
}

func claudeInterrupt(id, ts string) string {
	return fmt.Sprintf(`{"type":"user","isSidechain":false,"timestamp":%q,"message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user for tool use]"}]},"promptId":%q}`, ts, id)
}

// Codex records.
func codexMeta(id, cwd string) string {
	return fmt.Sprintf(`{"timestamp":"2026-10-08T02:22:20.000Z","type":"session_meta","payload":{"id":%q,"timestamp":"2026-10-08T02:22:20.000Z","cwd":%q,"originator":"codex_cli_rs","cli_version":"0.159.1","source":"cli"}}`, id, cwd)
}

func codexStarted(id, ts string, startedAt int64) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"task_started","turn_id":%q,"started_at":%d}}`, ts, id, startedAt)
}

func codexComplete(id, ts string) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"task_complete","turn_id":%q,"completed_at":1791426150,"error":null}}`, ts, id)
}

func codexAborted(id, ts string) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"turn_aborted","turn_id":%q,"reason":"interrupted"}}`, ts, id)
}

// Gemini records.
func geminiUser(id, ts string) string {
	return fmt.Sprintf(`{"id":%q,"timestamp":%q,"type":"user","content":[{"text":"a prompt"}]}`, id, ts)
}

func geminiReply(id, ts string) string {
	return fmt.Sprintf(`{"id":%q,"timestamp":%q,"type":"gemini","content":"a reply"}`, id, ts)
}

func writeTranscriptLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	body := strings.Join(lines, "\n")
	if len(lines) > 0 {
		body += "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

func appendTranscript(t *testing.T, path, raw string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile(%q): %v", path, err)
	}
	defer f.Close() //nolint:errcheck // test helper
	if _, err := f.WriteString(raw); err != nil {
		t.Fatalf("append %q: %v", path, err)
	}
}

// turnSession is a started session handle whose transcripts discovery finds
// under root.
type turnSession struct {
	handle *SessionHandle
	store  beads.Store
	sp     runtime.Provider
	info   sessionpkg.Info
	root   string
}

// turnSessionOptions shape a fixture session.
type turnSessionOptions struct {
	transport string
	// keyless starts the session without a conversation key: a Claude
	// without a session-id flag, or a Codex before its SessionStart hook.
	keyless bool
}

// newTurnSession starts a session of provider on sp the way production runs
// it: a Claude session under a generated key it passes with --session-id, a
// Codex session with the key its SessionStart hook stamps.
func newTurnSession(t *testing.T, sp runtime.Provider, provider, transport string) turnSession {
	t.Helper()
	return newTurnSessionWith(t, sp, provider, turnSessionOptions{transport: transport})
}

func newTurnSessionWith(t *testing.T, sp runtime.Provider, provider string, opts turnSessionOptions) turnSession {
	t.Helper()
	root := t.TempDir()
	store := beads.NewMemStore()
	manager := sessionpkg.NewManagerWithOptions(store, sp, sessionpkg.WithCityPath(t.TempDir()),
		// A keyed start would otherwise wait out the stale-key detection delay.
		sessionpkg.WithStaleKeyDetectionWaiter(func(context.Context, string) error { return nil }))
	spec := SessionSpec{
		Template:  "probe",
		Title:     "Probe",
		Command:   provider,
		WorkDir:   t.TempDir(),
		Provider:  provider,
		Transport: opts.transport,
	}
	if !opts.keyless && provider != "codex" {
		spec.Resume = sessionpkg.ProviderResume{SessionIDFlag: "--session-id"}
	}
	handle, err := NewSessionHandle(SessionHandleConfig{Manager: manager, SearchPaths: []string{root}, Session: spec})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}
	if err := handle.Start(context.Background(), handleActor(t, handle, sessionpkg.ActorAgent)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	s := turnSession{handle: handle, store: store, sp: sp, root: root}
	if !opts.keyless && provider == "codex" {
		s.setKey(t, codexConversation)
	}
	s.reload(t)
	if !opts.keyless && s.info.SessionKey == "" {
		t.Fatalf("session %s started without a key", s.info.ID)
	}
	return s
}

// reload re-reads the session's Info.
func (s *turnSession) reload(t *testing.T) {
	t.Helper()
	info, err := s.handle.manager.Get(s.handle.currentSessionID())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	s.info = info
}

// setKey stamps the session's conversation key, as a SessionStart hook or a
// fresh restart does.
func (s *turnSession) setKey(t *testing.T, key string) {
	t.Helper()
	if err := s.store.SetMetadata(s.handle.currentSessionID(), "session_key", key); err != nil {
		t.Fatalf("SetMetadata(session_key): %v", err)
	}
	s.reload(t)
}

// codexRolloutPath is where Codex writes the rollout of conversation id: a
// day directory and a filename of the session's start, which is today.
func (s turnSession) codexRolloutPath(id string) string {
	now := time.Now()
	return filepath.Join(s.root, now.Format("2006"), now.Format("01"), now.Format("02"), "rollout-"+now.Format("2006-01-02T15-04-05")+"-"+id+".jsonl")
}

// transcriptPath is where the session's provider writes the transcript of its
// conversation.
func (s turnSession) transcriptPath(family string) string {
	switch family {
	case "codex":
		return s.codexRolloutPath(s.info.SessionKey)
	case "gemini":
		return filepath.Join(s.root, "project", "chats", "session-1.json")
	default:
		return filepath.Join(s.root, sessionlog.ProjectSlug(s.info.WorkDir), s.info.SessionKey+".jsonl")
	}
}

// writeTranscript writes the session's transcript of family. A Codex
// rollout opens with the conversation's session_meta, and a Gemini chat is
// placed under a project directory naming the work directory, as discovery
// needs.
func (s turnSession) writeTranscript(t *testing.T, family string, lines ...string) string {
	t.Helper()
	path := s.transcriptPath(family)
	switch family {
	case "codex":
		lines = append([]string{codexMeta(s.info.SessionKey, s.info.WorkDir)}, lines...)
	case "gemini":
		s.writeGeminiProject(t)
		body := fmt.Sprintf(`{"sessionId":"g-1","projectHash":"project","messages":[%s]}`, strings.Join(lines, ","))
		writeTranscriptLines(t, path, body)
		return path
	}
	writeTranscriptLines(t, path, lines...)
	return path
}

func (s turnSession) writeGeminiProject(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(s.root, "project"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(s.root, "project", ".project_root"), []byte(s.info.WorkDir), 0o644); err != nil {
		t.Fatalf("WriteFile(.project_root): %v", err)
	}
}

// writeCompletedTurn writes a transcript of family holding one completed
// turn.
func (s turnSession) writeCompletedTurn(t *testing.T, family string) string {
	t.Helper()
	switch family {
	case "claude":
		return s.writeTranscript(t, family, claudePrompt(claudeTurn1, "2026-10-13T05:00:00.123Z"), claudeReply("2026-10-13T05:00:04.000Z"), claudeTurnDuration("2026-10-13T05:00:05.456Z"))
	case "codex":
		return s.writeTranscript(t, family, codexStarted(codexTurn1, "2026-10-08T02:22:22.191Z", 1791426142), codexComplete(codexTurn1, "2026-10-08T02:22:30.500Z"))
	case "gemini":
		return s.writeTranscript(t, family, geminiUser(geminiTurn1, "2026-06-21T17:08:00.694Z"), geminiReply("a1", "2026-06-21T17:08:10.000Z"))
	}
	return ""
}

// ageTranscript sets path's modification time to at.
func ageTranscript(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
}

// idleScript is a runtime backend whose SnapshotIdle answers from a script
// (repeating its last answer), and can run a hook before each answer.
type idleScript struct {
	runtime.FullFake
	state *idleScriptState
}

type idleScriptState struct {
	mu      sync.Mutex
	answers []idleAnswer
	calls   int
	before  func(call int)
}

type idleAnswer struct {
	idle bool
	err  error
}

func newIdleScript(answers ...idleAnswer) idleScript {
	return idleScript{FullFake: runtime.FullFake{Fake: runtime.NewFake()}, state: &idleScriptState{answers: answers}}
}

func (b idleScript) SnapshotIdle(string) (bool, error) {
	b.state.mu.Lock()
	b.state.calls++
	call, before := b.state.calls, b.state.before
	a := idleAnswer{idle: true}
	if len(b.state.answers) > 0 {
		a = b.state.answers[0]
		if len(b.state.answers) > 1 {
			b.state.answers = b.state.answers[1:]
		}
	}
	b.state.mu.Unlock()
	if before != nil {
		before(call)
	}
	return a.idle, a.err
}

func (b idleScript) idleCalls() int {
	b.state.mu.Lock()
	defer b.state.mu.Unlock()
	return b.state.calls
}

// manualTurnClock is a turnTiming whose now and after the test drives.
type manualTurnClock struct {
	mu   sync.Mutex
	at   time.Time
	asks chan turnAsk
	// onAfter, when set, runs as each wait is asked for, before it is
	// queued: a gap that lets time pass.
	onAfter func(d time.Duration)
}

type turnAsk struct {
	d    time.Duration
	fire chan time.Time
}

func newManualTurnClock(at time.Time) *manualTurnClock {
	return &manualTurnClock{at: at, asks: make(chan turnAsk, 64)}
}

func (c *manualTurnClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *manualTurnClock) advance(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

func (c *manualTurnClock) after(d time.Duration) <-chan time.Time {
	if c.onAfter != nil {
		c.onAfter(d)
	}
	ask := turnAsk{d: d, fire: make(chan time.Time, 1)}
	c.asks <- ask
	return ask.fire
}

// nextAsk waits for the watcher to ask for its next wait.
func (c *manualTurnClock) nextAsk(t *testing.T) turnAsk {
	t.Helper()
	select {
	case ask := <-c.asks:
		return ask
	case <-time.After(10 * time.Second):
		t.Fatal("watcher never asked to wait")
		return turnAsk{}
	}
}

func (c *manualTurnClock) timing() *turnTiming {
	return &turnTiming{now: c.now, after: c.after, noNotify: true}
}

func capsRow(c TurnCapabilities) string {
	return fmt.Sprintf("%s %s/%s/%s/%s/%v %s %s", c.Source, c.Start, c.End, c.Fence, c.Link, c.Lossless, c.Interrupt, c.Replace)
}

func profiled(p runtime.Profile, ff runtime.ProfileBackend) runtime.Provider {
	return runtime.NewFakeProfile(p, ff)
}

func assertCurrentTurn(t *testing.T, h TurnHandle, observedAt time.Time, want func(t *testing.T) runtime.TurnHistory, wantErr error) {
	t.Helper()
	got, err := h.CurrentTurn(context.Background())
	if wantErr != nil {
		assertNoHistory(t, got, err, wantErr)
		return
	}
	if err != nil {
		t.Fatalf("CurrentTurn: %v", err)
	}
	w := want(t)
	w.ObservedAt = observedAt
	assertTurnHistory(t, got, w)
}

// assertNoHistory asserts a failed read: err wraps wantErr, with the zero
// history beside it (never idle).
func assertNoHistory(t *testing.T, got runtime.TurnHistory, err, wantErr error) {
	t.Helper()
	if !errors.Is(err, wantErr) {
		t.Fatalf("CurrentTurn error = %v, want %v", err, wantErr)
	}
	if got.Current.Active() || len(got.Recent) != 0 || !got.ObservedAt.IsZero() {
		t.Fatalf("CurrentTurn = %+v with its error, want the zero history (never idle)", got)
	}
}

func assertTurnHistory(t *testing.T, got, want runtime.TurnHistory) {
	t.Helper()
	if got.Current != want.Current || !got.CurrentStart.Equal(want.CurrentStart) || !got.ObservedAt.Equal(want.ObservedAt) || len(got.Recent) != len(want.Recent) {
		t.Fatalf("history = %+v, want %+v", got, want)
	}
	for i := range want.Recent {
		g, w := got.Recent[i], want.Recent[i]
		if g.TurnID != w.TurnID || !g.StartedAt.Equal(w.StartedAt) || !g.EndedAt.Equal(w.EndedAt) || g.Status != w.Status || g.Inferred != w.Inferred {
			t.Fatalf("Recent[%d] = %+v, want %+v", i, g, w)
		}
	}
}

// receiveTurnUpdate waits for the watcher's next update.
func receiveTurnUpdate(t *testing.T, ch <-chan TurnUpdate) TurnUpdate {
	t.Helper()
	select {
	case u, ok := <-ch:
		if !ok {
			t.Fatal("watch closed early")
		}
		return u
	case <-time.After(10 * time.Second):
		t.Fatal("no turn update")
		return TurnUpdate{}
	}
}

func assertNoPendingUpdate(t *testing.T, ch <-chan TurnUpdate) {
	t.Helper()
	select {
	case u := <-ch:
		t.Fatalf("unexpected pending update %+v", u)
	default:
	}
}

func assertWatchCloses(t *testing.T, ch <-chan TurnUpdate) {
	t.Helper()
	assertWatchClosesWithin(t, ch, 10*time.Second)
}

// assertWatchClosesWithin asserts the watch closes within d of its ctx
// ending, taking what it sends meanwhile.
func assertWatchClosesWithin(t *testing.T, ch <-chan TurnUpdate, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatalf("watch did not close within %s after ctx ended", d)
		}
	}
}
