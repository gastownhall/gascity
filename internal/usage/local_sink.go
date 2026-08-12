package usage

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// LocalSink is the OSS-default [Sink]: a durable, append-only JSONL file. Each
// fact is one JSON line, appended and fsync'd before Record returns, so the file
// acts as a durable outbox — a crash cannot lose a fact whose Record returned
// nil. Duplicate facts (same IdempotencyKey) are not overwritten; they are
// collapsed at read time by [ReadFacts], so the sink is never last-write-wins.
//
// A crash mid-append can leave a torn (unterminated) final line. Before its next
// append, Record terminates such a tail with a newline so the torn fragment
// becomes its own line instead of being concatenated with — and hiding — the
// next fact. [ReadFacts] then skips the lone torn fragment and keeps every
// intact fact around it.
type LocalSink struct {
	mu   sync.Mutex
	path string
}

// NewLocalSink returns a LocalSink that appends facts to path. The parent
// directory is created on first write.
func NewLocalSink(path string) *LocalSink { return &LocalSink{path: path} }

// IsLocalSink reports whether sink records into the supervisor-local JSONL
// file. Read APIs use this to avoid presenting an old file as live telemetry
// after the configured provider changes to exec or discard.
func IsLocalSink(sink Sink) bool {
	_, ok := sink.(*LocalSink)
	return ok
}

// Record appends f to the underlying file and fsyncs before returning.
func (s *LocalSink) Record(_ context.Context, f Fact) error {
	line, err := json.Marshal(f)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()

	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	// O_RDWR (not O_WRONLY) so terminateTornTail can inspect the final byte;
	// O_APPEND still forces every write to the end.
	file, err := os.OpenFile(s.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if err := terminateTornTail(file); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(line); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// terminateTornTail appends a newline when the file ends without one, so a
// record torn by an earlier crash (an unterminated final line) becomes its own
// line instead of being concatenated with the fact appended next. For the
// common single-writer sink this preserves the durability contract end to end:
// the next fact lands on a fresh line and survives the read, while [ReadFacts]
// skips only the torn fragment itself.
func terminateTornTail(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return nil
	}
	last := make([]byte, 1)
	if _, err := file.ReadAt(last, info.Size()-1); err != nil {
		return err
	}
	if last[0] == '\n' {
		return nil
	}
	_, err = file.Write([]byte{'\n'})
	return err
}

// ReadFacts reads all facts from a LocalSink file, collapsing duplicates by
// IdempotencyKey (first occurrence wins; input order is preserved). Facts with
// an empty IdempotencyKey cannot be deduplicated and are all kept.
//
// A malformed line — a torn mid-append, or a record a torn predecessor merged
// into — is skipped rather than aborting the whole read, and reported in the
// returned warnings slice. This keeps gc costs usable on a partially corrupt log
// while honoring "never silently undercount": each skipped line is surfaced as
// a line-numbered warning, not dropped in silence. The returned error is
// reserved for I/O failures; a corrupt line is a warning, not an error. Returns
// nil facts and nil warnings when the file does not exist.
func ReadFacts(path string) (facts []Fact, warnings []string, err error) {
	file, openErr := os.Open(path)
	if errors.Is(openErr, os.ErrNotExist) {
		return nil, nil, nil
	}
	if openErr != nil {
		return nil, nil, openErr
	}
	defer file.Close() //nolint:errcheck // read-only handle

	seen := make(map[string]struct{})
	// bufio.Reader.ReadString grows to fit arbitrarily long lines instead of
	// failing on an over-budget token, and preserves the terminator so a trailing
	// empty chunk at EOF is distinguishable from a real record.
	reader := bufio.NewReaderSize(file, 64*1024)
	lineNo := 0
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return facts, warnings, readErr
		}
		atEOF := errors.Is(readErr, io.EOF)
		if atEOF && line == "" {
			break
		}
		lineNo++
		if content := strings.TrimRight(line, "\r\n"); content != "" {
			var f Fact
			if jsonErr := json.Unmarshal([]byte(content), &f); jsonErr != nil {
				warnings = append(warnings, fmt.Sprintf("usage: skipped malformed fact at %s:%d: %v", path, lineNo, jsonErr))
			} else if f.IdempotencyKey == "" {
				facts = append(facts, f)
			} else if _, dup := seen[f.IdempotencyKey]; !dup {
				seen[f.IdempotencyKey] = struct{}{}
				facts = append(facts, f)
			}
		}
		if atEOF {
			break
		}
	}
	return facts, warnings, nil
}
