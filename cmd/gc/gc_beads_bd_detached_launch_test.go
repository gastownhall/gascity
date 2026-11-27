package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// detachedLaunchHarness extracts launch_dolt_sql_server_detached from
// gc-beads-bd.sh into a standalone script that runs it once and prints the
// PID it reports. prelude runs inside the harness before the launch, so a
// test can start the starter with monitor mode on.
func detachedLaunchHarness(t *testing.T, configFile, logFile, prelude string) string {
	t.Helper()
	root := repoRootForLint(t)
	scriptBytes, err := os.ReadFile(filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh"))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	var harness strings.Builder
	harness.WriteString("#!/bin/sh\nset -u\n")
	harness.WriteString("CONFIG_FILE=" + shellQuotePOSIXPath(configFile) + "\n")
	harness.WriteString("LOG_FILE=" + shellQuotePOSIXPath(logFile) + "\n")
	if prelude != "" {
		harness.WriteString(prelude + "\n")
	}
	harness.WriteString(extractShellFunction(t, string(scriptBytes), "launch_dolt_sql_server_detached"))
	harness.WriteString("\nlaunch_dolt_sql_server_detached\n")
	return harness.String()
}

// detachedLaunchToolDir symlinks every command from the standard command
// directories except setsid, so a PATH naming it hides setsid(1) without
// hiding nohup, sh or sleep.
func detachedLaunchToolDir(t *testing.T) string {
	t.Helper()
	toolDir := filepath.Join(t.TempDir(), "tools")
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		t.Fatalf("mkdir tools: %v", err)
	}
	seen := map[string]bool{}
	for _, dir := range []string{"/usr/bin", "/bin"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if name == "setsid" || seen[name] {
				continue
			}
			seen[name] = true
			if err := os.Symlink(filepath.Join(dir, name), filepath.Join(toolDir, name)); err != nil {
				t.Fatalf("symlink %s: %v", name, err)
			}
		}
	}
	return toolDir
}

func detachedLaunchFileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestFallbackDoltLaunchSurvivesCallerSessionTeardown pins that a dolt
// sql-server started by the shell fallback (no gc helper) is not part of the
// process that started it, on every detach route the launcher can take: the
// server gets its own process group (setsid, or bash job control where
// setsid is missing), survives the starter's whole process group being
// SIGKILLed, keeps none of the starter's session identity, and the reported
// PID is the server's own — the start path writes it to the PID file and the
// saved state. Where no detach method exists (dash without setsid), the
// launcher says so on stderr instead of silently leaving the server in the
// caller's group.
func TestFallbackDoltLaunchSurvivesCallerSessionTeardown(t *testing.T) {
	const warnNoDetach = "warning: dolt sql-server shares the caller's process group: no setsid(1) and no bash job control"
	cases := []struct {
		name         string
		shell        string
		hideSetsid   bool
		forceMonitor bool
		wantWarn     string // "" expects no warning
		wantDetached bool
	}{
		{name: "setsid", shell: "/bin/sh", wantDetached: true},
		// Monitor mode makes the background job a process-group leader, so
		// setsid(1) would fork and $! would name the short-lived parent.
		{name: "setsid with monitor mode on", shell: "/bin/bash", forceMonitor: true, wantDetached: true},
		{name: "bash job control with setsid hidden", shell: "/bin/bash", hideSetsid: true, wantDetached: true},
		{name: "dash without setsid warns", shell: "/bin/dash", hideSetsid: true, wantWarn: warnNoDetach},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := exec.LookPath(tc.shell); err != nil {
				t.Skipf("%s not available: %v", tc.shell, err)
			}
			dir := t.TempDir()
			binDir := filepath.Join(dir, "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatalf("mkdir bin: %v", err)
			}
			pidFile := filepath.Join(dir, "dolt.pid")
			envFile := filepath.Join(dir, "dolt.env")
			// The fake server records its own PID and environment, then
			// stays up like a real one.
			writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/bin/sh
echo $$ > "$FAKE_DOLT_PID_FILE.tmp" && mv "$FAKE_DOLT_PID_FILE.tmp" "$FAKE_DOLT_PID_FILE"
env > "$FAKE_DOLT_ENV_FILE.tmp" && mv "$FAKE_DOLT_ENV_FILE.tmp" "$FAKE_DOLT_ENV_FILE"
exec sleep 60
`)
			logFile := filepath.Join(dir, "dolt.log")
			prelude := ""
			if tc.forceMonitor {
				prelude = "set -m"
			}
			harnessPath := filepath.Join(dir, "harness.sh")
			writeExecutable(t, harnessPath, detachedLaunchHarness(t, filepath.Join(dir, "config.yaml"), logFile, prelude))

			// The starter runs as the leader of its own process group,
			// standing in for an agent session's shell.
			cmd := exec.Command(tc.shell, harnessPath)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			env := os.Environ()
			for _, key := range append([]string{"PATH", "FAKE_DOLT_PID_FILE", "FAKE_DOLT_ENV_FILE"}, managedDoltSessionScopedEnvKeys...) {
				env = removeEnvKey(env, key)
			}
			path := binDir + string(os.PathListSeparator) + os.Getenv("PATH")
			if tc.hideSetsid {
				path = detachedLaunchToolDir(t) + string(os.PathListSeparator) + binDir
			}
			env = append(env,
				"PATH="+path,
				"FAKE_DOLT_PID_FILE="+pidFile,
				"FAKE_DOLT_ENV_FILE="+envFile,
				"GASCITY_DETACHED_LAUNCH_SENTINEL=present",
				"GC_SESSION_ID=starter-session",
				"GC_SESSION_NAME=starter",
				"GC_AGENT=starter-agent",
				"BEADS_HOLDER_TOKEN=starter-token",
			)
			cmd.Env = env
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("launch harness: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
			}
			starterPgid := cmd.Process.Pid
			pid, err := strconv.Atoi(strings.TrimSpace(stdout.String()))
			if err != nil || pid <= 0 {
				t.Fatalf("launch printed %q, want a PID", stdout.String())
			}
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

			deadline := time.Now().Add(10 * time.Second)
			for !detachedLaunchFileExists(pidFile) || !detachedLaunchFileExists(envFile) {
				if time.Now().After(deadline) {
					log, _ := os.ReadFile(logFile)
					t.Fatalf("fake dolt never started; log:\n%s\nstderr:\n%s", log, stderr.String())
				}
				time.Sleep(20 * time.Millisecond)
			}

			// The reported PID must be the server's own, not a wrapper's:
			// the start path writes it to the PID file and the saved state.
			recorded, err := strconv.Atoi(strings.TrimSpace(readFile(pidFile)))
			if err != nil || recorded <= 0 {
				t.Fatalf("fake dolt recorded pid %q", readFile(pidFile))
			}
			t.Cleanup(func() { _ = syscall.Kill(recorded, syscall.SIGKILL) })
			if pid != recorded {
				t.Errorf("launch reported pid %d, but the server runs as %d: the PID file would not name the server", pid, recorded)
			}

			if tc.wantWarn != "" && !strings.Contains(stderr.String(), tc.wantWarn) {
				t.Errorf("launch stderr missing %q:\n%s", tc.wantWarn, stderr.String())
			}
			if tc.wantDetached {
				if pgid, err := syscall.Getpgid(pid); err != nil {
					t.Errorf("getpgid(%d): %v", pid, err)
				} else if pgid == starterPgid {
					t.Errorf("dolt pid %d is in the starter's process group %d", pid, starterPgid)
				}
				// Tear the starter's session down the way a session kill
				// does: signal its whole process group, then give the
				// signal time to land before checking the server survived.
				_ = syscall.Kill(-starterPgid, syscall.SIGKILL)
				settle := time.Now().Add(300 * time.Millisecond)
				for time.Now().Before(settle) {
					if err := syscall.Kill(pid, 0); err != nil {
						t.Fatalf("dolt pid %d died with the starter's process group: %v", pid, err)
					}
					time.Sleep(20 * time.Millisecond)
				}
			}

			envBytes := readFile(envFile)
			for _, line := range strings.Split(envBytes, "\n") {
				key, _, _ := strings.Cut(line, "=")
				for _, scoped := range managedDoltSessionScopedEnvKeys {
					if key == scoped {
						t.Errorf("dolt inherited the starter's %s", line)
					}
				}
			}
			if !strings.Contains(envBytes, "GASCITY_DETACHED_LAUNCH_SENTINEL=present") {
				t.Errorf("dolt env lost the non-session sentinel variable:\n%s", envBytes)
			}
		})
	}
}

// TestFallbackDoltLaunchStripsTheManagedSessionKeys keeps the shell
// fallback's list of session-scoped variables equal to the Go managed start's
// (managedDoltSessionScopedEnvKeys), so the two start paths cannot drift.
func TestFallbackDoltLaunchStripsTheManagedSessionKeys(t *testing.T) {
	root := repoRootForLint(t)
	scriptBytes, err := os.ReadFile(filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh"))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	body := extractShellFunction(t, string(scriptBytes), "launch_dolt_sql_server_detached")
	m := regexp.MustCompile(`(?s)for key in (.*?); do`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("launch_dolt_sql_server_detached has no `for key in ...; do` unset loop:\n%s", body)
	}
	got := strings.Fields(strings.ReplaceAll(m[1], "\\\n", " "))
	want := managedDoltSessionScopedEnvKeys
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("shell fallback strips %v, managed start strips %v", got, want)
	}
}
