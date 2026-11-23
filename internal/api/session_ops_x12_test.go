package api

import (
	"net/http"
	"net/http/httptest"
	goruntime "runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/runtime"
)

// X12's in-process cases (DES-O0API §15 OJ-8): the crash points a
// cross-process run on the exec fake cannot serve, DES-T3I §14 item 8's K3
// interrupt_now on the T3 fake profile and /respond, plus the API's wiring of
// the State's crash-point hook. A "crash" here parks the execution at its
// crash point; a restart is a new supervisor over the same store and runtime,
// with an empty LRU and in-flight registry. At cleanup the parked goroutine
// ends with runtime.Goexit from inside the hook: its deferred cleanups run,
// but no further effect, close or event does, so like a killed process it
// leaves the store as it stood.

// crashPointState is a State that answers OpsCrashPointsProvider with fn.
type crashPointState struct {
	*fakeState
	fn legacyop.CrashPointFunc
}

func (c *crashPointState) OpsCrashPoints() legacyop.CrashPointFunc { return c.fn }

// parkAt parks the first execution at attempt 0 to reach point: it reports
// the op on reached and waits for release. A released park ends its
// goroutine (runtime.Goexit) unless proceed is set, in which case the
// execution goes on.
type parkAt struct {
	point   legacyop.CrashPoint
	proceed bool

	armed   atomic.Bool
	reached chan opjournal.Op
	release chan struct{}
	exited  chan struct{}
	once    sync.Once
	seen    atomic.Int32
}

func newParkAt(t *testing.T, point legacyop.CrashPoint, proceed bool) *parkAt {
	t.Helper()
	p := &parkAt{
		point: point, proceed: proceed,
		reached: make(chan opjournal.Op, 1), release: make(chan struct{}), exited: make(chan struct{}),
	}
	p.armed.Store(true)
	t.Cleanup(p.end)
	return p
}

func (p *parkAt) hook(cp legacyop.CrashPoint, op opjournal.Op) error {
	if cp != p.point {
		return nil
	}
	p.seen.Add(1)
	if op.Attempt != 0 || !p.armed.CompareAndSwap(true, false) {
		return nil
	}
	p.reached <- op
	<-p.release
	if p.proceed {
		close(p.exited)
		return nil
	}
	defer close(p.exited)
	goruntime.Goexit()
	return nil
}

// wait is the parked op, once the execution reached the point.
func (p *parkAt) wait(t *testing.T) opjournal.Op {
	t.Helper()
	select {
	case op := <-p.reached:
		return op
	case <-time.After(testEventTimeout):
		t.Fatalf("no execution reached %s", p.point)
		return opjournal.Op{}
	}
}

// end releases a parked execution and waits for its goroutine to leave the
// hook; a park that never fired has nothing to end.
func (p *parkAt) end() {
	p.once.Do(func() {
		close(p.release)
		if !p.armed.Load() {
			<-p.exited
		}
	})
}

// withCrashPoints serves e's city through a State that carries fn.
func (e *opsEnv) withCrashPoints(fn legacyop.CrashPointFunc) {
	e.resolver.state = &crashPointState{fakeState: e.fs, fn: fn}
}

// postAsync runs e.post on its own goroutine, so a handler parked at a crash
// point does not block the test; the answer arrives on the channel when the
// handler returns, and never when the park ends it.
func (e *opsEnv) postAsync(t *testing.T, path, key, body string) <-chan *httptest.ResponseRecorder {
	t.Helper()
	ch := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		ch <- postSessionAction(t, e.h, cityURL(e.fs, path), key, body)
	}()
	return ch
}

// callsSince are sp's calls of any of methods to name after the first n.
func callsSince(sp *runtime.Fake, n int, name string, methods ...string) []runtime.Call {
	var out []runtime.Call
	for i, c := range sp.SnapshotCalls() {
		if i >= n && c.Name == name && (len(methods) == 0 || slices.Contains(methods, c.Method)) {
			out = append(out, c)
		}
	}
	return out
}

// A State that provides a crash-point hook gives it to the content, control
// and lifecycle recorders of its Servers; a plain State gives them none.
func TestOpsCrashPointsReachEveryRecorder(t *testing.T) {
	e := newControlOpsEnv(t, "auto", "auto")
	s := e.server(t)
	for name, r := range map[string]*legacyop.Recorder{"content": s.ops.content, "control": s.ops.control, "lifecycle": s.ops.lifecycle} {
		if r.CrashPoints != nil {
			t.Fatalf("%s recorder carries a hook under a plain State", name)
		}
	}
	var heard atomic.Int32
	e.withCrashPoints(func(legacyop.CrashPoint, opjournal.Op) error {
		heard.Add(1)
		return nil
	})
	s = e.server(t)
	for name, r := range map[string]*legacyop.Recorder{"content": s.ops.content, "control": s.ops.control, "lifecycle": s.ops.lifecycle} {
		if r.CrashPoints == nil {
			t.Fatalf("%s recorder carries no hook", name)
		}
		before := heard.Load()
		_ = r.CrashPoints(legacyop.CrashPointAdmitted, opjournal.Op{})
		if heard.Load() != before+1 {
			t.Fatalf("%s recorder's hook is not the State's", name)
		}
	}
}

// K2 is in the delivery goroutine, after the 202 and before the delivery:
// held there, the POST still answers 202 and the runtime sees nothing; once
// released, the message is delivered once.
func TestAnsweredCrashPointHoldsTheDeliveryAfterThe202(t *testing.T) {
	for _, path := range []string{"/submit", "/messages"} {
		t.Run(path, func(t *testing.T) {
			e := newOpsEnv(t, "auto")
			info := e.session(t, "K2")
			park := newParkAt(t, legacyop.CrashPointAnswered, true)
			e.withCrashPoints(park.hook)
			rec := e.post(t, "/session/"+info.ID+path, "k2", `{"message":"held"}`)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("POST = %d %s, want 202 while the delivery is held", rec.Code, rec.Body.String())
			}
			acc := decodeAsyncAccepted(t, rec.Body)
			op := park.wait(t)
			if op.ID != acc.RequestID {
				t.Fatalf("Answered for op %q, want the request's %q", op.ID, acc.RequestID)
			}
			if got := deliveredCalls(e.fs.sp, info.SessionName); len(got) != 0 {
				t.Fatalf("deliveries before the release: %+v", got)
			}
			park.end()
			waitTerminalEvents(t, e.fs.eventProv, acc.RequestID, 1)
			e.waitClosed(t, acc.RequestID)
			if got := deliveredCalls(e.fs.sp, info.SessionName); len(got) != 1 || got[0].Message != "held" {
				t.Fatalf("deliveries = %+v, want exactly one", got)
			}
			if n := park.seen.Load(); n != 1 {
				t.Fatalf("Answered reported %d times, want once", n)
			}
		})
	}
}

// DES-T3I §14 item 8, through a real first execution: an interrupt_now whose
// keyed send landed (its sighting and its fence read found no turn, so it
// delivered at once, starting the turn every later read finds) and whose API died at K3 is adopted by a same-key retry after a
// restart, which finds the delivery and the turn it started, and sends
// nothing: one lookup, no sighting, interrupt, wait or send. The op closes
// succeeded{legacy.delivered}, and the terminal event is an unqueued
// interrupt_now with no interrupt report.
func TestX12K3InterruptNowAdoptionSendsNothing(t *testing.T) {
	for _, mode := range []string{"auto", "require"} {
		t.Run(mode, func(t *testing.T) {
			e, info := newT3OpsEnv(t, mode)
			e.fs.sp.TurnScripts = map[string][]string{info.SessionName: {"", "", "thread/t1"}}
			park := newParkAt(t, legacyop.CrashPointEffected, false)
			e.withCrashPoints(park.hook)
			body := `{"message":"now","intent":"interrupt_now"}`
			first := e.post(t, "/session/"+info.ID+"/submit", "k3", body)
			if first.Code != http.StatusAccepted {
				t.Fatalf("first POST = %d %s", first.Code, first.Body.String())
			}
			acc := decodeAsyncAccepted(t, first.Body)
			op := park.wait(t)
			if op.ID != acc.RequestID {
				t.Fatalf("parked op %q, want %q", op.ID, acc.RequestID)
			}
			sent := deliveredCalls(e.fs.sp, info.SessionName)
			if len(sent) != 1 || sent[0].DeliveryKey != opDeliveryKey(op.ID) {
				t.Fatalf("first execution's sends = %+v, want one under %s", sent, opDeliveryKey(op.ID))
			}
			if stored, err := e.opStore(t).Get(op.ID); err != nil || stored.Status == opjournal.StatusClosed {
				t.Fatalf("op at K3 = %+v, %v; want it open", stored, err)
			}

			e.restart(t)
			e.withCrashPoints(park.hook)
			since := len(e.fs.sp.SnapshotCalls())
			retry := e.post(t, "/session/"+info.ID+"/submit", "k3", body)
			racc := decodeAsyncAccepted(t, retry.Body)
			if retry.Code != http.StatusAccepted || racc.RequestID != op.ID || racc.Op == nil || racc.Op.Attempt != 1 || racc.Replayed {
				t.Fatalf("retry = %d %+v; want 202 adopting %s at attempt 1", retry.Code, racc, op.ID)
			}
			success, failure := waitForSessionSubmitResult(t, e.fs.eventProv, op.ID)
			if success == nil || success.Queued || success.Intent != "interrupt_now" || success.Interrupt != nil {
				t.Fatalf("terminal = %+v, %+v; want unqueued interrupt_now with no interrupt report", success, failure)
			}
			if closed := e.waitClosed(t, op.ID); closed.Outcome.Arm != opjournal.ArmSucceeded || closed.Outcome.Result.Via != legacyop.ViaLegacyDelivered || closed.Attempt != 1 {
				t.Fatalf("op = %+v, want succeeded via legacy.delivered at attempt 1", closed)
			}
			if looks := callsSince(e.fs.sp, since, info.SessionName, "LookupDelivery"); len(looks) != 1 || looks[0].DeliveryKey != opDeliveryKey(op.ID) {
				t.Fatalf("lookups since the restart = %+v, want one under %s", looks, opDeliveryKey(op.ID))
			}
			if got := callsSince(e.fs.sp, since, info.SessionName, "ObserveTurn", "InterruptTurn", "WaitTurnEnded", "Interrupt", "Nudge", "NudgeNow", "NudgeFenced", "SendKeys"); len(got) != 0 {
				t.Fatalf("calls since the restart = %+v, want none", got)
			}
		})
	}
}

// /respond at K1, K3 and K4, with and without a same-key retry after a
// restart. With the retry, K1 adopts and answers once; K3 adopts, finds the
// interaction its first execution answered gone, closes skipped{stale} and
// answers 409, the interaction still answered once (X12 finding F18); K4
// replays. Without it, the sweeper closes the K1 and K3 ops
// failed{deadline_in_flight} past their finish_by and leaves K4's closed op
// alone.
func TestX12RespondCrashPoints(t *testing.T) {
	type want struct {
		retryCode  int
		replayed   bool
		attempt    int
		arm        opjournal.Arm
		reason     string
		sweptArm   opjournal.Arm
		sweptRsn   string
		firstLands bool
	}
	cases := map[legacyop.CrashPoint]want{
		legacyop.CrashPointAdmitted: {
			retryCode: http.StatusAccepted, attempt: 1, arm: opjournal.ArmSucceeded,
			sweptArm: opjournal.ArmFailed, sweptRsn: opjournal.ReasonDeadlineInFlight,
		},
		legacyop.CrashPointEffected: {
			retryCode: http.StatusConflict, attempt: 1, arm: opjournal.ArmSkipped, reason: opjournal.ReasonStale,
			sweptArm: opjournal.ArmFailed, sweptRsn: opjournal.ReasonDeadlineInFlight, firstLands: true,
		},
		legacyop.CrashPointClosed: {
			retryCode: http.StatusAccepted, replayed: true, attempt: 0, arm: opjournal.ArmSucceeded,
			sweptArm: opjournal.ArmSucceeded, firstLands: true,
		},
	}
	body := `{"request_id":"req-1","action":"approve"}`
	for point, w := range cases {
		for _, retry := range []bool{true, false} {
			name := string(point) + "/none"
			if retry {
				name = string(point) + "/key"
			}
			t.Run(name, func(t *testing.T) {
				e := newControlOpsEnv(t, "auto", "off")
				info := e.session(t, "Respond X12")
				e.fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{RequestID: "req-1", Kind: "approval", Prompt: "ok?"})
				park := newParkAt(t, point, false)
				e.withCrashPoints(park.hook)
				answered := e.postAsync(t, "/session/"+info.ID+"/respond", "r-1", body)
				op := park.wait(t)
				id, _ := opjournal.DeriveID("gc", info.ID, legacyop.PrincipalOperator, "r-1")
				if op.ID != id {
					t.Fatalf("parked op %q, want %q", op.ID, id)
				}
				lands := 0
				if w.firstLands {
					lands = 1
				}
				if got := len(e.fs.sp.Responses[info.SessionName]); got != lands {
					t.Fatalf("responses at the crash = %d, want %d", got, lands)
				}
				select {
				case rec := <-answered:
					t.Fatalf("the crashed request answered %d %s", rec.Code, rec.Body.String())
				default:
				}

				e.restart(t)
				e.withCrashPoints(park.hook)
				if !retry {
					st := e.opStore(t)
					if _, err := legacyop.Sweep(st, time.Now().Add(legacyop.ExecutionBound+time.Minute), 100, "test:sweep"); err != nil {
						t.Fatalf("sweep: %v", err)
					}
					got, err := st.Get(id)
					if err != nil || got.Status != opjournal.StatusClosed || got.Outcome.Arm != w.sweptArm || got.Outcome.Reason != w.sweptRsn || got.Attempt != 0 {
						t.Fatalf("op after the sweep = %+v, %v; want %s{%s} at attempt 0", got.Outcome, err, w.sweptArm, w.sweptRsn)
					}
					if n := len(e.fs.sp.Responses[info.SessionName]); n != lands {
						t.Fatalf("responses = %d, want %d", n, lands)
					}
					return
				}
				rec := e.post(t, "/session/"+info.ID+"/respond", "r-1", body)
				if rec.Code != w.retryCode {
					t.Fatalf("retry = %d %s, want %d", rec.Code, rec.Body.String(), w.retryCode)
				}
				if w.retryCode == http.StatusAccepted {
					if a := decodeCommandAnswer(t, rec); a.Replayed != w.replayed {
						t.Fatalf("retry = %+v, want replayed %v", a, w.replayed)
					}
				}
				got, err := e.opStore(t).Get(id)
				if err != nil || got.Status != opjournal.StatusClosed || got.Attempt != w.attempt || got.Outcome.Arm != w.arm || got.Outcome.Reason != w.reason {
					t.Fatalf("op after the retry = %+v (attempt %d), %v; want %s{%s} at attempt %d", got.Outcome, got.Attempt, err, w.arm, w.reason, w.attempt)
				}
				if n := len(e.fs.sp.Responses[info.SessionName]); n != 1 {
					t.Fatalf("responses = %d (Respond calls %d), want the interaction answered once", n, countSessionResponds(e.fs.sp, info.SessionName))
				}
			})
		}
	}
}
