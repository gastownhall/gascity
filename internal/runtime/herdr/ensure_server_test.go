//go:build !windows

package herdr

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// serverFixture is a client whose server launch appends one byte to a file and
// whose readiness dial succeeds once readyAt() reports true.
type serverFixture struct {
	c      *client
	launch string
}

func newServerFixture(t *testing.T, ready func() bool) *serverFixture {
	t.Helper()
	dir := t.TempDir()
	f := &serverFixture{launch: filepath.Join(dir, "launches")}
	bin := writeFakeHerdr(t, "#!/bin/sh\nprintf x >> '"+f.launch+"'\n")
	f.c = &client{session: "gc-ensure-test", bin: bin, cityRoot: dir, sockPath: filepath.Join(dir, "herdr.sock")}
	f.c.readyAttempts, f.c.readyInterval = 5, 20*time.Millisecond
	f.c.dialUnix = func(context.Context, string) (net.Conn, error) {
		if !ready() {
			return nil, errors.New("nothing is listening")
		}
		server, client := net.Pipe()
		t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
		return client, nil
	}
	return f
}

// launches counts server launches.
func (f *serverFixture) launches(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile(f.launch)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return len(b)
}

// AT-H8: N concurrent callers launch the server exactly once, and every one of
// them sees the same outcome.
func TestEnsureServerConcurrentCallersLaunchOnce(t *testing.T) {
	var f *serverFixture
	f = newServerFixture(t, func() bool { return f.launches(t) > 0 })
	f.c.readyAttempts = 50
	errs := runEnsure(context.Background(), f.c, 8)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if n := f.launches(t); n != 1 {
		t.Fatalf("server launched %d times; want 1", n)
	}
}

// AT-H8: a server that never becomes ready fails every waiter with the same
// error, and nobody relaunches.
func TestEnsureServerNeverReadyFailsAllWaitersOnce(t *testing.T) {
	f := newServerFixture(t, func() bool { return false })
	errs := runEnsure(context.Background(), f.c, 4)
	for i, err := range errs {
		if err == nil {
			t.Fatalf("caller %d: nil error for a server that never became ready", i)
		}
		if err.Error() != errs[0].Error() {
			t.Fatalf("caller %d: %q differs from caller 0: %q", i, err, errs[0])
		}
	}
	if n := f.launches(t); n != 1 {
		t.Fatalf("server launched %d times; want 1 (no relaunch per waiter)", n)
	}
}

// AT-H8: a waiter whose own ctx ends returns its own ctx error without failing
// the others, and the flight it leaves keeps going to readiness.
func TestEnsureServerShortCtxWaiterDoesNotFailOthers(t *testing.T) {
	var readyAfter time.Time
	f := newServerFixture(t, func() bool { return !readyAfter.IsZero() && time.Now().After(readyAfter) })
	readyAfter = time.Now().Add(400 * time.Millisecond)
	f.c.readyAttempts = 50

	short, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var wg sync.WaitGroup
	var shortErr, longErr error
	var shortTook time.Duration
	wg.Add(2)
	go func() {
		defer wg.Done()
		start := time.Now()
		shortErr = f.c.ensureServer(short)
		shortTook = time.Since(start)
	}()
	go func() { defer wg.Done(); longErr = f.c.ensureServer(context.Background()) }()
	wg.Wait()

	if !errors.Is(shortErr, context.DeadlineExceeded) {
		t.Fatalf("short-ctx waiter = %v; want its own context.DeadlineExceeded", shortErr)
	}
	if shortTook > 300*time.Millisecond {
		t.Fatalf("short-ctx waiter returned after %v; want about its own 100ms", shortTook)
	}
	if longErr != nil {
		t.Fatalf("long-ctx waiter = %v; a sibling's ctx expiry must not fail it", longErr)
	}
	if n := f.launches(t); n != 1 {
		t.Fatalf("server launched %d times; want 1", n)
	}
}

// runEnsure runs n concurrent ensureServer calls under ctx and returns their errors.
func runEnsure(ctx context.Context, c *client, n int) []error {
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = c.ensureServer(ctx) }()
	}
	wg.Wait()
	return errs
}
