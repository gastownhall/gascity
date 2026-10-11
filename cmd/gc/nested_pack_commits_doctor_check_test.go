package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/doctor"
)

func runNestedPackCommits(t *testing.T, lock string) *doctor.CheckResult {
	t.Helper()
	cityDir := t.TempDir()
	if lock != "" {
		if err := os.WriteFile(filepath.Join(cityDir, "packs.lock"), []byte(lock), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return newNestedPackCommitsDoctorCheck(cityDir).Run(&doctor.CheckContext{CityPath: cityDir})
}

func TestNestedPackCommitsReportsSplitParentAndSubpack(t *testing.T) {
	res := runNestedPackCommits(t, `schema = 1

[packs."https://github.com/example/packs/tree/main/gascity"]
version = "sha:2e7ec4c5598173ff00c20a2048ef28d4202bd38a"
commit = "2e7ec4c5598173ff00c20a2048ef28d4202bd38a"

[packs."https://github.com/example/packs.git//gascity/roles"]
version = "sha:f69ec02b00000000000000000000000000000000"
commit = "f69ec02b00000000000000000000000000000000"
`)
	if res.Status != doctor.StatusWarning || res.Severity != doctor.SeverityAdvisory {
		t.Fatalf("status/severity = %v/%v, want warning/advisory; message=%q", res.Status, res.Severity, res.Message)
	}
	payload, ok := res.Payload.(nestedPackCommitsPayload)
	if !ok || len(payload.Splits) != 1 {
		t.Fatalf("payload = %+v, want one split", res.Payload)
	}
	got := payload.Splits[0]
	if got.ParentSource != "https://github.com/example/packs/tree/main/gascity" ||
		got.NestedSource != "https://github.com/example/packs.git//gascity/roles" ||
		got.ParentCommit != "2e7ec4c5598173ff00c20a2048ef28d4202bd38a" ||
		got.NestedCommit != "f69ec02b00000000000000000000000000000000" {
		t.Errorf("split = %+v", got)
	}
}

func TestNestedPackCommitsCleanCases(t *testing.T) {
	tests := map[string]string{
		"no lockfile": "",
		"nested pack at the parent's commit": `schema = 1
[packs."https://github.com/example/packs/tree/main/gascity"]
commit = "2e7ec4c5598173ff00c20a2048ef28d4202bd38a"
[packs."https://github.com/example/packs/tree/main/gascity/roles"]
commit = "2e7ec4c5598173ff00c20a2048ef28d4202bd38a"
`,
		"sibling packs in one repository may differ": `schema = 1
[packs."https://github.com/example/packs/tree/main/gascity"]
commit = "2e7ec4c5598173ff00c20a2048ef28d4202bd38a"
[packs."https://github.com/example/packs/tree/main/gastown"]
commit = "f69ec02b00000000000000000000000000000000"
`,
		"name prefix is not a directory prefix": `schema = 1
[packs."https://github.com/example/packs/tree/main/gas"]
commit = "2e7ec4c5598173ff00c20a2048ef28d4202bd38a"
[packs."https://github.com/example/packs/tree/main/gascity"]
commit = "f69ec02b00000000000000000000000000000000"
`,
		"different repositories": `schema = 1
[packs."https://github.com/example/a/tree/main/gascity"]
commit = "2e7ec4c5598173ff00c20a2048ef28d4202bd38a"
[packs."https://github.com/example/b/tree/main/gascity/roles"]
commit = "f69ec02b00000000000000000000000000000000"
`,
	}
	for name, lock := range tests {
		t.Run(name, func(t *testing.T) {
			res := runNestedPackCommits(t, lock)
			if res.Status != doctor.StatusOK || res.Payload != nil {
				t.Fatalf("status = %v payload = %+v, want OK; details=%v", res.Status, res.Payload, res.Details)
			}
		})
	}
}

func TestNestedPackCommitsRepositoryRootContainsEverySubpack(t *testing.T) {
	res := runNestedPackCommits(t, `schema = 1
[packs."https://example.com/packs.git"]
commit = "2e7ec4c5598173ff00c20a2048ef28d4202bd38a"
[packs."https://example.com/packs.git//roles"]
commit = "f69ec02b00000000000000000000000000000000"
`)
	if res.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning; message=%q", res.Status, res.Message)
	}
}

func TestSubpathContains(t *testing.T) {
	tests := []struct {
		parent, nested string
		want           bool
	}{
		{"gascity", "gascity/roles", true},
		{"", "roles", true},
		{"gascity", "gascity", false},
		{"gas", "gascity", false},
		{"gascity/roles", "gascity", false},
		{"/gascity/", "gascity/roles/", true},
	}
	for _, tt := range tests {
		if got := subpathContains(tt.parent, tt.nested); got != tt.want {
			t.Errorf("subpathContains(%q, %q) = %v, want %v", tt.parent, tt.nested, got, tt.want)
		}
	}
}
