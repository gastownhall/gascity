package beads

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestBdStoreGetBatchPrimaryFirstStableUniqueOrder(t *testing.T) {
	var calls [][]string
	runner := func(_ string, name string, args ...string) ([]byte, error) {
		if name != "bd" {
			return nil, fmt.Errorf("unexpected command %q", name)
		}
		calls = append(calls, append([]string(nil), args...))
		switch args[0] {
		case "show":
			if slices.Equal(args[2:], []string{"bd-wisp"}) {
				return nil, batchShowNotFoundError("bd-wisp")
			}
			if got, want := args[2:], []string{"bd-no-history", "bd-wisp", "bd-history"}; !slices.Equal(got, want) {
				t.Fatalf("bd show IDs = %v, want every unique requested ID %v", got, want)
			}
			return []byte(`[
				{"id":"bd-history","title":"history","status":"open","issue_type":"task","dependencies":[{"id":"bd-parent","dependency_type":"blocks"}]},
				{"id":"bd-no-history","title":"no history","status":"closed","issue_type":"task","no_history":true}
			]`), nil
		case "query":
			if got, want := args[2], "ephemeral=true AND id=bd-wisp"; got != want {
				t.Fatalf("wisp query = %q, want %q", got, want)
			}
			return []byte(`[{"id":"bd-wisp","title":"wisp","status":"open","issue_type":"message"}]`), nil
		default:
			return nil, fmt.Errorf("unexpected bd verb %q", args[0])
		}
	}

	got, err := NewBdStore("/city", runner).GetBatch([]string{
		"bd-no-history", "bd-wisp", "bd-history", "bd-no-history",
	})
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	if gotIDs := batchBeadIDs(got); !slices.Equal(gotIDs, []string{"bd-no-history", "bd-wisp", "bd-history"}) {
		t.Fatalf("GetBatch IDs = %v, want stable unique input order", gotIDs)
	}
	if !got[0].NoHistory || got[0].Ephemeral {
		t.Fatalf("no-history bead storage = ephemeral:%t no_history:%t", got[0].Ephemeral, got[0].NoHistory)
	}
	if !got[1].Ephemeral {
		t.Fatal("wisp fallback result Ephemeral = false, want true")
	}
	if len(got[2].Dependencies) != 1 || got[2].Dependencies[0].DependsOnID != "bd-parent" {
		t.Fatalf("history dependencies = %+v, want complete bd-show payload", got[2].Dependencies)
	}
	if len(calls) != 3 {
		t.Fatalf("bd calls = %d, want routed show, absent verification, and wisp read", len(calls))
	}
}

func TestBdStoreGetBatchSkipsWispReadWhenPrimaryIsComplete(t *testing.T) {
	calls := 0
	runner := func(_ string, _ string, args ...string) ([]byte, error) {
		calls++
		switch args[0] {
		case "show":
			return []byte(`[
				{"id":"bd-b","title":"b","status":"open","issue_type":"task"},
				{"id":"bd-a","title":"a","status":"open","issue_type":"task"}
			]`), nil
		default:
			return nil, fmt.Errorf("unexpected fallback command: bd %s", strings.Join(args, " "))
		}
	}

	got, err := NewBdStore("/city", runner).GetBatch([]string{"bd-a", "bd-b"})
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	if gotIDs := batchBeadIDs(got); !slices.Equal(gotIDs, []string{"bd-a", "bd-b"}) {
		t.Fatalf("GetBatch IDs = %v, want [bd-a bd-b]", gotIDs)
	}
	if calls != 1 {
		t.Fatalf("bd calls = %d, want one routed multi-show", calls)
	}
}

func TestBdStoreGetBatchFailsWholeBatchOnIncompleteCoverage(t *testing.T) {
	runner := func(_ string, _ string, args ...string) ([]byte, error) {
		switch args[0] {
		case "show":
			if slices.Equal(args[2:], []string{"bd-missing"}) {
				return nil, batchShowNotFoundError("bd-missing")
			}
			return []byte(`[{"id":"bd-found","title":"found","status":"open","issue_type":"task"}]`), nil
		case "query":
			return []byte(`[]`), nil
		default:
			return nil, fmt.Errorf("unexpected bd verb %q", args[0])
		}
	}

	got, err := NewBdStore("/city", runner).GetBatch([]string{"bd-found", "bd-missing"})
	if got != nil {
		t.Fatalf("GetBatch result = %+v, want nil on incomplete coverage", got)
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetBatch error = %v, want ErrNotFound", err)
	}
}

func TestBdStoreGetBatchDoesNotTreatRoutedReadFailureAsWispAbsence(t *testing.T) {
	queryCalled := false
	runner := func(_ string, _ string, args ...string) ([]byte, error) {
		switch args[0] {
		case "show":
			if slices.Equal(args[2:], []string{"remote-missing", "remote-broken"}) {
				return nil, errors.New("exit status 1: Error fetching remote-missing: issue not found\n" +
					"Error fetching remote-broken: routed store unavailable")
			}
			return []byte(`[{"id":"local-task","title":"local","status":"open","issue_type":"task"}]`), nil
		case "query":
			queryCalled = true
			return []byte(`[{"id":"remote-missing"},{"id":"remote-broken"}]`), nil
		default:
			return nil, fmt.Errorf("unexpected command: bd %s", strings.Join(args, " "))
		}
	}

	got, err := NewBdStore("/city", runner).GetBatch([]string{"local-task", "remote-missing", "remote-broken"})
	if err == nil || !strings.Contains(err.Error(), "routed store unavailable") {
		t.Fatalf("GetBatch error = %v, want routed read failure", err)
	}
	if got != nil {
		t.Fatalf("GetBatch result = %+v, want nil on routed read failure", got)
	}
	if queryCalled {
		t.Fatal("wisp query ran after an unproven routed absence")
	}
}

func TestBdStoreGetBatchRejectsPartialOmissionVerification(t *testing.T) {
	queryCalled := false
	runner := func(_ string, _ string, args ...string) ([]byte, error) {
		switch args[0] {
		case "show":
			if slices.Equal(args[2:], []string{"remote-found", "remote-broken"}) {
				// Multi-ID show exits successfully when any ID resolves and
				// suppresses the other ID's error from CommandRunner. This
				// partial response cannot prove that remote-broken is absent.
				return []byte(`[{"id":"remote-found","status":"open","issue_type":"task"}]`), nil
			}
			return []byte(`[{"id":"local-task","status":"open","issue_type":"task"}]`), nil
		case "query":
			queryCalled = true
			return []byte(`[{"id":"remote-broken"}]`), nil
		default:
			return nil, fmt.Errorf("unexpected command: bd %s", strings.Join(args, " "))
		}
	}

	got, err := NewBdStore("/city", runner).GetBatch([]string{"local-task", "remote-found", "remote-broken"})
	if got != nil {
		t.Fatalf("GetBatch result = %+v, want nil on partial omission verification", got)
	}
	if !IsPartialResult(err) {
		t.Fatalf("GetBatch error = %v, want PartialResultError", err)
	}
	if queryCalled {
		t.Fatal("wisp query ran after partial omission verification")
	}
}

func TestBdStoreGetBatchRequiresExactNotFoundEvidenceForEveryOmittedID(t *testing.T) {
	ids := []string{"bd-a", "bd-b"}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "recognized evidence for each ID",
			err: errors.New("exit status 1: Error fetching bd-a: not found: issue bd-a\n" +
				`Error fetching bd-b: no issue found matching "bd-b"`),
			want: true,
		},
		{
			name: "direct issue-not-found lines",
			err:  errors.New("exit status 1: Issue bd-a not found\nIssue bd-b not found"),
			want: true,
		},
		{
			name: "missing evidence for one ID",
			err:  errors.New("exit status 1: Error fetching bd-a: issue not found"),
		},
		{
			name: "backend failure mixed with not found",
			err: errors.New("exit status 1: Error fetching bd-a: issue not found\n" +
				"Error fetching bd-b: routed store unavailable"),
		},
		{
			name: "not-found prefix with backend suffix",
			err: errors.New("exit status 1: Error fetching bd-a: issue not found\n" +
				"Error fetching bd-b: get issue: not found; backend unavailable"),
		},
		{
			name: "extra unclassified line",
			err: errors.New("exit status 1: Error fetching bd-a: issue not found\n" +
				"Error fetching bd-b: issue not found\nwarning: routed store unavailable"),
		},
		{
			name: "duplicate evidence",
			err: errors.New("exit status 1: Error fetching bd-a: issue not found\n" +
				"Error fetching bd-a: issue not found\nError fetching bd-b: issue not found"),
		},
		{
			name: "case-distinct ID is not collapsed",
			err:  errors.New("exit status 1: Error fetching bd-a: issue not found"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testIDs := ids
			if tt.name == "case-distinct ID is not collapsed" {
				testIDs = []string{"BD-A", "bd-a"}
			}
			if got := bdBatchShowProvesAllNotFound(tt.err, testIDs); got != tt.want {
				t.Fatalf("bdBatchShowProvesAllNotFound(%v, %v) = %t, want %t", tt.err, testIDs, got, tt.want)
			}
		})
	}
}

func TestBdStoreGetBatchRejectsNonAuthoritativeShowResponses(t *testing.T) {
	tests := []struct {
		name          string
		show          string
		wantCollision bool
		wantPartial   bool
		wantNotFound  bool
	}{
		{name: "duplicate", show: `[{"id":"bd-a"},{"id":"bd-a"}]`},
		{name: "unexpected or fuzzy", show: `[{"id":"bd-wisp-a1"}]`, wantCollision: true},
		{name: "partial", show: `[{"id":"bd-a"},{"id":"bd-other","created_at":"not-a-time"}]`, wantPartial: true},
		{name: "missing", show: `[]`, wantNotFound: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := func(_ string, _ string, args ...string) ([]byte, error) {
				switch args[0] {
				case "show":
					return []byte(tt.show), nil
				case "query":
					return []byte(`[]`), nil
				default:
					return nil, fmt.Errorf("unexpected command: bd %s", strings.Join(args, " "))
				}
			}

			got, err := NewBdStore("/city", runner).GetBatch([]string{"bd-a"})
			if err == nil || got != nil {
				t.Fatalf("GetBatch = (%+v, %v), want nil strict show error", got, err)
			}
			if tt.wantCollision && !errors.Is(err, ErrIDCollision) {
				t.Fatalf("GetBatch error = %v, want ErrIDCollision", err)
			}
			if tt.wantPartial && !IsPartialResult(err) {
				t.Fatalf("GetBatch error = %v, want PartialResultError", err)
			}
			if tt.wantNotFound && !errors.Is(err, ErrNotFound) {
				t.Fatalf("GetBatch error = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestBdStoreGetBatchRejectsNonAuthoritativeWispResponses(t *testing.T) {
	tests := []struct {
		name         string
		wisp         string
		wantNotFound bool
		wantPartial  bool
	}{
		{
			name: "duplicate wisp row",
			wisp: `[{"id":"bd-a"},{"id":"bd-a"}]`,
		},
		{
			name:         "unexpected wisp row",
			wisp:         `[{"id":"bd-other"}]`,
			wantNotFound: true,
		},
		{
			name:        "partially malformed wisp response",
			wisp:        `[{"id":"bd-a"},{"id":"bd-other","created_at":"not-a-time"}]`,
			wantPartial: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := func(_ string, _ string, args ...string) ([]byte, error) {
				switch args[0] {
				case "show":
					return nil, batchShowNotFoundError("bd-a")
				case "query":
					return []byte(tt.wisp), nil
				default:
					return nil, fmt.Errorf("unexpected bd verb %q", args[0])
				}
			}

			got, err := NewBdStore("/city", runner).GetBatch([]string{"bd-a"})
			if err == nil {
				t.Fatal("GetBatch error = nil, want strict response failure")
			}
			if got != nil {
				t.Fatalf("GetBatch result = %+v, want nil on strict response failure", got)
			}
			if tt.wantNotFound && !errors.Is(err, ErrNotFound) {
				t.Fatalf("GetBatch error = %v, want ErrNotFound", err)
			}
			if tt.wantPartial && !IsPartialResult(err) {
				t.Fatalf("GetBatch error = %v, want PartialResultError", err)
			}
		})
	}
}

func TestBdStoreGetBatchChunksExternalReads(t *testing.T) {
	ids := make([]string, bdGetBatchChunkSize+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("bd-%04d", i)
	}

	var showCalls int
	runner := func(_ string, _ string, args ...string) ([]byte, error) {
		var requested []string
		switch args[0] {
		case "show":
			showCalls++
			requested = args[2:]
		default:
			return nil, fmt.Errorf("unexpected command: bd %s", strings.Join(args, " "))
		}
		rows := make([]map[string]string, len(requested))
		for i, id := range requested {
			rows[i] = map[string]string{"id": id, "status": "open", "issue_type": "task"}
		}
		return json.Marshal(rows)
	}

	got, err := NewBdStore("/city", runner).GetBatch(ids)
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	if gotIDs := batchBeadIDs(got); !slices.Equal(gotIDs, ids) {
		t.Fatalf("GetBatch IDs differ from input order")
	}
	if showCalls != 2 {
		t.Fatalf("primary show calls = %d, want 2 chunked calls", showCalls)
	}
}

func TestBdStoreGetBatchChunksAbsentOnlyWispReads(t *testing.T) {
	ids := make([]string, bdGetBatchChunkSize+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("bd-wisp-%04d", i)
	}

	var showCalls, queryCalls int
	runner := func(_ string, _ string, args ...string) ([]byte, error) {
		switch args[0] {
		case "show":
			showCalls++
			return nil, batchShowNotFoundError(args[2:]...)
		case "query":
			queryCalls++
			requested := batchWispQueryIDs(t, args[2])
			rows := make([]map[string]any, len(requested))
			for i, id := range requested {
				rows[len(requested)-1-i] = map[string]any{
					"id": id, "status": "open", "issue_type": "message", "ephemeral": true,
				}
			}
			return json.Marshal(rows)
		default:
			return nil, fmt.Errorf("unexpected command: bd %s", strings.Join(args, " "))
		}
	}

	got, err := NewBdStore("/city", runner).GetBatch(ids)
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	if gotIDs := batchBeadIDs(got); !slices.Equal(gotIDs, ids) {
		t.Fatalf("GetBatch wisp IDs differ from stable input order")
	}
	if showCalls != 2 || queryCalls != 2 {
		t.Fatalf("batch calls = show:%d query:%d, want two chunks in each tier", showCalls, queryCalls)
	}
}

func TestBdStoreGetBatchChunksByCommandBytes(t *testing.T) {
	longPart := strings.Repeat("a", bdGetBatchChunkBytes/2)
	ids := []string{"bd-" + longPart + "-1", "bd-" + longPart + "-2", "bd-" + longPart + "-3"}
	showCalls := 0
	runner := func(_ string, _ string, args ...string) ([]byte, error) {
		if args[0] != "show" {
			return nil, fmt.Errorf("unexpected command: bd %s", strings.Join(args, " "))
		}
		showCalls++
		rows := make([]map[string]string, len(args[2:]))
		for i, id := range args[2:] {
			rows[i] = map[string]string{"id": id, "status": "open", "issue_type": "task"}
		}
		return json.Marshal(rows)
	}

	got, err := NewBdStore("/city", runner).GetBatch(ids)
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	if gotIDs := batchBeadIDs(got); !slices.Equal(gotIDs, ids) {
		t.Fatalf("GetBatch IDs differ from stable input order")
	}
	if showCalls != 3 {
		t.Fatalf("primary show calls = %d, want 3 byte-bounded chunks", showCalls)
	}
}

func TestBdStoreGetBatchEmptyDoesNotRead(t *testing.T) {
	runner := func(_ string, _ string, _ ...string) ([]byte, error) {
		t.Fatal("empty GetBatch must not invoke bd")
		return nil, nil
	}
	got, err := NewBdStore("/city", runner).GetBatch(nil)
	if err != nil || got != nil {
		t.Fatalf("GetBatch(nil) = (%+v, %v), want (nil, nil)", got, err)
	}
}

func batchWispQueryIDs(t *testing.T, expression string) []string {
	t.Helper()
	const prefix = "ephemeral=true AND "
	if !strings.HasPrefix(expression, prefix) {
		t.Fatalf("wisp expression = %q, want prefix %q", expression, prefix)
	}
	idsExpression := strings.TrimPrefix(expression, prefix)
	idsExpression = strings.TrimPrefix(idsExpression, "(")
	idsExpression = strings.TrimSuffix(idsExpression, ")")
	clauses := strings.Split(idsExpression, " OR ")
	ids := make([]string, len(clauses))
	for i, clause := range clauses {
		if !strings.HasPrefix(clause, "id=") {
			t.Fatalf("wisp clause = %q, want id=<exact ID>", clause)
		}
		ids[i] = strings.TrimPrefix(clause, "id=")
	}
	return ids
}

func batchShowNotFoundError(ids ...string) error {
	lines := make([]string, len(ids))
	for i, id := range ids {
		lines[i] = fmt.Sprintf("Error fetching %s: issue not found", id)
	}
	return errors.New("exit status 1: " + strings.Join(lines, "\n"))
}

func batchBeadIDs(beads []Bead) []string {
	ids := make([]string, len(beads))
	for i := range beads {
		ids[i] = beads[i].ID
	}
	return ids
}
