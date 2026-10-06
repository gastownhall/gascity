package auto

import (
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// M2: the stale-route fall-through keeps a corpse on the routed backend when
// the other backend answers not-running, and still prefers a running answer.
// Running and Alive are what legacy saw before. Kills: the bool path hiding a
// corpse; a corpse winning over a running fall-through.
func TestProvider_ObserveLivenessKeepsCorpseOverAbsentFallThrough(t *testing.T) {
	corpse := runtime.Liveness{Corpse: true, ObjectID: "$7"}
	def := &livenessObserverStub{Fake: runtime.NewFake(), obs: corpse}
	absent := &livenessObserverStub{Fake: runtime.NewFake()}
	if got := New(def, absent).ObserveLiveness("worker", nil); got != corpse {
		t.Errorf("ObserveLiveness = %+v, want the corpse %+v", got, corpse)
	}
	running := &livenessObserverStub{Fake: runtime.NewFake(), obs: runtime.Liveness{Running: true, Alive: true}}
	if got := New(def, running).ObserveLiveness("worker", nil); got != running.obs {
		t.Errorf("ObserveLiveness = %+v, want the running fall-through %+v", got, running.obs)
	}
	errDef := &errorBearingLivenessObserverStub{Fake: runtime.NewFake(), obs: corpse}
	errAbsent := &errorBearingLivenessObserverStub{Fake: runtime.NewFake()}
	if got, err := New(errDef, errAbsent).ObserveLivenessWithError("worker", nil); err != nil || got != corpse {
		t.Errorf("ObserveLivenessWithError = (%+v, %v), want the corpse", got, err)
	}
}
