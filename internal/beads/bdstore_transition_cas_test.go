package beads

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"
)

type bdTransitionCASFixture struct {
	mu sync.Mutex

	issue                bdIssue
	reason               string
	session              string
	conditionalSupported bool

	rawCloseBeforeNextClose  bool
	rawCloseBeforeNextUpdate bool
	rawCloseAfterNextUpdate  bool
	closeArgs                [][]string
	updateArgs               [][]string
}

func newBDTransitionCASFixture() *bdTransitionCASFixture {
	return &bdTransitionCASFixture{
		conditionalSupported: true,
		issue: bdIssue{
			ID:        "bd-cas",
			Title:     "CAS target",
			Status:    "open",
			IssueType: "task",
			Revision:  11,
		},
	}
}

func (f *bdTransitionCASFixture) runner(_ string, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if name != "bd" || len(args) == 0 {
		return nil, fmt.Errorf("unexpected command %s %q", name, args)
	}
	if len(args) == 2 && args[1] == "--help" {
		switch args[0] {
		case "update", "close", "assign", "delete":
			if f.conditionalSupported {
				return []byte("Flags:\n      --if-revision int\n"), nil
			}
			return []byte("Flags:\n"), nil
		}
	}
	switch args[0] {
	case "version":
		return []byte("bd version 1.1.0\n"), nil
	case "query", "show":
		return f.issueJSON()
	case "dep":
		return []byte(`[]`), nil
	case "sql":
		return json.Marshal([]bdCloseIdentity{{
			ID:              f.issue.ID,
			Status:          f.issue.Status,
			CloseReason:     f.reason,
			ClosedBySession: f.session,
		}})
	case "close":
		f.closeArgs = append(f.closeArgs, slices.Clone(args))
		reason := argValue(args, "--reason")
		if reason == "" {
			reason = "Closed"
		}
		if f.rawCloseBeforeNextClose {
			f.rawCloseBeforeNextClose = false
			f.issue.Status = "closed"
			f.issue.Revision = 22
			f.reason = reason
			f.session = "raw-bd-winner"
		}
		if expected, ok := revisionArg(args); ok && expected != f.issue.Revision {
			return conditionalPreconditionBody(expected, f.issue.Revision), errors.New("exit status 9")
		}
		if f.issue.Status != "closed" {
			f.issue.Status = "closed"
			f.issue.Revision++
			f.reason = reason
			f.session = "gascity-winner"
		}
		f.issue.CloseReason = f.reason
		return f.issueJSON()
	case "update":
		f.updateArgs = append(f.updateArgs, slices.Clone(args))
		if f.rawCloseBeforeNextUpdate {
			f.rawCloseBeforeNextUpdate = false
			f.issue.Status = "closed"
			f.issue.Revision = 22
			f.reason = "raw update winner"
			f.session = "raw-bd-winner"
		}
		if expected, ok := revisionArg(args); ok && expected != f.issue.Revision {
			return conditionalPreconditionBody(expected, f.issue.Revision), errors.New("exit status 9")
		}
		if status := argValue(args, "--status"); status != "" {
			f.issue.Status = status
		}
		if title := argValue(args, "--title"); title != "" {
			f.issue.Title = title
		}
		f.issue.Revision++
		if f.rawCloseAfterNextUpdate {
			f.rawCloseAfterNextUpdate = false
			f.issue.Status = "closed"
			f.issue.Revision++
			f.reason = "raw post-update close"
			f.session = "raw-bd-winner"
		}
		return f.issueJSON()
	default:
		return nil, fmt.Errorf("unexpected bd args %q", args)
	}
}

func (f *bdTransitionCASFixture) envRunner(dir, name string, _ map[string]string, args ...string) ([]byte, error) {
	return f.runner(dir, name, args...)
}

func (f *bdTransitionCASFixture) issueJSON() ([]byte, error) {
	return json.Marshal([]map[string]any{{
		"id":           f.issue.ID,
		"title":        f.issue.Title,
		"status":       f.issue.Status,
		"issue_type":   f.issue.IssueType,
		"revision":     f.issue.Revision,
		"close_reason": f.reason,
	}})
}

func revisionArg(args []string) (int64, bool) {
	value := argValue(args, conditionalWriteFlag)
	if value == "" {
		return 0, false
	}
	revision, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, false
	}
	return revision, true
}

func conditionalPreconditionBody(expected, current int64) []byte {
	body, _ := json.Marshal(map[string]any{
		"error":             "precondition failed",
		"code":              "precondition_failed",
		"expected_revision": expected,
		"current_revision":  current,
	})
	return body
}

func TestBdStoreCloseTransitionDoesNotClaimRawBDWinnerWithSameReason(t *testing.T) {
	fixture := newBDTransitionCASFixture()
	fixture.rawCloseBeforeNextClose = true
	store := NewBdStore(t.TempDir(), fixture.runner, WithBdStoreCommandEnvRunner(fixture.envRunner))

	transition, err := store.CloseWithReasonIfOpen(fixture.issue.ID, "shared close reason")
	if err != nil {
		t.Fatalf("CloseWithReasonIfOpen: %v", err)
	}
	if transition.Transitioned {
		t.Fatal("Transitioned = true after a raw bd process won from the observed revision")
	}
	if !transition.AuthoritativeClosed(fixture.issue.ID) || transition.After.Metadata["close_reason"] != "shared close reason" {
		t.Fatalf("transition = %#v, want authoritative raw winner snapshot", transition)
	}

	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.closeArgs) != 1 {
		t.Fatalf("close calls = %d, want 1", len(fixture.closeArgs))
	}
	if got, ok := revisionArg(fixture.closeArgs[0]); !ok || got != 11 {
		t.Fatalf("bd close args = %q, want --if-revision 11", fixture.closeArgs[0])
	}
}

func TestBdStoreUpdateTransitionDoesNotClaimRawBDClose(t *testing.T) {
	fixture := newBDTransitionCASFixture()
	fixture.rawCloseBeforeNextUpdate = true
	store := NewBdStore(t.TempDir(), fixture.runner, WithBdStoreCommandEnvRunner(fixture.envRunner))

	closed := "closed"
	transition, err := store.UpdateWithTransition(fixture.issue.ID, UpdateOpts{Status: &closed})
	if !IsPreconditionFailed(err) {
		t.Fatalf("UpdateWithTransition error = %v, want revision precondition", err)
	}
	if transition.TransitionedToClosed {
		t.Fatal("TransitionedToClosed = true after a raw bd process won from the observed revision")
	}
	if !transition.AuthoritativeAfter(fixture.issue.ID) || transition.After.Status != "closed" {
		t.Fatalf("transition = %#v, want authoritative raw winner snapshot", transition)
	}

	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.updateArgs) != 1 {
		t.Fatalf("update calls = %d, want 1", len(fixture.updateArgs))
	}
	if got, ok := revisionArg(fixture.updateArgs[0]); !ok || got != 11 {
		t.Fatalf("bd update args = %q, want --if-revision 11", fixture.updateArgs[0])
	}
}

func TestBdStoreUpdateTransitionDoesNotClaimRawCloseAfterNonClosingUpdate(t *testing.T) {
	fixture := newBDTransitionCASFixture()
	fixture.rawCloseAfterNextUpdate = true
	store := NewBdStore(t.TempDir(), fixture.runner, WithBdStoreCommandEnvRunner(fixture.envRunner))

	title := "updated before raw close"
	transition, err := store.UpdateWithTransition(fixture.issue.ID, UpdateOpts{Title: &title})
	if err != nil {
		t.Fatalf("UpdateWithTransition: %v", err)
	}
	if transition.TransitionedToClosed {
		t.Fatal("TransitionedToClosed = true when this update did not request status closed")
	}
	if !transition.AuthoritativeAfter(fixture.issue.ID) || transition.After.Title != title || transition.After.Status != "closed" {
		t.Fatalf("transition = %#v, want authoritative post-update raw-close snapshot", transition)
	}
}

func TestBdStoreCloseAllTransitionDoesNotClaimRawBDWinner(t *testing.T) {
	fixture := newBDTransitionCASFixture()
	fixture.rawCloseBeforeNextClose = true
	store := NewBdStore(t.TempDir(), fixture.runner, WithBdStoreCommandEnvRunner(fixture.envRunner))

	result, err := store.CloseAllWithTransitions([]string{fixture.issue.ID}, nil)
	if err != nil {
		t.Fatalf("CloseAllWithTransitions: %v", err)
	}
	if result.Count != 1 {
		t.Fatalf("Count = %d, want existing successful count 1", result.Count)
	}
	transition, ok := result.TransitionFor(fixture.issue.ID)
	if !ok || transition.Transitioned || transition.After.Status != "closed" {
		t.Fatalf("transition = %#v ok=%v, want authoritative raw winner without ownership", transition, ok)
	}

	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.closeArgs) != 1 {
		t.Fatalf("close calls = %d, want 1", len(fixture.closeArgs))
	}
	if got, ok := revisionArg(fixture.closeArgs[0]); !ok || got != 11 {
		t.Fatalf("bd close args = %q, want --if-revision 11", fixture.closeArgs[0])
	}
}

func TestBdStoreTransitionsFailClosedBeforeMutationWithoutRevisionCAS(t *testing.T) {
	tests := []struct {
		name string
		run  func(*BdStore) error
		want error
	}{
		{
			name: "close",
			run: func(store *BdStore) error {
				_, err := store.CloseWithReasonIfOpen("bd-cas", "reason")
				return err
			},
			want: ErrCloseTransitionUnsupported,
		},
		{
			name: "update",
			run: func(store *BdStore) error {
				closed := "closed"
				_, err := store.UpdateWithTransition("bd-cas", UpdateOpts{Status: &closed})
				return err
			},
			want: ErrUpdateTransitionUnsupported,
		},
		{
			name: "close-all",
			run: func(store *BdStore) error {
				_, err := store.CloseAllWithTransitions([]string{"bd-cas"}, nil)
				return err
			},
			want: ErrCloseAllTransitionUnsupported,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newBDTransitionCASFixture()
			fixture.conditionalSupported = false
			store := NewBdStore(t.TempDir(), fixture.runner, WithBdStoreCommandEnvRunner(fixture.envRunner))

			if err := tt.run(store); !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if len(fixture.closeArgs) != 0 || len(fixture.updateArgs) != 0 {
				t.Fatalf("mutation args = close %q update %q, want none", fixture.closeArgs, fixture.updateArgs)
			}
		})
	}
}

type bdCloseAllRuntimeFixture struct {
	base *bdCloseAllTransitionFixture

	mu                    sync.Mutex
	rejectCloseID         string
	commitErrorID         string
	metadataCommitErrorID string
	commitError           error
}

func newBDCloseAllRuntimeFixture() *bdCloseAllRuntimeFixture {
	base := newBdCloseAllTransitionFixture()
	second := base.beads["bd-live"]
	second.ID = "bd-second"
	second.Title = "second live"
	second.Revision = 31
	base.beads[second.ID] = second
	return &bdCloseAllRuntimeFixture{base: base}
}

func (f *bdCloseAllRuntimeFixture) runner(dir, name string, args ...string) ([]byte, error) {
	if name == "bd" && len(args) > 0 && args[0] == "update" {
		f.mu.Lock()
		commitWithError := f.metadataCommitErrorID != "" && slices.Contains(args, f.metadataCommitErrorID)
		commitErr := f.commitError
		f.mu.Unlock()
		if commitWithError {
			out, err := f.base.runner(dir, name, args...)
			if err != nil {
				return out, err
			}
			return out, commitErr
		}
	}
	if name == "bd" && len(args) > 0 && args[0] == "close" {
		f.mu.Lock()
		reject := f.rejectCloseID != "" && slices.Contains(args, f.rejectCloseID)
		commitWithError := f.commitErrorID != "" && slices.Contains(args, f.commitErrorID)
		commitErr := f.commitError
		f.mu.Unlock()
		if reject {
			return nil, errors.New("exit status 1: unknown flag: --if-revision")
		}
		if commitWithError {
			out, err := f.base.runner(dir, name, args...)
			if err != nil {
				return out, err
			}
			return out, commitErr
		}
	}
	return f.base.runner(dir, name, args...)
}

func (f *bdCloseAllRuntimeFixture) envRunner(dir, name string, _ map[string]string, args ...string) ([]byte, error) {
	return f.runner(dir, name, args...)
}

func TestBdStoreCloseAllRuntimeUnsupportedBeforeMutationAllowsSafeFallback(t *testing.T) {
	fixture := newBDCloseAllRuntimeFixture()
	fixture.rejectCloseID = "bd-live"
	store := NewBdStore(t.TempDir(), fixture.runner, WithBdStoreCommandEnvRunner(fixture.envRunner))

	// The already-closed row yields a transition and count without mutating.
	// The first conditional command is still the rejected close for bd-live;
	// fallback safety must not be inferred from Count or transition-map size.
	result, err := store.CloseAllWithTransitions([]string{"bd-closed", "bd-live"}, nil)
	if !errors.Is(err, ErrCloseAllTransitionUnsupported) {
		t.Fatalf("CloseAllWithTransitions error = %v, want safe unsupported fallback", err)
	}
	if result.Count != 0 || len(result.Transitions) != 0 {
		t.Fatalf("result = %#v, want empty result before safe fallback", result)
	}
	fixture.base.mu.Lock()
	defer fixture.base.mu.Unlock()
	if got := fixture.base.beads["bd-live"]; got.Status != "open" || got.Revision != 11 {
		t.Fatalf("durable bead = %#v, want no mutation before fallback", got)
	}
}

func TestBdStoreCloseAllRuntimeUnsupportedAfterEarlierIDIsHardError(t *testing.T) {
	fixture := newBDCloseAllRuntimeFixture()
	fixture.rejectCloseID = "bd-second"
	store := NewBdStore(t.TempDir(), fixture.runner, WithBdStoreCommandEnvRunner(fixture.envRunner))

	result, err := store.CloseAllWithTransitions([]string{"bd-live", "bd-second"}, nil)
	if err == nil || errors.Is(err, ErrCloseAllTransitionUnsupported) {
		t.Fatalf("CloseAllWithTransitions error = %v, want non-fallback partial-mutation error", err)
	}
	if result.Count != 1 {
		t.Fatalf("Count = %d, want only the nil-error first ID", result.Count)
	}
	first, ok := result.TransitionFor("bd-live")
	if !ok || !first.Transitioned || first.After.Status != "closed" {
		t.Fatalf("first transition = %#v ok=%v, want retained committed snapshot", first, ok)
	}
}

func TestBdStoreCloseAllRuntimeUnsupportedAfterMetadataMutationIsHardError(t *testing.T) {
	fixture := newBDCloseAllRuntimeFixture()
	fixture.rejectCloseID = "bd-live"
	store := NewBdStore(t.TempDir(), fixture.runner, WithBdStoreCommandEnvRunner(fixture.envRunner))

	result, err := store.CloseAllWithTransitions([]string{"bd-live"}, map[string]string{"batch": "persisted"})
	if err == nil || errors.Is(err, ErrCloseAllTransitionUnsupported) {
		t.Fatalf("CloseAllWithTransitions error = %v, want non-fallback metadata-mutation error", err)
	}
	if result.Count != 0 {
		t.Fatalf("Count = %d, want zero for errored ID", result.Count)
	}
	transition, ok := result.TransitionFor("bd-live")
	if !ok || transition.After.Status != "open" || transition.After.Metadata["batch"] != "persisted" {
		t.Fatalf("transition = %#v ok=%v, want retained authoritative metadata snapshot", transition, ok)
	}
}

func TestBdStoreCloseAllRetainsCommittedSnapshotReturnedWithError(t *testing.T) {
	fixture := newBDCloseAllRuntimeFixture()
	fixture.commitErrorID = "bd-live"
	fixture.commitError = errors.New("connection reset after committed close")
	store := NewBdStore(t.TempDir(), fixture.runner, WithBdStoreCommandEnvRunner(fixture.envRunner))

	result, err := store.CloseAllWithTransitions([]string{"bd-live"}, nil)
	if !errors.Is(err, fixture.commitError) {
		t.Fatalf("CloseAllWithTransitions error = %v, want %v", err, fixture.commitError)
	}
	if result.Count != 0 {
		t.Fatalf("Count = %d, want zero for committed-with-error ID", result.Count)
	}
	transition, ok := result.TransitionFor("bd-live")
	if !ok || transition.Transitioned || transition.Before.Status != "open" || transition.After.Status != "closed" {
		t.Fatalf("transition = %#v ok=%v, want authoritative committed snapshot without ownership", transition, ok)
	}
}

func TestBdStoreCloseAllRereadsMetadataCommittedWithError(t *testing.T) {
	fixture := newBDCloseAllRuntimeFixture()
	fixture.metadataCommitErrorID = "bd-live"
	fixture.commitError = errors.New("connection reset after committed metadata")
	store := NewBdStore(t.TempDir(), fixture.runner, WithBdStoreCommandEnvRunner(fixture.envRunner))

	result, err := store.CloseAllWithTransitions([]string{"bd-live"}, map[string]string{"batch": "persisted"})
	if !errors.Is(err, fixture.commitError) {
		t.Fatalf("CloseAllWithTransitions error = %v, want %v", err, fixture.commitError)
	}
	if result.Count != 0 {
		t.Fatalf("Count = %d, want zero for committed-with-error ID", result.Count)
	}
	transition, ok := result.TransitionFor("bd-live")
	if !ok || transition.Transitioned || transition.Before.Status != "open" ||
		transition.After.Status != "open" || transition.After.Metadata["batch"] != "persisted" {
		t.Fatalf("transition = %#v ok=%v, want authoritative committed metadata snapshot without close ownership", transition, ok)
	}
}
