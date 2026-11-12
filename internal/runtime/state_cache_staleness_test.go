package runtime_test

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// stalenessLeaf is a Fake that reports a fixed state-cache staleness.
type stalenessLeaf struct {
	*runtime.Fake
	age   time.Duration
	known bool
}

func (l stalenessLeaf) StateCacheStaleness() (time.Duration, bool) { return l.age, l.known }

func TestMaxStateCacheStaleness(t *testing.T) {
	bare := runtime.Backend{Label: "bare", Provider: runtime.NewFake()}
	fresh := runtime.Backend{Label: "fresh", Provider: stalenessLeaf{Fake: runtime.NewFake(), age: 0, known: true}}
	stale := runtime.Backend{Label: "stale", Provider: stalenessLeaf{Fake: runtime.NewFake(), age: 40 * time.Second, known: true}}
	staler := runtime.Backend{Label: "staler", Provider: stalenessLeaf{Fake: runtime.NewFake(), age: 90 * time.Second, known: true}}
	unknown := runtime.Backend{Label: "unknown", Provider: stalenessLeaf{Fake: runtime.NewFake(), age: 5 * time.Minute, known: false}}

	tests := []struct {
		name     string
		backends []runtime.Backend
		wantAge  time.Duration
		wantOK   bool
	}{
		{"no backends", nil, 0, false},
		{"no backend keeps a cache", []runtime.Backend{bare}, 0, false},
		{"a cache that has not been primed reports nothing", []runtime.Backend{unknown}, 0, false},
		{"one fresh cache is known and not stale", []runtime.Backend{bare, fresh}, 0, true},
		{"the longest staleness wins", []runtime.Backend{stale, bare, staler, fresh}, 90 * time.Second, true},
		{"an unknown backend's age is ignored", []runtime.Backend{stale, unknown}, 40 * time.Second, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			age, known := runtime.MaxStateCacheStaleness(tc.backends)
			if age != tc.wantAge || known != tc.wantOK {
				t.Fatalf("MaxStateCacheStaleness = (%s, %v), want (%s, %v)", age, known, tc.wantAge, tc.wantOK)
			}
		})
	}
}
