package auto

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// M2: the stale-route fall-through keeps a corpse on the routed backend when
// the other backend answers not-running without error. A running or failing
// fall-through still wins, so legacy sees the Running, Alive and error it saw
// before. Kills: a corpse hidden; a corpse winning over a running
// fall-through; a corpse masking the fall-through's error (the err == nil
// guard dropped).
func TestProvider_ObserveLivenessWithErrorKeepsCorpseOverAbsentFallThrough(t *testing.T) {
	corpse := runtime.Liveness{Corpse: true, ObjectID: "$7"}
	errUnavailable := errors.New("acp probe failed")
	for _, tc := range []struct {
		desc    string
		other   runtime.Liveness
		err     error
		want    runtime.Liveness
		wantErr error
	}{
		{"absent fall-through", runtime.Liveness{}, nil, corpse, nil},
		{"running fall-through", runtime.Liveness{Running: true, Alive: true}, nil, runtime.Liveness{Running: true, Alive: true}, nil},
		{"failing fall-through", runtime.Liveness{}, errUnavailable, runtime.Liveness{}, errUnavailable},
	} {
		def := &errorBearingLivenessObserverStub{Fake: runtime.NewFake(), obs: corpse}
		other := &errorBearingLivenessObserverStub{Fake: runtime.NewFake(), obs: tc.other, err: tc.err}
		got, err := New(def, other).ObserveLivenessWithError("worker", nil)
		if got != tc.want || !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
			t.Errorf("%s: ObserveLivenessWithError = (%+v, %v), want (%+v, %v)", tc.desc, got, err, tc.want, tc.wantErr)
		}
	}
}
