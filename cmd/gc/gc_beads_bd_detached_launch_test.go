package main

import (
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
// PID it reports.
func detachedLaunchHarness(t *testing.T, configFile, logFile string) string {
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
	harness.WriteString(extractShellFunction(t, string(scriptBytes), "launch_dolt_sql_server_detached"))
	harness.WriteString("\nlaunch_dolt_sql_server_detached\n")
	return harness.String()
}

// TestFallbackDoltLaunchSurvivesCallerSessionTeardown pins that a dolt
// sql-server started by the shell fallback (no gc helper) is not part of the
// process that started it: killing the starter's whole process group leaves
// the server running, and the server does not carry the starter's session
// identity, so a process-table sweep keyed on GC_SESSION_ID would not count
// it as the starter's.
func TestFallbackDoltLaunchSurvivesCallerSessionTeardown(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	envFile := filepath.Join(dir, "dolt.env")
	// The fake server records its environment, then stays up like a real one.
	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/bin/sh
env > "$FAKE_DOLT_ENV_FILE.tmp" && mv "$FAKE_DOLT_ENV_FILE.tmp" "$FAKE_DOLT_ENV_FILE"
exec sleep 60
`)
	logFile := filepath.Join(dir, "dolt.log")
	harnessPath := filepath.Join(dir, "harness.sh")
	writeExecutable(t, harnessPath, detachedLaunchHarness(t, filepath.Join(dir, "config.yaml"), logFile))

	// The starter runs as the leader of its own process group, standing in
	// for an agent session's shell.
	cmd := exec.Command("/bin/sh", harnessPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	env := os.Environ()
	for _, key := range append([]string{"PATH", "FAKE_DOLT_ENV_FILE"}, managedDoltSessionScopedEnvKeys...) {
		env = removeEnvKey(env, key)
	}
	cmd.Env = append(env,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_DOLT_ENV_FILE="+envFile,
		"GC_SESSION_ID=starter-session",
		"GC_SESSION_NAME=starter",
		"GC_AGENT=starter-agent",
		"BEADS_HOLDER_TOKEN=starter-token",
	)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("launch harness: %v\n%s", err, out)
	}
	starterPgid := cmd.Process.Pid
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid <= 0 {
		t.Fatalf("launch printed %q, want a PID", out)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(envFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			log, _ := os.ReadFile(logFile)
			t.Fatalf("fake dolt never started; log:\n%s", log)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if pgid, err := syscall.Getpgid(pid); err != nil {
		t.Fatalf("getpgid(%d): %v", pid, err)
	} else if pgid == starterPgid {
		t.Errorf("dolt pid %d is in the starter's process group %d", pid, starterPgid)
	}

	envBytes, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read fake dolt env: %v", err)
	}
	for _, line := range strings.Split(string(envBytes), "\n") {
		key, _, _ := strings.Cut(line, "=")
		for _, scoped := range managedDoltSessionScopedEnvKeys {
			if key == scoped {
				t.Errorf("dolt inherited the starter's %s", line)
			}
		}
	}

	// Tear the starter's session down the way a session kill does: signal
	// its whole process group.
	_ = syscall.Kill(-starterPgid, syscall.SIGKILL)
	time.Sleep(300 * time.Millisecond)
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("dolt pid %d died with the starter's process group: %v", pid, err)
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
