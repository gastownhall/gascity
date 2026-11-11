package config

import "testing"

func TestAgentIsCommitClass(t *testing.T) {
	cases := []struct {
		name string
		a    Agent
		want bool
	}{
		{
			name: "freshen-commit flag present",
			a:    Agent{PreStart: []string{"scripts/worktree-setup.sh . . base --sync --freshen-commit"}},
			want: true,
		},
		{
			name: "reset-main flag present",
			a:    Agent{PreStart: []string{"scripts/worktree-setup.sh . . base --sync --reset-main"}},
			want: false,
		},
		{
			name: "no pre_start at all",
			a:    Agent{},
			want: false,
		},
		{
			name: "pre_start present but unrelated command",
			a:    Agent{PreStart: []string{"echo hello"}},
			want: false,
		},
		{
			name: "freshen-commit among multiple pre_start commands",
			a: Agent{PreStart: []string{
				"echo setup",
				"scripts/worktree-setup.sh . . base --sync --freshen-commit",
			}},
			want: true,
		},
		{
			name: "substring match must not false-positive on a longer flag",
			a:    Agent{PreStart: []string{"scripts/worktree-setup.sh . . base --freshen-commit-and-something-else"}},
			want: false,
		},
		{
			name: "both flags present: freshen-commit wins (mutually exclusive by design upstream)",
			a: Agent{PreStart: []string{
				"scripts/worktree-setup.sh . . base --sync --reset-main --freshen-commit",
			}},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.IsCommitClass(); got != tc.want {
				t.Errorf("Agent.IsCommitClass() = %v, want %v", got, tc.want)
			}
		})
	}
}
