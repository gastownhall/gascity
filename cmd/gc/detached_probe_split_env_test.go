package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	sessiontmux "github.com/gastownhall/gascity/internal/runtime/tmux"
)

// bindSocketAtPath binds a live unix listener at path, standing in for a tmux
// server alive in whatever namespace owns path.
func bindSocketAtPath(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create socket dir for %s: %v", path, err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("bind socket %s: %v", path, err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
		_ = os.Remove(path)
	})
}

// TestProbeDetachedWork_SplitTMUXTMPDIRIsErrorNeverDead pins the namespace
// split at the heart of this fix: a server alive under TMUX_TMPDIR=A is
// invisible from TMUX_TMPDIR=B, and invisible must read as error — never as
// death. The old probe asked tmux in B, heard "no server running" (exit 1),
// and answered dead.
func TestProbeDetachedWork_SplitTMUXTMPDIRIsErrorNeverDead(t *testing.T) {
	// Short roots: a unix socket's sun_path is ~107 bytes, and t.TempDir()'s
	// long test-name directories overflow it.
	namespaceA, err := os.MkdirTemp("/tmp", "gc-a3a-")
	if err != nil {
		t.Fatalf("create namespace A root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(namespaceA) })
	namespaceB, err := os.MkdirTemp("/tmp", "gc-a3b-")
	if err != nil {
		t.Fatalf("create namespace B root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(namespaceB) })
	serverInA := filepath.Join(namespaceA, fmt.Sprintf("tmux-%d", os.Getuid()), "gascity")
	bindSocketAtPath(t, serverInA) // the server is alive — in A

	t.Setenv("TMUX_TMPDIR", namespaceB)
	argsFile := filepath.Join(t.TempDir(), "args")
	installFakeTmux(t, `printf '%s\n' "$@" > "$FAKE_TMUX_ARGS"; exit 1`)
	t.Setenv("FAKE_TMUX_ARGS", argsFile)

	got := probeDetachedWork(context.Background(), "tmux:gascity:soak-loop")
	if got.Status != detachedProbeError {
		t.Fatalf("Status = %q, want %q for a namespace split (err=%v)", got.Status, detachedProbeError, got.Err)
	}
	if got.Err == nil {
		t.Fatal("Err = nil, want an unobservable-namespace error")
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Fatalf("tmux in namespace B was consulted (args file stat err=%v), want no invocation: the probe cannot reach the server", err)
	}
}

// TestProbeDetachedWork_AddressFollowsThisEnvironmentsCanonicalPath pins the
// probe's -S argument: a server attested live in this namespace is addressed
// by SocketPath resolved here — the realpath of this TMUX_TMPDIR — with
// TMPDIR pointing anywhere else.
func TestProbeDetachedWork_AddressFollowsThisEnvironmentsCanonicalPath(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	path := bindDetachedProbeSocket(t, "gascity")
	argsFile := filepath.Join(t.TempDir(), "args")
	installFakeTmux(t, `printf '%s\n' "$@" > "$FAKE_TMUX_ARGS"; printf 'soak-loop\n'`)
	t.Setenv("FAKE_TMUX_ARGS", argsFile)

	got := probeDetachedWork(context.Background(), "tmux:gascity:soak-loop")
	if got.Status != detachedProbeAlive {
		t.Fatalf("Status = %q, want %q (err=%v)", got.Status, detachedProbeAlive, got.Err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read fake tmux args: %v", err)
	}
	if want := fmt.Sprintf("-u\n-S\n%s\nlist-sessions\n-F\n#{session_name}\n", sessiontmux.SocketPath("gascity")); string(args) != want {
		t.Fatalf("tmux args = %q, want %q", string(args), want)
	}
	if sessiontmux.SocketPath("gascity") != path {
		t.Fatalf("SocketPath = %q, want the bound path %q", sessiontmux.SocketPath("gascity"), path)
	}
}
