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
			name:    "session with a colon tmux cannot name",
			spec:    "tmux:gascity:scope:soak-loop",
			wantErr: true,
		},
		{
			name:    "session with a dot tmux cannot name",
			spec:    "tmux:gascity:scope.soak-loop",
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

// AC-2: a server that answers is read by exact session name; only an answer
// that lacks the session is dead, and a failed tmux client is an error.
func TestProbeDetachedWork_TmuxExitStatus(t *testing.T) {
	tests := []struct {
		name       string
		sessions   string
		exitCode   string
		wantStatus detachedProbeStatus
	}{
		{name: "alive when the server lists the session", sessions: `soak-loop-2\nsoak-loop\n`, exitCode: "0", wantStatus: detachedProbeAlive},
		{name: "dead when the server lists only a longer name", sessions: `soak-loop-2\n`, exitCode: "0", wantStatus: detachedProbeDead},
		{name: "dead when the server lists no session", exitCode: "0", wantStatus: detachedProbeDead},
		{name: "error on exit one", exitCode: "1", wantStatus: detachedProbeError},
		{name: "error on other exit", exitCode: "2", wantStatus: detachedProbeError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			argsFile := filepath.Join(t.TempDir(), "args")
			listenDetachedProbeServer(t, "gascity")
			installFakeTmux(t, `printf '%s\n' "$@" > "$FAKE_TMUX_ARGS"; printf '`+tt.sessions+`'; exit `+tt.exitCode)
			t.Setenv("FAKE_TMUX_ARGS", argsFile)

			got := probeDetachedWork(context.Background(), "tmux:gascity:soak-loop")
			if got.Status != tt.wantStatus {
				t.Fatalf("Status = %q, want %q (err=%v)", got.Status, tt.wantStatus, got.Err)
			}
			args, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatalf("read fake tmux args: %v", err)
			}
			wantArgs := "-L\ngascity\nlist-sessions\n-F\n#{session_name}\n"
			if string(args) != wantArgs {
				t.Fatalf("tmux args = %q, want %q", string(args), wantArgs)
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
	listenDetachedProbeServer(t, "gascity")
	installFakeTmux(t, "sleep 1")

	got := probeDetachedWorkWithTimeout(context.Background(), "tmux:gascity:soak-loop", 10*time.Millisecond)
	if got.Status != detachedProbeTimeout {
		t.Fatalf("Status = %q, want %q (err=%v)", got.Status, detachedProbeTimeout, got.Err)
	}
	if got.Err == nil {
		t.Fatal("Err = nil, want timeout error")
	}
}

// AC-1: a probe that reaches no server at the canonical socket path reports
// error, never dead, whatever tmux's exit status says.
func TestProbeDetachedWork_UnreachableServerIsNeverDead(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{name: "socket file absent", setup: func(*testing.T, string) {}},
		{name: "socket path is not a socket", setup: func(t *testing.T, path string) {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatalf("write non-socket: %v", err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := useDetachedProbeSocketDir(t, "gascity")
			tt.setup(t, path)
			installFakeTmux(t, `echo "no server running on $0" >&2; exit 1`)

			got := probeDetachedWork(context.Background(), "tmux:gascity:soak-loop")
			if got.Status != detachedProbeError {
				t.Fatalf("Status = %q, want %q (err=%v)", got.Status, detachedProbeError, got.Err)
			}
			if got.Err == nil {
				t.Fatal("Err = nil, want the unobserved socket explained")
			}
		})
	}
}

// AC-1: a socket that refuses connections on a stable inode belonged to a
// server that is gone, so the session it hosted is dead.
func TestProbeDetachedWork_StaleRefusedSocketIsDead(t *testing.T) {
	path := useDetachedProbeSocketDir(t, "gascity")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	installFakeTmux(t, `echo "no server running on $0" >&2; exit 1`)

	got := probeDetachedWork(context.Background(), "tmux:gascity:soak-loop")
	if got.Status != detachedProbeDead {
		t.Fatalf("Status = %q, want %q (err=%v)", got.Status, detachedProbeDead, got.Err)
	}
}

// useDetachedProbeSocketDir points TMUX_TMPDIR at a fresh directory short
// enough for a unix socket address and returns the socket path the detached
// probe resolves name to under it. Nothing listens there yet.
func useDetachedProbeSocketDir(t *testing.T, name string) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "gcdp")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Setenv("TMUX_TMPDIR", root)
	path := sessiontmux.SocketPath(name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir socket dir: %v", err)
	}
	return path
}

// listenDetachedProbeServer stands in for a live tmux server on socket name:
// a unix listener at the path the detached probe resolves, so the probe
// reaches its tmux client call, which installFakeTmux answers.
func listenDetachedProbeServer(t *testing.T, name string) {
	t.Helper()
	path := useDetachedProbeSocketDir(t, name)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
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
