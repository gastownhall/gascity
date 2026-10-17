package core

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// reaperDeletedCountFilter extracts the jq filter the type-safe session
// prune path uses to read DELETED_COUNT out of `bd delete --json`'s output,
// straight from the shipped script, so this test cannot drift from it.
func reaperDeletedCountFilter(t *testing.T) string {
	t.Helper()
	data, err := PackFS.ReadFile("assets/scripts/reaper.sh")
	if err != nil {
		t.Fatalf("reading reaper.sh: %v", err)
	}
	re := regexp.MustCompile(`DELETED_COUNT=\$\(printf '%s' "\$DELETE_OUT" \| jq -r '([^']*)'`)
	m := re.FindSubmatch(data)
	if m == nil {
		t.Fatal("reaper.sh: could not find the DELETED_COUNT jq filter; script shape changed")
	}
	return string(m[1])
}

// runReaperDeletedCountFilter feeds json through the real jq filter the
// script uses and returns jq's stdout, trimmed.
func runReaperDeletedCountFilter(t *testing.T, filter, json string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq required")
	}
	cmd := exec.Command("jq", "-r", filter)
	cmd.Stdin = strings.NewReader(json)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// TestReaperDeletedCountFilterHandlesBothBdDeleteShapes pins gascity#8007: bd
// 1.3.1's `delete --json` omits deleted_count entirely for a one-id batch
// (deleted is a bare string there, not an array), which the old filter
// (`.deleted_count // empty`) read as empty and treated as a failure even
// though the delete succeeded.
func TestReaperDeletedCountFilterHandlesBothBdDeleteShapes(t *testing.T) {
	filter := reaperDeletedCountFilter(t)

	for _, tc := range []struct {
		name string
		json string
		want string
	}{
		{
			name: "single id omits deleted_count, deleted is a bare string",
			json: `{"deleted":"dc-1y8wp","dependencies_removed":0,"references_updated":0,"schema_version":1}`,
			want: "1",
		},
		{
			name: "two ids carries deleted_count, deleted is an array",
			json: `{"deleted":["dc-pa6rt","dc-yltjp"],"deleted_count":2,"dependencies_removed":0,"events_removed":2,"labels_removed":0,"orphaned_issues":null,"references_updated":0,"schema_version":1}`,
			want: "2",
		},
		{
			name: "zero ids: deleted is an empty array, no deleted_count",
			json: `{"deleted":[],"dependencies_removed":0,"references_updated":0,"schema_version":1}`,
			want: "0",
		},
		{
			name: "malformed output stays empty, so the caller's failure branch still fires",
			json: `{"error":"something else entirely"}`,
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := runReaperDeletedCountFilter(t, filter, tc.json)
			if err != nil {
				t.Fatalf("jq: %v", err)
			}
			if got != tc.want {
				t.Fatalf("DELETED_COUNT = %q, want %q", got, tc.want)
			}
		})
	}
}
