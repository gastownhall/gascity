package tmux

import (
	"path/filepath"
	goruntime "runtime"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// A scan that surfaced no roots has nothing to ask tmux: no listing, no
// per-session meta read. The provider must issue no tmux command at all, so a
// wedged or slow scan of an absent session never queues behind tmux work.
func TestFindRuntimesBySessionIDWithNoRootsAsksTmuxNothing(t *testing.T) {
	writeTrackingProcRoot(t) // pid 4100 belongs to session gc-1, not gc-9
	p := NewProviderWithConfig(Config{SocketName: "x"})
	p.tm.exec = &fakeExecutor{}

	found, err := p.FindRuntimesBySessionID("gc-9")

	if len(found) != 0 {
		t.Fatalf("found %+v, want none for a session the scan does not know", found)
	}
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if calls := p.tm.exec.(*fakeExecutor).calls; len(calls) != 0 {
		t.Fatalf("tmux commands = %v, want none: a scan with no roots asks tmux nothing", calls)
	}
}

// A scan that failed outright with no roots still returns its scan error
// unchanged, and asks tmux nothing on the way out.
func TestFindRuntimesBySessionIDWithNoRootsStillReturnsScanError(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("drives the scanner against a procfs-shaped tree")
	}
	proctable.SetScanRootForTesting(t, filepath.Join(t.TempDir(), "missing"))
	p := NewProviderWithConfig(Config{SocketName: "x"})
	p.tm.exec = &fakeExecutor{}

	found, err := p.FindRuntimesBySessionID("gc-9")

	if len(found) != 0 {
		t.Fatalf("found %+v, want none", found)
	}
	if err == nil || !proctable.HasWholeScanFailure(err) {
		t.Fatalf("err = %v, want the whole-scan failure preserved", err)
	}
	if calls := p.tm.exec.(*fakeExecutor).calls; len(calls) != 0 {
		t.Fatalf("tmux commands = %v, want none: a failed scan with no roots asks tmux nothing", calls)
	}
}
