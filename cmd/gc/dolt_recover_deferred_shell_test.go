package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AC-7: a helper deferral reaches gc-beads-bd.sh as parseable deferred=true
// output; recover fails with the helper's reason and a deferral message and
// never runs the shell-only stop/start fallback.
func TestGcBeadsBdRecoverReportsHelperDeferralWithoutFallback(t *testing.T) {
	f := newFsyncGuardFixture(t, true)
	binDir := filepath.Dir(f.fakeGC)
	deferReason := "gc dolt-state recover-managed: managed dolt recovery deferred: endpoint 127.0.0.1 is listening; first unresponsive observation"
	fakeGC := fmt.Sprintf(`#!/bin/sh
call_log=%q
sub="$1 $2"
shift 2
case "$sub" in
  "dolt-state allocate-port")
    printf '%%s\n' "$FAKE_PORT"
    ;;
  "dolt-state fsync-probe")
    printf 'status\tfinished\nfsync_ms\t5\n'
    ;;
  "dolt-state recover-managed")
    printf 'recover-managed\n' >> "$call_log"
    printf 'diagnosed_read_only\tfalse\nhad_pid\tfalse\nforced\tfalse\nready\tfalse\nready_inconclusive\tfalse\npid\t0\nport\t%%s\nhealthy\tfalse\nrestarted\tfalse\ndeferred\ttrue\n' "$FAKE_PORT"
    printf '%%s\n' %q >&2
    exit 1
    ;;
  *)
    printf 'other %%s %%s\n' "$sub" "$*" >> "$call_log"
    exit 1
    ;;
esac
`, f.callLog, deferReason)
	if err := os.WriteFile(f.fakeGC, []byte(fakeGC), 0o755); err != nil {
		t.Fatal(err)
	}
	doltStub := fmt.Sprintf("#!/bin/sh\nprintf 'dolt %%s\\n' \"$*\" >> %q\nexit 1\n", f.callLog)
	if err := os.WriteFile(filepath.Join(binDir, "dolt"), []byte(doltStub), 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := f.recover(t, false)
	if err == nil {
		t.Fatalf("recover succeeded after a helper deferral; output:\n%s", out)
	}
	if !strings.Contains(out, deferReason) {
		t.Fatalf("recover output hides the helper's deferral reason:\n%s", out)
	}
	if !strings.Contains(out, "dolt recovery deferred by gc helper") {
		t.Fatalf("recover output missing the deferral message:\n%s", out)
	}
	calls := f.calls(t)
	if n := countLinesContaining(calls, "recover-managed"); n != 1 {
		t.Fatalf("recover-managed called %d times, want 1; calls:\n%s", n, calls)
	}
	for _, fallback := range []string{"dolt sql-server", "stop-managed", "start-managed"} {
		if strings.Contains(calls, fallback) {
			t.Fatalf("shell ran fallback %q after a helper deferral; calls:\n%s", fallback, calls)
		}
	}
}
