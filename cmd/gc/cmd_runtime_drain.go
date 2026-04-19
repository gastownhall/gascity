package main

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/spf13/cobra"
)

// drainOps abstracts drain signal operations for testability.
type drainOps interface {
	setDrain(sessionName string) error
	clearDrain(sessionName string) error
	isDraining(sessionName string) (bool, error)
	drainStartTime(sessionName string) (time.Time, error)
	setDrainAck(sessionName string) error
	isDrainAcked(sessionName string) (bool, error)
	setRestartRequested(sessionName string) error
	isRestartRequested(sessionName string) (bool, error)
	clearRestartRequested(sessionName string) error
	setDriftRestart(sessionName string) error
	isDriftRestart(sessionName string) (bool, error)
	clearDriftRestart(sessionName string) error
}

// providerDrainOps implements drainOps using runtime.Provider metadata.
type providerDrainOps struct {
	sp runtime.Provider
}

func (o *providerDrainOps) setDrain(sessionName string) error {
	return o.sp.SetMeta(sessionName, "GC_DRAIN", strconv.FormatInt(time.Now().Unix(), 10))
}

func (o *providerDrainOps) clearDrain(sessionName string) error {
	_ = o.sp.RemoveMeta(sessionName, "GC_DRAIN_ACK")
	_ = o.sp.RemoveMeta(sessionName, reconcilerDrainAckSourceKey)
	_ = o.sp.RemoveMeta(sessionName, reconcilerDrainAckReasonKey)
	_ = o.sp.RemoveMeta(sessionName, reconcilerDrainAckGenerationKey)
	return o.sp.RemoveMeta(sessionName, "GC_DRAIN")
}

func (o *providerDrainOps) isDraining(sessionName string) (bool, error) {
	val, err := o.sp.GetMeta(sessionName, "GC_DRAIN")
	if err != nil {
		return false, nil // can't read = not draining
	}
	return val != "", nil
}

func (o *providerDrainOps) drainStartTime(sessionName string) (time.Time, error) {
	val, err := o.sp.GetMeta(sessionName, "GC_DRAIN")
	if err != nil {
		return time.Time{}, fmt.Errorf("reading GC_DRAIN: %w", err)
	}
	if val == "" {
		return time.Time{}, fmt.Errorf("GC_DRAIN not set")
	}
	unix, err := strconv.ParseInt(val, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing GC_DRAIN timestamp %q: %w", val, err)
	}
	return time.Unix(unix, 0), nil
}

func (o *providerDrainOps) setDrainAck(sessionName string) error {
	_ = o.sp.RemoveMeta(sessionName, reconcilerDrainAckReasonKey)
	_ = o.sp.RemoveMeta(sessionName, reconcilerDrainAckGenerationKey)
	if err := o.sp.SetMeta(sessionName, reconcilerDrainAckSourceKey, drainAckSourceAgentValue); err != nil {
		return err
	}
	return o.sp.SetMeta(sessionName, "GC_DRAIN_ACK", "1")
}

func (o *providerDrainOps) isDrainAcked(sessionName string) (bool, error) {
	val, err := o.sp.GetMeta(sessionName, "GC_DRAIN_ACK")
	if err != nil {
		return false, nil
	}
	return val == "1", nil
}

func (o *providerDrainOps) setRestartRequested(sessionName string) error {
	return o.sp.SetMeta(sessionName, "GC_RESTART_REQUESTED", strconv.FormatInt(time.Now().Unix(), 10))
}

func (o *providerDrainOps) isRestartRequested(sessionName string) (bool, error) {
	val, err := o.sp.GetMeta(sessionName, "GC_RESTART_REQUESTED")
	if err != nil {
		return false, nil
	}
	return val != "", nil
}

func (o *providerDrainOps) clearRestartRequested(sessionName string) error {
	err := o.sp.RemoveMeta(sessionName, "GC_RESTART_REQUESTED")
	if runtime.IsSessionGone(err) {
		return nil
	}
	return err
}

func (o *providerDrainOps) setDriftRestart(sessionName string) error {
	return o.sp.SetMeta(sessionName, "GC_DRIFT_RESTART", "1")
}

func (o *providerDrainOps) isDriftRestart(sessionName string) (bool, error) {
	val, err := o.sp.GetMeta(sessionName, "GC_DRIFT_RESTART")
	if err != nil {
		return false, nil
	}
	return val == "1", nil
}

func (o *providerDrainOps) clearDriftRestart(sessionName string) error {
	return o.sp.RemoveMeta(sessionName, "GC_DRIFT_RESTART")
}

// newDrainOps creates a drainOps from a runtime.Provider.
func newDrainOps(sp runtime.Provider) drainOps {
	return &providerDrainOps{sp: sp}
}

// ---------------------------------------------------------------------------
// gc runtime drain <name>
// ---------------------------------------------------------------------------

func newRuntimeDrainCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "drain <name>",
		Short: "Signal a session to drain (wind down gracefully)",
		Long: `Signal a session to drain — wind down its current work gracefully.

Sets a GC_DRAIN metadata flag on the session. The agent should check
for drain status periodically (via "gc runtime drain-check") and finish
its current task before exiting. Pass a session alias or ID. Use
"gc runtime undrain" to cancel.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdRuntimeDrain(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func cmdRuntimeDrain(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc runtime drain: missing session alias or ID") //nolint:errcheck // best-effort stderr
		return 1
	}
	target, err := resolveSessionRuntimeTarget(args[0], stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime drain: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	sp := newSessionProvider()
	dops := newDrainOps(sp)
	rec := openCityRecorder(stderr)
	return doRuntimeDrain(dops, sp, rec, target.display, target.sessionName, stdout, stderr)
}

// doRuntimeDrain sets the drain signal on a session.
func doRuntimeDrain(dops drainOps, sp runtime.Provider, rec events.Recorder,
	targetName, sn string, stdout, stderr io.Writer,
) int {
	running, err := workerSessionTargetRunningWithConfig("", nil, sp, nil, sn)
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime drain: observing %q: %v\n", targetName, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !running {
		fmt.Fprintf(stderr, "gc runtime drain: session %q is not running\n", targetName) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := dops.setDrain(sn); err != nil {
		fmt.Fprintf(stderr, "gc runtime drain: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	rec.Record(events.Event{
		Type:    events.SessionDraining,
		Actor:   eventActor(),
		Subject: targetName,
	})
	fmt.Fprintf(stdout, "Draining session '%s'\n", targetName) //nolint:errcheck // best-effort stdout
	return 0
}

// ---------------------------------------------------------------------------
// gc runtime undrain <name>
// ---------------------------------------------------------------------------

func newRuntimeUndrainCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "undrain <name>",
		Short: "Cancel drain on a session",
		Long: `Cancel a pending drain signal on a session.

Clears the GC_DRAIN and GC_DRAIN_ACK metadata flags, allowing the
session to continue normal operation. Pass a session alias or ID.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdRuntimeUndrain(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func cmdRuntimeUndrain(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc runtime undrain: missing session alias or ID") //nolint:errcheck // best-effort stderr
		return 1
	}
	target, err := resolveSessionRuntimeTarget(args[0], stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime undrain: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	sp := newSessionProvider()
	dops := newDrainOps(sp)
	rec := openCityRecorder(stderr)
	return doRuntimeUndrain(dops, sp, rec, target.display, target.sessionName, stdout, stderr)
}

// doRuntimeUndrain clears the drain signal on a session.
func doRuntimeUndrain(dops drainOps, sp runtime.Provider, rec events.Recorder,
	targetName, sn string, stdout, stderr io.Writer,
) int {
	running, err := workerSessionTargetRunningWithConfig("", nil, sp, nil, sn)
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime undrain: observing %q: %v\n", targetName, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !running {
		fmt.Fprintf(stderr, "gc runtime undrain: session %q is not running\n", targetName) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := dops.clearDrain(sn); err != nil {
		fmt.Fprintf(stderr, "gc runtime undrain: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	rec.Record(events.Event{
		Type:    events.SessionUndrained,
		Actor:   eventActor(),
		Subject: targetName,
	})
	fmt.Fprintf(stdout, "Undrained session '%s'\n", targetName) //nolint:errcheck // best-effort stdout
	return 0
}

// ---------------------------------------------------------------------------
// gc runtime drain-check
// ---------------------------------------------------------------------------

func newRuntimeDrainCheckCmd(stdout, stderr io.Writer) *cobra.Command {
	_ = stdout // drain-check is silent on stdout
	return &cobra.Command{
		Use:   "drain-check [name]",
		Short: "Check if a session is draining (exit 0 = draining)",
		Long: `Check if a session is currently draining.

Returns exit code 0 if draining, 1 if not. Designed for use in
conditionals: "if gc runtime drain-check; then finish-up; fi". Without
arguments, uses the current session context.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdRuntimeDrainCheck(args, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func cmdRuntimeDrainCheck(args []string, stderr io.Writer) int {
	if len(args) > 0 {
		target, err := resolveSessionRuntimeTarget(args[0], stderr)
		if err != nil {
			fmt.Fprintf(stderr, "gc runtime drain-check: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1                                                 // silent — same as current "not draining" behavior
		}
		sp := newSessionProvider()
		dops := newDrainOps(sp)
		return doRuntimeDrainCheck(dops, target.sessionName)
	}

	current, err := currentSessionRuntimeTarget()
	if err != nil {
		return 1 // not in agent context → not draining
	}
	sp := newSessionProvider()
	dops := newDrainOps(sp)
	return doRuntimeDrainCheck(dops, current.sessionName)
}

// doRuntimeDrainCheck returns 0 if the session is draining, 1 otherwise.
// Silent on stdout — designed for `if gc runtime drain-check; then ...`.
func doRuntimeDrainCheck(dops drainOps, sn string) int {
	draining, err := dops.isDraining(sn)
	if err != nil || !draining {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// gc runtime drain-ack
// ---------------------------------------------------------------------------

func newRuntimeDrainAckCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "drain-ack [name]",
		Short: "Acknowledge drain — signal the controller to stop this session",
		Long: `Acknowledge a drain signal — tell the controller to stop this session.

Sets GC_DRAIN_ACK metadata on the session. The controller will stop
the session on its next reconcile tick. Call this after the session has
finished its current work in response to a drain signal.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdRuntimeDrainAck(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func cmdRuntimeDrainAck(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		target, err := resolveSessionRuntimeTarget(args[0], stderr)
		if err != nil {
			fmt.Fprintf(stderr, "gc runtime drain-ack: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		sp := newSessionProvider()
		dops := newDrainOps(sp)
		return doRuntimeDrainAck(dops, target.sessionName, stdout, stderr)
	}

	current, err := currentSessionRuntimeTarget()
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime drain-ack: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	sp := newSessionProvider()
	dops := newDrainOps(sp)
	return doRuntimeDrainAck(dops, current.sessionName, stdout, stderr)
}

// ---------------------------------------------------------------------------
// gc runtime request-restart
// ---------------------------------------------------------------------------

func newRuntimeRequestRestartCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "request-restart",
		Short: "Request controller restart this session (blocks until killed)",
		Long: `Signal the controller to stop and restart this session.

Sets GC_RESTART_REQUESTED metadata on the session, then blocks forever.
The controller will stop the session on its next reconcile tick and
restart it fresh. The blocking prevents the agent from consuming more
context while waiting.

For on-demand configured named sessions, the controller cannot restart the
user-attended process. In that case this command reports that restart was
skipped and returns without blocking. No session.draining event is emitted
when restart is skipped.

This command is designed to be called from within a session context.
It emits a session.draining event before blocking.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdRuntimeRequestRestart(stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func cmdRuntimeRequestRestart(stdout, stderr io.Writer) int {
	current, err := currentSessionRuntimeTarget()
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime request-restart: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	sp := newSessionProvider()
	dops := newDrainOps(sp)
	store, storeErr := openCityStoreAt(current.cityPath)
	if storeErr != nil {
		fmt.Fprintf(stderr, "gc runtime request-restart: opening store: %v\n", storeErr) //nolint:errcheck // best-effort stderr
	}
	if store != nil {
		restartable, err := sessionRestartableByController(store, current.sessionName)
		if err != nil {
			fmt.Fprintf(stderr, "gc runtime request-restart: checking session type: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if !restartable {
			if err := clearRestartRequest(store, dops, current.sessionName); err != nil {
				fmt.Fprintf(stderr, "gc runtime request-restart: clearing stale restart request: %v\n", err) //nolint:errcheck // best-effort stderr
				return 1
			}
			fmt.Fprintln(stdout, "Restart skipped for named session; controller cannot restart on-demand named sessions.") //nolint:errcheck // best-effort stdout
			return 0
		}
	}
	rec := openCityRecorderAt(current.cityPath, stderr)
	cfg, _ := loadCityConfig(current.cityPath, stderr)
	var persistRestart func() error
	if store != nil {
		persistRestart = func() error {
			handle, err := workerHandleForSessionTargetWithConfig(current.cityPath, store, sp, cfg, current.sessionName)
			if err != nil {
				return err
			}
			return handle.Reset(context.Background())
		}
	}
	return doRuntimeRequestRestart(dops, persistRestart, rec, current.display, current.sessionName, stdout, stderr)
}

// doRuntimeRequestRestart sets the restart-requested flag and blocks forever.
// The controller will kill and restart the session on its next tick.
func doRuntimeRequestRestart(dops drainOps, persistRestart func() error, rec events.Recorder,
	targetName, sn string, stdout, stderr io.Writer,
) int {
	if err := dops.setRestartRequested(sn); err != nil {
		fmt.Fprintf(stderr, "gc runtime request-restart: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	// Also persist the request through the worker boundary so it survives
	// tmux session death. Non-fatal: the runtime flag above is primary.
	if persistRestart != nil {
		if err := persistRestart(); err != nil {
			fmt.Fprintf(stderr, "gc runtime request-restart: setting bead restart flag: %v\n", err) //nolint:errcheck // best-effort stderr
		}
	}
	rec.Record(events.Event{
		Type:    events.SessionDraining,
		Actor:   targetName,
		Subject: targetName,
		Message: "restart requested by session",
	})
	fmt.Fprintln(stdout, "Restart requested. Blocking until controller kills this session...") //nolint:errcheck // best-effort stdout

	// Block forever. The controller will kill the entire process tree.
	select {}
}

// doRuntimeDrainAck sets the drain-ack flag on the session. The controller
// will stop the session on the next tick.
func doRuntimeDrainAck(dops drainOps, sn string, stdout, stderr io.Writer) int {
	if err := dops.setDrainAck(sn); err != nil {
		fmt.Fprintf(stderr, "gc runtime drain-ack: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintln(stdout, "Drain acknowledged. Controller will stop this session.") //nolint:errcheck // best-effort stdout
	return 0
}
