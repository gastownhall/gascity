package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/doctor"
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
