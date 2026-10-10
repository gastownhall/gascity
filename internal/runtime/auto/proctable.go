package auto

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/runtime"
)

var (
	_ runtime.ProcessTableScanner            = (*Provider)(nil)
	_ runtime.ConditionalProcessTableScanner = (*Provider)(nil)
)

// CanScanProcessTable implements [runtime.ConditionalProcessTableScanner]: the
// composite scans only when its default backend does. Callers using
// [runtime.AsProcessTableScanner] therefore see a composite over a
// scannerless default exactly as they saw it before it forwarded the scanner.
func (p *Provider) CanScanProcessTable() bool {
	return len(runtime.ScanningBackends(p.Backends())) > 0
}

// FindRuntimesBySessionID implements [runtime.ProcessTableScanner] by querying
// every backend that can scan a process table and merging the results by PID
// ([runtime.FindRuntimesAcross]).
//
// Forwarding requires the default backend to scan. The ACP scanner tracks only
// ACP-hosted sessions, so behind a scannerless default (hybrid, herdr,
// t3bridge, exec, k8s) it would report every live default-hosted runtime
// carrying GC_SESSION_ID as an untracked orphan, and orphan reaping would kill
// it. With a scannerless default this therefore finds nothing, and
// [Provider.CanScanProcessTable] reports false.
//
// Without this forwarding, routing any session in a city to ACP would hide the
// default backend's scanner behind the composite and silently turn orphan
// reaping off for every session in that city.
func (p *Provider) FindRuntimesBySessionID(id string) ([]runtime.LiveRuntime, error) {
	return runtime.FindRuntimesAcross(p.Backends(), id)
}

// TerminateRuntime implements [runtime.ProcessTableScanner]. A scanned root is
// a host process rather than a routed session, so it is terminated by exactly
// one backend: the default backend's scanner. Local scanners terminate by
// identity-checked PID signaling, so the choice does not change which process
// is signaled. With a scannerless default it returns an error, matching
// FindRuntimesBySessionID, which surfaces nothing to terminate.
func (p *Provider) TerminateRuntime(r runtime.LiveRuntime) error {
	scanners := runtime.ScanningBackends(p.Backends())
	if len(scanners) == 0 {
		return fmt.Errorf("auto: no backend can terminate runtime PID %d for session %s", r.PID, r.SessionID)
	}
	return scanners[0].Scanner.TerminateRuntime(r)
}
