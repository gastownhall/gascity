//go:build unix

package subprocess

// These cases turn on POSIX-only mechanisms: directory permission bits and
// st_uid ownership, the euid-keyed private fallback root, signal-based
// liveness probes, and the sun_path length limit that makes the fallback
// necessary at all. Windows expresses the same privacy guarantee through a
// DACL rather than mode arithmetic, so it is covered separately.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/testutil"
)

func requirePrivateSocketDirectory(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat(%q): %v", path, err)
	}
	if !info.IsDir() {
		t.Fatalf("%q mode = %v, want directory", path, info.Mode())
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("%q permissions = %04o, want 0700", path, got)
	}
	if special := info.Mode() & (os.ModeSetuid | os.ModeSetgid | os.ModeSticky); special != 0 {
		t.Fatalf("%q special mode bits = %v, want none", path, special)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("%q ownership metadata = %T, want *syscall.Stat_t", path, info.Sys())
	}
	if got, want := stat.Uid, uint32(os.Geteuid()); got != want {
		t.Fatalf("%q uid = %d, want %d", path, got, want)
	}
}

func ensurePrivateFallbackRootForTest(t *testing.T) string {
	t.Helper()
	root := privateFallbackRoot(os.Geteuid())
	if err := os.Mkdir(root, 0o700); err != nil && !os.IsExist(err) {
		t.Fatalf("Mkdir private fallback root: %v", err)
	}
	requirePrivateSocketDirectory(t, root)
	return root
}

func TestFallbackKeepsBindableLegacyTempPathPastConservativeLimit(t *testing.T) {
	root := shortTempDir(t)
	longDir := filepath.Join(root, strings.Repeat("p", socketPathLimit+32))
	owner := NewProviderWithDir(longDir)
	observer := NewProviderWithDir(longDir)
	const name = "bindable-legacy-fallback"

	var bindableTemp string
	for length := 1; length <= socketPathLimit; length++ {
		candidate := filepath.Join(root, strings.Repeat("t", length))
		probe := filepath.Join(candidate, fallbackSocketDirName, owner.fallbackLeaf(), owner.sockKey(name)+".sock")
		if len(probe) > socketPathLimit && len(probe) <= nativeSocketPathLimit {
			bindableTemp = candidate
			break
		}
	}
	if bindableTemp == "" {
		t.Fatalf("could not construct fallback path in (%d, %d]", socketPathLimit, nativeSocketPathLimit)
	}
	if err := os.MkdirAll(bindableTemp, 0o700); err != nil {
		t.Fatalf("MkdirAll bindable TMPDIR: %v", err)
	}
	t.Setenv("TMPDIR", bindableTemp)

	fallback := owner.fallbackDir()
	wantFallback := filepath.Join(bindableTemp, fallbackSocketDirName, owner.fallbackLeaf())
	if fallback != wantFallback {
		t.Fatalf("fallback = %q, want legacy path %q", fallback, wantFallback)
	}
	if got := len(owner.sockPath(name)); got <= socketPathLimit || got > nativeSocketPathLimit {
		t.Fatalf("legacy fallback socket length = %d, want (%d, %d]", got, socketPathLimit, nativeSocketPathLimit)
	}
	t.Cleanup(func() {
		_ = owner.Stop(name)
		_ = observer.Stop(name)
		_ = syscall.Rmdir(fallback)
	})

	if err := owner.Start(context.Background(), name, runtime.Config{Command: "sleep 300"}); err != nil {
		t.Fatalf("Start on native-addressable legacy fallback: %v", err)
	}
	if _, err := os.Lstat(owner.sockPath(name)); err != nil {
		t.Fatalf("Lstat legacy fallback socket: %v", err)
	}
	if !observer.IsRunning(name) {
		t.Fatal("native-addressable legacy fallback is not visible through another provider")
	}
	if err := observer.Stop(name); err != nil {
		t.Fatalf("cross-provider Stop on native-addressable legacy fallback: %v", err)
	}
}

func TestLegacySocketRemainsVisibleWhenPrivateFallbackIsMissing(t *testing.T) {
	root := shortTempDir(t)
	longTemp := filepath.Join(root, strings.Repeat("t", nativeSocketPathLimit+32))
	if err := os.MkdirAll(longTemp, 0o700); err != nil {
		t.Fatalf("MkdirAll long TMPDIR: %v", err)
	}
	t.Setenv("TMPDIR", longTemp)
	const name = "x"

	var legacyDir string
	for length := 1; length <= socketPathLimit; length++ {
		candidate := filepath.Join(root, strings.Repeat("p", length))
		canonicalProbe := filepath.Join(candidate, "s00000000.sock")
		legacyPath := filepath.Join(candidate, name+".sock")
		if len(canonicalProbe) > socketPathLimit && len(legacyPath) <= nativeSocketPathLimit {
			legacyDir = candidate
			break
		}
	}
	if legacyDir == "" {
		t.Fatal("could not construct addressable legacy path with fallback canonical path")
	}
	p := NewProviderWithDir(legacyDir)
	privateLeaf := p.fallbackDir()
	if filepath.Dir(privateLeaf) != privateFallbackRoot(os.Geteuid()) {
		t.Fatalf("fallback = %q, want private root", privateLeaf)
	}
	if _, err := os.Lstat(privateLeaf); !os.IsNotExist(err) {
		t.Fatalf("private fallback before discovery: %v, want not exist", err)
	}

	gotCommand := startRecordingControlSocket(t, p.legacySockPath(name), "ok\n", 5)
	t.Cleanup(func() { _ = syscall.Rmdir(privateLeaf) })

	startCalls := 0
	p.ops.start = func(*exec.Cmd) (int, error) {
		startCalls++
		return 0, errors.New("process start must not be reached")
	}
	if !p.IsRunning(name) {
		t.Fatal("IsRunning missed addressable legacy socket")
	}
	names, err := p.ListRunning("")
	if err != nil {
		t.Fatalf("ListRunning legacy socket: %v", err)
	}
	if len(names) != 1 || names[0] != name {
		t.Fatalf("ListRunning = %#v, want [%q]", names, name)
	}
	if err := p.Interrupt(name); err != nil {
		t.Fatalf("Interrupt legacy socket: %v", err)
	}
	if err := p.Stop(name); err != nil {
		t.Fatalf("Stop legacy socket: %v", err)
	}
	err = p.Start(context.Background(), name, runtime.Config{Command: "sleep 300"})
	if !errors.Is(err, runtime.ErrSessionExists) {
		t.Fatalf("Start error = %v, want ErrSessionExists from legacy socket", err)
	}
	if startCalls != 0 {
		t.Fatalf("process start calls = %d, want 0", startCalls)
	}
	// Liveness probes only connect, so the owner sees just the two commands.
	for _, want := range []string{"interrupt", "stop"} {
		select {
		case got := <-gotCommand:
			if got != want {
				t.Fatalf("legacy socket command = %q, want %q", got, want)
			}
		case <-time.After(testutil.ExecRaceTimeout):
			t.Fatalf("timed out waiting for legacy socket command %q", want)
		}
	}
}

func TestOverlongTempDirUsesPrivateFallbackAcrossProviders(t *testing.T) {
	root := shortTempDir(t)
	longTemp := filepath.Join(root, strings.Repeat("t", nativeSocketPathLimit+32))
	if err := os.MkdirAll(longTemp, 0o700); err != nil {
		t.Fatalf("MkdirAll long TMPDIR: %v", err)
	}
	t.Setenv("TMPDIR", longTemp)

	longDir := filepath.Join(root, strings.Repeat("p", socketPathLimit+32))
	owner := NewProviderWithDir(longDir)
	observer := NewProviderWithDir(longDir)
	const name = "private-fallback-lifecycle"
	fallback := owner.fallbackDir()
	privateRoot := privateFallbackRoot(os.Geteuid())
	sentinel := filepath.Join(privateRoot, owner.fallbackLeaf()+".sentinel")
	t.Cleanup(func() {
		_ = owner.Stop(name)
		_ = observer.Stop(name)
		_ = os.Remove(sentinel)
		if err := syscall.Rmdir(fallback); err != nil && !os.IsNotExist(err) {
			t.Errorf("Rmdir private fallback leaf: %v", err)
		}
	})

	legacySocket := filepath.Join(longTemp, fallbackSocketDirName, owner.fallbackLeaf(), owner.sockKey(name)+".sock")
	if len(legacySocket) <= nativeSocketPathLimit {
		t.Fatalf("legacy fallback socket length = %d, want greater than %d", len(legacySocket), nativeSocketPathLimit)
	}
	if got, want := fallback, filepath.Join(privateRoot, owner.fallbackLeaf()); got != want {
		t.Fatalf("fallback = %q, want private path %q", got, want)
	}
	if err := owner.Start(context.Background(), name, runtime.Config{Command: "sleep 300"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	requirePrivateSocketDirectory(t, privateRoot)
	requirePrivateSocketDirectory(t, fallback)
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatalf("WriteFile private-root sentinel: %v", err)
	}

	if !observer.IsRunning(name) {
		t.Fatal("private fallback session is not visible through another provider")
	}
	names, err := observer.ListRunning("")
	if err != nil {
		t.Fatalf("cross-provider ListRunning: %v", err)
	}
	if len(names) != 1 || names[0] != name {
		t.Fatalf("cross-provider ListRunning = %#v, want [%q]", names, name)
	}
	if err := observer.Stop(name); err != nil {
		t.Fatalf("cross-provider Stop: %v", err)
	}

	requirePrivateSocketDirectory(t, fallback)
	if contents, err := os.ReadFile(sentinel); err != nil || string(contents) != "keep" {
		t.Fatalf("private-root sentinel after Stop: contents=%q err=%v", contents, err)
	}
	if info, err := os.Stat(longDir); err != nil || !info.IsDir() {
		t.Fatalf("caller-owned directory after Stop: info=%v err=%v", info, err)
	}
}

func TestPrivateFallbackRejectsHostilePrecreation(t *testing.T) {
	root := shortTempDir(t)
	longTemp := filepath.Join(root, strings.Repeat("t", nativeSocketPathLimit+32))
	if err := os.MkdirAll(longTemp, 0o700); err != nil {
		t.Fatalf("MkdirAll long TMPDIR: %v", err)
	}
	t.Setenv("TMPDIR", longTemp)
	privateRoot := ensurePrivateFallbackRootForTest(t)

	t.Run("symlink leaf", func(t *testing.T) {
		longDir := filepath.Join(root, "symlink-state", strings.Repeat("p", socketPathLimit+32))
		p := NewProviderWithDir(longDir)
		const name = "hostile-symlink-fallback"
		fallback := p.fallbackDir()
		if filepath.Dir(fallback) != privateRoot {
			t.Fatalf("fallback parent = %q, want %q", filepath.Dir(fallback), privateRoot)
		}

		target := shortTempDir(t)
		socketTarget := filepath.Join(target, p.sockKey(name)+".sock")
		nameTarget := filepath.Join(target, p.sockKey(name)+".name")
		for path, contents := range map[string]string{socketTarget: "keep-socket", nameTarget: "keep-name"} {
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatalf("WriteFile hostile target %q: %v", path, err)
			}
		}
		if err := os.Symlink(target, fallback); err != nil {
			t.Fatalf("Symlink fallback leaf: %v", err)
		}
		t.Cleanup(func() { _ = os.Remove(fallback) })

		startCalls := 0
		p.ops.start = func(*exec.Cmd) (int, error) {
			startCalls++
			return 0, errors.New("process start must not be reached")
		}
		err := p.Start(context.Background(), name, runtime.Config{Command: "sleep 300"})
		if err == nil || !strings.Contains(err.Error(), "private socket directory") {
			t.Fatalf("Start error = %v, want private socket directory validation", err)
		}
		if startCalls != 0 {
			t.Fatalf("process start calls = %d, want 0", startCalls)
		}
		requirePrivateFallbackRejected(t, p, name)
		for path, want := range map[string]string{socketTarget: "keep-socket", nameTarget: "keep-name"} {
			contents, err := os.ReadFile(path)
			if err != nil || string(contents) != want {
				t.Errorf("hostile target %q: contents=%q err=%v, want %q", path, contents, err, want)
			}
		}
	})

	t.Run("permissive leaf", func(t *testing.T) {
		longDir := filepath.Join(root, "permissive-state", strings.Repeat("p", socketPathLimit+32))
		p := NewProviderWithDir(longDir)
		const name = "hostile-permissive-fallback"
		fallback := p.fallbackDir()
		if err := os.Mkdir(fallback, 0o755); err != nil {
			t.Fatalf("Mkdir permissive fallback leaf: %v", err)
		}
		if err := os.Chmod(fallback, 0o755); err != nil {
			t.Fatalf("Chmod permissive fallback leaf: %v", err)
		}
		socketTarget := filepath.Join(fallback, p.sockKey(name)+".sock")
		nameTarget := filepath.Join(fallback, p.sockKey(name)+".name")
		for path, contents := range map[string]string{socketTarget: "keep-socket", nameTarget: "keep-name"} {
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatalf("WriteFile hostile target %q: %v", path, err)
			}
		}
		t.Cleanup(func() {
			_ = os.Remove(socketTarget)
			_ = os.Remove(nameTarget)
			_ = syscall.Rmdir(fallback)
		})

		startCalls := 0
		p.ops.start = func(*exec.Cmd) (int, error) {
			startCalls++
			return 0, errors.New("process start must not be reached")
		}
		err := p.Start(context.Background(), name, runtime.Config{Command: "sleep 300"})
		if err == nil || !strings.Contains(err.Error(), "private socket directory") {
			t.Fatalf("Start error = %v, want private socket directory validation", err)
		}
		if startCalls != 0 {
			t.Fatalf("process start calls = %d, want 0", startCalls)
		}
		requirePrivateFallbackRejected(t, p, name)
		for path, want := range map[string]string{socketTarget: "keep-socket", nameTarget: "keep-name"} {
			contents, err := os.ReadFile(path)
			if err != nil || string(contents) != want {
				t.Errorf("hostile target %q: contents=%q err=%v, want %q", path, contents, err, want)
			}
		}
	})
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

func TestStopBySocket_PreservesCanonicalErrorWhenLegacyPathIsTooLong(t *testing.T) {
	longDir := filepath.Join(t.TempDir(), strings.Repeat("p", 120))
	p := NewProviderWithDir(longDir)
	const name = "reject-stop"

	if got := len(p.legacySockPath(name)); got <= socketPathLimit {
		t.Fatalf("legacy socket path length = %d, want greater than %d", got, socketPathLimit)
	}
	euid := os.Geteuid()
	if err := p.ensureSocketDir(p.socketDirForEUID(euid), euid); err != nil {
		t.Fatalf("ensure canonical socket directory: %v", err)
	}
	_ = startRejectingControlSocket(t, p.sockPath(name))

	err := p.stopBySocket(name)
	if err == nil || !strings.Contains(err.Error(), "unexpected response") {
		t.Fatalf("stopBySocket error = %v, want canonical unexpected-response error", err)
	}
}
