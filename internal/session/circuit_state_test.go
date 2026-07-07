package session

import "testing"

// TestCircuitStateFromMetadataProjectsVerbatim is the codec-edge byte-identical
// oracle for the circuit-breaker front door: for representative session_circuit_*
// shapes, every CircuitState field must equal the raw metadata value it projects,
// with no transformation. Parsing (timestamps, restart JSON, state kind) stays
// with the breaker, so the codec must carry each value verbatim — including
// malformed and padded values, which the breaker validates downstream.
func TestCircuitStateFromMetadataProjectsVerbatim(t *testing.T) {
	shapes := map[string]map[string]string{
		"nil":   nil,
		"empty": {},
		"open-full": {
			SessionCircuitStateMetadataKey:             SessionCircuitStateOpen,
			SessionCircuitRestartsMetadataKey:          `["2026-04-01T12:00:00Z","2026-04-01T12:01:00Z"]`,
			SessionCircuitLastRestartMetadataKey:       "2026-04-01T12:01:00Z",
			SessionCircuitLastProgressMetadataKey:      "2026-04-01T11:30:00Z",
			SessionCircuitLastObservedMetadataKey:      "2026-04-01T12:02:00Z",
			SessionCircuitProgressSignatureMetadataKey: "assigned-work",
			SessionCircuitOpenedAtMetadataKey:          "2026-04-01T12:01:30Z",
			SessionCircuitOpenRestartCountMetadataKey:  "6",
			SessionCircuitResetGenerationMetadataKey:   "3",
		},
		"closed": {
			SessionCircuitStateMetadataKey:           SessionCircuitStateClosed,
			SessionCircuitResetGenerationMetadataKey: "1",
		},
		"reset-generation-only": {
			SessionCircuitResetGenerationMetadataKey: "9",
		},
		"malformed-and-padded": {
			// The codec must carry these through untouched; the breaker rejects or
			// trims them, not CircuitStateFromMetadata.
			SessionCircuitStateMetadataKey:            "BROKEN",
			SessionCircuitRestartsMetadataKey:         "not-json",
			SessionCircuitLastRestartMetadataKey:      "not-a-time",
			SessionCircuitOpenRestartCountMetadataKey: "  6  ",
		},
		// A bead carrying unrelated keys must project to the zero CircuitState.
		"unrelated-keys-only": {
			"state":        "awake",
			"session_name": "worker-1",
		},
	}

	for name, meta := range shapes {
		meta := meta
		t.Run(name, func(t *testing.T) {
			cs := CircuitStateFromMetadata(meta)
			checks := map[string]struct{ got, want string }{
				SessionCircuitStateMetadataKey:             {cs.State, meta[SessionCircuitStateMetadataKey]},
				SessionCircuitRestartsMetadataKey:          {cs.Restarts, meta[SessionCircuitRestartsMetadataKey]},
				SessionCircuitLastRestartMetadataKey:       {cs.LastRestart, meta[SessionCircuitLastRestartMetadataKey]},
				SessionCircuitLastProgressMetadataKey:      {cs.LastProgress, meta[SessionCircuitLastProgressMetadataKey]},
				SessionCircuitLastObservedMetadataKey:      {cs.LastObserved, meta[SessionCircuitLastObservedMetadataKey]},
				SessionCircuitProgressSignatureMetadataKey: {cs.ProgressSignature, meta[SessionCircuitProgressSignatureMetadataKey]},
				SessionCircuitOpenedAtMetadataKey:          {cs.OpenedAt, meta[SessionCircuitOpenedAtMetadataKey]},
				SessionCircuitOpenRestartCountMetadataKey:  {cs.OpenRestartCount, meta[SessionCircuitOpenRestartCountMetadataKey]},
				SessionCircuitResetGenerationMetadataKey:   {cs.ResetGeneration, meta[SessionCircuitResetGenerationMetadataKey]},
			}
			for key, c := range checks {
				if c.got != c.want {
					t.Errorf("field for %q = %q, want raw value %q", key, c.got, c.want)
				}
			}
		})
	}
}

// TestStoreCircuitStateReturnsClusterVerbatim proves the store-authoritative
// accessor returns the same projection as CircuitStateFromMetadata over the
// persisted bead, and confines a single Get (no mutating bead op) — the
// byte-identical replacement for the raw store.Get(id) + read of the
// session_circuit_* keys.
func TestStoreCircuitStateReturnsClusterVerbatim(t *testing.T) {
	meta := map[string]string{
		SessionCircuitStateMetadataKey:             SessionCircuitStateOpen,
		SessionCircuitRestartsMetadataKey:          `["2026-04-01T12:00:00Z"]`,
		SessionCircuitLastRestartMetadataKey:       "2026-04-01T12:01:00Z",
		SessionCircuitLastProgressMetadataKey:      "2026-04-01T11:30:00Z",
		SessionCircuitLastObservedMetadataKey:      "2026-04-01T12:02:00Z",
		SessionCircuitProgressSignatureMetadataKey: "assigned-work",
		SessionCircuitOpenedAtMetadataKey:          "2026-04-01T12:01:30Z",
		SessionCircuitOpenRestartCountMetadataKey:  "6",
		SessionCircuitResetGenerationMetadataKey:   "3",
	}
	b := sessionBeadFixture("s-1", "open", meta)
	is, rec := recordingStore(t, b)

	got, err := is.CircuitState("s-1")
	if err != nil {
		t.Fatalf("CircuitState: %v", err)
	}
	if want := CircuitStateFromMetadata(meta); got != want {
		t.Errorf("CircuitState = %#v, want %#v", got, want)
	}
	if mutating := opsOf(rec.Calls()); len(mutating) != 0 {
		t.Errorf("CircuitState emitted mutating ops %v, want none", mutating)
	}
}

// TestStoreCircuitStateSurfacesStoreError proves a missing bead surfaces the
// bare store error (the caller owns its diagnostic wrapping), matching the raw
// store.Get error path the front door replaces.
func TestStoreCircuitStateSurfacesStoreError(t *testing.T) {
	store := seedSessionStore(t)
	is := NewStore(store)
	if _, err := is.CircuitState("missing"); err == nil {
		t.Fatal("CircuitState(missing): want store error, got nil")
	}
}
