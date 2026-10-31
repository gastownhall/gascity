package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

// ErrProcessTableScanUnavailable reports that a process-table scan did not
// finish, so nothing is known about what is running. It wraps
// [ErrRuntimeUnavailable]: the caller got no answer, which is not the same as
// an empty one.
var ErrProcessTableScanUnavailable = fmt.Errorf("process-table scan did not finish: %w", ErrRuntimeUnavailable)

// outstandingScans holds the keys with a scan goroutine that has not returned.
// It keeps a session whose scan is wedged at one leaked goroutine instead of
// one per start attempt. Keys are removed when the scan returns.
var outstandingScans sync.Map // key string -> struct{}

type scanResult struct {
	found []LiveRuntime
	err   error
}

// FindRuntimesBySessionIDBounded runs sc.FindRuntimesBySessionID on its own
// goroutine and waits for it no longer than budget (or ctx) allows. A /proc
// walk stuck on a hung read cannot be canceled, so on expiry the scan is
// abandoned and the caller gets no roots and an error wrapping
// [ErrProcessTableScanUnavailable] and the reason the wait ended. A scan that
// returns by the deadline, including at the instant it expires, is honored: its
// roots and error pass through unchanged.
//
// At most one scan per key is in flight. While one is, a new call under that
// key returns the same error at once without starting another. The key is
// released when the scan returns. Pick the key to name what is scanned (one
// session in one city).
func FindRuntimesBySessionIDBounded(ctx context.Context, sc ProcessTableScanner, key, sessionID string, budget time.Duration) ([]LiveRuntime, error) {
	if _, outstanding := outstandingScans.LoadOrStore(key, struct{}{}); outstanding {
		return nil, fmt.Errorf("scan for %q is still running from an earlier attempt: %w", sessionID, ErrProcessTableScanUnavailable)
	}
	wctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	// Buffered, so a scan that finishes after its caller gave up delivers into
	// the buffer and exits instead of blocking forever.
	results := make(chan scanResult, 1)
	go func() {
		res := scanResult{err: fmt.Errorf("scan for %q ended without an answer: %w", sessionID, ErrProcessTableScanUnavailable)}
		defer func() {
			if r := recover(); r != nil {
				res = scanResult{err: fmt.Errorf("scan for %q panicked: %v: %w", sessionID, r, ErrProcessTableScanUnavailable)}
				slog.Error("runtime: bounded process-table scan panicked",
					slog.String("key", key), slog.Any("panic", r), slog.String("stack", string(debug.Stack())))
			}
			// Release the key before delivering, so a caller that scans again
			// the moment it has an answer is never told its own finished scan
			// is outstanding.
			outstandingScans.Delete(key)
			results <- res
		}()
		res.found, res.err = sc.FindRuntimesBySessionID(sessionID)
	}()

	select {
	case res := <-results:
		return res.found, res.err
	case <-wctx.Done():
		select {
		case res := <-results:
			return res.found, res.err
		default:
		}
		return nil, fmt.Errorf("scan for %q did not finish within %s: %w: %w", sessionID, budget, ErrProcessTableScanUnavailable, wctx.Err())
	}
}
