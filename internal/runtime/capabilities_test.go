package runtime_test

// capabilities_test.go asserts that each provider's Capabilities() return
// value matches the documented matrix in docs/reference/provider-capabilities.md.
//
// These tests serve as a regression guard: if a provider silently changes its
// capability declarations, this file fails and the maintainer knows to update
// both the code and the docs together.
//
// Provider-specific tests also verify that providers correctly implement (or
// do not implement) the optional extension interfaces IdleWaitProvider and
// InteractionProvider.

import (
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/acp"
	"github.com/gastownhall/gascity/internal/runtime/exec"
	"github.com/gastownhall/gascity/internal/runtime/subprocess"
	tmuxruntime "github.com/gastownhall/gascity/internal/runtime/tmux"
)

// capabilityRow describes the expected ProviderCapabilities for one provider.
type capabilityRow struct {
	name                string
	canReportAttachment bool
	canReportActivity   bool
}

// TestProviderCapabilityMatrix asserts that each concrete provider's
// Capabilities() return matches the documented matrix. Update this table and
// the docs together when a provider's capability declarations change.
func TestProviderCapabilityMatrix(t *testing.T) {
	rows := []struct {
		capabilityRow
		provider runtime.Provider
	}{
		{
			capabilityRow{"tmux", true, true},
			tmuxruntime.NewProvider(),
		},
		{
			capabilityRow{"subprocess", false, false},
			subprocess.NewProvider(),
		},
		{
			capabilityRow{"exec", false, false},
			exec.NewProvider("true"), // "true" is a no-op script available on all platforms
		},
		{
			capabilityRow{"acp", false, false},
			acp.NewProvider(acp.Config{}),
		},
	}

	for _, tt := range rows {
		t.Run(tt.name, func(t *testing.T) {
			caps := tt.provider.Capabilities()

			if caps.CanReportAttachment != tt.canReportAttachment {
				t.Errorf("%s: CanReportAttachment = %v, want %v (see docs/reference/provider-capabilities.md)",
					tt.name, caps.CanReportAttachment, tt.canReportAttachment)
			}
			if caps.CanReportActivity != tt.canReportActivity {
				t.Errorf("%s: CanReportActivity = %v, want %v (see docs/reference/provider-capabilities.md)",
					tt.name, caps.CanReportActivity, tt.canReportActivity)
			}
		})
	}
}

// TestProviderOptionalInterfaces asserts which providers implement the
// optional extension interfaces. A provider that unexpectedly gains or loses
// an interface will fail here, prompting a doc update.
func TestProviderOptionalInterfaces(t *testing.T) {
	t.Run("IdleWaitProvider", func(t *testing.T) {
		// tmux: implements IdleWaitProvider.
		if _, ok := runtime.Provider(tmuxruntime.NewProvider()).(runtime.IdleWaitProvider); !ok {
			t.Error("tmux: expected IdleWaitProvider implementation")
		}
		// subprocess: does NOT implement IdleWaitProvider.
		if _, ok := runtime.Provider(subprocess.NewProvider()).(runtime.IdleWaitProvider); ok {
			t.Error("subprocess: unexpected IdleWaitProvider implementation (update docs if intentional)")
		}
		// exec: does NOT implement IdleWaitProvider.
		if _, ok := runtime.Provider(exec.NewProvider("true")).(runtime.IdleWaitProvider); ok {
			t.Error("exec: unexpected IdleWaitProvider implementation (update docs if intentional)")
		}
		// acp: does NOT implement IdleWaitProvider.
		if _, ok := runtime.Provider(acp.NewProvider(acp.Config{})).(runtime.IdleWaitProvider); ok {
			t.Error("acp: unexpected IdleWaitProvider implementation (update docs if intentional)")
		}
	})

	t.Run("InteractionProvider", func(t *testing.T) {
		// tmux: does NOT implement InteractionProvider.
		if _, ok := runtime.Provider(tmuxruntime.NewProvider()).(runtime.InteractionProvider); ok {
			t.Error("tmux: unexpected InteractionProvider implementation (update docs if intentional)")
		}
		// subprocess: does NOT implement InteractionProvider.
		if _, ok := runtime.Provider(subprocess.NewProvider()).(runtime.InteractionProvider); ok {
			t.Error("subprocess: unexpected InteractionProvider implementation (update docs if intentional)")
		}
		// exec: does NOT implement InteractionProvider.
		if _, ok := runtime.Provider(exec.NewProvider("true")).(runtime.InteractionProvider); ok {
			t.Error("exec: unexpected InteractionProvider implementation (update docs if intentional)")
		}
		// acp: implements InteractionProvider.
		if _, ok := runtime.Provider(acp.NewProvider(acp.Config{})).(runtime.InteractionProvider); !ok {
			t.Error("acp: expected InteractionProvider implementation")
		}
	})
}
