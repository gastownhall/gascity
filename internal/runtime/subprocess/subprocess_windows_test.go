//go:build windows

package subprocess

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/platform/localsocket"
	"github.com/gastownhall/gascity/internal/processgroup"
	"github.com/gastownhall/gascity/internal/runtime"
	"golang.org/x/sys/windows"
)

// TestPrivateFallbackRootIsAnOwnedDirectory pins the defect that put the
// subprocess control plane outside everything Gas City owns. The root used to
// be the literal "/tmp", and filepath.Join makes that drive-relative on
// Windows: \tmp on whichever volume the process happens to be running from, a
// directory gc never creates, never protects, and may not be able to write.
func TestPrivateFallbackRootIsAnOwnedDirectory(t *testing.T) {
	root := privateFallbackRoot(os.Geteuid())

	if volume := filepath.VolumeName(root); volume == "" {
		t.Fatalf("privateFallbackRoot = %q, want a rooted path with a volume, not a drive-relative one", root)
	}
	if !filepath.IsAbs(root) {
		t.Fatalf("privateFallbackRoot = %q, want an absolute path", root)
	}
	temp, err := filepath.Abs(os.TempDir())
	if err != nil {
		t.Fatalf("resolving os.TempDir: %v", err)
	}
	if !strings.HasPrefix(root, temp) {
		t.Fatalf("privateFallbackRoot = %q, want it under the per-user temp dir %q", root, temp)
	}
	// The root has to be creatable and protectable, which is the property
	// \tmp on a foreign volume could not offer.
	leaf := filepath.Join(root, fmt.Sprintf("probe-%d", os.Getpid()))
	if err := runtime.EnsurePrivateDir(leaf); err != nil {
		t.Fatalf("EnsurePrivateDir(%q): %v", leaf, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(leaf) })
}

// TestPrivateFallbackRootCarriesNoEUIDSentinel holds that the -1 os.Geteuid
// reports for every Windows process never reaches a pathname. A sentinel in a
// directory name reads as an identity and is not one.
func TestPrivateFallbackRootCarriesNoEUIDSentinel(t *testing.T) {
	if got := os.Geteuid(); got != -1 {
		t.Skipf("os.Geteuid() = %d; this test pins the -1 Windows reports", got)
	}
	root := privateFallbackRoot(os.Geteuid())
	if strings.Contains(filepath.Base(root), "-1") {
		t.Fatalf("privateFallbackRoot = %q, want no euid sentinel in the name", root)
	}
	// Every euid resolves to the same root here, because the separation an
	// euid buys on a shared /tmp is already supplied by the per-user temp dir.
	if privateFallbackRoot(0) != privateFallbackRoot(-1) {
		t.Fatal("the Windows fallback root varies with a value the platform does not report")
	}
}

// TestPrivateFallbackRootIsDistinctFromTheLegacyFallback guards the collision
// that dropping the euid from the name could otherwise introduce. The legacy
// fallback is ordinary shared state; the private one is created and verified
// owner-only, and one path serving both would apply an owner-only DACL to the
// shared directory.
func TestPrivateFallbackRootIsDistinctFromTheLegacyFallback(t *testing.T) {
	legacy := filepath.Join(os.TempDir(), fallbackSocketDirName)
	private := privateFallbackRoot(os.Geteuid())
	if private == legacy {
		t.Fatalf("private fallback root %q collides with the legacy fallback root", private)
	}
	if strings.HasPrefix(private, legacy+string(os.PathSeparator)) {
		t.Fatalf("private fallback root %q sits inside the legacy fallback root %q", private, legacy)
	}
}

// TestSocketPathLimitComesFromLocalsocket holds the budget to one owner. Two
// private copies of this number are how the cmd/gc endpoints and this provider
// drifted apart.
func TestSocketPathLimitComesFromLocalsocket(t *testing.T) {
	if socketPathLimit != localsocket.PathLimit {
		t.Fatalf("socketPathLimit = %d, want localsocket.PathLimit = %d", socketPathLimit, localsocket.PathLimit)
	}
	if socketPathLimit > nativeSocketPathLimit {
		t.Fatalf("policy limit %d exceeds the platform's %d", socketPathLimit, nativeSocketPathLimit)
	}
}

// TestUnavailableSocketErrorsCoverWindowsDialFailures measures how this
// platform reports "nothing is listening here" instead of asserting it, the
// same way localsocket settled the transport question.
//
// It also records why the classification had to be platform-split: Go's
// syscall.ECONNREFUSED on Windows is a synthetic value no Winsock call
// returns, so the portable predicate matched none of these and Stop was not
// idempotent.
func TestUnavailableSocketErrorsCoverWindowsDialFailures(t *testing.T) {
	// A t.TempDir() named after this test overruns the platform's 107-byte
	// endpoint ceiling on its own, and connect then reports EINVAL for the
	// pathname rather than anything about the endpoint. The fixture has to fit
	// the budget for the measurement to be about what is being measured.
	root := shortTempDir(t)
	regular := filepath.Join(root, "plain.txt")
	if err := os.WriteFile(regular, []byte("not a socket"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cases := []struct {
		name string
		path string
	}{
		{name: "socket file absent", path: filepath.Join(root, "absent.sock")},
		{name: "parent directory absent", path: filepath.Join(root, "absent-dir", "s.sock")},
		{name: "name is a regular file", path: regular},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := net.DialTimeout("unix", tc.path, time.Second)
			if err == nil {
				conn.Close() //nolint:errcheck
				t.Fatalf("dial %q succeeded; the fixture is not an unavailable endpoint", tc.path)
			}
			var errno syscall.Errno
			if !errors.As(err, &errno) {
				t.Fatalf("dial error %v carries no syscall.Errno", err)
			}
			if errors.Is(err, syscall.EINVAL) {
				t.Fatalf("dial %q reported EINVAL; the fixture pathname is %d bytes and overran the platform ceiling of %d", tc.path, len(tc.path), nativeSocketPathLimit)
			}
			if errno != wsaeConnRefused && errno != wsaeNetDown {
				t.Fatalf("dial %q reported errno %d (%v); the constants in socketerr_windows.go no longer describe this platform", tc.path, uint(errno), err)
			}
			if errors.Is(err, syscall.ECONNREFUSED) {
				t.Fatal("syscall.ECONNREFUSED matched; the portable predicate would have been sufficient and this split can be deleted")
			}
			if !isUnavailableSocketError(err) {
				t.Fatalf("isUnavailableSocketError(%v) = false; Stop and Interrupt would report a gone session as a failure", err)
			}
		})
	}
}

// TestUnavailableSocketErrorStillSurfacesRealFailures holds the other half of
// the contract: the predicate was widened to cover this platform's dial
// errors, not switched off. A fault that is not "the endpoint is absent" must
// still reach the caller.
func TestUnavailableSocketErrorStillSurfacesRealFailures(t *testing.T) {
	for _, err := range []error{
		errors.New("unexpected response from socket"),
		syscall.EINVAL,
		os.ErrPermission,
		errPrivateSocketDirValidation,
	} {
		if isUnavailableSocketError(err) {
			t.Errorf("isUnavailableSocketError(%v) = true; a real failure would be reported as a stopped session", err)
		}
	}
}

// TestStartSpawnsAChildAndServesItsControlSocket is the provider-level form of
// the defect-2 verification: a child really runs, the control socket really
// answers, and Stop really tears it down.
func TestStartSpawnsAChildAndServesItsControlSocket(t *testing.T) {
	p := newTestProvider(t)
	const name = "windows-child-proof"
	marker := filepath.Join(t.TempDir(), "child-pid.txt")
	command := fmt.Sprintf(`[System.IO.File]::WriteAllText(%s, [string]$PID + [char]10); %s`,
		psLiteral(marker), cmdSleepLong)

	if err := p.Start(context.Background(), name, runtime.Config{Command: command}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = p.Stop(name)
		}
	})

	// The marker proves a real process ran the command, which a socket that
	// merely exists would not.
	deadline := time.Now().Add(30 * time.Second)
	var childPID string
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(marker)
		if err == nil && strings.TrimSpace(string(data)) != "" {
			childPID = strings.TrimSpace(string(data))
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if childPID == "" {
		t.Fatal("no child process wrote the marker; the session spawned nothing")
	}
	if childPID == fmt.Sprint(os.Getpid()) {
		t.Fatalf("marker names the test process (pid %s); the command did not run in a child", childPID)
	}

	if !p.IsRunning(name) {
		t.Fatal("IsRunning = false while the child is alive")
	}
	if got := requestControlSocket(t, p.sockPath(name), "ping"); got != "ok" {
		t.Fatalf("control socket ping = %q, want ok", got)
	}
	if got := requestControlSocket(t, p.sockPath(name), "pid"); got != childPID {
		t.Fatalf("control socket pid = %q, want the session's own child pid %q", got, childPID)
	}

	if err := p.Stop(name); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	stopped = true
	if p.IsRunning(name) {
		t.Fatal("IsRunning = true after Stop")
	}
	if err := p.Stop(name); err != nil {
		t.Fatalf("second Stop = %v, want nil; Stop must stay idempotent once the socket is gone", err)
	}
}

// TestSessionExitReleasesProcessOwnership pins the wiring that makes releasing
// a session's process tree reachable at all.
//
// The Job Object handle that owns a session's tree is held by internal/
// processgroup until something releases it, and for a long time nothing did:
// ReleaseCommand had no production caller. Every session ever started left a
// handle and a map entry behind for the life of the process, and the reap a
// kill-on-release tree is promised never happened until the launcher itself
// exited — the moment it is least expected.
func TestSessionExitReleasesProcessOwnership(t *testing.T) {
	p := newTestProvider(t)
	const name = "windows-ownership-release"

	if err := p.Start(context.Background(), name, runtime.Config{Command: "exit 0"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })

	p.mu.Lock()
	sc := p.procs[name]
	p.mu.Unlock()
	if sc == nil {
		t.Fatal("Start did not record the session")
	}
	if processgroup.CommandGroupID(sc.cmd) == 0 {
		t.Fatal("Start did not take ownership of the session tree; the rest of this test would pass vacuously")
	}

	select {
	case <-sc.done:
	case <-time.After(30 * time.Second):
		t.Fatal("session never exited")
	}

	deadline := time.Now().Add(10 * time.Second)
	for processgroup.CommandGroupID(sc.cmd) != 0 {
		if !time.Now().Before(deadline) {
			t.Fatal("the session's tree is still owned after the session exited; its job handle and map entry leak for the life of the process")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func requestControlSocket(t *testing.T, path, command string) string {
	t.Helper()
	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		t.Fatalf("dial control socket %q: %v", path, err)
	}
	defer conn.Close()                                //nolint:errcheck
	conn.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
	if _, err := fmt.Fprintf(conn, "%s\n", command); err != nil {
		t.Fatalf("writing %q to control socket: %v", command, err)
	}
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatalf("control socket returned nothing for %q: %v", command, scanner.Err())
	}
	return strings.TrimSpace(scanner.Text())
}

// startSessionThatLeavesAHelper starts a session whose command launches a
// long-lived helper, records the helper's PID, and exits. The helper stays in
// the session's job, so the tree outlives its leader. The helper is returned
// held, so whatever the test does through the handle reaches that process and
// no later holder of its PID; cleanup terminates it through the handle.
func startSessionThatLeavesAHelper(t *testing.T, p *Provider, name string) (windows.Handle, *sessionConn) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "helper-pid.txt")
	command := fmt.Sprintf(`$h = Start-Process -FilePath ping.exe -ArgumentList '-n','61','127.0.0.1' -NoNewWindow -PassThru; [System.IO.File]::WriteAllText(%s, [string]$h.Id); exit 0`,
		psLiteral(marker))
	started := time.Now()
	if err := p.Start(context.Background(), name, runtime.Config{Command: command}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	p.mu.Lock()
	sc := p.procs[name]
	p.mu.Unlock()
	if sc == nil {
		t.Fatal("Start did not record the session")
	}

	var pid uint32
	deadline := time.Now().Add(30 * time.Second)
	for pid == 0 && time.Now().Before(deadline) {
		if data, err := os.ReadFile(marker); err == nil {
			if value, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 32); err == nil {
				pid = uint32(value)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("the session never recorded its helper's PID")
	}
	helper, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		t.Fatalf("opening helper PID %d: %v", pid, err)
	}
	t.Cleanup(func() {
		_ = windows.TerminateProcess(helper, 1)
		_ = windows.CloseHandle(helper)
	})
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(helper, &creation, &exit, &kernel, &user); err != nil || time.Unix(0, creation.Nanoseconds()).Before(started.Add(-time.Second)) {
		t.Fatalf("PID %d is not the helper this session started (created %v, session started %v, %v)", pid, time.Unix(0, creation.Nanoseconds()), started, err)
	}

	select {
	case <-sc.done:
	case <-time.After(30 * time.Second):
		t.Fatal("the session's leader never exited")
	}
	if helperExited(helper, 0) {
		t.Fatal("the helper exited with its leader; the rest of this test would pass vacuously")
	}
	if !processgroup.HoldsTree(sc.cmd) {
		t.Fatal("the session's tree was released at leader exit although its helper is still running")
	}
	return helper, sc
}

func helperExited(handle windows.Handle, timeout time.Duration) bool {
	event, err := windows.WaitForSingleObject(handle, uint32(timeout.Milliseconds()))
	return err == nil && event == windows.WAIT_OBJECT_0
}

// TestStopReachesAHelperItsLeaderLeftBehind is the provider-level half of the
// false clear the job-retaining reaper was meant to close. The reaper keeps a
// tree whose leader exited with members left, but Stop answered "not alive" and
// returned nil without ever reaching it.
func TestStopReachesAHelperItsLeaderLeftBehind(t *testing.T) {
	p := newTestProvider(t)
	const name = "windows-leader-exit-stop"
	helper, _ := startSessionThatLeavesAHelper(t, p, name)

	if err := p.Stop(name); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !helperExited(helper, 5*time.Second) {
		t.Fatal("Stop returned nil while the helper the session's leader left behind was still running")
	}
}

// TestStartReapsAHelperThePreviousSessionLeftBehind is the other half: a new
// session under the same name must not start beside the old one's survivors.
func TestStartReapsAHelperThePreviousSessionLeftBehind(t *testing.T) {
	p := newTestProvider(t)
	const name = "windows-leader-exit-restart"
	helper, _ := startSessionThatLeavesAHelper(t, p, name)

	if err := p.Start(context.Background(), name, runtime.Config{Command: cmdSleepLong}); err != nil {
		t.Fatalf("restarting %q: %v", name, err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })
	if !helperExited(helper, 5*time.Second) {
		t.Fatal("a replacement session started beside the helper the previous session left running")
	}
}

// racingStopTeardown runs call from inside Stop, once Stop has taken the
// session and before it tears the tree down, and reports what call returned and
// whether the helper was still running when it did. A call that does not wait
// for the stop returns during the hook's pause, with the helper untouched; one
// that waits returns once the stop has confirmed the tree empty, which the
// helper's handle reflects within moments, so the check allows it those.
func racingStopTeardown(t *testing.T, p *Provider, name string, helper windows.Handle, call func() error) (error, bool) {
	t.Helper()
	type outcome struct {
		err         error
		helperAlive bool
	}
	result := make(chan outcome, 1)
	testHookBeforeStopTeardown = func(string) {
		testHookBeforeStopTeardown = nil
		go func() {
			err := call()
			result <- outcome{err: err, helperAlive: !helperExited(helper, 250*time.Millisecond)}
		}()
		time.Sleep(time.Second)
	}
	t.Cleanup(func() { testHookBeforeStopTeardown = nil })

	if err := p.Stop(name); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case got := <-result:
		return got.err, got.helperAlive
	case <-time.After(stopWaitTimeout + 10*time.Second):
		t.Fatal("the racing call never returned")
		return nil, false
	}
}

// TestStartWaitsForAStopStillTearingDownTheTree is the replacement that ran
// beside a helper a Stop was still taking down. Stop took
// the name out of tracking before its teardown, and with the leader gone its
// control socket was gone too, so a Start in that window found nothing to wait
// for.
func TestStartWaitsForAStopStillTearingDownTheTree(t *testing.T) {
	p := newTestProvider(t)
	const name = "windows-stop-then-start"
	helper, _ := startSessionThatLeavesAHelper(t, p, name)

	err, helperAlive := racingStopTeardown(t, p, name, helper, func() error {
		return p.Start(context.Background(), name, runtime.Config{Command: cmdSleepLong})
	})
	t.Cleanup(func() { _ = p.Stop(name) })
	if err != nil {
		t.Fatalf("the replacement Start = %v, want it to start once the stop had finished", err)
	}
	if helperAlive {
		t.Fatal("the replacement started while the previous session's helper was still running")
	}
}

// TestStopWaitsForAStopAlreadyTearingDownTheTree keeps a second Stop from
// reporting success while the first is still confirming the tree gone.
func TestStopWaitsForAStopAlreadyTearingDownTheTree(t *testing.T) {
	p := newTestProvider(t)
	const name = "windows-stop-twice"
	helper, _ := startSessionThatLeavesAHelper(t, p, name)

	err, helperAlive := racingStopTeardown(t, p, name, helper, func() error { return p.Stop(name) })
	if err != nil {
		t.Fatalf("the second Stop = %v", err)
	}
	if helperAlive {
		t.Fatal("a second Stop returned while the first was still taking the helper down")
	}
}
