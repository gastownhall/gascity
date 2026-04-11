package acceptancehelpers

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// StartWithSupervisor registers the city with the isolated supervisor
// and waits for it to come online. Stops any stale supervisor from a
// previous test first (tests share XDG_RUNTIME_DIR within a suite).
func (c *City) StartWithSupervisor() {
	c.t.Helper()
	// Stop stale supervisor/controller from a previous test.
	RunGC(c.Env, "", "supervisor", "stop") //nolint:errcheck
	RunGC(c.Env, c.Dir, "stop", c.Dir)     //nolint:errcheck
	time.Sleep(2 * time.Second)

	out, err := RunGC(c.Env, c.Dir, "start", c.Dir)
	if err != nil {
		c.t.Fatalf("gc start failed: %v\n%s", err, out)
	}
	c.started = true
	c.usedSupervisor = true
	c.t.Cleanup(func() {
		c.Stop()
		RunGC(c.Env, "", "supervisor", "stop") //nolint:errcheck
	})
}

// StartForeground starts gc in --foreground mode in the background and
// leaves it running until Stop is called. The controller log is written
// to .gc/acceptance-controller.log inside the city.
func (c *City) StartForeground() {
	c.t.Helper()
	if c.started {
		c.Stop()
	}

	gcPath, err := ResolveGCPath(c.Env)
	if err != nil {
		c.t.Fatal(err)
	}

	logPath := filepath.Join(c.Dir, ".gc", "acceptance-controller.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		c.t.Fatalf("creating foreground controller log: %v", err)
	}

	cmd := exec.Command(gcPath, "start", "--foreground", c.Dir)
	cmd.Dir = c.Dir
	cmd.Env = c.Env.List()
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		c.t.Fatalf("starting gc --foreground: %v", err)
	}

	c.cmd = cmd
	c.logFile = logFile
	c.started = true
	c.t.Cleanup(func() { c.Stop() })
}

// WriteReportScript writes a shell script to the city that dumps
// environment variables to a report file, then optionally drains.
// Returns the start_command string to use in agent config.
func (c *City) WriteReportScript(name string, drain bool) string {
	c.t.Helper()
	scriptsDir := filepath.Join(c.Dir, ".gc", "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		c.t.Fatal(err)
	}

	reportDir := filepath.Join(c.Dir, ".gc", "reports")
	if err := os.MkdirAll(reportDir, 0o755); err != nil {
		c.t.Fatal(err)
	}

	reportFile := filepath.Join(reportDir, name+".env")
	var drainLine string
	if drain {
		drainLine = "\n# Find gc in PATH\ngc runtime drain-ack 2>/dev/null || true"
	}

	script := fmt.Sprintf(`#!/bin/sh
# Acceptance test report script for agent %q.
# Dumps GC_* and other relevant env vars to a report file.
# No set -e: grep returns 1 on no match, which would abort before
# writing REPORT_DONE and cause a 60s timeout instead of a clear failure.

REPORT=%q

env | grep -E '^(GC_|GT_|BEADS_)' | sort > "$REPORT" || true
echo "CWD=$(pwd)" >> "$REPORT"
echo "REPORT_DONE=true" >> "$REPORT"
%s

# Brief sleep so the reconciler can observe the running state.
sleep 2
exit 0
`, name, reportFile, drainLine)

	scriptPath := filepath.Join(scriptsDir, "report-"+name+".sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		c.t.Fatal(err)
	}

	return "bash " + scriptPath
}

// WaitForReport polls until the agent's report file contains
// REPORT_DONE=true, or times out.
func (c *City) WaitForReport(name string, timeout time.Duration) map[string]string {
	c.t.Helper()
	reportFile := filepath.Join(c.Dir, ".gc", "reports", name+".env")
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		data, err := os.ReadFile(reportFile)
		if err == nil && strings.Contains(string(data), "REPORT_DONE=true") {
			return parseEnvReport(string(data))
		}
		time.Sleep(200 * time.Millisecond)
	}

	// One more try with diagnostics.
	data, err := os.ReadFile(reportFile)
	if err != nil {
		c.t.Fatalf("report for %q not found after %s: %v", name, timeout, err)
	}
	if !strings.Contains(string(data), "REPORT_DONE=true") {
		c.t.Fatalf("report for %q incomplete after %s:\n%s", name, timeout, string(data))
	}
	return parseEnvReport(string(data))
}

func parseEnvReport(s string) map[string]string {
	m := make(map[string]string)
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if k, v, ok := strings.Cut(line, "="); ok {
			m[k] = v
		}
	}
	return m
}

// WriteE2EConfig writes a full city.toml from structured config.
// Includes [beads] provider = "file" for test isolation.
func (c *City) WriteE2EConfig(agents []E2EAgent) {
	c.t.Helper()
	cityName := filepath.Base(c.Dir)

	var b strings.Builder
	fmt.Fprintf(&b, "[workspace]\nname = %q\n", cityName)
	b.WriteString("\n[beads]\nprovider = \"file\"\n")

	for _, a := range agents {
		fmt.Fprintf(&b, "\n[[agent]]\nname = %q\n", a.Name)
		if a.StartCommand != "" {
			fmt.Fprintf(&b, "start_command = %q\n", a.StartCommand)
		}
		if a.Dir != "" {
			fmt.Fprintf(&b, "dir = %q\n", a.Dir)
		}
		if a.WorkDir != "" {
			fmt.Fprintf(&b, "work_dir = %q\n", a.WorkDir)
		}
		if a.WorkQuery != "" {
			fmt.Fprintf(&b, "work_query = %q\n", a.WorkQuery)
		}
		if a.Suspended {
			b.WriteString("suspended = true\n")
		}
		if a.Pool == nil {
			fmt.Fprintf(&b, "max_active_sessions = 1\n")
		}
		if a.Pool != nil {
			fmt.Fprintf(&b, "\n[agent.pool]\nmin = %d\nmax = %d\n", a.Pool.Min, a.Pool.Max)
			if a.Pool.ScaleCheck != "" {
				fmt.Fprintf(&b, "check = %q\n", a.Pool.ScaleCheck)
			}
		}
	}

	c.WriteConfig(b.String())
}

// E2EAgent describes an agent for lifecycle tests.
type E2EAgent struct {
	Name         string
	StartCommand string
	Dir          string
	WorkDir      string
	WorkQuery    string
	Suspended    bool
	Pool         *PoolConfig
}

// WaitForCondition polls fn until it returns true or timeout expires.
func (c *City) WaitForCondition(fn func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// ReportTimeout returns the default timeout for waiting on agent reports.
func ReportTimeout() time.Duration {
	return 60 * time.Second
}
