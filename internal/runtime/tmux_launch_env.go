package runtime

import (
	"regexp"
	"slices"
	"sort"
	"strings"
)

// A warm-box relaunch respawns the agent with respawn-pane, which takes no env
// argument: the new process inherits the tmux server's global environment as
// filtered by the SESSION environment. A launch-only env change (an operator
// env value, a provider's env) therefore reaches a relaunched agent only if
// the session environment is brought to the new env first. The helpers here
// plan that update for the remote tmux providers (ssh, k8s), which drive tmux
// through their own connections.

// TmuxLaunchEnvKeysOption is the session user option that lists the keys a
// launch set in the session environment, so a later relaunch can tell a key
// its config no longer sets from one put there by other means (SetMeta,
// update-environment). A user option, not an environment entry, so no agent
// process inherits it. The value is the key names, space-separated.
const TmuxLaunchEnvKeysOption = "@gc_launch_env_keys"

// tmuxLaunchEnvKeysLine prefixes the record's line in the state read's output.
// No show-environment line can start with it: those are KEY=value or -KEY, and
// "@" starts no environment variable name.
const tmuxLaunchEnvKeysLine = TmuxLaunchEnvKeysOption + "="

// launchIdentityEnvKeys name the session incarnation. A relaunch keeps the
// incarnation, so it never rewrites or removes them: the session environment
// is where meta reads and the input fence find them.
var launchIdentityEnvKeys = map[string]bool{
	InstanceTokenEnv:     true,
	RuntimeEpochEnv:      true,
	ContinuationEpochEnv: true,
	"BEADS_HOLDER_TOKEN": true,
}

// tmuxEnvNameRe is the shape of a key a relaunch will write or remove. A key
// outside it cannot have come from a launch, and one starting with "-" would
// read as a flag.
var tmuxEnvNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// TmuxLaunchEnvState is a session's environment as a relaunch reads it.
type TmuxLaunchEnvState struct {
	// Env holds the session's KEY=value entries.
	Env map[string]string
	// Removed holds the keys marked removed (-KEY): hidden from new processes
	// even when the server's global environment sets them.
	Removed map[string]bool
	// Recorded is the record of the keys the last launch set, sorted; empty
	// when the session has none (created before the record existed).
	Recorded []string
}

// TmuxLaunchEnvStateArgs returns the tmux arguments (without the leading
// "tmux") that print a session's environment and its launch-key record in one
// invocation, so a relaunch costs a single read and sees the two as one
// snapshot. session is the -t target for show-environment, pane the
// target-pane form for display-message.
func TmuxLaunchEnvStateArgs(session, pane string) []string {
	return []string{
		"show-environment", "-t", session, ";",
		"display-message", "-p", "-t", pane, tmuxLaunchEnvKeysLine + "#{" + TmuxLaunchEnvKeysOption + "}",
	}
}

// ParseTmuxLaunchEnvState reads the output of [TmuxLaunchEnvStateArgs].
// Output without the record's line reads as no record.
func ParseTmuxLaunchEnvState(out string) TmuxLaunchEnvState {
	st := TmuxLaunchEnvState{Env: map[string]string{}, Removed: map[string]bool{}}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), tmuxLaunchEnvKeysLine); ok {
			st.Recorded = strings.Fields(rest)
			sort.Strings(st.Recorded)
			continue
		}
		if key, ok := strings.CutPrefix(line, "-"); ok {
			if tmuxEnvNameRe.MatchString(key) {
				st.Removed[key] = true
			}
			continue
		}
		if key, val, ok := strings.Cut(line, "="); ok && tmuxEnvNameRe.MatchString(key) {
			st.Env[key] = val
		}
	}
	return st
}

// TmuxLaunchEnvRecordArgs returns the tmux arguments that set pane's session
// launch-key record to the keys of env.
func TmuxLaunchEnvRecordArgs(pane string, env map[string]string) []string {
	return tmuxLaunchEnvRecordArgs(pane, launchEnvKeys(env))
}

func tmuxLaunchEnvRecordArgs(pane string, keys []string) []string {
	return []string{"set-option", "-t", pane, TmuxLaunchEnvKeysOption, strings.Join(keys, " ")}
}

// PlanTmuxLaunchEnv returns the tmux commands that bring a session's
// environment to what a fresh launch with want would leave the agent with,
// given the session's current state. It returns nil when nothing differs.
//
// base is the environment a new process gets where the session environment
// has no entry for a key: the tmux server's global environment as far as the
// launch set it (a k8s pod's container env), or nil when the launch put every
// key in the session environment itself (ssh).
//
//   - A key want sets is set when the agent would not already inherit that
//     value.
//   - A key the last launch set (the record, or base) that want no longer
//     names is hidden: marked removed (-r) when base sets it, so the global
//     value stays hidden, else unset (-u).
//   - The incarnation keys are never touched, nor is any key the last launch
//     did not set (gc's own session metadata lives there too).
//
// The commands run as one tmux command sequence, which stops at the first
// failure. The record is first widened to every key the sequence may set, and
// the exact record is written last, so a sequence cut short leaves a record
// that still names each key it may have left behind: the next relaunch, with
// this env or a later one, converges.
func PlanTmuxLaunchEnv(session, pane string, st TmuxLaunchEnvState, want, base map[string]string) [][]string {
	effective := func(k string) (string, bool) {
		if st.Removed[k] {
			return "", false
		}
		if v, ok := st.Env[k]; ok {
			return v, true
		}
		v, ok := base[k]
		return v, ok
	}
	var writes [][]string
	wantKeys := launchEnvKeys(want)
	for _, k := range wantKeys {
		if launchIdentityEnvKeys[k] {
			continue
		}
		if have, ok := effective(k); ok && have == want[k] {
			continue
		}
		writes = append(writes, []string{"set-environment", "-t", session, k, want[k]})
	}
	previous := mergeSortedKeys(st.Recorded, launchEnvKeys(base))
	for _, k := range previous {
		if _, still := want[k]; still || launchIdentityEnvKeys[k] || !tmuxEnvNameRe.MatchString(k) {
			continue
		}
		if _, ok := base[k]; ok {
			if !st.Removed[k] {
				writes = append(writes, []string{"set-environment", "-t", session, "-r", k})
			}
			continue
		}
		if _, ok := st.Env[k]; ok {
			writes = append(writes, []string{"set-environment", "-t", session, "-u", k})
		}
	}
	var cmds [][]string
	recorded := st.Recorded
	if len(writes) > 0 {
		if widened := mergeSortedKeys(recorded, wantKeys); !slices.Equal(widened, recorded) {
			cmds = append(cmds, tmuxLaunchEnvRecordArgs(pane, widened))
			recorded = widened
		}
		cmds = append(cmds, writes...)
	}
	if !slices.Equal(wantKeys, recorded) {
		cmds = append(cmds, tmuxLaunchEnvRecordArgs(pane, wantKeys))
	}
	return cmds
}

// TmuxCommandSequence flattens cmds into the arguments of one tmux invocation,
// separated by ";" arguments. tmux reads an argument that ends in ";" as a
// separator, so an argument of a command that ends in ";" gets a "\" before
// its last character (tmux turns a trailing "\;" back into ";").
func TmuxCommandSequence(cmds [][]string) []string {
	var out []string
	for i, cmd := range cmds {
		if i > 0 {
			out = append(out, ";")
		}
		for _, a := range cmd {
			if strings.HasSuffix(a, ";") {
				a = a[:len(a)-1] + `\;`
			}
			out = append(out, a)
		}
	}
	return out
}

// launchEnvKeys returns the sorted keys of env that are environment variable
// names; no other key is written to or recorded in a session.
func launchEnvKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		if tmuxEnvNameRe.MatchString(k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// mergeSortedKeys returns the sorted, de-duplicated union of a and b.
func mergeSortedKeys(a, b []string) []string {
	out := slices.Concat(a, b)
	slices.Sort(out)
	return slices.Compact(out)
}
