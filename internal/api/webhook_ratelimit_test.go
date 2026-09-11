package api

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestWebhookRateLimiter_BurstThenRefill(t *testing.T) {
	l := newWebhookRateLimiter()
	now := time.Now()
	l.now = func() time.Time { return now }

	// burst=2 → first two allowed, third denied.
	if ok, _ := l.allow("h", 60, 2); !ok {
		t.Fatal("1st delivery within burst must be allowed")
	}
	if ok, _ := l.allow("h", 60, 2); !ok {
		t.Fatal("2nd delivery within burst must be allowed")
	}
	ok, retry := l.allow("h", 60, 2)
	if ok {
		t.Fatal("3rd delivery must be denied (burst exhausted)")
	}
	if retry <= 0 {
		t.Fatalf("denied delivery must carry a positive Retry-After, got %v", retry)
	}

	// 60/min == 1/sec: advancing one second restores exactly one token.
	now = now.Add(time.Second)
	if ok, _ := l.allow("h", 60, 2); !ok {
		t.Fatal("after a 1s refill one token should be available")
	}
	if ok, _ := l.allow("h", 60, 2); ok {
		t.Fatal("only one token should have refilled")
	}
}

func TestWebhookRateLimiter_PerWebhookIsolation(t *testing.T) {
	l := newWebhookRateLimiter()
	now := time.Now()
	l.now = func() time.Time { return now }

	if ok, _ := l.allow("a", 60, 1); !ok {
		t.Fatal("webhook a first token")
	}
	if ok, _ := l.allow("a", 60, 1); ok {
		t.Fatal("webhook a should be exhausted")
	}
	// A different webhook has its own bucket and is unaffected.
	if ok, _ := l.allow("b", 60, 1); !ok {
		t.Fatal("webhook b must have its own independent bucket")
	}
}

func TestWebhookRateLimiter_NonPositiveDisables(t *testing.T) {
	l := newWebhookRateLimiter()
	if ok, retry := l.allow("h", 0, 1); ok || retry <= 0 {
		t.Fatalf("perMinute<=0 must fail closed with a wait, got ok=%v retry=%v", ok, retry)
	}
}

func TestSetRetryAfter_RoundsUpAndFloors(t *testing.T) {
	rec := httptest.NewRecorder()
	setRetryAfter(rec, 1500*time.Millisecond)
	if got := rec.Header().Get("Retry-After"); got != "2" {
		t.Errorf("Retry-After = %q, want 2 (ceil of 1.5s)", got)
	}
	rec2 := httptest.NewRecorder()
	setRetryAfter(rec2, 5*time.Millisecond)
	if got := rec2.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1 (floored)", got)
	}
}
