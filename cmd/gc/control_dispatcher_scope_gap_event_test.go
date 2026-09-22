package main

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// emitControlDispatcherScopeGapEvents records one typed event per scope gap,
// keyed on the scope that OWNS the suppressed work rather than the leg it was
// collected through — on a relocated city the leg is the class binding, which
// serves every scope and owns none, so a subscriber keying on it could not tell
// two rigs' gaps apart.
func TestEmitControlDispatcherScopeGapEvents_EmitsTypedPayload(t *testing.T) {
	rec := &capturingRecorder{}
	gaps := []ControlDispatcherScopeGap{
		{
			ScopeLabel:      `class binding "class:gmnos" serving rig "beads"`,
			RigContext:      "beads",
			StoreRef:        "class:gmnos",
			SuppressedCount: 4,
			SampleBeadID:    "ga-ods2a",
		},
		{
			ScopeLabel:      "the city store",
			StoreRef:        "",
			SuppressedCount: 1,
			SampleBeadID:    "ga-oytw9",
		},
	}

	emitControlDispatcherScopeGapEvents(rec, "maintainer-city", gaps, time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC))

	if len(rec.events) != 2 {
		t.Fatalf("event count = %d, want one per scope gap", len(rec.events))
	}
	first := rec.events[0]
	if first.Type != events.ControlDispatcherScopeGap {
		t.Fatalf("type = %q, want %q", first.Type, events.ControlDispatcherScopeGap)
	}
	if first.Subject != "rig:beads" {
		t.Fatalf("subject = %q, want the owning rig scope ref rig:beads", first.Subject)
	}
	if !strings.Contains(first.Message, "4") || !strings.Contains(first.Message, "ga-ods2a") {
		t.Fatalf("message = %q, want the suppressed count and a sample bead", first.Message)
	}
	var p events.ControlDispatcherScopeGapPayload
	if err := json.Unmarshal(first.Payload, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	want := events.ControlDispatcherScopeGapPayload{
		ScopeLabel:      `class binding "class:gmnos" serving rig "beads"`,
		RigContext:      "beads",
		StoreRef:        "class:gmnos",
		SuppressedCount: 4,
		SampleBeadID:    "ga-ods2a",
	}
	if p != want {
		t.Fatalf("payload = %+v, want %+v", p, want)
	}

	if got := rec.events[1].Subject; got != "city:maintainer-city" {
		t.Fatalf("city-scope subject = %q, want city:maintainer-city", got)
	}
}

// A nil recorder or an empty gap list is a no-op — no panic, no events. The
// healthy city is the common case and must stay silent.
func TestEmitControlDispatcherScopeGapEvents_NoOpOnEmpty(t *testing.T) {
	emitControlDispatcherScopeGapEvents(nil, "test-city", []ControlDispatcherScopeGap{{ScopeLabel: "the city store"}}, time.Now())
	rec := &capturingRecorder{}
	emitControlDispatcherScopeGapEvents(rec, "test-city", nil, time.Now())
	if len(rec.events) != 0 {
		t.Fatalf("expected no events, got %d", len(rec.events))
	}
}

// The reconciler must actually record the gaps its desired-state build reports.
// Without this the summary is computed and dropped, which is indistinguishable
// from the pre-fix behavior: the stderr line still prints and the event bus
// still shows nothing.
func TestCityRuntimeBuildDesiredStateRecordsControlDispatcherScopeGapEvents(t *testing.T) {
	rec := &capturingRecorder{}
	cr := &CityRuntime{
		cityName:            "test-city",
		cityPath:            t.TempDir(),
		cfg:                 &config.City{Workspace: config.Workspace{Name: "test-city"}},
		sp:                  runtime.NewFake(),
		standaloneCityStore: beads.NewMemStore(),
		rec:                 rec,
		stderr:              io.Discard,
		buildFnWithSessionBeads: func(*config.City, runtime.Provider, beads.Store, map[string]beads.Store, *sessionBeadSnapshot, *sessionReconcilerTraceCycle) DesiredStateResult {
			return DesiredStateResult{
				State: map[string]TemplateParams{},
				ControlDispatcherScopeGaps: []ControlDispatcherScopeGap{{
					ScopeLabel:      `rig store "fixture"`,
					RigContext:      "fixture",
					StoreRef:        "rig:fixture",
					SuppressedCount: 2,
					SampleBeadID:    "control-1",
				}},
			}
		},
	}

	cr.buildDesiredState(nil, nil)

	if len(rec.events) != 1 {
		t.Fatalf("recorded events = %+v, want one control.dispatcher_scope_gap", rec.events)
	}
	e := rec.events[0]
	if e.Type != events.ControlDispatcherScopeGap {
		t.Fatalf("type = %q, want %q", e.Type, events.ControlDispatcherScopeGap)
	}
	if e.Subject != "rig:fixture" {
		t.Fatalf("subject = %q, want rig:fixture", e.Subject)
	}
	var p events.ControlDispatcherScopeGapPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if p.SuppressedCount != 2 || p.RigContext != "fixture" {
		t.Fatalf("payload = %+v, want suppressed_count=2 rig_context=fixture", p)
	}
}

// A build that reports no gap records nothing, and a nil recorder does not
// panic the desired-state phase.
func TestCityRuntimeBuildDesiredStateRecordsNothingWithoutAScopeGap(t *testing.T) {
	rec := &capturingRecorder{}
	newRuntime := func(r events.Recorder) *CityRuntime {
		return &CityRuntime{
			cityName:            "test-city",
			cityPath:            t.TempDir(),
			cfg:                 &config.City{Workspace: config.Workspace{Name: "test-city"}},
			sp:                  runtime.NewFake(),
			standaloneCityStore: beads.NewMemStore(),
			rec:                 r,
			stderr:              io.Discard,
			buildFnWithSessionBeads: func(*config.City, runtime.Provider, beads.Store, map[string]beads.Store, *sessionBeadSnapshot, *sessionReconcilerTraceCycle) DesiredStateResult {
				return DesiredStateResult{State: map[string]TemplateParams{}}
			},
		}
	}

	newRuntime(rec).buildDesiredState(nil, nil)
	if len(rec.events) != 0 {
		t.Fatalf("recorded events = %+v, want none for a city with every scope dispatched", rec.events)
	}
	newRuntime(nil).buildDesiredState(nil, nil)
}
