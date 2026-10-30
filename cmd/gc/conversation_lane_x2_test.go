package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
)

// dedupFake is a runtime that deduplicates by delivery key, as T3's command
// receipts do: a send under a key it already applied is acked with the
// receipt held for the key's first delivery and not applied again. applied
// lists the texts it applied, sends every send.
type dedupFake struct {
	runtime.FullFake
	mu      sync.Mutex
	seen    map[string]bool
	applied []string
	sends   []runtime.Call
}

func newDedupFake(f *runtime.Fake) *dedupFake {
	return &dedupFake{FullFake: runtime.FullFake{Fake: f}, seen: map[string]bool{}}
}

// sent records a send and reports whether a send under its key already
// applied.
func (d *dedupFake) sent(key, text string) (repeat bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sends = append(d.sends, runtime.Call{Message: text, DeliveryKey: key})
	return key != "" && d.seen[key]
}

// apply records text applied under key.
func (d *dedupFake) apply(key, text string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen[key] = true
	d.applied = append(d.applied, text)
}

// NudgeKeyed implements runtime.KeyedNudgeProvider, applying a key once; a
// repeated key answers with the receipt held for its first delivery.
func (d *dedupFake) NudgeKeyed(name string, content []runtime.ContentBlock, key string) (runtime.DeliveryReceipt, error) {
	text := runtime.FlattenText(content)
	if d.sent(key, text) {
		return d.LookupDelivery(context.Background(), name, key)
	}
	r, err := d.FullFake.NudgeKeyed(name, content, key)
	if err == nil {
		d.apply(key, text)
	}
	return r, err
}

// NudgeFenced implements runtime.FencedInputProvider as NudgeKeyed applies a
// key: a first send is held and its receipt fills n.Receipt
// (FullFake.NudgeFenced), and a repeated key answers with the held receipt.
func (d *dedupFake) NudgeFenced(ctx context.Context, name string, content []runtime.ContentBlock, n runtime.FencedNudge) error {
	text := runtime.FlattenText(content)
	if d.sent(n.DeliveryKey, text) {
		r, err := d.LookupDelivery(ctx, name, n.DeliveryKey)
		if err == nil && n.Receipt != nil {
			*n.Receipt = r
		}
		return err
	}
	if err := d.FullFake.NudgeFenced(ctx, name, content, n); err != nil {
		return err
	}
	d.apply(n.DeliveryKey, text)
	return nil
}

// x2Exec is the live executor host runs for f's conversation over sp: the
// CLI's (under its lease) for cli, the controller's for ctl.
func x2Exec(t *testing.T, f *laneFixture, st *opjournal.Store, sp runtime.Provider, host string) opjournal.Executor {
	t.Helper()
	deps := f.deps(st)
	deps.sp = sp
	_, row := f.view(t)
	e := newConversationLaneExec(deps, row)
	if host == cliLaneHostName {
		return &cliLaneExec{conversationLaneExec: e, ownsInput: true}
	}
	return e
}

// x2Lane is a lane of host over st whose stage function panics laneCrash{}
// at crash ("" for none): the pass ends there as a kill would.
func x2Lane(t *testing.T, f *laneFixture, st *opjournal.Store, host string, crash opjournal.LaneStage) *opjournal.ConversationLane {
	t.Helper()
	l, err := opjournal.NewLane(st, host, f.clk)
	if err != nil {
		t.Fatalf("NewLane: %v", err)
	}
	if crash != "" {
		l.OnStage(func(s opjournal.LaneStage) {
			if s == crash {
				panic(laneCrash{})
			}
		})
	}
	return l
}

func x2Pass(t *testing.T, f *laneFixture, l *opjournal.ConversationLane, exec opjournal.Executor) opjournal.PassResult {
	t.Helper()
	view, _ := f.view(t)
	res, err := l.Pass(context.Background(), view, exec)
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	return res
}

func x2LeaseTTL(host string) time.Duration {
	if host == cliLaneHostName {
		return cliLaneLeaseTTL
	}
	return opjournal.DefaultLeaseTTL
}

// x2CheckDedupRecovery checks the deduplicating runtime's side of a
// recovery from a crash at stage. The recovery looks the delivery up under
// the batch's key once: after the effect the runtime holds it, so the
// recovery sends nothing and records the held receipt via replayed; after
// the Take it holds none, so the recovery sends once and records the
// receipt that send was held under. Either way the text is applied once,
// with no repeat line.
func x2CheckDedupRecovery(t *testing.T, f *laneFixture, dd *dedupFake, got opjournal.Op, stage opjournal.LaneStage) {
	t.Helper()
	key := opjournal.RuntimeDeliveryKey(got.DeliveryKey)
	lookups := 0
	for _, c := range f.sp.SnapshotCalls() {
		if c.Method == "LookupDelivery" && c.DeliveryKey == key {
			lookups++
		}
	}
	via := laneViaLive
	if stage == opjournal.LaneStageEffected {
		via = laneViaReplayed
	}
	held, ok := f.sp.KeyedDeliveries[f.name][key]
	r := got.Outcome.Result
	switch {
	case lookups != 1:
		t.Fatalf("lookups under %q = %d, want the recovery's one", key, lookups)
	case !ok || r.Via != via || r.Delivery.MessageID != held.MessageID || !r.Delivery.MessageAt.Equal(held.MessageAt):
		t.Fatalf("result %+v, held receipt %+v (held %v); want via %s with the held receipt", r, held, ok, via)
	case len(dd.sends) != 1 || dd.sends[0].DeliveryKey != key:
		t.Fatalf("sends %+v, want one send, under %q", dd.sends, key)
	case len(dd.applied) != 1 || strings.Contains(dd.applied[0], opjournal.RedeliveryNote):
		t.Fatalf("applied %q, want the text once with no repeat line", dd.applied)
	}
}

// TestLaneX2DedupColumn is X2's deduplicating-runtime column, in process: a
// lane host (ctl or cli) crashes after its Take or after its effect, and a
// fresh controller lane over the same store recovers the op once the lease
// lapses. On a runtime that deduplicates by delivery key the recovery looks
// the delivery up first and replays the receipt the runtime holds
// (x2CheckDedupRecovery); on a plain runtime the repeat is applied again
// with the repeat line. The op records the repeat either way.
func TestLaneX2DedupColumn(t *testing.T) {
	stores := map[string]func(t *testing.T) beads.Store{
		"mem": func(*testing.T) beads.Store { return newLaneTestStore() },
		"sqlite": func(t *testing.T) beads.Store {
			s, err := beads.OpenSQLiteStore(t.TempDir())
			if err != nil {
				t.Fatalf("OpenSQLiteStore: %v", err)
			}
			return s
		},
	}
	for storeName, open := range stores {
		for _, host := range []string{laneHostName, cliLaneHostName} {
			for _, stage := range []opjournal.LaneStage{opjournal.LaneStageTaken, opjournal.LaneStageEffected} {
				for _, dedup := range []bool{true, false} {
					t.Run(fmt.Sprintf("%s/%s/%s/dedup=%v", storeName, host, stage, dedup), func(t *testing.T) {
						f := newLaneFixtureOver(t, open(t))
						st := f.laneStore(t)
						var sp runtime.Provider = f.sp
						var dd *dedupFake
						if dedup {
							dd = newDedupFake(f.sp)
							sp = dd
						}
						op := f.propose(t, st, f.msg("k-x2", opjournal.RolePrompt, "x2 dedup alpha"))
						if !crashed(func() { x2Pass(t, f, x2Lane(t, f, st, host, stage), x2Exec(t, f, st, sp, host)) }) {
							t.Fatal("the pass did not reach its crash point")
						}
						f.clk.Advance(x2LeaseTTL(host) + opjournal.LeaseSkew + time.Second)
						res := x2Pass(t, f, x2Lane(t, f, st, laneHostName, ""), x2Exec(t, f, st, sp, laneHostName))
						if res.Done != opjournal.DoneAccepted {
							t.Fatalf("recovery pass %+v", res)
						}
						got := f.get(t, st, op.ID)
						if got.Outcome.Arm != opjournal.ArmSucceeded || got.Attempt != 2 || got.MaybeDelivered != 1 {
							t.Fatalf("op %+v, want succeeded on attempt 2 with maybe_delivered 1", got)
						}
						if dedup {
							x2CheckDedupRecovery(t, f, dd, got, stage)
							return
						}
						var texts []string
						for _, n := range f.nudges() {
							texts = append(texts, n.Message)
						}
						switch {
						case stage == opjournal.LaneStageTaken && (len(texts) != 1 || !strings.Contains(texts[0], opjournal.RedeliveryNote)):
							t.Fatalf("applied %q, want the one redelivery with its repeat line", texts)
						case stage == opjournal.LaneStageEffected && (len(texts) != 2 || strings.Contains(texts[0], opjournal.RedeliveryNote) || !strings.Contains(texts[1], opjournal.RedeliveryNote)):
							t.Fatalf("applied %q, want two, the second with the repeat line", texts)
						}
					})
				}
			}
		}
	}
}

// A CLI that dies after its Take holds the batch for its 30 s lease plus the
// skew, not the controller's 2 min: the controller re-takes it after 40 s.
func TestLaneX2CLILeaseRecovery(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	op := f.propose(t, st, f.msg("k-x2-cli", opjournal.RolePrompt, "x2 lease alpha"))
	if !crashed(func() {
		x2Pass(t, f, x2Lane(t, f, st, cliLaneHostName, opjournal.LaneStageTaken), x2Exec(t, f, st, f.sp, cliLaneHostName))
	}) {
		t.Fatal("the CLI's pass did not reach its Take")
	}
	f.clk.Advance(39 * time.Second)
	ctl := x2Lane(t, f, st, laneHostName, "")
	if res := x2Pass(t, f, ctl, x2Exec(t, f, st, f.sp, laneHostName)); res.Took != opjournal.TakeNone || !laneWaitsFor(res, opjournal.WaitInFlight) {
		t.Fatalf("pass 39 s after the CLI's Take = %+v, want in_flight", res)
	}
	f.clk.Advance(2 * time.Second)
	if res := x2Pass(t, f, ctl, x2Exec(t, f, st, f.sp, laneHostName)); res.Done != opjournal.DoneAccepted {
		t.Fatalf("pass 41 s after the CLI's Take = %+v, want the re-take", res)
	}
	if got := f.get(t, st, op.ID); got.Attempt != 2 || got.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("op %+v", got)
	}
}
