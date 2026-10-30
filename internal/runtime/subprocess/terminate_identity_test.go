package subprocess

import (
	"os"
	goruntime "runtime"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

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
	p := newTestProvider(t)
	if err := p.TerminateRuntime(runtime.LiveRuntime{SessionID: "gc-kill-bind", PID: self, StartIdentity: identity + "0"}); err != nil {
		t.Fatalf("TerminateRuntime(recycled identity) = %v, want nil (the scanned process is gone)", err)
	}
}
