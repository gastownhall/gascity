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
	"github.com/gastownhall/gascity/internal/runtime/guard"
	"github.com/gastownhall/gascity/internal/session"
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
	// Source names the deciding source: drainCheckSourceRow,
	// drainCheckSourceRuntime, or drainCheckSourceBoth when neither drains.
	Source string `json:"source"`
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
"gc runtime undrain" to cancel. Under the v2 session reconciler it also
marks the session row, so the agent's drain-ack stops the session even on a
min_active_sessions floor.`,
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
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime drain: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if onWorkerSite(cityPath) {
		fmt.Fprintf(stderr, "gc runtime drain: %v\n", errWorkerSiteDrainNeedsController) //nolint:errcheck // best-effort stderr
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
	return runtimeDrain(newDrainOps(sp), sp, openCityRecorder(stderr), target, jsonOutput, stdout, stderr)
}

// runtimeDrain decides the city's mode once. A legacy city runs
// doRuntimeDrain exactly as before; a v2 city also writes the row's
// operator drain mark.
func runtimeDrain(dops drainOps, sp runtime.Provider, rec events.Recorder, target sessionRuntimeTarget, jsonOutput bool, stdout, stderr io.Writer) int {
	cfg, strict := drainAckCityMode(target.cityPath)
	if !strict {
		return doRuntimeDrain(dops, sp, rec, target.display, target.sessionName, jsonOutput, stdout, stderr)
	}
	// A v2 city reads no GC_DRAIN: the row carries the operator's mark
	// (session.OperatorDrainKey), which explains the agent's ack (A11). The
	// env flag stays, for drain-check, legacy and a rollback.
	writeRow := func() error {
		store, id, err := drainAckOpenSessionRow(target.cityPath, cfg, target.sessionName, target.sessionID)
		if err != nil {
			return err
		}
		return writeOperatorDrainRow(store, id)
	}
	return drainRuntime(dops, writeRow, sp, rec, target.display, target.sessionName, jsonOutput, stdout, stderr)
}

// writeOperatorDrainRow writes the operator's drain mark on the row by CAS
// at the revision each attempt reads (operatorDrainPatch); no name lock: a
// controller's write in between loses the fence or re-decides. A store with
// no conditional writer is left untouched, as the row ack is (v2 refuses to
// boot on one, C0.7).
func writeOperatorDrainRow(store beads.Store, sessionID string) error {
	writer, _, err := beads.ResolveConditionalWriter(store)
	if err != nil || writer == nil {
		return err
	}
	return casDrainAckRow(store, writer, sessionID, func(b beads.Bead) (map[string]string, error) {
		return operatorDrainPatch(b.Metadata), nil
	})
}

// clearOperatorDrainRow clears the operator's drain mark by CAS
// (operatorUndrainPatch); a controller's request stands.
func clearOperatorDrainRow(store beads.Store, sessionID string) error {
	writer, _, err := beads.ResolveConditionalWriter(store)
	if err != nil || writer == nil {
		return err
	}
	return casDrainAckRow(store, writer, sessionID, func(b beads.Bead) (map[string]string, error) {
		return operatorUndrainPatch(b.Metadata), nil
	})
}

// doRuntimeDrain sets the drain signal on a session. It is the whole drain
// in a legacy city.
func doRuntimeDrain(dops drainOps, sp runtime.Provider, rec events.Recorder,
	targetName, sn string, jsonOutput bool, stdout, stderr io.Writer,
) int {
	return drainRuntime(dops, nil, sp, rec, targetName, sn, jsonOutput, stdout, stderr)
}

// drainRuntime writes, in a v2 city (writeRow set), the row's operator
// drain mark, then sets the runtime drain flag. A failed row write sets
// nothing and exits non-zero.
func drainRuntime(dops drainOps, writeRow func() error, sp runtime.Provider, rec events.Recorder,
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
	// The row first: an ack landing between the two writes would find no
	// mark and be canceled on a floor seat.
	if writeRow != nil {
		if err := writeRow(); err != nil {
			fmt.Fprintf(stderr, "gc runtime drain: writing the row's drain mark: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	if err := dops.setDrain(sn); err != nil {
		if !runtime.OnlyMetadataNotMirrored(err) {
			fmt.Fprintf(stderr, "gc runtime drain: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		// The drain flag is in force; only the runtime's view of it lags.
		fmt.Fprintf(stderr, "gc runtime drain: warning: %v\n", err) //nolint:errcheck // best-effort stderr
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

Clears the GC_DRAIN and GC_DRAIN_ACK metadata flags, allowing the
session to continue normal operation. Pass a session alias or ID. Under
the v2 session reconciler it also clears the session row's drain-ack and
its operator drain mark.`,
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
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime undrain: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if onWorkerSite(cityPath) {
		fmt.Fprintf(stderr, "gc runtime undrain: %v\n", errWorkerSiteDrainNeedsController) //nolint:errcheck // best-effort stderr
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
	return runtimeUndrain(newDrainOps(sp), sp, openCityRecorder(stderr), target, jsonOutput, stdout, stderr)
}

// runtimeUndrain clears the runtime drain keys and, in any city, the row's
// drain-ack and operator drain mark: a v2 city fails on a row clear that
// fails, a legacy city (which reads neither) only warns. A path that is not
// a city runs doRuntimeUndrain.
func runtimeUndrain(dops drainOps, sp runtime.Provider, rec events.Recorder, target sessionRuntimeTarget, jsonOutput bool, stdout, stderr io.Writer) int {
	cfg, strict := drainAckCityMode(target.cityPath)
	if cfg == nil {
		return doRuntimeUndrain(dops, sp, rec, target.display, target.sessionName, jsonOutput, stdout, stderr)
	}
	clearRow := func() error {
		store, id, err := drainAckOpenSessionRow(target.cityPath, cfg, target.sessionName, target.sessionID)
		if err != nil {
			return err
		}
		return errors.Join(clearDrainAckRow(store, id), clearOperatorDrainRow(store, id))
	}
	if !strict {
		// A legacy city ignores the row keys, but clears them too, so a mark
		// left by a v2 run (a mode switch, a config that failed to load) does
		// not outlive the undrain; a failure there only warns.
		legacy := clearRow
		clearRow = func() error {
			if err := legacy(); err != nil {
				fmt.Fprintf(stderr, "gc runtime undrain: warning: clearing the row's drain keys: %v\n", err) //nolint:errcheck // best-effort stderr
			}
			return nil
		}
	}
	return undrainRuntime(dops, clearRow, sp, rec, target.display, target.sessionName, jsonOutput, stdout, stderr)
}

// doRuntimeUndrain clears the drain signal on a session. It is the whole
// undrain in a legacy city.
func doRuntimeUndrain(dops drainOps, sp runtime.Provider, rec events.Recorder,
	targetName, sn string, jsonOutput bool, stdout, stderr io.Writer,
) int {
	return undrainRuntime(dops, nil, sp, rec, targetName, sn, jsonOutput, stdout, stderr)
}

// undrainRuntime clears the runtime drain keys and then, in a v2 city
// (clearRow set), the row-bound ack. A failed row clear keeps the runtime
// clear and its event and exits non-zero.
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
		if !runtime.OnlyMetadataNotMirrored(err) {
			fmt.Fprintf(stderr, "gc runtime undrain: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		// The drain is cleared; only the runtime's view of it lags.
		fmt.Fprintf(stderr, "gc runtime undrain: warning: %v\n", err) //nolint:errcheck // best-effort stderr
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

// Sources drain-check consults, as named in its JSON "source" field and in
// its unknown diagnostics.
const (
	drainCheckSourceRow     = "session_row"
	drainCheckSourceRuntime = "runtime_meta"
	drainCheckSourceBoth    = drainCheckSourceRow + "+" + drainCheckSourceRuntime
)

// drainCheckUnknownExit is drain-check's exit code when no source that
// answered says draining and at least one source could not answer.
const drainCheckUnknownExit = 3

// drainCheckUnknownCode is the JSON error code for drainCheckUnknownExit.
const drainCheckUnknownCode = "drain_state_unknown"

func newRuntimeDrainCheckCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "drain-check [name]",
		Short: "Check if a session is draining (exit 0 = draining)",
		Long: `Check if a session is currently draining.

Consults two sources: the runtime's GC_DRAIN metadata flag, which
"gc runtime drain" sets, and the session row's state, which reads
draining once the controller has begun stopping the session. Controller
drain intents recorded before that point are not consulted yet. Designed
for use in conditionals: "if gc runtime drain-check; then finish-up; fi".
Without arguments, uses the current session context.

Exit codes:
  0  draining: a source that answered says draining
  1  not draining: both sources answered and neither says draining;
     or there is no current session context; or the named session
     does not exist in a session store that opened; or the --json
     result could not be written
  3  unknown: a source could not be read, or the named session could
     not be resolved, and no source says draining; one stderr line
     names the source that failed

A runtime that reports the session absent, or keeps no metadata, counts
as an answer of "no GC_DRAIN"; any other runtime error, including a
runtime that is not running, is a failure to answer. With --json, the
result carries a "source" field naming the deciding source; an unknown
result is the JSON error envelope with code "drain_state_unknown" and
exit code 3.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return exitForCode(cmdRuntimeDrainCheck(args, jsonOutput, stdout, stderr))
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

func cmdRuntimeDrainCheck(args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	var (
		target  sessionRuntimeTarget
		readRow func() (bool, error)
	)
	if len(args) > 0 {
		named, cfg, err := resolveDrainCheckNamedTarget(args[0], stderr)
		switch {
		case errors.Is(err, session.ErrSessionNotFound):
			// Only a store that opened can say a session does not exist.
			fmt.Fprintf(stderr, "gc runtime drain-check: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		case err != nil:
			return writeRuntimeDrainCheckUnknown(args[0], []string{drainCheckFailure(drainCheckSourceRow, err)}, jsonOutput, stdout, stderr)
		}
		target = named
		readRow = drainCheckRowReader(target.cityPath, func() (*config.City, error) { return cfg, nil }, target.sessionName, target.sessionID)
	} else {
		current, rowName, err := currentDrainCheckTarget()
		switch {
		case isControlPlaneDescriptorError(err):
			// The session's city is there, but its plane is unknown, so no
			// source can answer: unknown, not "not draining".
			return writeRuntimeDrainCheckUnknown(defaultSessionDisplayIdentity(), []string{drainCheckFailure(drainCheckSourceRow, err)}, jsonOutput, stdout, stderr)
		case err != nil:
			return 1 // no session to check; silent without context, as before
		}
		target = current
		readRow = drainCheckRowReader(target.cityPath, func() (*config.City, error) {
			return loadCityConfigWithoutBuiltinPackRefresh(current.cityPath, io.Discard)
		}, rowName, target.sessionID)
	}
	sp, err := newSessionProvider()
	if err != nil {
		return reportRuntimeDrainCheck(false, fmt.Errorf("opening the session provider: %w", err),
			readRow, target.display, target.sessionName, jsonOutput, stdout, stderr)
	}
	return doRuntimeDrainCheck(newDrainOps(sp), readRow, target.display, target.sessionName, jsonOutput, stdout, stderr)
}

// resolveDrainCheckNamedTarget resolves drain-check's named session and
// returns the city config it loaded. session.ErrSessionNotFound means a store
// that opened has no such session; any other error means drain-check could not
// find out.
func resolveDrainCheckNamedTarget(identifier string, stderr io.Writer) (sessionRuntimeTarget, *config.City, error) {
	cityPath, err := resolveCity()
	if err != nil {
		return sessionRuntimeTarget{}, nil, fmt.Errorf("resolving the city: %w", err)
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		return sessionRuntimeTarget{}, nil, fmt.Errorf("loading the city config: %w", err)
	}
	target, err := resolveSessionRuntimeTargetInCity(cityPath, cfg, identifier)
	if err != nil {
		return sessionRuntimeTarget{}, nil, err
	}
	return target, cfg, nil
}

// currentDrainCheckTarget resolves drain-check's current session context and
// the name its session row is looked up by when GC_SESSION_ID is unset. That
// name is GC_SESSION_NAME, else the display identity; never GC_TMUX_SESSION,
// which in a container names the tmux session inside it ("main"), not the row.
func currentDrainCheckTarget() (sessionRuntimeTarget, string, error) {
	target, err := currentSessionRuntimeTarget()
	if err != nil {
		return sessionRuntimeTarget{}, "", err
	}
	target.sessionID = strings.TrimSpace(os.Getenv("GC_SESSION_ID"))
	rowName := strings.TrimSpace(os.Getenv("GC_SESSION_NAME"))
	if rowName == "" {
		rowName = target.display
	}
	return target, rowName, nil
}

// drainCheckOpenSessionLeg is a mutable global test seam over
// openCitySessionLeg. Tests that swap it MUST NOT call t.Parallel().
var drainCheckOpenSessionLeg = openCitySessionLeg

// drainCheckRowReader returns the session-row source: it loads the city
// config through loadCfg, opens the city's routed session leg and reads the
// row by sessionID, or by rowName when sessionID is empty.
func drainCheckRowReader(cityPath string, loadCfg func() (*config.City, error), rowName, sessionID string) func() (bool, error) {
	return func() (bool, error) {
		cfg, err := loadCfg()
		if err != nil {
			return false, fmt.Errorf("loading the city config: %w", err)
		}
		sessStore, err := drainCheckOpenSessionLeg(cityPath, cfg)
		if err != nil {
			return false, err
		}
		return drainCheckRowDraining(sessionFrontDoor(sessStore), rowName, sessionID)
	}
}

// drainCheckRowDraining reports whether the session row is in state draining.
// An empty sessionID resolves the row by sessionName, live rows first and then
// closed ones, so a closed row reads the same by name as by id: it is not
// draining. A row that cannot be found or read is an error.
func drainCheckRowDraining(front *session.Store, sessionName, sessionID string) (bool, error) {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		resolved, err := front.ResolveIDAllowClosed(sessionName)
		if err != nil {
			return false, fmt.Errorf("resolving session %q: %w", sessionName, err)
		}
		id = resolved
	}
	state, closed, err := front.GetState(id)
	if err != nil {
		return false, fmt.Errorf("reading session %q: %w", id, err)
	}
	return !closed && state == session.StateDraining, nil
}

// drainCheckRuntimeAbsent reports whether a GC_DRAIN read failed because the
// runtime has no such session, or keeps no metadata: definitive answers of "no
// GC_DRAIN". Only the typed sentinels count; an error that merely reads like
// absence (an adapter's "command not found") or that also says the runtime
// could not be observed is a failure to answer.
//
// The worker-site guard's refusal is "meta not applicable" too: a box holds no
// runtime meta, so the session row alone answers there. In-box drain-ack has
// no path under the legacy reconciler (D-DRAINACK); the guard makes that loud
// instead of reading a box-local runtime.
func drainCheckRuntimeAbsent(err error) bool {
	if errors.Is(err, runtime.ErrMetaUnsupported) || errors.Is(err, guard.ErrControllerOwnedRuntime) {
		return true
	}
	return errors.Is(err, runtime.ErrSessionNotFound) && !errors.Is(err, runtime.ErrRuntimeUnavailable)
}

// doRuntimeDrainCheck reads the runtime's GC_DRAIN through dops and, unless
// that already says draining, the session row through readRow. It returns 0
// if a source that answered says draining, 1 if both answered and neither
// does, and drainCheckUnknownExit otherwise. Silent on stdout without
// --json — designed for `if gc runtime drain-check; then ...`.
func doRuntimeDrainCheck(dops drainOps, readRow func() (bool, error), targetName, sn string, jsonOutput bool, stdout, stderr io.Writer) int {
	metaDraining, metaErr := dops.isDraining(sn)
	if drainCheckRuntimeAbsent(metaErr) {
		metaDraining, metaErr = false, nil
	}
	return reportRuntimeDrainCheck(metaDraining, metaErr, readRow, targetName, sn, jsonOutput, stdout, stderr)
}

// reportRuntimeDrainCheck combines the runtime answer with the session row's
// and writes the result. The row is not read when the runtime already says
// draining.
func reportRuntimeDrainCheck(metaDraining bool, metaErr error, readRow func() (bool, error),
	targetName, sn string, jsonOutput bool, stdout, stderr io.Writer,
) int {
	if metaErr == nil && metaDraining {
		return writeRuntimeDrainCheck(true, drainCheckSourceRuntime, targetName, sn, jsonOutput, stdout, stderr)
	}
	rowDraining, rowErr := readRow()
	switch {
	case rowErr == nil && rowDraining:
		return writeRuntimeDrainCheck(true, drainCheckSourceRow, targetName, sn, jsonOutput, stdout, stderr)
	case rowErr == nil && metaErr == nil:
		return writeRuntimeDrainCheck(false, drainCheckSourceBoth, targetName, sn, jsonOutput, stdout, stderr)
	}
	var failed []string
	if rowErr != nil {
		failed = append(failed, drainCheckFailure(drainCheckSourceRow, rowErr))
	}
	if metaErr != nil {
		failed = append(failed, drainCheckFailure(drainCheckSourceRuntime, metaErr))
	}
	return writeRuntimeDrainCheckUnknown(targetName, failed, jsonOutput, stdout, stderr)
}

// drainCheckFailure names a source that could not answer and why.
func drainCheckFailure(source string, err error) string {
	return fmt.Sprintf("%s: %v", source, err)
}

// writeRuntimeDrainCheckUnknown writes the unknown result for the failed
// sources as one line: on stderr, or as the JSON error envelope under --json.
// Embedded errors (an adapter's stderr, a multi-line store error) are
// collapsed onto that line.
func writeRuntimeDrainCheckUnknown(targetName string, failed []string, jsonOutput bool, stdout, stderr io.Writer) int {
	msg := fmt.Sprintf("gc runtime drain-check: drain state of %q unknown: %s", targetName, strings.Join(failed, "; "))
	msg = strings.Join(strings.Fields(msg), " ")
	if jsonOutput {
		return writeJSONError(stdout, stderr, drainCheckUnknownCode, msg, drainCheckUnknownExit)
	}
	fmt.Fprintln(stderr, msg) //nolint:errcheck // best-effort stderr
	return drainCheckUnknownExit
}

// writeRuntimeDrainCheck writes a definitive drain-check answer and returns
// its exit code: 0 when draining, 1 when not.
func writeRuntimeDrainCheck(draining bool, source, targetName, sn string, jsonOutput bool, stdout, stderr io.Writer) int {
	code := 1
	if draining {
		code = 0
	}
	if !jsonOutput {
		return code
	}
	if err := writeCLIJSONLine(stdout, runtimeDrainCheckJSON{
		SchemaVersion: "1",
		OK:            true,
		Command:       "runtime drain-check",
		Session:       sn,
		Target:        targetName,
		Draining:      draining,
		Source:        source,
	}); err != nil {
		fmt.Fprintf(stderr, "gc runtime drain-check: writing JSON: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return code
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

Under the v2 session reconciler the ack is first written to the session
row, bound to its current incarnation. A session acking itself must
carry a GC_INSTANCE_TOKEN that matches the row; an operator acking a
session from outside it passes --operator and a target, and the ack binds
to the incarnation the command read. A missing or stale token, or a store
that is unreachable or keeps changing, exits 1 with nothing acknowledged.
Under the legacy reconciler the ack is read back after the write; an ack
that did not persist, or cannot be read back from a live session, exits 1
with nothing acknowledged.`,
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
	// The current session's target is name-only (empty sessionID): the
	// env-derived GC_SESSION_ID can be stale.
	var target sessionRuntimeTarget
	var err error
	if len(args) > 0 {
		target, err = resolveSessionRuntimeTarget(args[0], stderr)
	} else {
		target, err = currentSessionRuntimeTarget()
	}
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime drain-ack: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	sp, err := newSessionProvider()
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime drain-ack: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return runtimeDrainAck(newDrainOps(sp), target, operator, os.Getenv("GC_INSTANCE_TOKEN"), jsonOutput, stdout, stderr)
}

// drainAckCaller is who runs a drain-ack: an operator (--operator), or the
// session itself, and the session identity its environment carries.
type drainAckCaller struct {
	operator         bool
	sessionID, token string // the caller's GC_SESSION_ID and GC_INSTANCE_TOKEN
}

// names reports whether the caller's own environment names the session
// bead b: an --operator ack from inside the session it targets is that
// session's own ack.
func (c drainAckCaller) names(b beads.Bead) bool {
	id, token := strings.TrimSpace(c.sessionID), strings.TrimSpace(c.token)
	return (id != "" && id == b.ID) || (token != "" && token == strings.TrimSpace(b.Metadata["instance_token"]))
}

// drainAckCallerSessionID is the caller's GC_SESSION_ID, a seam for tests,
// which MUST NOT call t.Parallel() when they swap it.
var drainAckCallerSessionID = callerSessionIDFromEnv

// callerSessionIDFromEnv is the calling process's GC_SESSION_ID.
func callerSessionIDFromEnv() string { return os.Getenv("GC_SESSION_ID") }

// drainAckOwnOperator is the refusal of an --operator ack the caller makes
// on its own session.
const drainAckOwnOperator = "--operator acks another session from outside it, and the caller's GC_SESSION_ID or GC_INSTANCE_TOKEN names this one; nothing acknowledged"

// runtimeDrainAck decides the city's mode once. A legacy city runs
// doRuntimeDrainAck exactly as before E3. A v2 city keeps legacy's order and
// binds the ack to the session row (CONTRACT v5.7 D5): the incarnation check,
// which writes nothing; the held-claim release; the row CAS; then the env ack
// and the poke. A refused check releases nothing. A CAS that loses after the
// release exits non-zero with no row or env write; the claims it released
// stay released, which is safe because they were unexecuted. envToken is the
// caller's GC_INSTANCE_TOKEN.
func runtimeDrainAck(dops drainOps, target sessionRuntimeTarget, operator bool, envToken string, jsonOutput bool, stdout, stderr io.Writer) int {
	caller := drainAckCaller{operator: operator, sessionID: drainAckCallerSessionID(), token: envToken}
	if operator && caller.sessionID != "" && caller.sessionID == strings.TrimSpace(target.sessionID) {
		fmt.Fprintf(stderr, "gc runtime drain-ack: %s\n", drainAckOwnOperator) //nolint:errcheck // best-effort stderr
		return drainAckRefused
	}
	cfg, strict := drainAckCityMode(target.cityPath)
	if !strict {
		return doRuntimeDrainAck(dops, target.cityPath, target.display, target.sessionName, target.sessionID, caller, jsonOutput, stdout, stderr)
	}
	commit, code := checkDrainAckRowAt(target.cityPath, cfg, target.sessionName, target.sessionID, operator, envToken, stderr)
	if code != 0 {
		return code
	}
	if refusal := drainAckReleaseHeldClaims(target.cityPath, target.sessionName, caller, stderr); refusal != "" {
		fmt.Fprintf(stderr, "gc runtime drain-ack: %s\n", refusal) //nolint:errcheck // best-effort stderr
		return drainAckRefused
	}
	if commit != nil {
		if err := commit(); err != nil {
			fmt.Fprintf(stderr, "gc runtime drain-ack: held claims released; row ack not written, nothing acknowledged (safe to re-run): %v\n", err) //nolint:errcheck // best-effort stderr
			return drainAckRefused
		}
	}
	return finishRuntimeDrainAck(rowAckedDrainOps{drainOps: dops, stderr: stderr}, target.cityPath, target.display, target.sessionName, target.sessionID, jsonOutput, stdout, stderr)
}

// ---------------------------------------------------------------------------
// gc runtime request-restart
// ---------------------------------------------------------------------------

func newRuntimeRequestRestartCmd(stdout, stderr io.Writer) *cobra.Command {
	var key string
	cmd := &cobra.Command{
		Use:   "request-restart",
		Short: "Request controller restart this session (returns immediately)",
		Long: `Ask the controller to stop this session and start it again fresh.

Records the restart request on this session's row (the request the
controller acts on). Outside a worker site it then sets the runtime's
GC_RESTART_REQUESTED hint. Then it pokes the controller for an immediate
reconcile. It returns without waiting for the restart.

The request is bound to this session's incarnation. It is recorded only while
the session row, or the live runtime under the session's name, still carries
the GC_INSTANCE_TOKEN this process runs under, so a process left over from an
earlier incarnation cannot restart its successor. An agent CLI that scrubs
*TOKEN* variables from the shells it spawns (for example Claude Code with
CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1) removes GC_INSTANCE_TOKEN, and the
request is refused; keep that variable in the agent's subprocess environment.

Exits 0 once the request is recorded, even when the controller could not be
signaled: a warning says so, and the controller acts at its next pass. Exits 1
with no restart requested and no runtime hint set when the request cannot be
recorded: no session context or GC_INSTANCE_TOKEN, an unreachable session
store, a session whose row and live runtime name another incarnation, or a
session store that cannot compare-and-swap where the request needs it: a v2
city records the request only by compare-and-swap, and
[beads] conditional_writes = "require" refuses any write the store cannot
fence.

While the op journal records lifecycle ops, the request is recorded as a
restart{fresh} op, which closes with the request's outcome, a refusal
included; a rerun with the same --key reports the first run's outcome instead
of requesting again. The key is request-restart's own: gc handoff or gc
session reset under the same --key records another op. The restart stops this
session, and so can stop the run before it closes its op: a rerun with the
same --key is then refused as in progress until the op's deadline passes, and
after that replays the first run's success while the session row still
records the restart as pending, else reports the op's failure, also once the
restart has run, since the op records no context to tell it by.

This command is designed to be called from within a session context.
It emits a session.draining event once the request is recorded.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdRuntimeRequestRestart(key, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	addOpKeyFlag(cmd, &key, restartOpKeyNote)
	return cmd
}

// cmdRuntimeRequestRestart records this session's restart request row first
// (SG-3), as a restart{fresh} op under the op key key (empty mints one; OJ-6).
func cmdRuntimeRequestRestart(key string, stdout, stderr io.Writer) int {
	const verb = "gc runtime request-restart"
	current, err := currentSessionRuntimeTarget()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", verb, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	r := rowFirstRestart{
		verb:        verb,
		cityPath:    current.cityPath,
		sessionName: current.sessionName,
		display:     current.display,
		token:       os.Getenv(runtime.InstanceTokenEnv),
		poke:        pokeControllerForRestart,
		stderr:      stderr,
	}
	// No token refuses before anything is opened or read.
	if strings.TrimSpace(r.token) == "" {
		fmt.Fprintf(stderr, "%s: %s\n", verb, r.notRecorded(session.ErrRestartRequestUnfenced)) //nolint:errcheck // best-effort stderr
		return 1
	}
	sp, err := newSessionProvider()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %s\n", verb, r.notRecorded(err)) //nolint:errcheck // best-effort stderr
		return 1
	}
	store, err := openCityStoreAt(current.cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %s\n", verb, r.notRecorded(fmt.Errorf("opening store: %w", err))) //nolint:errcheck // best-effort stderr
		return 1
	}
	// Route the SESSION-class access (the restart record, through the worker
	// boundary) to the session coordination-class store so a
	// [beads.classes.sessions] relocation reaches gc runtime request-restart.
	// The routing cfg is loaded refresh-free (the full cfg loads later, for
	// template resolution).
	routeCfg, _ := loadCityConfigWithoutBuiltinPackRefresh(current.cityPath, io.Discard)
	sessStore := cliSessionStore(store, routeCfg, current.cityPath)
	rec := openCityRecorderAt(current.cityPath, stderr)
	cfg, cfgErr := loadCityConfig(current.cityPath, stderr)
	r.record = sessionRestartRecorder(current.cityPath, sessStore, sp, cfg, current.sessionName)
	r.hint = restartHintOps(current.cityPath, sp)
	r.ops = newRestartOps(cfg, cfgErr, sessStore, key, current.sessionName, current.display, false, stdout, stderr)
	return doRuntimeRequestRestart(r, rec, stdout)
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

// doRuntimeRequestRestart records the restart request row first (SG-3), then
// emits session.draining and pokes the controller, returning without waiting
// for the controller to act. Control-plane authority over the actual
// stop/start stays with the controller's reconcile loop.
//
// It exits 0 once the request is recorded, whether or not the controller was
// notified, and 1 when nothing was recorded: then no runtime hint is set, no
// event is recorded and the controller is not poked (a v2 refusal on a store
// with no conditional writer has asked it for identify, nothing more).
//
// A recorded request (r.ops) proposes its restart op first: a refusal exits 1
// before any effect, and a replay of a successful op prints the success line
// and exits 0 without acting again.
func doRuntimeRequestRestart(r rowFirstRestart, rec events.Recorder, stdout io.Writer) int {
	switch r.ops.startRestart(r.verb) {
	case cliOpRefused:
		return 1
	case cliOpReplayed:
		fmt.Fprint(stdout, "Restart requested.\n") //nolint:errcheck // best-effort stdout
		return 0
	}
	defer r.ops.end()
	if err := r.recordRestart(); err != nil {
		fmt.Fprintf(r.stderr, "%s: %s\n", r.verb, r.notRecorded(err)) //nolint:errcheck // best-effort stderr
		return 1
	}
	rec.Record(events.Event{
		Type:    events.SessionDraining,
		Actor:   r.display,
		Subject: r.display,
		Message: "restart requested by session",
	})
	switch r.notifyController() {
	case restartNotified:
		fmt.Fprint(stdout, "Restart requested; controller notified for immediate reconcile.\n") //nolint:errcheck // best-effort stdout
	case restartRemote:
		fmt.Fprint(stdout, "Restart requested; the remote controller restarts this session at its next reconcile.\n") //nolint:errcheck // best-effort stdout
	default:
		fmt.Fprint(stdout, "Restart requested; the controller restarts this session at its next pass.\n") //nolint:errcheck // best-effort stdout
	}
	return 0
}

// restartFlagNotApplicable reports whether err is a worker site's guard
// refusing the runtime restart flag. The box holds no runtime to set it on or
// clear it from; the bead restart marker, which the remote controller's
// reconciler reads, is the whole request there.
func restartFlagNotApplicable(err error) bool {
	return errors.Is(err, guard.ErrControllerOwnedRuntime)
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
// A seat's own ack, and an --operator ack whose caller's environment names
// the seat, is the seat saying it holds nothing. It is refused, with nothing
// released, while the seat holds started work the release would keep
// (PHASE1-2 §6.5, OD9: the hook's drain-ack fence, for the direct command):
// acking would stop the seat with that claim kept on it, the assigned-work
// wake would restart it, and the cycle would repeat. An operator acks from
// outside the session, and the --operator self-ack is refused outright. The
// refusal is returned; "" is a pass that ran, or could not begin.
//
// The leg set mirrors `gc session close`, which leads with the WORK store and
// hands in the relocated graph binding as a class leg: a claim that claim-time
// routing left in the binding is invisible to a work-led scan, and would be
// released by nothing. Best-effort throughout — a city that cannot be resolved
// or a store that cannot be opened must never block the ack itself, which is the
// signal the controller is waiting on.
func releaseUnexecutedClaimsForSession(cityPath, sessionName string, caller drainAckCaller, stderr io.Writer) string {
	var refusal string
	openDrainAckCity(cityPath, sessionName, stderr, func(cfg *config.City, store beads.Store, rigStores func() map[string]beads.Store) {
		refusal = releaseUnexecutedClaimsForSessionStore(cityPath, cfg, store, rigStores, sessionName, caller, stderr)
	})
	return refusal
}

// openDrainAckCity opens the city at cityPath for a drain-ack's pass over
// sessionName's claims and hands visit its config, its store and its rig
// legs. It calls nothing when either is empty, cityPath is no city, or its
// store does not open.
func openDrainAckCity(cityPath, sessionName string, stderr io.Writer, visit func(cfg *config.City, store beads.Store, rigStores func() map[string]beads.Store)) {
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
	visit(cfg, store, rigStores)
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
// nothing for stores it would immediately discard. The refusal, and caller,
// are releaseUnexecutedClaimsForSession's.
func releaseUnexecutedClaimsForSessionStore(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	rigStores func() map[string]beads.Store,
	sessionName string,
	caller drainAckCaller,
	stderr io.Writer,
) string {
	legs, sessionBead, ok := drainAckSeat(cityPath, cfg, store, rigStores, sessionName)
	if !ok {
		return ""
	}
	if caller.operator && caller.names(sessionBead) {
		return drainAckOwnOperator
	}
	if held := releaseDrainAckClaims(legs, sessionBead, !caller.operator, drainAckReleaseBudget, stderr); len(held) > 0 {
		return fmt.Sprintf("this session holds %s, which it started and has not finished: continue it (an operator acks from outside this session with --operator); nothing acknowledged", strings.Join(held, ", "))
	}
	return ""
}

// drainAckSeat resolves sessionName to its session bead and the work legs
// its claims live on. A session that cannot be resolved holds nothing a
// drain-ack pass can find, so there is nothing to report (ok false): the ack
// is the signal the controller is waiting on, and a pass that could not begin
// must not decorate a successful ack with a warning an operator can do
// nothing about. A claim genuinely left behind is still caught by the
// dead-assignee lane.
func drainAckSeat(cityPath string, cfg *config.City, store beads.Store, rigStores func() map[string]beads.Store, sessionName string) (WorkLegs, beads.Bead, bool) {
	sessStore := cliSessionStore(store, cfg, cityPath)
	sessionID, err := resolveSessionID(sessStore, sessionName)
	if err != nil {
		return WorkLegs{}, beads.Bead{}, false
	}
	sessionBead, err := sessStore.Get(sessionID)
	if err != nil {
		return WorkLegs{}, beads.Bead{}, false
	}
	var rigs map[string]beads.Store
	if rigStores != nil {
		rigs = rigStores()
	}
	return workLegsFromCensus(cityPath, cfg, cityWorkLegOf(beads.WorkStore{Store: store}), rigs), sessionBead, true
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
func doRuntimeDrainAck(dops drainOps, cityPath, targetName, sn, sessionID string, caller drainAckCaller, jsonOutput bool, stdout, stderr io.Writer) int {
	if refusal := drainAckReleaseHeldClaims(cityPath, sn, caller, stderr); refusal != "" {
		fmt.Fprintf(stderr, "gc runtime drain-ack: %s\n", refusal) //nolint:errcheck // best-effort stderr
		return drainAckRefused
	}
	return finishRuntimeDrainAck(persistedAckDrainOps{drainOps: dops}, cityPath, targetName, sn, sessionID, jsonOutput, stdout, stderr)
}

// finishRuntimeDrainAck is the ack after the release: the env flag, the poke
// and the report.
func finishRuntimeDrainAck(dops drainOps, cityPath, targetName, sn, sessionID string, jsonOutput bool, stdout, stderr io.Writer) int {
	if err := dops.setDrainAck(sn); err != nil {
		if !runtime.OnlyMetadataNotMirrored(err) {
			fmt.Fprintf(stderr, "gc runtime drain-ack: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		// The ack is in force; only the runtime's view of it lags.
		fmt.Fprintf(stderr, "gc runtime drain-ack: warning: %v\n", err) //nolint:errcheck // best-effort stderr
	}
	workerSite := false
	if err := drainAckPokeController(cityPath, reconcilekey.SessionRef(sessionID, sn)); err != nil {
		if workerSite = noteWorkerSitePoke(stderr, "gc runtime drain-ack", err); !workerSite {
			fmt.Fprintf(stderr, "gc runtime drain-ack: warning: poke failed: %v\n", err) //nolint:errcheck // best-effort stderr
		}
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
	if workerSite {
		fmt.Fprintln(stdout, "Drain acknowledged. The remote controller stops this session at its next reconcile.") //nolint:errcheck // best-effort stdout
		return 0
	}
	fmt.Fprintln(stdout, "Drain acknowledged. Controller poked for immediate stop.") //nolint:errcheck // best-effort stdout
	return 0
}

// rowAckedDrainOps is the strict path's drainOps after the row ack committed.
// The row is the ack there; the env flag only mirrors it into the runtime. A
// worker site's guard holds no runtime to mirror into, so its
// ErrControllerOwnedRuntime is "not applicable" (a warning), as drain-check
// reads it, rather than failing an ack that is already in force.
type rowAckedDrainOps struct {
	drainOps
	stderr io.Writer
}

func (o rowAckedDrainOps) setDrainAck(sessionName string) error {
	err := o.drainOps.setDrainAck(sessionName)
	if errors.Is(err, guard.ErrControllerOwnedRuntime) {
		fmt.Fprintf(o.stderr, "gc runtime drain-ack: warning: the row ack is in force; no runtime here to mirror it into: %v\n", err) //nolint:errcheck // best-effort stderr
		return nil
	}
	return err
}

// persistedAckDrainOps is the legacy path's drainOps: the runtime flag is the
// ack there, so a write that reported success is read back. Its write join
// drops session-gone errors, and a write that reached the wrong tmux server, or
// none, can look gone (#8116); the read-back turns that into a failed ack.
// A read-back that finds the session gone is the benign reap race and passes.
type persistedAckDrainOps struct {
	drainOps
}

func (o persistedAckDrainOps) setDrainAck(sessionName string) error {
	if err := o.drainOps.setDrainAck(sessionName); err != nil {
		return err
	}
	acked, err := o.isDrainAcked(sessionName)
	if err != nil {
		if runtime.IsSessionGone(err) {
			return nil
		}
		return fmt.Errorf("drain ack on session %q could not be verified; held claims released, nothing acknowledged (safe to re-run): %w", sessionName, err)
	}
	if !acked {
		return fmt.Errorf("drain ack did not persist on session %q: GC_DRAIN_ACK is unset after the write; held claims released, nothing acknowledged (safe to re-run)", sessionName)
	}
	return nil
}

// drainAckRefused is runtimeDrainAck's status for a seat's own ack refused
// because it holds started work (either mode), and for a v2 ack that the row
// check or its CAS refused. Neither writes the row or the env ack; only a CAS
// refusal follows the held-claim release. The process still exits 1; the
// distinct value lets the hook tell a refusal from a failed ack.
const drainAckRefused = 2

// drainAckCityMode reports whether the city at cityPath runs the v2 session
// reconciler, the only reader of the row ack (cityRunsV2), and returns the
// config it read. A path that is not a city or a config that does not load is
// legacy.
func drainAckCityMode(cityPath string) (*config.City, bool) {
	if strings.TrimSpace(cityPath) == "" {
		return nil, false
	}
	if _, err := os.Stat(filepath.Join(cityPath, "city.toml")); err != nil {
		return nil, false
	}
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil || cfg == nil {
		return nil, false
	}
	return cfg, cityRunsV2(cityPath, cfg)
}

// cityRunsV2 reports whether the city at cityPath runs the v2 reconciler:
// the mode its running controller latched, when its identify reply tells
// (controllerLatchedReconcilerMode), else the mode cfg resolves to
// (drainAckStrictConfig), which the next controller start latches; legacy
// with neither. The running controller wins because the mode is
// boot-latched: a config edit that resolves the other way only takes effect
// at restart, and until then the drain marks, acks and wake verdicts must be
// the ones the running reconciler reads.
func cityRunsV2(cityPath string, cfg *config.City) bool {
	if mode, ok := controllerLatchedReconcilerMode(cityPath, cfg); ok {
		return mode == reconcilerV2
	}
	return cfg != nil && drainAckStrictConfig(cfg)
}

// drainAckStrictConfig reports whether cfg latches the v2 reconciler, as the
// controller latches it; a mode it would refuse to latch is legacy.
func drainAckStrictConfig(cfg *config.City) bool {
	mode, err := latchReconcilerMode(cfg, reconcilerModeLookupEnv)
	return err == nil && mode == reconcilerV2
}

// controllerLatchedReconcilerMode asks the city's running controller which
// session reconciler it latched (probeControllerIdentity). ok is false when
// no controller's identify reply parses: none runs, or only the ping fallback
// answered, as a current controller that missed the identify deadline does
// too, so a ping alone says nothing about the latch. A controller whose
// identify parsed without the mode predates both the field and the v2
// default, so with an unset key (or anything but an explicit "v2") it latched
// legacy; under an explicit v2 the answer is left to cfg's resolution. A var
// so tests can stand in a controller; tests that swap it MUST NOT call
// t.Parallel().
var controllerLatchedReconcilerMode = func(cityPath string, cfg *config.City) (reconcilerMode, bool) {
	identity := probeControllerIdentity(cityPath)
	switch identity.SessionReconciler {
	case config.SessionReconcilerV2:
		return reconcilerV2, true
	case config.SessionReconcilerLegacy:
		return reconcilerLegacy, true
	}
	if identity.HostingMode.known() && cfg != nil {
		if mode, _, ok := cfg.Daemon.SessionReconcilerMode(); !ok || mode != config.SessionReconcilerV2 {
			return reconcilerLegacy, true
		}
	}
	return reconcilerLegacy, false
}

// checkDrainAckRowAt is the v2 ack's incarnation check (CONTRACT v5.7 D5),
// before any side effect of the drain-ack. It writes nothing. On a match it
// returns the row CAS to run after the held-claim release, or a nil commit
// when the store has no conditional writer (with a warning: v2 refuses to
// boot on such a store, C0.7); otherwise drainAckRefused. The pane's token is
// compared in process and never leaves it.
func checkDrainAckRowAt(cityPath string, cfg *config.City, sessionName, sessionID string, operator bool, envToken string, stderr io.Writer) (func() error, int) {
	store, id, err := drainAckOpenSessionRow(cityPath, cfg, sessionName, sessionID)
	var commit func() error
	if err == nil {
		commit, err = checkDrainAckRow(store, id, operator, envToken, time.Now())
	}
	switch {
	case errors.Is(err, errDrainAckRowUnfenced):
		fmt.Fprintf(stderr, "gc runtime drain-ack: warning: %v\n", err) //nolint:errcheck // best-effort stderr
		return nil, 0
	case err != nil:
		fmt.Fprintf(stderr, "gc runtime drain-ack: refused, nothing acknowledged: %v\n", err) //nolint:errcheck // best-effort stderr
		return nil, drainAckRefused
	}
	return commit, 0
}

// drainAckOpenSessionRow is a mutable global test seam over
// drainAckSessionRow, matching drainAckPokeController. Tests that swap it
// MUST NOT call t.Parallel().
var drainAckOpenSessionRow = drainAckSessionRow

// drainAckSessionRow opens the session leg of the city and resolves the
// target's row. An empty sessionID resolves by name.
func drainAckSessionRow(cityPath string, cfg *config.City, sessionName, sessionID string) (beads.Store, string, error) {
	sessStore, err := openCitySessionLeg(cityPath, cfg)
	if err != nil {
		return nil, "", err
	}
	if sessionID == "" {
		if sessionID, err = resolveSessionID(sessStore, sessionName); err != nil {
			return nil, "", fmt.Errorf("resolving session %q: %w", sessionName, err)
		}
	}
	return sessStore, sessionID, nil
}

// openCitySessionLeg opens the store of the city at cityPath with cfg and
// routes it to the session class, so a [beads.classes.sessions] relocation is
// honored.
func openCitySessionLeg(cityPath string, cfg *config.City) (beads.Store, error) {
	store, err := openCityStoreAtWithConfig(cityPath, cfg)
	if err != nil {
		return nil, fmt.Errorf("opening the session store: %w", err)
	}
	if store == nil {
		return nil, errors.New("opening the session store: no store")
	}
	return cliSessionStore(store, cfg, cityPath), nil
}

// errDrainAckRowUnfenced reports a matching ack on a store with no
// conditional writer.
var errDrainAckRowUnfenced = errors.New("the session store has no conditional writer, so the ack is not written to the session row; the runtime ack is set as before")

// checkDrainAckRow reads the row once and decides the ack before anything is
// written; a refusal is returned as the error. The returned commit writes the
// ack by CAS, re-deciding on every attempt and bound to the generation this
// read saw, so a PreWake between this read and a write makes the commit refuse
// instead of acking the next incarnation (I-ACK-1). A match on a store with no
// conditional writer returns errDrainAckRowUnfenced; nothing is written blind.
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
	bound := patch[session.DrainAckIncarnationKey]
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
		if b.Metadata[session.DrainAckIncarnationKey] == "" && b.Metadata[session.DrainAckAtKey] == "" {
			return nil, nil
		}
		return map[string]string{session.DrainAckIncarnationKey: "", session.DrainAckAtKey: ""}, nil
	})
}

// casDrainAckRow is the kill fence's re-read/re-decide loop
// (session.ApplyKillFencePatch) without its unconditional fallback: every
// write is an UpdateIfMatch at the revision just read.
func casDrainAckRow(store beads.Store, writer beads.ConditionalWriter, sessionID string,
	decide func(beads.Bead) (map[string]string, error),
) error {
	var lastErr error
	for attempt := 0; attempt < session.KillFenceAttempts; attempt++ {
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
		return nil, fmt.Errorf("GC_INSTANCE_TOKEN does not match session %s: it has been restarted since this pane started, or this is not its pane (an operator passes --operator)", b.ID)
	case generation == "":
		return nil, fmt.Errorf("session %s has no generation to bind the ack to", b.ID)
	case bound != "" && generation != bound:
		return nil, fmt.Errorf("session %s was restarted while the ack was being written", b.ID)
	}
	return map[string]string{
		session.DrainAckIncarnationKey: generation,
		session.DrainAckAtKey:          now.UTC().Format(time.RFC3339),
	}, nil
}
