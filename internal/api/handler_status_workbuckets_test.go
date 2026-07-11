package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestStatusWorkBucketsCoverEveryCountField guards the single-mapping
// invariant: every int field of StatusWorkCounts must be reachable through
// exactly one statusWorkBuckets entry. A new count field without a bucket
// (or a bucket resolving to a duplicated/absent field) fails here instead of
// silently under-counting in whichever of the three count paths forgot it.
func TestStatusWorkBucketsCoverEveryCountField(t *testing.T) {
	var wc workCounts
	seen := map[*int]string{}
	for _, status := range statusWorkBuckets {
		p := workBucketPtr(&wc, status)
		if p == nil {
			t.Fatalf("statusWorkBuckets entry %q has no StatusWorkCounts field", status)
		}
		if prev, dup := seen[p]; dup {
			t.Fatalf("bucket %q resolves to the same field as %q", status, prev)
		}
		seen[p] = status
	}

	v := reflect.ValueOf(&wc).Elem()
	tp := v.Type()
	for i := 0; i < tp.NumField(); i++ {
		if tp.Field(i).Type.Kind() != reflect.Int {
			continue
		}
		ptr := v.Field(i).Addr().Interface().(*int)
		if _, ok := seen[ptr]; !ok {
			t.Errorf("StatusWorkCounts.%s has no statusWorkBuckets entry — it would be silently dropped from work counts", tp.Field(i).Name)
		}
	}
}

// cannedCounterStore wraps a Store with a canned per-status Counter so the
// Counter fast path — otherwise untested (MemStore is not a Counter) — is
// exercised end to end through the status endpoint.
type cannedCounterStore struct {
	beads.Store
	counts map[string]int
}

func (c *cannedCounterStore) Count(_ context.Context, query beads.ListQuery, _ ...string) (int, error) {
	return c.counts[query.Status], nil
}

func TestHandleStatusWorkCountsViaCounterPath(t *testing.T) {
	state := newFakeState(t)
	state.stores["myrig"] = &cannedCounterStore{
		Store: state.stores["myrig"],
		counts: map[string]int{
			"open":        4,
			"ready":       3,
			"in_progress": 2,
			"hooked":      5,
			"review":      1,
		},
	}
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/status"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var resp statusResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := workCounts{Open: 4, Ready: 3, InProgress: 2, Hooked: 5, Review: 1}
	if resp.Work != want {
		t.Errorf("Work = %+v, want %+v (Counter fast-path counts must flow through unmodified)", resp.Work, want)
	}
}
