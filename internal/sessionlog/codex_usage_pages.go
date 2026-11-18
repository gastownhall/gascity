package sessionlog

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// CodexUsageCheckpointVersion is the schema version of CodexUsageCheckpoint.
// A stored checkpoint with any other version is rejected by
// ParseCodexUsageCheckpoint, and its reader replays the rollout from byte zero.
const CodexUsageCheckpointVersion = 1

// codexUsageBoundaryBytes is how many bytes before a checkpoint's offset its
// Boundary digest covers. A rollout replaced in place (same path, rewritten
// content) almost never reproduces the bytes just before the offset.
const codexUsageBoundaryBytes = 64

// Default per-pass budgets of ReadCodexUsagePage. A pass reads at most
// DefaultCodexUsagePageMaxBytes of the rollout and yields at most
// DefaultCodexUsagePageMaxFacts usages; a backlog beyond either drains over
// later passes. A record longer than DefaultCodexUsageMaxRecordBytes is never
// decoded.
const (
	DefaultCodexUsagePageMaxBytes   int64 = 16 << 20
	DefaultCodexUsagePageMaxFacts         = 1024
	DefaultCodexUsageMaxRecordBytes       = 1 << 20
)

// codexUsageRecordHeadBytes is how much of a record's start is kept to
// classify a record too long to decode.
const codexUsageRecordHeadBytes = 512

// Codex usage page issue codes. Each names one reason a pass did not read its
// window completely; any of them makes CodexUsagePage.Incomplete true.
const (
	// CodexUsageIssueTornTail: the window ends in an unterminated record that
	// may still be being written. It stays pending; the checkpoint stops
	// before it.
	CodexUsageIssueTornTail = "torn_tail"
	// CodexUsageIssueOversizedRecord: a usage-bearing record (token_count or
	// turn_context) was longer than the record limit and was skipped.
	CodexUsageIssueOversizedRecord = "oversized_record"
	// CodexUsageIssueMalformedRecord: a complete record was not valid JSON
	// and was skipped.
	CodexUsageIssueMalformedRecord = "malformed_record"
	// CodexUsageIssueMissingTimestamp: a usage record had no parseable
	// timestamp, so its eligibility against the emit floor is unknown and it
	// was not emitted.
	CodexUsageIssueMissingTimestamp = "missing_timestamp"
	// CodexUsageIssueSourceReplaced: the checkpoint's source no longer matches
	// the rollout at its path (another session id, or different bytes before
	// the offset), so the pass restarted from byte zero.
	CodexUsageIssueSourceReplaced = "source_replaced"
	// CodexUsageIssueSourceTruncated: the rollout is shorter than the
	// checkpoint's offset, so the pass restarted from byte zero.
	CodexUsageIssueSourceTruncated = "source_truncated"
)

// CodexUsageCheckpoint is the durable parser state of a forward read of one
// Codex rollout: where the read stopped and the context the bytes before it
// established. It binds to its source (Path, SourceID, Boundary) so it is never
// applied to another rollout or to a rewritten one.
type CodexUsageCheckpoint struct {
	// Version is CodexUsageCheckpointVersion.
	Version int `json:"v"`
	// Path is the validated absolute rollout path the checkpoint was read
	// from.
	Path string `json:"path"`
	// SourceID is the rollout's session_meta payload id ("" when the first
	// record carries none).
	SourceID string `json:"source_id,omitempty"`
	// Offset is the byte offset the next read resumes at: the end of the last
	// processed complete record, or a position inside a record being skipped
	// (Skipping).
	Offset int64 `json:"offset"`
	// Boundary is a digest of the bytes just before Offset.
	Boundary string `json:"boundary,omitempty"`
	// Skipping reports that Offset lies inside a record longer than a whole
	// page, which the next read discards through its newline.
	Skipping bool `json:"skipping,omitempty"`
	// EmitFloor is the earliest invocation time the read emits, fixed when
	// the read starts from byte zero. Earlier usage only establishes context.
	EmitFloor time.Time `json:"emit_floor"`
	// TurnModel is the model of the latest turn_context before Offset.
	TurnModel string `json:"turn_model,omitempty"`
	// LastID, LastModel and LastAt identify the latest invocation before
	// Offset: its "total:<N>" identity, the model that produced it and when.
	// A later record repeating LastID is Codex re-emitting that invocation's
	// final snapshot, and is not a new invocation.
	LastID    string    `json:"last_id,omitempty"`
	LastModel string    `json:"last_model,omitempty"`
	LastAt    time.Time `json:"last_at"`
	// Cursor mirrors the flat invocation-usage cursor committed with this
	// checkpoint. A flat cursor that no longer matches it was written by
	// another writer, and the checkpoint is then not trusted.
	Cursor string `json:"cursor,omitempty"`
}

// IsZero reports whether the checkpoint holds no read state.
func (c CodexUsageCheckpoint) IsZero() bool {
	return c.Version == 0 && c.Path == "" && c.Offset == 0
}

// MarshalCodexUsageCheckpoint encodes a checkpoint for storage.
func MarshalCodexUsageCheckpoint(c CodexUsageCheckpoint) (string, error) {
	c.Version = CodexUsageCheckpointVersion
	data, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encoding codex usage checkpoint: %w", err)
	}
	return string(data), nil
}

// ErrCodexUsageCheckpointVersion reports a stored checkpoint of another
// schema version.
var ErrCodexUsageCheckpointVersion = errors.New("unsupported codex usage checkpoint version")

// ParseCodexUsageCheckpoint decodes a stored checkpoint. An empty value is the
// zero checkpoint. A value that does not decode, or carries another version,
// is an error.
func ParseCodexUsageCheckpoint(raw string) (CodexUsageCheckpoint, error) {
	raw = string(bytes.TrimSpace([]byte(raw)))
	if raw == "" {
		return CodexUsageCheckpoint{}, nil
	}
	var c CodexUsageCheckpoint
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return CodexUsageCheckpoint{}, fmt.Errorf("decoding codex usage checkpoint: %w", err)
	}
	if c.Version != CodexUsageCheckpointVersion {
		return CodexUsageCheckpoint{}, fmt.Errorf("%w: %d", ErrCodexUsageCheckpointVersion, c.Version)
	}
	return c, nil
}

// CodexUsagePageLimits bounds one pass of ReadCodexUsagePage. Zero fields take
// the package defaults.
type CodexUsagePageLimits struct {
	MaxBytes       int64
	MaxFacts       int
	MaxRecordBytes int
}

func (l CodexUsagePageLimits) withDefaults() CodexUsagePageLimits {
	if l.MaxBytes <= 0 {
		l.MaxBytes = DefaultCodexUsagePageMaxBytes
	}
	if l.MaxFacts <= 0 {
		l.MaxFacts = DefaultCodexUsagePageMaxFacts
	}
	if l.MaxRecordBytes <= 0 {
		l.MaxRecordBytes = DefaultCodexUsageMaxRecordBytes
	}
	return l
}

// CodexUsagePageRequest is one pass of ReadCodexUsagePage.
type CodexUsagePageRequest struct {
	// SearchPaths are the extra Codex roots Path must lie under (merged with
	// the default roots).
	SearchPaths []string
	// Path is the rollout to read.
	Path string
	// From is the checkpoint to resume from; the zero checkpoint reads from
	// byte zero.
	From CodexUsageCheckpoint
	// EmitFloor is the earliest invocation time to emit when the read starts
	// from byte zero (a zero From, or one the source no longer matches). A
	// resumed read keeps From.EmitFloor.
	EmitFloor time.Time
	// Limits bounds the pass.
	Limits CodexUsagePageLimits
}

// CodexUsagePageEntry is one emitted invocation and the checkpoint that
// resumes the read right after it.
type CodexUsagePageEntry struct {
	Usage      TailUsage
	Checkpoint CodexUsageCheckpoint
}

// CodexUsagePage is the result of one bounded forward pass.
type CodexUsagePage struct {
	// Entries are the eligible invocations read, oldest first.
	Entries []CodexUsagePageEntry
	// Checkpoint resumes the read after every byte this pass processed.
	Checkpoint CodexUsageCheckpoint
	// More reports that the pass stopped at a budget with complete records
	// left before the size snapshot it took.
	More bool
	// Issues lists why the pass did not read its window completely (the
	// CodexUsageIssue* codes), in the order met, without repeats.
	Issues []string
	// BytesRead is how many rollout bytes the pass consumed after its
	// checkpoint.
	BytesRead int64
}

// Incomplete reports whether the pass left anything unread or unaccounted:
// a backlog (More) or any issue.
func (p CodexUsagePage) Incomplete() bool {
	return p.More || len(p.Issues) > 0
}

func (p *CodexUsagePage) addIssue(code string) {
	for _, have := range p.Issues {
		if have == code {
			return
		}
	}
	p.Issues = append(p.Issues, code)
}

// ReadCodexUsagePage reads a Codex rollout forward from a checkpoint and
// returns the invocations it finds, each with the checkpoint that resumes
// after it. It never reads past one size snapshot taken when it starts, past
// Limits.MaxBytes, or past the record that yields Limits.MaxFacts invocations,
// and it never skips bytes it has not processed: a backlog beyond the budget is
// left for the next pass (More).
//
// Context is parsed in order: turn_context names the model of the
// invocations that follow, and a token_count repeating the previous
// invocation's cumulative total is Codex re-emitting that invocation's final
// snapshot after the next turn_context, so it is dropped and the invocation
// keeps the model and time it was first recorded with. That context travels in
// the checkpoint, so it survives page boundaries and restarts. Invocations
// before the checkpoint's EmitFloor establish context but are not emitted.
//
// The checkpoint is checked against the rollout before it is used. A rollout
// shorter than the offset, or one whose session id or bytes before the offset
// differ, restarts the read from byte zero with req.EmitFloor and reports the
// issue. A checkpoint for another path is not an issue: the read starts fresh.
//
// Records are complete lines. An unterminated final record stays pending. A
// record that is not valid JSON, or a usage record longer than the record
// limit, is skipped and reported. Every issue is reported in Issues; a page
// with issues or More is Incomplete.
func ReadCodexUsagePage(ctx context.Context, req CodexUsagePageRequest) (CodexUsagePage, error) {
	limits := req.Limits.withDefaults()
	path, err := validateSearchPathFile(mergeCodexSearchPaths(req.SearchPaths), req.Path)
	if err != nil {
		return CodexUsagePage{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return CodexUsagePage{}, err
	}
	defer f.Close() //nolint:errcheck // read-only

	info, err := f.Stat()
	if err != nil {
		return CodexUsagePage{}, fmt.Errorf("stat codex rollout: %w", err)
	}
	size := info.Size()

	var page CodexUsagePage
	cp, err := codexUsageResumePoint(f, path, size, req, &page)
	if err != nil {
		return CodexUsagePage{}, err
	}

	r := codexUsagePageReader{
		ctx:    ctx,
		limits: limits,
		cp:     cp,
		page:   &page,
		start:  cp.Offset,
		size:   size,
	}
	if cp.Offset > 0 {
		tail, err := codexUsageBytesBefore(f, cp.Offset)
		if err != nil {
			return CodexUsagePage{}, err
		}
		r.tail = tail
	}
	// A record crossing the byte budget is deferred to the next pass, where it
	// starts the page; the record that starts a page may run to the record
	// limit, so a budget smaller than one record still makes progress.
	r.limit = min(size, cp.Offset+limits.MaxBytes)
	window := min(size, max(r.limit, cp.Offset+int64(limits.MaxRecordBytes)+1))
	br := bufio.NewReaderSize(io.NewSectionReader(f, cp.Offset, window-cp.Offset), 64*1024)
	if err := r.run(br); err != nil {
		return CodexUsagePage{}, err
	}
	page.Checkpoint = r.cp
	page.BytesRead = r.cp.Offset - r.start
	return page, nil
}

// codexUsageResumePoint validates req.From against the rollout and returns
// the checkpoint the pass starts from.
func codexUsageResumePoint(f *os.File, path string, size int64, req CodexUsagePageRequest, page *CodexUsagePage) (CodexUsageCheckpoint, error) {
	fresh := CodexUsageCheckpoint{Version: CodexUsageCheckpointVersion, Path: path, EmitFloor: req.EmitFloor}
	from := req.From
	if from.IsZero() || from.Path != path {
		return fresh, nil
	}
	if size < from.Offset {
		page.addIssue(CodexUsageIssueSourceTruncated)
		return fresh, nil
	}
	if from.Offset == 0 {
		from.Version = CodexUsageCheckpointVersion
		return from, nil
	}
	sourceID, err := codexSessionMetaID(path)
	if err != nil {
		return CodexUsageCheckpoint{}, err
	}
	boundary, err := codexUsageBytesBefore(f, from.Offset)
	if err != nil {
		return CodexUsageCheckpoint{}, err
	}
	if sourceID != from.SourceID || codexUsageBoundaryDigest(boundary) != from.Boundary {
		page.addIssue(CodexUsageIssueSourceReplaced)
		return fresh, nil
	}
	from.Version = CodexUsageCheckpointVersion
	return from, nil
}

// codexSessionMetaLineID returns the session_meta payload id of one record,
// or "" when the record is not a decodable session_meta.
func codexSessionMetaLineID(line []byte) string {
	var meta struct {
		Type    string `json:"type"`
		Payload struct {
			ID string `json:"id"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &meta) != nil || meta.Type != "session_meta" {
		return ""
	}
	return strings.TrimSpace(meta.Payload.ID)
}

// codexUsageBytesBefore returns up to codexUsageBoundaryBytes bytes ending at
// offset.
func codexUsageBytesBefore(f *os.File, offset int64) ([]byte, error) {
	start := offset - codexUsageBoundaryBytes
	if start < 0 {
		start = 0
	}
	buf := make([]byte, offset-start)
	if _, err := f.ReadAt(buf, start); err != nil {
		return nil, fmt.Errorf("reading codex rollout boundary: %w", err)
	}
	return buf, nil
}

func codexUsageBoundaryDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

// codexUsagePageReader is the state of one ReadCodexUsagePage pass.
type codexUsagePageReader struct {
	ctx    context.Context
	limits CodexUsagePageLimits
	cp     CodexUsageCheckpoint
	page   *CodexUsagePage
	start  int64
	size   int64
	limit  int64
	// tail holds the last codexUsageBoundaryBytes bytes consumed, for the
	// checkpoint Boundary.
	tail []byte
}

func (r *codexUsagePageReader) consume(chunk []byte) {
	r.tail = append(r.tail, chunk...)
	if extra := len(r.tail) - codexUsageBoundaryBytes; extra > 0 {
		r.tail = append(r.tail[:0], r.tail[extra:]...)
	}
}

func (r *codexUsagePageReader) advance(n int64) {
	r.cp.Offset += n
	r.cp.Boundary = codexUsageBoundaryDigest(r.tail)
}

func (r *codexUsagePageReader) run(br *bufio.Reader) error {
	if r.cp.Skipping {
		n, terminated, err := discardCodexUsageRecord(br, r.consume)
		if err != nil {
			return err
		}
		r.advance(n)
		if !terminated {
			if r.cp.Offset < r.size {
				r.page.More = true
			} else {
				r.page.addIssue(CodexUsageIssueTornTail)
			}
			return nil
		}
		r.cp.Skipping = false
	}
	for lines := 0; r.cp.Offset < r.limit; lines++ {
		if lines%256 == 0 {
			if err := r.ctx.Err(); err != nil {
				return err
			}
		}
		if len(r.page.Entries) >= r.limits.MaxFacts {
			r.page.More = true
			return nil
		}
		recordStart := r.cp.Offset
		savedTail := append([]byte(nil), r.tail...)
		line, head, n, terminated, oversized, err := readCodexUsageRecord(br, r.limits.MaxRecordBytes, r.consume)
		if err != nil {
			return err
		}
		cutShort := !terminated && recordStart+n < r.size
		if recordStart != r.start && (cutShort || recordStart+n > r.limit) {
			// The record crosses the byte budget: the next pass starts with it.
			r.tail = savedTail
			r.page.More = true
			return nil
		}
		if !terminated {
			if cutShort {
				// The read window ended inside a record longer than the record
				// limit: consume what this pass read and discard the rest on
				// the next pass, so the read never stalls on it.
				r.advance(n)
				r.cp.Skipping = true
				if codexUsageRecordHeadIsUsage(head) {
					r.page.addIssue(CodexUsageIssueOversizedRecord)
				}
				r.page.More = true
				return nil
			}
			// The size snapshot ends inside this record: it may still be being
			// written, so it stays pending.
			r.tail = savedTail
			if n > 0 {
				r.page.addIssue(CodexUsageIssueTornTail)
			}
			return nil
		}
		r.advance(n)
		if oversized {
			if codexUsageRecordHeadIsUsage(head) {
				r.page.addIssue(CodexUsageIssueOversizedRecord)
			}
			continue
		}
		r.processRecord(line, recordStart)
	}
	if r.cp.Offset < r.size {
		r.page.More = true
	}
	return nil
}

var (
	codexTokenCountMarker  = []byte(`"token_count"`)
	codexTurnContextMarker = []byte(`"turn_context"`)
)

// codexUsageRecordHeadIsUsage reports whether a record's start names it a
// token_count or turn_context record, the records usage accounting reads.
func codexUsageRecordHeadIsUsage(head []byte) bool {
	return bytes.Contains(head, codexTokenCountMarker) || bytes.Contains(head, codexTurnContextMarker)
}

// processRecord folds one complete record into the checkpoint and emits it
// when it is an eligible new invocation.
func (r *codexUsagePageReader) processRecord(line []byte, recordStart int64) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	if !json.Valid(line) {
		r.page.addIssue(CodexUsageIssueMalformedRecord)
		return
	}
	first := recordStart == 0
	if !first && !bytes.Contains(line, codexTokenCountMarker) && !bytes.Contains(line, codexTurnContextMarker) {
		return
	}
	var entry codexRawEntry
	if json.Unmarshal(line, &entry) != nil {
		return
	}
	if entry.Type == "session_meta" {
		if first {
			r.cp.SourceID = codexSessionMetaLineID(line)
		}
		return
	}
	var payload codexUsagePayload
	if json.Unmarshal(entry.Payload, &payload) != nil {
		return
	}
	if entry.Type == "turn_context" {
		if payload.Model != "" {
			r.cp.TurnModel = payload.Model
		}
		return
	}
	u, ok := codexTokenCountUsage(entry, payload, r.cp.TurnModel)
	if !ok {
		return
	}
	if u.MessageID == r.cp.LastID {
		// Codex re-emits the previous invocation's final snapshot after the
		// next turn_context; that invocation was already read with its own
		// model and time.
		return
	}
	r.cp.LastID = u.MessageID
	r.cp.LastModel = u.Model
	r.cp.LastAt = u.Timestamp
	if u.Timestamp.IsZero() {
		r.page.addIssue(CodexUsageIssueMissingTimestamp)
		return
	}
	if !r.cp.EmitFloor.IsZero() && u.Timestamp.Before(r.cp.EmitFloor) {
		return
	}
	r.page.Entries = append(r.page.Entries, CodexUsagePageEntry{Usage: u, Checkpoint: r.cp})
}

// readCodexUsageRecord reads one line from br. It returns the line without
// its newline (nil when the line is longer than maxRecord, which reports
// oversized), the line's first codexUsageRecordHeadBytes bytes, the bytes
// consumed, and whether a newline ended it before br ran out. Each chunk read
// is passed to consume when it is non-nil.
func readCodexUsageRecord(br *bufio.Reader, maxRecord int, consume func([]byte)) (line, head []byte, n int64, terminated, oversized bool, err error) {
	for {
		chunk, readErr := br.ReadSlice('\n')
		n += int64(len(chunk))
		if consume != nil {
			consume(chunk)
		}
		if room := codexUsageRecordHeadBytes - len(head); room > 0 {
			take := chunk
			if len(take) > room {
				take = take[:room]
			}
			head = append(head, take...)
		}
		if !oversized {
			if len(line)+len(chunk) > maxRecord+1 {
				oversized = true
				line = nil
			} else {
				line = append(line, chunk...)
			}
		}
		switch {
		case readErr == nil:
			return bytes.TrimSuffix(line, []byte("\n")), head, n, true, oversized, nil
		case errors.Is(readErr, bufio.ErrBufferFull):
			continue
		case errors.Is(readErr, io.EOF):
			return line, head, n, false, oversized, nil
		default:
			return nil, nil, n, false, false, fmt.Errorf("reading codex rollout: %w", readErr)
		}
	}
}

// discardCodexUsageRecord consumes br through the next newline without
// buffering the record, reporting the bytes consumed and whether a newline
// was reached.
func discardCodexUsageRecord(br *bufio.Reader, consume func([]byte)) (n int64, terminated bool, err error) {
	for {
		chunk, readErr := br.ReadSlice('\n')
		n += int64(len(chunk))
		consume(chunk)
		switch {
		case readErr == nil:
			return n, true, nil
		case errors.Is(readErr, bufio.ErrBufferFull):
			continue
		case errors.Is(readErr, io.EOF):
			return n, false, nil
		default:
			return n, false, fmt.Errorf("reading codex rollout: %w", readErr)
		}
	}
}
