package herdr

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestServerAliveDialsThroughTheClientConnect pins serverAlive to c.dialUnix,
// the connect the event stream uses, so what reads as alive is what the stream
// can reach: on Windows that is herdr's named pipe, which a direct unix dial
// never reaches. It also pins the probe's bound.
func TestServerAliveDialsThroughTheClientConnect(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "herdr.sock")
	var dialed string
	var left time.Duration
	c := &client{session: "gc-test", sockPath: sock}
	c.dialUnix = func(ctx context.Context, path string) (net.Conn, error) {
		dialed = path
		if deadline, ok := ctx.Deadline(); ok {
			left = time.Until(deadline)
		}
		return nil, errors.New("nothing is listening")
	}
	if c.serverAlive() {
		t.Fatal("serverAlive() = true after the connect failed")
	}
	if dialed != sock {
		t.Fatalf("serverAlive dialed %q through the client's connect, want %q", dialed, sock)
	}
	if left <= 0 || left > 500*time.Millisecond {
		t.Fatalf("serverAlive's connect ran with %v left, want the 500ms probe bound", left)
	}

	server, conn := net.Pipe()
	defer func() { _ = server.Close() }()
	c.dialUnix = func(context.Context, string) (net.Conn, error) { return conn, nil }
	if !c.serverAlive() {
		t.Fatal("serverAlive() = false after the connect succeeded")
	}
}

// TestStartServerStopsAtARefusedEndpoint: a connect that reached something it
// refused (errEndpointRefused) fails startServer with the refusal, and nothing
// is launched. No server started now could take the endpoint from whatever
// holds it, so launching one would only turn the reason into "did not become
// ready" after the wait. The binary does not exist, so a launch would fail
// with a different error.
func TestStartServerStopsAtARefusedEndpoint(t *testing.T) {
	refusal := endpointRefusal{errors.New("herdr pipe P is owned by someone else")}
	c := &client{session: "gc-test", sockPath: filepath.Join(t.TempDir(), "herdr.sock"), bin: filepath.Join(t.TempDir(), "no-herdr-here")}
	c.dialUnix = func(context.Context, string) (net.Conn, error) { return nil, refusal }
	err := c.startServer()
	if !errors.Is(err, errEndpointRefused) || !strings.Contains(err.Error(), "owned by someone else") {
		t.Fatalf("startServer = %v; want the refusal", err)
	}
}

// TestStartServerStopsWhenItsEndpointIsRefusedWhileWaiting: a refusal that
// appears while startServer waits for the server it launched ends the wait.
// The launch is this test binary, which exits at once on the server's
// arguments.
func TestStartServerStopsWhenItsEndpointIsRefusedWhileWaiting(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c := &client{session: "gc-test", sockPath: filepath.Join(t.TempDir(), "herdr.sock"), bin: self}
	dials := 0
	c.dialUnix = func(context.Context, string) (net.Conn, error) {
		dials++
		if dials == 1 {
			return nil, errors.New("nothing is listening")
		}
		return nil, endpointRefusal{errors.New("herdr pipe P is owned by someone else")}
	}
	start := time.Now()
	err = c.startServer()
	if !errors.Is(err, errEndpointRefused) {
		t.Fatalf("startServer = %v; want the refusal", err)
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Errorf("startServer took %v; the refusal should end the wait", waited)
	}
}
