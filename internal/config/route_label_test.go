package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/shellquote"
)

// TestRouteLabelPredicateSharedWithWorkQuery is the structural counterpart to
// TestPoolDemandPredicateSharedWithWorkQuery, scoped to the new RouteLabel/
// RouteLabelAny fields (ga-i4lcx1). buildWorkQuery, buildRoutedPoolQuery, and
// buildPoolDemandQuery each construct routeLabelFilter{label: a.RouteLabel,
// labelAny: a.RouteLabelAny} independently from the same agent fields, so a
// future edit that reads the fields into one tier's filter but not the
// other's would silently reintroduce the ga-ut74jh divergence. The flag name
// and label text are asserted as separate substrings (rather than the fully
// quoted --label='...' fragment) because the final EffectiveWorkQuery/
// EffectivePoolDemandQuery output is itself shellquote-wrapped by
// shellquote.Join, which re-escapes any single quotes already present in the
// inner script — including the ones routeLabelFilter.shellArgs adds around
// the label text. The flag name and label text contain no quote characters,
// so they survive that outer wrap unchanged and are safe to assert on
// directly; TestRouteLabelQuotesShellMetacharacters below covers the escaping
// itself end-to-end.
func TestRouteLabelPredicateSharedWithWorkQuery(t *testing.T) {
	tests := []struct {
		name          string
		routeLabel    []string
		routeLabelAny []string
		wantFragments []string
	}{
		{
			name:          "label only",
			routeLabel:    []string{"needs-claude-review"},
			wantFragments: []string{"--label=", "needs-claude-review"},
		},
		{
			name:          "label-any only",
			routeLabelAny: []string{"urgent", "vip"},
			wantFragments: []string{"--label-any=", "urgent,vip"},
		},
		{
			name:          "both",
			routeLabel:    []string{"needs-claude-review"},
			routeLabelAny: []string{"urgent", "vip"},
			wantFragments: []string{"--label=", "needs-claude-review", "--label-any=", "urgent,vip"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := Agent{Name: "reviewer", Dir: "cairn", RouteLabel: tt.routeLabel, RouteLabelAny: tt.routeLabelAny}
			wq := a.EffectiveWorkQuery()
			demand := a.EffectivePoolDemandQuery()
			for _, frag := range tt.wantFragments {
				if !strings.Contains(wq, frag) {
					t.Errorf("EffectiveWorkQuery() missing route-label fragment %q in %q", frag, wq)
				}
				if !strings.Contains(demand, frag) {
					t.Errorf("EffectivePoolDemandQuery() missing route-label fragment %q in %q", frag, demand)
				}
			}
		})
	}
}

// TestRouteLabelNarrowsWorkQueryAndPoolDemandTogether is the behavioral
// counterpart: a fake bd that only returns the routed bead when --label is
// present proves the label actually reaches bd's argv (not just the
// rendered string) identically on both the claim path (EffectiveWorkQuery)
// and the demand path (EffectivePoolDemandQuery), and that removing
// RouteLabel narrows both back open together.
func TestRouteLabelNarrowsWorkQueryAndPoolDemandTogether(t *testing.T) {
	target := "cairn/reviewer"
	bdScript := `#!/bin/sh
case "$*" in
  *"ready --metadata-field gc.routed_to=` + target + `"*"--unassigned"*"--exclude-type=epic"*"--label=needs-claude-review"*)
    printf '[{"id":"ca-routed"}]'
    ;;
  *)
    printf '[]'
    ;;
esac
`
	labeled := reviewerShapedAgent()
	wqOut := strings.TrimSpace(runEffectiveWorkQuery(t, *labeled, nil, bdScript))
	if wqOut != `[{"id":"ca-routed"}]` {
		t.Errorf("EffectiveWorkQuery() output = %q, want the routed+labeled bead", wqOut)
	}
	demandOut := strings.TrimSpace(runShellWithFakeBd(t, labeled.EffectivePoolDemandQuery(), nil, bdScript))
	if demandOut == "0" {
		t.Errorf("EffectivePoolDemandQuery() output = %q, want >0 when the labeled routed bead is present", demandOut)
	}

	unlabeled := Agent{Name: "reviewer", Dir: "cairn"}
	wqUnlabeled := strings.TrimSpace(runEffectiveWorkQuery(t, unlabeled, nil, bdScript))
	if wqUnlabeled != `[]` {
		t.Errorf("EffectiveWorkQuery() output (no RouteLabel) = %q, want [] since the fake bd requires --label", wqUnlabeled)
	}
	demandUnlabeledOut := strings.TrimSpace(runShellWithFakeBd(t, unlabeled.EffectivePoolDemandQuery(), nil, bdScript))
	if demandUnlabeledOut != "0" {
		t.Errorf("EffectivePoolDemandQuery() output (no RouteLabel) = %q, want 0 since the fake bd requires --label", demandUnlabeledOut)
	}
}

// TestValidateAgentsRouteLabelExclusiveWithRawOverride covers FR-03: a raw
// work_query/scale_check override replaces the multi-tier discovery contract
// wholesale, while RouteLabel/RouteLabelAny narrow the shared routed-pool
// predicate in place. Combining both leaves it ambiguous which mechanism
// governs, so ValidateAgents rejects the combination at load time instead of
// picking a silent precedence rule.
func TestValidateAgentsRouteLabelExclusiveWithRawOverride(t *testing.T) {
	// RouteLabel alone: OK.
	agents := []Agent{{Name: "reviewer", Dir: "cairn", RouteLabel: []string{"needs-claude-review"}}}
	if err := ValidateAgents(agents); err != nil {
		t.Errorf("route_label alone: unexpected error: %v", err)
	}

	// RouteLabelAny alone: OK.
	agents = []Agent{{Name: "reviewer", Dir: "cairn", RouteLabelAny: []string{"urgent", "vip"}}}
	if err := ValidateAgents(agents); err != nil {
		t.Errorf("route_label_any alone: unexpected error: %v", err)
	}

	// RouteLabel + WorkQuery: rejected.
	agents = []Agent{{
		Name:       "reviewer",
		Dir:        "cairn",
		RouteLabel: []string{"needs-claude-review"},
		WorkQuery:  "custom-query",
	}}
	if err := ValidateAgents(agents); err == nil {
		t.Error("route_label + work_query: expected error, got nil")
	}

	// RouteLabelAny + ScaleCheck: rejected.
	agents = []Agent{{
		Name:          "reviewer",
		Dir:           "cairn",
		RouteLabelAny: []string{"urgent"},
		ScaleCheck:    "echo 3",
	}}
	if err := ValidateAgents(agents); err == nil {
		t.Error("route_label_any + scale_check: expected error, got nil")
	}

	// Neither set: OK (existing default behavior, unaffected by FR-03).
	agents = []Agent{{Name: "reviewer", Dir: "cairn"}}
	if err := ValidateAgents(agents); err != nil {
		t.Errorf("neither set: unexpected error: %v", err)
	}
}

// TestRouteLabelQuotesShellMetacharacters is the injection-safety proof for
// NFR-01. A label value carrying a command substitution must reach bd as
// inert argument text, never as executable shell syntax, regardless of how
// many shellquote.Join/Quote layers wrap the final EffectiveWorkQuery output.
// The canary file is the end-to-end proof: if it exists after actually
// executing the rendered command through a real shell, the label was
// interpolated unsafely somewhere in the chain.
func TestRouteLabelQuotesShellMetacharacters(t *testing.T) {
	tmp := t.TempDir()
	canary := filepath.Join(tmp, "pwned")
	malicious := "x $(touch " + canary + ") y"

	a := Agent{Name: "reviewer", Dir: "cairn", RouteLabel: []string{malicious}}
	wq := a.EffectiveWorkQuery()

	// The label text itself contains no quote characters, so it is
	// unaffected by shellquote.Quote's '-escaping and must appear verbatim
	// as inert data in the rendered command.
	if !strings.Contains(wq, malicious) {
		t.Fatalf("expected RouteLabel value to reach EffectiveWorkQuery() as inert data, not found in %q", wq)
	}

	runShellWithFakeBd(t, wq, nil, "#!/bin/sh\nprintf '[]'\n")

	if _, err := os.Stat(canary); err == nil {
		t.Fatal("RouteLabel value executed as a live shell command substitution — injection")
	}

	// Sanity-check the primitive this whole mechanism leans on, so a future
	// change to shellquote.Quote's escaping strategy fails here first rather
	// than as a silent hole discovered only via the canary above.
	if q := shellquote.Quote(malicious); !strings.Contains(q, "'") {
		t.Fatalf("shellquote.Quote(%q) = %q, expected single-quote wrapping", malicious, q)
	}
}
