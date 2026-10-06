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
	"github.com/gastownhall/gascity/internal/runtime/acp"
	"github.com/gastownhall/gascity/internal/runtime/auto"
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
// remain-on-exit pane reads present with its session object id; Running and
// Alive stay false. The bool path is legacy's, unchanged.
// Kills: a corpse read as gone; #{session_id} dropped from the list-panes
// format or parsed from the wrong field; a malformed id accepted; the bool
// path reporting the new fields.
func TestFreshReadCorpseIsPresent(t *testing.T) {
	p, fe := newListPanesProvider(
		"corpse\t1\tbash\t201\t0\t1000\t$7",
		"live\t0\tclaude\t101\t1\t2000\t$3",
		"odd\t1\tbash\t301\t0\t3000\t7",
	)
	for name, want := range map[string]struct{ fresh, since, legacy runtime.Liveness }{
		"corpse":  {runtime.Liveness{Corpse: true, ObjectID: "$7"}, runtime.Liveness{Corpse: true, ObjectID: "$7"}, runtime.Liveness{}},
		"live":    {runtime.Liveness{Running: true, Alive: true, ObjectID: "$3"}, runtime.Liveness{Running: true, Alive: true, ObjectID: "$3", PanePID: "101"}, runtime.Liveness{Running: true, Alive: true}},
		"odd":     {runtime.Liveness{Corpse: true}, runtime.Liveness{Corpse: true}, runtime.Liveness{}},
		"missing": {},
	} {
		if got, err := p.ObserveLivenessWithError(name, []string{"claude"}); err != nil || got != want.fresh {
			t.Errorf("ObserveLivenessWithError(%s) = (%+v, %v), want (%+v, nil)", name, got, err, want.fresh)
		}
		if got, err := p.ObserveLivenessSince(name, []string{"claude"}, time.Time{}); err != nil || got != want.since {
			t.Errorf("ObserveLivenessSince(%s) = (%+v, %v), want (%+v, nil)", name, got, err, want.since)
		}
		if got := p.ObserveLiveness(name, []string{"claude"}); got != want.legacy {
			t.Errorf("ObserveLiveness(%s) = %+v, want legacy %+v", name, got, want.legacy)
		}
		if want.fresh.Present() != (name != "missing") {
			t.Errorf("%s: Present() = %v", name, want.fresh.Present())
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

// A zombie (pane running, agent dead) carries its object id, and a fresh read
// its pane pid, for the start recycle's exact-object kill (v5 F2). Two live
// panes leave the pid empty, so a zombie kill cannot pick one.
// Kills: an object id only on corpses; a pid from a multi-pane session.
func TestFreshReadZombieCarriesObjectID(t *testing.T) {
	h := newObserveHarness(t, "city", nil)
	bash := paneRuntimeState{Command: "bash", PID: "101"}
	h.fetcher.state = runtimeStateSnapshot{
		Sessions: map[string]sessionRuntimeState{
			"worker-1": {Running: true, ID: "$4", Panes: []paneRuntimeState{bash}},
			"worker-2": {Running: true, ID: "$5", Panes: []paneRuntimeState{bash, {Command: "bash", PID: "102"}}},
		},
		Processes:          newProcessSnapshot([]processRuntimeState{{PID: "101", PPID: "1", Command: "bash", Args: "bash"}, {PID: "102", PPID: "1", Command: "bash", Args: "bash"}}),
		ProcessesAvailable: true,
	}
	got, err := h.observe("worker-1")
	requireLiveness(t, got, err, runtime.Liveness{Running: true, ObjectID: "$4"})
	got, err = h.p.ObserveLivenessSince("worker-1", []string{"codex"}, time.Time{})
	requireLiveness(t, got, err, runtime.Liveness{Running: true, ObjectID: "$4", PanePID: "101"})
	got, err = h.p.ObserveLivenessSince("worker-2", []string{"codex"}, time.Time{})
	requireLiveness(t, got, err, runtime.Liveness{Running: true, ObjectID: "$5"})
}

// v5 O1: a fresh read answers only from a fetch that started at or after
// since, never from an older snapshot inside the cache TTL, and never by
// invalidating the cache. A failed fetch is unknown, except a confirmed-dead
// server, which is absent.
// Kills: reusing an older snapshot; a global Invalidate; answering a failed
// refresh from the old snapshot; unbounded retries; trusting a dead server
// that still has a listener.
func TestObserveLivenessSinceRefreshesOnlyOlderSnapshots(t *testing.T) {
	h := newObserveHarness(t, "city", map[string]bool{"worker-1": true})
	h.prime(t)
	h.p.unixListening = func(string) (bool, error) { return false, nil }
	fresh := func(want runtime.Liveness, wantFetches int) {
		t.Helper()
		got, err := h.p.ObserveLivenessSince("worker-1", []string{"codex"}, h.now)
		requireLiveness(t, got, err, want)
		if h.fetcher.getCalls() != wantFetches {
			t.Fatalf("fetches = %d, want %d", h.fetcher.getCalls(), wantFetches)
		}
	}
	fresh(runtime.Liveness{Running: true, Alive: true}, 1) // the primed fetch started at now
	h.advance(time.Second)                                 // inside the 2s TTL
	h.fetcher.setResult(map[string]bool{}, nil)
	fresh(runtime.Liveness{}, 2)
	if obs := h.p.cache.observation(); obs.dirty || h.p.cache.generation != 0 {
		t.Fatalf("cache dirty=%v generation=%d, want no invalidation", obs.dirty, h.p.cache.generation)
	}

	h.advance(time.Second)
	h.fetcher.setResult(nil, errors.New("tmux list-panes: signal: killed"))
	got, err := h.p.ObserveLivenessSince("worker-1", nil, h.now)
	requireLivenessUnknown(t, got, err, nil)
	if h.fetcher.getCalls() != 4 {
		t.Fatalf("fetches = %d, want 4 (two refreshes, then unknown)", h.fetcher.getCalls())
	}

	h.fetcher.setResult(nil, errFetchNoServer)
	h.socketErr = nil
	fresh(runtime.Liveness{}, 6)
	h.p.unixListening = func(string) (bool, error) { return true, nil }
	got, err = h.p.ObserveLivenessSince("worker-1", nil, h.now)
	requireLivenessUnknown(t, got, err, ErrNoServer)
}

// killProvider is a primed Provider holding a corpse "corpse" ($7) whose
// executor then answers every kill with out and err.
func killProvider(t *testing.T) (*Provider, *fakeExecutor) {
	t.Helper()
	p, fe := newListPanesProvider("corpse\t1\tbash\t201\t0\t1000\t$7")
	if _, err := p.ObserveLivenessWithError("corpse", nil); err != nil || !p.cache.observation().primed() {
		t.Fatalf("priming the cache: %v", err)
	}
	fe.calls, fe.out = nil, ""
	return p, fe
}

// v5 F2: the re-check and the kill are one tmux command that targets the
// observed id verbatim, and each outcome maps to its typed result.
// Kills: '$'+id; an unquoted id (tmux expands $N); a kill without the
// re-check; a refusal or a stale id read as killed; a kill not evicted.
func TestKillSessionObjectOneCommandAndOutcomes(t *testing.T) {
	p, fe := killProvider(t)
	if got, err := p.KillCorpseObject("corpse", "$7"); err != nil || got != runtime.SessionObjectKilled {
		t.Fatalf("KillCorpseObject = %v, %v; want killed", got, err)
	}
	want := []string{
		"-u", "-L", "x", "if-shell", "-F", "-t", "$7",
		"#{&&:#{==:#{session_name},corpse},#{&&:#{==:#{session_windows},1},#{&&:#{==:#{window_panes},1},#{pane_dead}}}}",
		"kill-session -t '$7'", "display-message -t '$7' -p 'refused #{session_name}'",
	}
	if len(fe.calls) != 1 || strings.Join(fe.calls[0], "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("tmux calls = %q, want %q", fe.calls, want)
	}
	if _, listed := p.cache.observation().state.Sessions["corpse"]; listed {
		t.Fatal("killed session still in the cache")
	}
	if _, err := p.KillZombieObject("corpse", "$7", "201"); err != nil || !strings.Contains(fe.calls[1][7], "#{!=:#{pane_dead},1},#{==:#{pane_pid},201}") {
		t.Fatalf("zombie condition = %q, %v; want a live pane with pid 201", fe.calls[1][7], err)
	}

	for _, tc := range []struct {
		desc   string
		out    string
		err    error
		corpse runtime.SessionObjectKillResult
		zombie runtime.SessionObjectKillResult
	}{
		{"refused, same name", "refused corpse", nil, runtime.SessionObjectLive, runtime.SessionObjectChanged},
		{"refused, renamed", "refused other", nil, runtime.SessionObjectRenamed, runtime.SessionObjectRenamed},
		{"stale id, tmux 3.4", "refused ", nil, runtime.SessionObjectGone, runtime.SessionObjectGone},
		{"stale id, target error", "", ErrSessionNotFound, runtime.SessionObjectGone, runtime.SessionObjectGone},
		{"no server", "", ErrNoServer, runtime.SessionObjectNotKilled, runtime.SessionObjectNotKilled},
		{"unexpected output", "huh", nil, runtime.SessionObjectNotKilled, runtime.SessionObjectNotKilled},
	} {
		p, fe := killProvider(t)
		fe.out, fe.err = tc.out, tc.err
		corpse, corpseErr := p.KillCorpseObject("corpse", "$7")
		zombie, zombieErr := p.KillZombieObject("corpse", "$7", "201")
		if corpse != tc.corpse || zombie != tc.zombie || (corpseErr != nil) != (tc.corpse == runtime.SessionObjectNotKilled) || (zombieErr != nil) != (tc.zombie == runtime.SessionObjectNotKilled) {
			t.Errorf("%s: corpse = %v, %v; zombie = %v, %v; want %v, %v", tc.desc, corpse, corpseErr, zombie, zombieErr, tc.corpse, tc.zombie)
		}
		if _, listed := p.cache.observation().state.Sessions["corpse"]; !listed {
			t.Errorf("%s: a refused kill evicted the session", tc.desc)
		}
	}
}

// Every value embedded in the command is validated first; nothing reaches
// tmux otherwise. Kills: an empty id or '$'-less id sent; a format- or
// command-breaking name, id or pid sent.
func TestKillSessionObjectRefusesMalformedInput(t *testing.T) {
	p, fe := killProvider(t)
	for _, tc := range []struct{ name, id string }{
		{"corpse", ""},
		{"corpse", "7"},
		{"corpse", "$"},
		{"corpse", "$7 "},
		{"corpse", "$7;kill-server"},
		{"", "$7"},
		{"a,b", "$7"},
		{"a}b", "$7"},
	} {
		if got, err := p.KillCorpseObject(tc.name, tc.id); got != runtime.SessionObjectNotKilled || !errors.Is(err, runtime.ErrInvalidSessionObject) {
			t.Errorf("KillCorpseObject(%q, %q) = %v, %v; want ErrInvalidSessionObject", tc.name, tc.id, got, err)
		}
		if got, err := p.KillZombieObject(tc.name, tc.id, "201"); got != runtime.SessionObjectNotKilled || !errors.Is(err, runtime.ErrInvalidSessionObject) {
			t.Errorf("KillZombieObject(%q, %q) = %v, %v; want ErrInvalidSessionObject", tc.name, tc.id, got, err)
		}
	}
	for _, pid := range []string{"", "1,2", "#{pane_pid}"} {
		if got, err := p.KillZombieObject("corpse", "$7", pid); got != runtime.SessionObjectNotKilled || !errors.Is(err, runtime.ErrInvalidSessionObject) {
			t.Errorf("KillZombieObject pid %q = %v, %v; want ErrInvalidSessionObject", pid, got, err)
		}
	}
	if len(fe.calls) != 0 {
		t.Fatalf("tmux calls = %q, want none", fe.calls)
	}
}

// M2: auto must not hide a tmux corpse behind the other backend's absence,
// and forwards exact-object kills to tmux. Legacy's Running, Alive and error
// are those the fall-through answered, as before.
// Kills: auto returning the acp fall-through over a corpse; no forwarding.
func TestAutoKeepsTmuxCorpseAndForwardsKills(t *testing.T) {
	p, fe := newListPanesProvider("corpse\t1\tbash\t201\t0\t1000\t$7")
	sp := auto.New(&seamBackedProvider{Provider: p}, acp.NewProviderWithDir(t.TempDir(), acp.Config{}))
	want := runtime.Liveness{Corpse: true, ObjectID: "$7"}
	if got, err := runtime.ObserveLivenessWithError(sp, "corpse", nil); err != nil || got != want {
		t.Errorf("auto ObserveLivenessWithError = (%+v, %v), want (%+v, nil)", got, err, want)
	}
	if got, err := runtime.ObserveLivenessSince(sp, "corpse", nil, time.Time{}); err != nil || got != want {
		t.Errorf("auto ObserveLivenessSince = (%+v, %v), want (%+v, nil)", got, err, want)
	}
	fe.calls, fe.out = nil, ""
	if _, err := sp.KillCorpseObject("corpse", "$7"); err != nil || len(fe.calls) != 1 || fe.calls[0][3] != "if-shell" {
		t.Fatalf("auto KillCorpseObject reached tmux with %q, %v", fe.calls, err)
	}
}

// serverDeathProvider returns the seam-backed tmux provider whose server
// socket observation runs the production policy over the given lstat and dial,
// asked through runtime.ServerDeathConfirmer as the inventory lane will. No
// listener is listed unless the test sets one.
func serverDeathProvider(t *testing.T, lstat func(string) (os.FileInfo, error), dial func(context.Context, string) (net.Conn, error)) *Provider {
	t.Helper()
	tm := &Tmux{cfg: Config{SocketName: "city"}, exec: &fakeExecutor{}}
	tm.serverSocketObserver = func(ctx context.Context, path string) error {
		return observeNamedSocketWith(ctx, path, lstat, dial)
	}
	p := &Provider{tm: tm, unixListening: func(string) (bool, error) { return false, nil }}
	var sp runtime.Provider = &seamBackedProvider{Provider: p}
	if _, ok := sp.(runtime.ServerDeathConfirmer); !ok {
		t.Fatal("seam-backed tmux provider does not implement runtime.ServerDeathConfirmer")
	}
	return p
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

func missingSocket(string) (os.FileInfo, error) { return nil, os.ErrNotExist }

// v5 O1, F3: a missing socket with no listener confirms the server dead.
// Kills: ServerConfirmedDead always false (a dead server's pass stays partial).
func TestServerConfirmedDeadMissingSocket(t *testing.T) {
	if !serverDeathProvider(t, missingSocket, dialNotCalled(t)).ServerConfirmedDead() {
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

// Anything short of proof is not dead: a live, unreadable or replaced socket,
// a missing socket file whose path the kernel still lists as listening (a
// live server whose socket was unlinked), or an unreadable listener table.
// Kills: a live server read as dead, which would make every row gone and
// start a second server.
func TestServerConfirmedDeadLiveServerFalse(t *testing.T) {
	info := socketFixtureInfo(t)
	stable := func(string) (os.FileInfo, error) { return info, nil }
	unlinked := serverDeathProvider(t, missingSocket, dialNotCalled(t))
	unlinked.unixListening = func(string) (bool, error) { return true, nil }
	unreadable := serverDeathProvider(t, missingSocket, dialNotCalled(t))
	unreadable.unixListening = func(string) (bool, error) { return false, os.ErrPermission }
	for name, sp := range map[string]*Provider{
		"accepting socket": serverDeathProvider(t, stable, func(context.Context, string) (net.Conn, error) {
			server, client := net.Pipe()
			_ = server.Close()
			return client, nil
		}),
		"dial timeout": serverDeathProvider(t, stable, func(context.Context, string) (net.Conn, error) {
			return nil, context.DeadlineExceeded
		}),
		"unreadable socket":         serverDeathProvider(t, func(string) (os.FileInfo, error) { return nil, os.ErrPermission }, dialNotCalled(t)),
		"unlinked live socket":      unlinked,
		"unreadable /proc/net/unix": unreadable,
	} {
		if sp.ServerConfirmedDead() {
			t.Errorf("%s: ServerConfirmedDead() = true, want false", name)
		}
	}
}

// Kills: a non-listening or other-path entry read as a listener; a path
// with spaces split.
func TestProcNetUnixListening(t *testing.T) {
	table := "Num       RefCount Protocol Flags    Type St Inode Path\n" +
		"0000000000000000: 00000002 00000000 00010000 0001 01 2799733289 /tmp/tmux-1000/city\n" +
		"0000000000000000: 00000003 00000000 00000000 0001 03 2799733290 /tmp/tmux-1000/client\n" +
		"0000000000000000: 00000002 00000000 00010000 0001 01 2799733291 /tmp/a b/city\n"
	for path, want := range map[string]bool{
		"/tmp/tmux-1000/city": true, "/tmp/tmux-1000/client": false, "/tmp/tmux-1000/cit": false, "/tmp/a b/city": true,
	} {
		if got := procNetUnixListening(table, path); got != want {
			t.Errorf("procNetUnixListening(%q) = %v, want %v", path, got, want)
		}
	}
}
