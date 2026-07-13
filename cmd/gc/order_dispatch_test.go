package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/formulatest"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/pgauth"
	"github.com/gastownhall/gascity/internal/processgroup/processgrouptest"
)

func trackingBeads(t *testing.T, store beads.Store, label string) []beads.Bead {
	t.Helper()
	// Tracking beads dispatched by the production order dispatcher live in
	// the ephemeral (wisps) tier; seeded test beads typically live in the
	// issues tier. Query both so tests covering either path stay green.
	all, err := store.ListByLabel(label, 0, beads.IncludeClosed, beads.WithBothTiers)
	if err != nil {
		t.Fatalf("ListByLabel(%q): %v", label, err)
	}
	return all
}

func workBeadByOrderLabel(t *testing.T, store beads.Store, label string) beads.Bead {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		all := trackingBeads(t, store, label)
		for _, b := range all {
			if !strings.HasPrefix(b.Title, "order:") {
				return b
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no non-tracking bead found for %q", label)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type selectiveUpdateFailStore struct {
	beads.Store
}

type execLabelUpdateFailStore struct {
	beads.Store
}

type eventCursorUpdateFailStore struct {
	beads.Store
}

type latestSeqFailProvider struct {
	events.Provider
}

type triggerEvaluationFailStore struct {
	beads.Store

	lastRunOrder string
	cursorOrder  string
}

type countingListStore struct {
	beads.Store

	includeClosedLists int
}

func TestScanOrderSetSnapshotFSTracksAddChangeRemove(t *testing.T) {
	fs := fsys.NewFake()
	fs.Dirs["/city/orders"] = true
	cfg := &config.City{}

	fs.Files["/city/orders/tick.toml"] = []byte(`
[order]
exec = "true"
trigger = "cron"
schedule = "* * * * *"
`)
	first, err := scanOrderSetSnapshotFS(fs, "/city", cfg, io.Discard, "test")
	if err != nil {
		t.Fatalf("scanOrderSetSnapshotFS(first): %v", err)
	}
	if first.Signature == "" {
		t.Fatal("first signature is empty")
	}
	if got := orderSnapshotByName(first.Orders, "tick").Schedule; got != "* * * * *" {
		t.Fatalf("tick schedule = %q, want every minute", got)
	}

	fs.Files["/city/orders/tick.toml"] = []byte(`
[order]
exec = "true"
trigger = "cron"
schedule = "0 * * * *"
`)
	changed, err := scanOrderSetSnapshotFS(fs, "/city", cfg, io.Discard, "test")
	if err != nil {
		t.Fatalf("scanOrderSetSnapshotFS(changed): %v", err)
	}
	if changed.Signature == first.Signature {
		t.Fatal("signature did not change after order content changed")
	}
	if got := orderSnapshotByName(changed.Orders, "tick").Schedule; got != "0 * * * *" {
		t.Fatalf("tick schedule after change = %q, want hourly", got)
	}

	fs.Files["/city/orders/new-order.toml"] = []byte(`
[order]
exec = "true"
trigger = "cooldown"
interval = "1m"
`)
	added, err := scanOrderSetSnapshotFS(fs, "/city", cfg, io.Discard, "test")
	if err != nil {
		t.Fatalf("scanOrderSetSnapshotFS(added): %v", err)
	}
	if added.Signature == changed.Signature {
		t.Fatal("signature did not change after order add")
	}
	if got := orderSnapshotByName(added.Orders, "new-order").Interval; got != "1m" {
		t.Fatalf("new-order interval = %q, want 1m", got)
	}

	delete(fs.Files, "/city/orders/tick.toml")
	removed, err := scanOrderSetSnapshotFS(fs, "/city", cfg, io.Discard, "test")
	if err != nil {
		t.Fatalf("scanOrderSetSnapshotFS(removed): %v", err)
	}
	if removed.Signature == added.Signature {
		t.Fatal("signature did not change after order removal")
	}
	if orderSnapshotHasName(removed.Orders, "tick") {
		t.Fatalf("removed snapshot still contains tick: %#v", removed.Orders)
	}
}

func orderSnapshotByName(aa []orders.Order, name string) orders.Order {
	for _, a := range aa {
		if a.Name == name {
			return a
		}
	}
	return orders.Order{}
}

func orderSnapshotHasName(aa []orders.Order, name string) bool {
	return orderSnapshotByName(aa, name).Name != ""
}

type createdAtOverrideStore struct {
	beads.Store

	createdAt map[string]time.Time
}

type strictCloseReasonStore struct {
	beads.Store
}

func (s selectiveUpdateFailStore) Update(id string, opts beads.UpdateOpts) error {
	for _, label := range opts.Labels {
		if strings.HasPrefix(label, "order-run:") {
			return fmt.Errorf("label failed")
		}
	}
	return s.Store.Update(id, opts)
}

func (s execLabelUpdateFailStore) Update(id string, opts beads.UpdateOpts) error {
	for _, label := range opts.Labels {
		if label == "exec" {
			return fmt.Errorf("exec label failed")
		}
	}
	return s.Store.Update(id, opts)
}

func (s eventCursorUpdateFailStore) Update(id string, opts beads.UpdateOpts) error {
	for _, label := range opts.Labels {
		if strings.HasPrefix(label, "order:") {
			return fmt.Errorf("event cursor label failed")
		}
	}
	return s.Store.Update(id, opts)
}

func (p latestSeqFailProvider) LatestSeq() (uint64, error) {
	return 0, fmt.Errorf("latest seq failed")
}

func (s triggerEvaluationFailStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Label == "order-run:"+s.lastRunOrder && query.IncludeClosed {
		return nil, fmt.Errorf("last-run lookup should be skipped while trigger env failure is open")
	}
	if query.Label == "order:"+s.cursorOrder {
		return nil, fmt.Errorf("event cursor lookup should be skipped while trigger env failure is open")
	}
	return s.Store.List(query)
}

func (s *countingListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.IncludeClosed || query.Status == "closed" {
		s.includeClosedLists++
	}
	return s.Store.List(query)
}

func (s *countingListStore) reset() {
	s.includeClosedLists = 0
}

func (s *createdAtOverrideStore) Create(b beads.Bead) (beads.Bead, error) {
	created, err := s.Store.Create(b)
	if err != nil {
		return beads.Bead{}, err
	}
	if !b.CreatedAt.IsZero() {
		if s.createdAt == nil {
			s.createdAt = make(map[string]time.Time)
		}
		s.createdAt[created.ID] = b.CreatedAt
		created.CreatedAt = b.CreatedAt
	}
	return created, nil
}

func (s *createdAtOverrideStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	results, err := s.Store.List(query)
	if err != nil {
		return nil, err
	}
	for i := range results {
		if created, ok := s.createdAt[results[i].ID]; ok {
			results[i].CreatedAt = created
		}
	}
	return results, nil
}

func (s strictCloseReasonStore) Close(id string) error {
	return fmt.Errorf("strict close validation rejected reasonless close for %s", id)
}

func (s strictCloseReasonStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	reason := strings.TrimSpace(metadata["close_reason"])
	if len(reason) < 20 {
		return 0, fmt.Errorf("strict close validation rejected close_reason %q", reason)
	}
	return s.Store.CloseAll(ids, metadata)
}

func TestOrderDispatcherNil(t *testing.T) {
	ad := buildOrderDispatcher(t.TempDir(), &config.City{}, events.Discard, &bytes.Buffer{})
	if ad != nil {
		t.Error("expected nil dispatcher for empty orders")
	}
}

func TestBuildOrderDispatcherNoOrders(t *testing.T) {
	// City with formula layers that exist but contain no orders.
	dir := t.TempDir()
	cfg := &config.City{}
	ad := buildOrderDispatcher(dir, cfg, events.Discard, &bytes.Buffer{})
	if ad != nil {
		t.Error("expected nil dispatcher when no orders exist")
	}
}

func TestOrderDispatchManualFiltered(t *testing.T) {
	ad := buildOrderDispatcherFromList(
		[]orders.Order{{Name: "manual-only", Trigger: "manual", Formula: "noop"}},
		beads.NewMemStore(), nil,
	)
	if ad != nil {
		t.Error("expected nil dispatcher — manual orders should be filtered out")
	}
}

func TestOrderDispatchCooldownDue(t *testing.T) {
	store := beads.NewMemStore()

	aa := []orders.Order{{
		Name:         "test-order",
		Trigger:      "cooldown",
		Interval:     "1m",
		Formula:      "test-formula",
		Pool:         "worker",
		FormulaLayer: sharedTestFormulaDir,
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	ad.drain(context.Background())

	// Verify tracking bead was created.
	all := trackingBeads(t, store, "order-run:test-order")
	if len(all) == 0 {
		t.Fatal("expected tracking bead to be created")
	}
	found := false
	for _, b := range all {
		for _, l := range b.Labels {
			if l == "order-run:test-order" {
				found = true
			}
		}
	}
	if !found {
		t.Error("tracking bead missing order-run:test-order label")
	}

	work := workBeadByOrderLabel(t, store, "order-run:test-order")
	if !slicesContain(work.Labels, "order-run:test-order") {
		t.Errorf("work bead missing order-run:test-order label, got %v", work.Labels)
	}
	if work.Metadata["gc.routed_to"] != "worker" {
		t.Errorf("gc.routed_to = %q, want %q", work.Metadata["gc.routed_to"], "worker")
	}
}

// TestOrderDispatchResolvesPackBindingForPool reproduces issue #1268: a
// pack-imported agent has BindingName set, so its qualified name is
// "binding.name". A city-level order with pool="<name>" must resolve to the
// binding-qualified value at dispatch so the wisp's gc.routed_to matches what
// the scaler queries via Agent.QualifiedName().
func TestOrderDispatchResolvesPackBindingForPool(t *testing.T) {
	store := beads.NewMemStore()

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "dog", BindingName: "maintenance"},
		},
	}

	aa := []orders.Order{{
		Name:         "mol-dog-doctor",
		Trigger:      "cooldown",
		Interval:     "5m",
		Formula:      "test-formula",
		Pool:         "dog",
		FormulaLayer: sharedTestFormulaDir,
	}}

	m := &memoryOrderDispatcher{
		aa: aa,
		storeFn: func(_ execStoreTarget) (beads.Store, error) {
			return store, nil
		},
		execRun: shellExecRunner,
		rec:     events.Discard,
		stderr:  &bytes.Buffer{},
		cfg:     cfg,
	}

	m.dispatch(context.Background(), t.TempDir(), time.Now())
	m.drain(context.Background())

	work := workBeadByOrderLabel(t, store, "order-run:mol-dog-doctor")
	if got := work.Metadata["gc.routed_to"]; got != "maintenance.dog" {
		t.Errorf("gc.routed_to = %q, want %q (pack binding must qualify pool target)", got, "maintenance.dog")
	}
	assertNoDeprecatedPoolDemandMetadata(t, work.Metadata)
}

func TestOrderDispatchPoolLegacyFormulaWarnsWhenRootIsNotReadyVisible(t *testing.T) {
	formulaDir := t.TempDir()
	writeFile(t, filepath.Join(formulaDir, "mol-legacy-cleanup.toml"), `
formula = "mol-legacy-cleanup"
version = 1

[[steps]]
id = "work"
title = "Do legacy cleanup"
description = "Do the cleanup."
`)
	store := beads.NewMemStore()
	var stderr bytes.Buffer

	m := &memoryOrderDispatcher{
		aa: []orders.Order{{
			Name:         "legacy-cleanup",
			Trigger:      "cooldown",
			Interval:     "5m",
			Formula:      "mol-legacy-cleanup",
			Pool:         "dog",
			FormulaLayer: formulaDir,
		}},
		storeFn: func(_ execStoreTarget) (beads.Store, error) {
			return store, nil
		},
		execRun: shellExecRunner,
		rec:     events.Discard,
		stderr:  &stderr,
		cfg:     &config.City{},
	}

	m.dispatch(context.Background(), t.TempDir(), time.Now())
	m.drain(context.Background())

	if !strings.Contains(stderr.String(), "scale-from-zero pools will not wake") {
		t.Fatalf("stderr = %q, want pool visibility warning", stderr.String())
	}
	work := workBeadByOrderLabel(t, store, "order-run:legacy-cleanup")
	if work.Type != "molecule" {
		t.Fatalf("legacy root Type = %q, want molecule", work.Type)
	}
}

func TestOrderDispatchPrefersCityShadowForPool(t *testing.T) {
	store := beads.NewMemStore()

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "dog"},
			{Name: "dog", BindingName: "maintenance", SourceDir: "/city/packs/maintenance"},
		},
	}

	aa := []orders.Order{{
		Name:         "mol-dog-doctor",
		Trigger:      "cooldown",
		Interval:     "5m",
		Formula:      "test-formula",
		Pool:         "dog",
		FormulaLayer: sharedTestFormulaDir,
	}}

	m := &memoryOrderDispatcher{
		aa: aa,
		storeFn: func(_ execStoreTarget) (beads.Store, error) {
			return store, nil
		},
		execRun: shellExecRunner,
		rec:     events.Discard,
		stderr:  &bytes.Buffer{},
		cfg:     cfg,
	}

	m.dispatch(context.Background(), t.TempDir(), time.Now())
	m.drain(context.Background())

	work := workBeadByOrderLabel(t, store, "order-run:mol-dog-doctor")
	if got := work.Metadata["gc.routed_to"]; got != "dog" {
		t.Errorf("gc.routed_to = %q, want %q (city-local shadow should stay local)", got, "dog")
	}
}

func TestOrderDispatchRejectsAmbiguousPackPool(t *testing.T) {
	store := beads.NewMemStore()
	var rec memRecorder
	var stderr bytes.Buffer

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "dog", BindingName: "gastown"},
			{Name: "dog", BindingName: "maintenance"},
		},
	}

	aa := []orders.Order{{
		Name:         "mol-dog-doctor",
		Trigger:      "cooldown",
		Interval:     "5m",
		Formula:      "test-formula",
		Pool:         "dog",
		FormulaLayer: sharedTestFormulaDir,
	}}

	m := &memoryOrderDispatcher{
		aa: aa,
		storeFn: func(_ execStoreTarget) (beads.Store, error) {
			return store, nil
		},
		execRun: shellExecRunner,
		rec:     &rec,
		stderr:  &stderr,
		cfg:     cfg,
	}

	m.dispatch(context.Background(), t.TempDir(), time.Now())
	m.drain(context.Background())

	if !rec.hasType(events.OrderFailed) {
		t.Fatal("missing order.failed event for ambiguous pool")
	}
	if !strings.Contains(stderr.String(), `ambiguous pool "dog"`) {
		t.Fatalf("stderr = %q, want ambiguity error", stderr.String())
	}
	all := trackingBeads(t, store, "order-run:mol-dog-doctor")
	var workCount int
	for _, bead := range all {
		if !strings.HasPrefix(bead.Title, "order:") {
			workCount++
		}
	}
	if len(all) != 1 {
		t.Fatalf("tracking beads with order-run label = %d, want 1", len(all))
	}
	if workCount != 0 {
		t.Fatalf("work bead count = %d, want 0", workCount)
	}

	// An ambiguous failure should still count as the authoritative last run,
	// so the next patrol tick within the cooldown interval must not create a
	// second tracking bead or emit another order.failed event.
	failedEvents := 0
	for _, event := range rec.events {
		if event.Type == events.OrderFailed && event.Subject == "mol-dog-doctor" {
			failedEvents++
		}
	}
	if failedEvents != 1 {
		t.Fatalf("order.failed count after first dispatch = %d, want 1", failedEvents)
	}

	m.dispatch(context.Background(), t.TempDir(), time.Now().Add(10*time.Second))
	m.drain(context.Background())

	all = trackingBeads(t, store, "order-run:mol-dog-doctor")
	if len(all) != 1 {
		t.Fatalf("tracking beads with order-run label after second dispatch = %d, want 1", len(all))
	}
	failedEvents = 0
	for _, event := range rec.events {
		if event.Type == events.OrderFailed && event.Subject == "mol-dog-doctor" {
			failedEvents++
		}
	}
	if failedEvents != 1 {
		t.Fatalf("order.failed count after second dispatch = %d, want 1", failedEvents)
	}
}

func TestOrderDispatchRejectsAmbiguousEventPoolOncePerEvent(t *testing.T) {
	store := beads.NewMemStore()
	var rec memRecorder
	var stderr bytes.Buffer

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "dog", BindingName: "gastown"},
			{Name: "dog", BindingName: "maintenance"},
		},
	}

	eventLog := events.NewFake()
	eventLog.Record(events.Event{Type: events.BeadClosed, Actor: "test"})
	headSeq, err := eventLog.LatestSeq()
	if err != nil {
		t.Fatalf("LatestSeq(): %v", err)
	}

	aa := []orders.Order{{
		Name:         "release-watch",
		Trigger:      "event",
		On:           events.BeadClosed,
		Formula:      "test-formula",
		Pool:         "dog",
		FormulaLayer: sharedTestFormulaDir,
	}}

	m := &memoryOrderDispatcher{
		aa: aa,
		storeFn: func(_ execStoreTarget) (beads.Store, error) {
			return store, nil
		},
		ep:      eventLog,
		execRun: shellExecRunner,
		rec:     &rec,
		stderr:  &stderr,
		cfg:     cfg,
	}

	m.dispatch(context.Background(), t.TempDir(), time.Now())
	m.drain(context.Background())

	all := trackingBeads(t, store, "order-run:release-watch")
	if len(all) != 1 {
		t.Fatalf("tracking beads with order-run label after first dispatch = %d, want 1", len(all))
	}
	if !slicesContain(all[0].Labels, "order:release-watch") {
		t.Fatalf("tracking bead labels = %v, want order cursor label", all[0].Labels)
	}
	if !slicesContain(all[0].Labels, fmt.Sprintf("seq:%d", headSeq)) {
		t.Fatalf("tracking bead labels = %v, want seq:%d", all[0].Labels, headSeq)
	}

	failedEvents := 0
	for _, event := range rec.events {
		if event.Type == events.OrderFailed && event.Subject == "release-watch" {
			failedEvents++
		}
	}
	if failedEvents != 1 {
		t.Fatalf("order.failed count after first dispatch = %d, want 1", failedEvents)
	}

	m.dispatch(context.Background(), t.TempDir(), time.Now().Add(10*time.Second))
	m.drain(context.Background())

	all = trackingBeads(t, store, "order-run:release-watch")
	if len(all) != 1 {
		t.Fatalf("tracking beads with order-run label after second dispatch = %d, want 1", len(all))
	}
	failedEvents = 0
	for _, event := range rec.events {
		if event.Type == events.OrderFailed && event.Subject == "release-watch" {
			failedEvents++
		}
	}
	if failedEvents != 1 {
		t.Fatalf("order.failed count after second dispatch = %d, want 1", failedEvents)
	}
}

func TestOrderDispatchEventExecAdvancesCursor(t *testing.T) {
	store := beads.NewMemStore()
	eventLog := events.NewFake()
	eventLog.Record(events.Event{Type: events.BeadClosed, Actor: "test"})
	headSeq, err := eventLog.LatestSeq()
	if err != nil {
		t.Fatalf("LatestSeq(): %v", err)
	}

	var calls int
	execRun := func(context.Context, string, string, []string) ([]byte, error) {
		calls++
		return []byte("ok"), nil
	}

	ad := buildOrderDispatcherFromListExec([]orders.Order{{
		Name:    "release-exec",
		Trigger: "event",
		On:      events.BeadClosed,
		Exec:    "scripts/release.sh",
	}}, store, eventLog, execRun, events.Discard)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	ad.drain(context.Background())

	all := trackingBeads(t, store, "order-run:release-exec")
	if len(all) != 1 {
		t.Fatalf("tracking beads with order-run label after first dispatch = %d, want 1", len(all))
	}
	if !slicesContain(all[0].Labels, "order:release-exec") {
		t.Fatalf("tracking bead labels = %v, want order cursor label", all[0].Labels)
	}
	if !slicesContain(all[0].Labels, fmt.Sprintf("seq:%d", headSeq)) {
		t.Fatalf("tracking bead labels = %v, want seq:%d", all[0].Labels, headSeq)
	}
	if calls != 1 {
		t.Fatalf("exec calls after first dispatch = %d, want 1", calls)
	}

	ad.dispatch(context.Background(), t.TempDir(), time.Now().Add(10*time.Second))
	ad.drain(context.Background())

	all = trackingBeads(t, store, "order-run:release-exec")
	if len(all) != 1 {
		t.Fatalf("tracking beads with order-run label after second dispatch = %d, want 1", len(all))
	}
	if calls != 1 {
		t.Fatalf("exec calls after second dispatch = %d, want 1", calls)
	}
}

func TestOrderDispatchEventExecFailureAdvancesCursor(t *testing.T) {
	store := beads.NewMemStore()
	eventLog := events.NewFake()
	eventLog.Record(events.Event{Type: events.BeadClosed, Actor: "test"})
	headSeq, err := eventLog.LatestSeq()
	if err != nil {
		t.Fatalf("LatestSeq(): %v", err)
	}

	var calls int
	execRun := func(context.Context, string, string, []string) ([]byte, error) {
		calls++
		return []byte("failed"), fmt.Errorf("exit status 1")
	}

	ad := buildOrderDispatcherFromListExec([]orders.Order{{
		Name:    "release-exec",
		Trigger: "event",
		On:      events.BeadClosed,
		Exec:    "scripts/release.sh",
	}}, store, eventLog, execRun, events.Discard)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	ad.drain(context.Background())

	all := trackingBeads(t, store, "order-run:release-exec")
	if len(all) != 1 {
		t.Fatalf("tracking beads with order-run label after first dispatch = %d, want 1", len(all))
	}
	for _, want := range []string{"order:release-exec", fmt.Sprintf("seq:%d", headSeq), "exec-failed"} {
		if !slicesContain(all[0].Labels, want) {
			t.Fatalf("tracking bead labels = %v, want %s", all[0].Labels, want)
		}
	}
	if calls != 1 {
		t.Fatalf("exec calls after first dispatch = %d, want 1", calls)
	}

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	ad.drain(context.Background())

	all = trackingBeads(t, store, "order-run:release-exec")
	if len(all) != 1 {
		t.Fatalf("tracking beads with order-run label after second dispatch = %d, want 1", len(all))
	}
	if calls != 1 {
		t.Fatalf("exec calls after second dispatch = %d, want 1", calls)
	}
}

func TestOrderDispatchEventExecLatestSeqErrorDoesNotRunExec(t *testing.T) {
	store := beads.NewMemStore()
	tracking, err := store.Create(beads.Bead{
		Title:  "order:release-exec",
		Labels: []string{"order-run:release-exec", labelOrderTracking},
	})
	if err != nil {
		t.Fatal(err)
	}

	var calls int
	execRun := func(context.Context, string, string, []string) ([]byte, error) {
		calls++
		return []byte("ok"), nil
	}
	var rec memRecorder
	var stderr bytes.Buffer
	ad := buildOrderDispatcherFromListExec([]orders.Order{{
		Name:    "release-exec",
		Trigger: "event",
		On:      events.BeadClosed,
		Exec:    "scripts/release.sh",
	}}, store, events.NewFailFake(), execRun, &rec)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}
	mad := ad.(*memoryOrderDispatcher)
	mad.stderr = &stderr

	logs := captureCmdOrderLogs(t, func() {
		mad.dispatchExec(context.Background(), orders.NewStore(beads.OrdersStore{Store: store}), execStoreTarget{ScopeRoot: t.TempDir()}, mad.aa[0], t.TempDir(), tracking.ID, nil)
	})

	if calls != 0 {
		t.Fatalf("exec calls = %d, want 0", calls)
	}
	all := trackingBeads(t, store, "order-run:release-exec")
	if len(all) != 1 {
		t.Fatalf("tracking beads = %d, want 1", len(all))
	}
	if !slicesContain(all[0].Labels, "exec-failed") {
		t.Fatalf("tracking bead labels = %v, want exec-failed", all[0].Labels)
	}
	if !rec.hasType(events.OrderFailed) {
		t.Fatal("missing order.failed event")
	}
	combined := logs + "\n" + stderr.String()
	if !strings.Contains(combined, "reading event cursor for release-exec") {
		t.Fatalf("logs = %q, want event cursor read failure", combined)
	}

	eventLog := events.NewFake()
	eventLog.Record(events.Event{Type: events.BeadClosed, Actor: "test"})
	mad.ep = eventLog
	mad.dispatchExec(context.Background(), orders.NewStore(beads.OrdersStore{Store: store}), execStoreTarget{ScopeRoot: t.TempDir()}, mad.aa[0], t.TempDir(), tracking.ID, nil)

	if calls != 1 {
		t.Fatalf("exec calls after cursor read recovers = %d, want 1", calls)
	}
	all = trackingBeads(t, store, "order-run:release-exec")
	headSeq, err := eventLog.LatestSeq()
	if err != nil {
		t.Fatalf("LatestSeq(): %v", err)
	}
	for _, want := range []string{"order:release-exec", fmt.Sprintf("seq:%d", headSeq), "exec"} {
		if !slicesContain(all[0].Labels, want) {
			t.Fatalf("tracking bead labels after retry = %v, want %s", all[0].Labels, want)
		}
	}
}

func TestOrderDispatchEventExecLabelFailureRecordsOrderFailure(t *testing.T) {
	store := beads.NewMemStore()
	eventLog := events.NewFake()
	eventLog.Record(events.Event{Type: events.BeadClosed, Actor: "test"})
	headSeq, err := eventLog.LatestSeq()
	if err != nil {
		t.Fatalf("LatestSeq(): %v", err)
	}

	var calls int
	execRun := func(context.Context, string, string, []string) ([]byte, error) {
		calls++
		return []byte("ok"), nil
	}
	var rec memRecorder
	var stderr bytes.Buffer
	ad := buildOrderDispatcherFromListExec([]orders.Order{{
		Name:    "release-exec",
		Trigger: "event",
		On:      events.BeadClosed,
		Exec:    "scripts/release.sh",
	}}, execLabelUpdateFailStore{Store: store}, eventLog, execRun, &rec)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}
	mad := ad.(*memoryOrderDispatcher)
	mad.stderr = &stderr

	logs := captureCmdOrderLogs(t, func() {
		ad.dispatch(context.Background(), t.TempDir(), time.Now())
		ad.drain(context.Background())
	})

	if calls != 1 {
		t.Fatalf("exec calls = %d, want 1", calls)
	}
	if !rec.hasType(events.OrderFailed) {
		t.Fatal("missing order.failed event")
	}
	if rec.hasType(events.OrderCompleted) {
		t.Fatal("unexpected order.completed event")
	}
	all := trackingBeads(t, store, "order-run:release-exec")
	if len(all) != 1 {
		t.Fatalf("tracking beads with order-run label after first dispatch = %d, want 1", len(all))
	}
	for _, want := range []string{"order:release-exec", fmt.Sprintf("seq:%d", headSeq)} {
		if !slicesContain(all[0].Labels, want) {
			t.Fatalf("tracking bead labels = %v, want %s", all[0].Labels, want)
		}
	}

	ad.dispatch(context.Background(), t.TempDir(), time.Now().Add(10*time.Second))
	ad.drain(context.Background())

	all = trackingBeads(t, store, "order-run:release-exec")
	if len(all) != 1 {
		t.Fatalf("tracking beads with order-run label after second dispatch = %d, want 1", len(all))
	}
	if calls != 1 {
		t.Fatalf("exec calls after second dispatch = %d, want 1", calls)
	}
	combined := logs + "\n" + stderr.String()
	if !strings.Contains(combined, "failed to label exec tracking bead") {
		t.Fatalf("logs = %q, want tracking label failure", combined)
	}
}

func TestOrderDispatchEventExecCursorLabelFailureMarksExecFailed(t *testing.T) {
	store := beads.NewMemStore()
	eventLog := events.NewFake()
	eventLog.Record(events.Event{Type: events.BeadClosed, Actor: "test"})

	var calls int
	execRun := func(context.Context, string, string, []string) ([]byte, error) {
		calls++
		return []byte("ok"), nil
	}
	var rec memRecorder
	var stderr bytes.Buffer
	ad := buildOrderDispatcherFromListExec([]orders.Order{{
		Name:    "release-exec",
		Trigger: "event",
		On:      events.BeadClosed,
		Exec:    "scripts/release.sh",
	}}, eventCursorUpdateFailStore{Store: store}, eventLog, execRun, &rec)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}
	mad := ad.(*memoryOrderDispatcher)
	mad.stderr = &stderr

	logs := captureCmdOrderLogs(t, func() {
		ad.dispatch(context.Background(), t.TempDir(), time.Now())
		ad.drain(context.Background())
	})

	if calls != 0 {
		t.Fatalf("exec calls = %d, want 0", calls)
	}
	if !rec.hasType(events.OrderFailed) {
		t.Fatal("missing order.failed event")
	}
	if rec.hasType(events.OrderCompleted) {
		t.Fatal("unexpected order.completed event")
	}
	all := trackingBeads(t, store, "order-run:release-exec")
	if len(all) != 1 {
		t.Fatalf("tracking beads with order-run label = %d, want 1", len(all))
	}
	if !slicesContain(all[0].Labels, "exec-failed") {
		t.Fatalf("tracking bead labels = %v, want exec-failed", all[0].Labels)
	}
	combined := logs + "\n" + stderr.String()
	if !strings.Contains(combined, "failed to label exec event cursor") {
		t.Fatalf("logs = %q, want cursor label failure", combined)
	}
}

func TestOrderDispatchEventWispLatestSeqErrorDoesNotInstantiate(t *testing.T) {
	store := beads.NewMemStore()
	tracking, err := store.Create(beads.Bead{
		Title:  "order:release-watch",
		Labels: []string{"order-run:release-watch", labelOrderTracking},
	})
	if err != nil {
		t.Fatal(err)
	}

	var rec memRecorder
	var stderr bytes.Buffer
	ad := buildOrderDispatcherFromListExec([]orders.Order{{
		Name:         "release-watch",
		Trigger:      "event",
		On:           events.BeadClosed,
		Formula:      "test-formula",
		FormulaLayer: sharedTestFormulaDir,
	}}, store, latestSeqFailProvider{Provider: events.NewFake()}, successfulExec, &rec)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}
	mad := ad.(*memoryOrderDispatcher)
	mad.stderr = &stderr

	mad.dispatchWisp(context.Background(), store, execStoreTarget{}, mad.aa[0], t.TempDir(), tracking.ID, nil)

	all := trackingBeads(t, store, "order-run:release-watch")
	if len(all) != 1 {
		t.Fatalf("tracking beads with order-run label = %d, want only tracking bead", len(all))
	}
	if !slicesContain(all[0].Labels, "wisp-failed") {
		t.Fatalf("tracking bead labels = %v, want wisp-failed", all[0].Labels)
	}
	if !rec.hasType(events.OrderFailed) {
		t.Fatal("missing order.failed event")
	}
	if !strings.Contains(stderr.String(), "reading event cursor for release-watch") {
		t.Fatalf("stderr = %q, want event cursor read failure", stderr.String())
	}
}

func TestOrderDispatchGraphV2ConvoyReferenceFailsBeforeInstantiate(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	formulaBody := `
formula = "graph-needs-convoy"
version = 1
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "step"
title = "Do work"
description = "Inspect convoy {{convoy_id}}"
`
	if err := os.WriteFile(filepath.Join(dir, "graph-needs-convoy.formula.toml"), []byte(strings.TrimSpace(formulaBody)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := beads.NewMemStore()
	tracking, err := store.Create(beads.Bead{
		Title:  "order:convoy-patrol",
		Labels: []string{"order-run:convoy-patrol", labelOrderTracking},
	})
	if err != nil {
		t.Fatal(err)
	}
	var rec memRecorder
	ad := buildOrderDispatcherFromListExec([]orders.Order{{
		Name:         "convoy-patrol",
		Trigger:      "cooldown",
		Interval:     "15m",
		Formula:      "graph-needs-convoy",
		FormulaLayer: dir,
	}}, store, nil, successfulExec, &rec)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}
	mad := ad.(*memoryOrderDispatcher)

	mad.dispatchWisp(context.Background(), store, execStoreTarget{}, mad.aa[0], t.TempDir(), tracking.ID, nil)

	all := trackingBeads(t, store, "order-run:convoy-patrol")
	if len(all) != 1 {
		t.Fatalf("tracking beads with order-run label = %d, want only tracking bead", len(all))
	}
	if !slicesContain(all[0].Labels, "wisp-failed") {
		t.Fatalf("tracking bead labels = %v, want wisp-failed", all[0].Labels)
	}
	if !rec.hasType(events.OrderFailed) {
		t.Fatal("missing order.failed event")
	}
}

func TestOrderDispatchGraphWorkflowWithoutPoolUsesRigStoreScope(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "fixture")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	formulaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(formulaDir, "rig-order.toml"), []byte(`
formula = "rig-order"
version = 2
contract = "graph.v2"

[[steps]]
id = "work"
title = "Rig work"
metadata = { "gc.run_target" = "worker" }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	maxOne, maxTwo := 1, 2
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "fixture", Path: rigPath}},
		Agents: []config.Agent{
			{Name: "worker", Dir: "fixture", MaxActiveSessions: &maxTwo},
			{Name: config.ControlDispatcherAgentName, MaxActiveSessions: &maxOne},
			{Name: config.ControlDispatcherAgentName, Dir: "fixture", MaxActiveSessions: &maxOne},
		},
	}
	a := orders.Order{Name: "rig-patrol", Rig: "fixture", Formula: "rig-order", Trigger: "cooldown", Interval: "15m", FormulaLayer: formulaDir}
	store := beads.NewMemStore()
	var rec memRecorder
	dispatchCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var gotTargets []execStoreTarget
	m := &memoryOrderDispatcher{
		aa: []orders.Order{a},
		storeFn: func(target execStoreTarget) (beads.Store, error) {
			gotTargets = append(gotTargets, target)
			return store, nil
		},
		cfg:                  cfg,
		cityName:             "test-city",
		cityPath:             cityPath,
		rec:                  &rec,
		stderr:               io.Discard,
		maxDispatchesPerTick: 1,
		dispatchCtx:          dispatchCtx,
		dispatchCancel:       cancel,
	}
	m.dispatch(context.Background(), cityPath, time.Now())
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer drainCancel()
	if !m.drain(drainCtx) {
		t.Fatal("order dispatch did not drain")
	}
	foundRigTarget := false
	for _, gotTarget := range gotTargets {
		if gotTarget.ScopeKind == "rig" && gotTarget.RigName == "fixture" && samePath(gotTarget.ScopeRoot, rigPath) {
			foundRigTarget = true
		}
	}
	if !foundRigTarget {
		t.Fatalf("resolved targets = %+v, want fixture rig store", gotTargets)
	}

	all, err := store.ListOpen()
	if err != nil {
		t.Fatal(err)
	}
	var foundWork, foundControl bool
	for _, bead := range all {
		if got := bead.Metadata[beadmeta.RootStoreRefMetadataKey]; got != "rig:fixture" {
			t.Fatalf("%s gc.root_store_ref = %q, want rig:fixture", bead.Title, got)
		}
		if bead.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindWorkflow {
			if got := bead.Metadata[beadmeta.ScopeKindMetadataKey]; got != "rig" {
				t.Fatalf("workflow root gc.scope_kind = %q, want rig", got)
			}
			if got := bead.Metadata[beadmeta.ScopeRefMetadataKey]; got != "fixture" {
				t.Fatalf("workflow root gc.scope_ref = %q, want fixture", got)
			}
		}
		if bead.Title == "Rig work" {
			if got := bead.Metadata[beadmeta.RoutedToMetadataKey]; got != "fixture/worker" {
				t.Fatalf("work gc.routed_to = %q, want fixture/worker", got)
			}
			foundWork = true
		}
		if bead.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindWorkflowFinalize {
			if got := bead.Metadata[beadmeta.RoutedToMetadataKey]; got != "fixture/control-dispatcher" {
				t.Fatalf("finalize gc.routed_to = %q, want fixture/control-dispatcher", got)
			}
			foundControl = true
		}
	}
	if !foundWork || !foundControl {
		t.Fatalf("found work=%v control=%v; beads=%+v", foundWork, foundControl, all)
	}
	if !rec.hasType(events.OrderCompleted) || rec.hasType(events.OrderFailed) {
		t.Fatalf("events = %+v, want completed without failure", rec.events)
	}
}

func TestOrderDispatchRigOwnedGraphKeepsOwnerStoreWhenPoolRunsOnAnotherRig(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	cityPath := t.TempDir()
	ownerPath := filepath.Join(cityPath, "owner")
	executorPath := filepath.Join(cityPath, "executor")
	for _, path := range []string{ownerPath, executorPath} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	formulaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(formulaDir, "cross-rig-order.toml"), []byte(`
formula = "cross-rig-order"
version = 2
contract = "graph.v2"

[[steps]]
id = "work"
title = "Cross-rig work"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	maxOne, maxTwo := 1, 2
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{
			{Name: "owner", Path: ownerPath},
			{Name: "executor", Path: executorPath},
		},
		Agents: []config.Agent{
			{Name: "worker", Dir: "executor", MaxActiveSessions: &maxTwo},
			{Name: config.ControlDispatcherAgentName, MaxActiveSessions: &maxOne},
			{Name: config.ControlDispatcherAgentName, Dir: "owner", MaxActiveSessions: &maxOne},
		},
	}
	a := orders.Order{
		Name:         "cross-rig-patrol",
		Rig:          "owner",
		Formula:      "cross-rig-order",
		Pool:         "executor/worker",
		Trigger:      "cooldown",
		Interval:     "15m",
		FormulaLayer: formulaDir,
	}
	cityStore := beads.NewMemStore()
	ownerStore := beads.NewMemStore()
	executorStore := beads.NewMemStore()
	var rec memRecorder
	dispatchCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &memoryOrderDispatcher{
		aa: []orders.Order{a},
		storeFn: func(target execStoreTarget) (beads.Store, error) {
			switch {
			case target.ScopeKind == "city":
				return cityStore, nil
			case target.RigName == "owner":
				return ownerStore, nil
			case target.RigName == "executor":
				return executorStore, nil
			default:
				return nil, fmt.Errorf("unexpected order store target: %+v", target)
			}
		},
		cfg:                  cfg,
		cityName:             "test-city",
		cityPath:             cityPath,
		rec:                  &rec,
		stderr:               io.Discard,
		maxDispatchesPerTick: 1,
		dispatchCtx:          dispatchCtx,
		dispatchCancel:       cancel,
	}
	m.dispatch(context.Background(), cityPath, time.Now())
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer drainCancel()
	if !m.drain(drainCtx) {
		t.Fatal("order dispatch did not drain")
	}

	executorBeads, err := executorStore.ListOpen()
	if err != nil {
		t.Fatal(err)
	}
	if len(executorBeads) != 0 {
		t.Fatalf("executor store beads = %+v, want graph to remain in owner store", executorBeads)
	}
	all, err := ownerStore.ListOpen()
	if err != nil {
		t.Fatal(err)
	}
	var foundWork, foundControl bool
	for _, bead := range all {
		if got := bead.Metadata[beadmeta.RootStoreRefMetadataKey]; got != "rig:owner" {
			t.Fatalf("%s gc.root_store_ref = %q, want rig:owner", bead.Title, got)
		}
		if bead.Title == "Cross-rig work" {
			if got := bead.Metadata[beadmeta.RoutedToMetadataKey]; got != "executor/worker" {
				t.Fatalf("worker gc.routed_to = %q, want executor/worker", got)
			}
			foundWork = true
		}
		if bead.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindWorkflowFinalize {
			if got := bead.Metadata[beadmeta.RoutedToMetadataKey]; got != "owner/control-dispatcher" {
				t.Fatalf("finalize gc.routed_to = %q, want owner/control-dispatcher", got)
			}
			if got := bead.Metadata[beadmeta.ExecutionRoutedToMetadataKey]; got != "executor/worker" {
				t.Fatalf("finalize execution route = %q, want executor/worker", got)
			}
			foundControl = true
		}
	}
	if !foundWork || !foundControl {
		t.Fatalf("found work=%v control=%v; owner beads=%+v", foundWork, foundControl, all)
	}
	if !rec.hasType(events.OrderCompleted) || rec.hasType(events.OrderFailed) {
		t.Fatalf("events = %+v, want completed without failure", rec.events)
	}
}

func TestOrderDispatchMissingRigDispatcherFailsBeforeInstantiate(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "fixture")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	formulaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(formulaDir, "rig-order.toml"), []byte(`
formula = "rig-order"
version = 2
contract = "graph.v2"

[[steps]]
id = "work"
title = "Rig work"
metadata = { "gc.run_target" = "worker" }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	maxOne, maxTwo := 1, 2
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "fixture", Path: rigPath}},
		Agents: []config.Agent{
			{Name: "worker", Dir: "fixture", MaxActiveSessions: &maxTwo},
			{Name: config.ControlDispatcherAgentName, MaxActiveSessions: &maxOne},
		},
	}
	a := orders.Order{Name: "rig-patrol", Rig: "fixture", Formula: "rig-order", Trigger: "cooldown", Interval: "15m", FormulaLayer: formulaDir}
	target, err := resolveOrderStoreTarget(cityPath, cfg, a)
	if err != nil {
		t.Fatal(err)
	}
	store := beads.NewMemStore()
	tracking, err := store.Create(beads.Bead{Title: "order:rig-patrol", Labels: []string{"order-run:rig-patrol", labelOrderTracking}})
	if err != nil {
		t.Fatal(err)
	}
	var rec memRecorder
	m := &memoryOrderDispatcher{cfg: cfg, cityName: "test-city", rec: &rec, stderr: io.Discard}
	m.dispatchWisp(context.Background(), store, target, a, cityPath, tracking.ID, nil)

	all, err := store.ListOpen()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].ID != tracking.ID {
		t.Fatalf("open beads = %+v, want only tracking bead", all)
	}
	if !rec.hasType(events.OrderFailed) || rec.hasType(events.OrderCompleted) {
		t.Fatalf("events = %+v, want failed without completed", rec.events)
	}
	if !slicesContain(all[0].Labels, "wisp-failed") {
		t.Fatalf("tracking labels = %v, want wisp-failed", all[0].Labels)
	}
}

func TestApplyOrderRecipeRoutingNoPoolRejectsMissingAndUnknownStepTargets(t *testing.T) {
	cityPath := t.TempDir()
	target := execStoreTarget{ScopeRoot: cityPath, ScopeKind: "city"}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: config.ControlDispatcherAgentName}},
	}
	for _, tt := range []struct {
		name       string
		runTarget  string
		wantErrSub string
	}{
		{name: "missing", wantErrSub: `has no routing target`},
		{name: "unknown", runTarget: "unknown-worker", wantErrSub: `unknown formulas v2 target "unknown-worker"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			metadata := map[string]string{}
			if tt.runTarget != "" {
				metadata[beadmeta.RunTargetMetadataKey] = tt.runTarget
			}
			recipe := &formula.Recipe{
				Name: "order-graph",
				Steps: []formula.RecipeStep{
					{ID: "order-graph", IsRoot: true, Metadata: map[string]string{
						beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
						beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
					}},
					{ID: "order-graph.work", Title: "Work", Metadata: metadata},
				},
			}
			err := applyOrderRecipeRouting(recipe, "", nil, target, beads.NewMemStore(), "test-city", cityPath, cfg)
			if err == nil || !strings.Contains(err.Error(), tt.wantErrSub) {
				t.Fatalf("applyOrderRecipeRouting error = %v, want %q", err, tt.wantErrSub)
			}
		})
	}
}

func TestOrderDispatchResolvesImportedPackPoolAgainstCityShadow(t *testing.T) {
	cityDir := t.TempDir()
	writeImportedDogOrderFixture(t, cityDir, true)
	cfg, aa := loadImportedDogOrders(t, cityDir)
	store := beads.NewMemStore()
	digest := orders.Order{}
	for _, order := range aa {
		if order.Name == "digest" {
			digest = order
			break
		}
	}
	if digest.Name == "" {
		t.Fatal("missing digest order")
	}
	dispatchCtx, dispatchCancel := context.WithCancel(context.Background())

	m := &memoryOrderDispatcher{
		aa: []orders.Order{digest},
		storeFn: func(_ execStoreTarget) (beads.Store, error) {
			return store, nil
		},
		execRun:        successfulExec,
		rec:            events.Discard,
		stderr:         &bytes.Buffer{},
		cfg:            cfg,
		dispatchCtx:    dispatchCtx,
		dispatchCancel: dispatchCancel,
	}
	t.Cleanup(func() {
		m.cancel()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		m.drain(ctx)
	})

	m.dispatch(context.Background(), cityDir, time.Now())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if !m.drain(ctx) {
		t.Fatal("drain timed out waiting for imported-pack dispatch to finish")
	}

	work := workBeadByOrderLabel(t, store, "order-run:digest")
	if got := work.Metadata["gc.routed_to"]; got != "maintenance.dog" {
		t.Fatalf("gc.routed_to = %q, want maintenance.dog", got)
	}
}

func TestOrderDispatchResolvesImportedPackPoolAgainstSiblingImportCollision(t *testing.T) {
	cityDir := t.TempDir()
	writeImportedDogOrderFixture(t, cityDir, false, "gastown")
	cfg, aa := loadImportedDogOrders(t, cityDir)
	store := beads.NewMemStore()
	digest := orders.Order{}
	for _, order := range aa {
		if order.Name == "digest" {
			digest = order
			break
		}
	}
	if digest.Name == "" {
		t.Fatal("missing digest order")
	}
	dispatchCtx, dispatchCancel := context.WithCancel(context.Background())

	m := &memoryOrderDispatcher{
		aa: []orders.Order{digest},
		storeFn: func(_ execStoreTarget) (beads.Store, error) {
			return store, nil
		},
		execRun:        successfulExec,
		rec:            events.Discard,
		stderr:         &bytes.Buffer{},
		cfg:            cfg,
		dispatchCtx:    dispatchCtx,
		dispatchCancel: dispatchCancel,
	}
	t.Cleanup(func() {
		m.cancel()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		m.drain(ctx)
	})

	m.dispatch(context.Background(), cityDir, time.Now())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if !m.drain(ctx) {
		t.Fatal("drain timed out waiting for imported-pack dispatch to finish")
	}

	work := workBeadByOrderLabel(t, store, "order-run:digest")
	if got := work.Metadata["gc.routed_to"]; got != "maintenance.dog" {
		t.Fatalf("gc.routed_to = %q, want maintenance.dog", got)
	}
}

func TestDoltPackDogOrdersResolveWithNonGastownMaintenanceBinding(t *testing.T) {
	cityDir := t.TempDir()
	opsDir := filepath.Join(cityDir, "packs", "ops")
	if err := os.MkdirAll(opsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cityDir, "city.toml"), `
[workspace]
name = "portable-city"
`)
	writeFile(t, filepath.Join(opsDir, "pack.toml"), `
[pack]
name = "ops"
schema = 2

[[agent]]
name = "dog"
scope = "city"
`)
	doltDir, err := filepath.Abs(filepath.Join("..", "..", "examples", "bd", "dolt"))
	if err != nil {
		t.Fatalf("Abs(examples/bd/dolt): %v", err)
	}
	writeFile(t, filepath.Join(cityDir, "pack.toml"), `
[pack]
name = "portable-city"
schema = 2

[imports.ops]
source = "./packs/ops"

[imports.dolt]
source = "`+doltDir+`"
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	var stderr bytes.Buffer
	aa, err := scanAllOrders(cityDir, cfg, &stderr, "gc order list")
	if err != nil {
		t.Fatalf("scanAllOrders: %v; stderr: %s", err, stderr.String())
	}

	wantExecDogOrders := map[string]string{
		"mol-dog-backup":     "$PACK_DIR/assets/scripts/mol-dog-backup.sh",
		"mol-dog-compactor":  "gc dolt compact",
		"mol-dog-doctor":     "$PACK_DIR/assets/scripts/mol-dog-doctor.sh",
		"mol-dog-phantom-db": "$PACK_DIR/assets/scripts/mol-dog-phantom-db.sh",
	}
	gotExecDogOrders := map[string]bool{}
	const wantFormulaDogOrders = 1
	var gotFormulaDogOrders int
	for _, a := range aa {
		if !strings.HasPrefix(a.Name, "mol-dog-") {
			continue
		}
		if a.Exec != "" {
			wantExec, ok := wantExecDogOrders[a.Name]
			if !ok {
				t.Fatalf("unexpected exec dog order %q", a.Name)
			}
			if a.Pool != "" {
				t.Fatalf("%s exec order pool = %q, want empty", a.Name, a.Pool)
			}
			if a.Exec != wantExec {
				t.Fatalf("%s exec = %q, want %q", a.Name, a.Exec, wantExec)
			}
			const packScriptPrefix = "$PACK_DIR/assets/scripts/"
			if scriptName := strings.TrimPrefix(wantExec, packScriptPrefix); scriptName != wantExec {
				packDir := orderPoolSourceDirHint(a)
				if packDir == "" {
					t.Fatalf("%s pack dir hint is empty for script-backed exec order", a.Name)
				}
				scriptPath := filepath.Join(packDir, "assets", "scripts", scriptName)
				if _, err := os.Stat(scriptPath); err != nil {
					t.Fatalf("%s exec script missing: %v", a.Name, err)
				}
				if _, err := exec.LookPath("bash"); err == nil {
					if out, err := exec.Command("bash", "-n", scriptPath).CombinedOutput(); err != nil {
						t.Fatalf("%s bash -n failed: %v\n%s", a.Name, err, out)
					}
				}
			}
			gotExecDogOrders[a.Name] = true
			continue
		}
		gotFormulaDogOrders++
		if a.Pool != "dog" {
			t.Fatalf("%s pool = %q, want portable bare dog", a.Name, a.Pool)
		}
		got, err := qualifyOrderPool(a, cfg)
		if err != nil {
			t.Fatalf("qualifyOrderPool(%s): %v", a.Name, err)
		}
		if got != "dolt.dog" {
			t.Fatalf("qualifyOrderPool(%s) = %q, want Dolt-local dog", a.Name, got)
		}
	}
	if gotFormulaDogOrders != wantFormulaDogOrders {
		t.Fatalf("Dolt formula-based dog order count = %d, want %d", gotFormulaDogOrders, wantFormulaDogOrders)
	}
	if len(gotExecDogOrders) != len(wantExecDogOrders) {
		t.Fatalf("Dolt exec dog orders = %v, want %v", gotExecDogOrders, wantExecDogOrders)
	}
}

func TestOrderDispatchCooldownNotDue(t *testing.T) {
	store := beads.NewMemStore()

	// Seed a recent order-run bead.
	_, err := store.Create(beads.Bead{
		Title:  "order run",
		Labels: []string{"order-run:test-order"},
	})
	if err != nil {
		t.Fatal(err)
	}

	aa := []orders.Order{{
		Name:     "test-order",
		Trigger:  "cooldown",
		Interval: "1h", // 1 hour — far in the future
		Formula:  "test-formula",
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	ad.drain(context.Background())

	// Should still have only the seed bead.
	all, _ := store.ListOpen()
	if len(all) != 1 {
		t.Errorf("expected 1 bead (seed only), got %d", len(all))
	}
}

type strictOpenWorkListCountingStore struct {
	beads.Store

	strictOpenWorkLists int
}

func (s *strictOpenWorkListCountingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if strings.HasPrefix(query.Label, "order-run:") && !query.IncludeClosed && query.Limit == 0 {
		s.strictOpenWorkLists++
		return nil, fmt.Errorf("strict open-work scan should only run for due orders")
	}
	return s.Store.List(query)
}

func TestOrderDispatchCooldownNotDueSkipsStrictOpenWorkScan(t *testing.T) {
	store := &strictOpenWorkListCountingStore{Store: beads.NewMemStore()}
	now := time.Date(2026, 6, 3, 4, 40, 0, 0, time.UTC)

	if _, err := store.Create(beads.Bead{
		Title:     "recent run",
		Labels:    []string{"order-run:test-order"},
		CreatedAt: now.Add(-5 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	aa := []orders.Order{{
		Name:     "test-order",
		Trigger:  "cooldown",
		Interval: "1h",
		Exec:     "true",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, successfulExec, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	ad.dispatch(context.Background(), t.TempDir(), now)
	ad.drain(context.Background())

	if store.strictOpenWorkLists != 0 {
		t.Fatalf("strict open-work scans = %d, want 0 for not-due order", store.strictOpenWorkLists)
	}
}

func TestOrderDispatchMultiple(t *testing.T) {
	store := beads.NewMemStore()

	// Seed a recent run for order-b so only order-a is due.
	_, err := store.Create(beads.Bead{
		Title:  "recent run",
		Labels: []string{"order-run:order-b"},
	})
	if err != nil {
		t.Fatal(err)
	}

	aa := []orders.Order{
		{Name: "order-a", Trigger: "cooldown", Interval: "1m", Formula: "formula-a"},
		{Name: "order-b", Trigger: "cooldown", Interval: "1h", Formula: "formula-b"},
	}
	ad := buildOrderDispatcherFromList(aa, store, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	ad.drain(context.Background())

	// Should have the seed bead + 1 tracking bead for order-a.
	all := trackingBeads(t, store, "order-run:order-a")
	trackingCount := 0
	for _, b := range all {
		for _, l := range b.Labels {
			if l == "order-run:order-a" {
				trackingCount++
			}
		}
	}
	if trackingCount != 1 {
		t.Errorf("expected 1 tracking bead for order-a, got %d", trackingCount)
	}
}

func TestOrderDispatchRespectsMaxDispatchesPerTick(t *testing.T) {
	store := beads.NewMemStore()
	var aa []orders.Order
	for i := 0; i < 5; i++ {
		aa = append(aa, orders.Order{
			Name:     fmt.Sprintf("order-%d", i),
			Trigger:  "cooldown",
			Interval: "1m",
			Exec:     "true",
		})
	}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, func(context.Context, string, string, []string) ([]byte, error) {
		return []byte("ok\n"), nil
	}, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}
	m := ad.(*memoryOrderDispatcher)
	m.maxDispatchesPerTick = 2

	now := time.Date(2026, 5, 19, 2, 30, 0, 0, time.UTC)
	ad.dispatch(context.Background(), t.TempDir(), now)
	ad.drain(context.Background())

	if got := countOrderTrackingRuns(t, store); got != 2 {
		t.Fatalf("tracking runs after first tick = %d, want 2", got)
	}

	ad.dispatch(context.Background(), t.TempDir(), now.Add(time.Second))
	ad.drain(context.Background())
	if got := countOrderTrackingRuns(t, store); got != 4 {
		t.Fatalf("tracking runs after second tick = %d, want 4", got)
	}
}

func TestOrderDispatchBudgetRotatesAcrossAlwaysDueOrders(t *testing.T) {
	store := beads.NewMemStore()
	var aa []orders.Order
	for i := 0; i < 5; i++ {
		aa = append(aa, orders.Order{
			Name:    fmt.Sprintf("condition-%d", i),
			Trigger: "condition",
			Check:   "true",
			Exec:    "true",
		})
	}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, func(context.Context, string, string, []string) ([]byte, error) {
		return []byte("ok\n"), nil
	}, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}
	m := ad.(*memoryOrderDispatcher)
	m.maxDispatchesPerTick = 2

	now := time.Date(2026, 5, 19, 2, 30, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		ad.dispatch(context.Background(), t.TempDir(), now.Add(time.Duration(i)*time.Second))
		ad.drain(context.Background())
	}

	for i := 0; i < 5; i++ {
		label := fmt.Sprintf("order-run:condition-%d", i)
		if got := len(trackingBeads(t, store, label)); got == 0 {
			t.Fatalf("%s did not dispatch under a rotating budget", label)
		}
	}
}

func countOrderTrackingRuns(t *testing.T, store beads.Store) int {
	t.Helper()
	all, err := store.ListByLabel(labelOrderTracking, 0, beads.IncludeClosed, beads.WithBothTiers)
	if err != nil {
		t.Fatalf("ListByLabel(%q): %v", labelOrderTracking, err)
	}
	count := 0
	for _, b := range all {
		if strings.HasPrefix(b.Title, "order:") {
			count++
		}
	}
	return count
}

func TestOrderDispatchCachesLastRunBetweenDispatches(t *testing.T) {
	store := &countingListStore{Store: beads.NewMemStore()}

	if _, err := store.Create(beads.Bead{
		Title:  "recent run",
		Labels: []string{"order-run:test-order"},
	}); err != nil {
		t.Fatal(err)
	}

	aa := []orders.Order{{
		Name:     "test-order",
		Trigger:  "cooldown",
		Interval: "1h",
		Formula:  "test-formula",
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	cityPath := t.TempDir()
	now := time.Now()
	ad.dispatch(context.Background(), cityPath, now)
	if store.includeClosedLists == 0 {
		t.Fatal("first dispatch did not read persisted order history")
	}

	store.reset()
	ad.dispatch(context.Background(), cityPath, now.Add(time.Second))
	if store.includeClosedLists != 0 {
		t.Fatalf("second dispatch performed %d closed-history reads, want cached last-run result", store.includeClosedLists)
	}

	all, _ := store.ListOpen()
	if len(all) != 1 {
		t.Errorf("expected only seed bead, got %d", len(all))
	}
}

func TestOrderDispatchRefreshesCachedLastRunBeforeDueDispatch(t *testing.T) {
	baseStore := &createdAtOverrideStore{Store: beads.NewMemStore()}
	store := &countingListStore{Store: baseStore}
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)

	if _, err := store.Create(beads.Bead{
		Title:     "recent run",
		Labels:    []string{"order-run:test-order"},
		CreatedAt: now.Add(-30 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	aa := []orders.Order{{
		Name:     "test-order",
		Trigger:  "cooldown",
		Interval: "1h",
		Exec:     "true",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, successfulExec, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	cityPath := t.TempDir()
	ad.dispatch(context.Background(), cityPath, now)
	if store.includeClosedLists == 0 {
		t.Fatal("first dispatch did not read persisted order history")
	}

	store.reset()
	if _, err := store.Create(beads.Bead{
		Title:     "manual run",
		Labels:    []string{"order-run:test-order"},
		CreatedAt: now.Add(20 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	ad.dispatch(context.Background(), cityPath, now.Add(31*time.Minute))
	if store.includeClosedLists == 0 {
		t.Fatal("due cached dispatch did not refresh persisted order history")
	}

	all := trackingBeads(t, store, "order-run:test-order")
	if len(all) != 2 {
		t.Fatalf("order-run beads = %d, want only seed plus manual run", len(all))
	}
}

func TestOrderDispatchCachesAutoTrackingBeadCreatedAt(t *testing.T) {
	store := &countingListStore{Store: beads.NewMemStore()}
	now := time.Now()

	aa := []orders.Order{{
		Name:     "test-order",
		Trigger:  "cooldown",
		Interval: "1h",
		Exec:     "true",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, successfulExec, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	cityPath := t.TempDir()
	ad.dispatch(context.Background(), cityPath, now)
	all := trackingBeads(t, store, "order-run:test-order")
	if len(all) != 1 {
		t.Fatalf("order-run beads after first dispatch = %d, want 1", len(all))
	}

	store.reset()
	ad.dispatch(context.Background(), cityPath, now.Add(time.Second))
	if store.includeClosedLists != 0 {
		t.Fatalf("second dispatch performed %d closed-history reads, want cached tracking bead timestamp", store.includeClosedLists)
	}
	all = trackingBeads(t, store, "order-run:test-order")
	if len(all) != 1 {
		t.Fatalf("order-run beads after second dispatch = %d, want cached cooldown suppression", len(all))
	}
}

// --- exec order dispatch tests ---

func TestOrderDispatchExecDue(t *testing.T) {
	store := beads.NewMemStore()
	var rec memRecorder

	ran := false
	fakeExec := func(_ context.Context, _, _ string, _ []string) ([]byte, error) {
		ran = true
		return []byte("ok\n"), nil
	}

	aa := []orders.Order{{
		Name:     "wasteland-poll",
		Trigger:  "cooldown",
		Interval: "2m",
		Exec:     "$ORDER_DIR/scripts/poll.sh",
		Source:   "/city/orders/wasteland-poll.toml",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, &rec)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	ad.drain(context.Background())

	if !ran {
		t.Error("exec runner was not called")
	}

	// Check tracking bead exists with exec label.
	all := trackingBeads(t, store, "order-run:wasteland-poll")
	found := false
	hasExec := false
	for _, b := range all {
		for _, l := range b.Labels {
			if l == "order-run:wasteland-poll" {
				found = true
			}
			if l == "exec" {
				hasExec = true
			}
		}
	}
	if !found {
		t.Error("tracking bead missing order-run label")
	}
	if !hasExec {
		t.Error("tracking bead missing exec label")
	}

	// Check events.
	if !rec.hasType(events.OrderFired) {
		t.Error("missing order.fired event")
	}
	if !rec.hasType(events.OrderCompleted) {
		t.Error("missing order.completed event")
	}
}

func TestOrderDispatchExecFailure(t *testing.T) {
	store := beads.NewMemStore()
	var rec memRecorder
	var stderr bytes.Buffer
	tracking, err := store.Create(beads.Bead{
		Title:  "order:fail-exec",
		Labels: []string{"order-run:fail-exec", labelOrderTracking},
	})
	if err != nil {
		t.Fatal(err)
	}

	fakeExec := func(_ context.Context, _, _ string, _ []string) ([]byte, error) {
		return []byte("error output\n"), fmt.Errorf("exit status 1")
	}

	aa := []orders.Order{{
		Name:     "fail-exec",
		Trigger:  "cooldown",
		Interval: "2m",
		Exec:     "scripts/fail.sh",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, &rec)
	mad := ad.(*memoryOrderDispatcher)
	mad.stderr = &stderr

	logs := captureCmdOrderLogs(t, func() {
		mad.dispatchExec(context.Background(), orders.NewStore(beads.OrdersStore{Store: store}), execStoreTarget{ScopeRoot: t.TempDir()}, aa[0], t.TempDir(), tracking.ID, nil)
	})

	// Check tracking bead has exec-failed label.
	all := trackingBeads(t, store, "order-run:fail-exec")
	hasFailed := false
	for _, b := range all {
		for _, l := range b.Labels {
			if l == "exec-failed" {
				hasFailed = true
			}
		}
	}
	if !hasFailed {
		t.Error("tracking bead missing exec-failed label")
	}

	// Check order.failed event.
	if !rec.hasType(events.OrderFailed) {
		t.Error("missing order.failed event")
	}
	if !strings.Contains(logs, "order exec fail-exec failed") {
		t.Fatalf("logs = %q, want exec failure warning", logs)
	}
}

func TestOrderDispatchExecEnvFailureUsesEnvFailureLabel(t *testing.T) {
	clearAmbientPostgresEnv(t)
	t.Setenv("GC_BEADS", "bd")

	store := beads.NewMemStore()
	var rec memRecorder
	var stderr bytes.Buffer
	tracking, err := store.Create(beads.Bead{
		Title:  "order:pg-env",
		Labels: []string{"order-run:pg-env", labelOrderTracking},
	})
	if err != nil {
		t.Fatal(err)
	}

	cityDir := t.TempDir()
	writePGScopeFixture(t, cityDir, "")
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(`issue_prefix: city
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	a := orders.Order{Name: "pg-env", Trigger: "cooldown", Interval: "1m", Exec: "true"}
	ad := buildOrderDispatcherFromListExec([]orders.Order{a}, store, nil, successfulExec, &rec)
	mad := ad.(*memoryOrderDispatcher)
	mad.stderr = &stderr

	logs := captureCmdOrderLogs(t, func() {
		mad.dispatchExec(context.Background(), orders.NewStore(beads.OrdersStore{Store: store}), execStoreTarget{ScopeRoot: cityDir, ScopeKind: "city", Prefix: "ct"}, a, cityDir, tracking.ID, nil)
	})

	all := trackingBeads(t, store, "order-run:pg-env")
	if len(all) != 1 {
		t.Fatalf("tracking bead count = %d, want 1", len(all))
	}
	if !slicesContain(all[0].Labels, "exec-env-failed") {
		t.Fatalf("tracking bead labels = %v, want exec-env-failed", all[0].Labels)
	}
	if slicesContain(all[0].Labels, "exec-failed") {
		t.Fatalf("tracking bead labels = %v, want no exec-failed for env-build failure", all[0].Labels)
	}
	if !rec.hasType(events.OrderFailed) {
		t.Fatal("missing order.failed event")
	}
	combined := logs + "\n" + stderr.String()
	if !strings.Contains(combined, "order exec pg-env env failed") {
		t.Fatalf("logs = %q, want env failure warning", combined)
	}
}

func TestOrderDispatchExecFailureRedactsSecrets(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghs_order_secret")
	store := beads.NewMemStore()
	var rec memRecorder
	var stderr bytes.Buffer
	tracking, err := store.Create(beads.Bead{
		Title:  "order:leaky-exec",
		Labels: []string{"order-run:leaky-exec", labelOrderTracking},
	})
	if err != nil {
		t.Fatal(err)
	}

	fakeExec := func(_ context.Context, _, _ string, _ []string) ([]byte, error) {
		return []byte("GITHUB_TOKEN=ghs_order_secret\n--password hunter2\n"), fmt.Errorf("token=ghs_order_secret password=hunter2")
	}

	aa := []orders.Order{{
		Name:     "leaky-exec",
		Trigger:  "cooldown",
		Interval: "2m",
		Exec:     "scripts/fail.sh",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, &rec)
	mad := ad.(*memoryOrderDispatcher)
	mad.stderr = &stderr

	logs := captureCmdOrderLogs(t, func() {
		mad.dispatchExec(context.Background(), orders.NewStore(beads.OrdersStore{Store: store}), execStoreTarget{ScopeRoot: t.TempDir()}, aa[0], t.TempDir(), tracking.ID, nil)
	})

	combined := logs + "\n" + stderr.String()
	for _, secret := range []string{"ghs_order_secret", "hunter2"} {
		if strings.Contains(combined, secret) {
			t.Fatalf("order exec logs leaked %q:\n%s", secret, combined)
		}
	}
	if !strings.Contains(combined, "[redacted]") {
		t.Fatalf("order exec logs = %q, want redaction marker", combined)
	}
	for _, event := range rec.events {
		if strings.Contains(event.Message, "ghs_order_secret") || strings.Contains(event.Message, "hunter2") {
			t.Fatalf("order failed event leaked secret: %#v", event)
		}
	}
}

func TestOrderDispatchFormulaCookFailureLabelsTrackingBead(t *testing.T) {
	store := beads.NewMemStore()
	var rec memRecorder

	aa := []orders.Order{{
		Name:         "fail-formula",
		Trigger:      "cooldown",
		Interval:     "2m",
		Formula:      "missing-formula",
		FormulaLayer: sharedTestFormulaDir,
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	mad := ad.(*memoryOrderDispatcher)
	mad.rec = &rec

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	ad.drain(context.Background())

	all := trackingBeads(t, store, "order-run:fail-formula")
	hasFailed := false
	for _, b := range all {
		for _, l := range b.Labels {
			if l == "wisp-failed" {
				hasFailed = true
			}
		}
	}
	if !hasFailed {
		t.Error("tracking bead missing wisp-failed label after cook failure")
	}
	if !rec.hasType(events.OrderFailed) {
		t.Error("missing order.failed event")
	}
}

func TestOrderDispatchReportsAllMissingRequiredVarsAtOnce(t *testing.T) {
	store := beads.NewMemStore()
	var rec memRecorder

	formulaDir := t.TempDir()
	writeFile(t, filepath.Join(formulaDir, "order-required-vars.toml"), `
formula = "order-required-vars"
version = 1

[vars.target_id]
description = "Bead being worked on"
required = true

[vars.workspace]
description = "Workspace path"
required = true

[[steps]]
id = "do-work"
title = "Do work for {{target_id}}"
description = "Target: {{target_id}}, workspace: {{workspace}}"
`)

	aa := []orders.Order{{
		Name:         "fail-formula-vars",
		Trigger:      "cooldown",
		Interval:     "2m",
		Formula:      "order-required-vars",
		FormulaLayer: formulaDir,
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	mad := ad.(*memoryOrderDispatcher)
	mad.rec = &rec

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	ad.drain(context.Background())

	all := trackingBeads(t, store, "order-run:fail-formula-vars")
	hasFailed := false
	for _, b := range all {
		for _, l := range b.Labels {
			if l == "wisp-failed" {
				hasFailed = true
			}
		}
	}
	if !hasFailed {
		t.Error("tracking bead missing wisp-failed label after validation failure")
	}
	if !rec.hasType(events.OrderFailed) {
		t.Fatal("missing order.failed event")
	}

	var failedMessage string
	for _, event := range rec.events {
		if event.Type == events.OrderFailed && event.Subject == "fail-formula-vars" {
			failedMessage = event.Message
			break
		}
	}
	if failedMessage == "" {
		t.Fatal("missing order.failed message for formula validation failure")
	}
	if !strings.Contains(failedMessage, `variable "target_id" is required`) {
		t.Fatalf("order.failed message = %q, want missing target_id reported", failedMessage)
	}
	if !strings.Contains(failedMessage, `variable "workspace" is required`) {
		t.Fatalf("order.failed message = %q, want missing workspace reported", failedMessage)
	}
	if strings.Contains(failedMessage, "bead title contains unresolved variable(s)") {
		t.Fatalf("order.failed message = %q, want consolidated required-var validation instead of title-only failure", failedMessage)
	}
}

func TestOrderDispatchConditionTriggerEnvFailureRecordsOrderFailure(t *testing.T) {
	clearAmbientPostgresEnv(t)
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	writePGScopeFixture(t, cityDir, "")
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(`issue_prefix: city
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	store := beads.NewMemStore()
	var rec memRecorder
	var stderr bytes.Buffer
	a := orders.Order{Name: "pg-condition", Trigger: "condition", Check: "true", Exec: "true"}
	ad := buildOrderDispatcherFromListExec([]orders.Order{a}, store, nil, successfulExec, &rec)
	mad := ad.(*memoryOrderDispatcher)
	mad.stderr = &stderr

	now := time.Now()
	mad.dispatch(context.Background(), cityDir, now)
	mad.drain(context.Background())

	if !rec.hasType(events.OrderFailed) {
		t.Fatal("missing order.failed event for trigger env failure")
	}
	rec.mu.Lock()
	eventsSnapshot := append([]events.Event(nil), rec.events...)
	rec.mu.Unlock()
	if len(eventsSnapshot) != 1 {
		t.Fatalf("recorded events = %#v, want one order.failed event", eventsSnapshot)
	}
	if eventsSnapshot[0].Subject != "pg-condition" {
		t.Fatalf("order.failed subject = %q, want pg-condition", eventsSnapshot[0].Subject)
	}
	if !strings.Contains(eventsSnapshot[0].Message, "building trigger env") {
		t.Fatalf("order.failed message = %q, want trigger env context", eventsSnapshot[0].Message)
	}
	all := trackingBeads(t, store, "order-run:pg-condition")
	if len(all) != 1 {
		t.Fatalf("tracking beads = %#v, want one trigger env failure marker", all)
	}
	for _, want := range []string{labelOrderTracking, labelTriggerEnvFailed} {
		if !slicesContain(all[0].Labels, want) {
			t.Fatalf("tracking bead labels = %v, want %s", all[0].Labels, want)
		}
	}

	mad.dispatch(context.Background(), cityDir, now.Add(10*time.Second))
	mad.drain(context.Background())

	rec.mu.Lock()
	eventsSnapshot = append([]events.Event(nil), rec.events...)
	rec.mu.Unlock()
	failedEvents := 0
	for _, event := range eventsSnapshot {
		if event.Type == events.OrderFailed && event.Subject == "pg-condition" {
			failedEvents++
		}
	}
	if failedEvents != 1 {
		t.Fatalf("order.failed count after second dispatch = %d, want 1", failedEvents)
	}
	all = trackingBeads(t, store, "order-run:pg-condition")
	if len(all) != 1 {
		t.Fatalf("tracking beads after second dispatch = %#v, want original failure marker only", all)
	}
}

func TestOrderDispatchTriggerEnvFailuresRespectMaxDispatchesPerTick(t *testing.T) {
	clearAmbientPostgresEnv(t)
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	writePGScopeFixture(t, cityDir, "")
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(`issue_prefix: city
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	store := beads.NewMemStore()
	var rec memRecorder
	var aa []orders.Order
	for i := 0; i < 5; i++ {
		aa = append(aa, orders.Order{
			Name:    fmt.Sprintf("pg-condition-%d", i),
			Trigger: "condition",
			Check:   "true",
			Exec:    "true",
		})
	}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, successfulExec, &rec)
	mad := ad.(*memoryOrderDispatcher)
	mad.maxDispatchesPerTick = 2

	now := time.Date(2026, 5, 19, 2, 30, 0, 0, time.UTC)
	mad.dispatch(context.Background(), cityDir, now)
	mad.drain(context.Background())

	if got := countOrderTrackingRuns(t, store); got != 2 {
		t.Fatalf("tracking runs after first tick = %d, want 2", got)
	}
	if got := countRecordedOrderFailedEvents(t, &rec); got != 2 {
		t.Fatalf("order.failed events after first tick = %d, want 2", got)
	}

	mad.dispatch(context.Background(), cityDir, now.Add(time.Second))
	mad.drain(context.Background())
	if got := countOrderTrackingRuns(t, store); got != 4 {
		t.Fatalf("tracking runs after second tick = %d, want 4", got)
	}
	if got := countRecordedOrderFailedEvents(t, &rec); got != 4 {
		t.Fatalf("order.failed events after second tick = %d, want 4", got)
	}

	mad.dispatch(context.Background(), cityDir, now.Add(2*time.Second))
	mad.drain(context.Background())
	if got := countOrderTrackingRuns(t, store); got != 5 {
		t.Fatalf("tracking runs after third tick = %d, want 5", got)
	}
	if got := countRecordedOrderFailedEvents(t, &rec); got != 5 {
		t.Fatalf("order.failed events after third tick = %d, want 5", got)
	}
	for i := 0; i < 5; i++ {
		label := fmt.Sprintf("order-run:pg-condition-%d", i)
		if got := len(trackingBeads(t, store, label)); got != 1 {
			t.Fatalf("%s tracking markers = %d, want 1", label, got)
		}
	}
}

func countRecordedOrderFailedEvents(t *testing.T, rec *memRecorder) int {
	t.Helper()

	rec.mu.Lock()
	defer rec.mu.Unlock()

	count := 0
	for _, event := range rec.events {
		if event.Type == events.OrderFailed {
			count++
		}
	}
	return count
}

func TestOrderDispatchTriggerEnvFailureTrackingSuppressesNonConditionBeforeEvaluation(t *testing.T) {
	tests := []struct {
		name          string
		order         orders.Order
		ep            events.Provider
		lastRunOrder  string
		cursorOrder   string
		dispatchAfter time.Duration
	}{
		{
			name:          "cooldown",
			order:         orders.Order{Name: "pg-cooldown", Trigger: "cooldown", Interval: "1s", Exec: "true"},
			lastRunOrder:  "pg-cooldown",
			dispatchAfter: 2 * time.Second,
		},
		{
			name:        "event",
			order:       orders.Order{Name: "pg-event", Trigger: "event", On: events.BeadClosed, Exec: "true"},
			ep:          events.NewFake(),
			cursorOrder: "pg-event",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseStore := beads.NewMemStore()
			if _, err := baseStore.Create(beads.Bead{
				Title:     "order:" + tt.order.Name,
				Labels:    []string{"order-run:" + tt.order.Name, labelOrderTracking, labelTriggerEnvFailed},
				Ephemeral: true,
			}); err != nil {
				t.Fatal(err)
			}
			if fake, ok := tt.ep.(*events.Fake); ok {
				fake.Record(events.Event{Type: events.BeadClosed, Actor: "test"})
			}

			var rec memRecorder
			rec.Record(events.Event{Type: events.OrderFailed, Subject: tt.order.Name, Message: "building trigger env: previous failure"})
			var stderr bytes.Buffer
			store := triggerEvaluationFailStore{
				Store:        baseStore,
				lastRunOrder: tt.lastRunOrder,
				cursorOrder:  tt.cursorOrder,
			}
			ad := buildOrderDispatcherFromListExec([]orders.Order{tt.order}, store, tt.ep, successfulExec, &rec)
			mad := ad.(*memoryOrderDispatcher)
			mad.stderr = &stderr

			mad.dispatch(context.Background(), t.TempDir(), time.Now().Add(tt.dispatchAfter))
			mad.drain(context.Background())

			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want open trigger-env failure to suppress trigger evaluation", stderr.String())
			}
			all := trackingBeads(t, baseStore, "order-run:"+tt.order.Name)
			if len(all) != 1 {
				t.Fatalf("tracking beads after second dispatch = %#v, want original failure marker only", all)
			}
			failedEvents := 0
			rec.mu.Lock()
			eventsSnapshot := append([]events.Event(nil), rec.events...)
			rec.mu.Unlock()
			for _, event := range eventsSnapshot {
				if event.Type == events.OrderFailed && event.Subject == tt.order.Name {
					failedEvents++
				}
			}
			if failedEvents != 1 {
				t.Fatalf("order.failed count after second dispatch = %d, want 1", failedEvents)
			}
		})
	}
}

func TestRedactOrderEnvErrorUsesProcessEnv(t *testing.T) {
	t.Setenv("GC_ORDER_SECRET", "super-secret-token")

	got := redactOrderEnvError(fmt.Errorf("projection leaked super-secret-token"), os.Environ())
	if strings.Contains(got, "super-secret-token") {
		t.Fatalf("redacted error = %q, want secret removed", got)
	}
	if !strings.Contains(got, "projection leaked") {
		t.Fatalf("redacted error = %q, want non-secret context preserved", got)
	}
}

func TestOrderDispatchFormulaLabelFailureLabelsTrackingBead(t *testing.T) {
	store := beads.NewMemStore()
	var rec memRecorder
	var stderr bytes.Buffer

	aa := []orders.Order{{
		Name:         "fail-label",
		Trigger:      "cooldown",
		Interval:     "2m",
		Formula:      "test-formula",
		FormulaLayer: sharedTestFormulaDir,
	}}
	ad := buildOrderDispatcherFromList(aa, selectiveUpdateFailStore{Store: store}, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	mad := ad.(*memoryOrderDispatcher)
	mad.rec = &rec
	mad.stderr = &stderr

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	ad.drain(context.Background())

	all := trackingBeads(t, store, "order-run:fail-label")
	hasFailed := false
	for _, b := range all {
		for _, l := range b.Labels {
			if l == "wisp-failed" {
				hasFailed = true
			}
		}
	}
	if !hasFailed {
		t.Error("tracking bead missing wisp-failed label after label failure")
	}
	if !rec.hasType(events.OrderFailed) {
		t.Error("missing order.failed event")
	}
}

func TestOrderDispatchExecCooldown(t *testing.T) {
	store := beads.NewMemStore()

	// Seed a recent exec run.
	_, err := store.Create(beads.Bead{
		Title:  "order:wasteland-poll",
		Labels: []string{"order-run:wasteland-poll"},
	})
	if err != nil {
		t.Fatal(err)
	}

	ran := false
	fakeExec := func(_ context.Context, _, _ string, _ []string) ([]byte, error) {
		ran = true
		return nil, nil
	}

	aa := []orders.Order{{
		Name:     "wasteland-poll",
		Trigger:  "cooldown",
		Interval: "1h",
		Exec:     "scripts/poll.sh",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	ad.drain(context.Background())

	if ran {
		t.Error("exec should not have run — cooldown not elapsed")
	}
}

func TestOrderDispatchExecOrderDir(t *testing.T) {
	store := beads.NewMemStore()
	var gotEnv []string

	fakeExec := func(_ context.Context, _, _ string, env []string) ([]byte, error) {
		gotEnv = env
		return nil, nil
	}

	aa := []orders.Order{{
		Name:     "poll",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     "$ORDER_DIR/scripts/poll.sh",
		Source:   "/city/orders/poll.toml",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)

	ad.dispatch(context.Background(), "/city-root", time.Now())
	ad.drain(context.Background())

	foundDir := false
	foundCity := false
	foundCityPath := false
	foundRuntime := false
	for _, e := range gotEnv {
		if e == "ORDER_DIR=/city/orders" {
			foundDir = true
		}
		if e == "GC_CITY=/city-root" {
			foundCity = true
		}
		if e == "GC_CITY_PATH=/city-root" {
			foundCityPath = true
		}
		if e == "GC_CITY_RUNTIME_DIR=/city-root/.gc/runtime" {
			foundRuntime = true
		}
	}
	if !foundDir {
		t.Errorf("ORDER_DIR not set correctly, got env: %v", gotEnv)
	}
	if !foundCity {
		t.Errorf("GC_CITY not set correctly, got env: %v", gotEnv)
	}
	if !foundCityPath {
		t.Errorf("GC_CITY_PATH not set correctly, got env: %v", gotEnv)
	}
	if !foundRuntime {
		t.Errorf("GC_CITY_RUNTIME_DIR not set correctly, got env: %v", gotEnv)
	}
}

func TestOrderDispatchExecPackDir(t *testing.T) {
	store := beads.NewMemStore()
	var gotEnv []string

	fakeExec := func(_ context.Context, _, _ string, env []string) ([]byte, error) {
		gotEnv = env
		return nil, nil
	}

	aa := []orders.Order{{
		Name:         "gate-sweep",
		Trigger:      "cooldown",
		Interval:     "1m",
		Exec:         "$PACK_DIR/scripts/gate-sweep.sh",
		Source:       "/city/packs/maintenance/orders/gate-sweep.toml",
		FormulaLayer: "/city/packs/maintenance/formulas",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)

	ad.dispatch(context.Background(), "/city-root", time.Now())
	ad.drain(context.Background())

	foundPackDir := false
	foundAutoDir := false
	foundPackName := false
	foundPackState := false
	for _, e := range gotEnv {
		if e == "PACK_DIR=/city/packs/maintenance" {
			foundPackDir = true
		}
		if e == "ORDER_DIR=/city/packs/maintenance/orders" {
			foundAutoDir = true
		}
		if e == "GC_PACK_NAME=maintenance" {
			foundPackName = true
		}
		if e == "GC_PACK_STATE_DIR=/city-root/.gc/runtime/packs/maintenance" {
			foundPackState = true
		}
	}
	if !foundPackDir {
		t.Errorf("PACK_DIR not set correctly, got env: %v", gotEnv)
	}
	if !foundAutoDir {
		t.Errorf("ORDER_DIR not set correctly, got env: %v", gotEnv)
	}
	if !foundPackName {
		t.Errorf("GC_PACK_NAME not set correctly, got env: %v", gotEnv)
	}
	if !foundPackState {
		t.Errorf("GC_PACK_STATE_DIR not set correctly, got env: %v", gotEnv)
	}
}

func TestOrderDispatchExecManagedDoltPreservesOrderPackStateDir(t *testing.T) {
	store := beads.NewMemStore()
	cityDir := t.TempDir()
	dataDir := filepath.Join(cityDir, ".beads", "dolt")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cityDir, "city.toml"), `[workspace]
name = "test-city"
prefix = "ct"
`)
	writeFile(t, filepath.Join(cityDir, ".beads", "config.yaml"), strings.Join([]string{
		"issue_prefix: ct",
		"gc.endpoint_origin: managed_city",
		"gc.endpoint_status: verified",
		"dolt.auto-start: false",
		"",
	}, "\n"))
	writeFile(t, filepath.Join(cityDir, ".beads", "metadata.json"), `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"ct"}`)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Fatalf("Close listener: %v", err)
		}
	}()
	stateDir := filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(stateDir, "dolt-state.json"), fmt.Sprintf(
		`{"running":true,"pid":%d,"port":%d,"data_dir":%q}`,
		os.Getpid(),
		listener.Addr().(*net.TCPAddr).Port,
		dataDir,
	))

	envCh := make(chan []string, 1)
	fakeExec := func(_ context.Context, _, _ string, env []string) ([]byte, error) {
		envCh <- env
		return nil, nil
	}
	aa := []orders.Order{{
		Name:         "gate-sweep",
		Trigger:      "cooldown",
		Interval:     "1m",
		Exec:         "$PACK_DIR/scripts/gate-sweep.sh",
		Source:       filepath.Join(cityDir, "packs", "maintenance", "orders", "gate-sweep.toml"),
		FormulaLayer: filepath.Join(cityDir, "packs", "maintenance", "formulas"),
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)
	ad.dispatch(context.Background(), cityDir, time.Now())

	got := orderDispatchTestEnv(t, envCh)
	wantPackState := filepath.Join(cityDir, ".gc", "runtime", "packs", "maintenance")
	if got["GC_PACK_STATE_DIR"] != wantPackState {
		t.Fatalf("GC_PACK_STATE_DIR = %q, want order pack state %q; env=%v", got["GC_PACK_STATE_DIR"], wantPackState, got)
	}
	wantDoltState := filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "dolt-state.json")
	if got["GC_DOLT_STATE_FILE"] != wantDoltState {
		t.Fatalf("GC_DOLT_STATE_FILE = %q, want %q; env=%v", got["GC_DOLT_STATE_FILE"], wantDoltState, got)
	}
}

func TestOrderDispatchExecManagedDoltUsesTrustedCityRuntimeDir(t *testing.T) {
	store := beads.NewMemStore()
	cityDir := t.TempDir()
	dataDir := filepath.Join(cityDir, ".beads", "dolt")
	customRuntimeDir := filepath.Join(t.TempDir(), "runtime-root")
	packStateDir := filepath.Join(customRuntimeDir, "packs", "dolt")
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_CITY_RUNTIME_DIR", customRuntimeDir)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(packStateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cityDir, "city.toml"), `[workspace]
name = "test-city"
prefix = "ct"

[beads]
provider = "bd"
`)
	writeFile(t, filepath.Join(cityDir, ".beads", "config.yaml"), strings.Join([]string{
		"issue_prefix: ct",
		"gc.endpoint_origin: managed_city",
		"gc.endpoint_status: verified",
		"dolt.auto-start: false",
		"",
	}, "\n"))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Fatalf("Close listener: %v", err)
		}
	}()
	writeFile(t, filepath.Join(packStateDir, "dolt-state.json"), fmt.Sprintf(
		`{"running":true,"pid":%d,"port":%d,"data_dir":%q}`,
		os.Getpid(),
		listener.Addr().(*net.TCPAddr).Port,
		dataDir,
	))

	envCh := make(chan []string, 1)
	fakeExec := func(_ context.Context, _, _ string, env []string) ([]byte, error) {
		envCh <- env
		return nil, nil
	}
	aa := []orders.Order{{
		Name:     "dolt-test-cooldown",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     "echo test",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)
	ad.dispatch(context.Background(), cityDir, time.Now())

	got := orderDispatchTestEnv(t, envCh)
	if got["GC_CITY_RUNTIME_DIR"] != customRuntimeDir {
		t.Fatalf("GC_CITY_RUNTIME_DIR = %q, want %q; env=%v", got["GC_CITY_RUNTIME_DIR"], customRuntimeDir, got)
	}
	wantControlTrace := filepath.Join(customRuntimeDir, "control-dispatcher-trace.log")
	if got["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"] != wantControlTrace {
		t.Fatalf("GC_CONTROL_DISPATCHER_TRACE_DEFAULT = %q, want %q; env=%v", got["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"], wantControlTrace, got)
	}
	wantStateFile := filepath.Join(packStateDir, "dolt-state.json")
	if got["GC_DOLT_STATE_FILE"] != wantStateFile {
		t.Fatalf("GC_DOLT_STATE_FILE = %q, want %q; env=%v", got["GC_DOLT_STATE_FILE"], wantStateFile, got)
	}
}

func TestOrderDispatchExecManagedDoltCoercesInCityRuntimeDirForControlTraceDefault(t *testing.T) {
	store := beads.NewMemStore()
	cityDir := t.TempDir()
	dataDir := filepath.Join(cityDir, ".beads", "dolt")
	unsafeRuntimeDir := filepath.Join(cityDir, "runtime-outside-gc")
	packStateDir := filepath.Join(unsafeRuntimeDir, "packs", "dolt")
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_CITY_RUNTIME_DIR", unsafeRuntimeDir)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(packStateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cityDir, "city.toml"), `[workspace]
name = "test-city"
prefix = "ct"

[beads]
provider = "bd"
`)
	writeFile(t, filepath.Join(cityDir, ".beads", "config.yaml"), strings.Join([]string{
		"issue_prefix: ct",
		"gc.endpoint_origin: managed_city",
		"gc.endpoint_status: verified",
		"dolt.auto-start: false",
		"",
	}, "\n"))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Fatalf("Close listener: %v", err)
		}
	}()
	writeFile(t, filepath.Join(packStateDir, "dolt-state.json"), fmt.Sprintf(
		`{"running":true,"pid":%d,"port":%d,"data_dir":%q}`,
		os.Getpid(),
		listener.Addr().(*net.TCPAddr).Port,
		dataDir,
	))

	envCh := make(chan []string, 1)
	fakeExec := func(_ context.Context, _, _ string, env []string) ([]byte, error) {
		envCh <- env
		return nil, nil
	}
	aa := []orders.Order{{
		Name:     "dolt-test-cooldown",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     "echo test",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)
	ad.dispatch(context.Background(), cityDir, time.Now())

	got := orderDispatchTestEnv(t, envCh)
	if got["GC_CITY_RUNTIME_DIR"] != unsafeRuntimeDir {
		t.Fatalf("GC_CITY_RUNTIME_DIR = %q, want %q; env=%v", got["GC_CITY_RUNTIME_DIR"], unsafeRuntimeDir, got)
	}
	wantControlTrace := filepath.Join(cityDir, ".gc", "runtime", "control-dispatcher-trace.log")
	if got["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"] != wantControlTrace {
		t.Fatalf("GC_CONTROL_DISPATCHER_TRACE_DEFAULT = %q, want %q; env=%v", got["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"], wantControlTrace, got)
	}
}

func TestOrderDispatchExecPackDirEmpty(t *testing.T) {
	// When FormulaLayer is empty, PACK_DIR should not be in env.
	store := beads.NewMemStore()
	var gotEnv []string

	fakeExec := func(_ context.Context, _, _ string, env []string) ([]byte, error) {
		gotEnv = env
		return nil, nil
	}

	aa := []orders.Order{{
		Name:     "no-layer",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     "scripts/test.sh",
		Source:   "/city/orders/no-layer.toml",
		// FormulaLayer intentionally empty.
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)

	ad.dispatch(context.Background(), "/city-root", time.Now())
	ad.drain(context.Background())

	for _, e := range gotEnv {
		if strings.HasPrefix(e, "PACK_DIR=") {
			t.Errorf("PACK_DIR should not be set when FormulaLayer is empty, got: %s", e)
		}
		if strings.HasPrefix(e, "GC_PACK_STATE_DIR=") {
			t.Errorf("GC_PACK_STATE_DIR should not be set when FormulaLayer is empty, got: %s", e)
		}
	}
}

func TestOrderDispatchExecRigUsesScopedWorkdirAndStoreEnv(t *testing.T) {
	store := beads.NewMemStore()
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var gotDir string
	var gotEnv []string

	fakeExec := func(_ context.Context, _, dir string, env []string) ([]byte, error) {
		gotDir = dir
		gotEnv = env
		return nil, nil
	}

	aa := []orders.Order{{
		Name:     "poll",
		Rig:      "frontend",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     "$ORDER_DIR/scripts/poll.sh",
		Source:   "/city/orders/poll.toml",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)
	mad := ad.(*memoryOrderDispatcher)
	mad.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city", Prefix: "ct"},
		Rigs: []config.Rig{{
			Name:   "frontend",
			Path:   "frontend",
			Prefix: "fe",
		}},
	}

	ad.dispatch(context.Background(), cityDir, time.Now())
	ad.drain(context.Background())

	if gotDir != rigDir {
		t.Fatalf("exec dir = %q, want %q", gotDir, rigDir)
	}
	checks := map[string]string{
		"GC_CITY":         cityDir,
		"GC_CITY_PATH":    cityDir,
		"BEADS_DIR":       filepath.Join(rigDir, ".beads"),
		"GC_STORE_ROOT":   rigDir,
		"GC_STORE_SCOPE":  "rig",
		"GC_BEADS_PREFIX": "fe",
		"GC_RIG":          "frontend",
		"GC_RIG_ROOT":     rigDir,
		"ORDER_DIR":       "/city/orders",
	}
	for key, want := range checks {
		entry := key + "=" + want
		if !slicesContain(gotEnv, entry) {
			t.Fatalf("missing %s in env: %v", entry, gotEnv)
		}
	}
}

func TestOrderDispatchExecMarksExternalDoltTargetForManagedLocalOnlyOrders(t *testing.T) {
	store := beads.NewMemStore()
	cityDir := t.TempDir()
	t.Setenv("GC_PACK_STATE_DIR", filepath.Join(t.TempDir(), "poison-pack-state"))
	t.Setenv("GC_DOLT_DATA_DIR", filepath.Join(t.TempDir(), "poison-dolt-data"))
	t.Setenv("GC_DOLT_CONFIG_FILE", filepath.Join(t.TempDir(), "poison-dolt-config.yaml"))
	t.Setenv("GC_DOLT_STATE_FILE", filepath.Join(t.TempDir(), "poison-state.json"))
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cityDir, ".beads", "config.yaml"), strings.Join([]string{
		"issue_prefix: ct",
		"gc.endpoint_origin: city_canonical",
		"gc.endpoint_status: verified",
		"dolt.auto-start: false",
		"dolt.host: external.example.internal",
		"dolt.port: 4406",
		"",
	}, "\n"))

	envCh := make(chan []string, 1)
	fakeExec := func(_ context.Context, _, _ string, env []string) ([]byte, error) {
		envCh <- env
		return nil, nil
	}
	aa := []orders.Order{{
		Name:     "dolt-test-cooldown",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     "echo test",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)
	ad.dispatch(context.Background(), cityDir, time.Now())

	got := orderDispatchTestEnv(t, envCh)
	externalRoot := filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "external-target")
	checks := map[string]string{
		"GC_DOLT_MANAGED_LOCAL": "0",
		"GC_DOLT_HOST":          "external.example.internal",
		"GC_DOLT_PORT":          "4406",
		"GC_DOLT_DATA_DIR":      externalRoot,
		"GC_DOLT_CONFIG_FILE":   filepath.Join(externalRoot, "dolt-config.yaml"),
		"GC_DOLT_STATE_FILE":    filepath.Join(externalRoot, "dolt-state.json"),
	}
	for key, want := range checks {
		if got[key] != want {
			t.Fatalf("%s = %q, want %q; env=%v", key, got[key], want, got)
		}
	}
}

func TestOrderDispatchExecPropagatesManagedDoltLayout(t *testing.T) {
	store := beads.NewMemStore()
	cityDir := normalizePathForCompare(t.TempDir())
	dataDir := normalizePathForCompare(filepath.Join(t.TempDir(), "managed-dolt"))
	configFile := filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "dolt-config.yaml")
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cityDir, ".beads", "config.yaml"), strings.Join([]string{
		"issue_prefix: ct",
		"gc.endpoint_origin: managed_city",
		"gc.endpoint_status: verified",
		"dolt.auto-start: false",
		"",
	}, "\n"))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Fatalf("Close listener: %v", err)
		}
	}()
	port := fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
	stateDir := filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(stateDir, "dolt-state.json"), fmt.Sprintf(
		`{"running":true,"pid":%d,"port":%s,"data_dir":%q}`,
		os.Getpid(),
		port,
		dataDir,
	))
	t.Setenv("GC_DOLT_DATA_DIR", filepath.Join(t.TempDir(), "poison-dolt-data"))
	t.Setenv("GC_DOLT_CONFIG_FILE", filepath.Join(t.TempDir(), "poison-dolt-config.yaml"))
	t.Setenv("GC_PACK_STATE_DIR", filepath.Join(t.TempDir(), "poison-pack-state"))
	t.Setenv("GC_DOLT_STATE_FILE", filepath.Join(t.TempDir(), "poison-state.json"))

	envCh := make(chan []string, 1)
	fakeExec := func(_ context.Context, _, _ string, env []string) ([]byte, error) {
		envCh <- env
		return nil, nil
	}
	aa := []orders.Order{{
		Name:     "dolt-test-cooldown",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     "echo test",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)
	ad.dispatch(context.Background(), cityDir, time.Now())

	got := orderDispatchTestEnv(t, envCh)
	checks := map[string]string{
		"GC_DOLT_MANAGED_LOCAL": "1",
		"GC_DOLT_PORT":          port,
		"GC_DOLT_DATA_DIR":      dataDir,
		"GC_DOLT_CONFIG_FILE":   configFile,
	}
	for key, want := range checks {
		if got[key] != want {
			t.Fatalf("%s = %q, want %q; env=%v", key, got[key], want, got)
		}
	}
}

func TestOrderDispatchExecPropagatesLegacyManagedDoltDataDir(t *testing.T) {
	store := beads.NewMemStore()
	cityDir := normalizePathForCompare(t.TempDir())
	dataDir := normalizePathForCompare(filepath.Join(cityDir, ".gc", "dolt-data"))
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cityDir, ".beads", "config.yaml"), strings.Join([]string{
		"issue_prefix: ct",
		"gc.endpoint_origin: managed_city",
		"gc.endpoint_status: verified",
		"dolt.auto-start: false",
		"",
	}, "\n"))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Fatalf("Close listener: %v", err)
		}
	}()
	port := fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
	stateDir := filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(stateDir, "dolt-state.json"), fmt.Sprintf(
		`{"running":true,"pid":%d,"port":%s,"data_dir":%q}`,
		os.Getpid(),
		port,
		dataDir,
	))

	envCh := make(chan []string, 1)
	fakeExec := func(_ context.Context, _, _ string, env []string) ([]byte, error) {
		envCh <- env
		return nil, nil
	}
	aa := []orders.Order{{
		Name:     "dolt-test-cooldown",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     "echo test",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)
	ad.dispatch(context.Background(), cityDir, time.Now())

	got := orderDispatchTestEnv(t, envCh)
	checks := map[string]string{
		"GC_DOLT_MANAGED_LOCAL": "1",
		"GC_DOLT_PORT":          port,
		"GC_DOLT_DATA_DIR":      dataDir,
	}
	for key, want := range checks {
		if got[key] != want {
			t.Fatalf("%s = %q, want %q; env=%v", key, got[key], want, got)
		}
	}
}

func TestOrderDispatchExecIgnoresPublishedRunningDataDirWithUnreachablePort(t *testing.T) {
	store := beads.NewMemStore()
	cityDir := t.TempDir()
	staleDataDir := filepath.Join(t.TempDir(), "stale-published-dolt")
	defaultDataDir := filepath.Join(cityDir, ".beads", "dolt")
	if err := os.MkdirAll(defaultDataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(staleDataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cityDir, ".beads", "config.yaml"), strings.Join([]string{
		"issue_prefix: ct",
		"gc.endpoint_origin: managed_city",
		"gc.endpoint_status: verified",
		"dolt.auto-start: false",
		"",
	}, "\n"))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	port := fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
	if err := listener.Close(); err != nil {
		t.Fatalf("Close listener: %v", err)
	}
	stateDir := filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(stateDir, "dolt-state.json"), fmt.Sprintf(
		`{"running":true,"pid":%d,"port":%s,"data_dir":%q}`,
		os.Getpid(),
		port,
		staleDataDir,
	))

	envCh := make(chan []string, 1)
	fakeExec := func(_ context.Context, _, _ string, env []string) ([]byte, error) {
		envCh <- env
		return nil, nil
	}
	aa := []orders.Order{{
		Name:     "dolt-test-cooldown",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     "echo test",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)
	ad.dispatch(context.Background(), cityDir, time.Now())

	got := orderDispatchTestEnv(t, envCh)
	if got["GC_DOLT_DATA_DIR"] != defaultDataDir {
		t.Fatalf("GC_DOLT_DATA_DIR = %q, want default %q; env=%v", got["GC_DOLT_DATA_DIR"], defaultDataDir, got)
	}
}

func TestOrderExecManagedDoltFallbackSkipsInheritedExternalCity(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cityDir, ".beads", "config.yaml"), strings.Join([]string{
		"issue_prefix: ct",
		"gc.endpoint_origin: city_canonical",
		"gc.endpoint_status: verified",
		"dolt.host: external.example.internal",
		"dolt.port: 4406",
		"",
	}, "\n"))
	writeFile(t, filepath.Join(rigDir, ".beads", "config.yaml"), strings.Join([]string{
		"issue_prefix: fe",
		"gc.endpoint_origin: inherited_city",
		"gc.endpoint_status: verified",
		"dolt.host: external.example.internal",
		"dolt.port: 4406",
		"",
	}, "\n"))

	env := map[string]string{
		"GC_DOLT_HOST": "external.example.internal",
		"GC_DOLT_PORT": "4406",
	}
	if applyOrderExecManagedDoltFallback(cityDir, rigDir, env, fmt.Errorf("simulated target error")) {
		t.Fatal("managed fallback applied to inherited external city endpoint")
	}
	if env["GC_DOLT_MANAGED_LOCAL"] == "1" {
		t.Fatalf("GC_DOLT_MANAGED_LOCAL = %q, want not managed-local; env=%v", env["GC_DOLT_MANAGED_LOCAL"], env)
	}
}

func TestApplyOrderExecCanonicalDoltEnvClearsProjectedPasswordForExplicitRig(t *testing.T) {
	t.Setenv("GC_DOLT_HOST", "")
	t.Setenv("GC_DOLT_PORT", "")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")
	t.Setenv("BEADS_DOLT_PASSWORD", "")

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cityDir, ".beads", "config.yaml"), strings.Join([]string{
		"issue_prefix: ct",
		"gc.endpoint_origin: city_canonical",
		"gc.endpoint_status: verified",
		"dolt.host: city-db.example.com",
		"dolt.port: 4406",
		"dolt.user: city-user",
		"",
	}, "\n"))
	writeFile(t, filepath.Join(cityDir, ".beads", ".env"), "BEADS_DOLT_PASSWORD=city-secret\n")
	writeFile(t, filepath.Join(rigDir, ".beads", "config.yaml"), strings.Join([]string{
		"issue_prefix: fe",
		"gc.endpoint_origin: explicit",
		"gc.endpoint_status: verified",
		"dolt.host: rig-db.example.com",
		"dolt.port: 5506",
		"dolt.user: rig-user",
		"",
	}, "\n"))
	credentialsPath := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(credentialsPath, []byte("[rig-db.example.com:5506]\npassword=rig-credentials-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEADS_CREDENTIALS_FILE", credentialsPath)

	env := map[string]string{
		"GC_DOLT_HOST":           "city-db.example.com",
		"GC_DOLT_PORT":           "4406",
		"GC_DOLT_PASSWORD":       "city-secret",
		"BEADS_DOLT_PASSWORD":    "city-secret",
		"BEADS_CREDENTIALS_FILE": credentialsPath,
	}
	applyOrderExecCanonicalDoltEnv(cityDir, rigDir, env)
	if got := env["GC_DOLT_HOST"]; got != "rig-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want %q", got, "rig-db.example.com")
	}
	if got := env["GC_DOLT_PASSWORD"]; got != "rig-credentials-secret" {
		t.Fatalf("GC_DOLT_PASSWORD = %q, want %q", got, "rig-credentials-secret")
	}
	if got := env["BEADS_DOLT_PASSWORD"]; got != "rig-credentials-secret" {
		t.Fatalf("BEADS_DOLT_PASSWORD = %q, want %q", got, "rig-credentials-secret")
	}
}

func TestOrderDispatchExecTimeout(t *testing.T) {
	store := beads.NewMemStore()
	var rec memRecorder

	fakeExec := func(ctx context.Context, _, _ string, _ []string) ([]byte, error) {
		// Simulate a command that blocks until context is canceled.
		<-ctx.Done()
		return nil, ctx.Err()
	}

	aa := []orders.Order{{
		Name:     "slow-exec",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     "scripts/slow.sh",
		Timeout:  "100ms",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, &rec)

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	ad.drain(context.Background())

	// Should have failed due to timeout.
	if !rec.hasType(events.OrderFailed) {
		t.Error("missing order.failed event after timeout")
	}
}

func TestShellExecRunnerDoesNotStartWhenContextCanceled(t *testing.T) {
	workDir := t.TempDir()
	markerPath := filepath.Join(workDir, "started")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	command := fmt.Sprintf("printf started > %q; sleep 10", markerPath)
	_, err := shellExecRunner(ctx, command, workDir, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("shellExecRunner() error = %v, want %v", err, context.Canceled)
	}
	if _, err := os.Stat(markerPath); err == nil {
		t.Fatalf("shellExecRunner started command after context cancellation; marker exists at %s", markerPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat marker %s: %v", markerPath, err)
	}
}

func TestShellExecRunnerKillsProcessGroupOnTimeout(t *testing.T) {
	processgrouptest.RequireRealProcessSignals(t)

	workDir := t.TempDir()
	heartbeatPath := filepath.Join(workDir, "heartbeat")
	childPIDPath := filepath.Join(workDir, "child.pid")
	t.Cleanup(func() { processgrouptest.KillFromPIDFile(t, childPIDPath) })
	oldSignalGrace := shellExecSignalGrace
	shellExecSignalGrace = 100 * time.Millisecond
	t.Cleanup(func() { shellExecSignalGrace = oldSignalGrace })
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	command := fmt.Sprintf("sh -c 'printf \"%%s\\n\" \"$$\" > %q; trap \"\" TERM; while :; do printf . >> %q; sleep 0.05; done' & wait", childPIDPath, heartbeatPath)
	_, err := shellExecRunner(ctx, command, workDir, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shellExecRunner() error = %v, want %v", err, context.DeadlineExceeded)
	}

	size := processgrouptest.WaitForFileSize(t, heartbeatPath)
	processgrouptest.AssertFileSizeStable(t, heartbeatPath, size, 300*time.Millisecond)
}

func TestShellExecRunnerKillsProcessGroupAfterWaitDelay(t *testing.T) {
	processgrouptest.RequireRealProcessSignals(t)

	workDir := t.TempDir()
	heartbeatPath := filepath.Join(workDir, "heartbeat")
	childPIDPath := filepath.Join(workDir, "child.pid")
	t.Cleanup(func() { processgrouptest.KillFromPIDFile(t, childPIDPath) })
	oldWaitDelay := shellExecPostCancelWaitDelay
	oldSignalGrace := shellExecSignalGrace
	shellExecPostCancelWaitDelay = 100 * time.Millisecond
	shellExecSignalGrace = 100 * time.Millisecond
	t.Cleanup(func() {
		shellExecPostCancelWaitDelay = oldWaitDelay
		shellExecSignalGrace = oldSignalGrace
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	command := fmt.Sprintf("sh -c 'printf \"%%s\\n\" \"$$\" > %q; trap \"\" TERM; while :; do printf . >> %q; sleep 0.05; done' &", childPIDPath, heartbeatPath)
	_, err := shellExecRunner(ctx, command, workDir, nil)
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("shellExecRunner() error = %v, want %v", err, exec.ErrWaitDelay)
	}

	size := processgrouptest.WaitForFileSize(t, heartbeatPath)
	processgrouptest.AssertFileSizeStable(t, heartbeatPath, size, 300*time.Millisecond)
}

func TestShellExecRunnerReturnsPartialOutputOnTimeout(t *testing.T) {
	workDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	output, err := shellExecRunner(ctx, "while :; do printf .; sleep 0.01; done", workDir, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shellExecRunner() error = %v, want %v", err, context.DeadlineExceeded)
	}
	if !bytes.Contains(output, []byte(".")) {
		t.Fatalf("shellExecRunner() output = %q, want partial command output", string(output))
	}
}

func TestEffectiveTimeout(t *testing.T) {
	tests := []struct {
		name       string
		a          orders.Order
		maxTimeout time.Duration
		want       time.Duration
	}{
		{"exec default", orders.Order{Exec: "x.sh"}, 0, 300 * time.Second},
		{"formula default", orders.Order{Formula: "mol-x"}, 0, 30 * time.Second},
		{"custom timeout", orders.Order{Exec: "x.sh", Timeout: "90s"}, 0, 90 * time.Second},
		{"capped by max", orders.Order{Exec: "x.sh", Timeout: "120s"}, 60 * time.Second, 60 * time.Second},
		{"not capped under max", orders.Order{Exec: "x.sh", Timeout: "30s"}, 60 * time.Second, 30 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := effectiveTimeout(tt.a, tt.maxTimeout)
			if got != tt.want {
				t.Errorf("effectiveTimeout() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- suspended rig tests ---

func TestOrderDispatchSkipsSuspendedRig(t *testing.T) {
	store := beads.NewMemStore()

	aa := []orders.Order{{
		Name:         "rig-order",
		Trigger:      "cooldown",
		Interval:     "1m",
		Formula:      "test-formula",
		Rig:          "demo",
		FormulaLayer: sharedTestFormulaDir,
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	// Mark the rig as suspended.
	mad := ad.(*memoryOrderDispatcher)
	mad.cfg = &config.City{
		Rigs: []config.Rig{{Name: "demo", Path: "/tmp/demo", SuspendedOnStart: true}},
	}

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	ad.drain(context.Background())

	// No tracking bead should be created for a suspended rig.
	all := trackingBeads(t, store, "order-run:rig-order:rig:demo")
	if len(all) != 0 {
		t.Errorf("expected 0 tracking beads for suspended rig, got %d", len(all))
	}
}

func TestOrderDispatchSkipsSuspendedRigQualifiedPool(t *testing.T) {
	store := beads.NewMemStore()

	// City-level order with a qualified pool targeting a suspended rig.
	aa := []orders.Order{{
		Name:         "city-order",
		Trigger:      "cooldown",
		Interval:     "1m",
		Formula:      "test-formula",
		Pool:         "demo/polecat",
		FormulaLayer: sharedTestFormulaDir,
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	mad := ad.(*memoryOrderDispatcher)
	mad.cfg = &config.City{
		Rigs: []config.Rig{{Name: "demo", Path: "/tmp/demo", SuspendedOnStart: true}},
	}

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	ad.drain(context.Background())

	all := trackingBeads(t, store, "order-run:city-order")
	if len(all) != 0 {
		t.Errorf("expected 0 tracking beads for suspended rig pool, got %d", len(all))
	}
}

func TestOrderDispatchAllowsNonSuspendedRig(t *testing.T) {
	store := beads.NewMemStore()

	aa := []orders.Order{{
		Name:         "rig-order",
		Trigger:      "cooldown",
		Interval:     "1m",
		Formula:      "test-formula",
		Rig:          "demo",
		FormulaLayer: sharedTestFormulaDir,
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	// Rig exists but is NOT suspended.
	mad := ad.(*memoryOrderDispatcher)
	mad.cfg = &config.City{
		Rigs: []config.Rig{{Name: "demo", Path: "/tmp/demo", Suspended: false}},
	}

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	ad.drain(context.Background())

	all := trackingBeads(t, store, "order-run:rig-order:rig:demo")
	if len(all) == 0 {
		t.Error("expected tracking bead for non-suspended rig")
	}
}

func TestOrderDispatchSkipsCitySuspended(t *testing.T) {
	store := beads.NewMemStore()

	aa := []orders.Order{{
		Name:         "city-order",
		Trigger:      "cooldown",
		Interval:     "1m",
		Formula:      "test-formula",
		Pool:         "polecat",
		FormulaLayer: sharedTestFormulaDir,
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	// Suspend the entire workspace.
	mad := ad.(*memoryOrderDispatcher)
	mad.cfg = &config.City{
		Workspace: config.Workspace{SuspendedOnStart: true},
	}

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	ad.drain(context.Background())

	all := trackingBeads(t, store, "order-run:city-order")
	if len(all) != 0 {
		t.Errorf("expected 0 tracking beads for suspended city, got %d", len(all))
	}
}

func TestOrderDispatchSkipsSuspendedRigExec(t *testing.T) {
	store := beads.NewMemStore()

	aa := []orders.Order{{
		Name:     "exec-order",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     "echo hello",
		Rig:      "demo",
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	mad := ad.(*memoryOrderDispatcher)
	mad.cfg = &config.City{
		Rigs: []config.Rig{{Name: "demo", Path: "/tmp/demo", SuspendedOnStart: true}},
	}

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	ad.drain(context.Background())

	all := trackingBeads(t, store, "order-run:exec-order:rig:demo")
	if len(all) != 0 {
		t.Errorf("expected 0 tracking beads for exec order on suspended rig, got %d", len(all))
	}
}

func TestOrderRigSuspended(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "active", Path: "/tmp/active", Suspended: false},
			{Name: "frozen", Path: "/tmp/frozen", SuspendedOnStart: true},
		},
	}
	m := &memoryOrderDispatcher{cfg: cfg}

	tests := []struct {
		name string
		a    orders.Order
		want bool
	}{
		{"rig-scoped suspended", orders.Order{Rig: "frozen"}, true},
		{"rig-scoped active", orders.Order{Rig: "active"}, false},
		{"rig-scoped unknown", orders.Order{Rig: "unknown"}, false},
		{"qualified pool suspended", orders.Order{Pool: "frozen/polecat"}, true},
		{"qualified pool active", orders.Order{Pool: "active/polecat"}, false},
		{"unqualified pool", orders.Order{Pool: "polecat"}, false},
		{"cross-rig qualified pool", orders.Order{Rig: "active", Pool: "frozen/polecat"}, true},
		{"no rig no pool", orders.Order{}, false},
		{"nil cfg", orders.Order{Rig: "frozen"}, false}, // handled separately
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := m
			if tt.name == "nil cfg" {
				target = &memoryOrderDispatcher{}
			}
			if got := target.orderRigSuspended(tt.a); got != tt.want {
				t.Errorf("orderRigSuspended() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOrderRigSuspendedFallsBackToOrderRigOnPoolResolutionError(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "frozen", Path: "/tmp/frozen", SuspendedOnStart: true},
		},
		Agents: []config.Agent{
			{Name: "dog", Dir: "frozen", BindingName: "alpha"},
			{Name: "dog", Dir: "frozen", BindingName: "beta"},
		},
	}
	m := &memoryOrderDispatcher{cfg: cfg}

	if got := m.orderRigSuspended(orders.Order{Rig: "frozen", Pool: "dog"}); !got {
		t.Fatal("orderRigSuspended() = false, want true for suspended rig when pool resolution fails")
	}
}

// --- orphaned tracking bead sweep tests (#520) ---

func TestSweepOrphanedOrderTracking_ClosesOpenTrackingBeads(t *testing.T) {
	store := beads.NewMemStore()

	// Create some open ephemeral tracking beads (simulating goroutines killed on restart).
	for _, name := range []string{"dolt-health", "gate-sweep", "beads-health"} {
		_, err := store.Create(beads.Bead{
			Title:     "order:" + name,
			Labels:    []string{"order-run:" + name, labelOrderTracking},
			Ephemeral: true,
		})
		if err != nil {
			t.Fatalf("Create(%s): %v", name, err)
		}
	}
	_, err := store.Create(beads.Bead{
		Title:  "order:legacy-issues-tier",
		Labels: []string{"order-run:legacy-issues-tier", labelOrderTracking},
	})
	if err != nil {
		t.Fatalf("Create(legacy-issues-tier): %v", err)
	}

	// Create one that's already closed (should be left alone).
	b, err := store.Create(beads.Bead{
		Title:     "order:old-sweep",
		Labels:    []string{"order-run:old-sweep", labelOrderTracking},
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create(old-sweep): %v", err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatalf("Close(old-sweep): %v", err)
	}

	// Create a non-tracking bead that happens to be open (should not be touched).
	_, err = store.Create(beads.Bead{
		Title:  "real work",
		Labels: []string{"order-run:dolt-health"},
	})
	if err != nil {
		t.Fatalf("Create(real work): %v", err)
	}

	closed, err := sweepOrphanedOrderTracking(store)
	if err != nil {
		t.Fatalf("sweepOrphanedOrderTracking: %v", err)
	}
	if closed != 4 {
		t.Fatalf("closed = %d, want 4", closed)
	}

	// Verify the open tracking beads in both tiers are now closed.
	all := trackingBeads(t, store, labelOrderTracking)
	for _, b := range all {
		if b.Status != "closed" {
			t.Errorf("tracking bead %s (%s) still open", b.ID, b.Title)
		}
	}

	// Verify the non-tracking work bead is still open.
	work, err := store.ListByLabel("order-run:dolt-health", 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	for _, b := range work {
		if b.Title == "real work" && b.Status != "open" {
			t.Errorf("non-tracking bead %s should still be open, got %s", b.ID, b.Status)
		}
	}
}

func TestSweepOrphanedOrderTrackingLimit_ClosesAtMostBudget(t *testing.T) {
	store := beads.NewMemStore()

	ids := make([]string, 0, 5)
	for _, name := range []string{"one", "two", "three", "four", "five"} {
		b, err := store.Create(beads.Bead{
			Title:     "order:" + name,
			Labels:    []string{"order-run:" + name, labelOrderTracking},
			Ephemeral: true,
		})
		if err != nil {
			t.Fatalf("Create(%s): %v", name, err)
		}
		ids = append(ids, b.ID)
	}

	closed, err := sweepOrphanedOrderTrackingLimit(store, 2)
	if err != nil {
		t.Fatalf("sweepOrphanedOrderTrackingLimit: %v", err)
	}
	if closed != 2 {
		t.Fatalf("closed = %d, want 2", closed)
	}

	closedCount := 0
	for _, id := range ids {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status == "closed" {
			closedCount++
			if got.Metadata["close_reason"] != orphanedOrderTrackingCloseReason {
				t.Fatalf("close_reason for %s = %q, want %q", id, got.Metadata["close_reason"], orphanedOrderTrackingCloseReason)
			}
		}
	}
	if closedCount != 2 {
		t.Fatalf("closed tracking beads = %d, want 2", closedCount)
	}
}

func TestSweepOrphanedOrderTrackingRetryLimitSpendsRemainingBudget(t *testing.T) {
	inner := beads.NewMemStore()
	for _, name := range []string{"one", "two", "three", "four"} {
		_, err := inner.Create(beads.Bead{
			Title:  "order:" + name,
			Labels: []string{"order-run:" + name, labelOrderTracking},
		})
		if err != nil {
			t.Fatalf("Create(%s): %v", name, err)
		}
	}

	fs := &closeFailStore{Store: inner, closeN: 1}
	n, err := sweepOrphanedOrderTrackingRetryLimit(fs, 3, time.Millisecond, 2)
	if err == nil {
		t.Fatal("expected error from partial close failure")
	}
	if n != 2 {
		t.Fatalf("n = %d, want 2", n)
	}
	if fs.listCalls != 1 {
		t.Fatalf("ListByLabel calls = %d, want 1 (budget exhaustion should stop retries)", fs.listCalls)
	}
}

func TestSweepOrphanedOrderTracking_NoOrphans(t *testing.T) {
	store := beads.NewMemStore()

	closed, err := sweepOrphanedOrderTracking(store)
	if err != nil {
		t.Fatalf("sweepOrphanedOrderTracking: %v", err)
	}
	if closed != 0 {
		t.Fatalf("closed = %d, want 0", closed)
	}
}

func TestSweepOrphanedOrderTracking_OnlyClosedBeads(t *testing.T) {
	store := beads.NewMemStore()

	b, err := store.Create(beads.Bead{
		Title:  "order:dolt-health",
		Labels: []string{"order-run:dolt-health", labelOrderTracking},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	closed, err := sweepOrphanedOrderTracking(store)
	if err != nil {
		t.Fatalf("sweepOrphanedOrderTracking: %v", err)
	}
	if closed != 0 {
		t.Fatalf("closed = %d, want 0", closed)
	}
}

func TestSweepStaleOrderTracking_ClosesOnlyOldOpenTrackingBeads(t *testing.T) {
	store := beads.NewMemStore()

	old, err := store.Create(beads.Bead{
		Title:  "order:old-sweep",
		Labels: []string{"order-run:old-sweep", labelOrderTracking},
	})
	if err != nil {
		t.Fatalf("Create(old): %v", err)
	}
	oldEphemeral, err := store.Create(beads.Bead{
		Title:     "order:old-sweep-wisp-tier",
		Labels:    []string{"order-run:old-sweep-wisp-tier", labelOrderTracking},
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create(old ephemeral): %v", err)
	}
	oldWork, err := store.Create(beads.Bead{
		Title:  "real work",
		Labels: []string{"order-run:old-sweep"},
	})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}

	time.Sleep(150 * time.Millisecond)

	fresh, err := store.Create(beads.Bead{
		Title:  "order:fresh-sweep",
		Labels: []string{"order-run:fresh-sweep", labelOrderTracking},
	})
	if err != nil {
		t.Fatalf("Create(fresh): %v", err)
	}

	const expectedClosedTrackingBeads = 2 // issues-tier legacy bead + wisps-tier tracking bead
	closed, err := sweepStaleOrderTracking(store, time.Now(), 100*time.Millisecond, nil, orderTrackingSweepMetadataInitiator)
	if err != nil {
		t.Fatalf("sweepStaleOrderTracking: %v", err)
	}
	if closed != expectedClosedTrackingBeads {
		t.Fatalf("closed = %d, want %d", closed, expectedClosedTrackingBeads)
	}

	gotOld, err := store.Get(old.ID)
	if err != nil {
		t.Fatalf("Get(old): %v", err)
	}
	if gotOld.Status != "closed" {
		t.Fatalf("old tracking status = %s, want closed", gotOld.Status)
	}
	gotOldEphemeral, err := store.Get(oldEphemeral.ID)
	if err != nil {
		t.Fatalf("Get(old ephemeral): %v", err)
	}
	if gotOldEphemeral.Status != "closed" {
		t.Fatalf("old ephemeral tracking status = %s, want closed", gotOldEphemeral.Status)
	}
	gotFresh, err := store.Get(fresh.ID)
	if err != nil {
		t.Fatalf("Get(fresh): %v", err)
	}
	if gotFresh.Status != "open" {
		t.Fatalf("fresh tracking status = %s, want open", gotFresh.Status)
	}
	gotWork, err := store.Get(oldWork.ID)
	if err != nil {
		t.Fatalf("Get(work): %v", err)
	}
	if gotWork.Status != "open" {
		t.Fatalf("non-tracking work status = %s, want open", gotWork.Status)
	}
}

func TestOrderTrackingRetentionPolicyDefaultsToSevenDays(t *testing.T) {
	policy := orderTrackingRetentionPolicyForConfig(&config.City{})

	if policy.deleteAfterClose != 7*24*time.Hour {
		t.Fatalf("deleteAfterClose = %v, want 7d", policy.deleteAfterClose)
	}
	if policy.retainLast != minClosedOrderTrackingRetained {
		t.Fatalf("retainLast = %d, want %d", policy.retainLast, minClosedOrderTrackingRetained)
	}
}

func TestOrderTrackingRetentionPolicyUsesConfiguredDeleteAfterClose(t *testing.T) {
	cfg := &config.City{
		Beads: config.BeadsConfig{
			Policies: map[string]config.BeadPolicyConfig{
				orderTrackingBeadPolicyName: {DeleteAfterClose: "36h"},
			},
		},
	}

	policy := orderTrackingRetentionPolicyForConfig(cfg)

	if policy.deleteAfterClose != 36*time.Hour {
		t.Fatalf("deleteAfterClose = %v, want 36h", policy.deleteAfterClose)
	}
	if policy.retainLast != minClosedOrderTrackingRetained {
		t.Fatalf("retainLast = %d, want %d", policy.retainLast, minClosedOrderTrackingRetained)
	}
}

func TestSweepClosedOrderTrackingRetentionKeepsLatestTenPerOrderAcrossTiers(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	beadTime := now.Add(-48 * time.Hour)
	seed := make([]beads.Bead, 0, 36)
	for i := 0; i < 12; i++ {
		seed = append(seed, beads.Bead{
			ID:        fmt.Sprintf("alpha-%02d", i),
			Title:     "order:alpha",
			Status:    "closed",
			Type:      "task",
			CreatedAt: beadTime.Add(time.Duration(i) * time.Minute),
			Labels:    []string{"order-run:alpha", labelOrderTracking},
			Ephemeral: i%2 == 0,
		})
	}
	for i := 0; i < 9; i++ {
		seed = append(seed, beads.Bead{
			ID:        fmt.Sprintf("beta-%02d", i),
			Title:     "order:beta",
			Status:    "closed",
			Type:      "task",
			CreatedAt: beadTime.Add(time.Duration(i) * time.Minute),
			Labels:    []string{"order-run:beta", labelOrderTracking},
			Ephemeral: i%2 == 0,
		})
	}
	for i := 0; i < 12; i++ {
		seed = append(seed, beads.Bead{
			ID:        fmt.Sprintf("fresh-%02d", i),
			Title:     "order:fresh",
			Status:    "closed",
			Type:      "task",
			CreatedAt: now.Add(-time.Hour).Add(time.Duration(i) * time.Minute),
			Labels:    []string{"order-run:fresh", labelOrderTracking},
			Ephemeral: i%2 == 0,
		})
	}
	seed = append(seed,
		beads.Bead{
			ID:        "open-old",
			Title:     "order:alpha",
			Status:    "open",
			Type:      "task",
			CreatedAt: beadTime,
			Labels:    []string{"order-run:alpha", labelOrderTracking},
			Ephemeral: true,
		},
		beads.Bead{
			ID:        "unscoped-old",
			Title:     "tracking without order scope",
			Status:    "closed",
			Type:      "task",
			CreatedAt: beadTime,
			Labels:    []string{labelOrderTracking},
			Ephemeral: true,
		},
		beads.Bead{
			ID:        "title-only-old",
			Title:     "order:title-only",
			Status:    "closed",
			Type:      "task",
			CreatedAt: beadTime,
			Labels:    []string{labelOrderTracking},
			Ephemeral: true,
		},
	)
	store := beads.NewMemStoreFrom(100, seed, nil)

	deleted, err := sweepClosedOrderTrackingRetention(store, now, orderTrackingRetentionPolicy{
		deleteAfterClose: 24 * time.Hour,
		retainLast:       minClosedOrderTrackingRetained,
	}, nil)
	if err != nil {
		t.Fatalf("sweepClosedOrderTrackingRetention: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}

	for _, id := range []string{"alpha-00", "alpha-01"} {
		if _, err := store.Get(id); !errors.Is(err, beads.ErrNotFound) {
			t.Fatalf("Get(%s) err = %v, want ErrNotFound", id, err)
		}
	}
	for _, prefixAndCount := range []struct {
		prefix string
		start  int
		end    int
	}{
		{prefix: "alpha", start: 2, end: 12},
		{prefix: "beta", start: 0, end: 9},
		{prefix: "fresh", start: 0, end: 12},
	} {
		for i := prefixAndCount.start; i < prefixAndCount.end; i++ {
			id := fmt.Sprintf("%s-%02d", prefixAndCount.prefix, i)
			if _, err := store.Get(id); err != nil {
				t.Fatalf("%s should be preserved: %v", id, err)
			}
		}
	}
	for _, id := range []string{"open-old", "unscoped-old", "title-only-old"} {
		if _, err := store.Get(id); err != nil {
			t.Fatalf("%s should be preserved: %v", id, err)
		}
	}
}

func TestSweepClosedOrderTrackingRetentionPrunesLegacyUnscopedTracking(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	beadTime := now.Add(-48 * time.Hour)
	seed := make([]beads.Bead, 0, minClosedOrderTrackingRetained+2)
	for i := range minClosedOrderTrackingRetained + 2 {
		seed = append(seed, beads.Bead{
			ID:        fmt.Sprintf("legacy-%02d", i),
			Title:     "legacy tracking bead",
			Status:    "closed",
			Type:      "task",
			CreatedAt: beadTime.Add(time.Duration(i) * time.Minute),
			Labels:    []string{labelOrderTracking},
			Ephemeral: i%2 == 0,
		})
	}
	store := beads.NewMemStoreFrom(100, seed, nil)

	deleted, err := sweepClosedOrderTrackingRetention(store, now, orderTrackingRetentionPolicy{
		deleteAfterClose: 24 * time.Hour,
		retainLast:       minClosedOrderTrackingRetained,
	}, nil)
	if err != nil {
		t.Fatalf("sweepClosedOrderTrackingRetention: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}
	for _, id := range []string{"legacy-00", "legacy-01"} {
		if _, err := store.Get(id); !errors.Is(err, beads.ErrNotFound) {
			t.Fatalf("Get(%s) err = %v, want ErrNotFound", id, err)
		}
	}
	for i := 2; i < minClosedOrderTrackingRetained+2; i++ {
		id := fmt.Sprintf("legacy-%02d", i)
		if _, err := store.Get(id); err != nil {
			t.Fatalf("%s should be preserved by legacy retain floor: %v", id, err)
		}
	}
}

func TestSweepClosedOrderTrackingRetentionRanksLatestByClosedReferenceTime(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	seed := make([]beads.Bead, 0, minClosedOrderTrackingRetained+2)
	for i := range minClosedOrderTrackingRetained + 2 {
		seed = append(seed, beads.Bead{
			ID:        fmt.Sprintf("ranked-%02d", i),
			Title:     "order:ranked",
			Status:    "closed",
			Type:      "task",
			CreatedAt: now.Add(-72*time.Hour + time.Duration(i)*time.Minute),
			UpdatedAt: now.Add(-72*time.Hour + time.Duration(i)*time.Minute),
			Labels:    []string{"order-run:ranked", labelOrderTracking},
			Ephemeral: i%2 == 0,
		})
	}
	seed[0].UpdatedAt = now.Add(-25 * time.Hour)
	store := beads.NewMemStoreFrom(100, seed, nil)

	deleted, err := sweepClosedOrderTrackingRetention(store, now, orderTrackingRetentionPolicy{
		deleteAfterClose: 24 * time.Hour,
		retainLast:       minClosedOrderTrackingRetained,
	}, nil)
	if err != nil {
		t.Fatalf("sweepClosedOrderTrackingRetention: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}
	if _, err := store.Get("ranked-00"); err != nil {
		t.Fatalf("ranked-00 should be preserved as a recent close: %v", err)
	}
	for _, id := range []string{"ranked-01", "ranked-02"} {
		if _, err := store.Get(id); !errors.Is(err, beads.ErrNotFound) {
			t.Fatalf("Get(%s) err = %v, want ErrNotFound", id, err)
		}
	}
}

func TestSweepClosedOrderTrackingRetentionAcrossStoresTracksSuccessfulStores(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	seed := make([]beads.Bead, 0, minClosedOrderTrackingRetained+1)
	for i := range minClosedOrderTrackingRetained + 1 {
		seed = append(seed, beads.Bead{
			ID:        fmt.Sprintf("failed-%02d", i),
			Title:     "order:failed",
			Status:    "closed",
			Type:      "task",
			CreatedAt: now.Add(-48*time.Hour + time.Duration(i)*time.Minute),
			Labels:    []string{"order-run:failed", labelOrderTracking},
			Ephemeral: true,
		})
	}
	store := &failingDeleteStore{
		MemStore: beads.NewMemStoreFrom(100, seed, nil),
		failID:   "failed-00",
	}

	result, err := sweepClosedOrderTrackingRetentionAcrossStores([]beads.Store{store}, now, orderTrackingRetentionPolicy{
		deleteAfterClose: 24 * time.Hour,
		retainLast:       minClosedOrderTrackingRetained,
	}, nil)
	if err == nil {
		t.Fatal("sweepClosedOrderTrackingRetentionAcrossStores err = nil, want delete failure")
	}
	if result.storesSwept != 0 {
		t.Fatalf("storesSwept = %d, want 0 when retention prune failed", result.storesSwept)
	}
	if result.deleted != 0 {
		t.Fatalf("deleted = %d, want 0 after failed delete", result.deleted)
	}
}

func TestSweepClosedOrderTrackingRetentionDeletesForAnyConfiguredStorageTarget(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	storages := []string{
		config.BeadStorageHistory,
		config.BeadStorageNoHistory,
		config.BeadStorageEphemeral,
	}

	for _, storage := range storages {
		t.Run(storage, func(t *testing.T) {
			cfg := &config.City{
				Beads: config.BeadsConfig{
					Policies: map[string]config.BeadPolicyConfig{
						orderTrackingBeadPolicyName: {
							Storage:          storage,
							DeleteAfterClose: "1h",
						},
					},
				},
			}
			policy := orderTrackingRetentionPolicyForConfig(cfg)
			seed := make([]beads.Bead, 0, minClosedOrderTrackingRetained+2)
			for i := range minClosedOrderTrackingRetained + 2 {
				seed = append(seed, beads.Bead{
					ID:        fmt.Sprintf("%s-%02d", storage, i),
					Title:     "order:" + storage,
					Status:    "closed",
					Type:      "task",
					CreatedAt: now.Add(-48*time.Hour + time.Duration(i)*time.Minute),
					Labels:    []string{"order-run:" + storage, labelOrderTracking},
					Ephemeral: storage == config.BeadStorageEphemeral,
				})
			}
			store := beads.NewMemStoreFrom(100, seed, nil)

			deleted, err := sweepClosedOrderTrackingRetention(store, now, policy, nil)
			if err != nil {
				t.Fatalf("sweepClosedOrderTrackingRetention: %v", err)
			}
			if deleted != 2 {
				t.Fatalf("deleted = %d, want 2", deleted)
			}
			for _, id := range []string{fmt.Sprintf("%s-00", storage), fmt.Sprintf("%s-01", storage)} {
				if _, err := store.Get(id); !errors.Is(err, beads.ErrNotFound) {
					t.Fatalf("Get(%s) err = %v, want ErrNotFound", id, err)
				}
			}
			remaining, err := store.List(beads.ListQuery{
				Status:   "closed",
				Label:    labelOrderTracking,
				TierMode: beads.TierBoth,
			})
			if err != nil {
				t.Fatalf("List(remaining): %v", err)
			}
			if len(remaining) != minClosedOrderTrackingRetained {
				t.Fatalf("remaining = %d, want %d", len(remaining), minClosedOrderTrackingRetained)
			}
		})
	}
}

type noopCloseAllStore struct {
	beads.Store
	closeCalls int
}

func (s *noopCloseAllStore) CloseAll(_ []string, _ map[string]string) (int, error) {
	s.closeCalls++
	return 1, nil
}

func TestCloseOrderTrackingBeadErrorsWhenVerificationStillOpen(t *testing.T) {
	base := beads.NewMemStore()
	tracking, err := base.Create(beads.Bead{
		Title:     "order:stuck",
		Labels:    []string{"order-run:stuck", labelOrderTracking},
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create(tracking): %v", err)
	}
	store := &noopCloseAllStore{Store: base}

	err = closeOrderTrackingBead(context.Background(), store, tracking.ID)
	if err == nil {
		t.Fatal("closeOrderTrackingBead err = nil, want read-after-close verification error")
	}
	if !strings.Contains(err.Error(), tracking.ID) {
		t.Fatalf("err = %q, want stuck tracking bead id %q", err, tracking.ID)
	}
	if store.closeCalls < 2 {
		t.Fatalf("CloseAll calls = %d, want retry before verification failure", store.closeCalls)
	}
}

type flakyCloseAllStore struct {
	beads.Store
	failuresRemaining int
	closeCalls        int
}

func (s *flakyCloseAllStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	s.closeCalls++
	if s.failuresRemaining > 0 {
		s.failuresRemaining--
		return 0, fmt.Errorf("transient close conflict")
	}
	return s.Store.CloseAll(ids, metadata)
}

func TestCloseOrderTrackingBeadRetriesTransientCloseConflict(t *testing.T) {
	base := beads.NewMemStore()
	tracking, err := base.Create(beads.Bead{
		Title:     "order:retry",
		Labels:    []string{"order-run:retry", labelOrderTracking},
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create(tracking): %v", err)
	}
	store := &flakyCloseAllStore{Store: base, failuresRemaining: 1}

	if err := closeOrderTrackingBead(context.Background(), store, tracking.ID); err != nil {
		t.Fatalf("closeOrderTrackingBead: %v", err)
	}
	if store.closeCalls != 2 {
		t.Fatalf("CloseAll calls = %d, want 2", store.closeCalls)
	}
	got, err := base.Get(tracking.ID)
	if err != nil {
		t.Fatalf("Get(tracking): %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("tracking status = %q, want closed", got.Status)
	}
}

func TestSweepStaleOrderTrackingAcrossStoresClosesRigStoreAndUnblocksDispatch(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rigStore := beads.NewMemStore()
	legacyStore := beads.NewMemStore()
	ran := false
	m := &memoryOrderDispatcher{
		aa: []orders.Order{{
			Name:     "rig-digest",
			Rig:      "frontend",
			Trigger:  "cooldown",
			Interval: "1m",
			Exec:     "true",
			Timeout:  "1m",
		}},
		storeFn: func(target execStoreTarget) (beads.Store, error) {
			if target.ScopeKind == "city" {
				return legacyStore, nil
			}
			return rigStore, nil
		},
		execRun: func(context.Context, string, string, []string) ([]byte, error) {
			ran = true
			return nil, nil
		},
		rec:    events.Discard,
		stderr: &bytes.Buffer{},
		cfg: &config.City{
			Rigs: []config.Rig{{
				Name: "frontend",
				Path: rigDir,
			}},
		},
	}
	stale, err := rigStore.Create(beads.Bead{
		Title:     "order:rig-digest:rig:frontend",
		Labels:    []string{"order-run:rig-digest:rig:frontend", labelOrderTracking},
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create(stale): %v", err)
	}

	m.dispatch(context.Background(), cityDir, stale.CreatedAt.Add(time.Hour))
	m.drain(context.Background())
	if ran {
		t.Fatal("dispatch ran before stale rig tracking bead was cleaned")
	}

	result, err := sweepStaleOrderTrackingAcrossStores(
		[]beads.Store{rigStore, legacyStore},
		stale.CreatedAt.Add(time.Hour),
		time.Minute,
		orderFilterForTest("rig-digest:rig:frontend"),
		false,
	)
	if err != nil {
		t.Fatalf("sweepStaleOrderTrackingAcrossStores: %v", err)
	}
	if result.trackingClosed != 1 {
		t.Fatalf("trackingClosed = %d, want 1", result.trackingClosed)
	}

	m.dispatch(context.Background(), cityDir, stale.CreatedAt.Add(2*time.Hour))
	m.drain(context.Background())
	if !ran {
		t.Fatal("dispatch did not run after stale rig tracking bead was closed")
	}
}

type failingListOrderTrackingStore struct {
	beads.Store
	err error
}

func (s *failingListOrderTrackingStore) ListByLabel(label string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	if label == labelOrderTracking {
		return nil, s.err
	}
	return s.Store.ListByLabel(label, limit, opts...)
}

func (s *failingListOrderTrackingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Label == labelOrderTracking {
		return nil, s.err
	}
	return s.Store.List(query)
}

func TestSweepStaleOrderTrackingAcrossStoresContinuesAfterStoreError(t *testing.T) {
	failingStore := &failingListOrderTrackingStore{
		Store: beads.NewMemStore(),
		err:   fmt.Errorf("store unavailable"),
	}
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	cityStale, err := cityStore.Create(beads.Bead{
		Title:     "order:cleanup",
		Labels:    []string{"order-run:cleanup", labelOrderTracking},
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create(city stale): %v", err)
	}
	rigStale, err := rigStore.Create(beads.Bead{
		Title:     "order:cleanup:rig:frontend",
		Labels:    []string{"order-run:cleanup:rig:frontend", labelOrderTracking},
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create(rig stale): %v", err)
	}

	result, err := sweepStaleOrderTrackingAcrossStores(
		[]beads.Store{failingStore, cityStore, rigStore},
		cityStale.CreatedAt.Add(time.Hour),
		time.Minute,
		nil,
		false,
	)
	if err == nil {
		t.Fatal("sweepStaleOrderTrackingAcrossStores err = nil, want aggregate store error")
	}
	if result.trackingClosed != 2 {
		t.Fatalf("trackingClosed = %d, want 2", result.trackingClosed)
	}
	if result.storesSwept != 2 {
		t.Fatalf("storesSwept = %d, want 2", result.storesSwept)
	}
	for _, tc := range []struct {
		name  string
		store beads.Store
		id    string
	}{
		{name: "city", store: cityStore, id: cityStale.ID},
		{name: "rig", store: rigStore, id: rigStale.ID},
	} {
		got, err := tc.store.Get(tc.id)
		if err != nil {
			t.Fatalf("%s Get(%s): %v", tc.name, tc.id, err)
		}
		if got.Status != "closed" {
			t.Fatalf("%s stale tracking status = %q, want closed", tc.name, got.Status)
		}
	}
}

func TestSweepStaleOrderTrackingClosesTriggerEnvFailedBeadsAndUnblocksDispatch(t *testing.T) {
	store := beads.NewMemStore()
	failed, err := store.Create(beads.Bead{
		Title:     "order:pg-cooldown",
		Labels:    []string{"order-run:pg-cooldown", labelOrderTracking, labelTriggerEnvFailed},
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create(trigger-env failed): %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	normal, err := store.Create(beads.Bead{
		Title:     "order:pg-cooldown",
		Labels:    []string{"order-run:pg-cooldown", labelOrderTracking},
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create(normal tracking): %v", err)
	}

	result, err := sweepStaleOrderTrackingWithOptions(
		store,
		normal.CreatedAt.Add(time.Hour),
		nil,
		false,
	)
	if err != nil {
		t.Fatalf("sweepStaleOrderTrackingWithOptions: %v", err)
	}
	if result.trackingClosed != 2 {
		t.Fatalf("trackingClosed = %d, want 2", result.trackingClosed)
	}
	gotFailed, err := store.Get(failed.ID)
	if err != nil {
		t.Fatalf("Get(trigger-env failed): %v", err)
	}
	if gotFailed.Status != "closed" {
		t.Fatalf("trigger-env failed status = %s, want closed", gotFailed.Status)
	}
	gotNormal, err := store.Get(normal.ID)
	if err != nil {
		t.Fatalf("Get(normal tracking): %v", err)
	}
	if gotNormal.Status != "closed" {
		t.Fatalf("normal tracking status = %s, want closed", gotNormal.Status)
	}

	ran := false
	ad := buildOrderDispatcherFromListExec([]orders.Order{{
		Name:     "pg-cooldown",
		Trigger:  "cooldown",
		Interval: "1s",
		Exec:     "true",
	}}, store, nil, func(context.Context, string, string, []string) ([]byte, error) {
		ran = true
		return nil, nil
	}, nil)
	ad.dispatch(context.Background(), t.TempDir(), normal.CreatedAt.Add(2*time.Hour))
	ad.drain(context.Background())
	if !ran {
		t.Fatal("dispatch did not run after stale trigger-env marker was closed")
	}
}

func TestCloseAndVerifyOrderTrackingBeadsStopsRetryOnContextCancel(t *testing.T) {
	base := beads.NewMemStore()
	tracking, err := base.Create(beads.Bead{
		Title:     "order:canceled",
		Labels:    []string{"order-run:canceled", labelOrderTracking},
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create(tracking): %v", err)
	}
	store := &noopCloseAllStore{Store: base}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = closeAndVerifyOrderTrackingBeads(ctx, store, []string{tracking.ID}, map[string]string{
		"close_reason": completedOrderTrackingCloseReason,
	})
	if err == nil {
		t.Fatal("closeAndVerifyOrderTrackingBeads err = nil, want context cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if store.closeCalls != 1 {
		t.Fatalf("CloseAll calls = %d, want 1", store.closeCalls)
	}
}

func orderFilterForTest(names ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(names))
	for _, name := range names {
		out[name] = struct{}{}
	}
	return out
}

type parentLastCloseStore struct {
	beads.Store
}

func (s parentLastCloseStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	closed := 0
	for _, id := range ids {
		bead, err := s.Get(id)
		if err != nil {
			return closed, err
		}
		if bead.Status == "closed" {
			continue
		}
		children, err := s.List(beads.ListQuery{ParentID: id, TierMode: beads.TierBoth})
		if err != nil {
			return closed, err
		}
		for _, child := range children {
			if child.Status != "closed" {
				return closed, fmt.Errorf("cannot close %s before open child %s", id, child.ID)
			}
		}
		n, err := s.Store.CloseAll([]string{id}, metadata)
		closed += n
		if err != nil {
			return closed, err
		}
	}
	return closed, nil
}

type depListFailStore struct {
	beads.Store
	failID string
}

func (s depListFailStore) DepList(id, direction string) ([]beads.Dep, error) {
	if id == s.failID {
		return nil, fmt.Errorf("dependency list unavailable for %s", id)
	}
	return s.Store.DepList(id, direction)
}

func TestSweepStaleOrderTrackingWithWispsRequiresOrderFilter(t *testing.T) {
	store := beads.NewMemStore()

	result, err := sweepStaleOrderTrackingWithOptions(
		store,
		time.Now(),
		nil,
		true,
	)
	if err == nil {
		t.Fatal("sweepStaleOrderTrackingWithOptions err = nil, want order-filter error")
	}
	if !strings.Contains(err.Error(), "requires at least one order name") {
		t.Fatalf("err = %q, want order-filter context", err)
	}
	if result.trackingClosed != 0 || result.wispClosed != 0 {
		t.Fatalf("result = %+v, want no partial closes", result)
	}
}

func TestSweepStaleOrderTrackingWithWispsClosesOldOpenWispSubtree(t *testing.T) {
	store := beads.NewMemStore()

	wispRoot, err := store.Create(beads.Bead{
		Title:  "mol-digest-generate",
		Type:   "molecule",
		Labels: []string{"order-run:digest"},
	})
	if err != nil {
		t.Fatalf("Create(wisp root): %v", err)
	}
	child, err := store.Create(beads.Bead{
		Title:    "draft-digest",
		ParentID: wispRoot.ID,
	})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}
	closedChild, err := store.Create(beads.Bead{
		Title:    "prepare-submolecule",
		ParentID: wispRoot.ID,
	})
	if err != nil {
		t.Fatalf("Create(closed child): %v", err)
	}
	if err := store.Close(closedChild.ID); err != nil {
		t.Fatalf("Close(closed child): %v", err)
	}
	grandchild, err := store.Create(beads.Bead{
		Title:    "nested-step-still-running",
		ParentID: closedChild.ID,
	})
	if err != nil {
		t.Fatalf("Create(grandchild): %v", err)
	}
	otherRoot, err := store.Create(beads.Bead{
		Title:  "mol-other-order",
		Type:   "molecule",
		Labels: []string{"order-run:other"},
	})
	if err != nil {
		t.Fatalf("Create(other root): %v", err)
	}
	otherChild, err := store.Create(beads.Bead{
		Title:    "other-step",
		ParentID: otherRoot.ID,
	})
	if err != nil {
		t.Fatalf("Create(other child): %v", err)
	}

	result, err := sweepStaleOrderTrackingWithOptions(
		store,
		wispRoot.CreatedAt.Add(time.Hour),
		orderFilterForTest("digest"),
		true,
	)
	if err != nil {
		t.Fatalf("sweepStaleOrderTrackingWithOptions: %v", err)
	}
	if result.trackingClosed != 0 {
		t.Fatalf("trackingClosed = %d, want 0", result.trackingClosed)
	}
	if result.wispClosed != 3 {
		t.Fatalf("wispClosed = %d, want 3", result.wispClosed)
	}

	for _, id := range []string{wispRoot.ID, child.ID, grandchild.ID} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", id, got.Status)
		}
		if got.Metadata["close_reason"] != staleOrderWispCloseReason {
			t.Fatalf("%s close_reason = %q, want %q", id, got.Metadata["close_reason"], staleOrderWispCloseReason)
		}
		if got.Metadata["order_tracking_sweep"] != orderTrackingSweepMetadataReason {
			t.Fatalf("%s order_tracking_sweep = %q, want %q", id, got.Metadata["order_tracking_sweep"], orderTrackingSweepMetadataReason)
		}
	}
	for _, id := range []string{otherRoot.ID, otherChild.ID} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status != "open" {
			t.Fatalf("unscoped wisp %s status = %q, want open", id, got.Status)
		}
	}
}

func TestSweepStaleOrderTrackingWithWispsClosesGraphDependentSubtreeViaTrackingSweep(t *testing.T) {
	store := beads.NewMemStore()

	wispRoot, err := store.Create(beads.Bead{
		Title:  "mol-seth-patrol",
		Type:   "task",
		Labels: []string{"order-run:seth-patrol"},
		Metadata: map[string]string{
			"gc.kind": "workflow",
		},
	})
	if err != nil {
		t.Fatalf("Create(wisp root): %v", err)
	}
	step, err := store.Create(beads.Bead{
		Title: "Infrastructure patrol",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": wispRoot.ID,
			"gc.step_ref":     "mol-seth-patrol.patrol",
		},
	})
	if err != nil {
		t.Fatalf("Create(step): %v", err)
	}
	if err := store.DepAdd(step.ID, wispRoot.ID, "tracks"); err != nil {
		t.Fatalf("DepAdd(tracks): %v", err)
	}

	result, err := sweepStaleOrderTrackingWithOptions(
		store,
		wispRoot.CreatedAt.Add(time.Hour),
		orderFilterForTest("seth-patrol"),
		true,
	)
	if err != nil {
		t.Fatalf("sweepStaleOrderTrackingWithOptions: %v", err)
	}
	if result.wispClosed != 2 {
		t.Fatalf("wispClosed = %d, want 2", result.wispClosed)
	}

	for _, id := range []string{wispRoot.ID, step.ID} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", id, got.Status)
		}
		if got.Metadata["close_reason"] != staleOrderWispCloseReason {
			t.Fatalf("%s close_reason = %q, want %q", id, got.Metadata["close_reason"], staleOrderWispCloseReason)
		}
	}
}

func TestSweepStaleOrderTrackingWithWispsClosesMetadataGraphSubtreeWithoutCloseOrdering(t *testing.T) {
	base := beads.NewMemStore()

	wispRoot, err := base.Create(beads.Bead{
		Title:     "mol-seth-patrol",
		Type:      "task",
		Labels:    []string{"order-run:seth-patrol"},
		Ephemeral: true,
		Metadata: map[string]string{
			"gc.kind": "workflow",
		},
	})
	if err != nil {
		t.Fatalf("Create(wisp root): %v", err)
	}
	step, err := base.Create(beads.Bead{
		Title:     "Infrastructure patrol",
		Type:      "task",
		Ephemeral: true,
		Metadata: map[string]string{
			"gc.root_bead_id": wispRoot.ID,
			"gc.step_ref":     "mol-seth-patrol.patrol",
		},
	})
	if err != nil {
		t.Fatalf("Create(step): %v", err)
	}
	store := depListFailStore{Store: base, failID: wispRoot.ID}

	result, err := sweepStaleOrderTrackingWithOptions(
		store,
		wispRoot.CreatedAt.Add(time.Hour),
		orderFilterForTest("seth-patrol"),
		true,
	)
	if err != nil {
		t.Fatalf("sweepStaleOrderTrackingWithOptions: %v", err)
	}
	if result.wispClosed != 2 {
		t.Fatalf("wispClosed = %d, want 2", result.wispClosed)
	}

	for _, id := range []string{wispRoot.ID, step.ID} {
		got, err := base.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", id, got.Status)
		}
		if got.Metadata["close_reason"] != staleOrderWispCloseReason {
			t.Fatalf("%s close_reason = %q, want %q", id, got.Metadata["close_reason"], staleOrderWispCloseReason)
		}
	}
}

func TestSweepStaleOrderTrackingWithWispsMixedLegacyGraphClosesOnlyOwnedSubtree(t *testing.T) {
	store := beads.NewMemStore()

	metadataRoot, err := store.Create(beads.Bead{
		Title:     "mol-seth-patrol",
		Type:      "task",
		Labels:    []string{"order-run:seth-patrol"},
		Ephemeral: true,
		Metadata: map[string]string{
			"gc.kind": "workflow",
		},
	})
	if err != nil {
		t.Fatalf("Create(metadata root): %v", err)
	}
	metadataStep, err := store.Create(beads.Bead{
		Title:     "Metadata-backed step",
		Type:      "task",
		Ephemeral: true,
		Metadata: map[string]string{
			"gc.root_bead_id": metadataRoot.ID,
			"gc.step_ref":     "mol-seth-patrol.metadata",
		},
	})
	if err != nil {
		t.Fatalf("Create(metadata step): %v", err)
	}

	legacyRoot, err := store.Create(beads.Bead{
		Title:     "mol-seth-patrol",
		Type:      "task",
		Labels:    []string{"order-run:seth-patrol"},
		Ephemeral: true,
		Metadata: map[string]string{
			"gc.kind": "workflow",
		},
	})
	if err != nil {
		t.Fatalf("Create(legacy root): %v", err)
	}
	legacyStep, err := store.Create(beads.Bead{
		Title:     "Legacy graph step",
		Type:      "task",
		Ephemeral: true,
		Metadata: map[string]string{
			"gc.step_ref": "mol-seth-patrol.legacy",
		},
	})
	if err != nil {
		t.Fatalf("Create(legacy step): %v", err)
	}
	if err := store.DepAdd(legacyStep.ID, legacyRoot.ID, "tracks"); err != nil {
		t.Fatalf("DepAdd(legacy tracks): %v", err)
	}

	result, err := sweepStaleOrderTrackingWithOptions(
		store,
		metadataRoot.CreatedAt.Add(time.Hour),
		orderFilterForTest("seth-patrol"),
		true,
	)
	if err != nil {
		t.Fatalf("sweepStaleOrderTrackingWithOptions: %v", err)
	}
	if result.wispClosed != 2 {
		t.Fatalf("wispClosed = %d, want 2", result.wispClosed)
	}
	for _, id := range []string{metadataRoot.ID, metadataStep.ID} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", id, got.Status)
		}
	}
	// Dep-linked beads without gc.root_bead_id ownership metadata are outside
	// the sweep's close authority (orderWispGraphDependentOwnedByRoot), so the
	// legacy subtree is conservatively left open for dep-edge reaping.
	for _, id := range []string{legacyRoot.ID, legacyStep.ID} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status != "open" {
			t.Fatalf("unowned legacy wisp %s status = %q, want open", id, got.Status)
		}
	}
}

func TestSweepStaleOrderTrackingDryRunCountsGraphSubtreeWithoutClosing(t *testing.T) {
	store := beads.NewMemStore()

	tracking, err := store.Create(beads.Bead{
		Title:     "order:seth-patrol",
		Labels:    []string{"order-run:seth-patrol", labelOrderTracking},
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create(tracking): %v", err)
	}
	wispRoot, err := store.Create(beads.Bead{
		Title:  "mol-seth-patrol",
		Type:   "task",
		Labels: []string{"order-run:seth-patrol"},
		Metadata: map[string]string{
			"gc.kind": "workflow",
		},
	})
	if err != nil {
		t.Fatalf("Create(wisp root): %v", err)
	}
	step, err := store.Create(beads.Bead{
		Title: "Infrastructure patrol",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": wispRoot.ID,
			"gc.step_ref":     "mol-seth-patrol.patrol",
		},
	})
	if err != nil {
		t.Fatalf("Create(step): %v", err)
	}
	if err := store.DepAdd(step.ID, wispRoot.ID, "tracks"); err != nil {
		t.Fatalf("DepAdd(tracks): %v", err)
	}

	result, err := sweepStaleOrderTrackingWithOptionsLimitDryRun(
		store,
		wispRoot.CreatedAt.Add(time.Hour),
		time.Minute,
		orderFilterForTest("seth-patrol"),
		orderTrackingSweepMetadataInitiator,
		true,
		0,
	)
	if err != nil {
		t.Fatalf("sweepStaleOrderTrackingWithOptionsLimitDryRun: %v", err)
	}
	if result.trackingClosed != 1 {
		t.Fatalf("trackingClosed = %d, want 1 dry-run candidate", result.trackingClosed)
	}
	if result.wispClosed != 2 {
		t.Fatalf("wispClosed = %d, want 2 dry-run candidates", result.wispClosed)
	}

	for _, id := range []string{tracking.ID, wispRoot.ID, step.ID} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status != "open" {
			t.Fatalf("%s status after dry-run = %q, want open", id, got.Status)
		}
	}
}

func TestSweepStaleOrderTrackingDryRunSkipsCloseOrdering(t *testing.T) {
	base := beads.NewMemStore()

	wispRoot, err := base.Create(beads.Bead{
		Title:  "mol-digest-generate",
		Type:   "molecule",
		Labels: []string{"order-run:digest"},
	})
	if err != nil {
		t.Fatalf("Create(wisp root): %v", err)
	}
	child, err := base.Create(beads.Bead{
		Title:    "draft-digest",
		ParentID: wispRoot.ID,
	})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}
	store := depListFailStore{Store: base, failID: child.ID}

	result, err := sweepStaleOrderTrackingWithOptionsLimitDryRun(
		store,
		wispRoot.CreatedAt.Add(time.Hour),
		time.Minute,
		orderFilterForTest("digest"),
		orderTrackingSweepMetadataInitiator,
		true,
		0,
	)
	if err != nil {
		t.Fatalf("sweepStaleOrderTrackingWithOptionsLimitDryRun: %v", err)
	}
	if result.wispClosed != 2 {
		t.Fatalf("wispClosed = %d, want 2 dry-run candidates", result.wispClosed)
	}
	for _, id := range []string{wispRoot.ID, child.ID} {
		got, err := base.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status != "open" {
			t.Fatalf("%s status after dry-run = %q, want open", id, got.Status)
		}
	}
}

func TestSweepStaleOrderTrackingDryRunUsesRootMetadataDescendants(t *testing.T) {
	base := beads.NewMemStore()

	wispRoot, err := base.Create(beads.Bead{
		Title:     "mol-seth-patrol",
		Type:      "task",
		Labels:    []string{"order-run:seth-patrol"},
		Ephemeral: true,
		Metadata: map[string]string{
			"gc.kind": "workflow",
		},
	})
	if err != nil {
		t.Fatalf("Create(wisp root): %v", err)
	}
	step, err := base.Create(beads.Bead{
		Title:     "Infrastructure patrol",
		Type:      "task",
		Ephemeral: true,
		Metadata: map[string]string{
			"gc.root_bead_id": wispRoot.ID,
			"gc.step_ref":     "mol-seth-patrol.patrol",
		},
	})
	if err != nil {
		t.Fatalf("Create(step): %v", err)
	}
	store := depListFailStore{Store: base, failID: wispRoot.ID}

	result, err := sweepStaleOrderTrackingWithOptionsLimitDryRun(
		store,
		wispRoot.CreatedAt.Add(time.Hour),
		time.Minute,
		orderFilterForTest("seth-patrol"),
		orderTrackingSweepMetadataInitiator,
		true,
		0,
	)
	if err != nil {
		t.Fatalf("sweepStaleOrderTrackingWithOptionsLimitDryRun: %v", err)
	}
	if result.wispClosed != 2 {
		t.Fatalf("wispClosed = %d, want 2 dry-run candidates", result.wispClosed)
	}
	for _, id := range []string{wispRoot.ID, step.ID} {
		got, err := base.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status != "open" {
			t.Fatalf("%s status after dry-run = %q, want open", id, got.Status)
		}
	}
}

// Partial-stamp molecules carry gc.root_bead_id on some steps while sibling
// ParentID-only steps are un-stamped. The four tests below pin that such
// un-stamped children take part in both the freshness veto and the close set
// on the walk path (stamped sibling closed, so the batch path declines) and
// on the batch path (stamped sibling open, batch handles the store).

func TestSweepStaleOrderTrackingWithWispsPartialStampFreshChildVetoesClose(t *testing.T) {
	store := &createdAtOverrideStore{Store: beads.NewMemStore()}

	wispRoot, err := store.Create(beads.Bead{
		Title:  "mol-seth-patrol",
		Type:   "task",
		Labels: []string{"order-run:seth-patrol"},
		Metadata: map[string]string{
			"gc.kind": "workflow",
		},
	})
	if err != nil {
		t.Fatalf("Create(wisp root): %v", err)
	}
	stampedStep, err := store.Create(beads.Bead{
		Title: "Stamped finished step",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": wispRoot.ID,
			"gc.step_ref":     "mol-seth-patrol.done",
		},
	})
	if err != nil {
		t.Fatalf("Create(stamped step): %v", err)
	}
	if err := store.Close(stampedStep.ID); err != nil {
		t.Fatalf("Close(stamped step): %v", err)
	}
	now := wispRoot.CreatedAt.Add(time.Hour)
	unstampedChild, err := store.Create(beads.Bead{
		Title:     "Un-stamped live step",
		Type:      "task",
		ParentID:  wispRoot.ID,
		CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("Create(unstamped child): %v", err)
	}

	result, err := sweepStaleOrderTrackingWithOptions(
		store,
		now,
		orderFilterForTest("seth-patrol"),
		true,
	)
	if err != nil {
		t.Fatalf("sweepStaleOrderTrackingWithOptions: %v", err)
	}
	if result.wispClosed != 0 {
		t.Fatalf("wispClosed = %d, want 0 (fresh un-stamped child must veto)", result.wispClosed)
	}
	for _, id := range []string{wispRoot.ID, unstampedChild.ID} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status != "open" {
			t.Fatalf("%s status = %q, want open", id, got.Status)
		}
	}
}

func TestSweepStaleOrderTrackingWithWispsPartialStampClosesStaleChildWithSubtree(t *testing.T) {
	store := beads.NewMemStore()

	wispRoot, err := store.Create(beads.Bead{
		Title:  "mol-seth-patrol",
		Type:   "task",
		Labels: []string{"order-run:seth-patrol"},
		Metadata: map[string]string{
			"gc.kind": "workflow",
		},
	})
	if err != nil {
		t.Fatalf("Create(wisp root): %v", err)
	}
	stampedStep, err := store.Create(beads.Bead{
		Title: "Stamped finished step",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": wispRoot.ID,
			"gc.step_ref":     "mol-seth-patrol.done",
		},
	})
	if err != nil {
		t.Fatalf("Create(stamped step): %v", err)
	}
	if err := store.Close(stampedStep.ID); err != nil {
		t.Fatalf("Close(stamped step): %v", err)
	}
	unstampedChild, err := store.Create(beads.Bead{
		Title:    "Un-stamped stale step",
		Type:     "task",
		ParentID: wispRoot.ID,
	})
	if err != nil {
		t.Fatalf("Create(unstamped child): %v", err)
	}

	result, err := sweepStaleOrderTrackingWithOptions(
		store,
		wispRoot.CreatedAt.Add(time.Hour),
		orderFilterForTest("seth-patrol"),
		true,
	)
	if err != nil {
		t.Fatalf("sweepStaleOrderTrackingWithOptions: %v", err)
	}
	if result.wispClosed != 2 {
		t.Fatalf("wispClosed = %d, want 2 (root and un-stamped child)", result.wispClosed)
	}
	for _, id := range []string{wispRoot.ID, unstampedChild.ID} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status != "closed" {
			t.Fatalf("%s status = %q, want closed (un-stamped child must not be stranded)", id, got.Status)
		}
	}
}

func TestSweepStaleOrderTrackingBatchPartialStampFreshChildVetoesClose(t *testing.T) {
	base := &createdAtOverrideStore{Store: beads.NewMemStore()}

	wispRoot, err := base.Create(beads.Bead{
		Title:  "mol-seth-patrol",
		Type:   "task",
		Labels: []string{"order-run:seth-patrol"},
		Metadata: map[string]string{
			"gc.kind": "workflow",
		},
	})
	if err != nil {
		t.Fatalf("Create(wisp root): %v", err)
	}
	stampedStep, err := base.Create(beads.Bead{
		Title: "Stamped open step",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": wispRoot.ID,
			"gc.step_ref":     "mol-seth-patrol.open",
		},
	})
	if err != nil {
		t.Fatalf("Create(stamped step): %v", err)
	}
	now := wispRoot.CreatedAt.Add(time.Hour)
	unstampedChild, err := base.Create(beads.Bead{
		Title:     "Un-stamped live step",
		Type:      "task",
		ParentID:  wispRoot.ID,
		CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("Create(unstamped child): %v", err)
	}

	// depListFailStore pins that the batch path handles this store: any
	// fallback to the walk would DepList the root and fail the sweep.
	store := depListFailStore{Store: base, failID: wispRoot.ID}

	result, err := sweepStaleOrderTrackingWithOptions(
		store,
		now,
		orderFilterForTest("seth-patrol"),
		true,
	)
	if err != nil {
		t.Fatalf("sweepStaleOrderTrackingWithOptions: %v", err)
	}
	if result.wispClosed != 0 {
		t.Fatalf("wispClosed = %d, want 0 (fresh un-stamped child must veto)", result.wispClosed)
	}
	for _, id := range []string{wispRoot.ID, stampedStep.ID, unstampedChild.ID} {
		got, err := base.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status != "open" {
			t.Fatalf("%s status = %q, want open", id, got.Status)
		}
	}
}

func TestSweepStaleOrderTrackingBatchPartialStampClosesStaleChildWithSubtree(t *testing.T) {
	base := beads.NewMemStore()

	wispRoot, err := base.Create(beads.Bead{
		Title:  "mol-seth-patrol",
		Type:   "task",
		Labels: []string{"order-run:seth-patrol"},
		Metadata: map[string]string{
			"gc.kind": "workflow",
		},
	})
	if err != nil {
		t.Fatalf("Create(wisp root): %v", err)
	}
	stampedStep, err := base.Create(beads.Bead{
		Title: "Stamped open step",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": wispRoot.ID,
			"gc.step_ref":     "mol-seth-patrol.open",
		},
	})
	if err != nil {
		t.Fatalf("Create(stamped step): %v", err)
	}
	unstampedChild, err := base.Create(beads.Bead{
		Title:    "Un-stamped stale step",
		Type:     "task",
		ParentID: wispRoot.ID,
	})
	if err != nil {
		t.Fatalf("Create(unstamped child): %v", err)
	}
	// depListFailStore pins that the batch path handles this store without
	// close ordering: a walk fallback or closeorder pass would DepList the
	// root and fail the sweep.
	store := depListFailStore{Store: base, failID: wispRoot.ID}

	result, err := sweepStaleOrderTrackingWithOptions(
		store,
		wispRoot.CreatedAt.Add(time.Hour),
		orderFilterForTest("seth-patrol"),
		true,
	)
	if err != nil {
		t.Fatalf("sweepStaleOrderTrackingWithOptions: %v", err)
	}
	if result.wispClosed != 3 {
		t.Fatalf("wispClosed = %d, want 3 (root, stamped step, un-stamped child)", result.wispClosed)
	}
	for _, id := range []string{wispRoot.ID, stampedStep.ID, unstampedChild.ID} {
		got, err := base.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status != "closed" {
			t.Fatalf("%s status = %q, want closed (un-stamped child must not be stranded)", id, got.Status)
		}
	}
}

// The two tests below pin the closed-intermediate shape on the batch path:
// an open un-stamped grandchild reachable only through a closed ParentID
// intermediate (the production instance is a lingering nudge/mail chore
// parented under an already-closed step). The batch path must traverse the
// closed intermediate exactly like the walk path's IncludeClosed queries do,
// so a fresh grandchild vetoes the close and a stale one drains with the
// subtree instead of being stranded.

func TestSweepStaleOrderTrackingBatchFreshGrandchildBehindClosedIntermediateVetoesClose(t *testing.T) {
	base := &createdAtOverrideStore{Store: beads.NewMemStore()}

	wispRoot, err := base.Create(beads.Bead{
		Title:  "mol-seth-patrol",
		Type:   "task",
		Labels: []string{"order-run:seth-patrol"},
		Metadata: map[string]string{
			"gc.kind": "workflow",
		},
	})
	if err != nil {
		t.Fatalf("Create(wisp root): %v", err)
	}
	stampedStep, err := base.Create(beads.Bead{
		Title: "Stamped open step",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": wispRoot.ID,
			"gc.step_ref":     "mol-seth-patrol.open",
		},
	})
	if err != nil {
		t.Fatalf("Create(stamped step): %v", err)
	}
	intermediate, err := base.Create(beads.Bead{
		Title:    "Un-stamped finished step",
		Type:     "task",
		ParentID: wispRoot.ID,
	})
	if err != nil {
		t.Fatalf("Create(intermediate): %v", err)
	}
	now := wispRoot.CreatedAt.Add(time.Hour)
	grandchild, err := base.Create(beads.Bead{
		Title:     "Un-stamped live chore",
		Type:      "task",
		ParentID:  intermediate.ID,
		CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("Create(grandchild): %v", err)
	}
	if err := base.Close(intermediate.ID); err != nil {
		t.Fatalf("Close(intermediate): %v", err)
	}

	// depListFailStore pins that the batch path handles this store: any
	// fallback to the walk would DepList the root and fail the sweep.
	store := depListFailStore{Store: base, failID: wispRoot.ID}

	result, err := sweepStaleOrderTrackingWithOptions(
		store,
		now,
		orderFilterForTest("seth-patrol"),
		true,
	)
	if err != nil {
		t.Fatalf("sweepStaleOrderTrackingWithOptions: %v", err)
	}
	if result.wispClosed != 0 {
		t.Fatalf("wispClosed = %d, want 0 (fresh grandchild behind closed intermediate must veto)", result.wispClosed)
	}
	for _, id := range []string{wispRoot.ID, stampedStep.ID, grandchild.ID} {
		got, err := base.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status != "open" {
			t.Fatalf("%s status = %q, want open", id, got.Status)
		}
	}
}

func TestSweepStaleOrderTrackingBatchClosesStaleGrandchildBehindClosedIntermediate(t *testing.T) {
	base := beads.NewMemStore()

	wispRoot, err := base.Create(beads.Bead{
		Title:  "mol-seth-patrol",
		Type:   "task",
		Labels: []string{"order-run:seth-patrol"},
		Metadata: map[string]string{
			"gc.kind": "workflow",
		},
	})
	if err != nil {
		t.Fatalf("Create(wisp root): %v", err)
	}
	stampedStep, err := base.Create(beads.Bead{
		Title: "Stamped open step",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": wispRoot.ID,
			"gc.step_ref":     "mol-seth-patrol.open",
		},
	})
	if err != nil {
		t.Fatalf("Create(stamped step): %v", err)
	}
	intermediate, err := base.Create(beads.Bead{
		Title:    "Un-stamped finished step",
		Type:     "task",
		ParentID: wispRoot.ID,
	})
	if err != nil {
		t.Fatalf("Create(intermediate): %v", err)
	}
	grandchild, err := base.Create(beads.Bead{
		Title:    "Un-stamped stale chore",
		Type:     "task",
		ParentID: intermediate.ID,
	})
	if err != nil {
		t.Fatalf("Create(grandchild): %v", err)
	}
	if err := base.Close(intermediate.ID); err != nil {
		t.Fatalf("Close(intermediate): %v", err)
	}

	// depListFailStore pins that the batch path handles this store without
	// close ordering: a walk fallback or closeorder pass would DepList the
	// root and fail the sweep.
	store := depListFailStore{Store: base, failID: wispRoot.ID}

	result, err := sweepStaleOrderTrackingWithOptions(
		store,
		wispRoot.CreatedAt.Add(time.Hour),
		orderFilterForTest("seth-patrol"),
		true,
	)
	if err != nil {
		t.Fatalf("sweepStaleOrderTrackingWithOptions: %v", err)
	}
	if result.wispClosed != 3 {
		t.Fatalf("wispClosed = %d, want 3 (root, stamped step, grandchild behind closed intermediate)", result.wispClosed)
	}
	for _, id := range []string{wispRoot.ID, stampedStep.ID, grandchild.ID} {
		got, err := base.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status != "closed" {
			t.Fatalf("%s status = %q, want closed (grandchild must not be stranded)", id, got.Status)
		}
	}
}

func TestSweepStaleOrderTrackingBatchIgnoresTitleOnlyOrderRoots(t *testing.T) {
	base := beads.NewMemStore()

	labeledRoot, err := base.Create(beads.Bead{
		Title:  "mol-seth-patrol",
		Type:   "task",
		Labels: []string{"order-run:seth-patrol"},
		Metadata: map[string]string{
			"gc.kind": "workflow",
		},
	})
	if err != nil {
		t.Fatalf("Create(labeled root): %v", err)
	}
	labeledStep, err := base.Create(beads.Bead{
		Title: "Stamped open step",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": labeledRoot.ID,
			"gc.step_ref":     "mol-seth-patrol.open",
		},
	})
	if err != nil {
		t.Fatalf("Create(labeled step): %v", err)
	}
	// A workflow root that was never order-poured: no order-run label, but a
	// title that collides with the swept order name.
	titleRoot, err := base.Create(beads.Bead{
		Title: "order:seth-patrol",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind": "workflow",
		},
	})
	if err != nil {
		t.Fatalf("Create(title root): %v", err)
	}
	titleStep, err := base.Create(beads.Bead{
		Title: "Unrelated workflow step",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": titleRoot.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create(title step): %v", err)
	}

	result, err := sweepStaleOrderTrackingWithOptions(
		base,
		labeledRoot.CreatedAt.Add(time.Hour),
		orderFilterForTest("seth-patrol"),
		true,
	)
	if err != nil {
		t.Fatalf("sweepStaleOrderTrackingWithOptions: %v", err)
	}
	if result.wispClosed != 2 {
		t.Fatalf("wispClosed = %d, want 2 (labeled subtree only)", result.wispClosed)
	}
	for _, id := range []string{labeledRoot.ID, labeledStep.ID} {
		got, err := base.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", id, got.Status)
		}
	}
	for _, id := range []string{titleRoot.ID, titleStep.ID} {
		got, err := base.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status != "open" {
			t.Fatalf("title-only root subtree %s status = %q, want open (never order-poured)", id, got.Status)
		}
	}
}

func TestSweepStaleOrderTrackingWithoutWispsLeavesOpenWispSubtree(t *testing.T) {
	store := beads.NewMemStore()

	wispRoot, err := store.Create(beads.Bead{
		Title:  "mol-digest-generate",
		Type:   "molecule",
		Labels: []string{"order-run:digest"},
	})
	if err != nil {
		t.Fatalf("Create(wisp root): %v", err)
	}
	child, err := store.Create(beads.Bead{
		Title:    "draft-digest",
		ParentID: wispRoot.ID,
	})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}

	result, err := sweepStaleOrderTrackingWithOptions(
		store,
		wispRoot.CreatedAt.Add(time.Hour),
		nil,
		false,
	)
	if err != nil {
		t.Fatalf("sweepStaleOrderTrackingWithOptions: %v", err)
	}
	if result.wispClosed != 0 {
		t.Fatalf("wispClosed = %d, want 0", result.wispClosed)
	}
	gotChild, err := store.Get(child.ID)
	if err != nil {
		t.Fatalf("Get(child): %v", err)
	}
	if gotChild.Status != "open" {
		t.Fatalf("child status = %q, want open", gotChild.Status)
	}
}

func sweepStaleOrderTrackingWithOptions(store beads.Store, now time.Time, onlyOrders map[string]struct{}, includeWispSubtrees bool) (orderTrackingSweepResult, error) {
	return sweepStaleOrderTrackingWithOptionsLimit(store, now, time.Minute, onlyOrders, orderTrackingSweepMetadataInitiator, includeWispSubtrees, 0)
}

func TestSweepStaleOrderTrackingWithWispsClosesDeepestFirst(t *testing.T) {
	base := beads.NewMemStore()
	store := parentLastCloseStore{Store: base}

	wispRoot, err := base.Create(beads.Bead{
		Title:  "mol-digest-generate",
		Type:   "molecule",
		Labels: []string{"order-run:digest"},
	})
	if err != nil {
		t.Fatalf("Create(wisp root): %v", err)
	}
	child, err := base.Create(beads.Bead{
		Title:    "draft-digest",
		ParentID: wispRoot.ID,
	})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}
	grandchild, err := base.Create(beads.Bead{
		Title:    "nested-step-still-running",
		ParentID: child.ID,
	})
	if err != nil {
		t.Fatalf("Create(grandchild): %v", err)
	}

	result, err := sweepStaleOrderTrackingWithOptions(
		store,
		wispRoot.CreatedAt.Add(time.Hour),
		orderFilterForTest("digest"),
		true,
	)
	if err != nil {
		t.Fatalf("sweepStaleOrderTrackingWithOptions: %v", err)
	}
	if result.wispClosed != 3 {
		t.Fatalf("wispClosed = %d, want 3", result.wispClosed)
	}

	for _, id := range []string{wispRoot.ID, child.ID, grandchild.ID} {
		got, err := base.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", id, got.Status)
		}
	}
}

func TestSweepStaleOrderTrackingLimitOnlyAppliesToTrackingBeads(t *testing.T) {
	store := beads.NewMemStore()

	for _, name := range []string{"digest", "digest-retry"} {
		_, err := store.Create(beads.Bead{
			Title:     "order:" + name,
			Labels:    []string{"order-run:" + name, labelOrderTracking},
			Ephemeral: true,
		})
		if err != nil {
			t.Fatalf("Create(tracking %s): %v", name, err)
		}
	}
	wispRoot, err := store.Create(beads.Bead{
		Title:  "mol-digest-generate",
		Type:   "molecule",
		Labels: []string{"order-run:digest"},
	})
	if err != nil {
		t.Fatalf("Create(wisp root): %v", err)
	}
	child, err := store.Create(beads.Bead{
		Title:    "draft-digest",
		ParentID: wispRoot.ID,
	})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}
	grandchild, err := store.Create(beads.Bead{
		Title:    "nested-step-still-running",
		ParentID: child.ID,
	})
	if err != nil {
		t.Fatalf("Create(grandchild): %v", err)
	}

	result, err := sweepStaleOrderTrackingWithOptionsLimit(
		store,
		time.Now().Add(time.Hour),
		time.Minute,
		orderFilterForTest("digest", "digest-retry"),
		orderTrackingSweepMetadataInitiator,
		true,
		1,
	)
	if err != nil {
		t.Fatalf("sweepStaleOrderTrackingWithOptionsLimit: %v", err)
	}
	if result.trackingClosed != 1 {
		t.Fatalf("trackingClosed = %d, want 1", result.trackingClosed)
	}
	if result.wispClosed != 3 {
		t.Fatalf("wispClosed = %d, want 3", result.wispClosed)
	}
	for _, id := range []string{wispRoot.ID, child.ID, grandchild.ID} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", id, got.Status)
		}
	}
}

func TestSweepStaleOrderTrackingWithWispsClosesRootOnlyWisp(t *testing.T) {
	store := beads.NewMemStore()

	wispRoot, err := store.Create(beads.Bead{
		Title:  "vapor-order-root",
		Type:   "task",
		Labels: []string{"order-run:digest"},
		Metadata: map[string]string{
			"gc.kind": "wisp",
		},
	})
	if err != nil {
		t.Fatalf("Create(wisp root): %v", err)
	}

	result, err := sweepStaleOrderTrackingWithOptions(
		store,
		wispRoot.CreatedAt.Add(time.Hour),
		orderFilterForTest("digest"),
		true,
	)
	if err != nil {
		t.Fatalf("sweepStaleOrderTrackingWithOptions: %v", err)
	}
	if result.wispClosed != 1 {
		t.Fatalf("wispClosed = %d, want 1", result.wispClosed)
	}
	got, err := store.Get(wispRoot.ID)
	if err != nil {
		t.Fatalf("Get(wisp root): %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("wisp root status = %q, want closed", got.Status)
	}
}

func TestSweepStaleOrderTrackingWithWispsSkipsFreshOpenDescendant(t *testing.T) {
	store := beads.NewMemStore()

	wispRoot, err := store.Create(beads.Bead{
		Title:  "mol-digest-generate",
		Type:   "molecule",
		Labels: []string{"order-run:digest"},
	})
	if err != nil {
		t.Fatalf("Create(wisp root): %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	child, err := store.Create(beads.Bead{
		Title:    "fresh-step",
		ParentID: wispRoot.ID,
	})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}
	cutoff := wispRoot.CreatedAt.Add(child.CreatedAt.Sub(wispRoot.CreatedAt) / 2)

	closed, err := sweepStaleOrderWispSubtrees(store, cutoff, orderFilterForTest("digest"))
	if err != nil {
		t.Fatalf("sweepStaleOrderWispSubtrees: %v", err)
	}
	if closed != 0 {
		t.Fatalf("closed = %d, want 0", closed)
	}
	for _, id := range []string{wispRoot.ID, child.ID} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status != "open" {
			t.Fatalf("%s status = %q, want open", id, got.Status)
		}
	}
}

func TestSweepStaleOrderTrackingWithWispsClosesGraphDependentSubtree(t *testing.T) {
	store := beads.NewMemStore()

	wispRoot, err := store.Create(beads.Bead{
		Title:  "graph-workflow-digest",
		Type:   "task",
		Labels: []string{"order-run:digest"},
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	if err != nil {
		t.Fatalf("Create(wisp root): %v", err)
	}
	step, err := store.Create(beads.Bead{
		Title: "draft-digest",
		Metadata: map[string]string{
			"gc.root_bead_id": wispRoot.ID,
			"gc.step_ref":     "draft",
		},
	})
	if err != nil {
		t.Fatalf("Create(graph step): %v", err)
	}
	if err := store.DepAdd(step.ID, wispRoot.ID, "tracks"); err != nil {
		t.Fatalf("DepAdd(tracks): %v", err)
	}

	closed, err := sweepStaleOrderWispSubtrees(
		store,
		step.CreatedAt.Add(time.Minute),
		orderFilterForTest("digest"),
	)
	if err != nil {
		t.Fatalf("sweepStaleOrderWispSubtrees: %v", err)
	}
	if closed != 2 {
		t.Fatalf("closed = %d, want root and graph step closed", closed)
	}
	for _, id := range []string{wispRoot.ID, step.ID} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", id, got.Status)
		}
	}
}

func TestSweepStaleOrderTrackingWithWispsClosesSameWorkflowGraphDependencyTypes(t *testing.T) {
	for _, depType := range []string{"blocks", "parent-child"} {
		t.Run(depType, func(t *testing.T) {
			store := beads.NewMemStore()

			wispRoot, err := store.Create(beads.Bead{
				Title:  "graph-workflow-digest",
				Type:   "task",
				Labels: []string{"order-run:digest"},
				Metadata: map[string]string{
					"gc.kind":             "workflow",
					"gc.formula_contract": "graph.v2",
				},
			})
			if err != nil {
				t.Fatalf("Create(wisp root): %v", err)
			}
			step, err := store.Create(beads.Bead{
				Title: "draft-digest",
				Metadata: map[string]string{
					"gc.root_bead_id": wispRoot.ID,
					"gc.step_ref":     "draft",
				},
			})
			if err != nil {
				t.Fatalf("Create(graph step): %v", err)
			}
			if err := store.DepAdd(step.ID, wispRoot.ID, depType); err != nil {
				t.Fatalf("DepAdd(%s): %v", depType, err)
			}

			closed, err := sweepStaleOrderWispSubtrees(
				store,
				step.CreatedAt.Add(time.Minute),
				orderFilterForTest("digest"),
			)
			if err != nil {
				t.Fatalf("sweepStaleOrderWispSubtrees: %v", err)
			}
			if closed != 2 {
				t.Fatalf("closed = %d, want root and graph step closed", closed)
			}
			for _, id := range []string{wispRoot.ID, step.ID} {
				got, err := store.Get(id)
				if err != nil {
					t.Fatalf("Get(%s): %v", id, err)
				}
				if got.Status != "closed" {
					t.Fatalf("%s status = %q, want closed", id, got.Status)
				}
			}
		})
	}
}

func TestSweepStaleOrderTrackingWithWispsIgnoresForeignGraphDependents(t *testing.T) {
	tests := []struct {
		name     string
		depType  string
		metadata map[string]string
	}{
		{
			name:    "external convoy tracks edge",
			depType: "tracks",
		},
		{
			name:    "unrelated downstream blocks edge",
			depType: "blocks",
			metadata: map[string]string{
				"gc.root_bead_id": "other-workflow-root",
				"gc.step_ref":     "external",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := beads.NewMemStore()

			wispRoot, err := store.Create(beads.Bead{
				Title:  "graph-workflow-digest",
				Type:   "task",
				Labels: []string{"order-run:digest"},
				Metadata: map[string]string{
					"gc.kind":             "workflow",
					"gc.formula_contract": "graph.v2",
				},
			})
			if err != nil {
				t.Fatalf("Create(wisp root): %v", err)
			}
			external, err := store.Create(beads.Bead{
				Title:    "external-dependent",
				Metadata: tt.metadata,
			})
			if err != nil {
				t.Fatalf("Create(external): %v", err)
			}
			if err := store.DepAdd(external.ID, wispRoot.ID, tt.depType); err != nil {
				t.Fatalf("DepAdd(%s): %v", tt.depType, err)
			}

			closed, err := sweepStaleOrderWispSubtrees(
				store,
				external.CreatedAt.Add(time.Minute),
				orderFilterForTest("digest"),
			)
			if err != nil {
				t.Fatalf("sweepStaleOrderWispSubtrees: %v", err)
			}
			if closed != 0 {
				t.Fatalf("closed = %d, want foreign dependent ignored", closed)
			}
			for _, id := range []string{wispRoot.ID, external.ID} {
				got, err := store.Get(id)
				if err != nil {
					t.Fatalf("Get(%s): %v", id, err)
				}
				if got.Status != "open" {
					t.Fatalf("%s status = %q, want open", id, got.Status)
				}
			}
		})
	}
}

func TestSweepStaleOrderTrackingWithWispsPropagatesDescendantListError(t *testing.T) {
	base := beads.NewMemStore()

	wispRoot, err := base.Create(beads.Bead{
		Title:  "mol-digest-generate",
		Type:   "molecule",
		Labels: []string{"order-run:digest"},
	})
	if err != nil {
		t.Fatalf("Create(wisp root): %v", err)
	}
	store := parentListFailStore{
		Store:        base,
		failParentID: wispRoot.ID,
		err:          fmt.Errorf("child list unavailable"),
	}

	closed, err := sweepStaleOrderWispSubtrees(
		store,
		wispRoot.CreatedAt.Add(time.Minute),
		orderFilterForTest("digest"),
	)
	if err == nil {
		t.Fatal("sweepStaleOrderWispSubtrees err = nil, want descendant-list error")
	}
	if closed != 0 {
		t.Fatalf("closed = %d, want 0", closed)
	}
	if !strings.Contains(err.Error(), "checking stale wisp descendants") {
		t.Fatalf("err = %q, want descendant context", err)
	}
}

func TestSweepStaleOrderTrackingWithWispsPropagatesCloseOrderError(t *testing.T) {
	base := beads.NewMemStore()

	wispRoot, err := base.Create(beads.Bead{
		Title:  "mol-digest-generate",
		Type:   "molecule",
		Labels: []string{"order-run:digest"},
	})
	if err != nil {
		t.Fatalf("Create(wisp root): %v", err)
	}
	child, err := base.Create(beads.Bead{
		Title:    "draft-digest",
		ParentID: wispRoot.ID,
	})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}
	store := depListFailStore{Store: base, failID: child.ID}

	closed, err := sweepStaleOrderWispSubtrees(
		store,
		wispRoot.CreatedAt.Add(time.Minute),
		orderFilterForTest("digest"),
	)
	if err == nil {
		t.Fatal("sweepStaleOrderWispSubtrees err = nil, want close-order error")
	}
	if closed != 0 {
		t.Fatalf("closed = %d, want 0", closed)
	}
	if !strings.Contains(err.Error(), "ordering stale order wisp closes") {
		t.Fatalf("err = %q, want close-order context", err)
	}
}

func TestSweepStaleOrderTrackingLimit_ClosesAtMostBudget(t *testing.T) {
	store := beads.NewMemStore()
	now := time.Now()

	ids := make([]string, 0, 4)
	for _, name := range []string{"one", "two", "three", "four"} {
		b, err := store.Create(beads.Bead{
			Title:     "order:" + name,
			Labels:    []string{"order-run:" + name, labelOrderTracking},
			Ephemeral: true,
		})
		if err != nil {
			t.Fatalf("Create(%s): %v", name, err)
		}
		ids = append(ids, b.ID)
	}

	closed, err := sweepStaleOrderTrackingLimit(store, now.Add(time.Hour), time.Minute, nil, orderTrackingWatchdogMetadataInitiator, 3)
	if err != nil {
		t.Fatalf("sweepStaleOrderTrackingLimit: %v", err)
	}
	if closed != 3 {
		t.Fatalf("closed = %d, want 3", closed)
	}

	closedCount := 0
	for _, id := range ids {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status == "closed" {
			closedCount++
			if got.Metadata["close_reason"] != staleOrderTrackingCloseReason {
				t.Fatalf("close_reason for %s = %q, want %q", id, got.Metadata["close_reason"], staleOrderTrackingCloseReason)
			}
			if got.Metadata["order_tracking_sweep_by"] != orderTrackingWatchdogMetadataInitiator {
				t.Fatalf("order_tracking_sweep_by for %s = %q, want %q", id, got.Metadata["order_tracking_sweep_by"], orderTrackingWatchdogMetadataInitiator)
			}
		}
	}
	if closedCount != 3 {
		t.Fatalf("closed tracking beads = %d, want 3", closedCount)
	}
}

func TestStartupSweepThenBuildDispatcher(t *testing.T) {
	store := beads.NewMemStore()

	// Pre-create an orphaned tracking bead (simulating a crashed controller).
	_, err := store.Create(beads.Bead{
		Title:  "order:test-order",
		Labels: []string{"order-run:test-order", labelOrderTracking},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Production startup sequence: sweep first, then build dispatcher.
	// This mirrors newCityRuntime which calls sweepOrphanedOrderTrackingRetry
	// before buildOrderDispatcher. The sweep is intentionally NOT inside
	// buildOrderDispatcher so config reloads don't close in-flight beads.
	closed, err := sweepOrphanedOrderTrackingRetry(store, 3, time.Millisecond)
	if err != nil {
		t.Fatalf("sweepOrphanedOrderTrackingRetry: %v", err)
	}
	if closed != 1 {
		t.Fatalf("closed = %d, want 1", closed)
	}

	aa := []orders.Order{{
		Name:     "test-order",
		Trigger:  "cooldown",
		Interval: "1m",
		Formula:  "test-formula",
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	// The orphaned bead should have been closed before dispatcher construction.
	all := trackingBeads(t, store, labelOrderTracking)
	for _, b := range all {
		if b.Status != "closed" {
			t.Errorf("orphaned tracking bead %s still open after startup sweep", b.ID)
		}
	}
}

func TestStartupSweepPreservesTriggerEnvFailureMarker(t *testing.T) {
	store := beads.NewMemStore()

	marker, err := store.Create(beads.Bead{
		Title:     "order:pg-cooldown",
		Labels:    []string{"order-run:pg-cooldown", labelOrderTracking, labelTriggerEnvFailed},
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	closed, err := sweepOrphanedOrderTrackingRetry(store, 3, time.Millisecond)
	if err != nil {
		t.Fatalf("sweepOrphanedOrderTrackingRetry: %v", err)
	}
	if closed != 0 {
		t.Fatalf("closed = %d, want 0", closed)
	}
	gotMarker, err := store.Get(marker.ID)
	if err != nil {
		t.Fatalf("Get(marker): %v", err)
	}
	if gotMarker.Status != "open" {
		t.Fatalf("trigger env failure marker status = %q, want open", gotMarker.Status)
	}

	order := orders.Order{Name: "pg-cooldown", Trigger: "cooldown", Interval: "1s", Exec: "true"}
	rec := memRecorder{}
	rec.Record(events.Event{Type: events.OrderFailed, Subject: order.Name, Message: "building trigger env: previous failure"})
	var stderr bytes.Buffer
	ad := buildOrderDispatcherFromListExec([]orders.Order{order}, triggerEvaluationFailStore{
		Store:        store,
		lastRunOrder: order.Name,
	}, nil, successfulExec, &rec)
	mad := ad.(*memoryOrderDispatcher)
	mad.stderr = &stderr

	mad.dispatch(context.Background(), t.TempDir(), time.Now().Add(2*time.Second))
	mad.drain(context.Background())

	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want preserved trigger-env marker to suppress trigger evaluation", stderr.String())
	}
	rec.mu.Lock()
	eventsSnapshot := append([]events.Event(nil), rec.events...)
	rec.mu.Unlock()
	failedEvents := 0
	for _, event := range eventsSnapshot {
		if event.Type == events.OrderFailed && event.Subject == order.Name {
			failedEvents++
		}
	}
	if failedEvents != 1 {
		t.Fatalf("order.failed count after restart-like sweep and dispatch = %d, want 1", failedEvents)
	}
}

// TestSweepOrphanedOrderTracking_StampsCloseReason verifies that the
// startup-time orphan sweep also stamps close_reason. The original
// callsite passed nil metadata; under validation.on-close=error this
// silently failed and left orphaned tracking beads open.
func TestSweepOrphanedOrderTracking_StampsCloseReason(t *testing.T) {
	if got := len(orphanedOrderTrackingCloseReason); got < 20 {
		t.Fatalf("orphanedOrderTrackingCloseReason = %q (%d chars), want >=20", orphanedOrderTrackingCloseReason, got)
	}

	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{
		Title:  "order:dolt-health",
		Labels: []string{"order-run:dolt-health", labelOrderTracking},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	closed, err := sweepOrphanedOrderTracking(store)
	if err != nil {
		t.Fatalf("sweepOrphanedOrderTracking: %v", err)
	}
	if closed != 1 {
		t.Fatalf("closed = %d, want 1", closed)
	}

	final, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.Status != "closed" {
		t.Fatalf("status = %q, want closed", final.Status)
	}
	if got := final.Metadata["close_reason"]; got != orphanedOrderTrackingCloseReason {
		t.Errorf("close_reason = %q, want %q", got, orphanedOrderTrackingCloseReason)
	}
}

// TestSweepStaleOrderTracking_StampsCloseReason verifies that the
// runtime stale sweep (called every tick by the order-tracking-sweep
// order AND every 30s by the controller's runtime watchdog) stamps
// close_reason. The pre-fix callsite stamped order_tracking_sweep
// metadata but no close_reason; under validation.on-close=error every
// close was rejected, the watchdog retried indefinitely, and order
// firing silently wedged.
func TestSweepStaleOrderTracking_StampsCloseReason(t *testing.T) {
	if got := len(staleOrderTrackingCloseReason); got < 20 {
		t.Fatalf("staleOrderTrackingCloseReason = %q (%d chars), want >=20", staleOrderTrackingCloseReason, got)
	}

	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{
		Title:  "order:dolt-health",
		Labels: []string{"order-run:dolt-health", labelOrderTracking},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Push the bead's CreatedAt into the past so it's outside the
	// staleAfter window. MemStore stamps CreatedAt at Create time, but
	// callers expose UpdateMetadata; we round-trip via direct field
	// access on the returned bead by re-creating with the desired
	// timestamp via the underlying clock isn't available, so we set
	// staleAfter to a negative duration relative to the bead and pass
	// a future "now" that's well past CreatedAt.
	now := b.CreatedAt.Add(time.Hour)

	closed, err := sweepStaleOrderTracking(store, now, time.Minute, nil, orderTrackingWatchdogMetadataInitiator)
	if err != nil {
		t.Fatalf("sweepStaleOrderTracking: %v", err)
	}
	if closed != 1 {
		t.Fatalf("closed = %d, want 1", closed)
	}

	final, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.Status != "closed" {
		t.Fatalf("status = %q, want closed", final.Status)
	}
	if got := final.Metadata["close_reason"]; got != staleOrderTrackingCloseReason {
		t.Errorf("close_reason = %q, want %q", got, staleOrderTrackingCloseReason)
	}
	// Pre-fix metadata (order_tracking_sweep + initiator) must still be
	// stamped — the new close_reason is additive, not a replacement.
	if got := final.Metadata["order_tracking_sweep"]; got != orderTrackingSweepMetadataReason {
		t.Errorf("order_tracking_sweep = %q, want %q", got, orderTrackingSweepMetadataReason)
	}
	if got := final.Metadata["order_tracking_sweep_by"]; got != orderTrackingWatchdogMetadataInitiator {
		t.Errorf("order_tracking_sweep_by = %q, want %q", got, orderTrackingWatchdogMetadataInitiator)
	}
}

func TestSweepOrphanedOrderTracking_RetryOnTransientError(t *testing.T) {
	inner := beads.NewMemStore()
	_, err := inner.Create(beads.Bead{
		Title:  "order:test",
		Labels: []string{"order-run:test", labelOrderTracking},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Fail the first 2 ListByLabel calls, succeed on the 3rd.
	fs := &countFailStore{Store: inner, failCount: 2}
	closed, err := sweepOrphanedOrderTrackingRetry(fs, 3, time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error after retry: %v", err)
	}
	if closed != 1 {
		t.Fatalf("closed = %d, want 1", closed)
	}
	if fs.calls != 3 {
		t.Fatalf("ListByLabel calls = %d, want 3", fs.calls)
	}
}

func TestSweepOrphanedOrderTracking_RetryExhausted(t *testing.T) {
	inner := beads.NewMemStore()
	_, err := inner.Create(beads.Bead{
		Title:  "order:test",
		Labels: []string{"order-run:test", labelOrderTracking},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Fail all 3 attempts.
	fs := &countFailStore{Store: inner, failCount: 3}
	_, err = sweepOrphanedOrderTrackingRetry(fs, 3, time.Millisecond)
	if err == nil {
		t.Fatal("expected error when retries exhausted")
	}
	if fs.calls != 3 {
		t.Fatalf("ListByLabel calls = %d, want 3", fs.calls)
	}
}

func TestSweepOrphanedOrderTracking_RetryOnPartialClose(t *testing.T) {
	inner := beads.NewMemStore()
	_, err := inner.Create(beads.Bead{
		Title:  "order:test",
		Labels: []string{"order-run:test", labelOrderTracking},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// closeFailStore returns (1, err) from every CloseAll call — simulating
	// a partial close that keeps erroring. The retry loop MUST retry because
	// beads.Store.CloseAll skips already-closed beads, so retrying after a
	// partial close is safe. We verify the total count accumulates across
	// attempts and the final error is wrapped with the attempt count.
	fs := &closeFailStore{Store: inner, closeN: 1}
	n, err := sweepOrphanedOrderTrackingRetry(fs, 3, time.Millisecond)
	if err == nil {
		t.Fatal("expected error from CloseAll failure")
	}
	if !strings.Contains(err.Error(), "after 3 attempts") {
		t.Fatalf("error = %q, want attempt count in message", err.Error())
	}
	// Each of 3 attempts closes 1 bead → total = 3.
	if n != 3 {
		t.Fatalf("n = %d, want 3 (accumulated across retries)", n)
	}
	if fs.listCalls != 3 {
		t.Fatalf("ListByLabel calls = %d, want 3 (retry on partial close)", fs.listCalls)
	}
}

// countFailStore wraps a Store and fails the first N ListByLabel calls.
type countFailStore struct {
	beads.Store
	failCount int
	calls     int
}

func (f *countFailStore) ListByLabel(label string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	f.calls++
	if f.calls <= f.failCount {
		return nil, fmt.Errorf("connection refused")
	}
	return f.Store.ListByLabel(label, limit, opts...)
}

// closeFailStore wraps a Store and always fails CloseAll with a
// configurable partial-close count.
type closeFailStore struct {
	beads.Store
	listCalls int
	closeN    int // number of beads "closed" before error
}

func (f *closeFailStore) ListByLabel(label string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	f.listCalls++
	return f.Store.ListByLabel(label, limit, opts...)
}

func (f *closeFailStore) CloseAll(_ []string, _ map[string]string) (int, error) {
	return f.closeN, fmt.Errorf("close failed")
}

type labelFailListStore struct {
	beads.Store
	failLabel string
}

func (s labelFailListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Label == s.failLabel {
		return nil, fmt.Errorf("list failed for %s", query.Label)
	}
	return s.Store.List(query)
}

// --- helpers ---

func successfulExec(context.Context, string, string, []string) ([]byte, error) {
	return nil, nil
}

// buildOrderDispatcherFromList builds a dispatcher from pre-scanned orders,
// bypassing the filesystem scan. Returns nil if no auto-dispatchable orders.
func buildOrderDispatcherFromList(aa []orders.Order, store beads.Store, ep events.Provider) orderDispatcher { //nolint:unparam // ep is nil in current tests but needed for event-gate tests
	return buildOrderDispatcherFromListExec(aa, store, ep, nil, nil)
}

// buildOrderDispatcherFromListExec builds a dispatcher with exec runner support.
func buildOrderDispatcherFromListExec(aa []orders.Order, store beads.Store, ep events.Provider, execRun ExecRunner, rec events.Recorder) orderDispatcher {
	var auto []orders.Order
	cfg := &config.City{}
	seenRigs := make(map[string]bool)
	for _, a := range aa {
		if a.Trigger != "manual" {
			auto = append(auto, a)
		}
		if a.Rig != "" && !seenRigs[a.Rig] {
			cfg.Rigs = append(cfg.Rigs, config.Rig{Name: a.Rig, Path: a.Rig})
			seenRigs[a.Rig] = true
		}
	}
	if len(auto) == 0 {
		return nil
	}
	if rec == nil {
		rec = events.Discard
	}
	if execRun == nil {
		execRun = shellExecRunner
	}
	dispatchCtx, dispatchCancel := context.WithCancel(context.Background())
	return &memoryOrderDispatcher{
		aa: auto,
		storeFn: func(_ execStoreTarget) (beads.Store, error) {
			return store, nil
		},
		ep:                   ep,
		execRun:              execRun,
		rec:                  rec,
		stderr:               lockedStderr(&bytes.Buffer{}),
		maxDispatchesPerTick: defaultMaxOrderDispatchesPerTick,
		cfg:                  cfg,
		dispatchCtx:          dispatchCtx,
		dispatchCancel:       dispatchCancel,
	}
}

func slicesContain(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func orderDispatchTestEnv(t *testing.T, envCh <-chan []string) map[string]string {
	t.Helper()
	select {
	case entries := <-envCh:
		env := map[string]string{}
		for _, entry := range entries {
			key, value, ok := strings.Cut(entry, "=")
			if ok {
				env[key] = value
			}
		}
		return env
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for order exec env")
	}
	return nil
}

// --- rig-scoped dispatch tests ---

func TestBuildOrderDispatcherWithRigs(t *testing.T) {
	// Build a config with rig formula layers that include orders.
	rigDir := t.TempDir()
	rigLayer := filepath.Join(rigDir, "formulas")
	// Create an order in the rig-exclusive layer.
	orderDir := filepath.Join(rigDir, "orders")
	for _, dir := range []string{rigLayer, orderDir} {
		if err := mkdirAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(orderDir, "rig-health.toml"), `[order]
formula = "mol-rig-health"
trigger = "cooldown"
interval = "5m"
pool = "polecat"
`)

	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{"/nonexistent/city-layer"}, // no city orders
			Rigs: map[string][]string{
				"demo": {"/nonexistent/city-layer", rigLayer},
			},
		},
	}

	var stderr bytes.Buffer
	ad := buildOrderDispatcher(t.TempDir(), cfg, events.Discard, &stderr)
	if ad == nil {
		t.Fatalf("expected non-nil dispatcher; stderr: %s", stderr.String())
	}

	mad := ad.(*memoryOrderDispatcher)
	if len(mad.aa) != 1 {
		t.Fatalf("got %d orders, want 1", len(mad.aa))
	}
	if mad.aa[0].Rig != "demo" {
		t.Errorf("order Rig = %q, want %q", mad.aa[0].Rig, "demo")
	}
	if mad.aa[0].Name != "rig-health" {
		t.Errorf("order Name = %q, want %q", mad.aa[0].Name, "rig-health")
	}
}

func TestOrderDispatchRigScoped(t *testing.T) {
	store := beads.NewMemStore()

	aa := []orders.Order{{
		Name:         "db-health",
		Trigger:      "cooldown",
		Interval:     "1m",
		Formula:      "mol-db-health",
		Pool:         "polecat",
		Rig:          "demo-repo",
		FormulaLayer: sharedTestFormulaDir,
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	ad.drain(context.Background())

	work := workBeadByOrderLabel(t, store, "order-run:db-health:rig:demo-repo")
	if !slicesContain(work.Labels, "order-run:db-health:rig:demo-repo") {
		t.Errorf("missing scoped order-run label, got %v", work.Labels)
	}
	if work.Metadata["gc.routed_to"] != "demo-repo/polecat" {
		t.Errorf("gc.routed_to = %q, want %q", work.Metadata["gc.routed_to"], "demo-repo/polecat")
	}
}

func TestOrderDispatchRigCooldownIndependent(t *testing.T) {
	store := beads.NewMemStore()

	// Seed a recent run for rig-A's order (scoped name).
	_, err := store.Create(beads.Bead{
		Title:  "order run",
		Labels: []string{"order-run:db-health:rig:rig-a"},
	})
	if err != nil {
		t.Fatal(err)
	}

	aa := []orders.Order{
		{Name: "db-health", Trigger: "cooldown", Interval: "1h", Formula: "mol-db-health", Rig: "rig-a"},
		{Name: "db-health", Trigger: "cooldown", Interval: "1h", Formula: "mol-db-health", Rig: "rig-b"},
	}
	ad := buildOrderDispatcherFromList(aa, store, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	ad.drain(context.Background())

	// rig-b should have a tracking bead, rig-a should not.
	all := trackingBeads(t, store, "order-run:db-health:rig:rig-b")
	rigBTracked := false
	rigATracked := false
	for _, b := range all {
		for _, l := range b.Labels {
			if l == "order-run:db-health:rig:rig-b" {
				rigBTracked = true
			}
			// Check that no NEW bead was created for rig-a (only the seed).
			// The seed bead is the only one with rig-a label.
		}
	}
	if !rigBTracked {
		t.Error("missing tracking bead for rig-b")
	}

	// Count rig-a beads — should be exactly 1 (the seed).
	rigAAll := trackingBeads(t, store, "order-run:db-health:rig:rig-a")
	rigACount := 0
	for _, b := range rigAAll {
		for _, l := range b.Labels {
			if l == "order-run:db-health:rig:rig-a" {
				rigACount++
			}
		}
	}
	if rigACount != 1 {
		t.Errorf("rig-a bead count = %d, want 1 (seed only)", rigACount)
	}
	_ = rigATracked
}

func TestRigExclusiveLayers(t *testing.T) {
	city := []string{"/city/topo", "/city/local"}
	rig := []string{"/city/topo", "/city/local", "/rig/topo", "/rig/local"}

	got := rigExclusiveLayers(rig, city)
	if len(got) != 2 {
		t.Fatalf("got %d layers, want 2", len(got))
	}
	if got[0] != "/rig/topo" || got[1] != "/rig/local" {
		t.Errorf("got %v, want [/rig/topo /rig/local]", got)
	}
}

func TestRigExclusiveLayersNoCityPrefix(t *testing.T) {
	// Rig shorter than city → no exclusive layers.
	got := rigExclusiveLayers([]string{"/x"}, []string{"/a", "/b"})
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestQualifyPool(t *testing.T) {
	cityBindingCfg := &config.City{Agents: []config.Agent{
		{Name: "dog", BindingName: "maintenance"},
	}}
	cityNoBindingCfg := &config.City{Agents: []config.Agent{
		{Name: "dog"},
	}}
	rigBindingCfg := &config.City{Agents: []config.Agent{
		{Name: "dog", BindingName: "foo", Dir: "api"},
	}}
	ambiguousCfg := &config.City{Agents: []config.Agent{
		{Name: "dog", BindingName: "gastown"},
		{Name: "dog", BindingName: "maintenance"},
	}}
	importedOnlyCollisionCfg := &config.City{Agents: []config.Agent{
		{Name: "dog", BindingName: "maintenance", SourceDir: "/city/packs/maintenance"},
		{Name: "dog", BindingName: "gastown", SourceDir: "/city/packs/gastown"},
	}}
	importedShadowCfg := &config.City{Agents: []config.Agent{
		{Name: "dog"},
		{Name: "dog", BindingName: "maintenance", SourceDir: "/city/packs/maintenance"},
		{Name: "dog", BindingName: "gastown", SourceDir: "/city/packs/gastown"},
	}}
	materializedPackCfg := &config.City{Agents: []config.Agent{
		{Name: "dog"},
		{Name: "dog", BindingName: "dolt", PackName: "dolt", SourceDir: "/city/.gc/system/packs/bd/dolt"},
	}}
	transitiveNestedPackCfg := &config.City{Agents: []config.Agent{
		{Name: "dog", BindingName: "wrapper", PackName: "gastown", SourceDir: "/repo/examples/gastown/packs/gastown"},
		{Name: "dog", BindingName: "dolt", PackName: "dolt", SourceDir: "/city/.gc/system/packs/bd/dolt"},
	}}
	transitiveClosureCfg := &config.City{Agents: []config.Agent{
		{Name: "mayor", BindingName: "wrapper", PackName: "gastown", SourceDir: "/repo/examples/gastown/packs/gastown"},
		{Name: "dog", BindingName: "wrapper", PackName: "maintenance", SourceDir: "/repo/examples/gastown/packs/maintenance"},
		{Name: "dog", BindingName: "dolt", PackName: "dolt", SourceDir: "/city/.gc/system/packs/bd/dolt"},
	}}
	sameTailShadowForkCfg := &config.City{Agents: []config.Agent{
		{Name: "dog", BindingName: "fork", PackName: "gastown", SourceDir: "/city/packs/gastown"},
		{Name: "dog", BindingName: "gastown", PackName: "gastown", SourceDir: "/city/.gc/system/packs/gastown"},
	}}
	rigWithCityFallbackCfg := &config.City{Agents: []config.Agent{
		{Name: "dog", BindingName: "maintenance"},
	}}
	rigShadowCfg := &config.City{Agents: []config.Agent{
		{Name: "dog", BindingName: "maintenance"},
		{Name: "dog", BindingName: "foo", Dir: "api"},
	}}

	tests := []struct {
		name          string
		cfg           *config.City
		pool, rig     string
		sourceDirHint string
		want          string
		wantErr       string
	}{
		// Existing behavior preserved when cfg is nil (call sites that
		// don't have a loaded city, e.g. TestOrderRun fixtures).
		{"nil cfg city order", nil, "dog", "", "", "dog", ""},
		{"nil cfg rig order", nil, "polecat", "demo-repo", "", "demo-repo/polecat", ""},
		{"nil cfg pre-rig-qualified", nil, "demo-repo/polecat", "demo-repo", "", "demo-repo/polecat", ""},

		// Already-qualified passthroughs.
		{"already rig-qualified passthrough", cityBindingCfg, "demo-repo/dog", "", "", "demo-repo/dog", ""},
		{"already binding-qualified passthrough", cityBindingCfg, "maintenance.dog", "", "", "maintenance.dog", ""},
		{"binding-qualified gets rig prefix", rigBindingCfg, "foo.dog", "api", "", "api/foo.dog", ""},

		// City-order binding lookup (the bug fix).
		{"city order resolves binding", cityBindingCfg, "dog", "", "", "maintenance.dog", ""},
		{"city order no binding agent", cityNoBindingCfg, "dog", "", "", "dog", ""},
		{"city order miss falls through", cityBindingCfg, "wolf", "", "", "wolf", ""},
		{"city local shadow wins without hint", importedShadowCfg, "dog", "", "", "dog", ""},
		{"no hint stays ambiguous", importedOnlyCollisionCfg, "dog", "", "", "", `ambiguous pool "dog" for city order: matches maintenance.dog, gastown.dog`},
		{"source hint beats city shadow", importedShadowCfg, "dog", "", "/city/packs/maintenance", "maintenance.dog", ""},
		{"source hint beats sibling import collision", importedShadowCfg, "dog", "", "/city/packs/gastown", "gastown.dog", ""},
		{"source checkout hint matches materialized same pack", materializedPackCfg, "dog", "", "/repo/examples/bd/dolt", "dolt.dog", ""},
		{"source hint ignores unrelated nested materialized pack", transitiveNestedPackCfg, "dog", "", "/repo/examples/gastown/packs/gastown", "wrapper.dog", ""},
		{"source hint carries transitive import binding context", transitiveClosureCfg, "dog", "", "/repo/examples/gastown/packs/gastown", "wrapper.dog", ""},

		// Distinct packs sharing the same two-component source tail (a
		// city-local fork plus the builtin pack materialized under
		// .gc/system) must resolve by exact SourceDir, not go ambiguous
		// because the other pack tail-matches.
		{"same-tail distinct packs prefer exact fork source", sameTailShadowForkCfg, "dog", "", "/city/packs/gastown", "fork.dog", ""},
		{"same-tail distinct packs prefer exact materialized source", sameTailShadowForkCfg, "dog", "", "/city/.gc/system/packs/gastown", "gastown.dog", ""},

		// Rig-order binding lookup.
		{"rig order resolves binding", rigBindingCfg, "dog", "api", "", "api/foo.dog", ""},
		{"rig order falls back to city binding", rigWithCityFallbackCfg, "dog", "api", "", "maintenance.dog", ""},
		{"rig order binding-qualified city fallback", rigWithCityFallbackCfg, "maintenance.dog", "api", "", "maintenance.dog", ""},
		{"rig order local binding shadows city fallback", rigShadowCfg, "dog", "api", "", "api/foo.dog", ""},

		// Ambiguity is a hard failure — dispatch must not recreate the
		// original bare-name route/scaler mismatch.
		{"ambiguous bindings fail", ambiguousCfg, "dog", "", "", "", `ambiguous pool "dog" for city order: matches gastown.dog, maintenance.dog`},

		// Unresolved dotted pools preserve the legacy pass-through behavior.
		{"unresolved dotted pool passes through", cityBindingCfg, "team.alpha", "", "", "team.alpha", ""},

		// Empty/edge cases.
		{"empty cfg agents", &config.City{}, "dog", "", "", "dog", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := qualifyPool(tt.pool, tt.rig, tt.cfg, tt.sourceDirHint)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("qualifyPool(%q, %q, cfg) error = nil, want %q", tt.pool, tt.rig, tt.wantErr)
				}
				if err.Error() != tt.wantErr {
					t.Fatalf("qualifyPool(%q, %q, cfg) error = %q, want %q", tt.pool, tt.rig, err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("qualifyPool(%q, %q, cfg) error = %v", tt.pool, tt.rig, err)
			}
			if got != tt.want {
				t.Errorf("qualifyPool(%q, %q, cfg) = %q, want %q", tt.pool, tt.rig, got, tt.want)
			}
		})
	}
}

// --- city pack layer tests ---

func TestBuildOrderDispatcherUsesProviderAwareFileStore(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityDir := t.TempDir()
	layerDir := filepath.Join(cityDir, "formulas")
	orderDir := filepath.Join(cityDir, "orders")
	for _, dir := range []string{layerDir, orderDir} {
		if err := mkdirAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(orderDir, "file-order.toml"), `[order]
formula = "test-formula"
trigger = "cooldown"
interval = "1m"
pool = "worker"
`)
	formulaText, err := os.ReadFile(filepath.Join(sharedTestFormulaDir, "test-formula.toml"))
	if err != nil {
		t.Fatalf("ReadFile(test-formula): %v", err)
	}
	writeFile(t, filepath.Join(layerDir, "test-formula.toml"), string(formulaText))

	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{layerDir},
		},
	}

	var stderr bytes.Buffer
	ad := buildOrderDispatcher(cityDir, cfg, events.Discard, &stderr)
	if ad == nil {
		t.Fatalf("expected non-nil dispatcher; stderr: %s", stderr.String())
	}

	ad.dispatch(context.Background(), cityDir, time.Now())
	ad.drain(context.Background())

	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	work := workBeadByOrderLabel(t, store, "order-run:file-order")
	if work.Metadata["gc.routed_to"] != "worker" {
		t.Errorf("gc.routed_to = %q, want %q", work.Metadata["gc.routed_to"], "worker")
	}
}

func TestBuildOrderDispatcherRigOrderUsesRigFileStore(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	cityLayer := filepath.Join(cityDir, "formulas")
	rigLayer := filepath.Join(rigDir, "formulas")
	orderDir := filepath.Join(rigDir, "orders")
	for _, dir := range []string{cityLayer, rigLayer, orderDir} {
		if err := mkdirAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(orderDir, "rig-digest.toml"), `[order]
formula = "test-formula"
trigger = "cooldown"
interval = "1m"
pool = "worker"
`)
	formulaText, err := os.ReadFile(filepath.Join(sharedTestFormulaDir, "test-formula.toml"))
	if err != nil {
		t.Fatalf("ReadFile(test-formula): %v", err)
	}
	writeFile(t, filepath.Join(rigLayer, "test-formula.toml"), string(formulaText))
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(rigDir); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo", Prefix: "ct"},
		FormulaLayers: config.FormulaLayers{
			City: []string{cityLayer},
			Rigs: map[string][]string{
				"frontend": {cityLayer, rigLayer},
			},
		},
		Rigs: []config.Rig{{
			Name:   "frontend",
			Path:   "frontend",
			Prefix: "fe",
		}},
	}

	var stderr bytes.Buffer
	ad := buildOrderDispatcher(cityDir, cfg, events.Discard, &stderr)
	if ad == nil {
		t.Fatalf("expected non-nil dispatcher; stderr: %s", stderr.String())
	}

	ad.dispatch(context.Background(), cityDir, time.Now())
	ad.drain(context.Background())

	cityStore, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city): %v", err)
	}
	cityRuns := trackingBeads(t, cityStore, "order-run:rig-digest:rig:frontend")
	if len(cityRuns) != 0 {
		t.Fatalf("city store has %d rig order bead(s), want 0", len(cityRuns))
	}

	rigStore, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	work := workBeadByOrderLabel(t, rigStore, "order-run:rig-digest:rig:frontend")
	if work.Metadata["gc.routed_to"] != "frontend/worker" {
		t.Errorf("gc.routed_to = %q, want %q", work.Metadata["gc.routed_to"], "frontend/worker")
	}
}

func TestBuildOrderDispatcherRigOrderCityPoolUsesCityFileStore(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	cityLayer := filepath.Join(cityDir, "formulas")
	rigLayer := filepath.Join(rigDir, "formulas")
	orderDir := filepath.Join(rigDir, "orders")
	for _, dir := range []string{cityLayer, rigLayer, orderDir} {
		if err := mkdirAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(orderDir, "rig-digest.toml"), `[order]
formula = "test-formula"
trigger = "cooldown"
interval = "1m"
pool = "dog"
`)
	formulaText, err := os.ReadFile(filepath.Join(sharedTestFormulaDir, "test-formula.toml"))
	if err != nil {
		t.Fatalf("ReadFile(test-formula): %v", err)
	}
	writeFile(t, filepath.Join(rigLayer, "test-formula.toml"), string(formulaText))
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(rigDir); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo", Prefix: "ct"},
		Agents: []config.Agent{{
			Name:        "dog",
			BindingName: "maintenance",
		}},
		FormulaLayers: config.FormulaLayers{
			City: []string{cityLayer},
			Rigs: map[string][]string{
				"frontend": {cityLayer, rigLayer},
			},
		},
		Rigs: []config.Rig{{
			Name:   "frontend",
			Path:   "frontend",
			Prefix: "fe",
		}},
	}

	var stderr bytes.Buffer
	ad := buildOrderDispatcher(cityDir, cfg, events.Discard, &stderr)
	if ad == nil {
		t.Fatalf("expected non-nil dispatcher; stderr: %s", stderr.String())
	}

	ad.dispatch(context.Background(), cityDir, time.Now())
	ad.drain(context.Background())

	cityStore, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city): %v", err)
	}
	work := workBeadByOrderLabel(t, cityStore, "order-run:rig-digest:rig:frontend")
	if work.Metadata["gc.routed_to"] != "maintenance.dog" {
		t.Errorf("city work gc.routed_to = %q, want maintenance.dog", work.Metadata["gc.routed_to"])
	}

	rigStore, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	rigRuns := trackingBeads(t, rigStore, "order-run:rig-digest:rig:frontend")
	if len(rigRuns) != 0 {
		t.Fatalf("rig store has %d city-pool order bead(s), want 0", len(rigRuns))
	}
}

func TestBuildOrderDispatcherRigOrderHonorsLegacyCityRunHistory(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	cityLayer := filepath.Join(cityDir, "formulas")
	rigLayer := filepath.Join(rigDir, "formulas")
	orderDir := filepath.Join(rigDir, "orders")
	for _, dir := range []string{cityLayer, rigLayer, orderDir} {
		if err := mkdirAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(orderDir, "rig-digest.toml"), `[order]
formula = "test-formula"
trigger = "cooldown"
interval = "24h"
pool = "worker"
`)
	formulaText, err := os.ReadFile(filepath.Join(sharedTestFormulaDir, "test-formula.toml"))
	if err != nil {
		t.Fatalf("ReadFile(test-formula): %v", err)
	}
	writeFile(t, filepath.Join(rigLayer, "test-formula.toml"), string(formulaText))
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(rigDir); err != nil {
		t.Fatal(err)
	}

	cityStore, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city): %v", err)
	}
	if _, err := cityStore.Create(beads.Bead{
		Title:  "legacy rig digest run",
		Labels: []string{"order-run:rig-digest:rig:frontend"},
	}); err != nil {
		t.Fatalf("Create(legacy city run): %v", err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo", Prefix: "ct"},
		FormulaLayers: config.FormulaLayers{
			City: []string{cityLayer},
			Rigs: map[string][]string{
				"frontend": {cityLayer, rigLayer},
			},
		},
		Rigs: []config.Rig{{
			Name:   "frontend",
			Path:   "frontend",
			Prefix: "fe",
		}},
	}

	var stderr bytes.Buffer
	ad := buildOrderDispatcher(cityDir, cfg, events.Discard, &stderr)
	if ad == nil {
		t.Fatalf("expected non-nil dispatcher; stderr: %s", stderr.String())
	}

	ad.dispatch(context.Background(), cityDir, time.Now())
	ad.drain(context.Background())

	rigStore, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	rigRuns := trackingBeads(t, rigStore, "order-run:rig-digest:rig:frontend")
	if len(rigRuns) != 0 {
		t.Fatalf("rig store has %d new run bead(s), want 0 because legacy city run is still inside cooldown", len(rigRuns))
	}
}

func TestOrderDispatchSkipsRigOrderWhenLegacyCityFallbackUnavailable(t *testing.T) {
	rigStore := beads.NewMemStore()
	stderr := &bytes.Buffer{}
	m := &memoryOrderDispatcher{
		aa: []orders.Order{{
			Name:         "rig-digest",
			Rig:          "frontend",
			Trigger:      "cooldown",
			Interval:     "1m",
			Formula:      "test-formula",
			Pool:         "worker",
			FormulaLayer: sharedTestFormulaDir,
		}},
		storeFn: func(target execStoreTarget) (beads.Store, error) {
			if target.ScopeKind == "city" {
				return nil, fmt.Errorf("legacy city store unavailable")
			}
			return rigStore, nil
		},
		execRun:    shellExecRunner,
		rec:        events.Discard,
		stderr:     stderr,
		maxTimeout: time.Minute,
		cfg: &config.City{
			Rigs: []config.Rig{{
				Name: "frontend",
				Path: "frontend",
			}},
		},
	}

	m.dispatch(context.Background(), t.TempDir(), time.Now())
	m.drain(context.Background())

	rigRuns := trackingBeads(t, rigStore, "order-run:rig-digest:rig:frontend")
	if len(rigRuns) != 0 {
		t.Fatalf("rig store has %d new run bead(s), want 0 when legacy city fallback cannot be checked", len(rigRuns))
	}
	if !strings.Contains(stderr.String(), "legacy city store") {
		t.Fatalf("stderr missing legacy fallback error:\n%s", stderr.String())
	}
}

func TestOrderDispatchSkipsRigEventWhenLegacyCursorReadFails(t *testing.T) {
	rigStore := beads.NewMemStore()
	legacyStore := labelFailListStore{
		Store:     beads.NewMemStore(),
		failLabel: "order:release-watch:rig:frontend",
	}
	eventLog := events.NewFake()
	eventLog.Record(events.Event{Type: events.BeadClosed, Actor: "test"})

	stderr := &bytes.Buffer{}
	m := &memoryOrderDispatcher{
		aa: []orders.Order{{
			Name:    "release-watch",
			Rig:     "frontend",
			Trigger: "event",
			On:      events.BeadClosed,
			Exec:    "true",
			Pool:    "worker",
			Timeout: "1m",
		}},
		storeFn: func(target execStoreTarget) (beads.Store, error) {
			if target.ScopeKind == "city" {
				return legacyStore, nil
			}
			return rigStore, nil
		},
		ep:      eventLog,
		execRun: successfulExec,
		rec:     events.Discard,
		stderr:  stderr,
		cfg: &config.City{
			Rigs: []config.Rig{{
				Name: "frontend",
				Path: "frontend",
			}},
		},
	}

	m.dispatch(context.Background(), t.TempDir(), time.Now())
	m.drain(context.Background())

	rigRuns := trackingBeads(t, rigStore, "order-run:release-watch:rig:frontend")
	if len(rigRuns) != 0 {
		t.Fatalf("rig store has %d new run bead(s), want 0 when legacy event cursor cannot be read", len(rigRuns))
	}
	if !strings.Contains(stderr.String(), "event cursor") {
		t.Fatalf("stderr missing event cursor error:\n%s", stderr.String())
	}
}

func TestOrderDispatchSkipsRigConditionWhenLegacyOpenWorkReadFails(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rigStore := beads.NewMemStore()
	legacyStore := labelFailListStore{
		Store:     beads.NewMemStore(),
		failLabel: "order-run:rig-digest:rig:frontend",
	}

	stderr := &bytes.Buffer{}
	m := &memoryOrderDispatcher{
		aa: []orders.Order{{
			Name:    "rig-digest",
			Rig:     "frontend",
			Trigger: "condition",
			Check:   "true",
			Exec:    "true",
			Pool:    "worker",
			Timeout: "1m",
		}},
		storeFn: func(target execStoreTarget) (beads.Store, error) {
			if target.ScopeKind == "city" {
				return legacyStore, nil
			}
			return rigStore, nil
		},
		execRun: successfulExec,
		rec:     events.Discard,
		stderr:  stderr,
		cfg: &config.City{
			Rigs: []config.Rig{{
				Name: "frontend",
				Path: rigDir,
			}},
		},
	}

	m.dispatch(context.Background(), cityDir, time.Now())
	m.drain(context.Background())

	rigRuns := trackingBeads(t, rigStore, "order-run:rig-digest:rig:frontend")
	if len(rigRuns) != 0 {
		t.Fatalf("rig store has %d new run bead(s), want 0 when legacy open-work state cannot be read", len(rigRuns))
	}
	if !strings.Contains(stderr.String(), "open work") {
		t.Fatalf("stderr missing open-work error:\n%s", stderr.String())
	}
}

func TestOrderDispatchConditionUsesScopedEnv(t *testing.T) {
	cityDir := normalizePathForCompare(t.TempDir())
	store := beads.NewMemStore()
	if err := os.WriteFile(filepath.Join(cityDir, "scoped-marker"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	check := fmt.Sprintf(
		`test "$GC_CITY_PATH" = '%s' && test "$GC_STORE_ROOT" = '%s' && test "$GC_STORE_SCOPE" = city && test "$(pwd -P)" = "$(cd '%s' && pwd -P)" && test -f scoped-marker`,
		cityDir,
		cityDir,
		cityDir,
	)
	ran := make(chan struct{}, 1)
	fakeExec := func(_ context.Context, _, _ string, _ []string) ([]byte, error) {
		ran <- struct{}{}
		return nil, nil
	}
	aa := []orders.Order{{
		Name:    "scoped-check",
		Trigger: "condition",
		Check:   check,
		Exec:    "true",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)

	ad.dispatch(context.Background(), cityDir, time.Now())

	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("condition order did not dispatch with scoped cwd/env")
	}
}

func TestOrderDispatchSkipsRigCooldownWhenLegacyOpenWorkReadFails(t *testing.T) {
	rigStore := beads.NewMemStore()
	legacyStore := labelFailListStore{
		Store:     beads.NewMemStore(),
		failLabel: "order-run:rig-digest:rig:frontend",
	}

	stderr := &bytes.Buffer{}
	m := &memoryOrderDispatcher{
		aa: []orders.Order{{
			Name:         "rig-digest",
			Rig:          "frontend",
			Trigger:      "cooldown",
			Interval:     "1m",
			Formula:      "test-formula",
			Pool:         "worker",
			FormulaLayer: sharedTestFormulaDir,
		}},
		storeFn: func(target execStoreTarget) (beads.Store, error) {
			if target.ScopeKind == "city" {
				return legacyStore, nil
			}
			return rigStore, nil
		},
		execRun:    shellExecRunner,
		rec:        events.Discard,
		stderr:     stderr,
		maxTimeout: time.Minute,
		cfg: &config.City{
			Rigs: []config.Rig{{
				Name: "frontend",
				Path: "frontend",
			}},
		},
	}

	m.dispatch(context.Background(), t.TempDir(), time.Now())
	m.drain(context.Background())

	rigRuns := trackingBeads(t, rigStore, "order-run:rig-digest:rig:frontend")
	if len(rigRuns) != 0 {
		t.Fatalf("rig store has %d new run bead(s), want 0 when legacy last-run state cannot be read", len(rigRuns))
	}
	if !strings.Contains(stderr.String(), "reading last run") {
		t.Fatalf("stderr missing last-run gate error:\n%s", stderr.String())
	}
}

func TestBuildOrderDispatcherReopensStoreForScopedFileReads(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityDir := t.TempDir()
	layerDir := filepath.Join(cityDir, "formulas")
	orderDir := filepath.Join(cityDir, "orders")
	for _, dir := range []string{layerDir, orderDir} {
		if err := mkdirAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(orderDir, "file-order.toml"), `[order]
formula = "test-formula"
trigger = "cooldown"
interval = "1m"
pool = "worker"
`)
	formulaText, err := os.ReadFile(filepath.Join(sharedTestFormulaDir, "test-formula.toml"))
	if err != nil {
		t.Fatalf("ReadFile(test-formula): %v", err)
	}
	writeFile(t, filepath.Join(layerDir, "test-formula.toml"), string(formulaText))

	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{layerDir},
		},
	}

	ad := buildOrderDispatcher(cityDir, cfg, events.Discard, &bytes.Buffer{})
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Title:  "existing work",
		Labels: []string{"order-run:file-order"},
	}); err != nil {
		t.Fatalf("Create(existing work): %v", err)
	}

	ad.dispatch(context.Background(), cityDir, time.Now())
	ad.drain(context.Background())

	results := trackingBeads(t, store, "order-run:file-order")
	tracking := 0
	for _, b := range results {
		if b.Title == "order:file-order" {
			tracking++
		}
	}
	if tracking != 0 {
		t.Fatalf("dispatcher created %d tracking bead(s) despite existing open work", tracking)
	}
}

func TestBuildOrderDispatcherCityPackLayers(t *testing.T) {
	// Simulate system formulas + pack formulas as two city layers.
	sysDir := t.TempDir()
	topoDir := t.TempDir()
	sysLayer := filepath.Join(sysDir, "formulas")
	topoLayer := filepath.Join(topoDir, "formulas")
	for _, dir := range []string{sysLayer, topoLayer} {
		if err := mkdirAll(dir); err != nil {
			t.Fatal(err)
		}
	}

	// System dir: beads-health order.
	sysOrderDir := filepath.Join(sysDir, "orders")
	if err := mkdirAll(sysOrderDir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sysOrderDir, "beads-health.toml"), `[order]
exec = "scripts/beads-health.sh"
trigger = "cooldown"
interval = "30s"
`)

	// Pack dir: wasteland-poll order.
	topoOrderDir := filepath.Join(topoDir, "orders")
	if err := mkdirAll(topoOrderDir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(topoOrderDir, "wasteland-poll.toml"), `[order]
exec = "scripts/wasteland-poll.sh"
trigger = "cooldown"
interval = "2m"
`)

	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{sysLayer, topoLayer},
		},
	}

	var stderr bytes.Buffer
	ad := buildOrderDispatcher(t.TempDir(), cfg, events.Discard, &stderr)
	if ad == nil {
		t.Fatalf("expected non-nil dispatcher; stderr: %s", stderr.String())
	}

	mad := ad.(*memoryOrderDispatcher)
	if len(mad.aa) != 2 {
		t.Fatalf("got %d orders, want 2; stderr: %s", len(mad.aa), stderr.String())
	}

	names := map[string]bool{}
	for _, a := range mad.aa {
		names[a.Name] = true
	}
	if !names["beads-health"] {
		t.Error("missing beads-health order")
	}
	if !names["wasteland-poll"] {
		t.Error("missing wasteland-poll order")
	}
}

func TestBuildOrderDispatcherCityPackWithOverride(t *testing.T) {
	// Same two-layer setup, plus a config override on wasteland-poll interval.
	sysDir := t.TempDir()
	topoDir := t.TempDir()
	sysLayer := filepath.Join(sysDir, "formulas")
	topoLayer := filepath.Join(topoDir, "formulas")
	for _, dir := range []string{sysLayer, topoLayer} {
		if err := mkdirAll(dir); err != nil {
			t.Fatal(err)
		}
	}

	sysOrderDir := filepath.Join(sysDir, "orders")
	if err := mkdirAll(sysOrderDir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sysOrderDir, "beads-health.toml"), `[order]
exec = "scripts/beads-health.sh"
trigger = "cooldown"
interval = "30s"
`)

	topoOrderDir := filepath.Join(topoDir, "orders")
	if err := mkdirAll(topoOrderDir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(topoOrderDir, "wasteland-poll.toml"), `[order]
exec = "scripts/wasteland-poll.sh"
trigger = "cooldown"
interval = "2m"
`)

	tenSec := "10s"
	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{sysLayer, topoLayer},
		},
		Orders: config.OrdersConfig{
			Overrides: []config.OrderOverride{
				{Name: "wasteland-poll", Interval: &tenSec},
			},
		},
	}

	var stderr bytes.Buffer
	ad := buildOrderDispatcher(t.TempDir(), cfg, events.Discard, &stderr)
	if ad == nil {
		t.Fatalf("expected non-nil dispatcher; stderr: %s", stderr.String())
	}

	mad := ad.(*memoryOrderDispatcher)
	if len(mad.aa) != 2 {
		t.Fatalf("got %d orders, want 2", len(mad.aa))
	}

	// Verify wasteland-poll interval was overridden to 10s.
	for _, a := range mad.aa {
		if a.Name == "wasteland-poll" {
			if a.Interval != "10s" {
				t.Errorf("wasteland-poll interval = %q, want %q", a.Interval, "10s")
			}
			return
		}
	}
	t.Error("wasteland-poll not found in dispatcher orders")
}

// TestBuildOrderDispatcherOverrideDisablesDropsFromDispatcher is the
// regression test for gastownhall/gascity#2191's post-override
// filterEnabledOrders behavior. Without filtering after overrides are
// applied, an order whose enabled flag is flipped to false via
// [orders.overrides] survived to the dispatcher and was treated as
// runnable — the bug that #2191 closed. See gastownhall/gascity#2202.
func TestBuildOrderDispatcherOverrideDisablesDropsFromDispatcher(t *testing.T) {
	// Two on-disk orders, both enabled by default. The config overrides
	// wasteland-poll to enabled=false; only beads-health should reach
	// the dispatcher's auto-set.
	sysDir := t.TempDir()
	topoDir := t.TempDir()

	sysFormulaDir := sysDir + "/formulas"
	sysOrdersDir := sysDir + "/orders"
	if err := mkdirAll(sysFormulaDir); err != nil {
		t.Fatal(err)
	}
	if err := mkdirAll(sysOrdersDir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, sysOrdersDir+"/beads-health.toml", `[order]
exec = "scripts/beads-health.sh"
trigger = "cooldown"
interval = "30s"
`)

	topoFormulaDir := topoDir + "/formulas"
	topoOrdersDir := topoDir + "/orders"
	if err := mkdirAll(topoFormulaDir); err != nil {
		t.Fatal(err)
	}
	if err := mkdirAll(topoOrdersDir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, topoOrdersDir+"/wasteland-poll.toml", `[order]
exec = "scripts/wasteland-poll.sh"
trigger = "cooldown"
interval = "2m"
`)

	disabled := false
	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{sysFormulaDir, topoFormulaDir},
		},
		Orders: config.OrdersConfig{
			Overrides: []config.OrderOverride{
				{Name: "wasteland-poll", Enabled: &disabled},
			},
		},
	}

	var stderr bytes.Buffer
	ad := buildOrderDispatcher(t.TempDir(), cfg, events.Discard, &stderr)
	if ad == nil {
		t.Fatalf("expected non-nil dispatcher; stderr: %s", stderr.String())
	}

	mad := ad.(*memoryOrderDispatcher)
	if len(mad.aa) != 1 {
		var names []string
		for _, a := range mad.aa {
			names = append(names, a.Name)
		}
		t.Fatalf("dispatcher orders = %d (%v), want 1 (override-disabled wasteland-poll must be filtered)", len(mad.aa), names)
	}
	if got := mad.aa[0].Name; got != "beads-health" {
		t.Fatalf("dispatcher order = %q, want %q", got, "beads-health")
	}
	for _, a := range mad.aa {
		if a.Name == "wasteland-poll" {
			t.Fatalf("wasteland-poll reached the dispatcher despite enabled=false override (this is the #2191 bug)")
		}
	}
}

func TestBuildOrderDispatcherOverrideNotFoundNonFatal(t *testing.T) {
	// Single formula layer with beads-health only.
	// Override targets wasteland-poll (nonexistent).
	// Verify beads-health is still dispatched and stderr contains warning.
	sysDir := t.TempDir()
	sysLayer := filepath.Join(sysDir, "formulas")
	sysOrderDir := filepath.Join(sysDir, "orders")
	for _, dir := range []string{sysLayer, sysOrderDir} {
		if err := mkdirAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(sysOrderDir, "beads-health.toml"), `[order]
exec = "scripts/beads-health.sh"
trigger = "cooldown"
interval = "30s"
`)

	tenSec := "10s"
	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{sysLayer},
		},
		Orders: config.OrdersConfig{
			Overrides: []config.OrderOverride{
				{Name: "wasteland-poll", Interval: &tenSec},
			},
		},
	}

	var stderr bytes.Buffer
	ad := buildOrderDispatcher(t.TempDir(), cfg, events.Discard, &stderr)
	if ad == nil {
		t.Fatalf("expected non-nil dispatcher (beads-health should still be found); stderr: %s", stderr.String())
	}

	mad := ad.(*memoryOrderDispatcher)
	if len(mad.aa) != 1 {
		t.Fatalf("got %d orders, want 1", len(mad.aa))
	}
	if mad.aa[0].Name != "beads-health" {
		t.Errorf("order name = %q, want %q", mad.aa[0].Name, "beads-health")
	}

	// Verify stderr contains the "not found" warning from ApplyOverrides.
	if !strings.Contains(stderr.String(), "not found") {
		t.Errorf("expected stderr to contain 'not found' warning, got: %s", stderr.String())
	}
}

// --- helpers ---

func mkdirAll(path string) error {
	return os.MkdirAll(path, 0o755)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeImportedDogOrderFixture(t *testing.T, cityDir string, includeCityDog bool, extraBindings ...string) {
	t.Helper()

	const orderBinding = "maintenance"
	packRoot := filepath.Join(cityDir, "packs")
	if err := os.MkdirAll(packRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	cityToml := `
[workspace]
name = "test-city"
`
	if includeCityDog {
		cityToml += `

[[agent]]
name = "dog"
scope = "city"
`
	}
	writeFile(t, filepath.Join(cityDir, "city.toml"), cityToml)

	formulaText, err := os.ReadFile(filepath.Join(sharedTestFormulaDir, "test-formula.toml"))
	if err != nil {
		t.Fatalf("ReadFile(test-formula): %v", err)
	}

	allBindings := append([]string{orderBinding}, extraBindings...)
	var packToml strings.Builder
	packToml.WriteString(`
[pack]
name = "test-city"
schema = 1
`)

	for _, binding := range allBindings {
		packDir := filepath.Join(packRoot, binding)
		if err := os.MkdirAll(filepath.Join(packDir, "orders"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(packDir, "formulas"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(packDir, "pack.toml"), `
[pack]
name = "`+binding+`"
schema = 1

[[agent]]
name = "dog"
scope = "city"
`)
		if binding == orderBinding {
			writeFile(t, filepath.Join(packDir, "orders", "digest.toml"), `
[order]
formula = "test-formula"
trigger = "cooldown"
interval = "24h"
pool = "dog"
`)
			writeFile(t, filepath.Join(packDir, "formulas", "test-formula.toml"), string(formulaText))
		}
		packToml.WriteString(`
[imports.` + binding + `]
source = "./packs/` + binding + `"
`)
	}

	writeFile(t, filepath.Join(cityDir, "pack.toml"), packToml.String())
}

func loadImportedDogOrders(t *testing.T, cityDir string) (*config.City, []orders.Order) {
	t.Helper()

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	var stderr bytes.Buffer
	aa, err := scanAllOrders(cityDir, cfg, &stderr, "gc order list")
	if err != nil {
		t.Fatalf("scanAllOrders: %v; stderr: %s", err, stderr.String())
	}
	found := false
	for _, order := range aa {
		if order.Name == "digest" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("scanAllOrders() missing digest order (%#v)", aa)
	}
	return cfg, aa
}

// memRecorder records events in memory for test assertions.
// mu guards events against concurrent Record/hasType/hasSubject calls from
// dispatchOne goroutines and the test goroutine.
type memRecorder struct {
	mu     sync.Mutex
	events []events.Event
}

func (r *memRecorder) Record(e events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *memRecorder) hasType(typ string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e.Type == typ {
			return true
		}
	}
	return false
}

func (r *memRecorder) hasSubject(subject string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e.Subject == subject {
			return true
		}
	}
	return false
}

// --- dedup / tracking bead lifecycle tests ---

func TestOrderDispatchClosesTrackingBead(t *testing.T) {
	store := strictCloseReasonStore{Store: beads.NewMemStore()}
	var rec memRecorder

	fakeExec := func(_ context.Context, _, _ string, _ []string) ([]byte, error) {
		return []byte("ok\n"), nil
	}

	aa := []orders.Order{{
		Name:     "health-check",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     "scripts/health.sh",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, &rec)

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	ad.drain(context.Background())

	// Tracking bead should be closed after dispatch completes.
	all := trackingBeads(t, store, "order-run:health-check")
	for _, b := range all {
		for _, l := range b.Labels {
			if l == "order-run:health-check" {
				if b.Status != "closed" {
					t.Errorf("tracking bead status = %q, want %q", b.Status, "closed")
				}
				if got := b.Metadata["close_reason"]; got != completedOrderTrackingCloseReason {
					t.Errorf("close_reason = %q, want %q", got, completedOrderTrackingCloseReason)
				}
				return
			}
		}
	}
	t.Error("tracking bead not found")
}

func TestOrderDispatchSkipsOpenWork(t *testing.T) {
	store := beads.NewMemStore()

	// Seed an open wisp (non-tracking bead) for this order.
	_, err := store.Create(beads.Bead{
		Title:  "mol-do-work", // not "order:my-auto" → counts as real work
		Labels: []string{"order-run:my-auto"},
	})
	if err != nil {
		t.Fatal(err)
	}

	ran := false
	fakeExec := func(_ context.Context, _, _ string, _ []string) ([]byte, error) {
		ran = true
		return nil, nil
	}

	aa := []orders.Order{{
		Name:     "my-auto",
		Trigger:  "cooldown",
		Interval: "1s", // short cooldown — would fire if not deduped
		Exec:     "scripts/run.sh",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	ad.drain(context.Background())

	if ran {
		t.Error("exec should not have run — open work exists")
	}

	// No new beads should have been created (only the seed).
	all, _ := store.ListOpen()
	if len(all) != 1 {
		t.Errorf("expected 1 bead (seed only), got %d", len(all))
	}
}

func TestOrderDispatchSkipsOpenTrackingBeadForConditionOrder(t *testing.T) {
	store := beads.NewMemStore()

	_, err := store.Create(beads.Bead{
		Title:  "order:my-auto",
		Labels: []string{"order-run:my-auto", labelOrderTracking},
	})
	if err != nil {
		t.Fatal(err)
	}

	ran := false
	fakeExec := func(_ context.Context, _, _ string, _ []string) ([]byte, error) {
		ran = true
		return nil, nil
	}

	aa := []orders.Order{{
		Name:    "my-auto",
		Trigger: "condition",
		Check:   "true",
		Exec:    "scripts/run.sh",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	ad.drain(context.Background())

	if ran {
		t.Error("exec should not have run while an order-tracking bead is open")
	}
}

func TestOrderDispatchFiresAfterWorkClosed(t *testing.T) {
	store := beads.NewMemStore()

	// Seed a CLOSED wisp — should not block new dispatch.
	b, err := store.Create(beads.Bead{
		Title:  "mol-do-work",
		Labels: []string{"order-run:my-auto"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatal(err)
	}

	ran := false
	fakeExec := func(_ context.Context, _, _ string, _ []string) ([]byte, error) {
		ran = true
		return nil, nil
	}

	aa := []orders.Order{{
		Name:     "my-auto",
		Trigger:  "cooldown",
		Interval: "1s",
		Exec:     "scripts/run.sh",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)

	// Use a future "now" so cooldown trigger sees the seed bead as old enough.
	ad.dispatch(context.Background(), t.TempDir(), time.Now().Add(5*time.Second))
	ad.drain(context.Background())

	if !ran {
		t.Error("exec should have run — all previous work is closed")
	}
}

// TestOrderDispatchOpenWispRootDoesNotBlockRedispatch reproduces the
// formula+pool auto-dispatch failure tracked by ga-lo8c (continuation of
// closed ga-jra). After a city restart, the wisp root bead from a previous
// formula+pool dispatch persists in the store with status="open" and the
// "order-run:<scoped>" label (stamped by dispatchWisp at order_dispatch.go:706).
// Molecule roots are never auto-closed when their step beads complete, so
// the leftover open root permanently tripped hasOpenWorkStrict and starved
// the order's cooldown gate.
//
// In-flight dispatch is signaled by the tracking bead (carries both
// "order-run:<scoped>" AND labelOrderTracking). Wisp roots carry only the
// former — distinguishing the two is the fix.
func TestOrderDispatchOpenWispRootDoesNotBlockRedispatch(t *testing.T) {
	store := beads.NewMemStore()

	// Simulate post-restart state: an open wisp root from the prior
	// formula+pool dispatch. The tracking bead from that dispatch was
	// already closed by dispatchOne's deferred Close, but the molecule
	// root remains open because nothing in the dispatch path closes it.
	wispRoot, err := store.Create(beads.Bead{
		Title:  "mol-formula-pool-work",
		Type:   "molecule",
		Labels: []string{"order-run:my-pool-order"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if wispRoot.Status == "closed" {
		t.Fatalf("seed wisp root should be open, got status=%q", wispRoot.Status)
	}

	ran := false
	fakeExec := func(_ context.Context, _, _ string, _ []string) ([]byte, error) {
		ran = true
		return nil, nil
	}

	aa := []orders.Order{{
		Name:     "my-pool-order",
		Trigger:  "cooldown",
		Interval: "1s",
		Exec:     "scripts/run.sh",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)

	// Future "now" so cooldown evaluates Due=true (LastRun = wispRoot.CreatedAt).
	ad.dispatch(context.Background(), t.TempDir(), time.Now().Add(5*time.Second))
	ad.drain(context.Background())

	if !ran {
		t.Fatal("exec should have run — an open wisp root (no labelOrderTracking) " +
			"is leftover state, not in-flight work, and must not block re-dispatch")
	}
}

// listIncludingClosedStore forces List to include closed beads regardless of
// the caller's IncludeClosed setting. Production stores honor IncludeClosed:
// false, but hasOpenWorkStrict's defensive status check must still skip closed
// beads if a store implementation (or a stale cache) returns them.
type listIncludingClosedStore struct {
	beads.Store
}

func (s listIncludingClosedStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	q.IncludeClosed = true
	return s.Store.List(q)
}

// TestHasOpenWorkStrictSkipsClosedTrackingBead covers the defensive
// status-closed branch in hasOpenWorkStrict (cmd/gc/order_dispatch.go:802-803).
// The standard query passes IncludeClosed: false, so closed beads normally
// don't reach the loop — but a misbehaving store (or a stale CachingStore
// view) could leak one through. If hasOpenWorkStrict didn't skip closed
// beads, a completed tracking bead would permanently block re-dispatch — the
// same failure shape ga-lo8c hit with leftover wisp roots.
func TestHasOpenWorkStrictSkipsClosedTrackingBead(t *testing.T) {
	base := beads.NewMemStore()
	store := listIncludingClosedStore{Store: base}

	b, err := store.Create(beads.Bead{
		Title:  "order:my-order",
		Labels: []string{"order-run:my-order", labelOrderTracking},
	})
	if err != nil {
		t.Fatalf("seed bead: %v", err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatalf("close seed bead: %v", err)
	}

	ad := &memoryOrderDispatcher{}
	has, err := ad.hasOpenWorkStrict(store, "my-order")
	if err != nil {
		t.Fatalf("hasOpenWorkStrict: %v", err)
	}
	if has {
		t.Fatal("closed tracking bead must not count as in-flight work; " +
			"the defensive status filter at order_dispatch.go:802 should skip it")
	}
}

// TestHasOpenWorkStrictBlocksOnWispWithOpenChildren is the regression guard
// for tr-kds01: the deacon's cooldown gate fired a new digest wisp every 24h
// even when the prior wisp's step beads had never been picked up by the pool
// agent. The leftover open wisp root (no labelOrderTracking) is normally
// treated as orphan state (ga-jra/ga-lo8c). But when the wisp's child step
// beads are still open, the wisp is in-flight work — the pool agent simply
// hasn't completed it yet — and the gate must block.
func TestHasOpenWorkStrictBlocksOnWispWithOpenChildren(t *testing.T) {
	store := beads.NewMemStore()

	wispRoot, err := store.Create(beads.Bead{
		Title:  "mol-digest-generate",
		Type:   "molecule",
		Labels: []string{"order-run:digest"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Step bead still open — pool agent hasn't picked it up yet.
	if _, err := store.Create(beads.Bead{
		Title:    "determine-period",
		ParentID: wispRoot.ID,
	}); err != nil {
		t.Fatal(err)
	}

	ad := &memoryOrderDispatcher{}
	has, err := ad.hasOpenWorkStrict(store, "digest")
	if err != nil {
		t.Fatalf("hasOpenWorkStrict: %v", err)
	}
	if !has {
		t.Fatal("wisp root with open child step beads must count as in-flight; " +
			"the gate ignored them and the next cooldown tick poured a duplicate wisp (tr-kds01)")
	}
}

// TestStoreHasOpenDescendantsUsesMembershipFastPath proves the #2893
// optimization: an open descendant reachable ONLY by its gc.root_bead_id
// membership metadata (no ParentID, no dependency edge) is found in a single
// metadata-filtered List, without the O(tree) per-node ParentID/DepList walk.
// Because the descendant has no walkable edge to the root, a true result can
// only come from the membership fast path.
func TestStoreHasOpenDescendantsUsesMembershipFastPath(t *testing.T) {
	store := beads.NewMemStore()

	root, err := store.Create(beads.Bead{
		Title:  "mol-digest-generate",
		Type:   "molecule",
		Labels: []string{"order-run:digest"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Open member tied to the root only by membership metadata.
	if _, err := store.Create(beads.Bead{
		Title:    "determine-period",
		Metadata: map[string]string{"gc.root_bead_id": root.ID},
	}); err != nil {
		t.Fatal(err)
	}

	has, err := storeHasOpenDescendants(store, root.ID, nil)
	if err != nil {
		t.Fatalf("storeHasOpenDescendants: %v", err)
	}
	if !has {
		t.Fatal("open member carrying gc.root_bead_id must count as in-flight via the membership fast path")
	}
}

// TestStoreHasOpenDescendantsMembershipOrphanAllClosed guards against false
// positives: when every member carries gc.root_bead_id but all are closed, the
// root is an orphan (ga-jra/ga-lo8c) and must NOT count as in-flight work, so a
// later cooldown tick can re-dispatch.
func TestStoreHasOpenDescendantsMembershipOrphanAllClosed(t *testing.T) {
	store := beads.NewMemStore()

	root, err := store.Create(beads.Bead{
		Title:  "mol-digest-generate",
		Type:   "molecule",
		Labels: []string{"order-run:digest"},
	})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.Create(beads.Bead{
		Title:    "determine-period",
		Metadata: map[string]string{"gc.root_bead_id": root.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(child.ID); err != nil {
		t.Fatalf("close member: %v", err)
	}

	has, err := storeHasOpenDescendants(store, root.ID, nil)
	if err != nil {
		t.Fatalf("storeHasOpenDescendants: %v", err)
	}
	if has {
		t.Fatal("all-closed membership is an orphan root and must not count as in-flight work")
	}
}

// TestStoreHasOpenDescendantsFallsBackWithoutMembershipMetadata proves the
// fallback: a descendant materialized before gc.root_bead_id stamping is linked
// only by ParentID and carries no membership metadata. The metadata List
// returns nothing, so the gate must fall back to the authoritative tree walk
// and still find the open child — behavior is byte-identical to the historical
// implementation for un-stamped data.
func TestStoreHasOpenDescendantsFallsBackWithoutMembershipMetadata(t *testing.T) {
	store := beads.NewMemStore()

	root, err := store.Create(beads.Bead{
		Title:  "mol-digest-generate",
		Type:   "molecule",
		Labels: []string{"order-run:digest"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Open child linked only by ParentID — no gc.root_bead_id (pre-stamp data).
	if _, err := store.Create(beads.Bead{
		Title:    "determine-period",
		ParentID: root.ID,
	}); err != nil {
		t.Fatal(err)
	}

	has, err := storeHasOpenDescendants(store, root.ID, nil)
	if err != nil {
		t.Fatalf("storeHasOpenDescendants: %v", err)
	}
	if !has {
		t.Fatal("ParentID-linked open child without membership metadata must be found via the walk fallback")
	}
}

// TestStoreHasOpenDescendantsFallsBackOnPartialStampMembership guards the
// single-flight safety of a partial-stamp molecule: some steps carry
// gc.root_bead_id (a nested sub-molecule) while sibling ParentID-only steps do
// not. When every stamped member is closed but an un-stamped sibling is still
// open, the membership set is non-empty yet has no open member — the gate must
// still fall back to the walk and report in-flight work, not declare the root
// idle (which would re-dispatch while work is in flight).
func TestStoreHasOpenDescendantsFallsBackOnPartialStampMembership(t *testing.T) {
	base := beads.NewMemStore()
	store := listIncludingClosedStore{Store: base}

	root, err := store.Create(beads.Bead{
		Title:  "mol-digest-generate",
		Type:   "molecule",
		Labels: []string{"order-run:digest"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// A stamped member (e.g. a nested sub-molecule) that is CLOSED.
	stamped, err := store.Create(beads.Bead{
		Title:    "sub-molecule",
		Metadata: map[string]string{"gc.root_bead_id": root.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(stamped.ID); err != nil {
		t.Fatalf("close stamped member: %v", err)
	}
	// A sibling step linked only by ParentID, NOT stamped, still OPEN.
	if _, err := store.Create(beads.Bead{
		Title:    "unstamped-step",
		ParentID: root.ID,
	}); err != nil {
		t.Fatal(err)
	}

	has, err := storeHasOpenDescendants(store, root.ID, nil)
	if err != nil {
		t.Fatalf("storeHasOpenDescendants: %v", err)
	}
	if !has {
		t.Fatal("partial-stamp molecule with a closed stamped member and an open un-stamped ParentID sibling must still report in-flight via the walk fallback (single-flight false negative otherwise)")
	}
}

// TestStoreHasOpenDescendantsMembershipSkipsTransientNotification proves the
// #3102 skip predicate composes with the #2893 membership fast path: an OPEN
// stamped transient-notification bead (a nudge or mail/message chore reaped on
// its own TTL) carries gc.root_bead_id and so is returned by the membership
// List, but it must NOT wedge the single-flight gate. With skip =
// isTransientNotificationBead the fast path skips it, finds no other open
// member, and the walk fallback (which also honors skip) likewise reports the
// root idle so the order can re-dispatch.
func TestStoreHasOpenDescendantsMembershipSkipsTransientNotification(t *testing.T) {
	store := beads.NewMemStore()

	root, err := store.Create(beads.Bead{
		Title:  "mol-digest-generate",
		Type:   "molecule",
		Labels: []string{"order-run:digest"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// An OPEN stamped transient chore (a mail/message) tied to the root by
	// membership metadata. Without skip it would be reported as in-flight work.
	if _, err := store.Create(beads.Bead{
		Title:    "delivery-mail",
		Type:     "message",
		Metadata: map[string]string{"gc.root_bead_id": root.ID},
	}); err != nil {
		t.Fatal(err)
	}

	// Without a skip predicate the open transient member counts as in-flight.
	has, err := storeHasOpenDescendants(store, root.ID, nil)
	if err != nil {
		t.Fatalf("storeHasOpenDescendants(nil): %v", err)
	}
	if !has {
		t.Fatal("open stamped member must count as in-flight when skip is nil")
	}

	// With isTransientNotificationBead the transient chore is skipped on the
	// membership fast path and must NOT wedge the gate.
	has, err = storeHasOpenDescendants(store, root.ID, isTransientNotificationBead)
	if err != nil {
		t.Fatalf("storeHasOpenDescendants(skip): %v", err)
	}
	if has {
		t.Fatal("open stamped transient-notification bead must not count as in-flight on the membership fast path when skip=isTransientNotificationBead")
	}
}

func TestHasOpenWorkStrictFindsOlderInFlightWispBehindOrphanRoots(t *testing.T) {
	const formerOpenWorkProbeLimit = 50

	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	rows := []beads.Bead{{
		ID:        "gc-inflight-root",
		Title:     "older in-flight wisp",
		Status:    "open",
		Type:      "molecule",
		CreatedAt: base,
		UpdatedAt: base,
		Labels:    []string{"order-run:digest"},
	}, {
		ID:        "gc-inflight-child",
		Title:     "still running",
		Status:    "open",
		Type:      "task",
		ParentID:  "gc-inflight-root",
		CreatedAt: base.Add(time.Second),
		UpdatedAt: base.Add(time.Second),
	}}
	for i := 0; i < formerOpenWorkProbeLimit+1; i++ {
		created := base.Add(time.Duration(i+2) * time.Second)
		rows = append(rows, beads.Bead{
			ID:        fmt.Sprintf("gc-orphan-root-%02d", i),
			Title:     "newer orphan wisp root",
			Status:    "open",
			Type:      "molecule",
			CreatedAt: created,
			UpdatedAt: created,
			Labels:    []string{"order-run:digest"},
		})
	}
	store := beads.NewMemStoreFrom(len(rows), rows, nil)

	ad := &memoryOrderDispatcher{}
	has, err := ad.hasOpenWorkStrict(store, "digest")
	if err != nil {
		t.Fatalf("hasOpenWorkStrict: %v", err)
	}
	if !has {
		t.Fatal("older wisp with open descendants must block even after newer orphan roots exceed the old probe window")
	}
}

func TestHasOpenWorkStrictBlocksOnWispWithPartiallyClosedChildren(t *testing.T) {
	store := beads.NewMemStore()

	wispRoot, err := store.Create(beads.Bead{
		Title:  "mol-digest-generate",
		Type:   "molecule",
		Labels: []string{"order-run:digest"},
	})
	if err != nil {
		t.Fatal(err)
	}
	closedChild, err := store.Create(beads.Bead{
		Title:    "determine-period",
		ParentID: wispRoot.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(closedChild.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(beads.Bead{
		Title:    "draft-digest",
		ParentID: wispRoot.ID,
	}); err != nil {
		t.Fatal(err)
	}

	ad := &memoryOrderDispatcher{}
	has, err := ad.hasOpenWorkStrict(store, "digest")
	if err != nil {
		t.Fatalf("hasOpenWorkStrict: %v", err)
	}
	if !has {
		t.Fatal("wisp root with a remaining open child step must count as in-flight work")
	}
}

func TestHasOpenWorkStrictBlocksOnWispWithOpenDescendant(t *testing.T) {
	store := beads.NewMemStore()

	wispRoot, err := store.Create(beads.Bead{
		Title:  "mol-digest-generate",
		Type:   "molecule",
		Labels: []string{"order-run:digest"},
	})
	if err != nil {
		t.Fatal(err)
	}
	closedChild, err := store.Create(beads.Bead{
		Title:    "prepare-submolecule",
		ParentID: wispRoot.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(closedChild.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(beads.Bead{
		Title:    "nested-step-still-running",
		ParentID: closedChild.ID,
	}); err != nil {
		t.Fatal(err)
	}

	ad := &memoryOrderDispatcher{}
	has, err := ad.hasOpenWorkStrict(store, "digest")
	if err != nil {
		t.Fatalf("hasOpenWorkStrict: %v", err)
	}
	if !has {
		t.Fatal("wisp root with an open nested descendant must count as in-flight work")
	}
}

func TestHasOpenWorkStrictBlocksOnGraphWorkflowWispWithOpenDescendant(t *testing.T) {
	store := beads.NewMemStore()

	wispRoot, err := store.Create(beads.Bead{
		Title:  "graph-workflow-digest",
		Type:   "task",
		Labels: []string{"order-run:digest"},
		Metadata: map[string]string{
			"gc.kind": "workflow",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	closedChild, err := store.Create(beads.Bead{
		Title:    "prepare-graph-step",
		ParentID: wispRoot.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(closedChild.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(beads.Bead{
		Title:    "graph-step-still-running",
		ParentID: closedChild.ID,
	}); err != nil {
		t.Fatal(err)
	}

	ad := &memoryOrderDispatcher{}
	has, err := ad.hasOpenWorkStrict(store, "digest")
	if err != nil {
		t.Fatalf("hasOpenWorkStrict: %v", err)
	}
	if !has {
		t.Fatal("graph workflow wisp with an open nested descendant must count as in-flight work")
	}
}

func TestHasOpenWorkStrictBlocksOnGraphWorkflowTracksDependent(t *testing.T) {
	store := beads.NewMemStore()

	wispRoot, err := store.Create(beads.Bead{
		Title:  "graph-workflow-digest",
		Type:   "task",
		Labels: []string{"order-run:digest"},
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	step, err := store.Create(beads.Bead{
		Title: "draft-digest",
		Metadata: map[string]string{
			"gc.root_bead_id": wispRoot.ID,
			"gc.step_ref":     "draft",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DepAdd(step.ID, wispRoot.ID, "tracks"); err != nil {
		t.Fatal(err)
	}

	ad := &memoryOrderDispatcher{}
	has, err := ad.hasOpenWorkStrict(store, "digest")
	if err != nil {
		t.Fatalf("hasOpenWorkStrict: %v", err)
	}
	if !has {
		t.Fatal("graph workflow wisp with an open tracks dependent must count as in-flight work")
	}
}

func TestHasOpenWorkStrictBlocksOnSameWorkflowGraphDependencyTypes(t *testing.T) {
	for _, depType := range []string{"blocks", "parent-child"} {
		t.Run(depType, func(t *testing.T) {
			store := beads.NewMemStore()

			wispRoot, err := store.Create(beads.Bead{
				Title:  "graph-workflow-digest",
				Type:   "task",
				Labels: []string{"order-run:digest"},
				Metadata: map[string]string{
					"gc.kind":             "workflow",
					"gc.formula_contract": "graph.v2",
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			step, err := store.Create(beads.Bead{
				Title: "draft-digest",
				Metadata: map[string]string{
					"gc.root_bead_id": wispRoot.ID,
					"gc.step_ref":     "draft",
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.DepAdd(step.ID, wispRoot.ID, depType); err != nil {
				t.Fatal(err)
			}

			ad := &memoryOrderDispatcher{}
			has, err := ad.hasOpenWorkStrict(store, "digest")
			if err != nil {
				t.Fatalf("hasOpenWorkStrict: %v", err)
			}
			if !has {
				t.Fatalf("graph workflow wisp with an open %s dependent must count as in-flight work", depType)
			}
		})
	}
}

func TestHasOpenWorkStrictIgnoresForeignGraphDependents(t *testing.T) {
	tests := []struct {
		name     string
		depType  string
		metadata map[string]string
	}{
		{
			name:    "external convoy tracks edge",
			depType: "tracks",
		},
		{
			name:    "unrelated downstream blocks edge",
			depType: "blocks",
			metadata: map[string]string{
				"gc.root_bead_id": "other-workflow-root",
				"gc.step_ref":     "external",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := beads.NewMemStore()

			wispRoot, err := store.Create(beads.Bead{
				Title:  "graph-workflow-digest",
				Type:   "task",
				Labels: []string{"order-run:digest"},
				Metadata: map[string]string{
					"gc.kind":             "workflow",
					"gc.formula_contract": "graph.v2",
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			external, err := store.Create(beads.Bead{
				Title:    "external-dependent",
				Metadata: tt.metadata,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.DepAdd(external.ID, wispRoot.ID, tt.depType); err != nil {
				t.Fatal(err)
			}

			ad := &memoryOrderDispatcher{}
			has, err := ad.hasOpenWorkStrict(store, "digest")
			if err != nil {
				t.Fatalf("hasOpenWorkStrict: %v", err)
			}
			if has {
				t.Fatalf("foreign %s dependent must not count as in-flight order work", tt.depType)
			}
		})
	}
}

func TestHasOpenWorkStrictBlocksOnRootOnlyWisp(t *testing.T) {
	store := beads.NewMemStore()

	if _, err := store.Create(beads.Bead{
		Title:  "vapor-order-root",
		Type:   "task",
		Labels: []string{"order-run:digest"},
		Metadata: map[string]string{
			"gc.kind": "wisp",
		},
	}); err != nil {
		t.Fatal(err)
	}

	ad := &memoryOrderDispatcher{}
	has, err := ad.hasOpenWorkStrict(store, "digest")
	if err != nil {
		t.Fatalf("hasOpenWorkStrict: %v", err)
	}
	if !has {
		t.Fatal("open root-only gc.kind=wisp bead must count as in-flight order work")
	}
}

// TestHasOpenWorkStrictAllowsWispWithAllChildrenClosed protects the
// ga-jra/ga-lo8c invariant after the tr-kds01 fix. A wisp root whose step
// beads are ALL closed represents work that completed (or was hand-cleaned)
// but whose root was never auto-closed — molecule roots never close
// themselves. Such a root must not permanently block re-dispatch.
func TestHasOpenWorkStrictAllowsWispWithAllChildrenClosed(t *testing.T) {
	store := beads.NewMemStore()

	wispRoot, err := store.Create(beads.Bead{
		Title:  "mol-digest-generate",
		Type:   "molecule",
		Labels: []string{"order-run:digest"},
	})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.Create(beads.Bead{
		Title:    "determine-period",
		ParentID: wispRoot.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(child.ID); err != nil {
		t.Fatal(err)
	}

	ad := &memoryOrderDispatcher{}
	has, err := ad.hasOpenWorkStrict(store, "digest")
	if err != nil {
		t.Fatalf("hasOpenWorkStrict: %v", err)
	}
	if has {
		t.Fatal("wisp root with all children closed is leftover state, not in-flight work; " +
			"counting it would permanently block re-dispatch (ga-jra/ga-lo8c)")
	}
}

type parentListFailStore struct {
	beads.Store
	failParentID string
	err          error
}

func (s parentListFailStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.ParentID == s.failParentID {
		return nil, s.err
	}
	return s.Store.List(q)
}

func TestHasOpenWorkStrictPropagatesWispChildListError(t *testing.T) {
	base := beads.NewMemStore()

	wispRoot, err := base.Create(beads.Bead{
		Title:  "mol-digest-generate",
		Type:   "molecule",
		Labels: []string{"order-run:digest"},
	})
	if err != nil {
		t.Fatal(err)
	}

	store := parentListFailStore{
		Store:        base,
		failParentID: wispRoot.ID,
		err:          fmt.Errorf("children tier unavailable"),
	}
	ad := &memoryOrderDispatcher{}
	has, err := ad.hasOpenWorkStrict(store, "digest")
	if err == nil {
		t.Fatal("hasOpenWorkStrict err = nil, want child-list error")
	}
	if has {
		t.Fatal("hasOpenWorkStrict returned true with child-list error; caller must fail closed on the error")
	}
	if !strings.Contains(err.Error(), "checking open descendants of wisp") {
		t.Fatalf("hasOpenWorkStrict err = %q, want wisp descendant context", err)
	}
}

func TestStoreHasOpenDescendantsShortCircuitsOpenParentChildBeforeGraphReads(t *testing.T) {
	base := beads.NewMemStore()

	wispRoot, err := base.Create(beads.Bead{
		Title: "graph-workflow-digest",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := base.Create(beads.Bead{
		Title:    "direct-open-step",
		ParentID: wispRoot.ID,
	}); err != nil {
		t.Fatal(err)
	}

	store := depListFailStore{Store: base, failID: wispRoot.ID}
	has, err := storeHasOpenDescendants(store, wispRoot.ID, nil)
	if err != nil {
		t.Fatalf("storeHasOpenDescendants: %v", err)
	}
	if !has {
		t.Fatal("direct open ParentID child must count as an open descendant")
	}
}

// TestOrderDispatchOpenWispWithOpenStepsBlocksRedispatch is the
// integration-level guard for tr-kds01. The scenario: a periodic
// formula+pool order whose first dispatch's step beads were never picked
// up by the pool agent. The wisp root and its step beads sit open. The
// next cooldown tick MUST NOT fire another wisp.
func TestOrderDispatchOpenWispWithOpenStepsBlocksRedispatch(t *testing.T) {
	store := beads.NewMemStore()

	wispRoot, err := store.Create(beads.Bead{
		Title:  "mol-digest-generate",
		Type:   "molecule",
		Labels: []string{"order-run:my-pool-order"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Step bead still open — pool never picked up the prior wisp's work.
	if _, err := store.Create(beads.Bead{
		Title:    "determine-period",
		ParentID: wispRoot.ID,
	}); err != nil {
		t.Fatal(err)
	}

	ran := false
	fakeExec := func(_ context.Context, _, _ string, _ []string) ([]byte, error) {
		ran = true
		return nil, nil
	}

	aa := []orders.Order{{
		Name:     "my-pool-order",
		Trigger:  "cooldown",
		Interval: "1s",
		Exec:     "scripts/run.sh",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)

	ad.dispatch(context.Background(), t.TempDir(), time.Now().Add(5*time.Second))
	ad.drain(context.Background())

	if ran {
		t.Fatal("exec must NOT run — prior wisp has open step beads, " +
			"the cooldown gate must treat it as in-flight (tr-kds01)")
	}
}

func TestOrderDispatchClosedTrackingHistoryStillChecksOpenWispWork(t *testing.T) {
	store := beads.NewMemStore()

	wispRoot, err := store.Create(beads.Bead{
		Title:  "mol-digest-generate",
		Type:   "molecule",
		Labels: []string{"order-run:my-pool-order"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(beads.Bead{
		Title:    "determine-period",
		ParentID: wispRoot.ID,
	}); err != nil {
		t.Fatal(err)
	}
	tracking, err := store.Create(beads.Bead{
		Title:     "order:my-pool-order",
		Labels:    []string{"order-run:my-pool-order", labelOrderTracking},
		Ephemeral: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(tracking.ID); err != nil {
		t.Fatal(err)
	}

	ran := false
	fakeExec := func(_ context.Context, _, _ string, _ []string) ([]byte, error) {
		ran = true
		return nil, nil
	}

	aa := []orders.Order{{
		Name:     "my-pool-order",
		Trigger:  "cooldown",
		Interval: "1s",
		Exec:     "scripts/run.sh",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)

	ad.dispatch(context.Background(), t.TempDir(), tracking.CreatedAt.Add(5*time.Second))
	ad.drain(context.Background())

	if ran {
		t.Fatal("exec must NOT run — closed order-tracking history must not bypass " +
			"the strict open-wisp check while prior step work is still open")
	}
}

// Unused but keep for future event assertion tests.
var (
	_ = (*memRecorder).hasSubject
	_ = strings.Contains
)

func TestResolveOrderExecTarget_UnboundRigErrors(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Rigs: []config.Rig{{
			Name: "frontend",
			Path: "", // unbound — no site binding
		}},
	}
	_, err := resolveOrderExecTarget("/city", cfg, orders.Order{Name: "deploy", Rig: "frontend"})
	if err == nil {
		t.Fatal("resolveOrderExecTarget: expected error for unbound rig, got nil")
	}
	if !strings.Contains(err.Error(), "frontend") {
		t.Errorf("error = %q, want mention of rig name 'frontend'", err)
	}
	if !strings.Contains(err.Error(), "no path binding") {
		t.Errorf("error = %q, want mention of 'no path binding'", err)
	}
}

func TestResolveOrderExecTarget_BoundRigDispatchesNormally(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Rigs: []config.Rig{{
			Name: "frontend",
			Path: "/home/user/frontend",
		}},
	}
	target, err := resolveOrderExecTarget("/city", cfg, orders.Order{Name: "deploy", Rig: "frontend"})
	if err != nil {
		t.Fatalf("resolveOrderExecTarget: unexpected error: %v", err)
	}
	if target.ScopeKind != "rig" {
		t.Errorf("ScopeKind = %q, want %q", target.ScopeKind, "rig")
	}
	if target.RigName != "frontend" {
		t.Errorf("RigName = %q, want %q", target.RigName, "frontend")
	}
	if target.ScopeRoot != "/home/user/frontend" {
		t.Errorf("ScopeRoot = %q, want %q", target.ScopeRoot, "/home/user/frontend")
	}
}

// --- drain tests (#991) ---

// TestOrderDispatcherDrainWaitsForInFlightDispatch confirms drain blocks
// until all in-flight dispatchOne goroutines finish, so the tracking bead
// outcome label is written before the controller exit path returns.
func TestOrderDispatcherDrainWaitsForInFlightDispatch(t *testing.T) {
	store := beads.NewMemStore()
	release := make(chan struct{})
	execStarted := make(chan struct{})

	fakeExec := func(_ context.Context, _, _ string, _ []string) ([]byte, error) {
		close(execStarted)
		<-release
		return []byte("ok\n"), nil
	}

	aa := []orders.Order{{
		Name:     "drain-test",
		Trigger:  "cooldown",
		Interval: "2m",
		Exec:     "scripts/drain.sh",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	<-execStarted

	drainDone := make(chan struct{})
	go func() {
		ad.drain(context.Background())
		close(drainDone)
	}()

	select {
	case <-drainDone:
		t.Fatal("drain returned before in-flight dispatch completed")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)

	select {
	case <-drainDone:
	case <-time.After(2 * time.Second):
		t.Fatal("drain did not return after in-flight dispatch released")
	}

	all := trackingBeads(t, store, "order-run:drain-test")
	hasExec := false
	for _, b := range all {
		for _, l := range b.Labels {
			if l == "exec" {
				hasExec = true
			}
		}
	}
	if !hasExec {
		t.Fatalf("tracking bead missing exec outcome label after drain; beads=%+v", all)
	}
}

// TestOrderDispatcherDrainRespectsContext verifies drain returns when the
// provided context expires, so shutdown remains bounded even when a
// dispatch goroutine is wedged. Compensating control: startup sweep closes
// any orphaned tracking beads on the next boot.
func TestOrderDispatcherDrainRespectsContext(t *testing.T) {
	store := beads.NewMemStore()
	release := make(chan struct{})
	defer close(release)
	execStarted := make(chan struct{})

	fakeExec := func(_ context.Context, _, _ string, _ []string) ([]byte, error) {
		close(execStarted)
		<-release
		return nil, nil
	}

	aa := []orders.Order{{
		Name:     "wedged",
		Trigger:  "cooldown",
		Interval: "2m",
		Exec:     "scripts/wedged.sh",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	<-execStarted

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	ad.drain(ctx)
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("drain exceeded context deadline by too much: %v", elapsed)
	}
	if ctx.Err() == nil {
		t.Fatal("expected context to be expired after drain returned")
	}
}

// TestOrderDispatcherDrainIdleReturnsImmediately verifies drain is a no-op
// when no dispatchOne goroutines are in flight.
func TestOrderDispatcherDrainIdleReturnsImmediately(t *testing.T) {
	aa := []orders.Order{{Name: "noop", Trigger: "cooldown", Interval: "2m", Exec: "true"}}
	ad := buildOrderDispatcherFromListExec(aa, beads.NewMemStore(), nil, successfulExec, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}
	done := make(chan struct{})
	go func() {
		ad.drain(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("drain on idle dispatcher did not return promptly")
	}
}

// TestOrderDispatcherCancelTerminatesInFlight verifies cancel() propagates
// to in-flight dispatchOne goroutines via context, so a follow-up drain
// returns promptly without waiting out the per-order timeout. Without
// this, shutdown can race t.TempDir cleanup against subprocesses still
// holding files inside .gc/ open.
func TestOrderDispatcherCancelTerminatesInFlight(t *testing.T) {
	store := beads.NewMemStore()
	execStarted := make(chan struct{})

	// Exec respects ctx — returns when canceled. This mirrors the
	// production runner's forced subprocess teardown on ctx.Done.
	fakeExec := func(ctx context.Context, _, _ string, _ []string) ([]byte, error) {
		close(execStarted)
		<-ctx.Done()
		return nil, ctx.Err()
	}

	aa := []orders.Order{{
		Name:     "cancel-test",
		Trigger:  "cooldown",
		Interval: "2m",
		Exec:     "scripts/cancel.sh",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}
	m, ok := ad.(*memoryOrderDispatcher)
	if !ok {
		t.Fatalf("expected *memoryOrderDispatcher, got %T", ad)
	}

	// Use Background so the only ctx that can cancel the dispatchOne is
	// the one cancel() controls — proves the hookup works.
	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	<-execStarted

	m.cancel()

	drainDone := make(chan struct{})
	go func() {
		ad.drain(context.Background())
		close(drainDone)
	}()

	select {
	case <-drainDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("drain did not return promptly after cancel()")
	}
}

// lockedWriter must serialize concurrent Write calls so log lines emitted
// from parallel dispatchOne goroutines do not interleave. Run under -race
// to also catch the underlying data race on the shared writer.
func TestLockedWriterSerializesConcurrentWrites(t *testing.T) {
	var buf bytes.Buffer
	lw := &lockedWriter{w: &buf}
	const goroutines = 16
	const writesPerG = 100
	line := []byte("dispatch err line\n")

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < writesPerG; j++ {
				if _, err := lw.Write(line); err != nil {
					t.Errorf("Write: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	wantBytes := goroutines * writesPerG * len(line)
	if got := buf.Len(); got != wantBytes {
		t.Fatalf("buffer length: got %d, want %d (suggests torn or lost writes)", got, wantBytes)
	}
	wantLine := bytes.TrimRight(line, "\n")
	for i, l := range bytes.Split(bytes.TrimRight(buf.Bytes(), "\n"), []byte{'\n'}) {
		if !bytes.Equal(l, wantLine) {
			t.Fatalf("line %d: got %q, want %q (interleaved bytes)", i, l, wantLine)
		}
	}
}

// TestOrderExecEnvSetsBeadsActorToOrderName verifies that exec orders
// spawned by the controller carry an order-scoped BEADS_ACTOR into their
// subprocess env, so any bd shell-out from inside the order's command
// (e.g., `gc order sweep-tracking` → `bd close`) is audit-logged as
// "order:<name>" rather than an ambient identity. This is what gives
// the dashboard fine-grained attribution per order.
func TestOrderExecEnvSetsBeadsActorToOrderName(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	_ = os.Unsetenv("BEADS_ACTOR")

	cityDir := t.TempDir()
	target := execStoreTarget{ScopeRoot: cityDir, ScopeKind: "city", Prefix: "pc"}
	a := orders.Order{Name: "order-tracking-sweep", Trigger: "cooldown", Interval: "1m", Exec: "true"}

	envSlice, err := orderExecEnvWithError(cityDir, nil, target, a, nil)
	if err != nil {
		t.Fatalf("orderExecEnvWithError() error = %v", err)
	}

	want := "BEADS_ACTOR=order:order-tracking-sweep"
	found := false
	for _, entry := range envSlice {
		if entry == want {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("orderExecEnv missing %q; env=%v", want, envSlice)
	}
}

// TestOrderExecEnvScrubsAmbientDoltEnvForCityWithoutDoltTarget pins the
// projection contract the core maintenance scripts' no-Dolt guard relies
// on: for a city without a canonical Dolt target (e.g. `[beads] provider =
// "file"`), the order exec env defines every projected GC_DOLT_* key as
// explicitly empty, so mergeOrderExecEnv drops ambient operator values and
// Dolt-dependent core orders cannot be aimed at a server outside the city.
func TestOrderExecEnvScrubsAmbientDoltEnvForCityWithoutDoltTarget(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT_HOST", "ambient.example.internal")
	t.Setenv("GC_DOLT_PORT", "4406")
	_ = os.Unsetenv("BEADS_ACTOR")

	cityDir := t.TempDir()
	target := execStoreTarget{ScopeRoot: cityDir, ScopeKind: "city", Prefix: "pc"}
	a := orders.Order{Name: "jsonl-export", Trigger: "cooldown", Interval: "15m", Exec: "true"}

	envSlice, err := orderExecEnvWithError(cityDir, nil, target, a, nil)
	if err != nil {
		t.Fatalf("orderExecEnvWithError() error = %v", err)
	}
	overrides := map[string]string{}
	for _, entry := range envSlice {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			overrides[key] = value
		}
	}
	for _, key := range []string{"GC_DOLT_HOST", "GC_DOLT_PORT"} {
		value, defined := overrides[key]
		if !defined {
			t.Fatalf("order env does not define %s; ambient controller env would leak through: %v", key, envSlice)
		}
		if value != "" {
			t.Fatalf("%s = %q, want explicitly empty for a city without a dolt target", key, value)
		}
	}

	merged := mergeOrderExecEnv([]string{"GC_DOLT_HOST=ambient.example.internal", "GC_DOLT_PORT=4406"}, envSlice)
	for _, entry := range merged {
		if entry == "GC_DOLT_PORT=4406" || entry == "GC_DOLT_HOST=ambient.example.internal" {
			t.Fatalf("ambient dolt env survived merge: %q in %v", entry, merged)
		}
	}
}

// TestOrderExecEnvAppliesOrderEnvOverrides verifies that env entries
// declared in `[order.env]` on the order TOML reach the dispatched
// subprocess. This is the per-order tuning knob for threshold env vars
// (GC_DOCTOR_LATENCY_WARN_S, GC_JSONL_SPIKE_THRESHOLD, etc.) that would
// otherwise require editing the controller's parent environment.
func TestOrderExecEnvAppliesOrderEnvOverrides(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	_ = os.Unsetenv("BEADS_ACTOR")

	cityDir := t.TempDir()
	target := execStoreTarget{ScopeRoot: cityDir, ScopeKind: "city", Prefix: "pc"}
	a := orders.Order{
		Name:     "doctor",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     "true",
		Env: map[string]string{
			"GC_DOCTOR_LATENCY_WARN_S": "5",
			"CUSTOM_ORDER_FLAG":        "yes",
		},
	}

	envSlice, err := orderExecEnvWithError(cityDir, nil, target, a, nil)
	if err != nil {
		t.Fatalf("orderExecEnvWithError() error = %v", err)
	}

	wants := []string{
		"GC_DOCTOR_LATENCY_WARN_S=5",
		"CUSTOM_ORDER_FLAG=yes",
	}
	for _, want := range wants {
		found := false
		for _, entry := range envSlice {
			if entry == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("orderExecEnv missing %q; env=%v", want, envSlice)
		}
	}
}

// TestOrderExecEnvProjectsGitHubToken verifies that the controller's ambient
// GitHub CLI auth tokens reach an exec order's subprocess env. Merge orders
// shell out to `gh` (via the workflows pack), which authenticates from GH_TOKEN
// / GITHUB_TOKEN; both keys are execenv.IsSensitiveKey so the curated exec env
// would otherwise strip them and every merge order's `gh` call would fail auth.
func TestOrderExecEnvProjectsGitHubToken(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GH_TOKEN", "ghs_controller_token")
	t.Setenv("GITHUB_TOKEN", "github_pat_controller")
	_ = os.Unsetenv("BEADS_ACTOR")

	cityDir := t.TempDir()
	target := execStoreTarget{ScopeRoot: cityDir, ScopeKind: "city", Prefix: "pc"}
	a := orders.Order{Name: "pr-merge", Trigger: "cooldown", Interval: "1m", Exec: "gh pr merge"}

	envSlice, err := orderExecEnvWithError(cityDir, nil, target, a, nil)
	if err != nil {
		t.Fatalf("orderExecEnvWithError() error = %v", err)
	}
	for _, want := range []string{
		"GH_TOKEN=ghs_controller_token",
		"GITHUB_TOKEN=github_pat_controller",
	} {
		found := false
		for _, entry := range envSlice {
			if entry == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("orderExecEnv missing %q; every `gh` call in the order would fail auth. env=%v", want, envSlice)
		}
	}

	// Prove the projection survives the final mergeOrderExecEnv boundary, where
	// FilterInherited strips inherited sensitive keys before overrides are
	// appended. That boundary is where the bug lived: an ambient token inherited
	// from the parent env is dropped, so only the projected override keeps the
	// child's `gh` calls authenticated. Mirrors the Dolt-scrub merge-boundary
	// assertion in TestOrderExecEnvScrubsAmbientDoltEnvForCityWithoutDoltTarget.
	merged := mergeOrderExecEnv([]string{
		"GH_TOKEN=ambient_inherited",
		"GITHUB_TOKEN=ambient_inherited",
	}, envSlice)
	for _, want := range []string{
		"GH_TOKEN=ghs_controller_token",
		"GITHUB_TOKEN=github_pat_controller",
	} {
		found := false
		for _, entry := range merged {
			if entry == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("projected %q did not survive mergeOrderExecEnv; the sensitive-key filter would silently re-break the order's `gh` auth. merged=%v", want, merged)
		}
	}
	for _, unwanted := range []string{
		"GH_TOKEN=ambient_inherited",
		"GITHUB_TOKEN=ambient_inherited",
	} {
		for _, entry := range merged {
			if entry == unwanted {
				t.Errorf("ambient inherited token survived mergeOrderExecEnv: %q in %v", unwanted, merged)
			}
		}
	}
}

// TestOrderExecEnvGitHubTokenOrderEnvOverrideWins verifies an explicit
// [order.env] GH_TOKEN beats the controller's ambient token, so an order can
// scope its own credential when needed.
func TestOrderExecEnvGitHubTokenOrderEnvOverrideWins(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GH_TOKEN", "ghs_ambient")
	_ = os.Unsetenv("BEADS_ACTOR")

	cityDir := t.TempDir()
	target := execStoreTarget{ScopeRoot: cityDir, ScopeKind: "city", Prefix: "pc"}
	a := orders.Order{
		Name:     "pr-merge",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     "gh pr merge",
		Env:      map[string]string{"GH_TOKEN": "ghs_order_scoped"},
	}

	envSlice, err := orderExecEnvWithError(cityDir, nil, target, a, nil)
	if err != nil {
		t.Fatalf("orderExecEnvWithError() error = %v", err)
	}
	var gotScoped, gotAmbient bool
	for _, entry := range envSlice {
		switch entry {
		case "GH_TOKEN=ghs_order_scoped":
			gotScoped = true
		case "GH_TOKEN=ghs_ambient":
			gotAmbient = true
		}
	}
	if gotAmbient {
		t.Fatalf("ambient GH_TOKEN leaked past the order.env override; env=%v", envSlice)
	}
	if !gotScoped {
		t.Fatalf("order.env GH_TOKEN override missing; env=%v", envSlice)
	}
}

// TestOrderExecEnvRejectsReservedOrderEnvKeys verifies that `[order.env]`
// cannot shadow controller-owned routing and identity variables after the
// store target has already been resolved.
func TestOrderExecEnvRejectsReservedOrderEnvKeys(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")

	cityDir := t.TempDir()
	target := execStoreTarget{ScopeRoot: cityDir, ScopeKind: "city", Prefix: "pc"}
	for _, key := range []string{
		"GC_STORE_SCOPE",
		"ORDER_DIR",
		"PACK_DIR",
		"BD_EXPORT_AUTO",
		"GC_BEADS",
		"GC_BEADS_SCOPE_ROOT",
	} {
		t.Run(key, func(t *testing.T) {
			a := orders.Order{
				Name:     "scoped-order",
				Trigger:  "cooldown",
				Interval: "1m",
				Exec:     "true",
				Env: map[string]string{
					key: "overridden",
				},
			}

			_, err := orderExecEnvWithError(cityDir, nil, target, a, nil)
			if err == nil {
				t.Fatal("orderExecEnvWithError() succeeded; want reserved env key error")
			}
			if !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "controller-owned") {
				t.Fatalf("error = %q, want controller-owned %s diagnostic", err, key)
			}
		})
	}
}

func TestOrderExecEnvReservedKeysCoverProjectedEnv(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")

	cityDir := t.TempDir()
	packDir := filepath.Join(cityDir, "packs", "maintenance")
	target := execStoreTarget{ScopeRoot: cityDir, ScopeKind: "city", Prefix: "pc"}
	a := orders.Order{
		Name:         "scoped-order",
		Trigger:      "cooldown",
		Interval:     "1m",
		Exec:         "true",
		Source:       filepath.Join(packDir, "orders", "scoped-order.toml"),
		FormulaLayer: filepath.Join(packDir, "formulas"),
	}

	envSlice, err := orderExecEnvWithError(cityDir, nil, target, a, nil)
	if err != nil {
		t.Fatalf("orderExecEnvWithError() error = %v", err)
	}

	var unreserved []string
	for _, entry := range envSlice {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if !isReservedOrderExecEnvKey(key) {
			unreserved = append(unreserved, key)
		}
	}
	if len(unreserved) > 0 {
		t.Fatalf("projected order exec env keys missing from reserved guard: %v", unreserved)
	}
}

// TestOrderExecEnvSkipsBeadsActorForUnnamedOrder guards against accidentally
// emitting "BEADS_ACTOR=order:" (empty suffix) when an order has no name.
// The conditional in orderExecEnv prevents that — verified here so future
// edits don't regress.
func TestOrderExecEnvSkipsBeadsActorForUnnamedOrder(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	_ = os.Unsetenv("BEADS_ACTOR")

	cityDir := t.TempDir()
	target := execStoreTarget{ScopeRoot: cityDir, ScopeKind: "city", Prefix: "pc"}
	a := orders.Order{Trigger: "cooldown", Interval: "1m", Exec: "true"} // no Name

	envSlice, err := orderExecEnvWithError(cityDir, nil, target, a, nil)
	if err != nil {
		t.Fatalf("orderExecEnvWithError() error = %v", err)
	}

	for _, entry := range envSlice {
		if entry == "BEADS_ACTOR=order:" {
			t.Fatalf("orderExecEnv emitted bare order: prefix for unnamed order; env=%v", envSlice)
		}
	}
}

func TestOrderExecEnvWithError_SurfacesPostgresProjectionError(t *testing.T) {
	clearAmbientPostgresEnv(t)
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	writePGScopeFixture(t, cityDir, "")
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(`issue_prefix: city
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	target := execStoreTarget{ScopeRoot: cityDir, ScopeKind: "city", Prefix: "pc"}
	a := orders.Order{Name: "pg-order", Trigger: "cooldown", Interval: "1m", Exec: "true"}

	_, err := orderExecEnvWithError(cityDir, nil, target, a, nil)
	if err == nil {
		t.Fatal("orderExecEnvWithError() error = nil, want postgres projection error")
	}
	if !errors.Is(err, pgauth.ErrNoPasswordResolvable) {
		t.Fatalf("errors.Is(err, ErrNoPasswordResolvable) = false, want true; err=%v", err)
	}
}

func TestOrderExecEnvWithError_PostgresCityClearsDoltOverlay(t *testing.T) {
	clearAmbientPostgresEnv(t)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")

	cityDir := t.TempDir()
	writePGScopeFixture(t, cityDir, "citypw")
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(`issue_prefix: city
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = writeReachableManagedDoltState(t, cityDir)

	target := execStoreTarget{ScopeRoot: cityDir, ScopeKind: "city", Prefix: "ct"}
	a := orders.Order{Name: "pg-city-order", Trigger: "cooldown", Interval: "1m", Exec: "true"}

	env, err := orderExecEnvWithError(cityDir, nil, target, a, nil)
	if err != nil {
		t.Fatalf("orderExecEnvWithError() error = %v", err)
	}
	got := listToMap(env)

	assertPostgresOrderEnv(t, got, "citypw")
	assertNoDoltOrderEnv(t, got)
}

func TestOrderTriggerOptionsForTarget_PostgresRigClearsDoltOverlay(t *testing.T) {
	clearAmbientPostgresEnv(t)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")

	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(`issue_prefix: city
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = writeReachableManagedDoltState(t, cityDir)

	rigDir := filepath.Join(cityDir, "rigs", "pg")
	writePGScopeFixture(t, rigDir, "rigpw")
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: pg
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "pg", Path: "rigs/pg", Prefix: "pg"}}}
	resolveRigPaths(cityDir, cfg.Rigs)
	target := execStoreTarget{ScopeRoot: rigDir, ScopeKind: "rig", Prefix: "pg", RigName: "pg"}
	a := orders.Order{Name: "pg-rig-order", Rig: "pg", Trigger: "condition", Check: "bd ready --json", Exec: "true"}

	opts, err := orderTriggerOptionsForTarget(cityDir, cfg, target, a)
	if err != nil {
		t.Fatalf("orderTriggerOptionsForTarget() error = %v", err)
	}
	got := listToMap(opts.ConditionEnv)

	if opts.ConditionDir != rigDir {
		t.Fatalf("ConditionDir = %q, want %q", opts.ConditionDir, rigDir)
	}
	assertPostgresOrderEnv(t, got, "rigpw")
	assertNoDoltOrderEnv(t, got)
}

func assertPostgresOrderEnv(t *testing.T, env map[string]string, wantPassword string) {
	t.Helper()
	want := map[string]string{
		"GC_POSTGRES_PASSWORD":    wantPassword,
		"BEADS_POSTGRES_PASSWORD": wantPassword,
		"BEADS_POSTGRES_HOST":     "db.example.test",
		"BEADS_POSTGRES_PORT":     "5432",
		"BEADS_POSTGRES_USER":     "bd",
		"BEADS_POSTGRES_DATABASE": "beads",
	}
	for key, value := range want {
		if got := env[key]; got != value {
			t.Errorf("env[%q] = %q, want %q", key, got, value)
		}
	}
}

func assertNoDoltOrderEnv(t *testing.T, env map[string]string) {
	t.Helper()
	for _, key := range projectedDoltEnvKeys {
		if value, ok := env[key]; ok && value != "" {
			t.Errorf("env[%q] = %q, want empty/absent for PG-backed order", key, value)
		}
	}
	for _, key := range []string{
		"GC_DOLT_MANAGED_LOCAL",
		"GC_DOLT_DATA_DIR",
		"GC_DOLT_LOG_FILE",
		"GC_DOLT_STATE_FILE",
		"GC_DOLT_PID_FILE",
		"GC_DOLT_LOCK_FILE",
		"GC_DOLT_CONFIG_FILE",
	} {
		if value, ok := env[key]; ok && value != "" {
			t.Errorf("env[%q] = %q, want empty/absent for PG-backed order", key, value)
		}
	}
}

func TestLockedStderrPreservesNil(t *testing.T) {
	if got := lockedStderr(nil); got != nil {
		t.Fatalf("lockedStderr(nil): got %v, want nil", got)
	}
}

func TestLockedStderrWrapsNonNil(t *testing.T) {
	var buf bytes.Buffer
	w := lockedStderr(&buf)
	if _, ok := w.(*lockedWriter); !ok {
		t.Fatalf("lockedStderr(non-nil): got %T, want *lockedWriter", w)
	}
}

// TestOrderDispatchTrackingBeadIsNoHistory asserts the dispatcher routes the
// tracking bead to the no-history tier, so each cooldown cycle avoids a full
// Dolt commit while remaining visible to normal issue-tier reads.
func TestOrderDispatchTrackingBeadIsNoHistory(t *testing.T) {
	store := beads.NewMemStore()

	aa := []orders.Order{{
		Name:         "wisp-order",
		Trigger:      "cooldown",
		Interval:     "1m",
		Formula:      "test-formula",
		Pool:         "worker",
		FormulaLayer: sharedTestFormulaDir,
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	ad.drain(context.Background())

	all := trackingBeads(t, store, "order-run:wisp-order")
	var tb *beads.Bead
	for i := range all {
		if strings.HasPrefix(all[i].Title, "order:") {
			tb = &all[i]
			break
		}
	}
	if tb == nil {
		t.Fatal("no tracking bead found")
	}
	if tb.Ephemeral || !tb.NoHistory {
		t.Errorf("tracking bead storage = Ephemeral:%v NoHistory:%v, want no-history only", tb.Ephemeral, tb.NoHistory)
	}
	// No-history beads remain visible to issue-tier reads, which keeps
	// bd 1.0.4 ready/list compatibility while avoiding Dolt history pressure.
	issuesOnly, err := store.ListByLabel("order-run:wisp-order", 0, beads.IncludeClosed)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range issuesOnly {
		if strings.HasPrefix(b.Title, "order:") {
			found = true
			if b.Ephemeral || !b.NoHistory {
				t.Errorf("issue-tier tracking bead storage = Ephemeral:%v NoHistory:%v, want no-history only", b.Ephemeral, b.NoHistory)
			}
		}
	}
	if !found {
		t.Errorf("no-history tracking bead was not visible to issues-tier query")
	}
}

// TestOrderDispatchSingleFlightLockSeesNoHistoryTracker is the regression
// guard for the single-flight lock (`hasOpenWorkInStoresStrict`) after the
// tracking bead moved out of history. If the lock query were not
// tier-aware, the dispatcher would re-fire the same cooldown order on the
// next tick.
func TestOrderDispatchSingleFlightLockSeesNoHistoryTracker(t *testing.T) {
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:     "order:double-fire",
		Labels:    []string{"order-run:double-fire", labelOrderTracking},
		NoHistory: true,
	}); err != nil {
		t.Fatal(err)
	}

	m := &memoryOrderDispatcher{}
	hasOpen, err := m.hasOpenWorkStrict(store, "double-fire")
	if err != nil {
		t.Fatal(err)
	}
	if !hasOpen {
		t.Fatal("single-flight lock missed no-history tracking bead; would dispatch again")
	}
}

func TestOrderDispatchSingleFlightLockSeesBackingOnlyCachedTracker(t *testing.T) {
	backing := beads.NewMemStore()
	store := beads.NewCachingStoreForTest(backing, nil)
	if err := store.PrimeActive(); err != nil {
		t.Fatalf("prime cache: %v", err)
	}
	if _, err := backing.Create(beads.Bead{
		Title:     "order:double-fire",
		Labels:    []string{"order-run:double-fire", labelOrderTracking},
		NoHistory: true,
	}); err != nil {
		t.Fatal(err)
	}

	m := &memoryOrderDispatcher{}
	hasOpen, err := m.hasOpenWorkStrict(store, "double-fire")
	if err != nil {
		t.Fatal(err)
	}
	if !hasOpen {
		t.Fatal("single-flight lock missed backing-only tracking bead behind a primed cache")
	}
}

func TestOrderDispatchSingleFlightLockFailsClosedOnPartialTierError(t *testing.T) {
	store := &partialListStore{
		Store: beads.NewMemStore(),
		rows: []beads.Bead{{
			ID:     "tracker-1",
			Title:  "order:double-fire",
			Status: "open",
			Labels: []string{"order-run:double-fire", labelOrderTracking},
		}},
		err: fmt.Errorf("wisps tier unavailable"),
	}

	m := &memoryOrderDispatcher{}
	hasOpen, err := m.hasOpenWorkStrict(store, "double-fire")
	if err == nil {
		t.Fatal("hasOpenWorkStrict err = nil, want conservative failure on partial tier error")
	}
	if hasOpen {
		t.Fatal("hasOpenWorkStrict returned true with partial tier error; caller must fail closed instead")
	}
}

// --- dispatch() store-handle close regression tests (ga-anio6p) ---
//
// dispatch() must close every store handle it opens each pass via
// closeBeadStoreHandle, which type-asserts for interface{ CloseStore() error }.
// The close is deferred to a detached closer goroutine that runs once the
// in-flight dispatchOne goroutines launched that tick have released the handles
// (gascity#3157) — closing inline would race those goroutines on a native
// store's one-way close latch. These tests therefore drain and then poll for
// the close rather than asserting it synchronously at dispatch() return.

// dispatchCloseStoreSpy wraps MemStore and counts CloseStore() calls. CloseStore
// runs on dispatch()'s detached closer goroutine, so access to the counter is
// serialized through closeCount.
type dispatchCloseStoreSpy struct {
	*beads.MemStore
	mu       sync.Mutex
	closed   int
	closeErr error
}

func (s *dispatchCloseStoreSpy) CloseStore() error {
	s.mu.Lock()
	s.closed++
	s.mu.Unlock()
	return s.closeErr
}

// closeCount returns how many times CloseStore has been called, synchronized
// against the detached closer goroutine.
func (s *dispatchCloseStoreSpy) closeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// newDispatchCloseStoreSpyFn returns an orderStoreFunc that appends a fresh
// dispatchCloseStoreSpy to *spies on each call and returns it as a Store.
func newDispatchCloseStoreSpyFn(spies *[]*dispatchCloseStoreSpy) orderStoreFunc {
	return func(_ execStoreTarget) (beads.Store, error) {
		spy := &dispatchCloseStoreSpy{MemStore: beads.NewMemStore()}
		*spies = append(*spies, spy)
		return spy, nil
	}
}

// waitForDispatchCloseCounts drains the in-flight dispatchOne goroutines, then
// waits for dispatch()'s detached closer to close every spied handle exactly
// `want` times. The close lands shortly after drain() returns (gascity#3157),
// so this polls with a deadline rather than asserting synchronously.
func waitForDispatchCloseCounts(t *testing.T, m *memoryOrderDispatcher, spies []*dispatchCloseStoreSpy, want int) {
	t.Helper()
	drainCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !m.drain(drainCtx) {
		t.Fatal("drain timed out waiting for in-flight dispatchOne to finish")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		reached := true
		for _, spy := range spies {
			if spy.closeCount() < want {
				reached = false
				break
			}
		}
		if reached || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	for i, spy := range spies {
		if got := spy.closeCount(); got != want {
			t.Errorf("store[%d]: CloseStore() called %d times, want %d", i, got, want)
		}
	}
}

func TestDispatchClosesEveryOpenedStoreHandle(t *testing.T) {
	var spies []*dispatchCloseStoreSpy
	m := &memoryOrderDispatcher{
		aa: []orders.Order{{
			Name:     "noop",
			Trigger:  "cooldown",
			Interval: "1m",
			Exec:     "true",
		}},
		storeFn: newDispatchCloseStoreSpyFn(&spies),
		execRun: func(_ context.Context, _, _ string, _ []string) ([]byte, error) { return nil, nil },
		rec:     events.Discard,
		stderr:  &bytes.Buffer{},
		cfg:     &config.City{},
	}

	m.dispatch(context.Background(), t.TempDir(), time.Now())

	if len(spies) == 0 {
		t.Fatal("storeFn never called: expected at least one store to be opened")
	}
	waitForDispatchCloseCounts(t, m, spies, 1)
}

func TestDispatchClosesRigAndLegacyCityStoreHandles(t *testing.T) {
	var spies []*dispatchCloseStoreSpy
	cityPath := t.TempDir()
	rigPath := t.TempDir()

	m := &memoryOrderDispatcher{
		aa: []orders.Order{{
			Name:     "rig-noop",
			Rig:      "worker",
			Trigger:  "cooldown",
			Interval: "1m",
			Exec:     "true",
		}},
		storeFn: newDispatchCloseStoreSpyFn(&spies),
		execRun: func(_ context.Context, _, _ string, _ []string) ([]byte, error) { return nil, nil },
		rec:     events.Discard,
		stderr:  &bytes.Buffer{},
		cfg: &config.City{
			Rigs: []config.Rig{{Name: "worker", Path: rigPath}},
		},
	}

	m.dispatch(context.Background(), cityPath, time.Now())

	if len(spies) != 2 {
		t.Fatalf("storeFn called %d times, want 2 (rig + legacy city fallback)", len(spies))
	}
	waitForDispatchCloseCounts(t, m, spies, 1)
}

func TestDispatchDeduplicatesStoreHandlesAcrossOrders(t *testing.T) {
	var spies []*dispatchCloseStoreSpy
	m := &memoryOrderDispatcher{
		aa: []orders.Order{
			{Name: "order-a", Trigger: "cooldown", Interval: "1m", Exec: "true"},
			{Name: "order-b", Trigger: "cooldown", Interval: "1m", Exec: "true"},
		},
		storeFn: newDispatchCloseStoreSpyFn(&spies),
		execRun: func(_ context.Context, _, _ string, _ []string) ([]byte, error) { return nil, nil },
		rec:     events.Discard,
		stderr:  &bytes.Buffer{},
		cfg:     &config.City{},
	}

	m.dispatch(context.Background(), t.TempDir(), time.Now())

	if len(spies) != 1 {
		t.Fatalf("storeFn called %d times, want 1 (same target deduped across orders)", len(spies))
	}
	waitForDispatchCloseCounts(t, m, spies, 1)
}

func TestDispatchClosesNoStoresWhenCitySuspended(t *testing.T) {
	var spies []*dispatchCloseStoreSpy
	m := &memoryOrderDispatcher{
		aa: []orders.Order{{
			Name:     "noop",
			Trigger:  "cooldown",
			Interval: "1m",
			Exec:     "true",
		}},
		storeFn: newDispatchCloseStoreSpyFn(&spies),
		execRun: func(_ context.Context, _, _ string, _ []string) ([]byte, error) { return nil, nil },
		rec:     events.Discard,
		stderr:  &bytes.Buffer{},
		cfg:     &config.City{Workspace: config.Workspace{Suspended: true}},
	}

	m.dispatch(context.Background(), t.TempDir(), time.Now())

	if len(spies) != 0 {
		t.Errorf("storeFn called %d times, want 0 when city is suspended", len(spies))
	}
}

func TestSweepClosedOrderTrackingRetentionAcrossStoresBounded_HonorsBudgetAcrossStores(t *testing.T) {
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	policy := orderTrackingRetentionPolicy{
		deleteAfterClose: 24 * time.Hour,
		retainLast:       minClosedOrderTrackingRetained,
	}
	// Each store gets minClosedOrderTrackingRetained+3 beads (48h old, past 24h TTL), so 3 are eligible per store.
	makeStore := func(prefix string) *beads.MemStore {
		seed := make([]beads.Bead, 0, minClosedOrderTrackingRetained+3)
		for i := range minClosedOrderTrackingRetained + 3 {
			seed = append(seed, beads.Bead{
				ID:        fmt.Sprintf("%s-%02d", prefix, i),
				Title:     "order:" + prefix,
				Status:    "closed",
				Type:      "task",
				CreatedAt: now.Add(-48*time.Hour + time.Duration(i)*time.Minute),
				Labels:    []string{"order-run:" + prefix, labelOrderTracking},
				Ephemeral: true,
			})
		}
		return beads.NewMemStoreFrom(100, seed, nil)
	}
	storeA := makeStore("alpha")
	storeB := makeStore("beta")

	// limit=4: budget spans both stores (3 eligible each = 6 total), stops at 4.
	deleted, err := sweepClosedOrderTrackingRetentionAcrossStoresBounded(
		[]beads.Store{storeA, storeB}, now, policy, nil, 4)
	if err != nil {
		t.Fatalf("sweepClosedOrderTrackingRetentionAcrossStoresBounded: %v", err)
	}
	if deleted != 4 {
		t.Fatalf("deleted = %d, want 4 (budget limit)", deleted)
	}
}

func TestSweepClosedOrderTrackingRetentionAcrossStoresBounded_ReturnsPartialCountWithNilErrorOnBudgetExhaustion(t *testing.T) {
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	policy := orderTrackingRetentionPolicy{
		deleteAfterClose: 24 * time.Hour,
		retainLast:       minClosedOrderTrackingRetained,
	}
	seed := make([]beads.Bead, 0, minClosedOrderTrackingRetained+5)
	for i := range minClosedOrderTrackingRetained + 5 {
		seed = append(seed, beads.Bead{
			ID:        fmt.Sprintf("ga-%02d", i),
			Title:     "order:ga",
			Status:    "closed",
			Type:      "task",
			CreatedAt: now.Add(-48*time.Hour + time.Duration(i)*time.Minute),
			Labels:    []string{"order-run:ga", labelOrderTracking},
			Ephemeral: true,
		})
	}
	store := beads.NewMemStoreFrom(100, seed, nil)

	// limit=2, 5 eligible: returns 2 with nil error.
	deleted, err := sweepClosedOrderTrackingRetentionAcrossStoresBounded(
		[]beads.Store{store}, now, policy, nil, 2)
	if err != nil {
		t.Fatalf("sweepClosedOrderTrackingRetentionAcrossStoresBounded: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2 (budget limit)", deleted)
	}
}

func TestSweepClosedOrderTrackingRetentionAcrossStoresBounded_DoesNotBypassRetainFloor(t *testing.T) {
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	policy := orderTrackingRetentionPolicy{
		deleteAfterClose: 24 * time.Hour,
		retainLast:       minClosedOrderTrackingRetained,
	}
	// Exactly minClosedOrderTrackingRetained beads — all at the floor, none eligible.
	seed := make([]beads.Bead, 0, minClosedOrderTrackingRetained)
	for i := range minClosedOrderTrackingRetained {
		seed = append(seed, beads.Bead{
			ID:        fmt.Sprintf("floor-%02d", i),
			Title:     "order:floor",
			Status:    "closed",
			Type:      "task",
			CreatedAt: now.Add(-48*time.Hour + time.Duration(i)*time.Minute),
			Labels:    []string{"order-run:floor", labelOrderTracking},
			Ephemeral: true,
		})
	}
	store := beads.NewMemStoreFrom(100, seed, nil)

	deleted, err := sweepClosedOrderTrackingRetentionAcrossStoresBounded(
		[]beads.Store{store}, now, policy, nil, 100)
	if err != nil {
		t.Fatalf("sweepClosedOrderTrackingRetentionAcrossStoresBounded: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("deleted = %d, want 0 (retain-10 floor must hold)", deleted)
	}
}

func TestSweepClosedOrderTrackingRetentionAcrossStoresBounded_ZeroLimitDeletesNothing(t *testing.T) {
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	policy := orderTrackingRetentionPolicy{
		deleteAfterClose: 24 * time.Hour,
		retainLast:       minClosedOrderTrackingRetained,
	}
	seed := make([]beads.Bead, 0, minClosedOrderTrackingRetained+3)
	for i := range minClosedOrderTrackingRetained + 3 {
		seed = append(seed, beads.Bead{
			ID:        fmt.Sprintf("zero-%02d", i),
			Title:     "order:zero",
			Status:    "closed",
			Type:      "task",
			CreatedAt: now.Add(-48*time.Hour + time.Duration(i)*time.Minute),
			Labels:    []string{"order-run:zero", labelOrderTracking},
			Ephemeral: true,
		})
	}
	store := beads.NewMemStoreFrom(100, seed, nil)

	deleted, err := sweepClosedOrderTrackingRetentionAcrossStoresBounded(
		[]beads.Store{store}, now, policy, nil, 0)
	if err != nil {
		t.Fatalf("sweepClosedOrderTrackingRetentionAcrossStoresBounded: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("deleted = %d, want 0 (limit=0 means no budget)", deleted)
	}
}

// TestLastRunFuncGatesFallbackOnIndexMiss pins #3201: the per-order fallback
// (a serial bd-query) must fire only on a genuine index miss. An index hit must
// return the indexed time without consulting the fallback — otherwise every
// cooldown/cron order pays the query on each cold-cache dispatch and hangs
// gc reload/gc doctor.
func TestLastRunFuncGatesFallbackOnIndexMiss(t *testing.T) {
	store := beads.NewMemStore()
	const storeKey = "city"
	idx := newOrderDispatchTrackingIndex()
	indexed := time.Now().Add(-time.Hour)
	// Pre-seed the history index so lastRunForStore reads it without listing
	// the store. The "\x00history" suffix matches historyEntriesForStore's key.
	idx.entries[storeKey+"\x00history"] = map[string]orderTrackingSummary{
		"order-hit": {lastRun: indexed},
	}

	fallbackCalls := 0
	fallbackTime := time.Now() // deliberately newer than the indexed entry
	fallback := func(string) (time.Time, error) {
		fallbackCalls++
		return fallbackTime, nil
	}
	fn := idx.lastRunFunc([]beads.Store{store}, []string{storeKey}, fallback)

	got, err := fn("order-hit")
	if err != nil {
		t.Fatalf("lastRunFunc(order-hit): %v", err)
	}
	if !got.Equal(indexed) {
		t.Errorf("index hit returned %v, want indexed %v", got, indexed)
	}
	if fallbackCalls != 0 {
		t.Errorf("index hit invoked fallback %d times, want 0 (the cold-cache storm)", fallbackCalls)
	}

	got, err = fn("order-miss")
	if err != nil {
		t.Fatalf("lastRunFunc(order-miss): %v", err)
	}
	if !got.Equal(fallbackTime) {
		t.Errorf("index miss returned %v, want fallback %v", got, fallbackTime)
	}
	if fallbackCalls != 1 {
		t.Errorf("index miss invoked fallback %d times, want 1", fallbackCalls)
	}
}

// TestCarryLastRunCacheFrom pins the #3201 complement: a rebuilt dispatcher
// inherits warm last-run entries (forward-only) from its predecessor, and a
// nil/empty predecessor is a no-op.
func TestCarryLastRunCacheFrom(t *testing.T) {
	keys := []string{"city"}
	older := time.Now().Add(-2 * time.Hour)
	newer := time.Now().Add(-time.Hour)

	prev := &memoryOrderDispatcher{}
	prev.rememberLastRun("order-a", keys, newer)
	prev.rememberLastRun("order-b", keys, older)

	next := &memoryOrderDispatcher{}
	next.rememberLastRun("order-a", keys, older) // stale; carry must advance it
	next.carryLastRunCacheFrom(prev)

	if got := next.lastRunCache[orderHistoryCacheKey("order-a", keys)]; !got.Equal(newer) {
		t.Errorf("order-a = %v, want newer %v (forward-only carry)", got, newer)
	}
	if got := next.lastRunCache[orderHistoryCacheKey("order-b", keys)]; !got.Equal(older) {
		t.Errorf("order-b = %v, want %v", got, older)
	}

	next.carryLastRunCacheFrom(&memoryOrderDispatcher{}) // empty source
	next.carryLastRunCacheFrom(nil)                      // nil source
	if len(next.lastRunCache) != 2 {
		t.Errorf("cache size = %d after no-op carries, want 2", len(next.lastRunCache))
	}
}

// A panic inside a detached dispatch goroutine must be contained by
// runDispatchGuarded, not crash the supervisor (the webhook fast-ACK path has
// already returned its HTTP response past any recovery middleware). A nil
// recorder makes dispatchOne panic at its OrderFired emit; the guard must
// recover and log it rather than let the panic escape the goroutine.
func TestRunDispatchGuardedRecoversPanic(t *testing.T) {
	var logs bytes.Buffer
	m := &memoryOrderDispatcher{stderr: &logs} // rec is nil → dispatchOne panics on Record

	order := orders.Order{Name: "boom", Trigger: "webhook", Formula: "f"}
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.runDispatchGuarded(context.Background(), beads.NewMemStore(), execStoreTarget{}, order, "/city", "track-x", nil, nil)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runDispatchGuarded did not return — a dispatch-goroutine panic was not recovered")
	}
	if !strings.Contains(logs.String(), "panic") {
		t.Errorf("expected the recovered panic to be logged, got %q", logs.String())
	}
}
