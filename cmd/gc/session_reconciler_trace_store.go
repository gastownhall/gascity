package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
)

const (
	sessionReconcilerTraceMaxSegmentBytes  = 16 << 20
	sessionReconcilerTraceMaxBatches       = 512
	sessionReconcilerTraceOwnerDirPerm     = 0o700
	sessionReconcilerTraceOwnerFilePerm    = 0o600
	sessionReconcilerTraceLowSpaceMinFree  = 128 << 20
	sessionReconcilerTraceLowSpaceExitFree = 256 << 20
	sessionReconcilerTraceMaxTotalBytes    = 1 << 30
	sessionReconcilerTraceMaxAge           = 7 * 24 * time.Hour
	sessionReconcilerTracePruneInterval    = 5 * time.Minute
)

type SessionReconcilerTraceStore struct {
	mu             sync.Mutex
	cityPath       string
	rootDir        string
	stderr         io.Writer
	seq            uint64
	currentDay     string
	currentSegment int
	currentPath    string
	currentFile    *os.File
	currentBytes   int64
	currentBatches int
	disabled       bool
	lowSpace       bool
	lastPrune      time.Time
}

type sessionReconcilerTraceHead struct {
	SchemaVersion  int       `json:"schema_version"`
	Seq            uint64    `json:"seq"`
	CurrentPath    string    `json:"current_path,omitempty"`
	CurrentBytes   int64     `json:"current_bytes,omitempty"`
	CurrentBatches int       `json:"current_batches,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func newSessionReconcilerTraceStore(cityPath string, stderr io.Writer) (*SessionReconcilerTraceStore, error) {
	rootDir := filepath.Join(citylayout.RuntimeDataDir(cityPath), sessionReconcilerTraceRootDir)
	store := &SessionReconcilerTraceStore{
		cityPath: cityPath,
		rootDir:  rootDir,
		stderr:   stderr,
	}
	if err := store.ensureRoot(); err != nil {
		return nil, err
	}
	if ok, err := store.recoverFromHead(); err != nil {
		return nil, err
	} else if ok {
		return store, nil
	}
	if err := store.recoverExisting(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *SessionReconcilerTraceStore) ensureRoot() error {
	if s == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(s.rootDir, sessionReconcilerTraceSegments), sessionReconcilerTraceOwnerDirPerm); err != nil {
		return fmt.Errorf("creating trace root: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(s.rootDir, sessionReconcilerTraceQuarantine), sessionReconcilerTraceOwnerDirPerm); err != nil {
		return fmt.Errorf("creating trace quarantine: %w", err)
	}
	_ = os.Chmod(s.rootDir, sessionReconcilerTraceOwnerDirPerm)
	return nil
}

func (s *SessionReconcilerTraceStore) recoverExisting() error {
	paths, err := filepath.Glob(filepath.Join(s.rootDir, sessionReconcilerTraceSegments, "*", "*", "*", "*.jsonl"))
	if err != nil {
		return fmt.Errorf("glob trace segments: %w", err)
	}
	sort.Strings(paths)
	var maxSeq uint64
	for _, path := range paths {
		seq, ok, scanErr := scanTraceSegment(path)
		if scanErr != nil {
			if quarantineErr := s.quarantine(path, scanErr); quarantineErr != nil && s.stderr != nil {
				fmt.Fprintf(s.stderr, "trace: quarantine %s: %v\n", path, quarantineErr) //nolint:errcheck
			}
			continue
		}
		if ok && seq > maxSeq {
			maxSeq = seq
		}
	}
	s.seq = maxSeq
	if err := s.saveHeadLocked(); err != nil && s.stderr != nil {
		fmt.Fprintf(s.stderr, "trace: save head: %v\n", err) //nolint:errcheck
	}
	return nil
}

func scanTraceSegment(path string) (maxSeq uint64, ok bool, err error) {
	_, maxSeq, err = readTraceRecordsFile(path, TraceFilter{}, true)
	if err != nil {
		return 0, false, err
	}
	return maxSeq, true, nil
}

func (s *SessionReconcilerTraceStore) openSegment(path string, knownBytes int64, knownBatches int) error {
	if s.currentFile != nil && s.currentPath == path {
		return nil
	}
	if s.currentFile != nil {
		_ = s.currentFile.Close()
	}
	_, statErr := os.Stat(path)
	newFile := os.IsNotExist(statErr)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, sessionReconcilerTraceOwnerFilePerm)
	if err != nil {
		return err
	}
	fi, err := file.Stat()
	if err != nil {
		file.Close() //nolint:errcheck
		return err
	}
	s.currentFile = file
	s.currentPath = path
	s.currentDay = filepath.Dir(path)
	s.currentSegment = 0
	if name := filepath.Base(path); strings.HasPrefix(name, "segment-") && strings.HasSuffix(name, ".jsonl") {
		if idx, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "segment-"), ".jsonl")); err == nil {
			s.currentSegment = idx
		}
	}
	s.currentBytes = fi.Size()
	if knownBytes == fi.Size() && knownBatches >= 0 {
		s.currentBatches = knownBatches
	} else {
		s.currentBatches = countBatchesInSegment(path)
	}
	if newFile {
		if err := syncTraceAncestors(filepath.Dir(path), filepath.Join(s.rootDir, sessionReconcilerTraceSegments)); err != nil && s.stderr != nil {
			fmt.Fprintf(s.stderr, "trace: sync segment dir: %v\n", err) //nolint:errcheck
		}
	}
	return nil
}

func countBatchesInSegment(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close() //nolint:errcheck
	reader := bufio.NewReader(f)
	count := 0
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) == 0 && readErr == io.EOF {
			break
		}
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				return count
			}
			continue
		}
		var rec SessionReconcilerTraceRecord
		if err := json.Unmarshal(trimmed, &rec); err != nil {
			return count
		}
		if rec.RecordType == TraceRecordBatchCommit {
			count++
		}
		if readErr == io.EOF {
			break
		}
	}
	return count
}

func (s *SessionReconcilerTraceStore) recoverFromHead() (bool, error) {
	head, err := s.loadHead()
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if head.CurrentPath == "" {
		s.seq = head.Seq
		return true, nil
	}
	path := filepath.Join(s.rootDir, filepath.FromSlash(head.CurrentPath))
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if info.Size() != head.CurrentBytes {
		return false, nil
	}
	s.seq = head.Seq
	return true, nil
}

func (s *SessionReconcilerTraceStore) loadHead() (sessionReconcilerTraceHead, error) {
	path := filepath.Join(s.rootDir, sessionReconcilerTraceHeadFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return sessionReconcilerTraceHead{}, err
	}
	var head sessionReconcilerTraceHead
	if err := json.Unmarshal(data, &head); err != nil {
		return sessionReconcilerTraceHead{}, err
	}
	if head.SchemaVersion == 0 {
		head.SchemaVersion = sessionReconcilerTraceSchemaVersion
	}
	return head, nil
}

func (s *SessionReconcilerTraceStore) saveHeadLocked() error {
	if s == nil {
		return nil
	}
	head := sessionReconcilerTraceHead{
		SchemaVersion:  sessionReconcilerTraceSchemaVersion,
		Seq:            s.seq,
		CurrentBytes:   s.currentBytes,
		CurrentBatches: s.currentBatches,
		UpdatedAt:      time.Now().UTC(),
	}
	if s.currentPath != "" {
		if rel, err := filepath.Rel(s.rootDir, s.currentPath); err == nil {
			head.CurrentPath = filepath.ToSlash(rel)
		}
	}
	data, err := json.MarshalIndent(head, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(s.rootDir, sessionReconcilerTraceHeadFile)
	if err := fsys.WriteFileAtomic(fsys.OSFS{}, path, data, sessionReconcilerTraceOwnerFilePerm); err != nil {
		return err
	}
	return os.Chmod(path, sessionReconcilerTraceOwnerFilePerm)
}

func syncTraceAncestors(dir, stop string) error {
	dir = filepath.Clean(dir)
	stop = filepath.Clean(stop)
	for {
		if err := syncDir(dir); err != nil {
			return err
		}
		if dir == stop {
			return nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck
	return f.Sync()
}

func (s *SessionReconcilerTraceStore) currentSegmentPath(now time.Time) (string, error) {
	dayDir := traceDayDir(filepath.Join(s.rootDir, sessionReconcilerTraceSegments), now)
	if err := os.MkdirAll(dayDir, sessionReconcilerTraceOwnerDirPerm); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(dayDir)
	if err != nil {
		return "", err
	}
	maxIdx := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		if strings.HasPrefix(name, "segment-") {
			if n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "segment-"), ".jsonl")); err == nil && n > maxIdx {
				maxIdx = n
			}
		}
	}
	if s.currentPath != "" {
		curDay := filepath.Base(filepath.Dir(s.currentPath))
		if filepath.Dir(filepath.Dir(s.currentPath)) == dayDir {
			return s.currentPath, nil
		}
		if curDay == filepath.Base(dayDir) && s.currentDay == dayDir {
			return s.currentPath, nil
		}
	}
	nextIdx := maxIdx + 1
	s.currentDay = dayDir
	s.currentSegment = nextIdx
	return filepath.Join(dayDir, traceSegmentFileName(nextIdx)), nil
}

func (s *SessionReconcilerTraceStore) AppendBatch(records []SessionReconcilerTraceRecord, durability TraceDurabilityTier) error {
	if s == nil || len(records) == 0 || s.disabled {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	if err := s.ensureRoot(); err != nil {
		return err
	}
	if low, _ := s.isLowSpace(); low {
		s.lowSpace = true
		durability = TraceDurabilityMetadata
	} else {
		s.lowSpace = false
	}
	path, err := s.currentSegmentPath(now)
	if err != nil {
		return err
	}
	if s.currentFile == nil || s.currentPath != path {
		if err := s.openSegment(path, -1, -1); err != nil {
			return err
		}
	}
	if s.currentBytes >= sessionReconcilerTraceMaxSegmentBytes || s.currentBatches >= sessionReconcilerTraceMaxBatches {
		if err := s.rotateSegment(now); err != nil {
			return err
		}
	}
	if s.lowSpace {
		durability = TraceDurabilityMetadata
	}
	firstSeq := s.seq + 1
	seq := firstSeq
	var batchCRC uint32
	var writeBuf []byte
	for i := range records {
		rec := records[i].clone()
		rec.Seq = seq
		if rec.Ts.IsZero() {
			rec.Ts = now
		}
		if rec.RecordID == "" {
			rec.RecordID = stableTraceRecordID(rec.TraceID, rec.Seq, i)
		}
		line, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		line = append(line, '\n')
		writeBuf = append(writeBuf, line...)
		batchCRC = crc32.Update(batchCRC, crc32.IEEETable, bytes.TrimSpace(line))
		seq++
	}
	commit := newTraceRecord(TraceRecordBatchCommit)
	commit.TraceID = records[0].TraceID
	commit.TickID = records[0].TickID
	commit.RecordID = stableTraceRecordID(records[0].TraceID, seq, len(records))
	commit.Ts = now
	commit.Seq = seq
	commit.FirstSeq = firstSeq
	commit.LastSeq = seq - 1
	commit.RecordCount = len(records)
	commit.DurabilityTier = durability
	commit.BatchCRC32 = batchCRC
	commit.TraceMode = TraceModeBaseline
	commit.TraceSource = TraceSourceAlwaysOn
	commitLine, err := json.Marshal(commit)
	if err != nil {
		return err
	}
	commitLine = append(commitLine, '\n')
	writeBuf = append(writeBuf, commitLine...)

	if _, err := s.currentFile.Write(writeBuf); err != nil {
		return err
	}
	s.seq = seq
	s.currentBytes += int64(len(writeBuf))
	s.currentBatches++
	var syncErr error
	if durability == TraceDurabilityDurable {
		syncErr = s.currentFile.Sync()
	}
	if err := s.saveHeadLocked(); err != nil && s.stderr != nil {
		fmt.Fprintf(s.stderr, "trace: save head: %v\n", err) //nolint:errcheck
	}
	if pruneErr := s.maybePruneOldSegments(now); pruneErr != nil && s.stderr != nil {
		fmt.Fprintf(s.stderr, "trace: prune: %v\n", pruneErr) //nolint:errcheck
	}
	return syncErr
}

func (s *SessionReconcilerTraceStore) rotateSegment(now time.Time) error {
	if s.currentFile != nil {
		_ = s.currentFile.Close()
		s.currentFile = nil
	}
	s.currentPath = ""
	s.currentBytes = 0
	s.currentBatches = 0
	path, err := s.currentSegmentPath(now)
	if err != nil {
		return err
	}
	return s.openSegment(path, -1, -1)
}

func (s *SessionReconcilerTraceStore) LatestSeq() (uint64, error) {
	if s == nil {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq, nil
}

func (s *SessionReconcilerTraceStore) List(filter TraceFilter) ([]SessionReconcilerTraceRecord, error) {
	return ReadTraceRecords(s.rootDir, filter)
}

func (s *SessionReconcilerTraceStore) quarantine(path string, cause error) error {
	if s == nil {
		return nil
	}
	base := filepath.Base(path)
	dest := filepath.Join(s.rootDir, sessionReconcilerTraceQuarantine, fmt.Sprintf("%s.%d", base, time.Now().UTC().UnixNano()))
	if err := os.MkdirAll(filepath.Dir(dest), sessionReconcilerTraceOwnerDirPerm); err != nil {
		return err
	}
	if err := os.Rename(path, dest); err != nil {
		return err
	}
	if s.stderr != nil {
		fmt.Fprintf(s.stderr, "trace: quarantined %s: %v\n", path, cause) //nolint:errcheck
	}
	return nil
}

func (s *SessionReconcilerTraceStore) isLowSpace() (bool, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(s.rootDir, &st); err != nil {
		return false, err
	}
	free := st.Bavail * uint64(st.Bsize)
	if s.lowSpace {
		return free < sessionReconcilerTraceLowSpaceExitFree, nil
	}
	return free < sessionReconcilerTraceLowSpaceMinFree, nil
}

func (s *SessionReconcilerTraceStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.currentFile != nil {
		err := s.currentFile.Close()
		s.currentFile = nil
		return err
	}
	return nil
}

func (s *SessionReconcilerTraceStore) pruneOldSegments(now time.Time) error {
	if s == nil {
		return nil
	}
	paths, err := filepath.Glob(filepath.Join(s.rootDir, sessionReconcilerTraceSegments, "*", "*", "*", "*.jsonl"))
	if err != nil {
		return err
	}
	type segmentInfo struct {
		path string
		info os.FileInfo
	}
	var segments []segmentInfo
	var totalBytes int64
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		segments = append(segments, segmentInfo{path: path, info: info})
		totalBytes += info.Size()
	}
	sort.SliceStable(segments, func(i, j int) bool {
		return segments[i].info.ModTime().Before(segments[j].info.ModTime())
	})
	for _, seg := range segments {
		tooOld := now.Sub(seg.info.ModTime()) > sessionReconcilerTraceMaxAge
		tooLarge := totalBytes > sessionReconcilerTraceMaxTotalBytes
		if !tooOld && !tooLarge {
			continue
		}
		if s.currentPath == seg.path {
			continue
		}
		if err := os.Remove(seg.path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		totalBytes -= seg.info.Size()
	}
	return nil
}

func (s *SessionReconcilerTraceStore) maybePruneOldSegments(now time.Time) error {
	if s == nil {
		return nil
	}
	if !s.lastPrune.IsZero() && now.Sub(s.lastPrune) < sessionReconcilerTracePruneInterval {
		return nil
	}
	if err := s.pruneOldSegments(now); err != nil {
		return err
	}
	s.lastPrune = now
	return nil
}

func sortTraceRecords(records []SessionReconcilerTraceRecord) {
	sort.SliceStable(records, func(i, j int) bool {
		if !records[i].Ts.Equal(records[j].Ts) {
			return records[i].Ts.Before(records[j].Ts)
		}
		if records[i].TraceID != records[j].TraceID {
			return records[i].TraceID < records[j].TraceID
		}
		if records[i].TickID != records[j].TickID {
			return records[i].TickID < records[j].TickID
		}
		if records[i].RecordType != records[j].RecordType {
			return records[i].RecordType < records[j].RecordType
		}
		if records[i].Seq != records[j].Seq {
			return records[i].Seq < records[j].Seq
		}
		return records[i].RecordID < records[j].RecordID
	})
}

func sortTraceArms(arms []TraceArm) {
	sort.SliceStable(arms, func(i, j int) bool {
		if arms[i].ScopeValue != arms[j].ScopeValue {
			return arms[i].ScopeValue < arms[j].ScopeValue
		}
		if arms[i].Source != arms[j].Source {
			return arms[i].Source < arms[j].Source
		}
		return arms[i].Level < arms[j].Level
	})
}

func ReadTraceRecords(rootDir string, filter TraceFilter) ([]SessionReconcilerTraceRecord, error) {
	segmentRoot := filepath.Join(rootDir, sessionReconcilerTraceSegments)
	paths, err := filepath.Glob(filepath.Join(segmentRoot, "*", "*", "*", "*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	if filter.SeqAfter > 0 && len(paths) > 0 {
		var filtered []string
		for i := len(paths) - 1; i >= 0; i-- {
			maxSeq, ok, scanErr := scanTraceSegment(paths[i])
			if scanErr != nil {
				return nil, fmt.Errorf("reading trace file %s: %w", paths[i], scanErr)
			}
			if !ok {
				continue
			}
			if maxSeq <= filter.SeqAfter {
				break
			}
			filtered = append(filtered, paths[i])
		}
		for i, j := 0, len(filtered)-1; i < j; i, j = i+1, j-1 {
			filtered[i], filtered[j] = filtered[j], filtered[i]
		}
		paths = filtered
	}
	var records []SessionReconcilerTraceRecord
	for _, path := range paths {
		fileRecords, _, err := readTraceRecordsFile(path, filter, true)
		if err != nil {
			return nil, fmt.Errorf("reading trace file %s: %w", path, err)
		}
		records = append(records, fileRecords...)
	}
	sortTraceRecords(records)
	return records, nil
}

func readTraceRecordsFile(path string, filter TraceFilter, tolerateTail bool) ([]SessionReconcilerTraceRecord, uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close() //nolint:errcheck

	var out []SessionReconcilerTraceRecord
	var maxSeq uint64
	var batchRecords []SessionReconcilerTraceRecord
	var batchCRC uint32
	reader := bufio.NewReader(f)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) == 0 && readErr == io.EOF {
			break
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				return nil, 0, readErr
			}
			continue
		}
		var rec SessionReconcilerTraceRecord
		if parseErr := json.Unmarshal(line, &rec); parseErr != nil {
			if tolerateTail && readErr == io.EOF {
				break
			}
			return nil, 0, fmt.Errorf("unmarshal trace record %s: %w", path, parseErr)
		}
		if rec.RecordType == TraceRecordBatchCommit {
			if rec.RecordCount != len(batchRecords) {
				return nil, 0, fmt.Errorf("trace batch %s: commit record_count=%d does not match buffered records=%d", path, rec.RecordCount, len(batchRecords))
			}
			if rec.BatchCRC32 != batchCRC {
				return nil, 0, fmt.Errorf("trace batch %s: crc32 mismatch", path)
			}
			for _, buffered := range batchRecords {
				if matchesTraceFilter(buffered, filter) {
					out = append(out, buffered)
				}
			}
			if matchesTraceFilter(rec, filter) {
				out = append(out, rec)
			}
			if rec.Seq > maxSeq {
				maxSeq = rec.Seq
			}
			batchRecords = batchRecords[:0]
			batchCRC = 0
			if readErr == io.EOF {
				break
			}
			continue
		}
		batchRecords = append(batchRecords, rec)
		batchCRC = crc32.Update(batchCRC, crc32.IEEETable, line)
		if readErr == io.EOF {
			break
		}
	}
	return out, maxSeq, nil
}
