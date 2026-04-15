package account

import (
	"testing"
)

func TestTestRegistry_Basic(t *testing.T) {
	acctA := Account{
		Handle:      "work1",
		Email:       "work1@example.com",
		Description: "Work account one",
		ConfigDir:   "/tmp/work1",
	}
	acctB := Account{
		Handle:      "personal",
		Email:       "me@example.com",
		Description: "Personal account",
		ConfigDir:   "/tmp/personal",
	}

	// Two accounts: first should become the default.
	reg := TestRegistry(t, acctA, acctB)

	// Default should be the handle of the first account.
	if reg.Default != acctA.Handle {
		t.Fatalf("Default = %q, want %q", reg.Default, acctA.Handle)
	}

	// Accounts slice should contain both accounts in order.
	if len(reg.Accounts) != 2 {
		t.Fatalf("len(Accounts) = %d, want 2", len(reg.Accounts))
	}
	if reg.Accounts[0] != acctA {
		t.Errorf("Accounts[0] = %+v, want %+v", reg.Accounts[0], acctA)
	}
	if reg.Accounts[1] != acctB {
		t.Errorf("Accounts[1] = %+v, want %+v", reg.Accounts[1], acctB)
	}
}

func TestTestRegistry_Empty(t *testing.T) {
	// Zero accounts should yield empty registry with empty default.
	reg := TestRegistry(t)

	if reg.Default != "" {
		t.Fatalf("Default = %q, want \"\"", reg.Default)
	}
	if len(reg.Accounts) != 0 {
		t.Fatalf("len(Accounts) = %d, want 0", len(reg.Accounts))
	}
}

func TestTestRegistry_SingleAccount(t *testing.T) {
	acct := Account{
		Handle:    "solo",
		ConfigDir: "/tmp/solo",
	}

	reg := TestRegistry(t, acct)

	if reg.Default != "solo" {
		t.Fatalf("Default = %q, want %q", reg.Default, "solo")
	}
	if len(reg.Accounts) != 1 {
		t.Fatalf("len(Accounts) = %d, want 1", len(reg.Accounts))
	}
	if reg.Accounts[0] != acct {
		t.Errorf("Accounts[0] = %+v, want %+v", reg.Accounts[0], acct)
	}
}
