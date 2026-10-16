package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

// unsupportedInterruptLocal is a local backend that cannot interrupt its
// session's turn.
type unsupportedInterruptLocal struct{ runtime.Provider }

func (unsupportedInterruptLocal) TurnInterruptMode(string) runtime.TurnInterruptMode {
	return runtime.TurnInterruptUnsupported
}

func (unsupportedInterruptLocal) LocalTranscripts() bool { return true }

// The composition matrix over profiled fakes crossed with families (X11 §5.2,
// TI-2b acceptance). Every session is keyed as production runs it, and every
// transcript case writes the conversation's own transcript, so a row is the
// session's, not whatever file the fixture left.
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
	tmuxClaude := "transcript recorded/inferred/checked/correlated/true keystroke interrupt"
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
		{name: "tmux claude", provider: "claude", build: plain(runtime.ProfileTmux), transcript: "claude", want: tmuxClaude},
		// Capabilities are traits: the transcript not written yet does not
		// lower them (CurrentTurn says unavailable instead).
		{name: "tmux claude before its transcript", provider: "claude", build: plain(runtime.ProfileTmux), want: tmuxClaude},
		{name: "tmux codex", provider: "codex", build: plain(runtime.ProfileTmux), transcript: "codex", want: "transcript recorded/recorded/checked/correlated/true keystroke interrupt"},
		// Gemini has no lookup that finds one conversation's chat by its key,
		// so no chat file can be attributed to the session.
		{name: "tmux gemini", provider: "gemini", build: plain(runtime.ProfileTmux), transcript: "gemini", want: "none none/none/none/none/false keystroke interrupt_reset"},
		{name: "subprocess claude", provider: "claude", build: plain(runtime.ProfileSubprocess), transcript: "claude", want: "transcript recorded/none/checked/correlated/true keystroke interrupt"},
		// A remote runtime's transcript is never read here, even when a file
		// resolves for its work directory on this host.
		{name: "exec claude", provider: "claude", build: plain(runtime.ProfileExec), transcript: "claude", want: "none none/none/none/none/false keystroke interrupt"},
		{name: "k8s codex", provider: "codex", build: plain(runtime.ProfileK8s), transcript: "codex", want: "none none/none/none/none/false keystroke interrupt"},
		{name: "ssh claude", provider: "claude", build: plain(runtime.ProfileSSH), transcript: "claude", want: "none none/none/none/none/false keystroke interrupt"},
		{name: "tmux pi", provider: "pi", build: plain(runtime.ProfileTmux), want: "none none/none/none/none/false keystroke restart"},
		{
			name: "unsupported mode on a local transcript family", provider: "claude", transcript: "claude",
			build: func() (runtime.Provider, func(string)) {
				return unsupportedInterruptLocal{runtime.FullFake{Fake: runtime.NewFake()}}, noScript
			},
			want: "none none/none/none/none/false unsupported none",
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
			want: "protocol recorded/recorded/none/exact/true unsupported none",
		},
		{
			name: "auto routed to tmux", provider: "claude", transcript: "claude",
			build: func() (runtime.Provider, func(string)) {
				return auto.New(profiled(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), profiled(runtime.ProfileACP, runtime.FullFake{Fake: runtime.NewFake()})), noScript
			},
			want: tmuxClaude,
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
// families whose transcripts record turns, with each session's own transcript
// written on this host for every profile. Rows that differ from X11 §3.2's
// literal text (TI-2b corrections):
//   - herdr runs the agent on this host and declares local transcripts, so
//     Claude and Codex on herdr compose to the transcript tier, where X11
//     §3.2's herdr row reads "none for ids";
//   - subprocess and herdr take no idle snapshot, so Claude's inferred end
//     composes to none there;
//   - Gemini is none everywhere but protocol runtimes: no lookup finds one
//     conversation's chat by its key, and the work-directory fallback can
//     return another conversation's;
//   - exec, k8s and ssh are none because they declare no local transcripts
//     (runtime.LocalTranscriptProvider), not because no file resolves;
//   - Replace is interrupt on every protocol source and protocol-mode
//     runtime, whatever the family's keystroke sequence, as deliver-now runs
//     it there.
func TestTurnCapabilitiesProductionProfileTable(t *testing.T) {
	none := func(replace string) string { return "none none/none/none/none/false keystroke " + replace }
	t3Row := "none none/none/none/none/false protocol interrupt"
	if _, ok := profiled(runtime.ProfileT3Bridge, runtime.FullFake{Fake: runtime.NewFake()}).(runtime.TurnObservationProvider); ok {
		t3Row = "protocol recorded/recorded/checked/correlated/false protocol interrupt"
	}
	acpRow := "protocol recorded/recorded/atomic/exact/true keystroke interrupt"
	noSnapshots := map[string]string{
		"claude": "transcript recorded/none/checked/correlated/true keystroke interrupt",
		"codex":  "transcript recorded/recorded/checked/correlated/true keystroke interrupt",
		"gemini": none("interrupt_reset"),
		"pi":     none("restart"),
	}
	remote := map[string]string{"claude": none("interrupt"), "codex": none("interrupt"), "gemini": none("interrupt_reset"), "pi": none("restart")}
	table := []struct {
		profile runtime.Profile
		rows    map[string]string
	}{
		{runtime.ProfileTmux, map[string]string{
			"claude": "transcript recorded/inferred/checked/correlated/true keystroke interrupt",
			"codex":  "transcript recorded/recorded/checked/correlated/true keystroke interrupt",
			"gemini": none("interrupt_reset"),
			"pi":     none("restart"),
		}},
		{runtime.ProfileACP, map[string]string{"claude": acpRow, "codex": acpRow, "gemini": acpRow, "pi": acpRow}},
		{runtime.ProfileSubprocess, noSnapshots},
		{runtime.ProfileHerdr, noSnapshots},
		{runtime.ProfileK8s, remote},
		{runtime.ProfileExec, remote},
		{runtime.ProfileSSH, remote},
		{runtime.ProfileT3Bridge, map[string]string{"claude": t3Row, "codex": t3Row, "gemini": t3Row, "pi": t3Row}},
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
				s.writeCompletedTurn(t, family)
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

// Kills a replacement read off the family table where deliver-now does
// something else (internal/session/submit.go).
func TestTurnReplaceFollowsDeliverNow(t *testing.T) {
	cases := []struct {
		family TurnReplace
		source TurnSource
		mode   runtime.TurnInterruptMode
		want   TurnReplace
	}{
		{TurnReplaceInterruptReset, TurnSourceTranscript, runtime.TurnInterruptKeystroke, TurnReplaceInterruptReset},
		{TurnReplaceRestart, TurnSourceNone, runtime.TurnInterruptKeystroke, TurnReplaceRestart},
		{TurnReplaceInterruptReset, TurnSourceProtocol, runtime.TurnInterruptKeystroke, TurnReplaceInterrupt},
		{TurnReplaceRestart, TurnSourceProtocol, runtime.TurnInterruptKeystroke, TurnReplaceInterrupt},
		{TurnReplaceRestart, TurnSourceNone, runtime.TurnInterruptProtocol, TurnReplaceInterrupt},
		{TurnReplaceInterrupt, TurnSourceNone, runtime.TurnInterruptUnsupported, TurnReplaceNone},
		{TurnReplaceRestart, TurnSourceNone, runtime.TurnInterruptUnsupported, TurnReplaceRestart},
		{TurnReplaceRestart, TurnSourceProtocol, runtime.TurnInterruptUnsupported, TurnReplaceNone},
	}
	for _, tc := range cases {
		if got := turnReplaceFor(tc.family, tc.source, tc.mode); got != tc.want {
			t.Errorf("turnReplaceFor(%s, %s, %s) = %s, want %s", tc.family, tc.source, tc.mode, got, tc.want)
		}
	}
}

// Capabilities are fixed for a session's generation (X11 D1, I7): they do not
// rise when the transcript is first written, nor fall when it disappears.
func TestTurnCapabilitiesDoNotFollowTheTranscriptFile(t *testing.T) {
	s := newTurnSession(t, profiled(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), "claude", "")
	row := func() string {
		t.Helper()
		caps, err := s.handle.TurnCapabilities(context.Background())
		if err != nil {
			t.Fatalf("TurnCapabilities: %v", err)
		}
		return capsRow(caps)
	}
	before := row()
	path := s.writeCompletedTurn(t, "claude")
	written := row()
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	removed := row()
	if before != written || written != removed || written[:len("transcript")] != "transcript" {
		t.Fatalf("capabilities before %q, written %q, removed %q; want one transcript row", before, written, removed)
	}
}

// Kills a turn source picked by the work-directory fallback: another
// conversation's transcript is never read as this session's (X11 I1, D2).
func TestCurrentTurnNeverReadsAnotherConversation(t *testing.T) {
	tmux := func() runtime.Provider {
		return profiled(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()})
	}
	foreignClaude := func(t *testing.T, s turnSession) {
		t.Helper()
		writeTranscriptLines(t, filepath.Join(s.root, sessionlog.ProjectSlug(s.info.WorkDir), "old-conversation.jsonl"),
			claudePrompt("7f3e9a10-9999-4aaa-8000-00000000f0f0", "2026-10-13T04:00:00.000Z"))
	}
	foreignCodex := func(t *testing.T, s turnSession) {
		t.Helper()
		writeTranscriptLines(t, s.codexRolloutPath(otherConversation),
			codexMeta(otherConversation, s.info.WorkDir), codexStarted("019a9999-1111-7000-8000-00000000f0f0", "2026-10-08T03:00:00.000Z", 1791429600))
	}
	cases := []struct {
		name     string
		provider string
		keyless  bool
		foreign  func(t *testing.T, s turnSession)
		own      bool // the session's own transcript is written too
		wantErr  error
		wantTurn string
	}{
		{name: "keyless claude beside a stale conversation", provider: "claude", keyless: true, foreign: foreignClaude, wantErr: runtime.ErrRuntimeUnavailable},
		{name: "keyed claude before its transcript, beside another", provider: "claude", foreign: foreignClaude, wantErr: runtime.ErrRuntimeUnavailable},
		{name: "keyed claude reads its own", provider: "claude", foreign: foreignClaude, own: true, wantTurn: claudeTurn1},
		{name: "keyless codex beside another rollout", provider: "codex", keyless: true, foreign: foreignCodex, wantErr: runtime.ErrRuntimeUnavailable},
		{name: "keyed codex before its rollout, beside a newer one", provider: "codex", foreign: foreignCodex, wantErr: runtime.ErrRuntimeUnavailable},
		{name: "keyed codex reads its own beside a newer one", provider: "codex", foreign: foreignCodex, own: true, wantTurn: codexTurn1},
		{
			name: "codex rollout named for the key, recording another conversation", provider: "codex",
			foreign: func(t *testing.T, s turnSession) {
				t.Helper()
				writeTranscriptLines(t, s.codexRolloutPath(s.info.SessionKey),
					codexMeta(otherConversation, s.info.WorkDir), codexStarted(codexTurn2, "2026-10-08T03:00:00.000Z", 1791429600))
			},
			wantErr: runtime.ErrRuntimeUnavailable,
		},
		{
			name: "gemini beside a foreign chat", provider: "gemini",
			foreign: func(t *testing.T, s turnSession) {
				t.Helper()
				s.writeGeminiProject(t)
				writeTranscriptLines(t, filepath.Join(s.root, "project", "chats", "session-foreign.json"),
					`{"sessionId":"foreign","projectHash":"project","messages":[`+geminiUser(geminiTurn1, "2026-06-21T17:08:00.694Z")+`]}`)
			},
			wantErr: runtime.ErrTurnIdentityUnsupported,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTurnSessionWith(t, tmux(), tc.provider, turnSessionOptions{keyless: tc.keyless})
			tc.foreign(t, s)
			if tc.own {
				s.writeCompletedTurn(t, tc.provider)
			}
			got, err := s.handle.CurrentTurn(context.Background())
			if tc.wantErr != nil {
				assertNoHistory(t, got, err, tc.wantErr)
				return
			}
			if err != nil {
				t.Fatalf("CurrentTurn: %v", err)
			}
			if got.Current.Active() || len(got.Recent) != 1 || got.Recent[0].TurnID != tc.wantTurn {
				t.Fatalf("CurrentTurn = %+v, want only the session's own %s", got, tc.wantTurn)
			}
		})
	}
}

// Kills an unclassified lookup failure: a closed session is not found, and a
// store that cannot be read is unavailable (the TurnHandle contract).
func TestTurnReadsClassifySessionLookupFailures(t *testing.T) {
	s := newTurnSession(t, profiled(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), "claude", "")
	s.writeCompletedTurn(t, "claude")
	if err := s.store.Close(s.info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := s.handle.TurnCapabilities(context.Background()); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("TurnCapabilities of a closed session = %v, want ErrSessionNotFound", err)
	}
	got, err := s.handle.CurrentTurn(context.Background())
	assertNoHistory(t, got, err, runtime.ErrSessionNotFound)
	if _, err := s.handle.WatchTurns(context.Background()); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("WatchTurns of a closed session = %v, want ErrSessionNotFound", err)
	}

	failing := newTurnSession(t, profiled(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), "claude", "")
	failing.handle.manager = sessionManagerOnFailingStore(t, failing)
	got, err = failing.handle.CurrentTurn(context.Background())
	assertNoHistory(t, got, err, runtime.ErrRuntimeUnavailable)
}

// unreachableTurnStore is a store whose reads fail as an unreachable backend's do.
type unreachableTurnStore struct{ beads.Store }

func (unreachableTurnStore) Get(string) (beads.Bead, error) {
	return beads.Bead{}, errors.New("store unreachable")
}

// sessionManagerOnFailingStore is a Manager over s's runtime whose store reads
// fail.
func sessionManagerOnFailingStore(t *testing.T, s turnSession) *sessionpkg.Manager {
	t.Helper()
	return sessionpkg.NewManagerWithOptions(unreachableTurnStore{s.store}, s.sp, sessionpkg.WithCityPath(t.TempDir()))
}
