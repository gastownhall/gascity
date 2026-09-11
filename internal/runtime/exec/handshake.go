package exec

import (
	"encoding/json"
	"fmt"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Protocol performs the RPP handshake (`script protocol`) and returns the
// declared version and capabilities. The handshake runs once per provider
// instance and is cached, including its error. An executable without a
// `protocol` op (exit 2 → empty stdout) is version 0 with no optional
// capabilities, so pre-handshake scripts remain fully valid
// (RUNTIME-RPP-008 in internal/runtime/REQUIREMENTS.md).
func (p *Provider) Protocol() (runtime.ProtocolInfo, error) {
	p.handshakeOnce.Do(func() {
		p.handshakeInfo, p.handshakeErr = p.queryProtocol()
	})
	return p.handshakeInfo, p.handshakeErr
}

func (p *Provider) queryProtocol() (runtime.ProtocolInfo, error) {
	out, err := p.run(nil, "protocol")
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
