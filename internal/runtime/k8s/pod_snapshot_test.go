package k8s

import (
	"context"
	"errors"
	"io"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	execerr "k8s.io/client-go/util/exec"
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
// because the fake lists in map order.
type countingOps struct {
	mu       sync.Mutex
	inner    *fakeK8sOps
	lists    []listCall
	execs    int
	execPods []string
}

func newCountingOps() *countingOps {
	return &countingOps{inner: newFakeK8sOps()}
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
	defer c.mu.Unlock()
	c.lists = append(c.lists, listCall{selector: selector, fieldSelector: fieldSelector})
	pods, err := c.inner.listPods(ctx, selector, fieldSelector)
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	return pods, err
}

func (c *countingOps) execInPod(ctx context.Context, pod, container string, cmd []string, stdin io.Reader) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.execs++
	c.execPods = append(c.execPods, pod)
	return c.inner.execInPod(ctx, pod, container, cmd, stdin)
}

func (c *countingOps) listCalls() []listCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]listCall(nil), c.lists...)
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
