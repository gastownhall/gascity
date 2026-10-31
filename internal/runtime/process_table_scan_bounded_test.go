package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// scanBudget is the budget the tests hand FindRuntimesBySessionIDBounded. The
// tests run on synctest's fake clock, so it costs no wall time.
const scanBudget = 10 * time.Second

// blockedScanner is a ProcessTableScanner whose scan ignores everything and
// does not return until release is closed: a /proc walk stuck on a hung read.
type blockedScanner struct {
	release chan struct{}
	once    sync.Once
	started atomic.Int64
	result  []LiveRuntime
	err     error
}

func newBlockedScanner() *blockedScanner {
	return &blockedScanner{release: make(chan struct{})}
}

func (s *blockedScanner) FindRuntimesBySessionID(string) ([]LiveRuntime, error) {
	s.started.Add(1)
	<-s.release
	return s.result, s.err
}

func (s *blockedScanner) TerminateRuntime(LiveRuntime) error { return nil }

// unblock lets every scan return. It is idempotent so a test can both call it
// and defer it: the deferred call frees a wedged goroutine when a failed
// assertion ends the test early, which would otherwise deadlock the bubble.
func (s *blockedScanner) unblock() { s.once.Do(func() { close(s.release) }) }

// immediateScanner returns its canned answer at once.
type immediateScanner struct {
	started atomic.Int64
	result  []LiveRuntime
	err     error
}

func (s *immediateScanner) FindRuntimesBySessionID(string) ([]LiveRuntime, error) {
	s.started.Add(1)
	return s.result, s.err
}

func (s *immediateScanner) TerminateRuntime(LiveRuntime) error { return nil }

func TestFindRuntimesBySessionIDBoundedReturnsAnswerWithinBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		want := []LiveRuntime{{SessionID: "sid", PID: 7}}
		scanErr := errors.New("partial scan")
		sc := &immediateScanner{result: want, err: scanErr}

		got, err := FindRuntimesBySessionIDBounded(context.Background(), sc, "k-answer", "sid", scanBudget)

		// A scan that RETURNED with an error and roots is handed back
		// untouched: the caller keeps its fail-closed handling of it.
		if !errors.Is(err, scanErr) {
			t.Fatalf("err = %v, want the scan's own error", err)
		}
		if errors.Is(err, ErrProcessTableScanUnavailable) || errors.Is(err, ErrRuntimeUnavailable) {
			t.Fatalf("err = %v, a scan that returned must not read as unavailable", err)
		}
		if len(got) != 1 || got[0].PID != 7 {
			t.Fatalf("roots = %+v, want %+v", got, want)
		}
	})
}

func TestFindRuntimesBySessionIDBoundedExpiresAtBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := newBlockedScanner()
		defer sc.unblock()

		start := time.Now()
		got, err := FindRuntimesBySessionIDBounded(context.Background(), sc, "k-expire", "sid", scanBudget)

		if elapsed := time.Since(start); elapsed != scanBudget {
			t.Fatalf("returned after %s, want exactly the budget %s", elapsed, scanBudget)
		}
		if len(got) != 0 {
			t.Fatalf("roots = %+v, want none from a scan that never returned", got)
		}
		if !errors.Is(err, ErrProcessTableScanUnavailable) {
			t.Fatalf("err = %v, want ErrProcessTableScanUnavailable", err)
		}
		if !errors.Is(err, ErrRuntimeUnavailable) {
			t.Fatalf("err = %v, want it to wrap ErrRuntimeUnavailable", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want it to carry the deadline that ended the wait", err)
		}

		sc.unblock()
		synctest.Wait()
	})
}

func TestFindRuntimesBySessionIDBoundedCallerCancelIsUnavailable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := newBlockedScanner()
		defer sc.unblock()
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(scanBudget/2, cancel)

		_, err := FindRuntimesBySessionIDBounded(ctx, sc, "k-cancel", "sid", scanBudget)

		if !errors.Is(err, ErrProcessTableScanUnavailable) || !errors.Is(err, ErrRuntimeUnavailable) {
			t.Fatalf("err = %v, want unavailable", err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want it to carry context.Canceled", err)
		}

		sc.unblock()
		synctest.Wait()
	})
}

func TestFindRuntimesBySessionIDBoundedSecondCallWhileOutstandingStartsNoScan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := newBlockedScanner()
		defer sc.unblock()

		if _, err := FindRuntimesBySessionIDBounded(context.Background(), sc, "k-latch", "sid", scanBudget); !errors.Is(err, ErrProcessTableScanUnavailable) {
			t.Fatalf("first call err = %v, want unavailable", err)
		}

		start := time.Now()
		_, err := FindRuntimesBySessionIDBounded(context.Background(), sc, "k-latch", "sid", scanBudget)
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("second call waited %s, want an immediate refusal", elapsed)
		}
		if !errors.Is(err, ErrProcessTableScanUnavailable) || !errors.Is(err, ErrRuntimeUnavailable) {
			t.Fatalf("second call err = %v, want unavailable", err)
		}
		if n := sc.started.Load(); n != 1 {
			t.Fatalf("scans started = %d, want ONE for a wedged session across repeated calls", n)
		}

		sc.unblock()
		synctest.Wait()
	})
}

func TestFindRuntimesBySessionIDBoundedLatchIsPerKey(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wedged := newBlockedScanner()
		defer wedged.unblock()
		if _, err := FindRuntimesBySessionIDBounded(context.Background(), wedged, "k-wedged", "sid-a", scanBudget); !errors.Is(err, ErrProcessTableScanUnavailable) {
			t.Fatalf("wedged call err = %v, want unavailable", err)
		}

		other := &immediateScanner{result: []LiveRuntime{{SessionID: "sid-b", PID: 9}}}
		got, err := FindRuntimesBySessionIDBounded(context.Background(), other, "k-other", "sid-b", scanBudget)
		if err != nil || len(got) != 1 {
			t.Fatalf("other key = (%+v, %v), want its own scan to run unaffected", got, err)
		}

		wedged.unblock()
		synctest.Wait()
	})
}

func TestFindRuntimesBySessionIDBoundedKeyFreeAfterScanReturns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := newBlockedScanner()
		defer sc.unblock()
		if _, err := FindRuntimesBySessionIDBounded(context.Background(), sc, "k-free", "sid", scanBudget); !errors.Is(err, ErrProcessTableScanUnavailable) {
			t.Fatalf("first call err = %v, want unavailable", err)
		}

		// The abandoned scan finally returns: the latch releases with it and
		// nothing is left running.
		sc.unblock()
		synctest.Wait()

		fresh := &immediateScanner{result: []LiveRuntime{{SessionID: "sid", PID: 11}}}
		got, err := FindRuntimesBySessionIDBounded(context.Background(), fresh, "k-free", "sid", scanBudget)
		if err != nil || len(got) != 1 || got[0].PID != 11 {
			t.Fatalf("third call = (%+v, %v), want a fresh scan's answer", got, err)
		}
		if n := fresh.started.Load(); n != 1 {
			t.Fatalf("scans started after release = %d, want 1", n)
		}
	})
}

func TestFindRuntimesBySessionIDBoundedScanFinishingByBudgetWins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := newBlockedScanner()
		defer sc.unblock()
		sc.result = []LiveRuntime{{SessionID: "sid", PID: 21}}
		// The scan returns a tick before the budget expires: an answer that
		// arrives by the deadline is honored, not discarded as a timeout.
		time.AfterFunc(scanBudget-time.Nanosecond, sc.unblock)

		got, err := FindRuntimesBySessionIDBounded(context.Background(), sc, "k-edge", "sid", scanBudget)
		if err != nil {
			t.Fatalf("err = %v, want the answer that landed by the deadline", err)
		}
		if len(got) != 1 || got[0].PID != 21 {
			t.Fatalf("roots = %+v, want the scan's own answer", got)
		}
	})
}

func TestFindRuntimesBySessionIDBoundedLeavesNoGoroutineBehind(t *testing.T) {
	synctest.Test(t, func(_ *testing.T) {
		sc := newBlockedScanner()
		defer sc.unblock()
		_, _ = FindRuntimesBySessionIDBounded(context.Background(), sc, "k-leak", "sid", scanBudget)
		sc.unblock()

		// synctest.Test fails the bubble if a goroutine is still blocked when it
		// ends; Wait drains the released scan so only a real leak remains.
		synctest.Wait()
	})
}

func TestProcessTableScanUnavailableWrapsRuntimeUnavailable(t *testing.T) {
	if !errors.Is(ErrProcessTableScanUnavailable, ErrRuntimeUnavailable) {
		t.Fatal("ErrProcessTableScanUnavailable must wrap ErrRuntimeUnavailable")
	}
}

// panickingScanner's scan panics.
type panickingScanner struct{}

func (panickingScanner) FindRuntimesBySessionID(string) ([]LiveRuntime, error) {
	panic("reviewer probe: scanner blew up")
}

func (panickingScanner) TerminateRuntime(LiveRuntime) error { return nil }

func TestBoundedScanPanicIsUnavailableAndReleasesKey(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		got, err := FindRuntimesBySessionIDBounded(context.Background(), panickingScanner{}, "k-panic", "sid", scanBudget)
		if !errors.Is(err, ErrProcessTableScanUnavailable) || !errors.Is(err, ErrRuntimeUnavailable) {
			t.Fatalf("err = %v, want unavailable", err)
		}
		if len(got) != 0 {
			t.Fatalf("roots = %+v, want none", got)
		}
		synctest.Wait()

		fresh := &immediateScanner{result: []LiveRuntime{{SessionID: "sid", PID: 31}}}
		got, err = FindRuntimesBySessionIDBounded(context.Background(), fresh, "k-panic", "sid", scanBudget)
		if err != nil || len(got) != 1 || got[0].PID != 31 {
			t.Fatalf("after the panic = (%+v, %v), want a fresh scan's answer: the panicked scan must release its key", got, err)
		}
	})
}

// A scan that panics is loud: one error record names the latch key, carries
// the panic value, and has a stack, so a deterministic scanner bug that defers
// a session's starts on every attempt can be found from the log.
func TestBoundedScanPanicIsLoggedWithKeyValueAndStack(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	synctest.Test(t, func(t *testing.T) {
		if _, err := FindRuntimesBySessionIDBounded(context.Background(), panickingScanner{}, "k-panic-log", "sid", scanBudget); err == nil {
			t.Fatal("a panicking scan returned no error")
		}
		synctest.Wait()
	})

	var records []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("log line %q is not a slog JSON record: %v", line, err)
		}
		records = append(records, rec)
	}
	if len(records) != 1 {
		t.Fatalf("got %d log records %v, want exactly one error record for the panic", len(records), records)
	}
	rec := records[0]
	if rec["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", rec["level"])
	}
	if rec["key"] != "k-panic-log" {
		t.Errorf("key = %v, want the latch key k-panic-log", rec["key"])
	}
	if rec["panic"] != "reviewer probe: scanner blew up" {
		t.Errorf("panic = %v, want the panic value", rec["panic"])
	}
	if stack, _ := rec["stack"].(string); stack == "" {
		t.Errorf("stack is empty, want the panicking goroutine's stack: %v", rec)
	}
}
