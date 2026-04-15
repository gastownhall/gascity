package account

import (
	"strings"
	"testing"
)

// acctA and acctB are reusable test accounts for resolve tests.
var (
	resolveAcctA = Account{
		Handle:      "work1",
		Email:       "work1@example.com",
		Description: "Work account one",
		ConfigDir:   "/tmp/work1",
	}
	resolveAcctB = Account{
		Handle:      "personal",
		Email:       "me@example.com",
		Description: "Personal account",
		ConfigDir:   "/tmp/personal",
	}
	resolveAcctC = Account{
		Handle:      "team",
		Email:       "team@example.com",
		Description: "Team account",
		ConfigDir:   "/tmp/team",
	}
)

func TestResolve_EnvOverride(t *testing.T) {
	// GC_ACCOUNT env overrides flag, config, and default.
	reg := TestRegistry(t, resolveAcctA, resolveAcctB)

	got, err := Resolve(reg, resolveAcctB.Handle, "", "")
	if err != nil {
		t.Fatalf("Resolve() error = %v, want nil", err)
	}
	if got != resolveAcctB {
		t.Errorf("Resolve() = %+v, want %+v", got, resolveAcctB)
	}
}

func TestResolve_FlagOverride(t *testing.T) {
	// --account flag overrides config and default (no env).
	reg := TestRegistry(t, resolveAcctA, resolveAcctB)

	got, err := Resolve(reg, "", resolveAcctB.Handle, "")
	if err != nil {
		t.Fatalf("Resolve() error = %v, want nil", err)
	}
	if got != resolveAcctB {
		t.Errorf("Resolve() = %+v, want %+v", got, resolveAcctB)
	}
}

func TestResolve_ConfigField(t *testing.T) {
	// agent.Account config field used when no env/flag.
	reg := TestRegistry(t, resolveAcctA, resolveAcctB)

	got, err := Resolve(reg, "", "", resolveAcctB.Handle)
	if err != nil {
		t.Fatalf("Resolve() error = %v, want nil", err)
	}
	if got != resolveAcctB {
		t.Errorf("Resolve() = %+v, want %+v", got, resolveAcctB)
	}
}

func TestResolve_DefaultFallback(t *testing.T) {
	// Registry default used as last resort when no env/flag/config.
	reg := TestRegistry(t, resolveAcctA, resolveAcctB)
	// Default is "work1" (first account).

	got, err := Resolve(reg, "", "", "")
	if err != nil {
		t.Fatalf("Resolve() error = %v, want nil", err)
	}
	if got != resolveAcctA {
		t.Errorf("Resolve() = %+v, want %+v (default)", got, resolveAcctA)
	}
}

func TestResolve_NoAccount(t *testing.T) {
	// All empty inputs → zero Account, nil error (graceful no-op).
	reg := TestRegistry(t) // empty registry, no accounts, empty default

	got, err := Resolve(reg, "", "", "")
	if err != nil {
		t.Fatalf("Resolve() error = %v, want nil", err)
	}
	zero := Account{}
	if got != zero {
		t.Errorf("Resolve() = %+v, want zero Account", got)
	}
}

func TestResolve_UnknownHandle(t *testing.T) {
	// Handle not in registry produces error naming the handle.
	reg := TestRegistry(t, resolveAcctA)

	_, err := Resolve(reg, "nonexistent", "", "")
	if err == nil {
		t.Fatal("Resolve() error = nil, want error for unknown handle")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "nonexistent")
	}
}

func TestResolve_PriorityOrder(t *testing.T) {
	// All four sources set simultaneously → env wins.
	reg := TestRegistry(t, resolveAcctA, resolveAcctB, resolveAcctC)

	got, err := Resolve(reg, resolveAcctC.Handle, resolveAcctB.Handle, resolveAcctA.Handle)
	if err != nil {
		t.Fatalf("Resolve() error = %v, want nil", err)
	}
	// env (team) should win over flag (personal), config (work1), and default (work1).
	if got != resolveAcctC {
		t.Errorf("Resolve() = %+v, want %+v (env should win)", got, resolveAcctC)
	}
}

func TestResolve_EnvOverridesFlag(t *testing.T) {
	// env=A, flag=B → returns A.
	reg := TestRegistry(t, resolveAcctA, resolveAcctB)

	got, err := Resolve(reg, resolveAcctA.Handle, resolveAcctB.Handle, "")
	if err != nil {
		t.Fatalf("Resolve() error = %v, want nil", err)
	}
	if got != resolveAcctA {
		t.Errorf("Resolve() = %+v, want %+v (env should override flag)", got, resolveAcctA)
	}
}

func TestResolve_FlagOverridesConfig(t *testing.T) {
	// flag=A, config=B → returns A.
	reg := TestRegistry(t, resolveAcctA, resolveAcctB)

	got, err := Resolve(reg, "", resolveAcctA.Handle, resolveAcctB.Handle)
	if err != nil {
		t.Fatalf("Resolve() error = %v, want nil", err)
	}
	if got != resolveAcctA {
		t.Errorf("Resolve() = %+v, want %+v (flag should override config)", got, resolveAcctA)
	}
}
