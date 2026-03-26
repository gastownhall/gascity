package quota

import (
	"testing"
	"time"
)

func TestNextAvailableLRU(t *testing.T) {
	now := time.Now()
	state := QuotaState{
		Accounts: []AccountQuota{
			{Handle: "a", Status: StatusAvailable, LastUsed: now.Add(-1 * time.Hour)},
			{Handle: "b", Status: StatusAvailable, LastUsed: now.Add(-2 * time.Hour)},
			{Handle: "c", Status: StatusLimited},
		},
	}

	got, err := NextAvailable(state, "")
	if err != nil {
		t.Fatalf("NextAvailable: %v", err)
	}
	if got != "b" {
		t.Errorf("got %q, want b (LRU)", got)
	}
}

func TestNextAvailableSkipsCurrent(t *testing.T) {
	now := time.Now()
	state := QuotaState{
		Accounts: []AccountQuota{
			{Handle: "a", Status: StatusAvailable, LastUsed: now.Add(-2 * time.Hour)},
			{Handle: "b", Status: StatusAvailable, LastUsed: now.Add(-1 * time.Hour)},
		},
	}

	got, err := NextAvailable(state, "a")
	if err != nil {
		t.Fatalf("NextAvailable: %v", err)
	}
	if got != "b" {
		t.Errorf("got %q, want b (skipped current a)", got)
	}
}

func TestNextAvailableAllLimited(t *testing.T) {
	state := QuotaState{
		Accounts: []AccountQuota{
			{Handle: "a", Status: StatusLimited},
			{Handle: "b", Status: StatusCooldown},
		},
	}
	_, err := NextAvailable(state, "")
	if err == nil {
		t.Fatal("expected error when all accounts limited")
	}
}

func TestMarkLimited(t *testing.T) {
	now := time.Now()
	state := QuotaState{
		Accounts: []AccountQuota{
			{Handle: "a", Status: StatusAvailable},
		},
	}

	MarkLimited(&state, "a", 30*time.Minute, now)

	aq := state.Get("a")
	if aq.Status != StatusLimited {
		t.Errorf("Status = %q, want limited", aq.Status)
	}
	if aq.LimitedAt != now {
		t.Errorf("LimitedAt = %v, want %v", aq.LimitedAt, now)
	}
	if !aq.ResetsAt.Equal(now.Add(30 * time.Minute)) {
		t.Errorf("ResetsAt = %v, want %v", aq.ResetsAt, now.Add(30*time.Minute))
	}
}

func TestMarkUsed(t *testing.T) {
	now := time.Now()
	var state QuotaState

	MarkUsed(&state, "new", now)

	aq := state.Get("new")
	if aq == nil {
		t.Fatal("expected GetOrCreate to create entry")
	}
	if aq.UseCount != 1 {
		t.Errorf("UseCount = %d, want 1", aq.UseCount)
	}
	if aq.LastUsed != now {
		t.Errorf("LastUsed = %v, want %v", aq.LastUsed, now)
	}
}

func TestExpireStale(t *testing.T) {
	now := time.Now()
	state := QuotaState{
		Accounts: []AccountQuota{
			{Handle: "a", Status: StatusLimited, ResetsAt: now.Add(-1 * time.Minute)},
			{Handle: "b", Status: StatusLimited, ResetsAt: now.Add(1 * time.Hour)},
			{Handle: "c", Status: StatusAvailable},
		},
	}

	expired := ExpireStale(&state, now)
	if expired != 1 {
		t.Errorf("expired = %d, want 1", expired)
	}
	if state.Get("a").Status != StatusAvailable {
		t.Errorf("a should be available after expiry")
	}
	if state.Get("b").Status != StatusLimited {
		t.Errorf("b should still be limited")
	}
}

func TestClearAll(t *testing.T) {
	state := QuotaState{
		Accounts: []AccountQuota{
			{Handle: "a", Status: StatusLimited},
			{Handle: "b", Status: StatusCooldown},
		},
	}

	ClearAll(&state)

	for _, aq := range state.Accounts {
		if aq.Status != StatusAvailable {
			t.Errorf("%s: Status = %q, want available", aq.Handle, aq.Status)
		}
	}
}
