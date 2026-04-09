package main

import (
	"context"
	"fmt"
	"io"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/telemetry"
	"github.com/spf13/cobra"
)

// newSessionRecoverCmd creates the "gc session recover <id-or-alias>" command.
//
// This is the soft-recovery rung of mol-shutdown-dance: it delivers a
// provider-specific keystroke sequence (e.g. Claude Code's /rewind) to a
// running session as the cheapest possible attempt to unwedge an agent
// whose conversation context is broken but whose process is still alive.
//
// On success, the agent should resume its in-flight molecule with no
// state lost. On failure (no soft-recovery hint configured for the
// provider, or the keystrokes can't be delivered), the command exits
// non-zero so the dog ladder advances to interrogate/kill.
func newSessionRecoverCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "recover <session-id-or-alias>",
		Short: "Soft-recover a wedged session via provider-specific keystrokes",
		Long: `Soft-recover a session whose conversation context is broken but whose
process is still alive. Used as strike 1 of the dog's shutdown dance.

The keystroke sequence comes from the resolved provider's RecoveryHints
(e.g. Claude Code: Ctrl-U + /rewind + Enter, which rolls the conversation
back to before a 400 tool_use concurrency error without losing any
session, working-directory, or tool state).

Exits non-zero — and writes nothing to the session — when the resolved
provider has no soft-recovery hint configured. In that case stderr
contains the marker "no soft recovery; skipping"; callers
(mol-shutdown-dance strike 1) should grep for that marker to distinguish
"provider has no soft rung, advance immediately" from a hard error
(session not running, send-keys failed) and escalate to interrogate/kill.

Note: gc collapses all RunE errors to exit code 1, so the internal
function-level distinction between exit 1 (hard error) and exit 2
(no soft hint) is preserved only by stderr content at this boundary.

Accepts a session ID (e.g. gc-42) or session alias (e.g. witness-1).`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdSessionRecover(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// cmdSessionRecover is the CLI entry point for "gc session recover".
func cmdSessionRecover(args []string, stdout, stderr io.Writer) int {
	info, err := resolveNudgeTarget(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "gc session recover: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return deliverSessionRecover(info, newSessionProvider(), stdout, stderr)
}

// deliverSessionRecover is the testable core of cmdSessionRecover. The
// nudgeTarget and provider are both injected so unit tests can drive the
// state machine without loading a real city config or tmux runtime.
//
// Exit codes (function-level — collapsed to 1 by the gc CLI wrapper):
//   - 0: keystrokes were delivered.
//   - 1: hard error (provider error, session not running, etc).
//   - 2: provider has no soft-recovery hint configured — caller (the dog
//     ladder) should advance to the next strike immediately. Because the
//     gc CLI collapses non-zero to exit 1, the "advance immediately"
//     signal is ALSO conveyed by the literal stderr substring
//     "no soft recovery; skipping" so molecule prompts can grep for it
//     without depending on exit codes.
func deliverSessionRecover(info nudgeTarget, sp runtime.Provider, stdout, stderr io.Writer) int {
	if info.resolved == nil {
		fmt.Fprintf(stderr, "gc session recover: %s: no resolved provider, no soft recovery; skipping\n", info.agentKey()) //nolint:errcheck
		return 2
	}
	keys := info.resolved.RecoveryHints.SoftRecoveryKeys
	if len(keys) == 0 {
		fmt.Fprintf(stderr, "gc session recover: %s: provider %q has no soft recovery; skipping\n", info.agentKey(), info.resolved.Name) //nolint:errcheck
		return 2
	}

	if !sp.IsRunning(info.sessionName) {
		fmt.Fprintf(stderr, "gc session recover: %s: session is not running\n", info.agentKey()) //nolint:errcheck
		return 1
	}

	if err := sp.SendKeys(info.sessionName, keys...); err != nil {
		telemetry.RecordNudge(context.Background(), info.agentKey(), err)
		fmt.Fprintf(stderr, "gc session recover: %v\n", err) //nolint:errcheck
		return 1
	}
	telemetry.RecordNudge(context.Background(), info.agentKey(), nil)

	fmt.Fprintf(stdout, "Sent soft recovery to %s (%v)\n", info.agentKey(), keys) //nolint:errcheck
	return 0
}
