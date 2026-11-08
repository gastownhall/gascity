package k8s

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gastownhall/gascity/internal/runtime"
)

// The tests below pin the per-call deadlines of the ctx-free provider verbs
// (F4 of #7170). They run in a synctest bubble: the apiserver is a stub whose
// calls are held for fake time, so "returns within the bound" is an exact
// comparison on the fake clock and no test sleeps.

const (
	testRequestBound = 5 * time.Second
	testCopyBound    = 50 * time.Second
	// testHold is how long a "black hole" call is held when nothing cuts it.
	testHold = time.Hour
)

const deadlineSession = "deadline-session"

// holdOps wraps the in-memory fake and holds chosen calls in fake time. A held
// call ends when its ctx ends, or when hold elapses, whichever is first.
type holdOps struct {
	*fakeK8sOps
	// hold returns how long the n-th (0-based) call of method is held before it
	// reaches the fake; zero passes straight through.
	hold func(method string, n int, cmd []string) time.Duration

	mu    sync.Mutex
	calls map[string]int
	cut   int // calls ended by their ctx
}

func newHoldOps(hold func(method string, n int, cmd []string) time.Duration) *holdOps {
	return &holdOps{fakeK8sOps: newFakeK8sOps(), hold: hold, calls: map[string]int{}}
}

// holdAll holds every call for d.
func holdAll(d time.Duration) func(string, int, []string) time.Duration {
	return func(string, int, []string) time.Duration { return d }
}

// holdMethods holds every call of the named methods for testHold.
func holdMethods(methods ...string) func(string, int, []string) time.Duration {
	return func(m string, _ int, _ []string) time.Duration {
		for _, want := range methods {
			if m == want {
				return testHold
			}
		}
		return 0
	}
}

func (h *holdOps) gate(ctx context.Context, method string, cmd []string) error {
	h.mu.Lock()
	n := h.calls[method]
	h.calls[method]++
	h.mu.Unlock()
	d := h.hold(method, n, cmd)
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		h.mu.Lock()
		h.cut++
		h.mu.Unlock()
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (h *holdOps) cutCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cut
}

func (h *holdOps) createPod(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	if err := h.gate(ctx, "createPod", nil); err != nil {
		return nil, err
	}
	return h.fakeK8sOps.createPod(ctx, pod)
}

func (h *holdOps) getPod(ctx context.Context, name string) (*corev1.Pod, error) {
	if err := h.gate(ctx, "getPod", nil); err != nil {
		return nil, err
	}
	return h.fakeK8sOps.getPod(ctx, name)
}

func (h *holdOps) deletePod(ctx context.Context, name string, grace int64) error {
	if err := h.gate(ctx, "deletePod", nil); err != nil {
		return err
	}
	return h.fakeK8sOps.deletePod(ctx, name, grace)
}

func (h *holdOps) listPods(ctx context.Context, selector, fieldSelector string) ([]corev1.Pod, error) {
	if err := h.gate(ctx, "listPods", nil); err != nil {
		return nil, err
	}
	return h.fakeK8sOps.listPods(ctx, selector, fieldSelector)
}

func (h *holdOps) execInPod(ctx context.Context, pod, container string, cmd []string, stdin io.Reader) (string, error) {
	if err := h.gate(ctx, "execInPod", cmd); err != nil {
		return "", err
	}
	return h.fakeK8sOps.execInPod(ctx, pod, container, cmd, stdin)
}

// addRunningPods seeds n Running pods for deadlineSession.
func addRunningPods(fake *fakeK8sOps, n int) {
	for i := 0; i < n; i++ {
		podName := SanitizeName(deadlineSession)
		if i > 0 {
			podName += "-" + string(rune('a'+i))
		}
		fake.pods[podName] = &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:   podName,
				Labels: map[string]string{"app": "gc-agent", "gc-session": SanitizeLabel(deadlineSession)},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
	}
}

func deadlineProvider(ops k8sOps) *Provider {
	p := newProviderWithOps(ops)
	p.requestBound = testRequestBound
	p.copyBound = testCopyBound
	return p
}

// TestRequestVerbsReturnWithinRequestBound is acceptance (a): with the
// apiserver a black hole, each of the 14 request-style verbs returns within the
// request bound with its existing error shape.
func TestRequestVerbsReturnWithinRequestBound(t *testing.T) {
	type outcome struct {
		err error
		out string
		ok  bool
		ts  time.Time
	}
	cases := []struct {
		name  string
		pods  int
		hold  func(string, int, []string) time.Duration
		call  func(p *Provider) outcome
		check func(t *testing.T, o outcome)
	}{
		{
			name: "Stop/list", hold: holdAll(testHold),
			call: func(p *Provider) outcome { return outcome{err: p.Stop(deadlineSession)} },
			check: func(t *testing.T, o outcome) {
				if o.err == nil || !strings.Contains(o.err.Error(), "k8s stop") || !strings.Contains(o.err.Error(), "listing pods") {
					t.Fatalf("Stop = %v, want wrapped listing error", o.err)
				}
				if runtime.IsSessionGone(o.err) {
					t.Fatalf("Stop deadline error %v must not match IsSessionGone", o.err)
				}
			},
		},
		{
			name: "Stop/delete", pods: 1, hold: holdMethods("deletePod"),
			call: func(p *Provider) outcome { return outcome{err: p.Stop(deadlineSession)} },
			check: func(t *testing.T, o outcome) {
				if o.err == nil || !strings.Contains(o.err.Error(), "deleting pod") {
					t.Fatalf("Stop = %v, want joined delete error", o.err)
				}
				if runtime.IsSessionGone(o.err) {
					t.Fatalf("Stop deadline error %v must not match IsSessionGone", o.err)
				}
			},
		},
		{
			name: "Interrupt", hold: holdAll(testHold),
			call:  func(p *Provider) outcome { return outcome{err: p.Interrupt(deadlineSession)} },
			check: func(t *testing.T, o outcome) { wantNilErr(t, "Interrupt", o.err) },
		},
		{
			name: "IsRunning/list", hold: holdAll(testHold),
			call:  func(p *Provider) outcome { return outcome{ok: p.IsRunning(deadlineSession)} },
			check: func(t *testing.T, o outcome) { wantFalse(t, "IsRunning", o.ok) },
		},
		{
			name: "IsRunning/exec", pods: 1, hold: holdMethods("execInPod"),
			call:  func(p *Provider) outcome { return outcome{ok: p.IsRunning(deadlineSession)} },
			check: func(t *testing.T, o outcome) { wantFalse(t, "IsRunning", o.ok) },
		},
		{
			name: "IsAttached/list", hold: holdAll(testHold),
			call:  func(p *Provider) outcome { return outcome{ok: p.IsAttached(deadlineSession)} },
			check: func(t *testing.T, o outcome) { wantFalse(t, "IsAttached", o.ok) },
		},
		{
			name: "IsAttached/exec", pods: 1, hold: holdMethods("execInPod"),
			call:  func(p *Provider) outcome { return outcome{ok: p.IsAttached(deadlineSession)} },
			check: func(t *testing.T, o outcome) { wantFalse(t, "IsAttached", o.ok) },
		},
		{
			name: "ProcessAlive/list", hold: holdAll(testHold),
			call:  func(p *Provider) outcome { return outcome{ok: p.ProcessAlive(deadlineSession, []string{"claude"})} },
			check: func(t *testing.T, o outcome) { wantFalse(t, "ProcessAlive", o.ok) },
		},
		{
			name: "ProcessAlive/exec", pods: 1, hold: holdMethods("execInPod"),
			call:  func(p *Provider) outcome { return outcome{ok: p.ProcessAlive(deadlineSession, []string{"claude"})} },
			check: func(t *testing.T, o outcome) { wantFalse(t, "ProcessAlive", o.ok) },
		},
		{
			name: "Nudge", pods: 1, hold: holdMethods("execInPod"),
			call: func(p *Provider) outcome {
				return outcome{err: p.Nudge(deadlineSession, []runtime.ContentBlock{{Type: "text", Text: "hi"}})}
			},
			check: func(t *testing.T, o outcome) {
				if o.err == nil {
					t.Fatal("Nudge = nil, want the deadline error")
				}
			},
		},
		{
			name: "SendKeys", pods: 1, hold: holdMethods("execInPod"),
			call: func(p *Provider) outcome { return outcome{err: p.SendKeys(deadlineSession, "Enter")} },
			check: func(t *testing.T, o outcome) {
				if o.err == nil {
					t.Fatal("SendKeys = nil, want the deadline error")
				}
			},
		},
		{
			name: "SetMeta", hold: holdAll(testHold),
			call:  func(p *Provider) outcome { return outcome{err: p.SetMeta(deadlineSession, "k", "v")} },
			check: func(t *testing.T, o outcome) { wantNilErr(t, "SetMeta", o.err) },
		},
		{
			name: "GetMeta", hold: holdAll(testHold),
			call: func(p *Provider) outcome {
				v, err := p.GetMeta(deadlineSession, "k")
				return outcome{out: v, err: err}
			},
			check: func(t *testing.T, o outcome) {
				wantNilErr(t, "GetMeta", o.err)
				if o.out != "" {
					t.Fatalf("GetMeta = %q, want empty", o.out)
				}
			},
		},
		{
			name: "RemoveMeta", hold: holdAll(testHold),
			call:  func(p *Provider) outcome { return outcome{err: p.RemoveMeta(deadlineSession, "k")} },
			check: func(t *testing.T, o outcome) { wantNilErr(t, "RemoveMeta", o.err) },
		},
		{
			name: "Peek", pods: 1, hold: holdMethods("execInPod"),
			call: func(p *Provider) outcome {
				v, err := p.Peek(deadlineSession, 10)
				return outcome{out: v, err: err}
			},
			check: func(t *testing.T, o outcome) {
				wantNilErr(t, "Peek", o.err)
				if o.out != "" {
					t.Fatalf("Peek = %q, want empty", o.out)
				}
			},
		},
		{
			name: "ListRunning", hold: holdAll(testHold),
			call: func(p *Provider) outcome {
				names, err := p.ListRunning("")
				return outcome{out: strings.Join(names, ","), err: err}
			},
			check: func(t *testing.T, o outcome) {
				if o.err == nil {
					t.Fatal("ListRunning err = nil, want the deadline error")
				}
				if o.out != "" {
					t.Fatalf("ListRunning names = %q, want none", o.out)
				}
			},
		},
		{
			name: "GetLastActivity", pods: 1, hold: holdMethods("execInPod"),
			call: func(p *Provider) outcome {
				ts, err := p.GetLastActivity(deadlineSession)
				return outcome{ts: ts, err: err}
			},
			check: func(t *testing.T, o outcome) {
				wantNilErr(t, "GetLastActivity", o.err)
				if !o.ts.IsZero() {
					t.Fatalf("GetLastActivity = %v, want zero time", o.ts)
				}
			},
		},
		{
			name: "ClearScrollback", pods: 1, hold: holdMethods("execInPod"),
			call:  func(p *Provider) outcome { return outcome{err: p.ClearScrollback(deadlineSession)} },
			check: func(t *testing.T, o outcome) { wantNilErr(t, "ClearScrollback", o.err) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ops := newHoldOps(tc.hold)
				addRunningPods(ops.fakeK8sOps, tc.pods)
				p := deadlineProvider(ops)

				start := time.Now()
				o := tc.call(p)
				elapsed := time.Since(start)

				if elapsed > testRequestBound {
					t.Fatalf("verb took %v, want within the %v request bound", elapsed, testRequestBound)
				}
				if ops.cutCount() == 0 {
					t.Fatal("no apiserver call was cut by its ctx: the stub was never held, the test is vacuous")
				}
				tc.check(t, o)
			})
		})
	}
}

func wantNilErr(t *testing.T, verb string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s err = %v, want nil", verb, err)
	}
}

func wantFalse(t *testing.T, verb string, got bool) {
	t.Helper()
	if got {
		t.Fatalf("%s = true, want false", verb)
	}
}

// TestStopHasOneDeadlineAcrossItsDeleteLoop is acceptance (b) for Stop: the
// first delete eats most of the bound and the second blocks, and the verb still
// returns after one bound, not one bound per call.
func TestStopHasOneDeadlineAcrossItsDeleteLoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first := testRequestBound * 8 / 10
		ops := newHoldOps(func(method string, n int, _ []string) time.Duration {
			if method != "deletePod" {
				return 0
			}
			if n == 0 {
				return first
			}
			return testHold
		})
		addRunningPods(ops.fakeK8sOps, 2)
		p := deadlineProvider(ops)

		start := time.Now()
		err := p.Stop(deadlineSession)
		elapsed := time.Since(start)

		if err == nil {
			t.Fatal("Stop = nil, want the deadline error from the second delete")
		}
		if elapsed > testRequestBound {
			t.Fatalf("Stop took %v, want one %v bound over both deletes", elapsed, testRequestBound)
		}
		if ops.cutCount() != 1 {
			t.Fatalf("cut calls = %d, want exactly the second delete", ops.cutCount())
		}
	})
}

// TestProcessAliveHasOneDeadlineAcrossItsProbeLoop is acceptance (b) for
// ProcessAlive: one bound over the whole pgrep loop.
func TestProcessAliveHasOneDeadlineAcrossItsProbeLoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first := testRequestBound * 8 / 10
		ops := newHoldOps(func(method string, n int, _ []string) time.Duration {
			if method != "execInPod" {
				return 0
			}
			if n == 0 {
				return first
			}
			return testHold
		})
		ops.execFunc = func(string, []string) (string, error) { return "", errors.New("no such process") }
		addRunningPods(ops.fakeK8sOps, 1)
		p := deadlineProvider(ops)

		start := time.Now()
		alive := p.ProcessAlive(deadlineSession, []string{"a", "b", "c"})
		elapsed := time.Since(start)

		if alive {
			t.Fatal("ProcessAlive = true, want false")
		}
		if elapsed > testRequestBound {
			t.Fatalf("ProcessAlive took %v, want one %v bound over the whole loop", elapsed, testRequestBound)
		}
		// The second probe is cut at the bound; the third finds the ctx already
		// ended and fails at once, so at least one call, not exactly one.
		if ops.cutCount() == 0 {
			t.Fatal("no probe was cut by the ctx: the test is vacuous")
		}
	})
}

// TestAttachBoundsOnlyItsLookup is acceptance (c): a blocking lookup returns the
// "no running pod" error within the bound, and the ctx handed to kubectl has no
// deadline and outlives the request bound.
func TestAttachBoundsOnlyItsLookup(t *testing.T) {
	t.Run("lookup blocks", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ops := newHoldOps(holdAll(testHold))
			p := deadlineProvider(ops)
			p.run = func(context.Context, []string) error {
				t.Error("kubectl must not run when the lookup timed out")
				return nil
			}

			start := time.Now()
			err := p.Attach(deadlineSession)
			elapsed := time.Since(start)

			if err == nil || !strings.Contains(err.Error(), "attach: no running pod") {
				t.Fatalf("Attach = %v, want the attach: no running pod error", err)
			}
			if elapsed > testRequestBound {
				t.Fatalf("Attach lookup took %v, want within %v", elapsed, testRequestBound)
			}
			if ops.cutCount() == 0 {
				t.Fatal("the lookup was never cut: the test is vacuous")
			}
		})
	})
	t.Run("kubectl ctx is uncut", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ops := newHoldOps(holdAll(0))
			addRunningPods(ops.fakeK8sOps, 1)
			p := deadlineProvider(ops)

			var ran bool
			p.run = func(ctx context.Context, args []string) error {
				ran = true
				if _, ok := ctx.Deadline(); ok {
					t.Error("the ctx given to kubectl has a deadline: the interactive stream would be cut")
				}
				if !strings.Contains(strings.Join(args, " "), SanitizeName(deadlineSession)) {
					t.Errorf("kubectl args %v do not name the pod", args)
				}
				// Outlive the request bound; the stream is a user's terminal.
				<-time.After(3 * testRequestBound)
				if err := ctx.Err(); err != nil {
					t.Errorf("kubectl ctx ended after the request bound: %v", err)
				}
				return nil
			}

			if err := p.Attach(deadlineSession); err != nil {
				t.Fatalf("Attach = %v, want nil", err)
			}
			if !ran {
				t.Fatal("kubectl hook never ran")
			}
		})
	})
}

// TestCopyToRunsUnderTheCopyBound is acceptance (d).
func TestCopyToRunsUnderTheCopyBound(t *testing.T) {
	writeSrc := func(t *testing.T) string {
		t.Helper()
		src := filepath.Join(t.TempDir(), "payload.txt")
		if err := os.WriteFile(src, []byte("payload"), 0o600); err != nil {
			t.Fatal(err)
		}
		return src
	}
	tarHold := func(d time.Duration) func(string, int, []string) time.Duration {
		return func(method string, _ int, cmd []string) time.Duration {
			if method == "execInPod" && len(cmd) > 0 && cmd[0] == "tar" {
				return d
			}
			return 0
		}
	}

	t.Run("stream longer than the request bound completes", func(t *testing.T) {
		src := writeSrc(t)
		synctest.Test(t, func(t *testing.T) {
			ops := newHoldOps(tarHold(4 * testRequestBound))
			addRunningPods(ops.fakeK8sOps, 1)
			p := deadlineProvider(ops)

			if err := p.CopyTo(deadlineSession, src, "x/payload.txt"); err != nil {
				t.Fatalf("CopyTo = %v, want nil: a stream under the copy bound must finish", err)
			}
			if ops.cutCount() != 0 {
				t.Fatalf("cut calls = %d, want none", ops.cutCount())
			}
		})
	})
	t.Run("stream past the copy bound is cut", func(t *testing.T) {
		src := writeSrc(t)
		synctest.Test(t, func(t *testing.T) {
			ops := newHoldOps(tarHold(testHold))
			addRunningPods(ops.fakeK8sOps, 1)
			p := deadlineProvider(ops)

			start := time.Now()
			err := p.CopyTo(deadlineSession, src, "x/payload.txt")
			elapsed := time.Since(start)

			if err == nil {
				t.Fatal("CopyTo = nil, want the stream's deadline error")
			}
			if elapsed > testCopyBound {
				t.Fatalf("CopyTo took %v, want within the %v copy bound", elapsed, testCopyBound)
			}
			if ops.cutCount() == 0 {
				t.Fatal("the stream was never cut: the test is vacuous")
			}
		})
	})
	t.Run("lookup past the copy bound still returns nil", func(t *testing.T) {
		src := writeSrc(t)
		synctest.Test(t, func(t *testing.T) {
			ops := newHoldOps(holdAll(testHold))
			p := deadlineProvider(ops)

			start := time.Now()
			err := p.CopyTo(deadlineSession, src, "x/payload.txt")
			elapsed := time.Since(start)

			if err != nil {
				t.Fatalf("CopyTo = %v, want nil: a lookup failure is best-effort", err)
			}
			if elapsed > testCopyBound {
				t.Fatalf("CopyTo took %v, want within the %v copy bound", elapsed, testCopyBound)
			}
			if ops.cutCount() == 0 {
				t.Fatal("the lookup was never cut: the test is vacuous")
			}
		})
	})
}

// TestBoundsDefaultToTheNamedConstants is acceptance (e): a zero field resolves
// to its constant, with no sleep.
func TestBoundsDefaultToTheNamedConstants(t *testing.T) {
	if verbRequestBound != 15*time.Second {
		t.Fatalf("verbRequestBound = %v, want 15s", verbRequestBound)
	}
	if verbCopyBound != 120*time.Second {
		t.Fatalf("verbCopyBound = %v, want 120s", verbCopyBound)
	}
	var zero Provider
	if got := zero.requestTimeout(); got != verbRequestBound {
		t.Fatalf("zero requestBound resolves to %v, want %v", got, verbRequestBound)
	}
	if got := zero.copyTimeout(); got != verbCopyBound {
		t.Fatalf("zero copyBound resolves to %v, want %v", got, verbCopyBound)
	}
	set := Provider{requestBound: time.Second, copyBound: 2 * time.Second}
	if got := set.requestTimeout(); got != time.Second {
		t.Fatalf("injected requestBound resolves to %v, want 1s", got)
	}
	if got := set.copyTimeout(); got != 2*time.Second {
		t.Fatalf("injected copyBound resolves to %v, want 2s", got)
	}
}

// TestVerbCtxCarriesTheBound pins the helper: a deadline of exactly the bound
// from now, canceled by the returned cancel.
func TestVerbCtxCarriesTheBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var p Provider
		now := time.Now()
		ctx, cancel := p.verbCtx(testRequestBound)
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("verbCtx has no deadline")
		}
		if got := deadline.Sub(now); got != testRequestBound {
			t.Fatalf("deadline is %v from now, want %v", got, testRequestBound)
		}
		cancel()
		if ctx.Err() == nil {
			t.Fatal("cancel did not end the ctx")
		}
	})
}
