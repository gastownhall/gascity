//go:build !windows

package herdr

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestStopThatCannotReachHerdrIsRuntimeUnavailable: a Stop whose pane close
// never answers ends at the call's deadline. Unless the herdr server is
// confirmed dead, that is a runtime that could not be reached, not a stopped
// session: the error wraps both runtime.ErrRuntimeUnavailable and
// context.DeadlineExceeded, and the sidecar stays so a retried Stop still
// finds the pane. A confirmed-dead server took its panes with it, so the same
// stall there is a clean stop.
func TestStopThatCannotReachHerdrIsRuntimeUnavailable(t *testing.T) {
	const name = "gastown__worker"
	notDead := func(context.Context, string) (net.Conn, error) { return nil, context.DeadlineExceeded }
	refused := func(context.Context, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	}
	for _, tc := range []struct {
		name     string
		dial     func(context.Context, string) (net.Conn, error)
		wantErr  bool
		wantMeta bool
	}{
		{name: "server not confirmed dead", dial: notDead, wantErr: true, wantMeta: true},
		{name: "server confirmed dead", dial: refused, wantErr: false, wantMeta: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, dir := lookupProvider(t)
			touchFile(t, dir, "get_running")
			touchFile(t, dir, "hang_pane_close")
			p.c.dialUnix = tc.dial
			p.c.unixListening = func(string) (bool, error) { return false, nil }
			if err := p.SetMeta(name, metaBoundPane, "%5"); err != nil {
				t.Fatal(err)
			}
			sidecar := filepath.Join(p.metaDir, sanitize(name))

			err := p.Stop(name)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("Stop = %v; want nil once the server is confirmed dead", err)
				}
				if _, statErr := os.Stat(sidecar); !os.IsNotExist(statErr) {
					t.Fatalf("sidecar %s kept after a clean stop (stat err %v)", sidecar, statErr)
				}
				return
			}
			if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("Stop = %v; want it to wrap runtime.ErrRuntimeUnavailable", err)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Stop = %v; want it to wrap context.DeadlineExceeded", err)
			}
			if _, statErr := os.Stat(sidecar); statErr != nil {
				t.Fatalf("sidecar dropped after a Stop that never reached herdr: %v", statErr)
			}
		})
	}
}
