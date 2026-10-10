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

// declaredListingProvider declares a fixed ListRunning attestation.
type declaredListingProvider struct {
	*Fake
	complete bool
}

func (p declaredListingProvider) ListRunningComplete() bool { return p.complete }

// Kills: defaulting to attested. Only a provider that declares
// ListingAttestation and answers true may have an absence concluded from its
// error-free listing; every other shape must read unattested.
func TestListRunningAttested_UndeclaredProviderIsUnattested(t *testing.T) {
	t.Parallel()

	unattestedFake := NewFake()
	unattestedFake.ListingUnattested = true
	cases := []struct {
		name string
		sp   Provider
		want bool
	}{
		{name: "nil provider", sp: nil, want: false},
		{name: "undeclared wrapper", sp: struct{ Provider }{NewFake()}, want: false},
		{name: "declared false", sp: declaredListingProvider{Fake: NewFake(), complete: false}, want: false},
		{name: "declared true", sp: declaredListingProvider{Fake: NewFake(), complete: true}, want: true},
		{name: "fake default", sp: NewFake(), want: true},
		{name: "fake unattested knob", sp: unattestedFake, want: false},
	}
	for _, tc := range cases {
		if got := ListRunningAttested(tc.sp); got != tc.want {
			t.Errorf("%s: ListRunningAttested() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// countingListProvider records how often ListRunning is asked for each prefix.
type countingListProvider struct {
	*Fake
	names    []string
	err      error
	prefixes []string
}

func (p *countingListProvider) ListRunning(prefix string) ([]string, error) {
	p.prefixes = append(p.prefixes, prefix)
	return p.names, p.err
}

// Kills: a per-backend listing that skips, repeats or reorders a backend, or
// mislabels its result.
func TestListBackends_ListsEachBackendOnceInOrder(t *testing.T) {
	t.Parallel()

	a := &countingListProvider{Fake: NewFake(), names: []string{"gc-a"}}
	b := &countingListProvider{Fake: NewFake(), err: errors.New("b down")}
	listings := ListBackends([]Backend{{Label: "a", Provider: a}, {Label: "b", Provider: b}}, "gc-")

	if len(listings) != 2 {
		t.Fatalf("ListBackends() returned %d listings, want 2", len(listings))
	}
	if listings[0].Label != "a" || listings[0].Provider != Provider(a) || len(listings[0].Names) != 1 || listings[0].Err != nil {
		t.Errorf("listing 0 = %#v, want a's result", listings[0])
	}
	if listings[1].Label != "b" || listings[1].Provider != Provider(b) || listings[1].Err == nil {
		t.Errorf("listing 1 = %#v, want b's error", listings[1])
	}
	for label, sp := range map[string]*countingListProvider{"a": a, "b": b} {
		if len(sp.prefixes) != 1 || sp.prefixes[0] != "gc-" {
			t.Errorf("backend %s prefixes = %q, want exactly one gc- call", label, sp.prefixes)
		}
	}
}
