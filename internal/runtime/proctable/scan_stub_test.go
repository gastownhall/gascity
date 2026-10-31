//go:build !linux && !darwin

package proctable

import (
	"errors"
	"testing"
	"time"
)

// TestScanBySessionIDSinceFailsClosedForIncarnationBound pins that a platform
// with no process-table scanning refuses a bounded scan rather than certifying
// an absence it cannot prove: its empty inventory means "this platform cannot
// look", not "nothing is running".
func TestScanBySessionIDSinceFailsClosedForIncarnationBound(t *testing.T) {
	runtimes, err := ScanBySessionIDSince("s-test", time.Now())
	if !errors.Is(err, ErrIncarnationBoundUnsupported) {
		t.Fatalf("bounded ScanBySessionIDSince: err = %v, want ErrIncarnationBoundUnsupported", err)
	}
	if runtimes == nil || len(runtimes) != 0 {
		t.Fatalf("bounded ScanBySessionIDSince runtimes = %#v, want an empty non-nil slice alongside the error", runtimes)
	}
}

// TestScanBySessionIDSinceUnboundedStaysUnchanged pins that the refusal is
// scoped to a requested bound: a zero incarnationStartedAt is the unbounded
// scan and keeps returning an empty inventory with no error.
func TestScanBySessionIDSinceUnboundedStaysUnchanged(t *testing.T) {
	runtimes, err := ScanBySessionIDSince("s-test", time.Time{})
	if err != nil {
		t.Fatalf("unbounded ScanBySessionIDSince: err = %v, want nil", err)
	}
	if runtimes == nil || len(runtimes) != 0 {
		t.Fatalf("unbounded ScanBySessionIDSince runtimes = %#v, want an empty non-nil slice", runtimes)
	}
}
