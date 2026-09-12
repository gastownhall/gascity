package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// protectTestOpts builds cleanup options for one rig whose recorded port file
// names recordedPort. liveErr controls whether live resolution has an answer.
func protectTestOpts(t *testing.T, recordedPort string, live liveDoltPortResolution, liveErr error) cleanupOptions {
	t.Helper()
	rigRoot := t.TempDir()
	fs := fsys.NewFake()
	if err := fs.MkdirAll(filepath.Join(rigRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if recordedPort != "" {
		if err := fs.WriteFile(filepath.Join(rigRoot, ".beads", "dolt-server.port"), []byte(recordedPort+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return cleanupOptions{
		Rigs:        []resolverRig{{Name: "frontend", Path: rigRoot}},
		FS:          fs,
		CityPath:    t.TempDir(),
		LiveResolve: func(string) (liveDoltPortResolution, error) { return live, liveErr },
	}
}

// TestProtectedDoltPortsFallsBackToRecordedPortWhenLiveResolutionFails pins the
// degraded-mode guard on the destructive path.
//
// The reaper prefers live state precisely because a status file can lie, and a
// lying file fails to protect the real listener. That reasoning compares a stale
// file against WORKING live state. It does not cover ABSENT live state — no
// runtime handle, unreadable process table, permissions — where the file is the
// only signal left. Without the fallback the managed city port lands in the reap
// set with a live server still on it: SIGKILL plus DataDir removal.
//
// A live process is present on the recorded port here but is NOT attributable by
// argv (no --config/--data-dir under a rig root), which is what makes the live
// path silent and the file the last line of defence.
func TestProtectedDoltPortsFallsBackToRecordedPortWhenLiveResolutionFails(t *testing.T) {
	opts := protectTestOpts(t, "3310", liveDoltPortResolution{}, os.ErrNotExist)
	procs := []DoltProcInfo{{PID: 4242, Argv: []string{"dolt", "sql-server"}, Ports: []int{3310}}}

	got := protectedDoltPortsForReap(opts, procs)
	owner, protected := got[3310]
	if !protected {
		t.Fatalf("recorded port 3310 must be protected when live resolution fails; got protected set %v.\n"+
			"An unprotected live listener here is SIGKILL + DataDir removal — a stale entry only costs a skipped reap.", got)
	}
	if owner == "" {
		t.Error("protected port should carry an attribution naming the recorded-port fallback")
	}
}

// TestProtectedDoltPortsPrefersLiveResolutionOverRecordedPort pins the other
// half of the contract: the fallback must not become a second source of truth.
// When live resolution answers, the recorded file is not consulted at all, so a
// stale port in that file cannot widen the protected set and suppress a reap the
// operator asked for.
func TestProtectedDoltPortsPrefersLiveResolutionOverRecordedPort(t *testing.T) {
	// File records a STALE port; live resolution reports the real one.
	opts := protectTestOpts(t, "3310", liveDoltPortResolution{Port: 3399}, nil)

	got := protectedDoltPortsForReap(opts, nil)
	if owner, ok := got[3399]; !ok || owner != "managed city dolt" {
		t.Errorf("live-resolved port 3399 must be protected and attributed to live state; got %v", got)
	}
	if _, stale := got[3310]; stale {
		t.Errorf("stale recorded port 3310 must NOT be protected while live resolution has an answer; got %v.\n"+
			"Consulting the file in the healthy path would make a lying file authoritative — the thing live-state detection exists to avoid.", got)
	}
}

// TestProtectedDoltPortsIgnoresMalformedRecordedPort keeps the degraded path from
// widening on garbage: an unparseable or out-of-range file contributes nothing,
// leaving the reaper exactly where it would be with no fallback at all.
func TestProtectedDoltPortsIgnoresMalformedRecordedPort(t *testing.T) {
	for _, bad := range []string{"not-a-port", "0", "-1", "99999999", ""} {
		opts := protectTestOpts(t, bad, liveDoltPortResolution{}, os.ErrNotExist)
		got := protectedDoltPortsForReap(opts, nil)
		if len(got) != 0 {
			t.Errorf("recorded port %q is malformed and must contribute nothing; got %v", bad, got)
		}
	}
}

// TestProtectedDoltPortsStillAttributesLiveRigOwnedProcess guards the behaviour
// the PR added, so the fallback cannot be mistaken for a replacement: a process
// whose --config sits under a rig root is attributed from live state, with no
// recorded port file present at all.
func TestProtectedDoltPortsStillAttributesLiveRigOwnedProcess(t *testing.T) {
	opts := protectTestOpts(t, "", liveDoltPortResolution{}, os.ErrNotExist)
	rigRoot := opts.Rigs[0].Path
	procs := []DoltProcInfo{{
		PID:   4242,
		Argv:  []string{"dolt", "sql-server", "--config", filepath.Join(rigRoot, ".beads", "dolt", "config.yaml")},
		Ports: []int{3310},
	}}

	got := protectedDoltPortsForReap(opts, procs)
	if owner, ok := got[3310]; !ok || owner != "frontend" {
		t.Errorf("live rig-owned process must be attributed to its rig from argv alone; got %v", got)
	}
}
