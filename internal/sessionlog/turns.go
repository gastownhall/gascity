package sessionlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Turn identity from transcripts (engdocs/design/x11-turn-identity-coverage.md
// §3.2, §5.2; BUILD-PLAN TI-2a). A turn is one run of the agent, from the
// input that starts it to the moment it is idle again. The scanners here read
// the ids each provider records for its own turns. They never read delivered
// text for an id (X11 D13): nothing gc delivers carries one.
//
// That design document, cited below as X11, and its build plan are not on
// main yet; until they land, these comments and the tests that pin them are
// the in-tree contract.
//
// These are sessionlog types; the worker boundary converts them to the
// runtime's turn types (TI-2b).

// TurnStartLevel is how well a transcript family records turn starts (X11
// §3.1, the Start axis).
type TurnStartLevel string

// Turn start levels, lowest first.
const (
	TurnStartNone     TurnStartLevel = "none"
	TurnStartBoundary TurnStartLevel = "boundary"
	TurnStartRecorded TurnStartLevel = "recorded"
)

// TurnEndLevel is how well a transcript family records turn ends (X11 §3.1,
// the End axis).
type TurnEndLevel string

// Turn end levels, lowest first.
const (
	TurnEndNone     TurnEndLevel = "none"
	TurnEndBoundary TurnEndLevel = "boundary"
	TurnEndInferred TurnEndLevel = "inferred"
	TurnEndRecorded TurnEndLevel = "recorded"
)

// TurnLinkLevel is how a delivery is linked to the turn it started (X11 §3.1,
// the Link axis).
type TurnLinkLevel string

// Turn link levels, lowest first.
const (
	TurnLinkNone       TurnLinkLevel = "none"
	TurnLinkCorrelated TurnLinkLevel = "correlated"
	TurnLinkExact      TurnLinkLevel = "exact"
)

// TurnSupport is what a provider family's transcript tells about its turns.
// How deliver-now replaces a family's turn is not a transcript fact:
// internal/session decides it (session.RuntimeTurnTraits.Replace).
type TurnSupport struct {
	Start TurnStartLevel
	End   TurnEndLevel
	Link  TurnLinkLevel
	// HookTurnID names the hook payload field that carries the same id as the
	// transcript. It stays empty until the X11 live arm verifies the match.
	HookTurnID string
}

// turnFamily is one row of the family table: what the family supports and
// the format that reads it (nil when its transcript is not read for turns).
type turnFamily struct {
	support TurnSupport
	format  turnFormat
}

// turnFamilies is the family table. It is the only place that knows which
// provider families record turns (X11 §3.2).
var turnFamilies = map[string]turnFamily{
	"claude": {
		support: TurnSupport{Start: TurnStartRecorded, End: TurnEndInferred, Link: TurnLinkCorrelated},
		format:  claudeTurnFormat{},
	},
	"codex": {
		support: TurnSupport{Start: TurnStartRecorded, End: TurnEndRecorded, Link: TurnLinkCorrelated},
		format:  codexTurnFormat{},
	},
	"gemini": {
		support: TurnSupport{Start: TurnStartRecorded, End: TurnEndInferred, Link: TurnLinkCorrelated},
		format:  geminiTurnFormat{},
	},
}

// defaultTurnSupport is the row of every family the table does not list: no
// turn identity.
var defaultTurnSupport = TurnSupport{Start: TurnStartNone, End: TurnEndNone, Link: TurnLinkNone}

// TurnSupportFor reports what provider's transcript family records about its
// turns.
func TurnSupportFor(provider string) TurnSupport {
	if fam, ok := turnFamilies[turnFamilyKey(provider)]; ok {
		return fam.support
	}
	return defaultTurnSupport
}

// turnFamilyKey resolves provider to its row in turnFamilies. ProviderFamily
// has no claude clause, so a claude spelling it places in no other family
// ("claude/tmux-cli", the "claude-eco" alias) comes back as the name itself;
// that name is claude, as transcript discovery reads it. A name it places
// elsewhere ("opencode-claude") keeps that family.
func turnFamilyKey(provider string) string {
	family := ProviderFamily(provider)
	if strings.Contains(family, "claude") {
		return "claude"
	}
	return family
}

// TurnStatus is how an ended turn ended. The empty status means the source
// recorded no end: a newer turn started first.
type TurnStatus string

// Turn statuses.
const (
	TurnStatusCompleted TurnStatus = "completed"
	TurnStatusCancelled TurnStatus = "cancelled" //nolint:misspell // matches runtime.TurnStatusCancelled
	TurnStatusFailed    TurnStatus = "failed"
)

// ScannedTurn is one turn a transcript recorded.
type ScannedTurn struct {
	// ID is the source's own id, compared only for equality (X11 D2). It is
	// empty when the source recorded the turn without a usable id.
	ID string
	// StartedAt is the source's time for the turn's first record, at the
	// source's precision; zero when the start lies before the scanned window.
	StartedAt time.Time
	// EndedAt is the source's time for the turn's end; zero while it is
	// open. A turn closed by a newer one ends at the newer turn's start.
	EndedAt time.Time
	// Status is how the turn ended; empty while it is open or when only a
	// newer turn closed it.
	Status TurnStatus
	// Failure is the provider error a failed turn ended on, as the source
	// recorded it; zero for a turn that did not fail, and for a family whose
	// transcript records no such error.
	Failure TurnFailure
}

// MaxTurnFailureMessage bounds TurnFailure.Message, in bytes.
const MaxTurnFailureMessage = 512

// claudeRateLimitClass is the class Claude Code gives an API error that a
// rate limit refused.
const claudeRateLimitClass = "rate_limit"

// TurnFailure is the provider error a turn ended on, as the transcript's
// source classified it when it wrote the error.
type TurnFailure struct {
	// Code is the source's own class for the error, verbatim (Claude Code:
	// "rate_limit", "server_error", ...); empty when it recorded none.
	Code string
	// Status is the HTTP status the source recorded; 0 when it recorded
	// none.
	Status int
	// Message is the error's text, cut on a rune boundary to at most
	// MaxTurnFailureMessage bytes. It is for people: nothing classifies it.
	Message string
}

// RateLimited reports whether the provider refused the turn with a rate
// limit: HTTP 429, or the source's rate-limit class. It never reads the
// message.
func (f TurnFailure) RateLimited() bool {
	return f.Status == http.StatusTooManyRequests || f.Code == claudeRateLimitClass
}

// TurnScan is one read of a transcript's current and recent turns.
type TurnScan struct {
	// Open reports that the transcript records Current's start and no end
	// yet. Whether the agent is still running is the caller's call (session
	// liveness, TI-2b's idle overlay): a session killed mid-turn leaves its
	// turn open, and a local Claude command or bash-mode input opens one the
	// agent never runs.
	Open bool
	// Current is the open turn when Open is set, else the zero value.
	Current ScannedTurn
	// Recent holds ended turns, newest first, at most MaxRecentTurns.
	Recent []ScannedTurn
	// MissingIDs reports that the source recorded a turn boundary without a
	// usable id, so the transcript gives boundaries, not identity (X11 D1).
	MissingIDs bool
	// MalformedRecords counts records that could bear on turns but did not
	// decode, so were skipped. UntimedRecords counts turn records whose
	// timestamp was absent or did not parse, so their times read as zero
	// rather than as a start before the window. A follower counts every
	// record it folded since its last rescan.
	MalformedRecords int
	UntimedRecords   int
}

const (
	// MaxRecentTurns bounds TurnScan.Recent.
	MaxRecentTurns = 8
	// MaxTurnIDLen bounds a turn id. A longer one is not an identity: it
	// counts as missing (MissingIDs), never truncated or hashed, because a
	// changed id would not match the one other observers of the turn report
	// (X11 D2).
	MaxTurnIDLen = 128
	// turnScanBudget bounds how far back from the end ScanTurns reads: the
	// Codex activity scan's budget.
	turnScanBudget = codexLifecycleScanBudget
	// turnRacyModWindow covers the coarsest modification-time granularity
	// of the filesystems a transcript lives on.
	turnRacyModWindow = 2 * time.Second
)

var (
	// ErrTurnsUnsupported reports a provider family whose transcript is not
	// read for turns.
	ErrTurnsUnsupported = errors.New("transcript family records no turn identity")
	// ErrTornTurnRecord reports a transcript whose last record is incomplete.
	// The record may be the next turn's start, so the scan cannot say idle.
	ErrTornTurnRecord = errors.New("transcript ends in a torn record")
	// ErrTurnStartBeyondBudget reports a transcript whose scan window, cut
	// off by the budget, holds no turn record, so the current turn cannot be
	// told. A Gemini JSONL window must also hold a snapshot: without one, a
	// user line cannot be told from an edit of a message before the window.
	ErrTurnStartBeyondBudget = errors.New("no turn record within the transcript scan budget")
	// ErrTurnTranscriptBeyondBudget reports a whole-document transcript, read
	// whole rather than backward, that is larger than the scan budget.
	ErrTurnTranscriptBeyondBudget = errors.New("whole-document transcript exceeds the turn scan budget")
)

// ScanTurns reads the turns of provider's transcript at path: the open turn
// and up to MaxRecentTurns ended ones, newest first, from the last 16 MiB of
// the file. A torn last record, a truncated window holding no turn record, or
// a file that cannot be read is an error, never an idle scan. A scan reads up
// to the whole window; a caller that reads one transcript repeatedly should
// hold a TurnFollower, which reads only appended bytes.
func ScanTurns(provider, path string) (TurnScan, error) {
	format, err := turnFormatFor(provider)
	if err != nil {
		return TurnScan{}, err
	}
	fold, _, _, err := scanTurnFile(format, path, turnScanBudget)
	if err != nil {
		return TurnScan{}, err
	}
	return fold.scan(), nil
}

func turnFormatFor(provider string) (turnFormat, error) {
	fam, ok := turnFamilies[turnFamilyKey(provider)]
	if !ok || fam.format == nil {
		return nil, fmt.Errorf("%w: %q", ErrTurnsUnsupported, provider)
	}
	return fam.format, nil
}

// turnFormat reads one family's transcript format.
type turnFormat interface {
	// scan reads the last budget bytes of the size-byte transcript r and
	// returns the fold of its turn records.
	scan(path string, r io.ReaderAt, size, budget int64) (turnFold, error)
}

// turnFold is the turn state folded from a transcript's records, oldest first.
type turnFold interface {
	// add folds one complete record appended after the folded ones.
	add(line []byte)
	// appendable reports whether the transcript grows only by appended
	// records, so add can follow it.
	appendable() bool
	scan() TurnScan
}

// scanTurnFile opens path and scans it with format. It returns the fold, the
// file's identity and the size it read up to.
func scanTurnFile(format turnFormat, path string, budget int64) (turnFold, os.FileInfo, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("opening transcript for turns: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file
	info, err := f.Stat()
	if err != nil {
		return nil, nil, 0, fmt.Errorf("reading transcript for turns: %w", err)
	}
	fold, err := format.scan(path, f, info.Size(), budget)
	if err != nil {
		return nil, nil, 0, err
	}
	return fold, info, info.Size(), nil
}

// collectTurnLines reads JSONL records backward from the end of the
// size-byte r, within budget bytes, until visit returns true. It reports
// whether the window stopped at the budget with earlier bytes unread. The
// newest non-blank record must be valid JSON: a torn tail is an error.
func collectTurnLines(r io.ReaderAt, size, budget int64, visit func(line []byte) bool) (bool, error) {
	sawRecord := false
	var torn bool
	stopped, err := eachLineBackward(r, size, budget, func(line []byte) bool {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			return false
		}
		if !sawRecord {
			sawRecord = true
			if !json.Valid(line) {
				torn = true
				return true
			}
		}
		return visit(line)
	})
	switch {
	case err != nil:
		return false, fmt.Errorf("reading transcript for turns: %w", err)
	case torn:
		return false, ErrTornTurnRecord
	}
	return !stopped && size > budget, nil
}

// validTurnID reports raw as a turn id: non-empty, at most MaxTurnIDLen
// bytes, valid UTF-8 and free of control characters (X11 D2).
func validTurnID(raw string) (string, bool) {
	if raw == "" || len(raw) > MaxTurnIDLen || !utf8.ValidString(raw) {
		return "", false
	}
	if slices.ContainsFunc([]rune(raw), unicode.IsControl) {
		return "", false
	}
	return raw, true
}

// parseTurnTime parses a source timestamp, keeping its precision. An absent
// or unreadable timestamp is the zero time, and not ok.
func parseTurnTime(raw string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// turnLedger tracks the open turn and the ended ones, newest first.
type turnLedger struct {
	open       bool
	current    ScannedTurn
	recent     []ScannedTurn
	missingIDs bool
	malformed  int
	untimed    int
}

// begin opens a turn, closing a still-open one at the new start without a
// status.
func (l *turnLedger) begin(id string, at time.Time) {
	l.close(at, "")
	l.open = true
	l.current = ScannedTurn{ID: id, StartedAt: at}
}

// noteTime counts a record whose timestamp did not parse.
func (l *turnLedger) noteTime(ok bool) {
	if !ok {
		l.untimed++
	}
}

// close ends the open turn, if any.
func (l *turnLedger) close(at time.Time, status TurnStatus) {
	l.closeFailed(at, status, TurnFailure{})
}

// closeFailed ends the open turn, if any, with the error it failed on.
func (l *turnLedger) closeFailed(at time.Time, status TurnStatus, failure TurnFailure) {
	if !l.open {
		return
	}
	t := l.current
	t.EndedAt = at
	t.Status = status
	t.Failure = failure
	l.open = false
	l.current = ScannedTurn{}
	l.pushEnded(t)
}

// pushEnded records t as the newest ended turn.
func (l *turnLedger) pushEnded(t ScannedTurn) {
	l.recent = slices.Insert(l.recent, 0, t)
	if len(l.recent) > MaxRecentTurns {
		l.recent = l.recent[:MaxRecentTurns]
	}
}

// isCurrent reports whether id names the open turn.
func (l *turnLedger) isCurrent(id string) bool {
	return l.open && id != "" && l.current.ID == id
}

// endedIndex returns the index in recent of the ended turn id, or -1.
func (l *turnLedger) endedIndex(id string) int {
	if id == "" {
		return -1
	}
	return slices.IndexFunc(l.recent, func(t ScannedTurn) bool { return t.ID == id })
}

func (l *turnLedger) snapshot() TurnScan {
	s := TurnScan{
		Open:             l.open,
		MissingIDs:       l.missingIDs,
		Recent:           slices.Clone(l.recent),
		MalformedRecords: l.malformed,
		UntimedRecords:   l.untimed,
	}
	if l.open {
		s.Current = l.current
	}
	return s
}

// TurnFollower reads one transcript's turns repeatedly. It folds only the
// bytes appended since its last read, and rescans after the file shrinks or
// is replaced. A whole-document transcript is rescanned only when its size or
// modification time changes. A TurnFollower is not safe for concurrent use.
//
// A follower remembers everything it folded since its last rescan, where
// ScanTurns sees only its window. The two report the same turns while every
// turn they report began within the last 16 MiB and, for a Gemini JSONL
// transcript, that window holds a snapshot. Past that, the follower
// still reports such a turn's id and StartedAt, while ScanTurns reports a
// zero StartedAt or ErrTurnStartBeyondBudget. MissingIDs, MalformedRecords
// and UntimedRecords count every record folded since the last rescan. They
// can stay set after ScanTurns, which reads back only as far as the oldest
// turn it reports, no longer sees the record.
type TurnFollower struct {
	path   string
	format turnFormat
	budget int64

	fold   turnFold
	info   os.FileInfo
	offset int64
	// invalidTail is set while the newest record folded is not valid JSON:
	// a rescan reports that as a torn record, so the follower does too.
	invalidTail bool
	// readAt is when the last rescan began.
	readAt time.Time
}

// NewTurnFollower returns a follower of provider's transcript at path. Its
// first Read scans as ScanTurns does.
func NewTurnFollower(provider, path string) (*TurnFollower, error) {
	format, err := turnFormatFor(provider)
	if err != nil {
		return nil, err
	}
	return &TurnFollower{path: path, format: format, budget: turnScanBudget}, nil
}

// Read reports the transcript's turns now. Its errors are ScanTurns' errors.
func (f *TurnFollower) Read() (TurnScan, error) {
	info, err := os.Stat(f.path)
	if err != nil {
		f.reset()
		return TurnScan{}, fmt.Errorf("reading transcript for turns: %w", err)
	}
	if f.unchanged(info) {
		return f.fold.scan(), nil
	}
	if !f.canFollow(info) {
		return f.rescan()
	}
	if info.Size() != f.offset {
		if err := f.foldAppended(info); err != nil {
			return TurnScan{}, err
		}
	}
	if f.invalidTail {
		return TurnScan{}, ErrTornTurnRecord
	}
	return f.fold.scan(), nil
}

// canFollow reports whether info's file continues the one already folded.
func (f *TurnFollower) canFollow(info os.FileInfo) bool {
	return f.fold != nil && f.fold.appendable() && os.SameFile(f.info, info) &&
		info.Size() >= f.offset && info.Size()-f.offset <= f.budget
}

// unchanged reports whether info's file is a whole-document transcript
// already folded, with the same size and modification time. Such a file is
// rewritten rather than appended to, so only a rewrite changes it. A file
// modified within turnRacyModWindow of the last read is never taken as
// unchanged: a rewrite in the same clock tick keeps its modification time.
func (f *TurnFollower) unchanged(info os.FileInfo) bool {
	return f.fold != nil && !f.fold.appendable() && os.SameFile(f.info, info) &&
		info.Size() == f.offset && info.ModTime().Equal(f.info.ModTime()) &&
		info.ModTime().Before(f.readAt.Add(-turnRacyModWindow))
}

func (f *TurnFollower) rescan() (TurnScan, error) {
	readAt := time.Now()
	fold, info, size, err := scanTurnFile(f.format, f.path, f.budget)
	if err != nil {
		f.reset()
		return TurnScan{}, err
	}
	f.fold, f.info, f.offset, f.invalidTail, f.readAt = fold, info, size, false, readAt
	return fold.scan(), nil
}

func (f *TurnFollower) reset() {
	f.fold, f.info, f.offset, f.invalidTail = nil, nil, 0, false
}

// foldAppended folds the records appended since the last read. A torn final
// record is left unread, to be folded once complete, and reported as an
// error. A complete record that is not valid JSON is passed over, and marks
// the tail invalid until a valid record follows it. A record appended to the
// line of one already folded, which was written without its newline, makes
// the two one line to a rescan, so the follower rescans.
func (f *TurnFollower) foldAppended(info os.FileInfo) error {
	file, err := os.Open(f.path)
	if err != nil {
		f.reset()
		return fmt.Errorf("opening transcript for turns: %w", err)
	}
	defer file.Close() //nolint:errcheck // read-only file
	opened, err := file.Stat()
	if err != nil || !os.SameFile(opened, info) || opened.Size() < f.offset {
		_, err := f.rescan()
		return err
	}
	// Read from the byte before the offset, to see whether the last line
	// folded was ended.
	start := max(f.offset-1, 0)
	data := make([]byte, opened.Size()-start)
	if _, err := file.ReadAt(data, start); err != nil {
		f.reset()
		return fmt.Errorf("reading transcript for turns: %w", err)
	}
	if f.offset > 0 {
		ended := data[0] == '\n'
		data = data[1:]
		if line, _, _ := bytes.Cut(data, []byte{'\n'}); !ended && len(bytes.TrimSpace(line)) > 0 {
			_, err := f.rescan()
			return err
		}
	}
	for len(data) > 0 {
		line, rest, complete := bytes.Cut(data, []byte{'\n'})
		if record := bytes.TrimSpace(line); len(record) > 0 {
			valid := json.Valid(record)
			if !complete && !valid {
				return ErrTornTurnRecord
			}
			f.fold.add(record)
			f.invalidTail = !valid
		}
		f.offset += int64(len(line))
		if complete {
			f.offset++
		}
		data = rest
	}
	f.info = opened
	return nil
}
