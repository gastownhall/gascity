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
	"sort"
	"time"
)

// readRotationDir is the directory snapshot used by rotation catch-up readers.
// It is indirected so tests can deterministically promote a rotating file at
// the listing boundary instead of racing a gzip goroutine.
var readRotationDir = os.ReadDir

// Filter specifies predicates for ReadFiltered. Zero values are ignored.
type Filter struct {
	Type     string    // match events with this Type
	Actor    string    // match events with this Actor
	Subject  string    // match events with this Subject
	Since    time.Time // match events at or after this time
	Until    time.Time // match events at or before this time
	AfterSeq uint64    // match events with Seq > AfterSeq (0 = no filter)
	// BeforeSeq matches events with Seq < BeforeSeq (0 = no filter). The
	// keyset page boundary for descending event walks: the log is
	// append-only and seq-ordered, so "strictly before this seq" is a
	// stable resume point regardless of concurrent appends.
	BeforeSeq uint64
	Limit     int // cap results at this count (0 or negative = unlimited)
	// MaxScanBytes bounds how far a tail scan (ReadFilteredTail /
	// TailProvider.ListTail) walks backward from EOF before giving up,
	// even if Limit hasn't been reached (0 or negative = unbounded). It
	// exists for callers where "no match within the recent window" is an
	// acceptable, already-representable result — a rare or optional Type
	// filter can otherwise force a full-file backward walk with the same
	// cost as an unfiltered forward scan (#4418). It has no effect on
	// ReadFiltered's forward scan or on List/ListTail implementations
	// that are not byte-scanning a file (e.g. Fake, Multiplexer).
	MaxScanBytes int64
}

// matchesFilter reports whether e satisfies all non-zero predicates in f.
// It does not enforce Limit — that is applied by the caller.
func matchesFilter(e Event, f Filter) bool {
	if f.AfterSeq > 0 && e.Seq <= f.AfterSeq {
		return false
	}
	if f.BeforeSeq > 0 && e.Seq >= f.BeforeSeq {
		return false
	}
	if f.Type != "" && e.Type != f.Type {
		return false
	}
	if f.Actor != "" && e.Actor != f.Actor {
		return false
	}
	if f.Subject != "" && e.Subject != f.Subject {
		return false
	}
	if !f.Since.IsZero() && e.Ts.Before(f.Since) {
		return false
	}
	if !f.Until.IsZero() && e.Ts.After(f.Until) {
		return false
	}
	return true
}

// ApplyFilter returns events matching all non-zero predicates in filter.
// It preserves input order and applies a positive Limit after matching.
func ApplyFilter(evts []Event, filter Filter) []Event {
	var result []Event
	for _, e := range evts {
		if !matchesFilter(e, filter) {
			continue
		}
		result = append(result, e)
		if limitReached(len(result), filter) {
			break
		}
	}
	return result
}

func limitReached(count int, filter Filter) bool {
	return filter.Limit > 0 && count >= filter.Limit
}

// ReadAll reads all events from the JSONL file at path, transparently
// walking sibling archives produced by rotation. Archives are read in
// seq order before the active file, yielding a single chronological
// stream. Returns (nil, nil) if neither the active file nor any
// archives exist.
func ReadAll(path string) ([]Event, error) {
	return ReadFiltered(path, Filter{})
}

// seqProbeBufSize bounds the read-ahead used when probing a byte offset for the
// line that begins there. Events average ~1.4 KB on a live log; 64 KB
// comfortably spans one line plus the partial line a probe may land inside.
const seqProbeBufSize = 64 * 1024

// seqLineAt probes byte offset off and returns the seq of the next parseable
// line, along with that line's start offset.
//
// Contract, which the binary search in activeScanStart depends on: for off > 0
// the line containing off is treated as a partial line and DISCARDED, so the
// result is the first parseable line beginning strictly after off. Only off == 0
// reads the line starting at off itself. Probing a known line start therefore
// returns the FOLLOWING line, not the line at that offset.
//
// start is always a true line boundary. Unparseable lines are skipped, matching
// the scanner's long-standing tolerance for malformed rows. ok is false when EOF
// is reached without finding a parseable line.
func seqLineAt(f *os.File, off int64) (seq uint64, start int64, ok bool) {
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return 0, 0, false
	}
	r := bufio.NewReaderSize(f, seqProbeBufSize)
	pos := off
	if off > 0 {
		partial, err := r.ReadBytes('\n')
		if err != nil {
			return 0, 0, false
		}
		pos += int64(len(partial))
	}
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var probe struct {
				Seq uint64 `json:"seq"`
			}
			if json.Unmarshal(bytes.TrimRight(line, "\r\n"), &probe) == nil && probe.Seq > 0 {
				return probe.Seq, pos, true
			}
			pos += int64(len(line))
		}
		if err != nil {
			return 0, 0, false
		}
	}
}

// activeScanStart returns the byte offset in the active log at which a scan can
// begin without missing any event Filter.AfterSeq can match.
//
// The active log is append-only with strictly increasing seq (FileRecorder
// assigns e.Seq = r.seq++ under its mutex), so every line at or below the
// cursor sorts before every line above it and can be skipped wholesale. This is
// the seq-window skip archiveOverlapsFilter already applies to .gz archives,
// extended to the active file. Without it a cursored read re-parses the entire
// log on every call and AfterSeq saves nothing, because matchesFilter applies
// it only after each line has been unmarshalled -- the dominant cost of the
// supervisor's per-tick order-trigger check (gcy-ocb5).
//
// Returns 0 (full scan) whenever the boundary cannot be established, so a log
// that violates the ordering invariant degrades in speed, never in correctness.
func activeScanStart(f *os.File, size int64, afterSeq uint64) int64 {
	if afterSeq == 0 || size <= 0 {
		return 0
	}
	// Nothing to skip when the log's first line is already above the cursor.
	if first, _, ok := seqLineAt(f, 0); !ok || first > afterSeq {
		return 0
	}
	i := sort.Search(int(size), func(i int) bool {
		seq, _, ok := seqLineAt(f, int64(i))
		return !ok || seq > afterSeq
	})
	seq, start, ok := seqLineAt(f, int64(i))
	if !ok || seq <= afterSeq {
		// Cursor is at or beyond the newest event: skip the file entirely.
		return size
	}
	return start
}

// ReadFiltered reads events from path and sibling archives, returning
// only those matching all non-zero fields in filter. Archives whose
// seq window is fully excluded by the filter's AfterSeq predicate are
// skipped without gunzipping. Returns (nil, nil) if no events exist.
// Scanner errors return the events parsed before the error alongside
// the error.
func ReadFiltered(path string, filter Filter) ([]Event, error) {
	result, _, err := readFilteredTracked(path, filter)
	return result, err
}

type eventSeqWindow struct {
	first uint64
	last  uint64
}

// readFilteredTracked is ReadFiltered plus the archive windows present in its
// initial directory snapshot. ReadFilteredWithInFlight uses that set to avoid
// reopening stable archives (including later windows after a Limit is reached)
// while still detecting an archive promoted after this scan.
func readFilteredTracked(path string, filter Filter) ([]Event, map[eventSeqWindow]struct{}, error) {
	dir := filepath.Dir(path)
	archives, err := archiveFilesIn(dir)
	if err != nil {
		// Listing the dir failed (most often: dir doesn't exist).
		// Fall through to the active-file path; if that also fails,
		// the caller gets a single error.
		archives = nil
	}

	var result []Event
	listed := make(map[eventSeqWindow]struct{}, len(archives))
	for _, info := range archives {
		listed[eventSeqWindow{first: info.FirstSeq, last: info.LastSeq}] = struct{}{}
	}
	for _, info := range archives {
		if !archiveOverlapsFilter(info, filter) {
			continue
		}
		archivePath := filepath.Join(dir, info.Basename)
		err := streamArchive(archivePath, filter, func(e Event) bool {
			if !matchesFilter(e, filter) {
				return true
			}
			result = append(result, e)
			return !limitReached(len(result), filter)
		})
		if err != nil {
			return result, listed, fmt.Errorf("reading archive %q: %w", info.Basename, err)
		}
		if limitReached(len(result), filter) {
			return result, listed, nil
		}
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			if len(result) == 0 {
				return nil, listed, nil
			}
			return result, listed, nil
		}
		return result, listed, fmt.Errorf("reading events: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file

	// Skip the prefix of the active log the cursor already covers. seqLineAt
	// leaves the descriptor wherever its last probe ended, so seek explicitly
	// even when the scan starts at 0 (the uncursored case).
	scanFrom := int64(0)
	if st, statErr := f.Stat(); statErr == nil {
		scanFrom = activeScanStart(f, st.Size(), filter.AfterSeq)
	}
	if _, err := f.Seek(scanFrom, io.SeekStart); err != nil {
		return result, listed, fmt.Errorf("seeking events: %w", err)
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // handle lines up to 1MB
	for scanner.Scan() {
		var e Event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue // skip malformed lines
		}
		if !matchesFilter(e, filter) {
			continue
		}
		result = append(result, e)
		if limitReached(len(result), filter) {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return result, listed, fmt.Errorf("scanning events: %w", err)
	}
	return result, listed, nil
}

// ReadFilteredWithInFlight is ReadFiltered plus events still stranded in
// in-flight rotation files. When rotateLocked renames the active log to
// events.jsonl.rotating-<ts>-seq-<a>-<b>, a background goroutine gzips it into
// the canonical .gz archive and only then removes the rotating file. In that
// window the just-rotated events live ONLY in the plain-JSONL rotating file,
// which ReadFiltered (it lists only .gz archives) cannot see. The live run
// tailer folds these in when it detects a rotation, before resetting its
// active-file cursor, so events written to the old active log in the poll window
// before the rename are not lost during the asynchronous compression.
//
// Callers must be seq-idempotent: during the brief window when a canonical .gz
// and its source rotating file coexist, an event can appear in both. The result
// is de-duplicated by seq and returned in seq order. Intended for the AfterSeq
// catch-up path; a positive Filter.Limit bounds only ReadFiltered's own scan,
// not newly discovered rotation sources merged by the recovery pass.
func ReadFilteredWithInFlight(path string, filter Filter) ([]Event, error) {
	base, listedArchives, baseErr := readFilteredTracked(path, filter)
	rotated, rotationErr := readRotationSources(path, filter, listedArchives)
	if len(rotated) == 0 {
		if baseErr == nil {
			return base, rotationErr
		}
		return base, baseErr
	}
	merged := mergeEventsBySeq(base, rotated)
	if baseErr != nil {
		return merged, baseErr
	}
	return merged, rotationErr
}

// readRotationSources performs the post-active directory scan across BOTH
// canonical archives and in-flight rotating files. A rotation promotion can
// land after readFilteredTracked's archive snapshot: reading only rotating
// files here would then see neither the old source nor the newly-installed
// archive. listBackfillSources closes that gap, and openSegmentReader closes the
// second gap where a listed rotating source is promoted before open by falling
// back to its derived archive path.
//
// Stable archives present in the base scan's snapshot are skipped by seq
// window, so the normal cold-load path pays only a second directory listing
// rather than decoding the full archive history twice. That includes later
// archives the base intentionally did not open after satisfying Filter.Limit.
func readRotationSources(path string, filter Filter, listedArchives map[eventSeqWindow]struct{}) ([]Event, error) {
	dir := filepath.Dir(path)
	sources, err := listBackfillSources(dir, filter.AfterSeq)
	if err != nil {
		return nil, err
	}
	if len(sources) == 0 {
		return nil, nil
	}

	var result []Event
	maxSeq := filter.AfterSeq
	for _, src := range sources {
		// Any source whose exact seq window an already-read archive covers is
		// redundant: for a stable archive it IS that archive, and for a rotating
		// file it is the archive's not-yet-removed twin holding the same seqs
		// (the crash window between archive rename and source removal makes such
		// twins routine, and mergeEventsBySeq would drop every line anyway).
		if _, ok := listedArchives[eventSeqWindow{first: src.firstSeq, last: src.lastSeq}]; ok {
			continue
		}
		reader, err := openSegmentReader(src)
		if err != nil {
			return result, fmt.Errorf("reading rotation source %q: %w", filepath.Base(src.path), err)
		}
		if reader == nil {
			continue
		}
		for {
			done, readErr := reader.readInto(filter, &maxSeq, &result, backfillBatch)
			if readErr != nil {
				reader.close()
				return result, fmt.Errorf("reading rotation source %q: %w", filepath.Base(src.path), readErr)
			}
			if done {
				break
			}
		}
		reader.close()
	}
	return result, nil
}

// mergeEventsBySeq merges two seq-ascending event slices into one seq-ascending
// slice, dropping exact seq duplicates — an event present in both a canonical
// archive and its not-yet-removed source rotating file. Event seqs are globally
// monotonic and unique, so equal seq means the same event.
func mergeEventsBySeq(a, b []Event) []Event {
	out := make([]Event, 0, len(a)+len(b))
	appendUnique := func(e Event) {
		if n := len(out); n > 0 && out[n-1].Seq == e.Seq {
			return
		}
		out = append(out, e)
	}
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i].Seq <= b[j].Seq {
			appendUnique(a[i])
			i++
		} else {
			appendUnique(b[j])
			j++
		}
	}
	for ; i < len(a); i++ {
		appendUnique(a[i])
	}
	for ; j < len(b); j++ {
		appendUnique(b[j])
	}
	return out
}

// archiveFilesIn lists canonical events archives in dir, sorted by
// FirstSeq ascending so callers can read them in chronological order.
// Files that don't match the canonical name pattern (legacy archives,
// unrelated files) are silently skipped — a corrupt archive in the
// dir must not poison the read path.
func archiveFilesIn(dir string) ([]archiveInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var archives []archiveInfo
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := parseArchiveBasename(e.Name())
		if err != nil {
			continue
		}
		archives = append(archives, info)
	}
	sort.Slice(archives, func(i, j int) bool {
		return archives[i].FirstSeq < archives[j].FirstSeq
	})
	return archives, nil
}

// archiveSeq reads the top-level seq of a raw archive line without decoding
// the record. FileRecorder writes seq as the first field, so the hot path is a
// prefix scan with no allocation. Any other layout reports false and the
// caller falls back to a full decode, which keeps a foreign writer's archive
// readable.
func archiveSeq(line []byte) (uint64, bool) {
	const prefix = `{"seq":`
	if !bytes.HasPrefix(line, []byte(prefix)) {
		return 0, false
	}
	digits := line[len(prefix):]
	var seq uint64
	i := 0
	for ; i < len(digits) && digits[i] >= '0' && digits[i] <= '9'; i++ {
		seq = seq*10 + uint64(digits[i]-'0')
	}
	if i == 0 {
		return 0, false
	}
	return seq, true
}

// streamArchive gunzip-streams the file at path, decoding each line
// as an Event and invoking fn for every event. fn returns false to
// abort iteration early. Returns nil if iteration completed cleanly
// or fn requested abort; errors from gzip / scanner are wrapped.
//
// Records outside filter's seq window are skipped before json.Unmarshal.
// archiveOverlapsFilter only rules out an archive that ENDS at or below
// AfterSeq, so an order whose cursor sits INSIDE the archive's window still
// opens it — and without this skip every such read decoded the entire archive
// to reach a handful of trailing records.
//
// The skip deliberately does not early-return on BeforeSeq. Archives come from
// a monotonic log and should be seq-ordered, but `continue` saves the same
// decode without depending on that.
func streamArchive(path string, filter Filter, fn func(Event) bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // read-only file

	gr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gunzip: %w", err)
	}
	defer gr.Close() //nolint:errcheck // read-only stream

	scanner := bufio.NewScanner(gr)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if seq, ok := archiveSeq(line); ok {
			if filter.AfterSeq > 0 && seq <= filter.AfterSeq {
				continue
			}
			if filter.BeforeSeq > 0 && seq >= filter.BeforeSeq {
				continue
			}
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		if !fn(e) {
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scanning archive: %w", err)
	}
	return nil
}

// LatestArchivedMatch returns the newest archived event matching filter, and
// whether one was found. Archives are scanned newest-first and the scan stops
// at the first archive holding a match, so the cost is one archive rather than
// the whole retained history.
//
// ReadFiltered cannot answer this question cheaply: it walks archives
// oldest-first, so a Limit stops on the OLDEST match rather than the newest,
// and without a Limit it gunzips and decodes every retained archive. That walk
// grows with every rotation, which is why callers that only need the newest
// match use this instead.
//
// Only archives are searched. Callers that also care about the active log
// should read its tail first, which is the cheap case, and fall back here only
// when the active log holds no match.
func LatestArchivedMatch(path string, filter Filter) (Event, bool, error) {
	dir := filepath.Dir(path)
	archives, err := archiveFilesIn(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return Event{}, false, nil
		}
		return Event{}, false, fmt.Errorf("listing event archives in %q: %w", dir, err)
	}
	// archiveFilesIn sorts ascending by FirstSeq, so descending indexes walk
	// newest archive first.
	for i := len(archives) - 1; i >= 0; i-- {
		info := archives[i]
		if !archiveOverlapsFilter(info, filter) {
			continue
		}
		var (
			newest Event
			found  bool
		)
		// Events within one archive are ordered oldest-first, so the scan runs
		// to the end of this archive and keeps the last match.
		err := streamArchive(filepath.Join(dir, info.Basename), filter, func(e Event) bool {
			if !matchesFilter(e, filter) {
				return true
			}
			newest = e
			found = true
			return true
		})
		if err != nil {
			return Event{}, false, fmt.Errorf("reading archive %q: %w", info.Basename, err)
		}
		if found {
			return newest, true, nil
		}
	}
	return Event{}, false, nil
}

// ReadFilteredTail reads the trailing matching events from path. A positive
// limit returns at most that many events in chronological order; limit <= 0
// falls back to ReadFiltered.
func ReadFilteredTail(path string, filter Filter, limit int) ([]Event, error) {
	if limit <= 0 {
		return ReadFiltered(path, filter)
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading events tail: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat events tail: %w", err)
	}
	return readFilteredTailFromFile(f, info.Size(), filter, limit)
}

func readFilteredTailFromFile(f *os.File, size int64, filter Filter, limit int) ([]Event, error) {
	if size <= 0 {
		return nil, nil
	}
	const chunkSize int64 = 64 * 1024
	var reversed []Event
	var pending []byte
	end := size
	for end > 0 && len(reversed) < limit && (filter.MaxScanBytes <= 0 || size-end < filter.MaxScanBytes) {
		n := chunkSize
		if end < n {
			n = end
		}
		// Clamp the read to what remains of the byte budget so a
		// MaxScanBytes that is not a chunkSize multiple stops the walk
		// mid-chunk rather than overscanning by up to one full chunk.
		// The loop condition guarantees remaining > 0 on entry, and the
		// chunk is read at start = end - n, so a smaller n simply moves
		// the walk's stopping point without misaligning pending.
		if filter.MaxScanBytes > 0 {
			if remaining := filter.MaxScanBytes - (size - end); remaining < n {
				n = remaining
			}
		}
		start := end - n
		chunk := make([]byte, n)
		if _, err := f.ReadAt(chunk, start); err != nil && err != io.EOF {
			return nil, fmt.Errorf("reading events tail: %w", err)
		}
		data := make([]byte, 0, len(chunk)+len(pending))
		data = append(data, chunk...)
		data = append(data, pending...)
		parts := bytes.Split(data, []byte{'\n'})
		firstComplete := 0
		if start > 0 {
			pending = append(pending[:0], parts[0]...)
			firstComplete = 1
		} else {
			pending = nil
		}
		for i := len(parts) - 1; i >= firstComplete && len(reversed) < limit; i-- {
			line := bytes.TrimSuffix(parts[i], []byte{'\r'})
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var e Event
			if err := json.Unmarshal(line, &e); err != nil {
				continue
			}
			if matchesFilter(e, filter) {
				reversed = append(reversed, e)
			}
		}
		end = start
	}
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	return reversed, nil
}

// ReadLatestSeq returns the highest complete event Seq visible in the
// active events file or any canonical sibling archive. Event logs are
// append-only and sequence numbers are monotonic, so the active file
// is read backward from the tail and archives contribute their
// filename-encoded LastSeq without being gunzipped.
func ReadLatestSeq(path string) (uint64, error) {
	seq, err := readLatestActiveSeq(path)
	if err != nil {
		return 0, err
	}
	archives, err := archiveFilesIn(filepath.Dir(path))
	if err == nil {
		for _, info := range archives {
			if info.LastSeq > seq {
				seq = info.LastSeq
			}
		}
	}
	return seq, nil
}

func readLatestActiveSeq(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading latest seq: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file

	info, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat events: %w", err)
	}
	return readLatestSeqFromTail(f, info.Size())
}

func readLatestSeqFromTail(f *os.File, size int64) (uint64, error) {
	if size <= 0 {
		return 0, nil
	}
	const chunkSize int64 = 64 * 1024
	var suffix []byte
	end := size
	first := true
	for end > 0 {
		n := chunkSize
		if end < n {
			n = end
		}
		start := end - n
		chunk := make([]byte, n)
		if _, err := f.ReadAt(chunk, start); err != nil && err != io.EOF {
			return 0, fmt.Errorf("reading latest seq: %w", err)
		}
		data := make([]byte, 0, len(chunk)+len(suffix))
		data = append(data, chunk...)
		data = append(data, suffix...)
		searchEnd := len(data)
		if first && len(data) > 0 && data[len(data)-1] != '\n' {
			idx := bytes.LastIndexByte(data, '\n')
			if idx < 0 {
				suffix = data
				end = start
				first = false
				continue
			}
			searchEnd = idx
		}
		searchStart := 0
		if start > 0 {
			idx := bytes.IndexByte(data, '\n')
			if idx < 0 {
				suffix = data
				end = start
				first = false
				continue
			}
			searchStart = idx + 1
		}
		if seq, ok := latestSeqInCompleteLines(data[searchStart:searchEnd]); ok {
			return seq, nil
		}
		suffix = data
		end = start
		first = false
	}
	return 0, nil
}

func latestSeqInCompleteLines(data []byte) (uint64, bool) {
	for len(data) > 0 {
		idx := bytes.LastIndexByte(data, '\n')
		var line []byte
		if idx >= 0 {
			line = data[idx+1:]
			data = data[:idx]
		} else {
			line = data
			data = nil
		}
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var header struct {
			Seq uint64 `json:"seq"`
		}
		if err := json.Unmarshal(line, &header); err == nil && header.Seq > 0 {
			return header.Seq, true
		}
	}
	return 0, false
}

// ReadFrom reads events starting at the given byte offset in the file.
// Returns the events read, the byte offset after the last complete line,
// and any error. Returns (nil, offset, nil) if no new data is available
// or the file doesn't exist yet. Skips malformed lines (partial writes).
func ReadFrom(path string, offset int64) ([]Event, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, offset, nil
		}
		return nil, offset, fmt.Errorf("reading events: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file

	return readEventsFrom(f, offset)
}

// readEventsFrom scans events from an already-open active log starting at offset,
// returning the decoded events and the offset advanced past every complete line.
// A trailing partial line (no newline) does not advance the offset, so a later
// read re-reads it once the writer completes it. Reading from a caller-supplied
// fd (rather than re-opening by path) lets a tailer pin the file identity across
// a concurrent rotation.
func readEventsFrom(f *os.File, offset int64) ([]Event, int64, error) {
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, fmt.Errorf("seeking events: %w", err)
	}

	var result []Event
	r := bufio.NewReader(f)
	bytesRead := int64(0)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			if line[len(line)-1] == '\n' {
				// Complete line — safe to advance offset past it.
				bytesRead += int64(len(line))
				trimmed := line[:len(line)-1]
				if len(trimmed) > 0 && trimmed[len(trimmed)-1] == '\r' {
					trimmed = trimmed[:len(trimmed)-1]
				}
				var e Event
				if jsonErr := json.Unmarshal(trimmed, &e); jsonErr == nil {
					result = append(result, e)
				}
				// skip malformed lines (partial writes)
			}
			// Partial line (no trailing \n): don't advance offset.
			// The next ReadFrom call will re-read it once complete.
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return result, offset + bytesRead, fmt.Errorf("scanning events: %w", err)
		}
	}
	return result, offset + bytesRead, nil
}
