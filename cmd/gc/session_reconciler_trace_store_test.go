package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"testing"
	"time"
)

// failingTraceSegmentFile writes only the first cut(p) bytes of the next
// Write and then fails it, the way a write that hits ENOSPC midway does.
// truncateErr, when set, makes Truncate fail too.
type failingTraceSegmentFile struct {
	*os.File
	cut         func(p []byte) int
	truncateErr error
	failed      bool
}

func (f *failingTraceSegmentFile) Write(p []byte) (int, error) {
	if f.failed {
		return f.File.Write(p)
	}
	f.failed = true
	n := min(f.cut(p), len(p))
	written, err := f.File.Write(p[:n])
	if err != nil {
		return written, err
	}
	return written, syscall.ENOSPC
}

func (f *failingTraceSegmentFile) Truncate(size int64) error {
	if f.truncateErr != nil {
		return f.truncateErr
	}
	return f.File.Truncate(size)
}

func traceStoreTestRecord(traceID string, ts time.Time) SessionReconcilerTraceRecord {
	return SessionReconcilerTraceRecord{
		TraceSchemaVersion: sessionReconcilerTraceSchemaVersion,
		TraceID:            traceID,
		TickID:             traceID + "-tick",
		RecordType:         TraceRecordDecision,
		Template:           "repo/worker",
		SessionName:        "worker-1",
		SiteCode:           TraceSiteReconcilerWakeDecision,
		TraceMode:          TraceModeDetail,
		TraceSource:        TraceSourceManual,
		Ts:                 ts,
	}
}

func traceStoreTestRoot(cityDir string) string {
	return filepath.Join(cityDir, ".gc", "runtime", sessionReconcilerTraceRootDir)
}

// injectTraceWriteFailure makes the store's next segment write stop after
// cut(p) bytes and fail with ENOSPC.
func injectTraceWriteFailure(t *testing.T, store *SessionReconcilerTraceStore, cut func(p []byte) int, truncateErr error) {
	t.Helper()
	file, ok := store.currentFile.(*os.File)
	if !ok {
		t.Fatalf("current segment file = %T, want *os.File", store.currentFile)
	}
	store.currentFile = &failingTraceSegmentFile{File: file, cut: cut, truncateErr: truncateErr}
}

var traceFailedWriteCuts = []struct {
	name string
	cut  func(p []byte) int
}{
	{"torn inside a record", func([]byte) int { return 40 }},
	{"torn after a complete record line", func(p []byte) int { return bytes.IndexByte(p, '\n') + 1 }},
	{"commit line without its newline", func(p []byte) int { return len(p) - 1 }},
	{"nothing written", func([]byte) int { return 0 }},
}

// failedTraceAppend is the state a failed, rolled-back AppendBatch leaves.
type failedTraceAppend struct {
	store     *SessionReconcilerTraceStore
	root      string
	segPath   string
	before    []byte
	seqBefore uint64
	appendErr error
}

// failTraceAppendOnce appends batch cycle-a (unless fresh), then makes the
// next AppendBatch (two cycle-b records) fail after cut(p) bytes. With fresh
// set, the failure is the first write into a brand-new segment.
func failTraceAppendOnce(t *testing.T, fresh bool, durability TraceDurabilityTier, cut func(p []byte) int) failedTraceAppend {
	t.Helper()
	cityDir := t.TempDir()
	store, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if fresh {
		path, err := store.currentSegmentPath(now)
		if err != nil {
			t.Fatalf("currentSegmentPath: %v", err)
		}
		if err := store.openSegment(path, -1, -1); err != nil {
			t.Fatalf("openSegment: %v", err)
		}
	} else if err := store.AppendBatch([]SessionReconcilerTraceRecord{traceStoreTestRecord("cycle-a", now)}, durability); err != nil {
		t.Fatalf("append batch a: %v", err)
	}
	segPath := store.currentPath
	before, err := os.ReadFile(segPath)
	if err != nil {
		t.Fatalf("read segment: %v", err)
	}
	seqBefore, err := store.LatestSeq()
	if err != nil {
		t.Fatalf("LatestSeq: %v", err)
	}
	injectTraceWriteFailure(t, store, cut, nil)
	failed := []SessionReconcilerTraceRecord{
		traceStoreTestRecord("cycle-b", now.Add(time.Second)),
		traceStoreTestRecord("cycle-b", now.Add(time.Second)),
	}
	appendErr := store.AppendBatch(failed, durability)
	return failedTraceAppend{
		store:     store,
		root:      traceStoreTestRoot(cityDir),
		segPath:   segPath,
		before:    before,
		seqBefore: seqBefore,
		appendErr: appendErr,
	}
}

// forEachFailedTraceAppend runs fn for every cut point, durability tier, and
// for both a segment holding a committed batch and a brand-new segment.
func forEachFailedTraceAppend(t *testing.T, fn func(t *testing.T, fresh bool, durability TraceDurabilityTier, cut func(p []byte) int)) {
	for _, tc := range traceFailedWriteCuts {
		for _, durability := range []TraceDurabilityTier{TraceDurabilityDurable, TraceDurabilityMetadata} {
			for _, fresh := range []bool{false, true} {
				name := tc.name + "/" + string(durability) + "/existing segment"
				if fresh {
					name = tc.name + "/" + string(durability) + "/fresh segment"
				}
				t.Run(name, func(t *testing.T) {
					fn(t, fresh, durability, tc.cut)
				})
			}
		}
	}
}

func TestTraceStoreFailedWriteRollsBackPartialBatch(t *testing.T) {
	forEachFailedTraceAppend(t, func(t *testing.T, fresh bool, durability TraceDurabilityTier, cut func(p []byte) int) {
		got := failTraceAppendOnce(t, fresh, durability, cut)
		if !errors.Is(got.appendErr, syscall.ENOSPC) {
			t.Fatalf("append batch b error = %v, want ENOSPC", got.appendErr)
		}
		after, err := os.ReadFile(got.segPath)
		if err != nil {
			t.Fatalf("read segment: %v", err)
		}
		if !bytes.Equal(after, got.before) {
			t.Fatalf("segment after failed write:\n%s\nwant it unchanged:\n%s", after, got.before)
		}
		seqAfter, err := got.store.LatestSeq()
		if err != nil {
			t.Fatalf("LatestSeq: %v", err)
		}
		if seqAfter != got.seqBefore {
			t.Fatalf("LatestSeq after failed write = %d, want %d", seqAfter, got.seqBefore)
		}
	})
}

func traceStoreTestSegments(t *testing.T, root string) []string {
	t.Helper()
	segments, err := filepath.Glob(filepath.Join(root, sessionReconcilerTraceSegments, "*", "*", "*", "*.jsonl"))
	if err != nil {
		t.Fatalf("glob segments: %v", err)
	}
	sort.Strings(segments)
	return segments
}

// assertTraceSeqsUnique fails when two records share a seq.
func assertTraceSeqsUnique(t *testing.T, records []SessionReconcilerTraceRecord) {
	t.Helper()
	seen := make(map[uint64]SessionReconcilerTraceRecord)
	for _, rec := range records {
		if prev, ok := seen[rec.Seq]; ok {
			t.Fatalf("seq %d reused: %s %s and %s %s", rec.Seq, prev.TraceID, prev.RecordType, rec.TraceID, rec.RecordType)
		}
		seen[rec.Seq] = rec
	}
}

// traceRecordCounts counts the records of each trace id.
func traceRecordCounts(records []SessionReconcilerTraceRecord) map[string]int {
	counts := make(map[string]int)
	for _, rec := range records {
		counts[rec.TraceID]++
	}
	return counts
}

func TestTraceStoreAppendAfterRolledBackWriteContinuesSegment(t *testing.T) {
	forEachFailedTraceAppend(t, func(t *testing.T, fresh bool, durability TraceDurabilityTier, cut func(p []byte) int) {
		got := failTraceAppendOnce(t, fresh, durability, cut)
		if got.appendErr == nil {
			t.Fatal("append batch b: want a write error, got nil")
		}
		next := traceStoreTestRecord("cycle-c", time.Now().UTC().Add(2*time.Second))
		if err := got.store.AppendBatch([]SessionReconcilerTraceRecord{next}, durability); err != nil {
			t.Fatalf("append batch c: %v", err)
		}

		segments := traceStoreTestSegments(t, got.root)
		if len(segments) != 1 || segments[0] != got.segPath {
			t.Fatalf("segments = %v, want only %s (the rolled-back segment keeps being used)", segments, got.segPath)
		}
		after, err := os.ReadFile(got.segPath)
		if err != nil {
			t.Fatalf("read segment: %v", err)
		}
		if !bytes.HasPrefix(after, got.before) || !bytes.HasPrefix(after[len(got.before):], []byte(`{"`)) {
			t.Fatalf("batch c does not start on its own line after the committed bytes:\n%s", after)
		}

		records, err := ReadTraceRecords(got.root, TraceFilter{})
		if err != nil {
			t.Fatalf("ReadTraceRecords: %v", err)
		}
		assertTraceSeqsUnique(t, records)
		counts := traceRecordCounts(records)
		wantA := 2
		if fresh {
			wantA = 0
		}
		if counts["cycle-a"] != wantA || counts["cycle-c"] != 2 || counts["cycle-b"] != 0 {
			t.Fatalf("records by trace = %v, want cycle-a=%d cycle-c=2 and no cycle-b", counts, wantA)
		}
		for _, rec := range records {
			if rec.TraceID == "cycle-c" && rec.RecordType == TraceRecordBatchCommit && rec.FirstSeq != got.seqBefore+1 {
				t.Fatalf("batch c first_seq = %d, want %d", rec.FirstSeq, got.seqBefore+1)
			}
		}
	})
}

func TestTraceStoreFailedRollbackAbandonsSegment(t *testing.T) {
	for _, reopen := range []bool{false, true} {
		name := "same store"
		if reopen {
			name = "store reopened after the failure"
		}
		t.Run(name, func(t *testing.T) {
			cityDir := t.TempDir()
			store, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
			if err != nil {
				t.Fatalf("new store: %v", err)
			}
			defer func() { _ = store.Close() }()

			now := time.Now().UTC()
			if err := store.AppendBatch([]SessionReconcilerTraceRecord{traceStoreTestRecord("cycle-a", now)}, TraceDurabilityDurable); err != nil {
				t.Fatalf("append batch a: %v", err)
			}
			seqBefore, err := store.LatestSeq()
			if err != nil {
				t.Fatalf("LatestSeq: %v", err)
			}
			abandoned := store.currentPath
			// Fail after the first record line and partway into the second,
			// so a complete uncommitted line and a torn line stay on disk.
			failed := []SessionReconcilerTraceRecord{
				traceStoreTestRecord("cycle-b", now.Add(time.Second)),
				traceStoreTestRecord("cycle-b", now.Add(time.Second)),
			}
			failedCommitSeq := seqBefore + uint64(len(failed)) + 1
			cutInSecondLine := func(p []byte) int { return bytes.IndexByte(p, '\n') + 1 + 20 }
			injectTraceWriteFailure(t, store, cutInSecondLine, errors.New("truncate refused"))
			if err := store.AppendBatch(failed, TraceDurabilityDurable); !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("append batch b error = %v, want ENOSPC", err)
			}
			if reopen {
				if err := store.Close(); err != nil {
					t.Fatalf("close: %v", err)
				}
				store, err = newSessionReconcilerTraceStore(cityDir, io.Discard)
				if err != nil {
					t.Fatalf("reopen store: %v", err)
				}
			}
			next := []SessionReconcilerTraceRecord{traceStoreTestRecord("cycle-c", now.Add(2*time.Second))}
			if err := store.AppendBatch(next, TraceDurabilityDurable); err != nil {
				t.Fatalf("append batch c: %v", err)
			}

			root := traceStoreTestRoot(cityDir)
			segments := traceStoreTestSegments(t, root)
			if len(segments) != 2 || segments[0] != abandoned {
				t.Fatalf("segments = %v, want %s plus a newer one", segments, abandoned)
			}
			fresh, err := os.ReadFile(segments[1])
			if err != nil {
				t.Fatalf("read fresh segment: %v", err)
			}
			if !bytes.Contains(fresh, []byte("cycle-c")) || bytes.Contains(fresh, []byte("cycle-b")) {
				t.Fatalf("segment %s should hold batch c and nothing of batch b:\n%s", segments[1], fresh)
			}

			records, err := ReadTraceRecords(root, TraceFilter{})
			if err != nil {
				t.Fatalf("ReadTraceRecords: %v", err)
			}
			assertTraceSeqsUnique(t, records)
			counts := traceRecordCounts(records)
			if counts["cycle-a"] != 2 || counts["cycle-c"] != 2 || counts["cycle-b"] != 0 {
				t.Fatalf("records by trace = %v, want cycle-a=2 cycle-c=2 and no cycle-b", counts)
			}
			for _, rec := range records {
				if rec.TraceID == "cycle-c" && rec.Seq <= failedCommitSeq {
					t.Fatalf("batch c seq %d does not pass the failed batch's commit seq %d", rec.Seq, failedCommitSeq)
				}
			}
		})
	}
}

// traceTestBatchLines renders a committed batch the way AppendBatch writes
// it: n record lines with seqs from firstSeq, then the batch_commit line.
func traceTestBatchLines(t *testing.T, traceID string, firstSeq uint64, n int) [][]byte {
	t.Helper()
	ts := time.Date(2026, 10, 15, 2, 38, 20, 0, time.UTC).Add(time.Duration(firstSeq) * time.Second)
	marshal := func(rec SessionReconcilerTraceRecord) []byte {
		line, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal trace record: %v", err)
		}
		return append(line, '\n')
	}
	var lines [][]byte
	var crc uint32
	seq := firstSeq
	for i := 0; i < n; i++ {
		rec := traceStoreTestRecord(traceID, ts)
		rec.Seq = seq
		rec.RecordID = stableTraceRecordID(traceID, seq, i)
		line := marshal(rec)
		crc = crc32.Update(crc, crc32.IEEETable, bytes.TrimSpace(line))
		lines = append(lines, line)
		seq++
	}
	commit := newTraceRecord(TraceRecordBatchCommit)
	commit.TraceID = traceID
	commit.TickID = traceID + "-tick"
	commit.RecordID = stableTraceRecordID(traceID, seq, n)
	commit.Ts = ts
	commit.Seq = seq
	commit.FirstSeq = firstSeq
	commit.LastSeq = seq - 1
	commit.RecordCount = n
	commit.DurabilityTier = TraceDurabilityDurable
	commit.BatchCRC32 = crc
	commit.TraceMode = TraceModeBaseline
	commit.TraceSource = TraceSourceAlwaysOn
	return append(lines, marshal(commit))
}

// legacyPoisonedTraceSegment builds the shape issue #7954 reports: committed
// batch a (seqs 1-3); batch b (seqs 4-6) cut inside its second record; batch
// c reusing b's seqs with its first line glued onto the torn bytes; then, if
// tail is set, committed batches d (7-9) and e (10-11). gluedRecords is the
// size of batch c.
func legacyPoisonedTraceSegment(t *testing.T, gluedRecords int, tail bool) []byte {
	t.Helper()
	a := traceTestBatchLines(t, "cycle-a", 1, 2)
	b := traceTestBatchLines(t, "cycle-b", 4, 2)
	c := traceTestBatchLines(t, "cycle-c", 4, gluedRecords)
	var data []byte
	data = append(data, bytes.Join(a, nil)...)
	data = append(data, b[0]...)
	data = append(data, b[1][:30]...)
	data = append(data, bytes.Join(c, nil)...)
	if tail {
		data = append(data, bytes.Join(traceTestBatchLines(t, "cycle-d", 7, 2), nil)...)
		data = append(data, bytes.Join(traceTestBatchLines(t, "cycle-e", 10, 1), nil)...)
	}
	return data
}

// writeTraceSegmentFile writes data as segment-000001 of today's day dir
// under root and returns its path.
func writeTraceSegmentFile(t *testing.T, root string, data []byte) string {
	t.Helper()
	dayDir := traceDayDir(filepath.Join(root, sessionReconcilerTraceSegments), time.Now().UTC())
	if err := os.MkdirAll(dayDir, sessionReconcilerTraceOwnerDirPerm); err != nil {
		t.Fatalf("mkdir day dir: %v", err)
	}
	path := filepath.Join(dayDir, traceSegmentFileName(1))
	if err := os.WriteFile(path, data, sessionReconcilerTraceOwnerFilePerm); err != nil {
		t.Fatalf("write segment: %v", err)
	}
	return path
}

func TestTraceReaderSkipsTornBatchGluedToNextBatch(t *testing.T) {
	cases := []struct {
		name         string
		gluedRecords int
	}{
		{"glued batch of two records", 2},
		// The glued line swallows the only record, so the commit sees none.
		{"glued batch of one record", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeTraceSegmentFile(t, root, legacyPoisonedTraceSegment(t, tc.gluedRecords, true))

			records, err := ReadTraceRecords(root, TraceFilter{})
			if err != nil {
				t.Fatalf("ReadTraceRecords: %v", err)
			}
			assertTraceSeqsUnique(t, records)
			want := map[string]int{"cycle-a": 3, "cycle-d": 3, "cycle-e": 2}
			if got := traceRecordCounts(records); len(got) != len(want) || got["cycle-a"] != 3 || got["cycle-d"] != 3 || got["cycle-e"] != 2 {
				t.Fatalf("records by trace = %v, want %v", got, want)
			}

			records, err = ReadTraceRecords(root, TraceFilter{SeqAfter: 3})
			if err != nil {
				t.Fatalf("ReadTraceRecords SeqAfter=3: %v", err)
			}
			want = map[string]int{"cycle-d": 3, "cycle-e": 2}
			if got := traceRecordCounts(records); len(got) != len(want) || got["cycle-d"] != 3 || got["cycle-e"] != 2 {
				t.Fatalf("records after seq 3 by trace = %v, want %v", got, want)
			}
		})
	}
}

func TestTraceRecoveryKeepsTornSegment(t *testing.T) {
	cases := []struct {
		name       string
		tail       bool
		wantLatest uint64
	}{
		{"committed batches after the glued batch", true, 11},
		{"glued batch last in the segment", false, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityDir := t.TempDir()
			root := traceStoreTestRoot(cityDir)
			segPath := writeTraceSegmentFile(t, root, legacyPoisonedTraceSegment(t, 2, tc.tail))

			store, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
			if err != nil {
				t.Fatalf("new store: %v", err)
			}
			defer store.Close() //nolint:errcheck
			quarantined, err := filepath.Glob(filepath.Join(root, sessionReconcilerTraceQuarantine, "*"))
			if err != nil {
				t.Fatalf("glob quarantine: %v", err)
			}
			if len(quarantined) != 0 {
				t.Fatalf("quarantined = %v, want none for a torn batch", quarantined)
			}
			if _, err := os.Stat(segPath); err != nil {
				t.Fatalf("segment %s should stay in segments/: %v", segPath, err)
			}
			latest, err := store.LatestSeq()
			if err != nil {
				t.Fatalf("LatestSeq: %v", err)
			}
			if latest != tc.wantLatest {
				t.Fatalf("LatestSeq = %d, want %d (highest verified commit)", latest, tc.wantLatest)
			}

			if err := store.AppendBatch([]SessionReconcilerTraceRecord{traceStoreTestRecord("cycle-f", time.Now().UTC())}, TraceDurabilityDurable); err != nil {
				t.Fatalf("append batch f: %v", err)
			}
			segments := traceStoreTestSegments(t, root)
			if len(segments) != 2 || segments[0] != segPath {
				t.Fatalf("segments = %v, want %s plus a fresh one", segments, segPath)
			}
			records, err := ReadTraceRecords(root, TraceFilter{TraceID: "cycle-f", RecordType: TraceRecordBatchCommit})
			if err != nil {
				t.Fatalf("ReadTraceRecords: %v", err)
			}
			if len(records) != 1 || records[0].FirstSeq != tc.wantLatest+1 {
				t.Fatalf("batch f commits = %+v, want one with first_seq %d", records, tc.wantLatest+1)
			}
		})
	}
}

func TestTraceStoreRestartAfterTornTailUsesFreshSegment(t *testing.T) {
	cityDir := t.TempDir()
	store, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	now := time.Now().UTC()
	if err := store.AppendBatch([]SessionReconcilerTraceRecord{traceStoreTestRecord("cycle-a", now)}, TraceDurabilityDurable); err != nil {
		t.Fatalf("append batch a: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	root := traceStoreTestRoot(cityDir)
	segments := traceStoreTestSegments(t, root)
	if len(segments) != 1 {
		t.Fatalf("segments = %d, want 1", len(segments))
	}
	// A crash mid-write leaves a record cut off mid-line.
	f, err := os.OpenFile(segments[0], os.O_APPEND|os.O_WRONLY, sessionReconcilerTraceOwnerFilePerm)
	if err != nil {
		t.Fatalf("open segment: %v", err)
	}
	if _, err := f.WriteString(`{"trace_schema_version":1,"seq":3,"trace_id":"cycle-torn","ts":"2026-1`); err != nil {
		f.Close() //nolint:errcheck
		t.Fatalf("write torn tail: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close segment: %v", err)
	}
	torn, err := os.ReadFile(segments[0])
	if err != nil {
		t.Fatalf("read torn segment: %v", err)
	}

	reopened, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close() //nolint:errcheck
	if err := reopened.AppendBatch([]SessionReconcilerTraceRecord{traceStoreTestRecord("cycle-b", now.Add(time.Second))}, TraceDurabilityDurable); err != nil {
		t.Fatalf("append batch b: %v", err)
	}
	after, err := os.ReadFile(segments[0])
	if err != nil {
		t.Fatalf("read torn segment after append: %v", err)
	}
	if !bytes.Equal(after, torn) {
		t.Fatalf("batch b was appended onto the torn segment:\n%s", after)
	}
	if got := traceStoreTestSegments(t, root); len(got) != 2 {
		t.Fatalf("segments = %v, want the torn one plus a fresh one", got)
	}
	quarantined, err := filepath.Glob(filepath.Join(root, sessionReconcilerTraceQuarantine, "*"))
	if err != nil {
		t.Fatalf("glob quarantine: %v", err)
	}
	if len(quarantined) != 0 {
		t.Fatalf("quarantined = %v, want none for a torn tail", quarantined)
	}
	records, err := ReadTraceRecords(root, TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	assertTraceSeqsUnique(t, records)
	if counts := traceRecordCounts(records); counts["cycle-a"] != 2 || counts["cycle-b"] != 2 || len(counts) != 2 {
		t.Fatalf("records by trace = %v, want cycle-a=2 and cycle-b=2", counts)
	}
}

func TestTraceReaderRejectsCorruptCommittedBatch(t *testing.T) {
	join := func(parts ...[][]byte) []byte {
		var data []byte
		for _, p := range parts {
			data = append(data, bytes.Join(p, nil)...)
		}
		return data
	}
	crcBad := func(t *testing.T, traceID string, firstSeq uint64) [][]byte {
		lines := traceTestBatchLines(t, traceID, firstSeq, 1)
		// Still parses, but the batch CRC no longer matches.
		lines[0] = bytes.Replace(lines[0], []byte(`"worker-1"`), []byte(`"worker-2"`), 1)
		return lines
	}
	cases := []struct {
		name    string
		segment func(t *testing.T) []byte
	}{
		{"crc mismatch", func(t *testing.T) []byte {
			return join(traceTestBatchLines(t, "cycle-a", 1, 1), crcBad(t, "cycle-b", 3))
		}},
		{"record count mismatch", func(t *testing.T) []byte {
			b := traceTestBatchLines(t, "cycle-b", 3, 2)
			return join(traceTestBatchLines(t, "cycle-a", 1, 1), [][]byte{b[1], b[2]})
		}},
		{"crc mismatch after a verified commit closed the resync window", func(t *testing.T) []byte {
			return join([][]byte{legacyPoisonedTraceSegment(t, 2, false)}, traceTestBatchLines(t, "cycle-d", 7, 1), crcBad(t, "cycle-e", 9))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityDir := t.TempDir()
			root := traceStoreTestRoot(cityDir)
			segPath := writeTraceSegmentFile(t, root, tc.segment(t))

			if _, err := ReadTraceRecords(root, TraceFilter{}); err == nil {
				t.Fatal("ReadTraceRecords succeeded on a committed batch that does not verify, want an error")
			}
			store, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
			if err != nil {
				t.Fatalf("new store: %v", err)
			}
			defer store.Close() //nolint:errcheck
			if _, err := os.Stat(segPath); !os.IsNotExist(err) {
				t.Fatalf("segment %s should be quarantined, stat err = %v", segPath, err)
			}
			quarantined, err := filepath.Glob(filepath.Join(root, sessionReconcilerTraceQuarantine, "*"))
			if err != nil {
				t.Fatalf("glob quarantine: %v", err)
			}
			if len(quarantined) == 0 {
				t.Fatal("quarantine is empty, want the corrupt segment")
			}
		})
	}
}
