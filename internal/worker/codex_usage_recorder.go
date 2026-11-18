package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

// codexUsageEmitFloorSlack is how far before its anchor (the awake interval's
// start, or the seam's wake time) a fresh Codex read still emits usage. It
// matches the one-minute slack keyless rollout discovery allows before the
// wake.
const codexUsageEmitFloorSlack = time.Minute

// codexUsageEmitFloor is the earliest invocation time a fresh read of a
// session's rollout emits: anchor minus the discovery slack. A zero anchor has
// no floor.
func codexUsageEmitFloor(anchor time.Time) time.Time {
	if anchor.IsZero() {
		return time.Time{}
	}
	return anchor.Add(-codexUsageEmitFloorSlack)
}

// codexUsagePass is one bounded pass of the Codex usage recorder that the
// prompt-op seam and every model-usage sweep share. It reads the session's
// rollout forward from its committed checkpoint, hands each new invocation to
// record oldest first, and commits the checkpoint and flat cursor through the
// last invocation record accepted.
type codexUsagePass struct {
	manager *sessionpkg.Manager
	adapter SessionLogAdapter
	limits  sessionlog.CodexUsagePageLimits
	id      string
	path    string
	// emitFloor is the floor a read that starts from byte zero takes; a
	// resumed read keeps the floor its checkpoint carries.
	emitFloor time.Time
	// record emits one invocation (metrics and the usage fact). An error stops
	// the pass before that invocation, which stays pending.
	record func(u sessionlog.TailUsage) error
}

// run executes the pass. emitted counts invocations record accepted. settled
// is true only when every invocation the rollout holds up to the pass's size
// snapshot was read and recorded and the progress committed: a backlog left
// for a later pass, a page issue, a record failure or a failed commit leaves
// it false. err is the record failure, the read failure or the commit
// failure.
func (p codexUsagePass) run(ctx context.Context) (emitted int, settled bool, err error) {
	if p.manager == nil {
		return 0, false, fmt.Errorf("codex usage for session %s: no session manager", p.id)
	}
	progress, err := p.manager.InvocationUsageProgress(p.id)
	if err != nil {
		return 0, false, fmt.Errorf("reading codex usage progress for session %s: %w", p.id, err)
	}
	from := p.resumePoint(progress)
	page, err := p.adapter.CodexUsagePage(ctx, sessionlog.CodexUsagePageRequest{
		Path:      p.path,
		From:      from,
		EmitFloor: p.emitFloor,
		Limits:    p.limits,
	})
	if err != nil {
		return 0, false, fmt.Errorf("reading codex usage page for session %s: %w", p.id, err)
	}

	next := page.Checkpoint
	cursor := progress.Cursor
	var recordErr error
	for i, entry := range page.Entries {
		if recordErr = p.record(entry.Usage); recordErr != nil {
			// The failed invocation and everything after it stay pending: the
			// commit stops at the last invocation recorded, or commits nothing
			// when the first one failed.
			if i == 0 {
				return 0, false, recordErr
			}
			next = page.Entries[i-1].Checkpoint
			break
		}
		emitted++
		cursor = usageIdentity(entry.Usage)
	}
	if commitErr := p.commit(progress, next, cursor); commitErr != nil {
		return emitted, false, errors.Join(recordErr, commitErr)
	}
	if recordErr != nil {
		return emitted, false, recordErr
	}
	if len(page.Issues) > 0 {
		slog.Warn("codex usage: rollout pass incomplete; will retry",
			slog.String("session_id", p.id), slog.String("path", p.path),
			slog.Any("issues", page.Issues), slog.Int64("offset", page.Checkpoint.Offset))
	}
	return emitted, !page.Incomplete(), nil
}

// resumePoint is the checkpoint a pass resumes from. A checkpoint that does
// not decode, or whose mirrored cursor no longer matches the flat cursor
// (another writer moved it, or an older gc that keeps only the cursor ran), is
// not trusted: the pass replays the rollout from byte zero under its emit
// floor, and the facts it re-emits carry their original keys.
func (p codexUsagePass) resumePoint(progress sessionpkg.InvocationUsageProgress) sessionlog.CodexUsageCheckpoint {
	cp, err := sessionlog.ParseCodexUsageCheckpoint(progress.CodexCheckpoint)
	if err != nil {
		slog.Warn("codex usage: stored checkpoint unreadable; replaying rollout",
			slog.String("session_id", p.id), slog.Any("error", err))
		return sessionlog.CodexUsageCheckpoint{}
	}
	if !cp.IsZero() && cp.Cursor != progress.Cursor {
		slog.Info("codex usage: checkpoint does not match the usage cursor; replaying rollout",
			slog.String("session_id", p.id), slog.String("cursor", progress.Cursor), slog.String("checkpoint_cursor", cp.Cursor))
		return sessionlog.CodexUsageCheckpoint{}
	}
	return cp
}

// commit stores next (mirroring cursor) against the progress the pass read.
// An unchanged checkpoint and cursor write nothing.
func (p codexUsagePass) commit(read sessionpkg.InvocationUsageProgress, next sessionlog.CodexUsageCheckpoint, cursor string) error {
	next.Cursor = cursor
	raw, err := sessionlog.MarshalCodexUsageCheckpoint(next)
	if err != nil {
		return err
	}
	want := sessionpkg.InvocationUsageProgress{Cursor: cursor, CodexCheckpoint: raw}
	if want == read {
		return nil
	}
	if err := p.manager.CommitInvocationUsageProgress(p.id, read, want); err != nil {
		return fmt.Errorf("committing codex usage progress for session %s: %w", p.id, err)
	}
	return nil
}
