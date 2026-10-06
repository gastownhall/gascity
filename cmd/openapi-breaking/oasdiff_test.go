package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
)

// requireOasdiff returns the oasdiff binary. Local runs without it skip; the
// spec-ci lane sets GC_REQUIRE_OASDIFF=1 so the gate's end-to-end proof can
// never silently skip in CI.
func requireOasdiff(t *testing.T) string {
	t.Helper()
	bin, err := resolveOasdiff("")
	if err != nil {
		if os.Getenv("GC_REQUIRE_OASDIFF") == "1" {
			t.Fatalf("%v", err)
		}
		t.Skipf("%v (set GC_REQUIRE_OASDIFF=1 to fail instead)", err)
	}
	return bin
}

// TestGateAgainstOasdiff runs the real oasdiff binary with the checked-in
// policy's severity overrides over fixture specs.
func TestGateAgainstOasdiff(t *testing.T) {
	bin := requireOasdiff(t)
	checkedInPolicy := filepath.Join(bazeltest.RepoRoot(t), defaultPolicy)
	policyText, err := os.ReadFile(checkedInPolicy)
	if err != nil {
		t.Fatal(err)
	}
	waivedPolicy := filepath.Join(t.TempDir(), "policy.toml")
	if err := os.WriteFile(waivedPolicy, append(policyText, []byte(validWaiver)...), 0o600); err != nil {
		t.Fatal(err)
	}
	// An empty policy leaves every check at its oasdiff default level.
	defaultLevelsPolicy := filepath.Join(t.TempDir(), "default-levels.toml")
	if err := os.WriteFile(defaultLevelsPolicy, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name         string
		base         string // base spec fixture; empty means base.json
		revision     string
		policy       string
		wantUnwaived []string // "check target" of each unwaived finding
		wantWaived   int
	}{
		{
			name:         "removed optional response property is breaking",
			revision:     "removed-response-property.json",
			policy:       checkedInPolicy,
			wantUnwaived: []string{"response-optional-property-removed GET /v0/widgets/{id}"},
		},
		{
			name:         "removed problem-type URN is breaking",
			revision:     "removed-problem-type.json",
			policy:       checkedInPolicy,
			wantUnwaived: []string{"problem-type-removed components"},
		},
		{
			name:     "additive response field and problem type pass",
			revision: "additive.json",
			policy:   checkedInPolicy,
		},
		{
			name:       "waived removal passes",
			revision:   "removed-response-property.json",
			policy:     waivedPolicy,
			wantWaived: 1,
		},
		{
			name:     "new event type passes under the open-union overrides",
			base:     "event-union-base.json",
			revision: "added-event-type.json",
			policy:   checkedInPolicy,
		},
		{
			// Control: the same growth fails without the policy, so the
			// pass above is the two info overrides at work.
			name:     "new event type is breaking at oasdiff default levels",
			base:     "event-union-base.json",
			revision: "added-event-type.json",
			policy:   defaultLevelsPolicy,
			wantUnwaived: []string{
				"response-property-enum-value-added GET /v0/events/stream",
				"response-property-one-of-added GET /v0/events/stream",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := "base.json"
			if tc.base != "" {
				base = tc.base
			}
			v, err := check(context.Background(), options{
				Oasdiff:  bin,
				BaseFile: filepath.Join("testdata", base),
				Revision: filepath.Join("testdata", tc.revision),
				Policy:   tc.policy,
			})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, f := range v.Unwaived {
				got = append(got, f.ID+" "+f.target())
			}
			if strings.Join(got, "\n") != strings.Join(tc.wantUnwaived, "\n") {
				t.Errorf("unwaived = %q, want %q", got, tc.wantUnwaived)
			}
			if len(v.Waived) != tc.wantWaived {
				t.Errorf("waived = %+v, want %d", v.Waived, tc.wantWaived)
			}
		})
	}
}
