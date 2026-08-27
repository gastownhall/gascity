//go:build integration

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// drainFinalizeLiveReadBudget is the window the keyed drain-finalize guard
// lives inside. The journey gives finalization 15s; a live read that has not
// observed a durably committed foreign close well inside that window exhausts
// the whole budget (ga-f7v2ft.131).
const drainFinalizeLiveReadBudget = 10 * time.Second

// TestKeyedDrainFinalizeLiveReadObservesForeignProcessClose is the ga-f7v2ft.131
// repro at the exact layer the guard reads through in production.
//
// Shape (identical to the journey's routed_work_drain_finalize leg):
//   - the city's beads scope is a managed Dolt server (BEADS_DOLT_AUTO_START),
//   - the controller holds ONE long-lived native Dolt store for that scope and
//     has already read the routed trigger while it was open,
//   - a SEPARATE process (the bd CLI — exactly what the journey test's own store
//     handle is) durably closes that trigger,
//   - the guard's next live read must observe the close.
func TestKeyedDrainFinalizeLiveReadObservesForeignProcessClose(t *testing.T) {
	bdPath := strings.TrimSpace(os.Getenv("GC_TEST_BD_BIN"))
	if bdPath == "" {
		t.Skip("GC_TEST_BD_BIN is not set to a real bd binary")
	}
	bdPath, err := filepath.Abs(bdPath)
	if err != nil {
		t.Fatalf("resolve GC_TEST_BD_BIN: %v", err)
	}
	if info, statErr := os.Stat(bdPath); statErr != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		t.Fatalf("GC_TEST_BD_BIN %q is not an executable file: info=%v err=%v", bdPath, info, statErr)
	}
	shimDir := t.TempDir()
	if err := os.Symlink(bdPath, filepath.Join(shimDir, "bd")); err != nil {
		t.Fatalf("install bd PATH shim: %v", err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BEADS_DOLT_AUTO_START", "1")

	cityPath := t.TempDir()
	cleanupManagedDoltTestCity(t, cityPath)
	beadsDir := filepath.Join(cityPath, ".beads")
	scopeEnv := map[string]string{
		"BEADS_DIR":             beadsDir,
		"BEADS_DOLT_AUTO_START": "1",
		"BD_BIN":                bdPath,
	}
	initManagedDoltBeadsScope(t, cityPath, scopeEnv, "dl")

	env, err := nativeDoltOpenEnvForScope(cityPath, nil, cityPath)
	if err != nil {
		t.Fatalf("project native store env for %q: %v", cityPath, err)
	}
	reader, err := beads.OpenNativeDoltStoreAt(context.Background(), cityPath, env)
	if err != nil {
		t.Skipf("native Dolt store unavailable for %q: %v", cityPath, err)
	}
	t.Cleanup(func() {
		if err := reader.CloseStore(); err != nil {
			t.Logf("close native reader: %v", err)
		}
	})

	writer := beads.NewBdStoreWithPrefix(cityPath, beads.ExecCommandRunnerWithEnv(scopeEnv), "dl")
	work, err := writer.Create(beads.Bead{
		Title:    "routed trigger for keyed drain finalize",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create routed trigger through bd: %v", err)
	}

	// The guard has read this trigger many times before the worker closes it.
	primed, err := reader.Get(work.ID)
	if err != nil {
		t.Fatalf("controller store primes trigger %q: %v", work.ID, err)
	}
	if primed.Status != "open" {
		t.Fatalf("primed trigger status = %q, want open", primed.Status)
	}

	if err := writer.Close(work.ID); err != nil {
		t.Fatalf("bd closes trigger %q: %v", work.ID, err)
	}
	writerView, err := writer.Get(work.ID)
	if err != nil || writerView.Status != "closed" {
		t.Fatalf("bd view after close = %+v err=%v, want closed", writerView, err)
	}

	closedAt := time.Now()
	deadline := closedAt.Add(drainFinalizeLiveReadBudget)
	for {
		got, getErr := beads.HandlesFor(reader).Live.Get(work.ID)
		if getErr != nil {
			t.Fatalf("controller live read of trigger %q: %v", work.ID, getErr)
		}
		if got.Status == "closed" {
			t.Logf("controller live read observed the foreign close after %s", time.Since(closedAt))
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("controller live read still sees status=%q for %q %s after bd's durable close; "+
				"the drain-finalize guard must observe a commit the writer completed (ga-f7v2ft.131)",
				got.Status, work.ID, drainFinalizeLiveReadBudget)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// initManagedDoltBeadsScope initializes a bd scope backed by a managed Dolt
// server, which is what every real city runs and what the journey's city runs.
func initManagedDoltBeadsScope(t *testing.T, scopeRoot string, env map[string]string, prefix string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 240*time.Second)
	defer cancel()
	// bd resolves its scope through the enclosing git repository.
	gitInit := exec.CommandContext(ctx, "git", "init", "--quiet", scopeRoot)
	gitInit.Dir = scopeRoot
	if out, err := gitInit.CombinedOutput(); err != nil {
		t.Fatalf("git init %q: %v\n%s", scopeRoot, err, out)
	}
	runner := beads.ExecCommandRunnerWithEnvContext(ctx, env)
	if out, err := runner(scopeRoot, "bd", "init", "--prefix", prefix); err != nil {
		t.Fatalf("bd init %q: %v\n%s", scopeRoot, err, out)
	}
	out, err := runner(scopeRoot, "bd", "context", "--json")
	if err != nil {
		t.Fatalf("bd context %q: %v\n%s", scopeRoot, err, out)
	}
	t.Logf("bd context for %q: %s", scopeRoot, strings.TrimSpace(string(out)))
}
