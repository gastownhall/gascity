package events

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGzipAndArchiveCompressesAndRemovesSource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "events.jsonl.rotating-20260507T180000Z")
	const body = "line1\nline2\nline3\n"
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, formatArchiveBasename(time.Date(2026, 5, 7, 18, 0, 0, 0, time.UTC), 1, 3))

	var stderr bytes.Buffer
	if err := gzipAndArchive(src, dest, &stderr); err != nil {
		t.Fatalf("gzipAndArchive: %v", err)
	}
	if stderr.Len() > 0 {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}

	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source still exists after archive: stat err = %v", err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("destination missing: %v", err)
	}
	if _, err := os.Stat(dest + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp file still exists: stat err = %v", err)
	}

	got, err := readGzipFile(dest)
	if err != nil {
		t.Fatalf("readGzipFile: %v", err)
	}
	if got != body {
		t.Errorf("decompressed content = %q, want %q", got, body)
	}
}

func TestGzipAndArchiveCollisionGuard(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "events.jsonl.rotating-20260507T180000Z")
	if err := os.WriteFile(src, []byte("new content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, formatArchiveBasename(time.Date(2026, 5, 7, 18, 0, 0, 0, time.UTC), 1, 3))
	const existing = "do not overwrite\n"
	if err := os.WriteFile(dest, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	err := gzipAndArchive(src, dest, &stderr)
	if err == nil {
		t.Fatal("expected error on collision, got nil")
	}
	if !strings.Contains(stderr.String(), filepath.Base(dest)) {
		t.Errorf("expected stderr to mention %q, got %q", filepath.Base(dest), stderr.String())
	}

	if _, err := os.Stat(src); err != nil {
		t.Errorf("source removed despite collision: %v", err)
	}
	contents, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("ReadFile dest: %v", err)
	}
	if string(contents) != existing {
		t.Errorf("destination overwritten: got %q, want %q", string(contents), existing)
	}
}

func TestReapOrphanedRotatingFilesGzipsRotating(t *testing.T) {
	dir := t.TempDir()

	rotating := filepath.Join(dir, "events.jsonl.rotating-20260507T120000Z")
	const body = `{"seq":1,"type":"x"}
{"seq":2,"type":"y"}
{"seq":3,"type":"z"}
`
	if err := os.WriteFile(rotating, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	tmpOrphan := filepath.Join(dir, "events.jsonl.archive-20260101T000000Z-seq-1-2.gz.tmp")
	if err := os.WriteFile(tmpOrphan, []byte("incomplete"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	if err := reapOrphanedRotatingFiles(dir, &stderr); err != nil {
		t.Fatalf("reap: %v", err)
	}

	if _, err := os.Stat(rotating); !os.IsNotExist(err) {
		t.Errorf("rotating file should be gone after reap: %v", err)
	}
	if _, err := os.Stat(tmpOrphan); !os.IsNotExist(err) {
		t.Errorf(".gz.tmp orphan should be removed: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var archives []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".gz") {
			archives = append(archives, e.Name())
		}
	}
	if len(archives) != 1 {
		t.Fatalf("expected exactly one .gz after reap, got %v", archives)
	}
	info, err := parseArchiveBasename(archives[0])
	if err != nil {
		t.Fatalf("reaped archive %q is not canonical: %v", archives[0], err)
	}
	if info.FirstSeq != 1 || info.LastSeq != 3 {
		t.Errorf("reaped archive seq window = [%d,%d], want [1,3]", info.FirstSeq, info.LastSeq)
	}

	got, err := readGzipFile(filepath.Join(dir, archives[0]))
	if err != nil {
		t.Fatalf("decompress reaped archive: %v", err)
	}
	if got != body {
		t.Errorf("decompressed content mismatch:\n got=%q\nwant=%q", got, body)
	}
}

func TestReapOrphanedRotatingFilesNoOpWhenClean(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte("active\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	if err := reapOrphanedRotatingFiles(dir, &stderr); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if stderr.Len() > 0 {
		t.Errorf("expected no stderr on clean dir, got %q", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "events.jsonl")); err != nil {
		t.Errorf("active log missing: %v", err)
	}
}

func TestReapOrphanedRotatingFilesEmptyRotatingFile(t *testing.T) {
	dir := t.TempDir()
	rotating := filepath.Join(dir, "events.jsonl.rotating-20260507T120000Z")
	if err := os.WriteFile(rotating, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	if err := reapOrphanedRotatingFiles(dir, &stderr); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if _, err := os.Stat(rotating); !os.IsNotExist(err) {
		t.Errorf("empty rotating file should be removed: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".gz") {
			t.Errorf("empty rotating file should not produce an archive, got %s", e.Name())
		}
	}
}

func TestNewFileRecorderMigratesLegacyArchiveOnOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	legacyBase := "events.jsonl.archive-20260416.gz"
	legacyPath := filepath.Join(dir, legacyBase)
	const body = `{"seq":10,"type":"bead.created","actor":"human","subject":"first"}
{"seq":11,"type":"bead.updated","actor":"human","subject":"middle"}
{"seq":12,"type":"bead.closed","actor":"human","subject":"last"}
`
	writeGzipFile(t, legacyPath, body)

	beforeMigration := time.Now().UTC().Add(-time.Second)
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	rec.Record(Event{Type: BeadClosed, Actor: "human", Subject: "post-migration"})
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() > 0 {
		t.Errorf("unexpected stderr during legacy migration: %q", stderr.String())
	}
	afterMigration := time.Now().UTC().Add(time.Second)

	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Errorf("legacy archive should be renamed away: %v", err)
	}
	canonicalPath, info := findArchiveBySeq(t, dir, 10, 12)
	if info.Timestamp.Before(beforeMigration) || info.Timestamp.After(afterMigration) {
		t.Errorf("migrated archive timestamp = %s, want between %s and %s",
			info.Timestamp, beforeMigration, afterMigration)
	}
	if _, err := os.Stat(canonicalPath); err != nil {
		t.Fatalf("canonical archive missing after migration: %v", err)
	}
	got, err := readGzipFile(canonicalPath)
	if err != nil {
		t.Fatalf("read migrated archive: %v", err)
	}
	if got != body {
		t.Errorf("migrated archive content mismatch:\n got=%q\nwant=%q", got, body)
	}

	all, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("ReadAll after legacy migration returned %d events, want 4", len(all))
	}
	if all[0].Seq != 10 || all[2].Seq != 12 || all[3].Seq != 13 {
		t.Errorf("seqs after migration = [%d,%d,%d], want [10,12,13]",
			all[0].Seq, all[2].Seq, all[3].Seq)
	}

	var secondStderr bytes.Buffer
	rec2, err := NewFileRecorder(path, &secondStderr)
	if err != nil {
		t.Fatal(err)
	}
	if err := rec2.Close(); err != nil {
		t.Fatal(err)
	}
	if secondStderr.Len() > 0 {
		t.Errorf("legacy migration should be idempotent on second open, stderr = %q", secondStderr.String())
	}
}

func TestNewFileRecorderMigratedLegacyArchiveSurvivesRetainAgeAfterRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	legacyBase := "events.jsonl.archive-20000101.gz"
	legacyPath := filepath.Join(dir, legacyBase)
	const body = `{"seq":10,"type":"bead.created","actor":"human","subject":"first"}
{"seq":11,"type":"bead.updated","actor":"human","subject":"middle"}
{"seq":12,"type":"bead.closed","actor":"human","subject":"last"}
`
	writeGzipFile(t, legacyPath, body)

	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr, WithArchiveRetainAge(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	rec.Record(Event{Type: BeadCreated, Actor: "human", Subject: "post-migration"})
	res, err := rec.ForceRotate()
	if err != nil {
		t.Fatalf("ForceRotate: %v", err)
	}
	if res.Done != nil {
		<-res.Done
	}

	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Errorf("legacy archive should be renamed away: %v", err)
	}
	canonicalPath, _ := findArchiveBySeq(t, dir, 10, 12)
	if _, err := os.Stat(canonicalPath); err != nil {
		t.Fatalf("migrated archive should survive first retain-age pruning: %v", err)
	}
	if _, err := os.Stat(res.ArchivePath); err != nil {
		t.Fatalf("fresh rotation archive should remain after retention pruning: %v", err)
	}
}

func TestMigrateLegacyArchiveCollisionLeavesSourceAndDestination(t *testing.T) {
	dir := t.TempDir()
	legacyBase := "events.jsonl.archive-20260416.gz"
	legacyPath := filepath.Join(dir, legacyBase)
	const body = `{"seq":1,"type":"bead.created","actor":"human","subject":"first"}
{"seq":2,"type":"bead.closed","actor":"human","subject":"last"}
`
	writeGzipFile(t, legacyPath, body)

	migrationTime := time.Date(2026, 5, 17, 16, 30, 0, 0, time.UTC)
	canonicalPath := filepath.Join(dir, formatArchiveBasename(migrationTime, 1, 2))
	const existing = "existing archive"
	if err := os.WriteFile(canonicalPath, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	err := migrateLegacyArchive(legacyPath, dir, legacyBase, migrationTime)
	if err == nil {
		t.Fatal("expected collision error, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("collision error = %v, want already exists", err)
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("legacy archive should remain after collision: %v", err)
	}
	got, err := os.ReadFile(canonicalPath)
	if err != nil {
		t.Fatalf("ReadFile canonical archive: %v", err)
	}
	if string(got) != existing {
		t.Fatalf("canonical archive overwritten: got %q, want %q", string(got), existing)
	}
}

func TestNewFileRecorderLeavesUnparseableLegacyArchive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	legacyBase := "events.jsonl.archive-20260416.gz"
	legacyPath := filepath.Join(dir, legacyBase)
	if err := os.WriteFile(legacyPath, []byte("not gzip"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("unparseable legacy archive should be left in place: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "events.jsonl.archive-20260416T*-seq-*.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("unparseable legacy archive should not produce canonical archives, got %v", matches)
	}
	if !strings.Contains(stderr.String(), "legacy archive") || !strings.Contains(stderr.String(), legacyBase) {
		t.Errorf("stderr should mention legacy archive %q, got %q", legacyBase, stderr.String())
	}
}

func findArchiveBySeq(t *testing.T, dir string, first, last uint64) (string, archiveInfo) {
	t.Helper()

	archives, err := archiveFilesIn(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range archives {
		if info.FirstSeq == first && info.LastSeq == last {
			return filepath.Join(dir, info.Basename), info
		}
	}
	t.Fatalf("archive with seq window [%d,%d] not found in %v", first, last, archives)
	return "", archiveInfo{}
}

func writeGzipFile(t *testing.T, path, body string) {
	t.Helper()

	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	if _, err := gw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func readGzipFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck
	gr, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer gr.Close() //nolint:errcheck
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, gr); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// readActiveOnly reads only the active log at path without walking
// sibling archives. Used by tests that want to inspect the active
// file in isolation.
func readActiveOnly(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close() //nolint:errcheck
	dec := bufio.NewScanner(f)
	dec.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var out []Event
	for dec.Scan() {
		var e Event
		if err := json.Unmarshal(dec.Bytes(), &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, dec.Err()
}
