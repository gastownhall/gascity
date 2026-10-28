package tmux

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
// remain-on-exit pane reads present with its session object id and creation
// time; Running and Alive stay false. A row without the creation time (the
// format before it) still parses. The bool path is legacy's, unchanged.
// Kills: a corpse read as gone; #{session_id} or #{session_created} dropped
// from the list-panes format or parsed from the wrong field; a malformed id
// or creation time accepted; an old row rejected; the bool path reporting
// the new fields.
func TestFreshReadCorpseIsPresent(t *testing.T) {
	p, fe := newListPanesProvider(
		"corpse\t1\tbash\t201\t0\t1000\t$7\t900",
		"live\t0\tclaude\t101\t1\t2000\t$3\t800",
		"odd\t1\tbash\t301\t0\t3000\t7\t9x",
		"old\t1\tbash\t401\t0\t4000\t$9",
	)
	for name, want := range map[string]struct{ fresh, since, legacy runtime.Liveness }{
		"corpse":  {runtime.Liveness{Corpse: true, ObjectID: "$7", ObjectCreated: "900"}, runtime.Liveness{Corpse: true, ObjectID: "$7", ObjectCreated: "900"}, runtime.Liveness{}},
		"live":    {runtime.Liveness{Running: true, Alive: true, ObjectID: "$3", ObjectCreated: "800"}, runtime.Liveness{Running: true, Alive: true, ObjectID: "$3", ObjectCreated: "800", PanePID: "101"}, runtime.Liveness{Running: true, Alive: true}},
		"odd":     {runtime.Liveness{Corpse: true}, runtime.Liveness{Corpse: true}, runtime.Liveness{}},
		"old":     {runtime.Liveness{Corpse: true, ObjectID: "$9"}, runtime.Liveness{Corpse: true, ObjectID: "$9"}, runtime.Liveness{}},
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
	// The extra fields are the last ones, and the activity before them still
	// parses.
	if len(fe.calls) != 1 || !strings.HasSuffix(fe.calls[0][len(fe.calls[0])-1], "#{window_activity}\t#{session_id}\t#{session_created}") {
		t.Fatalf("tmux calls = %q, want one list-panes ending in #{session_id}\\t#{session_created}", fe.calls)
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

// killProvider is a primed Provider holding a corpse "corpse" ($7, created
// at 900) whose executor then answers every kill with out and err.
func killProvider(t *testing.T) (*Provider, *fakeExecutor) {
	t.Helper()
	p, fe := newListPanesProvider("corpse\t1\tbash\t201\t0\t1000\t$7\t900")
	if _, err := p.ObserveLivenessWithError("corpse", nil); err != nil || !p.cache.observation().primed() {
		t.Fatalf("priming the cache: %v", err)
	}
	fe.calls, fe.out = nil, ""
	return p, fe
}

// v5 F2: the re-check and the kill are one tmux command that targets the
// observed id verbatim, pinned to its creation time, and each outcome maps to
// its typed result.
// Kills: '$'+id; an unquoted id (tmux expands $N); a kill without the
// re-check, without the creation time or without the no-client check (J17)
// in either condition; a refusal or a
// stale or reused id read as killed or as refused live; a kill not evicted.
func TestKillSessionObjectOneCommandAndOutcomes(t *testing.T) {
	p, fe := killProvider(t)
	if got, err := p.KillCorpseObject("corpse", "$7", "900"); err != nil || got != runtime.SessionObjectKilled {
		t.Fatalf("KillCorpseObject = %v, %v; want killed", got, err)
	}
	want := []string{
		"-u", "-L", "x", "if-shell", "-F", "-t", "$7",
		"#{&&:#{==:#{session_created},900},#{&&:#{==:#{session_name},corpse},#{&&:#{==:#{session_attached},0},#{&&:#{==:#{session_windows},1},#{&&:#{==:#{window_panes},1},#{pane_dead}}}}}}",
		"kill-session -t '$7'", "display-message -t '$7' -p 'refused #{session_created} #{session_name}'",
	}
	if len(fe.calls) != 1 || strings.Join(fe.calls[0], "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("tmux calls = %q, want %q", fe.calls, want)
	}
	if _, listed := p.cache.observation().state.Sessions["corpse"]; listed {
		t.Fatal("killed session still in the cache")
	}
	if _, err := p.KillZombieObject("corpse", "$7", "900", "201"); err != nil || !strings.HasPrefix(fe.calls[1][7], "#{&&:#{==:#{session_created},900},") || !strings.Contains(fe.calls[1][7], "#{!=:#{pane_dead},1},#{==:#{pane_pid},201}") || !strings.Contains(fe.calls[1][7], "#{==:#{session_attached},0}") {
		t.Fatalf("zombie condition = %q, %v; want created 900, no client attached and a live pane with pid 201", fe.calls[1][7], err)
	}

	for _, tc := range []struct {
		desc   string
		out    string
		err    error
		corpse runtime.SessionObjectKillResult
		zombie runtime.SessionObjectKillResult
	}{
		{"refused, same name", "refused 900 corpse", nil, runtime.SessionObjectLive, runtime.SessionObjectChanged},
		{"refused, renamed", "refused 900 other", nil, runtime.SessionObjectRenamed, runtime.SessionObjectRenamed},
		{"reused id, same name", "refused 901 corpse", nil, runtime.SessionObjectGone, runtime.SessionObjectGone},
		{"stale id, tmux 3.4", "refused  ", nil, runtime.SessionObjectGone, runtime.SessionObjectGone},
		{"stale id, target error", "", ErrSessionNotFound, runtime.SessionObjectGone, runtime.SessionObjectGone},
		{"no server", "", ErrNoServer, runtime.SessionObjectNotKilled, runtime.SessionObjectNotKilled},
		{"unexpected output", "huh", nil, runtime.SessionObjectNotKilled, runtime.SessionObjectNotKilled},
	} {
		p, fe := killProvider(t)
		fe.out, fe.err = tc.out, tc.err
		corpse, corpseErr := p.KillCorpseObject("corpse", "$7", "900")
		zombie, zombieErr := p.KillZombieObject("corpse", "$7", "900", "201")
		if corpse != tc.corpse || zombie != tc.zombie || (corpseErr != nil) != (tc.corpse == runtime.SessionObjectNotKilled) || (zombieErr != nil) != (tc.zombie == runtime.SessionObjectNotKilled) {
			t.Errorf("%s: corpse = %v, %v; zombie = %v, %v; want %v, %v", tc.desc, corpse, corpseErr, zombie, zombieErr, tc.corpse, tc.zombie)
		}
		if _, listed := p.cache.observation().state.Sessions["corpse"]; !listed {
			t.Errorf("%s: a refused kill evicted the session", tc.desc)
		}
	}
}

// Every value embedded in the command is validated first; nothing reaches
// tmux otherwise. Kills: an empty id or creation time sent, or a '$'-less id;
// a format- or command-breaking name, id, creation time or pid sent.
func TestKillSessionObjectRefusesMalformedInput(t *testing.T) {
	p, fe := killProvider(t)
	for _, tc := range []struct{ name, id, created string }{
		{"corpse", "", "900"},
		{"corpse", "7", "900"},
		{"corpse", "$", "900"},
		{"corpse", "$7 ", "900"},
		{"corpse", "$7;kill-server", "900"},
		{"", "$7", "900"},
		{"a,b", "$7", "900"},
		{"a}b", "$7", "900"},
		{"corpse", "$7", ""},
		{"corpse", "$7", "9 0"},
		{"corpse", "$7", "-900"},
		{"corpse", "$7", "1},#{1"},
		{"corpse", "$7", "#{session_created}"},
	} {
		if got, err := p.KillCorpseObject(tc.name, tc.id, tc.created); got != runtime.SessionObjectNotKilled || !errors.Is(err, runtime.ErrInvalidSessionObject) {
			t.Errorf("KillCorpseObject(%q, %q, %q) = %v, %v; want ErrInvalidSessionObject", tc.name, tc.id, tc.created, got, err)
		}
		if got, err := p.KillZombieObject(tc.name, tc.id, tc.created, "201"); got != runtime.SessionObjectNotKilled || !errors.Is(err, runtime.ErrInvalidSessionObject) {
			t.Errorf("KillZombieObject(%q, %q, %q) = %v, %v; want ErrInvalidSessionObject", tc.name, tc.id, tc.created, got, err)
		}
	}
	for _, pid := range []string{"", "1,2", "#{pane_pid}"} {
		if got, err := p.KillZombieObject("corpse", "$7", "900", pid); got != runtime.SessionObjectNotKilled || !errors.Is(err, runtime.ErrInvalidSessionObject) {
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
	p, fe := newListPanesProvider("corpse\t1\tbash\t201\t0\t1000\t$7\t900")
	sp := auto.New(&seamBackedProvider{Provider: p}, acp.NewProviderWithDir(t.TempDir(), acp.Config{}))
	want := runtime.Liveness{Corpse: true, ObjectID: "$7", ObjectCreated: "900"}
	if got, err := runtime.ObserveLivenessWithError(sp, "corpse", nil); err != nil || got != want {
		t.Errorf("auto ObserveLivenessWithError = (%+v, %v), want (%+v, nil)", got, err, want)
	}
	if got, err := runtime.ObserveLivenessSince(sp, "corpse", nil, time.Time{}); err != nil || got != want {
		t.Errorf("auto ObserveLivenessSince = (%+v, %v), want (%+v, nil)", got, err, want)
	}
	fe.calls, fe.out = nil, ""
	if _, err := sp.KillCorpseObject("corpse", "$7", "900"); err != nil || len(fe.calls) != 1 || fe.calls[0][3] != "if-shell" || !strings.Contains(fe.calls[0][7], "#{session_created},900}") {
		t.Fatalf("auto KillCorpseObject reached tmux with %q, %v", fe.calls, err)
	}
	fe.calls = nil
	if _, err := sp.KillZombieObject("corpse", "$7", "900", "201"); err != nil || len(fe.calls) != 1 || !strings.Contains(fe.calls[0][7], "#{session_created},900}") || !strings.Contains(fe.calls[0][7], "#{pane_pid},201}") {
		t.Fatalf("auto KillZombieObject reached tmux with %q, %v", fe.calls, err)
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

// Kills: another or prefixed path, or a pid or fd field, read as a socket
// bound to the path, any of which would hold a dead server alive; a bound
// name missed, which would read a live server dead.
func TestLsofUnixBound(t *testing.T) {
	out := "p501\nf3\nn->0x1f2e3d4c5b6a7980\nf4\nn/private/tmp/tmux-501/client\n" +
		"p502\nf6\nn/private/tmp/tmux-501/city\nf7\nn/private/tmp/a b/city\n"
	for path, want := range map[string]bool{
		"/private/tmp/tmux-501/city": true, "/private/tmp/a b/city": true, "/private/tmp/tmux-501/cit": false,
		"/tmp/tmux-501/city": false, "501": false, "3": false,
	} {
		if got := lsofUnixBound(out, path); got != want {
			t.Errorf("lsofUnixBound(%q) = %v, want %v", path, got, want)
		}
	}
}

type exitCodeError int

func (e exitCodeError) Error() string { return "exit status " + strconv.Itoa(int(e)) }
func (e exitCodeError) ExitCode() int { return int(e) }

// Kills: a failure (stderr, partial output, any exit, a timeout's kill, a
// missing binary) read as an answer, which would confirm a live server dead;
// lsof's empty exit 0, its "nothing bound", read as an error, which would
// leave every dead server unconfirmed on macOS and fail gc suspend there.
func TestLsofAnswer(t *testing.T) {
	if out, err := lsofAnswer([]byte("p1\nf3\nn/x\n"), "", nil); err != nil || out != "p1\nf3\nn/x\n" {
		t.Errorf("lsofAnswer(exit 0) = (%q, %v), want the output", out, err)
	}
	if out, err := lsofAnswer(nil, "", nil); err != nil || out != "" {
		t.Errorf("lsofAnswer(empty exit 0) = (%q, %v), want the empty answer", out, err)
	}
	for name, run := range map[string]struct {
		out    string
		stderr string
		err    error
	}{
		"silent exit 1":      {err: exitCodeError(1)},
		"exit 1 with stderr": {stderr: "lsof: can't read", err: exitCodeError(1)},
		"exit 1 with output": {out: "p1\nf3\n", err: exitCodeError(1)},
		"killed by timeout":  {err: exitCodeError(-1)},
		"not found on PATH":  {err: exec.ErrNotFound},
	} {
		if _, err := lsofAnswer([]byte(run.out), run.stderr, run.err); err == nil {
			t.Errorf("%s: lsofAnswer error = nil, want an error", name)
		}
	}
}

// lsof escapes a backslash, control bytes and bytes from 0x80, so such a path
// never matches its listing. Kills: an escaped path accepted, which would read
// its live server dead; a plain path, spaces included, refused, which would
// leave its dead server unconfirmed.
func TestLsofPrintsVerbatim(t *testing.T) {
	for path, want := range map[string]bool{
		"/private/tmp/tmux-501/city": true, "/private/tmp/a b/city": true, "/tmp/a\\b": false,
		"/tmp/a\nb": false, "/tmp/a\tb": false, "/tmp/a\x7fb": false, "/tmp/caf\u00e9": false,
	} {
		if got := lsofPrintsVerbatim(path); got != want {
			t.Errorf("lsofPrintsVerbatim(%q) = %v, want %v", path, got, want)
		}
	}
}

// tmux binds under the resolved directory, so the candidates rejoin the path
// under its longest existing ancestor, even once the socket directory is
// removed. Kills: resolving only the socket directory, which loses the bound
// path when that directory is gone and reads its live server dead.
func TestUnixSocketPathCandidates(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		filepath.Join(root, "link", "city"):                  filepath.Join(resolvedRoot, "real", "city"),
		filepath.Join(root, "link", "tmux-501", "city"):      filepath.Join(resolvedRoot, "real", "tmux-501", "city"),
		filepath.Join(root, "link", "gone", "tmux-501", "c"): filepath.Join(resolvedRoot, "real", "gone", "tmux-501", "c"),
	} {
		if got := unixSocketPathCandidates(path); len(got) != 2 || got[0] != path || got[1] != want {
			t.Errorf("unixSocketPathCandidates(%q) = %q, want [%q %q]", path, got, path, want)
		}
	}
}

// presenceHarness is a Provider over the real tmuxFetcher: its tmux calls go
// to a fake executor, its process-table snapshot is counted instead of run,
// and its cache runs on a fake clock. The socket observer reports a live
// server, so a no-server answer is never confirmed dead.
type presenceHarness struct {
	p         *Provider
	fe        *fakeExecutor
	now       time.Time
	snapshots int
	processes []processRuntimeState
}

func newPresenceHarness(t *testing.T) *presenceHarness {
	t.Helper()
	h := &presenceHarness{fe: &fakeExecutor{}, now: time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)}
	tm := &Tmux{
		cfg:                  Config{SocketName: "presence-test"},
		exec:                 h.fe,
		serverSocketObserver: func(context.Context, string) error { return errors.New("live-unix-socket") },
	}
	fetcher := &tmuxFetcher{tm: tm, snapshot: func(context.Context) (processSnapshot, error) {
		h.snapshots++
		return newProcessSnapshot(h.processes), nil
	}}
	cache := NewStateCache(fetcher, 2*time.Second)
	cache.now = func() time.Time { return h.now }
	h.p = &Provider{tm: tm, cache: cache, unixListening: func(string) (bool, error) { return true, nil }, logf: func(string, ...any) {}}
	return h
}

// presenceLines is a presence listing's output for names, each line ended as
// presenceListingFormat ends it.
func presenceLines(names ...string) string {
	lines := make([]string, len(names))
	for i, name := range names {
		lines[i] = name + "\t" + runtimeInventoryEnd
	}
	return strings.Join(lines, "\n")
}

// presenceListings counts the presence-only listings (list-panes -a in
// presenceListingFormat) among the executor's calls.
func (h *presenceHarness) presenceListings() int {
	n := 0
	for _, call := range h.fe.calls {
		if call[len(call)-1] == presenceListingFormat {
			n++
		}
	}
	return n
}

// A fresh read of a name the server does not hold is proven absent by a
// presence-only listing taken at or after since: no process-table scan runs
// (the scan only refines sessions a listing holds), and the shared cache is
// neither refreshed nor published.
// Kills: refreshing the cache (and scanning the OS process table) to prove an
// absence; publishing the presence listing into the shared snapshot.
func TestFreshReadAbsentNameRunsNoProcessSnapshot(t *testing.T) {
	h := newPresenceHarness(t)
	h.fe.outs = []string{presenceLines("worker-1", "worker-1", "worker-2")}
	got, err := h.p.ObserveLivenessSince("worker-9", []string{"codex"}, h.now)
	requireLiveness(t, got, err, runtime.Liveness{})
	if h.snapshots != 0 {
		t.Fatalf("process snapshots = %d, want 0 for an absent name", h.snapshots)
	}
	if len(h.fe.calls) != 1 || h.presenceListings() != 1 {
		t.Fatalf("tmux calls = %q, want one presence-only list-panes", h.fe.calls)
	}
	if obs := h.p.cache.observation(); obs.primed() || h.p.cache.generation != 0 {
		t.Fatalf("cache primed=%v generation=%d, want the shared snapshot untouched", obs.primed(), h.p.cache.generation)
	}

	// A server that answers with no sessions holds no name either.
	h.fe.calls, h.fe.outs, h.fe.idx = nil, nil, 0
	h.fe.err = ErrNoCurrentTarget
	got, err = h.p.ObserveLivenessSince("worker-9", nil, h.now)
	requireLiveness(t, got, err, runtime.Liveness{})
	if h.snapshots != 0 || len(h.fe.calls) != 1 {
		t.Fatalf("empty server: snapshots=%d calls=%q, want 0 and one listing", h.snapshots, h.fe.calls)
	}
}

// A fresh read of a name the presence listing holds takes today's full
// refresh, whose process-table scan is read: the agent process under the pane
// decides alive versus zombie.
// Kills: answering a present name from the presence listing; skipping the
// process snapshot for it.
func TestFreshReadPresentNameTakesFullRefreshWithProcesses(t *testing.T) {
	h := newPresenceHarness(t)
	h.fe.outs = []string{
		presenceLines("worker-1"),
		"worker-1\t0\tbash\t101\t0\t1000\t$3\t800",
	}
	h.processes = []processRuntimeState{
		{PID: "101", PPID: "1", Command: "bash", Args: "bash"},
		{PID: "102", PPID: "101", Command: "codex", Args: "codex"},
	}
	got, err := h.p.ObserveLivenessSince("worker-1", []string{"codex"}, h.now)
	requireLiveness(t, got, err, runtime.Liveness{Running: true, Alive: true, ObjectID: "$3", ObjectCreated: "800", PanePID: "101"})
	if h.snapshots != 1 {
		t.Fatalf("process snapshots = %d, want 1 (the full refresh reads processes)", h.snapshots)
	}
	if len(h.fe.calls) != 2 || h.presenceListings() != 1 {
		t.Fatalf("tmux calls = %q, want the presence listing then the full list-panes", h.fe.calls)
	}

	// The agent gone from under the pane reads as a zombie from the same
	// full refresh, not as alive.
	h.now = h.now.Add(time.Second)
	h.fe.calls, h.fe.idx = nil, 0
	h.processes = h.processes[:1]
	got, err = h.p.ObserveLivenessSince("worker-1", []string{"codex"}, h.now)
	requireLiveness(t, got, err, runtime.Liveness{Running: true, ObjectID: "$3", ObjectCreated: "800", PanePID: "101"})
	if h.snapshots != 2 {
		t.Fatalf("process snapshots = %d, want 2", h.snapshots)
	}
}

// Only a listing taken at or after since is evidence. A published snapshot
// older than since that lacks the name proves nothing: the presence listing
// taken now holds it, and the read answers from a full refresh. A since the
// cache clock has not reached yet is never answered from a presence listing,
// which would start before it.
// Kills: answering absent from a snapshot fetched before since; a presence
// listing used when it starts before since.
func TestFreshReadNeverUsesAListingTakenBeforeSince(t *testing.T) {
	h := newPresenceHarness(t)
	// Publish a snapshot without worker-1.
	h.fe.outs = []string{"other\t0\tbash\t201\t0\t1000\t$1\t700"}
	if _, err := h.p.ObserveLivenessWithError("other", nil); err != nil || !h.p.cache.observation().primed() {
		t.Fatalf("priming the cache: %v", err)
	}
	h.now = h.now.Add(time.Second) // inside the TTL: the old snapshot is still a cache hit
	h.fe.calls, h.fe.idx = nil, 0
	h.fe.outs = []string{
		presenceLines("other", "worker-1"),
		"other\t0\tbash\t201\t0\t1000\t$1\t700\nworker-1\t0\tbash\t101\t0\t1000\t$3\t800",
	}
	h.processes = []processRuntimeState{{PID: "101", PPID: "1", Command: "bash", Args: "bash"}, {PID: "102", PPID: "101", Command: "codex", Args: "codex"}}
	got, err := h.p.ObserveLivenessSince("worker-1", []string{"codex"}, h.now)
	requireLiveness(t, got, err, runtime.Liveness{Running: true, Alive: true, ObjectID: "$3", ObjectCreated: "800", PanePID: "101"})
	if h.presenceListings() != 1 {
		t.Fatalf("tmux calls = %q, want a presence listing taken now", h.fe.calls)
	}

	// since ahead of the cache clock: no listing can start at or after it.
	h.fe.calls, h.fe.idx, h.fe.outs = nil, 0, nil
	h.fe.out = "other\t0\tbash\t201\t0\t1000\t$1\t700"
	got, err = h.p.ObserveLivenessSince("worker-9", nil, h.now.Add(time.Minute))
	requireLivenessUnknown(t, got, err, nil)
	if h.presenceListings() != 0 {
		t.Fatalf("tmux calls = %q, want no presence listing for a since ahead of the clock", h.fe.calls)
	}
}

// A presence listing that fails proves nothing: the read takes today's full
// refresh and answers as it does today. A refresh error is unknown; a missing
// server is unknown while its socket is not confirmed dead.
// Kills: reading a failed listing as an empty one; answering a no-server
// listing absent.
func TestFreshReadFailedPresenceListingTakesFullRefresh(t *testing.T) {
	h := newPresenceHarness(t)
	h.fe.errs = []error{errors.New("tmux list-panes: signal: killed"), nil}
	h.fe.outs = []string{"", "worker-1\t0\tbash\t101\t0\t1000\t$3\t800"}
	got, err := h.p.ObserveLivenessSince("worker-9", nil, h.now)
	requireLiveness(t, got, err, runtime.Liveness{})
	if h.snapshots != 1 || len(h.fe.calls) != 2 {
		t.Fatalf("snapshots=%d calls=%q, want the full refresh after the failed listing", h.snapshots, h.fe.calls)
	}

	h.now = h.now.Add(time.Minute)
	h.fe.calls, h.fe.idx, h.fe.outs, h.fe.errs = nil, 0, nil, nil
	h.fe.err = ErrNoServer
	got, err = h.p.ObserveLivenessSince("worker-9", nil, h.now)
	requireLivenessUnknown(t, got, err, ErrNoServer)
	if h.presenceListings() != 1 {
		t.Fatalf("tmux calls = %q, want one presence listing before the full refresh", h.fe.calls)
	}
}

// Only a well-formed listing is evidence, and only for a name gc could have
// created: a line without the end marker, or a name validateSessionName
// rejects (whose trailing whitespace the executor's trim could cut from a
// bare name listing), takes today's full refresh.
// Kills: parsing a listing whose lines lack the marker; answering a name gc
// cannot create from the presence listing.
func TestFreshReadPresenceListingOnlyForWellFormedValidNames(t *testing.T) {
	h := newPresenceHarness(t)
	h.fe.outs = []string{"worker-1", "worker-1\t0\tbash\t101\t0\t1000\t$3\t800"}
	got, err := h.p.ObserveLivenessSince("worker-9", nil, h.now)
	requireLiveness(t, got, err, runtime.Liveness{})
	if h.snapshots != 1 || len(h.fe.calls) != 2 {
		t.Fatalf("malformed listing: snapshots=%d calls=%q, want the full refresh", h.snapshots, h.fe.calls)
	}

	h.now = h.now.Add(time.Minute)
	h.fe.calls, h.fe.idx = nil, 0
	h.fe.outs = []string{"zz-trail \t0\tbash\t101\t0\t1000\t$3\t800"}
	got, err = h.p.ObserveLivenessSince("zz-trail ", nil, h.now)
	requireLiveness(t, got, err, runtime.Liveness{Running: true, Alive: true, ObjectID: "$3", ObjectCreated: "800", PanePID: "101"})
	if h.presenceListings() != 0 {
		t.Fatalf("tmux calls = %q, want no presence listing for a name gc cannot create", h.fe.calls)
	}
}

// deadlineExecutor is a fakeExecutor that records each call's context
// deadline.
type deadlineExecutor struct {
	fakeExecutor
	deadlines []time.Duration
}

func (d *deadlineExecutor) executeCtx(ctx context.Context, args []string) (string, error) {
	if dl, ok := ctx.Deadline(); ok {
		d.deadlines = append(d.deadlines, time.Until(dl))
	}
	return d.execute(args)
}

// The presence listing runs under its own short bound, so a wedged server
// costs a fresh read at most presenceListingTimeout more than the full refresh
// alone did.
// Kills: running the listing under fetchTimeout or with no bound.
func TestFreshReadPresenceListingHasItsOwnShortBound(t *testing.T) {
	h := newPresenceHarness(t)
	de := &deadlineExecutor{fakeExecutor: fakeExecutor{out: presenceLines("worker-1")}}
	h.p.tm.exec = de
	got, err := h.p.ObserveLivenessSince("worker-9", nil, h.now)
	requireLiveness(t, got, err, runtime.Liveness{})
	if len(de.deadlines) != 1 || de.deadlines[0] > presenceListingTimeout || de.deadlines[0] <= 0 {
		t.Fatalf("listing deadlines = %v, want one within %v", de.deadlines, presenceListingTimeout)
	}
}
