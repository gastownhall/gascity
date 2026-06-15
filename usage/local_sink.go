package usage

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// LocalSink is the OSS-default [Sink]: a durable, append-only JSONL file. Each
// fact is one JSON line, appended and fsync'd before Record returns, so the file
// acts as a durable outbox — a crash cannot lose a fact whose Record returned
// nil. Duplicate facts (same IdempotencyKey) are not overwritten; they are
// collapsed at read time by [ReadFacts], so the sink is never last-write-wins.
type LocalSink struct {
	mu   sync.Mutex
	path string
}

// NewLocalSink returns a LocalSink that appends facts to path. The parent
// directory is created on first write.
func NewLocalSink(path string) *LocalSink { return &LocalSink{path: path} }

// Record appends f to the underlying file and fsyncs before returning.
func (s *LocalSink) Record(_ context.Context, f UsageFact) error {
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
	file, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(line); err != nil {
		return err
	}
	return file.Sync()
}

// ReadFacts reads all facts from a LocalSink file, collapsing duplicates by
// IdempotencyKey (first occurrence wins; input order is preserved). Facts with
// an empty IdempotencyKey cannot be deduplicated and are all kept. A torn final
// line (e.g. a crash mid-append) is skipped rather than treated as an error, so
// the durable log stays readable. Returns nil when the file does not exist.
func ReadFacts(path string) ([]UsageFact, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var out []UsageFact
	seen := make(map[string]struct{})
	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var f UsageFact
		if err := json.Unmarshal(line, &f); err != nil {
			// Best-effort: skip an unparseable (e.g. torn final) line.
			continue
		}
		if f.IdempotencyKey != "" {
			if _, dup := seen[f.IdempotencyKey]; dup {
				continue
			}
			seen[f.IdempotencyKey] = struct{}{}
		}
		out = append(out, f)
	}
	return out, sc.Err()
}
