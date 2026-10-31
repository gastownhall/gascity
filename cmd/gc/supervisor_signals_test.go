package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/platform/localsocket"
)

// Windows graceful shutdown is driven by the supervisor's command socket.
// Console Ctrl+C is local foreground cancellation only, and nothing depends on
// SIGTERM or SIGHUP.
//
// signal.Notify cannot report that a platform will never deliver a signal, so
// a registration that is wrong is also silent. These cases are what makes it
// loud.

func TestSupervisorControlSignalsCoverShutdownOnEveryPlatform(t *testing.T) {
	got := supervisorControlSignals()
	for _, want := range []os.Signal{syscall.SIGINT, syscall.SIGTERM} {
		if !containsSignal(got, want) {
			t.Fatalf("supervisorControlSignals() = %v, missing %v", got, want)
		}
	}
}

// TestSupervisorReloadSignalMatchesPlatformDelivery pins the registration to
// what the OS can actually raise. Registering SIGHUP on Windows returns nil
// and then never fires, which would leave the supervisor advertising a reload
// path that does nothing.
func TestSupervisorReloadSignalMatchesPlatformDelivery(t *testing.T) {
	got := supervisorControlSignals()
	hint := supervisorReloadSignalHint()

	if goruntime.GOOS == "windows" {
		if containsSignal(got, syscall.SIGHUP) {
			t.Fatalf("supervisorControlSignals() = %v, registers SIGHUP on a platform that never raises it", got)
		}
		if !strings.Contains(hint, "gc supervisor reload") {
			t.Fatalf("supervisorReloadSignalHint() = %q, want it to name the replacement command", hint)
		}
		return
	}

	if !containsSignal(got, syscall.SIGHUP) {
		t.Fatalf("supervisorControlSignals() = %v, missing SIGHUP", got)
	}
	if hint != "" {
		t.Fatalf("supervisorReloadSignalHint() = %q, want empty where the signal exists", hint)
	}
}

// TestSupervisorReloadCommandIsTheSignalFreeReloadPath exercises the endpoint
// command Windows relies on instead of SIGHUP, through the same handler a real
// `gc supervisor reload` reaches. Without this, the platform split above would
// be pointing at a path nothing proves still works.
func TestSupervisorReloadCommandIsTheSignalFreeReloadPath(t *testing.T) {
	sockPath := filepath.Join(shortSocketTempDir(t, "gc-reload-"), "supervisor.sock")
	reconcileCh := make(chan reconcileRequest, 1)

	served := make(chan struct{})
	go func() {
		defer close(served)
		select {
		case req := <-reconcileCh:
			close(req.done)
		case <-time.After(10 * time.Second):
		}
	}()

	lis, err := startSupervisorSocket(sockPath, func(supervisorShutdownMode, shutdownTrigger) bool {
		t.Error("reload must not request shutdown")
		return false
	}, reconcileCh, newShutdownState())
	if err != nil {
		t.Fatalf("startSupervisorSocket: %v", err)
	}
	defer lis.Close() //nolint:errcheck // test cleanup

	conn, err := localsocket.Dial(sockPath, 5*time.Second)
	if err != nil {
		t.Fatalf("dial supervisor socket: %v", err)
	}
	defer conn.Close() //nolint:errcheck // test cleanup

	if _, err := conn.Write([]byte("reload\n")); err != nil {
		t.Fatalf("write reload: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(10 * time.Second)) //nolint:errcheck // bounded read

	var reply bytes.Buffer
	buf := make([]byte, 64)
	for {
		n, readErr := conn.Read(buf)
		reply.Write(buf[:n])
		if strings.Contains(reply.String(), "\n") {
			break
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			t.Fatalf("read reload reply: %v", readErr)
		}
	}
	if got := strings.TrimSpace(reply.String()); got != "ok" {
		t.Fatalf("reload reply = %q, want %q", got, "ok")
	}
	<-served
}

func containsSignal(list []os.Signal, want os.Signal) bool {
	for _, sig := range list {
		if sig == want {
			return true
		}
	}
	return false
}
