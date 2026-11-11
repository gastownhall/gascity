package config

import "strings"

// commitClassPreStartFlag is the pre_start token that marks an agent as
// commit-class (its worktree is freshened onto a specific commit rather
// than reset to main on every session start). This mirrors the flag used
// by the setup scripts referenced from pack.toml `pre_start` entries.
const commitClassPreStartFlag = "--freshen-commit"

// IsCommitClass reports whether a is a commit-class agent, i.e. its
// resolved pre_start commands contain the --freshen-commit flag as a
// standalone token. Commit-class agents keep their persistent worktree
// pinned to a specific commit across sessions (as opposed to reset-class
// agents, whose worktree is reset to main on every session start), which
// makes them susceptible to unnoticed drift when no session runs for an
// extended period.
//
// Classification is purely a function of resolved config (never a role
// name), so it applies uniformly to any agent configured with this flag.
func (a *Agent) IsCommitClass() bool {
	for _, cmd := range a.PreStart {
		for _, tok := range strings.Fields(cmd) {
			if tok == commitClassPreStartFlag {
				return true
			}
		}
	}
	return false
}
