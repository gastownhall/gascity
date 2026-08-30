package clock

import (
	"testing"
	"time"
)

func TestFakeAdvance(t *testing.T) {
	start := time.Date(2026, time.August, 30, 8, 0, 0, 0, time.UTC)
	fake := &Fake{Time: start}

	fake.Advance(90 * time.Second)

	if got, want := fake.Now(), start.Add(90*time.Second); !got.Equal(want) {
		t.Fatalf("Fake.Now() = %s, want %s", got, want)
	}
}

func TestRealNowIsRecent(t *testing.T) {
	before := time.Now()
	got := (Real{}).Now()
	after := time.Now()

	if got.Before(before) || got.After(after) {
		t.Fatalf("Real.Now() = %s, outside [%s, %s]", got, before, after)
	}
}
