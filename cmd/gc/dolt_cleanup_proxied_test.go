package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// The orphan reap stage is host-wide: it enumerates every `dolt sql-server` on
// the box and never touches the city's own Dolt. Skipping the whole command on
// a proxied city therefore retired host-wide reaping for every fresh city,
// because mol-dog-stale-db's only front door is `gc dolt-cleanup --probe` and
// it would have read a clean run with zero targets forever. bd's own processes
// are already protected by the proxy.pid ownership rule, so the stage is safe
// to run here.
func TestProxiedScopeCleanupStillReapsHostOrphans(t *testing.T) {
	leaked := filepath.Join(t.TempDir(), "TestSomething123", "dolt", "config.yaml")
	opts := cleanupOptions{
		JSON:    true,
		HomeDir: "/home/u",
		TempDir: filepath.Dir(filepath.Dir(filepath.Dir(leaked))),
		DiscoverProcesses: func() ([]DoltProcInfo, error) {
			return []DoltProcInfo{{PID: 9001, Argv: []string{"dolt", "sql-server", "--config", leaked}}}, nil
		},
		ActiveTestRoots: []string{},
	}
	var stdout, stderr bytes.Buffer
	if code := runProxiedScopeDoltCleanup(opts, &stdout, &stderr); code != 0 {
		t.Fatalf("runProxiedScopeDoltCleanup exit = %d, stderr = %s", code, stderr.String())
	}

	var report CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("parse envelope: %v\n%s", err, stdout.String())
	}
	if report.Skipped == nil || report.Skipped.Reason != cleanupSkipReasonProxiedScope {
		t.Fatalf("envelope does not report the bd-owned no-op: %s", stdout.String())
	}
	if len(report.Reaped.Targets) == 0 {
		t.Fatalf("proxied city reaped no host orphan:\n%s", stdout.String())
	}
	if !strings.Contains(report.Skipped.Message, "reap") {
		t.Errorf("skip message does not say the reap stage still ran: %q", report.Skipped.Message)
	}
}

// A bd-owned proxy under the same test-path allowlist is still protected: the
// reap stage running here must not kill the very processes this feature owns.
func TestProxiedScopeCleanupDoesNotReapBdOwnedProxy(t *testing.T) {
	tempDir := t.TempDir()
	scope := filepath.Join(tempDir, "TestSomething123", "city")
	configPath := writeBdProxyRoot(t, scope, 4242)
	stubLiveBdProxy(t, 4242, configPath)
	opts := cleanupOptions{
		JSON:    true,
		HomeDir: "/home/u",
		TempDir: tempDir,
		DiscoverProcesses: func() ([]DoltProcInfo, error) {
			return []DoltProcInfo{{PID: 9001, Argv: []string{"dolt", "sql-server", "--config", configPath}}}, nil
		},
		ActiveTestRoots: []string{},
	}
	var stdout, stderr bytes.Buffer
	if code := runProxiedScopeDoltCleanup(opts, &stdout, &stderr); code != 0 {
		t.Fatalf("runProxiedScopeDoltCleanup exit = %d, stderr = %s", code, stderr.String())
	}
	var report CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("parse envelope: %v\n%s", err, stdout.String())
	}
	if len(report.Reaped.Targets) != 0 {
		t.Fatalf("reaped a bd-owned proxy's dolt server:\n%s", stdout.String())
	}
	if len(report.Reaped.Protected) == 0 {
		t.Fatalf("bd-owned proxy was not reported as protected:\n%s", stdout.String())
	}
}
