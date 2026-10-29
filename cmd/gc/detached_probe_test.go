package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	sessiontmux "github.com/gastownhall/gascity/internal/runtime/tmux"
)

func TestParseDetachedProbeSpec(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		want    detachedProbeSpec
		wantErr bool
	}{
		{
			name: "tmux spec",
			spec: "tmux:gascity:soak-loop",
			want: detachedProbeSpec{
				Kind:    "tmux",
				Socket:  "gascity",
				Session: "soak-loop",
			},
		},
		{
			name: "absolute socket path",
			spec: "tmux:/run/gc/city.sock:soak-loop",
			want: detachedProbeSpec{
				Kind:    "tmux",
				Socket:  "/run/gc/city.sock",
				Session: "soak-loop",
			},
		},
		{
			// tmux session names never contain ':' or '.', so such a spec
			// could never be observed alive and must not read as dead.
			name:    "session with a colon",
			spec:    "tmux:gascity:scope:soak-loop",
			wantErr: true,
		},
		{
			name:    "session with a dot",
			spec:    "tmux:gascity:soak.loop",
			wantErr: true,
		},
		{
			name:    "relative socket path",
			spec:    "tmux:run/city.sock:soak-loop",
			wantErr: true,
		},
		{
			name:    "unsupported kind",
			spec:    "k8s:pod:agent",
			wantErr: true,
		},
		{
			name:    "missing session",
			spec:    "tmux:gascity",
			wantErr: true,
		},
		{
			name:    "empty socket",
			spec:    "tmux::soak-loop",
			wantErr: true,
		},
		{
			name:    "empty session",
			spec:    "tmux:gascity:",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDetachedProbeSpec(tt.spec)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseDetachedProbeSpec(%q) error = nil, want error", tt.spec)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDetachedProbeSpec(%q) error = %v", tt.spec, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseDetachedProbeSpec(%q) = %+v, want %+v", tt.spec, got, tt.want)
			}
		})
	}
}

// AC-1: a probe that reaches no tmux server at the canonical socket path
// observed nothing, so it reports error, never dead: the socket file is
// absent, is not a socket, or a live socket's server cannot be queried.
func TestProbeDetachedWork_UnobservableServerIsNeverDead(t *testing.T) {
	noServer := `echo "no server running on $TMUX_TMPDIR" >&2; exit 1`
	tests := []struct {
		name  string
		setup func(t *testing.T)
	}{
		{name: "socket file absent", setup: func(t *testing.T) {
			t.Setenv("TMUX_TMPDIR", shortTmuxTmpDir(t))
			installFakeTmux(t, noServer)
		}},
		{name: "socket path is not a socket", setup: func(t *testing.T) {
			t.Setenv("TMUX_TMPDIR", shortTmuxTmpDir(t))
			path := sessiontmux.SocketPath("gascity")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatalf("mkdir socket dir: %v", err)
			}
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatalf("write regular file at socket path: %v", err)
			}
			installFakeTmux(t, noServer)
		}},
		{name: "live socket whose server does not answer", setup: func(t *testing.T) {
			installFakeTmuxServer(t, "gascity", `echo "error connecting to $TMUX_TMPDIR" >&2; exit 1`)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setup(t)
			got := probeDetachedWork(context.Background(), "tmux:gascity:soak-loop")
			if got.Status != detachedProbeError {
				t.Fatalf("Status = %q, want %q (err=%v)", got.Status, detachedProbeError, got.Err)
			}
			if got.Err == nil {
				t.Fatal("Err = nil, want the reason nothing was observed")
			}
		})
	}
}

// AC-2: a server answering at the canonical socket path decides the session by
// its exact name, whatever TMPDIR holds: listed is alive, and a server that
// lists only a longer name sharing its prefix, or nothing, is dead.
func TestProbeDetachedWork_AnsweringServerDecidesByExactSessionName(t *testing.T) {
	tests := []struct {
		name     string
		sessions []string
		want     detachedProbeStatus
	}{
		{name: "server lists the session", sessions: []string{"soak-loop-2", "soak-loop"}, want: detachedProbeAlive},
		{name: "server lists only a prefix match", sessions: []string{"soak-loop-2"}, want: detachedProbeDead},
		{name: "server lists no sessions", want: detachedProbeDead},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installFakeTmuxServer(t, "gascity", fakeTmuxListSessions(tt.sessions...))
			argsPath := filepath.Join(t.TempDir(), "args")
			t.Setenv("FAKE_TMUX_ARGS", argsPath)
			t.Setenv("TMPDIR", t.TempDir())

			got := probeDetachedWork(context.Background(), "tmux:gascity:soak-loop")
			if got.Status != tt.want {
				t.Fatalf("Status = %q, want %q (err=%v)", got.Status, tt.want, got.Err)
			}
			if got.Err != nil {
				t.Fatalf("Err = %v, want nil", got.Err)
			}
			args, err := os.ReadFile(argsPath)
			if err != nil {
				t.Fatalf("read fake tmux args: %v", err)
			}
			want := strings.Join([]string{"-u", "-S", sessiontmux.SocketPath("gascity"), "list-sessions", "-F", "#{session_name}"}, "\n") + "\n"
			if string(args) != want {
				t.Fatalf("tmux args = %q, want %q", args, want)
			}
		})
	}
}

func TestProbeDetachedWork_MalformedSpecIsError(t *testing.T) {
	got := probeDetachedWorkWithTimeout(context.Background(), "tmux:gascity", time.Second)
	if got.Status != detachedProbeError {
		t.Fatalf("Status = %q, want %q", got.Status, detachedProbeError)
	}
	if got.Err == nil {
		t.Fatal("Err = nil, want parse error")
	}
}

func TestProbeDetachedWork_Timeout(t *testing.T) {
	installFakeTmuxServer(t, "gascity", "sleep 1")

	got := probeDetachedWorkWithTimeout(context.Background(), "tmux:gascity:soak-loop", 10*time.Millisecond)
	if got.Status != detachedProbeTimeout {
		t.Fatalf("Status = %q, want %q (err=%v)", got.Status, detachedProbeTimeout, got.Err)
	}
	if got.Err == nil {
		t.Fatal("Err = nil, want timeout error")
	}
}

func installFakeTmux(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "tmux")
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	pathEnv := dir
	if existing := os.Getenv("PATH"); strings.TrimSpace(existing) != "" {
		pathEnv += string(os.PathListSeparator) + existing
	}
	t.Setenv("PATH", pathEnv)
}

// shortTmuxTmpDir returns a fresh directory under /tmp, short enough that a
// tmux socket path below it fits a unix sun_path.
func shortTmuxTmpDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gcdp")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// installFakeTmuxServer points TMUX_TMPDIR at a fresh directory, binds a unix
// listener at the canonical path of the -L socket name there, so the detached
// probe observes a live server socket, and installs a fake tmux running body.
func installFakeTmuxServer(t *testing.T, socket, body string) {
	t.Helper()
	t.Setenv("TMUX_TMPDIR", shortTmuxTmpDir(t))
	path := sessiontmux.SocketPath(socket)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir socket dir: %v", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen on %s: %v", path, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	installFakeTmux(t, body)
}

// fakeTmuxListSessions is a fake tmux body that records its arguments in
// $FAKE_TMUX_ARGS, when set, and lists sessions one per line.
func fakeTmuxListSessions(sessions ...string) string {
	body := `[ -n "$FAKE_TMUX_ARGS" ] && printf '%s\n' "$@" > "$FAKE_TMUX_ARGS"`
	for _, s := range sessions {
		body += "\necho " + s
	}
	return body + "\nexit 0"
}
