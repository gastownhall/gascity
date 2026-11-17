//go:build !windows

package herdr

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The shared herdr server's environment reaches every pane, and is all a pane
// gets back after a server restart (herdr does not restore a pane's --env). So
// whichever process starts the server must not pass on its own session
// identity or its own HERDR_* values; city-wide values stay.
func TestServerEnvDropsCallerIdentity(t *testing.T) {
	got := serverEnv([]string{
		"GC_SESSION_ID=caller-sid", "GC_SESSION_NAME=caller", "GC_AGENT=rig/claude",
		"GC_RIG=rig", "GC_INSTANCE_TOKEN=tok", "BEADS_ACTOR=caller",
		"HERDR_PANE_ID=w1:p9", "HERDR_SOCKET_PATH=/home/u/.config/herdr/herdr.sock", "HERDR_ENV=1",
		"GC_CITY=/city", "GC_HOME=/gc", "PATH=/usr/bin", "HOME=/home/u", "NOT_HERDR_X=keep",
	})
	want := []string{"GC_CITY=/city", "GC_HOME=/gc", "PATH=/usr/bin", "HOME=/home/u", "NOT_HERDR_X=keep"}
	if !slices.Equal(got, want) {
		t.Errorf("serverEnv() = %q\nwant %q", got, want)
	}
}

// One failed dial is not proof the server is dead. When the dial cannot tell
// (here: a timeout) startServer must leave the socket alone and not launch a
// second server for the session.
func TestStartServerLeavesAServerItCannotConfirmDead(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", shortTempDir(t))
	fake := newFakeHerdr(t, filepath.Join(t.TempDir(), "unused-probe"))
	c := newClient("notdead", t.TempDir())
	c.bin = fake.bin
	c.dialUnix = func(context.Context, string) (net.Conn, error) { return nil, context.DeadlineExceeded }
	c.unixListening = func(string) (bool, error) { return true, nil }
	if err := os.MkdirAll(filepath.Dir(c.socketPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", c.socketPath())
	if err != nil {
		t.Fatalf("binding a unix socket at %q: %v", c.socketPath(), err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	err = c.startServer()
	if err == nil || !strings.Contains(err.Error(), "not confirmed dead") {
		t.Fatalf("startServer() = %v; want a refusal naming \"not confirmed dead\"", err)
	}
	if _, statErr := os.Stat(c.socketPath()); statErr != nil {
		t.Errorf("startServer() removed the live server's socket: %v", statErr)
	}
	if calls := fake.logLines(t); len(calls) != 0 {
		t.Errorf("startServer() ran herdr %q; want no launch", calls)
	}
}

// startServer launches the server with serverEnv, not gc's raw environment.
func TestStartServerScrubsTheServerEnvironment(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not on PATH")
	}
	t.Setenv("XDG_CONFIG_HOME", shortTempDir(t))
	t.Setenv("GC_SESSION_ID", "caller-sid")
	t.Setenv("HERDR_PANE_ID", "w1:p9")
	t.Setenv("GC_CITY", "/city")
	dir := t.TempDir()
	envOut := filepath.Join(dir, "server.env")
	c := newClient("scrub", t.TempDir())
	t.Setenv("FAKE_HERDR_SOCK", c.socketPath())
	// A fake herdr whose `server` dumps its environment and then serves the
	// socket, so startServer's readiness probe succeeds at once.
	bin := filepath.Join(dir, "herdr")
	script := "#!" + py + `
import os, socket, sys
if "server" in sys.argv:
    open(` + "'" + envOut + "'" + `, "w").write("\n".join(f"{k}={v}" for k, v in os.environ.items()))
    p = os.environ["FAKE_HERDR_SOCK"]
    os.makedirs(os.path.dirname(p), exist_ok=True)
    s = socket.socket(socket.AF_UNIX); s.bind(p); s.listen(8)
    while True:
        s.accept()[0].close()
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	c.bin = bin
	if err := c.startServer(); err != nil {
		t.Fatalf("startServer() = %v", err)
	}
	t.Cleanup(func() { _ = exec.Command("pkill", "-f", bin).Run() })
	raw, err := os.ReadFile(envOut)
	if err != nil {
		t.Fatal(err)
	}
	env := string(raw)
	for _, leaked := range []string{"GC_SESSION_ID=", "HERDR_PANE_ID="} {
		if strings.Contains(env, leaked) {
			t.Errorf("herdr server started with %s from gc's environment", strings.TrimSuffix(leaked, "="))
		}
	}
	if !strings.Contains(env, "GC_CITY=/city") {
		t.Errorf("herdr server lost the city-wide GC_CITY")
	}
}

// shortTempDir is a temp dir short enough for a unix socket path under it
// (macOS caps sun_path at 104 bytes; t.TempDir() paths run longer).
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
