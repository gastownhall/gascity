package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The testdata/bd_backup_status fixtures are real `bd backup status --json`
// output from a `bd init --proxied-server` scope in an isolated HOME:
//
//   - bd-1.3.0-proxied-refused.json: bd v1.3.0 (exit 1, on stdout).
//   - bd-1.3.1-proxied-unconfigured.json: beads hotfix/1.3.1 (1.3.1-rc.1,
//     f5a940279) before any destination is registered.
//   - bd-1.3.1-proxied-synced.json: the same bd after `bd backup init` and
//     `bd backup sync` (backup_url rewritten to file:///backups/px).
func bdBackupStatusFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "bd_backup_status", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// bdRefusesProxiedBackup is the status reader for a bd v1.3.0 scope.
func bdRefusesProxiedBackup(context.Context, *CheckContext, string, string) (bdBackupStatusReport, error) {
	return bdBackupStatusReport{}, errBdProxiedBackupRefused
}

// proxiesRunning is the liveness probe for a city whose proxies are all up.
func proxiesRunning(string) bool { return true }

func writeProxiedLocalScope(t *testing.T, scopeRoot, db string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(scopeRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scopeRoot, ".beads", "metadata.json"),
		[]byte(fmt.Sprintf(`{"backend":"dolt","database":"dolt","dolt_mode":"proxied-server","dolt_database":%q}`, db)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// proxiedCoverageCity builds a city root plus rigs/r1, both proxied-local, and
// returns the check with statuses answered per scope label.
func proxiedCoverageCity(t *testing.T, now time.Time, statuses map[string]func() (bdBackupStatusReport, error)) *ProxiedBackupCoverageCheck {
	t.Helper()
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "r1")
	writeProxiedLocalScope(t, city, "hq")
	writeProxiedLocalScope(t, rig, "r1")
	check := NewProxiedBackupCoverageCheckForConfig(city, nil, errors.New("no city.toml"), nil)
	if check == nil {
		t.Fatal("no check registered for a city with two proxied scopes")
	}
	check.now = func() time.Time { return now }
	check.proxyLive = proxiesRunning
	check.status = func(_ context.Context, _ *CheckContext, _ string, scopeRoot string) (bdBackupStatusReport, error) {
		answer, ok := statuses[proxiedScopeLabel(city, scopeRoot)]
		if !ok {
			t.Fatalf("unexpected status call for %s", scopeRoot)
		}
		return answer()
	}
	return check
}

func decodedStatus(t *testing.T, fixture string, mutate func(*bdBackupStatusReport)) func() (bdBackupStatusReport, error) {
	t.Helper()
	var report bdBackupStatusReport
	if err := json.Unmarshal(bdBackupStatusFixture(t, fixture), &report); err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(&report)
	}
	return func() (bdBackupStatusReport, error) { return report, nil }
}

func TestBdProxiedBackupRefusedMatchesBd130Output(t *testing.T) {
	if !bdProxiedBackupRefused(bdBackupStatusFixture(t, "bd-1.3.0-proxied-refused.json")) {
		t.Fatal("bd v1.3.0's proxied refusal was not recognized")
	}
	for _, name := range []string{"bd-1.3.1-proxied-unconfigured.json", "bd-1.3.1-proxied-synced.json"} {
		if bdProxiedBackupRefused(bdBackupStatusFixture(t, name)) {
			t.Errorf("%s read as a refusal", name)
		}
	}
}

func TestProxiedBackupCoverageReportsARecentBdBackup(t *testing.T) {
	synced := decodedStatus(t, "bd-1.3.1-proxied-synced.json", nil)
	now := time.Date(2026, 9, 29, 10, 0, 1, 0, time.UTC) // six hours after the fixture's last_sync
	check := proxiedCoverageCity(t, now, map[string]func() (bdBackupStatusReport, error){
		"city": synced, filepath.Join("rigs", "r1"): synced,
	})

	result := check.Run(&CheckContext{})
	if result.Status != StatusOK {
		t.Fatalf("status = %v (%q), want OK", result.Status, result.Message)
	}
	for _, want := range []string{"2 bd-owned proxied scopes", "city", filepath.Join("rigs", "r1"), "backed up through bd"} {
		if !strings.Contains(result.Message, want) {
			t.Errorf("message %q does not mention %q", result.Message, want)
		}
	}
	for _, stale := range []string{"refuses", "1.3.0", "only copy"} {
		if strings.Contains(result.Message, stale) {
			t.Errorf("message keeps the refusal text %q on a bd that backs up: %q", stale, result.Message)
		}
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "file:///backups/px") {
		t.Errorf("details do not name the backup destination: %q", result.Details)
	}
}

func TestProxiedBackupCoverageWarnsOnMissingOrStaleBdBackup(t *testing.T) {
	now := time.Date(2026, 9, 29, 10, 0, 1, 0, time.UTC)
	tests := []struct {
		name     string
		rig      func() (bdBackupStatusReport, error)
		wantText string
	}{
		{
			name:     "no destination",
			rig:      decodedStatus(t, "bd-1.3.1-proxied-unconfigured.json", nil),
			wantText: "no bd backup destination is configured",
		},
		{
			name: "stale sync",
			rig: decodedStatus(t, "bd-1.3.1-proxied-synced.json", func(r *bdBackupStatusReport) {
				r.Dolt.LastSync = "2026-09-20T04:00:01Z"
			}),
			wantText: "last sync was",
		},
		{
			name: "never synced",
			rig: decodedStatus(t, "bd-1.3.1-proxied-synced.json", func(r *bdBackupStatusReport) {
				r.Dolt.LastSync = ""
			}),
			wantText: "has no dolt.last_sync",
		},
		{
			name:     "status failed",
			rig:      func() (bdBackupStatusReport, error) { return bdBackupStatusReport{}, errors.New("exit status 1: boom") },
			wantText: "could not read bd backup status: exit status 1: boom",
		},
		{
			// Mixed bd pins: one scope's bd supports proxied backup, the
			// other refuses. The refusal is a gap too and must be named.
			name:     "refused beside a working scope",
			rig:      func() (bdBackupStatusReport, error) { return bdRefusesProxiedBackup(context.Background(), nil, "", "") },
			wantText: proxiedBackupRefusal,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			check := proxiedCoverageCity(t, now, map[string]func() (bdBackupStatusReport, error){
				"city":                      decodedStatus(t, "bd-1.3.1-proxied-synced.json", nil),
				filepath.Join("rigs", "r1"): tc.rig,
			})
			result := check.Run(&CheckContext{})
			if result.Status != StatusWarning || result.Severity != SeverityAdvisory {
				t.Fatalf("status/severity = %v/%v (%q), want warning/advisory", result.Status, result.Severity, result.Message)
			}
			if !strings.Contains(result.Message, filepath.Join("rigs", "r1")) || !strings.Contains(result.Message, tc.wantText) {
				t.Errorf("message %q does not name rigs/r1 with %q", result.Message, tc.wantText)
			}
			if strings.Contains(result.Message, "city:") {
				t.Errorf("message flags the backed-up city scope: %q", result.Message)
			}
			if !strings.Contains(result.FixHint, "bd backup") {
				t.Errorf("fix hint does not say how to back the scope up: %q", result.FixHint)
			}
		})
	}
}

// The status reader runs the pinned bd in the scope and turns bd v1.3.0's
// refusal (JSON on stdout, exit 1) into errBdProxiedBackupRefused.
func TestReadBdBackupStatusThroughThePinnedBd(t *testing.T) {
	dir := guardedTempDir(t)
	writeProxiedLocalScope(t, dir, "hq")
	t.Setenv("PATH", filepath.Join(dir, "empty-path"))

	stub := func(name string, fixture string, exit int) string {
		// PATH is empty, so the stub prints with the shell's own printf.
		path := filepath.Join(dir, name)
		script := fmt.Sprintf("#!/bin/sh\n[ \"$*\" = 'backup status --json' ] || exit 9\nprintf '%%s' '%s'\nexit %d\n",
			bdBackupStatusFixture(t, fixture), exit)
		if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}

	ctx := &CheckContext{CityPath: dir}
	if _, err := readBdBackupStatus(context.Background(), ctx, stub("bd-130", "bd-1.3.0-proxied-refused.json", 1), dir); !errors.Is(err, errBdProxiedBackupRefused) {
		t.Fatalf("bd v1.3.0 refusal: err = %v, want errBdProxiedBackupRefused", err)
	}
	report, err := readBdBackupStatus(context.Background(), ctx, stub("bd-131", "bd-1.3.1-proxied-synced.json", 0), dir)
	if err != nil {
		t.Fatalf("bd 1.3.1 status: %v", err)
	}
	if !report.Dolt.Configured || report.Dolt.LastSync != "2026-09-29T04:00:01Z" || report.Dolt.BackupURL != "file:///backups/px" {
		t.Fatalf("decoded report = %+v", report.Dolt)
	}
}

// bd 1.3.1's `backup status` starts a stopped scope's proxy and Dolt, so a
// scope whose proxy is not running is never handed to bd.
func TestProxiedBackupCoverageDoesNotAskBdAboutAStoppedScope(t *testing.T) {
	now := time.Date(2026, 9, 29, 10, 0, 1, 0, time.UTC)
	synced := decodedStatus(t, "bd-1.3.1-proxied-synced.json", nil)
	check := proxiedCoverageCity(t, now, map[string]func() (bdBackupStatusReport, error){
		"city": synced,
		filepath.Join("rigs", "r1"): func() (bdBackupStatusReport, error) {
			t.Error("bd was asked about a scope whose proxy is not running")
			return synced()
		},
	})
	check.proxyLive = func(scopeRoot string) bool { return filepath.Base(scopeRoot) != "r1" }

	result := check.Run(&CheckContext{})
	if result.Status != StatusOK {
		t.Fatalf("status = %v (%q), want OK", result.Status, result.Message)
	}
	for _, want := range []string{"1 bd-owned proxied scope (city) backed up", "not checked: store not running (" + filepath.Join("rigs", "r1") + ")"} {
		if !strings.Contains(result.Message, want) {
			t.Errorf("message %q does not contain %q", result.Message, want)
		}
	}

	check.proxyLive = func(string) bool { return false }
	check.status = func(context.Context, *CheckContext, string, string) (bdBackupStatusReport, error) {
		t.Error("bd was asked about a stopped scope")
		return synced()
	}
	result = check.Run(&CheckContext{})
	if result.Status != StatusOK || !strings.Contains(result.Message, "not checked: store not running (city, "+filepath.Join("rigs", "r1")+")") {
		t.Fatalf("all-stopped result = %v %q", result.Status, result.Message)
	}
}

// One deadline covers the whole check, so scopes that hang cannot outlast
// doctor's per-check timeout and lose the answers already in hand.
func TestProxiedBackupCoverageBoundsAllScopesByOneDeadline(t *testing.T) {
	city := t.TempDir()
	statuses := map[string]func() (bdBackupStatusReport, error){}
	for i := 0; i < 2*proxiedBackupCoverageParallelism+1; i++ {
		name := fmt.Sprintf("r%d", i)
		writeProxiedLocalScope(t, filepath.Join(city, "rigs", name), name)
		statuses[filepath.Join("rigs", name)] = nil
	}
	check := NewProxiedBackupCoverageCheckForConfig(city, nil, errors.New("no city.toml"), nil)
	check.proxyLive = proxiesRunning
	check.deadline = 50 * time.Millisecond
	check.status = func(ctx context.Context, _ *CheckContext, _ string, _ string) (bdBackupStatusReport, error) {
		<-ctx.Done()
		return bdBackupStatusReport{}, fmt.Errorf("check deadline reached: %w", ctx.Err())
	}

	start := time.Now()
	result := check.Run(&CheckContext{})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Run took %s with a 50ms deadline", elapsed)
	}
	if result.Status != StatusWarning || strings.Count(result.Message, "could not read bd backup status") != len(statuses) {
		t.Fatalf("result = %v %q, want one deadline finding per scope", result.Status, result.Message)
	}
}
