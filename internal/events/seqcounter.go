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
type seqCounter struct {
	path string
}

// newSeqCounter constructs a seqCounter for the given sidecar path and
// seeds it from seedIfMissing when the file does not yet exist. Seeding
// is only performed on first use; if the sidecar already exists its
// contents are trusted.
//
// Seeding is race-safe: we open with O_EXCL so only one process wins the
// initial create. Losing the race is treated as success because the
// winning process has already written a valid value.
func newSeqCounter(path string, seedIfMissing uint64) (*seqCounter, error) {
	sc := &seqCounter{path: path}

	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		f, ferr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if ferr == nil {
			_, werr := f.WriteString(strconv.FormatUint(seedIfMissing, 10))
			cerr := f.Close()
			if werr != nil {
				return nil, fmt.Errorf("seeding seq counter: %w", werr)
			}
			if cerr != nil {
				return nil, fmt.Errorf("closing seq counter: %w", cerr)
			}
		} else if !errors.Is(ferr, os.ErrExist) {
			return nil, fmt.Errorf("creating seq counter: %w", ferr)
		}
	} else if err != nil {
		return nil, fmt.Errorf("stat seq counter: %w", err)
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
