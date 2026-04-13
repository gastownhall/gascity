package events

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// seqCounterPath returns the sidecar counter path for a given events.jsonl
// path. The sidecar lives next to the events file with a ".seq" suffix.
func seqCounterPath(eventsPath string) string {
	return eventsPath + ".seq"
}

// seqCounter holds a cross-process sequence counter backed by a sidecar
// file protected by advisory file locks. See [seqCounter.Next] for the
// allocation protocol.
//
// Design note: seqCounter intentionally opens and closes the sidecar file
// on every call to Next/Current rather than holding a persistent file
// descriptor. This avoids the need for a Close method and simplifies
// lifecycle management in FileRecorder. If a persistent handle is ever
// introduced for performance, FileRecorder.Close must be updated to
// release it.
type seqCounter struct {
	path       string
	eventsPath string // path to the events.jsonl file; used for re-scan fallback
}

// newSeqCounter constructs a seqCounter for the given sidecar path and
// seeds it from seedIfMissing when the file does not yet exist. Seeding
// is only performed on first use; if the sidecar already exists its
// contents are trusted.
//
// Seeding is race-safe: we open with O_RDWR|O_CREATE, acquire the same
// advisory lock used by Next(), and only write the seed if the file is
// still empty. This ensures exactly one process wins the seed, and all
// others see the winner's value — no TOCTOU window.
func newSeqCounter(path string, eventsPath string, seedIfMissing uint64) (*seqCounter, error) {
	sc := &seqCounter{path: path, eventsPath: eventsPath}

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open seq counter: %w", err)
	}
	defer f.Close() //nolint:errcheck

	if err := lockFile(f); err != nil {
		return nil, fmt.Errorf("lock seq counter for seed: %w", err)
	}
	defer func() { _ = unlockFile(f) }()

	// Check if the file is empty — if so, we're the first process and
	// need to seed. If non-empty, another process already seeded.
	buf := make([]byte, 64)
	n, rerr := f.ReadAt(buf, 0)
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		return nil, fmt.Errorf("read seq counter: %w", rerr)
	}

	if n == 0 {
		out := strconv.FormatUint(seedIfMissing, 10)
		if _, err := f.WriteAt([]byte(out), 0); err != nil {
			return nil, fmt.Errorf("seeding seq counter: %w", err)
		}
		if err := f.Sync(); err != nil {
			return nil, fmt.Errorf("syncing seq counter seed: %w", err)
		}
	}

	return sc, nil
}

// Next atomically allocates the next sequence number. It:
//  1. Opens the sidecar file (creating if missing)
//  2. Acquires an exclusive advisory lock (flock LOCK_EX on Unix;
//     LockFileEx on Windows; no-op on unsupported platforms)
//  3. Reads the current value (treats empty/missing as 0)
//  4. Computes next = current + 1
//  5. Writes next back at offset 0 with truncation, then fsyncs
//  6. Releases the lock and closes the file
//
// This is cross-process safe on any platform where lockFile is backed by
// a kernel advisory lock. See lockFile_unix.go / lockFile_windows.go /
// lockFile_other.go for the platform fallbacks.
func (s *seqCounter) Next() (uint64, error) {
	f, err := os.OpenFile(s.path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return 0, fmt.Errorf("open seq counter: %w", err)
	}
	defer f.Close() //nolint:errcheck // best-effort close; write errors surface via Sync

	if err := lockFile(f); err != nil {
		return 0, fmt.Errorf("lock seq counter: %w", err)
	}
	defer func() { _ = unlockFile(f) }()

	buf := make([]byte, 64)
	n, rerr := f.ReadAt(buf, 0)
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		return 0, fmt.Errorf("read seq counter: %w", rerr)
	}

	var current uint64
	if n > 0 {
		str := strings.TrimSpace(string(buf[:n]))
		if str != "" {
			v, perr := strconv.ParseUint(str, 10, 64)
			if perr != nil {
				return 0, fmt.Errorf("parse seq counter %q: %w", str, perr)
			}
			current = v
		}
	}

	// Guard against sidecar deletion: if current is 0 but the events
	// file already has records, re-scan to recover. This prevents
	// duplicate seq values after accidental sidecar removal.
	if current == 0 && s.eventsPath != "" {
		if maxSeq, err := ReadLatestSeq(s.eventsPath); err == nil && maxSeq > 0 {
			current = maxSeq
		}
	}

	next := current + 1
	out := strconv.FormatUint(next, 10)

	if err := f.Truncate(0); err != nil {
		return 0, fmt.Errorf("truncate seq counter: %w", err)
	}
	if _, err := f.WriteAt([]byte(out), 0); err != nil {
		return 0, fmt.Errorf("write seq counter: %w", err)
	}
	if err := f.Sync(); err != nil {
		return 0, fmt.Errorf("sync seq counter: %w", err)
	}
	return next, nil
}

// Current returns the latest allocated sequence number without
// incrementing. It acquires the same advisory lock as Next to ensure
// cross-process visibility. This is O(1) — it reads a small sidecar
// file rather than scanning the entire events.jsonl.
//
// Note: Current returns the last allocated seq, which may be one ahead
// of what's readable in events.jsonl if a Record() call allocated a seq
// but hasn't written the JSONL line yet (or if the write failed). This
// is acceptable because the window is sub-millisecond in normal
// operation, and cross-process visibility is more valuable than strict
// JSONL-readability guarantees for LatestSeq callers.
func (s *seqCounter) Current() (uint64, error) {
	f, err := os.OpenFile(s.path, os.O_RDONLY, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Sidecar missing — fall back to file scan if events exist.
			if s.eventsPath != "" {
				if maxSeq, serr := ReadLatestSeq(s.eventsPath); serr == nil && maxSeq > 0 {
					return maxSeq, nil
				}
			}
			return 0, nil
		}
		return 0, fmt.Errorf("open seq counter: %w", err)
	}
	defer f.Close() //nolint:errcheck

	if err := lockFile(f); err != nil {
		return 0, fmt.Errorf("lock seq counter: %w", err)
	}
	defer func() { _ = unlockFile(f) }()

	buf := make([]byte, 64)
	n, rerr := f.ReadAt(buf, 0)
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		return 0, fmt.Errorf("read seq counter: %w", rerr)
	}

	if n == 0 {
		// Empty sidecar — fall back to file scan.
		if s.eventsPath != "" {
			if maxSeq, serr := ReadLatestSeq(s.eventsPath); serr == nil && maxSeq > 0 {
				return maxSeq, nil
			}
		}
		return 0, nil
	}
	str := strings.TrimSpace(string(buf[:n]))
	if str == "" {
		return 0, nil
	}
	v, perr := strconv.ParseUint(str, 10, 64)
	if perr != nil {
		return 0, fmt.Errorf("parse seq counter %q: %w", str, perr)
	}
	return v, nil
}
