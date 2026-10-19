package legacyop

import (
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

var t0 = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

// faultStore is a MemStore whose writes can be made to fail.
type faultStore struct {
	*beads.MemStore
	createErr     error // Create fails with this, writing nothing
	ambiguous     bool  // Create commits, then answers an error
	updateFails   atomic.Int32
	updateErr     error // UpdateIfMatch fails with this while updateFails > 0 (or always when < 0)
	updateForever bool
}

func (s *faultStore) Create(b beads.Bead) (beads.Bead, error) {
	if s.createErr != nil {
		return beads.Bead{}, s.createErr
	}
	created, err := s.MemStore.Create(b)
	if err == nil && s.ambiguous {
		return beads.Bead{}, errors.New("transport: connection reset after commit")
	}
	return created, err
}

func (s *faultStore) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	if s.updateForever || s.updateFails.Add(-1) >= 0 {
		return s.updateErr
	}
	return s.MemStore.UpdateIfMatch(id, rev, opts)
}

type env struct {
	mem   *beads.MemStore
	fault *faultStore
	rec   *beadstest.OpRecordingStore
	clk   *clock.Fake
	st    *opjournal.Store
	opens atomic.Int32
}

func newEnv(t *testing.T) *env {
	t.Helper()
	mem := &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}
	e := &env{mem: mem, fault: &faultStore{MemStore: mem, updateErr: errors.New("transport: update failed")}, clk: &clock.Fake{Time: t0}}
	e.rec = beadstest.NewOpRecordingStore(e.fault)
	st, err := opjournal.NewStore(beads.SessionStore{Store: e.rec}, e.clk, opjournal.WithIDPrefix("gc"))
	if err != nil {
		t.Fatal(err)
	}
	e.st = st
	return e
}

func (e *env) open() (*opjournal.Store, error) {
	e.opens.Add(1)
	return e.st, nil
}

func (e *env) recorder(mode gate.Mode, executor opjournal.ExecutorKind, inflight *Inflight) *Recorder {
	return &Recorder{Mode: mode, Executor: executor, Open: e.open, Inflight: inflight, Clock: e.clk, sleep: func(time.Duration) {}}
}

func apiRecorder(e *env, mode gate.Mode) *Recorder {
	return e.recorder(mode, ExecutorAPI, NewInflight())
}

func cliRecorder(e *env, mode gate.Mode) *Recorder { return e.recorder(mode, ExecutorCLI, nil) }

func msg(key, payload string) Request {
	return Request{
		Conversation: "gc-1", Principal: PrincipalOperator, Actor: opjournal.ActorOperator, Key: key,
		Payload: payload, Kind: opjournal.KindMessage,
	}
}

// seed proposes, under executor, the op req would record, at the clock now.
func (e *env) seed(t *testing.T, req Request, executor opjournal.ExecutorKind) opjournal.Op {
	t.Helper()
	now := e.clk.Now()
	op, created, err := e.st.Propose(opjournal.Request{
		Conversation: req.Conversation, Kind: req.Kind, Executor: executor, ClientKey: req.Key,
		Producer: opjournal.Producer{Principal: req.Principal, Actor: req.Actor}, Payload: req.Payload,
		Policy: req.Policy, Scope: req.Scope, StartBy: now.Add(ExecutionBound), FinishBy: now.Add(ExecutionBound),
	})
	if err != nil || !created {
		t.Fatalf("seed: created %v, %v", created, err)
	}
	return op
}

func TestMintKeyAndPrincipal(t *testing.T) {
	re := regexp.MustCompile(`^k-[a-z2-7]{26}$`)
	seen := map[string]bool{}
	for range 64 {
		k, err := MintKey()
		if err != nil || !re.MatchString(k) || opjournal.ValidateClientKey(k) != nil || seen[k] {
			t.Fatalf("MintKey = %q, %v", k, err)
		}
		seen[k] = true
	}
	if got := ConversationPrincipal("gc-7"); got != "conversation:gc-7" {
		t.Fatalf("ConversationPrincipal = %q", got)
	}
	if ExecutionBound != 15*time.Minute || ExecutorAPI != "legacy:api" || ExecutorCLI != "legacy:cli" || PrincipalOperator != "operator" {
		t.Fatal("constants moved")
	}
}

// Off records nothing, opens no store and mints no key.
func TestAdmitOffDoesNoIO(t *testing.T) {
	for _, mode := range []gate.Mode{gate.Off, gate.ModeUnset} {
		e := newEnv(t)
		for _, r := range []*Recorder{apiRecorder(e, mode), cliRecorder(e, mode)} {
			adm, err := r.Admit(msg("", "hello"))
			if err != nil || adm.Decision != Execute || adm.Recorded || adm.Key != "" || adm.Minted {
				t.Fatalf("mode %q: Admit = %+v, %v", mode, adm, err)
			}
			if f := adm.Finish(Result{Outcome: succeeded(ViaLegacyDelivered)}); f.Closed {
				t.Fatalf("mode %q: Finish closed an unrecorded request", mode)
			}
		}
		if e.opens.Load() != 0 || len(e.rec.Ops()) != 0 {
			t.Fatalf("mode %q: opened %d stores, issued %+v", mode, e.opens.Load(), e.rec.Ops())
		}
	}
}

// want is one cell of the mode x store-state matrix.
type want struct {
	err      error // the refusal Admit returns, or nil
	decision Decision
	recorded bool
	adopted  bool
	degraded string // the Degrade stage reported, or ""
}

// TestAdmitMatrix runs every store state under auto and require, for the
// API (with an in-flight registry) and the CLI (without one).
func TestAdmitMatrix(t *testing.T) {
	execRec := want{decision: Execute, recorded: true}
	execUnrec := func(stage string) want { return want{decision: Execute, degraded: stage} }
	unavailable := func(stage string) want { return want{err: ErrUnavailable, degraded: stage} }
	busy := want{err: ErrBusy}
	replay := want{decision: Replay, recorded: true}

	type state struct {
		name  string
		setup func(t *testing.T, e *env, r *Recorder) Request
		// auto and require per surface: api, cli
		auto, require [2]want
	}
	supplied := func(*testing.T, *env, *Recorder) Request { return msg("k1", "hello") }
	states := []state{
		{"created", supplied, [2]want{execRec, execRec}, [2]want{execRec, execRec}},
		{
			"created with a minted key", func(*testing.T, *env, *Recorder) Request { return msg("", "hello") },
			[2]want{execRec, execRec},
			[2]want{execRec, execRec},
		},
		{"incapable store", func(_ *testing.T, _ *env, r *Recorder) Request {
			r.Open = func() (*opjournal.Store, error) {
				return nil, fmt.Errorf("opening op store: %w", beads.ErrConditionalWriteUnsupported)
			}
			return msg("k1", "hello")
		}, [2]want{execUnrec(StageOpen), execUnrec(StageOpen)}, [2]want{unavailable(StageOpen), unavailable(StageOpen)}},
		{"no namespace", func(_ *testing.T, _ *env, r *Recorder) Request {
			r.Open = func() (*opjournal.Store, error) {
				return nil, fmt.Errorf("opening op store: %w", opjournal.ErrNoNamespace)
			}
			return msg("k1", "hello")
		}, [2]want{execUnrec(StageOpen), execUnrec(StageOpen)}, [2]want{unavailable(StageOpen), unavailable(StageOpen)}},
		{"type unregistered", func(_ *testing.T, e *env, _ *Recorder) Request {
			e.fault.createErr = errors.New(`invalid issue type "op"`)
			return msg("k1", "hello")
		}, [2]want{execUnrec(StagePropose), execUnrec(StagePropose)}, [2]want{unavailable(StagePropose), unavailable(StagePropose)}},
		{"propose error", func(_ *testing.T, e *env, _ *Recorder) Request {
			e.fault.createErr = errors.New("transport: refused")
			return msg("k1", "hello")
		}, [2]want{execUnrec(StagePropose), execUnrec(StagePropose)}, [2]want{unavailable(StagePropose), unavailable(StagePropose)}},
		{"ambiguous create", func(_ *testing.T, e *env, _ *Recorder) Request {
			e.fault.ambiguous = true
			return msg("k1", "hello")
		}, [2]want{{decision: Execute, recorded: true, adopted: true}, busy}, [2]want{{decision: Execute, recorded: true, adopted: true}, busy}},
		{
			// A minted 128-bit key names no other op: the open row Propose
			// found after a failed create is this call's own committed row.
			"ambiguous create with a minted key", func(_ *testing.T, e *env, _ *Recorder) Request {
				e.fault.ambiguous = true
				return msg("", "hello")
			},
			[2]want{execRec, execRec},
			[2]want{execRec, execRec},
		},
		{"key reuse", func(t *testing.T, e *env, r *Recorder) Request {
			e.seed(t, msg("k1", "first"), r.Executor)
			return msg("k1", "second")
		}, [2]want{{err: opjournal.ErrKeyReuse}, {err: opjournal.ErrKeyReuse}}, [2]want{{err: opjournal.ErrKeyReuse}, {err: opjournal.ErrKeyReuse}}},
		{"payload too large", func(*testing.T, *env, *Recorder) Request {
			return msg("k1", strings.Repeat("x", opjournal.MaxInlinePayload+1))
		}, [2]want{execUnrec(""), execUnrec("")}, [2]want{{err: opjournal.ErrPayloadTooLarge}, {err: opjournal.ErrPayloadTooLarge}}},
		{
			"invalid key", func(*testing.T, *env, *Recorder) Request { return msg(" k1", "hello") },
			[2]want{execUnrec(""), execUnrec("")},
			[2]want{{err: opjournal.ErrInvalidRequest}, {err: opjournal.ErrInvalidRequest}},
		},
		{"closed replay", func(t *testing.T, e *env, r *Recorder) Request {
			op := e.seed(t, msg("k1", "hello"), r.Executor)
			if _, err := e.st.Close(op.ID, succeeded(ViaLegacyQueued), "legacy:api"); err != nil {
				t.Fatal(err)
			}
			return msg("k1", "hello")
		}, [2]want{replay, replay}, [2]want{replay, replay}},
		{"open legacy:api orphan", func(t *testing.T, e *env, _ *Recorder) Request {
			e.seed(t, msg("k1", "hello"), ExecutorAPI)
			return msg("k1", "hello")
		}, [2]want{{decision: Execute, recorded: true, adopted: true}, busy}, [2]want{{decision: Execute, recorded: true, adopted: true}, busy}},
		{"open legacy:cli", func(t *testing.T, e *env, _ *Recorder) Request {
			e.seed(t, msg("k1", "hello"), ExecutorCLI)
			return msg("k1", "hello")
		}, [2]want{busy, busy}, [2]want{busy, busy}},
		{"open lane op", func(t *testing.T, e *env, _ *Recorder) Request {
			e.seed(t, msg("k1", "hello"), opjournal.ExecutorLane)
			return msg("k1", "hello")
		}, [2]want{busy, busy}, [2]want{busy, busy}},
		{"expired lane op", func(t *testing.T, e *env, _ *Recorder) Request {
			e.seed(t, msg("k1", "hello"), opjournal.ExecutorLane)
			e.clk.Advance(ExecutionBound + time.Second)
			return msg("k1", "hello")
		}, [2]want{busy, busy}, [2]want{busy, busy}},
		{"expired legacy:api op", func(t *testing.T, e *env, _ *Recorder) Request {
			e.seed(t, msg("k1", "hello"), ExecutorAPI)
			e.clk.Advance(ExecutionBound + time.Second)
			return msg("k1", "hello")
		}, [2]want{replay, replay}, [2]want{replay, replay}},
		{"expired legacy:cli op", func(t *testing.T, e *env, _ *Recorder) Request {
			e.seed(t, msg("k1", "hello"), ExecutorCLI)
			e.clk.Advance(ExecutionBound + time.Second)
			return msg("k1", "hello")
		}, [2]want{replay, replay}, [2]want{replay, replay}},
	}
	for _, s := range states {
		for _, mode := range []gate.Mode{gate.Auto, gate.Require} {
			for i, surface := range []string{"api", "cli"} {
				t.Run(fmt.Sprintf("%s/%s/%s", s.name, mode, surface), func(t *testing.T) {
					e := newEnv(t)
					r := cliRecorder(e, mode)
					if surface == "api" {
						r = apiRecorder(e, mode)
					}
					var degraded []Degrade
					r.Degraded = func(d Degrade) { degraded = append(degraded, d) }
					w := s.auto[i]
					if mode == gate.Require {
						w = s.require[i]
					}
					req := s.setup(t, e, r)
					adm, err := r.Admit(req)
					if w.err != nil {
						if !errors.Is(err, w.err) || adm != nil {
							t.Fatalf("Admit = %+v, %v; want %v", adm, err, w.err)
						}
					} else {
						if err != nil {
							t.Fatalf("Admit: %v", err)
						}
						if adm.Decision != w.decision || adm.Recorded != w.recorded || adm.Adopted != w.adopted {
							t.Fatalf("Admit = decision %d recorded %t adopted %t; want %+v", adm.Decision, adm.Recorded, adm.Adopted, w)
						}
						if adm.Key == "" || (req.Key == "") != adm.Minted {
							t.Fatalf("key %q minted %t for supplied %q", adm.Key, adm.Minted, req.Key)
						}
						if adm.Recorded && adm.Op.ID == "" {
							t.Fatal("a recorded admission carries no op")
						}
						if w.adopted && adm.Op.Attempt != 1 {
							t.Fatalf("adopted at attempt %d, want 1", adm.Op.Attempt)
						}
						if adm.Decision == Replay && adm.Op.Status != opjournal.StatusClosed {
							t.Fatalf("replayed an op in %s", adm.Op.Status)
						}
						if (adm.Entry != nil) != (surface == "api" && adm.Decision == Execute && adm.Recorded) {
							t.Fatalf("entry %v for %s %+v", adm.Entry, surface, w)
						}
					}
					switch {
					case w.degraded == "" && len(degraded) != 0:
						t.Fatalf("degraded %+v, want none", degraded)
					case w.degraded != "" && (len(degraded) != 1 || degraded[0].Stage != w.degraded || degraded[0].Mode != mode || degraded[0].Err == nil):
						t.Fatalf("degraded %+v, want one at %s", degraded, w.degraded)
					}
					if r.Inflight != nil && (adm == nil || adm.Entry == nil) && r.Inflight.Len() != 0 {
						t.Fatalf("registry holds %d entries after a refusal or replay", r.Inflight.Len())
					}
				})
			}
		}
	}
}

// Either surface closes an open legacy op past its finish_by itself, as the
// sweeper would, and replays that. Neither adopts it: its executor may have
// delivered, so a re-run could deliver twice.
func TestAdmitClosesAnExpiredOp(t *testing.T) {
	for _, surface := range []string{"api", "cli"} {
		for _, executor := range []opjournal.ExecutorKind{ExecutorAPI, ExecutorCLI} {
			t.Run(fmt.Sprintf("%s/%s", surface, executor), func(t *testing.T) {
				e := newEnv(t)
				r := cliRecorder(e, gate.Auto)
				if surface == "api" {
					r = apiRecorder(e, gate.Auto)
				}
				op := e.seed(t, msg("k1", "hello"), executor)
				e.clk.Advance(ExecutionBound + time.Second)
				adm, err := r.Admit(msg("k1", "hello"))
				if err != nil || adm.Decision != Replay || adm.Adopted || adm.Op.Status != opjournal.StatusClosed ||
					adm.Op.Attempt != 0 || adm.Op.Outcome != failed(opjournal.ReasonDeadlineInFlight) {
					t.Fatalf("Admit = %+v, %v", adm, err)
				}
				if got, err := e.st.Get(op.ID); err != nil || got.Status != opjournal.StatusClosed || got.Attempt != 0 {
					t.Fatalf("stored op = %+v, %v; want closed at attempt 0", got, err)
				}
				if r.Inflight != nil && r.Inflight.Len() != 0 {
					t.Fatalf("registry holds %d entries after the replay", r.Inflight.Len())
				}
			})
		}
	}
}

// Only lanes settle lane ops: either surface's expiry rule leaves an expired
// lane op that shares the id open, writing nothing.
func TestAdmitLeavesAnExpiredLaneOp(t *testing.T) {
	for _, surface := range []string{"api", "cli"} {
		t.Run(surface, func(t *testing.T) {
			e := newEnv(t)
			r := cliRecorder(e, gate.Auto)
			if surface == "api" {
				r = apiRecorder(e, gate.Auto)
			}
			lane := e.seed(t, msg("k1", "hello"), opjournal.ExecutorLane)
			e.clk.Advance(ExecutionBound + time.Second)
			rev := lane.Revision
			if adm, err := r.Admit(msg("k1", "hello")); !errors.Is(err, ErrBusy) {
				t.Fatalf("Admit = %+v, %v; want busy", adm, err)
			}
			if b, _ := e.mem.Get(lane.ID); b.Revision != rev || b.Status != "open" {
				t.Fatalf("the lane op was written: revision %d (was %d), %s", b.Revision, rev, b.Status)
			}
		})
	}
}

// Admit decides expiry on a clock read after it reserves the op's in-flight
// entry: a holder releases the entry only past the op's finish_by, so a
// clock read before Reserve could find the op unexpired and adopt an op the
// holder may have delivered. Here the clock passes finish_by while Admit
// opens the store, before Reserve.
func TestAdmitReadsTheClockAfterReserving(t *testing.T) {
	e := newEnv(t)
	e.seed(t, msg("k1", "hello"), ExecutorAPI)
	r := apiRecorder(e, gate.Auto)
	r.Open = func() (*opjournal.Store, error) {
		e.clk.Advance(ExecutionBound + time.Nanosecond)
		return e.st, nil
	}
	adm, err := r.Admit(msg("k1", "hello"))
	if err != nil || adm.Decision != Replay || adm.Adopted || adm.Op.Outcome != failed(opjournal.ReasonDeadlineInFlight) {
		t.Fatalf("Admit = %+v, %v; want a replay of deadline_in_flight", adm, err)
	}
}

// A recorder records only as a legacy surface, the API or the CLI: any other
// executor is refused before any store I/O, so Finish and the expiry rule can
// never act on a lane op.
func TestAdmitRefusesANonLegacySurface(t *testing.T) {
	for _, ex := range []opjournal.ExecutorKind{opjournal.ExecutorLane, opjournal.ExecutorLegacy, "", "box"} {
		for _, mode := range []gate.Mode{gate.Auto, gate.Require} {
			e := newEnv(t)
			r := e.recorder(mode, ex, nil)
			adm, err := r.Admit(msg("k1", "hello"))
			if !errors.Is(err, opjournal.ErrInvalidRequest) || adm != nil {
				t.Fatalf("%q/%s: Admit = %+v, %v; want ErrInvalidRequest", ex, mode, adm, err)
			}
			if e.opens.Load() != 0 {
				t.Fatalf("%q/%s: opened the store", ex, mode)
			}
		}
	}
}

// A request in flight here answers a same-key request with its cached answer
// once it has one, and busy before; a nil answer is none, since the replay
// carries the open op, which has no outcome to build one from. Another digest
// is a key reuse.
func TestAdmitInflightReplay(t *testing.T) {
	e := newEnv(t)
	r := apiRecorder(e, gate.Auto)
	first, err := r.Admit(msg("k1", "hello"))
	if err != nil || first.Decision != Execute {
		t.Fatalf("first = %+v, %v", first, err)
	}
	if _, err := r.Admit(msg("k1", "hello")); !errors.Is(err, ErrBusy) {
		t.Fatalf("before Accept: %v, want ErrBusy", err)
	}
	first.Accept(nil)
	if adm, err := r.Admit(msg("k1", "hello")); !errors.Is(err, ErrBusy) {
		t.Fatalf("after a nil Accept: %+v, %v; want ErrBusy", adm, err)
	}
	first.Accept("202 body")
	second, err := r.Admit(msg("k1", "hello"))
	if err != nil || second.Decision != Replay || second.Cached != "202 body" || second.Op.ID != first.Op.ID {
		t.Fatalf("after Accept: %+v, %v", second, err)
	}
	if second.Finish(Result{Outcome: succeeded(ViaLegacy)}).Closed {
		t.Fatal("Finish of a replay closed the op")
	}
	if _, err := r.Admit(msg("k1", "other")); !errors.Is(err, opjournal.ErrKeyReuse) {
		t.Fatalf("other digest: %v, want ErrKeyReuse", err)
	}
	if f := first.Finish(Result{Outcome: succeeded(ViaLegacyDelivered)}); !f.Closed || r.Inflight.Len() != 0 {
		t.Fatalf("Finish = %+v, registry %d", f, r.Inflight.Len())
	}
	third, err := r.Admit(msg("k1", "hello"))
	if err != nil || third.Decision != Replay || third.Op.Outcome != succeeded(ViaLegacyDelivered) {
		t.Fatalf("after Finish: %+v, %v", third, err)
	}
}

func TestFinish(t *testing.T) {
	out := succeeded(ViaLegacyDelivered)
	t.Run("closes and releases", func(t *testing.T) {
		e := newEnv(t)
		r := apiRecorder(e, gate.Auto)
		adm, _ := r.Admit(msg("k1", "hello"))
		f := adm.Finish(Result{Outcome: out})
		if !f.Closed || f.Op.Outcome != out || f.Background != nil || r.Inflight.Len() != 0 {
			t.Fatalf("Finish = %+v, registry %d", f, r.Inflight.Len())
		}
		if got, _ := e.st.Get(adm.Op.ID); got.Status != opjournal.StatusClosed || got.Outcome != out {
			t.Fatalf("stored %+v", got)
		}
		if again := adm.Finish(Result{Outcome: failed(opjournal.ReasonOther)}); again.Closed || again.Late {
			t.Fatalf("second Finish = %+v", again)
		}
	})
	t.Run("retries a failed close", func(t *testing.T) {
		e := newEnv(t)
		r := cliRecorder(e, gate.Require)
		var waits []time.Duration
		r.sleep = func(d time.Duration) { waits = append(waits, d) }
		adm, _ := r.Admit(msg("k1", "hello"))
		e.fault.updateFails.Store(2)
		if f := adm.Finish(Result{Outcome: out}); !f.Closed {
			t.Fatalf("Finish = %+v", f)
		}
		if fmt.Sprint(waits) != fmt.Sprint([]time.Duration{closeRetryGap, closeRetryGap}) {
			t.Fatalf("waits %v", waits)
		}
	})
	t.Run("the CLI gives up and leaves the op to the sweeper", func(t *testing.T) {
		e := newEnv(t)
		r := cliRecorder(e, gate.Auto)
		var degraded []Degrade
		r.Degraded = func(d Degrade) { degraded = append(degraded, d) }
		adm, _ := r.Admit(msg("k1", "hello"))
		e.fault.updateForever = true
		if f := adm.Finish(Result{Outcome: out}); f.Closed || f.Late || f.Background != nil {
			t.Fatalf("Finish = %+v", f)
		}
		if len(degraded) != 1 || degraded[0].Stage != StageClose || degraded[0].Surface != SurfaceCLI {
			t.Fatalf("degraded %+v", degraded)
		}
		if got, _ := e.st.Get(adm.Op.ID); got.Status != opjournal.StatusSubmitted {
			t.Fatalf("op %s after a failed close", got.Status)
		}
	})
	t.Run("the API retries in the background, holding the entry", func(t *testing.T) {
		e := newEnv(t)
		r := apiRecorder(e, gate.Auto)
		gate1 := make(chan struct{})
		var calls atomic.Int32
		r.sleep = func(time.Duration) {
			if calls.Add(1) == closeRetries { // the first background wait
				<-gate1
			}
		}
		adm, _ := r.Admit(msg("k1", "hello"))
		e.fault.updateFails.Store(closeRetries + 2)
		f := adm.Finish(Result{Outcome: out})
		if f.Closed || f.Background == nil {
			t.Fatalf("Finish = %+v", f)
		}
		if r.Inflight.Len() != 1 {
			t.Fatalf("registry %d while the background close runs", r.Inflight.Len())
		}
		if _, err := r.Admit(msg("k1", "hello")); !errors.Is(err, ErrBusy) {
			t.Fatalf("same key during the background close: %v, want ErrBusy", err)
		}
		close(gate1)
		<-f.Background
		if r.Inflight.Len() != 0 {
			t.Fatalf("registry %d after the background close", r.Inflight.Len())
		}
		if got, _ := e.st.Get(adm.Op.ID); got.Status != opjournal.StatusClosed || got.Outcome != out {
			t.Fatalf("stored %+v", got)
		}
	})
	t.Run("the background close gives up at finish_by", func(t *testing.T) {
		e := newEnv(t)
		r := apiRecorder(e, gate.Auto)
		adm, _ := r.Admit(msg("k1", "hello"))
		r.sleep = func(time.Duration) { e.clk.Advance(time.Minute) }
		e.fault.updateForever = true
		f := adm.Finish(Result{Outcome: out})
		<-f.Background
		if r.Inflight.Len() != 0 {
			t.Fatalf("registry %d after giving up", r.Inflight.Len())
		}
		if !e.clk.Now().After(adm.Op.FinishBy) {
			t.Fatalf("gave up at %s, before finish_by %s", e.clk.Now(), adm.Op.FinishBy)
		}
		// The store recovers before the sweeper runs: a same-key retry closes
		// the op as expired rather than adopting an op whose effect may have
		// landed.
		e.fault.updateForever = false
		again, err := r.Admit(msg("k1", "hello"))
		if err != nil || again.Decision != Replay || again.Adopted || again.Op.Attempt != 0 ||
			again.Op.Outcome != failed(opjournal.ReasonDeadlineInFlight) {
			t.Fatalf("same-key retry = %+v, %v; want a replay of deadline_in_flight", again, err)
		}
		if r.Inflight.Len() != 0 {
			t.Fatalf("registry %d after the retry", r.Inflight.Len())
		}
	})
	t.Run("the background close reopens the store for each attempt", func(t *testing.T) {
		e := newEnv(t)
		r := apiRecorder(e, gate.Auto)
		adm, _ := r.Admit(msg("k1", "hello"))
		r.sleep = func(time.Duration) { e.clk.Advance(time.Minute) }
		e.fault.updateForever = true
		healthy, err := opjournal.NewStore(beads.SessionStore{Store: e.mem}, e.clk, opjournal.WithIDPrefix("gc"))
		if err != nil {
			t.Fatal(err)
		}
		var reopens atomic.Int32
		r.Open = func() (*opjournal.Store, error) {
			if reopens.Add(1) == 1 {
				return nil, errors.New("transport: refused")
			}
			return healthy, nil
		}
		f := adm.Finish(Result{Outcome: out})
		if f.Closed || f.Background == nil {
			t.Fatalf("Finish = %+v, want a background close", f)
		}
		<-f.Background
		if got, _ := e.st.Get(adm.Op.ID); got.Status != opjournal.StatusClosed || got.Outcome != out {
			t.Fatalf("stored %+v", got)
		}
		if r.Inflight.Len() != 0 || reopens.Load() != 2 {
			t.Fatalf("registry %d, reopened %d times; want 0 and 2", r.Inflight.Len(), reopens.Load())
		}
	})
	t.Run("leave open, then a same-key retry adopts", func(t *testing.T) {
		e := newEnv(t)
		r := apiRecorder(e, gate.Auto)
		adm, _ := r.Admit(msg("k1", "hello"))
		res := Sentinels{}.SyncResult(opjournal.KindMessage, errors.New("starting"), false)
		if !res.LeaveOpen {
			t.Fatal("setup: a client-keyed retryable error is not left open")
		}
		if f := adm.Finish(res); f.Closed || r.Inflight.Len() != 0 {
			t.Fatalf("Finish = %+v, registry %d", f, r.Inflight.Len())
		}
		if got, _ := e.st.Get(adm.Op.ID); got.Status != opjournal.StatusSubmitted {
			t.Fatalf("left-open op is %s", got.Status)
		}
		again, err := r.Admit(msg("k1", "hello"))
		if err != nil || again.Decision != Execute || !again.Adopted || again.Op.Attempt != 1 {
			t.Fatalf("retry = %+v, %v", again, err)
		}
	})
	t.Run("the CLI closes what the API would leave open", func(t *testing.T) {
		// CLI ops always close (DES-O0API §8.2): nobody can adopt one, so a
		// left-open op would only turn the next same-key run busy and then
		// replay the sweeper's failed{deadline_in_flight}.
		e := newEnv(t)
		r := cliRecorder(e, gate.Auto)
		adm, err := r.Admit(msg("k1", "hello"))
		if err != nil {
			t.Fatal(err)
		}
		res := testSentinels.SyncResult(opjournal.KindWake, wrap(errStarting), adm.Minted)
		if !res.LeaveOpen {
			t.Fatal("setup: a client-keyed retryable error is not left open")
		}
		f := adm.Finish(res)
		if !f.Closed || f.Op.Outcome != res.Outcome {
			t.Fatalf("Finish = %+v", f)
		}
		again, err := r.Admit(msg("k1", "hello"))
		if err != nil || again.Decision != Replay || again.Op.Outcome != failed(opjournal.RefusedReasonPrefix+"session_starting") {
			t.Fatalf("same-key run = %+v, %v; want a replay of the refusal", again, err)
		}
	})
	t.Run("a late result", func(t *testing.T) {
		e := newEnv(t)
		r := cliRecorder(e, gate.Auto)
		adm, _ := r.Admit(msg("k1", "hello"))
		e.clk.Advance(ExecutionBound + time.Second)
		if stats, err := Sweep(e.st, e.clk.Now(), 10, "controller:op-sweep"); err != nil || stats.Closed != 1 {
			t.Fatalf("Sweep = %+v, %v", stats, err)
		}
		if f := adm.Finish(Result{Outcome: out}); !f.Late || f.Closed {
			t.Fatalf("Finish after the sweep = %+v", f)
		}
		if got, _ := e.st.Get(adm.Op.ID); got.Outcome != failed(opjournal.ReasonDeadlineInFlight) {
			t.Fatalf("the late result overwrote the swept outcome: %+v", got.Outcome)
		}
	})
	t.Run("a panicking handler still releases its entry", func(t *testing.T) {
		e := newEnv(t)
		r := apiRecorder(e, gate.Auto)
		func() {
			adm, err := r.Admit(msg("k1", "hello"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if recover() != nil {
					adm.Finish(Result{Outcome: failed(opjournal.ReasonOther)})
				}
			}()
			panic("handler bug")
		}()
		if r.Inflight.Len() != 0 {
			t.Fatalf("registry %d after the panic", r.Inflight.Len())
		}
		if got, _ := e.st.Get(mustPreviewID(t, msg("k1", "hello"))); got.Outcome != failed(opjournal.ReasonOther) {
			t.Fatalf("stored %+v", got.Outcome)
		}
	})
}

func mustPreviewID(t *testing.T, req Request) string {
	t.Helper()
	id, err := opjournal.DeriveID("gc", req.Conversation, req.Principal, req.Key)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Thirty-two concurrent same-key requests on one API: exactly one executes;
// every other one replays (the cached answer, or the stored outcome) or is
// busy.
func TestAdmitConcurrentSameKey(t *testing.T) {
	e := newEnv(t)
	r := apiRecorder(e, gate.Auto)
	var executed, replayed, busy atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			adm, err := r.Admit(msg("k1", "hello"))
			switch {
			case errors.Is(err, ErrBusy):
				busy.Add(1)
			case err != nil:
				t.Errorf("Admit = %v", err)
			case adm.Decision == Execute:
				executed.Add(1)
				adm.Accept("202 body")
				runtime.Gosched()
				adm.Finish(Result{Outcome: succeeded(ViaLegacyDelivered)})
			case adm.Decision == Replay:
				if adm.Cached != "202 body" && adm.Op.Status != opjournal.StatusClosed {
					t.Errorf("replay with neither a cached answer nor a closed op: %+v", adm)
				}
				replayed.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if executed.Load() != 1 || executed.Load()+replayed.Load()+busy.Load() != 32 {
		t.Fatalf("executed %d replayed %d busy %d", executed.Load(), replayed.Load(), busy.Load())
	}
	if r.Inflight.Len() != 0 {
		t.Fatalf("registry %d after every request finished", r.Inflight.Len())
	}
}

// legacyop reaches the journal through opjournal and the gate, clock and
// telemetry leaves only: never internal/api, cmd/gc, internal/config or
// internal/events (DES-O0API §5).
func TestImportBoundary(t *testing.T) {
	allowed := map[string]bool{
		"github.com/gastownhall/gascity/internal/beads":        true,
		"github.com/gastownhall/gascity/internal/clock":        true,
		"github.com/gastownhall/gascity/internal/opjournal":    true,
		"github.com/gastownhall/gascity/internal/rollout/gate": true,
		"github.com/gastownhall/gascity/internal/telemetry":    true,
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if strings.Contains(strings.SplitN(p, "/", 2)[0], ".") && !allowed[p] {
				t.Errorf("%s imports %s", name, p)
			}
		}
	}
	if checked == 0 {
		t.Fatal("scanned no package files")
	}
}

func TestDegradeReasonsAndUnrecorded(t *testing.T) {
	for err, want := range map[error]string{
		fmt.Errorf("open: %w", beads.ErrConditionalWriteUnsupported): DegradeConditionalWritesUnsupported,
		fmt.Errorf("x: %w", opjournal.ErrBeadTypeUnregistered):       DegradeTypeUnregistered,
		fmt.Errorf("x: %w", opjournal.ErrNoNamespace):                DegradeNoNamespace,
		fmt.Errorf("x: %w", opjournal.ErrContention):                 DegradeContention,
		errors.New("transport"):                                      DegradeStoreError,
	} {
		if got := DegradeReason(err); got != want {
			t.Errorf("DegradeReason(%v) = %s, want %s", err, got, want)
		}
	}
	if openReason(opjournal.ErrNoNamespace) != ReasonNoNamespace || openReason(errors.New("x")) != ReasonStoreUnavailable {
		t.Error("openReason")
	}
	if proposeReason(opjournal.ErrBeadTypeUnregistered) != ReasonTypeUnregistered || proposeReason(errors.New("x")) != ReasonProposeError {
		t.Error("proposeReason")
	}
	e := newEnv(t)
	adm := apiRecorder(e, gate.Auto).Unrecorded(opjournal.KindMessage, "k1", ReasonUnmaterializedTarget)
	if adm.Decision != Execute || adm.Recorded || adm.Key != "k1" {
		t.Fatalf("Unrecorded = %+v", adm)
	}
	if f := adm.Finish(Result{Outcome: succeeded(ViaLegacyDelivered)}); f.Closed || e.opens.Load() != 0 {
		t.Fatalf("Finish of an unrecorded request = %+v, opens %d", f, e.opens.Load())
	}
}

// panicStore panics on Create or UpdateIfMatch when armed: a store bug.
type panicStore struct {
	*beads.MemStore
	panicCreate, panicUpdate bool
}

func (s *panicStore) Create(b beads.Bead) (beads.Bead, error) {
	if s.panicCreate {
		panic("store bug in Create")
	}
	return s.MemStore.Create(b)
}

func (s *panicStore) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	if s.panicUpdate {
		panic("store bug in UpdateIfMatch")
	}
	return s.MemStore.UpdateIfMatch(id, rev, opts)
}

func newPanicRecorder(t *testing.T) (*panicStore, *Recorder) {
	t.Helper()
	ps := &panicStore{MemStore: &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}}
	clk := &clock.Fake{Time: t0}
	st, err := opjournal.NewStore(beads.SessionStore{Store: ps}, clk, opjournal.WithIDPrefix("gc"))
	if err != nil {
		t.Fatal(err)
	}
	return ps, &Recorder{
		Mode: gate.Auto, Executor: ExecutorAPI, Open: func() (*opjournal.Store, error) { return st, nil },
		Inflight: NewInflight(), Clock: clk, sleep: func(time.Duration) {},
	}
}

// recovered runs f and reports whether it panicked.
func recovered(f func()) (panicked bool) {
	defer func() { panicked = recover() != nil }()
	f()
	return false
}

// A panic inside Admit after it reserved the id releases the entry and
// propagates: the handler's recovery has no Admission to Finish.
func TestAdmitPanicReleasesTheEntry(t *testing.T) {
	ps, r := newPanicRecorder(t)
	ps.panicCreate = true
	if !recovered(func() { _, _ = r.Admit(msg("k1", "hello")) }) {
		t.Fatal("Admit swallowed the store's panic")
	}
	if n := r.Inflight.Len(); n != 0 {
		t.Fatalf("registry holds %d entries after a panic inside Admit", n)
	}
	ps.panicCreate = false
	if adm, err := r.Admit(msg("k1", "hello")); err != nil || adm.Decision != Execute {
		t.Fatalf("same-key retry = %+v, %v", adm, err)
	}
}

// A panic inside Finish's close releases the entry and propagates; the
// recovery's second Finish is a no-op.
func TestFinishPanicReleasesTheEntry(t *testing.T) {
	ps, r := newPanicRecorder(t)
	adm, err := r.Admit(msg("k1", "hello"))
	if err != nil {
		t.Fatal(err)
	}
	ps.panicUpdate = true
	if !recovered(func() { adm.Finish(Result{Outcome: succeeded(ViaLegacyDelivered)}) }) {
		t.Fatal("Finish swallowed the store's panic")
	}
	if n := r.Inflight.Len(); n != 0 {
		t.Fatalf("registry holds %d entries after a panic inside Finish", n)
	}
	if f := adm.Finish(Result{Outcome: failed(opjournal.ReasonOther)}); f.Closed || f.Late || f.Background != nil {
		t.Fatalf("second Finish = %+v", f)
	}
}
