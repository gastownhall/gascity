package main

import (
	"crypto/rand"
	"fmt"
	"io"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/spf13/cobra"
)

func newHandoffCmd(stdout, stderr io.Writer) *cobra.Command {
	var target string
	cmd := &cobra.Command{
		Use:   "handoff <subject> [message]",
		Short: "Send handoff mail and restart this session",
		Long: `Convenience command for context handoff.

Self-handoff (default): sends mail to self and blocks until controller
restarts the session. Equivalent to:

  gc mail send $GC_ALIAS <subject> [message]
  gc runtime request-restart

Remote handoff (--target): sends mail to a target session and kills its
session. The reconciler restarts it with the handoff mail waiting.
Returns immediately. Equivalent to:

  gc mail send <target> <subject> [message]
  gc session kill <target>

Self-handoff requires session context (GC_ALIAS or GC_SESSION_ID, plus
GC_SESSION_NAME and GC_CITY). Remote handoff accepts a session alias or ID.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdHandoff(args, target, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&target, "target", "", "Remote session alias or ID to handoff (sends mail + kills session)")
	return cmd
}

func cmdHandoff(args []string, target string, stdout, stderr io.Writer) int {
	if target != "" {
		return cmdHandoffRemote(args, target, stdout, stderr)
	}

	current, err := currentSessionRuntimeTarget()
	if err != nil {
		fmt.Fprintf(stderr, "gc handoff: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	readDoltPort(current.cityPath)
	store, err := openCityStoreAt(current.cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc handoff: %v\n", err)                    //nolint:errcheck // best-effort stderr
		fmt.Fprintln(stderr, "hint: run \"gc doctor\" for diagnostics") //nolint:errcheck // best-effort stderr
		return 1
	}
	sp := newSessionProvider()
	dops := newDrainOps(sp)
	rec := openCityRecorderAt(current.cityPath, stderr)

	code := doHandoff(store, rec, dops, current.display, current.sessionName, args, stdout, stderr)
	if code != 0 {
		return code
	}

	// Block forever. The controller will kill the entire process tree.
	select {}
}

// cmdHandoffRemote sends handoff mail to a remote session and kills its runtime.
// Returns immediately (non-blocking). The reconciler restarts the target.
func cmdHandoffRemote(args []string, target string, stdout, stderr io.Writer) int {
	targetInfo, err := resolveSessionRuntimeTarget(target)
	if err != nil {
		fmt.Fprintf(stderr, "gc handoff: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	store, code := openCityStore(stderr, "gc handoff")
	if store == nil {
		return code
	}

	sp := newSessionProvider()
	rec := openCityRecorder(stderr)
	return doHandoffRemote(store, rec, sp, targetInfo.sessionName, targetInfo.display, defaultMailIdentity(), args, stdout, stderr)
}

// doHandoff sends a handoff mail to self and sets the restart-requested flag.
// Testable: does not block.
func doHandoff(store beads.Store, rec events.Recorder, dops drainOps,
	sessionAddress, sessionName string, args []string, stdout, stderr io.Writer,
) int {
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
		Labels:      []string{"gc:message", "thread:" + handoffThreadID()},
	})
	if err != nil {
		fmt.Fprintf(stderr, "gc handoff: creating mail: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	rec.Record(events.Event{
		Type:    events.MailSent,
		Actor:   sessionAddress,
		Subject: b.ID,
		Message: sessionAddress,
		Payload: mailEventPayload(nil),
	})

	if err := dops.setRestartRequested(sessionName); err != nil {
		fmt.Fprintf(stderr, "gc handoff: setting restart flag: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	// Also persist the flag in bead metadata so it survives tmux session death.
	if err := setBeadRestartRequested(store, sessionName); err != nil {
		fmt.Fprintf(stderr, "gc handoff: setting bead restart flag: %v\n", err) //nolint:errcheck // best-effort stderr
		// Non-fatal: the tmux flag is already set as primary.
	}
	rec.Record(events.Event{
		Type:    events.SessionDraining,
		Actor:   sessionAddress,
		Subject: sessionAddress,
		Message: "handoff",
	})

	fmt.Fprintf(stdout, "Handoff: sent mail %s, requesting restart...\n", b.ID) //nolint:errcheck // best-effort stdout
	return 0
}

// doHandoffRemote sends handoff mail to a remote session and kills its runtime.
// Non-blocking: returns immediately after killing the session.
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
		Labels:      []string{"gc:message", "thread:" + handoffThreadID()},
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

	// Kill target session (reconciler restarts it).
	if !sp.IsRunning(sessionName) {
		fmt.Fprintf(stdout, "Handoff: sent mail %s to %s (session not running; will be delivered on next start)\n", b.ID, targetAddress) //nolint:errcheck // best-effort stdout
		return 0
	}
	if err := sp.Stop(sessionName); err != nil {
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
