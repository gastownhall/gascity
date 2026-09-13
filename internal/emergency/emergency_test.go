package emergency

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewRecordDefaultsTrimsAndBuildsStableID(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 34, 56, 0, time.FixedZone("test", -7*60*60))
	metadata := map[string]string{"ticket": "ga-guopsu"}

	rec, err := NewRecord(RecordOptions{
		Message:  "  controller cannot reach spool  ",
		Actor:    "   ",
		RefBead:  " ga-guopsu ",
		Metadata: metadata,
		Now:      func() time.Time { return now },
		Random:   bytes.NewReader([]byte{0xde, 0xad, 0xbe, 0xef}),
	})
	if err != nil {
		t.Fatalf("NewRecord() error = %v", err)
	}

	if rec.ID != "20260610T193456Z-deadbeef" {
		t.Fatalf("ID = %q, want UTC timestamp plus random suffix", rec.ID)
	}
	if !ValidRecordID(rec.ID) {
		t.Fatalf("ID %q does not pass ValidRecordID", rec.ID)
	}
	if rec.Severity != SeverityError {
		t.Errorf("Severity = %q, want default %q", rec.Severity, SeverityError)
	}
	if rec.Actor != "human" {
		t.Errorf("Actor = %q, want default human", rec.Actor)
	}
	if rec.Message != "controller cannot reach spool" {
		t.Errorf("Message = %q, want trimmed body", rec.Message)
	}
	if rec.RefBead != "ga-guopsu" {
		t.Errorf("RefBead = %q, want trimmed bead id", rec.RefBead)
	}
	if !rec.CreatedAt.Equal(now.UTC()) {
		t.Errorf("CreatedAt = %s, want %s", rec.CreatedAt, now.UTC())
	}

	metadata["ticket"] = "mutated"
	if rec.Metadata["ticket"] != "ga-guopsu" {
		t.Errorf("Metadata was not cloned: got %q", rec.Metadata["ticket"])
	}
}

func TestNewRecordValidatesInputs(t *testing.T) {
	tests := []struct {
		name    string
		opts    RecordOptions
		wantErr string
	}{
		{
			name:    "invalid severity",
			opts:    RecordOptions{Severity: "panic", Message: "message"},
			wantErr: "severity",
		},
		{
			name:    "empty message",
			opts:    RecordOptions{Severity: SeverityWarn, Message: " \t\n "},
			wantErr: "message is required",
		},
		{
			name:    "message cap",
			opts:    RecordOptions{Severity: SeverityInfo, Message: strings.Repeat("x", MaxMessageBytes+1)},
			wantErr: "4 KiB",
		},
		{
			name:    "short random source",
			opts:    RecordOptions{Severity: SeverityInfo, Message: "message", Random: bytes.NewReader([]byte{0x01})},
			wantErr: "generating emergency id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewRecord(tt.opts)
			if err == nil {
				t.Fatalf("NewRecord() error = nil, want %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("NewRecord() error = %q, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestWriteSpoolAtomicallyCreatesPrivateRecordAndRejectsDuplicateID(t *testing.T) {
	cityPath := t.TempDir()
	rec := Record{
		ID:        "20260610T193456Z-deadbeef",
		Severity:  SeverityCritical,
		Actor:     "controller",
		Message:   "dolt unavailable",
		CreatedAt: time.Date(2026, 6, 10, 19, 34, 56, 0, time.UTC),
	}

	path, err := WriteSpool(cityPath, rec)
	if err != nil {
		t.Fatalf("WriteSpool() error = %v", err)
	}
	wantPath := filepath.Join(cityPath, ".gc", "emergency", rec.ID+".json")
	if path != wantPath {
		t.Fatalf("WriteSpool() path = %q, want %q", path, wantPath)
	}

	spoolInfo, err := os.Stat(SpoolDir(cityPath))
	if err != nil {
		t.Fatalf("stat spool dir: %v", err)
	}
	if got, want := spoolInfo.Mode().Perm(), os.FileMode(0o700); got != want {
		t.Fatalf("spool dir mode = %v, want %v", got, want)
	}

	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat spool record: %v", err)
	}
	if got, want := fileInfo.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Fatalf("spool file mode = %v, want %v", got, want)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read spool record: %v", err)
	}
	if !bytes.HasSuffix(data, []byte("\n")) {
		t.Fatalf("spool record should end with a newline: %q", data)
	}
	var got Record
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode spool record: %v", err)
	}
	if got.ID != rec.ID || got.Severity != rec.Severity || got.Actor != rec.Actor || got.Message != rec.Message {
		t.Fatalf("decoded record = %+v, want %+v", got, rec)
	}
	assertNoTempFiles(t, filepath.Dir(path))

	duplicate := rec
	duplicate.Message = "should not overwrite"
	if _, err := WriteSpool(cityPath, duplicate); err == nil {
		t.Fatalf("WriteSpool() duplicate error = nil, want record already exists")
	} else if !strings.Contains(err.Error(), "record already exists") {
		t.Fatalf("WriteSpool() duplicate error = %q, want record already exists", err)
	}
	dataAfterDuplicate, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read spool record after duplicate: %v", err)
	}
	if !bytes.Equal(dataAfterDuplicate, data) {
		t.Fatalf("duplicate write changed existing spool record")
	}
	assertNoTempFiles(t, filepath.Dir(path))
}

func TestWriteSpoolRejectsInvalidInputs(t *testing.T) {
	rec := Record{ID: "20260610T193456Z-deadbeef", Severity: SeverityError, Actor: "controller", Message: "message"}
	if _, err := WriteSpool("", rec); err == nil {
		t.Fatalf("WriteSpool() empty city path error = nil")
	}

	rec.ID = "../../escape"
	if _, err := WriteSpool(t.TempDir(), rec); err == nil {
		t.Fatalf("WriteSpool() invalid id error = nil")
	}
}

func TestNotifyDedupeKeyIsStableTrimmedAndSHA256Backed(t *testing.T) {
	got := NotifyDedupeKey(" error ", " controller cannot reach spool ")
	sum := sha256.Sum256([]byte("error\x00controller cannot reach spool"))
	want := "error-" + hex.EncodeToString(sum[:])
	if got != want {
		t.Fatalf("NotifyDedupeKey() = %q, want %q", got, want)
	}
	if got != NotifyDedupeKey("error", "controller cannot reach spool") {
		t.Fatalf("NotifyDedupeKey() is not stable across trimmed inputs")
	}
	if strings.ContainsAny(got, `/\`) {
		t.Fatalf("NotifyDedupeKey() produced filesystem separator in %q", got)
	}
	if got == NotifyDedupeKey("error", "different message") {
		t.Fatalf("NotifyDedupeKey() must change when message changes")
	}
}

func TestMarkNotifyDedupeSuppressesWithinTTLAndRefiresAfterExpiry(t *testing.T) {
	cityPath := t.TempDir()
	key := NotifyDedupeKey(SeverityCritical, "controller down")
	start := time.Date(2026, 6, 10, 20, 0, 0, 0, time.UTC)
	ttl := time.Minute

	first, err := MarkNotifyDedupe(cityPath, key, start, ttl)
	if err != nil {
		t.Fatalf("MarkNotifyDedupe(first) error = %v", err)
	}
	if !first.Fire {
		t.Fatalf("first MarkNotifyDedupe Fire = false, want true")
	}
	if first.KeyPrefix != key[:16] {
		t.Fatalf("first KeyPrefix = %q, want %q", first.KeyPrefix, key[:16])
	}

	second, err := MarkNotifyDedupe(cityPath, key, start.Add(30*time.Second), ttl)
	if err != nil {
		t.Fatalf("MarkNotifyDedupe(second) error = %v", err)
	}
	if second.Fire {
		t.Fatalf("second MarkNotifyDedupe Fire = true, want false within TTL")
	}
	if second.Age < 0 || second.Age >= ttl {
		t.Fatalf("second Age = %s, want non-negative age below TTL %s", second.Age, ttl)
	}

	third, err := MarkNotifyDedupe(cityPath, key, start.Add(2*time.Minute), ttl)
	if err != nil {
		t.Fatalf("MarkNotifyDedupe(third) error = %v", err)
	}
	if !third.Fire {
		t.Fatalf("third MarkNotifyDedupe Fire = false, want true after TTL")
	}
	if third.Age < ttl {
		t.Fatalf("third Age = %s, want at least TTL %s", third.Age, ttl)
	}

	markerPath := filepath.Join(SpoolDir(cityPath), notifyDedupeDirName, key)
	info, err := os.Stat(markerPath)
	if err != nil {
		t.Fatalf("stat dedupe marker: %v", err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Fatalf("dedupe marker mode = %v, want %v", got, want)
	}
	dirInfo, err := os.Stat(filepath.Dir(markerPath))
	if err != nil {
		t.Fatalf("stat dedupe dir: %v", err)
	}
	if got, want := dirInfo.Mode().Perm(), os.FileMode(0o700); got != want {
		t.Fatalf("dedupe dir mode = %v, want %v", got, want)
	}
}

func TestMarkNotifyDedupeRejectsUnsafeKeys(t *testing.T) {
	for _, key := range []string{"", "severity/escape", `severity\escape`, ".", ".."} {
		t.Run(key, func(t *testing.T) {
			if _, err := MarkNotifyDedupe(t.TempDir(), key, time.Now(), time.Minute); err == nil {
				t.Fatalf("MarkNotifyDedupe(%q) error = nil, want validation error", key)
			}
		})
	}
}

func TestValidSeverityBoundaries(t *testing.T) {
	for _, severity := range []string{SeverityInfo, SeverityWarn, SeverityError, SeverityCritical, " " + SeverityInfo + " "} {
		if !ValidSeverity(severity) {
			t.Errorf("ValidSeverity(%q) = false, want true", severity)
		}
	}
	for _, severity := range []string{"", "debug", "INFO", "critical/error"} {
		if ValidSeverity(severity) {
			t.Errorf("ValidSeverity(%q) = true, want false", severity)
		}
	}
}

func TestValidRecordIDBoundaries(t *testing.T) {
	valid := []string{
		"20260610T193456Z-deadbeef",
		" 20260610T193456Z-00000000 ",
	}
	for _, id := range valid {
		if !ValidRecordID(id) {
			t.Errorf("ValidRecordID(%q) = false, want true", id)
		}
	}

	invalid := []string{
		"",
		"20260610T193456Z-deadbee",
		"20260610T193456Z-deadbeef00",
		"20260610T193456Z-DEADBEEF",
		"2026-06-10T19:34:56Z-deadbeef",
		"../20260610T193456Z-deadbeef",
		"20260610T193456Z-deadbeef.json",
	}
	for _, id := range invalid {
		if ValidRecordID(id) {
			t.Errorf("ValidRecordID(%q) = true, want false", id)
		}
	}
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read spool dir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("found leftover temp file %q in %s", entry.Name(), dir)
		}
	}
}
