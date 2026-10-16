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
	"github.com/gastownhall/gascity/internal/runtime/auto"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

// Turn ids in the record shapes TI-2a's scanners read.
const (
	claudeTurn1 = "7f3e9a10-1111-4aaa-8000-000000000001"
	claudeTurn2 = "7f3e9a10-2222-4aaa-8000-000000000002"
	codexTurn1  = "019a1111-0000-7000-8000-000000000001"
	codexTurn2  = "019a2222-0000-7000-8000-000000000002"
	geminiTurn1 = "9d1e0000-aaaa-4000-8000-000000000001"
	geminiTurn2 = "9d1e0000-bbbb-4000-8000-000000000002"
)

var (
	fullTurnCaps = runtime.TurnCapabilities{Start: runtime.TurnStartRecorded, End: runtime.TurnEndRecorded, Fence: runtime.TurnFenceAtomic, Link: runtime.TurnLinkExact, Lossless: true}
	v1TurnCaps   = runtime.TurnCapabilities{Start: runtime.TurnStartRecorded, End: runtime.TurnEndRecorded, Fence: runtime.TurnFenceChecked, Link: runtime.TurnLinkCorrelated}
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

func claudeReply(ts string) string {
	return fmt.Sprintf(`{"type":"assistant","isSidechain":false,"timestamp":%q,"message":{"role":"assistant","content":[{"type":"text","text":"a reply"}],"stop_reason":"end_turn"}}`, ts)
}

func claudeTurnDuration(ts string) string {
	return fmt.Sprintf(`{"type":"system","subtype":"turn_duration","isSidechain":false,"timestamp":%q,"durationMs":1000}`, ts)
}

func claudeInterrupt(id, ts string) string {
	return fmt.Sprintf(`{"type":"user","isSidechain":false,"timestamp":%q,"message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user for tool use]"}]},"promptId":%q}`, ts, id)
}

// Codex records.
func codexMeta(cwd string) string {
	return fmt.Sprintf(`{"timestamp":"2026-10-08T02:22:20.000Z","type":"session_meta","payload":{"id":"019a0000-0000-7000-8000-000000000000","timestamp":"2026-10-08T02:22:20.000Z","cwd":%q,"originator":"codex_cli_rs","cli_version":"0.159.1","source":"cli"}}`, cwd)
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
	info   sessionpkg.Info
	root   string
}

func newTurnSession(t *testing.T, sp runtime.Provider, provider, transport string) turnSession {
	t.Helper()
	root := t.TempDir()
	manager := sessionpkg.NewManagerWithOptions(beads.NewMemStore(), sp, sessionpkg.WithCityPath(t.TempDir()))
	handle, err := NewSessionHandle(SessionHandleConfig{
		Manager:     manager,
		SearchPaths: []string{root},
		Session: SessionSpec{
			Template:  "probe",
			Title:     "Probe",
			Command:   provider,
			WorkDir:   t.TempDir(),
			Provider:  provider,
			Transport: transport,
		},
	})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}
	if err := handle.Start(context.Background(), handleActor(t, handle, sessionpkg.ActorAgent)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	info, err := manager.Get(handle.currentSessionID())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return turnSession{handle: handle, info: info, root: root}
}

// transcriptPath is where discovery finds this session's transcript of family.
func (s turnSession) transcriptPath(family string) string {
	switch family {
	case "codex":
		return filepath.Join(s.root, "2026", "10", "08", "rollout-2026-10-08T02-22-20-019a0000-0000-7000-8000-000000000000.jsonl")
	case "gemini", "gemini-jsonl":
		name := "session-1.json"
		if family == "gemini-jsonl" {
			name = "session-1.jsonl"
		}
		return filepath.Join(s.root, "project", "chats", name)
	default:
		key := s.info.SessionKey
		if key == "" {
			key = "transcript"
		}
		return filepath.Join(s.root, sessionlog.ProjectSlug(s.info.WorkDir), key+".jsonl")
	}
}

// writeTranscript writes the session's transcript of family. A Codex
// transcript opens with session_meta and a Gemini one is placed under a
// project directory naming the work directory, as discovery needs.
func (s turnSession) writeTranscript(t *testing.T, family string, lines ...string) string {
	t.Helper()
	path := s.transcriptPath(family)
	switch family {
	case "codex":
		lines = append([]string{codexMeta(s.info.WorkDir)}, lines...)
	case "gemini", "gemini-jsonl":
		if err := os.MkdirAll(filepath.Join(s.root, "project"), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(s.root, "project", ".project_root"), []byte(s.info.WorkDir), 0o644); err != nil {
			t.Fatalf("WriteFile(.project_root): %v", err)
		}
	}
	if family == "gemini" {
		body := fmt.Sprintf(`{"sessionId":"g-1","projectHash":"project","messages":[%s]}`, strings.Join(lines, ","))
		writeTranscriptLines(t, path, body)
		return path
	}
	writeTranscriptLines(t, path, lines...)
	return path
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

func profiled(p runtime.Profile, ff runtime.FullFake) runtime.Provider {
	return runtime.NewFakeProfile(p, ff)
}

// The composition matrix over profiled fakes crossed with families (X11 §5.2,
// TI-2b acceptance).
func TestTurnCapabilitiesCompositionMatrix(t *testing.T) {
	type matrixCase struct {
		name     string
		provider string
		// build returns the session's provider and a function scripting
		// it once the session's runtime name is known.
		build      func() (runtime.Provider, func(name string))
		transport  string
		transcript string // the family whose transcript is written; "" writes none
		want       string
	}
	noScript := func(string) {}
	plain := func(p runtime.Profile) func() (runtime.Provider, func(string)) {
		return func() (runtime.Provider, func(string)) {
			return profiled(p, runtime.FullFake{Fake: runtime.NewFake()}), noScript
		}
	}
	t3 := func() (runtime.Provider, func(string)) {
		fake := runtime.NewFake()
		return profiled(runtime.ProfileT3Bridge, runtime.FullFake{Fake: fake}), func(name string) {
			fake.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{name: {v1TurnCaps}}
		}
	}
	wantT3 := "none none/none/none/none/false protocol interrupt"
	if _, ok := profiled(runtime.ProfileT3Bridge, runtime.FullFake{Fake: runtime.NewFake()}).(runtime.TurnObservationProvider); ok {
		wantT3 = "protocol recorded/recorded/checked/correlated/false protocol interrupt"
	}
	cases := []matrixCase{
		{
			name: "acp behind auto", provider: "claude", transport: "acp",
			build: func() (runtime.Provider, func(string)) {
				acpFake := runtime.NewFake()
				sp := auto.New(profiled(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), profiled(runtime.ProfileACP, runtime.FullFake{Fake: acpFake}))
				return sp, func(name string) {
					acpFake.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{name: {fullTurnCaps}}
				}
			},
			transcript: "claude",
			want:       "protocol recorded/recorded/atomic/exact/true keystroke interrupt",
		},
		{name: "tmux claude", provider: "claude", build: plain(runtime.ProfileTmux), transcript: "claude", want: "transcript recorded/inferred/checked/correlated/true keystroke interrupt"},
		{name: "tmux claude before its transcript", provider: "claude", build: plain(runtime.ProfileTmux), want: "none none/none/none/none/false keystroke interrupt"},
		{name: "tmux codex", provider: "codex", build: plain(runtime.ProfileTmux), transcript: "codex", want: "transcript recorded/recorded/checked/correlated/true keystroke interrupt"},
		{name: "tmux gemini", provider: "gemini", build: plain(runtime.ProfileTmux), transcript: "gemini", want: "transcript recorded/inferred/checked/correlated/true keystroke interrupt_reset"},
		{name: "subprocess claude", provider: "claude", build: plain(runtime.ProfileSubprocess), transcript: "claude", want: "transcript recorded/none/checked/correlated/true keystroke interrupt"},
		{name: "exec claude", provider: "claude", build: plain(runtime.ProfileExec), want: "none none/none/none/none/false keystroke interrupt"},
		{name: "k8s codex", provider: "codex", build: plain(runtime.ProfileK8s), want: "none none/none/none/none/false keystroke interrupt"},
		{name: "ssh gemini", provider: "gemini", build: plain(runtime.ProfileSSH), want: "none none/none/none/none/false keystroke interrupt_reset"},
		{name: "tmux pi", provider: "pi", build: plain(runtime.ProfileTmux), want: "none none/none/none/none/false keystroke restart"},
		{
			name: "unsupported mode on a transcript family", provider: "claude", transcript: "claude",
			build: func() (runtime.Provider, func(string)) {
				fake := runtime.NewFake()
				fake.TurnInterruptModeValue = runtime.TurnInterruptUnsupported
				return runtime.FullFake{Fake: fake}, noScript
			},
			want: "none none/none/none/none/false unsupported interrupt",
		},
		{
			name: "unsupported mode on a protocol runtime", provider: "claude",
			build: func() (runtime.Provider, func(string)) {
				fake := runtime.NewFake()
				fake.TurnInterruptModeValue = runtime.TurnInterruptUnsupported
				return runtime.FullFake{Fake: fake}, func(name string) {
					fake.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{name: {fullTurnCaps}}
				}
			},
			want: "protocol recorded/recorded/none/exact/true unsupported interrupt",
		},
		{
			name: "auto routed to tmux", provider: "claude", transcript: "claude",
			build: func() (runtime.Provider, func(string)) {
				return auto.New(profiled(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), profiled(runtime.ProfileACP, runtime.FullFake{Fake: runtime.NewFake()})), noScript
			},
			want: "transcript recorded/inferred/checked/correlated/true keystroke interrupt",
		},
		{name: "t3bridge", provider: "claude", build: t3, transcript: "claude", want: wantT3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp, script := tc.build()
			s := newTurnSession(t, sp, tc.provider, tc.transport)
			script(s.info.SessionName)
			if tc.transcript != "" {
				s.writeCompletedTurn(t, tc.transcript)
			}
			caps, err := s.handle.TurnCapabilities(context.Background())
			if err != nil {
				t.Fatalf("TurnCapabilities: %v", err)
			}
			if got := capsRow(caps); got != tc.want {
				t.Fatalf("TurnCapabilities = %q, want %q", got, tc.want)
			}
			if caps.HookTurnID != "" {
				t.Fatalf("HookTurnID = %q, want empty until the live arm verifies one", caps.HookTurnID)
			}
		})
	}
}

// The X11 §3.2 table, as composed for every production profile and the
// families whose transcripts record turns. A local runtime has its transcript
// on this host; exec, k8s and ssh run the agent elsewhere, so theirs is never
// found here. Rows that differ from X11 §3.2's literal text: subprocess has no
// idle snapshot, so Claude's and Gemini's inferred ends compose to none, as do
// herdr's, whose Codex transcript composes like tmux's.
func TestTurnCapabilitiesProductionProfileTable(t *testing.T) {
	none := func(replace string) string { return "none none/none/none/none/false keystroke " + replace }
	t3Interrupt := map[string]string{"claude": "interrupt", "codex": "interrupt", "gemini": "interrupt_reset", "pi": "restart"}
	t3Row := func(family string) string {
		if _, ok := profiled(runtime.ProfileT3Bridge, runtime.FullFake{Fake: runtime.NewFake()}).(runtime.TurnObservationProvider); ok {
			return "protocol recorded/recorded/checked/correlated/false protocol " + t3Interrupt[family]
		}
		return "none none/none/none/none/false protocol " + t3Interrupt[family]
	}
	table := []struct {
		profile runtime.Profile
		local   bool
		rows    map[string]string
	}{
		{runtime.ProfileTmux, true, map[string]string{
			"claude": "transcript recorded/inferred/checked/correlated/true keystroke interrupt",
			"codex":  "transcript recorded/recorded/checked/correlated/true keystroke interrupt",
			"gemini": "transcript recorded/inferred/checked/correlated/true keystroke interrupt_reset",
			"pi":     none("restart"),
		}},
		{runtime.ProfileACP, true, map[string]string{
			"claude": "protocol recorded/recorded/atomic/exact/true keystroke interrupt",
			"codex":  "protocol recorded/recorded/atomic/exact/true keystroke interrupt",
			"gemini": "protocol recorded/recorded/atomic/exact/true keystroke interrupt_reset",
			"pi":     "protocol recorded/recorded/atomic/exact/true keystroke restart",
		}},
		{runtime.ProfileSubprocess, true, map[string]string{
			"claude": "transcript recorded/none/checked/correlated/true keystroke interrupt",
			"codex":  "transcript recorded/recorded/checked/correlated/true keystroke interrupt",
			"gemini": "transcript recorded/none/checked/correlated/true keystroke interrupt_reset",
			"pi":     none("restart"),
		}},
		{runtime.ProfileHerdr, true, map[string]string{
			"claude": "transcript recorded/none/checked/correlated/true keystroke interrupt",
			"codex":  "transcript recorded/recorded/checked/correlated/true keystroke interrupt",
			"gemini": "transcript recorded/none/checked/correlated/true keystroke interrupt_reset",
			"pi":     none("restart"),
		}},
		{runtime.ProfileK8s, false, map[string]string{"claude": none("interrupt"), "codex": none("interrupt"), "gemini": none("interrupt_reset"), "pi": none("restart")}},
		{runtime.ProfileExec, false, map[string]string{"claude": none("interrupt"), "codex": none("interrupt"), "gemini": none("interrupt_reset"), "pi": none("restart")}},
		{runtime.ProfileSSH, false, map[string]string{"claude": none("interrupt"), "codex": none("interrupt"), "gemini": none("interrupt_reset"), "pi": none("restart")}},
		{runtime.ProfileT3Bridge, true, map[string]string{"claude": t3Row("claude"), "codex": t3Row("codex"), "gemini": t3Row("gemini"), "pi": t3Row("pi")}},
	}
	for _, row := range table {
		for _, family := range []string{"claude", "codex", "gemini", "pi"} {
			t.Run(row.profile.String()+"/"+family, func(t *testing.T) {
				fake := runtime.NewFake()
				s := newTurnSession(t, profiled(row.profile, runtime.FullFake{Fake: fake}), family, "")
				scripted := fullTurnCaps
				if row.profile == runtime.ProfileT3Bridge {
					scripted = v1TurnCaps
				}
				fake.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{s.info.SessionName: {scripted}}
				if row.local {
					s.writeCompletedTurn(t, family)
				}
				caps, err := s.handle.TurnCapabilities(context.Background())
				if err != nil {
					t.Fatalf("TurnCapabilities: %v", err)
				}
				if got := capsRow(caps); got != row.rows[family] {
					t.Fatalf("composed row = %q, want %q", got, row.rows[family])
				}
			})
		}
	}
}

func TestComposeTurnCapabilitiesNormalizesRuntimeAxes(t *testing.T) {
	support := sessionlog.TurnSupportFor("claude")
	// A runtime's unknown axis values read as none, so a declaration naming
	// only unknown values is no declaration and the transcript decides.
	caps := composeTurnCapabilities(runtime.TurnCapabilities{Start: "someday"}, runtime.TurnInterruptKeystroke, support, true, true)
	if caps.Source != TurnSourceTranscript {
		t.Fatalf("Source = %q, want transcript", caps.Source)
	}
	caps = composeTurnCapabilities(runtime.TurnCapabilities{Start: runtime.TurnStartRecorded, End: "someday"}, runtime.TurnInterruptKeystroke, support, true, true)
	if caps.Source != TurnSourceProtocol || caps.End != runtime.TurnEndNone || caps.Fence != runtime.TurnFenceNone {
		t.Fatalf("composed = %q, want protocol with unknown axes as none", capsRow(caps))
	}
}

func TestCurrentTurnReadsClaudeTranscripts(t *testing.T) {
	cases := []struct {
		name    string
		lines   []string
		want    func(t *testing.T) runtime.TurnHistory
		wantErr error
	}{
		{
			name: "completed",
			lines: []string{
				claudePrompt(claudeTurn1, "2026-10-13T05:00:00.123Z"), claudeReply("2026-10-13T05:00:04.000Z"), claudeTurnDuration("2026-10-13T05:00:05.456Z"),
				claudePrompt(claudeTurn2, "2026-10-13T05:01:00.789Z"), claudeReply("2026-10-13T05:01:02.000Z"), claudeTurnDuration("2026-10-13T05:01:03.001Z"),
			},
			want: func(t *testing.T) runtime.TurnHistory {
				return runtime.TurnHistory{Recent: []runtime.EndedTurn{
					{TurnID: claudeTurn2, StartedAt: mustTurnTime(t, "2026-10-13T05:01:00.789Z"), EndedAt: mustTurnTime(t, "2026-10-13T05:01:03.001Z"), Status: runtime.TurnStatusCompleted},
					{TurnID: claudeTurn1, StartedAt: mustTurnTime(t, "2026-10-13T05:00:00.123Z"), EndedAt: mustTurnTime(t, "2026-10-13T05:00:05.456Z"), Status: runtime.TurnStatusCompleted},
				}}
			},
		},
		{
			// The interrupt marker carries the next prompt's id: it closes the
			// open turn as canceled and is never the next turn's.
			name: "interrupt with the next prompt's id",
			lines: []string{
				claudePrompt(claudeTurn1, "2026-10-13T05:00:00.123Z"),
				claudeInterrupt(claudeTurn2, "2026-10-13T05:00:10.000Z"),
				claudePrompt(claudeTurn2, "2026-10-13T05:00:10.050Z"),
			},
			want: func(t *testing.T) runtime.TurnHistory {
				return runtime.TurnHistory{
					Current:      runtime.TurnState{TurnID: claudeTurn2},
					CurrentStart: mustTurnTime(t, "2026-10-13T05:00:10.050Z"),
					Recent: []runtime.EndedTurn{
						{TurnID: claudeTurn1, StartedAt: mustTurnTime(t, "2026-10-13T05:00:00.123Z"), EndedAt: mustTurnTime(t, "2026-10-13T05:00:10.000Z"), Status: runtime.TurnStatusCancelled},
					},
				}
			},
		},
		{
			name:    "torn tail",
			lines:   []string{claudePrompt(claudeTurn1, "2026-10-13T05:00:00.123Z"), claudeReply("2026-10-13T05:00:04.000Z"), claudeTurnDuration("2026-10-13T05:00:05.456Z"), `{"type":"user","timestamp":"2026-10-13T05:01`},
			wantErr: runtime.ErrRuntimeUnavailable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := newManualTurnClock(time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC))
			s := newTurnSession(t, profiled(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), "claude", "")
			s.handle.turns.timing = clock.timing()
			path := s.writeTranscript(t, "claude", tc.lines...)
			ageTranscript(t, path, clock.now()) // not quiet: the overlay stays out of it
			assertCurrentTurn(t, s.handle, clock.now(), tc.want, tc.wantErr)
		})
	}
}

func TestCurrentTurnReadsCodexTranscripts(t *testing.T) {
	cases := []struct {
		name    string
		lines   []string
		want    func(t *testing.T) runtime.TurnHistory
		wantErr error
	}{
		{
			// StartedAt is the task_started line's envelope timestamp, never its
			// whole-second started_at.
			name: "started and complete",
			lines: []string{
				codexStarted(codexTurn1, "2026-10-08T02:22:22.191Z", 1791426142), codexComplete(codexTurn1, "2026-10-08T02:22:30.500Z"),
				codexStarted(codexTurn2, "2026-10-08T02:23:01.007Z", 1791426181),
			},
			want: func(t *testing.T) runtime.TurnHistory {
				return runtime.TurnHistory{
					Current:      runtime.TurnState{TurnID: codexTurn2},
					CurrentStart: mustTurnTime(t, "2026-10-08T02:23:01.007Z"),
					Recent: []runtime.EndedTurn{
						{TurnID: codexTurn1, StartedAt: mustTurnTime(t, "2026-10-08T02:22:22.191Z"), EndedAt: mustTurnTime(t, "2026-10-08T02:22:30.500Z"), Status: runtime.TurnStatusCompleted},
					},
				}
			},
		},
		{
			name:  "aborted",
			lines: []string{codexStarted(codexTurn1, "2026-10-08T02:22:22.191Z", 1791426142), codexAborted(codexTurn1, "2026-10-08T02:22:27.401Z")},
			want: func(t *testing.T) runtime.TurnHistory {
				return runtime.TurnHistory{Recent: []runtime.EndedTurn{
					{TurnID: codexTurn1, StartedAt: mustTurnTime(t, "2026-10-08T02:22:22.191Z"), EndedAt: mustTurnTime(t, "2026-10-08T02:22:27.401Z"), Status: runtime.TurnStatusCancelled},
				}}
			},
		},
		{
			name:    "torn tail",
			lines:   []string{codexStarted(codexTurn1, "2026-10-08T02:22:22.191Z", 1791426142), `{"timestamp":"2026-10-08T02:22:30.500Z","type":"event_msg","payload":{"type":"task_comp`},
			wantErr: runtime.ErrRuntimeUnavailable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idle := newIdleScript()
			clock := newManualTurnClock(time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC))
			s := newTurnSession(t, profiled(runtime.ProfileTmux, idle.FullFake), "codex", "")
			s.handle.turns.timing = clock.timing()
			path := s.writeTranscript(t, "codex", tc.lines...)
			ageTranscript(t, path, clock.now().Add(-time.Hour))
			assertCurrentTurn(t, s.handle, clock.now(), tc.want, tc.wantErr)
		})
	}
}

func TestCurrentTurnReadsGeminiTranscripts(t *testing.T) {
	want := func(t *testing.T) runtime.TurnHistory {
		return runtime.TurnHistory{
			Current:      runtime.TurnState{TurnID: geminiTurn2},
			CurrentStart: mustTurnTime(t, "2026-06-21T17:09:00.123Z"),
			Recent: []runtime.EndedTurn{
				{TurnID: geminiTurn1, StartedAt: mustTurnTime(t, "2026-06-21T17:08:00.694Z"), EndedAt: mustTurnTime(t, "2026-06-21T17:09:00.123Z")},
			},
		}
	}
	lines := []string{
		geminiUser(geminiTurn1, "2026-06-21T17:08:00.694Z"), geminiReply("a1", "2026-06-21T17:08:10.000Z"),
		geminiUser(geminiTurn2, "2026-06-21T17:09:00.123Z"), geminiReply("a2", "2026-06-21T17:09:05.000Z"),
	}
	for _, form := range []string{"gemini", "gemini-jsonl"} {
		t.Run(form, func(t *testing.T) {
			clock := newManualTurnClock(time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC))
			s := newTurnSession(t, profiled(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), "gemini", "")
			s.handle.turns.timing = clock.timing()
			recordLines := lines
			if form == "gemini-jsonl" {
				recordLines = append([]string{`{"sessionId":"g-1","projectHash":"project","startTime":"2026-06-21T17:08:00.693Z","kind":"main"}`}, lines...)
			}
			path := s.writeTranscript(t, form, recordLines...)
			ageTranscript(t, path, clock.now())
			assertCurrentTurn(t, s.handle, clock.now(), want, nil)
		})
	}
}

func assertCurrentTurn(t *testing.T, h TurnHandle, observedAt time.Time, want func(t *testing.T) runtime.TurnHistory, wantErr error) {
	t.Helper()
	got, err := h.CurrentTurn(context.Background())
	if wantErr != nil {
		if !errors.Is(err, wantErr) {
			t.Fatalf("CurrentTurn error = %v, want %v", err, wantErr)
		}
		if got.Current.Active() || len(got.Recent) != 0 || !got.ObservedAt.IsZero() {
			t.Fatalf("CurrentTurn = %+v with its error, want the zero history (never idle)", got)
		}
		return
	}
	if err != nil {
		t.Fatalf("CurrentTurn: %v", err)
	}
	w := want(t)
	w.ObservedAt = observedAt
	assertTurnHistory(t, got, w)
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

func TestTurnHistoryFromScan(t *testing.T) {
	at := time.Date(2026, 10, 13, 5, 0, 0, 123456789, time.UTC)
	observed := time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC)
	t.Run("ids verbatim, millisecond times, statuses mapped", func(t *testing.T) {
		scan := sessionlog.TurnScan{
			Open:    true,
			Current: sessionlog.ScannedTurn{ID: "open", StartedAt: at},
			Recent: []sessionlog.ScannedTurn{
				{ID: "failed", StartedAt: at, EndedAt: at, Status: sessionlog.TurnStatusFailed},
				{ID: "closed-by-next", StartedAt: at, EndedAt: at},
			},
		}
		got, err := turnHistoryFromScan(scan, observed)
		if err != nil {
			t.Fatalf("turnHistoryFromScan: %v", err)
		}
		ms := at.Truncate(time.Millisecond)
		if got.Current.TurnID != "open" || !got.CurrentStart.Equal(ms) || !got.ObservedAt.Equal(observed) {
			t.Fatalf("current = %+v", got)
		}
		if got.Recent[0].Status != runtime.TurnStatusFailed || got.Recent[1].Status != "" || !got.Recent[0].StartedAt.Equal(ms) {
			t.Fatalf("recent = %+v", got.Recent)
		}
	})
	t.Run("an ended turn without an id is left out", func(t *testing.T) {
		got, err := turnHistoryFromScan(sessionlog.TurnScan{Recent: []sessionlog.ScannedTurn{{EndedAt: at}, {ID: "kept", EndedAt: at}}, MissingIDs: true}, observed)
		if err != nil || len(got.Recent) != 1 || got.Recent[0].TurnID != "kept" {
			t.Fatalf("turnHistoryFromScan = %+v, %v; want only the identified turn", got, err)
		}
	})
	t.Run("an open turn without an id is not idle", func(t *testing.T) {
		_, err := turnHistoryFromScan(sessionlog.TurnScan{Open: true, MissingIDs: true}, observed)
		if !errors.Is(err, runtime.ErrTurnIdentityUnsupported) {
			t.Fatalf("turnHistoryFromScan error = %v, want ErrTurnIdentityUnsupported", err)
		}
	})
}

func TestCurrentTurnUnreadableTranscriptIsUnavailable(t *testing.T) {
	s := newTurnSession(t, profiled(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), "codex", "")
	path := s.writeCompletedTurn(t, "codex")
	plan, err := s.handle.planTurns(context.Background())
	if err != nil || plan.caps.Source != TurnSourceTranscript {
		t.Fatalf("planTurns = %+v, %v; want a transcript source", plan, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	// The plan was made while the transcript resolved; the read finds it gone.
	_, err = s.handle.transcriptTurnHistory(context.Background(), plan, func() (sessionlog.TurnScan, error) {
		return s.handle.adapter.ScanTurns(plan.provider, plan.path)
	}, true)
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("read of a vanished transcript = %v, want ErrRuntimeUnavailable", err)
	}
}

func TestCurrentTurnWithoutSourceIsUnsupported(t *testing.T) {
	s := newTurnSession(t, profiled(runtime.ProfileExec, runtime.FullFake{Fake: runtime.NewFake()}), "claude", "")
	if _, err := s.handle.CurrentTurn(context.Background()); !errors.Is(err, runtime.ErrTurnIdentityUnsupported) {
		t.Fatalf("CurrentTurn = %v, want ErrTurnIdentityUnsupported", err)
	}
	if _, err := s.handle.WatchTurns(context.Background()); !errors.Is(err, runtime.ErrTurnIdentityUnsupported) {
		t.Fatalf("WatchTurns = %v, want ErrTurnIdentityUnsupported", err)
	}
}

func TestCurrentTurnProtocolSourceReadsTheRuntime(t *testing.T) {
	fake := runtime.NewFake()
	s := newTurnSession(t, profiled(runtime.ProfileACP, runtime.FullFake{Fake: fake}), "claude", "")
	at := time.Date(2026, 10, 13, 5, 0, 0, 0, time.UTC)
	want := runtime.TurnHistory{Current: runtime.TurnState{TurnID: "acp-1"}, CurrentStart: at, ObservedAt: at}
	fake.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{s.info.SessionName: {fullTurnCaps}}
	fake.TurnHistoryScripts = map[string][]runtime.TurnHistory{s.info.SessionName: {want}}
	got, err := s.handle.CurrentTurn(context.Background())
	if err != nil {
		t.Fatalf("CurrentTurn: %v", err)
	}
	assertTurnHistory(t, got, want)

	fake.TurnHistoryErrors = map[string][]error{s.info.SessionName: {runtime.ErrRuntimeUnavailable}}
	if _, err := s.handle.CurrentTurn(context.Background()); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("CurrentTurn on a failed read = %v, want ErrRuntimeUnavailable", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.handle.CurrentTurn(ctx); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("CurrentTurn under an ended ctx = %v, want ErrRuntimeUnavailable (bounded, unanswered)", err)
	}
}

// The idle overlay (X11 §5.2): after 2 s of quiet, two idle readings with no
// growth between them end the open turn, inferred, at the observer's clock.
func TestCurrentTurnIdleOverlay(t *testing.T) {
	growErr := errors.New("pane unreadable")
	cases := []struct {
		name      string
		quiet     time.Duration
		answers   []idleAnswer
		grow      bool // the transcript grows before the second reading
		wantEnded bool
		wantCalls int
	}{
		{name: "quiet and idle twice", quiet: 3 * time.Second, answers: []idleAnswer{{idle: true}, {idle: true}}, wantEnded: true, wantCalls: 2},
		{name: "not quiet long enough", quiet: time.Second, answers: []idleAnswer{{idle: true}}, wantCalls: 0},
		{name: "busy reading", quiet: 3 * time.Second, answers: []idleAnswer{{idle: true}, {idle: false}}, wantCalls: 2},
		{name: "first reading busy", quiet: 3 * time.Second, answers: []idleAnswer{{idle: false}}, wantCalls: 1},
		{name: "snapshot error", quiet: 3 * time.Second, answers: []idleAnswer{{idle: true, err: growErr}}, wantCalls: 1},
		{name: "growth between the readings", quiet: 3 * time.Second, answers: []idleAnswer{{idle: true}, {idle: true}}, grow: true, wantCalls: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Composing the capabilities takes one snapshot first, to learn
			// whether the runtime takes them; its answer does not matter.
			idle := newIdleScript(append([]idleAnswer{{idle: false}}, tc.answers...)...)
			tc.wantCalls++
			clock := newManualTurnClock(time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC))
			s := newTurnSession(t, runtime.NewFakeProfile(runtime.ProfileTmux, idle), "claude", "")
			s.handle.turns.timing = clock.timing()
			path := s.writeTranscript(t, "claude", claudePrompt(claudeTurn1, "2026-10-13T05:59:50.123Z"))
			ageTranscript(t, path, clock.now().Add(-tc.quiet))
			if tc.grow {
				idle.state.before = func(call int) {
					if call == 3 {
						appendTranscript(t, path, claudeReply("2026-10-13T05:59:59.000Z")+"\n")
					}
				}
			}
			got, err := s.handle.CurrentTurn(context.Background())
			if err != nil {
				t.Fatalf("CurrentTurn: %v", err)
			}
			if calls := idle.idleCalls(); calls != tc.wantCalls {
				t.Fatalf("SnapshotIdle calls = %d, want %d", calls, tc.wantCalls)
			}
			if !tc.wantEnded {
				if got.Current.TurnID != claudeTurn1 || len(got.Recent) != 0 {
					t.Fatalf("history = %+v, want the turn still open", got)
				}
				return
			}
			if got.Current.Active() || len(got.Recent) != 1 {
				t.Fatalf("history = %+v, want the turn ended", got)
			}
			ended := got.Recent[0]
			if ended.TurnID != claudeTurn1 || !ended.Inferred || ended.Status != "" || !ended.EndedAt.Equal(clock.now()) ||
				!ended.StartedAt.Equal(mustTurnTime(t, "2026-10-13T05:59:50.123Z")) {
				t.Fatalf("ended = %+v, want %s inferred at the observer clock", ended, claudeTurn1)
			}
			// The unchanged transcript keeps the inferred end without new
			// readings, at the same EndedAt.
			clock.advance(time.Minute)
			again, err := s.handle.CurrentTurn(context.Background())
			if err != nil || len(again.Recent) != 1 || !again.Recent[0].EndedAt.Equal(ended.EndedAt) || idle.idleCalls() != tc.wantCalls {
				t.Fatalf("second read = %+v, %v (calls %d); want the same inferred end", again, err, idle.idleCalls())
			}
			// New entries reopen it.
			appendTranscript(t, path, claudeReply("2026-10-13T06:00:30.000Z")+"\n")
			reopened, err := s.handle.CurrentTurn(context.Background())
			if err != nil || reopened.Current.TurnID != claudeTurn1 {
				t.Fatalf("after growth = %+v, %v; want the turn open again", reopened, err)
			}
		})
	}
}

func TestCurrentTurnOverlaySkipsRecordedEnds(t *testing.T) {
	idle := newIdleScript()
	clock := newManualTurnClock(time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC))
	s := newTurnSession(t, runtime.NewFakeProfile(runtime.ProfileTmux, idle), "codex", "")
	s.handle.turns.timing = clock.timing()
	path := s.writeTranscript(t, "codex", codexStarted(codexTurn1, "2026-10-08T02:22:22.191Z", 1791426142))
	ageTranscript(t, path, clock.now().Add(-time.Hour))
	got, err := s.handle.CurrentTurn(context.Background())
	if err != nil || got.Current.TurnID != codexTurn1 {
		t.Fatalf("CurrentTurn = %+v, %v; want the open turn", got, err)
	}
	// Codex records every end: composing its capabilities takes no snapshot,
	// and neither does the read.
	if calls := idle.idleCalls(); calls != 0 {
		t.Fatalf("SnapshotIdle calls = %d, want 0 for a family that records its ends", calls)
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
	deadline := time.After(10 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("watch did not close after ctx ended")
		}
	}
}

func TestWatchTurnsTranscript(t *testing.T) {
	idle := newIdleScript(idleAnswer{idle: false})
	clock := newManualTurnClock(time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC))
	s := newTurnSession(t, runtime.NewFakeProfile(runtime.ProfileTmux, idle), "claude", "")
	s.handle.turns.timing = clock.timing()
	path := s.writeTranscript(t, "claude", claudePrompt(claudeTurn1, "2026-10-13T05:00:00.123Z"), claudeTurnDuration("2026-10-13T05:00:05.456Z"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := s.handle.WatchTurns(ctx)
	if err != nil {
		t.Fatalf("WatchTurns: %v", err)
	}

	// Resync first: the full history, idle.
	first := receiveTurnUpdate(t, ch)
	if first.Err != nil || first.History.Current.Active() || len(first.History.Recent) != 1 || first.History.Recent[0].TurnID != claudeTurn1 {
		t.Fatalf("first update = %+v, want the history with %s ended", first, claudeTurn1)
	}
	if ask := clock.nextAsk(t); ask.d != turnPollIdle {
		t.Fatalf("idle wait = %s, want %s", ask.d, turnPollIdle)
	} else {
		// A poll with nothing new sends nothing.
		ask.fire <- clock.now()
	}
	ask := clock.nextAsk(t)
	assertNoPendingUpdate(t, ch)

	// Coalescing: two changes the reader has not taken yet arrive as the
	// latest one.
	appendTranscript(t, path, claudePrompt(claudeTurn2, "2026-10-13T05:01:00.789Z")+"\n")
	ask.fire <- clock.now()
	ask = clock.nextAsk(t)
	if ask.d != turnOverlayEvery {
		t.Fatalf("open-turn wait = %s, want the overlay's %s", ask.d, turnOverlayEvery)
	}
	appendTranscript(t, path, claudeTurnDuration("2026-10-13T05:01:03.001Z")+"\n")
	ask.fire <- clock.now()
	ask = clock.nextAsk(t)
	latest := receiveTurnUpdate(t, ch)
	if latest.Err != nil || latest.History.Current.Active() || len(latest.History.Recent) != 2 || latest.History.Recent[0].TurnID != claudeTurn2 {
		t.Fatalf("coalesced update = %+v, want %s ended", latest, claudeTurn2)
	}
	assertNoPendingUpdate(t, ch)

	// A torn tail is an error update, never an idle history.
	appendTranscript(t, path, `{"type":"user","timestamp":"2026-10-13T05:02`)
	ask.fire <- clock.now()
	ask = clock.nextAsk(t)
	torn := receiveTurnUpdate(t, ch)
	if !errors.Is(torn.Err, runtime.ErrRuntimeUnavailable) || torn.History.Current.Active() || len(torn.History.Recent) != 0 {
		t.Fatalf("torn update = %+v, want an ErrRuntimeUnavailable update with no history", torn)
	}
	if ask.d != turnPollOpen {
		t.Fatalf("wait while failing = %s, want %s", ask.d, turnPollOpen)
	}
	// Completing the record recovers with a full history.
	appendTranscript(t, path, `:00.000Z","message":{"role":"user","content":"a prompt"},"promptId":"`+claudeTurn1+`x","origin":"user","promptSource":"prompt","isSidechain":false}`+"\n")
	ask.fire <- clock.now()
	ask = clock.nextAsk(t)
	recovered := receiveTurnUpdate(t, ch)
	if recovered.Err != nil || recovered.History.Current.TurnID != claudeTurn1+"x" {
		t.Fatalf("recovered update = %+v, want the new open turn", recovered)
	}

	// Rotation: the file is replaced by a shorter one; the follower rescans.
	replacement := path + ".new"
	writeTranscriptLines(t, replacement, claudePrompt(claudeTurn2, "2026-10-13T07:00:00.000Z"), claudeTurnDuration("2026-10-13T07:00:01.000Z"))
	if err := os.Rename(replacement, path); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	ask.fire <- clock.now()
	clock.nextAsk(t)
	rotated := receiveTurnUpdate(t, ch)
	if rotated.Err != nil || rotated.History.Current.Active() || len(rotated.History.Recent) != 1 || rotated.History.Recent[0].TurnID != claudeTurn2 ||
		!rotated.History.Recent[0].StartedAt.Equal(mustTurnTime(t, "2026-10-13T07:00:00.000Z")) {
		t.Fatalf("rotated update = %+v, want only the replacement's turn", rotated)
	}

	cancel()
	assertWatchCloses(t, ch)
}

func TestWatchTurnsTranscriptOverlayEndsAQuietTurn(t *testing.T) {
	idle := newIdleScript(idleAnswer{idle: true})
	clock := newManualTurnClock(time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC))
	s := newTurnSession(t, runtime.NewFakeProfile(runtime.ProfileTmux, idle), "claude", "")
	timing := clock.timing()
	s.handle.turns.timing = timing
	path := s.writeTranscript(t, "claude", claudePrompt(claudeTurn1, "2026-10-13T05:59:59.000Z"))
	ageTranscript(t, path, clock.now())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := s.handle.WatchTurns(ctx)
	if err != nil {
		t.Fatalf("WatchTurns: %v", err)
	}
	if first := receiveTurnUpdate(t, ch); first.History.Current.TurnID != claudeTurn1 {
		t.Fatalf("first update = %+v, want the open turn", first)
	}
	ask := clock.nextAsk(t)
	clock.advance(3 * time.Second)
	ask.fire <- clock.now()
	clock.nextAsk(t)
	ended := receiveTurnUpdate(t, ch)
	if ended.Err != nil || ended.History.Current.Active() || len(ended.History.Recent) != 1 || !ended.History.Recent[0].Inferred {
		t.Fatalf("update = %+v, want the quiet turn ended, inferred", ended)
	}
}

// turnScriptBackend is a FullFake whose TurnHistory and turn events the test
// drives.
type turnScriptBackend struct {
	runtime.FullFake
	state *turnScriptState
}

type turnScriptState struct {
	mu     sync.Mutex
	hist   runtime.TurnHistory
	err    error
	reads  int
	events chan runtime.TurnEvent // nil: no turn events
}

func newTurnScriptBackend(events bool) turnScriptBackend {
	b := turnScriptBackend{FullFake: runtime.FullFake{Fake: runtime.NewFake()}, state: &turnScriptState{}}
	if events {
		b.state.events = make(chan runtime.TurnEvent, 8)
	}
	return b
}

func (b turnScriptBackend) TurnCapabilities(string) runtime.TurnCapabilities { return fullTurnCaps }

func (b turnScriptBackend) TurnHistory(context.Context, string) (runtime.TurnHistory, error) {
	b.state.mu.Lock()
	defer b.state.mu.Unlock()
	b.state.reads++
	if b.state.err != nil {
		return runtime.TurnHistory{}, b.state.err
	}
	h := b.state.hist
	h.ObservedAt = time.Now()
	return h, nil
}

func (b turnScriptBackend) SubscribeTurnEvents(ctx context.Context) (<-chan runtime.TurnEvent, error) {
	if b.state.events == nil {
		return nil, runtime.ErrInteractionUnsupported
	}
	out := make(chan runtime.TurnEvent)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-b.state.events:
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

func (b turnScriptBackend) set(h runtime.TurnHistory, err error) {
	b.state.mu.Lock()
	defer b.state.mu.Unlock()
	b.state.hist, b.state.err = h, err
}

func TestWatchTurnsProtocolHintTriggersARead(t *testing.T) {
	backend := newTurnScriptBackend(true)
	clock := newManualTurnClock(time.Now())
	s := newTurnSession(t, runtime.NewFakeProfile(runtime.ProfileACP, backend), "claude", "")
	s.handle.turns.timing = clock.timing()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := s.handle.WatchTurns(ctx)
	if err != nil {
		t.Fatalf("WatchTurns: %v", err)
	}
	if first := receiveTurnUpdate(t, ch); first.Err != nil || first.History.Current.Active() {
		t.Fatalf("first update = %+v, want the idle history", first)
	}
	if ask := clock.nextAsk(t); ask.d != turnPollIdle {
		t.Fatalf("idle wait with hints = %s, want %s", ask.d, turnPollIdle)
	}
	backend.set(runtime.TurnHistory{Current: runtime.TurnState{TurnID: "acp-1"}}, nil)
	backend.state.events <- runtime.TurnEvent{Kind: runtime.TurnEventStarted, Session: s.info.SessionName, TurnID: "acp-1"}
	if got := receiveTurnUpdate(t, ch); got.History.Current.TurnID != "acp-1" {
		t.Fatalf("update after the hint = %+v, want acp-1 running", got)
	}
	if ask := clock.nextAsk(t); ask.d != turnPollOpen {
		t.Fatalf("open-turn wait = %s, want %s", ask.d, turnPollOpen)
	}
	cancel()
	assertWatchCloses(t, ch)
}

func TestWatchTurnsProtocolPollAloneDeliversEveryChange(t *testing.T) {
	backend := newTurnScriptBackend(false)
	clock := newManualTurnClock(time.Now())
	s := newTurnSession(t, runtime.NewFakeProfile(runtime.ProfileACP, backend), "claude", "")
	s.handle.turns.timing = clock.timing()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := s.handle.WatchTurns(ctx)
	if err != nil {
		t.Fatalf("WatchTurns: %v", err)
	}
	receiveTurnUpdate(t, ch)
	at := time.Date(2026, 10, 13, 5, 0, 0, 0, time.UTC)
	steps := []struct {
		hist runtime.TurnHistory
		err  error
	}{
		{hist: runtime.TurnHistory{Current: runtime.TurnState{TurnID: "t1"}, CurrentStart: at}},
		{err: fmt.Errorf("owner gone: %w", runtime.ErrRuntimeUnavailable)},
		{hist: runtime.TurnHistory{Recent: []runtime.EndedTurn{{TurnID: "t1", StartedAt: at, EndedAt: at.Add(time.Second), Status: runtime.TurnStatusCompleted}}}},
		{hist: runtime.TurnHistory{Current: runtime.TurnState{TurnID: "t2"}, Recent: []runtime.EndedTurn{{TurnID: "t1", StartedAt: at, EndedAt: at.Add(time.Second), Status: runtime.TurnStatusCompleted}}}},
	}
	for i, step := range steps {
		ask := clock.nextAsk(t)
		if ask.d != turnPollOpen {
			t.Fatalf("step %d: wait without hints = %s, want %s", i, ask.d, turnPollOpen)
		}
		backend.set(step.hist, step.err)
		ask.fire <- clock.now()
		got := receiveTurnUpdate(t, ch)
		if step.err != nil {
			if !errors.Is(got.Err, runtime.ErrRuntimeUnavailable) || got.History.Current.Active() {
				t.Fatalf("step %d: update = %+v, want the error, never idle", i, got)
			}
			continue
		}
		if got.Err != nil || got.History.Current != step.hist.Current || len(got.History.Recent) != len(step.hist.Recent) {
			t.Fatalf("step %d: update = %+v, want %+v", i, got, step.hist)
		}
	}
	cancel()
	assertWatchCloses(t, ch)
}

func TestPublishTurnUpdateLatestWins(t *testing.T) {
	out := make(chan TurnUpdate, 1)
	publishTurnUpdate(out, TurnUpdate{History: runtime.TurnHistory{Current: runtime.TurnState{TurnID: "a"}}})
	publishTurnUpdate(out, TurnUpdate{History: runtime.TurnHistory{Current: runtime.TurnState{TurnID: "b"}}})
	if got := <-out; got.History.Current.TurnID != "b" {
		t.Fatalf("published = %+v, want the latest", got)
	}
	assertNoPendingUpdate(t, out)
}

// Interrupts keep today's paths: a session-scoped interrupt still calls
// StopTurn's sequence on a session whose turns are observed.
func TestTurnObservationLeavesInterruptUnchanged(t *testing.T) {
	fake := runtime.NewFake()
	s := newTurnSession(t, profiled(runtime.ProfileTmux, runtime.FullFake{Fake: fake}), "claude", "")
	s.writeCompletedTurn(t, "claude")
	if _, err := s.handle.CurrentTurn(context.Background()); err != nil {
		t.Fatalf("CurrentTurn: %v", err)
	}
	fake.Calls = nil
	if err := s.handle.Interrupt(context.Background(), InterruptRequest{}); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if lastCall(fake.Calls, "Interrupt") == nil {
		t.Fatalf("calls = %+v, want the keystroke interrupt StopTurn sends", fake.Calls)
	}
}

// With fsnotify on, a write wakes the watcher before any poll fires.
func TestWatchTurnsTranscriptWakesOnWrite(t *testing.T) {
	clock := newManualTurnClock(time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC))
	s := newTurnSession(t, profiled(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), "codex", "")
	timing := clock.timing()
	timing.noNotify = false
	s.handle.turns.timing = timing
	path := s.writeCompletedTurn(t, "codex")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := s.handle.WatchTurns(ctx)
	if err != nil {
		t.Fatalf("WatchTurns: %v", err)
	}
	if first := receiveTurnUpdate(t, ch); first.Err != nil || first.History.Current.Active() {
		t.Fatalf("first update = %+v, want the idle history", first)
	}
	clock.nextAsk(t) // the poll is never fired
	appendTranscript(t, path, codexStarted(codexTurn2, "2026-10-08T02:23:01.007Z", 1791426181)+"\n")
	if got := receiveTurnUpdate(t, ch); got.History.Current.TurnID != codexTurn2 {
		t.Fatalf("update after the write = %+v, want %s running", got, codexTurn2)
	}
	cancel()
	assertWatchCloses(t, ch)
}
