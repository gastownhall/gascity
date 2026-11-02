package hybrid

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/runtime"
)

var (
	_ runtime.ProcessTableScanner            = (*Provider)(nil)
	_ runtime.ConditionalProcessTableScanner = (*Provider)(nil)
)

// The process-table scanner is the local backend's alone. The remote backend
// hosts its sessions off this host, so it is never scanned: a scan it ran
// here could only report this host's runtimes, none of them its own.

// CanScanProcessTable implements [runtime.ConditionalProcessTableScanner]: the
// composite scans only when its local backend does, so a hybrid over a
// scannerless local backend reads as lacking the capability.
func (p *Provider) CanScanProcessTable() bool {
	_, ok := runtime.AsProcessTableScanner(p.local)
	return ok
}

// FindRuntimesBySessionID implements [runtime.ProcessTableScanner] with the
// local backend's scan. It finds nothing when the local backend cannot scan.
func (p *Provider) FindRuntimesBySessionID(id string) ([]runtime.LiveRuntime, error) {
	scanner, ok := runtime.AsProcessTableScanner(p.local)
	if !ok {
		return nil, nil
	}
	return scanner.FindRuntimesBySessionID(id)
}

// TerminateRuntime implements [runtime.ProcessTableScanner] with the local
// backend's scanner.
func (p *Provider) TerminateRuntime(r runtime.LiveRuntime) error {
	scanner, ok := runtime.AsProcessTableScanner(p.local)
	if !ok {
		return fmt.Errorf("hybrid: no backend can terminate runtime PID %d for session %s", r.PID, r.SessionID)
	}
	return scanner.TerminateRuntime(r)
}
