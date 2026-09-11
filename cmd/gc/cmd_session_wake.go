package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/spf13/cobra"
)

// newSessionWakeCmd creates the "gc session wake <id-or-alias>" command.
func newSessionWakeCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "wake <session-id-or-alias>",
		Short: "Wake a session (request start and clear holds)",
		Long: `Request wake for a session and release user hold or crash-loop quarantine metadata.

After waking, the reconciler will start the session on its next tick
if it has wake reasons (e.g., a matching config agent). If the session
has no wake reasons, it remains asleep.

Accepts a session ID (e.g., gc-42) or session alias (e.g., mayor).`,
		Example: `  gc session wake gc-42
  gc session wake mayor`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdSessionWake(args, stdout, stderr, jsonOutput) != 0 {
				return errExit
			}
			return nil
		},
		ValidArgsFunction: completeSessionIDs,
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit JSONL")
	return cmd
}

type sessionWakeDeps struct {
	store                     beads.Store
	cfg                       *config.City
	cityPath                  string
	cityResolved              bool
	now                       func() time.Time
	withdrawQueuedWaitNudges  func(string, []string) error
	cityUsesManagedReconciler func(string) bool
	pokeController            func(string) error
}

// cmdSessionWake is the CLI entry point for "gc session wake".
func cmdSessionWake(args []string, stdout, stderr io.Writer, jsonOutput ...bool) int {
	asJSON := sessionJSONRequested(jsonOutput)
	store, code := openCityStore(stderr, "gc session wake")
	if store == nil {
		return code
	}

	cityPath, cityErr := resolveCity()
	var cfg *config.City
	if cityErr == nil {
		cfg, _ = loadCityConfig(cityPath, stderr)
	}
	return doSessionWake(args[0], stdout, stderr, asJSON, sessionWakeDeps{
		store:                     store,
		cfg:                       cfg,
		cityPath:                  cityPath,
		cityResolved:              cityErr == nil,
		now:                       time.Now,
		withdrawQueuedWaitNudges:  withdrawQueuedWaitNudges,
		cityUsesManagedReconciler: cityUsesManagedReconciler,
		pokeController:            pokeController,
	})
}

func doSessionWake(target string, stdout, stderr io.Writer, asJSON bool, deps sessionWakeDeps) int {
	sessStore := cliSessionStore(deps.store, deps.cfg, deps.cityPath)
	id, err := resolveSessionIDMaterializingNamed(deps.cityPath, deps.cfg, sessStore, target)
	if err != nil {
		fmt.Fprintf(stderr, "gc session wake: %v\n", err) //nolint:errcheck
		return 1
	}

	sessFront := sessionFrontDoor(sessStore)
	res, err := sessFront.WakeSession(id, deps.now().UTC(), session.WakeOpts{})
	if err != nil {
		if state, conflict := session.WakeConflictState(err); conflict {
			fmt.Fprintf(stderr, "gc session wake: session %s is %s\n", id, state) //nolint:errcheck
			return 1
		}
		switch {
		case errors.Is(err, session.ErrNotSessionBead):
			fmt.Fprintf(stderr, "gc session wake: %s is not a session\n", id) //nolint:errcheck
		case errors.Is(err, beads.ErrNotFound):
			fmt.Fprintf(stderr, "gc session wake: %v\n", err) //nolint:errcheck
		default:
			fmt.Fprintf(stderr, "gc session wake: updating metadata: %v\n", err) //nolint:errcheck
		}
		return 1
	}
	nudgeIDs := res.NudgeIDs
	hasRunnableTemplate := sessionWakeHasRunnableTemplateInfo(res.Info, deps.cfg)
	startupTimeout := time.Duration(0)
	if deps.cfg != nil {
		startupTimeout = deps.cfg.Session.StartupTimeoutDuration()
	}
	createAbandoned := sessionWakeCreateAbandonedInfo(res.Info, startupTimeout)
	rejectStuck := false
	switch {
	case !hasRunnableTemplate && (sessionWakeRequestedCreateInfo(res.Info) || createAbandoned):
		if err := sessFront.ApplyPatch(id, map[string]string{
			"state":                     string(session.StateAsleep),
			"state_reason":              "",
			"pending_create_claim":      "",
			"pending_create_started_at": "",
			"wake_request":              "",
			"wake_requested_at":         "",
		}); err != nil {
			fmt.Fprintf(stderr, "gc session wake: updating metadata: %v\n", err) //nolint:errcheck
			return 1
		}
	// WakeSession has already recorded the wake on the bead (wake_request=explicit,
	// wake_requested_at set, quarantine cleared) before this arm is reached, so the
	// nonzero exit reports "wake cannot complete", not "nothing happened". The exit
	// is deferred to after the cleanup block below so the waits WakeSession already
	// canceled still get their queued nudges withdrawn.
	case hasRunnableTemplate && createAbandoned:
		since := "an unknown time"
		if started := stuckCreatingSinceInfo(res.Info); !started.IsZero() {
			since = started.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(stderr, "gc session wake: session %s has been in state %q since %s without completing its create; the wake request was recorded but cannot complete now. If its runtime is gone, use `gc session close` to release the slot.\n", id, res.Info.MetadataState, since) //nolint:errcheck
		rejectStuck = true
	}
	if deps.cityResolved {
		if err := deps.withdrawQueuedWaitNudges(deps.cityPath, nudgeIDs); err != nil {
			fmt.Fprintf(stderr, "gc session wake: warning: withdrawing queued wait nudges: %v\n", err) //nolint:errcheck
		}
		if deps.cityUsesManagedReconciler(deps.cityPath) {
			if err := deps.pokeController(deps.cityPath); err != nil {
				fmt.Fprintf(stderr, "gc session wake: warning: poke failed: %v\n", err) //nolint:errcheck
			}
		}
	}
	if rejectStuck {
		return 1
	}

	if asJSON {
		if err := writeSessionActionJSON(stdout, sessionActionResult{
			Action:              "wake",
			SessionID:           id,
			State:               "wake_requested",
			WaitNudgesWithdrawn: len(nudgeIDs),
		}); err != nil {
			fmt.Fprintf(stderr, "gc session wake: %v\n", err) //nolint:errcheck
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "Session %s: wake requested.\n", id) //nolint:errcheck
	return 0
}

func sessionWakeHasRunnableTemplateInfo(info session.Info, cfg *config.City) bool {
	if cfg == nil {
		return true
	}
	template := normalizedSessionTemplateInfo(info, cfg)
	if template == "" {
		template = info.Template
	}
	return findAgentByTemplate(cfg, template) != nil
}

func sessionWakeRequestedCreateInfo(info session.Info) bool {
	state := session.State(strings.TrimSpace(info.MetadataState))
	return state == session.StateSuspended || state == session.StateDrained
}

// sessionWakeStuckInFlightInfo reports whether info was already mid-create
// (creating or start-pending) before this wake call. Unlike
// sessionWakeRequestedCreateInfo, it deliberately excludes suspended/drained:
// those are the normal, successful wake path when a runnable template exists,
// and must not be treated as stuck.
func sessionWakeStuckInFlightInfo(info session.Info) bool {
	state := session.State(strings.TrimSpace(info.MetadataState))
	return state == session.StateCreating || state == session.StateStartPending
}

// sessionWakeCreateAbandonedInfo reports whether an in-flight create is
// genuinely abandoned and so may be acted on by the CLI.
//
// Mirrors the sweep's gate order in city_runtime.go:2853-2857: the
// pending-create lease is checked FIRST, staleness second. Checking only
// staleness rejects a create the reconciler still protects, because
// pendingCreateNeverStartedTimeout (10m) is deliberately longer than
// staleCreatingStateTimeout (1m).
func sessionWakeCreateAbandonedInfo(info session.Info, startupTimeout time.Duration) bool {
	return sessionWakeStuckInFlightInfo(info) &&
		!pendingCreateClaimStillLeasedForSweepInfo(info, startupTimeout) &&
		isStaleCreatingInfo(info)
}

// stuckCreatingSinceInfo returns the timestamp isStaleCreatingInfo measures
// staleness from (the per-attempt pending_create_started_at marker, falling
// back to CreatedAt), so the CLI's rejection message names exactly what was
// checked instead of duplicating the staleness decision itself.
func stuckCreatingSinceInfo(info session.Info) time.Time {
	if started, ok := parseRFC3339Metadata(info.PendingCreateStartedAt); ok {
		return started
	}
	return info.CreatedAt
}
