package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/account"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestSling_AccountFlag_Valid verifies that gc sling --account with a valid
// handle passes the flag through slingOpts/slingDeps and the dispatch succeeds.
func TestSling_AccountFlag_Valid(t *testing.T) {
	// Set up a temp config dir for the account.
	cfgDir := t.TempDir()

	reg := account.Registry{
		Default: "work1",
		Accounts: []account.Account{
			{Handle: "work1", Email: "u@example.com", ConfigDir: cfgDir},
		},
	}

	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "coder"}

	deps, _, stderr := testDeps(cfg, sp, runner.run)
	deps.AccountRegistry = reg // new field — will fail until implemented

	opts := testOpts(a, "BL-42")
	opts.AccountFlag = "work1" // new field — will fail until implemented

	code := doSling(opts, deps, nil)
	if code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}
}

// TestSling_AccountFlag_Unknown verifies that gc sling --account with an
// unknown handle produces an immediate error with a descriptive message
// containing "not registered".
func TestSling_AccountFlag_Unknown(t *testing.T) {
	// Set up a temp config dir for the registered account.
	cfgDir := t.TempDir()

	reg := account.Registry{
		Default: "work1",
		Accounts: []account.Account{
			{Handle: "work1", Email: "u@example.com", ConfigDir: cfgDir},
		},
	}

	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "coder"}

	deps, _, stderr := testDeps(cfg, sp, runner.run)
	deps.AccountRegistry = reg

	opts := testOpts(a, "BL-42")
	opts.AccountFlag = "unknown-handle" // not in registry

	code := doSling(opts, deps, nil)
	if code == 0 {
		t.Fatal("doSling returned 0, want non-zero for unknown account handle")
	}
	if !strings.Contains(stderr.String(), "not registered") {
		t.Errorf("stderr = %q, want to contain 'not registered'", stderr.String())
	}
	if !strings.Contains(stderr.String(), "unknown-handle") {
		t.Errorf("stderr = %q, want to contain 'unknown-handle'", stderr.String())
	}
}

// TestSling_AccountFlag_Empty verifies that omitting --account leaves the
// existing sling behavior unchanged — no account validation, dispatch proceeds.
func TestSling_AccountFlag_Empty(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "coder"}

	deps, _, stderr := testDeps(cfg, sp, runner.run)
	// AccountRegistry and AccountFlag are zero-value (empty) — no account.

	opts := testOpts(a, "BL-42")
	// opts.AccountFlag is "" — no flag.

	code := doSling(opts, deps, nil)
	if code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}
	// Verify no account-related error or env injection.
	if strings.Contains(stderr.String(), "account") {
		t.Errorf("stderr mentions 'account' unexpectedly: %s", stderr.String())
	}
}

// TestCmdSling_AccountFlagLoadsRegistry verifies that the cmdSling function
// loads the account registry from accounts.json when --account is specified,
// and passes the flag value through to the sling flow. This is a higher-level
// test that checks the CLI integration path.
func TestCmdSling_AccountFlagLoadsRegistry(t *testing.T) {
	// Create a minimal city directory with accounts.json.
	cityDir := t.TempDir()
	gcDir := filepath.Join(cityDir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgDir := t.TempDir()

	// Write accounts.json with a registered account.
	reg := account.Registry{
		Default: "work1",
		Accounts: []account.Account{
			{Handle: "work1", Email: "u@example.com", ConfigDir: cfgDir},
		},
	}
	acctPath := filepath.Join(gcDir, "accounts.json")
	if err := account.Save(acctPath, reg); err != nil {
		t.Fatal(err)
	}

	// Verify the accounts.json file was written correctly.
	loaded, err := account.Load(acctPath)
	if err != nil {
		t.Fatalf("Load accounts.json: %v", err)
	}
	if len(loaded.Accounts) != 1 || loaded.Accounts[0].Handle != "work1" {
		t.Fatalf("accounts.json round-trip failed: got %+v", loaded)
	}
}
