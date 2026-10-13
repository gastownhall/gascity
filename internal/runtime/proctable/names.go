package proctable

import (
	"path/filepath"
	"strings"
)

// ProcessNameSet builds the set of process names a liveness check accepts,
// trimming blanks. Shared by the local tmux provider's process state and the
// ssh provider's remote process tree, so both judge a pane alike.
func ProcessNameSet(names []string) map[string]struct{} {
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			set[name] = struct{}{}
			// The kimi entry point now sets COMM and argv[0] to kimi-code.
			// Keep existing provider process_names valid for both CLIs.
			if name == "kimi" {
				set["kimi-code"] = struct{}{}
			}
		}
	}
	return set
}

// ProcessMatchesNameSet reports whether a process, given its command
// identity (ps comm) and full command line (ps args), is one of names: by the
// basename of its command, of its argv[0], or, for a known interpreter (node,
// bun, npx, deno), of the script it runs.
func ProcessMatchesNameSet(command, args string, names map[string]struct{}) bool {
	if len(names) == 0 {
		return false
	}
	command = filepath.Base(strings.TrimSpace(command))
	if _, ok := names[command]; ok && command != "" {
		return true
	}
	argv := strings.Fields(strings.TrimSpace(args))
	if len(argv) == 0 {
		return false
	}
	argv0 := filepath.Base(argv[0])
	if _, ok := names[argv0]; ok {
		return true
	}
	if _, isInterpreter := knownInterpreters[argv0]; !isInterpreter {
		return false
	}
	for _, token := range argv[1:] {
		token = strings.TrimSpace(token)
		if token == "" || strings.HasPrefix(token, "-") {
			continue
		}
		if _, isRunner := runnerSubcommands[token]; isRunner {
			continue
		}
		base := filepath.Base(token)
		if _, ok := names[base]; ok {
			return true
		}
		baseNoExt := strings.TrimSuffix(base, filepath.Ext(base))
		if _, ok := names[baseNoExt]; ok {
			return true
		}
		break
	}
	return false
}

var knownInterpreters = map[string]struct{}{
	"node": {}, "bun": {}, "npx": {}, "deno": {},
}

var runnerSubcommands = map[string]struct{}{
	"run": {}, "exec": {}, "x": {},
}
