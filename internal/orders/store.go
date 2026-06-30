package orders

import (
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// This file is the order-class front-door skeleton per
// OBJECT-MODEL-FRONT-DOOR-DESIGN sec 3.4 / 6.4.
//
// Unlike session (Info), nudge (Item), and mail (Message), the order object has
// NO pre-existing domain type. OrderRun / RunOutcome / EventCursor are net-new
// and designed here. The dispatcher deliberately exploits bead mechanics, and
// the typed API NAMES them rather than hiding them:
//
//   - CreatedAt on the tracking bead is the COOLDOWN CLOCK (LastRun reads it).
//   - an OPEN tracking bead == an in-flight single-flight marker.
//   - created-then-closed-immediately == cooldown-advance-only.
//   - reads union the two tiers (wisps + issues) — TierBoth.
//
// The Store methods (CreateRun, CreateRunClosed, SetOutcome, SetCursor,
// CloseRun, RecentRuns) emit byte-identical bead writes to the raw ops they
// replace and are wired into order_dispatch.go / cmd_order.go. The cooldown
// clock (last-run) and event-cursor READS the dispatch gate uses go through the
// runtime helpers (LastRunFuncForStore/CursorFuncForStore and their
// *AcrossStores forms), which the in-memory tracking index batches per store —
// see cmd/gc/order_dispatch.go.

// Order-class label constants. These MUST stay in sync with the canonical
// declarations in cmd/gc/order_dispatch.go and the private mirrors in
// internal/coordclass (guarded by the coordclass drift test). They are
// re-declared here only so the codec edge can build label sets without
// importing package main (a layering inversion).
const (
	labelOrderTracking    = "order-tracking"
	labelOrderRunPrefix   = "order-run:"
	labelOrderTitlePrefix = "order:"
	labelSeqPrefix        = "seq:"

	labelExec           = "exec"
	labelExecFailed     = "exec-failed"
	labelExecEnvFailed  = "exec-env-failed"
	labelWisp           = "wisp"
	labelWispFailed     = "wisp-failed"
	labelWispCanceled   = "wisp-canceled"
	labelTriggerEnvFail = "trigger-env-failed"
)

// RunOutcome enumerates the terminal outcome of an order run. Each value maps
// to a fixed label set that the dispatcher stamps on the tracking bead. The
// zero value (RunOutcomeNone) means "no outcome stamped yet" (an open,
// in-flight run).
type RunOutcome int

const (
	// RunOutcomeNone is the zero value: no terminal outcome stamped (in-flight).
	RunOutcomeNone RunOutcome = iota
	// RunOutcomeExec — synchronous trigger executed successfully.
	RunOutcomeExec
	// RunOutcomeExecFailed — synchronous trigger ran but failed.
	RunOutcomeExecFailed
	// RunOutcomeExecEnvFailed — synchronous trigger failed building its env.
	RunOutcomeExecEnvFailed
	// RunOutcomeWisp — wisp dispatch succeeded.
	RunOutcomeWisp
	// RunOutcomeWispFailed — wisp dispatch failed.
	RunOutcomeWispFailed
	// RunOutcomeWispCanceled — wisp dispatch was canceled.
	RunOutcomeWispCanceled
	// RunOutcomeTriggerEnvFailed — pre-dispatch trigger env build failed.
	RunOutcomeTriggerEnvFailed
)

// Labels returns the exact label set the dispatcher stamps for this outcome,
// matching cmd/gc/order_dispatch.go verbatim. RunOutcomeNone returns nil.
func (o RunOutcome) Labels() []string {
	switch o {
	case RunOutcomeExec:
		return []string{labelExec}
	case RunOutcomeExecFailed:
		return []string{labelExecFailed}
	case RunOutcomeExecEnvFailed:
		return []string{labelExecEnvFailed}
	case RunOutcomeWisp:
		return []string{labelWisp}
	case RunOutcomeWispFailed:
		return []string{labelWisp, labelWispFailed}
	case RunOutcomeWispCanceled:
		return []string{labelWisp, labelWispCanceled}
	case RunOutcomeTriggerEnvFailed:
		return []string{labelTriggerEnvFail}
	default:
		return nil
	}
}

// EventCursor is the per-order event-bus cursor, encoded on the tracking bead
// as the label pair ("order:<scoped>", "seq:<N>"). It is the high-water mark of
// events the order has already consumed.
type EventCursor uint64

// OrderRun is the net-new domain type for one order tracking record. It names
// the load-bearing bead mechanics the dispatcher relies on.
type OrderRun struct {
	// ID is the tracking bead id.
	ID string
	// Scoped is the scoped order name ("<rig>/<agent>" style).
	Scoped string
	// Outcome is the terminal outcome, or RunOutcomeNone for an in-flight run.
	Outcome RunOutcome
	// CreatedAt is the COOLDOWN CLOCK: the dispatcher reads the most recent
	// run's CreatedAt to decide whether the cooldown has elapsed.
	CreatedAt time.Time
	// Open reports whether the tracking bead is still open. An open run is the
	// in-flight single-flight marker that suppresses repeat dispatch.
	Open bool
	// Cursor is the decoded EventCursor (max seq across the run's labels).
	Cursor EventCursor
}

// RunOpts configures CreateRun.
type RunOpts struct {
	// Outcome, when non-None, is stamped on the created (open) bead — used by
	// the trigger-env-failed pre-dispatch path which creates an already-labeled
	// open bead so the open-work gate suppresses repeat ticks.
	Outcome RunOutcome
}

// Store is the order-class domain wrapper. It holds the strongly-typed
// beads.OrdersStore by value and confines the Title/label codec.
type Store struct {
	store beads.OrdersStore
}

// NewStore wraps a strongly-typed orders-class store as the order front door.
func NewStore(store beads.OrdersStore) *Store {
	return &Store{store: store}
}

// trackingTitle returns the canonical tracking-bead title for a scoped order.
func trackingTitle(scoped string) string { return labelOrderTitlePrefix + scoped }

// baseLabels returns the order-run + order-tracking labels every tracking bead
// carries, plus any outcome labels.
func baseLabels(scoped string, outcome RunOutcome) []string {
	labels := []string{labelOrderRunPrefix + scoped, labelOrderTracking}
	return append(labels, outcome.Labels()...)
}

// CreateRun creates an OPEN tracking bead for scoped (the in-flight marker
// whose CreatedAt advances the cooldown clock). It is the byte-identical
// replacement for the store.Create(beads.Bead{Title:"order:"+scoped, Labels:
// {order-run, order-tracking[, outcome]}, NoHistory:true}) sites in
// order_dispatch.go.
func (s *Store) CreateRun(scoped string, opts RunOpts) (OrderRun, error) {
	created, err := s.store.Create(beads.Bead{
		Title:     trackingTitle(scoped),
		Labels:    baseLabels(scoped, opts.Outcome),
		NoHistory: true,
	})
	if err != nil {
		return OrderRun{}, fmt.Errorf("creating order run for %q: %w", scoped, err)
	}
	return OrderRun{
		ID:        created.ID,
		Scoped:    scoped,
		Outcome:   opts.Outcome,
		CreatedAt: created.CreatedAt,
		Open:      true,
	}, nil
}

// SetOutcome stamps the outcome label set on an existing tracking bead. It is
// the byte-identical replacement for the store.Update(id, {Labels: outcome})
// sites in order_dispatch.go / cmd_order.go.
func (s *Store) SetOutcome(runID string, outcome RunOutcome) error {
	if err := s.store.Update(runID, beads.UpdateOpts{Labels: outcome.Labels()}); err != nil {
		return fmt.Errorf("setting order run outcome on %q: %w", runID, err)
	}
	return nil
}

// SetCursor stamps the event cursor as the label pair (order:<scoped>,
// seq:<N>) on an existing tracking bead. Replaces the cursor-persist Update
// sites in order_dispatch.go.
func (s *Store) SetCursor(runID, scoped string, cursor EventCursor) error {
	labels := []string{
		labelOrderTitlePrefix + scoped,
		fmt.Sprintf("%s%d", labelSeqPrefix, uint64(cursor)),
	}
	if err := s.store.Update(runID, beads.UpdateOpts{Labels: labels}); err != nil {
		return fmt.Errorf("setting order run cursor on %q: %w", runID, err)
	}
	return nil
}

// CloseRun closes a tracking bead, stamping close_reason so validation.on-close
// cities accept it. Replaces the defer-Close / immediate-close sites in
// cmd_order.go.
func (s *Store) CloseRun(runID, reason string) error {
	if reason != "" {
		if err := s.store.SetMetadata(runID, "close_reason", reason); err != nil {
			return fmt.Errorf("stamping close reason on order run %q: %w", runID, err)
		}
	}
	if err := s.store.Close(runID); err != nil {
		return fmt.Errorf("closing order run %q: %w", runID, err)
	}
	return nil
}

// CreateRunClosed creates a tracking bead, optionally stamps an event cursor and
// outcome, then closes it — the cooldown-advance-only path used by manual
// `gc order run`. The bead's CreatedAt advances the cooldown clock, and it is
// closed immediately so a lingering open bead is not read as in-flight work
// (ga-jra/ga-lo8c). It emits byte-identical bead writes to the prior raw
// Create + (cursor Update) + (outcome Update) + (close_reason SetMetadata) +
// Close sequence in cmd_order.go. The returned OrderRun is closed (Open=false).
func (s *Store) CreateRunClosed(scoped string, outcome RunOutcome, cursor *EventCursor, closeReason string) (OrderRun, error) {
	created, err := s.store.Create(beads.Bead{
		Title:     trackingTitle(scoped),
		Labels:    baseLabels(scoped, RunOutcomeNone),
		NoHistory: true,
	})
	if err != nil {
		return OrderRun{}, fmt.Errorf("creating closed order run for %q: %w", scoped, err)
	}
	run := OrderRun{ID: created.ID, Scoped: scoped, CreatedAt: created.CreatedAt}
	if cursor != nil {
		if err := s.SetCursor(created.ID, scoped, *cursor); err != nil {
			return run, err
		}
		run.Cursor = *cursor
	}
	if outcome != RunOutcomeNone {
		if err := s.SetOutcome(created.ID, outcome); err != nil {
			return run, err
		}
		run.Outcome = outcome
	}
	if err := s.CloseRun(created.ID, closeReason); err != nil {
		return run, err
	}
	return run, nil
}

// RecentRuns lists the tracking/order-run beads for scoped newest-first
// (including closed), decoded into OrderRun values. It is the typed face of the
// `gc order history` read (cmd_order.go): it confines the order-run-label List
// and the bead->OrderRun decode. It reads through the raw store with TierMode
// TierBoth (unioning wisp + issue tiers), byte-identical to the `gc order
// history` loop.
func (s *Store) RecentRuns(scoped string, limit int) ([]OrderRun, error) {
	if s.store.Store == nil {
		return nil, nil
	}
	beadsList, err := s.store.List(beads.ListQuery{
		Label:         labelOrderRunPrefix + scoped,
		Limit:         limit,
		IncludeClosed: true,
		Sort:          beads.SortCreatedDesc,
		TierMode:      beads.TierBoth,
	})
	if err != nil {
		return decodeRuns(scoped, beadsList), err
	}
	return decodeRuns(scoped, beadsList), nil
}

// decodeRun projects an order tracking/run bead onto an OrderRun. It is pure,
// side-effect-free, and backend-invariant (reads only bead fields), matching the
// projection-invariance invariant. The cooldown clock (CreatedAt), open flag,
// outcome (from labels), and event cursor (max seq from labels) are decoded here.
func decodeRun(scoped string, b beads.Bead) OrderRun {
	return OrderRun{
		ID:        b.ID,
		Scoped:    scoped,
		Outcome:   outcomeFromLabels(b.Labels),
		CreatedAt: b.CreatedAt,
		Open:      b.Status != "closed",
		Cursor:    EventCursor(MaxSeqFromLabels([][]string{b.Labels})),
	}
}

func decodeRuns(scoped string, list []beads.Bead) []OrderRun {
	out := make([]OrderRun, 0, len(list))
	for _, b := range list {
		out = append(out, decodeRun(scoped, b))
	}
	return out
}

// outcomeFromLabels reverses RunOutcome.Labels, reporting the terminal outcome a
// tracking bead's labels encode, or RunOutcomeNone for an in-flight run.
func outcomeFromLabels(labels []string) RunOutcome {
	wisp := beadLabelsContain(labels, labelWisp)
	switch {
	case beadLabelsContain(labels, labelWispCanceled):
		return RunOutcomeWispCanceled
	case beadLabelsContain(labels, labelWispFailed):
		return RunOutcomeWispFailed
	case wisp:
		return RunOutcomeWisp
	case beadLabelsContain(labels, labelExecEnvFailed):
		return RunOutcomeExecEnvFailed
	case beadLabelsContain(labels, labelExecFailed):
		return RunOutcomeExecFailed
	case beadLabelsContain(labels, labelExec):
		return RunOutcomeExec
	case beadLabelsContain(labels, labelTriggerEnvFail):
		return RunOutcomeTriggerEnvFailed
	default:
		return RunOutcomeNone
	}
}

func beadLabelsContain(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}
