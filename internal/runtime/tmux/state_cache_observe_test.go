package tmux

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// errFetchNoServer is the refresh error FetchState returns for an unreachable
// tmux server.
var errFetchNoServer = fmt.Errorf("%w: %w", runtime.ErrRuntimeUnavailable, ErrNoServer)

// observeHarness is a Provider over a scripted fetcher, a fake cache clock and
// an injected server-socket observation, so no tmux process or socket is used.
type observeHarness struct {
	p       *Provider
	fetcher *mockFetcher
	now     time.Time
	// socketErr is the server-socket observation: nil means confirmed gone.
	socketErr   error
	socketPaths []string
	logs        []string
}

func newObserveHarness(t *testing.T, socketName string, sessions map[string]bool) *observeHarness {
	t.Helper()
	h := &observeHarness{
		fetcher:   &mockFetcher{sessions: sessions},
		now:       time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC),
		socketErr: errors.New("path=sock reason=live-unix-socket"),
	}
	cache := NewStateCache(h.fetcher, 2*time.Second)
	cache.now = func() time.Time { return h.now }
	tm := &Tmux{
		cfg:  Config{SocketName: socketName},
		exec: &fakeExecutor{},
		serverSocketObserver: func(_ context.Context, path string) error {
			h.socketPaths = append(h.socketPaths, path)
			return h.socketErr
		},
	}
	h.p = &Provider{tm: tm, cache: cache, logf: func(format string, args ...any) {
		h.logs = append(h.logs, fmt.Sprintf(format, args...))
	}}
	return h
}

func (h *observeHarness) advance(d time.Duration) { h.now = h.now.Add(d) }

// prime publishes a successful snapshot and checks it holds worker-1.
func (h *observeHarness) prime(t *testing.T) {
	t.Helper()
	got, err := h.observe("worker-1")
	requireLiveness(t, got, err, runtime.Liveness{Running: true, Alive: true})
}

func (h *observeHarness) observe(name string) (runtime.Liveness, error) {
	return h.p.ObserveLivenessWithError(name, []string{"codex"})
}

func requireLivenessUnknown(t *testing.T, got runtime.Liveness, err error, cause error) {
	t.Helper()
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("ObserveLivenessWithError = (%+v, %v), want an error wrapping runtime.ErrRuntimeUnavailable", got, err)
	}
	if cause != nil && !errors.Is(err, cause) {
		t.Fatalf("ObserveLivenessWithError error = %v, want it to wrap the refresh error %v", err, cause)
	}
	if errors.Is(err, runtime.ErrSessionNotFound) || runtime.IsSessionGone(err) {
		t.Fatalf("ObserveLivenessWithError error = %v, must not read as a gone session", err)
	}
	if got != (runtime.Liveness{}) {
		t.Fatalf("ObserveLivenessWithError value = %+v alongside an error, want zero", got)
	}
}

func requireLiveness(t *testing.T, got runtime.Liveness, err error, want runtime.Liveness) {
	t.Helper()
	if err != nil || got != want {
		t.Fatalf("ObserveLivenessWithError = (%+v, %v), want (%+v, nil)", got, err, want)
	}
}

func TestObserveFreshSnapshotIsComplete(t *testing.T) {
	h := newObserveHarness(t, "city", map[string]bool{"worker-1": true})

	got, err := h.observe("worker-1")
	requireLiveness(t, got, err, runtime.Liveness{Running: true, Alive: true})
	got, err = h.observe("worker-10")
	requireLiveness(t, got, err, runtime.Liveness{})
	if len(h.socketPaths) != 0 {
		t.Fatalf("socket observed %q on a successful refresh, want no socket check", h.socketPaths)
	}
}

func TestObserveStaleAfterFailedRefreshIsUnavailable(t *testing.T) {
	h := newObserveHarness(t, "city", map[string]bool{"worker-1": true})
	h.prime(t)
	timeout := errors.New("tmux list-panes: signal: killed")
	h.fetcher.setResult(nil, timeout)

	// Inside staleTTL a clean last-known-good snapshot still answers (#4082).
	h.advance(10 * time.Second)
	got, err := h.observe("worker-1")
	requireLiveness(t, got, err, runtime.Liveness{Running: true, Alive: true})

	// Past staleTTL it can no longer vouch either way.
	h.advance(defaultStaleTTL)
	got, err = h.observe("worker-1")
	requireLivenessUnknown(t, got, err, timeout)
	got, err = h.observe("worker-10")
	requireLivenessUnknown(t, got, err, timeout)

	// Only a no-server failure consults the socket: a timeout over a socket
	// that would read dead is still unknown.
	h.socketErr = nil
	got, err = h.observe("worker-1")
	requireLivenessUnknown(t, got, err, timeout)
	if len(h.socketPaths) != 0 {
		t.Fatalf("socket observed %q after a timeout, want no socket check", h.socketPaths)
	}
}

func TestObserveDeadServerCorroboratedIsAbsent(t *testing.T) {
	h := newObserveHarness(t, "city", map[string]bool{"worker-1": true})
	h.prime(t)
	h.fetcher.setResult(nil, errFetchNoServer)
	h.socketErr = nil

	// Inside staleTTL the bool path still reports the snapshot, so the error
	// form answers from it too (#4082) rather than contradicting it: a clean
	// snapshot answers as is, and a dirty one that still lists the session is
	// unknown. Only a session the snapshot no longer lists reads absent.
	h.advance(10 * time.Second)
	got, err := h.observe("worker-1")
	requireLiveness(t, got, err, runtime.Liveness{Running: true, Alive: true})
	h.p.cache.Invalidate()
	got, err = h.observe("worker-1")
	requireLivenessUnknown(t, got, err, ErrNoServer)
	got, err = h.observe("worker-10")
	requireLiveness(t, got, err, runtime.Liveness{})

	// Past staleTTL the snapshot cannot answer: a session cannot outlive its
	// server.
	h.advance(defaultStaleTTL)
	got, err = h.observe("worker-1")
	requireLiveness(t, got, err, runtime.Liveness{})
	if len(h.socketPaths) == 0 || h.socketPaths[0] != namedSocketPath("city") {
		t.Fatalf("socket observed at %q, want the city socket %q", h.socketPaths, namedSocketPath("city"))
	}
}

func TestObserveNoServerWithLiveSocketIsUnavailable(t *testing.T) {
	h := newObserveHarness(t, "", map[string]bool{"worker-1": true})
	h.prime(t)
	h.fetcher.setResult(nil, errFetchNoServer)

	h.advance(defaultStaleTTL + time.Second)
	got, err := h.observe("worker-1")
	requireLivenessUnknown(t, got, err, ErrNoServer)
	if len(h.socketPaths) != 1 || h.socketPaths[0] != namedSocketPath("default") {
		t.Fatalf("socket observed at %q, want the default socket %q", h.socketPaths, namedSocketPath("default"))
	}
}

func TestObserveDirtyAfterFailedRefreshIsUnavailable(t *testing.T) {
	h := newObserveHarness(t, "city", map[string]bool{"worker-1": true})
	h.prime(t)
	timeout := errors.New("tmux list-panes: signal: killed")
	h.fetcher.setResult(nil, timeout)

	// A Stop (or Start) landed after the snapshot, and the refresh that should
	// have observed it failed: the snapshot predates a known change.
	h.p.cache.EvictSession("worker-1")
	got, err := h.observe("worker-1")
	requireLivenessUnknown(t, got, err, timeout)
	h.p.cache.Invalidate()
	got, err = h.observe("worker-2")
	requireLivenessUnknown(t, got, err, timeout)
}

func TestObserveUnprimedFailureIsUnavailable(t *testing.T) {
	h := newObserveHarness(t, "city", nil)
	// A missing binary: its text contains "not found", which IsSessionGone
	// would read as a gone session if the message carried it.
	missing := errors.New(`tmux list-panes: exec: "tmux": executable file not found in $PATH`)
	h.fetcher.setResult(nil, missing)

	got, err := h.observe("worker-1")
	requireLivenessUnknown(t, got, err, missing)
}

func TestObserveUnprimedNoServerNeedsCorroboration(t *testing.T) {
	h := newObserveHarness(t, "city", nil)
	h.fetcher.setResult(nil, errFetchNoServer)

	// The empty no-server prime is not a server's answer.
	got, err := h.observe("worker-1")
	requireLivenessUnknown(t, got, err, ErrNoServer)

	// A missing or refusing socket confirms it: a fresh city may start.
	h.socketErr = nil
	got, err = h.observe("worker-1")
	requireLiveness(t, got, err, runtime.Liveness{})
}

func TestIsRunningKeepsLegacyStaleCliff(t *testing.T) {
	h := newObserveHarness(t, "city", map[string]bool{"worker-1": true})
	if !h.p.IsRunning("worker-1") {
		t.Fatal("IsRunning = false on a fresh snapshot, want true")
	}
	h.fetcher.setResult(nil, errors.New("tmux list-panes: signal: killed"))

	h.advance(10 * time.Second)
	if !h.p.IsRunning("worker-1") {
		t.Fatal("IsRunning = false inside staleTTL, want last-known-good true")
	}
	h.advance(defaultStaleTTL)
	if h.p.IsRunning("worker-1") {
		t.Fatal("IsRunning = true past staleTTL, want the legacy all-absent cliff")
	}
	if got := h.p.ObserveLiveness("worker-1", []string{"codex"}); got != (runtime.Liveness{}) {
		t.Fatalf("ObserveLiveness = %+v past staleTTL, want the legacy all-absent cliff", got)
	}
}

func TestObserveLivenessWithErrorPromotedThroughSeamBackedProvider(t *testing.T) {
	h := newObserveHarness(t, "city", map[string]bool{"worker-1": true})
	var sp runtime.Provider = &seamBackedProvider{Provider: h.p, seams: runtime.NewProviderFromSeams(h.p.Seams())}
	if _, ok := sp.(runtime.LivenessObserverWithError); !ok {
		t.Fatal("seam-backed tmux provider does not implement LivenessObserverWithError")
	}
	h.prime(t)
	h.fetcher.setResult(nil, errors.New("tmux list-panes: signal: killed"))
	h.advance(defaultStaleTTL + time.Second)

	got, err := runtime.ObserveLivenessWithError(sp, "worker-1", []string{"codex"})
	requireLivenessUnknown(t, got, err, nil)
}

func TestObserveLogsOncePerEpisode(t *testing.T) {
	h := newObserveHarness(t, "city", map[string]bool{"worker-1": true})
	h.prime(t)
	h.fetcher.setResult(nil, errFetchNoServer)
	h.advance(defaultStaleTTL + time.Second)

	observeEach := func() {
		for range 3 {
			_, _ = h.observe("worker-1")
			_, _ = h.observe("worker-2")
		}
	}
	observeEach() // live socket: unknown
	h.socketErr = nil
	observeEach() // the server is confirmed dead: absent
	h.socketErr = errors.New("path=sock reason=live-unix-socket")
	observeEach() // unknown again
	h.fetcher.setResult(map[string]bool{"worker-1": true}, nil)
	h.advance(5 * time.Second)
	observeEach() // a refresh succeeds

	// Only an answer ends an episode: unknown again after the dead server
	// is still the same unknown episode.
	requireLogs(t, h.logs, "reporting unknown", "confirmed dead", "answering again")
}

// TestObserveLogsOnceWhileOutcomesDifferPerSession pins that a dirty cache
// inside staleTTL, where a listed session is unknown and an unlisted one is
// absent behind a dead socket, logs each episode once rather than flapping.
func TestObserveLogsOnceWhileOutcomesDifferPerSession(t *testing.T) {
	h := newObserveHarness(t, "city", map[string]bool{"worker-1": true})
	h.prime(t)
	h.fetcher.setResult(nil, errFetchNoServer)
	h.socketErr = nil
	h.advance(10 * time.Second)
	h.p.cache.Invalidate()

	for range 3 {
		got, err := h.observe("worker-1")
		requireLivenessUnknown(t, got, err, ErrNoServer)
		got, err = h.observe("worker-10")
		requireLiveness(t, got, err, runtime.Liveness{})
	}
	requireLogs(t, h.logs, "reporting unknown", "confirmed dead")
}

// requireLogs checks logs holds exactly one line per want, in order.
func requireLogs(t *testing.T, logs []string, want ...string) {
	t.Helper()
	if len(logs) != len(want) {
		t.Fatalf("logs = %q, want one line per episode edge %q", logs, want)
	}
	for i := range want {
		if !strings.Contains(logs[i], want[i]) {
			t.Fatalf("logs = %q, want one line per episode edge %q", logs, want)
		}
	}
}

// TestObserveAgreesWithBoolLiveness pins that the two liveness forms never
// contradict each other: whenever ObserveLivenessWithError answers without an
// error, it answers exactly what IsRunning and ObserveLiveness report. A
// confirmed absence while IsRunning still reads true would let the start path
// record a start that the bool gate in StartResolved skipped.
func TestObserveAgreesWithBoolLiveness(t *testing.T) {
	scenarios := []struct {
		name  string
		setup func(h *observeHarness)
	}{
		{"fresh", func(*observeHarness) {}},
		{"no-server inside staleTTL, socket dead", func(h *observeHarness) {
			h.fetcher.setResult(nil, errFetchNoServer)
			h.socketErr = nil
			h.advance(10 * time.Second)
		}},
		{"no-server inside staleTTL, dirty, socket dead", func(h *observeHarness) {
			h.fetcher.setResult(nil, errFetchNoServer)
			h.socketErr = nil
			h.advance(10 * time.Second)
			h.p.cache.Invalidate()
		}},
		{"timeout inside staleTTL", func(h *observeHarness) {
			h.fetcher.setResult(nil, errors.New("tmux list-panes: signal: killed"))
			h.advance(10 * time.Second)
		}},
		{"no-server at exactly staleTTL, socket dead", func(h *observeHarness) {
			h.fetcher.setResult(nil, errFetchNoServer)
			h.socketErr = nil
			h.advance(defaultStaleTTL)
		}},
		{"no-server past staleTTL, socket dead", func(h *observeHarness) {
			h.fetcher.setResult(nil, errFetchNoServer)
			h.socketErr = nil
			h.advance(defaultStaleTTL + time.Second)
		}},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			h := newObserveHarness(t, "city", map[string]bool{"worker-1": true})
			h.prime(t)
			sc.setup(h)
			for _, name := range []string{"worker-1", "worker-10"} {
				got, err := h.observe(name)
				running := h.p.IsRunning(name)
				legacy := h.p.ObserveLiveness(name, []string{"codex"})
				if err != nil {
					continue
				}
				if got.Running != running || got != legacy {
					t.Fatalf("%s: ObserveLivenessWithError = %+v, but IsRunning = %v and ObserveLiveness = %+v", name, got, running, legacy)
				}
			}
		})
	}
}

func TestObserveZombiePaneIsRunningButNotAlive(t *testing.T) {
	h := newObserveHarness(t, "city", nil)
	h.fetcher.state = runtimeStateSnapshot{
		Sessions: map[string]sessionRuntimeState{
			"worker-1": {Running: true, Panes: []paneRuntimeState{{Command: "bash", PID: "101"}}},
		},
		Processes: newProcessSnapshot([]processRuntimeState{
			{PID: "101", PPID: "1", Command: "bash", Args: "bash"},
		}),
		ProcessesAvailable: true,
	}

	got, err := h.observe("worker-1")
	requireLiveness(t, got, err, runtime.Liveness{Running: true, Alive: false})
}

func TestObserveSuccessfulRefreshClearsNoServerPrime(t *testing.T) {
	h := newObserveHarness(t, "city", nil)
	h.fetcher.setResult(nil, errFetchNoServer)
	got, err := h.observe("worker-1")
	requireLivenessUnknown(t, got, err, ErrNoServer)

	// The server comes up empty and answers; then one refresh times out. The
	// server-listed snapshot is clean and inside staleTTL, so it answers.
	h.fetcher.setResult(map[string]bool{}, nil)
	h.advance(5 * time.Second)
	got, err = h.observe("worker-1")
	requireLiveness(t, got, err, runtime.Liveness{})
	h.fetcher.setResult(nil, errors.New("tmux list-panes: signal: killed"))
	h.advance(5 * time.Second)
	got, err = h.observe("worker-1")
	requireLiveness(t, got, err, runtime.Liveness{})
}
