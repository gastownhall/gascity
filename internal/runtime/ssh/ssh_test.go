package ssh

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// fakeRunner captures the remote argv it is asked to run and returns a
// configured result, so tests assert what travels over the connection without
// a real ssh client. When respond is set it provides the per-command result;
// otherwise the fixed out/code/err is returned.
type fakeRunner struct {
	calls   [][]string
	out     []byte
	code    int
	err     error
	respond func(remoteArgv []string) ([]byte, int, error)
}

func (f *fakeRunner) run(_ context.Context, _ Endpoint, remoteArgv []string) ([]byte, int, error) {
	f.calls = append(f.calls, remoteArgv)
	if f.respond != nil {
		return f.respond(remoteArgv)
	}
	return f.out, f.code, f.err
}

func TestSSHArgs_BuildsClientInvocation(t *testing.T) {
	ep := Endpoint{User: "gcagent", Host: "100.110.9.92", Port: 2222, KeyPath: "/k/id", KnownHostsPath: "/k/known"}
	got := sshArgs(ep, []string{"tmux", "send-keys", "-t", "main", "-l", "hi there"})
	want := []string{
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=/k/known",
		"-i", "/k/id",
		"-p", "2222",
		"--", "gcagent@100.110.9.92",
		"'tmux' 'send-keys' '-t' 'main' '-l' 'hi there'",
	}
	if !slices.Equal(got, want) {
		t.Errorf("sshArgs =\n  %v\nwant\n  %v", got, want)
	}
}

func TestSSHArgs_MinimalEndpoint(t *testing.T) {
	got := sshArgs(Endpoint{Host: "box"}, []string{"true"})
	want := []string{
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"--", "box", "'true'",
	}
	if !slices.Equal(got, want) {
		t.Errorf("sshArgs = %v, want %v", got, want)
	}
}

func TestConn_ExecReturnsRunnerResult(t *testing.T) {
	f := &fakeRunner{out: []byte("output"), code: 7}
	c := &Conn{ep: Endpoint{Host: "box"}, run: f}
	out, code, err := c.Exec(context.Background(), "ignored-name", []string{"false"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if string(out) != "output" || code != 7 {
		t.Errorf("Exec = (%q, %d), want (%q, 7)", out, code, "output")
	}
	if len(f.calls) != 1 || !slices.Equal(f.calls[0], []string{"false"}) {
		t.Errorf("runner received %v, want one call [false]", f.calls)
	}
}

func TestConn_ExecPropagatesTransportError(t *testing.T) {
	want := errors.New("connection refused")
	c := &Conn{ep: Endpoint{Host: "box"}, run: &fakeRunner{code: -1, err: want}}
	_, _, err := c.Exec(context.Background(), "", []string{"echo"})
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
}

func TestConn_ExecEmptyArgvErrors(t *testing.T) {
	// An empty remote command opens an interactive login shell over ssh, so
	// empty argv is rejected before the connection is even attempted.
	f := &fakeRunner{}
	c := &Conn{ep: Endpoint{Host: "box"}, run: f}
	_, code, err := c.Exec(context.Background(), "", nil)
	if err == nil {
		t.Fatal("empty argv must error")
	}
	if code != -1 {
		t.Errorf("code = %d, want -1", code)
	}
	if len(f.calls) != 0 {
		t.Errorf("runner must not be invoked for empty argv; got %v", f.calls)
	}
}

// TestConn_ConnectionRefusedIsTransportError exercises the real ssh client
// against a refused loopback port: ssh exits 255, which must surface as a
// transport error (code -1, non-nil err), not a clean command result.
func TestConn_ConnectionRefusedIsTransportError(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh client")
	}
	c := New(Endpoint{Host: "127.0.0.1", Port: 1, KnownHostsPath: filepath.Join(t.TempDir(), "known_hosts")})
	_, code, err := c.Exec(context.Background(), "", []string{"true"})
	if err == nil {
		t.Fatalf("connection refused must be a transport error; got code=%d err=nil", code)
	}
	if code != -1 {
		t.Errorf("code = %d, want -1 on transport failure", code)
	}
}

// TestTmuxCarrierDrivesOverSSH is the point of this slice: the tmux carrier
// drives a session over the ssh connection, issuing tmux commands as the
// remote argv.
func TestTmuxCarrierDrivesOverSSH(t *testing.T) {
	f := &fakeRunner{}
	c := &Conn{ep: Endpoint{User: "u", Host: "box"}, run: f}
	carrier := runtime.NewTmuxCarrier(c, "main")

	if err := carrier.Nudge(context.Background(), "s", runtime.TextContent("hello")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	want := [][]string{
		{"tmux", "send-keys", "-t", "main", "-l", "hello"},
		{"tmux", "send-keys", "-t", "main", "Enter"},
	}
	if len(f.calls) != len(want) {
		t.Fatalf("remote argv calls = %v, want %v", f.calls, want)
	}
	for i := range want {
		if !slices.Equal(f.calls[i], want[i]) {
			t.Errorf("remote argv[%d] = %v, want %v", i, f.calls[i], want[i])
		}
	}
}

// TestConn_ExecOverRealLocalhost exercises the actual ssh client when
// passwordless localhost ssh is available; it skips otherwise (e.g. CI).
func TestConn_ExecOverRealLocalhost(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh client")
	}
	kh := filepath.Join(t.TempDir(), "known_hosts")
	probe := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", "-o", "UserKnownHostsFile="+kh, "localhost", "true")
	if probe.Run() != nil {
		t.Skip("passwordless ssh to localhost unavailable")
	}
	c := New(Endpoint{Host: "localhost", KnownHostsPath: kh})
	out, code, err := c.Exec(context.Background(), "", []string{"printf", "%s", "ok"})
	if err != nil {
		t.Fatalf("Exec over localhost: %v", err)
	}
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if string(out) != "ok" {
		t.Errorf("out = %q, want %q", out, "ok")
	}
}
