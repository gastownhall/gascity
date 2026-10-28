package runtime

import (
	"reflect"
	"slices"
	"testing"
)

func TestParseTmuxLaunchEnvState(t *testing.T) {
	out := "A=1\nB=x=y\nEMPTY=\n-GONE\n@gc_launch_env_keys=B A\n"
	st := ParseTmuxLaunchEnvState(out)
	if want := map[string]string{"A": "1", "B": "x=y", "EMPTY": ""}; !reflect.DeepEqual(st.Env, want) {
		t.Errorf("Env = %v, want %v", st.Env, want)
	}
	if !st.Removed["GONE"] || len(st.Removed) != 1 {
		t.Errorf("Removed = %v, want GONE", st.Removed)
	}
	if want := []string{"A", "B"}; !slices.Equal(st.Recorded, want) {
		t.Errorf("Recorded = %v, want %v", st.Recorded, want)
	}
	if st := ParseTmuxLaunchEnvState("A=1\n@gc_launch_env_keys=\n"); len(st.Recorded) != 0 {
		t.Errorf("an empty record read as %v", st.Recorded)
	}
}

func TestPlanTmuxLaunchEnv(t *testing.T) {
	recordTo := func(keys string) []string {
		return []string{"set-option", "-t", "p", TmuxLaunchEnvKeysOption, keys}
	}
	set := func(k, v string) []string { return []string{"set-environment", "-t", "s", k, v} }
	tests := []struct {
		name string
		st   TmuxLaunchEnvState
		want map[string]string
		base map[string]string
		cmds [][]string
	}{{
		name: "unchanged writes nothing",
		st:   TmuxLaunchEnvState{Env: map[string]string{"A": "1"}, Recorded: []string{"A"}},
		want: map[string]string{"A": "1"},
	}, {
		name: "changed, added and dropped keys: the record is widened first, exact last",
		st:   TmuxLaunchEnvState{Env: map[string]string{"A": "1", "B": "2", "META": "m"}, Recorded: []string{"A", "B"}},
		want: map[string]string{"A": "9", "C": "3"},
		cmds: [][]string{
			recordTo("A B C"),
			set("A", "9"),
			set("C", "3"),
			{"set-environment", "-t", "s", "-u", "B"},
			recordTo("A C"),
		},
	}, {
		name: "an unrecorded session removes nothing",
		st:   TmuxLaunchEnvState{Env: map[string]string{"A": "1", "OLD": "x"}},
		want: map[string]string{"A": "2"},
		cmds: [][]string{recordTo("A"), set("A", "2")},
	}, {
		name: "the incarnation keys are never rewritten or removed",
		st: TmuxLaunchEnvState{
			Env:      map[string]string{InstanceTokenEnv: "t1", "BEADS_HOLDER_TOKEN": "t1", RuntimeEpochEnv: "1"},
			Recorded: []string{"BEADS_HOLDER_TOKEN", InstanceTokenEnv, RuntimeEpochEnv},
		},
		want: map[string]string{InstanceTokenEnv: "t2", "BEADS_HOLDER_TOKEN": "t2"},
		cmds: [][]string{recordTo("BEADS_HOLDER_TOKEN " + InstanceTokenEnv)},
	}, {
		name: "a base key is inherited unless changed, and hidden with -r once dropped",
		st:   TmuxLaunchEnvState{Env: map[string]string{}},
		want: map[string]string{"A": "1", "B": "new"},
		base: map[string]string{"A": "1", "B": "old", "D": "x"},
		cmds: [][]string{
			recordTo("A B"),
			set("B", "new"),
			{"set-environment", "-t", "s", "-r", "D"},
		},
	}, {
		name: "a base key already hidden is not marked again, and a re-added one is set",
		st:   TmuxLaunchEnvState{Env: map[string]string{}, Removed: map[string]bool{"D": true, "E": true}, Recorded: []string{"A"}},
		want: map[string]string{"A": "1", "E": "x"},
		base: map[string]string{"A": "1", "D": "x", "E": "x"},
		cmds: [][]string{recordTo("A E"), set("E", "x")},
	}, {
		name: "a key that is no variable name is neither written nor recorded",
		st:   TmuxLaunchEnvState{Env: map[string]string{}, Recorded: []string{"A"}},
		want: map[string]string{"A": "", "-u": "x", "B C": "y"},
		cmds: [][]string{set("A", "")},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.st.Removed == nil {
				tt.st.Removed = map[string]bool{}
			}
			got := PlanTmuxLaunchEnv("s", "p", tt.st, tt.want, tt.base)
			if !reflect.DeepEqual(got, tt.cmds) {
				t.Errorf("plan =\n  %q\nwant\n  %q", got, tt.cmds)
			}
		})
	}
}

func TestTmuxCommandSequence(t *testing.T) {
	got := TmuxCommandSequence([][]string{
		{"set-environment", "-t", "s", "A", "x;"},
		{"set-environment", "-t", "s", "B", ";"},
		{"set-option", "-t", "p", TmuxLaunchEnvKeysOption, "A B"},
	})
	want := []string{
		"set-environment", "-t", "s", "A", `x\;`, ";",
		"set-environment", "-t", "s", "B", `\;`, ";",
		"set-option", "-t", "p", TmuxLaunchEnvKeysOption, "A B",
	}
	if !slices.Equal(got, want) {
		t.Errorf("sequence =\n  %q\nwant\n  %q", got, want)
	}
}
