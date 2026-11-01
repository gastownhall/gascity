package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

const standbyCity = `
[workspace]
name = "test"

[providers.cursor-grok]
command = "cursor-agent"

[providers.codex-astra]
command = "codex"

[[agent]]
name = "review-codex"
provider = "cursor-grok"
standby_provider = "codex-astra"
max_active_sessions = 3

[[agent]]
name = "review-claude"
provider = "cursor-grok"
`

func standbyFS(t *testing.T, city string) *fsys.Fake {
	t.Helper()
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(city)
	return fs
}

func standbyEntry(provider, replaced string) StandbyEntry {
	since := time.Date(2026, 10, 31, 7, 25, 0, 0, time.UTC)
	return StandbyEntry{Name: "review-codex", Provider: provider, Replaced: replaced, Since: since, Until: since.Add(30 * time.Minute), Engagements: 1, Reason: "3 seats in a row failed to claim"}
}

func agentNamed(t *testing.T, cfg *City, name string) Agent {
	t.Helper()
	for _, a := range cfg.Agents {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("no agent %q", name)
	return Agent{}
}

// With no standby record, an agent declaring a standby runs its own provider.
func TestStandbyDeclaredButNotInEffect(t *testing.T) {
	cfg, _, err := LoadWithIncludes(standbyFS(t, standbyCity), "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	a := agentNamed(t, cfg, "review-codex")
	if a.Provider != "cursor-grok" || a.StandbyFor != "" || a.StandbyProvider != "codex-astra" {
		t.Fatalf("agent %+v, want its own provider with the standby declared", a)
	}
}

// The standby record moves the agent it names onto its standby provider and
// cap, for every reader of the composed config, and the revision covers it.
func TestStandbyRecordMovesTheAgent(t *testing.T) {
	fs := standbyFS(t, standbyCity)
	_, beforeProv, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatal(err)
	}
	beforeCfg, _, _ := LoadWithIncludes(fs, "/city/city.toml")
	before := Revision(fs, beforeProv, beforeCfg, "/city")

	if err := WriteStandby(fs, "/city", StandbyFile{Agents: []StandbyEntry{standbyEntry("codex-astra", "cursor-grok")}}); err != nil {
		t.Fatal(err)
	}
	cfg, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	a := agentNamed(t, cfg, "review-codex")
	if a.Provider != "codex-astra" || a.StandbyFor != "cursor-grok" {
		t.Fatalf("agent %+v, want it on codex-astra in place of cursor-grok", a)
	}
	// A move changes the provider and nothing else: no cap, no session, so
	// nothing a config check could refuse.
	if a.MaxActiveSessions == nil || *a.MaxActiveSessions != 3 {
		t.Fatalf("max_active_sessions %v, want the agent's own 3", a.MaxActiveSessions)
	}
	if AgentOwnProvider(&a, &cfg.Workspace) != "cursor-grok" {
		t.Fatalf("own provider %q, want cursor-grok", AgentOwnProvider(&a, &cfg.Workspace))
	}
	if other := agentNamed(t, cfg, "review-claude"); other.Provider != "cursor-grok" || other.StandbyFor != "" {
		t.Fatalf("an agent the record does not name moved: %+v", other)
	}
	if after := Revision(fs, prov, cfg, "/city"); after == before {
		t.Fatal("the revision did not change with the standby record")
	}
	for _, src := range prov.Sources {
		if strings.Contains(src, "/.gc/") {
			t.Fatalf("source %q: the runtime directory must never be a watched source", src)
		}
	}
	for _, w := range WatchTargets(prov, cfg, "/city") {
		if strings.Contains(w.Path, "/.gc") {
			t.Fatalf("watch target %q: the runtime directory churns and must not be watched", w.Path)
		}
	}
	// Removing the record puts the agent back and restores the revision.
	if err := WriteStandby(fs, "/city", StandbyFile{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := fs.Files[StandbyPath("/city")]; ok {
		t.Fatal("an empty record was written, want the file removed")
	}
	cfg, prov, err = LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatal(err)
	}
	if a := agentNamed(t, cfg, "review-codex"); a.Provider != "cursor-grok" || a.StandbyFor != "" {
		t.Fatalf("agent %+v after the record was removed, want its own provider", a)
	}
	if Revision(fs, prov, cfg, "/city") != before {
		t.Fatal("removing the record did not restore the revision")
	}
}

// An entry applies only while it still matches the agent's config: a person
// who moved the agent's own provider, or changed its standby, wins, and the
// stale entry is ignored with a warning rather than failing the load.
func TestStandbyRecordIgnoresStaleEntries(t *testing.T) {
	cases := []struct {
		name  string
		entry StandbyEntry
	}{
		{"own provider changed", standbyEntry("codex-astra", "cursor-old")},
		{"standby changed", standbyEntry("codex-other", "cursor-grok")},
		{"unknown agent", func() StandbyEntry { e := standbyEntry("codex-astra", "cursor-grok"); e.Name = "gone"; return e }()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := standbyFS(t, standbyCity)
			if err := WriteStandby(fs, "/city", StandbyFile{Agents: []StandbyEntry{tc.entry}}); err != nil {
				t.Fatal(err)
			}
			cfg, prov, err := LoadWithIncludes(fs, "/city/city.toml")
			if err != nil {
				t.Fatalf("LoadWithIncludes: %v", err)
			}
			if a := agentNamed(t, cfg, "review-codex"); a.Provider != "cursor-grok" || a.StandbyFor != "" {
				t.Fatalf("agent %+v, want the stale entry ignored", a)
			}
			if !strings.Contains(strings.Join(prov.Warnings, "\n"), "ignored") {
				t.Fatalf("warnings %v, want the ignored entry reported", prov.Warnings)
			}
		})
	}
}

// A record that cannot be parsed never keeps the city from loading.
func TestStandbyRecordUnreadableIsAWarning(t *testing.T) {
	fs := standbyFS(t, standbyCity)
	fs.Files[StandbyPath("/city")] = []byte("[[agent]\nname = ")
	cfg, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if a := agentNamed(t, cfg, "review-codex"); a.Provider != "cursor-grok" {
		t.Fatalf("agent %+v, want its own provider", a)
	}
	if !strings.Contains(strings.Join(prov.Warnings, "\n"), "standby record") {
		t.Fatalf("warnings %v, want the unreadable record reported", prov.Warnings)
	}
}

// A standby the agent cannot use is inert and said so, never a load error: a
// person setting the agent's provider to its standby during an outage must
// not have the reload refused.
func TestStandbyUsable(t *testing.T) {
	ws := &Workspace{Provider: "w"}
	providers := map[string]ProviderSpec{
		"a":       {Command: "a-agent"},
		"b":       {Command: "b-agent"},
		"w":       {Command: "w-agent"},
		"acp":     {Command: "acp-agent", SupportsACP: boolPtr(true)},
		"acp-too": {Command: "acp-too-agent", SupportsACP: boolPtr(true)},
	}
	cases := []struct {
		name string
		a    Agent
		want string
	}{
		{"usable", Agent{Provider: "a", StandbyProvider: "b"}, ""},
		{"none", Agent{Provider: "a"}, "no standby_provider"},
		{"own provider", Agent{Provider: "b", StandbyProvider: "b"}, "own provider"},
		{"the workspace's provider is its own", Agent{StandbyProvider: "w"}, "own provider"},
		{"usable off the workspace's provider", Agent{StandbyProvider: "b"}, ""},
		{"start_command", Agent{StartCommand: "run", StandbyProvider: "b"}, "start_command"},
		{"own process_names", Agent{Provider: "a", StandbyProvider: "b", ProcessNames: []string{"a-agent"}}, "process_names"},
		// The v2 planner refuses every seat of an ACP agent whose provider
		// cannot speak ACP: a move there would leave the agent seatless.
		{"an ACP agent's standby without ACP", Agent{Provider: "acp", Session: "acp", StandbyProvider: "b"}, "ACP"},
		{"an ACP agent's standby with ACP", Agent{Provider: "acp", Session: "acp", StandbyProvider: "acp-too"}, ""},
	}
	for _, tc := range cases {
		got := StandbyUsable(&tc.a, ws, providers)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: StandbyUsable = %q, want %q", tc.name, got, tc.want)
		}
	}

	fs := standbyFS(t, strings.Replace(standbyCity, `standby_provider = "codex-astra"`, `standby_provider = "cursor-grok"`, 1))
	_, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("a standby equal to the own provider failed the load: %v", err)
	}
	found := false
	for _, w := range prov.Warnings {
		if strings.Contains(w, "never moved to standby_provider") {
			found = true
			if !IsStandbyWarning(w) {
				t.Fatalf("warning %q is not classified as a standby warning: strict mode would refuse the config", w)
			}
		}
	}
	if !found {
		t.Fatalf("warnings %v, want the inert standby reported", prov.Warnings)
	}
}

// A session = "acp" agent whose standby cannot speak ACP is never moved: the
// load says so, and a recorded move is ignored rather than composed into an
// agent the reconciler would start no seat for.
func TestStandbyACPAgentNeedsAnACPStandby(t *testing.T) {
	city := strings.Replace(standbyCity, "[[agent]]\nname = \"review-codex\"\nprovider = \"cursor-grok\"\n",
		"[providers.cursor-acp]\ncommand = \"cursor-acp\"\nsupports_acp = true\n\n[[agent]]\nname = \"review-codex\"\nprovider = \"cursor-acp\"\nsession = \"acp\"\n", 1)
	fs := standbyFS(t, city)
	if err := WriteStandby(fs, "/city", StandbyFile{Agents: []StandbyEntry{standbyEntry("codex-astra", "cursor-acp")}}); err != nil {
		t.Fatal(err)
	}
	cfg, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if a := agentNamed(t, cfg, "review-codex"); a.Provider != "cursor-acp" || a.StandbyFor != "" {
		t.Fatalf("agent %+v, want the move to a standby without ACP ignored", a)
	}
	var inert, ignored bool
	for _, w := range prov.Warnings {
		if !strings.Contains(w, "ACP") {
			continue
		}
		if !IsStandbyWarning(w) {
			t.Fatalf("warning %q is not classified as a standby warning: strict mode would refuse the config", w)
		}
		inert = inert || strings.Contains(w, "never moved to standby_provider")
		ignored = ignored || strings.Contains(w, "recorded move is ignored")
	}
	if !inert || !ignored {
		t.Fatalf("warnings %v, want the inert standby and the ignored move reported", prov.Warnings)
	}
}

// An agent that takes its provider from [workspace] provider has that
// provider as its own: the record moves it, and an entry naming the
// workspace provider as replaced applies.
func TestStandbyWorkspaceProviderAgent(t *testing.T) {
	city := strings.Replace(strings.Replace(standbyCity, "name = \"test\"\n", "name = \"test\"\nprovider = \"cursor-grok\"\n", 1),
		"name = \"review-codex\"\nprovider = \"cursor-grok\"\n", "name = \"review-codex\"\n", 1)
	fs := standbyFS(t, city)
	if err := WriteStandby(fs, "/city", StandbyFile{Agents: []StandbyEntry{standbyEntry("codex-astra", "cursor-grok")}}); err != nil {
		t.Fatal(err)
	}
	cfg, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if a := agentNamed(t, cfg, "review-codex"); a.Provider != "codex-astra" || a.StandbyFor != "cursor-grok" {
		t.Fatalf("agent %+v (warnings %v), want it moved off the workspace provider", a, prov.Warnings)
	}
}

// A standby provider missing from the catalog is a load error, as a missing
// provider is.
func TestStandbyProviderMustExist(t *testing.T) {
	fs := standbyFS(t, strings.Replace(standbyCity, `standby_provider = "codex-astra"`, `standby_provider = "codex-astro"`, 1))
	_, _, err := LoadWithIncludes(fs, "/city/city.toml")
	var catalog *ProviderCatalogError
	if !errors.As(err, &catalog) || !strings.Contains(err.Error(), `standby_provider "codex-astro"`) {
		t.Fatalf("err = %v, want a provider catalog error naming the standby", err)
	}
}

// Patches and rig overrides set the standby like any other agent field.
func TestStandbyPatchable(t *testing.T) {
	city := standbyCity + `
[[patches.agent]]
name = "review-claude"
standby_provider = "codex-astra"
`
	cfg, _, err := LoadWithIncludes(standbyFS(t, city), "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	a := agentNamed(t, cfg, "review-claude")
	if a.StandbyProvider != "codex-astra" {
		t.Fatalf("patched agent %+v, want the standby from the patch", a)
	}
}

// A lane agent declared in a pack's agents/<name>/agent.toml, as
// maintainer-city's review lanes are, carries its standby, and the standby
// record moves it like any other agent.
func TestStandbyFromPackAgentToml(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "reviewers")
	agentDir := filepath.Join(packDir, "agents", "review-codex")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, packDir, "pack.toml", `
[pack]
name = "reviewers"
schema = 1
`)
	writeTestFile(t, agentDir, "agent.toml", `
scope = "city"
provider = "codex-astra"
standby_provider = "cursor-grok"
max_active_sessions = 2
`)
	writeTestFile(t, agentDir, "prompt.md", `You review code.`)
	cityDir := filepath.Join(dir, "city")
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
includes = ["../reviewers"]

[providers.codex-astra]
command = "codex"

[providers.cursor-grok]
command = "cursor-agent"
`)
	load := func() Agent {
		t.Helper()
		cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
		if err != nil {
			t.Fatalf("LoadWithIncludes: %v", err)
		}
		return agentNamed(t, cfg, "review-codex")
	}
	if a := load(); a.Provider != "codex-astra" || a.StandbyProvider != "cursor-grok" {
		t.Fatalf("agent %+v, want codex-astra with the cursor-grok standby declared", a)
	}
	entry := StandbyEntry{Name: "review-codex", Provider: "cursor-grok", Replaced: "codex-astra", Engagements: 1}
	if err := WriteStandby(fsys.OSFS{}, cityDir, StandbyFile{Agents: []StandbyEntry{entry}}); err != nil {
		t.Fatal(err)
	}
	if a := load(); a.Provider != "cursor-grok" || a.StandbyFor != "codex-astra" || a.MaxActiveSessions == nil || *a.MaxActiveSessions != 2 {
		t.Fatalf("agent %+v, want it moved to cursor-grok with its own cap 2", a)
	}
}

// Only the standby layer's own warnings are non-fatal in strict mode: an
// unknown-key or typo warning that merely names the key stays fatal.
func TestIsStandbyWarning(t *testing.T) {
	if IsStandbyWarning(`city.toml: unknown field "standby_providr"; did you mean "standby_provider"?`) {
		t.Fatal("a typo warning naming the key was classified as a standby warning")
	}
	fs := standbyFS(t, standbyCity)
	if err := WriteStandby(fs, "/city", StandbyFile{Agents: []StandbyEntry{standbyEntry("codex-astra", "cursor-old")}}); err != nil {
		t.Fatal(err)
	}
	_, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range prov.Warnings {
		if strings.Contains(w, "recorded move") {
			found = true
			if !IsStandbyWarning(w) {
				t.Fatalf("stale-entry warning %q is not classified as a standby warning", w)
			}
		}
	}
	if !found {
		t.Fatalf("warnings %v, want the stale entry reported", prov.Warnings)
	}
}
