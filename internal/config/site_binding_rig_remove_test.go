package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// TestCityRemoveRigSweepsBlocksThatReferToIt pins the in-memory removal
// cmdRigRemove applies (#3666): the rig goes, and so does every block that
// names it and would dangle. City-scoped and other rigs' blocks stay.
func TestCityRemoveRigSweepsBlocksThatReferToIt(t *testing.T) {
	cfg := City{
		Rigs: []Rig{{Name: "alpha"}, {Name: "beta"}},
		Patches: Patches{
			Agents: []AgentPatch{
				{Dir: "alpha", Name: "claude"},
				{Rig: "alpha", Name: "codex"},
				{Name: "mayor"},
				{Rig: "*", Name: "worker"},
			},
			NamedSessions:    []NamedSessionPatch{{Dir: "alpha", Name: "refinery"}, {Name: "deacon"}},
			Rigs:             []RigPatch{{Name: "alpha"}, {Name: "beta"}},
			GitHubPRMonitors: []GitHubPRMonitorPatch{{Name: "alpha-prs"}, {Name: "beta-prs"}},
		},
		GitHub: GitHubConfig{PRMonitors: []GitHubPRMonitor{
			{Name: "alpha-prs", Rig: "alpha"},
			{Name: "beta-prs", Rig: "beta"},
		}},
		Orders: OrdersConfig{Overrides: []OrderOverride{{Name: "nightly", Rig: "alpha"}, {Name: "nightly"}}},
	}
	want := City{
		Rigs: []Rig{{Name: "beta"}},
		Patches: Patches{
			Agents:           []AgentPatch{{Name: "mayor"}, {Rig: "*", Name: "worker"}},
			NamedSessions:    []NamedSessionPatch{{Name: "deacon"}},
			Rigs:             []RigPatch{{Name: "beta"}},
			GitHubPRMonitors: []GitHubPRMonitorPatch{{Name: "beta-prs"}},
		},
		GitHub: GitHubConfig{PRMonitors: []GitHubPRMonitor{{Name: "beta-prs", Rig: "beta"}}},
		Orders: OrdersConfig{Overrides: []OrderOverride{{Name: "nightly"}}},
	}

	if !cfg.RemoveRig("alpha") {
		t.Fatal(`RemoveRig("alpha") = false, want true`)
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("after RemoveRig(\"alpha\"):\n got %#v\nwant %#v", cfg, want)
	}
	if cfg.RemoveRig("gamma") {
		t.Fatal(`RemoveRig("gamma") = true for an undeclared rig, want false`)
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("RemoveRig of an undeclared rig changed the city:\n got %#v\nwant %#v", cfg, want)
	}
}

// TestRemoveRigFromCityTOMLDeletesEntriesInPlace pins #7899's in-place
// removal on hand-written city.toml shapes that gc's encoder never writes.
// Each input is used byte for byte, never re-marshaled. The result must be
// exactly the input without the removed rig's entries.
func TestRemoveRigFromCityTOMLDeletesEntriesInPlace(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "comment above the entry goes with it and comment above the next header stays",
			in: `# city rationale
[workspace]
name = "city"

# alpha rationale
[[rigs]]
name = "alpha"

# beta rationale
[[rigs]]
name = "beta"
`,
			want: `# city rationale
[workspace]
name = "city"

# beta rationale
[[rigs]]
name = "beta"
`,
		},
		{
			name: "comment after the removed entry's header",
			in: `[[rigs]] # alpha: retired
name = "alpha"

[[rigs]]
name = "beta"
`,
			want: `[[rigs]]
name = "beta"
`,
		},
		{
			name: "comment after the next entry's header",
			in: `[[rigs]]
name = "alpha"

[[rigs]] # beta: the docs repo
name = "beta"
`,
			want: `[[rigs]] # beta: the docs repo
name = "beta"
`,
		},
		{
			name: "comment after an unrelated table's header",
			in: `[[rigs]]
name = "alpha"

[beads] # managed store
provider = "bd"
`,
			want: `[beads] # managed store
provider = "bd"
`,
		},
		{
			name: "CRLF line endings",
			in:   "[workspace]\r\nname = \"city\"\r\n\r\n[[rigs]]\r\nname = \"alpha\"\r\n\r\n[[rigs]]\r\nname = \"beta\"\r\n",
			want: "[workspace]\r\nname = \"city\"\r\n\r\n[[rigs]]\r\nname = \"beta\"\r\n",
		},
		{
			name: "literal-string and escaped names",
			in: `[[rigs]]
name = 'alpha'

[[patches.agent]]
dir = "\u0061lpha"
name = "claude"

[[rigs]]
name = "beta"
`,
			want: `[[rigs]]
name = "beta"
`,
		},
		{
			name: "whitespace and quoted parts in a header",
			in: `[[ rigs ]]
name = "alpha"

[[ "patches" . 'agent' ]]
dir = "alpha"
name = "claude"

[[rigs]]
name = "beta"
`,
			want: `[[rigs]]
name = "beta"
`,
		},
		{
			name: "sub-tables of the removed entry go with it and the kept entry's stay",
			in: `[[rigs]]
name = "alpha"

[rigs.formula_vars]
target = "alpha-target"

[[rigs.overrides]]
agent = "worker"

[[rigs]]
name = "beta"

[rigs.formula_vars]
target = "beta-target"
`,
			want: `[[rigs]]
name = "beta"

[rigs.formula_vars]
target = "beta-target"
`,
		},
		{
			name: "a sub-table key named like an identity key does not claim its entry",
			in: `[[patches.agent]]
name = "claude"

[patches.agent.env]
dir = "alpha"

[[rigs]]
name = "alpha"
`,
			want: `[[patches.agent]]
name = "claude"

[patches.agent.env]
dir = "alpha"
`,
		},
		{
			name: "adjacent entries with no blank line between them",
			in: `[workspace]
name = "city"
[[rigs]]
name = "alpha"
[[patches.agent]]
dir = "alpha"
name = "claude"
[[rigs]]
name = "beta"
`,
			want: `[workspace]
name = "city"
[[rigs]]
name = "beta"
`,
		},
		{
			name: "last entry of a file keeps the file's final newline",
			in: `[workspace]
name = "city"
[[rigs]]
name = "alpha"
`,
			want: `[workspace]
name = "city"
`,
		},
		{
			name: "every block that refers to the rig",
			in: `[workspace]
name = "city"

[[rigs]]
name = "alpha"

[[rigs]]
name = "beta"

[[patches.agent]]
dir = "alpha"
name = "claude"

[[patches.agent]]
rig = "alpha"
name = "codex"

[[patches.agent]]
name = "mayor"
suspended = true

[[patches.named_session]]
dir = "alpha"
name = "refinery"

[[patches.rigs]]
name = "alpha"
prefix = "al"

[[patches.rigs]]
name = "beta"
prefix = "be"

[[github.pr_monitor]]
name = "alpha-prs"
owner = "acme"
repo = "alpha"
base_branches = ["main"]
rig = "alpha"

[[github.pr_monitor]]
name = "beta-prs"
owner = "acme"
repo = "beta"
base_branches = ["main"]
rig = "beta"

[[patches.github_pr_monitor]]
name = "alpha-prs"
repo = "alpha2"

[[patches.github_pr_monitor]]
name = "beta-prs"
repo = "beta2"

[[orders.overrides]]
name = "nightly"
rig = "alpha"
enabled = false

[[orders.overrides]]
name = "nightly"
enabled = true
`,
			want: `[workspace]
name = "city"

[[rigs]]
name = "beta"

[[patches.agent]]
name = "mayor"
suspended = true

[[patches.rigs]]
name = "beta"
prefix = "be"

[[github.pr_monitor]]
name = "beta-prs"
owner = "acme"
repo = "beta"
base_branches = ["main"]
rig = "beta"

[[patches.github_pr_monitor]]
name = "beta-prs"
repo = "beta2"

[[orders.overrides]]
name = "nightly"
enabled = true
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := removeRigFromCityTOML([]byte(tc.in), "alpha")
			if err != nil {
				t.Fatalf("removeRigFromCityTOML: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("removeRigFromCityTOML result:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

// TestRemoveRigFromCityTOMLRefusesWhatItCannotDeleteInPlace pins the check
// behind the in-place removal: when the line scanner misreads where an entry
// ends, the edit wraps ErrSurgicalRigRemoveUnsupported instead of returning
// a file that has lost, kept or moved settings.
func TestRemoveRigFromCityTOMLRefusesWhatItCannotDeleteInPlace(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{
			name: "rigs declared as an inline array",
			in: `rigs = [{ name = "alpha" }, { name = "beta" }]
`,
		},
		{
			name: "a [[rigs]] line inside a multi-line string",
			in: `[[rigs]]
name = "beta"

[future_settings]
text = """
[[rigs]]
"""

[[rigs]]
name = "alpha"
`,
		},
		{
			name: "a sub-table declared away from its entry",
			in: `[[rigs]]
name = "beta"

[[rigs]]
name = "alpha"

[workspace]
name = "city"

[rigs.formula_vars]
target = "alpha-target"
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := removeRigFromCityTOML([]byte(tc.in), "alpha")
			if !errors.Is(err, ErrSurgicalRigRemoveUnsupported) {
				t.Fatalf("removeRigFromCityTOML error = %v, result:\n%s\nwant an error wrapping ErrSurgicalRigRemoveUnsupported", err, got)
			}
		})
	}
}

// A rig the file does not declare is an error of its own, not a cue to fall
// back to a full rewrite: the caller loaded it from this file, so the file
// changed underneath it.
func TestRemoveRigFromCityTOMLRequiresTheRig(t *testing.T) {
	_, err := removeRigFromCityTOML([]byte("[[rigs]]\nname = \"beta\"\n"), "alpha")
	if err == nil {
		t.Fatal("removeRigFromCityTOML succeeded for an undeclared rig, want an error")
	}
	if errors.Is(err, ErrSurgicalRigRemoveUnsupported) {
		t.Fatalf("removeRigFromCityTOML error = %v, want an error that does not invite a full-rewrite fallback", err)
	}
}

func TestRemoveRigAndWriteSiteBindingsForEditKeepsCommentsAndDropsBinding(t *testing.T) {
	cityDir := t.TempDir()
	cityPath := filepath.Join(cityDir, "city.toml")
	original := `# city rationale
[workspace]
name = "city"

# alpha rationale
[[rigs]]
name = "alpha"

[[rigs]] # beta: the docs repo
name = "beta"
`
	if err := os.WriteFile(cityPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := PersistRigSiteBindings(fsys.OSFS{}, cityDir, []Rig{
		{Name: "alpha", Path: "/srv/alpha"},
		{Name: "beta", Path: "/srv/beta"},
	}); err != nil {
		t.Fatalf("PersistRigSiteBindings: %v", err)
	}

	cfg := &City{Workspace: Workspace{Name: "city"}, Rigs: []Rig{{Name: "beta", Path: "/srv/beta"}}}
	if err := RemoveRigAndWriteSiteBindingsForEdit(fsys.OSFS{}, cityPath, cfg, "alpha"); err != nil {
		t.Fatalf("RemoveRigAndWriteSiteBindingsForEdit: %v", err)
	}

	got, err := os.ReadFile(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	want := `# city rationale
[workspace]
name = "city"

[[rigs]] # beta: the docs repo
name = "beta"
`
	if string(got) != want {
		t.Fatalf("city.toml after removal:\n%s\nwant:\n%s", got, want)
	}
	binding, err := LoadSiteBinding(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("LoadSiteBinding: %v", err)
	}
	if wantRigs := []RigSiteBinding{{Name: "beta", Path: "/srv/beta"}}; !reflect.DeepEqual(binding.Rigs, wantRigs) {
		t.Fatalf("site binding rigs = %#v, want %#v", binding.Rigs, wantRigs)
	}
}

// When the in-place edit cannot be proven, the writer must leave both files
// as they were: the caller falls back to a full rewrite, and the removed
// rig's binding must not be dropped while city.toml still declares it.
func TestRemoveRigAndWriteSiteBindingsForEditWritesNothingWhenUnsupported(t *testing.T) {
	cityDir := t.TempDir()
	cityPath := filepath.Join(cityDir, "city.toml")
	original := []byte("rigs = [{ name = \"alpha\" }, { name = \"beta\" }]\n\n[workspace]\nname = \"city\"\n")
	if err := os.WriteFile(cityPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := PersistRigSiteBindings(fsys.OSFS{}, cityDir, []Rig{
		{Name: "alpha", Path: "/srv/alpha"},
		{Name: "beta", Path: "/srv/beta"},
	}); err != nil {
		t.Fatalf("PersistRigSiteBindings: %v", err)
	}
	originalSite, err := os.ReadFile(SiteBindingPath(cityDir))
	if err != nil {
		t.Fatal(err)
	}

	cfg := &City{Workspace: Workspace{Name: "city"}, Rigs: []Rig{{Name: "beta", Path: "/srv/beta"}}}
	err = RemoveRigAndWriteSiteBindingsForEdit(fsys.OSFS{}, cityPath, cfg, "alpha")
	if !errors.Is(err, ErrSurgicalRigRemoveUnsupported) {
		t.Fatalf("RemoveRigAndWriteSiteBindingsForEdit error = %v, want an error wrapping ErrSurgicalRigRemoveUnsupported", err)
	}

	if got, err := os.ReadFile(cityPath); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("city.toml = %q (read error %v), want it untouched: %q", got, err, original)
	}
	if got, err := os.ReadFile(SiteBindingPath(cityDir)); err != nil || !bytes.Equal(got, originalSite) {
		t.Fatalf(".gc/site.toml = %q (read error %v), want it untouched: %q", got, err, originalSite)
	}
}
