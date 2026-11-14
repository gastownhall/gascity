package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/fsys"
)

// This file expresses the route_label / route_label_any acceptance criteria
// (ga-878prh.1, port of PR #4647) at the level the rest of the pool-demand
// predicate is pinned: the descriptor in Go, the generated claim and count
// queries EXECUTED against a reader that filters the way bd does, and the load
// time validation. The controller's in-process predicate consumes the same
// descriptor and is pinned in cmd/gc (demand_serve_predicate_route_label_test.go).

const (
	routeLabelReview = "needs-claude-review"
	routeLabelUrgent = "urgent"
)

// routeLabelRows is a routed queue every case reads. Four rows are eligible by
// route, ownership, holds and human parking and differ only in labels; the rest
// are rows the pool-demand tier must never serve whatever the labels say.
func routeLabelRows(target string) []fakeReaderRow {
	routed := func(id, assignee string, labels ...string) fakeReaderRow {
		return fakeReaderRow{
			ID: id, Status: "open", Type: "task", Assignee: assignee, Labels: labels,
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: target},
		}
	}
	foreign := routed("r-foreign", "", routeLabelReview, routeLabelUrgent)
	foreign.Metadata = map[string]string{beadmeta.RoutedToMetadataKey: "example/someone-else"}
	return []fakeReaderRow{
		routed("r-both", "", routeLabelReview, routeLabelUrgent),
		routed("r-review", "", routeLabelReview),
		routed("r-urgent", "", routeLabelUrgent),
		routed("r-plain", ""),
		routed("r-held", "", routeLabelReview, routeLabelUrgent, beadmeta.DispatchHoldLabels[0]),
		routed("r-human", "", routeLabelReview, routeLabelUrgent, beadmeta.HumanLabel),
		routed("r-owned", "someone-else", routeLabelReview, routeLabelUrgent),
		foreign,
	}
}

// routeLabelCases is the AND / OR matrix over routeLabelRows: which eligible
// rows an agent carrying the labels is served.
func routeLabelCases() []struct {
	name     string
	label    []string
	labelAny []string
	want     []string
} {
	return []struct {
		name     string
		label    []string
		labelAny []string
		want     []string
	}{
		{name: "no labels is unchanged", want: []string{"r-both", "r-plain", "r-review", "r-urgent"}},
		{name: "label requires it", label: []string{routeLabelReview}, want: []string{"r-both", "r-review"}},
		{name: "labels are an AND", label: []string{routeLabelReview, routeLabelUrgent}, want: []string{"r-both"}},
		{name: "label-any is an OR", labelAny: []string{routeLabelUrgent, "absent"}, want: []string{"r-both", "r-urgent"}},
		{name: "label AND label-any combine", label: []string{routeLabelReview}, labelAny: []string{routeLabelUrgent}, want: []string{"r-both"}},
		{name: "unmatched label serves nothing", label: []string{"absent"}, want: nil},
		{name: "unmatched label-any serves nothing", labelAny: []string{"absent", "missing"}, want: nil},
		{name: "matched label with unmatched label-any serves nothing", label: []string{routeLabelReview}, labelAny: []string{"absent"}, want: nil},
	}
}

// runReaderQuery executes command against the filtering fake reader (as both bd
// and gc) over rows.
func runReaderQuery(t *testing.T, command string, env map[string]string, rows []fakeReaderRow) (generatedQueryResult, string) {
	t.Helper()
	requireJQ(t)
	tmp := t.TempDir()
	write := func(name string, set []fakeReaderRow) string {
		if set == nil {
			set = []fakeReaderRow{}
		}
		data, err := json.Marshal(set)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		path := filepath.Join(tmp, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}
	argvLog := filepath.Join(tmp, "argv.log")
	queryEnv := map[string]string{
		"FAKE_ROWS":     write("rows.json", rows),
		"FAKE_WISPS":    write("wisps.json", nil),
		"FAKE_READ_LOG": filepath.Join(tmp, "reads.log"),
		"FAKE_ARGV_LOG": argvLog,
	}
	for k, v := range env {
		queryEnv[k] = v
	}
	res := runGeneratedQueryWithBD(t, command, queryEnv, fakeFilteringReader, fakeFilteringReader)
	log, _ := os.ReadFile(argvLog)
	return res, string(log)
}

func servedIDs(t *testing.T, res generatedQueryResult) []string {
	t.Helper()
	if res.exit != 0 {
		t.Fatalf("query exited %d; stderr=%s", res.exit, res.stderr)
	}
	var rows []fakeReaderRow
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.stdout)), &rows); err != nil {
		t.Fatalf("query output is not a JSON row array: %v (output %q)", err, res.stdout)
	}
	var ids []string
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	slices.Sort(ids)
	return ids
}

func demandCount(t *testing.T, res generatedQueryResult) int {
	t.Helper()
	if res.exit != 0 {
		t.Fatalf("count query exited %d; stderr=%s", res.exit, res.stderr)
	}
	n, err := strconv.Atoi(strings.TrimSpace(res.stdout))
	if err != nil {
		t.Fatalf("count query printed %q, want an integer: %v", res.stdout, err)
	}
	return n
}

// TestRouteLabelClaimAndCountAgreeAcrossReaders is the load-bearing behavioral
// pin: for every label combination, on the single-store bd readers AND the
// federated `gc ready` reader, the worker's claim query serves exactly the rows
// the reconciler's count query counts, and both are the rows the AND / OR rules
// admit. A mismatched-only queue is zero demand (nothing to spawn a seat for).
func TestRouteLabelClaimAndCountAgreeAcrossReaders(t *testing.T) {
	agents := map[string]struct {
		agent  Agent
		target string
		env    map[string]string
	}{
		"pool": {
			agent:  Agent{Name: "worker", Dir: "example"},
			target: "example/worker",
			env:    map[string]string{"GC_SESSION_ORIGIN": "ephemeral"},
		},
		"named singleton": {
			agent:  Agent{Name: "olivia", Dir: "example"},
			target: "example/olivia",
			env:    map[string]string{"GC_SESSION_ORIGIN": "named", "GC_ALIAS": "example/olivia"},
		},
	}
	for agentName, fixture := range agents {
		for topoName, topo := range assignedHoldTopologies() {
			for _, tc := range routeLabelCases() {
				t.Run(agentName+"/"+topoName+"/"+tc.name, func(t *testing.T) {
					a := fixture.agent
					a.RouteLabel = tc.label
					a.RouteLabelAny = tc.labelAny
					rows := routeLabelRows(fixture.target)

					claim, _ := runReaderQuery(t, a.EffectiveWorkQueryFor(topo), fixture.env, rows)
					if got := servedIDs(t, claim); !slices.Equal(got, tc.want) {
						t.Errorf("EffectiveWorkQueryFor served %v, want %v", got, tc.want)
					}
					routedSplit, _ := runReaderQuery(t, a.EffectiveRoutedPoolQueryFor(topo), fixture.env, rows)
					if got := servedIDs(t, routedSplit); !slices.Equal(got, tc.want) {
						t.Errorf("EffectiveRoutedPoolQueryFor served %v, want %v", got, tc.want)
					}
					count, _ := runReaderQuery(t, a.EffectivePoolDemandQueryFor(topo), nil, rows)
					if got := demandCount(t, count); got != len(tc.want) {
						t.Errorf("EffectivePoolDemandQueryFor counted %d, want %d (%v)", got, len(tc.want), tc.want)
					}
				})
			}
		}
	}
}

// TestRouteLabelSingleStoreAndFederatedServeTheSameRows is the parity proof the
// federated reader owes the single-store one: same queue, same labels, same
// rows, whichever reader the topology selects.
func TestRouteLabelSingleStoreAndFederatedServeTheSameRows(t *testing.T) {
	for _, tc := range routeLabelCases() {
		t.Run(tc.name, func(t *testing.T) {
			a := Agent{Name: "worker", Dir: "example", RouteLabel: tc.label, RouteLabelAny: tc.labelAny}
			env := map[string]string{"GC_SESSION_ORIGIN": "ephemeral"}
			rows := routeLabelRows("example/worker")
			single, _ := runReaderQuery(t, a.EffectiveWorkQueryFor(singleStoreTopology()), env, rows)
			federated, _ := runReaderQuery(t, a.EffectiveWorkQueryFor(federatedTopology()), env, rows)
			if s, f := servedIDs(t, single), servedIDs(t, federated); !slices.Equal(s, f) {
				t.Errorf("single-store served %v, federated served %v", s, f)
			}
		})
	}
}

// TestRouteLabelDoesNotNarrowOwnedWork: label requirements narrow what an
// agent may pick up from the shared routed queue and nothing it already owns.
// An in_progress or assigned-ready row keeps being served whatever its labels,
// and no assignee-scoped read carries a label flag.
func TestRouteLabelDoesNotNarrowOwnedWork(t *testing.T) {
	owned := func(status string) fakeReaderRow {
		row := assignedOpenRow("owned", assignedHoldTestSessionID, "unrelated")
		row.Status = status
		return row
	}
	routed := routedOpenRow()
	routed.Labels = []string{routeLabelReview}
	for topoName, topo := range assignedHoldTopologies() {
		for _, status := range []string{"in_progress", "open"} {
			t.Run(topoName+"/"+status, func(t *testing.T) {
				a := assignedHoldPoolAgent()
				a.RouteLabel = []string{routeLabelReview}
				a.RouteLabelAny = []string{routeLabelUrgent, "other"}
				res, argv := runReaderQuery(t, a.EffectiveWorkQueryFor(topo), assignedHoldSeatEnv(), []fakeReaderRow{owned(status), routed})
				if got := servedIDs(t, res); !slices.Equal(got, []string{"owned"}) {
					t.Fatalf("owned %s work with unrelated labels: served %v, want only the owned row", status, got)
				}
				assertOwnedReadsCarryNoLabelFlags(t, argv)
			})
		}
	}
}

// assertOwnedReadsCarryNoLabelFlags walks the argv log (one block per call: the
// binary, then each argument indented) and fails if a call scoped to an
// assignee carries --label or --label-any.
func assertOwnedReadsCarryNoLabelFlags(t *testing.T, argv string) {
	t.Helper()
	var calls [][]string
	for _, line := range strings.Split(argv, "\n") {
		switch {
		case line == "":
		case !strings.HasPrefix(line, "  "):
			calls = append(calls, []string{line})
		default:
			calls[len(calls)-1] = append(calls[len(calls)-1], strings.TrimPrefix(line, "  "))
		}
	}
	for _, call := range calls {
		scoped, labeled := false, false
		for _, arg := range call[1:] {
			scoped = scoped || strings.HasPrefix(arg, "--assignee")
			labeled = labeled || arg == "--label" || arg == "--label-any" ||
				strings.HasPrefix(arg, "--label=") || strings.HasPrefix(arg, "--label-any=")
		}
		if scoped && labeled {
			t.Errorf("assignee-scoped read carries a route label flag: %v", call)
		}
	}
}

// TestRouteLabelLeavesBootAndDeathHooksUntouched: the on_boot / on_death
// recovery commands are about work an agent owned or abandoned, so labels must
// not reach them.
func TestRouteLabelLeavesBootAndDeathHooksUntouched(t *testing.T) {
	plain := Agent{Name: "worker", Dir: "example"}
	labeled := plain
	labeled.RouteLabel = []string{routeLabelReview}
	labeled.RouteLabelAny = []string{routeLabelUrgent}
	for _, topo := range assignedHoldTopologies() {
		if got, want := labeled.EffectiveOnBootFor(topo), plain.EffectiveOnBootFor(topo); got != want {
			t.Errorf("EffectiveOnBootFor changed with route labels:\n got %q\nwant %q", got, want)
		}
		if got, want := labeled.EffectiveOnDeathFor(topo), plain.EffectiveOnDeathFor(topo); got != want {
			t.Errorf("EffectiveOnDeathFor changed with route labels:\n got %q\nwant %q", got, want)
		}
	}
}

// TestRouteLabelEmptyLeavesGeneratedQueriesUnlabeled: an agent without labels
// renders no label flag, so every existing city's queries are byte-for-byte
// unchanged.
func TestRouteLabelEmptyLeavesGeneratedQueriesUnlabeled(t *testing.T) {
	a := Agent{Name: "worker", Dir: "example"}
	for name, cmd := range map[string]string{
		"work":   a.EffectiveWorkQuery(),
		"routed": a.EffectiveRoutedPoolQuery(),
		"demand": a.EffectivePoolDemandQuery(),
	} {
		if strings.Contains(cmd, "--label") {
			t.Errorf("%s query of an agent with no route labels carries a label flag: %q", name, cmd)
		}
	}
	if got, want := PoolDemandServeRulesFor(&a), PoolDemandServeRulesForQuery(); !slices.Equal(got.ExcludeLabels, want.ExcludeLabels) ||
		!slices.Equal(got.ExcludeTypes, want.ExcludeTypes) || got.RequireUnassigned != want.RequireUnassigned ||
		len(got.RequireLabels) != 0 || len(got.RequireAnyLabels) != 0 {
		t.Errorf("PoolDemandServeRulesFor(no labels) = %+v, want the base rules %+v", got, want)
	}
}

// TestPoolDemandServeRulesForCarriesRouteLabels: the descriptor is the single
// source the generated queries and the controller's in-process predicate read,
// so the per-agent requirements live on it, next to the base rules, and the
// base rules (holds, human, epic, unassigned) are never relaxed by them.
func TestPoolDemandServeRulesForCarriesRouteLabels(t *testing.T) {
	a := Agent{Name: "worker", Dir: "example", RouteLabel: []string{routeLabelReview}, RouteLabelAny: []string{routeLabelUrgent, "vip"}}
	rules := PoolDemandServeRulesFor(&a)
	if !slices.Equal(rules.RequireLabels, []string{routeLabelReview}) {
		t.Errorf("RequireLabels = %v, want %v", rules.RequireLabels, a.RouteLabel)
	}
	if !slices.Equal(rules.RequireAnyLabels, []string{routeLabelUrgent, "vip"}) {
		t.Errorf("RequireAnyLabels = %v, want %v", rules.RequireAnyLabels, a.RouteLabelAny)
	}
	base := PoolDemandServeRulesForQuery()
	if !rules.RequireUnassigned || !slices.Equal(rules.ExcludeTypes, base.ExcludeTypes) || !slices.Equal(rules.ExcludeLabels, base.ExcludeLabels) {
		t.Errorf("route labels relaxed the base serve rules: %+v vs %+v", rules, base)
	}
	// The rules own their slices: a caller mutating the descriptor must not
	// reach the agent's config.
	if len(rules.RequireLabels) == 0 {
		t.Fatal("RequireLabels is empty; nothing to check for aliasing")
	}
	rules.RequireLabels[0] = "mutated"
	if a.RouteLabel[0] != routeLabelReview {
		t.Errorf("PoolDemandServeRulesFor aliases Agent.RouteLabel: %v", a.RouteLabel)
	}
}

// TestRouteLabelValuesReachBdAsInertArguments is the injection-safety proof. A
// label carrying shell syntax must arrive at the reader as exactly one argument
// of the label flag, on the claim path, the count path and the federated reader,
// and must never execute. The canary file proves non-execution; the argv log
// proves the value was not split, expanded or re-quoted.
func TestRouteLabelValuesReachBdAsInertArguments(t *testing.T) {
	canary := filepath.Join(t.TempDir(), "pwned")
	hostile := []string{
		"x $(touch " + canary + ") y",
		"`touch " + canary + "`",
		"a;touch " + canary,
		`it's "quoted" \ back`,
		"glob*[ab]?",
		"two  spaces and\ttab",
	}
	for topoName, topo := range map[string]QueryTopology{"single-store": singleStoreTopology(), "federated": federatedTopology()} {
		for _, value := range hostile {
			t.Run(topoName+"/"+value, func(t *testing.T) {
				a := Agent{Name: "worker", Dir: "example", RouteLabel: []string{value}, RouteLabelAny: []string{value, "other"}}
				env := map[string]string{"GC_SESSION_ORIGIN": "ephemeral"}
				for name, command := range map[string]string{
					"work":   a.EffectiveWorkQueryFor(topo),
					"routed": a.EffectiveRoutedPoolQueryFor(topo),
					"demand": a.EffectivePoolDemandQueryFor(topo),
				} {
					res, argv := runReaderQuery(t, command, env, routeLabelRows("example/worker"))
					if res.exit != 0 {
						t.Fatalf("%s query exited %d: %s", name, res.exit, res.stderr)
					}
					if _, err := os.Stat(canary); err == nil {
						t.Fatalf("%s query executed the label as shell syntax", name)
					}
					if !argvHasFlagValue(argv, "--label", value) {
						t.Errorf("%s query never passed --label with exactly %q as one argument; argv:\n%s", name, value, argv)
					}
					if !argvHasFlagValue(argv, "--label-any", value) {
						t.Errorf("%s query never passed --label-any with exactly %q as one argument; argv:\n%s", name, value, argv)
					}
				}
			})
		}
	}
}

// argvHasFlagValue reports whether some logged call passes flag with exactly
// value, in either the `--flag value` or the `--flag=value` spelling.
func argvHasFlagValue(argv, flag, value string) bool {
	lines := strings.Split(argv, "\n")
	for i, line := range lines {
		arg := strings.TrimPrefix(line, "  ")
		if arg == flag+"="+value {
			return true
		}
		if arg == flag && i+1 < len(lines) && strings.TrimPrefix(lines[i+1], "  ") == value {
			return true
		}
	}
	return false
}

// TestValidateAgentsRouteLabelExclusiveWithRawOverride: a raw work_query or
// scale_check replaces the generated multi-tier discovery contract wholesale,
// while route labels narrow the shared routed tier in place. Combining them
// leaves it ambiguous which governs, so the combination is rejected instead of
// picking a silent precedence.
func TestValidateAgentsRouteLabelExclusiveWithRawOverride(t *testing.T) {
	ok := []Agent{
		{Name: "reviewer", Dir: "cairn", RouteLabel: []string{routeLabelReview}},
		{Name: "reviewer", Dir: "cairn", RouteLabelAny: []string{routeLabelUrgent, "vip"}},
		{Name: "reviewer", Dir: "cairn", RouteLabel: []string{routeLabelReview}, RouteLabelAny: []string{routeLabelUrgent}},
		{Name: "reviewer", Dir: "cairn"},
		{Name: "reviewer", Dir: "cairn", WorkQuery: "custom-query", ScaleCheck: "echo 3"},
	}
	for i, a := range ok {
		if err := ValidateAgents([]Agent{a}); err != nil {
			t.Errorf("ok[%d]: unexpected error: %v", i, err)
		}
	}
	rejected := map[string]Agent{
		"route_label+work_query":      {Name: "reviewer", Dir: "cairn", RouteLabel: []string{routeLabelReview}, WorkQuery: "custom-query"},
		"route_label+scale_check":     {Name: "reviewer", Dir: "cairn", RouteLabel: []string{routeLabelReview}, ScaleCheck: "echo 3"},
		"route_label_any+work_query":  {Name: "reviewer", Dir: "cairn", RouteLabelAny: []string{routeLabelUrgent}, WorkQuery: "custom-query"},
		"route_label_any+scale_check": {Name: "reviewer", Dir: "cairn", RouteLabelAny: []string{routeLabelUrgent}, ScaleCheck: "echo 3"},
	}
	for name, a := range rejected {
		err := ValidateAgents([]Agent{a})
		if err == nil {
			t.Errorf("%s: expected an error, got nil", name)
			continue
		}
		if !strings.Contains(err.Error(), "route_label") {
			t.Errorf("%s: error %q does not name route_label", name, err)
		}
	}
}

// TestValidateAgentsRouteLabelRejectsUnrepresentableLabels: bd splits a label
// flag's value on commas and drops blanks, so a label containing a comma
// silently becomes two labels and an empty label silently becomes none. Either
// would widen or narrow routing without saying so; reject them at load.
func TestValidateAgentsRouteLabelRejectsUnrepresentableLabels(t *testing.T) {
	for name, a := range map[string]Agent{
		"comma in route_label":      {Name: "reviewer", RouteLabel: []string{"a,b"}},
		"comma in route_label_any":  {Name: "reviewer", RouteLabelAny: []string{"ok", "a,b"}},
		"empty in route_label":      {Name: "reviewer", RouteLabel: []string{""}},
		"blank in route_label_any":  {Name: "reviewer", RouteLabelAny: []string{"  "}},
		"newline in route_label":    {Name: "reviewer", RouteLabel: []string{"a\nb"}},
		"newline in route_label_an": {Name: "reviewer", RouteLabelAny: []string{"a\nb"}},
	} {
		if err := ValidateAgents([]Agent{a}); err == nil {
			t.Errorf("%s: expected an error, got nil", name)
		} else if !strings.Contains(err.Error(), "route_label") {
			t.Errorf("%s: error %q does not name the field", name, err)
		}
	}
}

// TestLoadRejectsRouteLabelWithRawOverrideAfterPatches: the exclusivity is a
// property of the EFFECTIVE agent, so a patch that adds route_label to an agent
// whose pack declares a raw work_query must fail the load too, not only the
// literal combination in one [[agent]] table.
func TestLoadRejectsRouteLabelWithRawOverrideAfterPatches(t *testing.T) {
	cases := map[string]string{
		"direct": `
[workspace]
name = "test"

[[agent]]
name = "reviewer"
work_query = "custom-query"
route_label = ["needs-claude-review"]
`,
		"patched": `
[workspace]
name = "test"

[[agent]]
name = "reviewer"
work_query = "custom-query"

[[patches.agent]]
name = "reviewer"
route_label_any = ["urgent"]
`,
	}
	for name, toml := range cases {
		t.Run(name, func(t *testing.T) {
			fs := fsys.NewFake()
			fs.Files["/city/city.toml"] = []byte(toml)
			_, _, err := LoadWithIncludes(fs, "/city/city.toml")
			if err == nil {
				t.Fatal("expected the load to reject route labels combined with a raw work_query")
			}
			if !strings.Contains(err.Error(), "route_label") || !strings.Contains(err.Error(), "reviewer") {
				t.Errorf("error %q should name the agent and the route_label field", err)
			}
		})
	}
}

// TestRouteLabelTOMLRoundTripAndPatchApplication: the keys decode from
// city.toml, a patch replaces them, and a pack override replaces them.
func TestRouteLabelTOMLRoundTripAndPatchApplication(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test"

[[agent]]
name = "reviewer"
dir = "cairn"
route_label = ["needs-claude-review", "ready"]
route_label_any = ["urgent", "vip"]

[[agent]]
name = "patched"
dir = "cairn"
route_label = ["old"]

[[patches.agent]]
dir = "cairn"
name = "patched"
route_label = ["new"]
route_label_any = ["a", "b"]
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	byName := map[string]Agent{}
	for _, a := range cfg.Agents {
		byName[a.Name] = a
	}
	if r := byName["reviewer"]; !slices.Equal(r.RouteLabel, []string{"needs-claude-review", "ready"}) || !slices.Equal(r.RouteLabelAny, []string{"urgent", "vip"}) {
		t.Errorf("reviewer route labels = %v / %v", r.RouteLabel, r.RouteLabelAny)
	}
	if p := byName["patched"]; !slices.Equal(p.RouteLabel, []string{"new"}) || !slices.Equal(p.RouteLabelAny, []string{"a", "b"}) {
		t.Errorf("patched route labels = %v / %v, want the patch to replace them", p.RouteLabel, p.RouteLabelAny)
	}
}

func TestApplyAgentOverrideReplacesRouteLabels(t *testing.T) {
	a := Agent{Name: "reviewer", RouteLabel: []string{"old"}, RouteLabelAny: []string{"old-any"}}
	applyAgentOverride(&a, &AgentOverride{Agent: "reviewer", RouteLabel: []string{"new"}, RouteLabelAny: []string{"x", "y"}})
	if !slices.Equal(a.RouteLabel, []string{"new"}) || !slices.Equal(a.RouteLabelAny, []string{"x", "y"}) {
		t.Errorf("override left route labels %v / %v", a.RouteLabel, a.RouteLabelAny)
	}
	// An override that does not name them leaves them alone.
	applyAgentOverride(&a, &AgentOverride{Agent: "reviewer"})
	if !slices.Equal(a.RouteLabel, []string{"new"}) {
		t.Errorf("an override without route_label cleared it: %v", a.RouteLabel)
	}
}

// TestPackAgentCarriesRouteLabelsThroughComposition: an agent a pack declares
// keeps its route labels once the pack is expanded into the city, and a city
// patch targeting the pack's agent replaces them.
func TestPackAgentCarriesRouteLabelsThroughComposition(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/review/pack.toml", `
[pack]
name = "review"
schema = 1

[[agent]]
name = "reviewer"
scope = "city"
route_label = ["needs-claude-review"]
route_label_any = ["urgent", "vip"]

[[agent]]
name = "plain"
scope = "city"
`)
	cfg := &City{Workspace: Workspace{Includes: []string{"packs/review"}}}
	if _, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir); err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}
	byName := map[string]Agent{}
	for _, a := range cfg.Agents {
		byName[a.Name] = a
	}
	reviewer, ok := byName["reviewer"]
	if !ok {
		t.Fatalf("pack agent reviewer missing from %v", cfg.Agents)
	}
	if !slices.Equal(reviewer.RouteLabel, []string{"needs-claude-review"}) || !slices.Equal(reviewer.RouteLabelAny, []string{"urgent", "vip"}) {
		t.Errorf("pack agent route labels = %v / %v, want them carried through composition", reviewer.RouteLabel, reviewer.RouteLabelAny)
	}
	if plain := byName["plain"]; len(plain.RouteLabel) != 0 || len(plain.RouteLabelAny) != 0 {
		t.Errorf("an unlabeled sibling inherited route labels: %v / %v", plain.RouteLabel, plain.RouteLabelAny)
	}
}
