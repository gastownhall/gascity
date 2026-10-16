package runtime_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// declaredTurns is a Provider declaring fixed turn capabilities.
type declaredTurns struct {
	runtime.Provider
	caps runtime.TurnCapabilities
}

func (d declaredTurns) TurnCapabilities(string) runtime.TurnCapabilities { return d.caps }

func (declaredTurns) TurnHistory(context.Context, string) (runtime.TurnHistory, error) {
	return runtime.TurnHistory{}, nil
}

var noTurnCapabilities = runtime.TurnCapabilities{
	Start: runtime.TurnStartNone, End: runtime.TurnEndNone,
	Fence: runtime.TurnFenceNone, Link: runtime.TurnLinkNone,
}

// Kills a TurnCapabilitiesFor that invents capabilities for a runtime that
// declares none, passes an unknown or empty axis value through, or loses a
// known one.
func TestTurnCapabilitiesForNormalizes(t *testing.T) {
	if got := runtime.TurnCapabilitiesFor(keysOnlyProvider{runtime.NewFake()}, "s"); got != noTurnCapabilities {
		t.Fatalf("non-provider = %+v, want nothing on every axis", got)
	}
	if got := (runtime.TurnCapabilities{}).Normalized(); got != noTurnCapabilities {
		t.Fatalf("zero value normalized = %+v, want nothing on every axis", got)
	}
	unknown := runtime.TurnCapabilities{Start: "exact", End: "Recorded", Fence: "", Link: "atomic", Lossless: true}
	if got := runtime.TurnCapabilitiesFor(declaredTurns{runtime.NewFake(), unknown}, "s"); got != (runtime.TurnCapabilities{
		Start: runtime.TurnStartNone, End: runtime.TurnEndNone, Fence: runtime.TurnFenceNone, Link: runtime.TurnLinkNone, Lossless: true,
	}) {
		t.Fatalf("unknown values = %+v, want none on every axis and Lossless kept", got)
	}
	for _, caps := range []runtime.TurnCapabilities{
		{Start: runtime.TurnStartRecorded, End: runtime.TurnEndRecorded, Fence: runtime.TurnFenceAtomic, Link: runtime.TurnLinkExact, Lossless: true},
		{Start: runtime.TurnStartRecorded, End: runtime.TurnEndInferred, Fence: runtime.TurnFenceChecked, Link: runtime.TurnLinkCorrelated},
		{Start: runtime.TurnStartBoundary, End: runtime.TurnEndBoundary, Fence: runtime.TurnFenceNone, Link: runtime.TurnLinkNone},
	} {
		if got := runtime.TurnCapabilitiesFor(declaredTurns{runtime.NewFake(), caps}, "s"); got != caps {
			t.Errorf("declared %+v read as %+v", caps, got)
		}
	}
}

// Kills an axis order that is not the documented lowest-first one, or an
// unknown value that reads above none.
func TestTurnAxesOrderLowestFirst(t *testing.T) {
	starts := []runtime.TurnStart{runtime.TurnStartNone, runtime.TurnStartBoundary, runtime.TurnStartRecorded}
	ends := []runtime.TurnEnd{runtime.TurnEndNone, runtime.TurnEndBoundary, runtime.TurnEndInferred, runtime.TurnEndRecorded}
	fences := []runtime.TurnFence{runtime.TurnFenceNone, runtime.TurnFenceChecked, runtime.TurnFenceAtomic}
	links := []runtime.TurnLink{runtime.TurnLinkNone, runtime.TurnLinkCorrelated, runtime.TurnLinkExact}
	for i := range starts {
		for j := range starts {
			if got := starts[i].AtLeast(starts[j]); got != (i >= j) {
				t.Errorf("%s.AtLeast(%s) = %v", starts[i], starts[j], got)
			}
		}
	}
	for i := range ends {
		for j := range ends {
			if got := ends[i].AtLeast(ends[j]); got != (i >= j) {
				t.Errorf("%s.AtLeast(%s) = %v", ends[i], ends[j], got)
			}
		}
	}
	for i := range fences {
		for j := range fences {
			if got := fences[i].AtLeast(fences[j]); got != (i >= j) {
				t.Errorf("%s.AtLeast(%s) = %v", fences[i], fences[j], got)
			}
		}
	}
	for i := range links {
		for j := range links {
			if got := links[i].AtLeast(links[j]); got != (i >= j) {
				t.Errorf("%s.AtLeast(%s) = %v", links[i], links[j], got)
			}
		}
	}
	if runtime.TurnFence("future").AtLeast(runtime.TurnFenceChecked) || !runtime.TurnFence("future").AtLeast(runtime.TurnFenceNone) {
		t.Error("an unknown fence does not read as none")
	}
	if runtime.TurnStart("").AtLeast(runtime.TurnStartBoundary) {
		t.Error("an empty start reads above none")
	}
}

// Kills an id bound that truncates rather than refuses, is off by one, or
// accepts an empty id, invalid UTF-8 or a control character as an identity.
func TestTurnIDOverTheBoundIsNoIdentity(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"", false},
		{"t", true},
		{strings.Repeat("a", runtime.MaxTurnIDLen), true},
		{strings.Repeat("a", runtime.MaxTurnIDLen+1), false},
		{"7c9e6679-7425-40de-944b-e07fc1f90ae7", true},
		{"tür-1", true},
		{"a\xffb", false},
		{"a\nb", false},
		{"a\tb", false},
		{"a\x00b", false},
		{"a\u0085b", false},
	}
	for _, tc := range cases {
		if got := runtime.IsTurnIdentity(tc.id); got != tc.want {
			t.Errorf("IsTurnIdentity(%d bytes) = %v, want %v", len(tc.id), got, tc.want)
		}
	}
	if runtime.MaxTurnIDLen != 128 {
		t.Errorf("MaxTurnIDLen = %d, want 128", runtime.MaxTurnIDLen)
	}
}

// Kills a turn-fence outage a caller could mistake for a settled answer:
// it must read as the runtime being unavailable, so the caller retries.
func TestTurnFenceUnavailableIsRuntimeUnavailable(t *testing.T) {
	if !errors.Is(runtime.ErrTurnFenceUnavailable, runtime.ErrRuntimeUnavailable) {
		t.Fatal("ErrTurnFenceUnavailable does not wrap ErrRuntimeUnavailable")
	}
	fenceErr := runtime.ErrTurnFenceUnavailable
	for _, other := range []error{runtime.ErrSessionNotFound, runtime.ErrTurnNotCurrent, runtime.ErrTurnIdentityUnsupported} {
		if errors.Is(fenceErr, other) {
			t.Errorf("ErrTurnFenceUnavailable matches %v", other)
		}
	}
}

// Kills a FullFake whose turn observation does not follow its scripts: a
// script that does not pop or repeat its last entry, an error that applies
// out of order or consumes an entry, an unrecorded read, or an unscripted
// session that reads as anything but the canonical nothing
// TurnCapabilitiesFor reports for a non-provider.
func TestFullFakeTurnObservationScripts(t *testing.T) {
	f := runtime.NewFake()
	var tp runtime.TurnObservationProvider = runtime.FullFake{Fake: f}
	atomic := runtime.TurnCapabilities{Fence: runtime.TurnFenceAtomic}
	checked := runtime.TurnCapabilities{Fence: runtime.TurnFenceChecked}
	f.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{"s": {atomic, checked}}
	stamp := time.Unix(100, 0)
	f.TurnHistoryScripts = map[string][]runtime.TurnHistory{"s": {
		{Current: runtime.TurnState{TurnID: "t1"}, ObservedAt: stamp},
		{Recent: []runtime.EndedTurn{{TurnID: "t1", Status: runtime.TurnStatusCompleted}}},
		{Current: runtime.TurnState{TurnID: "t2"}, Recent: []runtime.EndedTurn{{TurnID: "t1", Status: runtime.TurnStatusCompleted}}},
	}}
	f.TurnHistoryErrors = map[string][]error{"s": {nil, runtime.ErrRuntimeUnavailable}}
	ctx := t.Context()

	var caps []runtime.TurnFence
	for range 3 {
		caps = append(caps, tp.TurnCapabilities("s").Fence)
	}
	if want := []runtime.TurnFence{runtime.TurnFenceAtomic, runtime.TurnFenceChecked, runtime.TurnFenceChecked}; !slices.Equal(caps, want) {
		t.Fatalf("capabilities = %v, want %v", caps, want)
	}
	if got := tp.TurnCapabilities("unscripted"); got != noTurnCapabilities {
		t.Fatalf("unscripted capabilities = %+v, want %+v", got, noTurnCapabilities)
	}

	if h, err := tp.TurnHistory(ctx, "s"); err != nil || h.Current.TurnID != "t1" || !h.ObservedAt.Equal(stamp) {
		t.Fatalf("first read = %+v, %v; want t1 at the scripted stamp", h, err)
	}
	if _, err := tp.TurnHistory(ctx, "s"); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("second read = %v, want the scripted error", err)
	}
	// The error consumed no entry, so the idle read the second entry
	// scripts comes next, then the last entry, repeated.
	if h, err := tp.TurnHistory(ctx, "s"); err != nil || h.Current.Active() || len(h.Recent) != 1 || h.ObservedAt.IsZero() {
		t.Fatalf("third read = %+v, %v; want idle after t1, stamped", h, err)
	}
	for range 2 {
		h, err := tp.TurnHistory(ctx, "s")
		if err != nil || h.Current.TurnID != "t2" || len(h.Recent) != 1 || h.ObservedAt.IsZero() {
			t.Fatalf("later read = %+v, %v; want t2 after t1, stamped", h, err)
		}
	}
	if h, err := tp.TurnHistory(ctx, "unscripted"); err != nil || h.Current.Active() || len(h.Recent) != 0 {
		t.Fatalf("unscripted read = %+v, %v; want no turns", h, err)
	}
	if n := f.CountCalls("TurnHistory", "s"); n != 5 {
		t.Fatalf("recorded TurnHistory calls = %d, want 5", n)
	}
	broken := runtime.FullFake{Fake: runtime.NewFailFake()}
	if _, err := broken.TurnHistory(ctx, "s"); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("broken read = %v, want ErrRuntimeUnavailable, never idle", err)
	}
}
