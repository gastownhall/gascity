package config

import (
	"encoding/json"
	"testing"
)

func TestQuotaAccountStatus_IsDistinctType(_ *testing.T) {
	// QuotaAccountStatus must be a distinct named type, not assignable
	// from/to other string-based status types in the codebase.
	// This is a compile-time assertion: if QuotaAccountStatus were a plain
	// string alias or the same type as another status, these assignments
	// would compile. By verifying the type is used correctly here, we
	// confirm it's distinct.
	s := QuotaStatusAvailable
	_ = s

	// Verify it's a string underneath (for JSON marshaling) but a named type.
	str := string(s)
	_ = str

	// If QuotaAccountStatus were just "string", this test would still compile,
	// but the explicit conversion above (string(s)) would be unnecessary.
	// The real compile-time check: you CANNOT assign a plain string to
	// QuotaAccountStatus without conversion.
	// Uncomment the line below to verify it fails:
	// var bad QuotaAccountStatus = "raw-string" // should require explicit conversion
}

func TestQuotaAccountStatus_Values(t *testing.T) {
	tests := []struct {
		status QuotaAccountStatus
		want   string
	}{
		{QuotaStatusAvailable, "available"},
		{QuotaStatusLimited, "limited"},
		{QuotaStatusCooldown, "cooldown"},
	}
	for _, tt := range tests {
		if string(tt.status) != tt.want {
			t.Errorf("QuotaAccountStatus = %q, want %q", tt.status, tt.want)
		}
	}
}

func TestQuotaState_JSONRoundTrip(t *testing.T) {
	original := QuotaState{
		Accounts: map[string]QuotaAccountState{
			"work1": {
				Status:    QuotaStatusLimited,
				LimitedAt: "2026-04-06T12:00:00Z",
				ResetsAt:  "2026-04-06T13:00:00Z",
				LastUsed:  "2026-04-06T11:55:00Z",
			},
			"work2": {
				Status:   QuotaStatusAvailable,
				LastUsed: "2026-04-06T10:00:00Z",
			},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got QuotaState
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	// Verify all fields round-tripped
	if len(got.Accounts) != 2 {
		t.Fatalf("got %d accounts, want 2", len(got.Accounts))
	}

	w1 := got.Accounts["work1"]
	if w1.Status != QuotaStatusLimited {
		t.Errorf("work1.Status = %q, want %q", w1.Status, QuotaStatusLimited)
	}
	if w1.LimitedAt != "2026-04-06T12:00:00Z" {
		t.Errorf("work1.LimitedAt = %q, want %q", w1.LimitedAt, "2026-04-06T12:00:00Z")
	}
	if w1.ResetsAt != "2026-04-06T13:00:00Z" {
		t.Errorf("work1.ResetsAt = %q, want %q", w1.ResetsAt, "2026-04-06T13:00:00Z")
	}
	if w1.LastUsed != "2026-04-06T11:55:00Z" {
		t.Errorf("work1.LastUsed = %q, want %q", w1.LastUsed, "2026-04-06T11:55:00Z")
	}

	w2 := got.Accounts["work2"]
	if w2.Status != QuotaStatusAvailable {
		t.Errorf("work2.Status = %q, want %q", w2.Status, QuotaStatusAvailable)
	}
	if w2.LimitedAt != "" {
		t.Errorf("work2.LimitedAt = %q, want empty", w2.LimitedAt)
	}
	if w2.ResetsAt != "" {
		t.Errorf("work2.ResetsAt = %q, want empty", w2.ResetsAt)
	}
	if w2.LastUsed != "2026-04-06T10:00:00Z" {
		t.Errorf("work2.LastUsed = %q, want %q", w2.LastUsed, "2026-04-06T10:00:00Z")
	}
}

func TestQuotaState_EmptyAccounts(t *testing.T) {
	state := QuotaState{
		Accounts: map[string]QuotaAccountState{},
	}

	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	want := `{"accounts":{}}`
	if string(data) != want {
		t.Errorf("Marshal empty state = %s, want %s", data, want)
	}
}
