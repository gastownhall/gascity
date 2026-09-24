package auto

import (
	"errors"
	"fmt"
	"sort"

	"github.com/gastownhall/gascity/internal/runtime"
)

var _ runtime.ProcessTableScanner = (*Provider)(nil)

// FindRuntimesBySessionID implements [runtime.ProcessTableScanner] by querying
// every backend that can scan a process table and merging the results by PID.
//
// Local backends scan the same host process table, so one root commonly
// appears in several results, each backend marking tracked only the sessions
// it hosts. A root is therefore tracked when ANY backend tracks it, and the
// tracking backend's record (with its ProviderName) is kept. Results are
// ordered by PID. Backend errors are labeled and joined; partial results from
// a failing backend are still merged, per the best-effort contract.
//
// Forwarding requires the default backend to scan. The ACP scanner tracks only
// ACP-hosted sessions, so behind a scannerless default (hybrid, herdr,
// t3bridge, exec, k8s) it would report every live default-hosted runtime
// carrying GC_SESSION_ID as an untracked orphan, and orphan reaping would kill
// it. With a scannerless default this therefore finds nothing, as before the
// composite forwarded the scanner.
//
// Without this forwarding, routing any session in a city to ACP would hide the
// default backend's scanner behind the composite and silently turn orphan
// reaping off for every session in that city.
func (p *Provider) FindRuntimesBySessionID(id string) ([]runtime.LiveRuntime, error) {
	merged := make(map[int]runtime.LiveRuntime)
	var errs []error
	for _, b := range p.scanningBackends() {
		found, err := b.scanner.FindRuntimesBySessionID(id)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s backend: %w", b.label, err))
		}
		for _, r := range found {
			if existing, ok := merged[r.PID]; !ok || (r.IsTracked && !existing.IsTracked) {
				merged[r.PID] = r
			}
		}
	}
	out := make([]runtime.LiveRuntime, 0, len(merged))
	for _, r := range merged {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out, errors.Join(errs...)
}

// TerminateRuntime implements [runtime.ProcessTableScanner]. A scanned root is
// a host process rather than a routed session, so it is terminated by exactly
// one backend: the default backend's scanner. Local scanners terminate by
// identity-checked PID signaling, so the choice does not change which process
// is signaled. With a scannerless default it returns an error, matching
// FindRuntimesBySessionID, which surfaces nothing to terminate.
func (p *Provider) TerminateRuntime(r runtime.LiveRuntime) error {
	backends := p.scanningBackends()
	if len(backends) == 0 {
		return fmt.Errorf("auto: no backend can terminate runtime PID %d for session %s", r.PID, r.SessionID)
	}
	return backends[0].scanner.TerminateRuntime(r)
}

type scanningBackend struct {
	label   string
	scanner runtime.ProcessTableScanner
}

// scanningBackends returns the backends implementing
// [runtime.ProcessTableScanner], default first. It returns none when the
// default backend cannot scan; see [Provider.FindRuntimesBySessionID].
func (p *Provider) scanningBackends() []scanningBackend {
	s, ok := p.defaultSP.(runtime.ProcessTableScanner)
	if !ok {
		return nil
	}
	out := []scanningBackend{{label: "default", scanner: s}}
	if s, ok := p.acpSP.(runtime.ProcessTableScanner); ok {
		out = append(out, scanningBackend{label: "acp", scanner: s})
	}
	return out
}
