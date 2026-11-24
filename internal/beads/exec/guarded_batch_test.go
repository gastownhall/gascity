package exec //nolint:revive // internal package, always imported with alias

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The exec provider drives a script with one command per write, so it has no
// way to apply several writes atomically and must not report guarded batches.
func TestExecStoreReportsNoGuardedBatches(t *testing.T) {
	if batcher, ok := beads.GuardedBatcherFor(NewStore("/nonexistent/beads-script")); ok {
		t.Fatalf("GuardedBatcherFor(exec.Store) = (%T, true), want no capability", batcher)
	}
}
