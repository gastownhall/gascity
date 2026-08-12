package api

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api/apierr"
)

// newIdemTestServer builds a Server with only the idempotency cache wired —
// withIdempotency touches nothing else.
func newIdemTestServer() *Server {
	return &Server{idem: newIdempotencyCache(30 * time.Minute)}
}

type idemVal struct {
	ID   string
	Body string
}

func TestWithIdempotency_FirstCallRunsCreate(t *testing.T) {
	s := newIdemTestServer()
	calls := 0
	got, err := withIdempotency(s.idem, "/v0/things", "key-1", map[string]string{"a": "1"},
		func() (idemVal, error) {
			calls++
			return idemVal{ID: "t1", Body: "1"}, nil
		})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("create calls = %d, want 1", calls)
	}
	if got.ID != "t1" {
		t.Fatalf("got.ID = %q, want t1", got.ID)
	}
}

func TestWithIdempotency_ReplayReturnsCachedWithoutRerunning(t *testing.T) {
	s := newIdemTestServer()
	body := map[string]string{"a": "1"}
	calls := 0
	create := func() (idemVal, error) {
		calls++
		return idemVal{ID: "t1", Body: "first"}, nil
	}
	if _, err := withIdempotency(s.idem, "/v0/things", "key-1", body, create); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Second call: same key + same body. create must NOT run again; the cached
	// value (not a fresh "second" run) must come back.
	got, err := withIdempotency(s.idem, "/v0/things", "key-1", body,
		func() (idemVal, error) {
			calls++
			return idemVal{ID: "t2", Body: "second"}, nil
		})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if calls != 1 {
		t.Fatalf("create calls = %d, want 1 (replay must not re-run)", calls)
	}
	if got.ID != "t1" || got.Body != "first" {
		t.Fatalf("replayed value = %+v, want the first-call value", got)
	}
}

func TestWithIdempotency_MismatchReturns422(t *testing.T) {
	s := newIdemTestServer()
	if _, err := withIdempotency(s.idem, "/v0/things", "key-1", map[string]string{"a": "1"},
		func() (idemVal, error) { return idemVal{ID: "t1"}, nil }); err != nil {
		t.Fatalf("first call: %v", err)
	}
	calls := 0
	_, err := withIdempotency(s.idem, "/v0/things", "key-1", map[string]string{"a": "DIFFERENT"},
		func() (idemVal, error) { calls++; return idemVal{}, nil })
	if calls != 0 {
		t.Fatalf("create ran on a body mismatch (calls=%d); it must not", calls)
	}
	var em *apierr.ErrorModel
	if !errors.As(err, &em) {
		t.Fatalf("error = %v, want *apierr.ErrorModel", err)
	}
	if em.Code != "idempotency-mismatch" || em.Status != 422 {
		t.Fatalf("mismatch error = code %q status %d, want idempotency-mismatch/422", em.Code, em.Status)
	}
}

func TestWithIdempotency_InFlightReturns409(t *testing.T) {
	s := newIdemTestServer()
	body := map[string]string{"a": "1"}
	// Simulate a concurrent in-flight request holding the reservation.
	scoped := "POST:/v0/things:key-1"
	s.idem.reserve(scoped, hashBody(body))

	calls := 0
	_, err := withIdempotency(s.idem, "/v0/things", "key-1", body,
		func() (idemVal, error) { calls++; return idemVal{}, nil })
	if calls != 0 {
		t.Fatalf("create ran while another request was in-flight (calls=%d)", calls)
	}
	var em *apierr.ErrorModel
	if !errors.As(err, &em) {
		t.Fatalf("error = %v, want *apierr.ErrorModel", err)
	}
	if em.Code != "idempotency-in-flight" || em.Status != 409 {
		t.Fatalf("in-flight error = code %q status %d, want idempotency-in-flight/409", em.Code, em.Status)
	}
}

func TestWithIdempotency_ErrorReleasesReservation(t *testing.T) {
	s := newIdemTestServer()
	body := map[string]string{"a": "1"}
	sentinel := errors.New("create boom")
	_, err := withIdempotency(s.idem, "/v0/things", "key-1", body,
		func() (idemVal, error) { return idemVal{}, sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want the create error propagated verbatim", err)
	}
	// The reservation must be released: a retry with the same key succeeds and
	// runs create again (no leaked 409).
	calls := 0
	got, err := withIdempotency(s.idem, "/v0/things", "key-1", body,
		func() (idemVal, error) { calls++; return idemVal{ID: "t1"}, nil })
	if err != nil {
		t.Fatalf("retry after failed create: %v (reservation leaked?)", err)
	}
	if calls != 1 || got.ID != "t1" {
		t.Fatalf("retry did not re-run create (calls=%d, got=%+v)", calls, got)
	}
}

func TestWithIdempotency_PanicReleasesReservation(t *testing.T) {
	s := newIdemTestServer()
	body := map[string]string{"a": "1"}
	func() {
		defer func() { _ = recover() }()
		_, _ = withIdempotency(s.idem, "/v0/things", "key-1", body,
			func() (idemVal, error) { panic("boom") })
		t.Fatal("expected panic to propagate")
	}()
	// The reservation must be released even though create() panicked: a retry
	// with the same key runs create again (no key wedged for the TTL).
	calls := 0
	got, err := withIdempotency(s.idem, "/v0/things", "key-1", body,
		func() (idemVal, error) { calls++; return idemVal{ID: "t1"}, nil })
	if err != nil {
		t.Fatalf("retry after panic: %v (reservation leaked on panic?)", err)
	}
	if calls != 1 || got.ID != "t1" {
		t.Fatalf("retry did not re-run create after panic (calls=%d, got=%+v)", calls, got)
	}
}

func TestWithIdempotency_WrongTypedEntryFallsThroughToCreate(t *testing.T) {
	s := newIdemTestServer()
	body := map[string]string{"a": "1"}
	// Seed a COMPLETED entry whose cached value is a different type than the
	// caller's T (idemVal). replayAs[idemVal] must fail and the helper must fall
	// through to create() rather than serve a wrong-typed/zero replay.
	s.idem.storeResponse("POST:/v0/things:key-1", hashBody(body), "not-an-idemVal")
	calls := 0
	got, err := withIdempotency(s.idem, "/v0/things", "key-1", body,
		func() (idemVal, error) { calls++; return idemVal{ID: "fresh"}, nil })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 || got.ID != "fresh" {
		t.Fatalf("wrong-typed entry did not fall through to create (calls=%d, got=%+v)", calls, got)
	}
}

func TestWithIdempotency_EmptyKeyPassthrough(t *testing.T) {
	s := newIdemTestServer()
	calls := 0
	create := func() (idemVal, error) { calls++; return idemVal{ID: "t"}, nil }
	if _, err := withIdempotency(s.idem, "/v0/things", "", nil, create); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := withIdempotency(s.idem, "/v0/things", "", nil, create); err != nil {
		t.Fatalf("second: %v", err)
	}
	if calls != 2 {
		t.Fatalf("empty key must be a passthrough: create calls = %d, want 2", calls)
	}
}

func TestWithIdempotency_DistinctKeysAndPathsIndependent(t *testing.T) {
	s := newIdemTestServer()
	body := map[string]string{"a": "1"}
	mk := func() (idemVal, error) { return idemVal{ID: "x"}, nil }
	if _, err := withIdempotency(s.idem, "/v0/things", "key-1", body, mk); err != nil {
		t.Fatalf("k1: %v", err)
	}
	// Same key, different path → must not collide (independent create).
	calls := 0
	if _, err := withIdempotency(s.idem, "/v0/others", "key-1", body,
		func() (idemVal, error) { calls++; return idemVal{ID: "y"}, nil }); err != nil {
		t.Fatalf("other path: %v", err)
	}
	if calls != 1 {
		t.Fatalf("same key on a different path collided (calls=%d, want 1)", calls)
	}
	// Different keys, same path, same body → must not collide either. This
	// pins that the KEY participates in the cache scope (dropping it from
	// scopedKey would make key-2 replay key-1's response).
	calls = 0
	if _, err := withIdempotency(s.idem, "/v0/things", "key-2", body,
		func() (idemVal, error) { calls++; return idemVal{ID: "z"}, nil }); err != nil {
		t.Fatalf("second key: %v", err)
	}
	if calls != 1 {
		t.Fatalf("a different key on the same path replayed the first key's response (calls=%d, want 1)", calls)
	}
}
