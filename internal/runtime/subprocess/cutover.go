package subprocess

import "github.com/gastownhall/gascity/internal/runtime"

// seamBackedProvider serves the legacy [runtime.Provider] entirely through the
// de-conflated seams (via [runtime.NewProviderFromSeams]), passing the non-seam
// extensions (ProcessTableScanner, SleepCapability) through to the underlying
// *Provider. It is the early CUT-OVER for the subprocess provider: real callers
// exercise the seams end-to-end, validated by TestSubprocessSeamConformance
// (the full Provider conformance suite, run against this composition).
//
// Subprocess is the cut-over target because the seam contract's known gaps do
// not bite it: its ProcessAlive ignores process names (so Observe's missing
// names parameter is harmless) and its RunLive is already a no-op. The carrier
// providers (exec/k8s/ssh), which pgrep on process names, need those gaps
// resolved before they cut over.
type seamBackedProvider struct {
	runtime.Provider           // the 19 Provider methods, served via the seams
	raw              *Provider // the non-seam extensions
}

var (
	_ runtime.Provider            = (*seamBackedProvider)(nil)
	_ runtime.ProcessTableScanner = (*seamBackedProvider)(nil)
)

// NewSeamBacked returns a subprocess provider served through the seams, storing
// socket/meta files in a default temporary directory. It returns the
// [runtime.Provider] interface (matching the other provider constructors); the
// non-seam extensions remain reachable via comma-ok type assertion on the
// concrete *seamBackedProvider behind it.
func NewSeamBacked() runtime.Provider { return seamBack(NewProvider()) }

// NewSeamBackedWithDir returns NewProviderWithDir served through the seams.
func NewSeamBackedWithDir(dir string) runtime.Provider { return seamBack(NewProviderWithDir(dir)) }

func seamBack(raw *Provider) *seamBackedProvider {
	rt, tp := raw.Seams()
	return &seamBackedProvider{Provider: runtime.NewProviderFromSeams(rt, tp), raw: raw}
}

// FindRuntimesBySessionID implements [runtime.ProcessTableScanner] (non-seam).
func (s *seamBackedProvider) FindRuntimesBySessionID(id string) ([]runtime.LiveRuntime, error) {
	return s.raw.FindRuntimesBySessionID(id)
}

// TerminateRuntime implements [runtime.ProcessTableScanner] (non-seam).
func (s *seamBackedProvider) TerminateRuntime(r runtime.LiveRuntime) error {
	return s.raw.TerminateRuntime(r)
}

// SleepCapability passes through to the underlying provider (non-seam).
func (s *seamBackedProvider) SleepCapability(name string) runtime.SessionSleepCapability {
	return s.raw.SleepCapability(name)
}
