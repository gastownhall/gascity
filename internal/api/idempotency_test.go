package api

import (
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// beadLike is a small struct used in these tests to exercise typed replay.
type beadLike struct {
	ID string `json:"id"`
}

func TestIdempotency_MissOnFirstRequest(t *testing.T) {
	c := newIdempotencyCache(30 * time.Minute)
	_, found := c.reserve("key-1", "hash-abc")
	if found {
		t.Error("reserve should return found=false on first request (key was reserved for us)")
	}
}

func TestIdempotency_HitOnReplay(t *testing.T) {
	c := newIdempotencyCache(30 * time.Minute)

	// Reserve, then store the typed response.
	_, found := c.reserve("key-1", "hash-abc")
	if found {
		t.Fatal("reserve should return found=false on first request")
	}
	c.storeResponse("key-1", "hash-abc", beadLike{ID: "bead-42"})

	// Second request with same key + hash should see the stored entry.
	entry, found := c.reserve("key-1", "hash-abc")
	if !found {
		t.Fatal("reserve should return found=true after storeResponse")
	}
	if entry.pending {
		t.Errorf("entry should not be pending after storeResponse")
	}
	if entry.bodyHash != "hash-abc" {
		t.Errorf("bodyHash = %q, want %q", entry.bodyHash, "hash-abc")
	}
	got, ok := replayAs[beadLike](entry)
	if !ok {
		t.Fatal("replayAs should return ok=true for stored beadLike")
	}
	if got.ID != "bead-42" {
		t.Errorf("replay value ID = %q, want %q", got.ID, "bead-42")
	}
}

func TestIdempotency_MismatchDetectable(t *testing.T) {
	c := newIdempotencyCache(30 * time.Minute)
	_, _ = c.reserve("key-1", "hash-abc")
	c.storeResponse("key-1", "hash-abc", beadLike{ID: "bead-42"})

	// Replay with same key but different hash: the caller compares hashes
	// and returns 422 idempotency_mismatch.
	entry, found := c.reserve("key-1", "hash-xyz")
	if !found {
		t.Fatal("reserve should return found=true for existing key")
	}
	if entry.bodyHash == "hash-xyz" {
		t.Errorf("stored entry.bodyHash should remain %q, got %q", "hash-abc", entry.bodyHash)
	}
}

func TestIdempotency_PendingDetectable(t *testing.T) {
	c := newIdempotencyCache(30 * time.Minute)

	// First request reserves.
	_, found := c.reserve("key-1", "hash-abc")
	if found {
		t.Fatal("first request should reserve, not find existing")
	}

	// Second request with the same key while first is still in-flight sees
	// the pending entry.
	entry, found := c.reserve("key-1", "hash-abc")
	if !found {
		t.Fatal("second request should find the pending reservation")
	}
	if !entry.pending {
		t.Error("entry should be pending while first request is in-flight")
	}
}

func TestIdempotency_UnreserveAllowsRetry(t *testing.T) {
	c := newIdempotencyCache(30 * time.Minute)
	_, _ = c.reserve("key-1", "hash-abc")
	c.unreserve("key-1")

	// Retry should succeed: second reserve gets found=false (key available).
	_, found := c.reserve("key-1", "hash-abc")
	if found {
		t.Error("after unreserve, reserve should return found=false")
	}
}

func TestIdempotency_ExpiredEntryMisses(t *testing.T) {
	c := newIdempotencyCache(1 * time.Millisecond)
	_, _ = c.reserve("key-1", "hash-abc")
	c.storeResponse("key-1", "hash-abc", beadLike{ID: "bead-42"})

	time.Sleep(5 * time.Millisecond)

	_, found := c.reserve("key-1", "hash-abc")
	if found {
		t.Error("reserve should return found=false for expired entry")
	}
}

func TestIdempotency_EmptyKeySkips(t *testing.T) {
	c := newIdempotencyCache(30 * time.Minute)
	c.storeResponse("", "hash-abc", beadLike{ID: "bead-42"})
	if len(c.entries) != 0 {
		t.Errorf("cache should be empty after storeResponse with empty key, got %d entries", len(c.entries))
	}
}

func TestIdempotency_ConcurrentSameKey(t *testing.T) {
	c := newIdempotencyCache(30 * time.Minute)
	const goroutines = 10
	var reserved atomic.Int32
	var wg sync.WaitGroup

	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if _, found := c.reserve("race-key", "hash-same"); !found {
				reserved.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := reserved.Load(); got != 1 {
		t.Errorf("exactly 1 goroutine should reserve the key, got %d", got)
	}
}

func TestHashBody_Deterministic(t *testing.T) {
	body := map[string]string{"title": "test", "rig": "myrig"}
	h1 := hashBody(body)
	h2 := hashBody(body)
	if h1 != h2 {
		t.Errorf("hashBody should be deterministic: %q != %q", h1, h2)
	}
	if len(h1) != 64 {
		t.Errorf("hashBody should return 64-char hex string, got %d chars", len(h1))
	}
}

func TestHashBody_DifferentInputs(t *testing.T) {
	h1 := hashBody(map[string]string{"title": "a"})
	h2 := hashBody(map[string]string{"title": "b"})
	if h1 == h2 {
		t.Error("hashBody should produce different hashes for different inputs")
	}
}

func TestScopedIdemKey_NamespacedByEndpoint(t *testing.T) {
	r1 := httptest.NewRequest("POST", "/v0/beads", nil)
	r2 := httptest.NewRequest("POST", "/v0/mail", nil)

	k1 := scopedIdemKey(r1, "abc")
	k2 := scopedIdemKey(r2, "abc")
	if k1 == k2 {
		t.Errorf("same Idempotency-Key on different endpoints should produce different scoped keys: %q == %q", k1, k2)
	}
	if k1 == "" {
		t.Error("scopedIdemKey should return non-empty for non-empty key")
	}
}

func TestScopedIdemKey_EmptyKey(t *testing.T) {
	r := httptest.NewRequest("POST", "/v0/beads", nil)
	if got := scopedIdemKey(r, ""); got != "" {
		t.Errorf("scopedIdemKey with empty key should return empty, got %q", got)
	}
}
