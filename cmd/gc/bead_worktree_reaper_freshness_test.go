package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// TestReapClosedBeadWorktrees_ProtectsFreshWorktreeUnderDefaultQuarantine
// proves FR-5: a worktree created moments ago is protected under the default
// quarantine window even though every other gate (closed bead, idle, clean
// git state) would allow reaping.
func TestReapClosedBeadWorktrees_ProtectsFreshWorktreeUnderDefaultQuarantine(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktreeWithAge(t, rigRoot, cityPath, "builder", "ga-fresh01", 0)
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-fresh01", Status: "closed"}}, nil)
	cfg := reapTestConfig(rigRoot) // no MinAgeMinutes set -> default quarantine window
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{"mrig": store}, nil, false, events.Discard, &stderr)

	if len(report.Reaped) != 0 {
		t.Fatalf("Reaped = %+v, want 0 for a fresh worktree under quarantine\nstderr:\n%s", report.Reaped, stderr.String())
	}
	if len(report.Protected) != 1 {
		t.Fatalf("Protected = %+v, want exactly 1 quarantine-protected entry", report.Protected)
	}
	reason := report.Protected[0].Reason
	if !strings.Contains(reason, "young") && !strings.Contains(reason, "quarantine") && !strings.Contains(reason, "age") {
		t.Errorf("Reason = %q, want it to mention freshness/quarantine/age", reason)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("fresh worktree %s was removed or unstattable: %v", wt, err)
	}
}

// TestReapClosedBeadWorktrees_ReapsWorktreeOlderThanDefaultMinAge proves a
// worktree older than the default quarantine window is not blocked by FR-5
// and proceeds to the rest of the gate chain (and is reaped, since every
// other gate passes).
func TestReapClosedBeadWorktrees_ReapsWorktreeOlderThanDefaultMinAge(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-old0001") // backdated 24h by the shared helper
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-old0001", Status: "closed"}}, nil)
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{"mrig": store}, nil, false, events.Discard, &stderr)

	if len(report.Reaped) != 1 || report.Reaped[0].BeadID != "ga-old0001" {
		t.Fatalf("Reaped = %+v, want exactly ga-old0001\nstderr:\n%s", report.Reaped, stderr.String())
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree %s still present after reap (stat err=%v)", wt, err)
	}
}

// TestReapClosedBeadWorktrees_ZeroMinAgeDisablesQuarantine proves an explicit
// zero for AutoReapClosedBeadWorktreesMinAgeMinutes disables the freshness
// gate entirely: a just-created worktree is immediately eligible.
func TestReapClosedBeadWorktrees_ZeroMinAgeDisablesQuarantine(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktreeWithAge(t, rigRoot, cityPath, "builder", "ga-nomin01", 0)
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-nomin01", Status: "closed"}}, nil)
	cfg := reapTestConfig(rigRoot)
	zero := 0
	cfg.Daemon.AutoReapClosedBeadWorktreesMinAgeMinutes = &zero
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{"mrig": store}, nil, false, events.Discard, &stderr)

	if len(report.Reaped) != 1 || report.Reaped[0].BeadID != "ga-nomin01" {
		t.Fatalf("Reaped = %+v, want exactly ga-nomin01 with quarantine disabled\nstderr:\n%s", report.Reaped, stderr.String())
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree %s still present after reap (stat err=%v)", wt, err)
	}
}

// TestReapClosedBeadWorktrees_ProtectsWhenAgeIndeterminate proves the age
// computation itself fails closed: if the worktree's .git pointer file is
// unstattable (a race with something else removing it mid-scan), the reaper
// protects rather than treating the age as zero or unlimited.
func TestReapClosedBeadWorktrees_ProtectsWhenAgeIndeterminate(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-noage01")
	if err := os.Remove(filepath.Join(wt, ".git")); err != nil {
		t.Fatalf("remove .git pointer file: %v", err)
	}
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-noage01", Status: "closed"}}, nil)
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{"mrig": store}, nil, false, events.Discard, &stderr)

	if len(report.Reaped) != 0 {
		t.Fatalf("Reaped = %+v, want 0 when worktree age is indeterminate\nstderr:\n%s", report.Reaped, stderr.String())
	}
	if len(report.Protected) != 1 {
		t.Fatalf("Protected = %+v, want exactly 1 fail-closed entry", report.Protected)
	}
}
