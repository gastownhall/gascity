package configedit_test

import (
	"errors"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/fsys"
)

const patchMergeCity = `[workspace]
name = "test-city"
[[agent]]
name = "worker"
provider = "claude"
max_active_sessions = 1
[[named_session]]
template = "worker"
`

func TestMergeAgentPatchPreservesAndRestores(t *testing.T) {
	path := writeTOML(t, t.TempDir(), patchMergeCity+`
[[patches.agent]]
name = "worker"
scope = "city"
pre_start = ["prepare"]
env = { KEEP = "yes" }
option_defaults = { effort = "high" }
`)
	ed := configedit.NewEditor(fsys.OSFS{}, path)
	before := readTOML(t, path).Patches.Agents[0]
	provider := "codex"
	result, err := ed.MergeAgentPatch(configedit.AgentPatchMerge{
		Name: "worker", Revision: configedit.AgentPatchRevision(&before), Provider: &provider,
		OptionDefaults: map[string]string{"model": "gpt-5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := readTOML(t, path).Patches.Agents[0]
	if got.Env["KEEP"] != "yes" || got.Scope == nil || *got.Scope != "city" || !reflect.DeepEqual(got.PreStart, before.PreStart) || got.OptionDefaults["effort"] != "high" || got.OptionDefaults["model"] != "gpt-5" || got.Provider == nil || *got.Provider != provider {
		t.Fatalf("merge dropped fields: %+v", got)
	}
	if !reflect.DeepEqual(result.Before, &before) {
		t.Fatalf("before = %+v", result.Before)
	}
	if _, err := ed.MergeAgentPatch(configedit.AgentPatchMerge{Name: "worker", Revision: result.Revision, ClearProvider: true, RemoveOptions: []string{"model"}}); err != nil {
		t.Fatal(err)
	}
	if restored := readTOML(t, path).Patches.Agents[0]; !reflect.DeepEqual(restored, before) {
		t.Fatalf("restore = %+v, want %+v", restored, before)
	}
}

func TestMergeAgentPatchAbsentAndCAS(t *testing.T) {
	path := writeTOML(t, t.TempDir(), patchMergeCity)
	ed := configedit.NewEditor(fsys.OSFS{}, path)
	provider := "codex"
	merge := configedit.AgentPatchMerge{Name: "worker", Revision: configedit.AgentPatchRevision(nil), Provider: &provider}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			_, err := ed.MergeAgentPatch(merge)
			if err == nil {
				wins.Add(1)
			} else if !errors.Is(err, configedit.ErrRevisionConflict) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("successful writes = %d", wins.Load())
	}
	got := readTOML(t, path).Patches.Agents[0]
	if _, err := ed.MergeAgentPatch(configedit.AgentPatchMerge{Name: "worker", Revision: configedit.AgentPatchRevision(&got), Delete: true}); err != nil {
		t.Fatal(err)
	}
	if patches := readTOML(t, path).Patches.Agents; len(patches) != 0 {
		t.Fatalf("restored absent patch = %+v", patches)
	}
}

func TestMergeAgentPatchRejectsInvalidMutationWithoutWriting(t *testing.T) {
	provider := "codex"
	for _, change := range []configedit.AgentPatchMerge{
		{Name: "worker"},
		{Name: "worker", Revision: "absent", Provider: &provider, ClearProvider: true},
		{Name: "worker", Revision: "absent", OptionDefaults: map[string]string{"model": "x"}, RemoveOptions: []string{"model"}},
		{Name: "worker", Revision: "absent", Delete: true, Provider: &provider},
		{Name: "worker", Revision: "absent", Rig: "*"},
	} {
		path := writeTOML(t, t.TempDir(), patchMergeCity)
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := configedit.NewEditor(fsys.OSFS{}, path).MergeAgentPatch(change); err == nil {
			t.Fatalf("accepted invalid mutation: %+v", change)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(before) != string(after) {
			t.Fatal("invalid mutation wrote config")
		}
	}
}

func TestMergeAgentPatchRefusesDuplicateRawEntries(t *testing.T) {
	path := writeTOML(t, t.TempDir(), patchMergeCity+`
[[patches.agent]]
name = "worker"
env = { FIRST = "1" }
[[patches.agent]]
name = "worker"
env = { SECOND = "2" }
`)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	patch := readTOML(t, path).Patches.Agents[0]
	_, err = configedit.NewEditor(fsys.OSFS{}, path).MergeAgentPatch(configedit.AgentPatchMerge{Name: "worker", Revision: configedit.AgentPatchRevision(&patch)})
	if !errors.Is(err, configedit.ErrPatchScope) {
		t.Fatalf("ambiguous patch error=%v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("ambiguous patch write changed disk")
	}
}

func TestMergeAgentPatchRefusesAmbiguousTargets(t *testing.T) {
	for _, tc := range []struct{ name, extra string }{
		{"multiple named identities", "\n[[named_session]]\nname = \"other\"\ntemplate = \"worker\"\n"},
		{"pool", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := patchMergeCity + tc.extra
			if tc.name == "pool" {
				content = `[workspace]
name = "test-city"
[[agent]]
name = "worker"
provider = "claude"
max_active_sessions = 3
`
			}
			path := writeTOML(t, t.TempDir(), content)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			ed := configedit.NewEditor(fsys.OSFS{}, path)
			_, err = ed.MergeAgentPatch(configedit.AgentPatchMerge{Name: "worker", Revision: configedit.AgentPatchRevision(nil)})
			if !errors.Is(err, configedit.ErrPatchScope) {
				t.Fatalf("error = %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatal("refused mutation changed disk")
			}
		})
	}
}
