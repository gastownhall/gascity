package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

// The retained c75 gcg-9gm receipt reported a transcript source but no native
// conversation key or written transcript while its fresh Codex composer read
// "Ask Codex to do anything". The runtime, not those absences, must prove the
// one-shot first-input permission; a quiet screen alone remains unknown.
func bootstrapLaneExecutor(t *testing.T) (*turnLaneFixture, *opjournal.Store, *conversationLaneExec, opjournal.ConversationView) {
	t.Helper()
	f, st, e, v := nativeBoundaryExecutor(t)
	f.turns.history = func(int, int) (runtime.TurnHistory, error) {
		return runtime.TurnHistory{}, runtime.ErrRuntimeUnavailable
	}
	e.wait = func(context.Context, time.Duration) bool { return false }
	return f, st, e, v
}

func grantBootstrapFixture(e *conversationLaneExec, token string) {
	e.bootstrapInput = func(_ context.Context, _ worker.Handle, fence runtime.InputFence) (*runtime.BootstrapInputProof, error) {
		return &runtime.BootstrapInputProof{Fence: fence, Token: token}, nil
	}
}

func TestLaneBootstrapCapturedFreshStartRequiresExplicitRuntimeProof(t *testing.T) {
	_, _, e, v := bootstrapLaneExecutor(t)
	p, err := e.Probe(context.Background(), v)
	if err != nil || p.Boundary != opjournal.TriUnknown || p.TurnLive != opjournal.TriUnknown {
		t.Fatalf("empty key or quiet screen inferred boundary: %+v %v", p, err)
	}
	grantBootstrapFixture(e, "one-runtime-owned-credit")
	p, err = e.Reprobe(context.Background(), v)
	if err != nil || p.Boundary != opjournal.TriYes || p.TurnLive != opjournal.TriUnknown || len(p.KnownTurns) != 0 {
		t.Fatalf("explicit initial boundary fabricated native history: %+v %v", p, err)
	}
}

func TestLaneBootstrapFirstOperationUsesSameBodyKeyAndIncarnationThenWaits(t *testing.T) {
	f, st, e, v := bootstrapLaneExecutor(t)
	calls := 0
	e.bootstrapInput = func(_ context.Context, _ worker.Handle, fence runtime.InputFence) (*runtime.BootstrapInputProof, error) {
		calls++
		if len(f.nudges()) != 0 {
			return nil, nil
		}
		return &runtime.BootstrapInputProof{Fence: fence, Token: "one-runtime-owned-credit"}, nil
	}
	op := f.propose(t, st, f.msg("first-native-input", opjournal.RolePrompt, "unchanged initial public input"))
	l := f.lane(t, st)
	res, err := l.Pass(context.Background(), v, e)
	if err != nil || res.Done != opjournal.DoneAccepted || len(f.nudges()) != 1 || calls < 2 {
		t.Fatalf("initial pass=%+v err=%v inputs=%+v proof reads=%d", res, err, f.nudges(), calls)
	}
	if got := f.get(t, st, op.ID); got.ClientKey != op.ClientKey || got.Payload != op.Payload {
		t.Fatalf("initial operation identity changed: %+v", got)
	}
	n := f.nudges()[0]
	if n.Message != op.Payload || n.Fence != (runtime.InputFence{Generation: 1, Epoch: 1, Token: f.token}) {
		t.Fatalf("input lost body/incarnation: %+v", n)
	}
	next := f.propose(t, st, f.msg("second-native-input", opjournal.RolePrompt, "must wait for actual native history"))
	res, err = l.Pass(context.Background(), v, e)
	if err != nil || len(f.nudges()) != 1 || f.get(t, st, next.ID).Status == opjournal.StatusClosed {
		t.Fatalf("spent credit permitted another unknown turn: pass=%+v err=%v inputs=%+v", res, err, f.nudges())
	}
}

func TestLaneBootstrapRecordedCurrentTurnAndQuestionStillVeto(t *testing.T) {
	for _, mode := range []string{"native current turn", "question"} {
		t.Run(mode, func(t *testing.T) {
			f, st, e, v := bootstrapLaneExecutor(t)
			proofs := 0
			e.bootstrapInput = func(_ context.Context, _ worker.Handle, fence runtime.InputFence) (*runtime.BootstrapInputProof, error) {
				proofs++
				return &runtime.BootstrapInputProof{Fence: fence, Token: "credit"}, nil
			}
			if mode == "native current turn" {
				f.turns.history = func(int, int) (runtime.TurnHistory, error) { return running(laneNativeHoldTurn), nil }
			} else {
				f.laneFixture.sp.SetPendingInteraction(f.name, &runtime.PendingInteraction{RequestID: "native-question", Kind: "question"})
			}
			op := f.propose(t, st, f.msg("never-answer-dialog", opjournal.RolePrompt, "ordinary queued input"))
			res, err := f.lane(t, st).Pass(context.Background(), v, e)
			if err != nil || proofs != 0 || len(f.nudges()) != 0 || f.get(t, st, op.ID).Status == opjournal.StatusClosed {
				t.Fatalf("active/question bypass: pass=%+v err=%v proofs=%d inputs=%+v", res, err, proofs, f.nudges())
			}
		})
	}
}

func TestLaneBootstrapUnprovenCapabilitiesOrProofRemainUnknown(t *testing.T) {
	for _, mode := range []string{"missing handle", "capability error", "wrong generation", "wrong epoch", "wrong token", "empty credit", "observer error"} {
		t.Run(mode, func(t *testing.T) {
			_, _, e, v := bootstrapLaneExecutor(t)
			grantBootstrapFixture(e, "credit")
			switch mode {
			case "missing handle":
				e.turns = func() (worker.TurnHandle, error) { return nil, runtime.ErrSessionNotFound }
			case "capability error":
				e.turns = func() (worker.TurnHandle, error) { return laneStubTurns{capsErr: runtime.ErrRuntimeUnavailable}, nil }
			default:
				e.bootstrapInput = func(_ context.Context, _ worker.Handle, fence runtime.InputFence) (*runtime.BootstrapInputProof, error) {
					p := &runtime.BootstrapInputProof{Fence: fence, Token: "credit"}
					switch mode {
					case "wrong generation":
						p.Fence.Generation++
					case "wrong epoch":
						p.Fence.Epoch++
					case "wrong token":
						p.Fence.Token = "other-instance"
					case "empty credit":
						p.Token = ""
					case "observer error":
						return nil, runtime.ErrRuntimeUnavailable
					}
					return p, nil
				}
			}
			p, err := e.Probe(context.Background(), v)
			if err != nil || p.Boundary != opjournal.TriUnknown {
				t.Fatalf("unproven %s admitted boundary: %+v %v", mode, p, err)
			}
		})
	}
}

func TestLaneBootstrapHandoffLostCreditOrNewTurnLeavesFrozenOperation(t *testing.T) {
	for _, mode := range []string{"spent credit", "new native turn"} {
		t.Run(mode, func(t *testing.T) {
			f, st, e, v := bootstrapLaneExecutor(t)
			grantBootstrapFixture(e, "credit")
			op := f.propose(t, st, f.msg("bootstrap-race", opjournal.RolePrompt, "frozen initial input"))
			exec := beforeDeliverExec{conversationLaneExec: e, before: func() {
				if mode == "spent credit" {
					grantBootstrapFixture(e, "")
				} else {
					f.turns.history = func(int, int) (runtime.TurnHistory, error) { return running(laneNativeHoldTurn), nil }
				}
			}}
			res, err := f.lane(t, st).Pass(context.Background(), v, exec)
			got := f.get(t, st, op.ID)
			if err != nil || len(f.nudges()) != 0 || got.Status == opjournal.StatusClosed || got.MaybeDelivered != 0 || got.ClientKey != op.ClientKey || got.Payload != op.Payload {
				t.Fatalf("handoff race consumed operation: pass=%+v err=%v op=%+v inputs=%+v", res, err, got, f.nudges())
			}
		})
	}
}

func TestLaneBootstrapRedeliveryCannotSpendFreshCredit(t *testing.T) {
	f, _, e, v := bootstrapLaneExecutor(t)
	grantBootstrapFixture(e, "credit")
	if _, err := e.Probe(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	result := e.Deliver(context.Background(), opjournal.Delivery{Text: "unknown previous attempt", Key: "stable-retry", Redelivery: true})
	refused, ok := result.(opjournal.NotDelivered)
	if !ok || !refused.Transient || !errors.Is(refused.Cause, runtime.ErrRuntimeUnavailable) || len(f.nudges()) != 0 {
		t.Fatalf("redelivery spent bootstrap credit: result=%+v inputs=%+v", result, f.nudges())
	}
}

func TestLaneBootstrapOptionalObservationIsBoundedAtFinalHandoff(t *testing.T) {
	_, _, e, v := bootstrapLaneExecutor(t)
	if _, err := e.Probe(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	e.bootstrapInput = func(context.Context, worker.Handle, runtime.InputFence) (*runtime.BootstrapInputProof, error) {
		close(entered)
		<-release // an optional observer that ignores cancellation
		close(returned)
		return nil, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { <-entered; cancel() }()
	t.Cleanup(func() { close(release); <-returned })
	proof, err := e.freshInputProof(ctx, e.live)
	if proof != nil || !errors.Is(err, context.Canceled) || !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("stalled optional observer was not bounded/unavailable: proof=%+v err=%v", proof, err)
	}
}
