package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// TestDecideDriftAction exercises the flag×outcome matrix from the
// designer brief (§ "Flag-combination matrix"). Six flag combinations
// times {no drift, drift detected} = twelve cells. Each cell pins a
// single decision so the operator-facing UX is locked behind a test.
func TestDecideDriftAction(t *testing.T) {
	const localID = "abc12345"
	const svID = "abc12345"
	const driftedID = "9e21abcd"

	tests := []struct {
		name          string
		localBuildID  string
		supervisorID  string
		flags         driftFlags
		wantProceed   bool
		wantRestart   bool
		wantError     bool
		wantDryRun    bool
		wantBinaryBit bool
	}{
		{
			name:         "no flags + no drift",
			localBuildID: localID,
			supervisorID: svID,
			flags:        driftFlags{},
			wantProceed:  true,
		},
		{
			name:          "no flags + drift",
			localBuildID:  localID,
			supervisorID:  driftedID,
			flags:         driftFlags{},
			wantRestart:   true,
			wantBinaryBit: true,
		},
		{
			name:         "--dry-run + no drift",
			localBuildID: localID,
			supervisorID: svID,
			flags:        driftFlags{DryRun: true},
			wantProceed:  true,
		},
		{
			name:          "--dry-run + drift",
			localBuildID:  localID,
			supervisorID:  driftedID,
			flags:         driftFlags{DryRun: true},
			wantDryRun:    true,
			wantBinaryBit: true,
		},
		{
			name:         "--no-auto-restart + no drift",
			localBuildID: localID,
			supervisorID: svID,
			flags:        driftFlags{NoAutoRestart: true},
			wantProceed:  true,
		},
		{
			name:          "--no-auto-restart + drift",
			localBuildID:  localID,
			supervisorID:  driftedID,
			flags:         driftFlags{NoAutoRestart: true},
			wantError:     true,
			wantBinaryBit: true,
		},
		{
			name:         "--dry-run --no-auto-restart + no drift",
			localBuildID: localID,
			supervisorID: svID,
			flags:        driftFlags{DryRun: true, NoAutoRestart: true},
			wantProceed:  true,
		},
		{
			name:          "--dry-run --no-auto-restart + drift (dry-run wins)",
			localBuildID:  localID,
			supervisorID:  driftedID,
			flags:         driftFlags{DryRun: true, NoAutoRestart: true},
			wantDryRun:    true,
			wantBinaryBit: true,
		},
		{
			name:         "kill-switch + no drift",
			localBuildID: localID,
			supervisorID: svID,
			flags:        driftFlags{KillSwitchActive: true},
			wantProceed:  true,
		},
		{
			name:          "kill-switch + drift (errors with config-disabled message)",
			localBuildID:  localID,
			supervisorID:  driftedID,
			flags:         driftFlags{KillSwitchActive: true},
			wantError:     true,
			wantBinaryBit: true,
		},
		{
			name:         "kill-switch + --dry-run + no drift",
			localBuildID: localID,
			supervisorID: svID,
			flags:        driftFlags{KillSwitchActive: true, DryRun: true},
			wantProceed:  true,
		},
		{
			name:          "kill-switch + --dry-run + drift",
			localBuildID:  localID,
			supervisorID:  driftedID,
			flags:         driftFlags{KillSwitchActive: true, DryRun: true},
			wantDryRun:    true,
			wantBinaryBit: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sv := SupervisorStatus{BuildID: tc.supervisorID}
			got := decideDriftAction(tc.localBuildID, sv, nil, tc.flags)
			if got.ProceedNormally != tc.wantProceed {
				t.Errorf("ProceedNormally = %v, want %v", got.ProceedNormally, tc.wantProceed)
			}
			if got.Restart != tc.wantRestart {
				t.Errorf("Restart = %v, want %v", got.Restart, tc.wantRestart)
			}
			if got.Error != tc.wantError {
				t.Errorf("Error = %v, want %v", got.Error, tc.wantError)
			}
			if got.DryRun != tc.wantDryRun {
				t.Errorf("DryRun = %v, want %v", got.DryRun, tc.wantDryRun)
			}
			if got.BinaryDrift != tc.wantBinaryBit {
				t.Errorf("BinaryDrift = %v, want %v", got.BinaryDrift, tc.wantBinaryBit)
			}
		})
	}
}

// TestPrintSupervisorIdentity pins the operator-facing first line of
// `gc start` output. The format is the single most load-bearing UX
// piece — operators scan it for the build hash to confirm they're on
// the right binary. The wording is referenced from runbooks and log
// scrapers; if the format drifts, downstream tooling silently breaks.
func TestPrintSupervisorIdentity(t *testing.T) {
	var buf bytes.Buffer
	now := time.Now()
	printSupervisorIdentity(&buf, supervisorIdentity{
		PID:     12345,
		ExePath: "/home/op/.local/bin/gc",
		BuildID: "abc12345",
		Started: now.Add(-2 * time.Minute),
	}, now)

	out := buf.String()
	if !strings.HasPrefix(out, "Supervisor:") {
		t.Fatalf("first line must start with %q; got %q", "Supervisor:", strings.SplitN(out, "\n", 2)[0])
	}
	for _, want := range []string{"pid=12345", "exe=/home/op/.local/bin/gc", "buildID=abc12345"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q is missing token %q", out, want)
		}
	}
}

// TestPrintSupervisorIdentity_StartedHumanization confirms started=
// uses humanized durations rather than RFC3339 epochs. Operators
// prefer "2m ago" / "just now" / "1h ago" — same humanizer convention
// as the rest of gc.
func TestPrintSupervisorIdentity_StartedHumanization(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name    string
		started time.Time
		want    string
	}{
		{"just now", now, "started=just now"},
		{"two minutes", now.Add(-2 * time.Minute), "started=2m ago"},
		{"one hour", now.Add(-1 * time.Hour), "started=1h ago"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			printSupervisorIdentity(&buf, supervisorIdentity{
				PID:     1,
				ExePath: "/x",
				BuildID: "x",
				Started: tc.started,
			}, now)
			if !strings.Contains(buf.String(), tc.want) {
				t.Errorf("output %q is missing %q", buf.String(), tc.want)
			}
		})
	}
}

// TestPrintSupervisorIdentity_EmptyBuildID surfaces the missing-buildID
// fallback. Older supervisors don't expose build_id; the line still
// prints (operators still need pid/exe/started) but the buildID token
// reads "buildID=(unknown)" so it's clear why we couldn't compare.
func TestPrintSupervisorIdentity_EmptyBuildID(t *testing.T) {
	var buf bytes.Buffer
	now := time.Now()
	printSupervisorIdentity(&buf, supervisorIdentity{
		PID:     1,
		ExePath: "/x",
		BuildID: "",
		Started: now,
	}, now)
	out := buf.String()
	if !strings.Contains(out, "buildID=(unknown)") {
		t.Errorf("expected buildID=(unknown) for empty buildID; got %q", out)
	}
}

// TestPrintDriftReport pins the drift report wording. `Drift detected:`
// is the greppable headline; the per-component lines are how the
// operator sees what changed.
func TestPrintDriftReport(t *testing.T) {
	var buf bytes.Buffer
	printDriftReport(&buf, driftReport{
		BinaryDrift:  true,
		LocalBuildID: "abc12345",
		SupervisorID: "9e21abcd",
		PackDrifted:  []string{"packs/gastown", "packs/foo"},
	})
	out := buf.String()
	for _, want := range []string{
		"Drift detected:",
		"binary: local=abc12345 supervisor=9e21abcd",
		"pack: packs/gastown",
		"pack: packs/foo",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("drift report missing %q\nfull output:\n%s", want, out)
		}
	}
}
