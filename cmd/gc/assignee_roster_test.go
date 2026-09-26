package main

import (
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func testRosterCity() *config.City {
	return &config.City{
		Agents: []config.Agent{
			{Name: "polecat"},
			{Name: "city-infra-worker"},
		},
		NamedSessions: []config.NamedSession{
			{Name: "goal-4-context"},
			{Name: "mayor"},
		},
	}
}

func TestAssigneeRosterRejectsUnconfiguredTargets(t *testing.T) {
	roster := newAssigneeRoster(testRosterCity())

	// The names the 2026-09-15 audit found holding open work hostage.
	for _, assignee := range []string{
		"goal-5-temporal",
		"/home/ds/gas-city/goal-5-temporal",
		"controller",
		"pr-pipeline-review-17",
	} {
		if roster.Resolves(assignee) {
			t.Errorf("assignee %q resolved, want unresolvable", assignee)
		}
	}
}

func TestAssigneeRosterAcceptsRoutableTargets(t *testing.T) {
	roster := newAssigneeRoster(testRosterCity())

	for _, assignee := range []string{
		"",                          // unowned is a valid state
		"goal-4-context",            // named session
		"polecat",                   // pool agent
		"polecat-4",                 // materialized pool instance
		"city-infra-worker",         // agent
		"human",                     // a person
		"mayor",                     // reserved mailbox
		"dr-toegp",                  // session bead id
		"gc-4cyi0a",                 // session bead id
		"claude-1-adhoc-6f649101fd", // runtime adhoc session
		"claude-auto-2",             // runtime auto session
	} {
		if !roster.Resolves(assignee) {
			t.Errorf("assignee %q did not resolve, want routable", assignee)
		}
	}
}

func TestAssigneeRosterEmptyIsReportedNotEnforced(t *testing.T) {
	// A config that fails to expand its packs loads with zero agents, which is
	// indistinguishable from a city with none. Measured on the real city
	// 2026-09-15: `gc doctor` reported `city.toml loaded (0 agents, 11 rigs)`
	// while packv2-import-state was failing. Enforcing against that roster
	// would have declared every assignee in the fleet unroutable.
	roster := newAssigneeRoster(nil)
	if !roster.Empty() {
		t.Fatal("roster built from nil config does not report Empty")
	}
	if newAssigneeRoster(testRosterCity()).Empty() {
		t.Fatal("a populated roster must not report Empty")
	}

	// The write gate must pass the write through, not block it.
	if err := checkBdAssigneeArgs(nil, []string{"update", "dr-1", "--assignee", "anything"}, io.Discard); err != nil {
		t.Fatalf("gate enforced against an empty roster: %v", err)
	}
}

func TestExtractAssigneeArgsCoversEverySpelling(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"separate flag", []string{"update", "dr-1", "--assignee", "ghost"}, []string{"ghost"}},
		{"joined flag", []string{"update", "dr-1", "--assignee=ghost"}, []string{"ghost"}},
		{"short flag", []string{"create", "t", "-a", "ghost"}, []string{"ghost"}},
		{"positional assign", []string{"assign", "dr-1", "ghost"}, []string{"ghost"}},
		{"no assignee", []string{"list", "--status", "open"}, nil},
		{"status value is not an assignee", []string{"list", "--status", "open", "--json"}, nil},
		// list/ready --assignee is a read-only FILTER (bd list --help: "-a,
		// --assignee string  Filter by assignee"), not a write. B1: this used
		// to be scanned unconditionally, so `bd list --assignee <phantom>`
		// refused the exact investigation this gate exists to enable.
		{"list assignee filter is not a write", []string{"list", "--assignee", "phantom-owner"}, nil},
		{"ready assignee filter is not a write", []string{"ready", "--assignee", "phantom-owner"}, nil},
		{"create still scans --assignee", []string{"create", "t", "--assignee", "ghost"}, []string{"ghost"}},
		{"mol pour still scans --assignee", []string{"mol", "pour", "f", "--assignee", "ghost"}, []string{"ghost"}},
		// M1: pflag's short-flag attached forms. Both write the assignee
		// exactly like "-a ghost" and must not bypass the gate.
		{"short flag joined with equals", []string{"update", "dr-1", "-a=ghost"}, []string{"ghost"}},
		{"short flag attached", []string{"update", "dr-1", "-aghost"}, []string{"ghost"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractAssigneeArgs(tc.args)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("extractAssigneeArgs(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestCheckBdAssigneeArgsGate(t *testing.T) {
	cfg := testRosterCity()

	err := checkBdAssigneeArgs(cfg, []string{"update", "dr-1", "--assignee", "goal-5-temporal"}, nil)
	if err == nil {
		t.Fatal("gate allowed an unresolvable assignee")
	}
	if !strings.Contains(err.Error(), assigneeGateEscapeEnv) {
		t.Errorf("rejection does not name the escape hatch: %v", err)
	}

	if err := checkBdAssigneeArgs(cfg, []string{"update", "dr-1", "--assignee", "polecat-2"}, nil); err != nil {
		t.Fatalf("gate rejected a routable assignee: %v", err)
	}
	if err := checkBdAssigneeArgs(cfg, []string{"list", "--status", "open"}, nil); err != nil {
		t.Fatalf("gate rejected a command that writes no assignee: %v", err)
	}

	// B1: `bd list --assignee <phantom>` / `bd ready --assignee <phantom>`
	// are read-only queries, not writes. Before the fix, extractAssigneeArgs
	// scanned them the same as a write and this refused the exact
	// investigation ("who does this unroutable bead belong to") the gate
	// exists to enable.
	if err := checkBdAssigneeArgs(cfg, []string{"list", "--assignee", "goal-5-temporal"}, nil); err != nil {
		t.Fatalf("gate refused a read-only bd list --assignee filter: %v", err)
	}
	if err := checkBdAssigneeArgs(cfg, []string{"ready", "--assignee", "goal-5-temporal"}, nil); err != nil {
		t.Fatalf("gate refused a read-only bd ready --assignee filter: %v", err)
	}
	// Control: the write path for the same unresolvable name is still
	// refused.
	if err := checkBdAssigneeArgs(cfg, []string{"update", "dr-1", "--assignee", "goal-5-temporal"}, nil); err == nil {
		t.Fatal("gate allowed an unresolvable assignee write via update")
	}
}

// rosterCityWithPrefixes declares the bead ID prefixes the city issues, so the
// roster can tell a session identity from a misspelled agent name.
func rosterCityWithPrefixes() *config.City {
	c := testRosterCity()
	c.Workspace.Prefix = "gc"
	c.Rigs = []config.Rig{{Name: "research", Path: "research", Prefix: "dr"}}
	return c
}

// A session identity is named after a bead the city issued. Checking the shape
// alone (2-4 letters, hyphen, alphanumerics) also matches an ordinary typo of
// an agent name, which would wave through the exact error class this exists to
// catch.
func TestAssigneeRosterDistinguishesSessionIDsFromTypos(t *testing.T) {
	roster := newAssigneeRoster(rosterCityWithPrefixes())

	for _, assignee := range []string{"dr-huhn", "gc-818bx", "dr-a95w9", "repo-adhoc-1a2b3c", "worker-auto-7"} {
		if !roster.Resolves(assignee) {
			t.Errorf("runtime identity %q was rejected; a static roster cannot confirm these", assignee)
		}
	}
	// Same shape, prefix the city never issues.
	for _, assignee := range []string{"poly-cat1", "xy-1234", "abc-9999"} {
		if roster.Resolves(assignee) {
			t.Errorf("assignee %q resolved as a session identity, but no city prefix issues it", assignee)
		}
	}
}

// With no prefixes to check against there is nothing to be right about, so any
// bead-shaped name is accepted. Same cannot-answer posture as Empty().
func TestAssigneeRosterAcceptsAnyIDShapeWhenNoPrefixesDeclared(t *testing.T) {
	c := &config.City{Agents: []config.Agent{{Name: "polecat"}}}
	if !newAssigneeRoster(c).Resolves("poly-cat1") {
		t.Error("bead-shaped name rejected although the city declares no prefixes")
	}
}

// A pool instance is not always <agent>-<slot>: an agent declaring a namepool
// materializes instances under the declared names, which share nothing with
// the stem.
func TestAssigneeRosterResolvesNamepoolInstances(t *testing.T) {
	c := testRosterCity()
	c.Agents = append(c.Agents, config.Agent{Name: "herder", NamepoolNames: []string{"rivet", "gasket"}})
	roster := newAssigneeRoster(c)

	for _, assignee := range []string{"rivet", "gasket", "herder-2"} {
		if !roster.Resolves(assignee) {
			t.Errorf("pool instance %q was rejected; it is routable", assignee)
		}
	}
}

func TestPositionalAssignArgSurvivesLeadingGlobalFlagValues(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"assign", "gc-1", "polecat"}, "polecat"},
		{[]string{"--db", "/tmp/x.db", "assign", "gc-1", "polecat"}, "polecat"},
		{[]string{"--db", "/tmp/x.db", "assign", "--force", "gc-1", "polecat"}, "polecat"},
	} {
		got, ok := positionalAssignArg(tc.args)
		if !ok || got != tc.want {
			t.Errorf("positionalAssignArg(%v) = (%q, %v), want (%q, true)", tc.args, got, ok, tc.want)
		}
	}
	// Not the assign subcommand, so there is no positional assignee to take.
	for _, args := range [][]string{
		{"list", "-s", "open"},
		{"update", "gc-1", "--assignee", "polecat"},
		{"assign", "gc-1"},
	} {
		if got, ok := positionalAssignArg(args); ok {
			t.Errorf("positionalAssignArg(%v) = (%q, true), want no positional assignee", args, got)
		}
	}
}

// M2: bd's global BOOLEAN flags (--json, -q, --global, ...) take no value and
// can sit directly before the "assign" subcommand. The old check ("does the
// preceding token start with a dash") treated all of these as a flag value
// donating the word "assign", and skipped the positional check entirely.
func TestPositionalAssignArgSurvivesLeadingGlobalBoolFlags(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--json", "assign", "gc-1", "polecat"}, "polecat"},
		{[]string{"-q", "assign", "gc-1", "polecat"}, "polecat"},
		{[]string{"--global", "assign", "gc-1", "polecat"}, "polecat"},
	} {
		got, ok := positionalAssignArg(tc.args)
		if !ok || got != tc.want {
			t.Errorf("positionalAssignArg(%v) = (%q, %v), want (%q, true)", tc.args, got, ok, tc.want)
		}
	}
}

// M3: --format is a bd global valued flag missing from the old hand-copied
// table, so "bd assign --format json <id> <who>" took the bead ID as the
// assignee and never inspected <who> -- a false refusal naming a bead ID.
func TestPositionalAssignArgSkipsFormatFlagAfterSubcommand(t *testing.T) {
	got, ok := positionalAssignArg([]string{"assign", "--format", "json", "gc-1", "polecat"})
	if !ok || got != "polecat" {
		t.Errorf("positionalAssignArg with --format = (%q, %v), want (%q, true)", got, ok, "polecat")
	}
}

// The word "assign" can appear as a flag value on an unrelated command. Taking
// it for the subcommand there would refuse a read command over an argument
// that is not an assignee at all.
func TestPositionalAssignArgIgnoresAssignAsFlagValue(t *testing.T) {
	for _, args := range [][]string{
		{"list", "--label", "assign", "foo", "bar"},
		{"search", "-l", "assign", "alpha", "beta"},
	} {
		if got, ok := positionalAssignArg(args); ok {
			t.Errorf("positionalAssignArg(%v) = (%q, true); the word was a flag value, not the subcommand", args, got)
		}
	}
	// Still found when it really is the subcommand behind a valued global flag.
	if got, ok := positionalAssignArg([]string{"--db", "/tmp/x.db", "assign", "gc-1", "polecat"}); !ok || got != "polecat" {
		t.Errorf("positionalAssignArg lost the real subcommand: (%q, %v)", got, ok)
	}
}

// bd documents `bd assign <id> ""` as the unassign form. An empty assignee is
// a valid state and must not be refused.
func TestCheckBdAssigneeArgsAllowsPositionalUnassign(t *testing.T) {
	if err := checkBdAssigneeArgs(testRosterCity(), []string{"assign", "gc-1", ""}, io.Discard); err != nil {
		t.Errorf("unassign was refused: %v", err)
	}
}

// The two stores spell the same owner differently: the file store records a
// path-qualified name where the bd store records it bare. A runtime identity
// written the long way is still a runtime identity, and a phantom written the
// long way is still a phantom.
func TestAssigneeRosterResolvesQualifiedSpellingsOfRuntimeIdentities(t *testing.T) {
	roster := newAssigneeRoster(rosterCityWithPrefixes())

	for _, assignee := range []string{
		"research/dr-huhn",
		"/home/ds/gas-city/research/dr-huhn",
		"research/repo-adhoc-1a2b3c",
	} {
		if !roster.Resolves(assignee) {
			t.Errorf("qualified runtime identity %q was rejected", assignee)
		}
	}
	// The qualification must not launder a name the city cannot route to.
	for _, assignee := range []string{
		"/home/ds/gas-city/goal-5-temporal",
		"research/poly-cat1",
	} {
		if roster.Resolves(assignee) {
			t.Errorf("qualified phantom %q resolved", assignee)
		}
	}
}

// M4: work is also assigned under the runtime session-name spelling, which
// encodes "/" as "--" and "." as "__" (internal/agent.SessionNameFor). A
// config-declared agent "polecat" in rig "repo" runs under the session name
// "repo--polecat-4"; the roster must decode that back to "repo/polecat-4"
// rather than refusing it as an unroutable assignee.
func TestAssigneeRosterResolvesSessionNameSpelling(t *testing.T) {
	c := testRosterCity()
	c.Agents = append(c.Agents, config.Agent{Name: "polecat-rig", Dir: "repo"})
	roster := newAssigneeRoster(c)

	// SessionNameFor("repo/polecat-rig") -> "repo--polecat-rig".
	if !roster.Resolves("repo--polecat-rig") {
		t.Error("session-name-spelled assignee \"repo--polecat-rig\" was rejected")
	}
	// A phantom name must not be laundered by the same decode.
	if roster.Resolves("repo--ghost") {
		t.Error("phantom \"repo--ghost\" resolved via the session-name decode")
	}
}

// bd global flags may appear after the subcommand. A valued one placed there
// donates its value to the operand list unless it is skipped, and the gate
// then checks the actor name instead of the assignee.
func TestPositionalAssignArgSkipsValuedFlagsAfterSubcommand(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"assign", "gc-1", "--actor", "bob", "polecat"}, "polecat"},
		{[]string{"assign", "--actor", "bob", "gc-1", "polecat"}, "polecat"},
		{[]string{"assign", "gc-1", "-C", "/tmp/rig", "polecat"}, "polecat"},
		{[]string{"assign", "gc-1", "--actor=bob", "polecat"}, "polecat"},
		{[]string{"assign", "gc-1", "--force", "polecat"}, "polecat"},
	} {
		got, ok := positionalAssignArg(tc.args)
		if !ok || got != tc.want {
			t.Errorf("positionalAssignArg(%v) = (%q, %v), want (%q, true)", tc.args, got, ok, tc.want)
		}
	}
}

// A dot-qualified name whose tail happens to be bead-ID shaped is a typo, not a
// session. The identity heuristics match on shape rather than on a declared
// name, so running them over the dot spelling would wave through an owner that
// never existed.
func TestAssigneeRosterRejectsDotQualifiedBeadIDShapes(t *testing.T) {
	r := newAssigneeRoster(rosterCityWithPrefixes())
	for _, name := range []string{
		"sjarmak.gc-818bx",
		"totally-bogus-typo.gc-818bx",
	} {
		if r.Resolves(name) {
			t.Errorf("Resolves(%q) = true, want false: a dot-qualified tail is not a session identity", name)
		}
	}
	// The path spellings the expansion exists for still resolve.
	if !r.Resolves("research/gc-818bx") {
		t.Error(`Resolves("research/gc-818bx") = false, want true`)
	}
}
