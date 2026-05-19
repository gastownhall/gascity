package subprocess

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// shortTempDir returns a temp directory short enough for Unix socket paths
// (macOS limit is 104 bytes). t.TempDir() paths often exceed this.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gc-t-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func newTestProvider(t *testing.T) *Provider {
	t.Helper()
	return NewProviderWithDir(filepath.Join(shortTempDir(t), "socks"))
}

func TestStartCreatesProcess(t *testing.T) {
	p := newTestProvider(t)
	err := p.Start(context.Background(), "test", runtime.Config{Command: "sleep 3600"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop("test") //nolint:errcheck

	if !p.IsRunning("test") {
		t.Error("expected IsRunning=true after Start")
	}
}

func TestStartPersistsRuntimeMetadataForGetMeta(t *testing.T) {
	p := newTestProvider(t)
	err := p.Start(context.Background(), "meta-start", runtime.Config{
		Command: "sleep 3600",
		Env: map[string]string{
			"GC_SESSION_ID":     "bead-123",
			"GC_INSTANCE_TOKEN": "token-456",
			"GC_TEMPLATE":       "worker",
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop("meta-start") //nolint:errcheck

	for key, want := range map[string]string{
		"GC_SESSION_ID":     "bead-123",
		"GC_INSTANCE_TOKEN": "token-456",
		"GC_TEMPLATE":       "worker",
	} {
		got, err := p.GetMeta("meta-start", key)
		if err != nil {
			t.Fatalf("GetMeta(%s): %v", key, err)
		}
		if got != want {
			t.Fatalf("GetMeta(%s) = %q, want %q", key, got, want)
		}
	}
}

func TestStartLongSocketPathUsesShortSocketName(t *testing.T) {
	// Use /tmp for a short base path — TMPDIR on macOS (/var/folders/...)
	// is too long to find a depth where legacy > limit but short < limit.
	root, err := os.MkdirTemp("/tmp", "gc-sock-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	const name = "control-dispatcher"
	// macOS socket path limit is 104 bytes; Linux is 108.
	const sunPathLimit = 104
	longDir := ""
	for i := 1; i <= 200; i++ {
		// Use single-char increments so the 10-char gap between legacy
		// and short socket names can straddle the sun_path limit.
		candidate := filepath.Join(root, strings.Repeat("p", i), "socks")
		p := NewProviderWithDir(candidate)
		if len(p.legacySockPath(name)) > sunPathLimit && len(p.sockPath(name)) < sunPathLimit {
			longDir = candidate
			break
		}
	}
	if longDir == "" {
		t.Fatal("failed to construct path where legacy socket is too long but short socket fits")
	}
	if err := os.MkdirAll(longDir, 0o755); err != nil {
		t.Fatalf("mkdir longDir: %v", err)
	}

	p := NewProviderWithDir(longDir)
	if err := p.Start(context.Background(), name, runtime.Config{Command: "sleep 3600"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop(name) //nolint:errcheck

	if _, err := os.Stat(p.sockPath(name)); err != nil {
		t.Fatalf("short socket path missing: %v", err)
	}
	if got, want := filepath.Base(p.sockPath(name)), name+".sock"; got == want {
		t.Fatalf("socket filename = %q, want shortened hashed filename", got)
	}
	if len(p.sockPath(name)) >= len(p.legacySockPath(name)) {
		t.Fatalf("short socket path = %q, legacy = %q; want shorter path", p.sockPath(name), p.legacySockPath(name))
	}
}

func TestStartVeryLongSocketDirFallsBackToTempDir(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "gc-sock-fallback-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	longDir := filepath.Join(root, strings.Repeat("p", 120), "runtime", "gc", "subprocess", "hash")
	if err := os.MkdirAll(longDir, 0o755); err != nil {
		t.Fatalf("mkdir longDir: %v", err)
	}

	p := NewProviderWithDir(longDir)
	name := "dog-gc-112"
	localShort := filepath.Join(longDir, p.sockKey(name)+".sock")
	if len(localShort) <= socketPathLimit {
		t.Fatalf("test setup failed: %q does not exceed socket path limit", localShort)
	}
	if !strings.HasPrefix(p.sockPath(name), os.TempDir()) {
		t.Fatalf("sockPath(%q) = %q, want temp-dir fallback", name, p.sockPath(name))
	}
	if len(p.sockPath(name)) > socketPathLimit {
		t.Fatalf("sockPath(%q) = %q exceeds limit %d", name, p.sockPath(name), socketPathLimit)
	}

	if err := p.Start(context.Background(), name, runtime.Config{Command: "sleep 3600"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop(name) //nolint:errcheck

	p2 := NewProviderWithDir(longDir)
	if !p2.socketAlive(name) {
		t.Fatalf("fallback socket for %q should be visible cross-process", name)
	}
	got, err := p2.ListRunning("")
	if err != nil {
		t.Fatalf("ListRunning: %v", err)
	}
	if len(got) != 1 || got[0] != name {
		t.Fatalf("ListRunning = %#v, want [%q]", got, name)
	}
}

func TestStartDuplicateNameFails(t *testing.T) {
	p := newTestProvider(t)
	if err := p.Start(context.Background(), "dup", runtime.Config{Command: "sleep 3600"}); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	defer p.Stop("dup") //nolint:errcheck

	err := p.Start(context.Background(), "dup", runtime.Config{Command: "sleep 3600"})
	if err == nil {
		t.Fatal("expected error for duplicate name")
	}
}

func TestStartReusesDeadName(t *testing.T) {
	p := newTestProvider(t)
	if err := p.Start(context.Background(), "reuse", runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if p.IsRunning("reuse") {
		t.Fatal("expected process to have exited")
	}

	if err := p.Start(context.Background(), "reuse", runtime.Config{Command: "sleep 3600"}); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	defer p.Stop("reuse") //nolint:errcheck

	if !p.IsRunning("reuse") {
		t.Error("expected IsRunning=true after reuse")
	}
}

func TestStopKillsProcess(t *testing.T) {
	p := newTestProvider(t)
	if err := p.Start(context.Background(), "kill", runtime.Config{Command: "sleep 3600"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Stop("kill"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if p.IsRunning("kill") {
		t.Error("expected IsRunning=false after Stop")
	}
}

func TestStopKillsProcessGroupDescendants(t *testing.T) {
	dir := t.TempDir()
	childPIDPath := filepath.Join(dir, "child.pid")

	p := newTestProvider(t)
	if err := p.Start(context.Background(), "group-kill", runtime.Config{
		Command: "sleep 3600 & echo $! > " + childPIDPath + "; wait",
		WorkDir: dir,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var childPID int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(childPIDPath)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil && pid > 0 {
				childPID = pid
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("timed out waiting for child PID marker")
	}

	if err := p.Stop("group-kill"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(childPID, syscall.Signal(0)); err != nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("child process %d still alive after Stop killed the parent session", childPID)
}

func TestStopIdempotent(t *testing.T) {
	p := newTestProvider(t)
	if err := p.Stop("nonexistent"); err != nil {
		t.Errorf("Stop(nonexistent) = %v, want nil", err)
	}
}

func TestStopDeadProcess(t *testing.T) {
	p := newTestProvider(t)
	if err := p.Start(context.Background(), "dead", runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	if err := p.Stop("dead"); err != nil {
		t.Errorf("Stop(dead) = %v, want nil", err)
	}
}

func TestIsRunningFalseAfterExit(t *testing.T) {
	p := newTestProvider(t)
	if err := p.Start(context.Background(), "short", runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	if p.IsRunning("short") {
		t.Error("expected IsRunning=false after process exits")
	}
}

func TestIsRunningFalseForUnknown(t *testing.T) {
	p := newTestProvider(t)
	if p.IsRunning("unknown") {
		t.Error("expected IsRunning=false for unknown session")
	}
}

func TestAttachReturnsError(t *testing.T) {
	p := newTestProvider(t)
	if err := p.Attach("anything"); err == nil {
		t.Error("expected Attach to return error")
	}
}

func TestEnvPassedToProcess(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "env.txt")

	p := newTestProvider(t)
	err := p.Start(context.Background(), "env-test", runtime.Config{
		Command: "echo $GC_TEST_VAR > " + marker,
		Env:     map[string]string{"GC_TEST_VAR": "hello-from-subprocess"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop("env-test") //nolint:errcheck

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(marker)
		if err == nil && len(data) > 0 {
			got := string(data)
			if got != "hello-from-subprocess\n" {
				t.Errorf("env var = %q, want %q", got, "hello-from-subprocess\n")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timed out waiting for env marker file")
}

func TestWorkDirSet(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "pwd.txt")

	p := newTestProvider(t)
	err := p.Start(context.Background(), "workdir-test", runtime.Config{
		Command: "pwd > " + marker,
		WorkDir: dir,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop("workdir-test") //nolint:errcheck

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(marker)
		if err == nil && len(data) > 0 {
			got := string(data)
			// Canonicalize to handle macOS /var → /private/var symlink.
			canonical, _ := filepath.EvalSymlinks(dir)
			want := canonical + "\n"
			if got != want {
				t.Errorf("workdir = %q, want %q", got, want)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timed out waiting for workdir marker file")
}

func TestStartStagesSingleFileCopyIntoWorkDirRoot(t *testing.T) {
	workDir := t.TempDir()
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "seed.txt")
	if err := os.WriteFile(src, []byte("seed data"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	p := newTestProvider(t)
	err := p.Start(context.Background(), "copy-root", runtime.Config{
		Command:   "sleep 3600",
		WorkDir:   workDir,
		CopyFiles: []runtime.CopyEntry{{Src: src}},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop("copy-root") //nolint:errcheck

	data, err := os.ReadFile(filepath.Join(workDir, "seed.txt"))
	if err != nil {
		t.Fatalf("read staged file: %v", err)
	}
	if string(data) != "seed data" {
		t.Fatalf("staged file = %q, want %q", string(data), "seed data")
	}
}

func TestStartFailsWhenCopyFileCannotBeStaged(t *testing.T) {
	workDir := t.TempDir()
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "seed.txt")
	if err := os.WriteFile(src, []byte("seed data"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	blocker := filepath.Join(workDir, "blocked")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	p := newTestProvider(t)
	err := p.Start(context.Background(), "copy-error", runtime.Config{
		Command: "sleep 3600",
		WorkDir: workDir,
		CopyFiles: []runtime.CopyEntry{{
			Src:    src,
			RelDst: filepath.Join("blocked", "seed.txt"),
		}},
	})
	if err == nil {
		t.Fatal("Start should fail when staging a copy file fails")
	}
}

func TestStartFailsWhenOverlayCannotBeStaged(t *testing.T) {
	workDir := t.TempDir()
	overlayDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(overlayDir, "ok.txt"), []byte("copied"), 0o644); err != nil {
		t.Fatalf("write ok overlay file: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(overlayDir, "blocked"), 0o755); err != nil {
		t.Fatalf("mkdir blocked src dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(overlayDir, "blocked", "nested.txt"), []byte("ignored"), 0o644); err != nil {
		t.Fatalf("write blocked overlay file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "blocked"), []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write blocked dst file: %v", err)
	}

	p := newTestProvider(t)
	err := p.Start(context.Background(), "overlay-error", runtime.Config{
		Command:    "sleep 3600",
		WorkDir:    workDir,
		OverlayDir: overlayDir,
	})
	if err == nil {
		t.Fatal("Start should fail when staging an overlay warns")
	}
}

func TestStartFailedStagingDoesNotRetainWorkDirForCopyTo(t *testing.T) {
	workDir := t.TempDir()
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "seed.txt")
	if err := os.WriteFile(src, []byte("seed data"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	blocker := filepath.Join(workDir, "blocked")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	p := newTestProvider(t)
	err := p.Start(context.Background(), "copy-error", runtime.Config{
		Command: "sleep 3600",
		WorkDir: workDir,
		CopyFiles: []runtime.CopyEntry{{
			Src:    src,
			RelDst: filepath.Join("blocked", "seed.txt"),
		}},
	})
	if err == nil {
		t.Fatal("Start should fail when staging a copy file fails")
	}

	lateSrc := filepath.Join(srcDir, "late.txt")
	if err := os.WriteFile(lateSrc, []byte("late data"), 0o644); err != nil {
		t.Fatalf("write late src: %v", err)
	}
	if err := p.CopyTo("copy-error", lateSrc, "late.txt"); err != nil {
		t.Fatalf("CopyTo: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "late.txt")); !os.IsNotExist(err) {
		t.Fatalf("late copy err = %v, want no copy into failed session workdir", err)
	}
}

func TestSocketCreated(t *testing.T) {
	p := newTestProvider(t)
	if err := p.Start(context.Background(), "sock-check", runtime.Config{Command: "sleep 3600"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop("sock-check") //nolint:errcheck

	if _, err := os.Stat(p.sockPath("sock-check")); err != nil {
		t.Fatalf("socket file should exist: %v", err)
	}
}

func TestSocketRemovedAfterStop(t *testing.T) {
	p := newTestProvider(t)
	if err := p.Start(context.Background(), "cleanup", runtime.Config{Command: "sleep 3600"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Stop("cleanup"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Wait a bit for the background goroutine to clean up.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(p.sockPath("cleanup")); os.IsNotExist(err) {
			return // success
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("socket file should be removed after Stop")
}

func TestStopBySocket_ReturnsErrorWhenSocketRejectsStop(t *testing.T) {
	p := newTestProvider(t)
	name := "reject-stop"

	if err := os.WriteFile(p.sockNamePath(name), []byte(name), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	lis, err := net.Listen("unix", p.sockPath(name))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = lis.Close() })

	gotCommand := make(chan string, 1)
	go func() {
		conn, acceptErr := lis.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close() //nolint:errcheck

		line, readErr := bufio.NewReader(conn).ReadString('\n')
		if readErr == nil {
			gotCommand <- strings.TrimSpace(line)
		}
		_, _ = conn.Write([]byte("nope\n"))
	}()

	err = p.stopBySocket(name)
	if err == nil {
		t.Fatal("stopBySocket succeeded, want error")
	}
	if !strings.Contains(err.Error(), "unexpected response") {
		t.Fatalf("stopBySocket error = %v, want unexpected response", err)
	}
	if got := <-gotCommand; got != "stop" {
		t.Fatalf("socket command = %q, want stop", got)
	}
	if _, statErr := os.Stat(p.sockPath(name)); statErr != nil {
		t.Fatalf("socket path err = %v, want socket preserved after failed stop", statErr)
	}
	if _, statErr := os.Stat(p.sockNamePath(name)); statErr != nil {
		t.Fatalf("socket name path err = %v, want socket name preserved after failed stop", statErr)
	}
}

func TestStopBySocket_FallsBackToLegacySocketWhenCanonicalRejectsStop(t *testing.T) {
	p := newTestProvider(t)
	name := "legacy-fallback"

	canonical, err := net.Listen("unix", p.sockPath(name))
	if err != nil {
		t.Fatalf("Listen canonical: %v", err)
	}
	t.Cleanup(func() { _ = canonical.Close() })

	legacy, err := net.Listen("unix", p.legacySockPath(name))
	if err != nil {
		t.Fatalf("Listen legacy: %v", err)
	}
	t.Cleanup(func() { _ = legacy.Close() })

	go func() {
		conn, acceptErr := canonical.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close() //nolint:errcheck

		_, _ = bufio.NewReader(conn).ReadString('\n')
		_, _ = conn.Write([]byte("nope\n"))
	}()

	gotLegacy := make(chan string, 1)
	go func() {
		conn, acceptErr := legacy.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close() //nolint:errcheck

		line, readErr := bufio.NewReader(conn).ReadString('\n')
		if readErr == nil {
			gotLegacy <- strings.TrimSpace(line)
		}
		_, _ = conn.Write([]byte("ok\n"))
	}()

	if err := p.stopBySocket(name); err != nil {
		t.Fatalf("stopBySocket error = %v, want legacy fallback success", err)
	}
	if got := <-gotLegacy; got != "stop" {
		t.Fatalf("legacy socket command = %q, want stop", got)
	}
}

func TestSocketGoneAfterProcessDeath(t *testing.T) {
	p := newTestProvider(t)
	if err := p.Start(context.Background(), "short-lived", runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for process to exit and socket cleanup.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(p.sockPath("short-lived")); os.IsNotExist(err) {
			return // success
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("socket file should be removed after process exits naturally")
}

func TestCrossProcessStopBySocket(t *testing.T) {
	// Simulate the gc start → gc stop cross-process pattern:
	// Provider 1 starts a process, Provider 2 (same dir) stops it.
	dir := filepath.Join(shortTempDir(t), "socks")

	p1 := NewProviderWithDir(dir)
	if err := p1.Start(context.Background(), "cross", runtime.Config{Command: "sleep 3600"}); err != nil {
		t.Fatalf("p1.Start: %v", err)
	}

	// Verify the process is alive via socket.
	if !p1.socketAlive("cross") {
		t.Fatal("socket should be alive")
	}

	// New provider (simulates gc stop in a separate process).
	p2 := NewProviderWithDir(dir)
	if !p2.IsRunning("cross") {
		t.Fatal("p2.IsRunning should be true via socket")
	}
	if err := p2.Stop("cross"); err != nil {
		t.Fatalf("p2.Stop: %v", err)
	}

	// Process should be dead.
	time.Sleep(200 * time.Millisecond)
	if p2.IsRunning("cross") {
		t.Error("process should be dead after cross-process Stop")
	}
}

func TestMetaPath_HashesUntrustedNameAndKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "socks")
	p := NewProviderWithDir(dir)

	path := p.metaPath("../escape", "../key")
	if filepath.Dir(path) != dir {
		t.Fatalf("metaPath escaped provider dir: %q", path)
	}
	if base := filepath.Base(path); strings.Contains(base, "..") || strings.ContainsAny(base, `/\`) {
		t.Fatalf("metaPath base = %q, want hashed file name without path tokens", base)
	}

	if err := p.SetMeta("../escape", "../key", "secret"); err != nil {
		t.Fatalf("SetMeta with untrusted tokens: %v", err)
	}
	got, err := p.GetMeta("../escape", "../key")
	if err != nil {
		t.Fatalf("GetMeta with untrusted tokens: %v", err)
	}
	if got != "secret" {
		t.Fatalf("GetMeta = %q, want secret", got)
	}
	if err := p.RemoveMeta("../escape", "../key"); err != nil {
		t.Fatalf("RemoveMeta with untrusted tokens: %v", err)
	}
}

func TestCrossProcessInterruptBySocket(t *testing.T) {
	dir := filepath.Join(shortTempDir(t), "socks")

	p1 := NewProviderWithDir(dir)
	// Use a command that traps SIGINT.
	if err := p1.Start(context.Background(), "intr", runtime.Config{Command: "sleep 3600"}); err != nil {
		t.Fatalf("p1.Start: %v", err)
	}
	defer p1.Stop("intr") //nolint:errcheck

	// Cross-process interrupt via socket.
	p2 := NewProviderWithDir(dir)
	if err := p2.Interrupt("intr"); err != nil {
		t.Fatalf("p2.Interrupt: %v", err)
	}

	// sleep may or may not die on SIGINT depending on shell;
	// just verify the interrupt was sent without error.
}

func TestIsRunningViaSocket(t *testing.T) {
	dir := filepath.Join(shortTempDir(t), "socks")

	p1 := NewProviderWithDir(dir)
	if err := p1.Start(context.Background(), "live", runtime.Config{Command: "sleep 3600"}); err != nil {
		t.Fatalf("p1.Start: %v", err)
	}
	defer p1.Stop("live") //nolint:errcheck

	// Different provider instance discovers liveness via socket.
	p2 := NewProviderWithDir(dir)
	if !p2.IsRunning("live") {
		t.Error("p2.IsRunning should be true via socket")
	}

	// Non-existent session.
	if p2.IsRunning("nonexistent") {
		t.Error("IsRunning should be false for non-existent session")
	}
}

func TestListRunningViaSocket(t *testing.T) {
	dir := filepath.Join(shortTempDir(t), "socks")

	p := NewProviderWithDir(dir)
	if err := p.Start(context.Background(), "gc-test-a", runtime.Config{Command: "sleep 3600"}); err != nil {
		t.Fatalf("Start a: %v", err)
	}
	defer p.Stop("gc-test-a") //nolint:errcheck
	if err := p.Start(context.Background(), "gc-test-b", runtime.Config{Command: "sleep 3600"}); err != nil {
		t.Fatalf("Start b: %v", err)
	}
	defer p.Stop("gc-test-b") //nolint:errcheck
	if err := p.Start(context.Background(), "other-x", runtime.Config{Command: "sleep 3600"}); err != nil {
		t.Fatalf("Start x: %v", err)
	}
	defer p.Stop("other-x") //nolint:errcheck

	names, err := p.ListRunning("gc-test-")
	if err != nil {
		t.Fatalf("ListRunning: %v", err)
	}
	if len(names) != 2 {
		t.Errorf("ListRunning(gc-test-) = %v, want 2 results", names)
	}

	all, err := p.ListRunning("")
	if err != nil {
		t.Fatalf("ListRunning all: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("ListRunning('') = %v, want 3 results", all)
	}
}
