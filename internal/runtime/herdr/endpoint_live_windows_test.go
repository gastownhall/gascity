//go:build windows

package herdr

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/gastownhall/gascity/internal/runtime/herdr/herdrtest"
)

// TestSocketPathMatchesHerdrLive asks the installed herdr where each probed
// environment puts the default session's socket (`herdr session list --json`
// reports it and creates nothing) and requires socketPath to name the same
// pipe. TestHerdrConfigDirReproducesHerdrsResolution records what herdr 0.9.1
// answered; this asks the herdr at hand, so a herdr that resolves differently
// fails here.
func TestSocketPathMatchesHerdrLive(t *testing.T) {
	requireLiveHerdr(t)
	for _, probe := range herdrConfigDirProbes(t.TempDir()) {
		t.Run(probe.name, func(t *testing.T) {
			applyProbeEnv(t, probe)
			c := newClient("default", "")
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			out, err := c.runRaw(ctx, "session", "list", "--json")
			if err != nil {
				t.Fatalf("herdr session list: %v", err)
			}
			var list struct {
				Sessions []struct {
					Name       string `json:"name"`
					SocketPath string `json:"socket_path"`
				} `json:"sessions"`
			}
			if err := json.Unmarshal([]byte(out), &list); err != nil {
				t.Fatalf("decode herdr session list: %v: %s", err, out)
			}
			herdrSock := ""
			for _, s := range list.Sessions {
				if s.Name == "default" {
					herdrSock = s.SocketPath
				}
			}
			if herdrSock == "" {
				t.Fatalf("herdr session list reported no default session: %s", out)
			}
			want, err := herdrPipeName(herdrSock)
			if err != nil {
				t.Fatalf("herdr's socket %q: %v", herdrSock, err)
			}
			got, err := herdrPipeName(c.socketPath())
			if err != nil || got != want {
				t.Errorf("socketPath() %q names pipe %q (%v); herdr's socket %q names %q", c.socketPath(), got, err, herdrSock, want)
			}
		})
	}
}

// TestServerLifecycleOverPipeLive runs the production server lifecycle against
// a real herdr on its Windows transport: ConfigureServer launches the server and
// waits for it through the pipe, the event connect reaches it, an unclean exit
// reads as dead, and a second ConfigureServer starts a new server over the
// pointer file the dead one left, which removeStaleSocket deliberately keeps.
func TestServerLifecycleOverPipeLive(t *testing.T) {
	requireLiveHerdr(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	session := fmt.Sprintf("gctest-pipe-%d", os.Getpid())
	p := New(session, t.TempDir(), t.TempDir(), 0, 0)
	t.Cleanup(func() { _ = p.TeardownServer() })

	if err := p.ConfigureServer(); err != nil {
		t.Fatalf("ConfigureServer: %v", err)
	}
	conn, err := p.c.dialSocket(context.Background())
	if err != nil {
		t.Fatalf("dialSocket to a live server: %v", err)
	}
	_ = conn.Close()

	first, wrote := herdrServerPointer(t, p.c.socketPath())
	killHerdrServer(t, first, wrote)
	herdrtest.Poll(t, "the killed server to stop reading as alive", 10*time.Second, func() (bool, string) {
		return !p.c.serverAlive(), "serverAlive() still true"
	})
	if _, err := os.Stat(p.c.socketPath()); err != nil {
		t.Fatalf("herdr's pointer file did not survive the unclean exit (%v); this test no longer covers a stale start", err)
	}

	if err := p.ConfigureServer(); err != nil {
		t.Fatalf("ConfigureServer over the dead server's pointer file: %v", err)
	}
	if second, _ := herdrServerPointer(t, p.c.socketPath()); second == first {
		t.Fatalf("pointer file still names the killed server %d after a new server started", first)
	}
}

// herdrServerPointer reads the pointer file herdr keeps at its socket path,
// "<pid>:<nanoseconds>": the server's PID and when the server wrote the file.
func herdrServerPointer(t *testing.T, sock string) (uint32, time.Time) {
	t.Helper()
	b, err := os.ReadFile(sock)
	if err != nil {
		t.Fatalf("read herdr's pointer file: %v", err)
	}
	field, stamp, ok := strings.Cut(strings.TrimSpace(string(b)), ":")
	pid, pidErr := strconv.ParseUint(field, 10, 32)
	nanos, stampErr := strconv.ParseInt(stamp, 10, 64)
	if !ok || pidErr != nil || stampErr != nil || pid == 0 {
		t.Fatalf("herdr's pointer file %s holds %q, not <pid>:<nanoseconds>", sock, b)
	}
	return uint32(pid), time.Unix(0, nanos)
}

// killHerdrServer ends the server uncleanly. Its PID was read from a file, so
// the process must first show it is that server: the herdr binary under test,
// started no later than the file was written. A PID reused since then names a
// process started after that. The checks and the kill go through one handle,
// so the PID cannot be reused between them.
func killHerdrServer(t *testing.T, pid uint32, wrote time.Time) {
	t.Helper()
	bin := herdrtest.Binary(t)
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, pid)
	if err != nil {
		t.Fatalf("open herdr server %d: %v", pid, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		t.Fatalf("image of process %d: %v", pid, err)
	}
	if image := windows.UTF16ToString(buf[:n]); !strings.EqualFold(filepath.Clean(image), filepath.Clean(bin)) {
		t.Fatalf("pointer file names process %d, which is %s, not the herdr under test %s", pid, image, bin)
	}
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		t.Fatalf("start time of process %d: %v", pid, err)
	}
	if started := time.Unix(0, created.Nanoseconds()); started.After(wrote) {
		t.Fatalf("process %d started at %v, after the pointer file naming it was written at %v: the PID was reused", pid, started, wrote)
	}
	if err := windows.TerminateProcess(h, 1); err != nil {
		t.Fatalf("terminate herdr server %d: %v", pid, err)
	}
	if ev, err := windows.WaitForSingleObject(h, 10_000); err != nil || ev != windows.WAIT_OBJECT_0 {
		t.Fatalf("herdr server %d did not exit: event %#x, %v", pid, ev, err)
	}
}
