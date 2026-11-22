package beads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNativeReadDeadline marks a native read that failed after reaching the
// server while the same pool's endpoint still answered a ping. The usual cause
// is a read that outlived the pool read deadline (BEADS_DOLT_POOL_READ_TIMEOUT):
// reopening against the same endpoint with the same deadline cannot make it
// finish, so the read loop returns instead of reconnecting.
var ErrNativeReadDeadline = errors.New("native Dolt read failed on an endpoint that still answers")

// nativeReadEndpointProbeBudget bounds the one ping that decides whether a
// failed read's endpoint still answers. It sits well under the default pool
// read deadline; a ping slower than it counts as "unproven", which reconnects
// as before. It is a var so tests can shorten it.
var nativeReadEndpointProbeBudget = 2 * time.Second

// nativeReadOutcomeError tags a read-loop error with one of the exported
// outcome sentinels. Its text is the wrapped error's text, byte for byte, and
// errors.Is/As reach both the sentinel and everything the error wraps.
type nativeReadOutcomeError struct {
	kind error
	err  error
}

func (e *nativeReadOutcomeError) Error() string { return e.err.Error() }

func (e *nativeReadOutcomeError) Unwrap() []error { return []error{e.kind, e.err} }

// newNativeReadDeadlineError is the outcome of a read that failed on a pool
// whose endpoint still answers.
func newNativeReadDeadlineError(readErr error) error {
	return &nativeReadOutcomeError{
		kind: ErrNativeReadDeadline,
		err: fmt.Errorf("native Dolt read failed but its endpoint still answers; not reconnecting "+
			"(a read that outlives the pool read deadline, BEADS_DOLT_POOL_READ_TIMEOUT, ends this way): %w", readErr),
	}
}

// endpointAnswers reports whether the store's current pool still answers one
// ping within nativeReadEndpointProbeBudget. Only a nil ping is evidence: a
// closed store, a handle that exports no pool, a ping error or an expired probe
// budget all report false, and the caller reconnects as it would
// without the probe. The ping goes straight to the library's *sql.DB, so it
// never runs the reopen hook or managed-Dolt recovery.
func (s *NativeDoltStore) endpointAnswers(ctx context.Context) bool {
	storage, _, release, err := s.acquireStorageGen()
	if err != nil {
		return false
	}
	defer release()
	accessor, ok := storage.(interface{ UnderlyingDB() *sql.DB })
	if !ok {
		return false
	}
	db := accessor.UnderlyingDB()
	if db == nil {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, nativeReadEndpointProbeBudget)
	defer cancel()
	return db.PingContext(probeCtx) == nil
}
