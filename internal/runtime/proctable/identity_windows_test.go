//go:build windows

package proctable

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/pidutil"
	"github.com/gastownhall/gascity/internal/runtime"
)

// impossiblePID is a number the kernel can never have issued: Windows PIDs are
// multiples of four and far below this, so OpenProcess answers it with
// ERROR_INVALID_PARAMETER rather than a handle to somebody's live process.
const impossiblePID = 0x7FFFFFFF

func TestProcessIdentityIsStableForOneProcess(t *testing.T) {
	helper := startHelper(t, uniqueSessionID(t, "identity-stable"), t.TempDir())

	first, err := ProcessIdentity(helper.pid)
	if err != nil {
		t.Fatalf("ProcessIdentity(%d): %v", helper.pid, err)
	}
	time.Sleep(50 * time.Millisecond)
	second, err := ProcessIdentity(helper.pid)
	if err != nil {
		t.Fatalf("ProcessIdentity(%d) on the second read: %v", helper.pid, err)
	}
	if first != second {
		t.Fatalf("ProcessIdentity(%d) returned %q then %q; a live process's start identity must not move", helper.pid, first, second)
	}
	if first == "" {
		t.Fatal("ProcessIdentity returned an empty token; nothing can be fenced against it")
	}
}

func TestProcessIdentityDiffersBetweenProcesses(t *testing.T) {
	city := t.TempDir()
	first := startHelper(t, uniqueSessionID(t, "identity-a"), city)
	second := startHelper(t, uniqueSessionID(t, "identity-b"), city)

	if first.identity == second.identity {
		t.Fatalf("helpers %d and %d share the start identity %q; a recycled PID would be indistinguishable from the original", first.pid, second.pid, first.identity)
	}
}

// TestProcessIdentityMatchesPidutilStartTime pins the token to the one
// pidutil produces. Callers compare identities captured through either package,
// so a divergence would silently turn every fence into a mismatch.
func TestProcessIdentityMatchesPidutilStartTime(t *testing.T) {
	helper := startHelper(t, uniqueSessionID(t, "identity-pidutil"), t.TempDir())

	fromProctable, err := ProcessIdentity(helper.pid)
	if err != nil {
		t.Fatalf("ProcessIdentity(%d): %v", helper.pid, err)
	}
	fromPidutil, err := pidutil.StartTime(helper.pid)
	if err != nil {
		t.Fatalf("pidutil.StartTime(%d): %v", helper.pid, err)
	}
	if fromProctable != fromPidutil {
		t.Fatalf("proctable reports %q and pidutil reports %q for PID %d; the two must be interchangeable", fromProctable, fromPidutil, helper.pid)
	}
}

func TestProcessIdentityReportsGoneForAPIDThatCannotExist(t *testing.T) {
	_, err := ProcessIdentity(impossiblePID)
	if !errors.Is(err, ErrProcessGone) {
		t.Fatalf("ProcessIdentity(%d) = %v, want ErrProcessGone", impossiblePID, err)
	}
}

func TestProcessIdentityRejectsNonPositivePIDs(t *testing.T) {
	for _, pid := range []int{0, -1} {
		if _, err := ProcessIdentity(pid); err == nil {
			t.Errorf("ProcessIdentity(%d) succeeded; a non-positive PID names no process", pid)
		}
	}
}

// TestAReapedProcessStaysIdentifiableButReadsAsExited pins the Windows
// semantics the kill fence depends on.
//
// Windows keeps a process object — and therefore its PID — reserved while any
// handle still references it, so a reaped process can go on answering
// ProcessIdentity with the token it always had. That is not a stale read: while
// it holds, the number cannot have been reissued to anyone else. What changes
// is liveness, and the kill path reads liveness from the handle rather than
// from the identity, so an already-dead target is success rather than a
// termination.
func TestAReapedProcessStaysIdentifiableButReadsAsExited(t *testing.T) {
	helper := startHelper(t, uniqueSessionID(t, "identity-reaped"), t.TempDir())
	original := helper.identity

	helper.stop()
	if helper.running() {
		t.Fatal("the helper was not reaped")
	}

	identity, err := ProcessIdentity(helper.pid)
	switch {
	case errors.Is(err, ErrProcessGone):
		t.Logf("PID %d was released as soon as it was reaped", helper.pid)
	case err != nil:
		t.Fatalf("ProcessIdentity(%d) after reaping: %v", helper.pid, err)
	case identity != original:
		// The PID was released and reissued to something else entirely. There
		// is nothing this test may safely address at that number any more.
		t.Skipf("PID %d was reissued before it could be observed: identity %q, was %q", helper.pid, identity, original)
	}

	// The termination is fenced on the helper's ORIGINAL identity. Observing
	// the PID unused a moment ago does not reserve it, so KillByPID — which
	// fences on whatever holds the number when it runs — could be handed an
	// unrelated process here, and would kill it.
	if err := killProcessWithIdentity(helper.pid, original, runtime.ManagedProcessReapGrace); err != nil {
		if current, idErr := ProcessIdentity(helper.pid); idErr == nil && current != original {
			t.Skipf("PID %d was reissued during the test: identity %q, was %q", helper.pid, current, original)
		}
		t.Fatalf("killProcessWithIdentity(%d) on an already-reaped process = %v, want nil", helper.pid, err)
	}
}
