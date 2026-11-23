package session

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// withSteerFamily adds family to the keystroke steer table for the test, so
// the table's mechanics can be pinned while it ships empty. Session tests do
// not run in parallel, so the swap is race-free.
func withSteerFamily(t *testing.T, family string) {
	t.Helper()
	prev := steerFamilies
	steerFamilies = map[string]bool{family: true}
	t.Cleanup(func() { steerFamilies = prev })
}

// Kills a steer table that lists a family whose CLI does not join typed input
// into its running turn: Codex reads tmux's trailing Escape as cancel, and
// Claude queues typed input for its next turn (DES-O1 §10.4, D-STEER).
func TestNoFamilyIsKeystrokeSteerable(t *testing.T) {
	for _, family := range []string{"codex", "claude", "gemini", "kimi", "pi", "opencode", ""} {
		if supportsSteer(family, "", runtime.SteerKeystroke) {
			t.Errorf("supportsSteer(%q, keystroke) = true, want false", family)
		}
	}
	if len(steerFamilies) != 0 {
		t.Fatalf("steerFamilies = %v, want empty until a runtime can confirm a steer", steerFamilies)
	}
}

// Kills a supportsSteer that ignores the table, steers by keystroke over ACP
// (whose prompts are whole turns), accepts an unknown mode, or gates a
// protocol steer on the family.
func TestSupportsSteerCombinesRuntimeAndFamily(t *testing.T) {
	withSteerFamily(t, "codex")
	for _, tc := range []struct {
		family, transport string
		mode              runtime.SteerMode
		want              bool
	}{
		{"codex", "", runtime.SteerKeystroke, true},
		{"Codex", " ", runtime.SteerKeystroke, true},
		{"gemini", "", runtime.SteerKeystroke, false},
		{"", "", runtime.SteerKeystroke, false},
		{"codex", "acp", runtime.SteerKeystroke, false},
		{"codex", "", runtime.SteerNone, false},
		{"codex", "", "telepathy", false},
		{"gemini", "", runtime.SteerProtocol, true},
		{"codex", "acp", runtime.SteerProtocol, true},
	} {
		if got := supportsSteer(tc.family, tc.transport, tc.mode); got != tc.want {
			t.Errorf("supportsSteer(%q, %q, %q) = %v, want %v", tc.family, tc.transport, tc.mode, got, tc.want)
		}
	}
}

// Kills a row read that ignores a wrapped provider's builtin ancestor or
// provider kind, does not normalize a provider name into its family, misses
// ACP named by the provider rather than the transport, or reads the
// live-detected Transport instead of the row's.
func TestSupportsSteerForInfoReadsTheRow(t *testing.T) {
	withSteerFamily(t, "codex")
	for _, tc := range []struct {
		name string
		meta map[string]string
		want bool
	}{
		{"codex", map[string]string{"provider": "codex"}, true},
		{"wrapped codex", map[string]string{"builtin_ancestor": "codex", "provider": "my-wrap"}, true},
		{"provider kind", map[string]string{"provider_kind": "codex", "provider": "fast"}, true},
		{"gemini", map[string]string{"provider": "gemini"}, false},
		{"no provider", map[string]string{}, false},
		{"codex over acp", map[string]string{"provider": "codex", "transport": "acp"}, false},
		{"acp provider", map[string]string{"provider_kind": "codex", "provider": "acp"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := infoFromPersistedBead(beads.Bead{Metadata: tc.meta})
			if got := SupportsSteerForInfo(info, runtime.SteerKeystroke); got != tc.want {
				t.Fatalf("SupportsSteerForInfo(%v) = %v, want %v", tc.meta, got, tc.want)
			}
			if got := supportsSteerForMetadata(tc.meta, runtime.SteerKeystroke); got != tc.want {
				t.Fatalf("supportsSteerForMetadata(%v) = %v, want %v", tc.meta, got, tc.want)
			}
		})
	}
	info := infoFromPersistedBead(beads.Bead{Metadata: map[string]string{"provider": "codex"}})
	info.Transport = "acp" // a live overlay; the capability reads the row
	if !SupportsSteerForInfo(info, runtime.SteerKeystroke) {
		t.Fatal("SupportsSteerForInfo read the live-detected transport, not the row's")
	}
	if SupportsSteerForInfo(infoFromPersistedBead(beads.Bead{Metadata: map[string]string{"provider": "codex"}}), runtime.SteerNone) {
		t.Fatal("a runtime that cannot steer advertised steer")
	}
}

// Kills a SubmissionCapabilities whose supports_steer reads only the family
// or only the runtime, and a metadata-only answer that assumes the runtime
// steers.
func TestSubmissionCapabilitiesSupportsSteer(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		mode     runtime.SteerMode
		table    bool
		want     bool
	}{
		{"codex keystroke ships off", "codex", "", false, false},
		{"table family keystroke", "codex", "", true, true}, // the fake steers by keystroke when unset
		{"table family none", "codex", runtime.SteerNone, true, false},
		{"other family keystroke", "gemini", runtime.SteerKeystroke, true, false},
		{"protocol", "gemini", runtime.SteerProtocol, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.table {
				withSteerFamily(t, "codex")
			}
			sp := runtime.NewFake()
			sp.SteerModeValue = tc.mode
			mgr := newTestManager(t, beads.NewMemStore(), sp)
			info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: tc.provider, WorkDir: t.TempDir(), Provider: tc.provider, ExtraMeta: map[string]string{"session_origin": "manual"}})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			caps, err := mgr.SubmissionCapabilities(info.ID)
			if err != nil {
				t.Fatalf("SubmissionCapabilities: %v", err)
			}
			if caps.SupportsSteer != tc.want {
				t.Fatalf("SupportsSteer = %v, want %v", caps.SupportsSteer, tc.want)
			}
		})
	}
	if SubmissionCapabilitiesForMetadata(map[string]string{"provider": "codex"}, true).SupportsSteer {
		t.Fatal("metadata alone advertised steer without a runtime that steers")
	}
}
