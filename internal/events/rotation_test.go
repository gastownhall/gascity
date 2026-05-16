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
