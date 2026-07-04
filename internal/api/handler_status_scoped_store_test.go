package api

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/processgroup/processgrouptest"
	"github.com/gastownhall/gascity/internal/session"
)

func newSessionBead(sessionName string) beads.Bead {
	return beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"state":        string(session.StateActive),
			"template":     "myrig/worker",
			"session_name": sessionName,
		},
	}
}

// TestStatusSessionSnapshotUsesScopedStoreWhenAvailable proves
// statusSessionSnapshot reads through state.ScopedStoreLike's result
// instead of the shared CityBeadStore when one is available — the shared
// store here carries a bead that must NOT show up in the snapshot.
func TestStatusSessionSnapshotUsesScopedStoreWhenAvailable(t *testing.T) {
	shared := beads.NewMemStore()
	if _, err := shared.Create(newSessionBead("shared-session")); err != nil {
		t.Fatalf("Create shared session bead: %v", err)
	}
	scoped := beads.NewMemStore()
	if _, err := scoped.Create(newSessionBead("scoped-session")); err != nil {
		t.Fatalf("Create scoped session bead: %v", err)
	}

	state := newFakeState(t)
	state.cityBeadStore = shared
	state.scopedStoreFn = func(context.Context, beads.Store) (beads.Store, error) {
		return scoped, nil
	}
	s := &Server{state: state}

	snapshot := s.statusSessionSnapshot(context.Background())

	if _, ok := snapshot.bySessionName["scoped-session"]; !ok {
		t.Errorf("snapshot missing scoped-session; bySessionName = %+v, want the scoped store's read used", snapshot.bySessionName)
	}
	if _, ok := snapshot.bySessionName["shared-session"]; ok {
		t.Error("snapshot contains shared-session, want the shared store bypassed in favor of the scoped one")
	}
}

// TestStatusSessionSnapshotFallsBackWhenScopedStoreUnavailable pins the
// pre-ga-cdmx6x behavior for non-bd-CLI-backed stores (native, file, mem):
// when ScopedStoreLike answers (nil, nil), the read goes through the
// shared store exactly as before.
func TestStatusSessionSnapshotFallsBackWhenScopedStoreUnavailable(t *testing.T) {
	shared := beads.NewMemStore()
	if _, err := shared.Create(newSessionBead("shared-session")); err != nil {
		t.Fatalf("Create shared session bead: %v", err)
	}

	state := newFakeState(t)
	state.cityBeadStore = shared
	// scopedStoreFn left nil: defaults to (nil, nil), matching the real
	// ScopedStoreLike's answer for a MemStore-backed shared store.
	s := &Server{state: state}

	snapshot := s.statusSessionSnapshot(context.Background())

	if _, ok := snapshot.bySessionName["shared-session"]; !ok {
		t.Errorf("snapshot missing shared-session; bySessionName = %+v, want fallback to the shared store", snapshot.bySessionName)
	}
}

// TestStatusSessionSnapshotSurfacesScopedStoreResolutionError proves a
// ScopedStoreLike failure (e.g. Dolt credential resolution) surfaces as a
// "sessions:" partial error instead of silently degrading or panicking.
func TestStatusSessionSnapshotSurfacesScopedStoreResolutionError(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	state.scopedStoreFn = func(context.Context, beads.Store) (beads.Store, error) {
		return nil, errors.New("resolving dolt credentials: boom")
	}
	s := &Server{state: state}

	snapshot := s.statusSessionSnapshot(context.Background())

	if len(snapshot.partialErrors) == 0 {
		t.Fatal("partialErrors empty, want the scoped-store resolution error surfaced")
	}
	joined := strings.Join(snapshot.partialErrors, "; ")
	if !strings.Contains(joined, "sessions:") || !strings.Contains(joined, "boom") {
		t.Fatalf("partialErrors = %v, want a sessions: ...boom entry", snapshot.partialErrors)
	}
}

// TestStatusSessionSnapshotKillsBdChildOnTimeout is the ga-cdmx6x
// regression test for the API-side call site: statusSessionSnapshot must
// bind whatever ScopedStoreLike hands back to a request-scoped ctx tightly
// enough that a slow bd child is killed instead of surviving past the
// caller's budget. ScopedStoreLike here builds a real ctx-bound BdStore
// (mirroring what cmd/gc's scopedStoreLike does in production) pointed at
// a fake `bd` that backgrounds a long sleep and records its PID, proving
// the ctx statusSessionSnapshot constructs actually propagates end to end.
func TestStatusSessionSnapshotKillsBdChildOnTimeout(t *testing.T) {
	processgrouptest.RequireRealProcessSignals(t)
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}

	oldTimeout := statusStoreReadTimeout
	statusStoreReadTimeout = 200 * time.Millisecond
	t.Cleanup(func() { statusStoreReadTimeout = oldTimeout })

	binDir := t.TempDir()
	pidFile := filepath.Join(binDir, "bd-child.pid")
	writeExecutableScopedTest(t, filepath.Join(binDir, "bd"), "#!/bin/sh\n"+
		"sleep 30 &\n"+
		"echo \"$!\" > "+pidFile+"\n"+
		"wait\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	state.scopedStoreFn = func(ctx context.Context, _ beads.Store) (beads.Store, error) {
		return beads.NewBdStore(t.TempDir(), beads.ExecCommandRunnerWithEnvContext(ctx, nil)), nil
	}
	s := &Server{state: state}

	start := time.Now()
	_ = s.statusSessionSnapshot(context.Background())
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("statusSessionSnapshot blocked %s; want bounded by statusStoreReadTimeout", elapsed)
	}

	childPid := waitForNonEmptyFileScopedTest(t, pidFile, 5*time.Second)
	for range 50 {
		if err := exec.Command("kill", "-0", childPid).Run(); err != nil {
			return // child is gone
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = exec.Command("kill", "-KILL", childPid).Run()
	t.Fatalf("bd child process %s survived statusSessionSnapshot's timeout", childPid)
}

// TestStatusListStoreWithTimeoutUsesScopedStoreWhenAvailable proves the
// per-rig work-count fallback (statusListStoreWithTimeout, reached when a
// store isn't Counter-capable) reads through state.ScopedStoreLike's
// result instead of the store it was handed, when one is available.
func TestStatusListStoreWithTimeoutUsesScopedStoreWhenAvailable(t *testing.T) {
	shared := beads.NewMemStore()
	if _, err := shared.Create(beads.Bead{Type: "task", Title: "shared work", Status: "open"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	scoped := beads.NewMemStore()
	if _, err := scoped.Create(beads.Bead{Type: "task", Title: "scoped work 1", Status: "open"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := scoped.Create(beads.Bead{Type: "task", Title: "scoped work 2", Status: "open"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	state := newFakeState(t)
	state.scopedStoreFn = func(context.Context, beads.Store) (beads.Store, error) {
		return scoped, nil
	}

	rows, err := statusListStoreWithTimeout(context.Background(), state, shared, beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("statusListStoreWithTimeout: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2 from the scoped store (the shared store has only 1)", len(rows))
	}
}

// TestStatusListStoreWithTimeoutFallsBackWhenScopedStoreUnavailable pins
// the pre-ga-cdmx6x behavior: when ScopedStoreLike answers (nil, nil), the
// read goes through the store it was handed, exactly as before.
func TestStatusListStoreWithTimeoutFallsBackWhenScopedStoreUnavailable(t *testing.T) {
	shared := beads.NewMemStore()
	if _, err := shared.Create(beads.Bead{Type: "task", Title: "shared work", Status: "open"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state := newFakeState(t)

	rows, err := statusListStoreWithTimeout(context.Background(), state, shared, beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("statusListStoreWithTimeout: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1 from the fallback store", len(rows))
	}
}

// TestStatusListStoreWithTimeoutSurfacesScopedStoreResolutionError proves
// a ScopedStoreLike failure surfaces as an error instead of silently
// falling back or panicking — statusStoreWorkCounts already formats
// whatever error this returns into a "rig %s work: ..." partial error.
func TestStatusListStoreWithTimeoutSurfacesScopedStoreResolutionError(t *testing.T) {
	state := newFakeState(t)
	state.scopedStoreFn = func(context.Context, beads.Store) (beads.Store, error) {
		return nil, errors.New("resolving dolt credentials: boom")
	}

	_, err := statusListStoreWithTimeout(context.Background(), state, beads.NewMemStore(), beads.ListQuery{AllowScan: true})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("statusListStoreWithTimeout error = %v, want it to contain the resolution error", err)
	}
}

// TestStatusListStoreWithTimeoutKillsBdChildOnTimeout is the ga-cdmx6x
// regression test for the per-rig work-count call site: mirrors
// TestStatusSessionSnapshotKillsBdChildOnTimeout, but through
// statusListStoreWithTimeout directly.
func TestStatusListStoreWithTimeoutKillsBdChildOnTimeout(t *testing.T) {
	processgrouptest.RequireRealProcessSignals(t)
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}

	oldTimeout := statusStoreReadTimeout
	statusStoreReadTimeout = 200 * time.Millisecond
	t.Cleanup(func() { statusStoreReadTimeout = oldTimeout })

	binDir := t.TempDir()
	pidFile := filepath.Join(binDir, "bd-child.pid")
	writeExecutableScopedTest(t, filepath.Join(binDir, "bd"), "#!/bin/sh\n"+
		"sleep 30 &\n"+
		"echo \"$!\" > "+pidFile+"\n"+
		"wait\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	state := newFakeState(t)
	state.scopedStoreFn = func(ctx context.Context, _ beads.Store) (beads.Store, error) {
		return beads.NewBdStore(t.TempDir(), beads.ExecCommandRunnerWithEnvContext(ctx, nil)), nil
	}

	start := time.Now()
	_, _ = statusListStoreWithTimeout(context.Background(), state, beads.NewMemStore(), beads.ListQuery{AllowScan: true})
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("statusListStoreWithTimeout blocked %s; want bounded by statusStoreReadTimeout", elapsed)
	}

	childPid := waitForNonEmptyFileScopedTest(t, pidFile, 5*time.Second)
	for range 50 {
		if err := exec.Command("kill", "-0", childPid).Run(); err != nil {
			return // child is gone
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = exec.Command("kill", "-KILL", childPid).Run()
	t.Fatalf("bd child process %s survived statusListStoreWithTimeout's timeout", childPid)
}

func writeExecutableScopedTest(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func waitForNonEmptyFileScopedTest(t *testing.T, path string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		data, err := os.ReadFile(path)
		if err == nil && len(strings.TrimSpace(string(data))) > 0 {
			return strings.TrimSpace(string(data))
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s to be written", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
