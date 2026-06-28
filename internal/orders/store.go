package orders

import (
	"fmt"
	"sort"
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
// PHASE 0 STATUS: the types + Store wrapper + method SIGNATURES are the
// contract; the trivial/exact methods (CreateRun, SetOutcome, SetCursor,
// CloseRun, LastRun, EventCursor) are implemented to emit byte-identical bead
// writes to the raw ops they replace. Production call sites in order_dispatch.go
// / cmd_order.go are routed in Phase 3.

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

// orderTrackingHistoryIndexLimit caps the recent-history list the dispatcher's
// tracking index reads. Mirrors cmd/gc/order_dispatch.go.
const orderTrackingHistoryIndexLimit = 2048

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
// `gc order history` read: it confines the order-run-label List and the
// bead->OrderRun decode. It reads through the raw store with TierMode TierBoth
// (unioning wisp + issue tiers), byte-identical to the `gc order history` loop
// and the LastRun/EventCursor runtime helpers — NOT through Live.List, which the
// open-work gate (OpenTracking) uses instead.
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

// OpenTracking returns the OPEN tracking beads for scoped, decoded into
// OrderRun values. An open tracking bead is the single-flight in-flight marker.
// It is the typed face of listCanonicalOpenOrderTrackingBeads filtered to the
// scoped order's order-run label.
func (s *Store) OpenTracking(scoped string) ([]OrderRun, error) {
	if s.store.Store == nil {
		return nil, nil
	}
	beadsList, err := beads.HandlesFor(s.store.Store).Live.List(beads.ListQuery{
		Label:    labelOrderRunPrefix + scoped,
		Status:   "open",
		Sort:     beads.SortCreatedDesc,
		TierMode: beads.TierBoth,
	})
	if err != nil {
		return nil, err
	}
	out := make([]OrderRun, 0, len(beadsList))
	for _, b := range beadsList {
		if !beadLabelsContain(b.Labels, labelOrderTracking) {
			continue
		}
		out = append(out, decodeRun(scoped, b))
	}
	return out, nil
}

// HasOpenWork reports whether any open tracking bead exists for scoped — the
// single-flight gate that suppresses repeat dispatch while a run is in flight.
// It only counts order-tracking beads (not wisp roots); the dispatcher's
// wisp-aware open-work gate (hasOpenWorkStrict) layers descendant checks on top
// and stays in cmd/gc.
func (s *Store) HasOpenWork(scoped string) (bool, error) {
	open, err := s.OpenTracking(scoped)
	if err != nil {
		return false, err
	}
	return len(open) > 0, nil
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

// RecentRunsAcrossStores unions RecentRuns across a set of order stores,
// returning the merged runs newest-first. It is the federated face of the
// multi-scope `gc order history` read (city + per-rig). Each store is queried
// independently; a partial failure returns the runs collected so far with the
// error so the caller can degrade gracefully like the raw loop did.
func RecentRunsAcrossStores(scoped string, limit int, stores ...beads.OrdersStore) ([]OrderRun, error) {
	var all []OrderRun
	for _, store := range stores {
		if store.Store == nil {
			continue
		}
		runs, err := NewStore(store).RecentRuns(scoped, limit)
		all = append(all, runs...)
		if err != nil {
			return all, err
		}
	}
	sortRunsCreatedDesc(all)
	return all, nil
}

// EventCursorAcrossStores returns the max event-bus seq for scoped across a set
// of order stores. It is the federated face of bdCursorAcrossStores.
func EventCursorAcrossStores(scoped string, stores ...beads.OrdersStore) (EventCursor, error) {
	var maxCur EventCursor
	for _, store := range stores {
		if store.Store == nil {
			continue
		}
		cur, err := NewStore(store).EventCursor(scoped)
		if err != nil {
			return 0, err
		}
		if cur > maxCur {
			maxCur = cur
		}
	}
	return maxCur, nil
}

func sortRunsCreatedDesc(runs []OrderRun) {
	sort.SliceStable(runs, func(i, j int) bool {
		return runs[i].CreatedAt.After(runs[j].CreatedAt)
	})
}

// LastRun returns the cooldown clock: the CreatedAt of the most recent
// tracking/order-run bead for scoped across both tiers, and whether one exists.
// It is the typed face of LastRunFuncForStore.
func (s *Store) LastRun(scoped string) (time.Time, bool, error) {
	ts, err := LastRunFuncForStore(s.store.Store)(scoped)
	if err != nil {
		return time.Time{}, false, err
	}
	return ts, !ts.IsZero(), nil
}

// EventCursor returns the max event-bus seq for scoped across both tiers. It is
// the typed face of CursorFuncForStore, confining MaxSeqFromLabels inside.
func (s *Store) EventCursor(scoped string) (EventCursor, error) {
	return EventCursor(CursorFuncForStore(s.store.Store)(scoped)), nil
}
