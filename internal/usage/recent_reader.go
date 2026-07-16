package usage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// RecentReadReport describes information lost while reading a bounded tail of
// the usage log. It deliberately carries counts rather than warning strings so
// API callers cannot accidentally expose the host filesystem path.
type RecentReadReport struct {
	Truncated     bool
	RecordLimited bool
	Malformed     int
	Oversized     int
}

const (
	recentFactMaxBytes   = 1 << 20
	recentFactMaxRecords = 50_000
)

// ReadRecentFacts reads at most maxBytes from the newest end of a LocalSink
// JSONL file. It is the bounded reader for latency-sensitive HTTP surfaces;
// the CLI's ReadFacts still scans the complete history for exact reports.
//
// When the file is larger than maxBytes, the leading partial line is discarded
// and report.Truncated is set. At most recentFactMaxRecords non-empty records
// are decoded, newest first, bounding both Fact storage and de-duplication state
// even when the byte tail contains millions of tiny lines. Facts are returned
// in input order and de-duplicated by idempotency key (newest occurrence wins).
// Missing files are an empty, available reading.
func ReadRecentFacts(path string, maxBytes int64) ([]Fact, RecentReadReport, error) {
	if maxBytes <= 0 {
		return nil, RecentReadReport{}, fmt.Errorf("usage read limit must be positive")
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, RecentReadReport{}, nil
	}
	if err != nil {
		return nil, RecentReadReport{}, err
	}
	defer file.Close() //nolint:errcheck // read-only handle

	info, err := file.Stat()
	if err != nil {
		return nil, RecentReadReport{}, err
	}
	report := RecentReadReport{Truncated: info.Size() > maxBytes}
	start := max(info.Size()-maxBytes, 0)
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, report, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes))
	if err != nil {
		return nil, report, err
	}
	if start > 0 {
		newline := bytes.IndexByte(data, '\n')
		if newline < 0 {
			return nil, report, nil
		}
		data = data[newline+1:]
	}

	seen := make(map[string]struct{})
	facts := make([]Fact, 0, min(recentFactMaxRecords, len(data)/64))
	processed := 0
	end := len(data)
	for end > 0 && processed < recentFactMaxRecords {
		lineEnd := end
		if data[lineEnd-1] == '\n' {
			lineEnd--
			end = lineEnd
			if lineEnd == 0 {
				break
			}
		}
		separator := bytes.LastIndexByte(data[:lineEnd], '\n')
		lineStart := separator + 1
		raw := data[lineStart:lineEnd]
		if separator < 0 {
			end = 0
		} else {
			end = separator
		}
		content := bytes.TrimSpace(raw)
		if len(content) == 0 {
			continue
		}
		processed++
		if len(raw) > recentFactMaxBytes {
			report.Oversized++
			continue
		}
		var fact Fact
		if err := json.Unmarshal(content, &fact); err != nil {
			report.Malformed++
			continue
		}
		if fact.IdempotencyKey == "" {
			facts = append(facts, fact)
			continue
		}
		if _, duplicate := seen[fact.IdempotencyKey]; duplicate {
			continue
		}
		seen[fact.IdempotencyKey] = struct{}{}
		facts = append(facts, fact)
	}
	if processed == recentFactMaxRecords && len(bytes.TrimSpace(data[:end])) > 0 {
		report.RecordLimited = true
	}
	for left, right := 0, len(facts)-1; left < right; left, right = left+1, right-1 {
		facts[left], facts[right] = facts[right], facts[left]
	}
	return facts, report, nil
}
