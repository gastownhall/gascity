package auto

import "github.com/gastownhall/gascity/internal/runtime"

var _ runtime.SteerCapabilityProvider = (*Provider)(nil)

// SteerMode reports how the backend that session name routes to delivers
// input into the session's running turn.
func (p *Provider) SteerMode(name string) runtime.SteerMode {
	return runtime.SteerModeFor(p.route(name), name)
}
