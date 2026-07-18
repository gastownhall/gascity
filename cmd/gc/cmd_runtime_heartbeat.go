package main

import (
	"fmt"
	"io"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/spf13/cobra"
)

const (
	// defaultHeartbeatDuration is the default idle-timeout extension when
	// --duration is not specified. 45 minutes covers long-running operations
	// that produce no terminal output.
	defaultHeartbeatDuration = 45 * time.Minute

	// minimumHeartbeatDuration prevents agents from setting arbitrarily short
	// holds that would expire before the next reconciler tick.
	minimumHeartbeatDuration = 1 * time.Minute

	// maximumHeartbeatDuration bounds how long a single heartbeat may suppress
	// the idle-timeout and max-session-age timers. A heartbeat is meant to be
	// refreshed by re-calling this command during a long operation, so no single
	// call needs an unbounded hold; the ceiling comfortably exceeds any realistic
	// silent operation while stopping an oversized --duration (e.g. 8760h) from
	// pinning a session's timers for an effectively unbounded window.
	maximumHeartbeatDuration = 12 * time.Hour
)

// validateHeartbeatDuration bounds a requested hold against the floor and
// ceiling. The floor keeps a hold from expiring before the next reconciler
// tick; the ceiling keeps an oversized --duration from pinning a session's
// idle-timeout / max-session-age timers for an unbounded window.
func validateHeartbeatDuration(d time.Duration) error {
	if d < minimumHeartbeatDuration {
		return fmt.Errorf("--duration must be at least %s", minimumHeartbeatDuration)
	}
	if d > maximumHeartbeatDuration {
		return fmt.Errorf("--duration must be at most %s", maximumHeartbeatDuration)
	}
	return nil
}

// newRuntimeHeartbeatCmd creates the "gc runtime heartbeat" command.
//
// Called by agents at the start of long operations to suppress idle-timeout
// and max-session-age timers for the specified duration. The existing
// held_until mechanism in the bead reconciler provides the timer-blocker
// semantics; this command is the agent-facing API for setting it without
// triggering a full user-hold suspend.
func newRuntimeHeartbeatCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		durationStr string
		jsonOutput  bool
	)
	cmd := &cobra.Command{
		Use:   "heartbeat",
		Short: "Extend idle-timeout window during a long operation",
		Long: `Extend the idle-timeout and max-session-age windows during a long operation.

Sets held_until on the current session's bead, suppressing the idle-timeout
and max-session-age timers until the hold expires. Call this at the start of
slow operations that produce no terminal output and would otherwise trigger
a false-alarm watchdog kill.

The hold is automatically cleared by the reconciler once held_until passes.
This is the agent-facing API for the held_until bead-metadata mechanism; it
does not put the session into a suspended state or change its sleep_intent.

The default duration (` + defaultHeartbeatDuration.String() + `) covers long-running operations.
Pass --duration to override.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			d := defaultHeartbeatDuration
			if durationStr != "" {
				var err error
				d, err = time.ParseDuration(durationStr)
				if err != nil {
					fmt.Fprintf(stderr, "gc runtime heartbeat: invalid --duration: %v\n", err) //nolint:errcheck
					return errExit
				}
				if err := validateHeartbeatDuration(d); err != nil {
					fmt.Fprintf(stderr, "gc runtime heartbeat: %v\n", err) //nolint:errcheck
					return errExit
				}
			}
			if cmdRuntimeHeartbeat(d, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&durationStr, "duration", "", "hold duration (e.g. 30m, 1h); default "+defaultHeartbeatDuration.String())
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

// runtimeHeartbeatJSON is the JSON output shape for gc runtime heartbeat.
type runtimeHeartbeatJSON struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	Command       string `json:"command"`
	Session       string `json:"session"`
	HeldUntil     string `json:"held_until"`
}

func cmdRuntimeHeartbeat(duration time.Duration, jsonOutput bool, stdout, stderr io.Writer) int {
	current, err := currentSessionRuntimeTarget()
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime heartbeat: %v\n", err) //nolint:errcheck
		return 1
	}

	store, err := openCityStoreAt(current.cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime heartbeat: opening store: %v\n", err) //nolint:errcheck
		return 1
	}

	// Route the SESSION-class access (held_until resolve + write) to the session
	// coordination-class store so a [beads.classes.sessions] relocation reaches
	// gc runtime heartbeat the same way it reaches gc runtime request-restart.
	// The routing cfg loads refresh-free; it is identity to the input store at
	// the default single-store backend, so this is byte-identical until a
	// session relocation is configured.
	routeCfg, _ := loadCityConfigWithoutBuiltinPackRefresh(current.cityPath, io.Discard)
	sessStore := cliSessionStore(store, routeCfg, current.cityPath)

	return doRuntimeHeartbeat(sessStore, duration, current.display, current.sessionName, jsonOutput, stdout, stderr)
}

// doRuntimeHeartbeat sets held_until on the session bead to suppress
// idle-timeout and max-session-age timers for the specified duration.
// Extracted for testability.
func doRuntimeHeartbeat(store beads.Store, duration time.Duration, display, sessionName string, jsonOutput bool, stdout, stderr io.Writer) int {
	sessionID, err := session.ResolveSessionID(store, sessionName)
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime heartbeat: resolving session %q: %v\n", display, err) //nolint:errcheck
		return 1
	}

	heldUntil := time.Now().Add(duration).UTC().Format(time.RFC3339)
	if err := store.SetMetadataBatch(sessionID, map[string]string{
		"held_until": heldUntil,
	}); err != nil {
		fmt.Fprintf(stderr, "gc runtime heartbeat: setting hold: %v\n", err) //nolint:errcheck
		return 1
	}

	if jsonOutput {
		if err := writeCLIJSONLine(stdout, runtimeHeartbeatJSON{
			SchemaVersion: "1",
			OK:            true,
			Command:       "runtime heartbeat",
			Session:       display,
			HeldUntil:     heldUntil,
		}); err != nil {
			fmt.Fprintf(stderr, "gc runtime heartbeat: writing JSON: %v\n", err) //nolint:errcheck
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "Heartbeat set: idle-timeout suppressed until %s\n", heldUntil) //nolint:errcheck
	return 0
}
