package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

func TestWorkOptionMetadataMigrationCheckFixesLegacyTaskMetadata(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir}}}

	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "LEG-1", Title: "legacy work", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.model": "gpt-5", "gc.reasoning": "high",
		}},
		{ID: "OPT-1", Title: "canonical wins", Type: "task", Status: "in_progress", Metadata: map[string]string{
			"gc.model": "legacy-model", "opt_model": "canonical-model", "gc.reasoning": "legacy-effort", "opt_effort": "canonical-effort",
		}},
		{ID: "CLOSED-1", Title: "closed work", Type: "task", Status: "closed", Metadata: map[string]string{
			"gc.model": "closed-model", "gc.reasoning": "closed-effort",
		}},
		{ID: "MSG-1", Title: "message", Type: "message", Status: "open", Metadata: map[string]string{
			"gc.model": "message-model", "gc.reasoning": "message-effort",
		}},
		{ID: "EMPTY-1", Title: "empty legacy", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.model": "   ", "gc.reasoning": "",
		}},
	}, nil)
	rigStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "RIG-1", Title: "rig work", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.reasoning": "medium",
		}},
	}, nil)
	stores := map[string]beads.Store{cityDir: cityStore, rigDir: rigStore}
	check := newWorkOptionMetadataMigrationCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		store, ok := stores[path]
		if !ok {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	})

	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusWarning {
		t.Fatalf("Run status = %v, want warning: %#v", res.Status, res)
	}
	details := strings.Join(res.Details, "\n")
	for _, want := range []string{"LEG-1", "OPT-1", "RIG-1", "gc.model -> opt_model", "gc.reasoning -> opt_effort"} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing %q:\n%s", want, details)
		}
	}
	for _, notWant := range []string{"CLOSED-1", "MSG-1", "EMPTY-1"} {
		if strings.Contains(details, notWant) {
			t.Fatalf("details should not mention %q:\n%s", notWant, details)
		}
	}

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if res2 := check.Run(&doctor.CheckContext{}); res2.Status != doctor.StatusOK {
		t.Fatalf("post-fix Run status = %v, want OK: %#v", res2.Status, res2)
	}

	leg, err := cityStore.Get("LEG-1")
	if err != nil {
		t.Fatalf("get LEG-1: %v", err)
	}
	if got := leg.Metadata["opt_model"]; got != "gpt-5" {
		t.Errorf("LEG-1 opt_model = %q, want gpt-5", got)
	}
	if got := leg.Metadata["opt_effort"]; got != "high" {
		t.Errorf("LEG-1 opt_effort = %q, want high", got)
	}
	if got := leg.Metadata["gc.model"]; got != "" {
		t.Errorf("LEG-1 gc.model = %q, want tombstone", got)
	}
	if got := leg.Metadata["gc.reasoning"]; got != "" {
		t.Errorf("LEG-1 gc.reasoning = %q, want tombstone", got)
	}

	opt, err := cityStore.Get("OPT-1")
	if err != nil {
		t.Fatalf("get OPT-1: %v", err)
	}
	if got := opt.Metadata["opt_model"]; got != "canonical-model" {
		t.Errorf("OPT-1 opt_model = %q, want canonical-model", got)
	}
	if got := opt.Metadata["opt_effort"]; got != "canonical-effort" {
		t.Errorf("OPT-1 opt_effort = %q, want canonical-effort", got)
	}
	if got := opt.Metadata["gc.model"]; got != "" {
		t.Errorf("OPT-1 gc.model = %q, want tombstone", got)
	}
	if got := opt.Metadata["gc.reasoning"]; got != "" {
		t.Errorf("OPT-1 gc.reasoning = %q, want tombstone", got)
	}

	rig, err := rigStore.Get("RIG-1")
	if err != nil {
		t.Fatalf("get RIG-1: %v", err)
	}
	if got := rig.Metadata["opt_effort"]; got != "medium" {
		t.Errorf("RIG-1 opt_effort = %q, want medium", got)
	}

	closed, err := cityStore.Get("CLOSED-1")
	if err != nil {
		t.Fatalf("get CLOSED-1: %v", err)
	}
	if got := closed.Metadata["opt_model"]; got != "" {
		t.Errorf("CLOSED-1 opt_model = %q, want untouched", got)
	}
	msg, err := cityStore.Get("MSG-1")
	if err != nil {
		t.Fatalf("get MSG-1: %v", err)
	}
	if got := msg.Metadata["opt_model"]; got != "" {
		t.Errorf("MSG-1 opt_model = %q, want untouched", got)
	}
}

func TestWorkOptionMetadataMigrationCheckCleanStore(t *testing.T) {
	cityDir := t.TempDir()
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "T-1", Title: "work", Type: "task", Status: "open", Metadata: map[string]string{
			"opt_model": "gpt-5", "opt_effort": "high",
		}},
	}, nil)
	check := newWorkOptionMetadataMigrationCheck(nil, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	})
	if !check.CanFix() {
		t.Fatal("CanFix() = false, want true")
	}
	if res := check.Run(&doctor.CheckContext{}); res.Status != doctor.StatusOK {
		t.Fatalf("Run status = %v, want OK: %#v", res.Status, res)
	}
}

func TestWorkOptionMetadataMigrationClearsStaleSessionAutoStampedModel(t *testing.T) {
	cityDir := t.TempDir()
	store := beads.NewMemStore()
	candidate := newOptionSessionCandidate(
		t,
		store,
		map[string]string{"model": "opus"},
		map[string]string{"model": "sonnet", "effort": "high"},
	)
	if err := store.SetMetadata(candidate.info.ID, "gc.per_dispatch_model", "sonnet"); err != nil {
		t.Fatalf("SetMetadata(gc.per_dispatch_model): %v", err)
	}
	check := newWorkOptionMetadataMigrationCheck(nil, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	})

	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusWarning {
		t.Fatalf("Run status = %v, want warning: %#v", res.Status, res)
	}
	details := strings.Join(res.Details, "\n")
	for _, want := range []string{candidate.info.ID, "gc.per_dispatch_model", "template_overrides.model"} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing %q:\n%s", want, details)
		}
	}

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if res2 := check.Run(&doctor.CheckContext{}); res2.Status != doctor.StatusOK {
		t.Fatalf("post-fix Run status = %v, want OK: %#v", res2.Status, res2)
	}
	session, err := store.Get(candidate.info.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if got := strings.TrimSpace(session.Metadata["gc.per_dispatch_model"]); got != "" {
		t.Fatalf("gc.per_dispatch_model = %q, want tombstone", got)
	}
	wantOverrides := map[string]string{"effort": "high"}
	if got := storedSessionOverrides(t, store, candidate.info.ID); !reflect.DeepEqual(got, wantOverrides) {
		t.Fatalf("template_overrides = %v, want %v", got, wantOverrides)
	}

	// Refresh the typed twin after swapping in the post-fix bead so buildPreparedStart
	// decodes the cleaned-up template_overrides off candidate.info (production keeps this
	// coherent via prepareStartCandidateForCity's front-door refresh).
	candidate.info, err = sessionFrontDoor(store).Get(candidate.info.ID)
	if err != nil {
		t.Fatalf("front-door Get(session): %v", err)
	}
	prepared, _, err := buildPreparedStart(candidate, &config.City{}, store)
	if err != nil {
		t.Fatalf("buildPreparedStart: %v", err)
	}
	if !strings.Contains(prepared.cfg.Command, "--model claude-opus-4-8") {
		t.Fatalf("prepared command = %q, want work opt_model opus after stale session cleanup", prepared.cfg.Command)
	}
	if strings.Contains(prepared.cfg.Command, "claude-sonnet-4-6") {
		t.Fatalf("prepared command = %q, stale session model should not win", prepared.cfg.Command)
	}
}

func TestWorkOptionMetadataMigrationPreservesExplicitSessionModelWhenMarkerDiffers(t *testing.T) {
	cityDir := t.TempDir()
	raw, err := json.Marshal(map[string]string{"model": "sonnet"})
	if err != nil {
		t.Fatalf("Marshal(template_overrides): %v", err)
	}
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "SESSION-1", Title: "worker", Type: sessionBeadType, Status: "open", Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
			"template_overrides":    string(raw),
			"gc.per_dispatch_model": "opus",
		}},
	}, nil)
	check := newWorkOptionMetadataMigrationCheck(nil, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	})

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	got, err := store.Get("SESSION-1")
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if got.Metadata["template_overrides"] != string(raw) {
		t.Fatalf("template_overrides = %q, want preserved %q", got.Metadata["template_overrides"], string(raw))
	}
	if got.Metadata["gc.per_dispatch_model"] != "" {
		t.Fatalf("gc.per_dispatch_model = %q, want tombstone", got.Metadata["gc.per_dispatch_model"])
	}
}

func TestWorkOptionMetadataMigrationFixReportsSkippedScopes(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir}}}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "T-1", Title: "work", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.model": "gpt-5",
		}},
	}, nil)
	check := newWorkOptionMetadataMigrationCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path == rigDir {
			return nil, errors.New("permission denied")
		}
		return cityStore, nil
	})

	err := check.Fix(&doctor.CheckContext{})
	if err == nil {
		t.Fatal("Fix error = nil, want skipped scope error")
	}
	if got := err.Error(); !strings.Contains(got, "rig repo skipped") || !strings.Contains(got, "permission denied") {
		t.Fatalf("Fix error = %q, want rig open failure detail", got)
	}
	got, getErr := cityStore.Get("T-1")
	if getErr != nil {
		t.Fatalf("get T-1: %v", getErr)
	}
	if got.Metadata["opt_model"] != "gpt-5" || got.Metadata["gc.model"] != "" {
		t.Fatalf("city fix was not applied before reporting skipped rig: metadata=%+v", got.Metadata)
	}
}

func TestWorkOptionMetadataMigrationSkipsEffectivelySuspendedRigs(t *testing.T) {
	cityDir := t.TempDir()
	startSuspendedDir := t.TempDir()
	runtimeSuspendedDir := t.TempDir()
	activeDir := t.TempDir()
	suspend := true
	if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, cityDir, "runtime-suspended", &suspend); err != nil {
		t.Fatalf("SetRigSuspended: %v", err)
	}
	cfg := &config.City{Rigs: []config.Rig{
		{Name: "start-suspended", Path: startSuspendedDir, SuspendedOnStart: true},
		{Name: "runtime-suspended", Path: runtimeSuspendedDir},
		{Name: "active", Path: activeDir},
	}}
	cityStore := beads.NewMemStore()
	activeStore := beads.NewMemStore()
	stores := map[string]beads.Store{
		cityDir:   cityStore,
		activeDir: activeStore,
	}
	var opened []string
	check := newWorkOptionMetadataMigrationCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		opened = append(opened, path)
		store, ok := stores[path]
		if !ok {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	})

	if res := check.Run(&doctor.CheckContext{}); res.Status != doctor.StatusOK {
		t.Fatalf("Run status = %v, want OK: %#v", res.Status, res)
	}
	for _, notWant := range []string{startSuspendedDir, runtimeSuspendedDir} {
		if containsString(opened, notWant) {
			t.Fatalf("opened suspended rig store %q; opened=%v", notWant, opened)
		}
	}
	for _, want := range []string{cityDir, activeDir} {
		if !containsString(opened, want) {
			t.Fatalf("did not open active scope %q; opened=%v", want, opened)
		}
	}
}

func TestBuildDoctorChecksRegistersWorkOptionMetadataMigration(t *testing.T) {
	checks := buildDoctorChecks(t.TempDir(), &config.City{}, nil, buildDoctorChecksOpts{
		Stderr:               io.Discard,
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
	})
	for _, check := range checks {
		if check.Name() == "work-option-metadata-migration" {
			return
		}
	}
	t.Fatal("buildDoctorChecks did not register work-option-metadata-migration")
}
