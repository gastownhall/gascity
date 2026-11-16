//go:build windows

package herdr

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

func TestStopTerminatesPaneProcessTreeBeforeClosingPane(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	setState(t, state, "busy")
	bindTestPane(t, p, "gastown__witness", bindModeAgent)
	if err := p.SetMeta("gastown__witness", metaBoundShellPID, "4242"); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMeta("gastown__witness", metaBoundShellID, "987654321"); err != nil {
		t.Fatal(err)
	}

	origSnapshot := snapshotProcesses
	origKill := killPaneRuntime
	origIdentity := processIdentity
	t.Cleanup(func() {
		snapshotProcesses = origSnapshot
		killPaneRuntime = origKill
		processIdentity = origIdentity
	})
	snapshotProcesses = func() ([]proctable.ProcessRecord, error) {
		return []proctable.ProcessRecord{{PID: 4242, StartTime: "987654321", Name: "powershell.exe"}}, nil
	}
	processIdentity = func(pid int) (string, error) {
		if pid != 4242 {
			t.Fatalf("processIdentity(%d), want 4242", pid)
		}
		return "987654321", nil
	}
	var killed runtime.LiveRuntime
	killPaneRuntime = func(live runtime.LiveRuntime) error {
		killed = live
		if calls := fakeCalls(t, state); strings.Contains(calls, "pane close %5") {
			t.Fatalf("pane closed before process tree termination:\n%s", calls)
		}
		return nil
	}

	if err := p.Stop("gastown__witness"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if killed.PID != 4242 || killed.StartIdentity != "987654321" {
		t.Fatalf("terminated runtime = %+v; want PID 4242 with start identity", killed)
	}
	if calls := fakeCalls(t, state); !strings.Contains(calls, "pane close %5") {
		t.Fatalf("pane was not closed after tree termination:\n%s", calls)
	}
}
