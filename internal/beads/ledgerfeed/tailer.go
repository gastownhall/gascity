// Package ledgerfeed tails a bead ledger's durable mutation journal
// (journalops.Journal) from a checkpoint the caller can persist.
//
// It is the read-side library journal consumers are to share: first the
// controller's cache-coherence feed, then the relay and the shard owner.
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
// at or below its old high-water mark so the head looks fine while the row the
// checkpoint names is a different row or gone. The checkpoint therefore
// carries the fingerprint of the row it names, and EVERY read re-reads that
// row: it asks from Seq-1 for one extra row and checks the first one. The
// check costs one row per read, and it is the only check that catches a reset
// whose new rows overtook the checkpoint between two reads.
//
// The tailer also checks that the rows it applies continue the checkpoint
// one seq at a time, so a Source that does not report truncation the way the
// library does still cannot make it skip rows silently.
//
// # What a gap does
//
// On any gap the tailer reads the journal's current head, hands the Gap —
// with the baseline it will move to — to OnGap, and moves the checkpoint only
// when OnGap returns nil. A gap never replays from seq 0: a consumer that saw
// a gap resyncs from its source of truth (a full scan) inside OnGap, and rows
// committed after the new baseline's head was read are delivered again by the
// next read, which an idempotent Apply absorbs.
//
// # What a lapse does
//
// A journal can stop recording and start again — bd's is switched per scope —
// and the lapse leaves no hole in the seqs: writes made while it was off have
// no rows, so the rows after it continue the checkpoint and no check above can
// see what is missing. The journal's refusal while it is off is the only
// evidence. A Tailer whose Lapse recognizes that refusal marks the checkpoint
// Lapsed on any read that gets it, and the next read that succeeds is a
// GapInactive gap, moved past like any other. A lapse no read saw leaves no
// trace at all: one that began and ended between two reads, or while nothing
// was reading, or before the Source noticed the switch (gc's stores re-check
// it only every few seconds, or later while a store open is in flight;
// beads.ErrEventsJournalInactive says when). So a consumer that must not miss
// a write still reconciles in full now and then.
//
// # Where a tail starts
//
// A new Tailer's checkpoint is seq 0, and a Step from seq 0 reads from the
// start of the journal's retained history — every retained row, which under
// default retention is a hundred thousand or more. That is right for a
// consumer that wants the history. A consumer that resyncs from its own source
// of truth instead (the cache-coherence feed) calls Baseline before its scan,
// or Resume with a persisted checkpoint, before the first Step.
package ledgerfeed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/steveyegge/beads/journalops"
)

// Defaults for the zero-valued Tailer knobs.
const (
	DefaultPageLimit = 500
	DefaultPoll      = 250 * time.Millisecond
	DefaultIdlePoll  = time.Second
)

// Fingerprint identifies the journal row at Seq. Seq is the identity; the
// rest tells a re-seeded counter's row at the same seq from the original. TS
// alone cannot: it is a label, and a re-seeded write can repeat it.
type Fingerprint struct {
	Seq     int64
	IssueID string
	TS      string
	Op      string
	// Digest is the hex SHA-256 of the row's actor and payloads.
	Digest string
}

// Checkpoint is the tailer's resumable position: every row with a seq at or
// below Seq has been applied. Last fingerprints the row at Seq; it is unknown
// (and the row is not re-checked) when Last.Seq != Seq, which is the case at
// seq 0 and after a re-baseline onto a row the journal no longer retains.
//
// Lapsed marks a checkpoint that a read since found the journal not recording
// (Tailer.Lapse); the next read that succeeds is a GapInactive gap. A failed
// Step sets it, so a caller that persists the checkpoint persists it after a
// failed Step too.
type Checkpoint struct {
	Seq    int64
	Last   Fingerprint
	Lapsed bool
}

// verifiable reports whether the checkpoint names a row it can check.
func (c Checkpoint) verifiable() bool {
	return c.Seq > 0 && c.Last.Seq == c.Seq
}

// GapKind names why the chain from a checkpoint to the journal broke.
type GapKind string

// The gap kinds.
const (
	// GapTruncated: rows after the checkpoint are missing — pruned before
	// they were read (*journalops.TruncatedError), or skipped by the Source.
	GapTruncated GapKind = "truncated"
	// GapHeadRegressed: the journal's head is below the checkpoint, so the
	// counter the checkpoint was taken against no longer exists.
	GapHeadRegressed GapKind = "head_regressed"
	// GapReset: the row at the checkpoint seq vanished or carries a different
	// fingerprint — a counter re-seeded at or below the old high-water mark,
	// which a head comparison alone cannot see. A checkpoint row pruned while
	// every later row survives reads the same way and is reported the same
	// way: the two cannot be told apart, and a needless resync is the safe
	// answer.
	GapReset GapKind = "reset"
	// GapInactive: a read found the journal not recording (Tailer.Lapse), so
	// writes made meanwhile have no rows. It is reported by the first read
	// that succeeds after the lapse, whatever that read served.
	GapInactive GapKind = "inactive"
)

// Gap describes one detected break and the baseline the tailer moves to.
type Gap struct {
	Kind GapKind
	// From is the checkpoint the break was detected at.
	From Checkpoint
	// To is the checkpoint the tailer moves to once OnGap returns nil: the
	// journal head read before OnGap was called.
	To Checkpoint
	// Head is the journal head observed with the break.
	Head int64
	// Floor is the lowest seq the read could serve after the break, for
	// GapTruncated.
	Floor int64
	// Err is the journal's error, when the journal reported the break. A
	// GapInactive gap has none: the refusal came on an earlier read, whose
	// Step returned it.
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
	// Errors counts failed reads, Apply calls and OnGap calls.
	Errors int64
	// LastError is the most recent of those failures and LastErrorAt when it
	// happened; a later success does not clear them. They are how a caller
	// that leaves OnError nil (or a doctor check) sees why the tail is
	// failing, not only that it is.
	LastError   error
	LastErrorAt time.Time
	// Gaps counts gaps the tailer moved past, by kind.
	Gaps map[GapKind]int64
}

// Tailer pages a journal from a checkpoint and hands each page to Apply.
//
// Configure the exported fields before the first call and leave them alone
// afterwards. Step, Run, Baseline and Resume serialize on one lock, and Apply
// and OnGap run while it is held, so a callback must not call Step, Run,
// Baseline or Resume. Checkpoint and Stats may be called from anywhere,
// callbacks included.
type Tailer struct {
	// Source is the journal read. Required.
	Source journalops.Journal
	// Apply receives each non-empty page in seq order. The checkpoint
	// advances past a page only when Apply returns nil; an error retries the
	// same page. Apply owns the page's rows: the tailer reads none of them
	// after handing them over. Required.
	Apply func(ctx context.Context, page journalops.Page) error
	// OnGap is told about every gap before the checkpoint moves to gap.To.
	// It resyncs the consumer from its source of truth; an error keeps the
	// old checkpoint, so the same gap is detected and offered again.
	// Required: a tailer that skipped a gap without telling anyone would
	// lose the gap's rows silently.
	OnGap func(ctx context.Context, gap Gap) error
	// OnError is told about every failed Step that Run would retry. Returning
	// a non-nil error stops Run with that error, which is how a caller ends
	// the tail on a refusal it knows to be permanent. Optional; nil retries
	// every error, and Stats keeps the last one (LastError) either way.
	OnError func(err error) error
	// Lapse reports whether a read error means the journal is not recording
	// right now; see the package doc for what a lapse does. A read it matches
	// marks the checkpoint Lapsed. A gc consumer sets it to match
	// beads.ErrEventsJournalInactive. Optional; nil treats every read error as
	// transient, which only a journal that is never switched off makes right.
	Lapse func(err error) bool
	// PageLimit caps rows applied per read. Zero is DefaultPageLimit.
	PageLimit int
	// Poll is the delay after an idle read. Zero is DefaultPoll.
	Poll time.Duration
	// IdlePoll caps the idle delay, which doubles from Poll on consecutive
	// idle reads. Zero is DefaultIdlePoll.
	IdlePoll time.Duration
	// Sleep waits d or until ctx is done. Nil uses a timer; tests inject one.
	Sleep func(ctx context.Context, d time.Duration) error

	stepMu sync.Mutex

	mu         sync.Mutex
	checkpoint Checkpoint
	delay      time.Duration
	// afterGap is set while the last completed Step moved past a gap, so a
	// gap that follows another one backs off instead of re-reading at once.
	afterGap bool
	stats    Stats
}

// Resume sets the checkpoint to a previously persisted one. The next read
// re-checks its fingerprint, so a journal reset while the caller was away
// surfaces as a gap rather than as silently skipped rows. Persist the whole
// Checkpoint: a seq alone cannot detect that reset, and only the Lapsed mark
// carries a lapse across a restart. A lapse that began and ended while nothing
// was reading left no trace for Resume to find.
//
// It waits for a Step in flight, so that Step's commit cannot overwrite the
// restored checkpoint, and refuses a negative seq, which no journal serves.
func (t *Tailer) Resume(cp Checkpoint) error {
	if cp.Seq < 0 {
		return fmt.Errorf("ledgerfeed: resuming at seq %d: a checkpoint seq is never negative", cp.Seq)
	}
	t.stepMu.Lock()
	defer t.stepMu.Unlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.checkpoint = cp
	t.stats.Seq = cp.Seq
	t.observeHead(t.stats.Head)
	return nil
}

// Baseline moves the checkpoint to the journal's current head without
// applying anything, and returns it.
//
// Call it BEFORE reading the consumer's source of truth: a mutation that
// commits between the head read and the scan is then both in the scan and
// delivered by the tailer, which an idempotent Apply absorbs. Baselining
// after the scan would lose a mutation committed in between. For the same
// reason a Baseline clears a lapse: the scan after it covers what was lost.
func (t *Tailer) Baseline(ctx context.Context) (Checkpoint, error) {
	if err := t.validate(); err != nil {
		return Checkpoint{}, err
	}
	t.stepMu.Lock()
	defer t.stepMu.Unlock()
	head, err := t.readHead(ctx)
	if err != nil {
		return Checkpoint{}, t.fail(err)
	}
	cp, err := t.checkpointAt(ctx, head)
	if err != nil {
		return Checkpoint{}, t.fail(err)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.checkpoint = cp
	t.stats.Seq = cp.Seq
	t.observeHead(head)
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
// read again at once: the page was full, the head is still ahead, or a gap was
// just moved past that did not follow another gap. An error reports more=false
// and leaves the checkpoint where it was, except that a read error Lapse
// matches marks it Lapsed.
//
// From the zero checkpoint Step reads from the start of retained history; see
// the package doc for where a tail starts.
func (t *Tailer) Step(ctx context.Context) (more bool, err error) {
	if err := t.validate(); err != nil {
		return false, err
	}
	t.stepMu.Lock()
	defer t.stepMu.Unlock()

	cp := t.Checkpoint()
	since, limit := cp.Seq, t.pageLimit()
	verify := cp.verifiable()
	if verify {
		// Re-read the checkpoint row with the page; see the package doc.
		since, limit = cp.Seq-1, limit+1
	}

	page, err := t.read(ctx, since, limit)
	if err != nil {
		if gap, ok := truncationGap(cp, err); ok {
			return t.movepast(ctx, gap)
		}
		return false, t.fail(fmt.Errorf("ledgerfeed: reading journal after seq %d for the checkpoint at seq %d: %w", since, cp.Seq, err))
	}
	if cp.Lapsed {
		return t.movepast(ctx, Gap{Kind: GapInactive, From: cp, Head: page.Head})
	}
	if page.Head < cp.Seq {
		return t.movepast(ctx, Gap{Kind: GapHeadRegressed, From: cp, Head: page.Head})
	}
	rows := page.Rows
	if verify {
		if len(rows) == 0 || fingerprintOf(rows[0]) != cp.Last {
			return t.movepast(ctx, Gap{Kind: GapReset, From: cp, Head: page.Head})
		}
		rows = rows[1:]
	}
	for i, row := range rows {
		if row.Seq != cp.Seq+1+int64(i) {
			return t.movepast(ctx, Gap{Kind: GapTruncated, From: cp, Head: page.Head, Floor: row.Seq})
		}
	}
	if len(rows) == 0 {
		t.mu.Lock()
		t.observeHead(page.Head)
		caughtUp := page.Head <= cp.Seq
		if caughtUp {
			t.afterGap = false
		}
		t.mu.Unlock()
		if caughtUp {
			return false, nil
		}
		// The head is ahead and the read served nothing past the checkpoint:
		// a Source capped at one row spends it on the verification row. Idling
		// here would stall for good while only the lag grew.
		return false, t.fail(fmt.Errorf("ledgerfeed: journal head %d is past checkpoint %d, but the read after it served no row past the checkpoint "+
			"(the Source must serve the re-read checkpoint row and at least one more per read)", page.Head, cp.Seq))
	}
	// Apply owns the rows once it has them, so the checkpoint is taken first.
	first, last := rows[0].Seq, rows[len(rows)-1]
	next := Checkpoint{Seq: last.Seq, Last: fingerprintOf(last)}
	page.Rows = rows
	if err := t.Apply(ctx, page); err != nil {
		return false, t.fail(fmt.Errorf("ledgerfeed: applying journal rows %d..%d: %w", first, next.Seq, err))
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.checkpoint = next
	t.delay = 0
	t.afterGap = false
	t.stats.RowsApplied += int64(len(rows))
	t.stats.Pages++
	t.stats.Seq = next.Seq
	t.observeHead(page.Head)
	return len(rows) >= t.pageLimit() || page.Head > next.Seq, nil
}

// Run steps until ctx is done or OnError stops it, re-reading at once while
// more is reported and waiting Poll (doubling to IdlePoll) after an idle read
// or a failure. It returns the context's error or OnError's.
//
// Like Step, Run from the zero checkpoint replays retained history; call
// Baseline or Resume first to start elsewhere.
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
				if stop := t.OnError(err); stop != nil {
					return stop
				}
			}
		}
		// A failed Step never re-reads at once, whatever it reported.
		if more && err == nil {
			continue
		}
		if err := t.sleep(ctx, t.nextDelay()); err != nil {
			return err
		}
	}
}

// movepast re-baselines at the head the gap observed, hands the gap to OnGap,
// and commits the new checkpoint only when OnGap accepts it.
//
// It asks for an immediate re-read only when the Step before it was not a gap
// too. A Source that keeps re-triggering gaps (replicas that disagree behind
// one address) would otherwise drive an unbounded loop of reads and consumer
// resyncs; a gap that follows a gap instead leaves the backoff growing.
func (t *Tailer) movepast(ctx context.Context, gap Gap) (bool, error) {
	to, err := t.checkpointAt(ctx, gap.Head)
	if err != nil {
		return false, t.fail(fmt.Errorf("ledgerfeed: re-baselining after %s gap at seq %d: %w", gap.Kind, gap.From.Seq, err))
	}
	gap.To = to
	if err := t.OnGap(ctx, gap); err != nil {
		return false, t.fail(fmt.Errorf("ledgerfeed: %s gap at seq %d not resolved: %w", gap.Kind, gap.From.Seq, err))
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	consecutive := t.afterGap
	t.afterGap = true
	t.checkpoint = to
	if !consecutive {
		t.delay = 0
	}
	t.stats.Seq = to.Seq
	t.observeHead(gap.Head)
	if t.stats.Gaps == nil {
		t.stats.Gaps = make(map[GapKind]int64)
	}
	t.stats.Gaps[gap.Kind]++
	return !consecutive, nil
}

// truncationGap classifies a read the journal refused as truncated; ok is
// false for any other refusal.
func truncationGap(cp Checkpoint, err error) (gap Gap, ok bool) {
	var truncated *journalops.TruncatedError
	if !errors.As(err, &truncated) {
		return Gap{}, false
	}
	gap = Gap{Kind: GapTruncated, From: cp, Head: truncated.Head, Floor: truncated.Floor, Err: err}
	switch {
	case truncated.Head < cp.Seq:
		gap.Kind = GapHeadRegressed
	case cp.verifiable() && truncated.Since < cp.Seq && truncated.Floor <= cp.Seq+1:
		// Only the checkpoint row is gone; every row after it survives.
		gap.Kind = GapReset
	}
	return gap, true
}

// read reads a page from the Source, marking the checkpoint Lapsed when Lapse
// matches the read's error. Callers hold stepMu, so no commit races the mark.
func (t *Tailer) read(ctx context.Context, since int64, limit int) (journalops.Page, error) {
	page, err := t.Source.ReadEventsJournalPage(ctx, since, limit)
	if err != nil && t.Lapse != nil && t.Lapse(err) {
		t.mu.Lock()
		t.checkpoint.Lapsed = true
		t.mu.Unlock()
	}
	return page, err
}

// readHead asks the journal for its head: a checkpoint at or above the head
// is caught up and returns no rows, only the head.
func (t *Tailer) readHead(ctx context.Context) (int64, error) {
	page, err := t.read(ctx, math.MaxInt64, 1)
	if err != nil {
		return 0, fmt.Errorf("ledgerfeed: reading journal head: %w", err)
	}
	return page.Head, nil
}

// checkpointAt builds a checkpoint at seq, fingerprinted when the journal
// still retains that row.
func (t *Tailer) checkpointAt(ctx context.Context, seq int64) (Checkpoint, error) {
	if seq <= 0 {
		return Checkpoint{}, nil
	}
	cp := Checkpoint{Seq: seq}
	page, err := t.read(ctx, seq-1, 1)
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

// fail counts and records an error and returns it.
func (t *Tailer) fail(err error) error {
	t.mu.Lock()
	t.stats.Errors++
	t.stats.LastError = err
	// The wall clock, not an injected one: only a reader of Stats sees it, and
	// nothing the tailer decides depends on it.
	t.stats.LastErrorAt = time.Now()
	t.mu.Unlock()
	return err
}

// nextDelay returns the wait before the next read and doubles it for the one
// after, from Poll up to IdlePoll. Rows applied, or a gap moved past that did
// not follow another gap, reset it.
func (t *Tailer) nextDelay() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	poll, ceiling := t.poll(), t.idlePoll()
	if t.delay < poll {
		t.delay = poll
	}
	current := min(t.delay, ceiling)
	t.delay = min(t.delay*2, ceiling)
	return current
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
	switch {
	case t.Source == nil:
		return errors.New("ledgerfeed: Tailer.Source is nil")
	case t.Apply == nil:
		return errors.New("ledgerfeed: Tailer.Apply is nil")
	case t.OnGap == nil:
		return errors.New("ledgerfeed: Tailer.OnGap is nil")
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

func fingerprintOf(row journalops.Row) Fingerprint {
	digest := sha256.New()
	for _, field := range [...]string{row.Actor, row.IssueJSON, row.DepJSON, row.CommentJSON} {
		// Length-prefixed, so a byte moved from one field to the next shows.
		fmt.Fprintf(digest, "%d:%s", len(field), field) //nolint:errcheck // hash.Write never errors
	}
	return Fingerprint{
		Seq: row.Seq, IssueID: row.IssueID, TS: row.TS, Op: row.Op,
		Digest: hex.EncodeToString(digest.Sum(nil)),
	}
}
