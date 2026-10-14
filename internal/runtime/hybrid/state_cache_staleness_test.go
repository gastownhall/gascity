package hybrid

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// stalenessLeaf is a Fake that reports a fixed state-cache staleness.
type stalenessLeaf struct {
	*runtime.Fake
	age time.Duration
}

func (l stalenessLeaf) StateCacheStaleness() (time.Duration, bool) { return l.age, true }

// hybrid forwards the longest state-cache staleness of its backends, so a
// reader asserting the capability on the city provider reaches the tmux leg.
func TestStateCacheStalenessIsTheLongestBackendStaleness(t *testing.T) {
	p := New(stalenessLeaf{Fake: runtime.NewFake(), age: 90 * time.Second}, stalenessLeaf{Fake: runtime.NewFake(), age: 40 * time.Second}, isRemote)
	age, known := p.StateCacheStaleness()
	if !known || age != 90*time.Second {
		t.Fatalf("StateCacheStaleness = (%s, %v), want (90s, true)", age, known)
	}
}

func TestStateCacheStalenessUnknownWithoutACacheBackend(t *testing.T) {
	p := New(runtime.NewFake(), runtime.NewFake(), isRemote)
	if age, known := p.StateCacheStaleness(); age != 0 || known {
		t.Fatalf("StateCacheStaleness = (%s, %v), want (0, false) when no backend keeps a state cache", age, known)
	}
}
