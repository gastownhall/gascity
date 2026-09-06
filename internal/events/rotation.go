package events

import (
	"bufio"
	"bytes"
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
	"syscall"
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
// supersededRotatingPrefix marks a colliding rotation source that was set
// aside: the canonical archive for its exact timestamp and seq window already
// exists, so the source's events are already archived. The prefix breaks the
// events.jsonl.* namespace, which is what makes the file invisible to
// listBackfillSources and to the startup reaper — the two scanners that
// otherwise re-decode and re-collide a stranded source forever (ga-jctn6).
const supersededRotatingPrefix = "superseded-"

// Collision guard (designer §8.3): if dest already exists, the destination is
// never overwritten — a hash-colliding rotation must not destroy a prior
// archive. The source is SET ASIDE under supersededRotatingPrefix rather than
// left in place: the canonical name embeds the timestamp and seq window, so a
// colliding source holds the same events the archive already holds, and left
// under its rotating name it is re-collided by the reaper on every startup and
// re-decoded by every AfterSeq=0 read. The bytes are preserved for operator
// inspection; the function still reports the collision as an error.
func gzipAndArchive(source, dest string, stderr io.Writer) error {
	if _, err := os.Stat(dest); err == nil {
		setAside := filepath.Join(filepath.Dir(source), supersededRotatingPrefix+filepath.Base(source))
		if renameErr := os.Rename(source, setAside); renameErr != nil {
			fmt.Fprintf(stderr, //nolint:errcheck // best-effort stderr
				"events: rotation: target archive %q already exists; failed to set %q aside: %v\n",
				filepath.Base(dest), filepath.Base(source), renameErr)
		} else {
			fmt.Fprintf(stderr, //nolint:errcheck // best-effort stderr
				"events: rotation: target archive %q already exists; set %q aside as %q for operator inspection\n",
				filepath.Base(dest), filepath.Base(source), filepath.Base(setAside))
		}
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

// reapOrphanedRotatingFiles cleans up rotation-era artifacts on
// startup: each legacy events.jsonl.archive-YYYYMMDD.gz file is
// renamed into the canonical seq-stamped convention, each
// events.jsonl.rotating-<ts> file is gzipped into its canonical
// archive name (the seq window is read from the rotating file's first
// and last lines when the filename lacks seqs), and each *.gz.tmp is
// removed outright (an incomplete gzip cannot be salvaged).
//
// The sweep is idempotent: re-running it on a clean directory is a
// no-op. Failures on individual files are logged to stderr and the
// sweep continues — a single corrupt orphan must not block recovery
// of the others.
//
// Designer §8.3: on canonical-name collision, gzipAndArchive sets the
// rotating source aside under supersededRotatingPrefix (see its doc)
// rather than overwriting the existing archive.
func reapOrphanedRotatingFiles(dir string, stderr io.Writer) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("listing %q: %w", dir, err)
	}

	var legacyArchives []string
	var rotatings []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		switch {
		case isLegacyArchiveBasename(name):
			legacyArchives = append(legacyArchives, name)
		case hasRotatingPrefix(name):
			rotatings = append(rotatings, name)
		case hasGzipTmpSuffix(name):
			path := filepath.Join(dir, name)
			if err := os.Remove(path); err != nil {
				fmt.Fprintf(stderr, "events: rotation: removing stale %q: %v\n", name, err) //nolint:errcheck // best-effort stderr
			}
		}
	}

	sort.Strings(legacyArchives)
	for _, base := range legacyArchives {
		if err := migrateLegacyArchive(filepath.Join(dir, base), dir, base, time.Now().UTC()); err != nil {
			fmt.Fprintf(stderr, "events: rotation: legacy archive %q: %v\n", base, err) //nolint:errcheck // best-effort stderr
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

// nulTailScanWindow bounds how much of the active log's tail
// truncateNulPaddedTail reads when checking for an unclean-shutdown NUL
// run. It comfortably covers the delayed-allocation extents a journaling
// filesystem (e.g. ext4) length-extends before flushing, while keeping the
// startup check O(1) regardless of how large the active log has grown.
const nulTailScanWindow = 64 * 1024

// readNulTailWindow reads the trailing n bytes of f, which the caller
// believes to be size bytes long. It returns ok=false, with no error, if
// fewer than n bytes came back: ReadAt returns io.EOF precisely when a read
// falls short, and short-circuiting on that (rather than accepting io.EOF as
// success) matters because tail is zero-initialized — a short read would
// otherwise leave its unfilled remainder looking exactly like a genuine
// NUL-padded tail. A short read means the file changed size underneath the
// caller (a concurrent rotation or truncation), so the caller must not guess
// at a NUL tail from that partial buffer.
func readNulTailWindow(f *os.File, size, n int64) (tail []byte, ok bool, err error) {
	tail = make([]byte, n)
	read, err := f.ReadAt(tail, size-n)
	if err != nil && err != io.EOF {
		return nil, false, err
	}
	if int64(read) < n {
		return nil, false, nil
	}
	return tail, true, nil
}

// truncateNulPaddedTail detects and removes a NUL-padded tail on the active
// log left by an unclean shutdown mid-write: a filesystem can extend a
// file's length before the corresponding bytes are flushed, so a crash
// between the length-extension and the flush leaves a run of \x00 bytes
// after the last complete record. Left in place, the next Record() call
// appends the new event directly after those NUL bytes with no separating
// newline, fusing the NUL run and the new record into one unparseable
// physical line — which silently truncates every event after it for any
// strict JSON Lines reader (this package's own readers tolerate it by
// skipping the malformed line, but external consumers such as `jq -e .`
// stop there).
//
// It reads only a bounded tail of the file (nulTailScanWindow), so cost is
// independent of file size. If the file does not end in a run of NUL bytes,
// or the last nulTailScanWindow bytes contain no newline to truncate back
// to, it is a no-op — this is deliberately conservative and only repairs
// the specific delayed-allocation pattern described above.
//
// The read-decide-truncate sequence runs under the same cross-process flock
// Record/AppendBatch use, so a concurrent append cannot land between the
// Stat and the Truncate and be discarded.
func truncateNulPaddedTail(path string, stderr io.Writer) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("checking for NUL-padded tail: %w", err)
	}
	defer f.Close() //nolint:errcheck // read handle; write path (Truncate) checked separately

	fd := int(f.Fd())
	if err := lockRecorderFile(fd, path); err != nil {
		return fmt.Errorf("checking for NUL-padded tail: %w", err)
	}
	defer func() {
		if err := syscall.Flock(fd, syscall.LOCK_UN); err != nil {
			fmt.Fprintf(stderr, "events: unlock: %v\n", err) //nolint:errcheck // best-effort stderr
		}
	}()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("checking for NUL-padded tail: %w", err)
	}
	size := info.Size()
	if size == 0 {
		return nil
	}

	n := int64(nulTailScanWindow)
	if size < n {
		n = size
	}
	tail, ok, err := readNulTailWindow(f, size, n)
	if err != nil {
		return fmt.Errorf("checking for NUL-padded tail: %w", err)
	}
	if !ok {
		return nil
	}

	if tail[len(tail)-1] != 0 {
		// File does not end in a NUL byte at all: nothing to repair.
		return nil
	}

	idx := bytes.LastIndexByte(tail, '\n')
	if idx < 0 {
		// No newline within the scan window: either the whole file is one
		// unterminated line, or the NUL run is longer than the window.
		// Neither is the pattern this repairs safely, so leave it alone.
		return nil
	}
	for _, b := range tail[idx+1:] {
		if b != 0 {
			// Trailing bytes after the last newline are not all NUL, so
			// this is not the delayed-allocation pattern — leave it for
			// an operator rather than guessing at a truncation point.
			return nil
		}
	}

	truncateAt := size - n + int64(idx) + 1
	if truncateAt >= size {
		return nil
	}
	if err := f.Truncate(truncateAt); err != nil {
		return fmt.Errorf("truncating NUL-padded tail: %w", err)
	}
	fmt.Fprintf(stderr, //nolint:errcheck // best-effort stderr
		"events: recovered %d-byte NUL-padded tail from unclean shutdown, truncated %q to %d bytes\n",
		size-truncateAt, path, truncateAt)
	return nil
}

// reapExpiredArchives removes canonical archive files older than
// retainAge. A non-positive retainAge is a no-op. Archive age is
// based on the UTC timestamp embedded in the canonical filename, so
// pruning does not need to open or gunzip archive contents.
func reapExpiredArchives(dir string, retainAge time.Duration, stderr io.Writer) error {
	if retainAge <= 0 {
		return nil
	}
	archives, err := archiveFilesIn(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	cutoff := time.Now().UTC().Add(-retainAge)
	for _, info := range archives {
		if !info.Timestamp.Before(cutoff) {
			continue
		}
		path := filepath.Join(dir, info.Basename)
		if err := os.Remove(path); err != nil {
			fmt.Fprintf(stderr, "events: rotation: removing expired archive %q: %v\n", info.Basename, err) //nolint:errcheck // best-effort stderr
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

// migrateLegacyArchive renames a pre-rotation gzip archive into the
// canonical seq-stamped archive convention. The canonical timestamp is
// the migration time, not the legacy filename day, so retain-age pruning
// gives operators a full retention window after the upgrade observes
// previously invisible archives.
func migrateLegacyArchive(src, dir, base string, migrationTime time.Time) error {
	if _, err := parseLegacyArchiveBasename(base); err != nil {
		return err
	}
	first, last, err := readGzipSeqWindow(src)
	if err != nil {
		return fmt.Errorf("reading seq window: %w", err)
	}
	dest := filepath.Join(dir, formatArchiveBasename(migrationTime, first, last))
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("target archive %q already exists; leaving legacy archive in place", dest)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat target archive %q: %w", dest, err)
	}
	if err := os.Rename(src, dest); err != nil {
		return fmt.Errorf("renaming to canonical archive: %w", err)
	}
	return nil
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

// readGzipSeqWindow returns the first and last Seq values stored in a
// gzip-compressed JSONL events archive. Unlike readSeqWindow, it scans
// the full stream forward because gzip streams do not support the
// active-log tail seek used for plain JSONL files.
func readGzipSeqWindow(path string) (uint64, uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close() //nolint:errcheck // read-only file

	gr, err := gzip.NewReader(f)
	if err != nil {
		return 0, 0, fmt.Errorf("gunzip: %w", err)
	}
	defer gr.Close() //nolint:errcheck // read-only stream

	scanner := bufio.NewScanner(gr)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var first, last uint64
	for scanner.Scan() {
		var header struct {
			Seq uint64 `json:"seq"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &header); err != nil {
			continue
		}
		if header.Seq == 0 {
			continue
		}
		if first == 0 {
			first = header.Seq
		}
		last = header.Seq
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, fmt.Errorf("scanning: %w", err)
	}
	if first == 0 {
		return 0, 0, fmt.Errorf("no parseable events in %q", path)
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
