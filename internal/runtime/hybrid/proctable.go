package hybrid

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/runtime"
)

var (
	_ runtime.ProcessTableScanner            = (*Provider)(nil)
	_ runtime.ConditionalProcessTableScanner = (*Provider)(nil)
)

// CanScanProcessTable implements [runtime.ConditionalProcessTableScanner]: the
// composite scans only when its local backend does, so a hybrid over a
// scannerless local backend reads as lacking the capability.
func (p *Provider) CanScanProcessTable() bool {
	return len(runtime.ScanningBackends(p.Backends())) > 0
}

// FindRuntimesBySessionID implements [runtime.ProcessTableScanner] by merging
// the scans of the backends that can scan, local first
// ([runtime.FindRuntimesAcross]). It finds nothing when the local backend
// cannot scan; see [runtime.ScanningBackends].
func (p *Provider) FindRuntimesBySessionID(id string) ([]runtime.LiveRuntime, error) {
	return runtime.FindRuntimesAcross(p.Backends(), id)
}

// TerminateRuntime implements [runtime.ProcessTableScanner]. A scanned root is
// a host process rather than a routed session, so the local backend's scanner
// terminates it.
func (p *Provider) TerminateRuntime(r runtime.LiveRuntime) error {
	scanner, ok := runtime.AsProcessTableScanner(p.local)
	if !ok {
		return fmt.Errorf("hybrid: no backend can terminate runtime PID %d for session %s", r.PID, r.SessionID)
	}
	return scanner.TerminateRuntime(r)
}
