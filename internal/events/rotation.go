package events

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// rotatingBasenameRE matches the new-format rotating filename
// produced by rotateLocked: events.jsonl.rotating-<ts>-seq-<a>-<b>.
// Captures: timestamp, firstSeq, lastSeq.
var rotatingBasenameRE = regexp.MustCompile(
	`^events\.jsonl\.rotating-(\d{8}T\d{6}Z)-seq-(\d+)-(\d+)$`,
)

// gzipAndArchive compresses source into a gzip file at dest, using a
// .gz.tmp + os.Rename to keep the operation atomic. On success, source
// is removed and dest is the canonical archive path.
//
// Collision guard (designer §8.3): if dest already exists, the source
// file is left in place for operator inspection and the function
// writes a warning to stderr and returns an error. This prevents a
// hash-colliding rotation from silently destroying a prior archive.
func gzipAndArchive(source, dest string, stderr io.Writer) error {
	if _, err := os.Stat(dest); err == nil {
		fmt.Fprintf(stderr, //nolint:errcheck // best-effort stderr
			"events: rotation: target archive %q already exists; leaving %q in place for operator inspection\n",
			filepath.Base(dest), filepath.Base(source))
		return fmt.Errorf("archive %q already exists", filepath.Base(dest))
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %q: %w", dest, err)
	}

	in, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("opening source: %w", err)
	}
	defer in.Close() //nolint:errcheck // read-only file

	tmp := dest + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("creating %q: %w", tmp, err)
	}

	gw := gzip.NewWriter(out)
	if _, err := io.Copy(gw, in); err != nil {
		_ = gw.Close()
		_ = out.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("compressing %q: %w", source, err)
	}
	if err := gw.Close(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("closing gzip writer: %w", err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("syncing %q: %w", tmp, err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("closing %q: %w", tmp, err)
	}

	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("renaming %q -> %q: %w", tmp, dest, err)
	}
	if err := os.Remove(source); err != nil {
		fmt.Fprintf(stderr, //nolint:errcheck // best-effort stderr
			"events: rotation: archive succeeded but failed to remove source %q: %v\n",
			source, err)
	}
	return nil
}

// reapOrphanedRotatingFiles cleans up artifacts left behind by a
// crashed rotation: each events.jsonl.rotating-<ts> is gzipped into
// its canonical archive name (the seq window is read from the
// rotating file's first and last lines), and each *.gz.tmp is removed
// outright (an incomplete gzip cannot be salvaged).
//
// The sweep is idempotent: re-running it on a clean directory is a
// no-op. Failures on individual files are logged to stderr and the
// sweep continues — a single corrupt orphan must not block recovery
// of the others.
//
// Designer §8.3: on canonical-name collision, the rotating-* file is
// left in place rather than overwriting the existing archive.
func reapOrphanedRotatingFiles(dir string, stderr io.Writer) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("listing %q: %w", dir, err)
	}

	var rotatings []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		switch {
		case hasRotatingPrefix(name):
			rotatings = append(rotatings, name)
		case hasGzipTmpSuffix(name):
			path := filepath.Join(dir, name)
			if err := os.Remove(path); err != nil {
				fmt.Fprintf(stderr, "events: rotation: removing stale %q: %v\n", name, err) //nolint:errcheck // best-effort stderr
			}
		}
	}

	sort.Strings(rotatings)
	for _, base := range rotatings {
		src := filepath.Join(dir, base)
		if err := archiveRotatingFile(src, dir, base, stderr); err != nil {
			fmt.Fprintf(stderr, "events: rotation: reaping %q: %v\n", base, err) //nolint:errcheck // best-effort stderr
		}
	}
	return nil
}

// archiveRotatingFile is the per-file branch of the reaper. The
// rotating filename embeds the timestamp and seq window so the
// archive name can be derived without scanning content. For legacy
// rotating files (older convention without the seq window), the
// reaper falls back to scanning the file for first/last seq.
//
// An empty rotating file (rotation crashed before any byte was
// renamed in) is simply removed.
func archiveRotatingFile(src, dir, base string, stderr io.Writer) error {
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	if info.Size() == 0 {
		if err := os.Remove(src); err != nil {
			return fmt.Errorf("removing empty rotating file: %w", err)
		}
		return nil
	}

	if ts, first, last, ok := parseRotatingBasename(base); ok {
		dest := filepath.Join(dir, formatArchiveBasename(ts, first, last))
		return gzipAndArchive(src, dest, stderr)
	}

	first, last, err := readSeqWindow(src)
	if err != nil {
		return fmt.Errorf("reading seq window: %w", err)
	}

	ts, err := timestampFromRotatingBasename(base)
	if err != nil {
		ts = info.ModTime().UTC()
	}

	dest := filepath.Join(dir, formatArchiveBasename(ts, first, last))
	return gzipAndArchive(src, dest, stderr)
}

// parseRotatingBasename extracts (timestamp, firstSeq, lastSeq) from a
// new-format rotating filename: events.jsonl.rotating-<ts>-seq-<a>-<b>.
// Returns ok=false for legacy filenames that lack the seq segment;
// callers should fall back to content-based seq window detection.
func parseRotatingBasename(name string) (time.Time, uint64, uint64, bool) {
	m := rotatingBasenameRE.FindStringSubmatch(name)
	if m == nil {
		return time.Time{}, 0, 0, false
	}
	ts, err := time.Parse(archiveTimestampLayout, m[1])
	if err != nil {
		return time.Time{}, 0, 0, false
	}
	first, err := strconv.ParseUint(m[2], 10, 64)
	if err != nil {
		return time.Time{}, 0, 0, false
	}
	last, err := strconv.ParseUint(m[3], 10, 64)
	if err != nil {
		return time.Time{}, 0, 0, false
	}
	if first > last {
		return time.Time{}, 0, 0, false
	}
	return ts, first, last, true
}

// readSeqWindow returns the first and last Seq values stored in a
// JSONL events file. It scans forward for the first complete
// well-formed line and uses the same tail-scanning logic as
// ReadLatestSeq for the last line. Returns an error if the file has
// no parseable events.
func readSeqWindow(path string) (uint64, uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close() //nolint:errcheck // read-only file

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var first uint64
	for scanner.Scan() {
		var header struct {
			Seq uint64 `json:"seq"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &header); err != nil {
			continue
		}
		if header.Seq > 0 {
			first = header.Seq
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, fmt.Errorf("scanning: %w", err)
	}
	if first == 0 {
		return 0, 0, fmt.Errorf("no parseable events in %q", path)
	}

	stat, err := f.Stat()
	if err != nil {
		return 0, 0, fmt.Errorf("stat: %w", err)
	}
	last, err := readLatestSeqFromTail(f, stat.Size())
	if err != nil {
		return 0, 0, fmt.Errorf("reading last seq: %w", err)
	}
	if last < first {
		// Single-event file collapses both ends.
		last = first
	}
	return first, last, nil
}

// timestampFromRotatingBasename extracts the rotation timestamp from
// an events.jsonl.rotating-<ts> filename. Returns an error if the
// basename does not carry a parseable timestamp suffix.
func timestampFromRotatingBasename(base string) (time.Time, error) {
	rest := strings.TrimPrefix(base, "events.jsonl.rotating-")
	if rest == base {
		return time.Time{}, fmt.Errorf("not a rotating filename: %q", base)
	}
	ts, err := time.Parse(archiveTimestampLayout, rest)
	if err != nil {
		return time.Time{}, err
	}
	return ts, nil
}
