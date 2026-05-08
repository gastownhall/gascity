package main

import (
	"bytes"
	"hash/fnv"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDoltGCNudgeSourcesRuntimeBeforePortValidation(t *testing.T) {
	cityPath := writeDoltGCNudgeCity(t)
	writeDoltGCNudgeRuntimeState(t, cityPath)
	binDir := doltGCNudgeToolPath(t, false, nil)

	cmd := doltGCNudgeCommand(t, cityPath, binDir,
		"GC_DOLT_GC_DRY_RUN=1",
		"GC_DOLT_GC_THRESHOLD_BYTES=0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-nudge without GC_DOLT_PORT failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "dry-run") {
		t.Fatalf("gc-nudge output = %q, want dry-run; runtime should default GC_DOLT_PORT", out)
	}
}

func TestDoltGCNudgeAcceptsSymlinkEquivalentRuntimeDataDir(t *testing.T) {
	cityPath := writeDoltGCNudgeCity(t)
	writeDoltGCNudgeRuntimeState(t, cityPath)
	linkPath := filepath.Join(t.TempDir(), "dolt-link")
	if err := os.Symlink(filepath.Join(cityPath, ".beads", "dolt"), linkPath); err != nil {
		t.Fatal(err)
	}
	binDir := doltGCNudgeToolPath(t, false, nil)

	cmd := doltGCNudgeCommand(t, cityPath, binDir,
		"GC_DOLT_DATA_DIR="+linkPath,
		"GC_DOLT_GC_DRY_RUN=1",
		"GC_DOLT_GC_THRESHOLD_BYTES=0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-nudge with symlink data dir failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "dry-run") {
		t.Fatalf("gc-nudge output = %q, want dry-run; symlink data_dir should match runtime state", out)
	}
}

func TestDoltGCNudgeSkipsWhenManagedRuntimeInactiveWithoutExplicitPort(t *testing.T) {
	cityPath := writeDoltGCNudgeCity(t)
	shouldNotRun := filepath.Join(t.TempDir(), "dolt-ran")
	binDir := doltGCNudgeToolPath(t, false, map[string]string{
		"dolt": "#!/bin/sh\n: > \"$DOLT_SHOULD_NOT_RUN\"\nexit 0\n",
	})

	cmd := doltGCNudgeCommand(t, cityPath, binDir,
		"GC_DOLT_GC_THRESHOLD_BYTES=0",
		"DOLT_SHOULD_NOT_RUN="+shouldNotRun,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-nudge without managed runtime failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "managed local Dolt runtime is not active") {
		t.Fatalf("gc-nudge output = %q, want managed-runtime skip", out)
	}
	if _, err := os.Stat(shouldNotRun); !os.IsNotExist(err) {
		t.Fatalf("dolt ran despite inactive managed runtime")
	}
}

func TestDoltGCNudgeSkipsWhenOrderEnvMarksExternalTarget(t *testing.T) {
	cityPath := writeDoltGCNudgeCity(t)
	shouldNotRun := filepath.Join(t.TempDir(), "dolt-ran")
	binDir := doltGCNudgeToolPath(t, false, map[string]string{
		"dolt": "#!/bin/sh\n: > \"$DOLT_SHOULD_NOT_RUN\"\nexit 0\n",
	})

	cmd := doltGCNudgeCommand(t, cityPath, binDir,
		"GC_DOLT_MANAGED_LOCAL=0",
		"GC_DOLT_HOST=external.example.internal",
		"GC_DOLT_PORT=4406",
		"GC_DOLT_GC_THRESHOLD_BYTES=0",
		"DOLT_SHOULD_NOT_RUN="+shouldNotRun,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-nudge with external order marker failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "not applicable") {
		t.Fatalf("gc-nudge output = %q, want not-applicable skip", out)
	}
	if _, err := os.Stat(shouldNotRun); !os.IsNotExist(err) {
		t.Fatalf("dolt ran despite external-order marker")
	}
}

func TestDoltGCNudgeSkipsUnmarkedExternalPort(t *testing.T) {
	cityPath := writeDoltGCNudgeCity(t)
	shouldNotRun := filepath.Join(t.TempDir(), "dolt-ran")
	binDir := doltGCNudgeToolPath(t, false, map[string]string{
		"dolt": "#!/bin/sh\n: > \"$DOLT_SHOULD_NOT_RUN\"\nexit 0\n",
	})

	cmd := doltGCNudgeCommand(t, cityPath, binDir,
		"GC_DOLT_MANAGED_LOCAL=",
		"GC_DOLT_HOST=external.example.internal",
		"GC_DOLT_PORT=4406",
		"GC_DOLT_GC_THRESHOLD_BYTES=0",
		"DOLT_SHOULD_NOT_RUN="+shouldNotRun,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-nudge with unmarked external port failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "not a local managed Dolt host") {
		t.Fatalf("gc-nudge output = %q, want external-host skip", out)
	}
	if _, err := os.Stat(shouldNotRun); !os.IsNotExist(err) {
		t.Fatalf("dolt ran despite unconfirmed external port")
	}
}

func TestDoltGCNudgeSkipsUnmarkedExternalHostEvenWhenPortMatchesManagedRuntime(t *testing.T) {
	cityPath := writeDoltGCNudgeCity(t)
	port := writeDoltGCNudgeRuntimeState(t, cityPath)
	shouldNotRun := filepath.Join(t.TempDir(), "dolt-ran")
	binDir := doltGCNudgeToolPath(t, false, map[string]string{
		"dolt": "#!/bin/sh\n: > \"$DOLT_SHOULD_NOT_RUN\"\nexit 0\n",
	})

	cmd := doltGCNudgeCommand(t, cityPath, binDir,
		"GC_DOLT_MANAGED_LOCAL=",
		"GC_DOLT_HOST=external.example.internal",
		"GC_DOLT_PORT="+port,
		"GC_DOLT_GC_THRESHOLD_BYTES=0",
		"DOLT_SHOULD_NOT_RUN="+shouldNotRun,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-nudge with matching external port failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "not a local managed Dolt host") {
		t.Fatalf("gc-nudge output = %q, want external-host skip", out)
	}
	if _, err := os.Stat(shouldNotRun); !os.IsNotExist(err) {
		t.Fatalf("dolt ran despite unconfirmed external port")
	}
}

func TestDoltGCNudgeSkipsUnmarkedLocalhostPortWithoutManagedRuntime(t *testing.T) {
	cityPath := writeDoltGCNudgeCity(t)
	shouldNotRun := filepath.Join(t.TempDir(), "dolt-ran")
	binDir := doltGCNudgeToolPath(t, false, map[string]string{
		"dolt": "#!/bin/sh\n: > \"$DOLT_SHOULD_NOT_RUN\"\nexit 0\n",
	})

	cmd := doltGCNudgeCommand(t, cityPath, binDir,
		"GC_DOLT_MANAGED_LOCAL=",
		"GC_DOLT_HOST=127.0.0.1",
		"GC_DOLT_PORT=3307",
		"GC_DOLT_GC_THRESHOLD_BYTES=0",
		"DOLT_SHOULD_NOT_RUN="+shouldNotRun,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-nudge with unmarked local port failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "does not match managed runtime") {
		t.Fatalf("gc-nudge output = %q, want managed-runtime mismatch skip", out)
	}
	if _, err := os.Stat(shouldNotRun); !os.IsNotExist(err) {
		t.Fatalf("dolt ran despite unmarked local port without managed runtime")
	}
}

func TestDoltGCNudgeRejectsInvalidThreshold(t *testing.T) {
	cityPath := writeDoltGCNudgeCity(t)
	binDir := doltGCNudgeToolPath(t, true, nil)

	cmd := doltGCNudgeCommand(t, cityPath, binDir,
		"GC_DOLT_PORT=3307",
		"GC_DOLT_GC_DRY_RUN=1",
		"GC_DOLT_GC_THRESHOLD_BYTES=2GB",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("gc-nudge with invalid threshold succeeded; output:\n%s", out)
	}
	if !strings.Contains(string(out), "invalid GC_DOLT_GC_THRESHOLD_BYTES") {
		t.Fatalf("gc-nudge output = %q, want invalid-threshold message", out)
	}
}

func TestDoltGCNudgePassesPasswordThroughEnvironment(t *testing.T) {
	cityPath := writeDoltGCNudgeCity(t)
	captureDir := t.TempDir()
	binDir := doltGCNudgeToolPath(t, true, map[string]string{
		"dolt": "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$DOLT_ARGV_CAPTURE\"\nprintf '%s\\n' \"${DOLT_CLI_PASSWORD:-}\" > \"$DOLT_PASSWORD_CAPTURE\"\nexit 0\n",
	})

	cmd := doltGCNudgeCommand(t, cityPath, binDir,
		"GC_DOLT_PORT=3307",
		"GC_DOLT_PASSWORD=supersecret",
		"GC_DOLT_GC_THRESHOLD_BYTES=0",
		"DOLT_ARGV_CAPTURE="+filepath.Join(captureDir, "argv"),
		"DOLT_PASSWORD_CAPTURE="+filepath.Join(captureDir, "password"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-nudge with fake dolt failed: %v\n%s", err, out)
	}

	argv := readFileString(t, filepath.Join(captureDir, "argv"))
	if strings.Contains(argv, "supersecret") || strings.Contains(argv, "--password") {
		t.Fatalf("dolt argv leaked password or --password flag: %q", argv)
	}
	password := strings.TrimSpace(readFileString(t, filepath.Join(captureDir, "password")))
	if password != "supersecret" {
		t.Fatalf("DOLT_CLI_PASSWORD = %q, want supersecret", password)
	}
}

func TestDoltGCNudgeFailsIfAnyDatabaseGCFails(t *testing.T) {
	cityPath := writeDoltGCNudgeCity(t)
	rigMeta := filepath.Join(cityPath, "rigs", "demo", ".beads", "metadata.json")
	if err := os.MkdirAll(filepath.Dir(rigMeta), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rigMeta, []byte(`{"dolt_database":"rigdb"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads", "dolt", "rigdb", ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "dolt", "rigdb", ".dolt", "chunk"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	captureDir := t.TempDir()
	binDir := doltGCNudgeToolPath(t, true, map[string]string{
		"dolt": "#!/bin/sh\ncount_file=${DOLT_CALL_COUNT_FILE:?}\ncount=0\nif [ -f \"$count_file\" ]; then\n  count=$(sed -n '1p' \"$count_file\")\nfi\ncount=$((count + 1))\nprintf '%s\\n' \"$count\" > \"$count_file\"\nif [ \"$count\" -eq 1 ]; then\n  exit 7\nfi\nexit 0\n",
		"gc":   "#!/bin/sh\nprintf '{\"rigs\":[{\"path\":\"%s/rigs/demo\"}]}\n' \"$GC_CITY_PATH\"\n",
	})

	cmd := doltGCNudgeCommand(t, cityPath, binDir,
		"GC_DOLT_PORT=3307",
		"GC_DOLT_GC_THRESHOLD_BYTES=0",
		"DOLT_CALL_COUNT_FILE="+filepath.Join(captureDir, "count"),
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("gc-nudge succeeded after one database GC failed:\n%s", out)
	}

	count := strings.TrimSpace(readFileString(t, filepath.Join(captureDir, "count")))
	if count != "2" {
		t.Fatalf("dolt call count = %q, want 2", count)
	}
}

func TestDoltGCNudgeDefaultCallTimeoutMatchesOrderBudget(t *testing.T) {
	cityPath := writeDoltGCNudgeCity(t)
	binDir := doltGCNudgeToolPath(t, true, map[string]string{
		"dolt": "#!/bin/sh\nexit 124\n",
	})

	cmd := doltGCNudgeCommand(t, cityPath, binDir,
		"GC_DOLT_PORT=3307",
		"GC_DOLT_GC_THRESHOLD_BYTES=0",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("gc-nudge succeeded despite timed-out fake dolt:\n%s", out)
	}
	if !strings.Contains(string(out), "timed out after=1800s") {
		t.Fatalf("gc-nudge output = %q, want default timeout marker", out)
	}
}

func TestDoltGCNudgeBoundsGCCall(t *testing.T) {
	skipSlowCmdGCTest(t, "runs dolt GC nudge shell timeout coverage; run make test-cmd-gc-process for full coverage")
	if _, err := exec.LookPath("timeout"); err != nil {
		if _, gtimeoutErr := exec.LookPath("gtimeout"); gtimeoutErr != nil {
			t.Skip("timeout/gtimeout not available")
		}
	}

	cityPath := writeDoltGCNudgeCity(t)
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatalf("LookPath(sleep): %v", err)
	}
	binDir := doltGCNudgeToolPath(t, true, map[string]string{
		"dolt": "#!/bin/sh\n" + sleepPath + " 5\nexit 0\n",
	})

	cmd := doltGCNudgeCommand(t, cityPath, binDir,
		"GC_DOLT_PORT=3307",
		"GC_DOLT_GC_THRESHOLD_BYTES=0",
		"GC_DOLT_GC_CALL_TIMEOUT_SECS=1",
	)
	start := time.Now()
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("gc-nudge succeeded despite hung dolt GC call:\n%s", out)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("gc-nudge elapsed = %s, want bounded timeout; output:\n%s", elapsed, out)
	}
	if !strings.Contains(string(out), "timed out after=1s") {
		t.Fatalf("gc-nudge output = %q, want timeout marker", out)
	}
}

func TestDoltGCNudgeFailsClosedWithoutBoundedRunner(t *testing.T) {
	cityPath := writeDoltGCNudgeCity(t)
	binDir := doltGCNudgeToolPath(t, false, map[string]string{
		"dolt": "#!/bin/sh\nexit 0\n",
	})
	for _, name := range []string{"gtimeout", "timeout", "python3"} {
		if err := os.Remove(filepath.Join(binDir, name)); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove %s: %v", name, err)
		}
	}

	cmd := doltGCNudgeCommand(t, cityPath, binDir,
		"GC_DOLT_PORT=3307",
		"GC_DOLT_GC_THRESHOLD_BYTES=0",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("gc-nudge succeeded without bounded runner:\n%s", out)
	}
	if !strings.Contains(string(out), "cannot run bounded command") {
		t.Fatalf("gc-nudge output = %q, want bounded-runner failure", out)
	}
}

func TestDoltGCNudgeFallbackLockHonorsFlockHolder(t *testing.T) {
	skipSlowCmdGCTest(t, "runs dolt GC nudge shell lock contention coverage; run make test-cmd-gc-process for full coverage")
	cityPath := writeDoltGCNudgeCity(t)
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatalf("LookPath(sleep): %v", err)
	}

	captureDir := t.TempDir()
	startedFile := filepath.Join(captureDir, "started")
	withFlock := doltGCNudgeToolPath(t, true, map[string]string{
		"dolt": "#!/bin/sh\n: > \"$DOLT_STARTED_FILE\"\n" + sleepPath + " 2\nexit 0\n",
	})

	cmd1 := doltGCNudgeCommand(t, cityPath, withFlock,
		"GC_DOLT_PORT=3307",
		"GC_DOLT_GC_THRESHOLD_BYTES=0",
		"DOLT_STARTED_FILE="+startedFile,
	)
	var firstStdout, firstStderr bytes.Buffer
	cmd1.Stdout = &firstStdout
	cmd1.Stderr = &firstStderr
	if err := cmd1.Start(); err != nil {
		t.Fatalf("start first gc-nudge: %v", err)
	}
	waitForFile(t, startedFile)
	defer func() {
		if err := cmd1.Wait(); err != nil {
			t.Fatalf("wait first gc-nudge: %v\nstdout=%s\nstderr=%s", err, firstStdout.String(), firstStderr.String())
		}
	}()

	shouldNotRun := filepath.Join(captureDir, "second-dolt")
	withoutFlock := doltGCNudgeToolPath(t, false, map[string]string{
		"dolt": "#!/bin/sh\n: > \"$DOLT_SHOULD_NOT_RUN\"\nexit 0\n",
	})
	cmd2 := doltGCNudgeCommand(t, cityPath, withoutFlock,
		"GC_DOLT_PORT=3307",
		"GC_DOLT_GC_THRESHOLD_BYTES=0",
		"DOLT_SHOULD_NOT_RUN="+shouldNotRun,
	)
	out, err := cmd2.CombinedOutput()
	if err != nil {
		t.Fatalf("second gc-nudge failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "another nudge already running") {
		t.Fatalf("second gc-nudge output = %q, want lock-held message", out)
	}
	if _, err := os.Stat(shouldNotRun); !os.IsNotExist(err) {
		t.Fatalf("second dolt invocation ran despite active flock lock")
	}
}

func TestDoltGCNudgeLockNormalizesLocalHostAliases(t *testing.T) {
	skipSlowCmdGCTest(t, "runs dolt GC nudge shell lock contention coverage; run make test-cmd-gc-process for full coverage")
	cityPath := writeDoltGCNudgeCity(t)
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatalf("LookPath(sleep): %v", err)
	}

	captureDir := t.TempDir()
	tmpDir := t.TempDir()
	startedFile := filepath.Join(captureDir, "started")
	withHostAlias := doltGCNudgeToolPath(t, false, map[string]string{
		"dolt": "#!/bin/sh\n: > \"$DOLT_STARTED_FILE\"\n" + sleepPath + " 2\nexit 0\n",
	})

	cmd1 := doltGCNudgeCommand(t, cityPath, withHostAlias,
		"GC_DOLT_HOST=127.0.0.1",
		"GC_DOLT_PORT=3307",
		"GC_DOLT_GC_THRESHOLD_BYTES=0",
		"DOLT_STARTED_FILE="+startedFile,
		"TMPDIR="+tmpDir,
	)
	var firstStdout, firstStderr bytes.Buffer
	cmd1.Stdout = &firstStdout
	cmd1.Stderr = &firstStderr
	if err := cmd1.Start(); err != nil {
		t.Fatalf("start first gc-nudge: %v", err)
	}
	waitForFile(t, startedFile)
	defer func() {
		if err := cmd1.Wait(); err != nil {
			t.Fatalf("wait first gc-nudge: %v\nstdout=%s\nstderr=%s", err, firstStdout.String(), firstStderr.String())
		}
	}()

	shouldNotRun := filepath.Join(captureDir, "second-dolt")
	withoutFlock := doltGCNudgeToolPath(t, false, map[string]string{
		"dolt": "#!/bin/sh\n: > \"$DOLT_SHOULD_NOT_RUN\"\nexit 0\n",
	})
	cmd2 := doltGCNudgeCommand(t, cityPath, withoutFlock,
		"GC_DOLT_HOST=localhost",
		"GC_DOLT_PORT=3307",
		"GC_DOLT_GC_THRESHOLD_BYTES=0",
		"DOLT_SHOULD_NOT_RUN="+shouldNotRun,
		"TMPDIR="+tmpDir,
	)
	out, err := cmd2.CombinedOutput()
	if err != nil {
		t.Fatalf("second gc-nudge failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "another nudge already running") {
		t.Fatalf("second gc-nudge output = %q, want lock-held message", out)
	}
	if _, err := os.Stat(shouldNotRun); !os.IsNotExist(err) {
		t.Fatalf("second dolt invocation ran despite alias-normalized lock")
	}
}

func TestDoltGCNudgeLockIgnoresDifferentTmpDirs(t *testing.T) {
	skipSlowCmdGCTest(t, "runs dolt GC nudge shell lock contention coverage; run make test-cmd-gc-process for full coverage")
	cityPath := writeDoltGCNudgeCity(t)
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatalf("LookPath(sleep): %v", err)
	}
	lockDir := doltGCNudgeTestLockDir(t)
	_ = os.RemoveAll(lockDir)
	t.Cleanup(func() { _ = os.RemoveAll(lockDir) })

	captureDir := t.TempDir()
	startedFile := filepath.Join(captureDir, "started")
	withTmpOne := doltGCNudgeToolPath(t, false, map[string]string{
		"dolt": "#!/bin/sh\n: > \"$DOLT_STARTED_FILE\"\n" + sleepPath + " 2\nexit 0\n",
	})

	cmd1 := doltGCNudgeCommand(t, cityPath, withTmpOne,
		"GC_DOLT_HOST=127.0.0.1",
		"GC_DOLT_PORT=3307",
		"GC_DOLT_GC_THRESHOLD_BYTES=0",
		"DOLT_STARTED_FILE="+startedFile,
		"TMPDIR="+t.TempDir(),
	)
	var firstStdout, firstStderr bytes.Buffer
	cmd1.Stdout = &firstStdout
	cmd1.Stderr = &firstStderr
	if err := cmd1.Start(); err != nil {
		t.Fatalf("start first gc-nudge: %v", err)
	}
	waitForFile(t, startedFile)
	defer func() {
		if err := cmd1.Wait(); err != nil {
			t.Fatalf("wait first gc-nudge: %v\nstdout=%s\nstderr=%s", err, firstStdout.String(), firstStderr.String())
		}
	}()

	shouldNotRun := filepath.Join(captureDir, "second-dolt")
	withTmpTwo := doltGCNudgeToolPath(t, false, map[string]string{
		"dolt": "#!/bin/sh\n: > \"$DOLT_SHOULD_NOT_RUN\"\nexit 0\n",
	})
	cmd2 := doltGCNudgeCommand(t, cityPath, withTmpTwo,
		"GC_DOLT_HOST=127.0.0.1",
		"GC_DOLT_PORT=3307",
		"GC_DOLT_GC_THRESHOLD_BYTES=0",
		"DOLT_SHOULD_NOT_RUN="+shouldNotRun,
		"TMPDIR="+t.TempDir(),
	)
	out, err := cmd2.CombinedOutput()
	if err != nil {
		t.Fatalf("second gc-nudge failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "another nudge already running") {
		t.Fatalf("second gc-nudge output = %q, want lock-held message", out)
	}
	if _, err := os.Stat(shouldNotRun); !os.IsNotExist(err) {
		t.Fatalf("second dolt invocation ran despite different TMPDIR")
	}
}

func TestDoltGCNudgeRecoversStaleLockMarker(t *testing.T) {
	for _, tc := range []struct {
		name         string
		includeFlock bool
	}{
		{name: "with flock", includeFlock: true},
		{name: "without flock", includeFlock: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := writeDoltGCNudgeCity(t)
			captureDir := t.TempDir()
			lockDir := doltGCNudgeTestLockDir(t)
			_ = os.RemoveAll(lockDir)
			t.Cleanup(func() { _ = os.RemoveAll(lockDir) })
			if err := os.MkdirAll(lockDir, 0o700); err != nil {
				t.Fatalf("MkdirAll(lockDir): %v", err)
			}
			if err := os.WriteFile(filepath.Join(lockDir, "pid"), []byte("999999\n"), 0o600); err != nil {
				t.Fatalf("WriteFile(lock pid): %v", err)
			}

			ranFile := filepath.Join(captureDir, "dolt-ran")
			binDir := doltGCNudgeToolPath(t, tc.includeFlock, map[string]string{
				"dolt": "#!/bin/sh\n: > \"$DOLT_RAN_FILE\"\nexit 0\n",
			})

			cmd := doltGCNudgeCommand(t, cityPath, binDir,
				"GC_DOLT_PORT=3307",
				"GC_DOLT_GC_THRESHOLD_BYTES=0",
				"DOLT_RAN_FILE="+ranFile,
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("gc-nudge with stale lock marker failed: %v\n%s", err, out)
			}
			if strings.Contains(string(out), "another nudge already running") {
				t.Fatalf("gc-nudge treated stale lock as active:\n%s", out)
			}
			if _, err := os.Stat(ranFile); err != nil {
				t.Fatalf("dolt did not run after stale lock recovery: %v\n%s", err, out)
			}
			if _, err := os.Stat(lockDir); !os.IsNotExist(err) {
				t.Fatalf("stale lock dir still exists after run, err=%v", err)
			}
		})
	}
}

func TestDoltGCNudgeSkipsExternalRigDatabaseWithoutLocalData(t *testing.T) {
	cityPath := writeDoltGCNudgeCity(t)
	externalRigDir := t.TempDir()
	externalMeta := filepath.Join(externalRigDir, ".beads", "metadata.json")
	if err := os.MkdirAll(filepath.Dir(externalMeta), 0o755); err != nil {
		t.Fatalf("MkdirAll(externalMeta): %v", err)
	}
	if err := os.WriteFile(externalMeta, []byte(`{"dolt_database":"extdb"}`), 0o644); err != nil {
		t.Fatalf("WriteFile(externalMeta): %v", err)
	}

	captureDir := t.TempDir()
	argvCapture := filepath.Join(captureDir, "argv")
	binDir := doltGCNudgeToolPath(t, true, map[string]string{
		"dolt": "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$DOLT_ARGV_CAPTURE\"\nexit 0\n",
		"gc":   "#!/bin/sh\nprintf '{\"rigs\":[{\"path\":\"%s\"}]}\n' \"$EXTERNAL_RIG_PATH\"\n",
	})

	cmd := doltGCNudgeCommand(t, cityPath, binDir,
		"GC_DOLT_PORT=3307",
		"GC_DOLT_GC_THRESHOLD_BYTES=0",
		"DOLT_ARGV_CAPTURE="+argvCapture,
		"EXTERNAL_RIG_PATH="+externalRigDir,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-nudge with external rig metadata failed: %v\n%s", err, out)
	}

	argv := strings.TrimSpace(readFileString(t, argvCapture))
	lines := strings.Split(argv, "\n")
	if len(lines) != 1 {
		t.Fatalf("dolt argv lines = %d, want 1 for local managed db only:\n%s", len(lines), argv)
	}
	if !strings.Contains(lines[0], "--use-db testdb") {
		t.Fatalf("dolt argv = %q, want local managed testdb", lines[0])
	}
	if strings.Contains(argv, "--use-db extdb") {
		t.Fatalf("dolt argv should not target external rig db:\n%s", argv)
	}
}

func TestDoltGCNudgeDefaultsMissingDatabaseMetadataToBeads(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads", "dolt", "beads", ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "dolt", "beads", ".dolt", "chunk"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	captureDir := t.TempDir()
	argvCapture := filepath.Join(captureDir, "argv")
	binDir := doltGCNudgeToolPath(t, true, map[string]string{
		"dolt": "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$DOLT_ARGV_CAPTURE\"\nexit 0\n",
	})

	cmd := doltGCNudgeCommand(t, cityPath, binDir,
		"GC_DOLT_PORT=3307",
		"GC_DOLT_GC_THRESHOLD_BYTES=0",
		"DOLT_ARGV_CAPTURE="+argvCapture,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-nudge with missing dolt_database failed: %v\n%s", err, out)
	}

	argv := strings.TrimSpace(readFileString(t, argvCapture))
	if !strings.Contains(argv, "--use-db beads") {
		t.Fatalf("dolt argv = %q, want default beads database", argv)
	}
}

func TestDoltGCNudgeSkipsInvalidDatabaseMetadata(t *testing.T) {
	cityPath := writeDoltGCNudgeCity(t)
	rigMeta := filepath.Join(cityPath, "rigs", "bad", ".beads", "metadata.json")
	if err := os.MkdirAll(filepath.Dir(rigMeta), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rigMeta, []byte(`{"dolt_database":"--help"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	captureDir := t.TempDir()
	argvCapture := filepath.Join(captureDir, "argv")
	binDir := doltGCNudgeToolPath(t, true, map[string]string{
		"dolt": "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$DOLT_ARGV_CAPTURE\"\nexit 0\n",
	})

	cmd := doltGCNudgeCommand(t, cityPath, binDir,
		"GC_DOLT_PORT=3307",
		"GC_DOLT_GC_THRESHOLD_BYTES=0",
		"DOLT_ARGV_CAPTURE="+argvCapture,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-nudge with invalid database metadata failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "invalid database name") {
		t.Fatalf("gc-nudge output = %q, want invalid database message", out)
	}

	argv := strings.TrimSpace(readFileString(t, argvCapture))
	lines := strings.Split(argv, "\n")
	if len(lines) != 1 {
		t.Fatalf("dolt argv lines = %d, want 1 valid database:\n%s", len(lines), argv)
	}
	if !strings.Contains(lines[0], "--use-db testdb") {
		t.Fatalf("dolt argv = %q, want local managed testdb", lines[0])
	}
	if strings.Contains(argv, "--use-db --help") {
		t.Fatalf("dolt argv should not target invalid database:\n%s", argv)
	}
}

func TestDoltGCNudgeSkipsSystemDatabaseMetadata(t *testing.T) {
	cityPath := writeDoltGCNudgeCity(t)
	rigMeta := filepath.Join(cityPath, "rigs", "bad", ".beads", "metadata.json")
	if err := os.MkdirAll(filepath.Dir(rigMeta), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rigMeta, []byte(`{"dolt_database":"mysql"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads", "dolt", "mysql", ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}

	captureDir := t.TempDir()
	argvCapture := filepath.Join(captureDir, "argv")
	binDir := doltGCNudgeToolPath(t, true, map[string]string{
		"dolt": "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$DOLT_ARGV_CAPTURE\"\nexit 0\n",
	})

	cmd := doltGCNudgeCommand(t, cityPath, binDir,
		"GC_DOLT_PORT=3307",
		"GC_DOLT_GC_THRESHOLD_BYTES=0",
		"DOLT_ARGV_CAPTURE="+argvCapture,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-nudge with system database metadata failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "system database") {
		t.Fatalf("gc-nudge output = %q, want system database message", out)
	}

	argv := strings.TrimSpace(readFileString(t, argvCapture))
	if strings.Contains(argv, "--use-db mysql") {
		t.Fatalf("dolt argv should not target system database:\n%s", argv)
	}
	if !strings.Contains(argv, "--use-db testdb") {
		t.Fatalf("dolt argv = %q, want valid testdb", argv)
	}
}

func TestDoltGCNudgeAllowsHyphenatedDatabaseMetadata(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads", "dolt", "frontend-db", ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(`{"dolt_database":"frontend-db"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "dolt", "frontend-db", ".dolt", "chunk"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	captureDir := t.TempDir()
	argvCapture := filepath.Join(captureDir, "argv")
	binDir := doltGCNudgeToolPath(t, true, map[string]string{
		"dolt": "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$DOLT_ARGV_CAPTURE\"\nexit 0\n",
	})

	cmd := doltGCNudgeCommand(t, cityPath, binDir,
		"GC_DOLT_PORT=3307",
		"GC_DOLT_GC_THRESHOLD_BYTES=0",
		"DOLT_ARGV_CAPTURE="+argvCapture,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-nudge with hyphenated database metadata failed: %v\n%s", err, out)
	}

	argv := strings.TrimSpace(readFileString(t, argvCapture))
	if !strings.Contains(argv, "--use-db frontend-db") {
		t.Fatalf("dolt argv = %q, want hyphenated database", argv)
	}
}

func TestDoltGCNudgeHonorsDataDirOverride(t *testing.T) {
	cityPath := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "managed-dolt")
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(`{"dolt_database":"testdb"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "testdb", ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "testdb", ".dolt", "chunk"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	captureDir := t.TempDir()
	argvCapture := filepath.Join(captureDir, "argv")
	binDir := doltGCNudgeToolPath(t, true, map[string]string{
		"dolt": "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$DOLT_ARGV_CAPTURE\"\nexit 0\n",
	})

	cmd := doltGCNudgeCommand(t, cityPath, binDir,
		"GC_DOLT_PORT=3307",
		"GC_DOLT_DATA_DIR="+dataDir,
		"GC_DOLT_GC_THRESHOLD_BYTES=0",
		"DOLT_ARGV_CAPTURE="+argvCapture,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-nudge with GC_DOLT_DATA_DIR failed: %v\n%s", err, out)
	}

	argv := strings.TrimSpace(readFileString(t, argvCapture))
	if !strings.Contains(argv, "--use-db testdb") {
		t.Fatalf("dolt argv = %q, want override-backed testdb", argv)
	}
}

func TestDoltGCNudgeDiscoversOrphanDatabaseDirs(t *testing.T) {
	cityPath := writeDoltGCNudgeCity(t)
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads", "dolt", "orphan-db", ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "dolt", "orphan-db", ".dolt", "chunk"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	captureDir := t.TempDir()
	argvCapture := filepath.Join(captureDir, "argv")
	binDir := doltGCNudgeToolPath(t, true, map[string]string{
		"dolt": "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$DOLT_ARGV_CAPTURE\"\nexit 0\n",
	})

	cmd := doltGCNudgeCommand(t, cityPath, binDir,
		"GC_DOLT_PORT=3307",
		"GC_DOLT_GC_THRESHOLD_BYTES=0",
		"DOLT_ARGV_CAPTURE="+argvCapture,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-nudge with orphan database dir failed: %v\n%s", err, out)
	}

	argv := strings.TrimSpace(readFileString(t, argvCapture))
	if !strings.Contains(argv, "--use-db orphan-db") {
		t.Fatalf("dolt argv = %q, want orphan database", argv)
	}
}

func TestDoltGCNudgeAggregateThresholdTriggersSubthresholdDatabases(t *testing.T) {
	cityPath := writeDoltGCNudgeCity(t)
	rigMeta := filepath.Join(cityPath, "rigs", "demo", ".beads", "metadata.json")
	if err := os.MkdirAll(filepath.Dir(rigMeta), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rigMeta, []byte(`{"dolt_database":"rigdb"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads", "dolt", "rigdb", ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "dolt", "rigdb", ".dolt", "chunk"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	testDBSize := doltGCNudgeDirBytes(t, filepath.Join(cityPath, ".beads", "dolt", "testdb", ".dolt"))
	rigDBSize := doltGCNudgeDirBytes(t, filepath.Join(cityPath, ".beads", "dolt", "rigdb", ".dolt"))
	threshold := testDBSize
	if rigDBSize > threshold {
		threshold = rigDBSize
	}
	threshold++
	if testDBSize+rigDBSize < threshold {
		t.Fatalf("test setup cannot create aggregate threshold: testdb=%d rigdb=%d threshold=%d", testDBSize, rigDBSize, threshold)
	}

	captureDir := t.TempDir()
	argvCapture := filepath.Join(captureDir, "argv")
	binDir := doltGCNudgeToolPath(t, true, map[string]string{
		"dolt": "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$DOLT_ARGV_CAPTURE\"\nexit 0\n",
	})

	cmd := doltGCNudgeCommand(t, cityPath, binDir,
		"GC_DOLT_PORT=3307",
		"GC_DOLT_GC_THRESHOLD_BYTES="+strconv.FormatInt(threshold, 10),
		"DOLT_ARGV_CAPTURE="+argvCapture,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-nudge aggregate threshold failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "aggregate_bytes=") {
		t.Fatalf("gc-nudge output = %q, want aggregate trigger marker", out)
	}

	argv := strings.TrimSpace(readFileString(t, argvCapture))
	if !strings.Contains(argv, "--use-db testdb") || !strings.Contains(argv, "--use-db rigdb") {
		t.Fatalf("dolt argv = %q, want both subthreshold databases under aggregate trigger", argv)
	}
}

func TestDoltGCNudgeFallbackFindsLocalRigOutsideRigsDir(t *testing.T) {
	cityPath := writeDoltGCNudgeCity(t)
	rigMeta := filepath.Join(cityPath, "frontend", ".beads", "metadata.json")
	if err := os.MkdirAll(filepath.Dir(rigMeta), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rigMeta, []byte(`{"dolt_database":"frontenddb"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads", "dolt", "frontenddb", ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "dolt", "frontenddb", ".dolt", "chunk"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	captureDir := t.TempDir()
	argvCapture := filepath.Join(captureDir, "argv")
	binDir := doltGCNudgeToolPath(t, true, map[string]string{
		"dolt": "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$DOLT_ARGV_CAPTURE\"\nexit 0\n",
	})

	cmd := doltGCNudgeCommand(t, cityPath, binDir,
		"GC_DOLT_PORT=3307",
		"GC_DOLT_GC_THRESHOLD_BYTES=0",
		"DOLT_ARGV_CAPTURE="+argvCapture,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-nudge fallback scan failed: %v\n%s", err, out)
	}

	argv := strings.TrimSpace(readFileString(t, argvCapture))
	lines := strings.Split(argv, "\n")
	if len(lines) != 2 {
		t.Fatalf("dolt argv lines = %d, want 2 databases from fallback scan:\n%s", len(lines), argv)
	}
	if !strings.Contains(argv, "--use-db testdb") {
		t.Fatalf("dolt argv = %q, want city database", argv)
	}
	if !strings.Contains(argv, "--use-db frontenddb") {
		t.Fatalf("dolt argv = %q, want rig database outside rigs/ dir", argv)
	}
}

func TestDoltGCNudgeWarnsWhenRigListFailsBeforeFallback(t *testing.T) {
	cityPath := writeDoltGCNudgeCity(t)
	captureDir := t.TempDir()
	argvCapture := filepath.Join(captureDir, "argv")
	binDir := doltGCNudgeToolPath(t, true, map[string]string{
		"dolt": "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$DOLT_ARGV_CAPTURE\"\nexit 0\n",
		"gc":   "#!/bin/sh\nexit 7\n",
	})

	cmd := doltGCNudgeCommand(t, cityPath, binDir,
		"GC_DOLT_PORT=3307",
		"GC_DOLT_GC_THRESHOLD_BYTES=0",
		"DOLT_ARGV_CAPTURE="+argvCapture,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-nudge fallback after rig list failure failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "gc rig list failed rc=7") {
		t.Fatalf("gc-nudge output = %q, want rig-list failure warning", out)
	}
	if !strings.Contains(readFileString(t, argvCapture), "--use-db testdb") {
		t.Fatalf("gc-nudge did not fall back to local metadata scan; output:\n%s", out)
	}
}

func writeDoltGCNudgeCity(t *testing.T) string {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads", "dolt", "testdb", ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(`{"dolt_database":"testdb"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "dolt", "testdb", ".dolt", "chunk"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	return cityPath
}

func writeDoltGCNudgeRuntimeState(t *testing.T, cityPath string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	stateDir := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	state := `{"running":true,"pid":` + strconv.Itoa(os.Getpid()) + `,"port":` + port + `,"data_dir":"` + filepath.Join(cityPath, ".beads", "dolt") + `"}`
	if err := os.WriteFile(filepath.Join(stateDir, "dolt-state.json"), []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}
	return port
}

func doltGCNudgeCommand(t *testing.T, cityPath, binDir string, extraEnv ...string) *exec.Cmd {
	t.Helper()
	scriptPath, err := filepath.Abs(filepath.Join("..", "..", "examples", "dolt", "commands", "gc-nudge", "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	packDir, err := filepath.Abs(filepath.Join("..", "..", "examples", "dolt"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(scriptPath)
	baseEnv := []string{
		"PATH=" + binDir,
		"GC_CITY_PATH=" + cityPath,
		"GC_PACK_DIR=" + packDir,
	}
	if !doltGCNudgeEnvHasKey(extraEnv, "GC_DOLT_MANAGED_LOCAL") {
		baseEnv = append(baseEnv, "GC_DOLT_MANAGED_LOCAL=1")
	}
	cmd.Env = append([]string{}, baseEnv...)
	extraEnv = doltGCNudgeIsolatedEnv(t, extraEnv)
	cmd.Env = append(cmd.Env, extraEnv...)
	return cmd
}

func doltGCNudgeIsolatedEnv(t *testing.T, env []string) []string {
	t.Helper()
	out := append([]string{}, env...)
	for i, entry := range out {
		if entry == "GC_DOLT_PORT=3307" {
			out[i] = "GC_DOLT_PORT=" + doltGCNudgeTestPort(t)
		}
	}
	return out
}

func doltGCNudgeTestLockDir(t *testing.T) string {
	t.Helper()
	return filepath.Join("/tmp", "gc-dolt-gc", "127.0.0.1-"+doltGCNudgeTestPort(t)+".lock.d")
}

func doltGCNudgeTestPort(t *testing.T) string {
	t.Helper()
	h := fnv.New32a()
	_, _ = h.Write([]byte(t.Name()))
	return strconv.Itoa(20000 + int(h.Sum32()%20000))
}

func doltGCNudgeEnvHasKey(env []string, key string) bool {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}

func doltGCNudgeToolPath(t *testing.T, includeFlock bool, custom map[string]string) string {
	t.Helper()
	binDir := t.TempDir()
	for _, name := range []string{"awk", "chmod", "date", "dirname", "du", "find", "grep", "head", "mkdir", "mktemp", "python3", "rm", "rmdir", "sed", "sleep", "tr"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("LookPath(%s): %v", name, err)
		}
		if err := os.Symlink(path, filepath.Join(binDir, name)); err != nil {
			t.Fatalf("symlink %s: %v", name, err)
		}
	}
	for _, name := range []string{"gtimeout", "timeout"} {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if err := os.Symlink(path, filepath.Join(binDir, name)); err != nil {
			t.Fatalf("symlink %s: %v", name, err)
		}
	}
	if includeFlock {
		path, err := exec.LookPath("flock")
		if err != nil {
			t.Fatalf("LookPath(flock): %v", err)
		}
		if err := os.Symlink(path, filepath.Join(binDir, "flock")); err != nil {
			t.Fatalf("symlink flock: %v", err)
		}
	}
	for name, body := range custom {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	return binDir
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func doltGCNudgeDirBytes(t *testing.T, path string) int64 {
	t.Helper()
	out, err := exec.Command("du", "-sk", path).Output()
	if err != nil {
		t.Fatalf("du -sk %s: %v", path, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		t.Fatalf("du -sk %s produced empty output", path)
	}
	kb, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		t.Fatalf("parse du output %q: %v", fields[0], err)
	}
	return kb * 1024
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}
