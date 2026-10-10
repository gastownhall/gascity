package dolttest

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// recordingTB is a testing.TB whose Fatalf records its message and ends the
// calling goroutine, as the real one does, so a test can observe a helper's
// failure without failing itself.
type recordingTB struct {
	testing.TB
	fatal string
}

func (r *recordingTB) Fatalf(format string, args ...any) {
	r.fatal = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// failureOf runs fn on its own goroutine against a recordingTB and returns what
// fn passed to Fatalf, or "" when fn returned.
func failureOf(t *testing.T, fn func(tb testing.TB)) string {
	t.Helper()
	rec := &recordingTB{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(rec)
	}()
	<-done
	return rec.fatal
}

// setDuration sets *target for the rest of the test.
func setDuration(t *testing.T, target *time.Duration, value time.Duration) {
	t.Helper()
	original := *target
	*target = value
	t.Cleanup(func() { *target = original })
}

// useListenerPID replaces listenerPID for the rest of the test.
func useListenerPID(t *testing.T, replacement func(port int, candidates ...int) (int, bool)) {
	t.Helper()
	original := listenerPID
	listenerPID = replacement
	t.Cleanup(func() { listenerPID = original })
}

// fakeDolt writes an executable shell stand-in for the dolt binary and returns
// its path.
func fakeDolt(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-dolt.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("writing the stand-in server: %v", err)
	}
	return path
}

func TestStartSQLServerGivesUpWhenEveryProbedPortIsLost(t *testing.T) {
	// Dolt's own wording for a port it cannot bind, in a different case than
	// the matcher spells it.
	dolt := fakeDolt(t, `echo "Port $1 Already In Use." >&2; exit 1`)
	picks, attempts := 0, 0

	failure := failureOf(t, func(tb testing.TB) {
		StartSQLServer(tb, SQLServerSpec{
			Dolt: dolt,
			Args: func(port int) []string {
				attempts++
				return []string{strconv.Itoa(port)}
			},
			PickPort: func() (int, error) {
				picks++
				return 40000 + picks, nil
			},
		})
	})

	if !strings.Contains(failure, "lost its probed port") {
		t.Fatalf("failure %q does not say the probed port was lost", failure)
	}
	if picks != sqlServerMaxAttempts || attempts != sqlServerMaxAttempts {
		t.Fatalf("picked %d ports and started dolt %d times, want %d of each", picks, attempts, sqlServerMaxAttempts)
	}
}

func TestStartSQLServerReportsTheLogTailWhenTheServerExitsEarly(t *testing.T) {
	dolt := fakeDolt(t, `i=1; while [ "$i" -le 60 ]; do echo "log line $i" >&2; i=$((i + 1)); done; exit 3`)
	attempts := 0

	failure := failureOf(t, func(tb testing.TB) {
		StartSQLServer(tb, SQLServerSpec{
			Dolt: dolt,
			Args: func(port int) []string {
				attempts++
				return []string{strconv.Itoa(port)}
			},
		})
	})

	if !strings.Contains(failure, "exited before it listened") || !strings.Contains(failure, "exit status 3") {
		t.Fatalf("failure %q does not report an early exit with its status", failure)
	}
	if !strings.Contains(failure, "log line 60\n") && !strings.HasSuffix(failure, "log line 60") {
		t.Fatalf("failure %q does not end with the server log's last line", failure)
	}
	if !strings.Contains(failure, "log line 21\n") || strings.Contains(failure, "log line 20\n") {
		t.Fatalf("failure %q is not the last %d lines of the server log", failure, sqlServerLogTailLines)
	}
	if attempts != 1 {
		t.Fatalf("started dolt %d times; an exit that is not a lost port is not retried", attempts)
	}
}

func TestSQLServerStopKillsAProcessThatIgnoresSIGTERM(t *testing.T) {
	armed := filepath.Join(t.TempDir(), "armed")
	// The stand-in listens on nothing, so report it as the listener once it has
	// armed its trap: StartSQLServer then returns only after that.
	useListenerPID(t, func(_ int, candidates ...int) (int, bool) {
		if _, err := os.Stat(armed); err != nil {
			return 0, true
		}
		return candidates[0], true
	})
	setDuration(t, &sqlServerStopGrace, 200*time.Millisecond)

	srv := StartSQLServer(t, SQLServerSpec{
		Dolt: fakeDolt(t, `trap '' TERM; : > "$1"; exec sleep 300`),
		Args: func(int) []string { return []string{armed} },
	})
	srv.Stop()

	select {
	case <-srv.Exited:
	default:
		t.Fatal("Stop returned before the process that ignores SIGTERM had exited")
	}
	srv.Stop()
}
