//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/doctor"
)

// Dolt persistence reproducer suite.
//
// These tests reproduce the destructive auto-import behavior reported in
// gastownhall/gascity issues #2079, #2080, #2081, #2093, #2094, and #2131.
// The persistence failure reduced to one mechanism in affected bd versions:
// server-mode, non-read-only bd subprocesses could auto-import
// .beads/issues.jsonl into Dolt as a blanket overwrite, with no merge or
// freshness check. Combined with the export.auto: false setting that Gas City
// intentionally uses for managed Dolt-backed stores, JSONL goes stale and a
// later bd write can revert Dolt mutations that occurred since the last export.
//
// Each test performs a sequence of writes in a managed-Dolt workspace with
// automatic JSONL export disabled, then asserts that authoritative Dolt state
// still contains the earlier write. They fail against bd versions with the
// destructive startup import bug and pass once that import path is guarded.

var doltPersistenceWorkspaceCounter atomic.Int64

// realBdRunner returns a beads.CommandRunner that resolves "bd" to the
// real bd binary (via realBDBinary, set in integration_test.go) rather
// than the file-shim used by the conformance suite. envOverrides is
// merged on top of the inherited environment so each test can mirror
// gascity's prod settings (BD_EXPORT_AUTO=false, BEADS_DOLT_AUTO_START=0).
//
// The runner logs one timing line for every bd subprocess. These tests are the
// guardrail for the Dolt persistence contract, and the timing lines make any
// fix that adds expensive subprocesses or export work visible in `go test -v`
// output without baking in brittle machine-specific thresholds.
func realBdRunner(t testing.TB, envOverrides map[string]string) beads.CommandRunner {
	t.Helper()
	base := beads.ExecCommandRunnerWithEnv(envOverrides)
	return func(dir, name string, args ...string) ([]byte, error) {
		displayName := name
		if name == "bd" && realBDBinary != "" {
			name = realBDBinary
		}
		start := time.Now()
		out, err := base(dir, name, args...)
		logSubprocessTiming(t, append([]string{displayName}, args...), start, err)
		return out, err
	}
}

// doltPersistenceWorkspace bundles the per-test bd workspace + the store handle
// configured to look like a gascity-managed city.
type doltPersistenceWorkspace struct {
	dir    string
	prefix string
	port   string
	env    []string
	store  *beads.BdStore
}

// newDoltPersistenceWorkspace builds a fresh bd workspace where bd manages its
// own embedded Dolt server (via `bd init --server`). The workspace starts in
// the "permissive" state with export.auto=true so initial writes populate
// .beads/issues.jsonl. After setup, callers must invoke
// freezeJSONLAndDisableAutoExport on the workspace to lock in the canonical
// Gas City managed-city stance (export.auto=false plus BD_EXPORT_AUTO=false on
// every subsequent runner invocation). At that point Dolt is the only live
// authority, and JSONL is only a stale snapshot. Affected bd versions violated
// that rule by importing the snapshot before later writes.
func newDoltPersistenceWorkspace(t *testing.T, env []string, doltPort string) *doltPersistenceWorkspace {
	t.Helper()

	n := doltPersistenceWorkspaceCounter.Add(1)
	prefix := fmt.Sprintf("pc%d", n)
	wsRoot := t.TempDir()
	wsDir := filepath.Join(wsRoot, fmt.Sprintf("ws-%d", n))
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatalf("creating workspace: %v", err)
	}

	gitCmd := exec.Command("git", "init", "--quiet")
	gitCmd.Dir = wsDir
	gitCmd.Env = env
	if out, err := gitCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	runRealBDInit(t, env, wsDir, prefix, doltPort)
	configureCustomTypesReal(t, env, wsDir, doctor.RequiredCustomTypes)

	// Permissive runner: matches the bd default. Initial Create()s on this
	// runner auto-export to JSONL, giving us a populated baseline to freeze.
	permissive := realBdRunner(t, nil)

	return &doltPersistenceWorkspace{
		dir:    wsDir,
		prefix: prefix,
		port:   doltPort,
		env:    env,
		store:  beads.NewBdStoreWithPrefix(wsDir, permissive, prefix),
	}
}

// freezeJSONLAndDisableAutoExport switches the workspace from the
// permissive setup mode to gascity's canonical managed-city stance:
//   - .beads/config.yaml has export.auto: false (persists across bd calls)
//   - every subsequent CommandRunner invocation also sets BD_EXPORT_AUTO=false
//   - JSONL is locked at the state captured by `bd export` here, so any
//     later mutation drifts JSONL out of sync with Dolt
//
// In affected bd versions, this setup loaded the destructive startup import
// path: the next non-read-only bd write subprocess treated the stale JSONL
// snapshot as authoritative and overwrote Dolt with it.
func (ws *doltPersistenceWorkspace) freezeJSONLAndDisableAutoExport(t *testing.T) {
	t.Helper()
	runRealBDExportAll(t, ws.env, ws.dir)
	runRealBDConfigSet(t, ws.env, ws.dir, "export.auto", "false")

	frozen := realBdRunner(t, map[string]string{
		"BD_EXPORT_AUTO": "false",
	})
	ws.store = beads.NewBdStoreWithPrefix(ws.dir, frozen, ws.prefix)
}

// runRealBDInit initializes a bd workspace against the real bd binary
// (not the file shim) so its data lands in Dolt and the auto-import code
// path is the one under test.
func runRealBDInit(t *testing.T, env []string, dir, prefix, port string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), bdInitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, realBDBinary, "init", "--server",
		"--server-host", "127.0.0.1", "--server-port", port, "-p", prefix,
		"--skip-hooks", "--skip-agents")
	cmd.Dir = dir
	cmd.Env = env
	start := time.Now()
	out, err := cmd.CombinedOutput()
	logSubprocessTiming(t, []string{"bd", "init", "--server"}, start, errorsForTiming(ctx.Err(), err))
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("bd init timed out: %s", out)
	}
	if err != nil {
		t.Fatalf("bd init: %v: %s", err, out)
	}
}

func runRealBDConfigSet(t *testing.T, env []string, dir, key, value string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), bdInitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, realBDBinary, "config", "set", key, value)
	cmd.Dir = dir
	cmd.Env = env
	start := time.Now()
	out, err := cmd.CombinedOutput()
	logSubprocessTiming(t, []string{"bd", "config", "set", key, value}, start, errorsForTiming(ctx.Err(), err))
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("bd config set %s=%s timed out: %s", key, value, out)
	}
	if err != nil {
		t.Fatalf("bd config set %s=%s: %v: %s", key, value, err, out)
	}
}

// runRealBDExportAll captures every issue (including infra beads) into
// .beads/issues.jsonl. Used to deliberately populate JSONL before the
// test freezes it, mirroring the production state where JSONL was
// populated by an earlier bd run and is now stale.
func runRealBDExportAll(t *testing.T, env []string, dir string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), bdInitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, realBDBinary, "export", "-o", ".beads/issues.jsonl", "--all")
	cmd.Dir = dir
	cmd.Env = env
	start := time.Now()
	out, err := cmd.CombinedOutput()
	logSubprocessTiming(t, []string{"bd", "export", "-o", ".beads/issues.jsonl", "--all"}, start, errorsForTiming(ctx.Err(), err))
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("bd export timed out: %s", out)
	}
	if err != nil {
		t.Fatalf("bd export: %v: %s", err, out)
	}
}

func configureCustomTypesReal(t *testing.T, env []string, dir string, types []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), bdInitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, realBDBinary, "config", "set", "types.custom", strings.Join(types, ","))
	cmd.Dir = dir
	cmd.Env = env
	start := time.Now()
	out, err := cmd.CombinedOutput()
	logSubprocessTiming(t, []string{"bd", "config", "set", "types.custom", strings.Join(types, ",")}, start, errorsForTiming(ctx.Err(), err))
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("bd config set types.custom timed out: %s", out)
	}
	if err != nil {
		t.Fatalf("bd config set types.custom: %v: %s", err, out)
	}
}

// stringPtr returns a pointer to s. Helper for UpdateOpts fields.
func stringPtr(s string) *string { return &s }

func errorsForTiming(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func logSubprocessTiming(t testing.TB, cmd []string, start time.Time, err error) {
	t.Helper()
	status := "ok"
	if err != nil {
		status = "error"
	}
	t.Logf("timing: bd subprocess status=%s duration=%s cmd=%q",
		status, roundDuration(time.Since(start)), strings.Join(cmd, " "))
}

func roundDuration(d time.Duration) time.Duration {
	if d >= time.Second {
		return d.Round(10 * time.Millisecond)
	}
	if d >= time.Millisecond {
		return d.Round(time.Millisecond)
	}
	return d
}

func timedCreate(t testing.TB, store *beads.BdStore, name string, b beads.Bead) (beads.Bead, time.Duration) {
	t.Helper()
	start := time.Now()
	created, err := store.Create(b)
	elapsed := time.Since(start)
	logStoreTiming(t, name, elapsed, err)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return created, elapsed
}

func logStoreTiming(t testing.TB, name string, elapsed time.Duration, err error) {
	t.Helper()
	status := "ok"
	if err != nil {
		status = "error"
	}
	t.Logf("timing: store operation status=%s duration=%s op=%q",
		status, roundDuration(elapsed), name)
}

func logDurationSummary(t testing.TB, label string, durations []time.Duration) {
	t.Helper()
	if len(durations) == 0 {
		t.Logf("timing-summary: %s count=0", label)
		return
	}
	sorted := append([]time.Duration(nil), durations...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var total time.Duration
	for _, d := range sorted {
		total += d
	}
	pick := func(percentile float64) time.Duration {
		if len(sorted) == 1 {
			return sorted[0]
		}
		idx := int(float64(len(sorted)-1) * percentile)
		return sorted[idx]
	}
	t.Logf("timing-summary: %s count=%d min=%s p50=%s p95=%s max=%s total=%s avg=%s",
		label,
		len(sorted),
		roundDuration(sorted[0]),
		roundDuration(pick(0.50)),
		roundDuration(pick(0.95)),
		roundDuration(sorted[len(sorted)-1]),
		roundDuration(total),
		roundDuration(total/time.Duration(len(sorted))),
	)
}

// dumpDoltPersistenceState reads the bd-side view of beadID plus the on-disk
// JSONL fragment for it, returning a single multi-line string suitable
// for t.Logf. Use when an assertion fails so the diagnostic is captured
// in CI artifacts without re-running the failing test.
func dumpDoltPersistenceState(ws *doltPersistenceWorkspace, beadID string) string {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "workspace=%s prefix=%s port=%s\n", ws.dir, ws.prefix, ws.port)

	if bead, err := ws.store.Get(beadID); err == nil {
		raw, _ := json.MarshalIndent(bead, "", "  ")
		fmt.Fprintf(&buf, "bd view of %s:\n%s\n", beadID, raw)
	} else {
		fmt.Fprintf(&buf, "bd view of %s: error: %v\n", beadID, err)
	}

	jsonlPath := filepath.Join(ws.dir, ".beads", "issues.jsonl")
	if data, err := os.ReadFile(jsonlPath); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, beadID) {
				fmt.Fprintf(&buf, "jsonl line: %s\n", line)
			}
		}
		if buf.Len() == 0 {
			fmt.Fprintf(&buf, "jsonl had no line for %s (file size=%d)\n", beadID, len(data))
		}
	} else {
		fmt.Fprintf(&buf, "reading jsonl: %v\n", err)
	}
	return buf.String()
}

// withDoltPersistenceFixture runs fn against a fresh workspace sharing one
// Dolt server across all subtests of a top-level Test. Centralizing
// fixture wiring keeps each subtest focused on the write sequence.
func withDoltPersistenceFixture(t *testing.T, fn func(ws *doltPersistenceWorkspace)) {
	t.Helper()
	requireDoltIntegration(t)
	env := newIsolatedToolEnv(t, true)

	doltDataDir := filepath.Join(t.TempDir(), "dolt")
	port := startSharedDoltServer(t, env, doltDataDir)

	ws := newDoltPersistenceWorkspace(t, env, port)
	fn(ws)
}

// TestDoltPersistence_CloseStatusSurvivesSubsequentBdWrite reproduces #2079:
// a close-status write lands in Dolt and must remain closed after a later bd
// write runs with managed-city JSONL export suppressed.
func TestDoltPersistence_CloseStatusSurvivesSubsequentBdWrite(t *testing.T) {
	withDoltPersistenceFixture(t, func(ws *doltPersistenceWorkspace) {
		target, err := ws.store.Create(beads.Bead{Title: "target bead"})
		if err != nil {
			t.Fatalf("create target: %v", err)
		}
		ws.freezeJSONLAndDisableAutoExport(t)

		if err := ws.store.Update(target.ID, beads.UpdateOpts{Status: stringPtr("closed")}); err != nil {
			t.Fatalf("close target: %v", err)
		}

		// Sanity: the close landed before we provoke the second bd call.
		got, err := ws.store.Get(target.ID)
		if err != nil {
			t.Fatalf("get after close: %v", err)
		}
		if got.Status != "closed" {
			t.Fatalf("precondition: target.Status = %q, want %q", got.Status, "closed")
		}

		// Provoke the affected path: a later bd write command must not erase
		// the earlier Dolt state.
		if _, err := ws.store.Create(beads.Bead{Title: "decoy bead"}); err != nil {
			t.Fatalf("create decoy: %v", err)
		}

		got, err = ws.store.Get(target.ID)
		if err != nil {
			t.Fatalf("get after decoy: %v", err)
		}
		if got.Status != "closed" {
			t.Errorf("target reverted to %q after subsequent bd call (#2079)\n%s",
				got.Status, dumpDoltPersistenceState(ws, target.ID))
		}
	})
}

// TestDoltPersistence_MetadataSurvivesSubsequentBdWrites reproduces #2080
// and the #2093 metadata-wipe shape: a metadata write to bead A must remain
// in Dolt after later bd write subprocesses run on any bead.
func TestDoltPersistence_MetadataSurvivesSubsequentBdWrites(t *testing.T) {
	withDoltPersistenceFixture(t, func(ws *doltPersistenceWorkspace) {
		target, err := ws.store.Create(beads.Bead{Title: "metadata target"})
		if err != nil {
			t.Fatalf("create target: %v", err)
		}
		ws.freezeJSONLAndDisableAutoExport(t)

		marker := fmt.Sprintf("probe-%d", time.Now().UnixNano())
		if err := ws.store.SetMetadata(target.ID, "probe_marker", marker); err != nil {
			t.Fatalf("set metadata on target: %v", err)
		}

		got, err := ws.store.Get(target.ID)
		if err != nil {
			t.Fatalf("get after set-metadata: %v", err)
		}
		if got.Metadata["probe_marker"] != marker {
			t.Fatalf("precondition: probe_marker = %q, want %q", got.Metadata["probe_marker"], marker)
		}

		// Exercise an unrelated bd write. In affected bd versions this command
		// triggered the destructive startup import.
		decoy, err := ws.store.Create(beads.Bead{Title: "decoy"})
		if err != nil {
			t.Fatalf("create decoy: %v", err)
		}
		if err := ws.store.SetMetadata(decoy.ID, "probe_other", "x"); err != nil {
			t.Fatalf("set metadata on decoy: %v", err)
		}

		got, err = ws.store.Get(target.ID)
		if err != nil {
			t.Fatalf("get after decoy write: %v", err)
		}
		if got.Metadata["probe_marker"] != marker {
			t.Errorf("probe_marker on %s = %q, want %q after subsequent bd writes (#2080/#2093)\n%s",
				target.ID, got.Metadata["probe_marker"], marker, dumpDoltPersistenceState(ws, target.ID))
		}
	})
}

// TestDoltPersistence_AssigneeAndInProgressSurviveSubsequentBdWrite
// covers the claim path: status=in_progress and assignee=<id> must remain in
// Dolt after an unrelated bd write runs with managed-city JSONL export
// suppressed.
func TestDoltPersistence_AssigneeAndInProgressSurviveSubsequentBdWrite(t *testing.T) {
	withDoltPersistenceFixture(t, func(ws *doltPersistenceWorkspace) {
		target, err := ws.store.Create(beads.Bead{Title: "claim target"})
		if err != nil {
			t.Fatalf("create target: %v", err)
		}
		ws.freezeJSONLAndDisableAutoExport(t)

		assignee := "test-rig/agent-a-abc12"
		if err := ws.store.Update(target.ID, beads.UpdateOpts{
			Status:   stringPtr("in_progress"),
			Assignee: stringPtr(assignee),
		}); err != nil {
			t.Fatalf("claim target: %v", err)
		}

		got, err := ws.store.Get(target.ID)
		if err != nil {
			t.Fatalf("get after claim: %v", err)
		}
		if got.Assignee != assignee {
			t.Fatalf("precondition: assignee = %q, want %q", got.Assignee, assignee)
		}

		// Exercise an unrelated write. In affected bd versions this command
		// triggered the destructive startup import.
		if _, err := ws.store.Create(beads.Bead{Title: "decoy"}); err != nil {
			t.Fatalf("create decoy: %v", err)
		}

		got, err = ws.store.Get(target.ID)
		if err != nil {
			t.Fatalf("get after decoy: %v", err)
		}
		if got.Assignee != assignee {
			t.Errorf("assignee on %s = %q, want %q after unrelated bd write\n%s",
				target.ID, got.Assignee, assignee, dumpDoltPersistenceState(ws, target.ID))
		}
		if got.Status != "in_progress" {
			t.Errorf("status on %s = %q, want %q after unrelated bd write\n%s",
				target.ID, got.Status, "in_progress", dumpDoltPersistenceState(ws, target.ID))
		}
	})
}

// TestDoltPersistence_SessionAwakeMetadataSurvivesSubsequentBdWrite
// covers the persistence portion of #2094: session wake metadata written to
// Dolt must remain visible after a later bd write runs with managed-city JSONL
// export suppressed.
func TestDoltPersistence_SessionAwakeMetadataSurvivesSubsequentBdWrite(t *testing.T) {
	withDoltPersistenceFixture(t, func(ws *doltPersistenceWorkspace) {
		target, err := ws.store.Create(beads.Bead{
			Title:    "session bead",
			Metadata: map[string]string{"state": "asleep"},
		})
		if err != nil {
			t.Fatalf("create target: %v", err)
		}
		ws.freezeJSONLAndDisableAutoExport(t)

		wokeAt := time.Now().UTC().Format(time.RFC3339)
		if err := ws.store.SetMetadataBatch(target.ID, map[string]string{
			"state":        "awake",
			"last_woke_at": wokeAt,
			"wake_reason":  "test",
		}); err != nil {
			t.Fatalf("set wake metadata: %v", err)
		}

		got, err := ws.store.Get(target.ID)
		if err != nil {
			t.Fatalf("get after wake: %v", err)
		}
		if got.Metadata["state"] != "awake" {
			t.Fatalf("precondition: state = %q, want %q", got.Metadata["state"], "awake")
		}

		// Any other bd write command should leave the wake metadata intact.
		if _, err := ws.store.Create(beads.Bead{Title: "decoy"}); err != nil {
			t.Fatalf("create decoy: %v", err)
		}

		got, err = ws.store.Get(target.ID)
		if err != nil {
			t.Fatalf("get after decoy: %v", err)
		}
		if got.Metadata["state"] != "awake" {
			t.Errorf("state on %s = %q, want %q after subsequent bd call (#2094)\n%s",
				target.ID, got.Metadata["state"], "awake", dumpDoltPersistenceState(ws, target.ID))
		}
		if got.Metadata["last_woke_at"] != wokeAt {
			t.Errorf("last_woke_at on %s = %q, want %q (#2094)\n%s",
				target.ID, got.Metadata["last_woke_at"], wokeAt, dumpDoltPersistenceState(ws, target.ID))
		}
	})
}

// TestDoltPersistence_HandoffRoutingMetadataSurvivesSubsequentBdWrite
// covers handoff routing: gc.routed_to updates must remain in Dolt after a
// later bd write runs with managed-city JSONL export suppressed.
func TestDoltPersistence_HandoffRoutingMetadataSurvivesSubsequentBdWrite(t *testing.T) {
	withDoltPersistenceFixture(t, func(ws *doltPersistenceWorkspace) {
		target, err := ws.store.Create(beads.Bead{
			Title:    "handoff target",
			Metadata: map[string]string{"gc.routed_to": "test-rig/agent-a"},
		})
		if err != nil {
			t.Fatalf("create target: %v", err)
		}
		ws.freezeJSONLAndDisableAutoExport(t)

		if err := ws.store.SetMetadata(target.ID, "gc.routed_to", "test-rig/agent-b"); err != nil {
			t.Fatalf("set handoff routed_to: %v", err)
		}

		got, err := ws.store.Get(target.ID)
		if err != nil {
			t.Fatalf("get after handoff: %v", err)
		}
		if got.Metadata["gc.routed_to"] != "test-rig/agent-b" {
			t.Fatalf("precondition: routed_to = %q, want %q",
				got.Metadata["gc.routed_to"], "test-rig/agent-b")
		}

		if _, err := ws.store.Create(beads.Bead{Title: "decoy"}); err != nil {
			t.Fatalf("create decoy: %v", err)
		}

		got, err = ws.store.Get(target.ID)
		if err != nil {
			t.Fatalf("get after decoy: %v", err)
		}
		if got.Metadata["gc.routed_to"] != "test-rig/agent-b" {
			t.Errorf("gc.routed_to on %s = %q, want %q after unrelated bd write (#2080)\n%s",
				target.ID, got.Metadata["gc.routed_to"], "test-rig/agent-b",
				dumpDoltPersistenceState(ws, target.ID))
		}
	})
}

// TestDoltPersistence_InProgressStatusSurvivesSubsequentBdWrite
// covers status-only claims: status=in_progress must remain in Dolt after a
// later bd write runs with managed-city JSONL export suppressed.
func TestDoltPersistence_InProgressStatusSurvivesSubsequentBdWrite(t *testing.T) {
	withDoltPersistenceFixture(t, func(ws *doltPersistenceWorkspace) {
		target, err := ws.store.Create(beads.Bead{Title: "step bead"})
		if err != nil {
			t.Fatalf("create target: %v", err)
		}
		ws.freezeJSONLAndDisableAutoExport(t)

		if err := ws.store.Update(target.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
			t.Fatalf("claim step: %v", err)
		}

		got, err := ws.store.Get(target.ID)
		if err != nil {
			t.Fatalf("get after claim: %v", err)
		}
		if got.Status != "in_progress" {
			t.Fatalf("precondition: status = %q, want %q", got.Status, "in_progress")
		}

		if _, err := ws.store.Create(beads.Bead{Title: "decoy"}); err != nil {
			t.Fatalf("create decoy: %v", err)
		}

		got, err = ws.store.Get(target.ID)
		if err != nil {
			t.Fatalf("get after decoy: %v", err)
		}
		if got.Status != "in_progress" {
			t.Errorf("status on %s = %q, want %q after unrelated bd write (#2081)\n%s",
				target.ID, got.Status, "in_progress", dumpDoltPersistenceState(ws, target.ID))
		}
	})
}

// TestDoltPersistence_SequentialMetadataWritesAccumulateInDolt
// reproduces the cascade observed in #2131: when many writes happen in
// sequence, every metadata write must accumulate in Dolt rather than being
// reverted by the next write's destructive startup import path.
//
// This is the most damaging variant in practice because order-tracking,
// session-state, and pool-assignment are all multi-write flows where
// even one in-the-middle revert wedges the whole sequence.
func TestDoltPersistence_SequentialMetadataWritesAccumulateInDolt(t *testing.T) {
	withDoltPersistenceFixture(t, func(ws *doltPersistenceWorkspace) {
		target, err := ws.store.Create(beads.Bead{Title: "cascade target"})
		if err != nil {
			t.Fatalf("create target: %v", err)
		}
		ws.freezeJSONLAndDisableAutoExport(t)

		// Sequence: each write should land and the cumulative state should
		// reflect all of them. In the broken regime, only the LAST write
		// shows up — every preceding one is reverted by the next bd call.
		steps := []struct {
			key, value string
		}{
			{"step_1", "claimed"},
			{"step_2", "branch_pushed"},
			{"step_3", "pr_opened"},
			{"step_4", "review_passed"},
			{"step_5", "merged"},
		}
		for _, step := range steps {
			if err := ws.store.SetMetadata(target.ID, step.key, step.value); err != nil {
				t.Fatalf("set %s: %v", step.key, err)
			}
		}

		got, err := ws.store.Get(target.ID)
		if err != nil {
			t.Fatalf("get after cascade: %v", err)
		}
		for _, step := range steps {
			if got.Metadata[step.key] != step.value {
				t.Errorf("metadata[%s] = %q, want %q (cascade write revert, #2131)\n%s",
					step.key, got.Metadata[step.key], step.value,
					dumpDoltPersistenceState(ws, target.ID))
			}
		}
	})
}

// TestDoltPersistence_CrossBeadMetadataWritesPersistInDolt proves
// the bug is not bead-isolated: writes on bead A must survive later writes on
// bead B, and vice versa. The destructive import overwrote all rows from stale
// JSONL, not just the row being modified.
func TestDoltPersistence_CrossBeadMetadataWritesPersistInDolt(t *testing.T) {
	withDoltPersistenceFixture(t, func(ws *doltPersistenceWorkspace) {
		a, err := ws.store.Create(beads.Bead{Title: "bead A"})
		if err != nil {
			t.Fatalf("create A: %v", err)
		}
		b, err := ws.store.Create(beads.Bead{Title: "bead B"})
		if err != nil {
			t.Fatalf("create B: %v", err)
		}
		ws.freezeJSONLAndDisableAutoExport(t)

		if err := ws.store.SetMetadata(a.ID, "marker", "A-payload"); err != nil {
			t.Fatalf("set A marker: %v", err)
		}
		if err := ws.store.SetMetadata(b.ID, "marker", "B-payload"); err != nil {
			t.Fatalf("set B marker: %v", err)
		}
		if err := ws.store.SetMetadata(a.ID, "marker2", "A-payload-2"); err != nil {
			t.Fatalf("re-set A marker: %v", err)
		}

		gotA, err := ws.store.Get(a.ID)
		if err != nil {
			t.Fatalf("get A: %v", err)
		}
		gotB, err := ws.store.Get(b.ID)
		if err != nil {
			t.Fatalf("get B: %v", err)
		}

		if gotA.Metadata["marker"] != "A-payload" {
			t.Errorf("A.marker = %q, want %q (cross-bead destructive startup import, #2093)\n%s",
				gotA.Metadata["marker"], "A-payload", dumpDoltPersistenceState(ws, a.ID))
		}
		if gotA.Metadata["marker2"] != "A-payload-2" {
			t.Errorf("A.marker2 = %q, want %q\n%s",
				gotA.Metadata["marker2"], "A-payload-2", dumpDoltPersistenceState(ws, a.ID))
		}
		if gotB.Metadata["marker"] != "B-payload" {
			t.Errorf("B.marker = %q, want %q (cross-bead destructive startup import, #2093)\n%s",
				gotB.Metadata["marker"], "B-payload", dumpDoltPersistenceState(ws, b.ID))
		}
	})
}

// TestDoltPersistence_ConcurrentMetadataWriteBurstTimingVisibility does not assert a
// wall-clock threshold. Its job is to make the cost profile of a 25-agent-style
// write burst visible while the Dolt persistence fix is being validated. Future
// fixes that add export subprocesses, async waits, or locking should leave
// clear timing evidence here before we promote them into broader concurrent
// tests.
func TestDoltPersistence_ConcurrentMetadataWriteBurstTimingVisibility(t *testing.T) {
	withDoltPersistenceFixture(t, func(ws *doltPersistenceWorkspace) {
		const workers = 25
		beadsByWorker := make([]beads.Bead, workers)
		createDurations := make([]time.Duration, workers)
		for i := 0; i < workers; i++ {
			created, elapsed := timedCreate(t, ws.store, fmt.Sprintf("setup-create-%02d", i), beads.Bead{
				Title: fmt.Sprintf("timing target %02d", i),
			})
			beadsByWorker[i] = created
			createDurations[i] = elapsed
		}
		logDurationSummary(t, "timing setup creates", createDurations)

		ws.freezeJSONLAndDisableAutoExport(t)

		var wg sync.WaitGroup
		start := make(chan struct{})
		durations := make([]time.Duration, workers)
		errs := make([]error, workers)
		for i := 0; i < workers; i++ {
			i := i
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				opName := fmt.Sprintf("concurrent-set-metadata-%02d", i)
				begin := time.Now()
				errs[i] = ws.store.SetMetadata(beadsByWorker[i].ID, "timing_worker", fmt.Sprintf("%02d", i))
				durations[i] = time.Since(begin)
				logStoreTiming(t, opName, durations[i], errs[i])
			}()
		}

		burstStart := time.Now()
		close(start)
		wg.Wait()
		t.Logf("timing-summary: concurrent write burst workers=%d wall=%s", workers, roundDuration(time.Since(burstStart)))
		logDurationSummary(t, "concurrent SetMetadata operations", durations)

		for i, err := range errs {
			if err != nil {
				t.Errorf("worker %02d SetMetadata failed: %v", i, err)
			}
		}
	})
}
