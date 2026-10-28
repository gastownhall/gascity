package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/spf13/cobra"
)

// newSessionWakeCmd creates the "gc session wake <id-or-alias>" command.
func newSessionWakeCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	var key string
	cmd := &cobra.Command{
		Use:   "wake <session-id-or-alias>",
		Short: "Wake a session (request start and clear holds)",
		Long: `Request wake for a session and release user hold or crash-loop quarantine metadata.

After waking, the reconciler will start the session on its next tick
if it has wake reasons (e.g., a matching config agent). If the session
has no wake reasons, it remains asleep.

When a configured named session has no session to wake or reopen, the wake
creates one as it resolves the target, before the wake is recorded. While the
ops.lifecycle gate is require, a wake whose op the journal then cannot record
is refused (op_journal_unavailable): the wake did not run, but the session it
created stays and may start.

Accepts a session ID (e.g., gc-42) or session alias (e.g., mayor).`,
		Example: `  gc session wake gc-42
  gc session wake mayor`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdSessionWake(args, key, stdout, stderr, jsonOutput) != 0 {
				return errExit
			}
			return nil
		},
		ValidArgsFunction: completeSessionIDs,
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit JSONL")
	addOpKeyFlag(cmd, &key, cliOpKeyNoWithdrawnNote)
	return cmd
}

type sessionWakeDeps struct {
	// opKey is --key, the wake op's client key; empty mints one.
	opKey string
	// cfgErr is the city config's load error, when cfg did not load.
	cfgErr                   error
	store                    beads.Store
	cfg                      *config.City
	cityPath                 string
	cityResolved             bool
	now                      func() time.Time
	withdrawQueuedWaitNudges func(string, []string) error
	// cityUsesManagedReconciler reports whether a controller owns the woken
	// session's start; production wiring answers sessionStartOwnedByReconciler.
	cityUsesManagedReconciler func(string) bool
	pokeController            func(string, reconcilekey.Key) error
	// rigStores opens the rig stores the will-not-start predicate reads for
	// assigned work; nil reads the city store only.
	rigStores func() map[string]beads.Store
	// sp reads the sleep policy's capability for the predicate; nil never
	// refuses on an idle latch.
	sp runtime.Provider
}

// wakeVerdictProvider is the session provider the will-not-start predicate
// reads the sleep policy's capability from; nil (no idle-latch refusal) when
// it cannot be built.
func wakeVerdictProvider() runtime.Provider {
	sp, err := newSessionProvider()
	if err != nil {
		return nil
	}
	return sp
}

// cmdSessionWake is the CLI entry point for "gc session wake", recorded
// under the op key key (empty mints one).
func cmdSessionWake(args []string, key string, stdout, stderr io.Writer, asJSON bool) int {
	store, code := openCityStore(stderr, "gc session wake")
	if store == nil {
		return code
	}

	cityPath, cityErr := resolveCity()
	cfg, cfgErr := cliCityConfig(cityPath, cityErr, stderr)
	return doSessionWake(args[0], stdout, stderr, asJSON, sessionWakeDeps{
		opKey:                    key,
		cfgErr:                   cfgErr,
		sp:                       wakeVerdictProvider(),
		store:                    store,
		cfg:                      cfg,
		cityPath:                 cityPath,
		cityResolved:             cityErr == nil,
		now:                      time.Now,
		withdrawQueuedWaitNudges: withdrawQueuedWaitNudges,
		cityUsesManagedReconciler: func(cityPath string) bool {
			owned, err := sessionStartOwnedByReconciler(cityPath)
			if err != nil {
				fmt.Fprintf(stderr, "gc session wake: warning: %v\n", err) //nolint:errcheck
			}
			return owned
		},
		pokeController: enqueueController,
		rigStores: func() map[string]beads.Store {
			return buildStandaloneRigStoresWithConfig(cfg, cityPath, io.Discard)
		},
	})
}

func doSessionWake(target string, stdout, stderr io.Writer, asJSON bool, deps sessionWakeDeps) int {
	sessStore := cliSessionStore(deps.store, deps.cfg, deps.cityPath)
	// Resolving the target may materialize a named session's row, so under
	// require the op store and the key are checked before it.
	op := newCLIOp(deps.cfg, sessStore, cliOpRequest{
		verb: "gc session wake", kind: opjournal.KindWake, key: deps.opKey,
		cfgErr: deps.cfgErr, asJSON: asJSON, stdout: stdout, stderr: stderr,
	})
	if !op.preflight() {
		return 1
	}
	defer op.end()
	id, start, err := admitWake(op, deps, sessStore, target)
	if err != nil {
		op.finishErr(err)
		fmt.Fprintf(stderr, "gc session wake: %v\n", err) //nolint:errcheck
		return 1
	}
	switch start {
	case cliOpRefused:
		return 1
	case cliOpReplayed:
		return emitSessionWakeResult(stdout, stderr, asJSON, id, 0, op)
	}

	sessFront := sessionFrontDoor(sessStore)
	res, err := sessFront.WakeSession(id, deps.now().UTC(), session.WakeOpts{})
	if err != nil {
		if state, conflict := session.WakeConflictState(err); conflict {
			op.finishOutcome(cliOpRefusedOutcome(cliOpWakeConflictReason(state)))
			fmt.Fprintf(stderr, "gc session wake: session %s is %s\n", id, state) //nolint:errcheck
			return 1
		}
		op.finishErr(err)
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
	agent := sessionWakeResolveAgentInfo(res.Info, deps.cfg)
	hasRunnableTemplate := deps.cfg == nil || agent != nil
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
			op.finishErr(err)
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
	// Same "wake recorded but cannot complete" shape: the controller will not
	// act on the wake. The one will-not-start predicate the API wake also uses
	// says why, read on the row after the wake (and after the reset above).
	woken := res.Info
	if !rejectStuck {
		rigStores := deps.rigStores
		if rigStores == nil {
			rigStores = func() map[string]beads.Store { return nil }
		}
		if info, err := sessFront.Get(id); err == nil {
			woken = info
		}
		if why, _ := wakeWillNotStart(woken, wakeVerdictDeps{
			cfg: deps.cfg, cityPath: deps.cityPath, sessFront: sessFront, sp: deps.sp, work: cityWorkLegOf(beads.WorkStore{Store: deps.store}), rigStores: rigStores,
			runsV2: func() bool { return deps.cityResolved && cityRunsV2(deps.cityPath, deps.cfg) },
		}, deps.now()); why != "" {
			fmt.Fprintf(stderr, "gc session wake: wake recorded for session %s, but it will not start: %s\n", id, why) //nolint:errcheck
			rejectStuck = true
		}
	}
	if deps.cityResolved {
		if err := deps.withdrawQueuedWaitNudges(deps.cityPath, nudgeIDs); err != nil {
			fmt.Fprintf(stderr, "gc session wake: warning: withdrawing queued wait nudges: %v\n", err) //nolint:errcheck
		}
		if deps.cityUsesManagedReconciler(deps.cityPath) {
			noteSessionWakePoke(stderr, woken, rejectStuck, deps.pokeController(deps.cityPath, reconcilekey.Session(id)))
		}
	}
	if rejectStuck {
		op.finishOutcome(cliOpRefusedOutcome(cliOpRefusedWillNotStart))
		return 1
	}
	op.finishErr(nil)
	return emitSessionWakeResult(stdout, stderr, asJSON, id, len(nudgeIDs), op)
}

// noteSessionWakePoke reports on stderr the poke that follows a recorded wake
// of row, the session as the wake left it. For a wake the controller acts on,
// that is how its start proceeds (noteDeferredSessionStart). A wake refused
// as one that cannot complete starts nothing, so only a failed poke is
// reported, with no promise of a start.
func noteSessionWakePoke(stderr io.Writer, row session.Info, refused bool, pokeErr error) {
	if !refused {
		noteDeferredSessionStart(stderr, "gc session wake", row, pokeErr)
		return
	}
	if pokeErr != nil && !noteWorkerSitePoke(stderr, "gc session wake", pokeErr) {
		fmt.Fprintf(stderr, "gc session wake: warning: poke failed: %v\n", pokeErr) //nolint:errcheck
	}
}

// errWakeNotAdmitted stops a wake's resolution from reopening a closed named
// row its op did not admit.
var errWakeNotAdmitted = errors.New("wake not admitted")

// admitWake resolves the wake's target and starts its op (DES-O0API §8.4)
// before the wake writes the row. A closed named row's op starts before
// resolution reopens the row, so a refusal or a replay leaves it closed. A
// named session with no row is created first (§6.4), under a new id no
// earlier op can name, so admission neither replays it nor refuses it for
// an earlier op; but under require a journal that then cannot record the op
// refuses the wake after that create, and the created session stays,
// started or left for the controller to start. An op admitted for a row
// resolution did not end up returning is closed as target_moved, and the op
// started over for the row it returned. A closed row named by its ID is
// refused before any op, unless the wake's key already names one on that row
// (refuseClosedWakeTarget).
func admitWake(op *cliOp, deps sessionWakeDeps, sessStore beads.Store, target string) (string, cliOpStart, error) {
	admitted, start := "", cliOpStart(0)
	id, err := resolveSessionIDMaterializingNamedBeforeReopen(deps.cityPath, deps.cfg, sessStore, target, func(closedID string) error {
		admitted, start = closedID, op.retarget(closedID)
		if start != cliOpExecute {
			return errWakeNotAdmitted
		}
		return nil
	})
	switch {
	case errors.Is(err, errWakeNotAdmitted):
		return admitted, start, nil
	case err != nil:
		return "", 0, err
	case admitted != "" && admitted == id:
		return id, start, nil
	}
	if err := refuseClosedWakeTarget(op, sessStore, id); err != nil {
		return "", 0, err
	}
	return id, op.retarget(id), nil
}

// refuseClosedWakeTarget refuses, before its op is proposed, a recorded wake
// whose target resolved to a closed row its key names no op on: a row named
// by its ID resolves closed, and the wake can only refuse it, as it does
// unrecorded ("session X is closed"). Recorded, that refusal would hold the
// key on the row's conversation, and a wake of a configured named session by
// alias under the same key, which reopens that row, would replay it instead
// of reopening. So it is a front-door refusal and writes no op. A key that
// already names an op on the row (a wake the row has closed since) passes, so
// admission answers from that op as it does by alias: a same-key retry
// replays the wake. A key whose op does not read is refused as one that names
// none, with a warning that it was not checked. A row that does not read is
// left to the wake, which reports it.
func refuseClosedWakeTarget(op *cliOp, sessStore beads.Store, id string) error {
	if !op.recording() {
		return nil
	}
	if b, err := sessStore.Get(id); err != nil || b.Status != "closed" {
		return nil
	}
	principal, _ := cliPrincipal(cliOpLookupEnv)
	_, found, err := op.keyedOp(id, principal)
	if found {
		return nil
	}
	if err != nil {
		fmt.Fprintf(op.req.stderr, "%s: warning: op journal: %v; --key %s was not checked for an op on session %s to answer from\n", op.req.verb, err, op.req.key, id) //nolint:errcheck // best-effort stderr
	}
	return fmt.Errorf("session %s is closed", id)
}

// emitSessionWakeResult prints a wake's success output, a replay's included.
func emitSessionWakeResult(stdout, stderr io.Writer, asJSON bool, id string, withdrawn int, op *cliOp) int {
	if asJSON {
		result := sessionActionResult{
			Action:              "wake",
			SessionID:           id,
			State:               "wake_requested",
			WaitNudgesWithdrawn: withdrawn,
		}
		op.annotate(&result)
		if err := writeSessionActionJSON(stdout, result); err != nil {
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
	return sessionWakeResolveAgentInfo(info, cfg) != nil
}

// sessionWakeResolveAgentInfo resolves the config.Agent that owns this
// session's template, the same lookup sessionWakeHasRunnableTemplateInfo
// uses to decide runnability. Returns nil when cfg is nil or no agent
// matches -- callers that also need "is there a runnable template"
// should treat a nil cfg as runnable themselves, since this helper
// can't distinguish "no cfg" from "no match" on its own.
func sessionWakeResolveAgentInfo(info session.Info, cfg *config.City) *config.Agent {
	if cfg == nil {
		return nil
	}
	template := normalizedSessionTemplateInfo(info, cfg)
	if template == "" {
		template = info.Template
	}
	return findAgentByTemplate(cfg, template)
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
