package k8s

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
	corev1 "k8s.io/api/core/v1"

	"github.com/gastownhall/gascity/internal/runtime"
)

// podSnapshotTTL is how long the observation reads (IsRunning, IsAttached,
// ProcessAlive, GetLastActivity) share one pod listing, matching the tmux
// provider's state-cache TTL.
const podSnapshotTTL = 2 * time.Second

// podSnapshotFetchTimeout bounds one snapshot LIST. The LIST runs on its own
// context, detached from the reader that started it.
const podSnapshotFetchTimeout = 10 * time.Second

// podSnapshot shares one gc-agent pod listing among the provider's
// observation reads for a short TTL, so a session-list request costs one
// apiserver LIST instead of several per session. A listing is taken lazily by
// the first read that finds none fresh, and reads that arrive while it is in
// flight wait for it rather than listing again (singleflight per generation).
// There is no watch, timer or goroutine. A failed LIST is returned to its readers and never cached. Start, Stop and
// Relaunch invalidate the snapshot, so this process's own pod changes are seen
// by the next read.
type podSnapshot struct {
	ttl time.Duration

	mu sync.Mutex
	// generation advances on every invalidate. A listing is served and
	// published only while the generation its LIST started in is current, so
	// a LIST that was in flight across an invalidation answers only the read
	// that started it.
	generation uint64
	listing    *podListing // the published listing; nil before the first success

	flights singleflight.Group // one LIST in flight per generation
}

// podListing is one snapshot LIST, projected to what the observation reads
// need.
type podListing struct {
	// generation is the snapshot generation the LIST started in.
	generation uint64
	// startedAt is when the LIST started, on the provider clock; the listing
	// is served until startedAt+ttl.
	startedAt time.Time
	// bySession holds the pods per gc-session label, in API list order, so
	// entry [0] is the pod a per-session label LIST would return first.
	bySession map[string][]snapshotPod
}

// snapshotPod is a pod as the observation reads see it.
type snapshotPod struct {
	name     string
	phase    corev1.PodPhase
	deleting bool // a deletionTimestamp is set: the pod is shutting down
}

// newPodSnapshot returns an empty snapshot that serves a listing for ttl.
func newPodSnapshot(ttl time.Duration) *podSnapshot {
	return &podSnapshot{ttl: ttl}
}

// invalidate opens a new generation, so the next read lists again and no LIST
// started before the call is published. It never waits for an in-flight LIST.
// A nil snapshot (the provider lists per session) has nothing to invalidate.
func (s *podSnapshot) invalidate() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.generation++
	s.mu.Unlock()
}

// projectPod projects a listed pod to its snapshot form.
func projectPod(pod *corev1.Pod) snapshotPod {
	return snapshotPod{name: pod.Name, phase: pod.Status.Phase, deleting: pod.DeletionTimestamp != nil}
}

// podListing returns a listing of the current generation no older than the
// snapshot's TTL, listing the gc-agent pods when the published one is
// missing, expired or from an earlier generation.
func (p *Provider) podListing() (*podListing, error) {
	s := p.snapshot
	l, gen := s.servable(p.clock())
	if l != nil {
		return l, nil
	}
	v, err, _ := s.flights.Do(strconv.FormatUint(gen, 10), func() (any, error) {
		// A flight for gen may have published while this reader queued.
		if l, current := s.servable(p.clock()); l != nil && current == gen {
			return l, nil
		}
		l, err := p.listAgentPods(gen)
		if err != nil {
			return nil, err
		}
		s.publish(l)
		return l, nil
	})
	if err != nil {
		return nil, err
	}
	l, ok := v.(*podListing)
	if !ok {
		return nil, fmt.Errorf("k8s pod snapshot: flight returned %T, want *podListing", v)
	}
	return l, nil
}

// servable returns the published listing when it is of the current generation
// and younger than the TTL at now (else nil), and the current generation.
func (s *podSnapshot) servable(now time.Time) (*podListing, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l := s.listing; l != nil && l.generation == s.generation && now.Sub(l.startedAt) < s.ttl {
		return l, s.generation
	}
	return nil, s.generation
}

// publish makes l the served listing unless an invalidation superseded the
// generation its LIST started in.
func (s *podSnapshot) publish(l *podListing) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l.generation == s.generation {
		s.listing = l
	}
}

// listAgentPods takes one listing of every gc-agent pod, in any phase (the
// label scope ListRunning uses), for snapshot generation gen.
func (p *Provider) listAgentPods(gen uint64) (*podListing, error) {
	ctx, cancel := context.WithTimeout(context.Background(), podSnapshotFetchTimeout)
	defer cancel()
	startedAt := p.clock()
	pods, err := p.ops.listPods(ctx, "app=gc-agent", "")
	if err != nil {
		return nil, fmt.Errorf("k8s pod snapshot: listing gc-agent pods: %w", err)
	}
	l := &podListing{generation: gen, startedAt: startedAt, bySession: make(map[string][]snapshotPod)}
	for i := range pods {
		label := pods[i].Labels["gc-session"]
		if label == "" {
			continue
		}
		l.bySession[label] = append(l.bySession[label], projectPod(&pods[i]))
	}
	return l, nil
}

// observedRunningPod resolves the name of the session's Running pod for an
// observation read: from the pod snapshot when the provider keeps one, else
// with a per-session LIST. A session with no Running pod is
// [runtime.ErrSessionNotFound].
func (p *Provider) observedRunningPod(ctx context.Context, name string) (string, error) {
	if p.snapshot == nil {
		return p.findRunningPod(ctx, name)
	}
	l, err := p.podListing()
	if err != nil {
		return "", err
	}
	for _, pod := range l.bySession[SanitizeLabel(name)] {
		if pod.phase == corev1.PodRunning {
			return pod.name, nil
		}
	}
	return "", fmt.Errorf("%w: no running pod for session %q", runtime.ErrSessionNotFound, name)
}

// observedPod returns the session's first pod in any phase for an observation
// read, and whether there is one: from the pod snapshot when the provider
// keeps one, else with a per-session LIST.
func (p *Provider) observedPod(ctx context.Context, name string) (snapshotPod, bool, error) {
	label := SanitizeLabel(name)
	if p.snapshot == nil {
		pods, err := p.ops.listPods(ctx, "gc-session="+label, "")
		if err != nil || len(pods) == 0 {
			return snapshotPod{}, false, err
		}
		return projectPod(&pods[0]), true, nil
	}
	l, err := p.podListing()
	if err != nil {
		return snapshotPod{}, false, err
	}
	pods := l.bySession[label]
	if len(pods) == 0 {
		return snapshotPod{}, false, nil
	}
	return pods[0], true, nil
}

// observationConn is the exec connection GetLastActivity reads tmux through:
// it resolves the session's pod with [Provider.observedRunningPod] rather than
// a fresh LIST, then execs as [Provider.Exec] does.
type observationConn struct{ p *Provider }

var _ runtime.ExecProvider = observationConn{}

// Exec runs argv in the session's observed Running pod.
func (c observationConn) Exec(ctx context.Context, name string, argv []string) ([]byte, int, error) {
	podName, err := c.p.observedRunningPod(ctx, name)
	if err != nil {
		return nil, -1, fmt.Errorf("k8s exec %q: %w", name, err)
	}
	return c.p.execResolved(ctx, podName, argv)
}
