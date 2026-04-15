package account

import (
	"encoding/json"
	"testing"
)

func TestAccount_ZeroValue(t *testing.T) {
	var acct Account

	// Zero-value Account should compile and be marshallable to JSON.
	data, err := json.Marshal(acct)
	if err != nil {
		t.Fatalf("json.Marshal(Account{}) failed: %v", err)
	}

	// Should round-trip back without error.
	var got Account
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal(Account{}) failed: %v", err)
	}

	if got != acct {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", got, acct)
	}
}

func TestRegistry_EmptyAccounts(t *testing.T) {
	var reg Registry

	// Zero-value Registry should have nil/empty Accounts slice.
	if len(reg.Accounts) != 0 {
		t.Fatalf("len(Registry{}.Accounts) = %d, want 0", len(reg.Accounts))
	}

	// Default should be empty string.
	if reg.Default != "" {
		t.Fatalf("Registry{}.Default = %q, want \"\"", reg.Default)
	}
}
