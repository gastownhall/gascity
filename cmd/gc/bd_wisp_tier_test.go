package main

import "testing"

// equalArgs compares two argument lists element-wise, treating a nil and an
// empty list as equal: callers care about the tokens forwarded to bd, not
// about which empty representation carried them.
func equalArgs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestRewriteBdWispTierArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "type molecule joined gains include-infra",
			args: []string{"list", "--type=molecule", "--json"},
			want: []string{"list", "--type=molecule", "--json", "--include-infra"},
		},
		{
			name: "type molecule separate gains include-infra",
			args: []string{"list", "--type", "molecule"},
			want: []string{"list", "--type", "molecule", "--include-infra"},
		},
		{
			name: "short type flag gains include-infra",
			args: []string{"list", "-t", "wisp"},
			want: []string{"list", "-t", "wisp", "--include-infra"},
		},
		{
			name: "type is normalized before matching",
			args: []string{"list", "--type=Molecule"},
			want: []string{"list", "--type=Molecule", "--include-infra"},
		},
		{
			name: "mol-type filter gains include-infra",
			args: []string{"list", "--mol-type=polecat-work"},
			want: []string{"list", "--mol-type=polecat-work", "--include-infra"},
		},
		{
			name: "wisp-type filter gains include-infra",
			args: []string{"list", "--wisp-type", "patrol"},
			want: []string{"list", "--wisp-type", "patrol", "--include-infra"},
		},
		{
			name: "global flags before the verb still resolve list",
			args: []string{"--json", "-C", "/some/dir", "list", "--type=molecule"},
			want: []string{"--json", "-C", "/some/dir", "list", "--type=molecule", "--include-infra"},
		},
		{
			name: "already includes infra is left alone",
			args: []string{"list", "--type=molecule", "--include-infra"},
			want: []string{"list", "--type=molecule", "--include-infra"},
		},
		{
			name: "non-wisp type is left alone",
			args: []string{"list", "--type=bug"},
			want: []string{"list", "--type=bug"},
		},
		{
			name: "unfiltered list is left alone",
			args: []string{"list", "--json", "--limit", "0"},
			want: []string{"list", "--json", "--limit", "0"},
		},
		{
			name: "molecule as a label value is not a type filter",
			args: []string{"list", "--label", "molecule"},
			want: []string{"list", "--label", "molecule"},
		},
		{
			name: "molecule as a title-contains value is not a type filter",
			args: []string{"list", "--title-contains=molecule"},
			want: []string{"list", "--title-contains=molecule"},
		},
		{
			name: "other subcommands are left alone",
			args: []string{"ready", "--type=molecule"},
			want: []string{"ready", "--type=molecule"},
		},
		{
			name: "create is left alone",
			args: []string{"create", "--type=molecule", "-t", "wisp"},
			want: []string{"create", "--type=molecule", "-t", "wisp"},
		},
		{
			name: "unrecognized flag leaves args untouched",
			args: []string{"list", "--brand-new-flag", "molecule", "--type=molecule"},
			want: []string{"list", "--brand-new-flag", "molecule", "--type=molecule"},
		},
		{
			name: "trailing type flag with no value is left alone",
			args: []string{"list", "--type"},
			want: []string{"list", "--type"},
		},
		{
			name: "empty args are left alone",
			args: []string{},
			want: []string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := append([]string{}, tc.args...)
			got := rewriteBdWispTierArgs(input)
			if !equalArgs(got, tc.want) {
				t.Fatalf("rewriteBdWispTierArgs(%v) = %v, want %v", tc.args, got, tc.want)
			}
			if !equalArgs(input, tc.args) {
				t.Fatalf("rewriteBdWispTierArgs mutated its input: got %v, want %v", input, tc.args)
			}
		})
	}
}

// TestRewriteBdWispTierArgsMatchesStoreListContract pins the rewrite to the
// reason it exists: internal/beads' own bd list path always passes
// --include-infra, so the gc bd passthrough must not silently disagree with it
// on the same query.
func TestRewriteBdWispTierArgsMatchesStoreListContract(t *testing.T) {
	got := rewriteBdWispTierArgs([]string{"list", "--json", "--status=open", "--type=molecule", "--limit", "0"})
	if !containsArg(got, "--include-infra") {
		t.Fatalf("args = %v, want --include-infra (matching BdStore.listViaBDList)", got)
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
