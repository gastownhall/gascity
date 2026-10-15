package session

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"testing"

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
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	sp.TurnInterruptModeValue = mode
	mgr := newTestManager(t, store, sp)
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
	return mgr, sp, info, f
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
