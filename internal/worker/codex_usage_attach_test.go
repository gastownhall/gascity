package worker

import (
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/sessionlog"
)

var codexAttachBase = time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

func codexAttachAt(step int) *time.Time {
	ts := codexAttachBase.Add(time.Duration(step) * time.Second)
	return &ts
}

func codexAttachUsage(step, output int) sessionlog.TailUsage {
	return sessionlog.TailUsage{
		EntryUUID:    codexAttachAt(step).Format(time.RFC3339Nano),
		Model:        "gpt-5.5",
		OutputTokens: output,
	}
}

func codexAttachEntry(id string, actor Actor, step int) HistoryEntry {
	return HistoryEntry{ID: id, Actor: actor, Timestamp: codexAttachAt(step)}
}

// A token_count with no new assistant row since the previous one has nowhere
// to land: it must not fall back onto an earlier reasoning row of that turn.
func TestAttachTailUsageSurplusUsageSkipsOlderReasoning(t *testing.T) {
	entries := []HistoryEntry{
		codexAttachEntry("user-1", ActorUser, 0),
		codexAttachEntry("reasoning-1", ActorAssistant, 1),
		codexAttachEntry("answer-1", ActorAssistant, 2),
		codexAttachEntry("reasoning-2", ActorAssistant, 5),
		codexAttachEntry("answer-2", ActorAssistant, 6),
	}
	got := attachTailUsageToAssistantEntries(entries, []sessionlog.TailUsage{
		codexAttachUsage(3, 1),
		codexAttachUsage(4, 2), // surplus: no assistant row since step 3
		codexAttachUsage(7, 3),
	})
	want := map[string]int{"answer-1": 1, "answer-2": 3}
	for _, entry := range got {
		output, ok := want[entry.ID]
		switch {
		case ok && (entry.Usage == nil || entry.Usage.OutputTokens != output):
			t.Errorf("%s usage = %+v, want output %d", entry.ID, entry.Usage, output)
		case !ok && entry.Usage != nil:
			t.Errorf("%s usage = %+v, want none", entry.ID, entry.Usage)
		}
	}
}

// Attachment must be one linear pass even when token_count records far
// outnumber assistant rows; a per-usage rescan from row 0 is quadratic here.
func TestAttachTailUsageLinearWhenUsageOutnumbersAssistants(t *testing.T) {
	const turns = 100_000
	entries := make([]HistoryEntry, 0, 2*turns)
	usages := make([]sessionlog.TailUsage, 0, 2*turns)
	for i := 0; i < turns; i++ {
		entries = append(entries,
			codexAttachEntry(fmt.Sprintf("reasoning-%d", i), ActorAssistant, 3*i),
			codexAttachEntry(fmt.Sprintf("answer-%d", i), ActorAssistant, 3*i+1))
		usages = append(usages, codexAttachUsage(3*i+2, i+1))
	}
	for i := 0; i < turns; i++ {
		usages = append(usages, codexAttachUsage(3*turns+i, -1))
	}

	done := make(chan []HistoryEntry, 1)
	go func() { done <- attachTailUsageToAssistantEntries(entries, usages) }()
	var got []HistoryEntry
	select {
	case got = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("attachment did not finish in 30s; scan is not linear")
	}
	for i := 0; i < turns; i++ {
		reasoning, answer := got[2*i], got[2*i+1]
		if reasoning.Usage != nil {
			t.Fatalf("%s got usage %+v", reasoning.ID, reasoning.Usage)
		}
		if answer.Usage == nil || answer.Usage.OutputTokens != i+1 {
			t.Fatalf("%s usage = %+v, want output %d", answer.ID, answer.Usage, i+1)
		}
	}
}
