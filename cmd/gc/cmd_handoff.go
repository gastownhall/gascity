package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os/signal"
	"syscall"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/spf13/cobra"
)

func newHandoffCmd(stdout, stderr io.Writer) *cobra.Command {
	var target string
	var auto bool
	var hookFormat string
	cmd := &cobra.Command{
		Use:   "handoff [subject] [message]",
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

Under normal operation the controller stops controller-restartable
self-handoff sessions before this command returns. If the controller does not
act within a bounded timeout, gc handoff exits 1 with a diagnostic instead of
blocking indefinitely. If interrupted, the restart request remains set for the
controller to process on its next reconcile tick.

Auto handoff (--auto): sends mail to self and returns without requesting a
restart. This is for PreCompact hooks, where the provider is already managing
the context compaction lifecycle.

Remote handoff (--target): sends mail to a target session. If the target is
controller-restartable, kills it so the reconciler restarts it with the handoff
mail waiting. For on-demand configured named targets, sends mail and returns
without killing the session.

For controller-restartable targets, equivalent to:

  gc mail send <target> <subject> [message]
  gc session kill <target>

Self-handoff requires session context (GC_ALIAS or GC_SESSION_ID, plus
GC_SESSION_NAME and city context env). Remote handoff accepts a session alias
or ID. Subject is required unless --auto is set.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if auto {
				return cobra.MaximumNArgs(2)(cmd, args)
			}
			return cobra.RangeArgs(1, 2)(cmd, args)
		},
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdHandoff(args, target, auto, hookFormat, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&target, "target", "", "Remote session alias or ID to handoff (kills only controller-restartable sessions)")
	cmd.Flags().BoolVar(&auto, "auto", false, "Send handoff mail without requesting restart (for PreCompact hooks)")
	cmd.Flags().StringVar(&hookFormat, "hook-format", "", "format hook output for a provider")
	return cmd
}

func cmdHandoff(args []string, target string, auto bool, hookFormat string, stdout, stderr io.Writer) int {
	if target != "" {
		if auto {
			fmt.Fprintln(stderr, "gc handoff: --auto cannot be used with --target") //nolint:errcheck // best-effort stderr
			return 1
		}
		return cmdHandoffRemote(args, target, stdout, stderr)
	}

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
	rec := openCityRecorderAt(current.cityPath, stderr)
	if auto {
		return doHandoffAuto(store, rec, current.display, args, hookFormat, stdout, stderr)
	}

	sp := newSessionProvider()
	dops := newDrainOps(sp)
	cfg, _ := loadCityConfig(current.cityPath, stderr)
	persistRestart := sessionRestartPersister(current.cityPath, store, sp, cfg, current.sessionName)

	outcome := doHandoffWithOutcome(store, rec, dops, persistRestart, current.display, current.sessionName, args, stdout, stderr)
	if outcome.code != 0 {
		return outcome.code
	}
	if !outcome.restartRequested {
		return 0
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return waitForControllerRestart(sigCtx, dops, current.sessionName, "gc handoff",
		controllerRestartPollInterval, controllerRestartTimeout(cfg), stderr)
}

// cmdHandoffRemote sends handoff mail to a remote session and kills its runtime.
// Returns immediately (non-blocking). The reconciler restarts the target.
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
	b, ok := createHandoffMail(store, rec, sessionAddress, sessionAddress, args, "HANDOFF: context cycle", stderr)
	if !ok {
		return handoffOutcome{code: 1}
	}

	restartable, err := sessionRestartableByController(store, sessionName)
	if err != nil {
		fmt.Fprintf(stderr, "gc handoff: checking session type: %v\n", err) //nolint:errcheck // best-effort stderr
		return handoffOutcome{code: 1}
	}
	if !restartable {
		if err := clearRestartRequest(store, dops, sessionName); err != nil {
			fmt.Fprintf(stderr, "gc handoff: clearing stale restart request: %v\n", err) //nolint:errcheck // best-effort stderr
			return handoffOutcome{code: 1}
		}
		fmt.Fprintf(stdout, "Handoff: sent mail %s (named session; restart skipped).\n", b.ID) //nolint:errcheck // best-effort stdout
		return handoffOutcome{code: 0}
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

// doHandoffAuto sends handoff mail to self without requesting restart.
func doHandoffAuto(store beads.Store, rec events.Recorder, sessionAddress string, args []string, hookFormat string, stdout, stderr io.Writer) int {
	b, ok := createHandoffMail(store, rec, sessionAddress, sessionAddress, args, "context cycle", stderr)
	if !ok {
		return 1
	}
	message := fmt.Sprintf("Handoff: sent auto mail %s (restart skipped).\n", b.ID)
	if err := writeProviderHookContextForEvent(stdout, hookFormat, "PreCompact", message); err != nil {
		fmt.Fprintf(stderr, "gc handoff: writing hook output: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}

func createHandoffMail(store beads.Store, rec events.Recorder, senderAddress, recipientAddress string, args []string, defaultSubject string, stderr io.Writer) (beads.Bead, bool) {
	subject := defaultSubject
	if len(args) > 0 {
		subject = args[0]
	}
	var message string
	if len(args) > 1 {
		message = args[1]
	}
	metadata, err := mailSenderRouteMetadata(store, senderAddress)
	if err != nil {
		fmt.Fprintf(stderr, "gc handoff: resolving sender route: %v\n", err) //nolint:errcheck // best-effort stderr
		return beads.Bead{}, false
	}
	senderDisplay := mailSenderDisplayFromMetadata(senderAddress, metadata)

	b, err := store.Create(beads.Bead{
		Title:       subject,
		Description: message,
		Type:        "message",
		Assignee:    recipientAddress,
		From:        senderDisplay,
		Labels:      []string{"thread:" + handoffThreadID()},
		Metadata:    metadata,
	})
	if err != nil {
		fmt.Fprintf(stderr, "gc handoff: creating mail: %v\n", err) //nolint:errcheck // best-effort stderr
		return beads.Bead{}, false
	}
	rec.Record(events.Event{
		Type:    events.MailSent,
		Actor:   senderDisplay,
		Subject: b.ID,
		Message: recipientAddress,
		Payload: mailEventPayload(nil),
	})
	return b, true
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
	if !isNamedSessionBead(b) {
		return true, nil
	}
	return namedSessionMode(b) == "always", nil
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

// doHandoffRemote sends handoff mail to a remote session and kills its runtime.
// Non-blocking: returns immediately after killing the session.
func doHandoffRemote(store beads.Store, rec events.Recorder, sp runtime.Provider,
	sessionName, targetAddress, sender string, args []string, stdout, stderr io.Writer,
) int {
	b, ok := createHandoffMail(store, rec, sender, targetAddress, args, "HANDOFF: context cycle", stderr)
	if !ok {
		return 1
	}

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
