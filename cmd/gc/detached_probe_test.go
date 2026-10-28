package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	sessiontmux "github.com/gastownhall/gascity/internal/runtime/tmux"
)

// bindDetachedProbeSocket binds a unix listener at the canonical path of the
// named tmux socket, standing in for the live server the detached probe must
// attest before it asks tmux anything. The probe dials the socket but never
// speaks the tmux protocol, so no accept loop is needed.
func bindDetachedProbeSocket(t *testing.T, socketName string) string {
	t.Helper()
	path := sessiontmux.SocketPath(socketName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create detached probe socket dir: %v", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("bind detached probe socket %s: %v", path, err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
		_ = os.Remove(path)
	})
	return path
}

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
			name:    "session with colon is rejected",
			spec:    "tmux:gascity:scope:soak-loop",
			wantErr: true,
		},
		{
			name:    "session with dot is rejected",
			spec:    "tmux:gascity:soak.loop",
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
		{
			name:    "relative socket with slash is rejected",
			spec:    "tmux:rel/ative:soak-loop",
			wantErr: true,
		},
		{
			name: "absolute socket path is accepted",
			spec: "tmux:/tmp/tmux-1000/probe:soak-loop",
			want: detachedProbeSpec{
				Kind:    "tmux",
				Socket:  "/tmp/tmux-1000/probe",
				Session: "soak-loop",
			},
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

// TestProbeDetachedWork_UnreachableSocketIsErrorNeverDead pins the probe's
// fail-closed contract for a socket it cannot observe: a tmux that reports
// "no server running" (exit 1 against a missing socket) means the namespace
// is unreachable, not that the session died. The probe must answer error —
// without even consulting tmux, because no socket file exists at the
// canonical path.
func TestProbeDetachedWork_UnreachableSocketIsErrorNeverDead(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	installFakeTmux(t, `printf '%s\n' "$@" > "$FAKE_TMUX_ARGS"; exit 1`)
	t.Setenv("FAKE_TMUX_ARGS", argsFile)

	got := probeDetachedWork(context.Background(), "tmux:gascity:soak-loop")
	if got.Status != detachedProbeError {
		t.Fatalf("Status = %q, want %q (err=%v)", got.Status, detachedProbeError, got.Err)
	}
	if got.Err == nil {
		t.Fatal("Err = nil, want unobservable-namespace error")
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Fatalf("tmux was consulted for an unreachable socket (args file stat err=%v), want no invocation", err)
	}
}

// TestProbeDetachedWork_TmuxListSessionsAnswer pins how the probe asks an
// attested live server: list-sessions addressed by the resolved -S path, alive
// only on an exact session-name line, dead when the server answers without
// the session, error when the client cannot ask.
func TestProbeDetachedWork_TmuxListSessionsAnswer(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus detachedProbeStatus
	}{
		{name: "alive on exact session line", body: `printf 'soak-loop\n'`, wantStatus: detachedProbeAlive},
		{name: "dead on an answer without the session", body: `printf 'soak-loop-2\n'`, wantStatus: detachedProbeDead},
		{name: "error on client failure", body: `exit 2`, wantStatus: detachedProbeError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := bindDetachedProbeSocket(t, "gascity")
			argsFile := filepath.Join(t.TempDir(), "args")
			installFakeTmux(t, `printf '%s\n' "$@" > "$FAKE_TMUX_ARGS"; `+tt.body)
			t.Setenv("FAKE_TMUX_ARGS", argsFile)

			got := probeDetachedWork(context.Background(), "tmux:gascity:soak-loop")
			if got.Status != tt.wantStatus {
				t.Fatalf("Status = %q, want %q (err=%v)", got.Status, tt.wantStatus, got.Err)
			}
			args, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatalf("read fake tmux args: %v", err)
			}
			wantArgs := fmt.Sprintf("-u\n-S\n%s\nlist-sessions\n-F\n#{session_name}\n", path)
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
	bindDetachedProbeSocket(t, "gascity")
	installFakeTmux(t, "sleep 1")

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
