package main

import (
	"strings"
	"testing"
)

// TestRepairedManagedDoltRuntimeStatePreservesStartedAt pins the
// adopt-vs-start distinction from ga-e5lyfu done-when item 5:
// repairedManagedDoltRuntimeState adopts the runtime-state record for a
// managed dolt process that is already running (discovered via port/PID
// introspection) rather than starting it, so it has no true knowledge of
// when that process actually started. It must never fabricate
// time.Now() as StartedAt — that produced a started_at off by days
// (claimed 2026-08-15T04:57:52Z for a pid that actually started
// 2026-08-11T11:08:38) during the 2026-08-10 incident this bead covers.
func TestRepairedManagedDoltRuntimeStatePreservesStartedAt(t *testing.T) {
	cityPath := t.TempDir()
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}

	port := reserveRandomTCPPort(t)
	listener := startTCPListenerProcessInDir(t, port, layout.DataDir)
	defer func() {
		_ = listener.Process.Kill()
		_ = listener.Wait()
	}()

	t.Run("preserves a real prior StartedAt", func(t *testing.T) {
		const want = "2020-01-01T00:00:00Z"
		repaired, ok := repairedManagedDoltRuntimeState("", layout, doltRuntimeState{
			Port:      port,
			DataDir:   layout.DataDir,
			StartedAt: want,
		})
		if !ok {
			t.Fatal("repairedManagedDoltRuntimeState(...) = not ok, want ok")
		}
		if repaired.StartedAt != want {
			t.Fatalf("repaired state StartedAt = %q, want unchanged %q", repaired.StartedAt, want)
		}
	})

	t.Run("does not fabricate a StartedAt when the input has none", func(t *testing.T) {
		repaired, ok := repairedManagedDoltRuntimeState("", layout, doltRuntimeState{
			Port:    port,
			DataDir: layout.DataDir,
		})
		if !ok {
			t.Fatal("repairedManagedDoltRuntimeState(...) = not ok, want ok")
		}
		if strings.TrimSpace(repaired.StartedAt) != "" {
			t.Fatalf("repaired state StartedAt = %q, want empty: adopting an already-running process is not a start, and its real start time is unknown here", repaired.StartedAt)
		}
	})
}
