package acceptancehelpers

import (
	"errors"
	"testing"
	"time"
)

func TestRemoveAllWithRetryFuncRetriesTransientFailure(t *testing.T) {
	calls := 0
	err := removeAllWithRetryFunc("synthetic-dir", time.Second, time.Nanosecond, func(string) error {
		calls++
		if calls == 1 {
			return errors.New("directory not empty")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("removeAllWithRetryFunc: %v", err)
	}
	if calls != 2 {
		t.Fatalf("remove calls = %d, want 2", calls)
	}
}
