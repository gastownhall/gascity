package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestCityRuntimeReloadRoutesLifecycleToConfiguredEventsProvider(t *testing.T) {
	clearInheritedBeadsEnv(t)

	previousOpenCityStore := newControllerStateOpenCityStore
	cityBacking := beads.NewMemStore()
	newControllerStateOpenCityStore = func(string, gate.Mode) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{Store: cityBacking}, nil
	}
	t.Cleanup(func() { newControllerStateOpenCityStore = previousOpenCityStore })

	previousStartLifecycle := cityRuntimeStartBeadsLifecycle
	cityRuntimeStartBeadsLifecycle = func(string, string, *config.City, io.Writer) error { return nil }
	t.Cleanup(func() { cityRuntimeStartBeadsLifecycle = previousStartLifecycle })

	previousAutocloseDispatch := beadCloseAutocloseDispatch
	beadCloseAutocloseDispatch = func(run func()) { run() }
	t.Cleanup(func() { beadCloseAutocloseDispatch = previousAutocloseDispatch })

	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	const initialCityTOML = `[workspace]
name = "events-reload-city"
prefix = "erc"

[beads]
provider = "file"
`
	if err := os.WriteFile(tomlPath, []byte(initialCityTOML), 0o644); err != nil {
		t.Fatalf("write initial city.toml: %v", err)
	}

	initial, err := tryReloadConfig(tomlPath, "events-reload-city", cityPath)
	if err != nil {
		t.Fatalf("load initial config: %v", err)
	}
	applyFeatureFlags(initial.Cfg)

	var stdout, stderr bytes.Buffer
	fileProvider, err := newCityEventsProvider(cityPath, initial.Cfg.Events, &stderr)
	if err != nil {
		t.Fatalf("open initial file events provider: %v", err)
	}
	sharedProvider := newReloadableEventsProvider(fileProvider)

	cs := newControllerState(
		context.Background(),
		initial.Cfg,
		runtime.NewFake(),
		sharedProvider,
		"events-reload-city",
		cityPath,
	)
	if cs.EventProvider() != sharedProvider {
		t.Fatalf("controller event provider = %T, want shared reloadable provider", cs.EventProvider())
	}

	cr := &CityRuntime{
		cityPath:   cityPath,
		cityName:   "events-reload-city",
		configName: "events-reload-city",
		tomlPath:   tomlPath,
		configRev:  initial.Revision,
		cfg:        initial.Cfg,
		sp:         runtime.NewFake(),
		dops:       newDrainOps(runtime.NewFake()),
		rec:        sharedProvider,
		cs:         cs,
		pokeCh:     make(chan struct{}, 1),
		stdout:     &stdout,
		stderr:     &stderr,
		logPrefix:  "gc test",
	}
	t.Cleanup(func() {
		cr.stopConfigWatcher()
		cs.stopBeadEventWorkers()
		if err := sharedProvider.Close(); err != nil {
			t.Errorf("close shared events provider: %v", err)
		}
	})

	store := cs.CityBeadStore()
	if store == nil {
		t.Fatal("controller city store is nil")
	}
	root, err := store.Create(beads.Bead{Title: "reload molecule", Type: "molecule"})
	if err != nil {
		t.Fatalf("create molecule root: %v", err)
	}
	child, err := store.Create(beads.Bead{
		Title: "reload step",
		Type:  "step",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: root.ID,
		},
	})
	if err != nil {
		t.Fatalf("create molecule child: %v", err)
	}

	oldHead, err := sharedProvider.LatestSeq()
	if err != nil {
		t.Fatalf("initial events head: %v", err)
	}
	if oldHead == 0 {
		t.Fatal("initial file events head is zero after creating lifecycle beads")
	}

	execDir := t.TempDir()
	execRecordsPath := filepath.Join(execDir, "records.jsonl")
	execHeadPath := filepath.Join(execDir, "head")
	execScriptPath := filepath.Join(execDir, "events-provider")
	writeReloadEventsExecScript(t, execScriptPath, execRecordsPath, execHeadPath)
	oldEvents, err := sharedProvider.List(events.Filter{})
	if err != nil {
		t.Fatalf("list initial file event history: %v", err)
	}
	seedReloadExecHistory(t, execRecordsPath, execHeadPath, oldEvents)

	nextCityTOML := fmt.Sprintf(`%s
[events]
provider = %q
`, initialCityTOML, "exec:"+execScriptPath)
	if err := os.WriteFile(tomlPath, []byte(nextCityTOML), 0o644); err != nil {
		t.Fatalf("write reloaded city.toml: %v", err)
	}

	lastSessionProvider := initial.Cfg.Session.Provider
	reply := cr.reloadConfigTraced(
		context.Background(),
		&lastSessionProvider,
		cityPath,
		nil,
		reloadSourceManual,
	)
	if reply.Outcome != reloadOutcomeApplied {
		t.Fatalf("reload outcome = %q, want %q; error=%q warnings=%v stderr=%q",
			reply.Outcome, reloadOutcomeApplied, reply.Error, reply.Warnings, stderr.String())
	}

	const runtimeProbeType = "runtime.reload.probe"
	cr.rec.Record(events.Event{
		Type:    runtimeProbeType,
		Actor:   "gc-test",
		Subject: "events-provider-cutover",
	})

	store = cs.CityBeadStore()
	if err := store.Close(child.ID); err != nil {
		t.Fatalf("close molecule child after reload: %v", err)
	}
	cs.runBeadCloseAutoclose(child.ID, store, "")

	execEvents, err := readReloadExecEvents(execRecordsPath)
	if err != nil {
		t.Fatalf("read exec provider events: %v", err)
	}
	var probeEvents, childEvents, rootEvents []events.Event
	for _, event := range execEvents {
		switch {
		case event.Type == runtimeProbeType:
			probeEvents = append(probeEvents, event)
		case event.Subject == child.ID && event.Type == events.BeadClosed:
			childEvents = append(childEvents, event)
		case event.Subject == root.ID &&
			(event.Type == events.BeadClosed || event.Type == events.MoleculeResolved):
			rootEvents = append(rootEvents, event)
		}
	}
	if len(probeEvents) != 1 || probeEvents[0].Subject != "events-provider-cutover" {
		t.Fatalf("exec runtime probe events = %+v, want exactly one post-reload probe", probeEvents)
	}
	if len(childEvents) != 1 || childEvents[0].Type != events.BeadClosed {
		t.Fatalf("exec child events = %+v, want exactly one bead.closed", childEvents)
	}
	if len(rootEvents) != 2 ||
		rootEvents[0].Type != events.BeadClosed ||
		rootEvents[1].Type != events.MoleculeResolved {
		t.Fatalf("exec root lifecycle events = %+v, want exactly bead.closed then molecule.resolved", rootEvents)
	}

	fileHead, err := events.ReadLatestSeq(filepath.Join(cityPath, ".gc", "events.jsonl"))
	if err != nil {
		t.Fatalf("read old file events head: %v", err)
	}
	if fileHead != oldHead {
		t.Fatalf("old file events head advanced from %d to %d after provider reload", oldHead, fileHead)
	}

	// Read the authoritative backing. The resumed watcher replays every
	// post-cutover cache event asynchronously, so the cache can briefly hold an
	// earlier pending-intent snapshot even after durable cleanup has committed.
	closedRoot, err := cityBacking.Get(root.ID)
	if err != nil {
		t.Fatalf("get molecule root after autoclose: %v", err)
	}
	if closedRoot.Status != "closed" {
		t.Fatalf("molecule root status = %q, want closed", closedRoot.Status)
	}
	if pending := closedRoot.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey]; pending != "" {
		t.Fatalf("molecule lifecycle pending marker = %q, want cleared", pending)
	}
	if intent := closedRoot.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey]; intent != "" {
		t.Fatalf("molecule lifecycle intent = %q, want cleared", intent)
	}
}

func TestCityRuntimeReloadRejectsEventsSequenceRegressionAndRetriesSameRevision(t *testing.T) {
	clearInheritedBeadsEnv(t)

	previousStartLifecycle := cityRuntimeStartBeadsLifecycle
	cityRuntimeStartBeadsLifecycle = func(string, string, *config.City, io.Writer) error { return nil }
	t.Cleanup(func() { cityRuntimeStartBeadsLifecycle = previousStartLifecycle })

	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	const initialCityTOML = `[workspace]
name = "events-retry-city"
prefix = "retry"

[beads]
provider = "file"
`
	if err := os.WriteFile(tomlPath, []byte(initialCityTOML), 0o644); err != nil {
		t.Fatalf("write initial city.toml: %v", err)
	}
	initial, err := tryReloadConfig(tomlPath, "events-retry-city", cityPath)
	if err != nil {
		t.Fatalf("load initial config: %v", err)
	}
	applyFeatureFlags(initial.Cfg)

	var stdout, stderr bytes.Buffer
	fileProvider, err := newCityEventsProvider(cityPath, initial.Cfg.Events, &stderr)
	if err != nil {
		t.Fatalf("open initial file events provider: %v", err)
	}
	sharedProvider := newReloadableEventsProvider(fileProvider)
	sharedProvider.Record(events.Event{Type: "before.regression", Actor: "gc-test"})

	cr := &CityRuntime{
		cityPath:   cityPath,
		cityName:   "events-retry-city",
		configName: "events-retry-city",
		tomlPath:   tomlPath,
		configRev:  initial.Revision,
		cfg:        initial.Cfg,
		sp:         runtime.NewFake(),
		dops:       newDrainOps(runtime.NewFake()),
		rec:        sharedProvider,
		pokeCh:     make(chan struct{}, 1),
		stdout:     &stdout,
		stderr:     &stderr,
		logPrefix:  "gc test",
	}
	t.Cleanup(func() {
		cr.stopConfigWatcher()
		if cr.standaloneCityStore != nil {
			_ = closeBeadStoreHandle(cr.standaloneCityStore)
		}
		if err := sharedProvider.Close(); err != nil {
			t.Errorf("close shared events provider: %v", err)
		}
	})

	execDir := t.TempDir()
	execRecordsPath := filepath.Join(execDir, "records.jsonl")
	execHeadPath := filepath.Join(execDir, "head")
	execScriptPath := filepath.Join(execDir, "events-provider")
	writeReloadEventsExecScript(t, execScriptPath, execRecordsPath, execHeadPath)
	if err := os.WriteFile(execHeadPath, []byte("0\n"), 0o644); err != nil {
		t.Fatalf("seed regressed exec head: %v", err)
	}

	nextCityTOML := fmt.Sprintf(`%s
[events]
provider = %q
`, initialCityTOML, "exec:"+execScriptPath)
	if err := os.WriteFile(tomlPath, []byte(nextCityTOML), 0o644); err != nil {
		t.Fatalf("write desired city.toml: %v", err)
	}

	lastSessionProvider := initial.Cfg.Session.Provider
	first := cr.reloadConfigTraced(context.Background(), &lastSessionProvider, cityPath, nil, reloadSourceManual)
	if first.Outcome != reloadOutcomeApplied {
		t.Fatalf("first reload outcome = %q, want applied; error=%q warnings=%v", first.Outcome, first.Error, first.Warnings)
	}
	if !warningsContain(first.Warnings, errReloadableEventsSequenceRegression.Error()) {
		t.Fatalf("first reload warnings = %v, want sequence-regression rejection", first.Warnings)
	}
	if got := sharedProvider.ActiveEventProviderName(); got != "file" {
		t.Fatalf("active provider after rejected swap = %q, want file", got)
	}
	sharedProvider.Record(events.Event{Type: "after.rejected.swap", Actor: "gc-test"})
	oldHead, err := events.ReadLatestSeq(filepath.Join(cityPath, ".gc", "events.jsonl"))
	if err != nil {
		t.Fatalf("read file head after rejected swap: %v", err)
	}
	if oldHead < 2 {
		t.Fatalf("file head after rejected swap = %d, want post-rejection record", oldHead)
	}

	// Repair only the replacement backend. city.toml—and therefore its
	// revision—does not change; the installed-config tracker must still retry.
	oldEvents, err := sharedProvider.List(events.Filter{})
	if err != nil {
		t.Fatalf("list current history for replacement repair: %v", err)
	}
	seedReloadExecHistory(t, execRecordsPath, execHeadPath, oldEvents)
	second := cr.reloadConfigTraced(context.Background(), &lastSessionProvider, cityPath, nil, reloadSourceManual)
	if second.Outcome != reloadOutcomeApplied {
		t.Fatalf("same-revision retry outcome = %q, want applied; error=%q warnings=%v", second.Outcome, second.Error, second.Warnings)
	}
	wantProvider := "exec:" + execScriptPath
	if got := sharedProvider.ActiveEventProviderName(); got != wantProvider {
		t.Fatalf("active provider after retry = %q, want %q", got, wantProvider)
	}
	if cr.installedEventsConfig.provider != wantProvider {
		t.Fatalf("installed events config = %+v, want provider %q", cr.installedEventsConfig, wantProvider)
	}

	sharedProvider.Record(events.Event{Type: "after.same.revision.retry", Actor: "gc-test"})
	execEvents, err := readReloadExecEvents(execRecordsPath)
	if err != nil {
		t.Fatalf("read exec events after retry: %v", err)
	}
	var retryEvents []events.Event
	for _, event := range execEvents {
		if event.Type == "after.same.revision.retry" {
			retryEvents = append(retryEvents, event)
		}
	}
	if len(retryEvents) != 1 {
		t.Fatalf("exec retry events = %+v, want exactly one post-retry event", retryEvents)
	}
}

func TestCityRuntimeReloadRejectedSwapResumesFromPrePauseCursor(t *testing.T) {
	clearInheritedBeadsEnv(t)

	previousOpenCityStore := newControllerStateOpenCityStore
	cityBacking := beads.NewMemStore()
	newControllerStateOpenCityStore = func(string, gate.Mode) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{Store: cityBacking}, nil
	}
	t.Cleanup(func() { newControllerStateOpenCityStore = previousOpenCityStore })

	previousStartLifecycle := cityRuntimeStartBeadsLifecycle
	cityRuntimeStartBeadsLifecycle = func(string, string, *config.City, io.Writer) error { return nil }
	t.Cleanup(func() { cityRuntimeStartBeadsLifecycle = previousStartLifecycle })

	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	const initialCityTOML = `[workspace]
name = "events-failed-swap-city"
prefix = "fsc"

[beads]
provider = "file"
`
	if err := os.WriteFile(tomlPath, []byte(initialCityTOML), 0o644); err != nil {
		t.Fatalf("write initial city.toml: %v", err)
	}
	initial, err := tryReloadConfig(tomlPath, "events-failed-swap-city", cityPath)
	if err != nil {
		t.Fatalf("load initial config: %v", err)
	}
	applyFeatureFlags(initial.Cfg)

	var stdout, stderr bytes.Buffer
	fileProvider, err := newCityEventsProvider(cityPath, initial.Cfg.Events, &stderr)
	if err != nil {
		t.Fatalf("open initial file events provider: %v", err)
	}
	sharedProvider := newReloadableEventsProvider(fileProvider)
	sharedProvider.Record(events.Event{Type: "before.swap.one", Actor: "gc-test"})
	sharedProvider.Record(events.Event{Type: "before.swap.two", Actor: "gc-test"})
	oldHead, err := sharedProvider.LatestSeq()
	if err != nil || oldHead == 0 {
		t.Fatalf("initial file head = %d, err = %v; want a non-zero baseline", oldHead, err)
	}

	cs := newControllerState(
		context.Background(),
		initial.Cfg,
		runtime.NewFake(),
		sharedProvider,
		"events-failed-swap-city",
		cityPath,
	)
	// Model a controller whose construction-time head probe failed, so no
	// watcher ever trusted a cursor. This is the exact precondition under which
	// the reload glue previously overwrote the replay cursor with the candidate
	// head before attempting the swap.
	cs.beadEventStartSeq = 0
	cs.beadEventStartSeqOK = false
	if seq, ok := cs.beadEventReplayCursor(); seq != 0 || ok {
		t.Fatalf("pre-reload forced cursor = (%d, %t), want (0, false)", seq, ok)
	}

	cr := &CityRuntime{
		cityPath:   cityPath,
		cityName:   "events-failed-swap-city",
		configName: "events-failed-swap-city",
		tomlPath:   tomlPath,
		configRev:  initial.Revision,
		cfg:        initial.Cfg,
		sp:         runtime.NewFake(),
		dops:       newDrainOps(runtime.NewFake()),
		rec:        sharedProvider,
		cs:         cs,
		pokeCh:     make(chan struct{}, 1),
		stdout:     &stdout,
		stderr:     &stderr,
		logPrefix:  "gc test",
	}
	t.Cleanup(func() {
		cr.stopConfigWatcher()
		cs.stopBeadEventWorkers()
		if err := sharedProvider.Close(); err != nil {
			t.Errorf("close shared events provider: %v", err)
		}
	})

	// Seed a replacement whose head is ABOVE the old head but whose retained
	// history diverges at a shared sequence, so validation rejects the swap.
	execDir := t.TempDir()
	execRecordsPath := filepath.Join(execDir, "records.jsonl")
	execHeadPath := filepath.Join(execDir, "head")
	execScriptPath := filepath.Join(execDir, "events-provider")
	writeReloadEventsExecScript(t, execScriptPath, execRecordsPath, execHeadPath)
	divergentHistory := make([]events.Event, 0, oldHead+3)
	for seq := uint64(1); seq <= oldHead+3; seq++ {
		divergentHistory = append(divergentHistory, events.Event{Seq: seq, Type: "exec.only.divergent", Actor: "exec"})
	}
	seedReloadExecHistory(t, execRecordsPath, execHeadPath, divergentHistory)

	nextCityTOML := fmt.Sprintf(`%s
[events]
provider = %q
`, initialCityTOML, "exec:"+execScriptPath)
	if err := os.WriteFile(tomlPath, []byte(nextCityTOML), 0o644); err != nil {
		t.Fatalf("write desired city.toml: %v", err)
	}

	lastSessionProvider := initial.Cfg.Session.Provider
	reply := cr.reloadConfigTraced(context.Background(), &lastSessionProvider, cityPath, nil, reloadSourceManual)
	if reply.Outcome != reloadOutcomeApplied {
		t.Fatalf("reload outcome = %q, want applied; error=%q warnings=%v", reply.Outcome, reply.Error, reply.Warnings)
	}
	if !warningsContain(reply.Warnings, errReloadableEventsHistoryMismatch.Error()) {
		t.Fatalf("reload warnings = %v, want history-mismatch rejection", reply.Warnings)
	}
	if got := sharedProvider.ActiveEventProviderName(); got != "file" {
		t.Fatalf("active provider after rejected swap = %q, want file", got)
	}

	// The workers must resume against the retained file provider from its own
	// head — NOT the rejected candidate's head. Because the pre-pause cursor was
	// the untrusted (0, false), resume re-resolves the old provider's live head
	// (oldHead) and fails closed there; adopting the candidate head (oldHead+3)
	// would make the watcher skip every event the old provider records next.
	seq, ok := cs.beadEventReplayCursor()
	if !ok || seq != oldHead {
		t.Fatalf("resumed replay cursor = (%d, %t) after rejected swap, want the old provider head (%d, true); the rejected candidate head was %d", seq, ok, oldHead, oldHead+3)
	}
}

func TestCityRuntimeReloadRetriesEventsProbeFailureOnSameRevision(t *testing.T) {
	clearInheritedBeadsEnv(t)

	previousStartLifecycle := cityRuntimeStartBeadsLifecycle
	cityRuntimeStartBeadsLifecycle = func(string, string, *config.City, io.Writer) error { return nil }
	t.Cleanup(func() { cityRuntimeStartBeadsLifecycle = previousStartLifecycle })

	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	const initialCityTOML = `[workspace]
name = "events-probe-retry-city"

[beads]
provider = "file"
`
	if err := os.WriteFile(tomlPath, []byte(initialCityTOML), 0o644); err != nil {
		t.Fatalf("write initial city.toml: %v", err)
	}
	initial, err := tryReloadConfig(tomlPath, "events-probe-retry-city", cityPath)
	if err != nil {
		t.Fatalf("load initial config: %v", err)
	}
	applyFeatureFlags(initial.Cfg)

	current := events.NewFake()
	current.Record(events.Event{Type: "initial", Actor: "gc-test"})
	sharedProvider := newReloadableEventsProvider(current)
	var stdout, stderr bytes.Buffer
	cr := &CityRuntime{
		cityPath:   cityPath,
		cityName:   "events-probe-retry-city",
		configName: "events-probe-retry-city",
		tomlPath:   tomlPath,
		configRev:  initial.Revision,
		cfg:        initial.Cfg,
		sp:         runtime.NewFake(),
		dops:       newDrainOps(runtime.NewFake()),
		rec:        sharedProvider,
		pokeCh:     make(chan struct{}, 1),
		stdout:     &stdout,
		stderr:     &stderr,
		logPrefix:  "gc test",
	}
	t.Cleanup(func() {
		cr.stopConfigWatcher()
		if cr.standaloneCityStore != nil {
			_ = closeBeadStoreHandle(cr.standaloneCityStore)
		}
		_ = sharedProvider.Close()
	})

	probeFailure := &failingLatestEventsProvider{
		Provider: events.NewFake(),
		err:      errors.New("injected latest-seq failure"),
	}
	healthyReplacement := events.NewFake()
	previousNewEventsProvider := cityRuntimeNewEventsProvider
	openCalls := 0
	cityRuntimeNewEventsProvider = func(string, config.EventsConfig, io.Writer) (events.Provider, error) {
		openCalls++
		if openCalls == 1 {
			return probeFailure, nil
		}
		return healthyReplacement, nil
	}
	t.Cleanup(func() { cityRuntimeNewEventsProvider = previousNewEventsProvider })

	desiredCityTOML := initialCityTOML + "\n[events]\nprovider = \"fake\"\n"
	if err := os.WriteFile(tomlPath, []byte(desiredCityTOML), 0o644); err != nil {
		t.Fatalf("write desired city.toml: %v", err)
	}
	lastSessionProvider := initial.Cfg.Session.Provider
	first := cr.reloadConfigTraced(context.Background(), &lastSessionProvider, cityPath, nil, reloadSourceManual)
	if first.Outcome != reloadOutcomeApplied {
		t.Fatalf("first reload outcome = %q, want applied; error=%q warnings=%v", first.Outcome, first.Error, first.Warnings)
	}
	if !warningsContain(first.Warnings, probeFailure.err.Error()) {
		t.Fatalf("first reload warnings = %v, want probe failure", first.Warnings)
	}
	if got := probeFailure.closeCalls.Load(); got != 1 {
		t.Fatalf("failed candidate close calls = %d, want 1", got)
	}
	sharedProvider.Record(events.Event{Type: "still-current", Actor: "gc-test"})
	if len(current.Events) != 2 || current.Events[1].Type != "still-current" {
		t.Fatalf("current provider events after probe failure = %+v", current.Events)
	}
	currentHistory, err := current.List(events.Filter{})
	if err != nil {
		t.Fatalf("list current history for probe retry: %v", err)
	}
	for _, event := range currentHistory {
		healthyReplacement.Record(event)
	}
	healthyReplacement.Record(events.Event{Type: "replacement.ahead", Actor: "migration", Ts: time.Unix(2, 0).UTC()})

	second := cr.reloadConfigTraced(context.Background(), &lastSessionProvider, cityPath, nil, reloadSourceManual)
	if second.Outcome != reloadOutcomeApplied {
		t.Fatalf("same-revision retry outcome = %q, want applied; error=%q warnings=%v", second.Outcome, second.Error, second.Warnings)
	}
	sharedProvider.Record(events.Event{Type: "after-probe-retry", Actor: "gc-test"})
	if len(healthyReplacement.Events) != 4 || healthyReplacement.Events[3].Type != "after-probe-retry" {
		t.Fatalf("replacement events after retry = %+v", healthyReplacement.Events)
	}
	if openCalls != 2 {
		t.Fatalf("events provider construction calls = %d, want same-revision retry", openCalls)
	}
}

type failingLatestEventsProvider struct {
	events.Provider
	err        error
	closeCalls atomic.Int32
}

func (p *failingLatestEventsProvider) LatestSeq() (uint64, error) {
	return 0, p.err
}

func (p *failingLatestEventsProvider) Close() error {
	p.closeCalls.Add(1)
	return p.Provider.Close()
}

func writeReloadEventsExecScript(t *testing.T, scriptPath, recordsPath, headPath string) {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
records=%q
head=%q

case "$1" in
  ensure-running)
    exit 0
    ;;
  latest-seq)
    if [ -s "$head" ]; then cat "$head"; else echo 0; fi
    ;;
  record)
    payload=$(cat)
    if [ -s "$head" ]; then current=$(cat "$head"); else current=0; fi
    next=$((current + 1))
	    payload=$(printf '%%s' "$payload" | sed "s/\"seq\":0/\"seq\":$next/")
    printf '%%s\n' "$next" > "$head"
    printf '%%s\n' "$payload" >> "$records"
    ;;
  list)
    cat >/dev/null
    if [ ! -s "$records" ]; then
      echo '[]'
    else
      awk 'BEGIN { printf "[" } { if (NR > 1) printf ","; printf "%%s", $0 } END { print "]" }' "$records"
    fi
    ;;
  watch)
	    cursor=$2
	    while :; do
	      if [ -s "$records" ]; then
	        while IFS= read -r event; do
	          seq=$(printf '%%s' "$event" | sed -n 's/^{"seq":\([0-9][0-9]*\).*/\1/p')
	          if [ -n "$seq" ] && [ "$seq" -gt "$cursor" ]; then
	            printf '%%s\n' "$event"
	            cursor=$seq
	          fi
	        done < "$records"
	      fi
	      sleep 0.05
	    done
    ;;
  *)
    exit 2
    ;;
esac
`, recordsPath, headPath)
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write exec events provider: %v", err)
	}
}

func seedReloadExecHistory(t *testing.T, recordsPath, headPath string, history []events.Event) {
	t.Helper()
	var records bytes.Buffer
	var head uint64
	for _, event := range history {
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("encode replacement event history: %v", err)
		}
		records.Write(encoded)
		records.WriteByte('\n')
		if event.Seq > head {
			head = event.Seq
		}
	}
	if err := os.WriteFile(recordsPath, records.Bytes(), 0o644); err != nil {
		t.Fatalf("seed replacement event records: %v", err)
	}
	if err := os.WriteFile(headPath, []byte(strconv.FormatUint(head, 10)+"\n"), 0o644); err != nil {
		t.Fatalf("seed replacement event head: %v", err)
	}
}

func readReloadExecEvents(path string) ([]events.Event, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, nil
	}
	lines := bytes.Split(data, []byte{'\n'})
	result := make([]events.Event, 0, len(lines))
	for i, line := range lines {
		var event events.Event
		if err := json.Unmarshal(line, &event); err != nil {
			return nil, fmt.Errorf("decode record %d: %w", i+1, err)
		}
		result = append(result, event)
	}
	return result, nil
}
