package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/executionevent"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/graphroute"
	"github.com/gastownhall/gascity/internal/graphv2"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
	"github.com/gastownhall/gascity/internal/storeref"
	"github.com/spf13/cobra"
)

var dispatchControlSessionProvider = newSessionProvider

const maxControlQuarantineReasonMetadata = 512

func sourceWorkflowCommandContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// convoyDispatchSubcommands returns the dispatch-related subcommands to add to gc convoy.
func convoyDispatchSubcommands(stdout, stderr io.Writer) []*cobra.Command {
	return []*cobra.Command{
		newConvoyControlCmd(stdout, stderr),
		newConvoyPokeCmd(stdout, stderr),
		newConvoyDeleteCmd(stdout, stderr),
		newConvoyDeleteSourceCmd(stdout, stderr),
		newConvoyReopenSourceCmd(stdout, stderr),
	}
}

// newWorkflowCmd returns a hidden alias for backwards compatibility.
func newWorkflowCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "workflow",
		Short:  "Alias for gc convoy (deprecated)",
		Hidden: true,
	}
	cmd.AddCommand(convoyDispatchSubcommands(stdout, stderr)...)
	return cmd
}

func newConvoyControlCmd(stdout, stderr io.Writer) *cobra.Command {
	var serve bool
	var follow string
	cmd := &cobra.Command{
		Use:   "control [bead-id]",
		Short: "Execute control beads or run the control-dispatcher loop",
		Long: `Process a single control bead, or run the control-dispatcher loop
with --serve to continuously process ready control beads.
Use --follow <agent> to filter the serve loop to a specific agent template.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if serve || follow != "" {
				if follow != "" {
					args = append(args, follow)
				}
				return runConvoyControlServe(args, stdout, stderr)
			}
			if len(args) == 0 {
				return fmt.Errorf("bead-id is required (or use --serve)")
			}
			if err := runControlDispatcher(args[0], stdout, stderr); err != nil {
				if errors.Is(err, dispatch.ErrControlPending) {
					return nil
				}
				_, _ = fmt.Fprintf(stderr, "gc convoy control: %v\n", err)
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&serve, "serve", false, "Run the control-dispatcher loop (continuous)")
	cmd.Flags().StringVar(&follow, "follow", "", "Run serve loop filtered to a specific agent template")
	return cmd
}

func newConvoyPokeCmd(_ io.Writer, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "poke",
		Short:  "Trigger immediate control dispatch reconciliation",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cityPath, err := resolveCity()
			if err != nil {
				_, _ = fmt.Fprintf(stderr, "gc convoy poke: %v\n", err)
				return errExit
			}
			if err := pokeControlDispatch(cityPath); err != nil {
				_, _ = fmt.Fprintf(stderr, "gc convoy poke: %v\n", err)
				return errExit
			}
			return nil
		},
	}
	return cmd
}

func pokeControlDispatch(cityPath string) error {
	if _, err := sendControllerCommand(cityPath, "control-dispatcher"); err == nil {
		return nil
	}
	return pokeController(cityPath)
}

func runControlDispatcher(beadID string, stdout, stderr io.Writer) error {
	cityPath, err := resolveCity()
	if err != nil {
		return err
	}

	// Manual control dispatch keeps the operator convenience of resolving a
	// bead ID across city and rig stores. That resolution answers WHICH SCOPE
	// owns the id; the bead the dispatch gates on is read below from the store
	// it is about to mutate, not from the unrouted scope store searched here.
	store, storePath, err := findBeadScopeAcrossStores(cityPath, beadID, stderr)
	if err != nil {
		return fmt.Errorf("loading bead %s: %w", beadID, err)
	}

	return runControlDispatcherWithStore(cityPath, storePath, store, beadID, stdout, stderr)
}

func runControlDispatcherInStore(cityPath, storePath, beadID string, stdout, stderr io.Writer) error {
	if cityPath == "" {
		var err error
		cityPath, err = resolveCity()
		if err != nil {
			return err
		}
	}
	if storePath == "" {
		storePath = cityPath
	}

	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		return err
	}
	resolveRigPaths(cityPath, cfg.Rigs)
	store, err := openControlStoreAtForCity(storePath, cityPath, cfg)
	if err != nil {
		return fmt.Errorf("opening scoped control store %q: %w", storePath, err)
	}

	return runControlDispatcherWithStoreAndConfig(cityPath, storePath, store, beadID, cfg, stdout, stderr)
}

func runControlDispatcherWithStore(cityPath, storePath string, store beads.Store, beadID string, stdout, stderr io.Writer) error {
	return runControlDispatcherWithStoreAndConfig(cityPath, storePath, store, beadID, nil, stdout, stderr)
}

// runControlDispatcherWithStoreAndConfig reads the control bead itself rather
// than accepting a value, so the copy ProcessControl's idempotence gate consults
// is by construction the copy the dispatch is about to mutate. Both entry points
// above resolve a SCOPE and hand it over; a bead value resolved alongside that
// scope comes from an unrouted store, and gating on it while writing elsewhere
// re-runs a control kind the graph store had already finished.
func runControlDispatcherWithStoreAndConfig(cityPath, storePath string, store beads.Store, beadID string, cfg *config.City, stdout, stderr io.Writer) error {
	restoreTraceWarnings := useWorkflowTraceWarnings(stderr)
	defer restoreTraceWarnings()
	var cfgLoadErr error
	if cfg == nil {
		cfg, cfgLoadErr = loadCityConfig(cityPath, stderr)
		if cfg != nil {
			resolveRigPaths(cityPath, cfg.Rigs)
		}
	}
	if cfg != nil {
		warnLegacyWorkflowTracePath(cityPath, cfg.Rigs, stderr)
	} else {
		warnLegacyWorkflowTracePath(cityPath, nil, stderr)
	}

	// store is the SCOPE store. Control beads, the workflow topology they
	// mutate, and the graph beads the control kinds create (retry attempts,
	// fanout fragments, drain item roots) are graph class, so all of that runs
	// against the graph store. store itself stays the work leg: EVERY convoy is
	// a work bead, the synthetic drain-unit ones included, so it owns both the
	// input convoy whose tracks edges the execution snapshot below reads and the
	// unit convoys a drain mints alongside its members.
	graphStore, bead, err := controlBeadLedger(cityPath, storePath, cfg, store, beadID)
	if err != nil {
		return err
	}

	opts := dispatch.ProcessOptions{CityPath: cityPath, StorePath: storePath}
	opts.Tracef = workflowTracef
	loadCfg := false
	// This is a per-kind capability switch (does this control kind need city
	// config loaded to resolve store-refs/formulas/sessions?), not a
	// control-kind membership predicate, so it intentionally lists literals
	// rather than deriving from the beadmeta taxonomy. "scope-check" is
	// deliberately absent because it needs no config resolution; a future
	// control kind that needs cfg must be added here explicitly.
	switch bead.Metadata[beadmeta.KindMetadataKey] {
	case "check", "drain", "fanout", "retry-eval", "retry", "ralph":
		loadCfg = true
	case "workflow-finalize":
		// Need cfg to resolve "city:<name>" / "rig:<name>" store refs when
		// closing parent source beads in their native stores.
		loadCfg = true
	}
	if loadCfg {
		if cfg == nil {
			if cfgLoadErr != nil {
				return cfgLoadErr
			}
			return fmt.Errorf("loading city config for %s: unavailable after warning-only load", cityPath)
		}
		opts.ResolveStoreRef = makeStoreRefResolver(cityPath, cfg)
		if bead.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindWorkflowFinalize {
			sourceWorkflowCtx, cancelSourceWorkflowCtx := sourceWorkflowCommandContext()
			defer cancelSourceWorkflowCtx()
			opts.SourceWorkflowLock = makeSourceWorkflowLocker(sourceWorkflowCtx, cityPath, cfg, storePath)
			opts.SourceWorkflowStores = makeSourceWorkflowStoresLister(cityPath, cfg)
		}
		switch bead.Metadata[beadmeta.KindMetadataKey] {
		case "check", "fanout":
			opts.FormulaSearchPaths = workflowFormulaSearchPaths(cfg, bead)
			opts.PrepareFragment = func(fragment *formula.FragmentRecipe, source beads.Bead) error {
				return decorateDynamicFragmentRecipe(fragment, source, graphStore, loadedCityName(cfg, cityPath), cityPath, cfg)
			}
		case "drain":
			opts.FormulaSearchPaths = workflowFormulaSearchPaths(cfg, bead)
			opts.PrepareRecipe = func(recipe *formula.Recipe, source beads.Bead) error {
				return decorateDrainItemRecipe(recipe, source, graphStore, workflowStoreRefForDir(storePath, cityPath, loadedCityName(cfg, cityPath), cfg), loadedCityName(cfg, cityPath), cityPath, cfg)
			}
			// A drain is the one control kind that reads beads it did not
			// create. Its control and item roots are graph class and run
			// against graphStore above, but the convoy it expands over is
			// minted alongside its work members and stays in the scope store —
			// the same store handed to EmitCurrent below as the work leg that
			// owns that convoy's tracks edges. Naming it here is what lets the
			// membership read, the member reservations and the member
			// dependency projection cross the class boundary. Only when the
			// class actually relocated: on every other city graphStore IS
			// store, and an empty tail keeps each of those reads on the single
			// direct call it makes today.
			// Both readings above assume the drain's convoy lives in the SCOPE
			// store it is dispatched from. Enforce that (ga-w2mf3).
			if err := assertDrainRootScopeMatchesDispatch(bead, workflowStoreRefForDir(storePath, cityPath, loadedCityName(cfg, cityPath), cfg)); err != nil {
				return err
			}
			if graphStore != store {
				opts.MemberStores = []beads.Store{store}
			}
		case "retry-eval":
			sp, err := dispatchControlSessionProvider()
			if err != nil {
				return err
			}
			opts.RecycleSession = func(subject beads.Bead) error {
				if strings.TrimSpace(subject.Assignee) == "" {
					return fmt.Errorf("subject %s missing assignee for pooled retry recycle", subject.ID)
				}
				return workerKillSessionTargetWithConfig("", store, sp, cfg, subject.Assignee)
			}
		case "retry", "ralph":
			opts.FormulaSearchPaths = workflowFormulaSearchPaths(cfg, bead)
			sp, err := dispatchControlSessionProvider()
			if err != nil {
				return err
			}
			opts.RecycleSession = func(subject beads.Bead) error {
				if strings.TrimSpace(subject.Assignee) == "" {
					return fmt.Errorf("subject %s missing assignee for pooled retry recycle", subject.ID)
				}
				return workerKillSessionTargetWithConfig("", store, sp, cfg, subject.Assignee)
			}
		}
	}

	result, err := dispatch.ProcessControl(graphStore, bead, opts)
	if err != nil {
		return handleControlDispatchError(cityPath, storePath, graphStore, bead, beadID, err, stderr)
	}
	if result.Processed {
		rootID := strings.TrimSpace(bead.Metadata[beadmeta.RootBeadIDMetadataKey])
		if rootID != "" {
			recorder := openCityRecorderAt(cityPath, stderr)
			emitErr := executionevent.EmitCurrent(recorder, beads.GraphStore{Store: graphStore}, beads.WorkStore{Store: executionEmitStore(store, cityPath)}, rootID, "control-dispatch")
			var closeErr error
			if closer, ok := recorder.(io.Closer); ok {
				closeErr = closer.Close()
			}
			if err := errors.Join(emitErr, closeErr); err != nil {
				fmt.Fprintf(stderr, "warning: control dispatch: projecting execution facts for %s: %v\n", rootID, err) //nolint:errcheck // successful control processing is preserved
			}
		}
		_, _ = fmt.Fprintf(stdout, "control dispatch: bead=%s action=%s", beadID, result.Action)
		if result.Created > 0 {
			_, _ = fmt.Fprintf(stdout, " created=%d", result.Created)
		}
		if result.Skipped > 0 {
			_, _ = fmt.Fprintf(stdout, " skipped=%d", result.Skipped)
		}
		fmt.Fprintln(stdout) //nolint:errcheck
	}
	return nil
}

// handleControlDispatchError resolves a failed ProcessControl call into the
// error the dispatcher should return. It is the Tier-B semantic-refusal budget
// and quarantine path, split out of runControlDispatcherWithStoreAndConfig so
// the dispatcher entry point stays legible. An availability-tier refusal retries
// unbounded (the store never answered, and recording a budget needs that same
// store); a semantic-tier refusal is recorded against the bead-persisted budget
// and either retried — quietly once it repeats — or, once the budget expires,
// quarantined; any other classification quarantines on the first refusal. It
// returns nil once the bead is quarantined and the (possibly quiet-wrapped)
// cause when the bead should be retried.
func handleControlDispatchError(cityPath, storePath string, graphStore beads.Store, bead beads.Bead, beadID string, cause error, stderr io.Writer) error {
	if errors.Is(cause, dispatch.ErrControlPending) {
		return cause
	}
	var stalled *dispatch.SemanticRetryState
	switch dispatch.ClassifyControllerError(cause) {
	case dispatch.TierAvailability:
		// The store never answered. Retry unbounded and write nothing:
		// recording a budget needs the very store that is unavailable.
		return cause
	case dispatch.TierSemantic:
		retry, recordErr := dispatch.RecordSemanticControlRetry(
			graphStore, beadID, cause, workflowTraceNow().UTC(), semanticControlRetryBudget())
		if recordErr != nil {
			// Losing the budget write is itself a store problem: keep the
			// pre-tier behavior (retry) rather than escalating on it.
			workflowTracef("control-retry-budget bead=%s recording semantic refusal failed err=%v", beadID, recordErr)
			return cause
		}
		workflowTracef("control-retry-budget bead=%s attempts=%d first_seen=%s expired=%t repeat=%t err=%v",
			beadID, retry.Attempts, retry.FirstSeen.UTC().Format(time.RFC3339), retry.Expired, retry.Repeat, cause)
		if !retry.Expired {
			if retry.Repeat {
				return dispatch.MarkQuietControllerRetry(cause)
			}
			return cause
		}
		stalled = &retry
	}
	// Quarantine closes the control bead BEFORE emitControlStalled runs. The
	// ordering is deliberate — emitting first would risk a duplicate event if the
	// close then failed — but it leaves a crash window: a process death between
	// the close and the emit drops the control.stalled event. That is tolerable
	// because RecordSemanticControlRetry already persisted first_seen/count/error
	// on the bead, so `bd show` still explains the stall even when the event is
	// lost.
	settleFailure, quarantineErr := quarantineControlFailureBead(graphStore, bead, cause)
	if quarantineErr != nil {
		return errors.Join(cause, quarantineErr)
	}
	if settleFailure != nil {
		emitControlRootSettleFailed(cityPath, storePath, *settleFailure, stderr)
		_, _ = fmt.Fprintf(stderr,
			"control dispatch: root=%s failed to settle after finalizer=%s quarantined reason=%v\n",
			settleFailure.RootBeadID, settleFailure.FinalizerBeadID, cause)
	}
	if stalled != nil {
		emitControlStalled(cityPath, storePath, graphStore, bead, cause, *stalled, stderr)
		_, _ = fmt.Fprintf(stderr,
			"control dispatch: stalled bead=%s attempts=%d first_seen=%s budget=%s reason=%v\n",
			beadID, stalled.Attempts, stalled.FirstSeen.UTC().Format(time.RFC3339),
			semanticControlRetryBudget(), cause)
		return nil
	}
	_, _ = fmt.Fprintf(stderr, "control dispatch: quarantined bead=%s reason=%v\n", beadID, cause)
	return nil
}

// semanticControlRetryBudget returns how long the control dispatcher keeps
// retrying a store refusal before quarantining the control bead.
//
// GC_CONTROL_SEMANTIC_RETRY_BUDGET overrides the default as a Go duration. It
// is an incident knob, not a tuning parameter: "0s" restores quarantine-on-
// first-refusal (the pre-#5020 behavior) to clear a wedged fleet immediately,
// and a negative value restores unbounded retry if a bad classification ever
// starts quarantining healthy work. An unparseable value falls back to the
// default rather than failing the dispatcher.
func semanticControlRetryBudget() time.Duration {
	raw := strings.TrimSpace(os.Getenv("GC_CONTROL_SEMANTIC_RETRY_BUDGET"))
	if raw == "" {
		return dispatch.DefaultSemanticRetryBudget
	}
	budget, err := time.ParseDuration(raw)
	if err != nil {
		return dispatch.DefaultSemanticRetryBudget
	}
	return budget
}

// emitControlStalled publishes the control.stalled record for a control bead
// whose semantic-refusal budget expired, plus order.failed when the workflow
// root belongs to a scheduled order — so the existing order-health surfaces
// light up instead of needing a new dashboard to notice a dead control plane.
func emitControlStalled(cityPath, storePath string, store beads.Store, bead beads.Bead, cause error, state dispatch.SemanticRetryState, stderr io.Writer) {
	rootID := strings.TrimSpace(bead.Metadata[beadmeta.RootBeadIDMetadataKey])
	orderName := ""
	if rootID != "" {
		if root, err := store.Get(rootID); err == nil {
			orderName, _ = orders.NameFromOrderRunLabel(root)
		}
	}

	payload := events.ControlStalledPayload{
		BeadID:     bead.ID,
		Kind:       strings.TrimSpace(bead.Metadata[beadmeta.KindMetadataKey]),
		RootBeadID: rootID,
		StorePath:  storePath,
		ErrorClass: dispatch.TierSemantic.String(),
		FirstSeen:  state.FirstSeen.UTC().Format(time.RFC3339),
		Attempts:   state.Attempts,
		Error:      controlQuarantineReason(cause, "control_dispatch_error"),
		OrderName:  orderName,
	}

	rec := openCityRecorderAt(cityPath, stderr)
	rec.Record(events.Event{
		Type:    events.ControlStalled,
		Actor:   "controller",
		Subject: bead.ID,
		Message: fmt.Sprintf("control bead %s stalled after %d semantic retries since %s: %s",
			bead.ID, state.Attempts, payload.FirstSeen, payload.Error),
		Payload: events.ControlStalledPayloadJSON(payload),
		RunID:   rootID,
	})
	if orderName != "" {
		rec.Record(events.Event{
			Type:    events.OrderFailed,
			Actor:   "controller",
			Subject: orderName,
			Message: fmt.Sprintf("control bead %s stalled: %s", bead.ID, payload.Error),
			RunID:   rootID,
		})
	}
	if closer, ok := rec.(io.Closer); ok {
		if err := closer.Close(); err != nil {
			fmt.Fprintf(stderr, "warning: control dispatch: closing event recorder for %s: %v\n", bead.ID, err) //nolint:errcheck // the quarantine already succeeded
		}
	}
}

// emitControlRootSettleFailed publishes the control.root_settle_failed record
// for a workflow root that a quarantined finalizer could not close, mirroring
// emitControlStalled's pattern above. The marker write onto the root
// (recordRootSettleFailure) already made the failure durable in the store;
// this makes it observable from the event bus too, so a stranded root shows
// up next to control.stalled instead of only in a trace line. See ga-li4qa4.
func emitControlRootSettleFailed(cityPath, storePath string, failure rootSettleFailure, stderr io.Writer) {
	payload := events.ControlRootSettleFailedPayload{
		RootBeadID:      failure.RootBeadID,
		FinalizerBeadID: failure.FinalizerBeadID,
		StorePath:       storePath,
		ErrorClass:      failure.ErrorClass,
		Error:           failure.Error,
		FollowUpBeadID:  failure.FollowUpBeadID,
	}

	rec := openCityRecorderAt(cityPath, stderr)
	rec.Record(events.Event{
		Type:    events.ControlRootSettleFailed,
		Actor:   "controller",
		Subject: failure.RootBeadID,
		Message: fmt.Sprintf("workflow root %s failed to settle after finalizer %s was quarantined: %s",
			failure.RootBeadID, failure.FinalizerBeadID, failure.Error),
		Payload: events.ControlRootSettleFailedPayloadJSON(payload),
		RunID:   failure.RootBeadID,
	})
	if closer, ok := rec.(io.Closer); ok {
		if err := closer.Close(); err != nil {
			fmt.Fprintf(stderr, "warning: control dispatch: closing event recorder for %s: %v\n", failure.RootBeadID, err) //nolint:errcheck // the settle-failure marker already succeeded
		}
	}
}

// rootSettleFailure records a workflow root that failed to settle after its
// workflow-finalize control bead was quarantined. quarantineControlFailureBead
// hands one back (non-nil) whenever settleRootForQuarantinedFinalizer's close
// attempt fails, so the caller can make the failure durably visible instead of
// letting it vanish into a trace line. See ga-li4qa4.
type rootSettleFailure struct {
	RootBeadID      string
	FinalizerBeadID string
	// ErrorClass names the failure tier, mirroring beadmeta.ControllerErrorClassMetadataKey.
	ErrorClass string
	// Error is the store's refusal, truncated the same way controlQuarantineReason
	// truncates the control bead's own reason.
	Error string
	// FollowUpBeadID is the reconciliation bead filed for this settle failure,
	// when bead creation itself succeeded.
	FollowUpBeadID string
}

// quarantineControlFailureBead closes and labels a control bead that cannot
// make progress. When the bead is a workflow-finalize control whose workflow
// root fails to settle behind it, the returned *rootSettleFailure is non-nil
// so the caller can surface the stranded root instead of it going silent.
func quarantineControlFailureBead(store beads.Store, bead beads.Bead, cause error) (*rootSettleFailure, error) {
	beadID := bead.ID
	failureReason := "control_dispatch_error"
	if errors.Is(cause, dispatch.ErrControlGraphMalformed) {
		failureReason = "malformed_control_graph"
	}
	reason := controlQuarantineReason(cause, failureReason)
	status := "closed"
	if err := store.Update(beadID, beads.UpdateOpts{
		Status: &status,
		Labels: []string{"gc:control-quarantined"},
		Metadata: map[string]string{
			beadmeta.OutcomeMetadataKey:                 beadmeta.OutcomeFail,
			beadmeta.FailureClassMetadataKey:            beadmeta.FailureClassHard,
			beadmeta.FailureReasonMetadataKey:           failureReason,
			beadmeta.ControllerErrorMetadataKey:         reason,
			beadmeta.ControllerErrorClassMetadataKey:    beadmeta.FailureClassHard,
			beadmeta.ControllerRetryableMetadataKey:     "",
			beadmeta.FinalDispositionMetadataKey:        beadmeta.DispositionControlQuarantine,
			beadmeta.ControlQuarantinedMetadataKey:      "true",
			beadmeta.ControlQuarantineReasonMetadataKey: reason,
			beadmeta.ControlQuarantinedAtMetadataKey:    workflowTraceNow().UTC().Format(time.RFC3339),
		},
	}); err != nil {
		return nil, err
	}
	_, _ = dispatch.ReconcileClosedScopeMember(store, beadID)

	if strings.TrimSpace(bead.Metadata[beadmeta.KindMetadataKey]) == beadmeta.KindWorkflowFinalize {
		// Best-effort, like the ReconcileClosedScopeMember call above: the
		// finalizer's own quarantine (closed + labeled, just above) is the
		// load-bearing action and must stand regardless of what happens next.
		// A close failure here is expected transiently -- the store may not
		// yet reflect the finalizer close this root's "blocked by" edge is
		// waiting on. There is no later reconciliation pass that retries this,
		// so instead of letting the stranded root go silent, record the
		// failure durably on the root and hand it back to the caller. See
		// ga-li4qa4.
		if settleErr := settleRootForQuarantinedFinalizer(store, bead); settleErr != nil {
			workflowTracef("control-quarantine bead=%s: closing root for quarantined finalizer failed err=%v", beadID, settleErr)
			rootID := strings.TrimSpace(bead.Metadata[beadmeta.RootBeadIDMetadataKey])
			return recordRootSettleFailure(store, rootID, beadID, settleErr), nil
		}
	}
	return nil, nil
}

// recordRootSettleFailure makes a stranded workflow root durably visible when
// settleRootForQuarantinedFinalizer could not close it: it marks the root
// with the same controller-error vocabulary a quarantined control bead
// carries, and files a follow-up reconciliation bead linking the root and the
// quarantined finalizer. Both the marker write and the bead creation are
// best-effort -- the finalizer's own quarantine already succeeded and must
// stand regardless of whether either of these persists. See ga-li4qa4.
func recordRootSettleFailure(store beads.Store, rootID, finalizerBeadID string, settleErr error) *rootSettleFailure {
	reason := controlQuarantineReason(settleErr, "root_settle_failed")
	failure := &rootSettleFailure{
		RootBeadID:      rootID,
		FinalizerBeadID: finalizerBeadID,
		ErrorClass:      beadmeta.FailureClassHard,
		Error:           reason,
	}
	if rootID != "" {
		if err := store.Update(rootID, beads.UpdateOpts{
			Metadata: map[string]string{
				beadmeta.ControllerErrorMetadataKey:      reason,
				beadmeta.ControllerErrorClassMetadataKey: beadmeta.FailureClassHard,
				beadmeta.RootSettleFailedMetadataKey:     "true",
				beadmeta.RootSettleFailedAtMetadataKey:   workflowTraceNow().UTC().Format(time.RFC3339),
			},
		}); err != nil {
			workflowTracef("control-quarantine root=%s: recording settle-failure marker failed err=%v", rootID, err)
		}
	}
	followUp, err := store.Create(beads.Bead{
		Title: fmt.Sprintf("Reconcile stranded workflow root %s (finalizer %s quarantined)", rootID, finalizerBeadID),
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey:      rootID,
			beadmeta.FinalizerBeadIDMetadataKey: finalizerBeadID,
		},
	})
	if err != nil {
		workflowTracef("control-quarantine root=%s: filing reconciliation follow-up bead failed err=%v", rootID, err)
		return failure
	}
	failure.FollowUpBeadID = followUp.ID
	return failure
}

// settleRootForQuarantinedFinalizer closes a workflow root whose
// workflow-finalize control bead was just quarantined. Quarantine short-
// circuits the normal finalize path (processWorkflowFinalize closes the root
// via setOutcomeAndClose before closing the finalizer itself), so without
// this the root never reaches Status=="closed" and survives every
// rootSettled-gated check forever even though its finalizer is dead. See
// ga-japz50 / ga-xn8ml8.
func settleRootForQuarantinedFinalizer(store beads.Store, finalizer beads.Bead) error {
	rootID := strings.TrimSpace(finalizer.Metadata[beadmeta.RootBeadIDMetadataKey])
	if rootID == "" || rootID == finalizer.ID {
		return nil
	}
	root, err := store.Get(rootID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return nil
		}
		return err
	}
	if root.Status == "closed" {
		// Already settled elsewhere -- never downgrade a real outcome.
		return nil
	}
	status := "closed"
	if err := store.Update(rootID, beads.UpdateOpts{
		Status: &status,
		Metadata: map[string]string{
			beadmeta.OutcomeMetadataKey:          beadmeta.OutcomeFail,
			beadmeta.FailureClassMetadataKey:     beadmeta.FailureClassHard,
			beadmeta.FailureReasonMetadataKey:    "finalizer_control_quarantined",
			beadmeta.FinalDispositionMetadataKey: beadmeta.DispositionControlQuarantine,
		},
	}); err != nil {
		return err
	}
	_, _ = dispatch.ReconcileClosedScopeMember(store, rootID)
	return nil
}

func controlQuarantineReason(cause error, fallback string) string {
	reason := ""
	if cause != nil {
		reason = strings.TrimSpace(cause.Error())
	}
	if reason == "" {
		reason = fallback
	}
	if len(reason) <= maxControlQuarantineReasonMetadata {
		return reason
	}
	limit := maxControlQuarantineReasonMetadata
	for limit > 0 && !utf8.ValidString(reason[:limit]) {
		limit--
	}
	return reason[:limit]
}

// makeStoreRefResolver returns a dispatch.ProcessOptions.ResolveStoreRef
// closure for the given city. The resolver maps "city:<name>" and
// "rig:<name>" gc.source_store_ref values to a beads.Store rooted at the
// matching scope. processWorkflowFinalize uses it to walk the source bead
// chain across store boundaries so a successful rig-scope workflow closes
// the city-scope source bead that spawned it (e.g. PR-review "Adopt PR"
// requests).
func makeStoreRefResolver(cityPath string, cfg *config.City) func(string) (beads.Store, error) {
	cityName := loadedCityName(cfg, cityPath)
	return func(ref string) (beads.Store, error) {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			return nil, fmt.Errorf("empty store ref")
		}
		switch {
		case strings.HasPrefix(ref, "city:"):
			name := strings.TrimSpace(strings.TrimPrefix(ref, "city:"))
			// "city:" without a name still resolves to this city's store -
			// older callers stamp ambiguous refs and the only reachable city
			// from a control-dispatcher is the one it was launched in.
			if name != "" && cityName != "" && name != cityName {
				return nil, fmt.Errorf("city ref %q does not match this city %q", ref, cityName)
			}
			return openStoreAtForCity(cityPath, cityPath)
		case strings.HasPrefix(ref, "rig:"):
			name := strings.TrimSpace(strings.TrimPrefix(ref, "rig:"))
			if name == "" {
				return nil, fmt.Errorf("rig ref %q missing rig name", ref)
			}
			if cfg == nil {
				return nil, fmt.Errorf("no city config available to resolve %q", ref)
			}
			for _, rig := range cfg.Rigs {
				if rig.Name != name {
					continue
				}
				return openControlStoreAtForCity(rig.Path, cityPath, cfg)
			}
			return nil, fmt.Errorf("rig %q not found in city config", name)
		default:
			return nil, fmt.Errorf("unsupported store ref scheme: %q", ref)
		}
	}
}

func makeSourceWorkflowLocker(ctx context.Context, cityPath string, cfg *config.City, defaultStorePath string) func(storeRef, sourceBeadID string, fn func() error) error {
	return func(storeRef, sourceBeadID string, fn func() error) error {
		return sourceworkflow.WithLock(ctx, cityPath, sourceWorkflowLockScopeForStoreRef(cityPath, cfg, defaultStorePath, storeRef), sourceBeadID, fn)
	}
}

// makeSourceWorkflowStoresLister lists every store that can hold a LIVE workflow
// root, which is the precondition workflow-finalize checks before closing a
// source bead: a source bead with another workflow still running against it must
// stay open.
//
// Each scope is opened through the same class hop the dispatch itself takes.
// Workflow roots are graph class (coordclass classifies gc.kind=workflow that
// way), so on a converged split city the city scope's roots are in the binding,
// and a scan of the city WORK store finds none of them. That is a guard that
// silently answers "no live roots" for the one arrangement it exists to catch —
// and unlike a missed read it is destructive, because the answer closes and
// terminally stamps a human-visible source bead while its other workflow is
// still executing. Routing the scan and the mutation to the same ledger is the
// whole point.
//
// The hop is scope-guarded by controlGraphBinding, so rig scopes keep their own
// stores; a relocated scope does not open the scope store at all, because that
// would be a bd process this scan never reads.
func makeSourceWorkflowStoresLister(cityPath string, cfg *config.City) func() ([]dispatch.SourceWorkflowStore, error) {
	return makeSourceWorkflowStoresListerWithOpenStore(cityPath, cfg, func(dir string) (beads.Store, error) {
		if binding, relocated := controlGraphBinding(cityPath, dir); relocated {
			return binding, nil
		}
		return openStoreAtForCity(dir, cityPath)
	})
}

func makeSourceWorkflowStoresListerWithOpenStore(cityPath string, cfg *config.City, openStore func(string) (beads.Store, error)) func() ([]dispatch.SourceWorkflowStore, error) {
	var (
		loaded  bool
		stores  []dispatch.SourceWorkflowStore
		loadErr error
	)
	return func() ([]dispatch.SourceWorkflowStore, error) {
		if loaded {
			return stores, loadErr
		}
		loaded = true
		views, skips, err := openSourceWorkflowStoresWith(cfg, cityPath, "", openStore)
		if err != nil {
			loadErr = err
			return nil, err
		}
		if len(skips) > 0 {
			msg := formatSourceWorkflowStoreSkips(skips)
			workflowTracef("source-workflow stores warning=%q", msg)
			loadErr = errors.New(msg)
			return nil, loadErr
		}
		cityName := loadedCityName(cfg, cityPath)
		stores = make([]dispatch.SourceWorkflowStore, 0, len(views))
		for _, view := range views {
			stores = append(stores, dispatch.SourceWorkflowStore{
				Store:    view.store,
				StoreRef: workflowStoreRefForDir(view.path, cityPath, cityName, cfg),
			})
		}
		return stores, nil
	}
}

func sourceWorkflowLockScopeForStoreRef(cityPath string, cfg *config.City, defaultStorePath string, storeRef string) string {
	return sourceworkflow.LockScopeForStoreRef(cityPath, defaultStorePath, storeRef, func(rigName string) (string, bool) {
		if cfg != nil {
			for _, rig := range cfg.Rigs {
				if rig.Name != rigName {
					continue
				}
				return rig.Path, true
			}
		}
		return "", false
	})
}

// controlScopeTakesGraphClass reports whether control dispatch for a scope
// resolves its control beads through the graph class instead of staying on the
// store the scope opened.
//
// Only the CITY scope does, and "instead of" is the whole content of the word
// only. resolveClassStore holds a single city-level store per class, so there is
// no per-rig graph binding, and `gc storage migrate` copies only the city work
// store (openInfraMigrationSource) — a rig scope has no retained copy to be
// misled by and no binding of its own to move to, so it keeps the store it
// opened. That store is also the only place its WORK leg exists, which is the
// load-bearing half: the dispatch reads a control bead's input convoy from it.
//
// It does NOT follow that the city binding never holds beads routed to a rig.
// It does. A city-scoped molecule materializes graph-class control beads into
// the city binding and then stamps gc.routed_to=<rig>/<agent>, so the binding
// accumulates a rig dispatcher's queue while the rig's own ledger stays empty of
// it — 148 such beads stranded on the live city before this was found, against 0
// ever processed. Reading that as "a rig's beads are never in the binding" is
// what let a dispatcher scan a structurally empty ledger for a month and report
// idle rather than fail. A rig scope reaches those beads through
// controlGraphExtraLeg, which is an ADDITIONAL leg precisely because this
// predicate is false for it.
func controlScopeTakesGraphClass(cityPath, storePath string) bool {
	return scopeIsCity(cityPath, storePath)
}

// controlGraphBinding returns the store this scope's control beads live in when
// that store is somewhere the scope directory's own `bd` cannot reach, and
// whether that is the case at all.
//
// It is the question a shell-based readiness scan has to ask before running:
// `bd ready` in the work directory enumerates the copies the migration retained
// there, which no longer receive the workflow's mutations.
func controlGraphBinding(cityPath, storePath string) (beads.Store, bool) {
	if !controlScopeTakesGraphClass(cityPath, storePath) {
		return nil, false
	}
	return graphClassBinding(cliStorageRoutes(cityPath))
}

// controlGraphRelocated reports whether this scope's control beads are served by
// a database the scope directory's own `bd` cannot reach.
func controlGraphRelocated(cityPath, storePath string) bool {
	_, relocated := controlGraphBinding(cityPath, storePath)
	return relocated
}

// controlGraphExtraLeg returns the city's graph binding when a scope must read
// it IN ADDITION to the store it opened, and whether that is the case at all.
//
// This is the complement of controlGraphBinding, not a second copy of it, and
// the two together are the whole routing rule:
//
//   - A CITY scope reads the binding INSTEAD of its own store. `gc storage
//     migrate` copies the class out and RETAINS the source, so the work ledger's
//     rows are frozen at cutover; unioning them back in would re-offer ids the
//     binding has already finished and the drain loop would never return.
//   - A RIG scope reads the binding AS WELL AS its own store. A rig is never a
//     migration target, so it has no retained copies to be misled by — but the
//     binding is CITY-keyed, and a city-scoped molecule materializes its control
//     beads there and then routes them to a rig's dispatcher by name. Those beads
//     are unreachable from the rig directory's own `bd`, which is how a
//     dispatcher ends up scanning forever against a ledger that structurally
//     cannot hold its queue.
//
// The two ledgers hold disjoint ids today precisely because a rig is never a
// migration target, so there is no live copy for the second leg to shadow.
// Callers still take the scope store first; see controlReadyFallbackReady for
// the scan side and controlBeadLedger for the dispatch side.
//
// A REFUSING binding is not a leg. On a city the one-shot funnel refused, every
// infrastructure class resolves to refusedClassStore, whose every read returns
// the standing refusal — so federating it would turn a city-level storage
// misconfiguration into a hard scan error on EVERY rig dispatcher, and a scan
// error is fatal to the drain loop, so all rig control dispatch would crash-loop
// on a city that is otherwise still serving work. cliByIDOwner states the
// governing rule for exactly this error: the standing refusal "is a verdict
// about a CITY's storage configuration and says nothing about a bead, and a
// refused city still serves WORK from its work ledger." A rig's own control
// beads are that case, so the refusal establishes nothing about them and its
// own store still answers. The
// beads the skipped leg would have carried belong to a graph plane that is down
// by this build's own verdict, already reported by the boot gate, and equally
// unreachable to the CITY dispatcher.
//
// The identity gate the sibling surfaces apply (relocatedGraphLegFrom's
// `binding == cityStore`, and storeref's own dedupeLegs, which folds a binding
// that IS the caller's work store into one probed leg) is not restated here:
// this arm runs only for a RIG scope, whose store is that rig's own bd/Dolt
// handle and never the city's binding, and the scan-side caller holds no store
// handle to compare against at all — it shells `bd` for its scope leg.
func controlGraphExtraLeg(cityPath, storePath string) (beads.Store, bool) {
	if controlScopeTakesGraphClass(cityPath, storePath) {
		return nil, false
	}
	binding, relocated := graphClassBinding(cliStorageRoutes(cityPath))
	if !relocated || binding == nil {
		return nil, false
	}
	if _, refused := binding.(refusedClassStore); refused {
		warnControlGraphLegRefused(cityPath)
		return nil, false
	}
	return binding, true
}

// controlGraphLegRefusedOnce keeps the refusal notice to one line per process.
// The dispatcher re-asks this question every tick, and a standing refusal does
// not change until the city's storage config does.
var controlGraphLegRefusedOnce sync.Once

// warnControlGraphLegRefused reports the one thing the skip is not allowed to
// hide: control beads the city binding holds for this rig are unreachable until
// the storage refusal is resolved, so a queue that looks empty may not be.
func warnControlGraphLegRefused(cityPath string) {
	controlGraphLegRefusedOnce.Do(func() {
		log.Printf("control-ready: city %s refuses its storage configuration, so rig control dispatch reads only its own store; any control beads in the city graph binding stay unreachable until `gc storage` is resolved", cityPath)
	})
}

// controlBeadLedger returns the ledger that actually holds beadID, together with
// the bead, so ProcessControl's idempotence gate and every mutation the dispatch
// makes act on ONE copy.
//
// This resolves the GRAPH leg only. The caller keeps its scope store as the WORK
// leg, and that asymmetry is the point rather than an oversight: the binding is
// city-keyed for the graph CLASS, and the work class is not relocated at all, so
// a rig scope here lands on (rig work store, city graph binding) — structurally
// the same shape as the city arm's (city work store, city graph binding), not a
// new one. Measured on the live city: the stranded control beads and their
// workflow root are `gcg-` and binding-resident, while the input convoy those
// same beads name in gc.input_convoy_id is `ga-xz2hu`, absent from the binding
// and present in the rig store. Re-entering the whole dispatch at the city scope
// would fix the graph leg and break the work leg, which is what EmitCurrent reads
// the convoy's tracks edges from.
//
// Residence rather than scope alone is what lets the readiness scan federate
// safely: a federated scan hands the drain loop ids the scope store does not
// hold, and resolving those against the scope store returns "bead not found",
// which IsTransientControllerError does not match — drainWorkflowServeWork
// returns it as fatal and the dispatcher session exits and crash-loops.
//
// The scope store's own class hop stays FIRST, so every id it holds resolves
// exactly where it resolves today and the extra leg is consulted only for ids
// that would otherwise be a hard not-found.
func controlBeadLedger(cityPath, storePath string, cfg *config.City, scopeStore beads.Store, beadID string) (beads.Store, beads.Bead, error) {
	primary := controlGraphStore(cityPath, storePath, cfg, scopeStore)
	bead, err := primary.Get(beadID)
	if err == nil {
		return primary, bead, nil
	}
	extra, federated := controlGraphExtraLeg(cityPath, storePath)
	if !federated || !errors.Is(err, beads.ErrNotFound) {
		return nil, beads.Bead{}, fmt.Errorf("loading control bead %s from the %s for scope %q: %w",
			beadID, controlStoreDescription(cityPath, storePath), storePath, err)
	}
	graphBead, graphErr := extra.Get(beadID)
	if graphErr != nil {
		// BOTH legs are joined rather than one wrapped and one rendered, because
		// the binding's error is the one that decides whether this dispatch is
		// fatal. A binding that is briefly unreachable must reach
		// IsTransientControllerError as a typed error and be retried; rendering
		// it with %v leaves that classification to substring matching on the
		// message, which is luck rather than a contract, and losing the coin
		// flip exits the dispatcher session.
		return nil, beads.Bead{}, fmt.Errorf("loading control bead %s for scope %q: not in the %s, and not in the city graph binding: %w",
			beadID, storePath, controlStoreDescription(cityPath, storePath), errors.Join(err, graphErr))
	}
	return extra, graphBead, nil
}

// controlStoreDescription names the ledger a control-bead read actually went to,
// so a not-found sends the operator to the database that was searched rather
// than to the scope directory that merely selected it.
func controlStoreDescription(cityPath, storePath string) string {
	if controlGraphRelocated(cityPath, storePath) {
		return "graph-class binding"
	}
	return "scoped control store"
}

// controlGraphStore returns the store that owns a control bead and everything
// its dispatch creates, given the scope store the caller resolved.
//
// Control beads are graph class: coordclass counts every gc.kind control bead,
// and the molecule/step topology they mutate, as ClassGraph. The scope store
// answers WHICH city or rig; for the city scope the graph class then answers
// WHICH database inside it. Running control dispatch against the scope store on
// a split city reads the copy the migration retained in the work ledger and
// writes the results back there, where no graph-routed reader looks.
//
// When the routes relocate nothing — every city with no [storage] section, and
// every rig scope — this returns the exact store value it was handed, so those
// callers dispatch against the very store they always did: same bd command
// runner, same scope issue prefix, same instance for the optional-capability
// assertion (DepListBatch) the scope-skip paths make against it.
func controlGraphStore(cityPath, storePath string, cfg *config.City, scopeStore beads.Store) beads.Store {
	return scopeGraphStore(cityPath, storePath, cfg, scopeStore)
}

// openControlStoreAtForCity resolves the control store for a city or rig SCOPE.
// It answers WHICH scope only; the coordination class — which database within
// that scope — is applied by controlGraphStore at the point of use, because the
// control dispatcher needs BOTH: the graph store that owns control beads, and
// this scope/work store that owns the input convoy an execution snapshot reads.
func openControlStoreAtForCity(storePath, cityPath string, cfg *config.City) (beads.Store, error) {
	scopeRoot := resolveStoreScopeRoot(cityPath, storePath)
	provider := rawBeadsProviderForScope(scopeRoot, cityPath)
	if provider == "file" || strings.HasPrefix(provider, "exec:") {
		return openStoreAtForCity(storePath, cityPath)
	}
	if samePath(scopeRoot, cityPath) {
		return openControlBdStoreThroughFactory(scopeRoot, cityPath, provider, cfg, func() (beads.Store, error) {
			return controlBdStoreForCity(scopeRoot, cityPath, cfg), nil
		})
	}
	if cfg != nil {
		for _, rig := range cfg.Rigs {
			rigPath := rig.Path
			if !filepath.IsAbs(rigPath) {
				rigPath = filepath.Join(cityPath, rigPath)
			}
			if samePath(rigPath, scopeRoot) {
				return openControlBdStoreThroughFactory(scopeRoot, cityPath, provider, cfg, func() (beads.Store, error) {
					return controlBdStoreForRig(scopeRoot, cityPath, cfg), nil
				})
			}
		}
	}
	// A bd-backed scope can outlive its rig entry in city.toml. Control paths
	// still need write-capable bd commands with auto-export suppressed.
	return openControlBdStoreThroughFactory(scopeRoot, cityPath, provider, cfg, func() (beads.Store, error) {
		return controlBdStoreForRig(scopeRoot, cityPath, cfg), nil
	})
}

// findBeadScopeAcrossStores tries the city store first, then all rig stores,
// returning the scope store and its path on first match.
//
// It answers WHICH SCOPE owns an id, and nothing else. The bead it reads along
// the way is deliberately not returned: these are unrouted scope stores, so on a
// split city a graph-class bead's value here is the copy the migration retained,
// and a caller that gated on it while writing the graph store would act on work
// the graph store had already finished.
func findBeadScopeAcrossStores(cityPath, beadID string, warningWriter io.Writer) (beads.Store, string, error) {
	// Try city store first.
	cityStore, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		return nil, "", fmt.Errorf("opening city store: %w", err)
	}
	if _, err := cityStore.Get(beadID); err == nil {
		return cityStore, cityPath, nil
	} else if !errors.Is(err, beads.ErrNotFound) {
		return nil, "", fmt.Errorf("getting bead %q from %s: %w", beadID, cityPath, err)
	}

	// Try rig stores.
	cfg, err := loadCityConfig(cityPath, warningWriter)
	if err != nil {
		return nil, "", fmt.Errorf("getting bead %q: not in city store, and config unavailable: %w", beadID, err)
	}
	resolveRigPaths(cityPath, cfg.Rigs)
	for _, rig := range cfg.Rigs {
		store, err := openControlStoreAtForCity(rig.Path, cityPath, cfg)
		if err != nil {
			return nil, "", fmt.Errorf("opening rig store %q: %w", rig.Name, err)
		}
		if _, err := store.Get(beadID); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return nil, "", fmt.Errorf("getting bead %q from %s: %w", beadID, rig.Path, err)
		}
		return store, rig.Path, nil
	}

	// The city and rig SCOPE stores are the copies a migration retained and the
	// rig's own ledger; neither is the city graph binding a split city relocates
	// its control beads into. A city-scoped molecule materializes graph-class
	// control beads there and routes them to a rig by name, so the manual entry
	// point must consult that binding before declaring an id unreachable — the
	// same leg the serve loop federates.
	if store, storePath, err := graphBindingResidentScope(cityPath, cfg, beadID); err == nil {
		return store, storePath, nil
	} else if !errors.Is(err, beads.ErrNotFound) {
		return nil, "", fmt.Errorf("getting bead %q from the city graph binding: %w", beadID, err)
	}
	return nil, "", fmt.Errorf("getting bead %q: %w", beadID, beads.ErrNotFound)
}

// graphBindingResidentScope resolves the SCOPE that owns a control bead which
// lives only in the city graph binding, for the manual `gc convoy control <id>`
// entry point. It returns beads.ErrNotFound when the city relocates no graph
// class or the bead is absent from the binding, so findBeadScopeAcrossStores
// falls through to its own not-found exactly as it did before on unsplit cities.
//
// The store returned is the WORK leg, not the graph leg. controlBeadLedger keeps
// the scope store first and consults the binding as an ADDITIONAL leg, so the
// work class — the input convoy an execution snapshot reads — must resolve where
// it actually lives. A binding-resident bead routed to a rig therefore resolves
// to that RIG's scope, mirroring the serve loop whose rig-scoped scan is what
// surfaces the bead; a bead with no rig route resolves to the city scope, whose
// own graph hop reads the binding directly. Deriving residence from the bead
// rather than defaulting to the city is what keeps a rig-routed bead from losing
// its input-convoy store.
//
// A REFUSING binding is skipped, not surfaced: a standing refusal is a fact about
// the city's storage configuration and none about a particular bead, the graph
// plane is already down by the boot gate's own verdict, and the bead is equally
// unreachable to every scope — so this returns not-found and lets the caller
// report it, exactly as controlGraphExtraLeg skips the same leg on the serve side.
func graphBindingResidentScope(cityPath string, cfg *config.City, beadID string) (beads.Store, string, error) {
	binding, relocated := controlGraphBinding(cityPath, cityPath)
	if !relocated || binding == nil {
		return nil, "", beads.ErrNotFound
	}
	if _, refused := binding.(refusedClassStore); refused {
		warnControlGraphLegRefused(cityPath)
		return nil, "", beads.ErrNotFound
	}
	bead, err := binding.Get(beadID)
	if err != nil {
		return nil, "", err
	}

	// Residence: a bead routed to a rig belongs to that rig's scope, whose work
	// store owns the input convoy. Anything else belongs to the city scope, whose
	// own graph hop resolves the binding directly.
	if cfg != nil {
		if rigContext := workflowExecutionRigContext(bead); rigContext != "" {
			if rig, ok := rigByName(cfg, rigContext); ok {
				store, err := openControlStoreAtForCity(rig.Path, cityPath, cfg)
				if err != nil {
					return nil, "", fmt.Errorf("opening rig store %q for binding-resident control bead %q: %w", rig.Name, beadID, err)
				}
				return store, rig.Path, nil
			}
		}
	}

	cityStore, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		return nil, "", fmt.Errorf("opening city store for binding-resident control bead %q: %w", beadID, err)
	}
	return cityStore, cityPath, nil
}

// findUniqueBeadAcrossStoresView resolves the store view holding beadID, and
// the row itself, refusing an id that more than one store answers.
//
// The binding leg runs in front of the scan for the reason resolveOwningStoreDir
// states: a relocated class binding is not one of the city's directories, so a
// directory scan answers a relocated id from the frozen copy the migration
// retained. That matters more here than for a read — the view this returns is
// the one delete-source and reopen-source WRITE the source bead's metadata
// through, and clearing workflow_id on the frozen twin leaves the live row still
// pointing at a workflow that no longer exists.
//
// A binding hit skips the uniqueness rule for the city's own retained copy,
// which is dual residency working as designed, and still refuses a rig holding
// the same id, which never is.
func findUniqueBeadAcrossStoresView(cityPath, beadID string) (convoyStoreView, beads.Bead, error) {
	cfg, err := loadCityConfig(cityPath, os.Stderr)
	if err != nil {
		return convoyStoreView{}, beads.Bead{}, fmt.Errorf("loading city config for bead %q: %w", beadID, err)
	}
	owner, ownedByBinding, err := cliByIDBindingOwner(cityPath, beadID)
	if err != nil {
		return convoyStoreView{}, beads.Bead{}, err
	}
	if ownedByBinding {
		openStore := func(dir string) (beads.Store, error) { return openStoreAtForCity(dir, cityPath) }
		if err := refuseBindingRigCollision(beadID, cfg, cityPath, openStore); err != nil {
			return convoyStoreView{}, beads.Bead{}, err
		}
		bead, err := beadForOwner(owner, beadID)
		if err != nil {
			return convoyStoreView{}, beads.Bead{}, fmt.Errorf("getting bead %q from %s: %w", beadID, convoyBindingViewPath, err)
		}
		return convoyStoreView{path: convoyBindingViewPath, store: owner.Store, role: convoyViewClassBinding}, bead, nil
	}
	stores, skips, err := openSourceWorkflowStores(cfg, cityPath, beadID)
	if err != nil {
		return convoyStoreView{}, beads.Bead{}, err
	}
	if len(skips) > 0 {
		// Surface skipped stores so a not-found isn't silently masking a
		// store we couldn't open.
		fmt.Fprintln(os.Stderr, "warning:", formatSourceWorkflowStoreSkips(skips)) //nolint:errcheck
	}
	var (
		foundView convoyStoreView
		foundBead beads.Bead
		found     bool
	)
	for _, candidate := range stores {
		bead, err := candidate.store.Get(beadID)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return convoyStoreView{}, beads.Bead{}, fmt.Errorf("getting bead %q from %s: %w", beadID, candidate.path, err)
		}
		if found {
			return convoyStoreView{}, beads.Bead{}, fmt.Errorf(
				"source bead %s exists in multiple stores (%s and %s); source workflow commands require a uniquely resolvable source bead id",
				beadID,
				foundView.path,
				candidate.path,
			)
		}
		foundView = candidate
		foundBead = bead
		found = true
	}
	if !found {
		return convoyStoreView{}, beads.Bead{}, fmt.Errorf("getting bead %q: %w", beadID, beads.ErrNotFound)
	}
	return foundView, foundBead, nil
}

func workflowFormulaSearchPaths(cfg *config.City, bead beads.Bead) []string {
	if cfg == nil {
		return nil
	}
	if rigName := strings.TrimSpace(bead.Metadata[graphroute.GraphExecutionRigContextMetaKey]); rigName != "" {
		if paths := cfg.FormulaLayers.SearchPaths(rigName); len(paths) > 0 {
			return paths
		}
	}
	routedTo := graphroute.WorkflowExecutionRoute(bead)
	if routedTo == "" {
		return cfg.FormulaLayers.City
	}
	rigName, _ := config.ParseQualifiedName(routedTo)
	if paths := cfg.FormulaLayers.SearchPaths(rigName); len(paths) > 0 {
		return paths
	}
	return cfg.FormulaLayers.City
}

func decorateDynamicFragmentRecipe(fragment *formula.FragmentRecipe, source beads.Bead, store beads.Store, cityName, cityPath string, cfg *config.City) error {
	if fragment == nil {
		return fmt.Errorf("fragment recipe is nil")
	}
	defaultRoute, err := graphFallbackBindingForBead(source, store, cityName, cityPath, cfg)
	if err != nil {
		return err
	}
	routingRigContext := strings.TrimSpace(defaultRoute.RigContext)
	if routingRigContext == "" {
		routingRigContext = graphroute.GraphRouteRigContext(defaultRoute.QualifiedName)
	}
	storeRigContext, storeScoped := storeref.ScopeRigContext(source.Metadata[beadmeta.RootStoreRefMetadataKey])
	controlRoutes := map[string]graphRouteBinding{}
	controlRouteFor := func(rigContext string) (graphRouteBinding, error) {
		rigContext = strings.TrimSpace(rigContext)
		if binding, ok := controlRoutes[rigContext]; ok {
			return binding, nil
		}
		binding, err := graphroute.ControlDispatcherBinding(store, cityName, cfg, rigContext, cliGraphrouteDeps(cityPath))
		if err != nil {
			return graphRouteBinding{}, err
		}
		controlRoutes[rigContext] = binding
		return binding, nil
	}

	for i := range fragment.Steps {
		step := &fragment.Steps[i]
		if step.Metadata == nil {
			step.Metadata = make(map[string]string)
		} else {
			step.Metadata = maps.Clone(step.Metadata)
		}
		step.Metadata[beadmeta.DynamicFragmentMetadataKey] = "true"
		propagateDynamicScopeMetadata(step, source)
	}
	formula.ApplyFragmentRecipeGraphControls(fragment)

	stepByID := make(map[string]*formula.RecipeStep, len(fragment.Steps))
	stepAlias := make(map[string]string, len(fragment.Steps))
	for i := range fragment.Steps {
		stepByID[fragment.Steps[i].ID] = &fragment.Steps[i]
		if short, ok := strings.CutPrefix(fragment.Steps[i].ID, fragment.Name+"."); ok {
			stepAlias[short] = fragment.Steps[i].ID
		}
	}
	depsByStep := make(map[string][]string, len(fragment.Deps))
	for _, dep := range fragment.Deps {
		if dep.Type != "blocks" && dep.Type != "waits-for" && dep.Type != "conditional-blocks" {
			continue
		}
		depsByStep[dep.StepID] = append(depsByStep[dep.StepID], dep.DependsOnID)
	}
	bindingCache := make(map[string]graphRouteBinding, len(fragment.Steps))
	resolving := make(map[string]bool, len(fragment.Steps))
	for i := range fragment.Steps {
		step := &fragment.Steps[i]
		switch step.Metadata[beadmeta.KindMetadataKey] {
		case "workflow", "scope", "ralph", "retry", "spec":
			continue
		}
		binding, err := resolveGraphStepBinding(step.ID, stepByID, stepAlias, depsByStep, bindingCache, resolving, defaultRoute, routingRigContext, store, cityName, cityPath, cfg)
		if err != nil {
			return err
		}
		// Capture the formula-declared continuation group from the authored
		// (freshly-cloned) step metadata so the pool opt-in is sourced immutably
		// through the binding, matching DecorateGraphWorkflowRecipeWithDefaultBinding.
		// The leaf now clears any group the binding does not carry, so omitting
		// this on the dynamic-fragment path would drop every declared group.
		binding.ContinuationGroup = strings.TrimSpace(step.Metadata[beadmeta.ContinuationGroupMetadataKey])
		if graphroute.IsControlDispatcherKind(step.Metadata[beadmeta.KindMetadataKey]) {
			controlRigContext := graphRouteBindingRigContext(binding)
			if storeScoped {
				controlRigContext = storeRigContext
			} else if controlRigContext == "" {
				controlRigContext = routingRigContext
			}
			controlRoute, err := controlRouteFor(controlRigContext)
			if err != nil {
				return err
			}
			graphroute.AssignGraphStepRoute(step, binding, &controlRoute)
			continue
		}
		graphroute.AssignGraphStepRoute(step, binding, nil)
	}
	return nil
}

func graphRouteBindingRigContext(binding graphRouteBinding) string {
	if rigContext := strings.TrimSpace(binding.RigContext); rigContext != "" {
		return rigContext
	}
	return graphroute.GraphRouteRigContext(binding.QualifiedName)
}

func decorateDrainItemRecipe(recipe *formula.Recipe, source beads.Bead, store beads.Store, storeRef, cityName, cityPath string, cfg *config.City) error {
	if recipe == nil {
		return fmt.Errorf("recipe is nil")
	}
	routedTo := graphroute.WorkflowExecutionRoute(source)
	if strings.TrimSpace(routedTo) == "" {
		if strings.TrimSpace(source.Metadata[beadmeta.KindMetadataKey]) == beadmeta.KindDrain {
			vars, err := drainItemRecipeVars(recipe)
			if err != nil {
				return err
			}
			scopeKind := strings.TrimSpace(source.Metadata[beadmeta.ScopeKindMetadataKey])
			scopeRef := strings.TrimSpace(source.Metadata[beadmeta.ScopeRefMetadataKey])
			return graphroute.DecorateGraphWorkflowRecipeWithDefaultBinding(recipe, graphroute.GraphWorkflowRouteVars(recipe, vars), "", scopeKind, scopeRef, storeRef, graphroute.GraphRouteBinding{}, store, cityName, cfg, cliGraphrouteDeps(cityPath))
		}
		binding, err := graphFallbackBindingForBead(source, store, cityName, cityPath, cfg)
		if err != nil {
			return err
		}
		if binding.QualifiedName == "" && binding.SessionName == "" && binding.DirectSessionID == "" {
			return nil
		}
		vars, err := drainItemRecipeVars(recipe)
		if err != nil {
			return err
		}
		scopeKind := strings.TrimSpace(source.Metadata[beadmeta.ScopeKindMetadataKey])
		scopeRef := strings.TrimSpace(source.Metadata[beadmeta.ScopeRefMetadataKey])
		return graphroute.DecorateGraphWorkflowRecipe(recipe, graphroute.GraphWorkflowRouteVars(recipe, vars), "", scopeKind, scopeRef, storeRef, binding.QualifiedName, binding.SessionName, store, cityName, cfg, cliGraphrouteDeps(cityPath))
	}
	vars, err := drainItemRecipeVars(recipe)
	if err != nil {
		return err
	}
	scopeKind := strings.TrimSpace(source.Metadata[beadmeta.ScopeKindMetadataKey])
	scopeRef := strings.TrimSpace(source.Metadata[beadmeta.ScopeRefMetadataKey])
	if binding, ok, err := graphroute.ResolveGraphDirectSessionBinding(store, cityName, cfg, routedTo, workflowExecutionRigContext(source), cliGraphrouteDeps(cityPath)); err != nil {
		return err
	} else if ok {
		defaultRoute := graphroute.GraphRouteBinding{DirectSessionID: binding.DirectSessionID, RigContext: binding.RigContext}
		return graphroute.DecorateGraphWorkflowRecipeWithDefaultBinding(recipe, graphroute.GraphWorkflowRouteVars(recipe, vars), "", scopeKind, scopeRef, storeRef, defaultRoute, store, cityName, cfg, cliGraphrouteDeps(cityPath))
	}
	return applyGraphRouting(recipe, nil, routedTo, vars, scopeKind, scopeRef, storeRef, store, cityName, cityPath, cfg)
}

func workflowExecutionRigContext(bead beads.Bead) string {
	if bead.Metadata == nil {
		return ""
	}
	if rigContext := strings.TrimSpace(bead.Metadata[graphroute.GraphExecutionRigContextMetaKey]); rigContext != "" {
		return rigContext
	}
	return graphroute.GraphRouteRigContext(graphroute.WorkflowExecutionRoute(bead))
}

func drainItemRecipeVars(recipe *formula.Recipe) (map[string]string, error) {
	vars := map[string]string{}
	if root := recipe.RootStep(); root != nil {
		if raw := strings.TrimSpace(root.Metadata[graphv2.RuntimeVarsMetadataKey]); raw != "" {
			decoded, err := graphv2.ParseRuntimeVarsMetadata(raw)
			if err != nil {
				return nil, fmt.Errorf("parsing drain item runtime vars: %w", err)
			}
			maps.Copy(vars, decoded)
		}
		if inputConvoyID := strings.TrimSpace(root.Metadata[beadmeta.InputConvoyIDMetadataKey]); inputConvoyID != "" {
			vars["convoy_id"] = inputConvoyID
		}
	}
	return vars, nil
}

func graphFallbackBindingForBead(source beads.Bead, store beads.Store, cityName, cityPath string, cfg *config.City) (graphRouteBinding, error) {
	routedTo := graphroute.WorkflowExecutionRoute(source)
	if routedTo == "" {
		return graphRouteBinding{SessionName: source.Assignee}, nil
	}
	rigContext := workflowExecutionRigContext(source)
	if binding, ok, err := graphroute.ResolveGraphDirectSessionBinding(store, cityName, cfg, routedTo, rigContext, cliGraphrouteDeps(cityPath)); err != nil {
		return graphRouteBinding{}, err
	} else if ok {
		return binding, nil
	}
	if cfg == nil {
		return graphRouteBinding{}, fmt.Errorf("formulas v2 routing for %s requires config", source.ID)
	}

	agentCfg, ok := resolveAgentIdentity(cfg, routedTo, rigContext)
	if !ok {
		return graphRouteBinding{}, fmt.Errorf("unknown formulas v2 fallback target %q on %s", routedTo, source.ID)
	}

	binding := graphRouteBinding{QualifiedName: agentutil.RoutedToIdentity(&agentCfg)}
	if agentCfg.SupportsInstanceExpansion() {
		binding.MetadataOnly = true
		return binding, nil
	}
	if source.Assignee != "" {
		binding.SessionName = source.Assignee
		return binding, nil
	}
	sn := lookupSessionNameOrLegacy(store, cityName, agentCfg.QualifiedName(), cfg.Workspace.SessionTemplate)
	if sn == "" {
		return graphRouteBinding{}, fmt.Errorf("could not resolve session name for %q", agentCfg.QualifiedName())
	}
	binding.SessionName = sn
	return binding, nil
}

func propagateDynamicScopeMetadata(step *formula.RecipeStep, source beads.Bead) {
	if step == nil {
		return
	}
	if step.Metadata == nil {
		step.Metadata = make(map[string]string)
	}
	if rootStoreRef := strings.TrimSpace(source.Metadata[beadmeta.RootStoreRefMetadataKey]); rootStoreRef != "" {
		// Dynamically attached steps live in the source graph store. Overwrite a
		// stale template value before routing so gc.routed_to and the store ref
		// molecule.Attach persists cannot disagree.
		step.Metadata[beadmeta.RootStoreRefMetadataKey] = rootStoreRef
	}
	if scopeRef := strings.TrimSpace(source.Metadata[beadmeta.ScopeRefMetadataKey]); scopeRef != "" && step.Metadata[beadmeta.ScopeRefMetadataKey] == "" {
		step.Metadata[beadmeta.ScopeRefMetadataKey] = scopeRef
	}
	if onFail := strings.TrimSpace(source.Metadata[beadmeta.OnFailMetadataKey]); onFail != "" && step.Metadata[beadmeta.OnFailMetadataKey] == "" {
		step.Metadata[beadmeta.OnFailMetadataKey] = onFail
	}
	for _, key := range []string{beadmeta.StepIDMetadataKey, beadmeta.RalphStepIDMetadataKey, beadmeta.AttemptMetadataKey} {
		if value := strings.TrimSpace(source.Metadata[key]); value != "" && step.Metadata[key] == "" {
			step.Metadata[key] = value
		}
	}
	if step.Metadata[beadmeta.ScopeRefMetadataKey] == "" || step.Metadata[beadmeta.ScopeRoleMetadataKey] != "" {
		return
	}
	kind := step.Metadata[beadmeta.KindMetadataKey]
	switch {
	case kind == beadmeta.KindScope:
		return
	case beadmeta.IsControlKind(kind):
		step.Metadata[beadmeta.ScopeRoleMetadataKey] = beadmeta.ScopeRoleControl
	default:
		step.Metadata[beadmeta.ScopeRoleMetadataKey] = beadmeta.ScopeRoleMember
	}
}

func newConvoyDeleteCmd(stdout, stderr io.Writer) *cobra.Command {
	var force bool
	var deleteBeads bool
	cmd := &cobra.Command{
		Use:   "delete <convoy-id>",
		Short: "Close or delete a convoy and all its beads",
		Long: `Close all open beads in a convoy, or delete them.

Searches all stores (city + rigs) for the convoy root and all beads
with matching gc.root_bead_id. Without --force, shows a preview.

By default, beads are closed with gc.outcome=skipped. Use --delete to
remove them from the store via bd delete --cascade --force.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdWorkflowDelete(args[0], force, deleteBeads, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Actually close/delete (without this, shows preview)")
	cmd.Flags().BoolVar(&deleteBeads, "delete", false, "Delete beads from the store instead of closing")
	return cmd
}

func newConvoyDeleteSourceCmd(stdout, stderr io.Writer) *cobra.Command {
	var apply bool
	var deleteBeads bool
	var rigName string
	var storeRef string
	cmd := &cobra.Command{
		Use:   "delete-source <source-bead-id>",
		Short: "Close workflows sourced from a bead",
		Long: `Find every live workflow root sourced from the given bead and close
its subtree. By default this is a preview. Use --apply to mutate.
Use --delete with --apply to also delete closed beads.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if deleteBeads && !apply {
				fmt.Fprintln(stderr, "gc workflow delete-source: --delete requires --apply") //nolint:errcheck
				return errExit
			}
			selector, err := parseSourceWorkflowStoreSelector(rigName, storeRef)
			if err != nil {
				_, _ = fmt.Fprintf(stderr, "gc workflow delete-source: %v\n", err)
				return errExit
			}
			return exitForCode(cmdWorkflowDeleteSource(args[0], selector, apply, deleteBeads, stdout, stderr))
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "Actually close/delete matched workflows")
	cmd.Flags().BoolVar(&deleteBeads, "delete", false, "Also delete beads from the store after closing")
	cmd.Flags().StringVar(&rigName, "rig", "", "Select the rig store for the source bead")
	cmd.Flags().StringVar(&storeRef, "store-ref", "", "Select the source bead store (city:<name> or rig:<name>)")
	return cmd
}

func newConvoyReopenSourceCmd(stdout, stderr io.Writer) *cobra.Command {
	var rigName string
	var storeRef string
	cmd := &cobra.Command{
		Use:   "reopen-source <source-bead-id>",
		Short: "Reopen a source bead after workflow cleanup",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			selector, err := parseSourceWorkflowStoreSelector(rigName, storeRef)
			if err != nil {
				_, _ = fmt.Fprintf(stderr, "gc workflow reopen-source: %v\n", err)
				return errExit
			}
			return exitForCode(cmdWorkflowReopenSource(args[0], selector, stdout, stderr))
		},
	}
	cmd.Flags().StringVar(&rigName, "rig", "", "Select the rig store for the source bead")
	cmd.Flags().StringVar(&storeRef, "store-ref", "", "Select the source bead store (city:<name> or rig:<name>)")
	return cmd
}

type workflowStoreMatch struct {
	store  beads.Store
	beads  []beads.Bead
	label  string
	path   string
	role   convoyViewRole
	runner beads.CommandRunner
}

// refusePartialSweep is the single refusal every destructive arm speaks when a
// store it was told to sweep could not answer.
//
// This is the one policy difference between a sweep and a read fan-out. A read
// prints what it reached and names what it could not; a destructive one-shot has
// no such output — "I could not see the binding's tree" and "the binding's tree
// is gone" leave the operator believing the same thing, and only one of them is
// true.
//
// It is one function because the ways a store stops answering do not look alike
// and have to be treated alike. A standing refusal arrives at federation time; a
// dropped connection arrives mid-scan and shows up as zero rows, which is what
// an empty store contributes; a lock contended mid-close arrives after the other
// views have already been swept. Each of those was, at some point, handled
// somewhere else and differently, and every one of them ends in a store that
// COULD NOT ANSWER being treated as a store with NOTHING TO SAY — the partial
// sweep reported as success. Routing all three here is what keeps the three
// callers from drifting apart again.
// It carries no command name: every caller already prefixes what it prints with
// its own, and the source-workflow arms surface this through a returned error
// that the command prefixes on the way out.
func refusePartialSweep(operation, storeLabel string, cause error) error {
	return fmt.Errorf("%s %s failed, so part of the workflow is left untouched: %w", operation, storeLabel, cause)
}

// federateSweepViews federates a sweep's store views with the city's relocated
// class binding, and turns a binding this build must not serve into a refusal
// rather than a store the sweep quietly does without.
func federateSweepViews(cityPath string, views []convoyStoreView) ([]convoyStoreView, error) {
	merged, err := convoyStoreViewsWithBinding(cityPath, views)
	if err != nil {
		return nil, refusePartialSweep("resolving", convoyBindingViewPath, err)
	}
	return merged, nil
}

// workflowDeleteStoreLabelForView names a store view in sweep output. The class
// binding has no directory to fall back on, so it is named for what it is.
func workflowDeleteStoreLabelForView(cfg *config.City, cityPath string, view convoyStoreView) string {
	if view.isClassBinding() {
		return convoyBindingViewPath
	}
	return workflowDeleteStoreLabel(cfg, cityPath, view.path)
}

// workflowDeleteRunnerForView returns the `bd` runner for a view's scope, and
// nil for the class binding.
//
// bd runs IN a directory. The binding is a database the city was rebound to, not
// a directory, so there is nothing to root an invocation at; its beads are
// deleted through the store handle instead — the same route delete-source has
// always taken for every store.
func workflowDeleteRunnerForView(cfg *config.City, cityPath string, view convoyStoreView) beads.CommandRunner {
	if view.isClassBinding() {
		return nil
	}
	return workflowDeleteRunnerForPath(cfg, cityPath, view.path)
}

func cmdWorkflowDelete(workflowID string, force, deleteBeads bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gc workflow delete: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc workflow delete: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	resolveRigPaths(cityPath, cfg.Rigs)

	var matches []workflowStoreMatch

	stores, err := openConvoyStores(cfg, cityPath, workflowID, func(dir string) (beads.Store, error) {
		return openControlStoreAtForCity(dir, cityPath, cfg)
	})
	if err != nil {
		fmt.Fprintf(stderr, "gc workflow delete: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	// The scan enumerates directories, and a relocated binding is not one. Every
	// row it holds is the row the city is actually running; the copies retained
	// in the work ledger are frozen twins of the same ids. Both are swept — the
	// operator is erasing the workflow, not choosing between its copies.
	stores, err = federateSweepViews(cityPath, stores)
	if err != nil {
		fmt.Fprintf(stderr, "gc workflow delete: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	for _, info := range stores {
		label := workflowDeleteStoreLabelForView(cfg, cityPath, info)
		found, err := findWorkflowBeads(info.store, workflowID)
		if err != nil {
			fmt.Fprintf(stderr, "gc workflow delete: %v\n", refusePartialSweep("scanning", label, err)) //nolint:errcheck // best-effort stderr
			return 1
		}
		if len(found) == 0 {
			continue
		}
		matches = append(matches, workflowStoreMatch{
			store:  info.store,
			beads:  found,
			label:  label,
			path:   info.path,
			role:   info.role,
			runner: workflowDeleteRunnerForView(cfg, cityPath, info),
		})
	}

	total := 0
	openCount := 0
	for _, m := range matches {
		total += len(m.beads)
		for _, b := range m.beads {
			if b.Status != "closed" {
				openCount++
			}
		}
	}
	if total == 0 {
		fmt.Fprintf(stderr, "gc workflow delete: no beads found for workflow %s\n", workflowID) //nolint:errcheck // best-effort stderr
		return 1
	}

	action := "close"
	if deleteBeads {
		action = "delete"
	}
	fmt.Fprintf(stdout, "Workflow %s: %d beads (%d open) — %s\n", workflowID, total, openCount, action) //nolint:errcheck // best-effort stdout
	for _, m := range matches {
		fmt.Fprintf(stdout, "  %s: %d beads\n", m.label, len(m.beads)) //nolint:errcheck // best-effort stdout
	}

	if !force {
		fmt.Fprintln(stdout, "\nDry run. Use --force to proceed.") //nolint:errcheck // best-effort stdout
		return 0
	}

	if deleteBeads {
		deleted, err := deleteWorkflowMatches(matches)
		if err != nil {
			fmt.Fprintf(stderr, "gc workflow delete: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		fmt.Fprintf(stdout, "Deleted %d beads\n", deleted) //nolint:errcheck // best-effort stdout
		return 0
	}

	closed, err := closeWorkflowMatches(matches)
	if err != nil {
		fmt.Fprintf(stdout, "Closed %d open beads\n", closed) //nolint:errcheck // best-effort stdout
		fmt.Fprintf(stderr, "gc workflow delete: %v\n", err)  //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "Closed %d open beads\n", closed) //nolint:errcheck // best-effort stdout
	return 0
}

// sweepOrder is the match order a MUTATION runs in: the class binding first,
// then everything else in the order the views were collected.
//
// The binding is appended last by the federation, which is the right order to
// PRINT (it reads as one more store after the city's own) and the wrong order to
// write in. On a converged city the binding holds the tree the city is running
// and the work ledger holds the frozen twins the migration retained, so a sweep
// that writes the ledger first and then faults has stopped the copy nobody was
// watching and left the live tree running. Writing the binding first inverts
// both outcomes: a fault before it touches anything sweeps nothing, and a fault
// after it means the workflow really did stop and a cosmetic twin is left open.
func sweepOrder(matches []workflowStoreMatch) []workflowStoreMatch {
	ordered := make([]workflowStoreMatch, 0, len(matches))
	for _, m := range matches {
		if m.role == convoyViewClassBinding {
			ordered = append(ordered, m)
		}
	}
	for _, m := range matches {
		if m.role != convoyViewClassBinding {
			ordered = append(ordered, m)
		}
	}
	return ordered
}

// closeWorkflowMatches closes every matched bead in every matched store, and
// reports the first store that would not finish.
//
// The close IS the destructive act of the default mode, and its error used to be
// discarded — so `gc workflow delete` on a converged city could close the
// retained frozen copies, no-op on the live tree because the binding refused
// every write, print the count it managed and exit 0. Delete mode fails loud and
// delete-source verifies what it closed; the default mode of the same command
// now does both.
func closeWorkflowMatches(matches []workflowStoreMatch) (int, error) {
	closed := 0
	for _, m := range sweepOrder(matches) {
		ids := workflowBeadIDs(m.beads)
		n, err := m.store.CloseAll(ids, map[string]string{
			beadmeta.OutcomeMetadataKey: beadmeta.OutcomeSkipped,
			"close_reason":              sourceworkflow.WorkflowSkippedCloseReason,
		})
		closed += n
		if err != nil {
			return closed, refusePartialSweep("closing beads in", m.label, err)
		}
	}
	if err := verifyWorkflowMatchesClosed(matches); err != nil {
		return closed, err
	}
	return closed, nil
}

// verifyWorkflowMatchesClosed re-reads every bead the sweep closed and refuses
// if any is still open.
//
// A store that accepts a close and does not apply it reports the same success as
// one that did, which is countOpenMatchedBeads' reason on the delete-source arm.
// A bead that has since been DELETED is gone, which is what the sweep wanted.
func verifyWorkflowMatchesClosed(matches []workflowStoreMatch) error {
	for _, m := range matches {
		for _, b := range m.beads {
			current, err := m.store.Get(b.ID)
			if err != nil {
				if errors.Is(err, beads.ErrNotFound) {
					continue
				}
				return refusePartialSweep("re-reading beads in", m.label, err)
			}
			if current.Status != "closed" {
				return refusePartialSweep("closing beads in", m.label,
					fmt.Errorf("bead %s is still %s after the sweep", b.ID, current.Status))
			}
		}
	}
	return nil
}

func workflowDeleteRunnerForPath(cfg *config.City, cityPath, scopePath string) beads.CommandRunner {
	if samePath(scopePath, cityPath) {
		return bdCommandRunnerForCity(cityPath)
	}
	return bdCommandRunnerForRig(cityPath, cfg, scopePath)
}

// deleteWorkflowMatches erases every matched bead in every matched store.
//
// The binding is deleted through the store handle rather than a `bd` invocation
// and is swept first, for sweepOrder's reason.
func deleteWorkflowMatches(matches []workflowStoreMatch) (int, error) {
	deleted := 0
	for _, m := range sweepOrder(matches) {
		ids := workflowBeadIDs(m.beads)
		if m.role == convoyViewClassBinding {
			// No directory, so no bd invocation. The store handle is the whole
			// access path to a relocated class, and it is the same one
			// delete-source deletes every match through.
			n, errs := deleteWorkflowBeads(m.store, ids)
			deleted += n
			if len(errs) > 0 {
				return deleted, refusePartialSweep("deleting beads in", m.label, errors.Join(errs...))
			}
			continue
		}
		if m.runner == nil {
			return deleted, refusePartialSweep("deleting beads in", m.label, errors.New("delete runner missing"))
		}
		args := append([]string{"delete"}, ids...)
		args = append(args, "--cascade", "--force")
		if _, err := m.runner(m.path, "bd", args...); err != nil {
			return deleted, refusePartialSweep("deleting beads in", m.label, err)
		}
		deleted += len(ids)
	}
	return deleted, nil
}

type sourceWorkflowStoreMatch struct {
	label string
	store beads.Store
	roots []beads.Bead
	beads []beads.Bead
	path  string
	// scopePath is the directory whose scope owns these rows. It differs from
	// path only for the class binding, whose rows are the city's — which is what
	// keeps a converged city's two matched views from reading as two stores.
	scopePath string
}

// distinctSourceWorkflowScopes counts the SCOPES a match set spans.
//
// The multi-store guard exists because a live root in a rig and a live root in
// the city are two different workflows, and delete-source cannot pick one. A
// converged city's binding is not that case: it and the city's work ledger are
// one scope holding one workflow in two copies, deliberately, and refusing there
// would take delete-source away from every city that has migrated.
func distinctSourceWorkflowScopes(matches []sourceWorkflowStoreMatch) []string {
	scopes := make([]string, 0, len(matches))
	for _, match := range matches {
		scope := match.scopePath
		if strings.TrimSpace(scope) == "" {
			scope = match.path
		}
		if !slices.ContainsFunc(scopes, func(existing string) bool { return samePath(existing, scope) }) {
			scopes = append(scopes, scope)
		}
	}
	return scopes
}

type sourceWorkflowStoreSelector struct {
	storeRef string
}

type resolvedSourceWorkflowTarget struct {
	sourceBeadID string
	storeRef     string
	storeView    convoyStoreView
	sourceBead   beads.Bead
}

func parseSourceWorkflowStoreSelector(rigName, storeRef string) (sourceWorkflowStoreSelector, error) {
	rigName = strings.TrimSpace(rigName)
	storeRef = strings.TrimSpace(storeRef)
	if rigName != "" && storeRef != "" {
		return sourceWorkflowStoreSelector{}, fmt.Errorf("--rig and --store-ref are mutually exclusive")
	}
	if rigName != "" {
		storeRef = "rig:" + rigName
	}
	return sourceWorkflowStoreSelector{storeRef: storeRef}, nil
}

func resolveSourceWorkflowTarget(cfg *config.City, cityPath, sourceBeadID string, selector sourceWorkflowStoreSelector, requireSource bool) (resolvedSourceWorkflowTarget, error) {
	sourceBeadID = sourceworkflow.NormalizeSourceBeadID(sourceBeadID)
	target := resolvedSourceWorkflowTarget{sourceBeadID: sourceBeadID}
	if selector.storeRef != "" {
		view, resolvedStoreRef, err := openSourceWorkflowStoreRef(cfg, cityPath, selector.storeRef)
		if err != nil {
			return resolvedSourceWorkflowTarget{}, err
		}
		target.storeRef = resolvedStoreRef
		target.storeView = view
		bead, err := view.store.Get(sourceBeadID)
		switch {
		case err == nil:
			target.sourceBead = bead
		case errors.Is(err, beads.ErrNotFound):
			if requireSource {
				return resolvedSourceWorkflowTarget{}, fmt.Errorf("getting bead %q: %w", sourceBeadID, beads.ErrNotFound)
			}
		default:
			return resolvedSourceWorkflowTarget{}, fmt.Errorf("getting bead %q from %s: %w", sourceBeadID, workflowDeleteStoreLabelForView(cfg, cityPath, view), err)
		}
		return target, nil
	}
	view, bead, err := findUniqueBeadAcrossStoresView(cityPath, sourceBeadID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) && !requireSource {
			return target, nil
		}
		return resolvedSourceWorkflowTarget{}, sourceWorkflowSelectionError(err, sourceBeadID)
	}
	target.storeView = view
	target.sourceBead = bead
	target.storeRef = workflowStoreRefForDir(view.scopePath(cityPath), cityPath, loadedCityName(cfg, cityPath), cfg)
	return target, nil
}

func sourceWorkflowSelectionError(err error, sourceBeadID string) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "exists in multiple stores") {
		return fmt.Errorf("%w; rerun with --rig <name> or --store-ref <city:name|rig:name>", err)
	}
	if errors.Is(err, beads.ErrNotFound) {
		return fmt.Errorf("getting bead %q: %w", sourceBeadID, beads.ErrNotFound)
	}
	return err
}

func openSourceWorkflowStoreRef(cfg *config.City, cityPath, storeRef string) (convoyStoreView, string, error) {
	storeRef = strings.TrimSpace(storeRef)
	switch {
	case storeRef == "", storeRef == "city":
		store, err := openStoreAtForCity(cityPath, cityPath)
		if err != nil {
			return convoyStoreView{}, "", fmt.Errorf("opening city store: %w", err)
		}
		cityName := loadedCityName(cfg, cityPath)
		if cityName == "" {
			cityName = "city"
		}
		return convoyStoreView{path: cityPath, store: store}, "city:" + cityName, nil
	case strings.HasPrefix(storeRef, "city:"):
		store, err := openStoreAtForCity(cityPath, cityPath)
		if err != nil {
			return convoyStoreView{}, "", fmt.Errorf("opening city store: %w", err)
		}
		return convoyStoreView{path: cityPath, store: store}, storeRef, nil
	case strings.HasPrefix(storeRef, "rig:"):
		rigName := strings.TrimPrefix(storeRef, "rig:")
		for _, rig := range cfg.Rigs {
			if rig.Name != rigName {
				continue
			}
			rigPath := resolveStoreScopeRoot(cityPath, rig.Path)
			store, err := openStoreAtForCity(rigPath, cityPath)
			if err != nil {
				return convoyStoreView{}, "", fmt.Errorf("opening rig store %s: %w", rigName, err)
			}
			return convoyStoreView{path: rigPath, store: store}, "rig:" + rigName, nil
		}
		return convoyStoreView{}, "", fmt.Errorf("rig %q not found", rigName)
	default:
		return convoyStoreView{}, "", fmt.Errorf("invalid --store-ref %q (want city:<name> or rig:<name>)", storeRef)
	}
}

func applySourceWorkflowMatchCleanup(match sourceWorkflowStoreMatch, deleteBeads bool, stderr io.Writer) (closed, deleted int, incomplete bool) {
	ids := workflowBeadIDs(match.beads)
	n, closeErr := match.store.CloseAll(ids, map[string]string{
		beadmeta.OutcomeMetadataKey: beadmeta.OutcomeSkipped,
		"close_reason":              sourceworkflow.WorkflowSkippedCloseReason,
	})
	closed += n
	if closeErr != nil {
		incomplete = true
		_, _ = fmt.Fprintf(stderr, "store=%s close_error=%v\n", match.label, closeErr)
		return closed, deleted, incomplete
	}
	if !deleteBeads {
		return closed, deleted, incomplete
	}
	count, errs := deleteSourceWorkflowMatchBeads(match, ids)
	deleted += count
	for _, deleteErr := range errs {
		incomplete = true
		_, _ = fmt.Fprintf(stderr, "store=%s delete_error=%v\n", match.label, deleteErr)
	}
	return closed, deleted, incomplete
}

func deleteSourceWorkflowMatchBeads(match sourceWorkflowStoreMatch, ids []string) (int, []error) {
	if len(ids) == 0 {
		return 0, nil
	}
	return deleteWorkflowBeads(match.store, ids)
}

func cmdWorkflowDeleteSource(sourceBeadID string, selector sourceWorkflowStoreSelector, apply, deleteBeads bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gc workflow delete-source: %v\n", err)
		return 1
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gc workflow delete-source: %v\n", err)
		return 1
	}

	var (
		resultCode int
		runErr     error
	)
	target, err := resolveSourceWorkflowTarget(cfg, cityPath, sourceBeadID, selector, false)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gc workflow delete-source: %v\n", err)
		return 1
	}
	// The lock follows the view's SCOPE, not the view. Every lock file lives
	// under the city's runtime dir and the scope is only HASHED into its name,
	// which is what makes getting this wrong quiet: the binding's own view path
	// is a perfectly valid scope, it just hashes to a different file, so a
	// binding-resolved run and a city-resolved run over the same workflow would
	// each take a lock and neither would see the other. A relocated class
	// binding's rows are the city's, so it locks the city.
	lockScope := target.storeView.scopePath(cityPath)
	if strings.TrimSpace(lockScope) == "" {
		lockScope = cityPath
	}
	ctx, cancel := sourceWorkflowCommandContext()
	defer cancel()
	runErr = sourceworkflow.WithLock(ctx, cityPath, lockScope, sourceBeadID, func() error {
		target, err := resolveSourceWorkflowTarget(cfg, cityPath, sourceBeadID, selector, false)
		if err != nil {
			return err
		}
		matches, skips, err := collectSourceWorkflowMatches(cfg, cityPath, sourceBeadID, target.storeRef)
		if err != nil {
			return err
		}
		if len(skips) > 0 {
			// delete-source cannot close live roots it can't see. Warn
			// rather than silently declaring success.
			fmt.Fprintln(stderr, "warning:", formatSourceWorkflowStoreSkips(skips)) //nolint:errcheck
		}
		if target.storeRef == "" && len(distinctSourceWorkflowScopes(matches)) > 1 {
			return fmt.Errorf(
				"source workflow %s has live roots in multiple stores (%s); rerun with --rig <name> or --store-ref <city:name|rig:name>",
				sourceBeadID,
				strings.Join(sourceWorkflowMatchLabels(matches), ", "),
			)
		}
		totalRoots, totalBeads, openCount := summarizeSourceWorkflowMatches(matches)
		if totalRoots == 0 {
			cleared := false
			if apply {
				var clearErr error
				cleared, clearErr = clearSourceWorkflowMetadata(cfg, cityPath, target)
				if clearErr != nil {
					return clearErr
				}
			}
			_, _ = fmt.Fprintf(
				stdout,
				"result=already_clean source_bead_id=%s matched_roots=0 matched_beads=0 closed=0 deleted=0 metadata_cleared=%t\n",
				sourceBeadID,
				cleared,
			)
			resultCode = 0
			return nil
		}
		if !apply {
			_, _ = fmt.Fprintf(
				stdout,
				"result=preview source_bead_id=%s matched_roots=%d matched_beads=%d open_beads=%d\n",
				sourceBeadID,
				totalRoots,
				totalBeads,
				openCount,
			)
			for _, match := range matches {
				_, _ = fmt.Fprintf(stdout, "store=%s roots=%s beads=%d\n", match.label, strings.Join(rootIDs(match.roots), ","), len(match.beads))
			}
			resultCode = 0
			return nil
		}

		closed := 0
		deleted := 0
		incomplete := false
		// Must attempt every match before verifying — do not short-circuit on the
		// first fault. Unlike cmdWorkflowDelete (abort-on-first-error, which needs
		// binding-first sweepOrder), this arm tolerates all matches, then gates the
		// metadata clear on completeness. An early break/return would clear
		// workflow_id while a live tree is still open, orphaning the source; if that
		// ever becomes necessary, impose binding-first ordering here too.
		for _, match := range matches {
			matchClosed, matchDeleted, matchIncomplete := applySourceWorkflowMatchCleanup(match, deleteBeads, stderr)
			closed += matchClosed
			deleted += matchDeleted
			if matchIncomplete {
				incomplete = true
			}
		}

		stillOpen, verifyErr := countOpenMatchedBeads(matches)
		if verifyErr != nil {
			return verifyErr
		}
		if stillOpen > 0 {
			incomplete = true
		}
		cleared := false
		if !incomplete {
			var clearErr error
			cleared, clearErr = clearSourceWorkflowMetadata(cfg, cityPath, target)
			if clearErr != nil {
				return clearErr
			}
		}
		if incomplete {
			_, _ = fmt.Fprintf(
				stdout,
				"result=incomplete source_bead_id=%s matched_roots=%d matched_beads=%d closed=%d deleted=%d metadata_cleared=false still_open=%d\n",
				sourceBeadID,
				totalRoots,
				totalBeads,
				closed,
				deleted,
				stillOpen,
			)
			resultCode = 1
			return nil
		}
		_, _ = fmt.Fprintf(
			stdout,
			"result=cleaned source_bead_id=%s matched_roots=%d matched_beads=%d closed=%d deleted=%d metadata_cleared=%t\n",
			sourceBeadID,
			totalRoots,
			totalBeads,
			closed,
			deleted,
			cleared,
		)
		resultCode = 0
		return nil
	})
	if runErr != nil {
		_, _ = fmt.Fprintf(stderr, "gc workflow delete-source: %v\n", runErr)
		return 1
	}
	return resultCode
}

func cmdWorkflowReopenSource(sourceBeadID string, selector sourceWorkflowStoreSelector, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gc workflow reopen-source: %v\n", err)
		return 1
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gc workflow reopen-source: %v\n", err)
		return 1
	}

	resultCode := 0
	target, err := resolveSourceWorkflowTarget(cfg, cityPath, sourceBeadID, selector, true)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gc workflow reopen-source: %v\n", err)
		return 1
	}
	if target.storeView.store == nil || strings.TrimSpace(target.sourceBead.ID) == "" {
		_, _ = fmt.Fprintf(stderr, "gc workflow reopen-source: getting bead %q: %v\n", sourceBeadID, beads.ErrNotFound)
		return 1
	}
	ctx, cancel := sourceWorkflowCommandContext()
	defer cancel()
	// The scope, not the view — see cmdWorkflowDeleteSource. Reopen and delete
	// have to key on the same thing or they do not exclude each other.
	runErr := sourceworkflow.WithLock(ctx, cityPath, target.storeView.scopePath(cityPath), sourceBeadID, func() error {
		target, err := resolveSourceWorkflowTarget(cfg, cityPath, sourceBeadID, selector, true)
		if err != nil {
			return err
		}
		if target.storeView.store == nil || strings.TrimSpace(target.sourceBead.ID) == "" {
			return fmt.Errorf("getting bead %q: %w", sourceBeadID, beads.ErrNotFound)
		}
		matches, skips, err := collectSourceWorkflowMatches(cfg, cityPath, sourceBeadID, target.storeRef)
		if err != nil {
			return err
		}
		if len(skips) > 0 {
			// reopen-source risks re-slinging a bead whose true blocking
			// root sits in a store we couldn't scan. Surface the skipped
			// stores so operators know coverage was degraded.
			fmt.Fprintln(stderr, "warning:", formatSourceWorkflowStoreSkips(skips)) //nolint:errcheck
		}
		totalRoots, _, _ := summarizeSourceWorkflowMatches(matches)
		if totalRoots > 0 {
			ids := make([]string, 0, totalRoots)
			for _, match := range matches {
				ids = append(ids, rootIDs(match.roots)...)
			}
			_, _ = fmt.Fprintf(
				stderr,
				"result=conflict source_bead_id=%s blocking_workflow_ids=%s\n",
				sourceBeadID,
				strings.Join(ids, ","),
			)
			resultCode = 3
			return nil
		}
		currentSource, err := target.storeView.store.Get(target.sourceBead.ID)
		if err != nil {
			return err
		}
		open := "open"
		unassigned := ""
		if err := target.storeView.store.SetMetadata(currentSource.ID, "workflow_id", ""); err != nil {
			return err
		}
		// Pre-route so the bead is never left unrouted between the reopen and
		// the caller's follow-up re-sling (vp-nq8 / FR-C0.1). A blank
		// gc.routed_to is invisible to route-reclaim (which only heals
		// set-but-dead/stuck routes) and causes unrouted-feeder to mis-route to
		// the rig planner instead of the correct next step, so an unset route
		// orphans the bead if the re-sling fails to land.
		//
		// gc.run_target wins when present. Otherwise keep the route the bead
		// already carries instead of blanking it (ga-20zd). Re-pooling a bead
		// takes two separate commands — the caller writes the route with
		// `gc bd update`, and calls reopen-source — and blanking made that pair
		// order-dependent: a reopen landing after the route write silently
		// erased it. The bead then looked correctly re-pooled (rejection
		// metadata set, branch intact) while being invisible to pool-demand
		// dispatch, which filters on gc.routed_to. Nothing healed it either:
		// restoreCarriedWorkRoutes can only recover a route from
		// gc.run_target, which plain work beads never carry, so the bead sat
		// until a human re-slung it by hand.
		//
		// Preserving costs the caller nothing. A re-sling to a different target
		// overwrites the route, and one to the same target still re-runs
		// finalize via resolveConvoyRecovery, which sees the just-deleted
		// workflow rather than short-circuiting as idempotent.
		nextRoute := strings.TrimSpace(currentSource.Metadata[beadmeta.RunTargetMetadataKey])
		if nextRoute == "" {
			nextRoute = strings.TrimSpace(currentSource.Metadata[beadmeta.RoutedToMetadataKey])
		}
		if err := target.storeView.store.SetMetadata(currentSource.ID, beadmeta.RoutedToMetadataKey, nextRoute); err != nil {
			return err
		}
		if err := clearSessionAffinityMetadataOnBead(target.storeView.store, currentSource.ID); err != nil {
			return err
		}
		if err := target.storeView.store.Update(currentSource.ID, beads.UpdateOpts{
			Status:   &open,
			Assignee: &unassigned,
		}); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "result=reopened source_bead_id=%s\n", sourceBeadID)
		return nil
	})
	if runErr != nil {
		_, _ = fmt.Fprintf(stderr, "gc workflow reopen-source: %v\n", runErr)
		return 1
	}
	return resultCode
}

// findWorkflowBeads returns all beads belonging to a workflow resolved by
// either root bead ID or logical gc.workflow_id, plus descendants keyed by the
// resolved root bead IDs.
func workflowDeleteStoreLabel(cfg *config.City, cityPath, scopePath string) string {
	if scopePath == cityPath {
		return "city"
	}
	if cfg != nil {
		for _, rig := range cfg.Rigs {
			if strings.TrimSpace(rig.Path) == "" {
				continue
			}
			if resolveStoreScopeRoot(cityPath, rig.Path) == scopePath {
				return "rig:" + rig.Name
			}
		}
	}
	return scopePath
}

func deleteWorkflowBeads(store beads.Store, ids []string) (int, []error) {
	deleted := 0
	var errs []error
	for _, id := range ids {
		if err := deleteWorkflowBead(store, id); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", id, err))
			continue
		}
		deleted++
	}
	return deleted, errs
}

// deleteWorkflowBeadsBatch removes exactly the given ids using the store's
// batched delete when the backend implements beads.BatchDeleter (one
// `bd delete … --force` per chunk, which orphans external dependents and lets
// the schema's ON DELETE CASCADE drop the deleted beads' own edge rows), and
// otherwise deletes each bead individually. On the sqlite/Dolt graph store this
// collapses an O(subprocess-per-edge) closure teardown into O(chunks), which
// keeps a large wisp-GC purge from blocking the controller tick for minutes.
// It is not dependent-recursive: beads outside the collected closure that
// depend on a deleted bead are preserved.
func deleteWorkflowBeadsBatch(store beads.Store, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if cd, ok := store.(beads.BatchDeleter); ok {
		// A policy/capability wrapper advertises BatchDeleter to forward it, but
		// reports ErrBatchDeleteUnsupported when its own backing lacks the
		// capability; treat that as "not batchable" and fall through to the
		// per-bead path rather than surfacing it as a delete failure.
		if err := cd.DeleteBatch(ids); !errors.Is(err, beads.ErrBatchDeleteUnsupported) {
			return err
		}
	}
	for _, id := range ids {
		if err := deleteWorkflowBead(store, id); err != nil {
			return err
		}
	}
	return nil
}

func deleteWorkflowBead(store beads.Store, id string) error {
	downDeps, err := store.DepList(id, "down")
	if err != nil {
		return fmt.Errorf("list down deps: %w", err)
	}
	upDeps, err := store.DepList(id, "up")
	if err != nil {
		return fmt.Errorf("list up deps: %w", err)
	}
	removedDown := make([]beads.Dep, 0, len(downDeps))
	for _, dep := range downDeps {
		if err := store.DepRemove(id, dep.DependsOnID); err != nil {
			return withWorkflowDeleteRestoreError(
				fmt.Errorf("remove down dep %s -> %s: %w", id, dep.DependsOnID, err),
				restoreWorkflowDeleteDeps(store, removedDown, nil),
			)
		}
		removedDown = append(removedDown, dep)
	}
	removedUp := make([]beads.Dep, 0, len(upDeps))
	for _, dep := range upDeps {
		if err := store.DepRemove(dep.IssueID, id); err != nil {
			return withWorkflowDeleteRestoreError(
				fmt.Errorf("remove up dep %s -> %s: %w", dep.IssueID, id, err),
				restoreWorkflowDeleteDeps(store, removedDown, removedUp),
			)
		}
		removedUp = append(removedUp, dep)
	}
	if err := store.Delete(id); err != nil {
		return withWorkflowDeleteRestoreError(
			fmt.Errorf("delete bead: %w", err),
			restoreWorkflowDeleteDeps(store, removedDown, removedUp),
		)
	}
	return nil
}

func withWorkflowDeleteRestoreError(primary, restoreErr error) error {
	if restoreErr == nil {
		return primary
	}
	return errors.Join(primary, fmt.Errorf("rollback failed: %w", restoreErr))
}

func restoreWorkflowDeleteDeps(store beads.Store, downDeps, upDeps []beads.Dep) error {
	var restoreErr error
	for _, dep := range downDeps {
		if err := store.DepAdd(dep.IssueID, dep.DependsOnID, dep.Type); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restore dep %s -> %s: %w", dep.IssueID, dep.DependsOnID, err))
		}
	}
	for _, dep := range upDeps {
		if err := store.DepAdd(dep.IssueID, dep.DependsOnID, dep.Type); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restore dep %s -> %s: %w", dep.IssueID, dep.DependsOnID, err))
		}
	}
	return restoreErr
}

func collectSourceWorkflowMatches(cfg *config.City, cityPath, sourceBeadID, sourceStoreRef string) ([]sourceWorkflowStoreMatch, []sourceWorkflowStoreSkip, error) {
	stores, skips, err := openSourceWorkflowStores(cfg, cityPath, sourceBeadID)
	if err != nil {
		return nil, skips, err
	}
	// A relocated class binding is not one of the directories the scan
	// enumerated, and on a converged city it is where the live workflow graph
	// actually is. Sweeping without it would delete the frozen retained copy and
	// leave the running one.
	stores, err = federateSweepViews(cityPath, stores)
	if err != nil {
		return nil, skips, err
	}
	return collectSourceWorkflowMatchesFromStores(cfg, cityPath, sourceBeadID, sourceStoreRef, stores, skips)
}

func collectSourceWorkflowMatchesFromStores(cfg *config.City, cityPath, sourceBeadID, sourceStoreRef string, stores []convoyStoreView, skips []sourceWorkflowStoreSkip) ([]sourceWorkflowStoreMatch, []sourceWorkflowStoreSkip, error) {
	cityName := loadedCityName(cfg, cityPath)
	if err := ensureSelectedSourceStorePresent(cfg, cityPath, cityName, sourceStoreRef, stores, skips); err != nil {
		return nil, skips, err
	}
	c := &sourceWorkflowMatchCollector{
		cfg:            cfg,
		cityPath:       cityPath,
		cityName:       cityName,
		stores:         stores,
		skips:          skips,
		matchesByLabel: map[string]sourceWorkflowStoreMatch{},
		visited:        map[string]struct{}{},
		failedStores:   map[int]struct{}{},
	}
	if err := c.collect(sourceBeadID, sourceStoreRef); err != nil {
		return nil, c.skips, err
	}
	if !c.anyStoreScanned {
		if c.firstScanErr != nil {
			return nil, c.skips, c.firstScanErr
		}
		return nil, c.skips, fmt.Errorf("no source workflow stores were available to scan")
	}
	return c.matches(), c.skips, nil
}

// ensureSelectedSourceStorePresent fails when a specific source store was
// selected but is absent from the opened stores. The selected store is always
// strict, so its absence — or a recorded open failure for it — must abort the
// walk rather than silently degrade singleton coverage.
func ensureSelectedSourceStorePresent(cfg *config.City, cityPath, cityName, sourceStoreRef string, stores []convoyStoreView, skips []sourceWorkflowStoreSkip) error {
	selectedRef := sourceworkflow.NormalizeSourceStoreRef(sourceStoreRef)
	if selectedRef == "" {
		return nil
	}
	present := slices.ContainsFunc(stores, func(info convoyStoreView) bool {
		return info.store != nil &&
			sourceworkflow.NormalizeSourceStoreRef(workflowStoreRefForDir(info.scopePath(cityPath), cityPath, cityName, cfg)) == selectedRef
	})
	if present {
		return nil
	}
	for _, skip := range skips {
		skipRef := sourceworkflow.NormalizeSourceStoreRef(workflowStoreRefForDir(skip.path, cityPath, cityName, cfg))
		if skipRef == selectedRef && skip.err != nil {
			return fmt.Errorf("selected source workflow store %s is unavailable to scan: %w", selectedRef, skip.err)
		}
	}
	return fmt.Errorf("selected source workflow store %s is unavailable to scan", selectedRef)
}

// sourceWorkflowMatchCollector walks the source-workflow graph across every
// candidate store for a delete/reopen-source operation. It tolerates unrelated
// (non-selected) store scan failures — recording them as skips — while keeping
// the selected source store strict, and carries the shared walk state so each
// step reads as a small, single-purpose method.
type sourceWorkflowMatchCollector struct {
	cfg      *config.City
	cityPath string
	cityName string
	stores   []convoyStoreView

	matchesByLabel  map[string]sourceWorkflowStoreMatch
	visited         map[string]struct{}
	failedStores    map[int]struct{}
	skips           []sourceWorkflowStoreSkip
	anyStoreScanned bool
	firstScanErr    error
}

// collect walks every store for currentSourceID, then recurses into each child
// source discovered under it. A tolerated per-store scan failure yields no
// children and no error so the walk continues; a selected-store failure aborts.
func (c *sourceWorkflowMatchCollector) collect(currentSourceID, currentSourceStoreRef string) error {
	currentSourceID = strings.TrimSpace(currentSourceID)
	if currentSourceID == "" {
		return nil
	}
	for i, info := range c.stores {
		children, err := c.scanStore(i, info, currentSourceID, currentSourceStoreRef)
		if err != nil {
			return err
		}
		rootStoreRef := workflowStoreRefForDir(info.scopePath(c.cityPath), c.cityPath, c.cityName, c.cfg)
		for _, child := range children {
			if err := c.collect(child.ID, rootStoreRef); err != nil {
				return err
			}
		}
	}
	return nil
}

// scanStore walks a single candidate store for one source identity and returns
// the child source beads to recurse into. Nil, already-failed, or
// already-visited stores yield no children. A ListLiveRoots, bead, or child
// scan failure is classified by recordScanFailure: a tolerated failure returns
// (nil, nil) so the caller skips the store; a selected-store failure returns the
// wrapped error to abort.
func (c *sourceWorkflowMatchCollector) scanStore(index int, info convoyStoreView, currentSourceID, currentSourceStoreRef string) ([]beads.Bead, error) {
	if info.store == nil {
		return nil, nil
	}
	if _, failed := c.failedStores[index]; failed {
		return nil, nil
	}
	rootStoreRef := workflowStoreRefForDir(info.scopePath(c.cityPath), c.cityPath, c.cityName, c.cfg)
	// Downward delete-source walks key by the store actually being read plus the
	// source identity. The upward finalize walk in internal/dispatch only needs
	// source store plus bead ID because each hop has one parent.
	//
	// The key is the VIEW, not its store ref: a converged city's binding and its
	// work ledger share one scope and therefore one ref, so keying on the ref
	// would mark the pair visited after the first and leave the second unswept.
	visitKey := info.path + "\x00" + currentSourceStoreRef + "\x00" + currentSourceID
	if _, ok := c.visited[visitKey]; ok {
		return nil, nil
	}
	c.visited[visitKey] = struct{}{}

	roots, err := sourceworkflow.ListLiveRoots(info.store, currentSourceID, currentSourceStoreRef, rootStoreRef)
	if err != nil {
		return nil, c.recordScanFailure(index, info, currentSourceStoreRef, "listing live source workflows", err)
	}
	if err := c.mergeRootMatches(info, roots); err != nil {
		return nil, c.recordScanFailure(index, info, currentSourceStoreRef, "listing source workflow beads", err)
	}
	children, err := sourceWorkflowChildSources(info.store, currentSourceID, currentSourceStoreRef, rootStoreRef)
	if err != nil {
		return nil, c.recordScanFailure(index, info, currentSourceStoreRef, "listing source workflow children", err)
	}
	c.anyStoreScanned = true
	return children, nil
}

// mergeRootMatches gathers every workflow bead under the given roots in one
// store and merges them into the match set. It returns the first bead-scan
// error (leaving the match set unmerged) so the caller can classify it as a
// tolerated or strict scan failure.
func (c *sourceWorkflowMatchCollector) mergeRootMatches(info convoyStoreView, roots []beads.Bead) error {
	if len(roots) == 0 {
		return nil
	}
	beadSet := make([]beads.Bead, 0, len(roots))
	for _, root := range roots {
		workflowBeads, err := findWorkflowBeadsFromRoot(info.store, root)
		if err != nil {
			return err
		}
		beadSet = append(beadSet, workflowBeads...)
	}
	mergeSourceWorkflowMatch(c.matchesByLabel, sourceWorkflowStoreMatch{
		label:     workflowDeleteStoreLabelForView(c.cfg, c.cityPath, info),
		store:     info.store,
		roots:     roots,
		beads:     uniqueBeads(beadSet),
		path:      info.path,
		scopePath: info.scopePath(c.cityPath),
	})
	return nil
}

// recordScanFailure records a store whose scan failed: it remembers the first
// error, marks the store failed and skipped, and returns the wrapped error when
// the walk must abort rather than tolerate the failure.
//
// Tolerance exists so one sick rig cannot take down a walk that spans several,
// and it is bounded by two stores that are never optional. The SELECTED source
// store is the one the caller named. The class BINDING is the city's own store
// after relocation, and the selected-store test cannot speak for it: a binding's
// rows carry the CITY's ref, so a leg carrying a rig's ref — or carrying none,
// which is what an orphaned source bead produces — puts the binding outside the
// strict set on exactly the sweeps that still have to reach it. Tolerating it
// there closes the frozen twins the migration retained, leaves the live tree
// running, and prints result=cleaned.
func (c *sourceWorkflowMatchCollector) recordScanFailure(index int, info convoyStoreView, currentSourceStoreRef, operation string, scanErr error) error {
	label := workflowDeleteStoreLabelForView(c.cfg, c.cityPath, info)
	wrapped := fmt.Errorf("%s in %s: %w", operation, label, scanErr)
	if c.firstScanErr == nil {
		c.firstScanErr = wrapped
	}
	c.failedStores[index] = struct{}{}
	c.skips = append(c.skips, sourceWorkflowStoreSkip{path: info.path, err: wrapped})

	if info.isClassBinding() {
		return refusePartialSweep(operation+" in", label, scanErr)
	}
	rootStoreRef := workflowStoreRefForDir(info.scopePath(c.cityPath), c.cityPath, c.cityName, c.cfg)
	selectedStore := strings.TrimSpace(currentSourceStoreRef) != "" &&
		sourceworkflow.NormalizeSourceStoreRef(rootStoreRef) == sourceworkflow.NormalizeSourceStoreRef(currentSourceStoreRef)
	if selectedStore {
		return wrapped
	}
	return nil
}

// matches finalizes the deduplicated per-store match set.
func (c *sourceWorkflowMatchCollector) matches() []sourceWorkflowStoreMatch {
	matches := make([]sourceWorkflowStoreMatch, 0, len(c.matchesByLabel))
	for _, match := range c.matchesByLabel {
		match.roots = uniqueBeads(match.roots)
		match.beads = uniqueBeads(match.beads)
		matches = append(matches, match)
	}
	return matches
}

func mergeSourceWorkflowMatch(matches map[string]sourceWorkflowStoreMatch, next sourceWorkflowStoreMatch) {
	if next.label == "" {
		return
	}
	current := matches[next.label]
	if current.label == "" {
		matches[next.label] = next
		return
	}
	current.roots = append(current.roots, next.roots...)
	current.beads = append(current.beads, next.beads...)
	matches[next.label] = current
}

func sourceWorkflowChildSources(store beads.Store, sourceBeadID, sourceStoreRef, rootStoreRef string) ([]beads.Bead, error) {
	sourceBeadID = strings.TrimSpace(sourceBeadID)
	if store == nil || sourceBeadID == "" {
		return nil, nil
	}
	candidates, err := store.List(beads.ListQuery{
		IncludeClosed: true,
		Metadata: map[string]string{
			beadmeta.SourceBeadIDMetadataKey: sourceBeadID,
		},
	})
	if err != nil {
		return nil, err
	}
	children := make([]beads.Bead, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.ID == "" || sourceworkflow.IsWorkflowRoot(candidate) {
			continue
		}
		if !sourceworkflow.WorkflowMatchesSource(candidate, sourceBeadID, sourceStoreRef, rootStoreRef) {
			continue
		}
		children = append(children, candidate)
	}
	return children, nil
}

func sourceWorkflowMatchLabels(matches []sourceWorkflowStoreMatch) []string {
	labels := make([]string, 0, len(matches))
	for _, match := range matches {
		labels = append(labels, match.label)
	}
	return labels
}

func summarizeSourceWorkflowMatches(matches []sourceWorkflowStoreMatch) (roots, beadsTotal, openCount int) {
	for _, match := range matches {
		roots += len(match.roots)
		beadsTotal += len(match.beads)
		for _, bead := range match.beads {
			if bead.Status != "closed" {
				openCount++
			}
		}
	}
	return roots, beadsTotal, openCount
}

func countOpenMatchedBeads(matches []sourceWorkflowStoreMatch) (int, error) {
	open := 0
	for _, match := range matches {
		for _, bead := range match.beads {
			current, err := match.store.Get(bead.ID)
			if err != nil {
				if errors.Is(err, beads.ErrNotFound) {
					continue
				}
				return 0, refusePartialSweep("re-reading beads in", match.label, err)
			}
			if current.Status != "closed" {
				open++
			}
		}
	}
	return open, nil
}

// sourceWorkflowStoreSkip records a candidate store that could not be opened
// or queried during a source-workflow singleton scan. Tolerating unavailable stores
// avoids turning a rig-local problem into a city-wide outage, but the
// silent skip creates a correctness hole: a cross-store live root living
// in the broken rig is invisible to the singleton check. Callers MUST
// surface skips (stderr, SlingResult.MetadataErrors, etc.) so operators
// can see when singleton coverage has degraded and decide whether to
// proceed or repair the rig first.
type sourceWorkflowStoreSkip struct {
	path string
	err  error
}

// formatSourceWorkflowStoreSkips renders skipped stores as a single
// human-readable warning line suitable for stderr or MetadataErrors.
func formatSourceWorkflowStoreSkips(skips []sourceWorkflowStoreSkip) string {
	if len(skips) == 0 {
		return ""
	}
	parts := make([]string, 0, len(skips))
	for _, skip := range skips {
		parts = append(parts, fmt.Sprintf("%s (%v)", skip.path, skip.err))
	}
	return fmt.Sprintf(
		"source-workflow singleton scan skipped %d unavailable store(s); cross-store roots in those stores are invisible: %s",
		len(skips),
		strings.Join(parts, "; "),
	)
}

func unscannedSourceWorkflowStoreSkips(cfg *config.City, cityPath, selectedStoreRef string, skips []sourceWorkflowStoreSkip) ([]sourceWorkflowStoreSkip, bool) {
	selectedStoreRef = sourceworkflow.NormalizeSourceStoreRef(selectedStoreRef)
	if selectedStoreRef == "" || len(skips) == 0 {
		return skips, false
	}
	cityName := loadedCityName(cfg, cityPath)
	unscanned := make([]sourceWorkflowStoreSkip, 0, len(skips))
	selectedRecovered := false
	for _, skip := range skips {
		skipRef := sourceworkflow.NormalizeSourceStoreRef(workflowStoreRefForDir(skip.path, cityPath, cityName, cfg))
		if skipRef == selectedStoreRef {
			selectedRecovered = true
			continue
		}
		unscanned = append(unscanned, skip)
	}
	return unscanned, selectedRecovered
}

// openSourceWorkflowStores opens every candidate bead store used for
// source-workflow singleton checks. It tolerates broken non-selected stores
// the same way openConvoyStores does: a failure to open one rig's store must
// not block launches or recovery city-wide. Only when *every* candidate is
// unopenable do we surface the first error, because at that point the
// singleton check has no stores to scan and we cannot proceed safely. Stores
// explicitly selected via --rig / --store-ref still go through
// openSourceWorkflowStoreRef, which is strict on purpose.
//
// The second return value lists the stores that were skipped — callers are
// expected to surface these (see formatSourceWorkflowStoreSkips) so operators
// can see when singleton coverage degraded.
func openSourceWorkflowStores(cfg *config.City, cityPath, beadID string) ([]convoyStoreView, []sourceWorkflowStoreSkip, error) {
	return openSourceWorkflowStoresWith(cfg, cityPath, beadID, func(dir string) (beads.Store, error) {
		return openStoreAtForCity(dir, cityPath)
	})
}

// openSourceWorkflowStoresWith is the testable core of openSourceWorkflowStores.
// It takes the store-opening callback explicitly so tests can inject broken
// rig stores without touching the filesystem.
func openSourceWorkflowStoresWith(cfg *config.City, cityPath, beadID string, openStore func(string) (beads.Store, error)) ([]convoyStoreView, []sourceWorkflowStoreSkip, error) {
	return openSourceWorkflowStoresWithProvider(cfg, cityPath, beadID, func(scopeRoot string) string {
		return rawBeadsProviderForScope(scopeRoot, cityPath)
	}, openStore)
}

func openSourceWorkflowStoresWithProvider(cfg *config.City, cityPath, beadID string, providerForScope func(string) string, openStore func(string) (beads.Store, error)) ([]convoyStoreView, []sourceWorkflowStoreSkip, error) {
	candidates := convoyStoreCandidatesWithProvider(cfg, cityPath, beadID, providerForScope)
	var (
		stores   = make([]convoyStoreView, 0, len(candidates))
		skips    []sourceWorkflowStoreSkip
		firstErr error
	)
	for _, dir := range candidates {
		store, err := openStore(dir)
		if err != nil {
			wrapped := fmt.Errorf("opening source workflow store %s: %w", dir, err)
			skips = append(skips, sourceWorkflowStoreSkip{path: dir, err: err})
			if firstErr == nil {
				firstErr = wrapped
			}
			continue
		}
		stores = append(stores, convoyStoreView{path: dir, store: store})
	}
	if len(stores) > 0 {
		return stores, skips, nil
	}
	if firstErr != nil {
		return nil, skips, firstErr
	}
	return nil, skips, fmt.Errorf("no source workflow stores available")
}

func clearSourceWorkflowMetadata(cfg *config.City, cityPath string, target resolvedSourceWorkflowTarget) (bool, error) {
	bead := target.sourceBead
	storeView := target.storeView
	if storeView.store == nil || strings.TrimSpace(storeView.path) == "" {
		if target.storeRef == "" {
			return false, nil
		}
		var err error
		storeView, _, err = openSourceWorkflowStoreRef(cfg, cityPath, target.storeRef)
		if err != nil {
			return false, err
		}
	}
	if strings.TrimSpace(bead.ID) == "" {
		current, err := storeView.store.Get(target.sourceBeadID)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				return false, nil
			}
			return false, err
		}
		bead = current
	}
	if strings.TrimSpace(bead.Metadata["workflow_id"]) == "" {
		return false, nil
	}
	if err := storeView.store.SetMetadata(bead.ID, "workflow_id", ""); err != nil {
		return false, err
	}
	return true, nil
}

func rootIDs(roots []beads.Bead) []string {
	ids := make([]string, 0, len(roots))
	for _, root := range roots {
		if root.ID == "" {
			continue
		}
		ids = append(ids, root.ID)
	}
	return ids
}

func uniqueBeads(bb []beads.Bead) []beads.Bead {
	out := make([]beads.Bead, 0, len(bb))
	seen := make(map[string]struct{}, len(bb))
	for _, bead := range bb {
		if bead.ID == "" {
			continue
		}
		if _, ok := seen[bead.ID]; ok {
			continue
		}
		seen[bead.ID] = struct{}{}
		out = append(out, bead)
	}
	return out
}

// findWorkflowBeads returns every bead of a workflow that this store holds.
//
// A read that FAILS is returned, never folded into the empty result. The
// difference is invisible in the return value alone — a store that lost its
// connection and a store that holds none of this workflow both contribute no
// beads — and a sweep planned off the second reading of the first is the
// partial sweep that reports success. A not-found root is genuinely absent and
// stays a skip.
func findWorkflowBeads(store beads.Store, workflowID string) ([]beads.Bead, error) {
	result := make([]beads.Bead, 0, 4)
	seen := make(map[string]struct{}, 4)
	rootIDs := make([]string, 0, 2)
	rootSeen := make(map[string]struct{}, 2)
	addBead := func(b beads.Bead) {
		if b.ID == "" {
			return
		}
		if _, ok := seen[b.ID]; ok {
			return
		}
		seen[b.ID] = struct{}{}
		result = append(result, b)
	}
	addRoot := func(root beads.Bead) {
		resolvedWorkflowID := strings.TrimSpace(root.Metadata[beadmeta.WorkflowIDMetadataKey])
		// Match sourceworkflow.IsWorkflowRoot so graph.v2-only roots (marked
		// via gc.formula_contract=graph.v2 without gc.kind=workflow) are
		// collected here. Without this, delete-source lists the root but
		// fails to close its descendants — a hole in the singleton recovery
		// flow that this PR is trying to enforce.
		if !sourceworkflow.IsWorkflowRoot(root) {
			return
		}
		if root.ID != workflowID && resolvedWorkflowID != workflowID {
			return
		}
		if _, ok := rootSeen[root.ID]; ok {
			return
		}
		rootSeen[root.ID] = struct{}{}
		rootIDs = append(rootIDs, root.ID)
		addBead(root)
	}
	switch root, err := store.Get(workflowID); {
	case err == nil:
		addRoot(root)
	case !errors.Is(err, beads.ErrNotFound):
		return nil, fmt.Errorf("getting workflow root %s: %w", workflowID, err)
	}
	// Query on gc.workflow_id only; the predicate is applied in-memory via
	// addRoot so we pick up graph.v2-only roots alongside legacy roots.
	roots, err := store.List(beads.ListQuery{
		Metadata: map[string]string{
			beadmeta.WorkflowIDMetadataKey: workflowID,
		},
		IncludeClosed: true,
	})
	if err != nil {
		return nil, fmt.Errorf("listing roots of workflow %s: %w", workflowID, err)
	}
	for _, root := range roots {
		addRoot(root)
	}
	for _, rootID := range rootIDs {
		all, err := store.List(beads.ListQuery{
			Metadata:      map[string]string{beadmeta.RootBeadIDMetadataKey: rootID},
			IncludeClosed: true,
		})
		if err != nil {
			return nil, fmt.Errorf("listing descendants of workflow %s: %w", rootID, err)
		}
		for _, b := range all {
			addBead(b)
		}
	}
	return result, nil
}

func findWorkflowBeadsFromRoot(store beads.Store, root beads.Bead) ([]beads.Bead, error) {
	if store == nil || root.ID == "" {
		return nil, nil
	}
	descendants, err := store.List(beads.ListQuery{
		Metadata:      map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
		IncludeClosed: true,
	})
	if err != nil {
		return nil, fmt.Errorf("listing descendants of workflow %s: %w", root.ID, err)
	}
	return uniqueBeads(append([]beads.Bead{root}, descendants...)), nil
}

func workflowBeadIDs(bb []beads.Bead) []string {
	ids := make([]string, len(bb))
	for i, b := range bb {
		ids[i] = b.ID
	}
	return ids
}

// assertDrainRootScopeMatchesDispatch fails a drain rooted in a different work
// scope than it is dispatched from: its members would resolve empty, not absent.
func assertDrainRootScopeMatchesDispatch(bead beads.Bead, dispatchStoreRef string) error {
	rootRef := strings.TrimSpace(bead.Metadata[beadmeta.RootStoreRefMetadataKey])
	dispatchStoreRef = strings.TrimSpace(dispatchStoreRef)
	if rootRef == "" || dispatchStoreRef == "" || rootRef == dispatchStoreRef {
		return nil
	}
	return fmt.Errorf("drain %s is rooted in %s but dispatched from %s: its convoy members live in the root's work store, so draining from here would resolve an empty convoy and report success (ga-w2mf3)", bead.ID, rootRef, dispatchStoreRef)
}
