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
// receipts do: a send under a key it already applied is acked and not
// applied again. applied lists the texts it applied, sends every send.
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

// once records a send and reports whether it is the key's first.
func (d *dedupFake) once(key, text string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sends = append(d.sends, runtime.Call{Message: text, DeliveryKey: key})
	if key != "" && d.seen[key] {
		return false
	}
	d.seen[key] = true
	d.applied = append(d.applied, text)
	return true
}

// NudgeKeyed implements runtime.KeyedNudgeProvider, applying a key once.
func (d *dedupFake) NudgeKeyed(name string, content []runtime.ContentBlock, key string) error {
	if !d.once(key, runtime.FlattenText(content)) {
		return nil
	}
	return d.FullFake.NudgeKeyed(name, content, key)
}

// NudgeFenced implements runtime.FencedInputProvider, applying a key once.
func (d *dedupFake) NudgeFenced(ctx context.Context, name string, content []runtime.ContentBlock, n runtime.FencedNudge) error {
	if !d.once(n.DeliveryKey, runtime.FlattenText(content)) {
		return nil
	}
	return d.Fake.NudgeFenced(ctx, name, content, n)
}

// x2Exec is the live executor host runs for f's conversation over sp: the
// CLI's (under its lease) for cli, the controller's for ctl.
func x2Exec(t *testing.T, f *laneFixture, st *opjournal.Store, sp runtime.Provider, host string) opjournal.Executor {
	t.Helper()
	deps := f.deps(st)
	deps.sp = sp
	_, info := f.view(t)
	e := newConversationLaneExec(deps, info)
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

// TestLaneX2DedupColumn is X2's deduplicating-runtime column, in process: a
// lane host (ctl or cli) crashes after its Take or after its effect, and a
// fresh controller lane over the same store recovers the op once the lease
// lapses. On a runtime that deduplicates by delivery key the text is applied
// once and every send under the key is byte-identical, with no repeat line;
// on a plain runtime the repeat is applied again with the line. The op
// records the repeat either way.
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
						var texts []string
						if dedup {
							texts = dd.applied
							for _, s := range dd.sends {
								if s.Message != dd.sends[0].Message || s.DeliveryKey != dd.sends[0].DeliveryKey {
									t.Fatalf("sends under the key differ: %+v", dd.sends)
								}
							}
						} else {
							for _, n := range f.nudges() {
								texts = append(texts, n.Message)
							}
						}
						switch {
						case dedup && (len(texts) != 1 || strings.Contains(texts[0], opjournal.RedeliveryNote)):
							t.Fatalf("applied %q, want the text once with no repeat line", texts)
						case !dedup && stage == opjournal.LaneStageTaken && (len(texts) != 1 || !strings.Contains(texts[0], opjournal.RedeliveryNote)):
							t.Fatalf("applied %q, want the one redelivery with its repeat line", texts)
						case !dedup && stage == opjournal.LaneStageEffected && (len(texts) != 2 || strings.Contains(texts[0], opjournal.RedeliveryNote) || !strings.Contains(texts[1], opjournal.RedeliveryNote)):
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
