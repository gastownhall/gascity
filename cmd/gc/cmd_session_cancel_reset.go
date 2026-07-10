package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/session"
	"github.com/spf13/cobra"
)

// cancelResetOutcome is what `gc session cancel-reset` should do for a session,
// derived from its runtime liveness and reset-state metadata.
type cancelResetOutcome int

const (
	// cancelResetRefuseNotRunning: the runtime is not alive. Refuse — clearing a
	// pending reset is only safe for a live session. A not-running session may be
	// mid-restart (past the point of no return, marked by reset_committed_at set
	// after the controller kills the runtime), and the pending reset only matters
	// at the next wake the controller drives anyway.
	cancelResetRefuseNotRunning cancelResetOutcome = iota
	// cancelResetNothingPending: the session is live but carries no pending reset
	// — nothing to cancel.
	cancelResetNothingPending
	// cancelResetClear: the session is live and carries a pending reset that would
	// force a fresh (context-losing) conversation at its next wake; clear it.
	cancelResetClear
)

// decideCancelReset chooses the cancel-reset action from the session's runtime
// liveness and its reset-state metadata. Keeping the decision pure keeps the
// judgment out of the I/O-bound command body and directly testable.
func decideCancelReset(running bool, meta map[string]string) cancelResetOutcome {
	if !running {
		return cancelResetRefuseNotRunning
	}
	if meta["restart_requested"] != "true" && meta["continuation_reset_pending"] != "true" {
		return cancelResetNothingPending
	}
	return cancelResetClear
}

// newSessionCancelResetCmd creates the "gc session cancel-reset <id-or-alias>"
// command. It is the inverse of `gc session reset`: where reset requests a fresh
// restart, cancel-reset clears a pending one.
func newSessionCancelResetCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "cancel-reset <session-id-or-alias>",
		Short: "Clear a pending reset on a live session without recycling it",
		Long: `Clear a pending fresh-restart request on a running session, in place.

A session can carry a pending reset (restart_requested / continuation_reset_pending)
that has not executed yet — for example from an earlier "gc session reset" or
"gc handoff", or from config drift. While the runtime stays alive the pending
flag is harmless, but at the session's next sleep->wake the controller starts a
fresh conversation and the prior context is lost. cancel-reset clears the pending
request in place so the running session keeps its current conversation.

It only acts on a running session (clearing is unsafe once a restart has been
committed and the runtime killed) and does not touch session_key or the
conversation itself. If the session is configured wake_mode=fresh, clearing the
flag cannot prevent a fresh conversation on its next wake — change the agent
config for that.

Accepts a session ID (e.g., gc-42) or session alias (e.g., mayor).`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdSessionCancelReset(args, stdout, stderr, jsonOutput) != 0 {
				return errExit
			}
			return nil
		},
		ValidArgsFunction: completeSessionIDs,
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit JSONL")
	return cmd
}

// cmdSessionCancelReset is the CLI entry point for "gc session cancel-reset".
func cmdSessionCancelReset(args []string, stdout, stderr io.Writer, jsonOutput ...bool) int {
	asJSON := sessionJSONRequested(jsonOutput)
	store, code := openCityStore(stderr, "gc session cancel-reset")
	if store == nil {
		return code
	}

	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc session cancel-reset: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, _ := loadCityConfig(cityPath, stderr)

	// Every store consumer here is session-class (ID resolution, session-bead
	// load, restart-flag clear), so route the whole flow through the session
	// coordination-class store for relocation-safety.
	sessStore := cliSessionStore(store, cfg, cityPath)
	sessionID, err := resolveSessionIDWithConfig(cityPath, cfg, sessStore, args[0])
	if err != nil {
		fmt.Fprintf(stderr, "gc session cancel-reset: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	bead, err := sessStore.Get(sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "gc session cancel-reset: loading session %s: %v\n", sessionID, err) //nolint:errcheck // best-effort stderr
		return 1
	}

	sp := newSessionProvider()
	sessionName := strings.TrimSpace(bead.Metadata["session_name"])
	running := sessionName != "" && sp.IsRunning(sessionName)

	switch decideCancelReset(running, bead.Metadata) {
	case cancelResetRefuseNotRunning:
		fmt.Fprintf(stderr, "gc session cancel-reset: session %s is not running; cancel-reset clears a live session's pending reset (a not-running session may be mid-restart). Use \"gc session reset\" or \"gc session wake\" instead.\n", sessionID) //nolint:errcheck // best-effort stderr
		return 1
	case cancelResetNothingPending:
		fmt.Fprintf(stdout, "Session %s has no pending reset to cancel.\n", sessionID) //nolint:errcheck // best-effort stdout
		return 0
	}

	// Clear the reset intent in place. clearRestartRequest clears
	// restart_requested + continuation_reset_pending (bead) and the runtime
	// GC_RESTART_REQUESTED flag; additionally clear reset_committed_at so a later
	// re-arm of continuation_reset_pending cannot pair with a stale commit marker
	// and trip a false reset-stalled alarm. Neither touches session_key or
	// started_config_hash, so the live conversation is preserved.
	if err := clearRestartRequest(sessStore, newDrainOps(sp), sessionName); err != nil {
		fmt.Fprintf(stderr, "gc session cancel-reset: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := sessionFrontDoor(sessStore).ApplyPatch(sessionID, map[string]string{
		session.ResetCommittedAtKey: "",
	}); err != nil {
		fmt.Fprintf(stderr, "gc session cancel-reset: clearing reset commit marker for %s: %v\n", sessionID, err) //nolint:errcheck // best-effort stderr
		return 1
	}

	_ = pokeController(cityPath)

	if bead.Metadata["wake_mode"] == "fresh" {
		fmt.Fprintf(stderr, "gc session cancel-reset: note: session %s is configured wake_mode=fresh; clearing the pending reset will not prevent a fresh conversation on its next wake (change the agent config for that).\n", sessionID) //nolint:errcheck // best-effort stderr
	}

	if asJSON {
		if err := writeSessionActionJSON(stdout, sessionActionResult{
			Action:    "cancel-reset",
			SessionID: sessionID,
			Identity:  namedSessionIdentity(bead),
		}); err != nil {
			fmt.Fprintf(stderr, "gc session cancel-reset: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "Session %s pending reset cleared; the running session keeps its current conversation.\n", sessionID) //nolint:errcheck // best-effort stdout
	return 0
}
