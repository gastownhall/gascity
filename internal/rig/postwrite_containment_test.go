package rig

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// postWriteFixture is provisionToWritePhase with a rig path that does not exist
// yet, so Provision's step 10 creates it (the write the post-write containment
// hook follows).
func postWriteFixture(t *testing.T) (Deps, ProvisionRequest, string) {
	t.Helper()
	deps, req, tomlPath := provisionToWritePhase(t)
	req.Path = filepath.Join(t.TempDir(), "newrig")
	return deps, req, tomlPath
}

// TestProvisionVerifyRigPathRunsAfterDirCreation pins that the post-write hook
// is invoked with the rig path once step 10 has created it, so the callback
// observes the directory the write produced.
func TestProvisionVerifyRigPathRunsAfterDirCreation(t *testing.T) {
	deps, req, _ := postWriteFixture(t)

	var gotPath string
	var existedAtCall bool
	deps.VerifyRigPath = func(rigPath string) error {
		gotPath = rigPath
		_, err := os.Stat(rigPath)
		existedAtCall = err == nil
		return nil
	}

	if _, _, err := Provision(deps, req); err != nil {
		t.Fatalf("Provision = %v, want nil", err)
	}
	if gotPath == "" {
		t.Fatal("VerifyRigPath was never called after the rig directory was created")
	}
	if !existedAtCall {
		t.Fatalf("VerifyRigPath ran before %s existed; it must follow the write", gotPath)
	}
}

// TestProvisionVerifyRigPathFailureAbortsDetectOnly pins the fail-loud, detect-only
// contract: a verification error is returned (wrapped) and Provision stops before
// any later step, and it does not delete, rename or modify the directory.
func TestProvisionVerifyRigPathFailureAbortsDetectOnly(t *testing.T) {
	deps, req, tomlPath := postWriteFixture(t)

	original, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	verifyErr := errors.New("rig path escapes the city root")
	deps.VerifyRigPath = func(string) error { return verifyErr }
	initStoreCalled := false
	deps.InitStore = func(string, string, string) (bool, error) {
		initStoreCalled = true
		return false, nil
	}

	_, _, provErr := Provision(deps, req)
	if provErr == nil {
		t.Fatal("Provision succeeded despite a failing VerifyRigPath; the failure must be a returned error")
	}
	if !errors.Is(provErr, verifyErr) {
		t.Fatalf("Provision error %v does not wrap the VerifyRigPath failure", provErr)
	}
	if initStoreCalled {
		t.Fatal("InitStore ran after VerifyRigPath failed; no later step may run")
	}
	after, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatal("city.toml changed although Provision aborted before the config write")
	}
	if _, err := os.Stat(req.Path); err != nil {
		t.Fatalf("rig directory was removed or moved on a failed verification (stat: %v); the check is detect-only", err)
	}
}

// TestProvisionVerifyRigPathNotCalledForPreexistingDir pins that the hook guards
// the directory this call creates: a path that already existed is not re-checked
// here (a clone that materialized it is verified by its own caller).
func TestProvisionVerifyRigPathNotCalledForPreexistingDir(t *testing.T) {
	deps, req, _ := provisionToWritePhase(t) // req.Path is an existing t.TempDir()
	called := false
	deps.VerifyRigPath = func(string) error { called = true; return nil }

	if _, _, err := Provision(deps, req); err != nil {
		t.Fatalf("Provision = %v, want nil", err)
	}
	if called {
		t.Fatal("VerifyRigPath ran for a pre-existing rig directory")
	}
}

// TestProvisionNilVerifyRigPathIsUnchanged pins that leaving the hook unset (the
// CLI path) behaves exactly as before: the rig is created and registered.
func TestProvisionNilVerifyRigPathIsUnchanged(t *testing.T) {
	deps, req, tomlPath := postWriteFixture(t)
	if deps.VerifyRigPath != nil {
		t.Fatal("precondition: stubDeps must leave VerifyRigPath nil")
	}
	if _, _, err := Provision(deps, req); err != nil {
		t.Fatalf("Provision = %v, want nil", err)
	}
	if _, err := os.Stat(req.Path); err != nil {
		t.Fatalf("rig directory not created: %v", err)
	}
	if got, err := os.ReadFile(tomlPath); err != nil || len(got) == 0 {
		t.Fatalf("city.toml unreadable after provision: %v", err)
	}
}
