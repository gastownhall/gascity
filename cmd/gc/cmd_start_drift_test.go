package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// driftCheckEnv stands up the shared seams runStartDriftCheck needs:
// an httptest server serving /health with the chosen build_id, a
// GC_HOME pointed at a temp dir, and stubbed supervisorAliveHook /
// supervisorAPIBaseURLHook / supervisorSystemctlActive /
// restartHelpersHook. Production globals are saved on entry and
// restored via t.Cleanup. The returned commit string is the local
// build identity the test should compare against the supervisor's
// reported build_id.
func driftCheckEnv(t *testing.T, supervisorBuildID string) (cityPath string, restoreCommit func(string)) {
	t.Helper()

	// Isolated GC_HOME so recordDriftRestartAttempt writes into a temp
	// dir instead of the user's real ~/.gc.
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_DOLT", "skip")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status":"ok","version":"v0","build_id":%q,"uptime_sec":1,"cities_total":0,"cities_running":0}`, supervisorBuildID)
	}))
	t.Cleanup(srv.Close)

	oldAlive := supervisorAliveHook
	oldBaseURL := supervisorAPIBaseURLHook
	oldActive := supervisorSystemctlActive
	oldHelpers := restartHelpersHook
	oldReadExe := readSupervisorExePathHook
	t.Cleanup(func() {
		supervisorAliveHook = oldAlive
		supervisorAPIBaseURLHook = oldBaseURL
		supervisorSystemctlActive = oldActive
		restartHelpersHook = oldHelpers
		readSupervisorExePathHook = oldReadExe
	})

	supervisorAliveHook = os.Getpid
	supervisorAPIBaseURLHook = func() (string, error) { return srv.URL, nil }
	supervisorSystemctlActive = func(string) bool { return false }
	readSupervisorExePathHook = func(int) (string, error) { return "/tmp/gc-test-supervisor", nil }
	restartHelpersHook = func() restartHelpers {
		return restartHelpers{
			Systemctl: func(...string) error { return nil },
			Kill:      func(int) error { return nil },
			WaitExit:  func(int) error { return nil },
			Spawn:     func(string, ...string) error { return nil },
		}
	}

	oldCommit := commit
	restoreCommit = func(local string) { commit = local }
	t.Cleanup(func() { commit = oldCommit })

	cityPath = t.TempDir()
	return cityPath, restoreCommit
}

// shrinkDriftReadyTimeout shortens the post-restart verification budget to
// 300ms so delegated drift tests whose restart never lands (or whose probe
// never verifies) fail fast instead of waiting the full production
// driftReadyTimeout — pollDelegatedRestartVerified polls until that budget
// expires before reporting the last obstacle.
func shrinkDriftReadyTimeout(t *testing.T) {
	t.Helper()
	old := driftReadyTimeout
	driftReadyTimeout = 300 * time.Millisecond
	t.Cleanup(func() { driftReadyTimeout = old })
}

// TestRunStartDriftCheck_RestartReturnsContinue pins the load-bearing
// post-restart contract: when drift triggers a successful auto-restart,
// runStartDriftCheck must return (0, true) so the caller continues into
// normal supervisor registration / start. Returning (0, false) here
// (which was the previous behavior on the success arm) left the
// requested city un-registered after the supervisor came back up.
func TestRunStartDriftCheck_RestartReturnsContinue(t *testing.T) {
	cityPath, setCommit := driftCheckEnv(t, "old-build-id")
	setCommit("new-build-id") // local commit differs → drift detected → Restart disposition

	// Make sure dry-run / no-auto-restart flags are off so the
	// decideDriftAction switch lands on Restart.
	oldDry, oldNoAR := dryRunMode, noAutoRestartMode
	dryRunMode, noAutoRestartMode = false, false
	t.Cleanup(func() { dryRunMode, noAutoRestartMode = oldDry, oldNoAR })

	var stdout, stderr bytes.Buffer
	exitCode, cont := runStartDriftCheck(cityPath, &stdout, &stderr)
	if exitCode != 0 {
		t.Errorf("exitCode = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if !cont {
		t.Fatalf("cont = false on successful Restart; caller would skip city registration.\nstdout:\n%s\nstderr:\n%s",
			stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Drift detected:") {
		t.Errorf("stdout missing drift report:\n%s", stdout.String())
	}
}

// TestRunStartDriftCheck_DarwinLaunchdRestartDoesNotRequireProcExe pins the
// macOS production upgrade path: launchd owns the supervisor lifecycle, so
// binary drift restart must delegate to launchctl even though /proc/<pid>/exe
// is unavailable on Darwin.
func TestRunStartDriftCheck_DarwinLaunchdRestartDoesNotRequireProcExe(t *testing.T) {
	cityPath, setCommit := driftCheckEnv(t, "old-build-id")
	setCommit("new-build-id")

	oldDry, oldNoAR := dryRunMode, noAutoRestartMode
	dryRunMode, noAutoRestartMode = false, false
	t.Cleanup(func() { dryRunMode, noAutoRestartMode = oldDry, oldNoAR })

	oldGOOS := supervisorRuntimeGOOS
	oldLaunchdActive := supervisorLaunchdActive
	oldReadExe := readSupervisorExePathHook
	oldHelpers := restartHelpersHook
	t.Cleanup(func() {
		supervisorRuntimeGOOS = oldGOOS
		supervisorLaunchdActive = oldLaunchdActive
		readSupervisorExePathHook = oldReadExe
		restartHelpersHook = oldHelpers
	})

	supervisorRuntimeGOOS = "darwin"
	supervisorLaunchdActive = func(label string) bool {
		return label == supervisorLaunchdLabel()
	}
	readSupervisorExePathHook = func(pid int) (string, error) {
		return "", fmt.Errorf("readlink /proc/%d/exe: no such file or directory", pid)
	}
	var launchctlArgs []string
	restartHelpersHook = func() restartHelpers {
		return restartHelpers{
			Launchctl: func(args ...string) error {
				launchctlArgs = append([]string(nil), args...)
				return nil
			},
			Systemctl: func(...string) error {
				t.Fatal("systemctl should not handle a Darwin launchd supervisor")
				return nil
			},
			Kill: func(int) error {
				t.Fatal("direct kill should not handle a Darwin launchd supervisor")
				return nil
			},
			WaitExit: func(int) error { return nil },
			Spawn: func(string, ...string) error {
				t.Fatal("direct spawn should not handle a Darwin launchd supervisor")
				return nil
			},
		}
	}

	var stdout, stderr bytes.Buffer
	exitCode, cont := runStartDriftCheck(cityPath, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, want 0; stdout=%q stderr=%q", exitCode, stdout.String(), stderr.String())
	}
	if !cont {
		t.Fatalf("cont = false after successful launchd restart; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	wantArgs := []string{"kickstart", "-k", supervisorLaunchdServiceTarget(supervisorLaunchdLabel())}
	if len(launchctlArgs) != len(wantArgs) {
		t.Fatalf("launchctl args = %v, want %v", launchctlArgs, wantArgs)
	}
	for i := range wantArgs {
		if launchctlArgs[i] != wantArgs[i] {
			t.Fatalf("launchctl arg %d = %q, want %q; full args=%v", i, launchctlArgs[i], wantArgs[i], launchctlArgs)
		}
	}
	if strings.Contains(stderr.String(), "/proc/") {
		t.Fatalf("stderr leaked /proc restart failure despite launchd management:\n%s", stderr.String())
	}
}

// TestRunStartDriftCheck_DryRunReturnsTerminal pins the dry-run arm:
// drift is reported, the operator is told what would happen, and the
// caller exits (cont == false) so no further side effects fire.
func TestRunStartDriftCheck_DryRunReturnsTerminal(t *testing.T) {
	cityPath, setCommit := driftCheckEnv(t, "old-build-id")
	setCommit("new-build-id")

	oldDry, oldNoAR := dryRunMode, noAutoRestartMode
	dryRunMode, noAutoRestartMode = true, false
	t.Cleanup(func() { dryRunMode, noAutoRestartMode = oldDry, oldNoAR })

	var stdout, stderr bytes.Buffer
	exitCode, cont := runStartDriftCheck(cityPath, &stdout, &stderr)
	if exitCode != 0 {
		t.Errorf("exitCode = %d, want 0 on --dry-run", exitCode)
	}
	if cont {
		t.Errorf("cont = true on --dry-run; should be terminal")
	}
	if !strings.Contains(stdout.String(), "(would auto-restart; --dry-run)") {
		t.Errorf("stdout missing dry-run suffix:\n%s", stdout.String())
	}
}

// TestRunStartDriftCheck_ErrorReturnsTerminal pins the --no-auto-restart
// arm: drift is detected, an error is printed, and the caller exits
// non-zero with cont == false.
func TestRunStartDriftCheck_ErrorReturnsTerminal(t *testing.T) {
	cityPath, setCommit := driftCheckEnv(t, "old-build-id")
	setCommit("new-build-id")

	oldDry, oldNoAR := dryRunMode, noAutoRestartMode
	dryRunMode, noAutoRestartMode = false, true
	t.Cleanup(func() { dryRunMode, noAutoRestartMode = oldDry, oldNoAR })

	var stdout, stderr bytes.Buffer
	exitCode, cont := runStartDriftCheck(cityPath, &stdout, &stderr)
	if exitCode != 1 {
		t.Errorf("exitCode = %d, want 1 on --no-auto-restart drift", exitCode)
	}
	if cont {
		t.Errorf("cont = true on --no-auto-restart error; should be terminal")
	}
	if !strings.Contains(stderr.String(), "supervisor binary drift") {
		t.Errorf("stderr missing drift error:\n%s", stderr.String())
	}
}

func TestDoStartJSONAlreadyRunningSupervisorKeepsStdoutJSONOnly(t *testing.T) {
	cases := []struct {
		name              string
		localBuildID      string
		supervisorBuildID string
	}{
		{
			name:              "no drift",
			localBuildID:      "same-build-id",
			supervisorBuildID: "same-build-id",
		},
		{
			name:              "drift restart",
			localBuildID:      "new-build-id",
			supervisorBuildID: "old-build-id",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_DOLT", "skip")
			t.Setenv("GC_BEADS", "file")
			cityPath, setCommit := driftCheckEnv(t, tc.supervisorBuildID)
			setCommit(tc.localBuildID)
			if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
				t.Fatalf("mkdir city .gc: %v", err)
			}
			writeCityToml(t, cityPath, "[workspace]\nname = \"test-city\"\n")

			oldDry, oldNoAR := dryRunMode, noAutoRestartMode
			dryRunMode, noAutoRestartMode = false, false
			t.Cleanup(func() { dryRunMode, noAutoRestartMode = oldDry, oldNoAR })

			oldRegister := registerCityWithSupervisorTestHook
			registerCityWithSupervisorTestHook = func(_ string, _ string, stdout, _ io.Writer) (bool, int) {
				if _, err := fmt.Fprintln(stdout, "Registered city 'test-city'"); err != nil {
					t.Fatalf("write registration output: %v", err)
				}
				return true, 0
			}
			t.Cleanup(func() { registerCityWithSupervisorTestHook = oldRegister })

			var stdout, stderr bytes.Buffer
			code := doStartWithNameOverrideJSON([]string{cityPath}, false, &stdout, &stderr, "", true)
			if code != 0 {
				t.Fatalf("doStartWithNameOverrideJSON = %d; stderr=%q stdout=%q", code, stderr.String(), stdout.String())
			}
			if strings.Contains(stdout.String(), "Supervisor:") || strings.Contains(stdout.String(), "Drift detected:") || strings.Contains(stdout.String(), "Registered city") {
				t.Fatalf("stdout contains human text in JSON mode:\n%s", stdout.String())
			}
			lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
			if len(lines) != 1 {
				t.Fatalf("stdout lines = %d, want 1 JSONL record:\n%s", len(lines), stdout.String())
			}
			var payload map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
				t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
			}
			if payload["command"] != "start" || payload["action"] != "start" {
				t.Fatalf("payload command/action = %v/%v, want start/start\n%s", payload["command"], payload["action"], stdout.String())
			}
		})
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
