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
//
// An ACP tool_use entry is never partial, yet the capture reader rewrites it
// in place when a later tool_call_update brings the tool's input or a better
// title, so these streams (and the agent-output streams, which also send each
// entry once) keep the first rendering they sent. The structured stream
// delivers the refined entry, as an upsert or a history_rewritten reset, and
// any new snapshot shows it. Holding a tool_use back until its call reaches a
// terminal status would lose it instead: the cursor is positional, so an
// entry withheld behind a later sent entry is never sent. Appending the
// refinement as an entry of its own, as the reader's turnEnd does for a stop
// reason, is the safe remedy; ga-t0c4nz owns that.
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
