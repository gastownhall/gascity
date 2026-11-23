package legacyop

import (
	"errors"
	"fmt"
	goruntime "runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// heardPoint is one crash point a hook heard, with the op the recorder passed
// and the op's stored row at that moment.
type heardPoint struct {
	point  CrashPoint
	op     opjournal.Op
	stored opjournal.Op
}

// crashLog is a CrashPointFunc that records every point it hears, reading the
// op's row from the store at that moment.
type crashLog struct {
	e   *env
	mu  sync.Mutex
	got []heardPoint
	// fail, when set, is the error the hook returns at CrashPointClosing.
	fail func() error
}

func (l *crashLog) hook(p CrashPoint, op opjournal.Op) error {
	stored, _ := l.e.st.Get(op.ID)
	l.mu.Lock()
	l.got = append(l.got, heardPoint{point: p, op: op, stored: stored})
	fail := l.fail
	l.mu.Unlock()
	if p == CrashPointClosing && fail != nil {
		return fail()
	}
	return nil
}

func (l *crashLog) points() []CrashPoint {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]CrashPoint, 0, len(l.got))
	for _, h := range l.got {
		out = append(out, h.point)
	}
	return out
}

func (l *crashLog) heard() []heardPoint {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]heardPoint(nil), l.got...)
}

// A recorded execution reports Admitted with its row durable and open,
// Effected before anything about the op is written, Closing before the close
// and Closed once it landed, in that order, on both surfaces.
func TestCrashPointsReportTheExecutionInOrder(t *testing.T) {
	for _, surface := range []string{SurfaceAPI, SurfaceCLI} {
		for _, mode := range []gate.Mode{gate.Auto, gate.Require} {
			t.Run(fmt.Sprintf("%s/%s", surface, mode), func(t *testing.T) {
				e := newEnv(t)
				r := cliRecorder(e, mode)
				if surface == SurfaceAPI {
					r = apiRecorder(e, mode)
				}
				log := &crashLog{e: e}
				r.CrashPoints = log.hook
				out := succeeded(ViaLegacyDelivered)
				adm, err := r.Admit(msg("k1", "hello"))
				if err != nil || adm.Decision != Execute || !adm.Recorded {
					t.Fatalf("Admit = %+v, %v", adm, err)
				}
				if f := adm.Finish(Result{Outcome: out}); !f.Closed {
					t.Fatalf("Finish = %+v", f)
				}
				want := []CrashPoint{CrashPointAdmitted, CrashPointEffected, CrashPointClosing, CrashPointClosed}
				if got := log.points(); !slices.Equal(got, want) {
					t.Fatalf("points %v, want %v", got, want)
				}
				h := log.heard()
				for i, p := range h {
					if p.op.ID != adm.Op.ID {
						t.Fatalf("%s: op %q, want %q", p.point, p.op.ID, adm.Op.ID)
					}
					if i < 3 && p.stored.Status != opjournal.StatusSubmitted {
						t.Fatalf("%s: stored status %s, want the op open", p.point, p.stored.Status)
					}
				}
				if h[3].stored.Status != opjournal.StatusClosed || h[3].stored.Outcome != out || h[3].op.Status != opjournal.StatusClosed {
					t.Fatalf("Closed: stored %+v, passed %+v; want the closed op", h[3].stored, h[3].op)
				}
			})
		}
	}
}

// A recorder reports nothing for a request it does not execute under an op
// row: off, an unrecorded auto request, a replay, a busy answer, a key reuse
// and an expired op's close.
func TestCrashPointsSkipEveryNonExecution(t *testing.T) {
	cases := []struct {
		name  string
		mode  gate.Mode
		setup func(t *testing.T, e *env, r *Recorder) Request
	}{
		{"off", gate.Off, func(*testing.T, *env, *Recorder) Request { return msg("k1", "hello") }},
		{"unrecorded auto", gate.Auto, func(_ *testing.T, _ *env, r *Recorder) Request {
			r.Open = func() (*opjournal.Store, error) { return nil, beads.ErrConditionalWriteUnsupported }
			return msg("k1", "hello")
		}},
		{"unrecordable payload", gate.Auto, func(*testing.T, *env, *Recorder) Request { return msg("k1", "") }},
		{"replay", gate.Auto, func(t *testing.T, e *env, _ *Recorder) Request {
			op := e.seed(t, msg("k1", "hello"), ExecutorAPI)
			if _, err := e.st.CloseKnown(op, succeeded(ViaLegacyDelivered), string(ExecutorAPI)); err != nil {
				t.Fatal(err)
			}
			return msg("k1", "hello")
		}},
		{"busy", gate.Auto, func(t *testing.T, e *env, _ *Recorder) Request {
			e.seed(t, msg("k1", "hello"), ExecutorCLI)
			return msg("k1", "hello")
		}},
		{"key reuse", gate.Auto, func(t *testing.T, e *env, _ *Recorder) Request {
			e.seed(t, msg("k1", "hello"), ExecutorCLI)
			return msg("k1", "another body")
		}},
		{"expired", gate.Auto, func(t *testing.T, e *env, _ *Recorder) Request {
			e.seed(t, msg("k1", "hello"), ExecutorAPI)
			e.clk.Advance(ExecutionBound + time.Second)
			return msg("k1", "hello")
		}},
	}
	for _, tc := range cases {
		for _, surface := range []string{SurfaceAPI, SurfaceCLI} {
			t.Run(tc.name+"/"+surface, func(t *testing.T) {
				e := newEnv(t)
				r := cliRecorder(e, tc.mode)
				if surface == SurfaceAPI {
					r = apiRecorder(e, tc.mode)
				}
				log := &crashLog{e: e}
				r.CrashPoints = log.hook
				req := tc.setup(t, e, r)
				adm, err := r.Admit(req)
				if err == nil && adm.Decision == Execute && adm.Recorded {
					t.Fatalf("setup: Admit executes a recorded op: %+v", adm)
				}
				if err == nil {
					adm.Reach(CrashPointAnswered)
					adm.Finish(Result{Outcome: succeeded(ViaLegacyDelivered)})
				}
				if got := log.points(); len(got) != 0 {
					t.Fatalf("points %v, want none", got)
				}
			})
		}
	}
}

// The API's adoption of an orphaned op reports Admitted with the adopted
// attempt.
func TestCrashPointAdmittedOnAdoption(t *testing.T) {
	e := newEnv(t)
	r := apiRecorder(e, gate.Auto)
	log := &crashLog{e: e}
	r.CrashPoints = log.hook
	e.seed(t, msg("k1", "hello"), ExecutorAPI)
	adm, err := r.Admit(msg("k1", "hello"))
	if err != nil || !adm.Adopted {
		t.Fatalf("Admit = %+v, %v; want an adoption", adm, err)
	}
	h := log.heard()
	if len(h) != 1 || h[0].point != CrashPointAdmitted || h[0].op.Attempt != 1 || h[0].stored.Attempt != 1 {
		t.Fatalf("heard %+v; want Admitted at attempt 1", h)
	}
}

// An error from the hook at Closing fails that close attempt as a store error
// would: the API degrades with store_error and retries in the background,
// where each attempt hears Closing again, until the hook lets one through.
// That landing reports no Closed: Finish has returned, so the surface may
// have answered, which is past K4. The CLI gives up and leaves the op open.
func TestCrashPointClosingFailsTheAttempt(t *testing.T) {
	injected := errors.New("injected close failure")
	t.Run("api", func(t *testing.T) {
		e := newEnv(t)
		r := apiRecorder(e, gate.Auto)
		var degraded []Degrade
		r.Degraded = func(d Degrade) { degraded = append(degraded, d) }
		var fails atomic.Int32
		fails.Store(closeRetries + 2)
		log := &crashLog{e: e, fail: func() error {
			if fails.Add(-1) >= 0 {
				return injected
			}
			return nil
		}}
		r.CrashPoints = log.hook
		adm, _ := r.Admit(msg("k1", "hello"))
		out := succeeded(ViaLegacyDelivered)
		f := adm.Finish(Result{Outcome: out})
		if f.Closed || f.Background == nil {
			t.Fatalf("Finish = %+v, want a background close", f)
		}
		<-f.Background
		if len(degraded) != 1 || degraded[0].Stage != StageClose || degraded[0].Reason != DegradeStoreError {
			t.Fatalf("degraded %+v, want one store_error close degrade", degraded)
		}
		want := []CrashPoint{CrashPointAdmitted, CrashPointEffected}
		for range closeRetries + 3 {
			want = append(want, CrashPointClosing)
		}
		if got := log.points(); !slices.Equal(got, want) {
			t.Fatalf("points %v, want %v", got, want)
		}
		if got, _ := e.st.Get(adm.Op.ID); got.Status != opjournal.StatusClosed || got.Outcome != out {
			t.Fatalf("stored %+v", got)
		}
		if r.Inflight.Len() != 0 {
			t.Fatalf("registry %d after the background close", r.Inflight.Len())
		}
	})
	t.Run("cli", func(t *testing.T) {
		e := newEnv(t)
		r := cliRecorder(e, gate.Auto)
		var degraded []Degrade
		r.Degraded = func(d Degrade) { degraded = append(degraded, d) }
		log := &crashLog{e: e, fail: func() error { return injected }}
		r.CrashPoints = log.hook
		adm, _ := r.Admit(msg("k1", "hello"))
		if f := adm.Finish(Result{Outcome: succeeded(ViaLegacyDelivered)}); f.Closed || f.Late || f.Background != nil {
			t.Fatalf("Finish = %+v", f)
		}
		if len(degraded) != 1 || degraded[0].Reason != DegradeStoreError || degraded[0].Surface != SurfaceCLI {
			t.Fatalf("degraded %+v", degraded)
		}
		want := []CrashPoint{CrashPointAdmitted, CrashPointEffected, CrashPointClosing, CrashPointClosing, CrashPointClosing}
		if got := log.points(); !slices.Equal(got, want) {
			t.Fatalf("points %v, want %v", got, want)
		}
		if got, _ := e.st.Get(adm.Op.ID); got.Status != opjournal.StatusSubmitted {
			t.Fatalf("op %s after failed closes, want it open", got.Status)
		}
	})
}

// Effected is reported for a result the API leaves open: the effect was
// attempted. Closing and Closed are not.
func TestCrashPointEffectedOnLeaveOpen(t *testing.T) {
	e := newEnv(t)
	r := apiRecorder(e, gate.Auto)
	log := &crashLog{e: e}
	r.CrashPoints = log.hook
	adm, _ := r.Admit(msg("k1", "hello"))
	adm.Finish(Result{Outcome: failed(opjournal.ReasonOther), LeaveOpen: true})
	want := []CrashPoint{CrashPointAdmitted, CrashPointEffected}
	if got := log.points(); !slices.Equal(got, want) {
		t.Fatalf("points %v, want %v", got, want)
	}
}

// Reach reports the point only for a recorded Execute admission.
func TestReachOnlyForRecordedExecutions(t *testing.T) {
	e := newEnv(t)
	r := apiRecorder(e, gate.Auto)
	log := &crashLog{e: e}
	r.CrashPoints = log.hook

	var none *Admission
	none.Reach(CrashPointAnswered)
	r.Unrecorded(opjournal.KindMessage, "k9", ReasonInvalidKey).Reach(CrashPointAnswered)
	op := e.seed(t, msg("k2", "hello"), ExecutorAPI)
	if _, err := e.st.CloseKnown(op, succeeded(ViaLegacyDelivered), string(ExecutorAPI)); err != nil {
		t.Fatal(err)
	}
	replay, err := r.Admit(msg("k2", "hello"))
	if err != nil || replay.Decision != Replay {
		t.Fatalf("setup: Admit = %+v, %v; want a replay", replay, err)
	}
	replay.Reach(CrashPointAnswered)
	if got := log.points(); len(got) != 0 {
		t.Fatalf("points %v, want none", got)
	}

	adm, _ := r.Admit(msg("k1", "hello"))
	adm.Reach(CrashPointAnswered)
	h := log.heard()
	if len(h) != 2 || h[1].point != CrashPointAnswered || h[1].op.ID != adm.Op.ID {
		t.Fatalf("heard %+v; want Admitted then Answered for %s", h, adm.Op.ID)
	}
	// A recorder without a hook: Reach is a no-op.
	r.CrashPoints = nil
	adm2, _ := r.Admit(msg("k3", "hello"))
	adm2.Reach(CrashPointAnswered)
}

// A nil hook and a hook that does nothing leave the store calls of every
// Admit and Finish path identical.
func TestNilCrashPointsChangesNothing(t *testing.T) {
	type path struct {
		name string
		run  func(t *testing.T, e *env, r *Recorder)
	}
	out := succeeded(ViaLegacyDelivered)
	paths := []path{
		{"execute and close", func(_ *testing.T, _ *env, r *Recorder) {
			adm, _ := r.Admit(msg("k1", "hello"))
			adm.Reach(CrashPointAnswered)
			adm.Finish(Result{Outcome: out})
		}},
		{"adopt and close", func(t *testing.T, e *env, r *Recorder) {
			e.seed(t, msg("k1", "hello"), ExecutorAPI)
			adm, _ := r.Admit(msg("k1", "hello"))
			adm.Finish(Result{Outcome: out})
		}},
		{"replay", func(_ *testing.T, _ *env, r *Recorder) {
			adm, _ := r.Admit(msg("k1", "hello"))
			adm.Finish(Result{Outcome: out})
			_, _ = r.Admit(msg("k1", "hello"))
		}},
		{"retried close", func(_ *testing.T, e *env, r *Recorder) {
			adm, _ := r.Admit(msg("k1", "hello"))
			e.fault.updateFails.Store(2)
			adm.Finish(Result{Outcome: out})
		}},
		{"background close", func(_ *testing.T, e *env, r *Recorder) {
			adm, _ := r.Admit(msg("k1", "hello"))
			e.fault.updateFails.Store(closeRetries + 1)
			if f := adm.Finish(Result{Outcome: out}); f.Background != nil {
				<-f.Background
			}
		}},
		{"expired", func(t *testing.T, e *env, r *Recorder) {
			e.seed(t, msg("k1", "hello"), ExecutorAPI)
			e.clk.Advance(ExecutionBound + time.Second)
			_, _ = r.Admit(msg("k1", "hello"))
		}},
	}
	calls := func(t *testing.T, p path, surface string, hook CrashPointFunc) []string {
		e := newEnv(t)
		r := cliRecorder(e, gate.Auto)
		if surface == SurfaceAPI {
			r = apiRecorder(e, gate.Auto)
		}
		r.CrashPoints = hook
		p.run(t, e, r)
		var kinds []string
		for _, op := range e.rec.Ops() {
			kinds = append(kinds, fmt.Sprintf("%s %s %v", op.Kind, op.ID, op.Err))
		}
		return kinds
	}
	for _, p := range paths {
		for _, surface := range []string{SurfaceAPI, SurfaceCLI} {
			t.Run(p.name+"/"+surface, func(t *testing.T) {
				without := calls(t, p, surface, nil)
				with := calls(t, p, surface, func(CrashPoint, opjournal.Op) error { return nil })
				if !slices.Equal(without, with) {
					t.Fatalf("store calls differ:\nnil hook  %v\nno-op hook %v", without, with)
				}
			})
		}
	}
}

// A hook that panics at any point, or ends its goroutine (runtime.Goexit) at
// a point Finish reports, still leaves the in-flight registry empty: a crash
// test that does not kill its process cannot strand the entry, and a panic
// still propagates.
func TestCrashPointHookThatUnwindsReleasesTheEntry(t *testing.T) {
	for _, point := range []CrashPoint{CrashPointAdmitted, CrashPointEffected, CrashPointClosing, CrashPointClosed} {
		for _, unwind := range []string{"panic", "goexit"} {
			if point == CrashPointAdmitted && unwind == "goexit" {
				continue
			}
			t.Run(string(point)+"/"+unwind, func(t *testing.T) {
				e := newEnv(t)
				r := apiRecorder(e, gate.Auto)
				r.CrashPoints = func(p CrashPoint, _ opjournal.Op) error {
					if p != point {
						return nil
					}
					if unwind == "panic" {
						panic("hook at " + string(p))
					}
					goruntime.Goexit()
					return nil
				}
				panicked := make(chan bool, 1)
				go func() {
					ended := false
					defer func() {
						if !ended {
							panicked <- recover() != nil
						}
					}()
					adm, err := r.Admit(msg("k1", "hello"))
					if err != nil {
						t.Error(err)
						return
					}
					if r.Inflight.Len() != 1 {
						t.Errorf("registry %d after Admit, want 1", r.Inflight.Len())
					}
					adm.Finish(Result{Outcome: succeeded(ViaLegacyDelivered)})
					ended = true
					panicked <- false
				}()
				if got := <-panicked; got != (unwind == "panic") {
					t.Fatalf("Finish panicked = %v, want %v", got, unwind == "panic")
				}
				if r.Inflight.Len() != 0 {
					t.Fatalf("registry %d after the hook unwound at %s, want 0", r.Inflight.Len(), point)
				}
			})
		}
	}
}

// A hook that ends the background close retry's goroutine (runtime.Goexit)
// at one of its close attempts leaves the in-flight registry empty too: the
// retry outlives Finish, so it releases the entry itself on the way out.
func TestCrashPointHookThatUnwindsTheBackgroundCloseReleasesTheEntry(t *testing.T) {
	e := newEnv(t)
	r := apiRecorder(e, gate.Auto)
	var closings atomic.Int32
	r.CrashPoints = func(p CrashPoint, _ opjournal.Op) error {
		if p != CrashPointClosing {
			return nil
		}
		if closings.Add(1) <= closeRetries {
			return errors.New("injected close failure")
		}
		goruntime.Goexit()
		return nil
	}
	adm, err := r.Admit(msg("k1", "hello"))
	if err != nil {
		t.Fatal(err)
	}
	f := adm.Finish(Result{Outcome: succeeded(ViaLegacyDelivered)})
	if f.Closed || f.Background == nil {
		t.Fatalf("Finish = %+v, want a background close", f)
	}
	<-f.Background
	if got := closings.Load(); got != closeRetries+1 {
		t.Fatalf("close attempts %d, want %d: Finish's %d and the retry's first", got, closeRetries+1, closeRetries)
	}
	if r.Inflight.Len() != 0 {
		t.Fatalf("registry %d after the hook ended the background close, want 0", r.Inflight.Len())
	}
	if got, _ := e.st.Get(adm.Op.ID); got.Status != opjournal.StatusSubmitted {
		t.Fatalf("op %s, want it open: no close attempt ran", got.Status)
	}
}
