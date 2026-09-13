package runtime

import (
	"errors"
	"testing"
)

func TestMergeBackendListResultsReturnsBestEffortResultsOnPartialFailure(t *testing.T) {
	t.Parallel()

	names, err := MergeBackendListResults(
		BackendListResult{Label: "local", Names: []string{"sess-a"}},
		BackendListResult{Label: "remote", Err: errors.New("backend down")},
	)
	if !IsPartialListError(err) {
		t.Fatalf("MergeBackendListResults() error = %v, want partial list error", err)
	}
	if len(names) != 1 || names[0] != "sess-a" {
		t.Fatalf("MergeBackendListResults() names = %v, want [sess-a]", names)
	}
}

func TestMergeBackendListResultsFailsWhenAllBackendsFail(t *testing.T) {
	t.Parallel()

	names, err := MergeBackendListResults(
		BackendListResult{Label: "local", Err: errors.New("local down")},
		BackendListResult{Label: "remote", Err: errors.New("remote down")},
	)
	if err == nil {
		t.Fatal("MergeBackendListResults() error = nil, want joined error")
	}
	if IsPartialListError(err) {
		t.Fatalf("MergeBackendListResults() error = %v, want total failure not partial", err)
	}
	if names != nil {
		t.Fatalf("MergeBackendListResults() names = %v, want nil", names)
	}
}

func TestMergeBackendListResultsPreservesNamesWhenAllBackendsAreDegraded(t *testing.T) {
	t.Parallel()

	names, err := MergeBackendListResults(
		BackendListResult{Label: "local", Names: []string{"sess-a"}, Err: errors.New("local degraded")},
		BackendListResult{Label: "remote", Names: []string{"sess-b"}, Err: errors.New("remote degraded")},
	)
	if !IsPartialListError(err) {
		t.Fatalf("MergeBackendListResults() error = %v, want partial list error", err)
	}
	if len(names) != 2 || names[0] != "sess-a" || names[1] != "sess-b" {
		t.Fatalf("MergeBackendListResults() names = %v, want [sess-a sess-b]", names)
	}
}
