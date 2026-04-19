package api

import "testing"

// cacheKeyFor walked a struct and called fv.Int() on any Int/Int64/Uint64
// field, but reflect.Value.Int() panics on uint64 values. The kind guard
// was on the wrong side of && (fv.Int() evaluated first). R2-2 switches
// to fv.IsZero() which is Kind-safe.
type cacheUintInput struct {
	After uint64 `query:"after"`
}

func TestCacheKeyForHandlesUint64Field(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("cacheKeyFor panicked on uint64 field: %v", r)
		}
	}()
	_ = cacheKeyFor("test", &cacheUintInput{After: 42})
	_ = cacheKeyFor("test", &cacheUintInput{After: 0})
}
