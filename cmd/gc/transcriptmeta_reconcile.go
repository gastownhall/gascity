package main

import (
	"context"
	"fmt"

	storebindingsqlite "github.com/gastownhall/gascity/internal/storebinding/sqlite"
)

// transcriptMetaReconcilePageSize bounds each background batch's exact-path
// resolution and sidecar writes. The worker owns the immutable lexical snapshot
// and all provider-specific keyed lookup semantics.
const transcriptMetaReconcilePageSize = 64

// startHistoricalTranscriptMetaReconcile launches the one supervisor-lifetime
// historical sidecar pass. It is intentionally a CityRuntime helper, never a
// CLI command. The machine-wide supervisor is the sole composition root that
// sets transcriptMetaEnabled after arming the correlated event stream.
//
// The snapshot is processed promptly in bounded batches outside controller
// ticks. Completion is terminal for this supervisor lifetime: new sessions use
// the post-turn retry; a restart rebuilds the authoritative snapshot and safely
// retries any sidecar whose write previously failed.
func (cr *CityRuntime) startHistoricalTranscriptMetaReconcile(ctx context.Context) {
	if cr == nil || !cr.transcriptMetaEnabled || cr.cfg == nil {
		return
	}
	cr.transcriptMetaMu.Lock()
	if cr.transcriptMetaStarted {
		cr.transcriptMetaMu.Unlock()
		return
	}
	cr.transcriptMetaStarted = true
	done := make(chan struct{})
	cr.transcriptMetaDone = done
	cr.transcriptMetaMu.Unlock()

	go func() {
		defer close(done)
		if err := ctx.Err(); err != nil {
			return
		}
		sessionStore := cr.sessionsBeadStore()
		if sessionStore.Store == nil {
			return
		}
		factory, err := workerFactoryWithConfig(cr.cityPath, sessionStore.Store, cr.sp, cr.cfg)
		if err != nil {
			fmt.Fprintf(cr.stderr, "%s: transcript metadata: building worker factory: %v\n", cr.logPrefix, err) //nolint:errcheck // best-effort patrol diagnostic
			return
		}
		legacy, err := storebindingsqlite.ReadLegacySessionsSnapshot(cr.cityPath)
		if err != nil {
			fmt.Fprintf(cr.stderr, "%s: transcript metadata: legacy sessions snapshot: %v\n", cr.logPrefix, err) //nolint:errcheck // fail closed: no partial historical pass
			return
		}
		reconciler, err := factory.NewTranscriptMetaReconcilerWithSupplemental(transcriptMetaReconcilePageSize, legacy)
		if err != nil {
			fmt.Fprintf(cr.stderr, "%s: transcript metadata: snapshot: %v\n", cr.logPrefix, err) //nolint:errcheck // best-effort patrol diagnostic
			return
		}
		if reconciler == nil {
			return
		}
		for {
			page, complete, nextErr := reconciler.Next(ctx)
			if nextErr != nil {
				if ctx.Err() == nil {
					fmt.Fprintf(cr.stderr, "%s: transcript metadata: batch: %v\n", cr.logPrefix, nextErr) //nolint:errcheck // best-effort patrol diagnostic
				}
				return
			}
			if page.WriteFailures > 0 {
				fmt.Fprintf(cr.stderr, "%s: transcript metadata: %d sidecar write failure(s); restart retries failed records (first: %v)\n", cr.logPrefix, page.WriteFailures, page.FirstWriteErr) //nolint:errcheck // best-effort patrol diagnostic
			}
			if complete {
				return
			}
		}
	}()
}
