package tmux

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// newListPanesProvider is a Provider over the real list-panes parser, fed by
// a fake executor. The process-table scan is gated off, so nothing runs.
func newListPanesProvider(lines ...string) (*Provider, *fakeExecutor) {
	fe := &fakeExecutor{out: strings.Join(lines, "\n")}
	tm := &Tmux{cfg: Config{SocketName: "x"}, exec: fe}
	fetcher := &tmuxFetcher{tm: tm}
	fetcher.snapshotGate.nextAttempt = time.Now().Add(time.Hour)
	return &Provider{tm: tm, cache: NewStateCache(fetcher, time.Hour)}, fe
}

// I23, v5 O1: the census and every fresh read agree on a corpse. A
// remain-on-exit pane reads present with its session object id, and Running
// and Alive stay false, as legacy reads them.
// Kills: a corpse read as gone; #{session_id} dropped from the list-panes
// format or parsed from the wrong field; an object id on an unlisted name.
func TestFreshReadCorpseIsPresent(t *testing.T) {
	p, fe := newListPanesProvider(
		"corpse\t1\tbash\t201\t0\t1000\t$7",
		"live\t0\tclaude\t101\t1\t2000\t$3",
	)
	want := map[string]runtime.Liveness{
		"corpse":  {Corpse: true, ObjectID: "$7"},
		"live":    {Running: true, Alive: true, ObjectID: "$3"},
		"missing": {},
	}
	for name, want := range want {
		got, err := p.ObserveLivenessWithError(name, []string{"claude"})
		if err != nil || got != want {
			t.Errorf("ObserveLivenessWithError(%s) = (%+v, %v), want (%+v, nil)", name, got, err, want)
		}
		if got := p.ObserveLiveness(name, []string{"claude"}); got != want {
			t.Errorf("ObserveLiveness(%s) = %+v, want %+v", name, got, want)
		}
		if got.Present() != (name != "missing") {
			t.Errorf("%s: Present() = %v", name, got.Present())
		}
		if p.IsRunning(name) != want.Running {
			t.Errorf("IsRunning(%s) = %v, want %v", name, p.IsRunning(name), want.Running)
		}
	}
	// The extra field is the last one, and the activity before it still parses.
	if len(fe.calls) != 1 || !strings.HasSuffix(fe.calls[0][len(fe.calls[0])-1], "#{window_activity}\t#{session_id}") {
		t.Fatalf("tmux calls = %q, want one list-panes ending in #{session_id}", fe.calls)
	}
	if at, ok := p.cache.SessionActivity("live"); !ok || !at.Equal(time.Unix(2000, 0)) {
		t.Fatalf("SessionActivity(live) = %v, %v; want 2000", at, ok)
	}
}

// A zombie (pane running, agent dead) carries its object id too, for the
// start recycle's exact-object kill (v5 F2).
// Kills: an object id set only on corpse rows.
func TestFreshReadZombieCarriesObjectID(t *testing.T) {
	h := newObserveHarness(t, "city", nil)
	h.fetcher.state = runtimeStateSnapshot{
		Sessions: map[string]sessionRuntimeState{
			"worker-1": {Running: true, ID: "$4", Panes: []paneRuntimeState{{Command: "bash", PID: "101"}}},
		},
		Processes:          newProcessSnapshot([]processRuntimeState{{PID: "101", PPID: "1", Command: "bash", Args: "bash"}}),
		ProcessesAvailable: true,
	}
	got, err := h.observe("worker-1")
	requireLiveness(t, got, err, runtime.Liveness{Running: true, ObjectID: "$4"})
}

// serverDeathProvider returns the seam-backed tmux provider whose server
// socket observation runs the production policy over the given lstat and dial,
// asked through runtime.ServerDeathConfirmer as the inventory lane will.
func serverDeathProvider(t *testing.T, lstat func(string) (os.FileInfo, error), dial func(context.Context, string) (net.Conn, error)) runtime.ServerDeathConfirmer {
	t.Helper()
	tm := &Tmux{cfg: Config{SocketName: "city"}, exec: &fakeExecutor{}}
	tm.serverSocketObserver = func(ctx context.Context, path string) error {
		return observeNamedSocketWith(ctx, path, lstat, dial)
	}
	var sp runtime.Provider = &seamBackedProvider{Provider: &Provider{tm: tm}}
	confirmer, ok := sp.(runtime.ServerDeathConfirmer)
	if !ok {
		t.Fatal("seam-backed tmux provider does not implement runtime.ServerDeathConfirmer")
	}
	return confirmer
}

func socketFixtureInfo(t *testing.T) os.FileInfo {
	t.Helper()
	path := filepath.Join(t.TempDir(), "socket-fixture")
	if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
		t.Fatalf("write socket fixture: %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat socket fixture: %v", err)
	}
	return socketModeFileInfo{FileInfo: info}
}

func dialNotCalled(t *testing.T) func(context.Context, string) (net.Conn, error) {
	return func(context.Context, string) (net.Conn, error) {
		t.Error("dialed a missing socket")
		return nil, errors.New("unexpected dial")
	}
}

// v5 O1, F3: a missing socket confirms the server dead.
// Kills: ServerConfirmedDead always false (a dead server's pass stays partial).
func TestServerConfirmedDeadMissingSocket(t *testing.T) {
	sp := serverDeathProvider(t, func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }, dialNotCalled(t))
	if !sp.ServerConfirmedDead() {
		t.Fatal("ServerConfirmedDead() = false for a missing socket, want true")
	}
}

// v5 O1, F3: a socket that refuses connections on a stable inode confirms the
// server dead. The socket inode is made with mknod, so nothing listens.
// Kills: ServerConfirmedDead always false.
func TestServerConfirmedDeadRefusedSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stale-socket")
	if err := syscall.Mknod(path, syscall.S_IFSOCK|0o600, 0); err != nil {
		t.Skipf("mknod socket inode: %v", err)
	}
	sp := serverDeathProvider(t, func(string) (os.FileInfo, error) { return os.Lstat(path) },
		func(context.Context, string) (net.Conn, error) { return nil, syscall.ECONNREFUSED })
	if !sp.ServerConfirmedDead() {
		t.Fatal("ServerConfirmedDead() = false for a refusing socket on a stable inode, want true")
	}
}

// Anything short of proof, including a live or replaced socket, is not dead.
// Kills: a live server read as dead, which would make every row gone.
func TestServerConfirmedDeadLiveServerFalse(t *testing.T) {
	info := socketFixtureInfo(t)
	stable := func(string) (os.FileInfo, error) { return info, nil }
	for name, sp := range map[string]runtime.ServerDeathConfirmer{
		"accepting socket": serverDeathProvider(t, stable, func(context.Context, string) (net.Conn, error) {
			server, client := net.Pipe()
			_ = server.Close()
			return client, nil
		}),
		"dial timeout": serverDeathProvider(t, stable, func(context.Context, string) (net.Conn, error) {
			return nil, context.DeadlineExceeded
		}),
		"unreadable socket": serverDeathProvider(t, func(string) (os.FileInfo, error) { return nil, os.ErrPermission }, dialNotCalled(t)),
	} {
		if sp.ServerConfirmedDead() {
			t.Errorf("%s: ServerConfirmedDead() = true, want false", name)
		}
	}
}
