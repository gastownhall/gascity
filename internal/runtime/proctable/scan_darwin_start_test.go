//go:build darwin

package proctable

import (
	"os"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Kills: a darwin scan that leaves StartIdentity or StartedAt unset, fills
// them from anything but the kernel's start time, or invents them for a
// process whose identity cannot be read (CONTRACT C4: the sweep binds its
// kill to StartIdentity and bounds rule 2 on StartedAt).
func TestWithStartIdentitiesFillsKernelStartTime(t *testing.T) {
	self := os.Getpid()
	want, err := ProcessIdentity(self)
	if err != nil {
		t.Fatalf("ProcessIdentity(self): %v", err)
	}
	got := withStartIdentities([]runtime.LiveRuntime{{PID: self}, {PID: 1 << 30}})
	if got[0].StartIdentity != want {
		t.Fatalf("StartIdentity = %q, want ProcessIdentity's %q", got[0].StartIdentity, want)
	}
	if started := got[0].StartedAt; started.IsZero() || started.After(time.Now()) || time.Since(started) > 24*time.Hour {
		t.Fatalf("StartedAt = %v, want this test process's recent start", started)
	}
	if got[1].StartIdentity != "" || !got[1].StartedAt.IsZero() {
		t.Fatalf("gone pid filled as %+v, want empty", got[1])
	}
}
