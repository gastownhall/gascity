package worker

import (
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// routedProvider routes every session to one backend.
type routedProvider struct {
	runtime.Provider
	backend runtime.Provider
	known   bool
}

func (p routedProvider) RouteFor(string) runtime.Route {
	return runtime.Route{Backend: runtime.Backend{Label: "leaf", Provider: p.backend}, Known: p.known}
}

// TestDeliveryKeysDeduplicated pins the read of whether a session's routed
// runtime deduplicates keyed deliveries: only a leaf that implements
// runtime.KeyedNudgeProvider does, whatever composite routes to it, and a
// route the composite cannot vouch for does not.
func TestDeliveryKeysDeduplicated(t *testing.T) {
	keyed := runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: runtime.NewFake()})
	plain := runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()})
	// FullFake implements NudgeKeyed, as the auto and hybrid composites do
	// for every session: implementing it is not deduplicating.
	composite := runtime.FullFake{Fake: runtime.NewFake()}
	for _, tc := range []struct {
		name string
		sp   runtime.Provider
		want bool
	}{
		{name: "nil", sp: nil},
		{name: "keyed leaf", sp: keyed, want: true},
		{name: "plain leaf", sp: plain},
		{name: "routed to a keyed leaf", sp: routedProvider{Provider: composite, backend: keyed, known: true}, want: true},
		{name: "routed to a plain leaf", sp: routedProvider{Provider: composite, backend: plain, known: true}},
		{name: "route not known", sp: routedProvider{Provider: composite, backend: keyed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeliveryKeysDeduplicated(tc.sp, "s"); got != tc.want {
				t.Fatalf("DeliveryKeysDeduplicated = %v, want %v", got, tc.want)
			}
		})
	}
}
