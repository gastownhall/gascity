package exec

import (
	"context"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// v2ProfileCapabilities are the handshake capabilities reconciler v2 needs
// from an exec runtime, in the order V2ProfileGaps reports them. observe.v1
// carries the fresh reads, exact kills and strict exits; report-attachment
// keeps the attach fence a reporter, so v2 never reads an unreported
// terminal as no terminal.
var v2ProfileCapabilities = [...]string{
	runtime.ProtocolCapabilityObserveV1,
	runtime.ProtocolCapabilityReportAttachment,
}

// V2ProfileGaps returns the handshake capabilities reconciler v2 needs that
// info lacks, in a fixed order (observe.v1, report-attachment); nil means
// v2-ready. It is the one definition of exec v2 readiness: the provider, the
// latch, the doctor and the conformance kit all ask it.
func V2ProfileGaps(info runtime.ProtocolInfo) []string {
	var missing []string
	for _, c := range v2ProfileCapabilities {
		if !info.Has(c) {
			missing = append(missing, c)
		}
	}
	return missing
}

// ProbeV2Readiness runs `script protocol` once under timeout, for callers
// that hold no provider (the latch, the doctor), and returns the handshake
// and its gaps. The op runs with the city env, as the provider the registry
// builds for the city runs it (NewProviderForCity), so a handshake scoped by
// city answers the probe as it answers that provider. Nothing is cached, so
// an error is never remembered.
func ProbeV2Readiness(ctx context.Context, script, cityName, cityPath string, timeout time.Duration) (runtime.ProtocolInfo, []string, error) {
	info, err := NewProviderForCity(script, cityName, cityPath).queryProtocol(ctx, timeout)
	if err != nil {
		return runtime.ProtocolInfo{}, nil, err
	}
	return info, V2ProfileGaps(info), nil
}

// V2Readiness reports the gaps in this provider's own handshake, or the
// handshake's error, and implements [runtime.V2ReadinessReporter].
func (p *Provider) V2Readiness() ([]string, error) {
	info, err := p.Protocol()
	if err != nil {
		return nil, err
	}
	return V2ProfileGaps(info), nil
}

// v2Ready reports a handshake with no v2 gaps. Every observe.v1 behavior is
// gated on it, so a script that does not declare the profile, or whose
// handshake failed, behaves exactly as before. A nil provider is not ready.
func (p *Provider) v2Ready() bool {
	if p == nil {
		return false
	}
	missing, err := p.V2Readiness()
	return err == nil && len(missing) == 0
}
