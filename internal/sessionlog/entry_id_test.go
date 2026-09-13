package sessionlog

import (
	"errors"
	"strings"
	"testing"
)

func TestStableSyntheticEntryID(t *testing.T) {
	t.Parallel()

	compact := stableSyntheticEntryID("provider", []byte(`{"type":"message","content":"same"}`), "message")
	whitespace := stableSyntheticEntryID("provider", []byte(" { \n  \"type\": \"message\", \"content\": \"same\" \n } "), "message")
	if compact != whitespace {
		t.Fatalf("whitespace-only JSON rewrite changed synthetic ID: %q != %q", compact, whitespace)
	}
	if got := stableSyntheticEntryID("provider", []byte(`{"type":"message","content":"changed"}`), "message"); got == compact {
		t.Fatalf("content change retained synthetic ID %q", got)
	}
	if got := stableSyntheticEntryID("provider", []byte(`{"type":"message","content":"same"}`), "tool_result"); got == compact {
		t.Fatalf("different normalized parts shared synthetic ID %q", got)
	}
	if !strings.HasPrefix(compact, "provider-") || len(strings.TrimPrefix(compact, "provider-")) != 64 {
		t.Fatalf("synthetic ID = %q, want provider- plus full SHA-256", compact)
	}
}

func TestStableSyntheticEntryIDSurfacesExactDuplicateRecords(t *testing.T) {
	t.Parallel()

	id := stableSyntheticEntryID("provider", []byte(`{"type":"message","content":"same"}`), "message")
	page, err := paginateSession(&Session{Messages: []*Entry{
		{UUID: id, Type: "assistant"},
		{UUID: id, Type: "assistant"},
	}}, 0, "", id)
	if err == nil {
		t.Fatalf("duplicate synthetic entries returned page %+v, want ErrDuplicateEntryID", page)
	}
	if !errors.Is(err, ErrDuplicateEntryID) {
		t.Fatalf("duplicate synthetic entry error = %v, want ErrDuplicateEntryID", err)
	}
}

func TestPageSessionDoesNotMutateSource(t *testing.T) {
	t.Parallel()

	first := &Entry{UUID: "first", Type: "user"}
	second := &Entry{UUID: "second", Type: "assistant"}
	source := &Session{Messages: []*Entry{first, second}}

	page, err := PageSession(source, 0, "second", "")
	if err != nil {
		t.Fatalf("PageSession: %v", err)
	}
	if len(page.Messages) != 1 || page.Messages[0] != first {
		t.Fatalf("page messages = %#v, want first entry", page.Messages)
	}
	page.Messages[0] = nil
	if len(source.Messages) != 2 || source.Messages[0] != first || source.Messages[1] != second {
		t.Fatalf("source messages changed through page: %#v", source.Messages)
	}
	if source.Pagination != nil {
		t.Fatalf("source pagination = %+v, want nil", source.Pagination)
	}
}
