package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/sessionlog"
)

func TestBuildStructuredStreamUpdateStartsWithSnapshot(t *testing.T) {
	projection := testStructuredStreamProjection("stream-a", testStructuredMessage("m1", "partial"))

	update := buildStructuredStreamUpdate("", projection, false)
	if update == nil {
		t.Fatal("update = nil, want initial snapshot")
	}
	if update.Operation != sessionStructuredOperationSnapshot {
		t.Fatalf("operation = %q, want %q", update.Operation, sessionStructuredOperationSnapshot)
	}
	if update.ResetReason != "" {
		t.Fatalf("reset_reason = %q, want empty", update.ResetReason)
	}
	if update.History == nil || update.History.Cursor.ResumeToken == "" {
		t.Fatalf("history cursor = %+v, want resume token", update.History)
	}
	if len(update.StructuredMessages) != 1 || update.StructuredMessages[0].ID != "m1" {
		t.Fatalf("messages = %+v, want full snapshot", update.StructuredMessages)
	}
}

func TestBuildStructuredStreamUpdateSuppressesExactResume(t *testing.T) {
	projection := testStructuredStreamProjection("stream-a", testStructuredMessage("m1", "final"))
	initial := buildStructuredStreamUpdate("", projection, false)

	if got := buildStructuredStreamUpdate(initial.History.Cursor.ResumeToken, projection, false); got != nil {
		t.Fatalf("exact resume update = %+v, want nil", got)
	}
}

func TestBuildStructuredStreamUpdateEmitsInclusiveTailUpsert(t *testing.T) {
	previous := testStructuredStreamProjection("stream-a",
		testStructuredMessage("m1", "final"),
		testStructuredMessage("m2", "partial"),
	)
	initial := buildStructuredStreamUpdate("", previous, false)
	current := testStructuredStreamProjection("stream-a",
		testStructuredMessage("m1", "final"),
		testStructuredMessage("m2", "final"),
		testStructuredMessage("m3", "partial"),
	)

	update := buildStructuredStreamUpdate(initial.History.Cursor.ResumeToken, current, false)
	if update == nil {
		t.Fatal("update = nil, want upsert")
	}
	if update.Operation != sessionStructuredOperationUpsert {
		t.Fatalf("operation = %q, want %q", update.Operation, sessionStructuredOperationUpsert)
	}
	if got := structuredMessageIDs(update.StructuredMessages); !equalStrings(got, []string{"m2", "m3"}) {
		t.Fatalf("upsert message IDs = %v, want [m2 m3]", got)
	}
}

func TestBuildStructuredStreamUpdateEmitsSameIDFinalization(t *testing.T) {
	previous := testStructuredStreamProjection("stream-a", testStructuredMessage("m1", "partial"))
	initial := buildStructuredStreamUpdate("", previous, false)
	current := testStructuredStreamProjection("stream-a", testStructuredMessage("m1", "final"))

	update := buildStructuredStreamUpdate(initial.History.Cursor.ResumeToken, current, false)
	if update == nil || update.Operation != sessionStructuredOperationUpsert {
		t.Fatalf("update = %+v, want upsert", update)
	}
	if len(update.StructuredMessages) != 1 || update.StructuredMessages[0].Status != "final" {
		t.Fatalf("messages = %+v, want finalized m1", update.StructuredMessages)
	}
}

func TestBuildStructuredStreamUpdateResetsOnInvalidResume(t *testing.T) {
	projection := testStructuredStreamProjection("stream-a", testStructuredMessage("m1", "final"))

	update := buildStructuredStreamUpdate("not-a-token", projection, false)
	assertStructuredReset(t, update, sessionStructuredResetResumeInvalid, []string{"m1"})
}

func TestBuildStructuredStreamUpdateResetsOnStreamChange(t *testing.T) {
	previous := testStructuredStreamProjection("stream-a", testStructuredMessage("m1", "final"))
	initial := buildStructuredStreamUpdate("", previous, false)
	current := testStructuredStreamProjection("stream-b", testStructuredMessage("n1", "final"))

	update := buildStructuredStreamUpdate(initial.History.Cursor.ResumeToken, current, false)
	assertStructuredReset(t, update, sessionStructuredResetStreamChanged, []string{"n1"})
}

func TestBuildStructuredStreamUpdateResetsOnCursorInvalidationIncludingEmptyReplacement(t *testing.T) {
	previous := testStructuredStreamProjection("stream-a", testStructuredMessage("m1", "final"))
	initial := buildStructuredStreamUpdate("", previous, false)
	current := testStructuredStreamProjection("stream-a")

	update := buildStructuredStreamUpdate(initial.History.Cursor.ResumeToken, current, false)
	assertStructuredReset(t, update, sessionStructuredResetCursorInvalidated, []string{})
	if update.StructuredMessages == nil {
		t.Fatal("reset messages = nil, want non-nil empty replacement")
	}
}

func TestBuildStructuredStreamUpdateResetsOnHistoryRewrite(t *testing.T) {
	previous := testStructuredStreamProjection("stream-a",
		testStructuredMessage("m1", "final"),
		testStructuredMessage("m2", "partial"),
	)
	initial := buildStructuredStreamUpdate("", previous, false)
	current := testStructuredStreamProjection("stream-a",
		testStructuredMessageWithText("m1", "final", "rewritten"),
		testStructuredMessage("m2", "final"),
	)

	update := buildStructuredStreamUpdate(initial.History.Cursor.ResumeToken, current, false)
	assertStructuredReset(t, update, sessionStructuredResetHistoryRewritten, []string{"m1", "m2"})
}

func TestBuildStructuredStreamUpdateIgnoresObservationGenerationChanges(t *testing.T) {
	previous := testStructuredStreamProjection("stream-a", testStructuredMessage("m1", "final"))
	previous.History.Generation = SessionStructuredGeneration{ID: "1:10", ObservedAt: "2026-01-01T00:00:00Z"}
	initial := buildStructuredStreamUpdate("", previous, false)
	current := testStructuredStreamProjection("stream-a", testStructuredMessage("m1", "final"))
	current.History.Generation = SessionStructuredGeneration{ID: "2:20", ObservedAt: "2026-01-02T00:00:00Z"}

	if got := buildStructuredStreamUpdate(initial.History.Cursor.ResumeToken, current, false); got != nil {
		t.Fatalf("generation-only update = %+v, want nil", got)
	}
}

func TestBuildStructuredStreamUpdateRejectsThinkingModeTokenReuse(t *testing.T) {
	projection := testStructuredStreamProjection("stream-a", testStructuredMessage("m1", "final"))
	initial := buildStructuredStreamUpdate("", projection, false)

	update := buildStructuredStreamUpdate(initial.History.Cursor.ResumeToken, projection, true)
	assertStructuredReset(t, update, sessionStructuredResetResumeInvalid, []string{"m1"})
}

func TestBuildStructuredStreamUpdateResetsWhenEmptySuffixAnchorDisappears(t *testing.T) {
	page := testStructuredStreamProjection("stream-a")
	page.History.Cursor.AfterEntryID = "m4"
	page.Pagination = &sessionlog.PaginationInfo{
		HasOlderMessages:     true,
		TotalMessageCount:    4,
		ReturnedMessageCount: 0,
	}
	snapshot := structuredSnapshotProjection(page, false)

	rewritten := testStructuredStreamProjection("stream-a",
		testStructuredMessage("x", "final"),
		testStructuredMessage("y", "final"),
	)
	update := buildStructuredStreamUpdate(snapshot.History.Cursor.ResumeToken, rewritten, false)
	assertStructuredReset(t, update, sessionStructuredResetCursorInvalidated, []string{"x", "y"})
}

func TestBuildStructuredStreamUpdateReplaysEmptySuffixAnchorInclusively(t *testing.T) {
	page := testStructuredStreamProjection("stream-a")
	page.History.Cursor.AfterEntryID = "m4"
	page.Pagination = &sessionlog.PaginationInfo{
		HasOlderMessages:     true,
		TotalMessageCount:    4,
		ReturnedMessageCount: 0,
	}
	snapshot := structuredSnapshotProjection(page, false)

	current := testStructuredStreamProjection("stream-a",
		testStructuredMessage("m1", "final"),
		testStructuredMessage("m2", "final"),
		testStructuredMessage("m3", "final"),
		testStructuredMessageWithText("m4", "final", "rewritten anchor"),
	)
	current.History.Cursor.AfterEntryID = "m4"
	update := buildStructuredStreamUpdate(snapshot.History.Cursor.ResumeToken, current, false)
	if update == nil || update.Operation != sessionStructuredOperationUpsert {
		t.Fatalf("update = %+v, want bounded anchor upsert", update)
	}
	if got := structuredMessageIDs(update.StructuredMessages); !equalStrings(got, []string{"m4"}) {
		t.Fatalf("upsert IDs = %v, want [m4]", got)
	}
	if got := update.StructuredMessages[0].Blocks[0].Text; got != "rewritten anchor" {
		t.Fatalf("anchor text = %q, want rewritten anchor", got)
	}
}

func TestBuildStructuredStreamUpdateResumesFromInteriorPaginatedWindow(t *testing.T) {
	page := testStructuredStreamProjection("stream-a",
		testStructuredMessage("m2", "final"),
		testStructuredMessage("m3", "final"),
	)
	page.History.Cursor.AfterEntryID = "m4"
	page.Pagination = &sessionlog.PaginationInfo{
		HasOlderMessages:     true,
		HasNewerMessages:     true,
		TotalMessageCount:    4,
		ReturnedMessageCount: 2,
	}
	snapshot := structuredSnapshotProjection(page, false)

	current := testStructuredStreamProjection("stream-a",
		testStructuredMessage("m1", "final"),
		testStructuredMessage("m2", "final"),
		testStructuredMessage("m3", "final"),
		testStructuredMessage("m4", "final"),
	)
	current.History.Cursor.AfterEntryID = "m4"
	update := buildStructuredStreamUpdate(snapshot.History.Cursor.ResumeToken, current, false)
	if update == nil || update.Operation != sessionStructuredOperationUpsert {
		t.Fatalf("update = %+v, want upsert", update)
	}
	if got := structuredMessageIDs(update.StructuredMessages); !equalStrings(got, []string{"m3", "m4"}) {
		t.Fatalf("upsert IDs = %v, want inclusive tail [m3 m4]", got)
	}
}

func testStructuredStreamProjection(streamID string, messages ...SessionStructuredMessage) SessionStreamStructuredMessageEvent {
	return SessionStreamStructuredMessageEvent{
		ID:            "gc-1",
		Template:      "myrig/worker",
		Provider:      "test",
		Format:        "structured",
		SchemaVersion: sessionStructuredSchemaVersion,
		History: &SessionStructuredHistory{
			GCSessionID:        "gc-1",
			ProviderSessionID:  streamID + "-provider",
			TranscriptStreamID: streamID,
			Generation:         SessionStructuredGeneration{ID: "volatile"},
			Cursor:             SessionStructuredCursor{},
			Continuity:         SessionStructuredContinuity{Status: "continuous"},
			TailState:          SessionStructuredTailState{Activity: "idle"},
		},
		StructuredMessages: messages,
	}
}

func testStructuredMessage(id, status string) SessionStructuredMessage {
	return testStructuredMessageWithText(id, status, id+" text")
}

func testStructuredMessageWithText(id, status, text string) SessionStructuredMessage {
	return SessionStructuredMessage{
		ID:     id,
		Role:   "assistant",
		Status: status,
		Blocks: []SessionStructuredBlock{{Type: "text", Text: text}},
	}
}

func structuredMessageIDs(messages []SessionStructuredMessage) []string {
	ids := make([]string, 0, len(messages))
	for _, message := range messages {
		ids = append(ids, message.ID)
	}
	return ids
}

func assertStructuredReset(t *testing.T, update *SessionStreamStructuredMessageEvent, reason string, wantIDs []string) {
	t.Helper()
	if update == nil {
		t.Fatal("update = nil, want reset")
	}
	if update.Operation != sessionStructuredOperationReset {
		t.Fatalf("operation = %q, want %q", update.Operation, sessionStructuredOperationReset)
	}
	if update.ResetReason != reason {
		t.Fatalf("reset_reason = %q, want %q", update.ResetReason, reason)
	}
	if got := structuredMessageIDs(update.StructuredMessages); !equalStrings(got, wantIDs) {
		t.Fatalf("reset message IDs = %v, want %v", got, wantIDs)
	}
	if update.History == nil || update.History.Cursor.ResumeToken == "" {
		t.Fatalf("history cursor = %+v, want replacement resume token", update.History)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
