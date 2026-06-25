package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeBackupStateForFreshness(t *testing.T, scopeRoot, timestamp string) {
	t.Helper()
	dir := filepath.Join(scopeRoot, ".beads", "backup")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir backup dir: %v", err)
	}
	body := `{"last_dolt_commit":"abc123","timestamp":"` + timestamp + `"}`
	if err := os.WriteFile(filepath.Join(dir, "backup_state.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write backup_state.json: %v", err)
	}
}

func TestBdBackupFreshnessCheck(t *testing.T) {
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	maxAge := 24 * time.Hour

	t.Run("fresh sync is OK", func(t *testing.T) {
		scope := t.TempDir()
		writeBackupStateForFreshness(t, scope, now.Add(-1*time.Hour).Format(time.RFC3339Nano))
		r := NewBdBackupFreshnessCheckForScopeRoots("", []string{scope}, maxAge, clock).Run(nil)
		if r.Status != StatusOK {
			t.Fatalf("fresh backup: want StatusOK, got %v (%s)", r.Status, r.Message)
		}
	})

	t.Run("stale sync warns and reports the age", func(t *testing.T) {
		scope := t.TempDir()
		writeBackupStateForFreshness(t, scope, now.Add(-72*time.Hour).Format(time.RFC3339Nano))
		r := NewBdBackupFreshnessCheckForScopeRoots("", []string{scope}, maxAge, clock).Run(nil)
		if r.Status != StatusWarning {
			t.Fatalf("stale backup: want StatusWarning, got %v (%s)", r.Status, r.Message)
		}
		if !strings.Contains(r.Message, "ago") {
			t.Fatalf("stale message should describe the age, got %q", r.Message)
		}
		if r.FixHint == "" {
			t.Fatalf("stale finding should carry a FixHint")
		}
	})

	t.Run("missing backup_state.json is skipped (OK, not this check's job)", func(t *testing.T) {
		scope := t.TempDir() // no .beads/backup at all
		r := NewBdBackupFreshnessCheckForScopeRoots("", []string{scope}, maxAge, clock).Run(nil)
		if r.Status != StatusOK {
			t.Fatalf("no backup: want StatusOK, got %v (%s)", r.Status, r.Message)
		}
	})

	t.Run("missing timestamp warns", func(t *testing.T) {
		scope := t.TempDir()
		dir := filepath.Join(scope, ".beads", "backup")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "backup_state.json"), []byte(`{"last_dolt_commit":"x"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		r := NewBdBackupFreshnessCheckForScopeRoots("", []string{scope}, maxAge, clock).Run(nil)
		if r.Status != StatusWarning {
			t.Fatalf("missing timestamp: want StatusWarning, got %v (%s)", r.Status, r.Message)
		}
	})

	t.Run("unparseable timestamp warns", func(t *testing.T) {
		scope := t.TempDir()
		writeBackupStateForFreshness(t, scope, "not-a-timestamp")
		r := NewBdBackupFreshnessCheckForScopeRoots("", []string{scope}, maxAge, clock).Run(nil)
		if r.Status != StatusWarning {
			t.Fatalf("bad timestamp: want StatusWarning, got %v (%s)", r.Status, r.Message)
		}
	})

	t.Run("one stale scope among fresh ones still warns", func(t *testing.T) {
		fresh := t.TempDir()
		stale := t.TempDir()
		writeBackupStateForFreshness(t, fresh, now.Add(-2*time.Hour).Format(time.RFC3339Nano))
		writeBackupStateForFreshness(t, stale, now.Add(-100*time.Hour).Format(time.RFC3339Nano))
		r := NewBdBackupFreshnessCheckForScopeRoots("", []string{fresh, stale}, maxAge, clock).Run(nil)
		if r.Status != StatusWarning {
			t.Fatalf("mixed: want StatusWarning, got %v (%s)", r.Status, r.Message)
		}
	})

	t.Run("Name and CanFix are stable", func(t *testing.T) {
		c := NewBdBackupFreshnessCheckForScopeRoots("", nil, maxAge, clock)
		if c.Name() != "bd-backup-freshness" {
			t.Fatalf("unexpected name %q", c.Name())
		}
		if c.CanFix() {
			t.Fatalf("CanFix should be false (report-only)")
		}
	})
}
