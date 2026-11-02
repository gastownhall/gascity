package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/bdflags"
)

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
			name: "non-wisp type alone is left alone",
			args: []string{"list", "--type=bug"},
			want: []string{"list", "--type=bug"},
		},
		{
			name: "non-wisp type with output flags alone is left alone",
			args: []string{"list", "--type=task", "--json", "--limit", "10"},
			want: []string{"list", "--type=task", "--json", "--limit", "10"},
		},
		{
			name: "unfiltered list is left alone",
			args: []string{"list", "--json", "--limit", "0"},
			want: []string{"list", "--json", "--limit", "0"},
		},
		{
			name: "output-shaping flags alone are left alone",
			args: []string{"list", "--json", "--limit", "0", "--offset", "5", "--sort", "created", "--brief", "--skip-labels"},
			want: []string{"list", "--json", "--limit", "0", "--offset", "5", "--sort", "created", "--brief", "--skip-labels"},
		},
		{
			name: "store-scoping globals alone are left alone",
			args: []string{"list", "--json", "-C", "/tmp/city", "--profile", "default"},
			want: []string{"list", "--json", "-C", "/tmp/city", "--profile", "default"},
		},
		{
			name: "assignee and status filters gain include-infra (deacon startup shape)",
			args: []string{"list", "--status=open", "--assignee=gastown.deacon", "--json", "--limit", "0"},
			want: []string{"list", "--status=open", "--assignee=gastown.deacon", "--json", "--limit", "0", "--include-infra"},
		},
		{
			name: "comma-separated status widens",
			args: []string{"list", "--status=open,in_progress", "--assignee=gastown.boot"},
			want: []string{"list", "--status=open,in_progress", "--assignee=gastown.boot", "--include-infra"},
		},
		{
			name: "short status flag widens",
			args: []string{"list", "-s", "open"},
			want: []string{"list", "-s", "open", "--include-infra"},
		},
		{
			name: "short assignee flag widens",
			args: []string{"list", "-a", "worker-1", "--json"},
			want: []string{"list", "-a", "worker-1", "--json", "--include-infra"},
		},
		{
			name: "label filter widens even when its value names the tier",
			args: []string{"list", "--label", "molecule"},
			want: []string{"list", "--label", "molecule", "--include-infra"},
		},
		{
			name: "title-contains filter widens even when its value names the tier",
			args: []string{"list", "--title-contains=molecule"},
			want: []string{"list", "--title-contains=molecule", "--include-infra"},
		},
		{
			name: "parent filter widens",
			args: []string{"list", "--parent", "city-abc", "--json"},
			want: []string{"list", "--parent", "city-abc", "--json", "--include-infra"},
		},
		{
			name: "time-window filter widens",
			args: []string{"list", "--created-after", "2026-10-30T00:00:00Z"},
			want: []string{"list", "--created-after", "2026-10-30T00:00:00Z", "--include-infra"},
		},
		{
			name: "metadata-field filter widens",
			args: []string{"list", "--metadata-field=gc.routed_to=gastown.dog"},
			want: []string{"list", "--metadata-field=gc.routed_to=gastown.dog", "--include-infra"},
		},
		{
			name: "exclude-type filter widens",
			args: []string{"list", "--exclude-type=task"},
			want: []string{"list", "--exclude-type=task", "--include-infra"},
		},
		{
			name: "row predicate bool widens",
			args: []string{"list", "--pinned", "--json"},
			want: []string{"list", "--pinned", "--json", "--include-infra"},
		},
		{
			name: "no-assignee predicate widens",
			args: []string{"list", "--no-assignee"},
			want: []string{"list", "--no-assignee", "--include-infra"},
		},
		{
			name: "non-wisp type plus a row predicate widens",
			args: []string{"list", "--type=bug", "--status=open"},
			want: []string{"list", "--type=bug", "--status=open", "--include-infra"},
		},
		{
			name: "include-ephemeral short-circuits",
			args: []string{"list", "--include-ephemeral", "--status=open"},
			want: []string{"list", "--include-ephemeral", "--status=open"},
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
			name: "unrecognized flag before a predicate still fails open",
			args: []string{"list", "--brand-new-flag", "x", "--status=open"},
			want: []string{"list", "--brand-new-flag", "x", "--status=open"},
		},
		{
			name: "dangling value flag is left alone",
			args: []string{"list", "--status"},
			want: []string{"list", "--status"},
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

// TestRewriteBdWispTierArgsDeaconStartupQuery pins the exact shape that
// leaked: the deacon startup reconciliation asks for its own open patrol
// wisps with --assignee and --status, and before the widening bd hid the
// wisps tier from that query, so an already-poured successor wisp was
// invisible and every startup poured a duplicate (city-jf7: three surplus
// wisps on 2026-10-30).
func TestRewriteBdWispTierArgsDeaconStartupQuery(t *testing.T) {
	got := rewriteBdWispTierArgs([]string{"list", "--status=open,in_progress", "--assignee=gastown.deacon", "--json", "--limit=0"})
	if !containsArg(got, "--include-infra") {
		t.Fatalf("args = %v, want --include-infra: the deacon startup check must see its own ephemeral patrol wisp", got)
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

// TestBdWispTierOutputOnlyTablesAreGroundedInBdFlagManifest keeps the
// scanner's output-only tables honest against bdflags' `list` manifest. Row
// predicates need no table entry — they are the default — but an entry naming
// a flag bd no longer has (or never had) is dead weight that silently classifies
// nothing, and a renamed bd flag could leave a real display flag misclassified
// as a predicate (harmless widening) or a predicate misclassified as display
// (the leak class). The manifest test in bdflags pins the vocabulary; this
// pins the tables to it.
func TestBdWispTierOutputOnlyTablesAreGroundedInBdFlagManifest(t *testing.T) {
	knownAny := bdflags.ValueFlags("list") // merged with the global value flags
	for name := range bdListOutputOnlyValueFlags {
		if !knownAny[name] {
			t.Errorf("bdListOutputOnlyValueFlags entry %q is not a known bd list value flag (per-sub manifest or global set): typo or drift — remove or reclassify it", name)
		}
	}
	knownBool := bdflags.BoolFlags("list") // merged with the global bool flags
	for name := range bdListOutputOnlyBoolFlags {
		if !knownBool[name] {
			t.Errorf("bdListOutputOnlyBoolFlags entry %q is not a known bd list bool flag (per-sub manifest or global set): typo or drift — remove or reclassify it", name)
		}
	}
	for _, name := range []string{"--include-infra", "--include-ephemeral"} {
		if !knownBool[name] {
			t.Errorf("short-circuit flag %q is not a known bd list bool flag: bd renamed or dropped it — update bdListNeedsWispTierAccess", name)
		}
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
