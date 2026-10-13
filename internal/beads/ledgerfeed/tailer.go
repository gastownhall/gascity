// Package ledgerfeed tails a bead ledger's durable mutation journal
// (journalops.Journal) from a checkpoint the caller can persist.
//
// It is the one read-side library every journal consumer shares: the
// controller's cache-coherence feed, and later the relay and the shard owner.
// It moves pages and proves nothing was skipped; what a page MEANS is the
// caller's Apply. It imports the journal's read role and nothing of gc's store
// layer, so any journalops.Journal — a native store, a proxied store, an HTTP
// client of `bd serve` — can feed it.
//
// # What a checkpoint proves
//
// The journal's seq is a commit-ordered counter, and a page read from an
// exclusive checkpoint is gapless. Two failures break that chain without
// leaving an empty page as a clue: a prune below the checkpoint (the journal
// reports it as a typed *journalops.TruncatedError), and a counter that went
// away — the head regressed below the checkpoint, or the journal was re-seeded
// at its old high-water mark so the head looks fine while the row the
// checkpoint names is a different row or gone. The checkpoint therefore
// carries the fingerprint of the row it names, and the tailer re-reads that
// row on resume and every VerifyEvery idle polls.
//
// # What a gap does
//
// On any gap the tailer re-baselines at the journal's current head, THEN calls
// OnGap. It never replays from seq 0: a consumer that saw a gap must resync
// from its source of truth (a full scan), and rows committed between the new
// baseline and that resync are delivered again by the next read, which an
// idempotent Apply absorbs.
package ledgerfeed

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/steveyegge/beads/journalops"
)

// Defaults for the zero-valued Tailer knobs.
const (
	DefaultPageLimit   = 500
	DefaultPoll        = 250 * time.Millisecond
	DefaultIdlePoll    = time.Second
	DefaultVerifyEvery = 20
)

// Fingerprint identifies the journal row at Seq. Seq is the identity; IssueID
// and TS tell a re-seeded counter's row at the same seq from the original.
type Fingerprint struct {
	Seq     int64
	IssueID string
	TS      string
}

// Checkpoint is the tailer's resumable position: every row with a seq at or
// below Seq has been applied. Last fingerprints the row at Seq; it is unknown
// (and verification is skipped) when Last.Seq != Seq, which is the case at
// seq 0 and after a re-baseline onto a row the journal no longer retains.
type Checkpoint struct {
	Seq  int64
	Last Fingerprint
}

// verifiable reports whether the checkpoint names a row it can check.
func (c Checkpoint) verifiable() bool {
	return c.Seq > 0 && c.Last.Seq == c.Seq
}

// GapKind names why the chain from a checkpoint to the journal broke.
type GapKind string

// The gap kinds.
const (
	// GapTruncated: rows after the checkpoint were pruned before they were
	// read (*journalops.TruncatedError).
	GapTruncated GapKind = "truncated"
	// GapHeadRegressed: the journal's head is below the checkpoint, so the
	// counter the checkpoint was taken against no longer exists.
	GapHeadRegressed GapKind = "head_regressed"
	// GapReset: the row at the checkpoint seq vanished or carries a different
	// fingerprint — a counter re-seeded at (or above) the old high-water mark,
	// which a head comparison alone cannot see.
	GapReset GapKind = "reset"
)

// Gap describes one detected break and the baseline the tailer moved to.
type Gap struct {
	Kind GapKind
	// From is the checkpoint the break was detected at.
	From Checkpoint
	// To is the checkpoint the tailer re-baselined to: the journal head.
	To Checkpoint
	// Head is the journal head observed with the break.
	Head int64
	// Floor is the lowest seq the journal still retains, for GapTruncated.
	Floor int64
	// Err is the journal's error for GapTruncated, nil otherwise.
	Err error
}

// Stats is a snapshot of the tailer's progress.
type Stats struct {
	RowsApplied int64
	Pages       int64
	// Seq is the checkpoint seq; Head the last journal head observed.
	Seq  int64
	Head int64
	// Lag is Head - Seq, floored at zero.
	Lag int64
	// Errors counts failed reads and failed Apply calls.
	Errors int64
	// Gaps counts detected gaps by kind.
	Gaps map[GapKind]int64
}

// Tailer pages a journal from a checkpoint and hands each page to Apply.
//
// Configure the exported fields before the first Step or Run and leave them
// alone afterwards. Step and Run must not run concurrently with each other;
// Checkpoint and Stats may be called from any goroutine, including Apply.
type Tailer struct {
	// Source is the journal read. Required.
	Source journalops.Journal
	// Apply receives each non-empty page in seq order. The checkpoint
	// advances past a page only when Apply returns nil; an error retries the
	// same page. Required.
	Apply func(ctx context.Context, page journalops.Page) error
	// OnGap is told about every gap, after the tailer re-baselined. Optional.
	OnGap func(Gap)
	// OnError is told about every failed read or Apply that Run retries.
	// Optional.
	OnError func(error)
	// PageLimit caps rows per read. Zero is DefaultPageLimit.
	PageLimit int
	// Poll is the delay after an idle read. Zero is DefaultPoll.
	Poll time.Duration
	// IdlePoll caps the idle delay, which doubles from Poll on consecutive
	// idle reads. Zero is DefaultIdlePoll.
	IdlePoll time.Duration
	// VerifyEvery re-checks the checkpoint fingerprint every N consecutive
	// idle reads. Zero is DefaultVerifyEvery.
	VerifyEvery int
	// Sleep waits d or until ctx is done. Nil uses a timer; tests inject one.
	Sleep func(ctx context.Context, d time.Duration) error

	stepMu sync.Mutex

	mu         sync.Mutex
	checkpoint Checkpoint
	verify     bool
	idleReads  int
	delay      time.Duration
	stats      Stats
}

// Resume sets the checkpoint to a previously persisted one. Its fingerprint is
// verified before the next read, so a journal reset while the caller was away
// surfaces as a gap rather than as silently skipped rows.
func (t *Tailer) Resume(cp Checkpoint) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.checkpoint = cp
	t.verify = cp.verifiable()
	t.idleReads = 0
	t.stats.Seq = cp.Seq
}

// Baseline moves the checkpoint to the journal's current head without
// applying anything, for a caller that has just read its source of truth and
// wants only what commits after. It returns the new checkpoint.
func (t *Tailer) Baseline(ctx context.Context) (Checkpoint, error) {
	if err := t.validate(); err != nil {
		return Checkpoint{}, err
	}
	t.stepMu.Lock()
	defer t.stepMu.Unlock()
	head, err := t.readHead(ctx)
	if err != nil {
		return Checkpoint{}, err
	}
	cp, err := t.checkpointAt(ctx, head)
	if err != nil {
		return Checkpoint{}, err
	}
	t.Resume(cp)
	t.mu.Lock()
	t.verify = false
	t.observeHead(head)
	t.mu.Unlock()
	return cp, nil
}

// Checkpoint returns the current checkpoint, for a caller to persist.
func (t *Tailer) Checkpoint() Checkpoint {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.checkpoint
}

// Stats returns a snapshot of the tailer's counters.
func (t *Tailer) Stats() Stats {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := t.stats
	out.Gaps = make(map[GapKind]int64, len(t.stats.Gaps))
	for kind, n := range t.stats.Gaps {
		out.Gaps[kind] = n
	}
	return out
}

// Step runs one read-and-apply. It reports more=true when the caller should
// read again at once: the page was full, or the head is still ahead. A gap is
// handled inside Step (re-baseline, OnGap) and reports more=true so the next
// read starts from the new baseline. An error leaves the checkpoint where it
// was.
func (t *Tailer) Step(ctx context.Context) (more bool, err error) {
	if err := t.validate(); err != nil {
		return false, err
	}
	t.stepMu.Lock()
	defer t.stepMu.Unlock()

	t.mu.Lock()
	cp := t.checkpoint
	verify := t.verify
	t.mu.Unlock()

	if verify {
		gap, err := t.verifyCheckpoint(ctx, cp)
		if err != nil {
			return false, t.fail(err)
		}
		if gap != nil {
			return true, t.rebaseline(ctx, *gap)
		}
		t.mu.Lock()
		t.verify = false
		t.mu.Unlock()
	}

	page, err := t.Source.ReadEventsJournalPage(ctx, cp.Seq, t.pageLimit())
	if err != nil {
		var truncated *journalops.TruncatedError
		if errors.As(err, &truncated) {
			if truncated.Head < cp.Seq {
				return true, t.rebaseline(ctx, Gap{Kind: GapHeadRegressed, From: cp, Head: truncated.Head})
			}
			return true, t.rebaseline(ctx, Gap{
				Kind:  GapTruncated,
				From:  cp,
				Head:  truncated.Head,
				Floor: truncated.Floor,
				Err:   err,
			})
		}
		return false, t.fail(fmt.Errorf("ledgerfeed: reading journal after seq %d: %w", cp.Seq, err))
	}
	if page.Head < cp.Seq {
		return true, t.rebaseline(ctx, Gap{Kind: GapHeadRegressed, From: cp, Head: page.Head})
	}
	if len(page.Rows) == 0 {
		t.noteIdle(page.Head)
		return false, nil
	}
	if err := t.Apply(ctx, page); err != nil {
		return false, t.fail(fmt.Errorf("ledgerfeed: applying journal rows %d..%d: %w",
			page.Rows[0].Seq, page.Rows[len(page.Rows)-1].Seq, err))
	}
	last := page.Rows[len(page.Rows)-1]
	next := Checkpoint{Seq: last.Seq, Last: fingerprintOf(last)}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.checkpoint = next
	t.idleReads = 0
	t.delay = 0
	t.stats.RowsApplied += int64(len(page.Rows))
	t.stats.Pages++
	t.stats.Seq = next.Seq
	t.observeHead(page.Head)
	full := len(page.Rows) >= t.pageLimit()
	return full || page.Head > next.Seq, nil
}

// Run steps until ctx is done, re-reading at once while more is reported and
// waiting Poll (doubling to IdlePoll) after an idle read or a failure. It
// returns the context's error.
func (t *Tailer) Run(ctx context.Context) error {
	if err := t.validate(); err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		more, err := t.Step(ctx)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if t.OnError != nil {
				t.OnError(err)
			}
		}
		if more && err == nil {
			continue
		}
		if err := t.sleep(ctx, t.nextDelay()); err != nil {
			return err
		}
	}
}

// verifyCheckpoint re-reads the row the checkpoint names. It returns a gap
// when the row is gone or different, or when the head regressed below it.
// A checkpoint row pruned by retention cannot be checked and is not a gap by
// itself; the read that follows reports truncation if rows after it are gone.
func (t *Tailer) verifyCheckpoint(ctx context.Context, cp Checkpoint) (*Gap, error) {
	page, err := t.Source.ReadEventsJournalPage(ctx, cp.Seq-1, 1)
	if err != nil {
		var truncated *journalops.TruncatedError
		if errors.As(err, &truncated) {
			if truncated.Head < cp.Seq {
				return &Gap{Kind: GapHeadRegressed, From: cp, Head: truncated.Head}, nil
			}
			return nil, nil
		}
		return nil, fmt.Errorf("ledgerfeed: verifying checkpoint seq %d: %w", cp.Seq, err)
	}
	if page.Head < cp.Seq {
		return &Gap{Kind: GapHeadRegressed, From: cp, Head: page.Head}, nil
	}
	if len(page.Rows) == 0 || fingerprintOf(page.Rows[0]) != cp.Last {
		return &Gap{Kind: GapReset, From: cp, Head: page.Head}, nil
	}
	return nil, nil
}

// rebaseline moves the checkpoint to the head the gap observed, then tells
// OnGap. A failure to read the new baseline's fingerprint leaves the old
// checkpoint in place, so the next Step detects the same gap again.
func (t *Tailer) rebaseline(ctx context.Context, gap Gap) error {
	to, err := t.checkpointAt(ctx, gap.Head)
	if err != nil {
		return t.fail(fmt.Errorf("ledgerfeed: re-baselining after %s gap: %w", gap.Kind, err))
	}
	gap.To = to
	t.mu.Lock()
	t.checkpoint = to
	t.verify = false
	t.idleReads = 0
	t.delay = 0
	t.stats.Seq = to.Seq
	t.observeHead(gap.Head)
	if t.stats.Gaps == nil {
		t.stats.Gaps = make(map[GapKind]int64)
	}
	t.stats.Gaps[gap.Kind]++
	t.mu.Unlock()
	if t.OnGap != nil {
		t.OnGap(gap)
	}
	return nil
}

// readHead asks the journal for its head: a checkpoint at or above the head
// is caught up and returns no rows, only the head.
func (t *Tailer) readHead(ctx context.Context) (int64, error) {
	page, err := t.Source.ReadEventsJournalPage(ctx, math.MaxInt64, 1)
	if err != nil {
		return 0, fmt.Errorf("ledgerfeed: reading journal head: %w", err)
	}
	return page.Head, nil
}

// checkpointAt builds a checkpoint at seq, fingerprinted when the journal
// still retains that row.
func (t *Tailer) checkpointAt(ctx context.Context, seq int64) (Checkpoint, error) {
	cp := Checkpoint{Seq: seq}
	if seq <= 0 {
		return Checkpoint{}, nil
	}
	page, err := t.Source.ReadEventsJournalPage(ctx, seq-1, 1)
	if err != nil {
		var truncated *journalops.TruncatedError
		if errors.As(err, &truncated) {
			return cp, nil
		}
		return Checkpoint{}, fmt.Errorf("fingerprinting seq %d: %w", seq, err)
	}
	if len(page.Rows) > 0 && page.Rows[0].Seq == seq {
		cp.Last = fingerprintOf(page.Rows[0])
	}
	return cp, nil
}

// noteIdle records an idle read and schedules a fingerprint check every
// VerifyEvery of them.
func (t *Tailer) noteIdle(head int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.observeHead(head)
	t.idleReads++
	if t.idleReads%t.verifyEvery() == 0 && t.checkpoint.verifiable() {
		t.verify = true
	}
}

// fail counts an error and returns it.
func (t *Tailer) fail(err error) error {
	t.mu.Lock()
	t.stats.Errors++
	t.mu.Unlock()
	return err
}

// nextDelay returns the wait before the next read and doubles it for the one
// after, from Poll up to IdlePoll. Rows applied reset it.
func (t *Tailer) nextDelay() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	poll, ceiling := t.poll(), t.idlePoll()
	if t.delay < poll {
		t.delay = poll
	}
	current := t.delay
	t.delay = min(t.delay*2, ceiling)
	return min(current, ceiling)
}

// observeHead records the latest head and the lag it implies. Callers hold mu.
func (t *Tailer) observeHead(head int64) {
	t.stats.Head = head
	t.stats.Lag = max(head-t.checkpoint.Seq, 0)
}

func (t *Tailer) sleep(ctx context.Context, d time.Duration) error {
	if t.Sleep != nil {
		return t.Sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (t *Tailer) validate() error {
	if t.Source == nil {
		return errors.New("ledgerfeed: Tailer.Source is nil")
	}
	if t.Apply == nil {
		return errors.New("ledgerfeed: Tailer.Apply is nil")
	}
	return nil
}

func (t *Tailer) pageLimit() int {
	if t.PageLimit > 0 {
		return t.PageLimit
	}
	return DefaultPageLimit
}

func (t *Tailer) poll() time.Duration {
	if t.Poll > 0 {
		return t.Poll
	}
	return DefaultPoll
}

func (t *Tailer) idlePoll() time.Duration {
	if t.IdlePoll > 0 {
		return max(t.IdlePoll, t.poll())
	}
	return max(DefaultIdlePoll, t.poll())
}

func (t *Tailer) verifyEvery() int {
	if t.VerifyEvery > 0 {
		return t.VerifyEvery
	}
	return DefaultVerifyEvery
}

func fingerprintOf(row journalops.Row) Fingerprint {
	return Fingerprint{Seq: row.Seq, IssueID: row.IssueID, TS: row.TS}
}
