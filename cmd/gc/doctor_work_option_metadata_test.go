package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
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
