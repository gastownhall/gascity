package localsocket

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Windows has AF_UNIX support, so Gas City's own local sockets stay on AF_UNIX
// on every supported platform rather than move to another transport, as long
// as path lengths and permissions are handled correctly.
//
// These cases hold that line. They run the real Listen/Dial pair this
// package exports, so the day AF_UNIX stops carrying Gas City's control plane
// on a supported platform, it is this file that fails rather than a supervisor
// in the field.

func TestListenAndDialRoundTrip(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "probe.sock")

	lis, err := Listen(sock, Options{DirMode: 0o700})
	if err != nil {
		t.Fatalf("Listen(%q) on %s: %v", sock, runtime.GOOS, err)
	}
	defer lis.Close() //nolint:errcheck // test cleanup

	go func() {
		conn, acceptErr := lis.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()  //nolint:errcheck // test cleanup
		io.Copy(conn, conn) //nolint:errcheck // echo until the peer closes
	}()

	conn, err := Dial(sock, 2*time.Second)
	if err != nil {
		t.Fatalf("Dial(%q): %v", sock, err)
	}
	defer conn.Close() //nolint:errcheck // test cleanup

	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "ping\n" {
		t.Fatalf("echo = %q, want %q", buf, "ping\n")
	}
}

// TestListenRebindsOverAStaleSocket is the crash-recovery contract. A
// supervisor killed hard can leave the socket name behind, and the next one
// must be able to take the endpoint back.
func TestListenRebindsOverAStaleSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "stale.sock")

	first, err := Listen(sock, Options{DirMode: 0o700})
	if err != nil {
		t.Fatalf("first Listen: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first listener: %v", err)
	}

	second, err := Listen(sock, Options{DirMode: 0o700})
	if err != nil {
		t.Fatalf("rebind after a stale socket: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("close second listener: %v", err)
	}
}

// TestDialWithoutAListenerFailsPromptly pins the shape every liveness probe in
// cmd/gc depends on: an endpoint nobody is serving answers with an error, not
// with a hang that would be read as a slow but living supervisor.
func TestDialWithoutAListenerFailsPromptly(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "absent.sock")

	start := time.Now()
	conn, err := Dial(sock, 2*time.Second)
	if err == nil {
		conn.Close() //nolint:errcheck // test cleanup
		t.Fatal("dial to an absent endpoint succeeded")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("dial to an absent endpoint took %v, past its own timeout", elapsed)
	}
}

// TestListenInsideAPrivateDirectory covers the endpoints that keep their state
// in a directory Gas City creates and owns. On Unix that is a 0700 directory;
// on Windows prepareDir routes through runtime.EnsurePrivateDir, which fails
// closed on foreign ownership and reparse points, so a failure here is a real
// refusal rather than a transport problem.
func TestListenInsideAPrivateDirectory(t *testing.T) {
	dir := filepath.Join(shortTempDir(t), "p")
	sock := filepath.Join(dir, "owner-only.sock")

	lis, err := Listen(sock, Options{DirMode: 0o700, SocketMode: 0o600})
	if err != nil {
		t.Fatalf("Listen inside a private dir: %v", err)
	}
	defer lis.Close() //nolint:errcheck // test cleanup

	conn, err := Dial(sock, 2*time.Second)
	if err != nil {
		t.Fatalf("Dial inside a private dir: %v", err)
	}
	conn.Close() //nolint:errcheck // test cleanup

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat private dir: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", dir)
	}
}

// TestPathLimitIsTheRealCeiling measures the platform rather than trusting the
// constant. PathLimit is a budget held below every supported sockaddr_un, so a
// pathname at the limit must bind and the platform's own ceiling must be at or
// above it. A kernel that tightened would otherwise show up as an unexplained
// "bind: invalid argument" in the field.
func TestPathLimitIsTheRealCeiling(t *testing.T) {
	base := t.TempDir()

	atLimit, err := padPathTo(base, PathLimit)
	if err != nil {
		t.Skipf("cannot build a %d-byte path under %q: %v", PathLimit, base, err)
	}
	if err := os.MkdirAll(filepath.Dir(atLimit), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	lis, err := Listen(atLimit, Options{DirMode: 0o700})
	if err != nil {
		t.Fatalf("a %d-byte pathname is at PathLimit but did not bind: %v", len(atLimit), err)
	}
	if err := lis.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestOverLongPathIsRejectedWithItsLimit pins the diagnosis rather than the
// failure. The kernel answers an over-long pathname with EINVAL, which names
// neither the limit nor the path; a caller resolving its own endpoint path
// needs both to know what to shorten.
func TestOverLongPathIsRejectedWithItsLimit(t *testing.T) {
	base := t.TempDir()
	long, err := padPathTo(base, PathLimit+8)
	if err != nil {
		t.Skipf("cannot build a %d-byte path under %q: %v", PathLimit+8, base, err)
	}

	_, listenErr := Listen(long, Options{DirMode: 0o700})
	var tooLong *PathTooLongError
	if !errorAs(listenErr, &tooLong) {
		t.Fatalf("Listen error = %v, want *PathTooLongError", listenErr)
	}
	if tooLong.Limit != PathLimit {
		t.Fatalf("PathTooLongError.Limit = %d, want %d", tooLong.Limit, PathLimit)
	}
	if !strings.Contains(tooLong.Error(), "over the") {
		t.Fatalf("PathTooLongError.Error() = %q, want it to name the limit", tooLong.Error())
	}

	_, dialErr := Dial(long, time.Second)
	if !errorAs(dialErr, &tooLong) {
		t.Fatalf("Dial error = %v, want *PathTooLongError", dialErr)
	}

	// Nothing was created on the way to the refusal.
	if _, statErr := os.Lstat(long); statErr == nil {
		t.Fatalf("a rejected endpoint path was created anyway: %s", long)
	}
}

// TestFallbackDirIsUsableForItsOwnPurpose is the case the "/tmp" literal failed.
// A fallback directory exists to hold a short pathname when the canonical one
// is too long, so it has to be a real, writable directory on this platform and
// short enough that a hashed endpoint name still fits inside PathLimit.
func TestFallbackDirIsUsableForItsOwnPurpose(t *testing.T) {
	const namespace = "gascity-localsocket-test"
	dir := FallbackDir(namespace)

	if !filepath.IsAbs(dir) {
		t.Fatalf("FallbackDir(%q) = %q, want an absolute path", namespace, dir)
	}
	if runtime.GOOS == "windows" && filepath.VolumeName(dir) == "" {
		t.Fatalf("FallbackDir(%q) = %q has no volume: it would resolve against the current drive", namespace, dir)
	}

	// 32 hex characters plus ".sock" is the hashed name every caller builds.
	hashed := filepath.Join(dir, strings.Repeat("a", 32)+".sock")
	if len(hashed) > PathLimit {
		t.Fatalf("a hashed endpoint under FallbackDir(%q) is %d bytes, over the %d-byte limit: %s",
			namespace, len(hashed), PathLimit, hashed)
	}

	sock := filepath.Join(dir, fmt.Sprintf("probe-%d.sock", os.Getpid()))
	lis, err := Listen(sock, Options{DirMode: 0o700})
	if err != nil {
		t.Fatalf("Listen under FallbackDir(%q): %v", namespace, err)
	}
	defer os.Remove(sock) //nolint:errcheck // test cleanup
	if err := lis.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	os.Remove(dir) //nolint:errcheck // best-effort; a shared namespace dir may be busy
}

// shortTempDir is t.TempDir for cases that must stay inside PathLimit.
// t.TempDir embeds the test name and a nonce, which alone can exceed the
// pathname budget an endpoint has to fit in.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ls")
	if err != nil {
		t.Fatalf("creating a short temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) }) //nolint:errcheck // test cleanup
	return dir
}

// errorAs is errors.As without the import, kept local so the assertion reads
// the same in every case above.
func errorAs(err error, target **PathTooLongError) bool {
	for err != nil {
		if typed, ok := err.(*PathTooLongError); ok { //nolint:errorlint // exact type is the assertion
			*target = typed
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

// padPathTo builds a pathname under base of exactly target bytes, split across
// components so no single one exceeds a filesystem's name limit.
func padPathTo(base string, target int) (string, error) {
	const leaf = "p.sock"
	prefix := base + string(filepath.Separator)
	minimum := len(prefix) + len(leaf)
	if target < minimum {
		return "", fmt.Errorf("target %d is below the %d-byte minimum under %q", target, minimum, base)
	}
	padding := target - minimum
	if padding == 0 {
		return prefix + leaf, nil
	}
	var segments []string
	for padding > 0 {
		chunk := padding
		if chunk > 40 {
			chunk = 40
		}
		if chunk < 2 {
			segments = append(segments, "x")
			padding -= 2
			continue
		}
		segments = append(segments, strings.Repeat("x", chunk-1))
		padding -= chunk
	}
	if padding < 0 {
		return "", fmt.Errorf("cannot land on %d bytes exactly", target)
	}
	return prefix + strings.Join(segments, string(filepath.Separator)) + string(filepath.Separator) + leaf, nil
}
