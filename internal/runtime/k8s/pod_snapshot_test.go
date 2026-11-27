package k8s

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	execerr "k8s.io/client-go/util/exec"

	"github.com/gastownhall/gascity/internal/runtime"
)

// testSnapshotTTL is the pod snapshot TTL the snapshot tests opt into; it
// matches the production TTL NewProvider sets.
const testSnapshotTTL = 2 * time.Second

// listCall is one listPods call as countingOps saw it.
type listCall struct {
	selector      string
	fieldSelector string
}

// countingOps wraps fakeK8sOps for the snapshot tests: it serializes access to
// the fake (which is not goroutine-safe), records every listPods call's
// selectors and every exec's target pod. listPods returns pods sorted by name,
// because the fake lists in map order. A listPods call whose index has a gate
// reads the fake's pods when it starts, then blocks until the gate is closed,
// so a test can hold a LIST in flight while it mutates the fake.
type countingOps struct {
	mu       sync.Mutex
	inner    *fakeK8sOps
	lists    []listCall
	execs    int
	execPods []string
	gates    map[int]chan struct{}
	// started, when non-nil, receives each listPods call's index once the
	// call has read the fake. Set it before any read runs.
	started chan int
}

func newCountingOps() *countingOps {
	return &countingOps{inner: newFakeK8sOps(), gates: make(map[int]chan struct{})}
}

func (c *countingOps) createPod(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inner.createPod(ctx, pod)
}

func (c *countingOps) getPod(ctx context.Context, name string) (*corev1.Pod, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inner.getPod(ctx, name)
}

func (c *countingOps) deletePod(ctx context.Context, name string, grace int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inner.deletePod(ctx, name, grace)
}

func (c *countingOps) listPods(ctx context.Context, selector, fieldSelector string) ([]corev1.Pod, error) {
	c.mu.Lock()
	idx := len(c.lists)
	c.lists = append(c.lists, listCall{selector: selector, fieldSelector: fieldSelector})
	pods, err := c.inner.listPods(ctx, selector, fieldSelector)
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	gate := c.gates[idx]
	c.mu.Unlock()
	if c.started != nil {
		c.started <- idx
	}
	if gate != nil {
		<-gate
	}
	return pods, err
}

func (c *countingOps) execInPod(ctx context.Context, pod, container string, cmd []string, stdin io.Reader) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.execs++
	c.execPods = append(c.execPods, pod)
	return c.inner.execInPod(ctx, pod, container, cmd, stdin)
}

// gate makes the idx-th listPods call block, after reading the fake, until
// the returned channel is closed.
func (c *countingOps) gate(idx int) chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan struct{})
	c.gates[idx] = ch
	return ch
}

func (c *countingOps) listCalls() []listCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]listCall(nil), c.lists...)
}

func (c *countingOps) listCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.lists)
}

func (c *countingOps) execCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.execs
}

// execTargetPods returns the pod each exec ran in, in call order.
func (c *countingOps) execTargetPods() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.execPods...)
}

// reset forgets the calls recorded so far.
func (c *countingOps) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lists = nil
	c.execs = 0
	c.execPods = nil
}

// withFake runs fn on the wrapped fake under the wrapper's lock, so a test can
// mutate pods while other goroutines are reading through the wrapper.
func (c *countingOps) withFake(fn func(*fakeK8sOps)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn(c.inner)
}

// testClock is a settable provider clock, safe for concurrent readers.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// newSnapshotProvider is a provider over ops that keeps a testSnapshotTTL pod
// snapshot and reads the returned clock.
func newSnapshotProvider(ops k8sOps) (*Provider, *testClock) {
	p := newProviderWithOps(ops)
	p.snapshot = newPodSnapshot(testSnapshotTTL)
	clock := &testClock{t: time.Unix(1709300000, 0)}
	p.now = clock.now
	return p, clock
}

// probeSession is one session of a probe fake: whether it has a Running pod,
// whether the in-pod tmux server answers, and what tmux reports.
type probeSession struct {
	name     string
	pod      bool
	tmux     bool
	attached string // #{session_attached}
	activity int64  // #{window_activity}, unix seconds
}

// probeEcho is the activity a send-keys into a probe session leaves behind:
// tmux counts gc's own keystrokes as window activity.
const probeEcho = int64(1709400000)

// newProbeOps builds a counting fake over sessions. Each session with a pod
// gets a Running pod of the same name and label; its tmux answers has-session,
// display-message and list-windows from the session's state, or fails every
// tmux command when the session's tmux is down.
func newProbeOps(sessions []probeSession) *countingOps {
	ops := newCountingOps()
	state := make(map[string]*probeSession, len(sessions))
	for i := range sessions {
		s := sessions[i]
		if !s.pod {
			continue
		}
		addRunningPod(ops.inner, s.name, s.name)
		state[s.name] = &s
	}
	ops.inner.execFunc = func(pod string, cmd []string) (string, error) {
		s := state[pod]
		if s == nil || len(cmd) < 2 || cmd[0] != "tmux" {
			return "", nil
		}
		if !s.tmux {
			return "", execerr.CodeExitError{Err: errors.New("no server running"), Code: 1}
		}
		switch cmd[1] {
		case "display-message":
			return s.attached + "\n", nil
		case "list-windows":
			return strconv.FormatInt(s.activity, 10) + "\n", nil
		case "send-keys":
			s.activity = probeEcho
		}
		return "", nil
	}
	return ops
}

// TestPodSnapshotLookupSemantics pins the snapshot's lookup projection: the
// first pod in list order wins a shared label, ProcessAlive's empty-names
// contract needs no LIST, and a deleting or non-Running pod reads false
// without a pgrep probe.
func TestPodSnapshotLookupSemantics(t *testing.T) {
	t.Run("first pod in list order wins a shared label", func(t *testing.T) {
		ops := newCountingOps()
		// Two pods share gc-session=dupe; countingOps lists by name, so
		// a-pod is first.
		addRunningPod(ops.inner, "a-pod", "dupe")
		addRunningPod(ops.inner, "z-pod", "dupe")
		hasSessionAlive(ops.inner, "a-pod")
		hasSessionAlive(ops.inner, "z-pod")
		p, _ := newSnapshotProvider(ops)

		if !p.IsRunning("dupe") {
			t.Fatal("IsRunning(dupe) = false; want true")
		}
		if targets := ops.execTargetPods(); len(targets) == 0 || targets[0] != "a-pod" {
			t.Fatalf("probe ran in pods %v; want the first pod in list order (a-pod)", targets)
		}
		if lists := ops.listCalls(); len(lists) != 1 || lists[0].selector != "app=gc-agent" {
			t.Fatalf("LIST calls = %+v; want one fleet listing", lists)
		}
	})

	t.Run("ProcessAlive with empty names stays true with no LIST", func(t *testing.T) {
		ops := newCountingOps()
		addRunningPod(ops.inner, "s", "s")
		p, _ := newSnapshotProvider(ops)

		if !p.ProcessAlive("s", nil) {
			t.Fatal("ProcessAlive(s, nil) = false; want true (the contract)")
		}
		if lists := ops.listCalls(); len(lists) != 0 {
			t.Fatalf("LIST calls = %+v; want none for the empty-names contract", lists)
		}
	})

	t.Run("deleting pod is not process-alive, without probing", func(t *testing.T) {
		ops := newCountingOps()
		now := metav1.Now()
		addRunningPod(ops.inner, "s", "s")
		ops.inner.pods["s"].DeletionTimestamp = &now
		p, _ := newSnapshotProvider(ops)

		if p.ProcessAlive("s", []string{"agent"}) {
			t.Fatal("ProcessAlive on a deleting pod = true; want false")
		}
		if got := ops.execCount(); got != 0 {
			t.Fatalf("exec count = %d; want 0 (no pgrep into a deleting pod)", got)
		}
		if lists := ops.listCalls(); len(lists) != 1 || lists[0].selector != "app=gc-agent" {
			t.Fatalf("LIST calls = %+v; want one fleet listing", lists)
		}
	})

	t.Run("Pending pod is not process-alive, without probing", func(t *testing.T) {
		ops := newCountingOps()
		addRunningPod(ops.inner, "s", "s")
		ops.inner.pods["s"].Status.Phase = corev1.PodPending
		p, _ := newSnapshotProvider(ops)

		if p.ProcessAlive("s", []string{"agent"}) {
			t.Fatal("ProcessAlive on a non-Running pod = true; want false")
		}
		if got := ops.execCount(); got != 0 {
			t.Fatalf("exec count = %d; want 0 (no pgrep into a non-Running pod)", got)
		}
	})
}

// TestPodSnapshotRefreshesAfterTTL: observation reads share one pod listing
// for a TTL measured from when its LIST started; the first read at or past it
// lists again, once, and sees a pod created after the first listing.
func TestPodSnapshotRefreshesAfterTTL(t *testing.T) {
	ops := newCountingOps()
	addRunningPod(ops.inner, "a", "a")
	p, clock := newSnapshotProvider(ops)
	populatedAt := clock.now()

	if !p.IsRunning("a") {
		t.Fatal("IsRunning(a) = false, want true")
	}
	ops.withFake(func(f *fakeK8sOps) { addRunningPod(f, "b", "b") })

	clock.set(populatedAt.Add(testSnapshotTTL - time.Nanosecond))
	if p.IsRunning("b") {
		t.Fatal("IsRunning(b) inside the TTL = true, want false: the listing predates b")
	}
	if n := ops.listCount(); n != 1 {
		t.Fatalf("LISTs inside the TTL = %d, want 1", n)
	}

	clock.set(populatedAt.Add(testSnapshotTTL))
	if !p.IsRunning("b") {
		t.Fatal("IsRunning(b) at the TTL = false, want true from a fresh listing")
	}
	if !p.IsRunning("a") || !p.IsRunning("b") {
		t.Fatal("reads after the refresh lost a running session")
	}
	if lists := ops.listCalls(); len(lists) != 2 || lists[1] != (listCall{selector: "app=gc-agent"}) {
		t.Fatalf("LISTs = %+v, want exactly one refresh {selector: app=gc-agent}", lists)
	}
}

// TestPodSnapshotInvalidatedByLifecycle: Start, Stop and Relaunch change this
// process's pods, so once one returns, with or without an error, the next
// observation read lists again instead of answering from the earlier
// listing: a session reads running right after its Start and stopped right
// after its Stop.
func TestPodSnapshotInvalidatedByLifecycle(t *testing.T) {
	ctx := context.Background()
	cfg := runtime.Config{Command: "sleep infinity"}
	for _, tc := range []struct {
		name        string
		setup       func(*fakeK8sOps)
		op          func(*Provider) error
		wantErr     bool
		wantRunning bool
	}{
		{
			name:        "Start success",
			op:          func(p *Provider) error { return p.Start(ctx, "s", cfg) },
			wantRunning: true,
		},
		{
			name:    "Start failure",
			setup:   func(f *fakeK8sOps) { f.createErr = errors.New("quota exceeded") },
			op:      func(p *Provider) error { return p.Start(ctx, "s", cfg) },
			wantErr: true,
		},
		{
			name:  "Stop success",
			setup: func(f *fakeK8sOps) { addRunningPod(f, "s", "s") },
			op:    func(p *Provider) error { return p.Stop("s") },
		},
		{
			name: "Stop failure",
			setup: func(f *fakeK8sOps) {
				addRunningPod(f, "s", "s")
				f.deleteErr = errors.New("connection refused")
			},
			op:          func(p *Provider) error { return p.Stop("s") },
			wantErr:     true,
			wantRunning: true,
		},
		{
			name:        "Relaunch success",
			setup:       func(f *fakeK8sOps) { addRunningPod(f, "s", "s") },
			op:          func(p *Provider) error { return p.Relaunch(ctx, "s", cfg) },
			wantRunning: true,
		},
		{
			name:    "Relaunch failure",
			op:      func(p *Provider) error { return p.Relaunch(ctx, "s", cfg) },
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops := newCountingOps()
			if tc.setup != nil {
				tc.setup(ops.inner)
			}
			p, _ := newSnapshotProvider(ops)
			p.IsRunning("s") // populate the snapshot

			if err := tc.op(p); (err != nil) != tc.wantErr {
				t.Fatalf("op error = %v, want error %v", err, tc.wantErr)
			}
			before := ops.listCount()
			if got := p.IsRunning("s"); got != tc.wantRunning {
				t.Errorf("IsRunning right after the op = %v, want %v", got, tc.wantRunning)
			}
			lists := ops.listCalls()
			if len(lists) != before+1 || lists[before] != (listCall{selector: "app=gc-agent"}) {
				t.Errorf("LISTs after the op = %+v, want one fresh {selector: app=gc-agent}", lists[before:])
			}
		})
	}
}

// TestPodSnapshotDoesNotServePreInvalidationListing: a LIST in flight when
// the snapshot is invalidated may predate the change the invalidation
// announced. A read that starts after the invalidation lists for itself and
// is answered from its own LIST, and when the older LIST lands it answers
// only the read that started it: later reads are served from the newer
// listing, with no further LIST.
func TestPodSnapshotDoesNotServePreInvalidationListing(t *testing.T) {
	ops := newCountingOps()
	addRunningPod(ops.inner, "a", "a")
	ops.started = make(chan int, 8)
	releaseFirst := ops.gate(0)
	p, _ := newSnapshotProvider(ops)

	readA := make(chan bool, 1)
	go func() { readA <- p.IsRunning("a") }()
	if idx := <-ops.started; idx != 0 {
		t.Fatalf("first LIST index = %d, want 0", idx)
	}
	// LIST 1 has read the fake (a running) and is held in flight.

	p.snapshot.invalidate()
	ops.withFake(func(f *fakeK8sOps) {
		delete(f.pods, "a")
		addRunningPod(f, "b", "b")
	})

	// Read B starts after the invalidation.
	if p.IsRunning("a") {
		t.Error("read B: IsRunning(a) = true, want false from its own post-invalidation LIST")
	}
	if n := ops.listCount(); n != 2 {
		t.Fatalf("LISTs after read B = %d, want 2: read B must not wait for the pre-invalidation LIST", n)
	}

	close(releaseFirst)
	if !<-readA {
		t.Error("read A: IsRunning(a) = false, want true from the LIST it started")
	}

	// Read C is served from LIST 2, never from the late LIST 1.
	if p.IsRunning("a") {
		t.Error("read C: IsRunning(a) = true: the pre-invalidation listing was published")
	}
	if !p.IsRunning("b") {
		t.Error("read C: IsRunning(b) = false, want true from LIST 2")
	}
	if n := ops.listCount(); n != 2 {
		t.Fatalf("LISTs after read C = %d, want 2 (served from LIST 2)", n)
	}
}

// TestPodSnapshotDoesNotCacheListErrors: a failed snapshot LIST answers the
// reads that waited on it as a failed per-session lookup does (not running,
// not attached, no activity, no process) and is never cached: every later
// read lists again, and the first successful LIST serves them.
func TestPodSnapshotDoesNotCacheListErrors(t *testing.T) {
	ops := newProbeOps([]probeSession{{name: "a", pod: true, tmux: true, attached: "1", activity: 1709300000}})
	ops.inner.listErr = errors.New("apiserver unavailable")
	p, _ := newSnapshotProvider(ops)

	if p.IsRunning("a") {
		t.Error("IsRunning on a failed LIST = true, want false")
	}
	if p.IsAttached("a") {
		t.Error("IsAttached on a failed LIST = true, want false")
	}
	if activity, err := p.GetLastActivity("a"); err != nil || !activity.IsZero() {
		t.Errorf("GetLastActivity on a failed LIST = (%v, %v), want (zero, nil)", activity, err)
	}
	if p.ProcessAlive("a", []string{"claude"}) {
		t.Error("ProcessAlive on a failed LIST = true, want false")
	}
	if n := ops.listCount(); n != 4 {
		t.Fatalf("LISTs after 4 failed reads = %d, want 4: a failed LIST must not be cached", n)
	}
	if n := ops.execCount(); n != 0 {
		t.Fatalf("execs after failed LISTs = %d, want 0", n)
	}

	ops.withFake(func(f *fakeK8sOps) { f.listErr = nil })
	if !p.IsRunning("a") || !p.IsAttached("a") {
		t.Fatal("reads after the apiserver recovered do not see the running, attached session")
	}
	if n := ops.listCount(); n != 5 {
		t.Fatalf("LISTs after recovery = %d, want 5 (one fresh listing serves both reads)", n)
	}
}

// TestPodSnapshotCoalescesConcurrentRefresh: reads for different sessions
// that arrive while the snapshot LIST is in flight wait for it instead of
// listing for themselves, so 16 concurrent readers cost one LIST in total,
// and each still answers from its own session's pod and tmux state.
func TestPodSnapshotCoalescesConcurrentRefresh(t *testing.T) {
	const readers = 16
	sessions := make([]probeSession, readers)
	for i := range sessions {
		s := probeSession{name: fmt.Sprintf("s%02d", i), activity: 1709300000}
		switch i % 4 {
		case 0: // live, attached
			s.pod, s.tmux, s.attached = true, true, "1"
		case 1: // live, detached
			s.pod, s.tmux, s.attached = true, true, "0"
		case 2: // Running pod, tmux down
			s.pod = true
		case 3: // no pod
		}
		sessions[i] = s
	}
	ops := newProbeOps(sessions)
	ops.started = make(chan int, readers)
	release := ops.gate(0)
	p, _ := newSnapshotProvider(ops)

	type result struct{ running, attached bool }
	results := make([]result, readers)
	begin := make(chan struct{})
	var wg sync.WaitGroup
	for i := range sessions {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-begin
			name := sessions[i].name
			results[i] = result{running: p.IsRunning(name), attached: p.IsAttached(name)}
		}(i)
	}
	close(begin)
	if idx := <-ops.started; idx != 0 {
		t.Fatalf("first LIST index = %d, want 0", idx)
	}
	close(release)
	wg.Wait()

	if lists := ops.listCalls(); len(lists) != 1 || lists[0] != (listCall{selector: "app=gc-agent"}) {
		t.Errorf("LISTs = %+v, want exactly one {selector: app=gc-agent} for %d concurrent readers", lists, readers)
	}
	for i, s := range sessions {
		wantRunning := s.pod && s.tmux
		want := result{running: wantRunning, attached: wantRunning && s.attached == "1"}
		if results[i] != want {
			t.Errorf("%s: (running, attached) = %+v, want %+v", s.name, results[i], want)
		}
	}
}
