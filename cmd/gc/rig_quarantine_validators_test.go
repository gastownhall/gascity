package main

import (
	"bytes"
	"errors"
	"io"
	"maps"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/runtime"
)

// gc doctor's expanded-config-load check is a validator: a rig the CLI
// loaders quarantine is still a broken config, reported by name.
func TestExpandedConfigLoadCheckReportsQuarantinedRig(t *testing.T) {
	c := newRigQuarantineCity(t)

	got := expandedConfigLoadCheck{}.Run(&doctor.CheckContext{CityPath: c.cityDir})
	if got.Status != doctor.StatusError {
		t.Fatalf("status = %v, want error; message=%q", got.Status, got.Message)
	}
	for _, want := range []string{`rig "bad"`, "has local worktree changes"} {
		if !strings.Contains(got.Message, want) {
			t.Errorf("message = %q, want substring %q", got.Message, want)
		}
	}
}

// gc doctor builds its checks from the one-shot loader, which quarantines a
// rig whose packs fail. That composition is the load error a strict load
// reports, not a city without the rig's agents: doctor registers no session
// check against it, so --fix cannot stop the quarantined rig's live sessions
// as orphans while the strict controller cannot start to adopt them. The
// expanded-config-load check still names the rig.
func TestDoctorSessionChecksSpareAQuarantinedRigsSessions(t *testing.T) {
	clearGCEnv(t)
	c := newRigQuarantineCity(t)
	t.Setenv("GC_CITY", c.cityDir)
	t.Setenv("GC_BEADS", "file")

	cfg, err := loadCityConfig(c.cityDir, io.Discard)
	if err != nil {
		t.Fatalf("doctor's config load of a city with one broken rig: %v", err)
	}
	if !cfg.RigQuarantined("bad") {
		t.Fatalf("quarantined rigs = %v, want rig bad", cfg.QuarantinedRigs)
	}
	// Each rig's agent is named <rig>/<import binding>.<agent>.
	cityName := loadedCityName(cfg, c.cityDir)
	goodSession := agent.SessionNameFor(cityName, "good/tools.worker", cfg.Workspace.SessionTemplate)
	badSession := agent.SessionNameFor(cityName, "bad/broken.brokenworker", cfg.Workspace.SessionTemplate)
	sp := runtime.NewFake()
	for _, name := range []string{goodSession, badSession} {
		if err := sp.Start(t.Context(), name, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	oldBuild := buildSessionProviderByName
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return sp, nil
	}
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })

	ctx := &doctor.CheckContext{CityPath: c.cityDir}
	var configLoad *doctor.CheckResult
	for _, check := range buildDoctorChecks(c.cityDir, cfg, nil, buildDoctorChecksOpts{
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
		SkipStorePreflight:   true,
	}) {
		switch check.Name() {
		case "agent-sessions", "zombie-sessions", "orphan-sessions":
			r := check.Run(ctx)
			t.Errorf("doctor registered %s against a composition without rig bad's agents: %q %v", check.Name(), r.Message, r.Details)
			if !check.CanFix() {
				continue
			}
			if err := check.Fix(ctx); err != nil {
				t.Errorf("%s Fix: %v", check.Name(), err)
			}
		case "expanded-config-load":
			configLoad = check.Run(ctx)
		}
	}
	for _, name := range []string{goodSession, badSession} {
		if !sp.IsRunning(name) {
			t.Errorf("doctor stopped live session %s", name)
		}
	}
	if configLoad == nil || configLoad.Status != doctor.StatusError || !strings.Contains(configLoad.Message, `rig "bad"`) {
		t.Errorf("expanded-config-load = %+v, want an error naming rig bad", configLoad)
	}
}

// gc import install's post-install validation fails closed on the same
// broken rig doctor reports.
func TestImportInstallValidationReportsQuarantinedRig(t *testing.T) {
	c := newRigQuarantineCity(t)

	err := validateComposedConfigAfterInstall(c.cityDir)
	if err == nil {
		t.Fatal("post-install validation passed a city with a broken rig import")
	}
	if !strings.Contains(err.Error(), `rig "bad"`) {
		t.Errorf("err = %q, want it to name rig %q", err, "bad")
	}
}

// Code that reads an agent, formula or named session missing from the config
// as unconfigured, and acts on that, must not run on a composition that
// withheld a rig's content: each of these callers sees the quarantine as the
// load error a strict load reports for the rig.

// The session verbs (wake, close, suspend, kill) judge a session's template
// and claims against the configured agents: a wake of a quarantined rig's
// session would put it back to sleep, and a close would release its claims on
// a guess. They run under the strict load, which leaves them their
// no-config path.
func TestSessionVerbConfigReportsAQuarantinedRig(t *testing.T) {
	c := newRigQuarantineCity(t)

	cfg, err := cliCityConfig(c.cityDir, nil, io.Discard)
	if cfg != nil {
		t.Errorf("session verb config = a composition with rigs %v quarantined, want none", cfg.QuarantinedRigs)
	}
	if !quarantinedRigError(err, "bad") {
		t.Errorf("session verb config err = %v, want rig bad's pack error", err)
	}
}

// gc session attach resumes a session no configured agent runs from its own
// process, bypassing the controller; a quarantined rig's session is not such
// a session, so attach reports the quarantine instead.
func TestSessionAttachReportsAQuarantinedRig(t *testing.T) {
	clearGCEnv(t)
	c := newRigQuarantineCity(t)
	t.Setenv("GC_CITY", c.cityDir)

	var stdout, stderr bytes.Buffer
	if code := cmdSessionAttachWith([]string{"bd-1"}, sessionAttachOpts{noStart: true}, &stdout, &stderr); code != 1 {
		t.Fatalf("gc session attach = %d, want 1; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "gc session attach: expanding packs: ") {
		t.Errorf("stderr = %q, want attach to fail on the quarantined rig's pack error", stderr.String())
	}
}

// The control dispatcher compiles and routes workflow steps from the
// configured formulas and agents, and quarantines a control bead it cannot
// compile or route as a hard failure. On a composition without a rig's packs
// it would fail that rig's workflows for good, so it reports the quarantine
// and processes nothing, as it did when the load failed outright.
func TestControlDispatcherReportsAQuarantinedRig(t *testing.T) {
	c := newRigQuarantineCity(t)

	err := runControlDispatcherInStore(c.cityDir, "", "gd-1", io.Discard, io.Discard, nil)
	if !quarantinedRigError(err, "bad") {
		t.Fatalf("control dispatch err = %v, want rig bad's pack error", err)
	}

	store := beads.NewMemStore()
	// A control bead carries the graph contract marker, as every compiled
	// workflow step does, so a split city files it in the graph class the
	// dispatcher reads control beads from.
	check, err := store.Create(beads.Bead{Title: "check", Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey:            "check",
		beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
	}})
	if err != nil {
		t.Fatal(err)
	}
	err = runControlDispatcherWithStore(c.cityDir, c.cityDir, store, check.ID, io.Discard, io.Discard)
	if !quarantinedRigError(err, "bad") {
		t.Fatalf("control dispatch of a check err = %v, want rig bad's pack error", err)
	}
	after, err := store.Get(check.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "open" || len(after.Labels) != 0 || !maps.Equal(after.Metadata, check.Metadata) {
		t.Errorf("control bead after the refused dispatch = status %q labels %v metadata %v, want it untouched", after.Status, after.Labels, after.Metadata)
	}
}

// quarantinedRigError reports whether err is the strict load's error for the
// named rig.
func quarantinedRigError(err error, rig string) bool {
	var rigErr *config.RigPackError
	return errors.As(err, &rigErr) && rigErr.Rig == rig
}
