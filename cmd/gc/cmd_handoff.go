package main

import (
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/spf13/cobra"
)

func newHandoffCmd(stdout, stderr io.Writer) *cobra.Command {
	var target string
	cmd := &cobra.Command{
		Use:   "handoff <subject> [message]",
		Short: "Send handoff mail and restart agent session",
		Long: `Convenience command for context handoff.

Self-handoff (default): sends mail to self and blocks until controller
restarts the session. Equivalent to:

  gc mail send $GC_AGENT <subject> [message]
  gc runtime request-restart

Remote handoff (--target): sends mail to target agent and kills its
session. The reconciler restarts it with the handoff mail waiting.
Returns immediately. Equivalent to:

  gc mail send <target> <subject> [message]
  gc session kill <target>

Self-handoff requires agent context (GC_AGENT/GC_CITY env vars).
Remote handoff can be run from any context with access to the city.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdHandoff(args, target, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&target, "target", "", "Remote agent to handoff (sends mail + kills session)")
	return cmd
}

func cmdHandoff(args []string, target string, stdout, stderr io.Writer) int {
	if target != "" {
		return cmdHandoffRemote(args, target, stdout, stderr)
	}

	agentName := os.Getenv("GC_AGENT")
	cityDir := os.Getenv("GC_CITY")
	if agentName == "" || cityDir == "" {
		fmt.Fprintln(stderr, "gc handoff: not in agent context (GC_AGENT/GC_CITY not set)") //nolint:errcheck // best-effort stderr
		return 1
	}

	store, code := openCityStore(stderr, "gc handoff")
	if store == nil {
		return code
	}

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		fmt.Fprintf(stderr, "gc handoff: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cityName := cfg.Workspace.Name
	if cityName == "" {
		cityName = filepath.Base(cityDir)
	}
	sn := sessionName(store, cityName, agentName, cfg.Workspace.SessionTemplate)
	sp := newSessionProvider()
	dops := newDrainOps(sp)
	rec := openCityRecorder(stderr)

	code = doHandoff(store, rec, dops, agentName, sn, args, stdout, stderr)
	if code != 0 {
		return code
	}

	// Block forever. The controller will kill the entire process tree.
	select {}
}

// cmdHandoffRemote sends handoff mail to a remote agent and kills its session.
// Returns immediately (non-blocking). The reconciler restarts the target.
func cmdHandoffRemote(args []string, target string, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc handoff: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc handoff: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Resolve target agent.
	found, ok := resolveAgentIdentity(cfg, target, currentRigContext(cfg))
	if !ok {
		fmt.Fprintln(stderr, agentNotFoundMsg("gc handoff", target, cfg)) //nolint:errcheck // best-effort stderr
		return 1
	}
	targetName := found.QualifiedName()

	store, code := openCityStore(stderr, "gc handoff")
	if store == nil {
		return code
	}

	cityName := cfg.Workspace.Name
	if cityName == "" {
		cityName = filepath.Base(cityPath)
	}
	sp := newSessionProvider()
	rec := openCityRecorder(stderr)

	sender := os.Getenv("GC_AGENT")
	if sender == "" {
		sender = "human"
	}

	sn := lookupSessionNameOrLegacy(store, cityName, targetName, cfg.Workspace.SessionTemplate)
	return doHandoffRemote(store, rec, sp, sn, targetName, sender, args, stdout, stderr)
}

// doHandoff sends a handoff mail to self and sets the restart-requested flag.
// Testable: does not block.
func doHandoff(store beads.Store, rec events.Recorder, dops drainOps,
	agentName, sn string, args []string, stdout, stderr io.Writer,
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
		Assignee:    agentName,
		From:        agentName,
		Labels:      []string{"gc:message", "thread:" + handoffThreadID()},
	})
	if err != nil {
		fmt.Fprintf(stderr, "gc handoff: creating mail: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	rec.Record(events.Event{
		Type:    events.MailSent,
		Actor:   agentName,
		Subject: b.ID,
		Message: agentName,
		Payload: mailEventPayload(nil),
	})

	if err := dops.setRestartRequested(sn); err != nil {
		fmt.Fprintf(stderr, "gc handoff: setting restart flag: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	rec.Record(events.Event{
		Type:    events.SessionDraining,
		Actor:   agentName,
		Subject: agentName,
		Message: "handoff",
	})

	fmt.Fprintf(stdout, "Handoff: sent mail %s, requesting restart...\n", b.ID) //nolint:errcheck // best-effort stdout
	return 0
}

// doHandoffRemote sends handoff mail to a remote agent and kills its session.
// Non-blocking: returns immediately after killing the session.
func doHandoffRemote(store beads.Store, rec events.Recorder, sp runtime.Provider,
	sessionName, targetName, sender string, args []string, stdout, stderr io.Writer,
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
		Assignee:    targetName,
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
		Message: targetName,
		Payload: mailEventPayload(nil),
	})

	// Kill target session (reconciler restarts it).
	if !sp.IsRunning(sessionName) {
		fmt.Fprintf(stdout, "Handoff: sent mail %s to %s (session not running; will be delivered on next start)\n", b.ID, targetName) //nolint:errcheck // best-effort stdout
		return 0
	}
	if err := sp.Stop(sessionName); err != nil {
		fmt.Fprintf(stderr, "gc handoff: killing %s: %v\n", targetName, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	rec.Record(events.Event{
		Type:    events.SessionStopped,
		Actor:   sender,
		Subject: targetName,
		Message: "handoff",
	})

	fmt.Fprintf(stdout, "Handoff: sent mail %s to %s, killed session (reconciler will restart)\n", b.ID, targetName) //nolint:errcheck // best-effort stdout
	return 0
}

// handoffThreadID generates a unique thread ID for handoff messages.
func handoffThreadID() string {
	b := make([]byte, 6)
	rand.Read(b) //nolint:errcheck
	return fmt.Sprintf("thread-%x", b)
}
