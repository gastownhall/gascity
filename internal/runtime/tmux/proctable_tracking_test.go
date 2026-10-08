package tmux

import (
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// writeTrackingProcRoot writes one agent root of session gc-1, reparented to
// init, under a procfs-shaped root the scanner reads.
func writeTrackingProcRoot(t *testing.T) {
	t.Helper()
	if goruntime.GOOS != "linux" {
		t.Skip("drives the scanner against a procfs-shaped tree")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "4100")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	uid := strconv.Itoa(os.Geteuid())
	for name, body := range map[string]string{
		"environ": "GC_SESSION_ID=gc-1\x00",
		"stat":    "4100 (sleep) S 1" + strings.Repeat(" 0", 48),
		"comm":    "sleep\n",
		"status":  "Name:\tsleep\nUid:\t" + uid + "\t" + uid + "\t" + uid + "\t" + uid + "\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(proctable.SetScanRootForTesting(root))
}

// Kills: a tracking read that drops a failed per-session GC_SESSION_ID read.
// The process-table orphan sweep reaps only untracked roots, so a session
// whose ID could not be read must make the scan's tracking unknown (a whole
// scan failure), not quietly report its roots untracked (CONTRACT C4).
func TestFindRuntimesBySessionIDReportsTrackingFailure(t *testing.T) {
	writeTrackingProcRoot(t)
	for _, tc := range []struct {
		name        string
		outs        []string
		errs        []error
		wantTracked bool
		wantWhole   bool
	}{
		{name: "tracked", outs: []string{"worker", "GC_SESSION_ID=gc-1"}, wantTracked: true},
		{name: "other session", outs: []string{"worker", "GC_SESSION_ID=gc-2"}},
		{name: "per-session read fails", outs: []string{"worker", ""}, errs: []error{nil, errors.New("server busy")}, wantWhole: true},
		{name: "listing fails", errs: []error{errors.New("list timed out")}, wantWhole: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewProviderWithConfig(Config{SocketName: "x"})
			p.tm.exec = &fakeExecutor{outs: tc.outs, errs: tc.errs}
			found, err := p.FindRuntimesBySessionID("")
			if len(found) != 1 || found[0].PID != 4100 {
				t.Fatalf("found %+v, want the one root", found)
			}
			if found[0].IsTracked != tc.wantTracked {
				t.Errorf("IsTracked = %v, want %v", found[0].IsTracked, tc.wantTracked)
			}
			if got := proctable.HasWholeScanFailure(err); got != tc.wantWhole {
				t.Errorf("HasWholeScanFailure(%v) = %v, want %v", err, got, tc.wantWhole)
			}
		})
	}
}

// Kills: a TerminateRuntime that is not bound to the scanned process
// (CONTRACT C4). Its target is this test process under a start identity that
// is not its own, as a recycled PID would present: the bound kill finds the
// scanned process gone and signals nothing. An unbound kill would terminate
// the test binary, failing the run. proctable's
// TestKillByPIDIdentityRefusesRecycledPID kills a real child under its own
// identity.
func TestTerminateRuntimeBindsKillToScannedIdentity(t *testing.T) {
	if goruntime.GOOS != "linux" && goruntime.GOOS != "darwin" {
		t.Skip("process identity is read on linux and darwin")
	}
	self := os.Getpid()
	identity, err := proctable.ProcessIdentity(self)
	if err != nil {
		t.Fatalf("ProcessIdentity(self): %v", err)
	}
	p := NewProviderWithConfig(Config{SocketName: "x"})
	if err := p.TerminateRuntime(runtime.LiveRuntime{SessionID: "gc-kill-bind", PID: self, StartIdentity: identity + "0"}); err != nil {
		t.Fatalf("TerminateRuntime(recycled identity) = %v, want nil (the scanned process is gone)", err)
	}
}
