package api

import "github.com/gastownhall/gascity/internal/worker"

// settledHistorySnapshot returns snapshot without its partial entries.
//
// The conversation and raw live streams send each entry once, after the
// last entry id they sent, and their frames carry no id a client could use
// to replace an earlier frame. An entry a provider is still growing in place
// (worker.ResultStatusPartial, for example an ACP agent message still
// streaming chunks) would be sent truncated and never corrected, so these
// streams hold it back until it settles. The structured stream upserts by
// entry id and keeps partial entries.
//
// A transcript that stops growing with a run still open never settles it, so
// these two streams withhold its last entry for the life of the live session.
// An ACP capture marks any open chunk run partial, including a post-end_turn
// agent run that may be the session's final record. Releasing such an entry
// needs the quiescence signal only the consumer has -- the reader cannot tell
// a dead file from a growing one, while handler_agent_output_stream.go
// already tracks the transcript size across poll intervals -- so ga-xqv68
// owns that. Closed-session snapshots and the structured stream are
// unaffected.
func settledHistorySnapshot(snapshot *worker.HistorySnapshot) *worker.HistorySnapshot {
	if snapshot == nil {
		return nil
	}
	partial := false
	for _, entry := range snapshot.Entries {
		if entry.Status == worker.ResultStatusPartial {
			partial = true
			break
		}
	}
	if !partial {
		return snapshot
	}
	settled := *snapshot
	settled.Entries = make([]worker.HistoryEntry, 0, len(snapshot.Entries))
	for _, entry := range snapshot.Entries {
		if entry.Status != worker.ResultStatusPartial {
			settled.Entries = append(settled.Entries, entry)
		}
	}
	return &settled
}
