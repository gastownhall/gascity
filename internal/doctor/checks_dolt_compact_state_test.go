package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const compactStateOldCreatedAt = "2000-01-02T03:04:05Z"

func newDoltCompactStateTestCity(t *testing.T) string {
	t.Helper()
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_RUNTIME_DIR", "")
	return setupManagedDoltCity(t)
}

func newTestDoltCompactStateCheck(cityPath string) *DoltCompactStateCheck {
	return NewDoltCompactStateCheck(cityPath, false)
}

func writeDoltCompactStateMarker(t *testing.T, cityPath, markerType, db, reason, createdAt string) string {
	t.Helper()
	markerPath := filepath.Join(doctorDoltPackStateDir(cityPath), markerType, db)
	if err := os.MkdirAll(filepath.Dir(markerPath), 0o700); err != nil {
		t.Fatal(err)
	}
	content := fmt.Sprintf("db=%s\nreason=%s\ncreated_at=%s\n", db, reason, createdAt)
	if err := os.WriteFile(markerPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return markerPath
}

func doltCompactStateResultText(r *CheckResult) string {
	parts := []string{r.Message, r.FixHint}
	parts = append(parts, r.Details...)
	return strings.Join(parts, "\n")
}

func assertDoltCompactStateMentions(t *testing.T, r *CheckResult, parts ...string) {
	t.Helper()
	text := doltCompactStateResultText(r)
	for _, part := range parts {
		if !strings.Contains(text, part) {
			t.Fatalf("result text missing %q:\n%s", part, text)
		}
	}
}

func TestDoltCompactStateCheckCleanStateOK(t *testing.T) {
	dir := newDoltCompactStateTestCity(t)
	if err := os.MkdirAll(doctorDoltPackStateDir(dir), 0o700); err != nil {
		t.Fatal(err)
	}

	r := newTestDoltCompactStateCheck(dir).Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK for clean compact state; msg = %s", r.Status, r.Message)
	}
	if r.FixHint != "" {
		t.Fatalf("FixHint = %q, want empty for clean compact state", r.FixHint)
	}
}

func TestDoltCompactStateCheckReportsStaleMarkersWithFixHints(t *testing.T) {
	tests := []struct {
		markerType string
		db         string
		reason     string
	}{
		{
			markerType: "compact-quarantine",
			db:         "hq",
			reason:     "post-flatten row count decreased",
		},
		{
			markerType: "compact-pending-gc",
			db:         "analytics",
			reason:     "flatten succeeded but full GC failed",
		},
		{
			markerType: "compact-pending-push",
			db:         "search",
			reason:     "remote push failed after full GC",
		},
	}

	for _, tc := range tests {
		t.Run(tc.markerType, func(t *testing.T) {
			dir := newDoltCompactStateTestCity(t)
			markerPath := writeDoltCompactStateMarker(t, dir, tc.markerType, tc.db, tc.reason, compactStateOldCreatedAt)

			r := newTestDoltCompactStateCheck(dir).Run(&CheckContext{CityPath: dir})
			if r.Status == StatusOK {
				t.Fatalf("status = OK, want warning or error for stale %s marker", tc.markerType)
			}
			if r.FixHint == "" {
				t.Fatalf("FixHint is empty for stale %s marker", tc.markerType)
			}
			assertDoltCompactStateMentions(t, r,
				tc.db,
				tc.markerType,
				markerPath,
				tc.reason,
				compactStateOldCreatedAt,
			)
		})
	}
}

func TestDoltCompactStateCheckSurfacesUnreadableMarker(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can read any file")
	}

	dir := newDoltCompactStateTestCity(t)
	markerPath := writeDoltCompactStateMarker(t, dir, "compact-quarantine", "unreadable-db", "post-flatten row count decreased", compactStateOldCreatedAt)
	if err := os.Chmod(markerPath, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(markerPath, 0o600) })

	r := newTestDoltCompactStateCheck(dir).Run(&CheckContext{CityPath: dir})
	if r.Status == StatusOK {
		t.Fatalf("status = OK, want warning for unreadable marker; msg = %s", r.Message)
	}
	assertDoltCompactStateMentions(t, r, markerPath)
}

func TestDoltCompactStateCheckRepresentsKnownPendingPushMarkers(t *testing.T) {
	dir := newDoltCompactStateTestCity(t)
	markers := map[string]string{
		"gascity":   writeDoltCompactStateMarker(t, dir, "compact-pending-push", "gascity", "remote push failed after full GC", "2026-05-23T00:00:00Z"),
		"mcdclient": writeDoltCompactStateMarker(t, dir, "compact-pending-push", "mcdclient", "remote push failed after full GC", "2026-05-28T00:00:00Z"),
	}

	r := newTestDoltCompactStateCheck(dir).Run(&CheckContext{CityPath: dir})
	if r.Status == StatusOK {
		t.Fatalf("status = OK, want stale pending-push markers to report warning or error")
	}
	if r.FixHint == "" {
		t.Fatal("FixHint is empty for stale pending-push markers")
	}
	for db, markerPath := range markers {
		assertDoltCompactStateMentions(t, r, db, "compact-pending-push", markerPath)
	}
	assertDoltCompactStateMentions(t, r, "2026-05-23T00:00:00Z", "2026-05-28T00:00:00Z")
}
