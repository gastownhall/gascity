package main

import (
	"context"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// The window-31 stall (ga-f7v2ft.194): after ga-lp5w6 narrowed the sweep's
// completeness domain to the session's reachable scope, one population still
// escaped every proof — zombies. An unreaped process keeps a readable status
// (still our uid) but its environ answers EACCES, and an ORPHANED zombie is
// re-parented to init, the one lineage shape both scope proofs decline. So the
// drain-ack observation stayed incomplete, the obligation escalated onto the
// slow cadence, and the parked row held its pool seat: 14,011 "session name
// already exists (skipping)" refusals, no pools, no wakes.
//
// This test walks the whole sequence end to end against the REAL scanner: the
// session's runtime is alive and undecidable (escalation), then it dies and is
// left unreaped (the exact production transition), and the very next
// re-examination of the SAME retained obligation must finalize — no manual
// close, no bounce.

type zombieRecoveryProcFixture struct {
	root       string
	bootedAt   time.Time
	sessionID  string
	runtimePID int
}

// newZombieRecoveryProcFixture builds a fake procfs holding one owned,
// unreadable, orphaned process started after the incarnation: undecidable by
// every ga-lp5w6 proof, so the sweep reads incomplete.
func newZombieRecoveryProcFixture(t *testing.T, sessionID string) *zombieRecoveryProcFixture {
	t.Helper()
	fixture := &zombieRecoveryProcFixture{
		root:       t.TempDir(),
		bootedAt:   time.Now().Add(-time.Hour).Truncate(time.Second),
		sessionID:  sessionID,
		runtimePID: 4242,
	}
	if err := os.WriteFile(
		filepath.Join(fixture.root, "stat"),
		[]byte("btime "+strconv.FormatInt(fixture.bootedAt.Unix(), 10)+"\n"),
		0o644,
	); err != nil {
		t.Fatalf("write fixture btime: %v", err)
	}

	dir := filepath.Join(fixture.root, strconv.Itoa(fixture.runtimePID))
	// environ as a directory: unreadable for every uid, including root.
	if err := os.MkdirAll(filepath.Join(dir, "environ"), 0o755); err != nil {
		t.Fatalf("create unreadable environ fixture: %v", err)
	}
	uid := strconv.Itoa(os.Geteuid())
	if err := os.WriteFile(
		filepath.Join(dir, "status"),
		[]byte("Name:\tagent\nUid:\t"+uid+"\t"+uid+"\t"+uid+"\t"+uid+"\n"),
		0o644,
	); err != nil {
		t.Fatalf("write fixture status: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(dir, "cgroup"),
		[]byte("0::/user.slice/user-1000.slice/user@1000.service/tmux-spawn-31.scope\n"),
		0o644,
	); err != nil {
		t.Fatalf("write fixture cgroup: %v", err)
	}
	fixture.setRuntimeState(t, "S")

	restore := proctable.SetScanRootForTesting(fixture.root)
	t.Cleanup(restore)
	return fixture
}

// setRuntimeState rewrites the runtime's stat record with the given process
// state, orphaned onto init exactly as the kernel re-parents a survivor.
func (f *zombieRecoveryProcFixture) setRuntimeState(t *testing.T, state string) {
	t.Helper()
	fields := make([]string, 20)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0] = state
	fields[1] = "1"
	// Started 30 minutes after boot: well after the incarnation boundary, so
	// the predates-incarnation adjudication cannot exclude it either.
	fields[19] = strconv.FormatUint(30*60*100, 10)
	stat := strconv.Itoa(f.runtimePID) + " (agent) " + strings.Join(fields, " ")
	if err := os.WriteFile(
		filepath.Join(f.root, strconv.Itoa(f.runtimePID), "stat"),
		[]byte(stat),
		0o644,
	); err != nil {
		t.Fatalf("write fixture stat: %v", err)
	}
}

// incarnationStartedAt is the boundary the drain-ack sweep scans since: 20
// minutes after boot, before the fixture process started.
func (f *zombieRecoveryProcFixture) incarnationStartedAt() time.Time {
	return f.bootedAt.Add(20 * time.Minute)
}

// observationComplete runs the real process-table sweep and reports what
// tmux.ObserveFreshLiveness would publish as Liveness.Complete.
func (f *zombieRecoveryProcFixture) observationComplete(t *testing.T) bool {
	t.Helper()
	_, err := proctable.ScanBySessionIDSince(f.sessionID, f.incarnationStartedAt())
	return err == nil
}

// TestSessionStartControllerEscalatedDrainAckResolvesWhenTheRuntimeBecomesAZombie
// is the ga-f7v2ft.194 self-recovery contract, mirroring the ga-lp5w6 shape
// but with completeness produced by the real scanner instead of a hand-set
// verdict: while the undecidable runtime lives the obligation escalates and
// parks; the moment it dies — even unreaped, environ still unreadable, still
// orphaned onto init — the same retained obligation resolves, lifts its
// ownership fence and clears its refusal history, freeing the pool seat.
func TestSessionStartControllerEscalatedDrainAckResolvesWhenTheRuntimeBecomesAZombie(t *testing.T) {
	// The fixture IS a procfs: it writes stat/status/environ/cwd under a fake
	// /proc root and hands it to the real proctable scanner. On a platform
	// without procfs the scanner has nothing to decline, so the undecidable
	// premise this test rests on cannot be built at all — the guard below fires
	// on darwin, which is exactly what it is protecting against.
	if goruntime.GOOS != "linux" {
		t.Skip("the zombie recovery premise is built from a fake procfs; linux only")
	}

	fixture := newZombieRecoveryProcFixture(t, "ga-drain-esc")
	if fixture.observationComplete(t) {
		t.Fatal("a live, unreadable, orphaned in-scope process read complete; the fixture models nothing")
	}

	start := time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	results := make(chan sessionStartReconcileResult, 256)
	controller := drainAckEscalationTestController(t,
		func(context.Context, sessionStartAdmission) error {
			mu.Lock()
			defer mu.Unlock()
			if !fixture.observationComplete(t) {
				return errSessionStartPoolDrainAckPending
			}
			return nil
		},
		func(result sessionStartReconcileResult) {
			select {
			case results <- result:
			default:
			}
		},
		func() time.Time { return start },
	)

	lease := testDrainAckEscalationLease()
	if _, err := controller.AdmitPoolDrainAck(lease); err != nil {
		t.Fatalf("admit drain ack: %v", err)
	}
	awaitDrainAckResult(t, results, func(result sessionStartReconcileResult) bool {
		return result.Outcome == sessionStartReconcileDrainAckEscalated
	}, "the escalation crossing on an incomplete observation")

	// The production transition: the runtime exits and nobody reaps it. Its
	// environ stays unreadable and its parent chain still exits to init — only
	// its state changes.
	mu.Lock()
	fixture.setRuntimeState(t, "Z")
	mu.Unlock()
	if !fixture.observationComplete(t) {
		t.Fatal("a zombie still poisons the observation; the obligation can never finalize")
	}

	// Drive the retained obligation's re-examination through the audit's
	// level-triggered re-detection instead of sleeping out the 5m cadence.
	if _, err := controller.AdmitPoolDrainAck(lease); err != nil {
		t.Fatalf("re-admit escalated drain ack: %v", err)
	}
	awaitDrainAckResult(t, results, func(result sessionStartReconcileResult) bool {
		return result.Outcome == sessionStartReconcileSucceeded
	}, "the escalated obligation's resolution")

	if controller.ownsPoolDrainAckStop(lease.SessionID, lease.InstanceToken) {
		t.Fatal("resolution left the drain-ack ownership fence in place; the seat is still fenced")
	}
	if controller.holdsAnyAdmission(lease.SessionID) {
		t.Fatal("resolution retained the admission; the obligation must end when the stop finalizes")
	}
	controller.mu.Lock()
	refusals := controller.drainAckRefusalHistory[lease.SessionID].Count
	controller.mu.Unlock()
	if refusals != 0 {
		t.Fatalf("refusal history after resolution = %d, want the obligation's streak cleared", refusals)
	}
}
