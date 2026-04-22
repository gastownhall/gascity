package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/spf13/cobra"
)

// handoffRestartWait bounds how long the handoff process blocks after
// requesting a controller restart. The controller normally tears the
// process tree down well within this window; if it doesn't (broken
// reconciler, missed flag, hook fired outside a managed session), we
// exit cleanly so callers like PreCompact don't hang Claude indefinitely.
const handoffRestartWait = 30 * time.Second

// readClaudeHookEventName best-effort reads the Claude Code hook payload
// from stdin and returns the hook_event_name field. Returns "" if stdin
// is a TTY, empty, or not a valid hook payload — i.e. when gc handoff is
// invoked manually rather than from a hook.
//
// Claude Code passes a JSON object to every hook on stdin; the field
// hook_event_name distinguishes PreCompact from SessionStart, Stop,
// PostToolUse, etc. We use this to detect when handoff is being called
// from PreCompact, where killing the session mid-compaction discards
// in-flight context (the compaction summary the user wants to keep).
func readClaudeHookEventName(r io.Reader) string {
	if r == nil {
		return ""
	}
	if f, ok := r.(*os.File); ok {
		info, err := f.Stat()
		if err != nil {
			return ""
		}
		// TTY → no hook payload to read; don't block on user input.
		if info.Mode()&os.ModeCharDevice != 0 {
			return ""
		}
	}
	// Hook payloads are tiny (low-KB). Cap the read so a misconfigured
	// pipe can't make us swallow megabytes.
	data, err := io.ReadAll(io.LimitReader(r, 64*1024))
	if err != nil || len(data) == 0 {
		return ""
	}
	var payload struct {
		HookEventName string `json:"hook_event_name"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return ""
	}
	return payload.HookEventName
}

func newHandoffCmd(stdout, stderr io.Writer) *cobra.Command {
	var target string
	cmd := &cobra.Command{
		Use:   "handoff <subject> [message]",
		Short: "Send handoff mail and restart controller-managed sessions",
		Long: `Convenience command for context handoff.

Self-handoff (default): sends mail to self. If the current session is
controller-restartable, requests a restart and blocks until the controller
stops the session. For on-demand configured named sessions, sends mail and
returns without requesting restart because the controller cannot restart the
user-attended process.

For controller-restartable sessions, equivalent to:

  gc mail send $GC_ALIAS <subject> [message]
  gc runtime request-restart

Remote handoff (--target): sends mail to a target session. If the target is
controller-restartable, kills it so the reconciler restarts it with the handoff
mail waiting. For on-demand configured named targets, sends mail and returns
without killing the session.

For controller-restartable targets, equivalent to:

  gc mail send <target> <subject> [message]
  gc session kill <target>

Self-handoff requires session context (GC_ALIAS or GC_SESSION_ID, plus
GC_SESSION_NAME and city context env). Remote handoff accepts a session alias or ID.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdHandoff(args, target, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&target, "target", "", "Remote session alias or ID to handoff (kills only controller-restartable sessions)")
	return cmd
}

func cmdHandoff(args []string, target string, stdout, stderr io.Writer) int {
	if target != "" {
		return cmdHandoffRemote(args, target, stdout, stderr)
	}

	// Detect Claude Code hook context BEFORE doing anything else — stdin is
	// consumed by the read and we want this to drive whether we even
	// consider requesting a restart.
	hookEvent := readClaudeHookEventName(os.Stdin)

	current, err := currentSessionRuntimeTarget()
	if err != nil {
		fmt.Fprintf(stderr, "gc handoff: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	store, err := openCityStoreAt(current.cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc handoff: %v\n", err)                    //nolint:errcheck // best-effort stderr
		fmt.Fprintln(stderr, "hint: run \"gc doctor\" for diagnostics") //nolint:errcheck // best-effort stderr
		return 1
	}
	sp := newSessionProvider()
	dops := newDrainOps(sp)
	rec := openCityRecorderAt(current.cityPath, stderr)
	cfg, _ := loadCityConfig(current.cityPath, stderr)
	persistRestart := sessionRestartPersister(current.cityPath, store, sp, cfg, current.sessionName)

	// PreCompact hooks must finish without killing the session: Claude is
	// in the middle of producing the compaction summary, and tearing the
	// process tree down here discards everything in flight (the user's
	// "the mayor crashed and lost context" symptom). Send the handoff
	// mail so the post-compact session sees it, then return so compaction
	// proceeds normally.
	opts := handoffOptions{forceSkipRestart: hookEvent == "PreCompact"}

	outcome := doHandoffWithOptions(store, rec, dops, persistRestart, current.display, current.sessionName, args, stdout, stderr, opts)
	if outcome.code != 0 {
		return outcome.code
	}
	if !outcome.restartRequested {
		return 0
	}

	// Block until the controller tears the process tree down, with a bounded
	// timeout so a stuck or unreachable controller can't hang the caller
	// (e.g. PreCompact) forever. A timeout means the controller didn't act on
	// the persisted restart_requested flag in time; the flag stays set so the
	// next reconcile cycle can still pick it up.
	<-time.After(handoffRestartWait)
	fmt.Fprintf(stderr, "gc handoff: controller did not stop session within %s; exiting hook (restart flag remains set)\n", handoffRestartWait) //nolint:errcheck // best-effort stderr
	return 1
}

// cmdHandoffRemote sends handoff mail to a remote session and stops the target
// only when the controller can restart it. Returns immediately.
func cmdHandoffRemote(args []string, target string, stdout, stderr io.Writer) int {
	targetInfo, err := resolveSessionRuntimeTarget(target, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc handoff: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	store, code := openCityStore(stderr, "gc handoff")
	if store == nil {
		return code
	}
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc handoff: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, _ := loadCityConfig(cityPath, stderr)
	sender, ok := resolveDefaultMailSenderForCommand(cityPath, cfg, store, stderr, "gc handoff")
	if !ok {
		return 1
	}

	sp := newSessionProvider()
	rec := openCityRecorder(stderr)
	return doHandoffRemote(store, rec, sp, targetInfo.sessionName, targetInfo.display, sender, args, stdout, stderr)
}

func sessionRestartPersister(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, target string) func() error {
	if store == nil {
		return nil
	}
	return func() error {
		handle, err := workerHandleForSessionTargetWithConfig(cityPath, store, sp, cfg, target)
		if err != nil {
			return err
		}
		return handle.Reset(context.Background())
	}
}

type handoffOutcome struct {
	code             int
	restartRequested bool
}

// handoffOptions holds optional parameters for doHandoffWithOptions. Zero
// value preserves the historical doHandoffWithOutcome behavior.
type handoffOptions struct {
	// forceSkipRestart, when true, prevents the restart-requested flag from
	// being set regardless of session classification. Use for callers that
	// know killing the session here would lose data — e.g. PreCompact, where
	// Claude is mid-compaction and the kill discards the in-flight summary.
	forceSkipRestart bool
}

// doHandoff sends a handoff mail to self and requests restart when the
// controller can restart the current session. Testable: does not block.
func doHandoff(store beads.Store, rec events.Recorder, dops drainOps, persistRestart func() error,
	sessionAddress, sessionName string, args []string, stdout, stderr io.Writer,
) int {
	return doHandoffWithOutcome(store, rec, dops, persistRestart, sessionAddress, sessionName, args, stdout, stderr).code
}

func doHandoffWithOutcome(store beads.Store, rec events.Recorder, dops drainOps, persistRestart func() error,
	sessionAddress, sessionName string, args []string, stdout, stderr io.Writer,
) handoffOutcome {
	return doHandoffWithOptions(store, rec, dops, persistRestart, sessionAddress, sessionName, args, stdout, stderr, handoffOptions{})
}

func doHandoffWithOptions(store beads.Store, rec events.Recorder, dops drainOps, persistRestart func() error,
	sessionAddress, sessionName string, args []string, stdout, stderr io.Writer, opts handoffOptions,
) handoffOutcome {
	subject := args[0]
	var message string
	if len(args) > 1 {
		message = args[1]
	}

	b, err := store.Create(beads.Bead{
		Title:       subject,
		Description: message,
		Type:        "message",
		Assignee:    sessionAddress,
		From:        sessionAddress,
		Labels:      []string{"thread:" + handoffThreadID()},
	})
	if err != nil {
		fmt.Fprintf(stderr, "gc handoff: creating mail: %v\n", err) //nolint:errcheck // best-effort stderr
		return handoffOutcome{code: 1}
	}
	rec.Record(events.Event{
		Type:    events.MailSent,
		Actor:   sessionAddress,
		Subject: b.ID,
		Message: sessionAddress,
		Payload: mailEventPayload(nil),
	})

	restartable, err := sessionRestartableByController(store, sessionName)
	if err != nil {
		fmt.Fprintf(stderr, "gc handoff: checking session type: %v\n", err) //nolint:errcheck // best-effort stderr
		return handoffOutcome{code: 1}
	}
	// PreCompact (and other callers that explicitly forbid the restart kill)
	// override the bead-derived classification — even an always-mode session
	// must not be torn down here, because Claude's compaction is in flight
	// and the kill discards the in-flight summary.
	if opts.forceSkipRestart {
		restartable = false
	}
	// Non-restartable sessions: on-demand configured-named beads, pool-managed
	// ephemeral beads (which are user-attended through tmux), and any caller
	// with forceSkipRestart set. Preserve the handoff mail; skip both restart
	// flags. Regression guard: gastownhall/gascity#744.
	if !restartable {
		if err := clearRestartRequest(store, dops, sessionName); err != nil {
			fmt.Fprintf(stderr, "gc handoff: clearing stale restart request: %v\n", err) //nolint:errcheck // best-effort stderr
			return handoffOutcome{code: 1, restartRequested: false}
		}
		msg := "named session; restart skipped"
		if opts.forceSkipRestart {
			msg = "PreCompact context; restart skipped to preserve in-flight context"
		}
		fmt.Fprintf(stdout, "Handoff: sent mail %s (%s).\n", b.ID, msg) //nolint:errcheck // best-effort stdout
		return handoffOutcome{code: 0, restartRequested: false}
	}

	if err := dops.setRestartRequested(sessionName); err != nil {
		fmt.Fprintf(stderr, "gc handoff: setting restart flag: %v\n", err) //nolint:errcheck // best-effort stderr
		return handoffOutcome{code: 1}
	}
	// Also persist the request through the worker boundary so it survives
	// tmux session death. Non-fatal: the runtime flag above is primary.
	if persistRestart != nil {
		if err := persistRestart(); err != nil {
			fmt.Fprintf(stderr, "gc handoff: setting bead restart flag: %v\n", err) //nolint:errcheck // best-effort stderr
		}
	}
	rec.Record(events.Event{
		Type:    events.SessionDraining,
		Actor:   sessionAddress,
		Subject: sessionAddress,
		Message: "handoff",
	})

	fmt.Fprintf(stdout, "Handoff: sent mail %s, requesting restart...\n", b.ID) //nolint:errcheck // best-effort stdout
	return handoffOutcome{code: 0, restartRequested: true}
}

func sessionRestartableByController(store beads.Store, sessionName string) (bool, error) {
	if store == nil || sessionName == "" {
		return true, nil
	}
	id, err := resolveSessionID(store, sessionName)
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			return true, nil
		}
		return false, fmt.Errorf("resolving session %q: %w", sessionName, err)
	}
	b, err := store.Get(id)
	if err != nil {
		return false, fmt.Errorf("loading session %q: %w", id, err)
	}
	if isNamedSessionBead(b) {
		return namedSessionMode(b) == "always", nil
	}
	// Pool-managed (and otherwise ephemeral) session beads are commonly
	// attached to a human tmux/iTerm window via `gc session attach`. Killing
	// them mid-PreCompact discards in-flight context (the compaction summary
	// is never written) and crashes the visible terminal even though the
	// controller will eventually spin up a fresh pool slot. Treat them like
	// on-demand named sessions for handoff purposes: keep the handoff mail,
	// skip the restart kill. Regression: pool-managed mayor crashed on every
	// PreCompact because the #744 fix only protected configured_named beads.
	if isPoolManagedSessionBead(b) {
		return false, nil
	}
	return true, nil
}

func clearRestartRequest(store beads.Store, dops drainOps, sessionName string) error {
	if sessionName == "" {
		return nil
	}
	var errs []error
	if dops != nil {
		if err := dops.clearRestartRequested(sessionName); err != nil {
			errs = append(errs, fmt.Errorf("clearing runtime restart flag: %w", err))
		}
	}
	if store == nil {
		return errors.Join(errs...)
	}
	id, err := resolveSessionID(store, sessionName)
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			return errors.Join(errs...)
		}
		errs = append(errs, fmt.Errorf("resolving session %q: %w", sessionName, err))
		return errors.Join(errs...)
	}
	if err := store.SetMetadataBatch(id, map[string]string{
		"restart_requested":          "",
		"continuation_reset_pending": "",
	}); err != nil {
		errs = append(errs, fmt.Errorf("clearing bead restart flag: %w", err))
	}
	return errors.Join(errs...)
}

// doHandoffRemote sends handoff mail to a remote session and stops the target
// only when the controller can restart it.
func doHandoffRemote(store beads.Store, rec events.Recorder, sp runtime.Provider,
	sessionName, targetAddress, sender string, args []string, stdout, stderr io.Writer,
) int {
	subject := args[0]
	var message string
	if len(args) > 1 {
		message = args[1]
	}

	// Send mail to target.
	b, err := store.Create(beads.Bead{
		Title:       subject,
		Description: message,
		Type:        "message",
		Assignee:    targetAddress,
		From:        sender,
		Labels:      []string{"thread:" + handoffThreadID()},
	})
	if err != nil {
		fmt.Fprintf(stderr, "gc handoff: creating mail: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	rec.Record(events.Event{
		Type:    events.MailSent,
		Actor:   sender,
		Subject: b.ID,
		Message: targetAddress,
		Payload: mailEventPayload(nil),
	})

	restartable, err := sessionRestartableByController(store, sessionName)
	if err != nil {
		fmt.Fprintf(stderr, "gc handoff: checking session type: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !restartable {
		if err := clearRestartRequest(store, newDrainOps(sp), sessionName); err != nil {
			fmt.Fprintf(stderr, "gc handoff: clearing stale restart request: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		fmt.Fprintf(stdout, "Handoff: sent mail %s to %s (named session; kill skipped because the controller cannot restart it)\n", b.ID, targetAddress) //nolint:errcheck // best-effort stdout
		return 0
	}

	// Kill target session (reconciler restarts it).
	running, err := workerSessionTargetRunningWithConfig("", store, sp, nil, sessionName)
	if err != nil {
		fmt.Fprintf(stderr, "gc handoff: observing %s: %v\n", targetAddress, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !running {
		fmt.Fprintf(stdout, "Handoff: sent mail %s to %s (session not running; will be delivered on next start)\n", b.ID, targetAddress) //nolint:errcheck // best-effort stdout
		return 0
	}
	if err := workerKillSessionTargetWithConfig("", store, sp, nil, sessionName); err != nil {
		fmt.Fprintf(stderr, "gc handoff: killing %s: %v\n", targetAddress, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	rec.Record(events.Event{
		Type:    events.SessionStopped,
		Actor:   sender,
		Subject: targetAddress,
		Message: "handoff",
	})

	fmt.Fprintf(stdout, "Handoff: sent mail %s to %s, killed session (reconciler will restart)\n", b.ID, targetAddress) //nolint:errcheck // best-effort stdout
	return 0
}

// handoffThreadID generates a unique thread ID for handoff messages.
func handoffThreadID() string {
	b := make([]byte, 6)
	rand.Read(b) //nolint:errcheck
	return fmt.Sprintf("thread-%x", b)
}
