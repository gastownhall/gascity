package session

import "testing"

// These tests pin the session target classification ladder extracted from
// the API resolver (resolveSessionTargetIDWithContext). The precedence is a
// contract: template-form rejection, exact bead ID, configured named
// session, live session (with config-orphan rejection), path alias, then —
// on allow-closed surfaces only — named-spec rejection ahead of closed
// lookup. Caller-side characterization lives in
// internal/api/session_resolution_precedence_test.go and the phase0 specs.

func classify(f TargetFacts) TargetDecision { return DecideSessionTarget(f) }

func gatherSequence(t *testing.T, f TargetFacts, supply func(TargetStep, *TargetFacts) bool) TargetDecision {
	t.Helper()
	for range [16]int{} {
		dec := DecideSessionTarget(f)
		if dec.Action != TargetGather {
			return dec
		}
		if !supply(dec.Gather, &f) {
			t.Fatalf("unexpected gather request %v", dec.Gather)
		}
	}
	t.Fatal("classifier did not terminate within 16 decisions")
	return TargetDecision{}
}

func TestDecideSessionTargetTemplateFormRejected(t *testing.T) {
	dec := classify(TargetFacts{TemplateForm: true})
	if dec.Action != TargetDone || dec.Result != TargetNotFound {
		t.Fatalf("template-form target: got %+v, want done/not-found", dec)
	}
}

func TestDecideSessionTargetGatherOrder(t *testing.T) {
	var order []TargetStep
	dec := gatherSequence(t, TargetFacts{AllowClosed: true}, func(step TargetStep, f *TargetFacts) bool {
		order = append(order, step)
		switch step {
		case TargetStepExactID:
			f.ExactID = LookupNoMatch
		case TargetStepConfiguredName:
			f.ConfiguredName = NamedLookupNoMatch
		case TargetStepLive:
			f.Live = LookupNoMatch
		case TargetStepPathAlias:
			f.PathAlias = LookupNoMatch
		case TargetStepClosedNamedSpec:
			f.ClosedNamedSpec = LookupNoMatch
		case TargetStepClosed:
			f.Closed = LookupNoMatch
		default:
			return false
		}
		return true
	})
	want := []TargetStep{
		TargetStepExactID, TargetStepConfiguredName, TargetStepLive,
		TargetStepPathAlias, TargetStepClosedNamedSpec, TargetStepClosed,
	}
	if len(order) != len(want) {
		t.Fatalf("gather order %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("gather order %v, want %v", order, want)
		}
	}
	if dec.Result != TargetNotFound {
		t.Fatalf("all-miss ladder result %v, want not-found", dec.Result)
	}
}

func TestDecideSessionTargetSelectionShortCircuits(t *testing.T) {
	cases := []struct {
		name   string
		facts  TargetFacts
		source TargetStep
	}{
		{
			"exact ID match wins immediately",
			TargetFacts{ExactID: LookupMatch},
			TargetStepExactID,
		},
		{
			"configured name match wins before live",
			TargetFacts{ExactID: LookupNoMatch, ConfiguredName: NamedLookupMatch},
			TargetStepConfiguredName,
		},
		{
			"live match wins before path alias",
			TargetFacts{ExactID: LookupNoMatch, ConfiguredName: NamedLookupNoMatch, Live: LookupMatch},
			TargetStepLive,
		},
		{
			"path alias match wins before closed ladder",
			TargetFacts{ExactID: LookupNoMatch, ConfiguredName: NamedLookupNoMatch, Live: LookupNoMatch, PathAlias: LookupMatch, AllowClosed: true},
			TargetStepPathAlias,
		},
		{
			"closed match selected on allow-closed surfaces",
			TargetFacts{ExactID: LookupNoMatch, ConfiguredName: NamedLookupNoMatch, Live: LookupNoMatch, PathAlias: LookupNoMatch, AllowClosed: true, ClosedNamedSpec: LookupNoMatch, Closed: LookupMatch},
			TargetStepClosed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec := classify(tc.facts)
			if dec.Action != TargetDone || dec.Result != TargetSelected {
				t.Fatalf("got %+v, want done/selected", dec)
			}
			if dec.Source != tc.source {
				t.Fatalf("source = %v, want %v", dec.Source, tc.source)
			}
		})
	}
}

func TestDecideSessionTargetErrorsAreTerminal(t *testing.T) {
	cases := []struct {
		name   string
		facts  TargetFacts
		source TargetStep
	}{
		{
			"exact ID store error",
			TargetFacts{ExactID: LookupError},
			TargetStepExactID,
		},
		{
			"configured name terminal error",
			TargetFacts{ExactID: LookupNoMatch, ConfiguredName: NamedLookupTerminalError},
			TargetStepConfiguredName,
		},
		{
			"live lookup error",
			TargetFacts{ExactID: LookupNoMatch, ConfiguredName: NamedLookupNoMatch, Live: LookupError},
			TargetStepLive,
		},
		{
			"path alias error",
			TargetFacts{ExactID: LookupNoMatch, ConfiguredName: NamedLookupNoMatch, Live: LookupNoMatch, PathAlias: LookupError},
			TargetStepPathAlias,
		},
		{
			"named spec error",
			TargetFacts{ExactID: LookupNoMatch, ConfiguredName: NamedLookupNoMatch, Live: LookupNoMatch, PathAlias: LookupNoMatch, AllowClosed: true, ClosedNamedSpec: LookupError},
			TargetStepClosedNamedSpec,
		},
		{
			"closed lookup error",
			TargetFacts{ExactID: LookupNoMatch, ConfiguredName: NamedLookupNoMatch, Live: LookupNoMatch, PathAlias: LookupNoMatch, AllowClosed: true, ClosedNamedSpec: LookupNoMatch, Closed: LookupError},
			TargetStepClosed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec := classify(tc.facts)
			if dec.Action != TargetDone || dec.Result != TargetError {
				t.Fatalf("got %+v, want done/error", dec)
			}
			if dec.Source != tc.source {
				t.Fatalf("source = %v, want %v", dec.Source, tc.source)
			}
		})
	}
}

// A live match for a named-session bead whose configured identity is gone
// is rejected by config, not selected and not silently skipped.
func TestDecideSessionTargetLiveConfigOrphanRejected(t *testing.T) {
	dec := classify(TargetFacts{
		ExactID:          LookupNoMatch,
		ConfiguredName:   NamedLookupNoMatch,
		Live:             LookupMatch,
		LiveConfigOrphan: true,
	})
	if dec.Action != TargetDone || dec.Result != TargetRejectedByConfig {
		t.Fatalf("config-orphan live match: got %+v, want done/rejected-by-config", dec)
	}
	if dec.Source != TargetStepLive {
		t.Fatalf("source = %v, want live step", dec.Source)
	}
}

// Reserved configured identities never reach closed lookup: a named-spec
// match on an allow-closed surface terminates as not-found.
func TestDecideSessionTargetNamedSpecBlocksClosedLookup(t *testing.T) {
	dec := classify(TargetFacts{
		ExactID:         LookupNoMatch,
		ConfiguredName:  NamedLookupNoMatch,
		Live:            LookupNoMatch,
		PathAlias:       LookupNoMatch,
		AllowClosed:     true,
		ClosedNamedSpec: LookupMatch,
		Closed:          LookupMatch, // must be ignored
	})
	if dec.Action != TargetDone || dec.Result != TargetNotFound {
		t.Fatalf("named-spec on allow-closed: got %+v, want done/not-found", dec)
	}
	if dec.Source != TargetStepClosedNamedSpec {
		t.Fatalf("source = %v, want closed-named-spec step", dec.Source)
	}
}

// Without allow-closed, the ladder ends after path alias: no closed-side
// facts are requested.
func TestDecideSessionTargetClosedLadderGatedOnAllowClosed(t *testing.T) {
	dec := classify(TargetFacts{
		ExactID:        LookupNoMatch,
		ConfiguredName: NamedLookupNoMatch,
		Live:           LookupNoMatch,
		PathAlias:      LookupNoMatch,
	})
	if dec.Action != TargetDone || dec.Result != TargetNotFound {
		t.Fatalf("live-only ladder miss: got %+v, want done/not-found", dec)
	}
}
