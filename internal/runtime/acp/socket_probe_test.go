package acp

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// dialError is the shape net.DialTimeout returns for a unix socket.
func dialError(err error) error {
	return &net.OpError{Op: "dial", Net: "unix", Err: &os.SyscallError{Syscall: "connect", Err: err}}
}

var (
	errDialMissing = dialError(syscall.ENOENT)
	errDialRefused = dialError(syscall.ECONNREFUSED)
	errDialTimeout = &net.OpError{Op: "dial", Net: "unix", Err: os.ErrDeadlineExceeded}
	errDialDenied  = dialError(syscall.EACCES)
	errDialBusy    = dialError(syscall.EAGAIN)
)

// socketOutcome is one fake dial result: an error, or a connection whose owner
// reads the ping and answers reply ("" answers nothing).
type socketOutcome struct {
	err   error
	reply string
}

var (
	socketMissing = socketOutcome{err: errDialMissing}
	socketOK      = socketOutcome{reply: "ok\n"}
)

// fakeSocketDial answers each dialed path from outcomes over net.Pipe, so no
// listener is bound. A path without an outcome is missing.
func fakeSocketDial(t *testing.T, outcomes map[string]socketOutcome) func(string, string, time.Duration) (net.Conn, error) {
	t.Helper()
	return func(_, addr string, _ time.Duration) (net.Conn, error) {
		o, ok := outcomes[addr]
		if !ok {
			return nil, errDialMissing
		}
		if o.err != nil {
			return nil, o.err
		}
		client, owner := net.Pipe()
		t.Cleanup(func() { _ = owner.Close() })
		go func() {
			r := bufio.NewReader(owner)
			if _, err := r.ReadString('\n'); err != nil {
				return
			}
			if o.reply != "" {
				_, _ = owner.Write([]byte(o.reply))
			}
			_, _ = io.Copy(io.Discard, r)
		}()
		return client, nil
	}
}

func newProbeProvider(t *testing.T) *Provider {
	t.Helper()
	return NewProviderWithDir(filepath.Join(shortTempDir(t), "acp"), Config{})
}

// addSocketEntry makes ListRunning see a socket entry for name. It is a plain
// file: the fake dial, not the file, decides what the socket answers.
func addSocketEntry(t *testing.T, p *Provider, name string) {
	t.Helper()
	for path, data := range map[string]string{p.sockPath(name): "", p.sockNamePath(name): name} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatalf("WriteFile %q: %v", filepath.Base(path), err)
		}
	}
}

// Kills: a connected owner that is slow, silent or garbled read as dead (the
// old 500 ms ping rule), a missing or refusing socket read as unknown, and a
// failed dial (on either path) read as absent.
func TestProbeSessionSocketClassifies(t *testing.T) {
	const name = "worker"
	tests := []struct {
		desc              string
		canonical, legacy socketOutcome
		wantPresent       bool
		wantUnknown       bool
	}{
		{desc: "ping answered ok", canonical: socketOK, legacy: socketMissing, wantPresent: true},
		{desc: "connected but silent", canonical: socketOutcome{}, legacy: socketMissing, wantPresent: true},
		{desc: "connected but garbled", canonical: socketOutcome{reply: "busy\n"}, legacy: socketMissing, wantPresent: true},
		{desc: "legacy path connected", canonical: socketOutcome{err: errDialRefused}, legacy: socketOK, wantPresent: true},
		{desc: "ENOENT", canonical: socketMissing, legacy: socketMissing},
		{desc: "ECONNREFUSED", canonical: socketOutcome{err: errDialRefused}, legacy: socketMissing},
		{desc: "dial timeout", canonical: socketOutcome{err: errDialTimeout}, legacy: socketMissing, wantUnknown: true},
		{desc: "EACCES", canonical: socketOutcome{err: errDialDenied}, legacy: socketMissing, wantUnknown: true},
		{desc: "EAGAIN (full backlog)", canonical: socketOutcome{err: errDialBusy}, legacy: socketMissing, wantUnknown: true},
		{desc: "hashed refused, legacy EAGAIN", canonical: socketOutcome{err: errDialRefused}, legacy: socketOutcome{err: errDialBusy}, wantUnknown: true},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			p := newProbeProvider(t)
			p.dial = fakeSocketDial(t, map[string]socketOutcome{
				p.sockPath(name):       tt.canonical,
				p.legacySockPath(name): tt.legacy,
			})
			present, err := p.probeSessionSocket(name)
			if present != tt.wantPresent {
				t.Errorf("present = %v, want %v", present, tt.wantPresent)
			}
			if tt.wantUnknown {
				if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
					t.Errorf("err = %v, want ErrRuntimeUnavailable", err)
				}
				if runtime.IsSessionGone(err) {
					t.Errorf("err = %v reads as session gone", err)
				}
			} else if err != nil {
				t.Errorf("err = %v, want nil", err)
			}
		})
	}
}

// Kills: a ping miss or a failed dial reported as a confirmed absence.
func TestACPObserveLivenessWithError(t *testing.T) {
	const name = "worker"
	tests := []struct {
		desc    string
		outcome socketOutcome
		want    runtime.Liveness
		wantErr bool
	}{
		{desc: "connected but silent", outcome: socketOutcome{}, want: runtime.Liveness{Running: true, Alive: true}},
		{desc: "missing", outcome: socketMissing},
		{desc: "dial timeout", outcome: socketOutcome{err: errDialTimeout}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			p := newProbeProvider(t)
			p.dial = fakeSocketDial(t, map[string]socketOutcome{p.sockPath(name): tt.outcome})
			got, err := runtime.ObserveLivenessWithError(p, name, []string{"agent"})
			if got != tt.want {
				t.Errorf("liveness = %+v, want %+v", got, tt.want)
			}
			if tt.wantErr != errors.Is(err, runtime.ErrRuntimeUnavailable) || (!tt.wantErr && err != nil) {
				t.Errorf("err = %v, want unknown %v", err, tt.wantErr)
			}
			if running := p.IsRunning(name); running != tt.want.Running {
				t.Errorf("IsRunning = %v, want %v", running, tt.want.Running)
			}
		})
	}
}

// Kills: the silent drop of a busy live session from the listing.
func TestACPListRunningKeepsConnectedSlowSession(t *testing.T) {
	p := newProbeProvider(t)
	addSocketEntry(t, p, "slow")
	p.dial = fakeSocketDial(t, map[string]socketOutcome{p.sockPath("slow"): {}})

	names, err := p.ListRunning("")
	if err != nil || !slices.Equal(names, []string{"slow"}) {
		t.Fatalf("ListRunning = %v, %v; want [slow], nil", names, err)
	}
}

// Kills: a nil error with a name missing, which reads as a death to every list
// consumer.
func TestACPListRunningDialFailureIsPartial(t *testing.T) {
	p := newProbeProvider(t)
	addSocketEntry(t, p, "live")
	addSocketEntry(t, p, "stuck")
	p.dial = fakeSocketDial(t, map[string]socketOutcome{
		p.sockPath("live"):  socketOK,
		p.sockPath("stuck"): {err: errDialTimeout},
	})

	names, err := p.ListRunning("")
	if !runtime.IsPartialListError(err) || !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("ListRunning err = %v, want a partial list wrapping ErrRuntimeUnavailable", err)
	}
	if !slices.Equal(names, []string{"live"}) {
		t.Fatalf("ListRunning names = %v, want [live]", names)
	}
}

// Kills: over-correcting, so a dead socket stays listed or makes the listing
// partial.
func TestACPListRunningRefusedSocketIsAbsent(t *testing.T) {
	p := newProbeProvider(t)
	addSocketEntry(t, p, "dead")
	p.dial = fakeSocketDial(t, map[string]socketOutcome{p.sockPath("dead"): {err: errDialRefused}})

	names, err := p.ListRunning("")
	if err != nil || len(names) != 0 {
		t.Fatalf("ListRunning = %v, %v; want none, nil", names, err)
	}
}

// Kills: the three-outcome liveness lost behind the seam adapter, which only
// has the bool read.
func TestACPSeamForwardsLivenessObserver(t *testing.T) {
	raw := newProbeProvider(t)
	raw.dial = fakeSocketDial(t, map[string]socketOutcome{raw.sockPath("worker"): {err: errDialTimeout}})
	var sp runtime.Provider = seamBack(raw)
	if _, ok := sp.(runtime.LivenessObserverWithError); !ok {
		t.Fatalf("%T does not implement runtime.LivenessObserverWithError", sp)
	}
	if _, err := runtime.ObserveLivenessWithError(sp, "worker", nil); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("ObserveLivenessWithError err = %v, want ErrRuntimeUnavailable", err)
	}
}

// Kills: an over-long socket path, where nothing can be bound and the dial
// fails with EINVAL, read as unknown. That left a dead long-named session
// unrestarted and a stale socket entry keeping the listing partial for good.
// The real dial is used; no listener is bound.
func TestACPOverlongSocketPathIsAbsent(t *testing.T) {
	dir := filepath.Join(shortTempDir(t), strings.Repeat("d", 110))
	p := NewProviderWithDir(dir, Config{})
	const name = "worker"
	if len(p.sockPath(name)) < len(syscall.RawSockaddrUnix{}.Path) {
		t.Fatalf("socket path %q is not over-long", p.sockPath(name))
	}
	addSocketEntry(t, p, name)

	if got, err := p.ObserveLivenessWithError(name, nil); err != nil || got.Running {
		t.Errorf("ObserveLivenessWithError = %+v, %v; want absent, nil", got, err)
	}
	if names, err := p.ListRunning(""); err != nil || len(names) != 0 {
		t.Errorf("ListRunning = %v, %v; want none, nil", names, err)
	}
	if err := p.Stop(name); err != nil {
		t.Errorf("Stop = %v, want nil", err)
	}
}

// Kills: a session this provider owns answered from its socket instead of its
// process.
func TestACPObserveLivenessPrefersOwnedProcess(t *testing.T) {
	p := newProbeProvider(t)
	exited := make(chan struct{})
	close(exited)
	p.conns["exited"] = &sessionConn{done: exited}
	p.conns["running"] = &sessionConn{done: make(chan struct{})}
	p.dial = fakeSocketDial(t, map[string]socketOutcome{p.sockPath("exited"): socketOK})

	if got, err := p.ObserveLivenessWithError("exited", nil); err != nil || got.Running {
		t.Errorf("exited: %+v, %v; want absent, nil", got, err)
	}
	if got, err := p.ObserveLivenessWithError("running", nil); err != nil || !got.Running {
		t.Errorf("running: %+v, %v; want present, nil", got, err)
	}
}

// Kills: Start taking a busy session's name (and removing its socket) because
// its owner did not answer the ping in time.
func TestACPStartRefusesConnectedSilentSocket(t *testing.T) {
	p := newProbeProvider(t)
	p.dial = fakeSocketDial(t, map[string]socketOutcome{p.sockPath("busy"): {}})
	err := p.Start(context.Background(), "busy", runtime.Config{Command: "true"})
	if !errors.Is(err, runtime.ErrSessionExists) {
		t.Fatalf("Start = %v, want ErrSessionExists", err)
	}
}

// Kills: a stale Stop of a dead connection removing the metadata of a busy
// replacement that did not answer the ping in time.
func TestACPStopKeepsSilentReplacementMeta(t *testing.T) {
	p := newProbeProvider(t)
	exited := make(chan struct{})
	close(exited)
	p.conns["worker"] = &sessionConn{cmd: &exec.Cmd{}, done: exited}
	if err := p.SetMeta("worker", "GC_SESSION_ID", "replacement"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	p.dial = fakeSocketDial(t, map[string]socketOutcome{p.sockPath("worker"): {}})

	if err := p.Stop("worker"); err != nil {
		t.Fatalf("Stop = %v, want nil", err)
	}
	if got, err := p.GetMeta("worker", "GC_SESSION_ID"); err != nil || got != "replacement" {
		t.Fatalf("GetMeta after Stop = %q, %v; want the replacement's value", got, err)
	}
}
