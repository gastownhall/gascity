package main

import "strings"

// rootCommandOptions controls side effects performed while constructing the
// Cobra tree. invocationArgs is always the injected run(args) slice and never
// includes argv[0].
type rootCommandOptions struct {
	invocationArgs            []string
	discoverPackCommands      bool
	eagerPackCommandDiscovery bool
}

func rootCommandOptionsForArgs(args []string) rootCommandOptions {
	command, ok := firstRootCommand(args)
	discoverPackCommands := !ok || !rootCommandSkipsPackDiscovery(command)
	return rootCommandOptions{
		invocationArgs:            append([]string(nil), args...),
		discoverPackCommands:      discoverPackCommands,
		eagerPackCommandDiscovery: discoverPackCommands,
	}
}

// rootCommandSkipsPackDiscovery identifies built-in commands that cannot
// resolve to a pack binding. Pack discovery only adds city-config and pack
// loading work; each command still performs its normal scope and config
// resolution when it runs.
//
// hook, nudge, mail, and prime are the managed provider-hook surface: every
// session start runs `gc prime --hook`, and every agent turn runs `gc hook run
// -- nudge drain --inject` and `gc hook run -- mail check --inject`, where
// `hook run` re-execs gc for its child. Discovery would otherwise load the city
// config and pack tree once per gc process before the built-in command even
// starts. Packs mount only as root-level bindings and a binding that names a
// core command is skipped (addDiscoveredCommandsToRoot), so no pack can
// contribute to any of these trees.
func rootCommandSkipsPackDiscovery(command string) bool {
	switch command {
	case "metrics", "bd", "git-credential", "dolt-state", "dolt-config", "bd-store-bridge", "hook", "nudge", "mail", "prime":
		return true
	default:
		return false
	}
}

// firstRootCommand returns the first command word under the root's narrow
// persistent-scope grammar. Unknown flags fail closed because this pre-scan
// cannot know whether a later token is their value. A separate known value
// flag consumes exactly one following token, including "--", matching pflag.
func firstRootCommand(args []string) (string, bool) {
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "--":
			return "", false
		case isRootPersistentValueFlag(arg):
			if index+1 >= len(args) {
				return "", false
			}
			index++
		case isRootPersistentValueAssignment(arg):
			continue
		case strings.HasPrefix(arg, "-"):
			return "", false
		default:
			return arg, true
		}
	}
	return "", false
}

func isRootPersistentValueFlag(arg string) bool {
	switch arg {
	case "--city", "--rig", "--context", "--city-url", "--city-name":
		return true
	default:
		return false
	}
}

func isRootPersistentValueAssignment(arg string) bool {
	name, _, hasValue := strings.Cut(arg, "=")
	return hasValue && isRootPersistentValueFlag(name)
}
