package doctor

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeRigExport writes records as one JSON object per line into the
// accepted-export location judgeRigStoreBinding reads, and returns the path.
func writeRigExport(t *testing.T, cityDir, rig string, lines []string) string {
	t.Helper()
	dir := filepath.Join(cityDir, ".beads-jsonl-export", rig)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, rig+"-2026-10-05.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func issueLine(id, created string) string {
	return fmt.Sprintf(`{"id":%q,"created_at":%q}`, id, created)
}

func wispLine(id, created string) string {
	return fmt.Sprintf(`{"id":%q,"created_at":%q,"wisp_plane":true}`, id, created)
}

func nullTime(ts string, t *testing.T) sql.NullTime {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatal(err)
	}
	return sql.NullTime{Time: parsed, Valid: true}
}

// TestRigStoreBindingBaselineIsDurableOnly pins architect rule R1: an export
// carries wisp_plane rows that the live issues table does not, so a healthy
// store holding every durable row must not be judged against the raw line
// count. Shape measured on hold-court: 168 durable rows among 644 lines.
func TestRigStoreBindingBaselineIsDurableOnly(t *testing.T) {
	cityDir := t.TempDir()
	var lines []string
	for i := 0; i < 168; i++ {
		lines = append(lines, issueLine(fmt.Sprintf("d-%d", i), "2026-10-01T00:00:00Z"))
	}
	for i := 0; i < 476; i++ {
		// The newest row in the export is a wisp, after every durable row.
		lines = append(lines, wispLine(fmt.Sprintf("w-%d", i), "2026-10-04T00:00:00Z"))
	}
	path := writeRigExport(t, cityDir, "hold-court", lines)

	r := judgeRigStoreBinding(&CheckResult{Name: "rig:hold-court:store-binding"}, "hold-court", "hc", "127.0.0.1:1", path, 168, nullTime("2026-10-01T00:00:00Z", t))
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK for a store holding all 168 durable rows; msg = %s", r.Status, r.Message)
	}
}

// TestRigStoreBindingFloorMatchesTheExportPack pins rule R3: the floor is
// durable*80/100 with integer division, the arithmetic the export pack uses,
// so doctor errors exactly when the next export would be quarantined.
func TestRigStoreBindingFloorMatchesTheExportPack(t *testing.T) {
	cases := []struct {
		name    string
		durable int
		live    int
		want    int
	}{
		{"export 5 live 4 is at the floor", 5, 4, int(StatusOK)},
		{"export 5 live 3 is below the floor", 5, 3, int(StatusError)},
		{"export 3 live 2 is accepted by the pack", 3, 2, int(StatusOK)},
		{"export 3 live 1 is below the floor", 3, 1, int(StatusError)},
		{"3330 collapsed to 9", 3330, 9, int(StatusError)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityDir := t.TempDir()
			var lines []string
			for i := 0; i < tc.durable; i++ {
				lines = append(lines, issueLine(fmt.Sprintf("d-%d", i), "2026-10-01T00:00:00Z"))
			}
			path := writeRigExport(t, cityDir, "demo", lines)
			r := judgeRigStoreBinding(&CheckResult{Name: "rig:demo:store-binding"}, "demo", "demo", "127.0.0.1:1", path, tc.live, nullTime("2026-10-02T00:00:00Z", t))
			if int(r.Status) != tc.want {
				t.Fatalf("status = %d, want %d; msg = %s", r.Status, tc.want, r.Message)
			}
		})
	}
}

// TestRigStoreBindingFreshnessIsStrict pins rule R2: a dormant rig keeps the
// same newest created_at as its export forever, which is healthy; only a
// live store strictly older than the export looks rolled back or replaced.
func TestRigStoreBindingFreshnessIsStrict(t *testing.T) {
	cityDir := t.TempDir()
	path := writeRigExport(t, cityDir, "demo", []string{
		issueLine("d-1", "2026-10-01T00:00:00Z"),
		// Sub-second precision in the export against a second-precision DB.
		issueLine("d-2", "2026-10-03T12:00:00.789Z"),
	})
	cases := []struct {
		name string
		live string
		want int
	}{
		{"equal newest is healthy", "2026-10-03T12:00:00Z", int(StatusOK)},
		{"live newer is healthy", "2026-10-04T00:00:00Z", int(StatusOK)},
		{"live older warns", "2026-10-03T11:59:59Z", int(StatusWarning)},
		{"same instant in another zone is healthy", "2026-10-03T14:00:00+02:00", int(StatusOK)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := judgeRigStoreBinding(&CheckResult{Name: "rig:demo:store-binding"}, "demo", "demo", "127.0.0.1:1", path, 2, nullTime(tc.live, t))
			if int(r.Status) != tc.want {
				t.Fatalf("status = %d, want %d; msg = %s", r.Status, tc.want, r.Message)
			}
		})
	}
}

// TestRigStoreBindingLongExportLineDoesNotFailTheRead pins the streaming half
// of rule R4: the gascity export is 174 MB and a single record can exceed the
// old bufio.Scanner cap of 10 MB.
func TestRigStoreBindingLongExportLineDoesNotFailTheRead(t *testing.T) {
	cityDir := t.TempDir()
	long := fmt.Sprintf(`{"id":"d-1","created_at":"2026-10-01T00:00:00Z","description":%q}`, strings.Repeat("x", 11*1024*1024))
	path := writeRigExport(t, cityDir, "demo", []string{long, issueLine("d-2", "2026-10-01T00:00:00Z")})

	r := judgeRigStoreBinding(&CheckResult{Name: "rig:demo:store-binding"}, "demo", "demo", "127.0.0.1:1", path, 2, nullTime("2026-10-01T00:00:00Z", t))
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

// TestRigStoreBindingUnreadableBaselineWarns pins rule R4: damage to a backup
// file is not evidence about the live store, so it never ERRORs.
func TestRigStoreBindingUnreadableBaselineWarns(t *testing.T) {
	cityDir := t.TempDir()
	path := writeRigExport(t, cityDir, "demo", []string{issueLine("d-1", "2026-10-01T00:00:00Z"), `{"id": truncated`})

	r := judgeRigStoreBinding(&CheckResult{Name: "rig:demo:store-binding"}, "demo", "demo", "127.0.0.1:1", path, 2, nullTime("2026-10-01T00:00:00Z", t))
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "export baseline") {
		t.Errorf("message = %q, want it to name the unreadable baseline", r.Message)
	}
}
