package exec

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// handshakeRetryAfter is how long a failed handshake is cached before the
// next caller re-runs it. A success is cached for the provider's life.
const handshakeRetryAfter = 30 * time.Second

// handshakeNow is the handshake cache's clock.
var handshakeNow = time.Now

// Protocol performs the RPP handshake (`script protocol`) and returns the
// declared version and capabilities. A successful handshake runs once per
// provider instance and is cached; a failed one is cached for
// handshakeRetryAfter, so one transient failure does not leave the provider
// on the zero-capability floor for its whole life. The first handshake runs
// under the lock, so every caller waits for its one answer; a retry runs
// outside it, and other callers keep the cached failure meanwhile, so a slow
// or hung `protocol` op never stalls them. An executable without a
// `protocol` op (exit 2 → empty stdout) is version 0 with no optional
// capabilities, so pre-handshake scripts remain fully valid
// (RUNTIME-RPP-008 in internal/runtime/REQUIREMENTS.md).
func (p *Provider) Protocol() (runtime.ProtocolInfo, error) {
	p.handshakeMu.Lock()
	if !p.handshakeRan {
		defer p.handshakeMu.Unlock()
		p.handshakeInfo, p.handshakeErr = p.queryProtocol(context.Background(), p.timeout)
		p.handshakeRan, p.handshakeAt = true, handshakeNow()
		return p.handshakeInfo, p.handshakeErr
	}
	if p.handshakeErr == nil || p.handshakeRetrying || handshakeNow().Sub(p.handshakeAt) < handshakeRetryAfter {
		defer p.handshakeMu.Unlock()
		return p.handshakeInfo, p.handshakeErr
	}
	p.handshakeRetrying = true
	p.handshakeMu.Unlock()

	info, err := p.queryProtocol(context.Background(), p.timeout)
	p.handshakeMu.Lock()
	defer p.handshakeMu.Unlock()
	p.handshakeInfo, p.handshakeErr, p.handshakeAt, p.handshakeRetrying = info, err, handshakeNow(), false
	return info, err
}

func (p *Provider) queryProtocol(ctx context.Context, timeout time.Duration) (runtime.ProtocolInfo, error) {
	out, err := p.runWithContext(ctx, timeout, nil, "protocol")
	if err != nil {
		return runtime.ProtocolInfo{}, err
	}
	if out == "" {
		return runtime.ProtocolInfo{Version: runtime.ProtocolVersion0}, nil
	}
	var info runtime.ProtocolInfo
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		return runtime.ProtocolInfo{}, fmt.Errorf("exec provider %s protocol: invalid handshake JSON: %w", p.script, err)
	}
	if err := info.Validate(); err != nil {
		return runtime.ProtocolInfo{}, fmt.Errorf("exec provider %s: %w", p.script, err)
	}
	return info, nil
}

// handshakeCapability reports whether the executable declared the given
// capability. A failed handshake degrades to the zero-capability floor
// here; the error itself stays observable through Protocol so conformance
// checks and doctor can surface it.
func (p *Provider) handshakeCapability(capability string) bool {
	info, err := p.Protocol()
	if err != nil {
		return false
	}
	return info.Has(capability)
}
