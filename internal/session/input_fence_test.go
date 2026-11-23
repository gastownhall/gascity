package session

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// inputCallMethods are the Fake calls that type into or interrupt a session.
var inputCallMethods = map[string]bool{
	"Nudge": true, "NudgeNow": true, "SendKeys": true, "Interrupt": true,
	"ClearInput": true, "DismissKnownDialogs": true, "ResetInterruptedTurn": true,
}

func inputCalls(calls []runtime.Call, name string) []runtime.Call {
	var out []runtime.Call
	for _, c := range calls {
		if c.Name == name && inputCallMethods[c.Method] {
			out = append(out, c)
		}
	}
	return out
}

// Kills any drift between how a start publishes the incarnation (RuntimeEnv
// keys, ensureRunning's parsing, a pending continuation reset) and how
// InputFenceFor reads it back off the row: for random raw generations,
// epochs and tokens, the fence of the env the runtime was started with is
// the fence of the re-read row.
func TestInputFenceForMatchesStartedRuntimeEnv(t *testing.T) {
	raws := []string{"", "0", "-3", "x", " 2", "1", "5", "2147483647"}
	tokens := []string{"", "tok", " spaced ", "0123456789abcdef0123456789abcdef"}
	rng := rand.New(rand.NewSource(12))
	for i := range 40 {
		gen, epoch := raws[rng.Intn(len(raws))], raws[rng.Intn(len(raws))]
		token := tokens[rng.Intn(len(tokens))]
		pending := rng.Intn(2) == 1
		t.Run(fmt.Sprintf("%d gen=%q epoch=%q token=%q pending=%v", i, gen, epoch, token, pending), func(t *testing.T) {
			store := beads.NewMemStore()
			sp := runtime.NewFake()
			mgr := newTestManager(t, store, sp)
			info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
			if err != nil {
				t.Fatal(err)
			}
			if f, ok := InputFenceFor(info); !ok || f != startedFence(t, sp, info.SessionName) {
				t.Fatalf("created row's fence %v, %v differs from its start env's", f, ok)
			}
			if err := sp.Stop(info.SessionName); err != nil {
				t.Fatal(err)
			}
			marker := ""
			if pending {
				marker = "1"
			}
			if err := store.SetMetadataBatch(info.ID, map[string]string{
				"generation": gen, "continuation_epoch": epoch, "instance_token": token, "continuation_reset_pending": marker,
			}); err != nil {
				t.Fatal(err)
			}
			if err := mgr.Start(context.Background(), testActor(mgr, ActorOperator), info.ID, "claude --resume", runtime.Config{WorkDir: t.TempDir()}); err != nil {
				t.Fatalf("Start: %v", err)
			}
			reread, err := mgr.Get(info.ID)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := InputFenceFor(reread)
			if want := startedFence(t, sp, info.SessionName); !ok || got != want {
				t.Fatalf("InputFenceFor(row) = %v, %v; the started env names %v", got, ok, want)
			}
		})
	}
}

func startedFence(t *testing.T, sp *runtime.Fake, name string) runtime.InputFence {
	t.Helper()
	cfg := sp.LastStartConfig(name)
	if cfg == nil {
		t.Fatalf("no start of %s", name)
	}
	f, ok := runtime.InputFenceFromEnv(cfg.Env)
	if !ok {
		t.Fatalf("start env of %s names no incarnation", name)
	}
	return f
}

func TestInputFenceForDefaults(t *testing.T) {
	if _, ok := InputFenceFor(Info{Generation: "3"}); ok {
		t.Fatal("a row with no token named an incarnation")
	}
	got, ok := InputFenceFor(Info{Generation: "x", ContinuationEpoch: "-1", InstanceToken: " t "})
	if want := (runtime.InputFence{Generation: DefaultGeneration, Epoch: DefaultContinuationEpoch, Token: " t "}); !ok || got != want {
		t.Fatalf("InputFenceFor = %v, %v; want %v", got, ok, want)
	}
}

// stopFamilies are the stop-turn shapes: provider, extra metadata.
var stopFamilies = []struct {
	name     string
	provider string
	meta     map[string]string
}{
	{"claude", "claude", nil},
	{"codex", "codex", nil},
	{"gemini", "gemini", nil},
	{"kimi", "kimi", nil},
	{"wrapped gemini", "my-gemini", map[string]string{"builtin_ancestor": "gemini"}},
	{"unknown", "someagent", nil},
}

func newFencedStopSession(t *testing.T, provider string, meta map[string]string, mode runtime.TurnInterruptMode) (*Manager, *runtime.Fake, Info, runtime.InputFence) {
	t.Helper()
	sp := runtime.NewFake()
	mgr, info, f := newFencedStopSessionOn(t, sp, sp, provider, meta, mode)
	return mgr, sp, info, f
}

// newFencedStopSessionOn is newFencedStopSession with its manager on rt, a
// runtime over sp that must behave as sp until the test changes it.
func newFencedStopSessionOn(t *testing.T, rt runtime.Provider, sp *runtime.Fake, provider string, meta map[string]string, mode runtime.TurnInterruptMode) (*Manager, Info, runtime.InputFence) {
	t.Helper()
	store := beads.NewMemStore()
	sp.TurnInterruptModeValue = mode
	mgr := newTestManager(t, store, rt)
	extra := map[string]string{"session_origin": "manual"}
	for k, v := range meta {
		extra[k] = v
	}
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: provider, WorkDir: t.TempDir(), Provider: provider, ExtraMeta: extra})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f, ok := InputFenceFor(info)
	if !ok {
		t.Fatal("created row names no incarnation")
	}
	if (provider == "codex" || provider == "kimi") && mode == runtime.TurnInterruptKeystroke {
		// Force the soft-escape families onto their Ctrl-C fallback.
		sp.WaitForIdleSequence[info.SessionName] = []error{errors.New("not idle yet"), nil}
	}
	sp.Calls = nil
	return mgr, info, f
}

// Kills a stop-turn path that sends any key or interrupt unfenced: for every
// family and both interrupt modes, every input call carries the fence, a
// mismatched fence sends nothing at all, and the fenced call sequence is the
// unfenced one.
func TestStopTurnFencedFencesEveryInput(t *testing.T) {
	for _, fam := range stopFamilies {
		for _, mode := range []runtime.TurnInterruptMode{runtime.TurnInterruptKeystroke, runtime.TurnInterruptProtocol} {
			t.Run(fam.name+"/"+string(mode), func(t *testing.T) {
				mgr, sp, info, f := newFencedStopSession(t, fam.provider, fam.meta, mode)
				if err := mgr.StopTurnFenced(context.Background(), info.ID, f); err != nil {
					t.Fatalf("StopTurnFenced: %v", err)
				}
				fenced := inputCalls(sp.SnapshotCalls(), info.SessionName)
				if len(fenced) == 0 {
					t.Fatal("no input sent")
				}
				for _, c := range fenced {
					if c.Fence != f {
						t.Errorf("%s(%s) carries %v, want the fence", c.Method, c.Message, c.Fence)
					}
				}

				umgr, usp, uinfo, _ := newFencedStopSession(t, fam.provider, fam.meta, mode)
				if err := umgr.StopTurn(uinfo.ID); err != nil {
					t.Fatalf("StopTurn: %v", err)
				}
				unfenced := inputCalls(usp.SnapshotCalls(), uinfo.SessionName)
				if !slices.Equal(callMethods(fenced), callMethods(unfenced)) {
					t.Errorf("fenced %v, unfenced %v", callMethods(fenced), callMethods(unfenced))
				}
				for _, c := range unfenced {
					if !c.Fence.IsZero() {
						t.Errorf("unfenced %s carries a fence", c.Method)
					}
				}

				mmgr, msp, minfo, mf := newFencedStopSession(t, fam.provider, fam.meta, mode)
				mf.Token += "-other"
				err := mmgr.StopTurnFenced(context.Background(), minfo.ID, mf)
				if !errors.Is(err, runtime.ErrFenceMismatch) {
					t.Fatalf("StopTurnFenced(other) = %v, want ErrFenceMismatch", err)
				}
				if calls := inputCalls(msp.SnapshotCalls(), minfo.SessionName); len(calls) != 0 {
					t.Fatalf("a mismatched fence sent %v", callMethods(calls))
				}
			})
		}
	}
}

// Pins today's unfenced StopTurn key sequences, so threading the fence
// through it changes nothing for unfenced callers.
func TestStopTurnUnfencedSequencesGolden(t *testing.T) {
	want := map[string][]string{
		"claude":         {"Interrupt", "ClearInput"},
		"codex":          {"SendKeys(Escape)", "Interrupt"},
		"gemini":         {"Interrupt"},
		"kimi":           {"SendKeys(Escape)", "Interrupt"},
		"wrapped gemini": {"SendKeys(Escape)"},
		"unknown":        {"Interrupt"},
	}
	for _, fam := range stopFamilies {
		mgr, sp, info, _ := newFencedStopSession(t, fam.provider, fam.meta, runtime.TurnInterruptKeystroke)
		if err := mgr.StopTurn(info.ID); err != nil {
			t.Fatalf("%s: StopTurn: %v", fam.name, err)
		}
		if got := callMethods(inputCalls(sp.SnapshotCalls(), info.SessionName)); !slices.Equal(got, want[fam.name]) {
			t.Errorf("%s: StopTurn sent %v, want %v", fam.name, got, want[fam.name])
		}
	}
}

// A runtime that cannot fence the session is refused before any input.
func TestStopTurnFencedRefusesUnfenceableRuntime(t *testing.T) {
	mgr, sp, info, f := newFencedStopSession(t, "claude", nil, runtime.TurnInterruptKeystroke)
	sp.InputFencingValue = runtime.InputFencingNone
	if err := mgr.StopTurnFenced(context.Background(), info.ID, f); !errors.Is(err, runtime.ErrInputFenceUnsupported) {
		t.Fatalf("StopTurnFenced = %v, want ErrInputFenceUnsupported", err)
	}
	if err := mgr.StopTurnFenced(context.Background(), info.ID, runtime.InputFence{}); !errors.Is(err, runtime.ErrInvalidInputFence) {
		t.Fatalf("StopTurnFenced(zero) = %v, want ErrInvalidInputFence", err)
	}
	if calls := inputCalls(sp.SnapshotCalls(), info.SessionName); len(calls) != 0 {
		t.Fatalf("refused stop sent %v", callMethods(calls))
	}
	if got, err := mgr.InputFencing(info.ID); err != nil || got != runtime.InputFencingNone {
		t.Fatalf("InputFencing = %q, %v; want none", got, err)
	}
}

// The live-only sends carry a ctx fence to the runtime, and a mismatch sends
// nothing.
func TestFencedLiveOnlySendsCarryFence(t *testing.T) {
	for _, immediate := range []bool{false, true} {
		mgr, sp, info, f := newFencedStopSession(t, "claude", nil, runtime.TurnInterruptKeystroke)
		send := mgr.SendLiveOnly
		method := "Nudge"
		if immediate {
			send, method = mgr.SendImmediateLiveOnly, "NudgeNow"
		}
		delivered, err := send(runtime.WithInputFence(context.Background(), f), info.ID, "hi", nil)
		if err != nil || !delivered {
			t.Fatalf("%s: %v, %v", method, delivered, err)
		}
		calls := inputCalls(sp.SnapshotCalls(), info.SessionName)
		if len(calls) != 1 || calls[0].Method != method || calls[0].Fence != f {
			t.Fatalf("%s calls = %+v", method, calls)
		}
		other := f
		other.Epoch++
		sp.Calls = nil
		if _, err := send(runtime.WithInputFence(context.Background(), other), info.ID, "hi", nil); !errors.Is(err, runtime.ErrFenceMismatch) {
			t.Fatalf("%s under another fence = %v", method, err)
		}
		if calls := inputCalls(sp.SnapshotCalls(), info.SessionName); len(calls) != 0 {
			t.Fatalf("%s under another fence sent %v", method, callMethods(calls))
		}
	}
}

// startFenceProbe records whether Start was handed a ctx input fence.
type startFenceProbe struct {
	*runtime.Fake
	startFenced bool
}

func (p *startFenceProbe) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if _, ok := runtime.InputFenceFrom(ctx); ok {
		p.startFenced = true
	}
	return p.Fake.Start(ctx, name, cfg)
}

// FI9 at the session layer: a start never runs under a caller's input
// fence, which names an incarnation the start is replacing.
func TestEnsureRunningStartsWithoutCallerFence(t *testing.T) {
	store := beads.NewMemStore()
	sp := &startFenceProbe{Fake: runtime.NewFake()}
	mgr := newTestManager(t, store, sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatal(err)
	}
	f, _ := InputFenceFor(info)
	if err := sp.Stop(info.SessionName); err != nil {
		t.Fatal(err)
	}
	sp.startFenced = false
	_, _ = mgr.Send(runtime.WithInputFence(context.Background(), f), testActor(mgr, ActorOperator), info.ID, "hi", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir})
	if !slices.ContainsFunc(sp.SnapshotCalls(), func(c runtime.Call) bool { return c.Method == "Start" }) {
		t.Fatal("Send never restarted the stopped session: the test misses the start path")
	}
	if sp.startFenced {
		t.Fatal("Start ran under the caller's input fence")
	}
}

// A fenced stop of a session that is not running reports the incarnation
// absent, as a fenced call never reports success into an absent one; the
// unfenced stop stays nil.
func TestStopTurnFencedOfStoppedSessionIsAbsent(t *testing.T) {
	mgr, sp, info, f := newFencedStopSession(t, "claude", nil, runtime.TurnInterruptKeystroke)
	if err := sp.Stop(info.SessionName); err != nil {
		t.Fatal(err)
	}
	err := mgr.StopTurnFenced(context.Background(), info.ID, f)
	var miss *runtime.FenceMismatchError
	if !errors.As(err, &miss) || miss.Found != runtime.FenceFoundAbsent {
		t.Fatalf("StopTurnFenced of a stopped session = %v, want an absent miss", err)
	}
	if err := mgr.StopTurn(info.ID); err != nil {
		t.Fatalf("StopTurn of a stopped session = %v, want nil", err)
	}
}

// dyingRuntime stops session dies when a stop reads its turn interrupt mode,
// which a stop does only once it has read the session running, and before
// its first input.
type dyingRuntime struct {
	*runtime.Fake
	dies string
}

func (d *dyingRuntime) TurnInterruptMode(name string) runtime.TurnInterruptMode {
	if name == d.dies {
		d.dies = ""
		_ = d.Stop(name)
	}
	return d.Fake.TurnInterruptMode(name)
}

// Kills a fenced stop that reports success into an incarnation that exits
// between the stop's running check and its first input: for every family
// and both interrupt modes, a session gone right after the stop reads it
// running is a non-partial miss finding it absent, decided before anything
// is sent; the unfenced stop of the same race stays nil.
func TestStopTurnFencedOfADyingSessionIsAbsent(t *testing.T) {
	for _, fam := range stopFamilies {
		for _, mode := range []runtime.TurnInterruptMode{runtime.TurnInterruptKeystroke, runtime.TurnInterruptProtocol} {
			t.Run(fam.name+"/"+string(mode), func(t *testing.T) {
				rt := &dyingRuntime{Fake: runtime.NewFake()}
				mgr, info, f := newFencedStopSessionOn(t, rt, rt.Fake, fam.provider, fam.meta, mode)
				rt.dies = info.SessionName
				err := mgr.StopTurnFenced(context.Background(), info.ID, f)
				if rt.dies != "" {
					t.Fatal("the stop never read the session running: the race was not run")
				}
				var miss *runtime.FenceMismatchError
				if !errors.As(err, &miss) || miss.Session != info.SessionName || miss.Found != runtime.FenceFoundAbsent || miss.Partial {
					t.Fatalf("StopTurnFenced of a dying session = %v, want a non-partial miss finding it absent", err)
				}
				calls := rt.SnapshotCalls()
				if sent := inputCalls(calls, info.SessionName); len(sent) != 0 {
					t.Fatalf("a stop of a gone session sent %v", callMethods(sent))
				}
				if slices.ContainsFunc(calls, func(c runtime.Call) bool { return c.Method == "FenceMismatch" }) {
					t.Fatal("the miss came from a fenced input sent into the gone session, not from the stop's read")
				}

				urt := &dyingRuntime{Fake: runtime.NewFake()}
				umgr, uinfo, _ := newFencedStopSessionOn(t, urt, urt.Fake, fam.provider, fam.meta, mode)
				urt.dies = uinfo.SessionName
				if err := umgr.StopTurn(uinfo.ID); err != nil || urt.dies != "" {
					t.Fatalf("StopTurn of a dying session = %v (race run: %v), want nil", err, urt.dies == "")
				}
			})
		}
	}
}

// Kills a fenced live-only send that reports nothing delivered, as if
// unfenced, into an incarnation its fresh liveness read rules out: under a
// fence, a stopped session is a miss finding it absent, an exited agent one
// finding it dead, and a read that cannot answer is ErrRuntimeUnavailable,
// with nothing sent; unfenced, each stays undelivered and nil.
func TestFencedLiveOnlySendOfAGoneAgentMisses(t *testing.T) {
	readErr := errors.New("tmux state refresh failed")
	for _, tc := range []struct {
		name  string
		gone  func(rt *unreadableAfter, name string) error
		found string // the miss's Found; "" for ErrRuntimeUnavailable
	}{
		{"stopped", func(rt *unreadableAfter, name string) error { return rt.Stop(name) }, runtime.FenceFoundAbsent},
		{"agent exited", func(rt *unreadableAfter, name string) error { rt.Zombies[name] = true; return nil }, runtime.FenceFoundDead},
		{"unreadable", func(rt *unreadableAfter, _ string) error { rt.err = readErr; return nil }, ""},
	} {
		for _, immediate := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/immediate=%v", tc.name, immediate), func(t *testing.T) {
				rt := &unreadableAfter{Fake: runtime.NewFake()}
				mgr, info, f := newFencedStopSessionOn(t, rt, rt.Fake, "claude", nil, runtime.TurnInterruptKeystroke)
				if err := tc.gone(rt, info.SessionName); err != nil {
					t.Fatal(err)
				}
				rt.Calls = nil
				send := mgr.SendLiveOnly
				if immediate {
					send = mgr.SendImmediateLiveOnly
				}
				processNames := []string{"claude"}
				delivered, err := send(runtime.WithInputFence(context.Background(), f), info.ID, "hi", processNames)
				var miss *runtime.FenceMismatchError
				switch {
				case tc.found == "":
					if !errors.Is(err, runtime.ErrRuntimeUnavailable) || !errors.Is(err, readErr) || errors.Is(err, runtime.ErrFenceMismatch) {
						t.Fatalf("fenced send = %v, %v; want ErrRuntimeUnavailable wrapping the read's error, not a miss", delivered, err)
					}
				case !errors.As(err, &miss) || miss.Session != info.SessionName || miss.Found != tc.found || miss.Partial:
					t.Fatalf("fenced send = %v, %v; want a non-partial miss finding it %s", delivered, err, tc.found)
				}
				if delivered {
					t.Fatal("a missed send reports delivered")
				}
				if sent := inputCalls(rt.SnapshotCalls(), info.SessionName); len(sent) != 0 {
					t.Fatalf("a send to a gone agent sent %v", callMethods(sent))
				}
				if delivered, err := send(context.Background(), info.ID, "hi", processNames); delivered || err != nil {
					t.Fatalf("unfenced send = %v, %v; want undelivered, nil", delivered, err)
				}
			})
		}
	}
}

// unreadableAfter is a runtime whose fresh liveness read fails with err once
// err is set.
type unreadableAfter struct {
	*runtime.Fake
	err error
}

func (u *unreadableAfter) ObserveLivenessSince(name string, processNames []string, since time.Time) (runtime.Liveness, error) {
	if u.err != nil {
		return runtime.Liveness{}, u.err
	}
	return runtime.ObserveLivenessSince(u.Fake, name, processNames, since)
}

// staleCacheRuntime is unreadableAfter whose cached running read
// (IsRunning) finds no session once cacheGone is set, as the tmux state
// cache does past its stale window, while its fresh read still answers.
type staleCacheRuntime struct {
	unreadableAfter
	cacheGone bool
}

func (s *staleCacheRuntime) IsRunning(name string) bool {
	return !s.cacheGone && s.Fake.IsRunning(name)
}

// Kills a fenced stop that decides its incarnation absent from the runtime's
// cached running read, which reads a live session not running: for every
// family and both interrupt modes, a stop whose cache finds no session but
// whose fresh read finds it alive sends the unfenced stop's inputs, each
// fenced; one whose fresh read cannot answer is ErrRuntimeUnavailable, not a
// miss, and one of an exited agent a miss finding it dead, not absent, each
// with nothing sent. The unfenced stop still goes by the cache.
func TestStopTurnFencedReadsLivenessFresh(t *testing.T) {
	readErr := errors.New("tmux state refresh failed")
	for _, fam := range stopFamilies {
		for _, mode := range []runtime.TurnInterruptMode{runtime.TurnInterruptKeystroke, runtime.TurnInterruptProtocol} {
			t.Run(fam.name+"/"+string(mode), func(t *testing.T) {
				rt := &staleCacheRuntime{unreadableAfter: unreadableAfter{Fake: runtime.NewFake()}}
				mgr, info, f := newFencedStopSessionOn(t, rt, rt.Fake, fam.provider, fam.meta, mode)
				rt.cacheGone = true
				if err := mgr.StopTurnFenced(context.Background(), info.ID, f); err != nil {
					t.Fatalf("StopTurnFenced of a live session its cache misses = %v, want nil", err)
				}
				fenced := inputCalls(rt.SnapshotCalls(), info.SessionName)
				umgr, usp, uinfo, _ := newFencedStopSession(t, fam.provider, fam.meta, mode)
				if err := umgr.StopTurn(uinfo.ID); err != nil {
					t.Fatalf("StopTurn: %v", err)
				}
				if want := callMethods(inputCalls(usp.SnapshotCalls(), uinfo.SessionName)); len(fenced) == 0 || !slices.Equal(callMethods(fenced), want) {
					t.Fatalf("StopTurnFenced sent %v, want the unfenced stop's %v", callMethods(fenced), want)
				}
				for _, c := range fenced {
					if c.Fence != f {
						t.Errorf("%s(%s) carries %v, want the fence", c.Method, c.Message, c.Fence)
					}
				}

				rt.err = readErr
				rt.Calls = nil
				err := mgr.StopTurnFenced(context.Background(), info.ID, f)
				if !errors.Is(err, runtime.ErrRuntimeUnavailable) || !errors.Is(err, readErr) || errors.Is(err, runtime.ErrFenceMismatch) {
					t.Fatalf("StopTurnFenced on a read that cannot answer = %v, want ErrRuntimeUnavailable wrapping the read's error, not a miss", err)
				}
				if sent := inputCalls(rt.SnapshotCalls(), info.SessionName); len(sent) != 0 {
					t.Fatalf("a stop whose read cannot answer sent %v", callMethods(sent))
				}

				rt.err = nil
				rt.Zombies[info.SessionName] = true
				var miss *runtime.FenceMismatchError
				if err := mgr.StopTurnFenced(context.Background(), info.ID, f); !errors.As(err, &miss) || miss.Session != info.SessionName || miss.Found != runtime.FenceFoundDead || miss.Partial {
					t.Fatalf("StopTurnFenced of an exited agent its cache misses = %v, want a non-partial miss finding it dead", err)
				}
				if sent := inputCalls(rt.SnapshotCalls(), info.SessionName); len(sent) != 0 {
					t.Fatalf("a stop of an exited agent sent %v", callMethods(sent))
				}
				if err := mgr.StopTurn(info.ID); err != nil {
					t.Fatalf("StopTurn of a session its cache misses = %v, want nil", err)
				}
				if sent := inputCalls(rt.SnapshotCalls(), info.SessionName); len(sent) != 0 {
					t.Fatalf("the unfenced stop of a session its cache misses sent %v", callMethods(sent))
				}
			})
		}
	}
}
