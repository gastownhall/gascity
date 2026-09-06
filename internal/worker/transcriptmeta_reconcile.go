package worker

import (
	"context"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/transcriptmeta"
)

// transcriptMetaReconcileDefaultPageSize bounds one background batch's exact
// path resolution and sidecar writes.
const transcriptMetaReconcileDefaultPageSize = 64

// TranscriptMetaReconcilePage reports one bounded historical sidecar batch.
// Written means transcriptmeta.Write confirmed a current sidecar (new or
// already idempotently current). The exact keyed resolver may inspect Codex's
// existing session_meta identity header, but this reconciliation never parses
// conversation/event bodies.
type TranscriptMetaReconcilePage struct {
	Scanned       int
	Resolved      int
	Written       int
	WriteFailures int
	FirstWriteErr error
}

// TranscriptMetaReconciler owns one supervisor-lifetime immutable session
// snapshot. It progresses through that snapshot exactly once in bounded batches;
// completion is terminal until a supervisor restart deliberately builds a fresh,
// idempotent snapshot. New sessions are covered by the existing post-turn retry.
type TranscriptMetaReconciler struct {
	manager     *sessionpkg.Manager
	searchPaths []string
	infos       []sessionpkg.Info
	pageSize    int
	next        int
}

// NewTranscriptMetaReconciler captures one authoritative typed session snapshot
// for the enabled supervisor-only historical pass. It makes no transcript reads
// itself. A disabled process returns nil so one-shot callers remain inert.
func (f *Factory) NewTranscriptMetaReconciler(pageSize int) (*TranscriptMetaReconciler, error) {
	return f.NewTranscriptMetaReconcilerWithSupplemental(pageSize, nil)
}

// NewTranscriptMetaReconcilerWithSupplemental accepts read-only historical
// session rows from the controller composition root. It never opens a storage
// provider; session owns projection and current-store-wins deduplication.
func (f *Factory) NewTranscriptMetaReconcilerWithSupplemental(pageSize int, supplemental []beads.Bead) (*TranscriptMetaReconciler, error) {
	if !transcriptmeta.Enabled() {
		return nil, nil
	}
	if f == nil || f.manager == nil {
		return nil, fmt.Errorf("%w: manager is required", ErrHandleConfig)
	}
	if pageSize <= 0 {
		pageSize = transcriptMetaReconcileDefaultPageSize
	}
	infos, err := f.manager.PersistedTranscriptMetaSnapshotWithSupplemental(supplemental)
	if err != nil {
		return nil, err
	}
	return &TranscriptMetaReconciler{
		manager:     f.manager,
		searchPaths: append([]string(nil), f.searchPaths...),
		infos:       infos,
		pageSize:    pageSize,
	}, nil
}

// Next processes at most one exact-key batch. A per-sidecar write failure is
// recorded in the result and does not block later records: its only bounded
// retry is the next supervisor lifetime's idempotent replay. Snapshot/read
// failures and cancellation are returned so the supervisor can report them and
// stop safely without pretending the pass completed.
func (r *TranscriptMetaReconciler) Next(ctx context.Context) (TranscriptMetaReconcilePage, bool, error) {
	if r == nil || r.next >= len(r.infos) {
		return TranscriptMetaReconcilePage{}, true, nil
	}
	if err := ctx.Err(); err != nil {
		return TranscriptMetaReconcilePage{}, false, err
	}
	end := r.next + r.pageSize
	if end > len(r.infos) {
		end = len(r.infos)
	}
	infos := r.infos[r.next:end]
	r.next = end
	result := TranscriptMetaReconcilePage{Scanned: len(infos)}
	paths := r.manager.KeyedTranscriptPaths(infos, r.searchPaths)
	for _, info := range infos {
		if err := ctx.Err(); err != nil {
			return result, false, err
		}
		path := strings.TrimSpace(paths[info.ID])
		if path == "" {
			continue
		}
		result.Resolved++
		ok, err := transcriptmeta.Write(path, info.ID)
		if err != nil {
			result.WriteFailures++
			if result.FirstWriteErr == nil {
				result.FirstWriteErr = fmt.Errorf("session %s: %w", info.ID, err)
			}
			continue
		}
		if ok {
			result.Written++
		}
	}
	return result, r.next >= len(r.infos), nil
}
