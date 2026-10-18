package session

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// Kills a family table that steers an agent CLI which never takes typed input
// into its running turn, a keystroke steer over ACP (whose prompts are whole
// turns), or a protocol steer gated on the family.
func TestSupportsSteerCombinesRuntimeAndFamily(t *testing.T) {
	for _, tc := range []struct {
		family, transport string
		mode              runtime.SteerMode
		want              bool
	}{
		{"codex", "", runtime.SteerKeystroke, true},
		{"claude", "", runtime.SteerKeystroke, true},
		{"gemini", "", runtime.SteerKeystroke, false},
		{"kimi", "", runtime.SteerKeystroke, false},
		{"pi", "", runtime.SteerKeystroke, false},
		{"opencode", "", runtime.SteerKeystroke, false},
		{"", "", runtime.SteerKeystroke, false},
		{"codex", "acp", runtime.SteerKeystroke, false},
		{"codex", "", runtime.SteerNone, false},
		{"codex", "", "telepathy", false},
		{"gemini", "", runtime.SteerProtocol, true},
		{"codex", "acp", runtime.SteerProtocol, true},
	} {
		if got := SupportsSteer(tc.family, tc.transport, tc.mode); got != tc.want {
			t.Errorf("SupportsSteer(%q, %q, %q) = %v, want %v", tc.family, tc.transport, tc.mode, got, tc.want)
		}
	}
}

// Kills a family read that ignores a wrapped provider's builtin ancestor, or
// one that does not normalize a provider name into its family.
func TestSupportsSteerForMetadataReadsTheFamily(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta map[string]string
		want bool
	}{
		{"codex", map[string]string{"provider": "codex"}, true},
		{"wrapped codex", map[string]string{"builtin_ancestor": "codex", "provider": "my-wrap"}, true},
		{"provider kind", map[string]string{"provider_kind": "claude", "provider": "fast"}, true},
		{"gemini", map[string]string{"provider": "gemini"}, false},
		{"codex over acp", map[string]string{"provider": "codex", "transport": "acp"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := supportsSteerForMetadata(tc.meta, runtime.SteerKeystroke); got != tc.want {
				t.Fatalf("supportsSteerForMetadata(%v) = %v, want %v", tc.meta, got, tc.want)
			}
		})
	}
	if supportsSteerForMetadata(map[string]string{"provider": "codex"}, runtime.SteerNone) {
		t.Fatal("a runtime that cannot steer advertised steer")
	}
}

// Kills a SupportsSteer that reads only the family or only the runtime, and a
// metadata-only answer that assumes the runtime steers.
func TestSubmissionCapabilitiesSupportsSteer(t *testing.T) {
	for _, tc := range []struct {
		provider string
		mode     runtime.SteerMode
		want     bool
	}{
		{"codex", "", true}, // the fake steers by keystroke when unset
		{"codex", runtime.SteerNone, false},
		{"gemini", runtime.SteerKeystroke, false},
		{"gemini", runtime.SteerProtocol, true},
	} {
		t.Run(tc.provider+"/"+string(tc.mode), func(t *testing.T) {
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
