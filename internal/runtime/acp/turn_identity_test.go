package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// stdinRecorder is an agent's stdin that keeps every line gc writes, in
// write order. gc writes each message with one Write under stdinMu.
type stdinRecorder struct {
	mu    sync.Mutex
	lines []string
	// onWrite, when set, sees each line as it is written, under mu.
	onWrite func(method, promptText string)
}

func (r *stdinRecorder) Write(b []byte) (int, error) {
	var msg struct {
		Method string `json:"method"`
		Params struct {
			Prompt []ContentBlock `json:"prompt"`
		} `json:"params"`
	}
	_ = json.Unmarshal(b, &msg)
	var text string
	for _, block := range msg.Params.Prompt {
		text += block.Text
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, string(b))
	if r.onWrite != nil {
		r.onWrite(msg.Method, text)
	}
	return len(b), nil
}

func (*stdinRecorder) Close() error { return nil }

// methods are the recorded lines' methods, "reply" for a response.
func (r *stdinRecorder) methods() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, line := range r.lines {
		var msg struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal([]byte(line), &msg)
		if msg.Method == "" {
			msg.Method = "reply"
		}
		out = append(out, msg.Method)
	}
	return out
}

// recordedConn registers an in-process connection whose stdin is rec and
// whose process is a never-started command, so SIGINT is a no-op.
func recordedConn(t *testing.T, p *Provider, rec *stdinRecorder) (string, *sessionConn) {
	t.Helper()
	name := testName()
	sc := newSessionConn(&exec.Cmd{}, rec, nil, 10, nil)
	sc.sessionID = "s1"
	p.mu.Lock()
	p.conns[name] = sc
	p.mu.Unlock()
	t.Cleanup(func() { sc.drainPending(nil) })
	return name, sc
}

// startTurn opens a turn for prompt id and returns its id.
func startTurn(t *testing.T, sc *sessionConn, id int64) string {
	t.Helper()
	if !sc.setActivePrompt(id) {
		t.Fatal("setActivePrompt refused")
	}
	current, _ := sc.turns()
	return current.ID
}

// Kills a ring that keeps more or fewer than eight turns, drops the newest
// instead of the oldest, reports oldest first, reports another id than its
// turn events carry, or misreports how a turn ended; and a turns() whose
// last turn is not the newest.
func TestTurnHistoryKeepsTheLastEightNewestFirst(t *testing.T) {
	p := newTestProvider(t)
	name, sc := injectConn(t, p)
	events, err := p.SubscribeTurnEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	p.attachTurnEvents(name, nil, sc)

	results := []string{
		`{"stopReason":"end_turn"}`,
		`{"stopReason":"` + acpStopReasonCancelled + `"}`,
	}
	var ids []string
	for id := int64(1); id <= 10; id++ {
		ids = append(ids, startTurn(t, sc, id))
		if id == 10 {
			sc.abandonPrompt(id, errors.New("pipe"))
			break
		}
		respondTo(sc, id, results[id%2])
	}
	if _, last := sc.turns(); last.ID != ids[9] {
		t.Fatalf("turns() last = %q, want the newest %q", last.ID, ids[9])
	}

	h, err := p.TurnHistory(t.Context(), name)
	if err != nil {
		t.Fatalf("TurnHistory: %v", err)
	}
	if h.Current.Active() || !h.CurrentStart.IsZero() || h.ObservedAt.IsZero() {
		t.Fatalf("history = %+v, want idle and stamped", h)
	}
	var got []string
	for _, ended := range h.Recent {
		got = append(got, ended.TurnID)
		if ended.StartedAt.IsZero() || ended.EndedAt.Before(ended.StartedAt) || ended.Inferred || ended.DeliveryKeys != nil {
			t.Errorf("ended turn %+v: want recorded start and end, not inferred, no keys", ended)
		}
	}
	want := slices.Clone(ids[2:])
	slices.Reverse(want)
	if !slices.Equal(got, want) {
		t.Fatalf("recent = %q, want the last eight newest first %q", got, want)
	}
	statuses := map[string]runtime.TurnStatus{}
	for _, ended := range h.Recent {
		statuses[ended.TurnID] = ended.Status
	}
	if statuses[ids[9]] != runtime.TurnStatusFailed || statuses[ids[8]] != runtime.TurnStatusCancelled || statuses[ids[7]] != runtime.TurnStatusCompleted {
		t.Fatalf("statuses = %v, want failed, canceled, completed for the three newest", statuses)
	}

	var eventIDs []string
	for len(eventIDs) < 10 {
		select {
		case ev := <-events:
			if ev.Kind == runtime.TurnEventStarted {
				eventIDs = append(eventIDs, ev.TurnID)
			}
		case <-time.After(protocolUnitWait):
			t.Fatalf("turn events = %q, want ten starts", eventIDs)
		}
	}
	if !slices.Equal(eventIDs, ids) {
		t.Fatalf("turn event ids = %q, want the history's ids %q", eventIDs, ids)
	}
}

// Kills reads that call a running turn idle, report a starting or drained
// session as idle, or answer for a session nobody owns.
func TestObserveTurnReadsTheRunningRecord(t *testing.T) {
	p := newTestProvider(t)
	ctx := t.Context()
	name, sc := injectConn(t, p)

	if st, err := p.ObserveTurn(ctx, name); err != nil || st.Active() {
		t.Fatalf("idle = %+v, %v; want no turn", st, err)
	}
	id := startTurn(t, sc, 1)
	if st, err := p.ObserveTurn(ctx, name); err != nil || st.TurnID != id {
		t.Fatalf("running = %+v, %v; want %q", st, err, id)
	}
	h, err := p.TurnHistory(ctx, name)
	if err != nil || h.Current.TurnID != id || h.CurrentStart.IsZero() || len(h.Recent) != 0 {
		t.Fatalf("running history = %+v, %v; want %q with its start and no recent turn", h, err, id)
	}

	starting := testName()
	p.mu.Lock()
	p.conns[starting] = &sessionConn{done: make(chan struct{}), cancel: func() {}}
	p.mu.Unlock()
	if _, err := p.ObserveTurn(ctx, starting); !errors.Is(err, runtime.ErrSessionInitializing) {
		t.Fatalf("starting ObserveTurn = %v, want ErrSessionInitializing", err)
	}
	if _, err := p.TurnHistory(ctx, starting); !errors.Is(err, runtime.ErrSessionInitializing) {
		t.Fatalf("starting TurnHistory = %v, want ErrSessionInitializing", err)
	}

	sc.drainPending(nil)
	if _, err := p.ObserveTurn(ctx, name); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("drained ObserveTurn = %v, want ErrSessionNotFound, never idle", err)
	}
	if _, err := p.TurnHistory(ctx, name); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("drained TurnHistory = %v, want ErrSessionNotFound, never idle", err)
	}
	if _, err := p.ObserveTurn(ctx, "nobody"); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("unowned ObserveTurn = %v, want ErrSessionNotFound", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := p.TurnHistory(canceled, name); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled TurnHistory = %v, want context.Canceled", err)
	}
}

// Kills capabilities that differ from the X11 ACP row, or that claim the
// owner's ring for a session another process owns.
func TestTurnCapabilitiesAreFullInTheOwner(t *testing.T) {
	p := newTestProvider(t)
	name, _ := injectConn(t, p)
	want := runtime.TurnCapabilities{
		Start: runtime.TurnStartRecorded, End: runtime.TurnEndRecorded,
		Fence: runtime.TurnFenceAtomic, Link: runtime.TurnLinkExact, Lossless: true,
	}
	if got := runtime.TurnCapabilitiesFor(NewSeamBackedWithDir(p.dir, Config{}), name); got.Lossless {
		t.Fatalf("another process's capabilities = %+v, want not lossless", got)
	}
	if got := runtime.TurnCapabilitiesFor(p, name); got != want {
		t.Fatalf("owner's capabilities = %+v, want %+v", got, want)
	}
	if got := runtime.TurnInterruptModeFor(NewSeamBackedWithDir(p.dir, Config{}), name); got != runtime.TurnInterruptKeystroke {
		t.Fatalf("TurnInterruptModeFor(acp) = %q, want keystroke unchanged", got)
	}
}

// Kills an InterruptTurn that sends anything for a turn that is not running:
// no session/cancel, no permission reply, no SIGINT, whether another turn
// runs or none does, and none for an empty id.
func TestInterruptTurnOnAStaleTurnSendsNothing(t *testing.T) {
	p := newTestProvider(t)
	rec := &stdinRecorder{}
	name, sc := recordedConn(t, p, rec)
	var signals atomic.Int32
	sc.beforeTurnSignal = func() { signals.Add(1) }
	ctx := t.Context()

	stale := startTurn(t, sc, 1)
	respondTo(sc, 1, `{"stopReason":"end_turn"}`)
	if _, err := p.InterruptTurn(ctx, name, stale); !errors.Is(err, runtime.ErrTurnNotCurrent) {
		t.Fatalf("idle InterruptTurn = %v, want ErrTurnNotCurrent", err)
	}
	current := startTurn(t, sc, 2)
	req, ok := parseAgentRequest([]byte(permissionRequestLine(7)))
	if !ok {
		t.Fatal("permission request did not parse")
	}
	sc.handleAgentRequest(req)
	waitUntil(t, "the permission request to be held", func() bool {
		sc.mu.Lock()
		defer sc.mu.Unlock()
		return len(sc.permissions) == 1
	})

	_, err := p.InterruptTurn(ctx, name, stale)
	var nc *runtime.TurnNotCurrentError
	if !errors.As(err, &nc) || nc.TurnID != stale || nc.Current.TurnID != current || nc.Session != name {
		t.Fatalf("stale InterruptTurn = %v, want not-current reading %q", err, current)
	}
	if _, err := p.InterruptTurn(ctx, name, ""); !errors.Is(err, runtime.ErrTurnUnnamed) {
		t.Fatalf("unnamed InterruptTurn = %v, want ErrTurnUnnamed", err)
	}
	sc.mu.Lock()
	held := len(sc.permissions)
	sc.mu.Unlock()
	if held != 1 {
		t.Fatalf("held permissions = %d, want the current turn's request kept", held)
	}
	// The current turn's own interrupt writes one permission reply and one
	// session/cancel. Any write a refused call had started would show beside
	// them.
	if _, err := p.InterruptTurn(ctx, name, current); err != nil {
		t.Fatalf("current InterruptTurn = %v", err)
	}
	waitUntil(t, "session/cancel", func() bool { return slices.Contains(rec.methods(), methodSessionCancel) })
	respondTo(sc, 2, `{"stopReason":"`+acpStopReasonCancelled+`"}`)
	if got := rec.methods(); !slices.Equal(got, []string{"reply", methodSessionCancel}) {
		t.Fatalf("stdin = %q, want only the current turn's reply and cancel", got)
	}
	if n := signals.Load(); n != 0 {
		t.Fatalf("SIGINT attempts = %d, want none", n)
	}
}

// Kills an InterruptTurn on the running turn that waits for the settle, is
// not fenced, skips the permission replies, or orders them after the cancel;
// and a WaitTurnEnded that misses the end or waits past it.
func TestInterruptTurnCancelsTheRunningTurn(t *testing.T) {
	p := newTestProvider(t)
	rec := &stdinRecorder{}
	name, sc := recordedConn(t, p, rec)
	ctx := t.Context()
	id := startTurn(t, sc, 1)
	req, _ := parseAgentRequest([]byte(permissionRequestLine(7)))
	sc.handleAgentRequest(req)

	ack, err := p.InterruptTurn(ctx, name, id)
	if err != nil || !ack.Fenced {
		t.Fatalf("InterruptTurn = %+v, %v; want sent, fenced", ack, err)
	}
	waitUntil(t, "session/cancel", func() bool { return slices.Contains(rec.methods(), methodSessionCancel) })
	if got := rec.methods(); !slices.Equal(got, []string{"reply", methodSessionCancel}) {
		t.Fatalf("stdin = %q, want the permission reply then session/cancel", got)
	}

	if st, err := p.WaitTurnEnded(ctx, name, id, 10*time.Millisecond); !errors.Is(err, runtime.ErrTurnNotEnded) || st.TurnID != id {
		t.Fatalf("WaitTurnEnded(still running) = %+v, %v; want ErrTurnNotEnded with %q", st, err, id)
	}
	ended := make(chan error, 1)
	go func() {
		st, err := p.WaitTurnEnded(ctx, name, id, protocolUnitWait)
		if err == nil && st.Active() {
			err = errors.New("a turn still runs")
		}
		ended <- err
	}()
	respondTo(sc, 1, `{"stopReason":"`+acpStopReasonCancelled+`"}`)
	if err := <-ended; err != nil {
		t.Fatalf("WaitTurnEnded = %v, want the end", err)
	}
	if st, err := p.WaitTurnEnded(ctx, name, id, 0); err != nil || st.Active() {
		t.Fatalf("WaitTurnEnded(ended) = %+v, %v; want ended at once", st, err)
	}
	if _, err := p.WaitTurnEnded(ctx, name, "", time.Second); !errors.Is(err, runtime.ErrTurnUnnamed) {
		t.Fatalf("WaitTurnEnded(unnamed) = %v, want ErrTurnUnnamed", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	next := startTurn(t, sc, 2)
	cancel()
	if _, err := p.WaitTurnEnded(canceled, name, next, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitTurnEnded(canceled) = %v, want context.Canceled", err)
	}
}

// Kills a turn-scoped interrupt that can cancel the next turn. Each seed
// interleaves turn T ending, the next turn U starting through Nudge, and
// InterruptTurn(T). U's prompt is written only after T's idle channel closes,
// so a session/cancel written after it, or a SIGINT attempt while U runs,
// cancels U.
func TestInterruptTurnNeverCancelsTheNextTurn(t *testing.T) {
	p := NewProviderWithDir(filepath.Join(shortTempDir(t), "acp"), Config{
		NudgeBusyTimeout: protocolUnitWait,
		CancelTimeout:    protocolUnitWait,
	})
	const seeds = 1000
	var lateCancels, signalsInU, fenced, stale atomic.Int32
	for seed := uint64(0); seed < seeds; seed++ {
		rec := &stdinRecorder{}
		var uWritten bool
		rec.onWrite = func(method, text string) {
			if method == "session/prompt" && text == "U" {
				uWritten = true
			}
			if method == methodSessionCancel && uWritten {
				lateCancels.Add(1)
			}
		}
		name, sc := recordedConn(t, p, rec)
		sc.beforeTurnSignal = func() {
			if cur, _ := sc.turns(); cur != nil && cur.PromptID != 1 {
				signalsInU.Add(1)
			}
		}
		if err := p.Nudge(name, runtime.TextContent("T")); err != nil {
			t.Fatalf("seed %d: Nudge(T): %v", seed, err)
		}
		cur, _ := sc.turns()
		turnT, promptT := cur.ID, cur.PromptID

		r := rand.New(rand.NewPCG(seed, 0x7e11))
		ops := []func(){
			func() { respondTo(sc, promptT, `{"stopReason":"end_turn"}`) },
			func() {
				if err := p.Nudge(name, runtime.TextContent("U")); err != nil {
					t.Errorf("seed %d: Nudge(U): %v", seed, err)
				}
			},
			func() {
				_, err := p.InterruptTurn(context.Background(), name, turnT)
				switch {
				case err == nil:
					fenced.Add(1)
				case errors.Is(err, runtime.ErrTurnNotCurrent):
					stale.Add(1)
				default:
					t.Errorf("seed %d: InterruptTurn: %v", seed, err)
				}
			},
		}
		r.Shuffle(len(ops), func(i, j int) { ops[i], ops[j] = ops[j], ops[i] })
		var start, done sync.WaitGroup
		start.Add(1)
		for _, op := range ops {
			yields := r.IntN(8)
			done.Add(1)
			go func() {
				defer done.Done()
				start.Wait()
				for range yields {
					goruntime.Gosched()
				}
				op()
			}()
		}
		start.Done()
		done.Wait()
		sc.drainPending(nil)
		p.mu.Lock()
		delete(p.conns, name)
		p.mu.Unlock()
	}
	if n := lateCancels.Load(); n != 0 {
		t.Fatalf("session/cancel written after U's prompt in %d of %d interleavings", n, seeds)
	}
	if n := signalsInU.Load(); n != 0 {
		t.Fatalf("SIGINT attempted while U ran in %d of %d interleavings", n, seeds)
	}
	if fenced.Load() == 0 || stale.Load() == 0 {
		t.Fatalf("interleavings covered %d fenced and %d stale interrupts, want both", fenced.Load(), stale.Load())
	}
}

// ownerSocket serves name's control socket from p, as Start does, on sc.
func ownerSocket(t *testing.T, p *Provider, name string, sc *sessionConn) {
	t.Helper()
	var owned atomic.Pointer[sessionConn]
	owned.Store(sc)
	lis, err := p.startControlSocket(name, &exec.Cmd{}, make(chan struct{}), p.ownedControlCommands(name, &owned))
	if err != nil {
		t.Fatalf("startControlSocket: %v", err)
	}
	t.Cleanup(func() { _ = lis.Close() })
}

// Kills cross-process turn commands that do not reach the owner's record, or
// that the owner answers without its fence: another provider over the same
// directory reads idle, running and the recent turns' absence, interrupts
// the running turn fenced, is refused a stale one with what runs, waits for
// the end, and reads a gone owner as gone.
func TestTurnCommandsAcrossProcesses(t *testing.T) {
	withTurnPoll(t, time.Millisecond, 5*time.Millisecond)
	owner := newTestProvider(t)
	rec := &stdinRecorder{}
	name, sc := recordedConn(t, owner, rec)
	ownerSocket(t, owner, name, sc)
	other := NewProviderWithDir(owner.dir, Config{})
	ctx := t.Context()

	if st, err := other.ObserveTurn(ctx, name); err != nil || st.Active() {
		t.Fatalf("idle = %+v, %v; want no turn", st, err)
	}
	id := startTurn(t, sc, 1)
	if st, err := other.ObserveTurn(ctx, name); err != nil || st.TurnID != id {
		t.Fatalf("running = %+v, %v; want %q", st, err, id)
	}
	if h, err := other.TurnHistory(ctx, name); err != nil || h.Current.TurnID != id || h.ObservedAt.IsZero() {
		t.Fatalf("history = %+v, %v; want %q", h, err, id)
	}
	_, err := other.InterruptTurn(ctx, name, "not-"+id)
	var nc *runtime.TurnNotCurrentError
	if !errors.As(err, &nc) || nc.Current.TurnID != id {
		t.Fatalf("stale = %v, want not-current reading %q", err, id)
	}
	if got := rec.methods(); len(got) != 0 {
		t.Fatalf("stale interrupt wrote %q, want nothing", got)
	}
	if ack, err := other.InterruptTurn(ctx, name, id); err != nil || !ack.Fenced {
		t.Fatalf("current = %+v, %v; want sent, fenced", ack, err)
	}
	waitUntil(t, "session/cancel in the owner", func() bool { return slices.Contains(rec.methods(), methodSessionCancel) })
	ended := make(chan error, 1)
	go func() {
		_, err := other.WaitTurnEnded(ctx, name, id, protocolUnitWait)
		ended <- err
	}()
	respondTo(sc, 1, `{"stopReason":"`+acpStopReasonCancelled+`"}`)
	if err := <-ended; err != nil {
		t.Fatalf("WaitTurnEnded = %v, want the end", err)
	}
	if _, err := other.InterruptTurn(ctx, name, id); !errors.As(err, &nc) || nc.Current.Active() {
		t.Fatalf("idle interrupt = %v, want not-current reading no turn", err)
	}
	if st, err := other.WaitTurnEnded(ctx, name, id, 20*time.Millisecond); err != nil || st.Active() {
		t.Fatalf("WaitTurnEnded(ended) = %+v, %v; want ended", st, err)
	}

	sc.drainPending(nil)
	if _, err := other.ObserveTurn(ctx, name); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("drained owner = %v, want ErrSessionNotFound", err)
	}
	if _, err := other.InterruptTurn(ctx, name, id); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("drained owner interrupt = %v, want ErrSessionNotFound", err)
	}
	if got := runtime.TurnCapabilitiesFor(other, name); got.Lossless || got.Fence != runtime.TurnFenceAtomic {
		t.Fatalf("other's capabilities = %+v, want atomic, not lossless", got)
	}
}

// Kills a caller that reads an owner predating the turn commands as idle or
// gone, or that falls back to the untargeted interrupt command: the owner
// double closes every connection unanswered, and the only commands it
// receives are the turn commands themselves.
func TestTurnCommandsAgainstAnOldOwner(t *testing.T) {
	p := newTestProvider(t)
	name := testName()
	var mu sync.Mutex
	var commands []string
	p.dial = func(_, _ string, _ time.Duration) (net.Conn, error) {
		client, owner := net.Pipe()
		go func() {
			scanner := bufio.NewScanner(owner)
			if scanner.Scan() {
				mu.Lock()
				commands = append(commands, scanner.Text())
				mu.Unlock()
			}
			_ = owner.Close()
		}()
		return client, nil
	}
	ctx := t.Context()

	if _, err := p.ObserveTurn(ctx, name); !errors.Is(err, runtime.ErrTurnFenceUnavailable) {
		t.Fatalf("ObserveTurn = %v, want ErrTurnFenceUnavailable", err)
	}
	if _, err := p.TurnHistory(ctx, name); !errors.Is(err, runtime.ErrTurnFenceUnavailable) {
		t.Fatalf("TurnHistory = %v, want ErrTurnFenceUnavailable", err)
	}
	if _, err := p.InterruptTurn(ctx, name, "t1"); !errors.Is(err, runtime.ErrTurnFenceUnavailable) || !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("InterruptTurn = %v, want ErrTurnFenceUnavailable", err)
	}
	mu.Lock()
	got := slices.Clone(commands)
	mu.Unlock()
	if want := []string{"turn", "turn", "interrupt-turn t1"}; !slices.Equal(got, want) {
		t.Fatalf("owner received %q, want %q and no untargeted interrupt", got, want)
	}
}

// withTurnPoll shortens the cross-process WaitTurnEnded poll. It sets
// package variables, so a test that uses it must not run in parallel.
func withTurnPoll(t *testing.T, initial, maxDelay time.Duration) {
	t.Helper()
	prevInitial, prevMax := turnPollInitial, turnPollMax
	turnPollInitial, turnPollMax = initial, maxDelay
	t.Cleanup(func() { turnPollInitial, turnPollMax = prevInitial, prevMax })
}

// Kills a turn-scoped interrupt that misses the real agent's turn or reaches
// it when stale: against the inline agent, a stale id sends no session/cancel
// and no SIGINT, the current id is canceled and its end observed, and the
// control socket of a real owner carries the same commands.
func TestInterruptTurnAgainstTheAgent(t *testing.T) {
	withTurnPoll(t, 5*time.Millisecond, 50*time.Millisecond)
	f := startInterruptFake(t, protocolUnitWait, nil)
	ctx := t.Context()
	f.hold(t)
	st, err := f.p.ObserveTurn(ctx, f.name)
	if err != nil || !st.Active() {
		t.Fatalf("ObserveTurn = %+v, %v; want the held turn", st, err)
	}
	other := NewProviderWithDir(f.p.dir, Config{})
	if remote, err := other.ObserveTurn(ctx, f.name); err != nil || remote != st {
		t.Fatalf("cross-process ObserveTurn = %+v, %v; want %+v", remote, err, st)
	}
	if _, err := other.InterruptTurn(ctx, f.name, "stale"); !errors.Is(err, runtime.ErrTurnNotCurrent) {
		t.Fatalf("stale InterruptTurn = %v, want ErrTurnNotCurrent", err)
	}
	if _, err := f.p.InterruptTurn(ctx, f.name, "stale"); !errors.Is(err, runtime.ErrTurnNotCurrent) {
		t.Fatalf("stale owner InterruptTurn = %v, want ErrTurnNotCurrent", err)
	}
	if ack, err := other.InterruptTurn(ctx, f.name, st.TurnID); err != nil || !ack.Fenced {
		t.Fatalf("InterruptTurn = %+v, %v; want sent, fenced", ack, err)
	}
	if after, err := other.WaitTurnEnded(ctx, f.name, st.TurnID, protocolUnitWait); err != nil || after.Active() {
		t.Fatalf("WaitTurnEnded = %+v, %v; want the turn ended", after, err)
	}
	if n := f.countMethod(t, methodSessionCancel); n != 1 {
		t.Fatalf("session/cancel count = %d, want 1; wire order = %v", n, f.wireOrder(t))
	}
	if n := f.countSIGINT(t); n != 0 {
		t.Fatalf("SIGINT count = %d, want 0", n)
	}
	h, err := f.p.TurnHistory(ctx, f.name)
	if err != nil || len(h.Recent) != 1 || h.Recent[0].TurnID != st.TurnID || h.Recent[0].Status != runtime.TurnStatusCancelled {
		t.Fatalf("history = %+v, %v; want the held turn canceled", h, err)
	}
}
