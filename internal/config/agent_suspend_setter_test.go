package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func writeSetterCity(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "city.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSetRigAgentSuspendedForEdit(t *testing.T) {
	twoRigs := `[workspace]
name = "c"

[[rigs]]
name = "a"

[[rigs.patches]]
agent = "worker"
suspended = true # rig a

[[rigs]]
name = "b"

[[rigs.overrides]]
agent = "worker"
suspended = true # rig b override

[[rigs.patches]]
agent = "worker"
suspended  =  true   # rig b patch
`
	for _, tc := range []struct {
		name    string
		city    string
		rig     string
		table   string
		want    string // substring that must appear after the edit
		wantErr bool
	}{
		{name: "edits only the named rig", city: twoRigs, rig: "b", table: RigSetterPatches, want: "suspended  =  false   # rig b patch"},
		{name: "edits the override table separately", city: twoRigs, rig: "b", table: RigSetterOverrides, want: "suspended = false # rig b override"},
		{name: "duplicate rig name refused", city: twoRigs + "\n[[rigs]]\nname = \"b\"\n", rig: "b", table: RigSetterPatches, wantErr: true},
		{name: "repeated setter refused", city: twoRigs + "\n[[rigs.patches]]\nagent = \"worker\"\nsuspended = true\n", rig: "b", table: RigSetterPatches, wantErr: true},
		{name: "inline table form refused", city: "[[rigs]]\nname = \"a\"\npatches = [{agent = \"worker\", suspended = true}]\n", rig: "a", table: RigSetterPatches, wantErr: true},
		{name: "setter without suspended key refused", city: "[[rigs]]\nname = \"a\"\n\n[[rigs.patches]]\nagent = \"worker\"\nnudge = \"x\"\n", rig: "a", table: RigSetterPatches, wantErr: true},
		{name: "unknown table refused", city: twoRigs, rig: "a", table: "imports", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeSetterCity(t, tc.city)
			err := SetRigAgentSuspendedForEdit(fsys.OSFS{}, path, &City{}, tc.rig, tc.table, "worker", false)
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want refusal, got nil; city.toml now:\n%s", after)
				}
				if string(after) != tc.city {
					t.Fatalf("refused edit changed city.toml:\n%s", after)
				}
				return
			}
			if err != nil {
				t.Fatalf("SetRigAgentSuspendedForEdit: %v", err)
			}
			if !strings.Contains(string(after), tc.want) {
				t.Fatalf("city.toml missing %q:\n%s", tc.want, after)
			}
			if strings.Count(string(after), "false") != 1 {
				t.Fatalf("edit changed more than one setter:\n%s", after)
			}
			if _, err := Parse(after); err != nil {
				t.Fatalf("edited city.toml no longer parses: %v", err)
			}
		})
	}
}

func TestSetAgentPatchSuspendedForEdit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		city    string
		dir     string
		agent   string
		want    string
		wantErr bool
	}{
		{
			name: "inserts explicit false after last key",
			city: "[[patches.agent]]\nname = \"base.worker\"\nnudge = \"n\" # keep\n\n[daemon]\n",
			// The new key lands after the last non-blank body line.
			agent: "base.worker", want: "nudge = \"n\" # keep\nsuspended = false\n\n[daemon]\n",
		},
		{
			name:  "replaces existing value keeping comment",
			city:  "[[patches.agent]]\ndir = \"proj\"\nname = \"base.worker\"\nsuspended = true # held\n",
			dir:   "proj",
			agent: "base.worker", want: "suspended = false # held\n",
		},
		{name: "bare name does not match binding-qualified block", city: "[[patches.agent]]\nname = \"base.worker\"\nsuspended = true\n", agent: "worker", wantErr: true},
		{name: "rig-keyed block refused", city: "[[patches.agent]]\nrig = \"proj\"\nname = \"base.worker\"\nsuspended = true\n", dir: "proj", agent: "base.worker", wantErr: true},
		{name: "wildcard block refused", city: "[[patches.agent]]\nrig = \"*\"\nname = \"worker\"\nsuspended = true\n", agent: "worker", wantErr: true},
		{name: "duplicate blocks refused", city: "[[patches.agent]]\nname = \"w\"\n\n[[patches.agent]]\nname = \"w\"\n", agent: "w", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeSetterCity(t, tc.city)
			err := SetAgentPatchSuspendedForEdit(fsys.OSFS{}, path, &City{}, tc.dir, tc.agent, false)
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if tc.wantErr {
				if !errors.Is(err, ErrSurgicalAgentEditUnsupported) {
					t.Fatalf("err = %v, want ErrSurgicalAgentEditUnsupported", err)
				}
				if string(after) != tc.city {
					t.Fatalf("refused edit changed city.toml:\n%s", after)
				}
				return
			}
			if err != nil {
				t.Fatalf("SetAgentPatchSuspendedForEdit: %v", err)
			}
			if !strings.Contains(string(after), tc.want) {
				t.Fatalf("city.toml missing %q:\n%s", tc.want, after)
			}
			cfg, err := Parse(after)
			if err != nil {
				t.Fatalf("edited city.toml no longer parses: %v", err)
			}
			if len(cfg.Patches.Agents) != 1 || cfg.Patches.Agents[0].Suspended == nil || *cfg.Patches.Agents[0].Suspended {
				t.Fatalf("patch suspended = %v, want explicit false", cfg.Patches.Agents)
			}
		})
	}
}
