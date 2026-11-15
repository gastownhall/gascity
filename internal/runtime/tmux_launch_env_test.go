package runtime

import (
	"reflect"
	"slices"
	"testing"
)

// The state is read with show-environment -s, which prints each value
// double-quoted for a POSIX shell: $, `, \ and " are backslash-escaped and a
// newline stays literal inside the quotes; a removed key prints as unset.
func TestParseTmuxLaunchEnvState(t *testing.T) {
	out := `A="1"; export A;` + "\n" +
		`B="x=y"; export B;` + "\n" +
		`EMPTY=""; export EMPTY;` + "\n" +
		`Q="a\"b\$c\` + "`" + `d\\e;f"; export Q;` + "\n" +
		"unset GONE;\n" +
		"@gc_launch_env_keys=B A\n"
	st := ParseTmuxLaunchEnvState(out)
	if want := map[string]string{"A": "1", "B": "x=y", "EMPTY": "", "Q": "a\"b$c`d\\e;f"}; !reflect.DeepEqual(st.Env, want) {
		t.Errorf("Env = %q, want %q", st.Env, want)
	}
	if !st.Removed["GONE"] || len(st.Removed) != 1 {
		t.Errorf("Removed = %v, want GONE", st.Removed)
	}
	if want := []string{"A", "B"}; !slices.Equal(st.Recorded, want) {
		t.Errorf("Recorded = %v, want %v", st.Recorded, want)
	}
	if st := ParseTmuxLaunchEnvState(`A="1"; export A;` + "\n@gc_launch_env_keys=\n"); len(st.Recorded) != 0 {
		t.Errorf("an empty record read as %v", st.Recorded)
	}
}

// A multi-line value is one entry: none of its later lines reads as a key of
// its own, a removal mark, or the record.
func TestParseTmuxLaunchEnvStateMultiLineValue(t *testing.T) {
	ml := "line1\nB=spurious\n-GHOST\nunset A;\n@gc_launch_env_keys=B\nend"
	out := "ML=\"" + ml + "\"; export ML;\n" +
		"unset R;\n" +
		"@gc_launch_env_keys=ML\n"
	st := ParseTmuxLaunchEnvState(out)
	if want := map[string]string{"ML": ml}; !reflect.DeepEqual(st.Env, want) {
		t.Errorf("Env = %q, want %q", st.Env, want)
	}
	if want := map[string]bool{"R": true}; !reflect.DeepEqual(st.Removed, want) {
		t.Errorf("Removed = %v, want %v", st.Removed, want)
	}
	if want := []string{"ML"}; !slices.Equal(st.Recorded, want) {
		t.Errorf("Recorded = %v, want %v", st.Recorded, want)
	}
}

// An unterminated quoted value (output cut short) yields no entry, and does
// not swallow the lines after it as its value.
func TestParseTmuxLaunchEnvStateUnterminatedValue(t *testing.T) {
	st := ParseTmuxLaunchEnvState("A=\"cut\nshort")
	if len(st.Env) != 0 || len(st.Removed) != 0 || len(st.Recorded) != 0 {
		t.Errorf("state = %+v, want empty", st)
	}
}

func TestTmuxLaunchEnvStateArgsReadsQuotedValues(t *testing.T) {
	args := TmuxLaunchEnvStateArgs("s", "p")
	if want := []string{"show-environment", "-s", "-t", "s", ";"}; !slices.Equal(args[:len(want)], want) {
		t.Errorf("state read = %q, want it to start %q", args, want)
	}
}

// The planner, fed what tmux prints for a session whose launch set a
// multi-line value, sees the value unchanged and plans nothing.
func TestPlanTmuxLaunchEnvUnchangedMultiLineValue(t *testing.T) {
	ml := "line1\nB=spurious\n-GHOST\nend"
	st := ParseTmuxLaunchEnvState("A=\"1\"; export A;\nML=\"" + ml + "\"; export ML;\n@gc_launch_env_keys=A ML\n")
	if cmds := PlanTmuxLaunchEnv("s", "p", st, map[string]string{"A": "1", "ML": ml}, nil); cmds != nil {
		t.Errorf("plan = %q, want nothing", cmds)
	}
	want := [][]string{
		{"set-environment", "-t", "s", "-u", "ML"},
		{"set-option", "-t", "p", TmuxLaunchEnvKeysOption, "A"},
	}
	if cmds := PlanTmuxLaunchEnv("s", "p", st, map[string]string{"A": "1"}, nil); !reflect.DeepEqual(cmds, want) {
		t.Errorf("plan for a dropped multi-line key =\n  %q\nwant\n  %q", cmds, want)
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
