package targetscope

import (
	"path/filepath"
	"strings"
	"testing"
)

// The tri-state is the load-bearing contract: absent is the ONLY state that may
// re-enable a cwd stamp, so every way a scope can be unusable must classify as
// invalid rather than collapsing to absent.
func TestParseTriState(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want State
	}{
		{"empty is absent", "", StateAbsent},
		{"whitespace is absent", "   \n", StateAbsent},
		{"field-empty object is valid unknown", `{"v":1}`, StateValid},
		{"branch only is valid", `{"v":1,"branch":"main"}`, StateValid},
		{"absolute worktree is valid", `{"v":1,"worktree":"/srv/wt"}`, StateValid},
		{"both fields valid", `{"v":1,"branch":"release","worktree":"/srv/wt"}`, StateValid},

		{"missing version is invalid", `{}`, StateInvalid},
		{"version zero is invalid", `{"v":0,"branch":"main"}`, StateInvalid},
		{"future version is invalid", `{"v":2,"branch":"main"}`, StateInvalid},
		{"malformed json is invalid", `{"v":1,`, StateInvalid},
		{"not an object is invalid", `"main"`, StateInvalid},
		{"wrong field type is invalid", `{"v":1,"branch":5}`, StateInvalid},
		{"trailing content is invalid", `{"v":1} {"v":1}`, StateInvalid},
		{"relative worktree is invalid", `{"v":1,"worktree":"worktrees/T"}`, StateInvalid},
		{"uncleaned worktree is invalid", `{"v":1,"worktree":"/srv/../srv/wt"}`, StateInvalid},
		{"untrimmed branch is invalid", `{"v":1,"branch":" main"}`, StateInvalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Parse(tc.raw)
			if got.State != tc.want {
				t.Fatalf("Parse(%q) state = %v, want %v (reason=%v)", tc.raw, got.State, tc.want, got.Reason)
			}
			if got.State == StateInvalid && got.Reason == nil {
				t.Fatal("invalid resolution must carry a reason")
			}
			if got.State != StateInvalid && got.Reason != nil {
				t.Fatalf("non-invalid resolution must not carry a reason: %v", got.Reason)
			}
		})
	}
}

// A relative worktree is the cross-store divergence in disguise: re-anchoring
// it would give the graph-store reader and the work-store reader two different
// absolute paths for one declared value. Readers must fail instead.
func TestParseRelativeWorktreeIsInvalidNotReanchored(t *testing.T) {
	res := Parse(`{"v":1,"worktree":"worktrees/T"}`)
	if res.State != StateInvalid {
		t.Fatalf("state = %v, want invalid", res.State)
	}
	if !strings.Contains(res.Reason.Error(), "relative") {
		t.Fatalf("reason %q should explain the relative-path rejection", res.Reason)
	}
}

// Raw must survive parsing verbatim: the declaration protocol feeds it back as
// the compare-and-set expected value, so a re-marshaled approximation would
// make every CAS miss.
func TestParsePreservesRawForCAS(t *testing.T) {
	raw := `{"v":1, "branch":"main"}` // note the non-canonical spacing
	res := Parse(raw)
	if res.State != StateValid {
		t.Fatalf("state = %v, want valid", res.State)
	}
	if res.Raw != raw {
		t.Fatalf("Raw = %q, want the exact input %q", res.Raw, raw)
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	for _, scope := range []Scope{
		Unknown(),
		{V: 1, Branch: "main"},
		{V: 1, Worktree: "/srv/wt"},
		{V: 1, Branch: "release/x", Worktree: "/srv/wt"},
	} {
		blob, err := Marshal(scope)
		if err != nil {
			t.Fatalf("Marshal(%+v): %v", scope, err)
		}
		res := Parse(blob)
		if res.State != StateValid {
			t.Fatalf("Marshal(%+v) produced %q which parses %v (%v)", scope, blob, res.State, res.Reason)
		}
		if !res.Scope.Equal(scope) {
			t.Fatalf("round trip: got %+v want %+v", res.Scope, scope)
		}
	}
}

// Marshal must refuse what Parse would reject, so no writer can persist a blob
// that every reader then fails closed on.
func TestMarshalRejectsRelativeWorktree(t *testing.T) {
	if _, err := Marshal(Scope{V: 1, Worktree: "worktrees/T"}); err == nil {
		t.Fatal("Marshal accepted a relative worktree; boundaries must normalize first")
	}
}

func TestUnknownIsValidAndFieldEmpty(t *testing.T) {
	blob, err := Marshal(Unknown())
	if err != nil {
		t.Fatalf("Marshal(Unknown()): %v", err)
	}
	res := Parse(blob)
	if res.State != StateValid {
		t.Fatalf("Unknown() must persist as VALID, got %v", res.State)
	}
	if !res.Scope.IsUnknown() {
		t.Fatalf("Unknown() must declare no fields, got %+v", res.Scope)
	}
	// The distinction that matters: this is not the same as writing nothing.
	if Parse("").State == res.State {
		t.Fatal("field-empty {v:1} must not classify the same as an absent object")
	}
}

func TestNormalizeWorktree(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "srv", "store")
	tests := []struct {
		name      string
		worktree  string
		storeRoot string
		want      string
		wantErr   bool
	}{
		{"empty stays empty", "", root, "", false},
		{"absolute is cleaned", "/srv/store/../store/wt", root, "/srv/store/wt", false},
		{"relative anchors to store root", "worktrees/T", root, filepath.Join(root, "worktrees", "T"), false},
		{"relative without root errors", "worktrees/T", "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeWorktree(tc.worktree, tc.storeRoot)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
			if got != "" && !filepath.IsAbs(got) {
				t.Fatalf("normalized worktree %q must be absolute", got)
			}
		})
	}
}

func TestEqualComparesEveryField(t *testing.T) {
	base := Scope{V: 1, Branch: "main", Worktree: "/srv/wt"}
	if !base.Equal(Scope{V: 1, Branch: "main", Worktree: "/srv/wt"}) {
		t.Fatal("identical scopes must compare equal")
	}
	for _, other := range []Scope{
		{V: 1, Branch: "release", Worktree: "/srv/wt"},
		{V: 1, Branch: "main", Worktree: "/srv/other"},
		{V: 1, Branch: "main"},
		{V: 1, Worktree: "/srv/wt"},
	} {
		if base.Equal(other) {
			t.Fatalf("%+v must not compare equal to %+v", base, other)
		}
	}
}
