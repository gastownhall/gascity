package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/reconcilekey"
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

type runtimeDrainCheckJSON struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	Command       string `json:"command"`
	Session       string `json:"session"`
	Target        string `json:"target,omitempty"`
	Draining      bool   `json:"draining"`
}

type runtimeActionJSON struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	Command       string `json:"command"`
	Action        string `json:"action"`
	Session       string `json:"session"`
	Target        string `json:"target,omitempty"`
	Status        string `json:"status"`
}

func (o *providerDrainOps) setDrain(sessionName string) error {
	return o.sp.SetMeta(sessionName, "GC_DRAIN", strconv.FormatInt(time.Now().Unix(), 10))
}

func (o *providerDrainOps) clearDrain(sessionName string) error {
	return errors.Join(
		o.sp.RemoveMeta(sessionName, "GC_DRAIN_ACK"),
		o.sp.RemoveMeta(sessionName, reconcilerDrainAckSourceKey),
		// The incarnation stamp has exactly the acknowledgement's lifetime. Left
		// behind, it outlives every drain it described and waits on the pane to be
		// paired with some later ack's source.
		o.sp.RemoveMeta(sessionName, drainAckRequesterInstanceTokenKey),
		o.sp.RemoveMeta(sessionName, reconcilerDrainAckReasonKey),
		o.sp.RemoveMeta(sessionName, reconcilerDrainAckGenerationKey),
		o.sp.RemoveMeta(sessionName, "GC_DRAIN"),
	)
}

func (o *providerDrainOps) isDraining(sessionName string) (bool, error) {
	val, err := o.sp.GetMeta(sessionName, "GC_DRAIN")
	if err != nil {
		return false, fmt.Errorf("reading GC_DRAIN: %w", err)
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
	// The acknowledging agent records which incarnation it was. Readers bind
	// against this rather than trusting a bare source value that a recycled
	// chair carries over from whoever sat there last.
	//
	// The stamp lands BEFORE the source, and that order is load-bearing. The one
	// reader of this pairing (drainReminderAckPin) admits an acknowledgement on
	// the source alone, then reads the stamp in a SECOND provider round-trip.
	// Source-first, a reader landing between the two writes sees this ack's fresh
	// source beside the PREVIOUS occupant's stamp and mints agentAckBindingStale
	// — positive proof of residue about an acknowledgement that landed
	// microseconds ago, which is the one verdict that must never be minted by
	// accident. Stamp-first, the only stamp state a source-keyed reader can
	// observe is this ack's own: its digest, or empty and therefore unprovable.
	requesterInstanceToken := drainAckRequesterInstanceToken(sessionName)
	return joinDrainAckMutationErrors(
		o.sp.RemoveMeta(sessionName, reconcilerDrainAckReasonKey),
		o.sp.RemoveMeta(sessionName, reconcilerDrainAckGenerationKey),
		o.sp.SetMeta(sessionName, drainAckRequesterInstanceTokenKey, requesterInstanceToken),
		o.sp.SetMeta(sessionName, reconcilerDrainAckSourceKey, drainAckSourceAgentValue),
		o.sp.SetMeta(sessionName, "GC_DRAIN_ACK", "1"),
	)
}

// drainAckRequesterInstanceToken returns the binding stamp for the acking pane's
// own incarnation, and ONLY when this pane is the session being acked. `gc
// runtime drain-ack <other>` is a cross-session ack: the caller's token is
// evidence about the CALLER, not about the target, and stamping it on the
// target's row reads back as agentAckBindingStale — positive proof of residue
// for an acknowledgement that landed seconds ago. An empty stamp degrades to
// agentAckBindingUnprovable instead, which is the direction this reader is
// meant to fail in.
//
// The identity comparison is deliberately made against the pane's own
// environment rather than through currentSessionRuntimeTarget: that resolver
// also demands a city path, so it errors for reasons that have nothing to do
// with WHO is acking, and a legitimate self-ack would silently degrade to
// unprovable whenever the city context was unresolvable.
func drainAckRequesterInstanceToken(sessionName string) string {
	target := strings.TrimSpace(sessionName)
	self := strings.TrimSpace(os.Getenv("GC_TMUX_SESSION"))
	if self == "" {
		self = strings.TrimSpace(os.Getenv("GC_SESSION_NAME"))
	}
	if target == "" || self == "" || self != target {
		return ""
	}
	return drainAckInstanceTokenDigest(os.Getenv("GC_INSTANCE_TOKEN"))
}

// drainAckInstanceTokenDigest maps an incarnation token onto the value that is
// safe to leave on a pane. GC_INSTANCE_TOKEN is a capability rather than an
// identifier — it fences drain and stop against stale incarnations — and this
// codebase keeps that value class off every command line for it: envArgvSafe
// excludes the key, and session start stages it through a private 0600 file
// instead of argv. A provider SetMeta is an argv channel on tmux
// (`set-environment -t <name> <key> <value>`), whose argument vector is
// world-readable in /proc/<pid>/cmdline, so stamping the token itself would
// reopen that exposure on every self-ack.
//
// The binding only ever needs an equality compare, which a digest answers
// without carrying the capability: equal tokens digest equal, and the digest
// grants nothing. Empty stays empty, so a degraded pane that has no token stamps
// nothing rather than the digest of "" — the unprovable arm is unchanged.
func drainAckInstanceTokenDigest(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (o *providerDrainOps) isDrainAcked(sessionName string) (bool, error) {
	val, err := o.sp.GetMeta(sessionName, "GC_DRAIN_ACK")
	if err != nil {
		return false, fmt.Errorf("reading GC_DRAIN_ACK: %w", err)
	}
	return val == "1", nil
}

func (o *providerDrainOps) setRestartRequested(sessionName string) error {
	return o.sp.SetMeta(sessionName, "GC_RESTART_REQUESTED", strconv.FormatInt(time.Now().Unix(), 10))
}

func (o *providerDrainOps) isRestartRequested(sessionName string) (bool, error) {
	val, err := o.sp.GetMeta(sessionName, "GC_RESTART_REQUESTED")
	if err != nil {
		if runtime.IsSessionGone(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading GC_RESTART_REQUESTED: %w", err)
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
		return false, fmt.Errorf("reading GC_DRIFT_RESTART: %w", err)
	}
	return val == "1", nil
}

func (o *providerDrainOps) clearDriftRestart(sessionName string) error {
	return o.sp.RemoveMeta(sessionName, "GC_DRIFT_RESTART")
}

func joinDrainAckMutationErrors(errs ...error) error {
	var joined []error
	for _, err := range errs {
		if err == nil || drainAckMissingSessionBeadError(err) {
			continue
		}
		joined = append(joined, err)
	}
	return errors.Join(joined...)
}

func drainAckMissingSessionBeadError(err error) bool {
	return runtime.IsSessionGone(err) || errors.Is(err, beads.ErrNotFound)
}

// newDrainOps creates a drainOps from a runtime.Provider.
func newDrainOps(sp runtime.Provider) drainOps {
	return &providerDrainOps{sp: sp}
}

// ---------------------------------------------------------------------------
// gc runtime drain <name>
// ---------------------------------------------------------------------------

func newRuntimeDrainCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "drain <name>",
		Short: "Signal a session to drain (wind down gracefully)",
		Long: `Signal a session to drain — wind down its current work gracefully.

Sets a GC_DRAIN metadata flag on the session. The agent should check
for drain status periodically (via "gc runtime drain-check") and finish
its current task before exiting. Pass a session alias or ID. Use
"gc runtime undrain" to cancel.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdRuntimeDrain(args, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

func cmdRuntimeDrain(args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc runtime drain: missing session alias or ID") //nolint:errcheck // best-effort stderr
		return 1
	}
	target, err := resolveSessionRuntimeTarget(args[0], stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime drain: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	sp, err := newSessionProvider()
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime drain: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	dops := newDrainOps(sp)
	rec := openCityRecorder(stderr)
	return doRuntimeDrain(dops, sp, rec, target.display, target.sessionName, jsonOutput, stdout, stderr)
}

// doRuntimeDrain sets the drain signal on a session.
func doRuntimeDrain(dops drainOps, sp runtime.Provider, rec events.Recorder,
	targetName, sn string, jsonOutput bool, stdout, stderr io.Writer,
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
	if jsonOutput {
		if err := writeCLIJSONLine(stdout, runtimeActionJSON{
			SchemaVersion: "1",
			OK:            true,
			Command:       "runtime drain",
			Action:        "drain",
			Session:       sn,
			Target:        targetName,
			Status:        "draining",
		}); err != nil {
			fmt.Fprintf(stderr, "gc runtime drain: writing JSON: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "Draining session '%s'\n", targetName) //nolint:errcheck // best-effort stdout
	return 0
}

// ---------------------------------------------------------------------------
// gc runtime undrain <name>
// ---------------------------------------------------------------------------

func newRuntimeUndrainCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "undrain <name>",
		Short: "Cancel drain on a session",
		Long: `Cancel a pending drain signal on a session.

Clears the GC_DRAIN and GC_DRAIN_ACK metadata flags and the session
row's drain-ack, allowing the session to continue normal operation.
Pass a session alias or ID.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdRuntimeUndrain(args, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

func cmdRuntimeUndrain(args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc runtime undrain: missing session alias or ID") //nolint:errcheck // best-effort stderr
		return 1
	}
	target, err := resolveSessionRuntimeTarget(args[0], stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime undrain: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	sp, err := newSessionProvider()
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime undrain: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	dops := newDrainOps(sp)
	rec := openCityRecorder(stderr)
	clearRow := func() error { return clearDrainAckRowAt(target.cityPath, target.sessionName, target.sessionID) }
	return undrainRuntime(dops, clearRow, sp, rec, target.display, target.sessionName, jsonOutput, stdout, stderr)
}

// doRuntimeUndrain clears the drain signal on a session.
func doRuntimeUndrain(dops drainOps, sp runtime.Provider, rec events.Recorder,
	targetName, sn string, jsonOutput bool, stdout, stderr io.Writer,
) int {
	return undrainRuntime(dops, nil, sp, rec, targetName, sn, jsonOutput, stdout, stderr)
}

// undrainRuntime clears the runtime drain keys exactly as legacy does and
// then, when clearRow is set, the row-bound ack. A failed row clear keeps the
// legacy clear and its event and exits non-zero.
func undrainRuntime(dops drainOps, clearRow func() error, sp runtime.Provider, rec events.Recorder,
	targetName, sn string, jsonOutput bool, stdout, stderr io.Writer,
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
	if clearRow != nil {
		if err := clearRow(); err != nil {
			fmt.Fprintf(stderr, "gc runtime undrain: clearing the row drain-ack: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	if jsonOutput {
		if err := writeCLIJSONLine(stdout, runtimeActionJSON{
			SchemaVersion: "1",
			OK:            true,
			Command:       "runtime undrain",
			Action:        "undrain",
			Session:       sn,
			Target:        targetName,
			Status:        "undrained",
		}); err != nil {
			fmt.Fprintf(stderr, "gc runtime undrain: writing JSON: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "Undrained session '%s'\n", targetName) //nolint:errcheck // best-effort stdout
	return 0
}

// ---------------------------------------------------------------------------
// gc runtime drain-check
// ---------------------------------------------------------------------------

func newRuntimeDrainCheckCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "drain-check [name]",
		Short: "Check if a session is draining (exit 0 = draining)",
		Long: `Check if a session is currently draining.

Returns exit code 0 if draining, 1 if not. Designed for use in
conditionals: "if gc runtime drain-check; then finish-up; fi". Without
arguments, uses the current session context.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdRuntimeDrainCheck(args, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

func cmdRuntimeDrainCheck(args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		target, err := resolveSessionRuntimeTarget(args[0], stderr)
		if err != nil {
			fmt.Fprintf(stderr, "gc runtime drain-check: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1                                                 // silent — same as current "not draining" behavior
		}
		sp, err := newSessionProvider()
		if err != nil {
			fmt.Fprintf(stderr, "gc runtime drain-check: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		dops := newDrainOps(sp)
		return doRuntimeDrainCheck(dops, target.display, target.sessionName, jsonOutput, stdout, stderr)
	}

	current, err := currentSessionRuntimeTarget()
	if err != nil {
		return 1 // not in agent context → not draining
	}
	sp, err := newSessionProvider()
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime drain-check: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	dops := newDrainOps(sp)
	return doRuntimeDrainCheck(dops, current.display, current.sessionName, jsonOutput, stdout, stderr)
}

// doRuntimeDrainCheck returns 0 if the session is draining, 1 otherwise.
// Silent on stdout — designed for `if gc runtime drain-check; then ...`.
func doRuntimeDrainCheck(dops drainOps, targetName, sn string, jsonOutput bool, stdout, stderr io.Writer) int {
	draining, err := dops.isDraining(sn)
	if err != nil {
		return 1
	}
	if !draining {
		if jsonOutput {
			if err := writeCLIJSONLine(stdout, runtimeDrainCheckJSON{
				SchemaVersion: "1",
				OK:            true,
				Command:       "runtime drain-check",
				Session:       sn,
				Target:        targetName,
				Draining:      false,
			}); err != nil {
				fmt.Fprintf(stderr, "gc runtime drain-check: writing JSON: %v\n", err) //nolint:errcheck // best-effort stderr
				return 1
			}
		}
		return 1
	}
	if jsonOutput {
		if err := writeCLIJSONLine(stdout, runtimeDrainCheckJSON{
			SchemaVersion: "1",
			OK:            true,
			Command:       "runtime drain-check",
			Session:       sn,
			Target:        targetName,
			Draining:      true,
		}); err != nil {
			fmt.Fprintf(stderr, "gc runtime drain-check: writing JSON: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// gc runtime drain-ack
// ---------------------------------------------------------------------------

func newRuntimeDrainAckCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput, operator bool
	cmd := &cobra.Command{
		Use:   "drain-ack [name]",
		Short: "Acknowledge drain — signal the controller to stop this session",
		Long: `Acknowledge a drain signal — tell the controller to stop this session.

Sets GC_DRAIN_ACK metadata on the session, then pokes the controller
socket so the reconciler stops the session immediately rather than on
its next patrol tick. Call this after the session has finished its
current work in response to a drain signal.

The ack is bound to the session row's current incarnation and written
there before held claims are released and GC_DRAIN_ACK is set. A session
acking itself must carry a GC_INSTANCE_TOKEN that matches the row; a
missing or stale token is refused with exit 1 and nothing is written. An
operator acking a session from outside it passes --operator and a target,
and the ack binds to the incarnation the command read. A store that is
unreachable or keeps changing also exits 1 with nothing acknowledged; run
the command again. A store without conditional writes skips the row write
with a warning and acknowledges as before.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdRuntimeDrainAck(args, jsonOutput, operator, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&operator, "operator", false, "ack another session as an operator, bound to the incarnation read (requires a target)")
	return cmd
}

func cmdRuntimeDrainAck(args []string, jsonOutput, operator bool, stdout, stderr io.Writer) int {
	if operator && len(args) == 0 {
		fmt.Fprintln(stderr, "gc runtime drain-ack: --operator requires a session alias or ID") //nolint:errcheck // best-effort stderr
		return 1
	}
	if len(args) > 0 {
		target, err := resolveSessionRuntimeTarget(args[0], stderr)
		if err != nil {
			fmt.Fprintf(stderr, "gc runtime drain-ack: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		sp, err := newSessionProvider()
		if err != nil {
			fmt.Fprintf(stderr, "gc runtime drain-ack: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		dops := newDrainOps(sp)
		checkRow := func() (func() error, error) {
			return checkDrainAckRowAt(target.cityPath, target.sessionName, target.sessionID, operator)
		}
		return ackRuntimeDrain(dops, checkRow, target.cityPath, target.display, target.sessionName, target.sessionID, jsonOutput, stdout, stderr)
	}

	current, err := currentSessionRuntimeTarget()
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime drain-ack: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	sp, err := newSessionProvider()
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime drain-ack: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	dops := newDrainOps(sp)
	checkRow := func() (func() error, error) {
		return checkDrainAckRowAt(current.cityPath, current.sessionName, "", false)
	}
	// Name-only key: the env-derived GC_SESSION_ID can be stale.
	return ackRuntimeDrain(dops, checkRow, current.cityPath, current.display, current.sessionName, "", jsonOutput, stdout, stderr)
}

// ---------------------------------------------------------------------------
// gc runtime request-restart
// ---------------------------------------------------------------------------

func newRuntimeRequestRestartCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "request-restart",
		Short: "Request controller restart this session (returns immediately)",
		Long: `Signal the controller to stop and restart this session.

Sets GC_RESTART_REQUESTED metadata on the session, pokes the controller for
an immediate reconcile tick, and returns without waiting. Control-plane
authority over the actual stop/start stays with the controller's reconcile
loop; this command only signals it so the request need not wait for the next
periodic patrol tick.

The command exits 0 once the restart request is durably persisted and the
controller has been signaled, even if the controller has not yet acted. If
the controller cannot be signaled, the command exits 1 with a diagnostic —
the restart request itself remains durably set, so the controller still
picks it up on its next periodic reconcile tick regardless.

This command is designed to be called from within a session context.
It emits a session.draining event before signaling the controller.`,
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

	sp, err := newSessionProvider()
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime request-restart: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	dops := newDrainOps(sp)
	store, storeErr := openCityStoreAt(current.cityPath)
	if storeErr != nil {
		fmt.Fprintf(stderr, "gc runtime request-restart: opening store: %v\n", storeErr) //nolint:errcheck // best-effort stderr
	}
	// Route the SESSION-class access (restart persist through the worker
	// boundary) to the session coordination-class store so a
	// [beads.classes.sessions] relocation reaches gc runtime request-restart.
	// The routing cfg is loaded refresh-free (the full cfg loads later, for
	// timeout/template resolution). Identity today, so byte-identical.
	var sessStore beads.Store
	if store != nil {
		routeCfg, _ := loadCityConfigWithoutBuiltinPackRefresh(current.cityPath, io.Discard)
		sessStore = cliSessionStore(store, routeCfg, current.cityPath)
	}
	rec := openCityRecorderAt(current.cityPath, stderr)
	cfg, _ := loadCityConfig(current.cityPath, stderr)
	var persistRestart func() error
	if store != nil {
		persistRestart = func() error {
			handle, err := workerHandleForSessionTargetWithConfig(current.cityPath, sessStore, sp, cfg, current.sessionName)
			if err != nil {
				return err
			}
			return handle.Reset(context.Background())
		}
	}
	_, pinned, err := sessionRestartableByController(sessStore, current.sessionName)
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime request-restart: checking session type: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return doRuntimeRequestRestart(dops, persistRestart, pinned, rec, current.display, current.sessionName,
		current.cityPath, stdout, stderr)
}

// controllerRestartTimeout computes the bounded timeout for waiting on the
// controller to act on a restart request: max(5*PatrolInterval, 5min), capped at 30min.
func controllerRestartTimeout(cfg *config.City) time.Duration {
	const floor = 5 * time.Minute
	const ceil = 30 * time.Minute
	patrol := 30 * time.Second
	if cfg != nil {
		patrol = cfg.Daemon.PatrolIntervalDuration()
	}
	d := 5 * patrol
	if d < floor {
		d = floor
	}
	if d > ceil {
		d = ceil
	}
	return d
}

// doRuntimeRequestRestart sets the restart-requested flag, pokes the
// controller for an immediate reconcile tick, and returns without waiting for
// the controller to act. Control-plane authority over the actual stop/start
// stays with the controller's reconcile loop; this call only signals it so
// the request need not wait for the next periodic tick.
//
// pinned marks a kill-protected named session (pin_awake == "true"): the
// reconciler refuses to collaterally kill such a session on a bare runtime
// restart-requested flag, so for pinned sessions persistRestart (which lands
// continuation_reset_pending, the explicit-reset escape hatch) is mandatory
// rather than best-effort. See sessionRestartableByController.
func doRuntimeRequestRestart(dops drainOps, persistRestart func() error, pinned bool, rec events.Recorder,
	targetName, sn, cityPath string, stdout, stderr io.Writer,
) int {
	if err := dops.setRestartRequested(sn); err != nil {
		fmt.Fprintf(stderr, "gc runtime request-restart: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if pinned {
		if persistRestart == nil {
			fmt.Fprintf(stderr, "gc runtime request-restart: pinned session %q has no restart persistence available; not requesting restart\n", sn) //nolint:errcheck // best-effort stderr
			return 1
		}
		if err := persistRestart(); err != nil {
			fmt.Fprintf(stderr, "gc runtime request-restart: could not persist restart marker for pinned session %q; not requesting restart: %v\n", sn, err) //nolint:errcheck // best-effort stderr
			return 1
		}
	} else if persistRestart != nil {
		// Also persist the request through the worker boundary so it survives
		// tmux session death. Non-fatal here: the runtime flag above is primary.
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

	// Name-only key: sn is authoritative here, while GC_SESSION_ID can be stale.
	if err := pokeControllerForRestart(cityPath, reconcilekey.SessionNamed(sn)); err != nil {
		fmt.Fprintf(stderr, "gc runtime request-restart: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprint(stdout, "Restart requested; controller notified for immediate reconcile.\n") //nolint:errcheck // best-effort stdout
	return 0
}

// waitForControllerRestart polls until the controller accepts the stop
// handoff (exit 0), the context is canceled by a signal (exit 0), or the
// bounded timeout expires (exit 1 with diagnostic).
//
// A cleared GC_RESTART_REQUESTED flag alone is not proof the controller acted:
// the reconciler's pinned-session collateral-skip clears the very same flag
// without killing the session (see pinnedConfiguredNamedSessionKillProtected
// in session_reconciler.go). sp confirms the session actually stopped before
// this reports success; while the flag is clear but the session is still
// running, polling continues until the deadline instead of returning early.
func waitForControllerRestart(ctx context.Context, dops drainOps, sp runtime.Provider, sn string, timeout time.Duration, stderr io.Writer) int {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var lastPollErr error

	for {
		select {
		case <-ctx.Done():
			// Signal received; leave the flag set so the controller still acts on its next tick.
			fmt.Fprint(stderr, "gc handoff: signal received; restart request remains set; controller will stop this session on its next reconcile tick\n") //nolint:errcheck // best-effort stderr
			return 0
		case <-ticker.C:
			requested, err := dops.isRestartRequested(sn)
			switch {
			case err != nil:
				lastPollErr = err
			case !requested && !sp.IsRunning(sn):
				// The controller accepted the stop handoff or the runtime is already gone.
				return 0
			default:
				lastPollErr = nil
			}
			if time.Now().After(deadline) {
				if lastPollErr != nil {
					fmt.Fprintf(stderr, "gc handoff: controller did not act within %s; last poll error: %v; check `gc dashboard` or `gc trace`\n", timeout, lastPollErr) //nolint:errcheck // best-effort stderr
				} else {
					fmt.Fprintf(stderr, "gc handoff: controller did not act within %s; check `gc dashboard` or `gc trace`\n", timeout) //nolint:errcheck // best-effort stderr
				}
				return 1
			}
		}
	}
}

// drainAckPokeController is a mutable global test seam over enqueueController.
// Tests that swap it MUST NOT call t.Parallel().
var drainAckPokeController = enqueueController

// drainAckReleaseHeldClaims is a mutable global test seam over
// releaseUnexecutedClaimsForSession, matching drainAckPokeController above.
// Tests that swap it MUST NOT call t.Parallel().
var drainAckReleaseHeldClaims = releaseUnexecutedClaimsForSession

// releaseUnexecutedClaimsForSession resolves this city's residency-correct work
// legs and gives back every in_progress claim the draining session still holds.
//
// The leg set mirrors `gc session close`, which leads with the WORK store and
// hands in the relocated graph binding as a class leg: a claim that claim-time
// routing left in the binding is invisible to a work-led scan, and would be
// released by nothing. Best-effort throughout — a city that cannot be resolved
// or a store that cannot be opened must never block the ack itself, which is the
// signal the controller is waiting on.
func releaseUnexecutedClaimsForSession(cityPath, sessionName string, stderr io.Writer) {
	if strings.TrimSpace(cityPath) == "" || strings.TrimSpace(sessionName) == "" {
		return
	}
	// Only read a real city. A city is a directory holding city.toml, and
	// opening a store somewhere that is not one does not find claims — it
	// PROVISIONS a store (a managed Dolt server included) in whatever directory
	// the caller happened to resolve. Drain-ack is reachable from contexts with
	// no city at all (a bare `gc hook --claim --drain-ack`, a test harness), and
	// before this release step it did no store I/O whatsoever, so the cost of
	// getting that wrong is a data directory and a server process where neither
	// belongs.
	if _, err := os.Stat(filepath.Join(cityPath, "city.toml")); err != nil {
		// A runtime root carrying .gc/ but no city.toml is the legacy shape, and
		// it is the one skip worth naming: it looks like a city to a human, a
		// session really can hold claims there, and staying silent would make a
		// release that never ran indistinguishable from one that found nothing.
		// It is NOT auto-upgraded here — provisioning a store against a layout
		// this function does not understand is the failure the city.toml gate
		// exists to prevent.
		if _, gcErr := os.Stat(filepath.Join(cityPath, ".gc")); gcErr == nil {
			fmt.Fprintf(stderr, "gc runtime drain-ack: %s has .gc/ but no city.toml; skipping the held-claim release for this legacy runtime root (run `gc doctor` to check the layout)\n", cityPath) //nolint:errcheck
		}
		return
	}
	// One load serves every open below: drain-ack is a one-shot command, so
	// the config it just read is current. The saving applies only when this
	// load succeeds: a failed load leaves cfg nil, the city open then loads
	// again on its own, and the rig legs are skipped as before.
	cfg, _ := loadCityConfig(cityPath, io.Discard)
	store, err := openCityStoreAtWithConfig(cityPath, cfg)
	if err != nil || store == nil {
		return
	}
	rigStores := func() map[string]beads.Store {
		if cfg == nil {
			return nil
		}
		return buildStandaloneRigStoresWithConfig(cfg, cityPath, io.Discard)
	}
	releaseUnexecutedClaimsForSessionStore(cityPath, cfg, store, rigStores, sessionName, drainAckReleaseBudget, stderr)
}

// releaseUnexecutedClaimsForSessionStore resolves a runtime session name to the
// durable session bead behind it and releases the claims that bead's identities
// still hold. Resolution is the reason this step exists separately: a pool
// worker's runtime name lives in `session_name` metadata on a bead whose ID is
// something else entirely, so a direct Get on the runtime name would miss it.
//
// rigStores is a thunk, not a map, because opening the rig stores is real store
// I/O on the pre-ack path. It is called only once resolution and the session
// Get have both succeeded — a drain-ack that cannot find its session pays
// nothing for stores it would immediately discard.
func releaseUnexecutedClaimsForSessionStore(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	rigStores func() map[string]beads.Store,
	sessionName string,
	budget time.Duration,
	stderr io.Writer,
) {
	sessStore := cliSessionStore(store, cfg, cityPath)
	sessionID, err := resolveSessionID(sessStore, sessionName)
	if err != nil {
		// A session that cannot be resolved holds nothing this pass can find, so
		// there is nothing to report. The ack is the signal the controller is
		// waiting on, and a release that could not begin must not decorate a
		// successful ack with a warning an operator can do nothing about. A claim
		// genuinely left behind here is still caught by the dead-assignee lane.
		return
	}
	sessionBead, err := sessStore.Get(sessionID)
	if err != nil {
		return
	}
	var rigs map[string]beads.Store
	if rigStores != nil {
		rigs = rigStores()
	}
	releaseUnexecutedClaimsOnDrainAck(cityPath, cfg, store, rigs, sessionBead, budget, stderr)
}

// drainAckReleaseBudget bounds the whole held-claim release pass.
//
// The pass runs BEFORE the ack, which is the signal the controller waits on to
// stop this session, and it fans out over every work leg × every identity with
// only per-command ceilings underneath it. A slow or contended store would
// therefore make drain-ack hang for the product of those, turning a safety net
// into a stall on the exact path a draining worker needs to be fast. Releasing
// SOME claims and acking is strictly better than releasing all of them late:
// whatever this budget leaves behind is the dead-assignee lane's to collect.
const drainAckReleaseBudget = 15 * time.Second

// doRuntimeDrainAck releases any unexecuted claim the session still holds, sets
// the drain-ack flag, then pokes the controller so the reconciler observes the
// drained state immediately instead of waiting for its next patrol tick.
//
// The release runs BEFORE the ack, and the order is load-bearing: the ack is what
// tells the controller it may stop this session, so acknowledging first opens a
// window in which the session dies still holding exactly the claim this release
// exists to clear.
func doRuntimeDrainAck(dops drainOps, cityPath, targetName, sn, sessionID string, jsonOutput bool, stdout, stderr io.Writer) int {
	return ackRuntimeDrain(dops, nil, cityPath, targetName, sn, sessionID, jsonOutput, stdout, stderr)
}

// ackRuntimeDrain is doRuntimeDrainAck with the row-bound ack (CONTRACT v5
// D5). checkRow decides against one read of the row before anything is
// written:
//
//   - a refusal (token missing or mismatched, row unreadable) writes nothing:
//     no row, no claim release, no env key; exit 1;
//   - a match on a store with no conditional writer (legacy only: v2 refuses
//     to boot without one) warns and runs legacy's flow unchanged; exit 0;
//   - a match commits the row CAS first. A CAS that loses every retry, or a
//     store that fails, writes nothing; exit 1. Once it lands, the claim
//     release and the env ack follow in legacy's order (release first).
//
// A nil checkRow, or a nil commit with no error (not a city), keeps legacy's
// flow silently.
func ackRuntimeDrain(dops drainOps, checkRow func() (func() error, error),
	cityPath, targetName, sn, sessionID string, jsonOutput bool, stdout, stderr io.Writer,
) int {
	if checkRow != nil {
		commitRow, err := checkRow()
		switch {
		case errors.Is(err, errDrainAckRowUnfenced):
			fmt.Fprintf(stderr, "gc runtime drain-ack: warning: %v\n", err) //nolint:errcheck // best-effort stderr
		case err != nil:
			fmt.Fprintf(stderr, "gc runtime drain-ack: refused, nothing acknowledged: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		case commitRow != nil:
			if err := commitRow(); err != nil {
				fmt.Fprintf(stderr, "gc runtime drain-ack: row ack not written, nothing acknowledged: %v\n", err) //nolint:errcheck // best-effort stderr
				return 1
			}
		}
	}
	drainAckReleaseHeldClaims(cityPath, sn, stderr)
	if err := dops.setDrainAck(sn); err != nil {
		fmt.Fprintf(stderr, "gc runtime drain-ack: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := drainAckPokeController(cityPath, reconcilekey.SessionRef(sessionID, sn)); err != nil {
		fmt.Fprintf(stderr, "gc runtime drain-ack: warning: poke failed: %v\n", err) //nolint:errcheck // best-effort stderr
	}
	if jsonOutput {
		if err := writeCLIJSONLine(stdout, runtimeActionJSON{
			SchemaVersion: "1",
			OK:            true,
			Command:       "runtime drain-ack",
			Action:        "drain-ack",
			Session:       sn,
			Target:        targetName,
			Status:        "acknowledged",
		}); err != nil {
			fmt.Fprintf(stderr, "gc runtime drain-ack: writing JSON: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	}
	fmt.Fprintln(stdout, "Drain acknowledged. Controller poked for immediate stop.") //nolint:errcheck // best-effort stdout
	return 0
}

// Row-bound drain-ack keys (CONTRACT v5 D5). The CLI is their only writer;
// legacy ignores both, so the dual write is rollback-safe.
const (
	drainAckIncarnationKey = "drain_ack_incarnation"
	drainAckAtKey          = "drain_ack_at"
)

// checkDrainAckRowAt resolves the target's row in the city at cityPath and
// decides the ack against it (checkDrainAckRow). The pane's GC_INSTANCE_TOKEN
// is compared in process and never leaves it.
func checkDrainAckRowAt(cityPath, sessionName, sessionID string, operator bool) (func() error, error) {
	store, id, err := drainAckSessionRow(cityPath, sessionName, sessionID)
	if err != nil || store == nil {
		return nil, err
	}
	return checkDrainAckRow(store, id, operator, os.Getenv("GC_INSTANCE_TOKEN"), time.Now())
}

// clearDrainAckRowAt clears the row-bound ack for `gc runtime undrain`.
func clearDrainAckRowAt(cityPath, sessionName, sessionID string) error {
	store, id, err := drainAckSessionRow(cityPath, sessionName, sessionID)
	if err != nil || store == nil {
		return err
	}
	return clearDrainAckRow(store, id)
}

// drainAckSessionRow opens the session leg of the city at cityPath and
// resolves the target's row. A path that is not a city has no row and yields
// a nil store, for the reason releaseUnexecutedClaimsForSession gives. An
// empty sessionID resolves by name: the env-derived GC_SESSION_ID can be
// stale.
func drainAckSessionRow(cityPath, sessionName, sessionID string) (beads.Store, string, error) {
	if strings.TrimSpace(cityPath) == "" {
		return nil, "", nil
	}
	if _, err := os.Stat(filepath.Join(cityPath, "city.toml")); err != nil {
		return nil, "", nil
	}
	cfg, _ := loadCityConfig(cityPath, io.Discard)
	store, err := openCityStoreAtWithConfig(cityPath, cfg)
	if err != nil {
		return nil, "", fmt.Errorf("opening the session store: %w", err)
	}
	if store == nil {
		return nil, "", errors.New("opening the session store: no store")
	}
	sessStore := cliSessionStore(store, cfg, cityPath)
	if sessionID == "" {
		if sessionID, err = resolveSessionID(sessStore, sessionName); err != nil {
			return nil, "", fmt.Errorf("resolving session %q: %w", sessionName, err)
		}
	}
	return sessStore, sessionID, nil
}

// errDrainAckRowUnfenced reports a matching ack on a store with no
// conditional writer. Only legacy runs there (v2 refuses to boot, C0.7), so
// the ack proceeds as legacy's alone.
var errDrainAckRowUnfenced = errors.New("the session store has no conditional writer, so the ack is not written to the session row; the runtime ack is set as before")

// checkDrainAckRow reads the row once and decides the ack before anything is
// written; a refusal is returned as the error. The returned commit writes the
// ack by CAS, bound to the generation this read saw, so a PreWake between this
// read and the write makes the commit refuse instead of acking the next
// incarnation (I-ACK-1). A match on a store with no conditional writer returns
// errDrainAckRowUnfenced and no commit; nothing is ever written blind.
func checkDrainAckRow(store beads.Store, sessionID string, operator bool, envToken string, now time.Time) (func() error, error) {
	b, err := store.Get(sessionID)
	if err != nil {
		return nil, err
	}
	patch, err := drainAckRowPatch(b, operator, envToken, "", now)
	if err != nil {
		return nil, err
	}
	writer, _, err := beads.ResolveConditionalWriter(store)
	if err != nil {
		return nil, err
	}
	if writer == nil {
		return nil, errDrainAckRowUnfenced
	}
	bound := patch[drainAckIncarnationKey]
	return func() error {
		return casDrainAckRow(store, writer, sessionID, func(b beads.Bead) (map[string]string, error) {
			return drainAckRowPatch(b, operator, envToken, bound, now)
		})
	}, nil
}

// clearDrainAckRow clears both row-ack keys by CAS. A row that carries
// neither key, or a store with no conditional writer, is left untouched.
func clearDrainAckRow(store beads.Store, sessionID string) error {
	writer, _, err := beads.ResolveConditionalWriter(store)
	if err != nil || writer == nil {
		return err
	}
	return casDrainAckRow(store, writer, sessionID, func(b beads.Bead) (map[string]string, error) {
		if b.Metadata[drainAckIncarnationKey] == "" && b.Metadata[drainAckAtKey] == "" {
			return nil, nil
		}
		return map[string]string{drainAckIncarnationKey: "", drainAckAtKey: ""}, nil
	})
}

// casDrainAckRow is the kill fence's re-read/re-decide loop
// (applySessionKillFencePatch) without its unconditional fallback: every
// write is an UpdateIfMatch at the revision just read.
func casDrainAckRow(store beads.Store, writer beads.ConditionalWriter, sessionID string,
	decide func(beads.Bead) (map[string]string, error),
) error {
	var lastErr error
	for attempt := 0; attempt < sessionKillFenceAttempts; attempt++ {
		b, err := store.Get(sessionID)
		if err != nil {
			return err
		}
		patch, err := decide(b)
		if err != nil || len(patch) == 0 {
			return err
		}
		lastErr = writer.UpdateIfMatch(sessionID, b.Revision, beads.UpdateOpts{Metadata: patch})
		if lastErr == nil || !beads.IsPreconditionFailed(lastErr) {
			return lastErr
		}
	}
	return fmt.Errorf("conditional write kept losing to concurrent updates: %w", lastErr)
}

// drainAckRowPatch decides the row ack against one read of the row. The self
// form proves its incarnation with the pane's token; the operator form binds
// to the incarnation it read. The ack names the row's generation alone, so a
// rekey, which moves only the token, does not void it. bound is the
// generation the first read saw, or "" on that read.
func drainAckRowPatch(b beads.Bead, operator bool, envToken, bound string, now time.Time) (map[string]string, error) {
	envToken = strings.TrimSpace(envToken)
	generation := strings.TrimSpace(b.Metadata["generation"])
	switch {
	case b.Status == "closed":
		return nil, fmt.Errorf("session %s is closed", b.ID)
	case !operator && envToken == "":
		return nil, errors.New("GC_INSTANCE_TOKEN is not set, so this pane cannot prove which incarnation it is (an operator acking from outside the session passes --operator)")
	case !operator && strings.TrimSpace(b.Metadata["instance_token"]) != envToken:
		return nil, fmt.Errorf("GC_INSTANCE_TOKEN does not match session %s: it has been restarted since this pane started, or this is not its pane", b.ID)
	case generation == "":
		return nil, fmt.Errorf("session %s has no generation to bind the ack to", b.ID)
	case bound != "" && generation != bound:
		return nil, fmt.Errorf("session %s was restarted while the ack was being written", b.ID)
	}
	return map[string]string{
		drainAckIncarnationKey: generation,
		drainAckAtKey:          now.UTC().Format(time.RFC3339),
	}, nil
}
